//go:build integration

// Integration tests for the gateway entrypoint itself: they build the real gateway image from
// the embedded assets and start it with a fabricated /etc/hosts, which is the only way to
// reproduce how a container runtime hands the host gateway address over. Run them with
// `make itest`.
//
// The generated configuration is copied into the container rather than bind-mounted, so the
// tests also pass against a remote daemon, where a bind mount would silently resolve to an
// empty directory.
package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lukashornych/hole/v2/assets"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/image"
	"github.com/lukashornych/hole/v2/internal/network"
	"github.com/lukashornych/hole/v2/internal/state"
)

// hostGatewayTestAddress is in TEST-NET-1, so it can never collide with a real host gateway and
// is distinguishable from the loopback address the health zone legitimately answers.
const hostGatewayTestAddress = "192.0.2.77"

// gatewayTestContext builds the gateway image and materializes the artifacts a policy with one
// `hostGatewayDomains` entry generates — the only policy shape that puts `{HOST_GATEWAY_IP}`
// into the Corefile, and therefore the only one that arms the entrypoint's check.
func gatewayTestContext(t *testing.T, containerEngine *engine.Engine) (gatewayImage, confDir string) {
	t.Helper()
	runTmpDir := t.TempDir()
	policy := network.BuildPolicy(nil, nil,
		[]network.HostGatewayDomain{{Domain: "mydb.local", Ports: []int{5432}}}, false)
	confDir, err := writeGatewayArtifacts(runTmpDir, policy)
	if err != nil {
		t.Fatalf("writeGatewayArtifacts: %v", err)
	}

	gatewayImage = image.GatewayImage(assets.BuildInputsHash())
	if !containerEngine.ImageExists(gatewayImage) {
		// The build pulls CoreDNS from GitHub and packages from the Ubuntu mirrors, which the
		// agent sandbox cannot reach; skipping there beats failing for an unrelated reason.
		if err := containerEngine.RunQuiet("build", "-t", gatewayImage,
			filepath.Join(runTmpDir, "gateway")); err != nil {
			t.Skipf("could not build the gateway image (the build needs network access): %v", err)
		}
	}
	return gatewayImage, confDir
}

// startGatewayContainer creates the container, copies the generated configuration in and starts
// it. addHosts entries are passed through verbatim, in order, which is what decides the order of
// the /etc/hosts lines the entrypoint reads.
func startGatewayContainer(t *testing.T, containerEngine *engine.Engine,
	container, gatewayImage, confDir, subnet string, networks []string, addHosts ...string) {

	t.Helper()
	args := []string{"create", "--name", container,
		"--cap-add", "NET_ADMIN",
		"--sysctl", "net.ipv4.ip_forward=1",
		"--label", engine.LabelManaged + "=true",
		"--label", engine.LabelInstance + "=" + container,
		"--label", engine.LabelProject + "=itest-gateway",
		"-e", "HOLE_SANDBOX_SUBNET=" + subnet,
	}
	for _, addHost := range addHosts {
		args = append(args, "--add-host", addHost)
	}
	if len(networks) > 0 {
		args = append(args, "--network", networks[0])
	}
	args = append(args, gatewayImage)
	if err := containerEngine.RunQuiet(args...); err != nil {
		t.Skipf("could not create the gateway container: %v", err)
	}
	if len(networks) > 1 {
		for _, name := range networks[1:] {
			if err := containerEngine.NetworkConnect(name, container); err != nil {
				t.Fatalf("connect %s: %v", name, err)
			}
		}
	}
	// docker cp creates /etc/hole from the source directory, so the container needs no volume.
	if err := containerEngine.RunQuiet("cp", confDir, container+":/etc/hole"); err != nil {
		t.Fatalf("copy the generated configuration into the container: %v", err)
	}
	if err := containerEngine.RunQuiet("start", container); err != nil {
		t.Fatalf("start %s: %v", container, err)
	}
}

