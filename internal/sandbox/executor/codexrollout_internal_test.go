package executor

import (
	"bytes"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// codexRolloutFixture is a Codex 0.171.0 rollout, trimmed: one exec turn of
// three requests against a fake provider, with a hook's developer note and a
// Stop hook's user prompt (testdata/codex/0.171.0/rollout.jsonl).
func codexRolloutFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex", "0.171.0", "rollout.jsonl"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return raw
}

// A CODEX ROLLOUT READS ITS SESSION, MODEL, WINDOW, LIMITS AND SPEND as 0.171.0
// writes them: the thread id and release off session_meta, the model off
// turn_context, the window and the plan's two windows off the newest
// token_count, and the turn's spend off its LAST token_usage_record, whose
// turn_token_usage is the turn's running total (never the three summed).
//
// Mutation: read the first token_count, sum a turn's records, read
// thread_token_usage or usage, or drop the minutes-to-duration conversion, and
// a figure here goes wrong.
func TestACodexRolloutReadsItsSessionModelWindowLimitsAndSpend(t *testing.T) {
	t.Parallel()
	r, err := readCodexRollout(bytes.NewReader(codexRolloutFixture(t)))
	if err != nil {
		t.Fatalf("read the rollout: %v", err)
	}
	if r.ThreadID() != "01a1ff62-fbda-7aa2-af57-6c0abeb8e50b" || r.CLIVersion() != "0.171.0" {
		t.Errorf("the session reads as %q on %q", r.ThreadID(), r.CLIVersion())
	}
	if r.Model() != "gpt-test" || r.ContextWindow() != 258400 {
		t.Errorf("the model reads as %q with a window of %d", r.Model(), r.ContextWindow())
	}
	limits, ok := r.Limits()
	want := codexLimits{
		LimitID: "codex",
		Primary: &codexLimitWindow{UsedPercent: 37.5, Window: 5 * time.Hour,
			ResetsAt: time.Unix(1795287240, 0).UTC()},
		Secondary: &codexLimitWindow{UsedPercent: 12, Window: 7 * 24 * time.Hour,
			ResetsAt: time.Unix(1795372440, 0).UTC()},
	}
	if !ok || !reflect.DeepEqual(limits, want) {
		t.Errorf("the limits read as %+v (%v), want %+v", limits, ok, want)
	}
	turn := codexTokens{Input: 3702, CachedInput: 3000, Output: 168, ReasoningOutput: 21, Total: 3870}
	if got := r.Turns(); len(got) != 1 || got[0].TurnID != "01a1ff62-fc32-75a3-8242-77edbc9d7845" ||
		got[0].Tokens != turn {
		t.Errorf("the turns read as %+v, want the one turn at %+v", got, turn)
	}
	if got, want := r.Usage().spend(), (runSpend{input: 702, cacheRead: 3000, output: 168}); got != want {
		t.Errorf("the spend reads as %+v, want %+v", got, want)
	}
	if r.Torn() {
		t.Error("a rollout ending on a newline reads as torn")
	}
}

// A CODEX SESSION'S SPEND IS EVERY TURN'S, each turn at its last record: a
// resumed session's rollout holds the turns before, so a continued run's own
// spend is the difference between two such sums.
//
// Mutation: keep only the newest turn, or replace a turn's earlier record by
// appending it as a turn, and the sum goes wrong.
func TestACodexSessionsSpendIsEveryTurnsLastRecordSummed(t *testing.T) {
	t.Parallel()
	rollout := strings.Join([]string{
		codexUsageLine("t1", 100, 10), codexUsageLine("t1", 250, 30),
		codexUsageLine("t2", 400, 5),
		codexUsageLine("t2", 900, 50), "",
	}, "\n")
	r, err := readCodexRollout(strings.NewReader(rollout))
	if err != nil {
		t.Fatalf("read the rollout: %v", err)
	}
	if got := r.Usage(); got.Input != 1150 || got.Output != 80 || len(r.Turns()) != 2 {
		t.Errorf("the session reads as %+v over %d turns, want 1150 in and 80 out over 2", got, len(r.Turns()))
	}
}

