package sandbox

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lukashornych/hole/v2/assets"
	"github.com/lukashornych/hole/v2/internal/agents"
	"github.com/lukashornych/hole/v2/internal/agentversion"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/engine"
	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/logging"
)

const (
	// agentUpdateRepository holds the temporary references of an update build:
	// `<instanceID>` for the result and `<instanceID>-base` for the image it starts from. It is a
	// repository of its own because the image GC of a concurrent start removes every other tag of
	// the agent repositories; project repositories always end in an 8-hex hash, so the name
	// cannot collide with one.
	agentUpdateRepository = "hole-sandbox/agent-update"
	// labelCleanSize is the `.Size` of an image before its first in-place update.
	labelCleanSize = "com.hole.clean-size"
	// labelAgentUpdates counts the in-place updates since the clean build.
	labelAgentUpdates = "com.hole.agent-updates"
	// growthRatioLimit is the size, relative to the clean build, at which a clean rebuild is
	// suggested: every update keeps the old CLI in a lower layer.
	growthRatioLimit = 2.0
	// agentUpdateLimit suggests a clean rebuild regardless of size, well below the overlay
	// layer limit — each update adds two layers.
	agentUpdateLimit = 40
	// versionMarker precedes each agent's section in the output of the version probe.
	versionMarker = "@@hole-agent-version "
	// updateDockerfile is the embedded Dockerfile of the update build.
	updateDockerfile = "Dockerfile.update"
)

// imageGeneration is how far an image has grown through in-place agent updates.
type imageGeneration struct {
	size      int64
	cleanSize int64
	updates   int
}

// generationFromLabels reads the update labels. An image without them is a clean build: zero
// updates, and its own size is the clean size.
func generationFromLabels(size int64, labels map[string]string) imageGeneration {
	generation := imageGeneration{size: size, cleanSize: size}
	if cleanSize, err := strconv.ParseInt(labels[labelCleanSize], 10, 64); err == nil && cleanSize > 0 {
		generation.cleanSize = cleanSize
	}
	if updates, err := strconv.Atoi(labels[labelAgentUpdates]); err == nil && updates > 0 {
		generation.updates = updates
	}
	return generation
}

func (g imageGeneration) ratio() float64 {
	if g.cleanSize <= 0 {
		return 1
	}
	return float64(g.size) / float64(g.cleanSize)
}

// needsCleanRebuild reports whether the image has grown enough that a clean rebuild should be
// suggested before another update.
func (g imageGeneration) needsCleanRebuild() bool {
	return g.updates >= agentUpdateLimit || g.ratio() >= growthRatioLimit
}

// describe renders the generation for the start banner, or "" for a clean image.
func (g imageGeneration) describe() string {
	if g.updates == 0 {
		return ""
	}
	noun := "updates"
	if g.updates == 1 {
		noun = "update"
	}
	return fmt.Sprintf("%d agent %s since clean build (%s → %s, %.2f×)",
		g.updates, noun, formatSize(g.cleanSize), formatSize(g.size), g.ratio())
}

// formatSize renders bytes in decimal units, the way the runtimes print image sizes.
func formatSize(bytes int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	value := float64(bytes)
	unit := 0
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	text := strconv.FormatFloat(value, 'f', 2, 64)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	return text + " " + units[unit]
}

// versionedAgent is an enabled agent with a valid version.json.
type versionedAgent struct {
	agent *agents.Agent
	spec  *agents.VersionSpec
}

// agentUpdater runs the start's version check and in-place agent updates. Nothing it does fails
// the start: every problem is a warning, and the current image stays in use.
type agentUpdater struct {
	engine     *engine.Engine
	host       hostenv.Host
	reference  string
	runTmpDir  string
	instanceID string
	// instanceName names the version-probe container `<instanceName>-versionprobe-1`, the shape
	// compose gives its containers, so the container GC sweeps one left behind.
	instanceName string
	enabled      []*agents.Agent
	interactive  bool
	in           io.Reader
	out          io.Writer
	// interrupted reports a signal received since the start began. A prompt keeps blocking on
	// its read after Ctrl-C, so the answer that follows must not be taken. Nil means never.
	interrupted func() bool
	// statePath is the agent version state file on the host.
	statePath string
	// image is the agent image as read at the start, valid when imageFound. The update path reads
	// it again: the prompt can block for minutes while a sibling start retags the reference.
	image      engine.ImageInfo
	imageFound bool

	state     *agentversion.State
	versioned []versionedAgent
	resolved  bool
}