// waitForGatewayLog polls the container log for a marker, which is how both tests observe the
// entrypoint without waiting for a healthcheck that may never pass.
func waitForGatewayLog(t *testing.T, containerEngine *engine.Engine, container, marker string) string {
	t.Helper()
	var logs string
	for attempt := 0; attempt < 40; attempt++ {
		logs, _ = containerEngine.ContainerLogs(container)
		if strings.Contains(logs, marker) {
			return logs
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the gateway never logged %q; log was:\n%s", marker, logs)
	return logs
}

// TestGatewayFailsOnLoopbackHostGateway covers the fail-loudly half of the fix: a runtime that
// offers nothing but a loopback address for the host gateway must abort startup rather than
// substitute it, which used to make every configured name resolve to the sandbox itself.
//
// One network is enough — the check precedes interface discovery.
func TestGatewayFailsOnLoopbackHostGateway(t *testing.T) {
	containerEngine := testEngine(t)
	gatewayImage, confDir := gatewayTestContext(t, containerEngine)

	container := "hole-sandbox-itest-gateway-loopback"
	_ = containerEngine.ContainerRemove(container)
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	startGatewayContainer(t, containerEngine, container, gatewayImage, confDir,
		"10.225.90.0/24", nil, "host.internal:127.0.0.1")

	code, err := containerEngine.ContainerWait(container)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code == 0 {
		logs, _ := containerEngine.ContainerLogs(container)
		t.Fatalf("the gateway started with a loopback host gateway address; log was:\n%s", logs)
	}
	logs, _ := containerEngine.ContainerLogs(container)
	if !strings.Contains(logs, "no usable IPv4 host gateway address") {
		t.Errorf("the abort did not name its cause; log was:\n%s", logs)
	}
}

// TestGatewayPrefersIPv4HostGateway is the regression itself: with both an IPv4 and an IPv6
// entry for the same name — IPv6 first, as OrbStack writes them — the entrypoint has to pick the
// IPv4 one. `getent hosts` answers AF_INET6 first, so the old lookup found nothing here and fell
// back to 127.0.0.1.
//
// The address the entrypoint logs is the exact value it substitutes into both generated files,
// so asserting on the log needs no exec into a container that may still be starting.
func TestGatewayPrefersIPv4HostGateway(t *testing.T) {
	containerEngine := testEngine(t)
	gatewayImage, confDir := gatewayTestContext(t, containerEngine)

	container := "hole-sandbox-itest-gateway-dualstack"
	_ = containerEngine.ContainerRemove(container)

	// Two networks, because interface discovery demands one address inside the sandbox subnet
	// and one outside it; the entrypoint logs the host gateway address only after that succeeds.
	subnets := []string{"10.225.91.0/24", "10.225.92.0/24"}
	networks := []string{container + "-sandbox", container + "-internet"}
	labels := resourceLabels(container, "itest-gateway")
	for i, name := range networks {
		_ = containerEngine.NetworkRemove(name)
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: name, Subnet: netip.MustParsePrefix(subnets[i]), Internal: i == 0, Labels: labels,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = containerEngine.NetworkRemove(name) })
	}
	// Registered after the networks so that LIFO cleanup removes the container first: Docker
	// refuses to remove a network while a container is still attached, and a network that
	// survives here leaks its subnet into every later test in this package.
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })

	startGatewayContainer(t, containerEngine, container, gatewayImage, confDir, subnets[0], networks,
		"host.internal:fd07:b51a:cc66:f0::fe", "host.internal:"+hostGatewayTestAddress)

	logs := waitForGatewayLog(t, containerEngine, container, "host gateway=")
	if !strings.Contains(logs, "host gateway="+hostGatewayTestAddress) {
		t.Errorf("the gateway did not use the IPv4 host gateway address; log was:\n%s", logs)
	}
}

// applyGateway is a running gateway with a base policy plus a peer container on its internet
// network. Docker's embedded DNS resolves the peer's name, so the tests can watch names resolve
// and sets fill without any internet access.
type applyGateway struct {
	engine    *engine.Engine
	container string
	peer      string
	instance  *state.Instance
}