// codexUsageLine is a token_usage_record of turn whose running total is in
// tokens in and out tokens out.
func codexUsageLine(turn string, in, out int) string {
	return `{"type":"token_usage_record","payload":{"turn_id":"` + turn + `","turn_token_usage":{"input_tokens":` +
		strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}`
}

// A CODEX ROLLOUT'S USER MESSAGES ARE ITS USER ITEMS ALONE, in order: a
// developer item (a hook's note) is not one, nor the item_completed event that
// repeats each, nor anything a compaction wrote. Codex writes a compaction as
// its own `compacted` line, the summary and the history it keeps inside it, so
// a note that survives only there is never read as delivered (ADR-0177).
//
// Mutation: read developer items, read item_completed UserMessage events, or
// read the user items inside a compacted line's replacement_history, and a
// message appears here that the model was never handed as a user item.
func TestACodexRolloutsUserMessagesAreItsUserItemsAlone(t *testing.T) {
	t.Parallel()
	compaction := `{"type":"compacted","payload":{"message":"SUMMARY of the turn",` +
		`"replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"NOTE kept in a summary"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"SUMMARY of the turn"}]}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"context_compacted"}}` + "\n"
	raw := append(codexRolloutFixture(t), compaction...)
	r, err := readCodexRollout(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read the rollout: %v", err)
	}
	got := r.UserMessages()
	if len(got) != 3 || !strings.HasPrefix(got[0], "<environment_context>") ||
		got[1] != "SCENARIO:slowmcp:1 go" || !strings.HasPrefix(got[2], "<hook_prompt ") {
		t.Fatalf("the user messages read as %q, want the context, the prompt and the Stop prompt", got)
	}
	for _, text := range got {
		if strings.Contains(text, "NOTE-HOOK") || strings.Contains(text, "SUMMARY") || strings.Contains(text, "NOTE kept") {
			t.Errorf("%q reads as a user message", text)
		}
	}
}

// A TORN LAST CODEX LINE IS NEVER READ AS WHOLE: Codex writes a line and its
// newline together, so a last line with no newline is one still being written,
// even where what has arrived happens to parse.
//
// Mutation: parse the bytes after the last newline, or report a rollout torn
// that ends on one, and a message half-written (or whole but unterminated)
// reads as delivered.
func TestATornLastCodexLineIsNeverReadAsWhole(t *testing.T) {
	t.Parallel()
	user := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"LATE NOTE"}]}}`
	for name, tail := range map[string]string{
		"cut":          user[:len(user)-7],
		"unterminated": user,
	} {
		raw := append(codexRolloutFixture(t), tail...)
		r, err := readCodexRollout(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: read the rollout: %v", name, err)
		}
		if !r.Torn() || len(r.UserMessages()) != 3 {
			t.Errorf("%s: torn %v with %d user messages, want torn and the 3 before it", name, r.Torn(), len(r.UserMessages()))
		}
	}
}

// AN UNKNOWN OR UNREADABLE CODEX LINE IS SKIPPED, never the end of the read: a
// release adds kinds, and a line that is no JSON object holds nothing to read.
//
// Mutation: fail the read on either, and a rollout this build half-knows reads
// as nothing.
func TestAnUnknownOrUnreadableCodexLineIsSkipped(t *testing.T) {
	t.Parallel()
	raw := append([]byte("not json\n{\"type\":\"a_kind_from_later\",\"payload\":{\"x\":1}}\n[1,2]\n\n"),
		codexRolloutFixture(t)...)
	r, err := readCodexRollout(bytes.NewReader(raw))
	if err != nil || r.ThreadID() == "" || len(r.UserMessages()) != 3 {
		t.Errorf("read %q with %d user messages (%v), want the fixture whole", r.ThreadID(), len(r.UserMessages()), err)
	}
}

// A CODEX LINE PAST ITS BOUND FAILS THE READ, and one at it is read: the bound
// holds the longest note turn/steer takes, escaped, so a longer line is a shape
// this build never read and the rollout is unreadable, not read without it.
//
// Mutation: skip the long line, cut it, or lower the bound below a steer's
// ceiling, and the read passes over a line or refuses a note Codex took.
func TestACodexLinePastItsBoundFailsTheRead(t *testing.T) {
	t.Parallel()
	head := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`
	tail := `"}]}}`
	note := strings.Repeat("\\u001f", codexSteerMaxCodePoints)
	fits := head + note + tail + "\n"
	if len(fits) > codexRolloutLineMaxBytes {
		t.Fatalf("a steer's longest note, escaped, is %d bytes, past the %d bound", len(fits), codexRolloutLineMaxBytes)
	}
	r, err := readCodexRollout(strings.NewReader(fits))
	if err != nil || len(r.UserMessages()) != 1 || len([]rune(r.UserMessages()[0])) != codexSteerMaxCodePoints {
		t.Errorf("a line within the bound read with err %v", err)
	}
	long := head + strings.Repeat("a", codexRolloutLineMaxBytes) + tail + "\n"
	if _, err := readCodexRollout(strings.NewReader(long)); !errors.Is(err, errCodexRolloutLine) {
		t.Errorf("a line past the bound read with err %v, want errCodexRolloutLine", err)
	}
}

