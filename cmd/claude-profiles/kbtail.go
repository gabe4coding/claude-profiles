package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// cmdKbTail is the Monitor `command` for the kb-curator profile. It polls
// (a) the on-disk transcript jsonl files for the cwd (one per Claude Code
// session) detecting assistant turns that end with stop_reason=end_turn,
// and (b) `git log` of the cwd repo detecting new commits. On each new
// event it writes a small JSON file under <repo>/.kb/inbox/ and emits one
// short stdout line, which Claude Code delivers to the curator agent as
// a notification (waking it up to drain the inbox).
//
// Invariants:
//   - never writes to stdout except the one-line ping per event
//   - never crashes on transient errors (only stderr + continue)
//   - on first run records current state without emitting historical events
//   - inbox state survives restart via .kb/inbox/.kb-tail-state.json
func cmdKbTail(args []string) {
	// --self-agent <name>: name(s) of agent(s) whose transcripts kb-tail
	// must ignore. The kb-curator profile passes "kb-curator" so the
	// curator's own end_turn events never feed back into the inbox.
	// Without this, the curator would loop-curate itself.
	selfAgents := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--self-agent" && i+1 < len(args) {
			selfAgents[args[i+1]] = true
			i++
		}
	}

	repoRoot, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kb-tail: not in a git repo (%v)\n", err)
		os.Exit(1)
	}

	kbDir := filepath.Join(repoRoot, ".kb")
	inboxDir := filepath.Join(kbDir, "inbox")
	processedDir := filepath.Join(inboxDir, "processed")
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "kb-tail: mkdir %s: %v\n", processedDir, err)
		os.Exit(1)
	}

	transcriptsDir := transcriptsDirFor(repoRoot)
	statePath := filepath.Join(inboxDir, ".kb-tail-state.json")
	state := loadKbTailState(statePath)
	firstScan := state.LastSeenSHA == "" && len(state.Offsets) == 0

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	excludeMsg := "(no self-agent filter)"
	if len(selfAgents) > 0 {
		names := make([]string, 0, len(selfAgents))
		for k := range selfAgents {
			names = append(names, k)
		}
		sort.Strings(names)
		excludeMsg = "self-agents=" + strings.Join(names, ",")
	}
	fmt.Fprintf(os.Stderr, "kb-tail: repo=%s transcripts=%s (firstScan=%v) %s\n",
		repoRoot, transcriptsDir, firstScan, excludeMsg)

	interval := 2 * time.Second
	for {
		kbTailTick(transcriptsDir, inboxDir, repoRoot, &state, firstScan, selfAgents)
		firstScan = false
		if err := saveKbTailState(statePath, state); err != nil {
			fmt.Fprintf(os.Stderr, "kb-tail: save state: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func kbTailTick(transcriptsDir, inboxDir, repoRoot string, state *kbTailState, firstScan bool, selfAgents map[string]bool) {
	// 1. Transcripts
	if entries, err := os.ReadDir(transcriptsDir); err == nil {
		if state.IgnoredTranscripts == nil {
			state.IgnoredTranscripts = map[string]bool{}
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(transcriptsDir, e.Name())
			if state.IgnoredTranscripts[full] {
				continue
			}
			known, hasKnown := state.Offsets[full]
			if firstScan || !hasKnown {
				// New file. Two things to do before tracking it:
				// 1. Detect self-curator transcripts (so the curator's own
				//    end_turn events never re-enter its inbox). The agent
				//    name lives in a `type=agent-name` metadata record at
				//    the top of the file.
				// 2. Record current size, so we never emit retroactively.
				if isSelfAgentTranscript(full, selfAgents) {
					state.IgnoredTranscripts[full] = true
					fmt.Fprintf(os.Stderr, "kb-tail: ignoring self-agent transcript %s\n", e.Name())
					continue
				}
				if st, err := os.Stat(full); err == nil {
					state.Offsets[full] = st.Size()
				}
				continue
			}
			res := scanTranscriptStops(full, known)
			state.Offsets[full] = res.newOffset
			for _, ev := range res.events {
				if err := writeInboxEvent(inboxDir, ev); err != nil {
					fmt.Fprintf(os.Stderr, "kb-tail: write inbox: %v\n", err)
					continue
				}
				fmt.Printf("kb-tail: +1 stop %s\n", kbShortID(ev.SessionID))
				_ = os.Stdout.Sync()
			}
		}
	}

	// 2. Git commits
	head, err := gitOutput("rev-parse", "HEAD")
	if err == nil && head != "" && head != state.LastSeenSHA {
		if firstScan || state.LastSeenSHA == "" {
			state.LastSeenSHA = head
		} else {
			shas := newCommitsSince(state.LastSeenSHA, head)
			for _, sha := range shas {
				ev := inboxEvent{
					Type:      "commit",
					SHA:       sha,
					Timestamp: time.Now().UTC(),
					RepoRoot:  repoRoot,
				}
				if err := writeInboxEvent(inboxDir, ev); err != nil {
					fmt.Fprintf(os.Stderr, "kb-tail: write inbox: %v\n", err)
					continue
				}
				fmt.Printf("kb-tail: +1 commit %s\n", kbShortSHA(sha))
				_ = os.Stdout.Sync()
			}
			state.LastSeenSHA = head
		}
	}
}

// transcriptsDirFor returns the on-disk directory where Claude Code stores
// session transcripts for sessions whose cwd is `cwd`. Claude Code encodes
// the cwd path into the directory name by replacing every '/' and '.' with
// '-' (so `/foo/bar/.baz` becomes `-foo-bar--baz`). The current repo's
// transcripts live under ~/.claude/projects/<encoded(repoRoot)>/.
func transcriptsDirFor(cwd string) string {
	home, _ := os.UserHomeDir()
	enc := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	return filepath.Join(home, ".claude", "projects", enc)
}

// jsonlLine is the partial schema we need from a Claude Code transcript
// jsonl record. We only look at `assistant` records whose message has
// stop_reason=end_turn — those mark a full assistant turn close, which is
// the semantic "Stop event" we curate from. Other stop_reason values
// (tool_use, max_tokens) are mid-turn and ignored.
type jsonlLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

type scanResult struct {
	events    []inboxEvent
	newOffset int64
}

// scanTranscriptStops reads `path` starting at byte offset `from` and
// returns the new file offset and any end_turn stop events found. Partial
// last lines (file still being written) are NOT consumed — the offset
// stops at the last newline so the next tick re-reads from there.
func scanTranscriptStops(path string, from int64) scanResult {
	f, err := os.Open(path)
	if err != nil {
		return scanResult{newOffset: from}
	}
	defer f.Close()

	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return scanResult{newOffset: from}
	}

	var events []inboxEvent
	offset := from
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// Partial trailing line. Do not advance offset past its start.
			break
		}
		if err != nil {
			break
		}
		var l jsonlLine
		if jsonErr := json.Unmarshal(line, &l); jsonErr == nil {
			if l.Type == "assistant" && l.Message.StopReason == "end_turn" {
				ts := time.Now().UTC()
				if l.Timestamp != "" {
					if t, perr := time.Parse(time.RFC3339Nano, l.Timestamp); perr == nil {
						ts = t.UTC()
					}
				}
				events = append(events, inboxEvent{
					Type:           "stop",
					SessionID:      l.SessionID,
					UUID:           l.UUID,
					TranscriptPath: path,
					Timestamp:      ts,
				})
			}
		}
		offset += int64(len(line))
	}
	return scanResult{events: events, newOffset: offset}
}