func startApplyGateway(t *testing.T, name string, octet int, addHosts ...string) *applyGateway {
	t.Helper()
	containerEngine := testEngine(t)
	runTmpDir := t.TempDir()
	instance := &state.Instance{
		InstanceID: "itest0",
		BaseAllow:  []state.SourcedEntry{{Entry: "api.github.com:80,443", Source: SourceSettings}},
	}
	config, err := parseGatewayConfig(instance, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	confDir, err := writeGatewayArtifacts(runTmpDir, config.policy())
	if err != nil {
		t.Fatal(err)
	}
	gatewayImage := image.GatewayImage(assets.BuildInputsHash())
	if !containerEngine.ImageExists(gatewayImage) {
		if err := containerEngine.RunQuiet("build", "-t", gatewayImage, filepath.Join(runTmpDir, "gateway")); err != nil {
			t.Skipf("could not build the gateway image (the build needs network access): %v", err)
		}
	}

	container := "hole-sandbox-itest-apply-" + name
	gateway := &applyGateway{engine: containerEngine, container: container, peer: container + "-peer", instance: instance}
	subnets := []string{fmt.Sprintf("10.225.%d.0/24", octet), fmt.Sprintf("10.225.%d.0/24", octet+1)}
	networks := []string{container + "-sandbox", container + "-internet"}
	labels := resourceLabels(container, "itest-gateway")
	for i, network := range networks {
		_ = containerEngine.NetworkRemove(network)
		if err := containerEngine.NetworkCreate(engine.NetworkOptions{
			Name: network, Subnet: netip.MustParsePrefix(subnets[i]), Internal: i == 0, Labels: labels,
		}); err != nil {
			t.Fatalf("create %s: %v", network, err)
		}
		t.Cleanup(func() { _ = containerEngine.NetworkRemove(network) })
	}
	_ = containerEngine.ContainerRemove(gateway.peer)
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(gateway.peer) })
	if err := containerEngine.RunQuiet("run", "-d", "--name", gateway.peer, "--network", networks[1],
		"--label", engine.LabelManaged+"=true", "alpine:3.19", "sleep", "300"); err != nil {
		t.Skipf("could not start the peer container: %v", err)
	}
	_ = containerEngine.ContainerRemove(container)
	t.Cleanup(func() { _ = containerEngine.ContainerRemove(container) })
	startGatewayContainer(t, containerEngine, container, gatewayImage, confDir, subnets[0], networks, addHosts...)
	gateway.waitHealthy(t)
	return gateway
}

func (g *applyGateway) waitHealthy(t *testing.T) {
	t.Helper()
	for attempt := 0; attempt < 60; attempt++ {
		if out, _ := g.engine.Exec(g.container, "dig", "+short", "+time=1", "+tries=1", "@127.0.0.1", network.HealthZone, "A"); strings.TrimSpace(out) == "127.0.0.1" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := g.engine.ContainerLogs(g.container)
	t.Fatalf("the gateway never became healthy; log was:\n%s", logs)
}

// resolve returns the gateway's answer for a name, and whether the policy refused it.
func (g *applyGateway) resolve(t *testing.T, name string) (string, bool) {
	t.Helper()
	out, _ := g.engine.Exec(g.container, "dig", "+time=2", "+tries=1", "@127.0.0.1", name, "A")
	answer, _ := g.engine.Exec(g.container, "dig", "+short", "+time=2", "+tries=1", "@127.0.0.1", name, "A")
	return strings.TrimSpace(answer), strings.Contains(out, "status: NXDOMAIN")
}

func (g *applyGateway) table(t *testing.T) string {
	t.Helper()
	out, err := g.engine.Exec(g.container, "nft", "list", "table", "inet", "hole")
	if err != nil {
		t.Fatalf("nft list table: %v", err)
	}
	return out
}

// apply ships a change and returns the script's exit code; the instance's runtime lists follow a
// successful apply, as Apply would record them.
func (g *applyGateway) apply(t *testing.T, allow, hostGateway []string, tamper func(map[string]string)) (int, string) {
	t.Helper()
	payload, err := renderApplyPayload(g.instance, allow, hostGateway)
	if err != nil {
		t.Fatal(err)
	}
	if tamper != nil {
		payload = retar(t, payload, tamper)
	}
	_, stderr, code, err := g.engine.ExecInput(g.container, bytes.NewReader(payload), "hole-gateway-apply")
	if err != nil {
		t.Fatalf("exec hole-gateway-apply: %v", err)
	}
	if code == 0 {
		g.instance.RuntimeAllow, g.instance.RuntimeHostGatewayDomains = allow, hostGateway
	}
	return code, stderr
}

// retar rewrites files of an apply payload.
func retar(t *testing.T, payload []byte, tamper func(map[string]string)) []byte {
	t.Helper()
	files := map[string]string{}
	var order []string
	reader := tar.NewReader(bytes.NewReader(payload))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		data, _ := io.ReadAll(reader)
		files[header.Name] = string(data)
		order = append(order, header.Name)
	}
	tamper(files)
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	_ = writer.WriteHeader(&tar.Header{Name: "previous/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, name := range order {
		_ = writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))})
		_, _ = writer.Write([]byte(files[name]))
	}
	_ = writer.Close()
	return buf.Bytes()
}