// prepare runs the version check and any update it or --update-agents asked for, and reports
// whether the user chose a clean rebuild instead.
func (u *agentUpdater) prepare(rebuild, updateRequested, checkEnabled bool) bool {
	if rebuild {
		if updateRequested {
			logging.Info("Agent update skipped: the image is rebuilt cleanly anyway")
		}
		return false
	}
	if !u.imageFound {
		if updateRequested {
			logging.Info("Nothing to update: there is no agent image yet, so a fresh one is built")
		}
		return false
	}
	defer u.saveState()

	var selection []*agents.Agent
	switch {
	case updateRequested:
		selection = u.updatableAgents(true)
		if len(selection) == 0 {
			logging.Warn("no enabled agent has an update script; rebuild with -r to refresh them")
		}
	case checkEnabled:
		selection = u.check()
	}
	if len(selection) == 0 {
		return false
	}

	current, found, err := u.engine.ImageInspect(u.reference)
	if !found {
		logging.Warn("agent update skipped: the agent image %s is gone", u.reference)
		return false
	}
	if err != nil {
		logging.Warn("agent update skipped: could not read the agent image %s: %v", u.reference, err)
		return false
	}
	generation := generationFromLabels(current.Size, current.Labels)
	if generation.needsCleanRebuild() && u.askCleanRebuild(generation) {
		return true
	}
	u.update(selection, current.ID, generation)
	return false
}

