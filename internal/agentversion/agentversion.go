// Package agentversion decides when an agent CLI in the sandbox image is out of date: it parses
// installed versions, fetches the latest ones over plain HTTP from the host, and keeps the state
// that throttles those fetches and remembers declined updates.
//
// Nothing here runs a program. The host is not assumed to have npm, node, curl or any other tool,
// and no agent-provided script may ever run on it, so a latest version is learnt only from an
// HTTP response.
package agentversion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lukashornych/hole/v2/internal/agents"
	"github.com/lukashornych/hole/v2/internal/config"
	"github.com/lukashornych/hole/v2/internal/logging"
	"github.com/lukashornych/hole/v2/internal/version"
)

const (
	// FileName is the state file under ~/.hole.
	FileName = "agent-versions.json"
	// fetchInterval throttles latest-version fetches per agent. It does not protect the servers —
	// the requests are tiny — but bounds how often a slow or offline network costs fetchTimeout.
	fetchInterval = 4 * time.Hour
	// fetchTimeout bounds each agent's fetch; the fetches run in parallel, so a start waits at most
	// about this long.
	fetchTimeout = time.Second
	// maxResponseSize caps what is read from a version endpoint; a version is a few bytes and the
	// npm `latest` document a few kilobytes.
	maxResponseSize = 1 << 20
)

// npmRegistry is the npm registry base URL; tests point it at a local server.
var npmRegistry = "https://registry.npmjs.org"

var (
	versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)
	latestPattern  = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
)

// ParseVersion returns the first `x.y.z` in a version command's output (`2.1.296 (Claude Code)`,
// `codex-cli 0.162.1`), or "" when there is none.
func ParseVersion(output string) string {
	return versionPattern.FindString(output)
}

// AgentState is what is remembered about one agent across starts.
type AgentState struct {
	// Latest is the last successfully fetched latest version.
	Latest string `json:"latest,omitempty"`
	// FetchedAt is the last successful fetch. A failed one is not recorded, so it is retried on the
	// next start.
	FetchedAt time.Time `json:"fetchedAt"`
	// Skipped is the latest version the user declined with `s`.
	Skipped string `json:"skipped,omitempty"`
}

// State is the content of the state file.
type State struct {
	// Images maps an image ID to the agent versions installed in it.
	Images map[string]map[string]string `json:"images"`
	Agents map[string]*AgentState       `json:"agents"`

	path string
	// loaded is the encoding of the state as loaded, so Save can tell that nothing changed.
	loaded []byte
}

// Load reads the state file. A missing or unreadable file is an empty state: everything in it can
// be learnt again.
func Load(path string) *State {
	state := &State{path: path}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			logging.Debug("ignoring unreadable %s: %v", path, err)
			state = &State{path: path}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		logging.Debug("could not read %s: %v", path, err)
	}
	if state.Images == nil {
		state.Images = map[string]map[string]string{}
	}
	if state.Agents == nil {
		state.Agents = map[string]*AgentState{}
	}
	state.loaded, _ = state.encode()
	return state
}

func (s *State) encode() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// Save writes the state file, dropping the entries of images that no longer exist. A state that
// has not changed since Load is neither pruned nor written — pruning costs one image inspect per
// cached image — so the entries of removed images wait for the next write.
//
// Concurrent starts write the same file, so it goes through a uniquely named temporary file and a
// rename: a reader never sees half a file, and a lost concurrent update only costs a re-fetch or
// a re-probe.
func (s *State) Save(imageExists func(id string) bool) error {
	if current, err := s.encode(); err == nil && s.loaded != nil && bytes.Equal(current, s.loaded) {
		return nil
	}
	for id := range s.Images {
		if !imageExists(id) {
			delete(s.Images, id)
		}
	}
	data, err := s.encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.path, err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.path), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	if err := os.Rename(temp.Name(), s.path); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	s.loaded = data
	return nil
}

// Installed returns the cached agent versions of an image.
func (s *State) Installed(imageID string) (map[string]string, bool) {
	versions, ok := s.Images[imageID]
	return versions, ok
}

// RecordInstalled caches the agent versions of an image.
func (s *State) RecordInstalled(imageID string, versions map[string]string) {
	s.Images[imageID] = versions
}

func (s *State) agent(name string) *AgentState {
	if s.Agents[name] == nil {
		s.Agents[name] = &AgentState{}
	}
	return s.Agents[name]
}

// Fetcher looks up the latest version from one source.
type Fetcher func(ctx context.Context, source agents.LatestSource) (string, error)

