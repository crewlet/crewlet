package executor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// kimiHandHelper is a stand-in note helper for the driver's hands: each run
// appends its arguments to `fired` beside it, prints its own trouble, in its
// first run says so into the pipe `hold` beside it where there is one and
// waits for `release` there, and prints `hand-<n>` beside it, n its run's
// number, where there is one.
const kimiHandHelper = `#!/bin/sh
d=$(dirname "$0"); n=1
[ -f "$d/fired" ] && n=$(( $(wc -l < "$d/fired") + 1 ))
echo "$*" >> "$d/fired"
echo "notehelper: trouble $n" >&2
[ "$n" != 1 ] || [ ! -p "$d/hold" ] || { echo held > "$d/hold"; until [ -f "$d/release" ]; do sleep 0.05; done; }
[ ! -f "$d/hand-$n" ] || cat "$d/hand-$n"
`

// kimiToolEnded is a tool call's end as the session's socket sends it, of the
// agent agent ("main" for the main loop).
func kimiToolEnded(agent, id string) string {
	return `{"type":"item.ended","turn_id":"t_1","agent_id":"` + agent + `","item":{"type":"tool_call","item_id":"` + id +
		`","name":"Bash","status":"ended","outcome":"completed"}}`
}

// kimiHands is a probe whose run hands checkouts through [kimiHandHelper],
// each firing n's text hands[n-1] ("" for none), and the helper's directory.
func kimiHands(t *testing.T, events []string, hands ...string) (*kimiProbe, string) {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, "notehelper")
	writeHostProgram(t, helper, []byte(kimiHandHelper), 0o700)
	for i, text := range hands {
		if text == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "hand-"+string(rune('1'+i))), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &kimiProbe{t: t, idle: make(chan struct{}, 4), events: events, checkouts: map[string]any{"helper": helper,
		"config": "/state/nested.json", "timeoutMs": 10_000, "maxOutput": 1 << 20,
		"skipped": filepath.Join(dir, "skipped")}}, dir
}

// kimiFired is each firing's arguments the stand-in helper recorded.
func kimiFired(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "fired"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(raw), " ", "_"))
}

// A KIMI RUN HANDS A CHECKOUT AT THE MAIN LOOP'S TOOL CALLS, EACH TEXT ONCE:
// each main-loop call's end runs a firing of kimi-nested, one at a time, and
// a firing's text is steered whole under a prompt_id of its own, with the
// run's disabled tools; a firing that hands nothing steers nothing, and a
// subagent's call (2.7.2 sends none; here one naming another agent) fires
// nothing; the helper's own trouble reaches the run's error stream, and the
// run ends 0 once every firing has settled.
//
// Mutation: fire on every event, steer an empty text, take a note's prefix,
// or drop the helper's stderr: red.
func TestAKimiRunHandsACheckoutAtTheMainLoopsToolCalls(t *testing.T) {
	probe, dir := kimiHands(t, []string{kimiToolEnded("agent_sub", "call_s"), kimiToolEnded("main", "call_1"),
		kimiToolEnded("main", "call_2")}, "for `late` with root /w/late\n\nLATE", "")
	probe.webDenied = true
	cmd, stdout := kimiDriverRun(t, probe, "")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s%s", code, stdout, &probe.stderr)
	}
	want := "-adapter_kimi-nested_-config_/state/nested.json"
	if got := kimiFired(t, dir); !slices.Equal(got, []string{want, want}) {
		t.Errorf("the helper ran %q, want once per main-loop call", got)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.steers) != 1 || !strings.HasPrefix(probe.steers[0], "crewlet-checkouts-") ||
		!strings.HasSuffix(probe.steers[0], " for `late` with root /w/late\n\nLATE") {
		t.Errorf("steers = %q, want the one hand under its own prompt_id", probe.steers)
	}
	if !slices.Equal(probe.steerDisabled, []string{"FetchURL,WebSearch"}) {
		t.Errorf("the hand's disabled_tools = %q, want the web-denied run's", probe.steerDisabled)
	}
	for _, n := range []string{"1", "2"} {
		if !strings.Contains(probe.stderr.String(), "notehelper: trouble "+n+"\n") {
			t.Errorf("the driver's stderr lacks firing %s's trouble:\n%s", n, &probe.stderr)
		}
	}
	// A subagent's call alone: the session goes idle as the brief is posted,
	// its event sent before the brief's answer, and no firing runs.
	sub, subDir := kimiHands(t, []string{kimiToolEnded("agent_sub", "call_s")}, "SUB")
	sub.idle <- struct{}{}
	cmd, stdout = kimiDriverRun(t, sub, "")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the subagent's run exited %d, want 0:\n%s%s", code, stdout, &sub.stderr)
	}
	if got := kimiFired(t, subDir); len(got) != 0 {
		t.Errorf("a subagent's call ran the helper %q", got)
	}
}