// A CHATGPT PLAN'S CODEX LIMITS HAVE ONE WINDOW: measured on a real plan, the
// secondary is null and the plan type is named, and a missing window is none,
// never a zero window at zero percent.
//
// Mutation: read a null window as a zero one, or drop plan_type, and the plan
// reads as one with a spent or unknown second window.
func TestAChatGPTPlansCodexLimitsHaveOneWindow(t *testing.T) {
	t.Parallel()
	line := `{"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":475000},` +
		`"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":0.0,"window_minutes":10080,` +
		`"resets_at":1795891012},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":null},` +
		`"plan_type":"self_serve_business_prolite"}}}` + "\n"
	r, err := readCodexRollout(strings.NewReader(line))
	limits, ok := r.Limits()
	if err != nil || !ok || limits.Secondary != nil || limits.Primary == nil ||
		limits.Primary.Window != 7*24*time.Hour || limits.PlanType != "self_serve_business_prolite" ||
		r.ContextWindow() != 475000 {
		t.Errorf("the plan reads as %+v (%v, %v)", limits, ok, err)
	}
	if _, ok := (codexRollout{}).Limits(); ok {
		t.Error("a rollout with no token_count reads as stating limits")
	}
}

// A RESTORED CODEX ROLLOUT IS NAMED AS A RESUME FINDS IT: Codex resumes a
// thread only from a file named `rollout-*-<id>.jsonl` under sessions/
// (renamed otherwise, the resume failed: measured), and the locator looks for
// that name. Neither names a path for an id a pattern could misread.
//
// Mutation: restore under another name, look under another directory or
// pattern, or pass an id with a glob character, and a carried session is not
// found.
func TestARestoredCodexRolloutIsNamedAsAResumeFindsIt(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	id := "01a1ff62-fbda-7aa2-af57-6c0abeb8e50b"
	restored := codexRestoredRollout(p, id)
	if dir, name := path.Split(restored); dir != "/home/user/.codex/sessions/" {
		t.Errorf("restored into %q, not the sessions root", dir)
	} else if ok, _ := path.Match("rollout-*-"+id+".jsonl", name); !ok {
		t.Errorf("restored as %q, a name a resume does not find", name)
	}
	want := `find '/home/user/.codex/sessions' -type f -name 'rollout-*-` + id + `.jsonl'`
	if got := codexRolloutPath(p, id); !strings.HasPrefix(got, want) {
		t.Errorf("the locator is %q, want it to begin %q", got, want)
	}
	for _, bad := range []string{"", "a*b", "../x", "a b"} {
		if codexRolloutPath(p, bad) != "" || codexRestoredRollout(p, bad) != "" {
			t.Errorf("an id %q names a path", bad)
		}
	}
}
