package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/muesli/termenv"
)

// hubUITestSetup forces lipgloss to render with no ANSI escapes so View()
// output is stable across CI runners with different TERM / NO_COLOR values.
// Called once via init() since lipgloss's default renderer is process-wide.
func init() {
	lipgloss.SetColorProfile(termenv.Ascii)
}

// newTestHubModel builds a hubModel populated with the given profile
// locations and otherwise-empty maps. Mirrors the construction in runHub()
// but skips disk-backed sources (pins, recents, running, prefs) so the test
// doesn't depend on CLAUDE_PROFILES_ROOT or filesystem state. Returned by
// value so it matches the value-receiver Update/View methods.
func newTestHubModel(locs []ProfileLocation) hubModel {
	ti := textinput.New()
	ti.Focus()

	row := list.NewDefaultDelegate()
	row.SetSpacing(1)
	row.ShowDescription = true
	del := hubDelegate{rowDelegate: row}

	m := hubModel{
		input:      ti,
		delegate:   del,
		locs:       locs,
		pinMap:     map[string]PinEntry{},
		runningMap: map[string][]RunningWrapper{},
		bgMap:      map[string][]BackgroundedSession{},
		prefsStore: ProfilePrefsStore{},
	}
	m.rebuildIndex()

	l := list.New(m.fullItems, del, 0, 0)
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false)
	l.SetShowStatusBar(false)
	if idx := firstSelectableIndex(m.fullItems); idx >= 0 {
		l.Select(idx)
	}
	m.list = l
	return m
}

func sampleLocs() []ProfileLocation {
	return []ProfileLocation{
		{Name: "alpha", QualifiedID: "alpha", JSONPath: "/tmp/np-test/alpha/profile.json"},
		{Name: "beta", QualifiedID: "beta", JSONPath: "/tmp/np-test/beta/profile.json"},
	}
}

// TestHubRendersProfileNames: with two user-level profiles, the rendered
// view contains both names; Esc on empty input quits with actQuit.
func TestHubRendersProfileNames(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha")) && bytes.Contains(b, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actQuit {
		t.Fatalf("expected actQuit on Esc, got %q", fm.result.action)
	}
}