// askCleanRebuild offers a clean rebuild once the image has grown past the growth guard. Without
// a terminal the answer is no: a rebuild re-runs setup hooks and needs the network for every
// installer, which nobody agreed to.
func (u *agentUpdater) askCleanRebuild(generation imageGeneration) bool {
	message := fmt.Sprintf("Agent image grew from %s to %s (%.1f×) over %d agent updates since the last clean build.",
		formatSize(generation.cleanSize), formatSize(generation.size), generation.ratio(), generation.updates)
	if !u.interactive {
		logging.Warn("%s Rebuild it cleanly with -r (also re-runs setup hooks); updating in place for now.", message)
		return false
	}
	_, _ = fmt.Fprintf(u.out, "%s\nRebuild cleanly now (also re-runs setup hooks)? [Y/n] ", message)
	answer, err := bufio.NewReader(u.in).ReadString('\n')
	if (err != nil && answer == "") || u.isInterrupted() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

// check compares the installed agent versions with the latest releases and asks whether to
// update. It returns the agents to update, if any.
func (u *agentUpdater) check() []*agents.Agent {
	versioned := u.versionedAgents()
	if len(versioned) == 0 {
		return nil
	}
	installed := u.installedVersions(u.image.ID)

	state := u.loadState()
	sources := map[string]agents.LatestSource{}
	for _, entry := range versioned {
		sources[entry.agent.Name] = entry.spec.Latest
	}
	state.RefreshLatest(sources, time.Now(), agentversion.FetchLatest)

	// Every enabled agent with a version.json also has an update script (agents.CheckUpdateFiles).
	byName := map[string]*agents.Agent{}
	candidates := make([]agentversion.Candidate, 0, len(versioned))
	for _, entry := range versioned {
		byName[entry.agent.Name] = entry.agent
		candidates = append(candidates, agentversion.Candidate{
			Agent:     entry.agent.Name,
			Installed: installed[entry.agent.Name],
		})
	}
	evaluation := state.Evaluate(candidates)

	if len(evaluation.Offers) == 0 {
		return nil
	}
	if !u.interactive {
		logging.Info("Agent updates available: %s — start with --update-agents to install them",
			agentversion.DescribeOffers(evaluation.Offers))
		return nil
	}
	answer := agentversion.Ask(u.in, u.out, evaluation.Offers)
	if u.isInterrupted() {
		return nil
	}
	switch answer {
	case agentversion.AnswerYes:
		selection := make([]*agents.Agent, 0, len(evaluation.Offers))
		for _, offer := range evaluation.Offers {
			selection = append(selection, byName[offer.Agent])
		}
		return selection
	case agentversion.AnswerSkip:
		state.Skip(evaluation.Offers)
		logging.Info("Skipping %s until a newer release appears", agentversion.DescribeOffers(evaluation.Offers))
	}
	return nil
}

// update builds the update layer for the selected agents on top of the current image, validates
// it, and moves the image tag only when every agent still answers with a version and at least
// one version changed.
func (u *agentUpdater) update(selection []*agents.Agent, currentID string, generation imageGeneration) {
	before := u.installedVersions(currentID)

	if u.engine.IsDocker() {
		// Only the docker driver builds against the daemon's own image store; any other would try
		// to pull the base tag. The builder is not switched behind the user's back.
		builder, err := u.engine.Builder()
		if err != nil {
			logging.Warn("agent update skipped: could not read the active buildx builder (%v); "+
				"install the docker buildx plugin or rebuild with -r", err)
			return
		}
		if builder.Driver != "docker" {
			logging.Warn("agent update skipped: the active buildx builder '%s' uses the '%s' driver, which cannot "+
				"build on a local image; select a builder with the docker driver ('docker buildx use <name>') "+
				"or rebuild with -r", builder.Name, builder.Driver)
			return
		}
	}

	contextDir, err := materializeUpdateContext(u.runTmpDir, selection)
	if err != nil {
		logging.Warn("agent update skipped: %v", err)
		return
	}

	temporary := agentUpdateRepository + ":" + u.instanceID
	base := temporary + "-base"
	defer removeUpdateReferences(u.engine, temporary, base)
	// Tagging the ID that was read pins exactly that image: a concurrent retag of the real
	// reference cannot slip in between, and BuildKit does not accept an ID in FROM.
	if err := u.engine.ImageTag(currentID, base); err != nil {
		logging.Warn("agent update skipped: could not tag the current image: %v", err)
		return
	}

	if u.isInterrupted() {
		return
	}
	logging.Info("Updating agents: %s...", strings.Join(agents.EnabledNames(selection), ", "))
	// Without the cache: after a no-op or a rejected update the base and the scripts are the
	// same as last time, and a cache hit would never fetch a newer release again.
	err = u.engine.ImageBuild(contextDir, updateDockerfile, temporary, map[string]string{
		"BASE":           base,
		"AGENT_USERNAME": u.host.Username,
		"CLEAN_SIZE":     strconv.FormatInt(generation.cleanSize, 10),
		"AGENT_UPDATES":  strconv.Itoa(generation.updates + 1),
	}, true)
	if err != nil {
		logging.Warn("agent update failed, keeping the current image: %v", err)
		return
	}

	// Every enabled agent is validated, not just the updated ones: an update of one must not
	// break another.
	after, failures, err := u.probeVersions(temporary)
	if err != nil {
		logging.Warn("could not verify the updated agents, keeping the current image: %v", err)
		return
	}
	if len(failures) > 0 {
		for _, name := range config.SortedKeys(failures) {
			logging.Warn("agent '%s' does not report a version after the update: %s", name, failures[name])
		}
		logging.Warn("agent update rejected, keeping the current image")
		return
	}
	if len(u.versionedAgents()) > 0 && sameVersions(before, after) {
		logging.Info("Agents are already up to date (%s)", describeVersions(after))
		return
	}

	updatedID, err := u.engine.ImageID(temporary)
	if err != nil {
		logging.Warn("agent update failed, keeping the current image: %v", err)
		return
	}
	if err := u.engine.ImageTag(temporary, u.reference); err != nil {
		logging.Warn("agent update failed, keeping the current image: could not tag %s: %v", u.reference, err)
		return
	}
	state := u.loadState()
	state.RecordInstalled(updatedID, after)
	state.ClearSatisfiedSkips(after)

	logging.Info("Agents updated: %s", describeChanges(before, after))
	if updated, found, err := u.engine.ImageInspect(u.reference); found && err == nil {
		logging.Info("Agent image: %s — %s", u.reference, generationFromLabels(updated.Size, updated.Labels).describe())
	}
}

// installedVersions returns the agent versions of an image, from the state cache or, on a miss,
// from one throwaway container. Only a complete probe is cached: an agent missing from the cache
// entry would never be offered an update for that image. The probe runs the ID, not the
// reference: a concurrent update or rebuild may retag the reference to a different image.
func (u *agentUpdater) installedVersions(imageID string) map[string]string {
	state := u.loadState()
	if versions, ok := state.Installed(imageID); ok {
		return versions
	}
	versions, failures, err := u.probeVersions(imageID)
	if err != nil {
		logging.Debug("could not read the agent versions of %s (%s): %v", u.reference, imageID, err)
		return map[string]string{}
	}
	for _, name := range config.SortedKeys(failures) {
		logging.Warn("agent '%s' reports no version, so its update check is skipped: %s", name, failures[name])
	}
	if len(failures) == 0 {
		state.RecordInstalled(imageID, versions)
	}
	return versions
}

// probeVersions runs every version.json command in one throwaway container. It returns the
// versions found and, per agent, the output of a command that printed none.
func (u *agentUpdater) probeVersions(reference string) (map[string]string, map[string]string, error) {
	versioned := u.versionedAgents()
	versions := map[string]string{}
	failures := map[string]string{}
	if len(versioned) == 0 {
		return versions, failures, nil
	}
	out, err := u.engine.RunThrowaway(u.instanceName+"-versionprobe-1", reference, "bash", "-c", versionProbeScript(u.host, versioned))
	if err != nil {
		return nil, nil, err
	}
	outputs := splitProbeOutput(out)
	for _, entry := range versioned {
		output := outputs[entry.agent.Name]
		if parsed := agentversion.ParseVersion(output); parsed != "" {
			versions[entry.agent.Name] = parsed
		} else {
			failures[entry.agent.Name] = strings.TrimSpace(output)
		}
	}
	return versions, failures, nil
}

// versionProbeScript prints a marker per agent followed by its version command's output. A
// failing command does not stop the script: its missing version is the failure.
func versionProbeScript(host hostenv.Host, versioned []versionedAgent) string {
	var script strings.Builder
	for _, entry := range versioned {
		parts := make([]string, 0, len(entry.spec.Command))
		for _, part := range entry.spec.Command {
			parts = append(parts, shellQuote(expandContainerValue(part, host)))
		}
		fmt.Fprintf(&script, "echo %s\n%s 2>&1 </dev/null\n",
			shellQuote(versionMarker+entry.agent.Name), strings.Join(parts, " "))
	}
	// The last command's status must not become the script's: a container that ran is a probe
	// that worked, whatever each agent answered.
	script.WriteString("exit 0\n")
	return script.String()
}

// splitProbeOutput maps each agent to the output between its marker and the next one.
func splitProbeOutput(out string) map[string]string {
	outputs := map[string]string{}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		if name, found := strings.CutPrefix(line, versionMarker); found {
			current = strings.TrimSpace(name)
			outputs[current] = ""
			continue
		}
		if current != "" {
			outputs[current] += line + "\n"
		}
	}
	return outputs
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// versionedAgents lists the enabled agents with a valid version.json. A malformed one is the
// agent author's problem and only costs that agent its version check.
func (u *agentUpdater) versionedAgents() []versionedAgent {
	if u.resolved {
		return u.versioned
	}
	u.resolved = true
	for _, agent := range u.enabled {
		spec, err := agent.VersionSpec()
		if err != nil {
			logging.Warn("%v; its version is not checked", err)
			continue
		}
		if spec != nil {
			u.versioned = append(u.versioned, versionedAgent{agent: agent, spec: spec})
		}
	}
	return u.versioned
}

// updatableAgents lists the enabled agents with an update script, optionally warning about each
// one without.
func (u *agentUpdater) updatableAgents(warn bool) []*agents.Agent {
	var updatable []*agents.Agent
	for _, agent := range u.enabled {
		scripts, err := agent.UpdateScripts()
		if err != nil {
			logging.Warn("%v; the agent is not updated", err)
			continue
		}
		if len(scripts) == 0 {
			if warn {
				logging.Warn("agent '%s' has no update script and is not updated; rebuild with -r to refresh it", agent.Name)
			}
			continue
		}
		updatable = append(updatable, agent)
	}
	return updatable
}

func (u *agentUpdater) isInterrupted() bool {
	return u.interrupted != nil && u.interrupted()
}

func (u *agentUpdater) loadState() *agentversion.State {
	if u.state == nil {
		u.state = agentversion.Load(u.statePath)
	}
	return u.state
}

func (u *agentUpdater) saveState() {
	if u.state == nil {
		return
	}
	if err := u.state.Save(func(id string) bool { return u.engine.ImageExists(id) }); err != nil {
		logging.Warn("could not save the agent version state: %v", err)
	}
}

// materializeUpdateContext writes the update build context: the embedded Dockerfile.update and
// the update scripts of the selected agents only.
func materializeUpdateContext(runTmpDir string, selection []*agents.Agent) (string, error) {
	contextDir := filepath.Join(runTmpDir, "agent-update")
	if err := os.RemoveAll(contextDir); err != nil {
		return "", fmt.Errorf("clear update build context: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(contextDir, "agent-updates"), 0o755); err != nil {
		return "", fmt.Errorf("create update build context: %w", err)
	}
	dockerfile, err := fs.ReadFile(assets.Agents(), updateDockerfile)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", updateDockerfile, err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, updateDockerfile), dockerfile, 0o644); err != nil {
		return "", fmt.Errorf("write %s into the update build context: %w", updateDockerfile, err)
	}
	for _, agent := range selection {
		scripts, err := agent.UpdateScripts()
		if err != nil {
			return "", err
		}
		agentDir := filepath.Join(contextDir, "agent-updates", agent.Name)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			return "", fmt.Errorf("create update build context for agent '%s': %w", agent.Name, err)
		}
		for name, data := range scripts {
			if err := os.WriteFile(filepath.Join(agentDir, name), data, 0o755); err != nil {
				return "", fmt.Errorf("write update script for agent '%s': %w", agent.Name, err)
			}
		}
	}
	return contextDir, nil
}