// RefreshLatest fetches the latest version of every agent whose last attempt is older than the
// fetch interval, in parallel, each within its own timeout. A failed fetch is not recorded: the
// next start tries again, and the previously known latest version stays, so a known update keeps
// being offered offline.
func (s *State) RefreshLatest(sources map[string]agents.LatestSource, now time.Time, fetch Fetcher) {
	var group sync.WaitGroup
	for _, name := range config.SortedKeys(sources) {
		agentState := s.agent(name)
		// A timestamp in the future (a clock that was wrong) must not suppress fetches for good.
		if elapsed := now.Sub(agentState.FetchedAt); elapsed >= 0 && elapsed < fetchInterval {
			continue
		}
		group.Add(1)
		go func(name string, agentState *AgentState, source agents.LatestSource) {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()
			latest, err := fetch(ctx, source)
			if err != nil {
				logging.Debug("latest version of agent '%s' not fetched: %v", name, err)
				return
			}
			agentState.Latest = latest
			agentState.FetchedAt = now
		}(name, agentState, sources[name])
	}
	group.Wait()
}

// FetchLatest reads the latest version from an npm package's `latest` dist-tag or from a URL
// serving plain text or `{"version": "…"}`. Anything that does not look like a version is
// rejected, so an HTML error page can never become "latest".
func FetchLatest(ctx context.Context, source agents.LatestSource) (string, error) {
	target := source.URL
	if source.NPM != "" {
		target = npmRegistry + "/" + source.NPM + "/latest"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", target, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", target, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", target, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", target, err)
	}
	return parseLatest(body)
}

// parseLatest accepts a JSON document with a `version` field or a bare version string.
func parseLatest(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	var document struct {
		Version string `json:"version"`
	}
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &document); err != nil {
			return "", fmt.Errorf("parse version document: %w", err)
		}
		text = strings.TrimSpace(document.Version)
	}
	if !latestPattern.MatchString(text) {
		return "", fmt.Errorf("response is not a version: %.40q", text)
	}
	return strings.TrimPrefix(text, "v"), nil
}

// Candidate is one enabled agent whose installed version is known.
type Candidate struct {
	Agent     string
	Installed string
}

// Offer is a newer version of one agent.
type Offer struct {
	Agent     string
	Installed string
	Latest    string
}

// Evaluation is what a version check found.
type Evaluation struct {
	// Offers are updates the user can install in place.
	Offers []Offer
}

// Evaluate compares installed and latest versions. An agent whose newest version the user skipped
// stays quiet until an even newer one appears — compared, not matched, so a dist-tag rolled back
// below the skipped version does not re-offer an older release than the one declined.
func (s *State) Evaluate(candidates []Candidate) Evaluation {
	var evaluation Evaluation
	for _, candidate := range candidates {
		agentState := s.Agents[candidate.Agent]
		if agentState == nil || agentState.Latest == "" || candidate.Installed == "" {
			continue
		}
		if !version.GreaterThan(agentState.Latest, candidate.Installed) {
			continue
		}
		if agentState.Skipped != "" && !version.GreaterThan(agentState.Latest, agentState.Skipped) {
			continue
		}
		evaluation.Offers = append(evaluation.Offers,
			Offer{Agent: candidate.Agent, Installed: candidate.Installed, Latest: agentState.Latest})
	}
	return evaluation
}

// Skip records the offered versions as declined.
func (s *State) Skip(offers []Offer) {
	for _, offer := range offers {
		s.agent(offer.Agent).Skipped = offer.Latest
	}
}

// ClearSatisfiedSkips forgets a skip once the installed version reached the skipped one.
func (s *State) ClearSatisfiedSkips(installed map[string]string) {
	for name, agentState := range s.Agents {
		current, ok := installed[name]
		if !ok || current == "" || agentState.Skipped == "" {
			continue
		}
		if !version.GreaterThan(agentState.Skipped, current) {
			agentState.Skipped = ""
		}
	}
}

// DescribeOffers renders offers as `claude 2.1.282 → 2.1.296, codex 0.160.0 → 0.162.1`.
func DescribeOffers(offers []Offer) string {
	parts := make([]string, 0, len(offers))
	for _, offer := range offers {
		parts = append(parts, fmt.Sprintf("%s %s → %s", offer.Agent, offer.Installed, offer.Latest))
	}
	return strings.Join(parts, ", ")
}

// Answer is the reply to the update prompt.
type Answer int

const (
	// AnswerNo starts unchanged and asks again next time.
	AnswerNo Answer = iota
	// AnswerYes updates the offered agents.
	AnswerYes
	// AnswerSkip declines the offered versions for good.
	AnswerSkip
)

// Ask prompts for the offered updates. An empty reply is a yes; anything unrecognised is a no,
// because a typo must not trigger an image build.
func Ask(in io.Reader, out io.Writer, offers []Offer) Answer {
	_, _ = fmt.Fprintf(out, "Agent updates available: %s\n", DescribeOffers(offers))
	_, _ = fmt.Fprint(out, "Update agents now? [Y/n/s = skip these versions] ")
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && answer == "" {
		return AnswerNo
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return AnswerYes
	case "s", "skip":
		return AnswerSkip
	default:
		return AnswerNo
	}
}