// TestGatewayRuntimeApply walks one runtime entry through its whole life on a real gateway: added,
// resolving and recorded in its own set while base sets keep their elements, surviving a gateway
// restart, kept through a rejected payload, then removed with its set.
func TestGatewayRuntimeApply(t *testing.T) {
	gateway := startApplyGateway(t, "lifecycle", 93)

	if _, refused := gateway.resolve(t, gateway.peer); !refused {
		t.Fatal("the peer resolves before it is allowed")
	}
	if code, stderr := gateway.apply(t, []string{gateway.peer + ":80,443"}, nil, nil); code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	answer, refused := gateway.resolve(t, gateway.peer)
	if refused || answer == "" {
		t.Fatalf("the allowed peer does not resolve (answer %q, refused %v)", answer, refused)
	}
	if table := gateway.table(t); !strings.Contains(table, answer) || !strings.Contains(table, "set r_") {
		t.Errorf("the peer's address %s is not in a runtime set:\n%s", answer, table)
	}

	if err := gateway.engine.RunQuiet("restart", gateway.container); err != nil {
		t.Fatal(err)
	}
	gateway.waitHealthy(t)
	if _, refused := gateway.resolve(t, gateway.peer); refused {
		t.Error("the runtime entry did not survive a gateway restart")
	}

	code, _ := gateway.apply(t, []string{gateway.peer + ":80,443", "other.example:80,443"}, nil, func(files map[string]string) {
		files["final.nft"] = "this is not nft\n"
	})
	if code != 11 {
		t.Errorf("a broken payload exit = %d, want 11", code)
	}
	if _, refused := gateway.resolve(t, gateway.peer); refused {
		t.Error("a rejected payload changed the policy in force")
	}

	if code, stderr := gateway.apply(t, nil, nil, nil); code != 0 {
		t.Fatalf("remove exit %d: %s", code, stderr)
	}
	if _, refused := gateway.resolve(t, gateway.peer); !refused {
		t.Error("the removed entry still resolves")
	}
	if table := gateway.table(t); strings.Contains(table, "set r_") {
		t.Errorf("the removed entry's set is still there:\n%s", table)
	}
}

// A Corefile CoreDNS refuses to load must be rolled back, with the previous generation served.
func TestGatewayRuntimeApplyRollsBackCorefile(t *testing.T) {
	gateway := startApplyGateway(t, "rollback", 95)
	before, _ := gateway.engine.Exec(gateway.container, "dig", "+short", "TXT", network.HealthZone, "@127.0.0.1")

	code, _ := gateway.apply(t, []string{gateway.peer + ":80,443"}, nil, func(files map[string]string) {
		files["Corefile"] += "\nbroken {\n    no-such-plugin\n}\n"
	})
	if code != 12 {
		t.Fatalf("an unloadable Corefile exit = %d, want 12", code)
	}
	after, _ := gateway.engine.Exec(gateway.container, "dig", "+short", "TXT", network.HealthZone, "@127.0.0.1")
	if strings.TrimSpace(before) == "" || before != after {
		t.Errorf("generation after rollback = %q, want %q", after, before)
	}
	if _, refused := gateway.resolve(t, gateway.peer); !refused {
		t.Error("the rolled-back entry resolves")
	}
}

// A second static entry in the same port group is a new content-addressed set; the old one goes.
func TestGatewayRuntimeApplyStatics(t *testing.T) {
	gateway := startApplyGateway(t, "statics", 97)
	if code, stderr := gateway.apply(t, []string{"203.0.113.1:443"}, nil, nil); code != 0 {
		t.Fatalf("first static exit %d: %s", code, stderr)
	}
	first := gateway.table(t)
	if code, stderr := gateway.apply(t, []string{"203.0.113.1:443", "203.0.113.2:443"}, nil, nil); code != 0 {
		t.Fatalf("second static exit %d: %s", code, stderr)
	}
	second := gateway.table(t)
	if !strings.Contains(second, "203.0.113.2") || strings.Count(second, "_static {") != 1 {
		t.Errorf("after the second static entry:\n%s\nbefore:\n%s", second, first)
	}
}

func TestGatewayRuntimeHostGateway(t *testing.T) {
	gateway := startApplyGateway(t, "hostgw", 99, "host.internal:"+hostGatewayTestAddress)
	if code, stderr := gateway.apply(t, nil, []string{"app.test:8080"}, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if answer, _ := gateway.resolve(t, "app.test"); answer != hostGatewayTestAddress {
		t.Errorf("app.test resolves to %q, want %s", answer, hostGatewayTestAddress)
	}
	if table := gateway.table(t); !strings.Contains(table, "ip daddr "+hostGatewayTestAddress+" tcp dport 8080 accept") &&
		!strings.Contains(table, "ip daddr "+hostGatewayTestAddress+" tcp dport { 8080 } accept") {
		t.Errorf("the host gateway port is not open:\n%s", table)
	}

	loopback := startApplyGateway(t, "hostgw-loop", 101, "host.internal:127.0.0.1")
	if code, _ := loopback.apply(t, nil, []string{"app.test:8080"}, nil); code != 10 {
		t.Errorf("no usable host gateway address: exit %d, want 10", code)
	}
}
