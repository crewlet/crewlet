package executor

import (
	"bytes"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
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

// codexFixture is a trimmed Codex 0.171.0 rollout of testdata/codex/0.171.0:
// compaction-note-before.jsonl, a note steered before a forced compaction
// (written plain, then kept in the compacted line's history, whose summary
// names it); compaction-note-after.jsonl, a note steered after it (written
// after the compacted line); torn-mid-file.jsonl, a run killed, a fragment
// left mid-file, and two resumes after it. The compacted lines' history is
// cut to the shape CODEX-MEASURE §15.2 records.
func codexFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex", "0.171.0", name))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return raw
}

// personsWords is every person's message the rollout raw holds, in order, as
// a note's confirm reads them ([codexConversation]), and whether its last line
// was torn.
func personsWords(t *testing.T, raw []byte) ([]string, bool) {
	t.Helper()
	var words []string
	torn, err := codexConversation(bytes.NewReader(raw), 0, nil, func(kind codexTurnKind, text string) {
		if kind == codexPersonsWords {
			words = append(words, text)
		}
	})
	if err != nil {
		t.Fatalf("read the rollout's conversation: %v", err)
	}
	return words, torn
}

// A CODEX ROLLOUT'S USER MESSAGES ARE A PERSON'S WORDS ALONE, in order, each
// once: Codex writes its environment context, a hook's prompt and a
// compaction's summary as user messages too, marked as its own, and keeps a
// compaction's history inside its `compacted` line. A note steered before a
// compaction reads once, from its own item; one steered after reads from the
// item written after the compacted line; one only named by the summary never
// reads (ADR-0177).
//
// Mutation: read every role:user item, read the compacted line's history or
// its summary, or read item_completed events, and a message appears here the
// model was never handed as a person's.
func TestACodexRolloutsUserMessagesAreAPersonsWordsAlone(t *testing.T) {
	t.Parallel()
	for name, want := range map[string][]string{
		"rollout.jsonl": {"SCENARIO:slowmcp:1 go"},
		"compaction-note-before.jsonl": {"SCENARIO:slow:4 cmp-A",
			"NOTE-CMPA: a person says also create NOTE-A.txt"},
		"compaction-note-after.jsonl": {"SCENARIO:slow:4 cmp-B",
			"NOTE-CMPB: a person says also create NOTE-B.txt"},
	} {
		if got, _ := personsWords(t, codexFixture(t, name)); !slices.Equal(got, want) {
			t.Errorf("%s: the user messages read as %q, want %q", name, got, want)
		}
	}
}

// CODEX'S ONE RULE FOR A PERSON'S MESSAGE reads the part kinds where Codex
// records them and the provenance where it records only that: a `user.text`
// message is a person's, a `compaction.summary` or harness-written one never
// is, and a user message with no metadata at all is taken as a person's.
//
// Mutation: read the provenance before the kinds, read the provenance alone,
// or refuse a message with no metadata, and a row below fails.
func TestCodexsOneRuleForAPersonsMessage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, role, meta string
		want             bool
	}{
		{"a note", "user", `{"content_item_kinds":["user.text"],"content_item_metadata":[{"provenance":{"type":"user"}}]}`, true},
		{"a summary", "user", `{"content_item_kinds":["compaction.summary"],"content_item_metadata":[{"provenance":{"type":"harness","hook":false}}]}`, false},
		{"a hook's prompt", "user", `{"content_item_kinds":["unknown"],"content_item_metadata":[{"provenance":{"type":"harness","hook":true}}]}`, false},
		{"provenance alone", "user", `{"content_item_metadata":[{"provenance":{"type":"user"}}]}`, true},
		{"harness provenance alone", "user", `{"content_item_metadata":[{"provenance":{"type":"harness"}}]}`, false},
		{"no metadata", "user", `{}`, true},
		// KINDS AND PROVENANCE DISAGREE: the kinds decide, both ways.
		{"a person's kind, harness provenance", "user", `{"content_item_kinds":["user.text"],"content_item_metadata":[{"provenance":{"type":"harness"}}]}`, true},
		{"a summary's kind, user provenance", "user", `{"content_item_kinds":["compaction.summary"],"content_item_metadata":[{"provenance":{"type":"user"}}]}`, false},
		{"the model", "assistant", `{"content_item_kinds":["user.text"]}`, false},
	} {
		var meta codexItemMeta
		if err := wireDecode([]byte(c.meta), &meta); err != nil {
			t.Fatal(err)
		}
		if got := codexPersonsMessage(c.role, meta); got != c.want {
			t.Errorf("%s: read as a person's %v, want %v", c.name, got, c.want)
		}
	}
}