// A HAND A HOOK KEEPS FROM THE MODEL NEVER ENDS A KIMI RUN: the hand
// carries no note, brief or tool of the run's, so the run goes on and ends 0,
// its outcome its last turn's before the kept hand's, the skip log saying
// once which hook kept it; a note the same hook keeps still ends the run,
// naming the note.
//
// Mutation: refuse a kept hand, take its turn for the run's outcome, write
// nothing or a line per hand, or let a kept hand hide a kept note: red.
func TestAHandAHookKeepsNeverEndsAKimiRun(t *testing.T) {
	hooks := map[string]any{"hooks": []any{map[string]any{"event": "UserPromptSubmit", "matcher": "^for |^ship", "command": "exit 2"}}}
	for _, note := range []bool{false, true} {
		t.Run(map[bool]string{false: "the hand alone", true: "and a note"}[note], func(t *testing.T) {
			probe, dir := kimiHands(t, []string{kimiToolEnded("main", "call_1"), kimiToolEnded("main", "call_2")},
				"for `late` with root /w/late", "for `later` with root /w/later")
			probe.keepHands, probe.config = true, hooks
			probe.turns = []map[string]any{{"turn_id": "t_1", "outcome": "completed", "steps": []any{map[string]any{}},
				"items": []map[string]any{}}}
			if note {
				probe.early = []map[string]any{kimiNote(1, "a", "ship it")}
				probe.turns = append(probe.turns, map[string]any{"turn_id": "t_2", "end_reason": "blocked", "steps": []any{},
					"items": []map[string]any{{"type": "user_message", "item_id": KimiNoteID("a"), "text": "ship it"}}})
			}
			cmd, stdout := kimiDriverRun(t, probe, "")
			code := kimiDriverEnds(t, cmd)
			skipped, _ := os.ReadFile(filepath.Join(dir, "skipped"))
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if note {
				if code != 1 || !strings.Contains(probe.stderr.String(), "Kimi kept the note a from the model") {
					t.Errorf("the driver exited %d saying %q, want the note's refusal", code, &probe.stderr)
				}
				return
			}
			if code != 0 || strings.Contains(stdout.String(), "crewlet.refused") {
				t.Fatalf("the driver exited %d, want 0 and no refusal:\n%s%s", code, stdout, &probe.stderr)
			}
			want := "crewlet: a UserPromptSubmit hook kept the instructions and skills of a checkout cloned during the run " +
				"from the model: " + filepath.Join(probe.home, "config.toml") +
				`: its [[hooks]] entry event UserPromptSubmit, matcher "^for |^ship", command "exit 2"` + "\n"
			if len(probe.steers) != 2 || string(skipped) != want {
				t.Errorf("after %d hands the skip log holds %q, want once\n%q", len(probe.steers), skipped, want)
			}
		})
	}
}

// A FIRING IN FLIGHT AS THE SESSION GOES IDLE IS STEERED BEFORE THE RUN ENDS:
// the clone the run's last call made is handed, its turn waited out, so the
// driver settles every firing, one at a time, before it judges the run over.
// Here the session reads idle once the first firing has begun, which is held
// until it has, a second call's end coming meanwhile.
//
// Mutation: judge the run at idle without settling the firings, or run
// firings at once (the later one alone then settled): red.
func TestAKimiFiringInFlightAtIdleIsSteeredBeforeTheRunEnds(t *testing.T) {
	probe, dir := kimiHands(t, []string{kimiToolEnded("main", "call_1"), kimiToolEnded("main", "call_2")},
		"for `late` with root /w/late")
	hold := filepath.Join(dir, "hold")
	if err := syscall.Mkfifo(hold, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		raw, err := os.ReadFile(hold) // returns once the firing has begun and said so
		if err == nil && len(raw) > 0 {
			probe.idle <- struct{}{}
		}
	}()
	t.Cleanup(func() { // a reader still waiting is let go
		if f, err := os.OpenFile(hold, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	probe.onIdle = func() { _ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600) }
	cmd, stdout := kimiDriverRun(t, probe, "")
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s%s", code, stdout, &probe.stderr)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.steers) != 1 || !strings.HasSuffix(probe.steers[0], " for `late` with root /w/late") {
		t.Errorf("steers = %q, want the held firing's hand", probe.steers)
	}
	steered := slices.IndexFunc(probe.calls, func(c string) bool { return strings.HasPrefix(c, "steer ") })
	if settled := slices.Index(probe.calls, "prompts"); steered < 0 || settled < steered {
		t.Errorf("calls = %q, want the hand steered before the session's prompts are first asked after", probe.calls)
	}
}