// inboxEvent is the on-disk shape of a single event file under
// <repo>/.kb/inbox/. The curator agent reads these to decide what to
// curate. Both `stop` and `commit` flavors use the same struct; unused
// fields stay at their zero value and json-marshal as empty.
type inboxEvent struct {
	Type           string    `json:"type"`
	Timestamp      time.Time `json:"timestamp"`
	SessionID      string    `json:"session_id,omitempty"`
	UUID           string    `json:"uuid,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	SHA            string    `json:"sha,omitempty"`
	RepoRoot       string    `json:"repo_root,omitempty"`
}

func writeInboxEvent(inboxDir string, ev inboxEvent) error {
	ts := ev.Timestamp.Format("20060102T150405.000Z")
	ts = strings.ReplaceAll(ts, ":", "-")
	ts = strings.ReplaceAll(ts, ".", "-")
	var id string
	switch ev.Type {
	case "stop":
		id = kbShortID(ev.SessionID)
	case "commit":
		id = kbShortSHA(ev.SHA)
	default:
		id = "unknown"
	}
	name := fmt.Sprintf("%s-%s-%s.json", ts, ev.Type, id)
	path := filepath.Join(inboxDir, name)
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newCommitsSince(from, to string) []string {
	if from == "" {
		return nil
	}
	out, err := gitOutput("rev-list", "--reverse", from+".."+to)
	if err != nil || out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	// Trim any empties.
	cleaned := lines[:0]
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			cleaned = append(cleaned, l)
		}
	}
	return cleaned
}

type kbTailState struct {
	Offsets            map[string]int64 `json:"offsets"`
	LastSeenSHA        string           `json:"last_seen_sha"`
	IgnoredTranscripts map[string]bool  `json:"ignored_transcripts,omitempty"`
}

// isSelfAgentTranscript peeks at the first few lines of a Claude Code
// transcript jsonl looking for a `type=agent-name` metadata record whose
// `agentName` matches one of the configured self-agents. Claude Code
// writes this record near the top of every session started with --agent,
// so a short bounded read is enough. Returns false on read/parse errors
// or when no marker is found within the probe budget.
func isSelfAgentTranscript(path string, selfAgents map[string]bool) bool {
	if len(selfAgents) == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Increase line buffer — transcript lines can be large (assistant content
	// embedded). Probe records (agent-name, attachment) are small but we
	// scan past them in case ordering shifts in future Claude Code versions.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for probed := 0; probed < 40 && scanner.Scan(); probed++ {
		var rec struct {
			Type      string `json:"type"`
			AgentName string `json:"agentName"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Type == "agent-name" && selfAgents[rec.AgentName] {
			return true
		}
	}
	// scanner.Err is intentionally swallowed: a partial/torn read here means
	// "no marker found yet"; the next tick will probe again.
	_ = scanner.Err()
	return false
}

func loadKbTailState(path string) kbTailState {
	s := kbTailState{Offsets: map[string]int64{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return kbTailState{Offsets: map[string]int64{}}
	}
	if s.Offsets == nil {
		s.Offsets = map[string]int64{}
	}
	return s
}

func saveKbTailState(path string, s kbTailState) error {
	// Sort offsets for stable on-disk diffing.
	keys := make([]string, 0, len(s.Offsets))
	for k := range s.Offsets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := make(map[string]int64, len(keys))
	for _, k := range keys {
		sorted[k] = s.Offsets[k]
	}
	out := kbTailState{Offsets: sorted, LastSeenSHA: s.LastSeenSHA}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func kbShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func kbShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