// A FRAGMENT MID-ROLLOUT IS PASSED OVER: Codex cut off mid-line leaves the
// fragment in place and writes the next record on a line of its own, so the
// lines after it read and the rollout is not torn.
//
// Mutation: end the read at an unparsable line, or call the rollout torn for
// one that is not its last, and the resumed turns are lost.
func TestAFragmentMidRolloutIsPassedOver(t *testing.T) {
	t.Parallel()
	raw := codexFixture(t, "torn-mid-file.jsonl")
	r, err := readCodexRollout(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read the rollout: %v", err)
	}
	want := []string{"SCENARIO:loop kill-k9-6_1", "SCENARIO:plain after-kill-k9-6_1", "SCENARIO:plain after-kill-torn3"}
	if got, torn := personsWords(t, raw); !slices.Equal(got, want) || torn || r.Torn() || r.ThreadID() == "" {
		t.Errorf("read %q, torn %v, thread %q; want %q, whole", got, r.Torn(), r.ThreadID(), want)
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
		if words, torn := personsWords(t, raw); !r.Torn() || !torn || len(words) != 1 {
			t.Errorf("%s: torn %v with %d user messages, want torn and the 1 before it", name, r.Torn(), len(words))
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
	if words, _ := personsWords(t, raw); err != nil || r.ThreadID() == "" || len(words) != 1 {
		t.Errorf("read %q with %d user messages (%v), want the fixture whole", r.ThreadID(), len(words), err)
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
	if words, _ := personsWords(t, []byte(fits)); len(words) != 1 || len([]rune(words[0])) != codexSteerMaxCodePoints {
		t.Errorf("a line within the bound read as %d messages", len(words))
	}
	long := head + strings.Repeat("a", codexRolloutLineMaxBytes) + tail + "\n"
	if _, err := readCodexRollout(strings.NewReader(long)); !errors.Is(err, errCodexRolloutLine) {
		t.Errorf("a line past the bound read with err %v, want errCodexRolloutLine", err)
	}
	if _, err := codexNotesHeld(strings.NewReader(long), nil, 0, nil); !errors.Is(err, errCodexRolloutLine) {
		t.Errorf("a line past the bound confirmed with err %v, want errCodexRolloutLine", err)
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
// thread only from `rollout-YYYY-MM-DDThh-mm-ss-<id>.jsonl` under sessions/
// (any other name refused: measured), and the locator looks for that name. Neither names a path for an id a pattern could misread.
//
// Mutation: restore under another name, look under another directory or
// pattern, or pass an id with a glob character, and a carried session is not
// found.
func TestARestoredCodexRolloutIsNamedAsAResumeFindsIt(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	id := "01a1ff62-fbda-7aa2-af57-6c0abeb8e50b"
	restored := codexRestoredRollout(p, id)
	canonical := regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-` + id + `\.jsonl$`)
	if dir, name := path.Split(restored); dir != "/home/user/.codex/sessions/" {
		t.Errorf("restored into %q, not the sessions root", dir)
	} else if !canonical.MatchString(name) {
		t.Errorf("restored as %q, a name a resume refuses", name)
	} else if ok, _ := path.Match("rollout-*-"+id+".jsonl", name); !ok {
		t.Errorf("restored as %q, a name the locator does not find", name)
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