// TestHubFilterByTyping: typing into the input narrows the visible list to
// matching profiles. "alph" must keep alpha visible and hide beta.
func TestHubFilterByTyping(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	// Wait for first render so the typing isn't dropped on the floor.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha")) && bytes.Contains(b, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Type("alph")

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		// Output() is cumulative: we need beta to disappear from the LATEST
		// rendered frame, not from the buffer. Reading the tail covers that —
		// bubbletea redraws the full View on every update, so the most-recent
		// frame dominates the buffer once a few frames have flushed.
		// The shortcut: check the last 4 KiB only.
		tail := b
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return bytes.Contains(tail, []byte("alpha")) && !bytes.Contains(tail, []byte("beta"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	// Esc clears the filter rather than quitting (input is non-empty).
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	// Second Esc with cleared input quits.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actQuit {
		t.Fatalf("expected actQuit after clear+quit, got %q", fm.result.action)
	}
}

// TestBgStatusSuffix pins the four user-visible render branches of
// spec-issue-9 Step 4. The "all zero" case is the most load-bearing —
// it's the graceful-fallback render when `claude agents --json` errors
// or has nothing to say. A regression there would leak an empty "()"
// into the hub.
func TestBgStatusSuffix(t *testing.T) {
	cases := []struct {
		name string
		in   BgStatusCounts
		want string
	}{
		{"both zero → no suffix", BgStatusCounts{}, ""},
		{"busy only", BgStatusCounts{Busy: 2}, "(2 busy)"},
		{"idle only", BgStatusCounts{Idle: 3}, "(3 idle)"},
		{"mixed", BgStatusCounts{Busy: 1, Idle: 2}, "(1 busy · 2 idle)"},
		{"single each", BgStatusCounts{Busy: 1, Idle: 1}, "(1 busy · 1 idle)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bgStatusSuffix(c.in); got != c.want {
				t.Errorf("bgStatusSuffix(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAgentMapsEqual confirms the cheap-diff guard the tick path uses
// to skip rebuilds. Wrong logic here would either cause spurious cursor
// jumps every 3s (always returning false) or stale annotation that
// never updates (always returning true).
func TestAgentMapsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"empty vs nil", map[string]string{}, nil, true},
		{"identical", map[string]string{"s1": "busy"}, map[string]string{"s1": "busy"}, true},
		{"different value", map[string]string{"s1": "busy"}, map[string]string{"s1": "idle"}, false},
		{"different keys, same len", map[string]string{"s1": "busy"}, map[string]string{"s2": "busy"}, false},
		{"different lengths", map[string]string{"s1": "busy"}, map[string]string{"s1": "busy", "s2": "idle"}, false},
		// Regression: a key mapped to "" in a and absent from b both yield
		// b[k]=="" under non-comma-ok lookup. Comma-ok disambiguates and
		// reports them as different (which they are — different len anyway,
		// but the principle holds even at equal length with overlapping keys).
		{"empty value vs missing key, same len", map[string]string{"s1": "", "s2": "busy"}, map[string]string{"s2": "busy", "s3": ""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentMapsEqual(c.a, c.b); got != c.want {
				t.Errorf("agentMapsEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestHubTitleBgSuffixRendering exercises hubTitle end-to-end for the
// new agentsByID path: a profile with two bg sessions (one busy, one
// idle) must produce the locked suffix "(1 busy · 1 idle)" inside the
// bg marker. Locks the integration between hubTitle's bg branch,
// bgStatusCounts, and bgStatusSuffix — if any of the three drifts,
// the rendered title catches it.
func TestHubTitleBgSuffixRendering(t *testing.T) {
	loc := ProfileLocation{Name: "alpha", QualifiedID: "alpha", JSONPath: "/tmp/missing/profile.json"}
	bg := []BackgroundedSession{
		{SessionID: "sid-busy"},
		{SessionID: "sid-idle"},
	}
	agentsByID := map[string]string{
		"sid-busy": "busy",
		"sid-idle": "idle",
	}
	got := hubTitle(loc, nil, bg, false, "", modalTags{}, "", agentsByID)
	if !bytes.Contains([]byte(got), []byte("(1 busy · 1 idle)")) {
		t.Errorf("hubTitle suffix missing: %q", got)
	}

	// With nil agentsByID (graceful fallback), the suffix is omitted but
	// the underlying bg marker is still there.
	got = hubTitle(loc, nil, bg, false, "", modalTags{}, "", nil)
	if bytes.Contains([]byte(got), []byte("busy")) || bytes.Contains([]byte(got), []byte("idle")) {
		t.Errorf("hubTitle leaked busy/idle without agentsByID: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte("bg")) {
		t.Errorf("hubTitle dropped the bg marker when agentsByID was nil: %q", got)
	}
}

// TestUpdateAgentsRefreshPreservesCursor pins the trickiest branch of
// the new agentsRefreshMsg handler: after a successful refresh that
// triggers a rebuild, the previously-selected list index must be
// restored. Without restoration, the 3s tick would visibly yank the
// cursor every poll cycle — a UX regression we'd otherwise only catch
// in real-world use.
func TestUpdateAgentsRefreshPreservesCursor(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	if got := len(m.list.Items()); got < 2 {
		t.Fatalf("test setup expected ≥2 items in list, got %d", got)
	}
	m.list.Select(1) // simulate the user having moved past the first row

	updated, _ := m.Update(agentsRefreshMsg{
		{SessionID: "sid", Status: "busy"},
	})
	m2, ok := updated.(hubModel)
	if !ok {
		t.Fatalf("Update returned non-hubModel: %T", updated)
	}
	if got := m2.agentsByID["sid"]; got != "busy" {
		t.Errorf("agentsByID not populated after refresh: got %q, want %q", got, "busy")
	}
	if got := m2.list.Index(); got != 1 {
		t.Errorf("cursor not restored: got %d, want 1", got)
	}

	// Second identical refresh — agentMapsEqual short-circuit means no
	// rebuild, no Select call, cursor naturally stays put. This is the
	// "common case" path that runs every 3s when nothing is changing.
	updated2, _ := m2.Update(agentsRefreshMsg{
		{SessionID: "sid", Status: "busy"},
	})
	m3, _ := updated2.(hubModel)
	if got := m3.list.Index(); got != 1 {
		t.Errorf("cursor moved on no-op refresh: got %d, want 1", got)
	}
}

// TestHubPaletteNewAction: Tab opens the action palette; pressing 'n' inside
// the palette quits the hub with actNew so the outer dispatcher creates a
// new profile.
func TestHubPaletteNewAction(t *testing.T) {
	m := newTestHubModel(sampleLocs())
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(140, 40))

	// Wait for first render before opening palette.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("alpha"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	// Wait for the palette overlay to render (it shows the bracketed action
	// hotkeys — picking the New entry's hotkey letter as the signal).
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		tail := b
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		// Palette overlay renders entries like "n   new profile". Match the
		// uniquely-palette text without locking in to colour codes.
		return bytes.Contains(tail, []byte("new profile"))
	}, teatest.WithDuration(2*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	final := tm.FinalModel(t, teatest.WithFinalTimeout(2*time.Second))
	fm, ok := final.(hubModel)
	if !ok {
		t.Fatalf("expected hubModel, got %T", final)
	}
	if fm.result.action != actNew {
		t.Fatalf("expected actNew from palette 'n', got %q", fm.result.action)
	}
}