// removeUpdateReferences drops the temporary references of an update build. The base tag may be
// the last name of an image a running sandbox still uses, hence an untag rather than a removal.
func removeUpdateReferences(containerEngine *engine.Engine, references ...string) {
	for _, reference := range references {
		if !containerEngine.ImageExists(reference) {
			continue
		}
		if err := containerEngine.ImageUntag(reference); err != nil {
			logging.Warn("could not remove the temporary image reference %s: %v", reference, err)
		}
	}
}

// collectUpdateReferences removes temporary update references whose instance is no longer
// registered — leftovers of a start that was killed mid-update.
func collectUpdateReferences(containerEngine *engine.Engine, liveInstanceIDs map[string]bool) {
	for _, reference := range containerEngine.ImagesByReference(agentUpdateRepository) {
		_, tag := engine.SplitReference(reference)
		if tag == "" || liveInstanceIDs[strings.TrimSuffix(tag, "-base")] {
			continue
		}
		logging.Debug("removing leftover update reference %s", reference)
		if err := containerEngine.ImageUntag(reference); err != nil {
			logging.Warn("could not remove leftover image reference %s: %v", reference, err)
		}
	}
}

// sameVersions reports whether an update changed nothing. An agent whose version was unknown
// before counts as changed.
func sameVersions(before, after map[string]string) bool {
	for name, version := range after {
		if before[name] != version {
			return false
		}
	}
	return true
}

func describeVersions(versions map[string]string) string {
	parts := make([]string, 0, len(versions))
	for _, name := range config.SortedKeys(versions) {
		parts = append(parts, name+" "+versions[name])
	}
	return strings.Join(parts, ", ")
}

// describeChanges renders `claude 2.1.282 → 2.1.296, codex 0.162.1 (unchanged)`.
func describeChanges(before, after map[string]string) string {
	if len(after) == 0 {
		return "versions unknown (no agent has a version.json)"
	}
	parts := make([]string, 0, len(after))
	for _, name := range config.SortedKeys(after) {
		switch previous := before[name]; previous {
		case after[name]:
			parts = append(parts, fmt.Sprintf("%s %s (unchanged)", name, after[name]))
		case "":
			parts = append(parts, fmt.Sprintf("%s %s", name, after[name]))
		default:
			parts = append(parts, fmt.Sprintf("%s %s → %s", name, previous, after[name]))
		}
	}
	return strings.Join(parts, ", ")
}
