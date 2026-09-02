package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestElideCutsRunesNotBytes(t *testing.T) {
	t.Parallel()
	// The hazard a character-wise language does not have. A byte slice at
	// limit 4 lands inside the second rune of "日本語テスト"
	// and produces invalid UTF-8, which a JSON encoder then replaces with
	// U+FFFD — so a truncated argument reaches the model as mojibake rather
	// than as a short version of itself.
	got := elide("日本語テスト", 4)
	if !utf8.ValidString(got) {
		t.Fatalf("elide produced invalid UTF-8: %q", got)
	}
	if got != "日本語テ…" {
		t.Errorf("elide = %q, want 日本語テ…", got)
	}
	// And the budget counts runes, so a 6-rune string is untouched at 6
	// even though it is 18 bytes.
	if got := elide("日本語テスト", 6); got != "日本語テスト" {
		t.Errorf("a string exactly at the rune budget was cut: %q", got)
	}
}

func TestElideIsUnboundedAtZero(t *testing.T) {
	t.Parallel()
	// The verbatim contract Review's evidence log depends on. If zero ever
	// starts meaning "cut everything", Review judges an empty log and
	// passes every turn.
	long := strings.Repeat("x", 5000)
	if got := elide(long, 0); got != long {
		t.Errorf("limit 0 truncated to %d chars", len(got))
	}
	if got := elide(long, -1); got != long {
		t.Errorf("a negative limit truncated to %d chars", len(got))
	}
}

func TestPerValueElisionKeepsTheDiscriminator(t *testing.T) {
	t.Parallel()
	// The bug this whole design exists to prevent: capping the SERIALISED
	// object would keep whichever keys sort early and drop the rest, and
	// the argument that says WHICH delivery fired is usually the shortest.
	// A line that kept the message body but lost `channel` looks precise
	// while hiding which of two posts actually happened.
	body := strings.Repeat("prose ", 400)
	got := FormatCalls([]Call{{
		Name: "slack_post",
		Args: map[string]any{"channel": "C0ENGINEERING", "text": body},
	}}, Format(nil, nil))

	if !strings.Contains(got, "C0ENGINEERING") {
		t.Errorf("the discriminating argument was lost:\n%s", got)
	}
	if strings.Contains(got, body) {
		t.Error("the full payload survived; nothing was elided")
	}
	if !strings.Contains(got, "…") {
		t.Errorf("no elision marker, so a trimmed line reads as complete:\n%s", got)
	}
}

func TestValuesThatFitKeepTheirNativeType(t *testing.T) {
	t.Parallel()
	// A number that survives must stay a number. Round-tripping every value
	// through its JSON rendering would turn 42 into "42", and a ledger that
	// restates the arguments with different types misreports what was sent.
	got := renderArgs(map[string]any{"count": 42, "ok": true}, Format(nil, nil))
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("rendered args are not JSON: %v (%s)", err, got)
	}
	if _, isString := back["count"].(string); isString {
		t.Errorf("a fitting number was stringified: %s", got)
	}
	if _, isString := back["ok"].(string); isString {
		t.Errorf("a fitting bool was stringified: %s", got)
	}
}

func TestBlobLimitDropsWholeKeysAndSaysHowMany(t *testing.T) {
	t.Parallel()
	// Shortest-first admission plus an explicit remainder. Cutting the
	// serialised string instead would drop whichever keys sort last, which
	// is the failure per-value elision exists to prevent, one step later.
	args := map[string]any{
		"key":  "PROJ-1",
		"a":    strings.Repeat("a", 300),
		"b":    strings.Repeat("b", 300),
		"c":    strings.Repeat("c", 300),
		"d":    strings.Repeat("d", 300),
		"page": "9912",
	}
	got := fitArguments(args, 400)

	if !strings.Contains(got, "PROJ-1") || !strings.Contains(got, "9912") {
		t.Errorf("short identifiers were dropped before long payloads:\n%s", got)
	}
	if !strings.Contains(got, "more") {
		t.Errorf("a trimmed line did not report its remainder:\n%s", got)
	}
	// The kept object must still parse: the remainder is a suffix, never a
	// cut into the JSON.
	head, _, _ := strings.Cut(got, " +")
	var back map[string]any
	if err := json.Unmarshal([]byte(head), &back); err != nil {
		t.Fatalf("the kept object is not valid JSON: %v (%s)", err, head)
	}
	if len(back) >= len(args) {
		t.Errorf("nothing was dropped despite exceeding the budget: %s", got)
	}
}

func TestOneOversizedArgumentStillRenders(t *testing.T) {
	t.Parallel()
	// The first key is admitted unconditionally. Without that, a call whose
	// single argument blows the budget renders as "{}" — a line claiming a
	// tool was called with nothing, which is a different and false fact.
	got := fitArguments(map[string]any{"body": strings.Repeat("x", 5000)}, 100)
	if !strings.Contains(got, "body") {
		t.Errorf("a lone oversized argument vanished entirely: %s", got)
	}
}

func TestBlobLimitIsStableAcrossRuns(t *testing.T) {
	t.Parallel()
	// Go map iteration is randomised, so "which key got dropped" must not
	// come from range order. Equal-cost keys are broken by name.
	args := map[string]any{}
	for _, k := range []string{"aa", "bb", "cc", "dd", "ee", "ff"} {
		args[k] = strings.Repeat(k, 100)
	}
	first := fitArguments(args, 300)
	for range 40 {
		if got := fitArguments(args, 300); got != first {
			t.Fatalf("unstable output:\n%s\nvs\n%s", first, got)
		}
	}
}

func TestZeroOptionsIsTheVerbatimContract(t *testing.T) {
	t.Parallel()
	// Review's single-iteration evidence log passes the zero value and
	// depends on getting every character back. If the zero value ever
	// starts eliding, the reviewer judges a summary and calls it evidence.
	body := strings.Repeat("y", 3000)
	got := FormatCalls([]Call{{Name: "post", Args: map[string]any{"text": body}}}, FormatOptions{})
	if !strings.Contains(got, body) {
		t.Error("the zero FormatOptions elided; Review's evidence is no longer verbatim")
	}
}

func TestOnlyReadsAreEverDropped(t *testing.T) {
	t.Parallel()
	// A write is the whole reason the ledger exists. However many calls a
	// busy round makes, every write renders and only reads are capped.
	var calls []Call
	for range 20 {
		calls = append(calls, Call{Name: "jira_get_issue"})
	}
	calls = append(calls, Call{Name: "slack_post", Args: map[string]any{"channel": "C1"}})

	got := FormatCalls(calls, Format(nil, []string{"jira_get_issue"}))
	if strings.Count(got, "jira_get_issue") != MaxReadCalls {
		t.Errorf("reads rendered = %d, want the cap %d", strings.Count(got, "jira_get_issue"), MaxReadCalls)
	}
	if !strings.Contains(got, "slack_post") {
		t.Error("the write was dropped to fit the read cap")
	}
	if !strings.Contains(got, "further read call(s) omitted") {
		t.Error("dropped reads were not reported, so the line reads as complete")
	}

	// The counterfactual: 20 WRITES all render. Without this the assertion
	// above passes for a cap that drops everything past 12.
	var writes []Call
	for range 20 {
		writes = append(writes, Call{Name: "slack_post"})
	}
	if n := strings.Count(FormatCalls(writes, Format(nil, nil)), "slack_post"); n != 20 {
		t.Errorf("writes rendered = %d, want all 20", n)
	}
}

func TestReadsAreMarkedSoTheNextRoundMayRerunThem(t *testing.T) {
	t.Parallel()
	// Results are never carried across rounds, so a read the next round
	// needs must be re-run. The marker is what lets the prompt permit
	// exactly that — telling a model "do not repeat" a read pushes it to
	// fabricate the data instead.
	got := FormatCalls([]Call{
		{Name: "jira_get_issue"},
		{Name: "slack_post"},
	}, Format(nil, []string{"jira_get_issue"}))

	if !strings.Contains(got, "jira_get_issue() → success (read)") {
		t.Errorf("the read carries no marker:\n%s", got)
	}
	if strings.Contains(got, "slack_post() → success (read)") {
		t.Errorf("a write was marked as a read:\n%s", got)
	}
}

func TestNoCallsIsAnExplicitNone(t *testing.T) {
	t.Parallel()
	// An absent section reads as one the engine forgot to fill in. "(none)"
	// says the phase took no action, which is a fact the reviewer needs.
	if got := FormatCalls(nil, FormatOptions{}); got != "(none)" {
		t.Errorf("no calls rendered %q, want (none)", got)
	}
	// And skipping every call reaches the same place.
	got := FormatCalls([]Call{{Name: "activate_tool"}}, FormatOptions{Skip: []string{"activate_tool"}})
	if got != "(none)" {
		t.Errorf("an all-skipped run rendered %q, want (none)", got)
	}
}

func TestFailureRendersAsFailureNotSuccess(t *testing.T) {
	t.Parallel()
	// Recorded, never inferred from the output text: a tool whose
	// successful result happens to begin "error:" is not a failure, and a
	// phase reading it as one loops trying to fix something that worked.
	got := FormatCalls([]Call{
		{Name: "slack_post", Failed: true, Result: "channel_not_found"},
		{Name: "jira_note", Result: "error: none found, which is fine"},
	}, Format(nil, nil))

	if !strings.Contains(got, "slack_post() → error: channel_not_found") {
		t.Errorf("a failed call did not render as an error:\n%s", got)
	}
	if !strings.Contains(got, "jira_note() → success") {
		t.Errorf("a successful call whose output mentions an error was read as failed:\n%s", got)
	}
}

func TestNoIterationsRendersNothing(t *testing.T) {
	t.Parallel()
	// The first round of every turn. Callers drop the whole section on an
	// empty string rather than emit a heading with nothing under it.
	if got := RenderIterations(nil, nil); got != "" {
		t.Errorf("an empty ledger rendered %q", got)
	}
}

func TestARenderedIterationCarriesWhatTheNextRoundActsOn(t *testing.T) {
	t.Parallel()
	got := RenderIterations([]Iteration{{
		Iteration:     1,
		Intent:        "post the summary to #eng",
		Calls:         []Call{{Name: "slack_post", Args: map[string]any{"channel": "C0ENG"}}},
		Text:          "Posted the weekly summary.",
		ReviewNotes:   "the link was wrong, repost with the corrected one",
		CompletedWork: "the #eng post landed",
	}}, []string{"activate_tool"})

	for _, want := range []string{
		"### Iteration 1",
		"Set out to: post the summary to #eng",
		"Called:",
		"C0ENG",
		"Produced: Posted the weekly summary.",
		"Reviewer, on what already landed: the #eng post landed",
		"Reviewer's correction: the link was wrong",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestARoundThatCalledNothingSaysSo(t *testing.T) {
	t.Parallel()
	// A round where the executor silently made no calls is
	// indistinguishable from a rendering bug without an explicit "(none)".
	got := RenderIterations([]Iteration{{Iteration: 1, Intent: "triage"}}, nil)
	if !strings.Contains(got, "Called:\n(none)") {
		t.Errorf("a round that called nothing left no trace:\n%s", got)
	}
}

func TestMetaToolsAreFilteredFromTheCallList(t *testing.T) {
	t.Parallel()
	// A meta-tool is never a delivery, so in a record whose only job is
	// "what already happened that matters" it is pure noise.
	got := RenderIterations([]Iteration{{
		Iteration: 1,
		Calls:     []Call{{Name: "activate_tool"}, {Name: "slack_post"}},
	}}, []string{"activate_tool"})

	if strings.Contains(got, "activate_tool") {
		t.Errorf("a skipped meta-tool rendered:\n%s", got)
	}
	if !strings.Contains(got, "slack_post") {
		t.Errorf("the real call was filtered too:\n%s", got)
	}
}

func TestAnIterationRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	// A detached sandbox run ends the turn and its completion starts a NEW
	// one. Without this round-trip the resumed turn forgets every earlier
	// round and can re-fire its deliveries — the exact bug the ledger
	// exists to prevent, reached by a different road.
	want := Iteration{
		Iteration:     2,
		Intent:        "retry the post",
		Calls:         []Call{{Name: "slack_post", Args: map[string]any{"channel": "C0ENG"}, Failed: true, Result: "rate limited"}},
		Reads:         []string{"jira_get_issue"},
		Text:          "draft",
		CompletedWork: "nothing landed",
	}
	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Iteration
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if RenderIterations([]Iteration{got}, nil) != RenderIterations([]Iteration{want}, nil) {
		t.Errorf("round-trip changed what the ledger says:\n%s", blob)
	}
}

func TestARowFromAnOlderEngineDecodesToLessContextNotAnError(t *testing.T) {
	t.Parallel()
	// Losing a field costs the next turn some history; raising would cost
	// it the whole turn.
	var it Iteration
	if err := json.Unmarshal([]byte(`{"iteration":3}`), &it); err != nil {
		t.Fatalf("a sparse row failed to decode: %v", err)
	}
	if it.Iteration != 3 {
		t.Errorf("iteration = %d, want 3", it.Iteration)
	}
	var s Session
	if err := json.Unmarshal([]byte(`{"reply":"ok"}`), &s); err != nil {
		t.Fatalf("a sparse session failed to decode: %v", err)
	}
	if s.Reply != "ok" {
		t.Errorf("reply = %q", s.Reply)
	}
}

// ELIDE PAYLOADS, NEVER STRUCTURE — asserted on the write path, which is
// where the distinction is permanent. This row is the store's only record of
// the turn, so a field cut here is not a shortened rendering, it is the only
// copy. Every structural field must survive whole; only a tool ARGUMENT is
// elided, and the discriminating argument must survive that.
func TestBuildSessionKeepsStructureWholeAndElidesOnlyArguments(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("z", 6000)
	got := BuildSession(SessionInput{
		Trigger: long, Intent: long,
		Reply: long, CompletedWork: long,
		Calls: []Call{{Name: "slack_post", Args: map[string]any{"text": long, "channel": "C1"}}},
	})
	for _, c := range []struct {
		name  string
		value string
	}{
		{"trigger", got.Trigger},
		{"intent", got.Intent},
		{"reply", got.Reply},
		{"completed work", got.CompletedWork},
	} {
		if c.value != long {
			t.Errorf("%s was cut at write time: %d runes of %d",
				c.name, utf8.RuneCountInString(c.value), utf8.RuneCountInString(long))
		}
	}
	// The argument payload IS elided — that is the half of the principle
	// this package keeps — and the identifier beside it survives.
	if utf8.RuneCountInString(got.Calls) > BlobLimit+len("- slack_post() → success")+8 {
		t.Errorf("the argument blob was not elided:\n%s", got.Calls)
	}
	if !strings.Contains(got.Calls, "C1") {
		t.Errorf("the discriminating argument was lost at write time:\n%s", got.Calls)
	}
}

// The RENDER is bounded, the RECORD is not — and the drop is reported. A
// silently shortened history reads as the whole conversation, and a seat that
// believes it has seen everything it said will not go and look for the rest.
func TestATrimmedHistorySaysHowMuchItDropped(t *testing.T) {
	t.Parallel()
	entries := make([]Session, 6)
	for i := range entries {
		entries[i] = Session{TurnID: itoa(i), Reply: strings.Repeat("z", 5000)}
	}
	got := RenderHistory(entries, HistoryOptions{MaxChars: InjectedMaxChars})
	if len(got) > InjectedMaxChars+500 {
		t.Errorf("the rendered block is %d bytes, past its bound", len(got))
	}
	if !strings.Contains(got, "are not shown") {
		t.Errorf("entries were dropped silently:\n%s", got[:200])
	}
	// WHOLE ENTRIES. A cut inside one would leave a half-recorded reply
	// reading as the whole of what the seat said.
	if strings.Count(got, strings.Repeat("z", 5000)) < 1 {
		t.Errorf("an entry was cut rather than dropped:\n%s", got[:200])
	}
	// The newest always survives, however long: a block trimmed to nothing
	// tells the next turn this conversation has no history.
	solo := RenderHistory([]Session{{Reply: strings.Repeat("d", InjectedMaxChars*2)}},
		HistoryOptions{MaxChars: InjectedMaxChars})
	if !strings.Contains(solo, strings.Repeat("d", InjectedMaxChars*2)) {
		t.Error("the only entry was dropped, so the turn reads as having no history")
	}
}

// A prior round's produced text is kept WHOLE in the record and TAIL-elided
// when rendered — the deliverable is what the round ended with, not what it
// opened by thinking, and Execution.Text is the whole tool loop concatenated.
func TestAPriorRoundsOutputIsTailElidedNotHeadCut(t *testing.T) {
	t.Parallel()
	produced := "<think>" + strings.Repeat("reasoning ", 2000) + "</think>\nTHE DRAFT ENDS HERE."
	got := RenderIterations([]Iteration{{Iteration: 1, Text: produced}}, nil)
	if !strings.Contains(got, "THE DRAFT ENDS HERE.") {
		t.Error("the deliverable at the end of the round was cut away")
	}
	if strings.Count(got, "reasoning reasoning") > RenderedArtifactLimit {
		t.Error("the block was not bounded at all")
	}
	if !strings.Contains(got, "…") {
		t.Error("the cut is silent")
	}
	// And the record itself is untouched: this is a render bound.
	whole := RenderIterations([]Iteration{{Iteration: 1, Text: "short"}}, nil)
	if !strings.Contains(whole, "Produced: short") {
		t.Errorf("a short round was altered: %q", whole)
	}
}

func TestNoHistoryRendersNothing(t *testing.T) {
	t.Parallel()
	if got := RenderHistory(nil, HistoryOptions{}); got != "" {
		t.Errorf("an empty history rendered %q", got)
	}
}

func TestHistoryKeepsTheNewestEntries(t *testing.T) {
	t.Parallel()
	// Recency is what a follow-up turn needs: the message it is answering
	// is the newest one, and the turn before it is the one most likely to
	// have already answered it.
	entries := []Session{{Reply: "first"}, {Reply: "second"}, {Reply: "third"}}
	got := RenderHistory(entries, HistoryOptions{MaxEntries: 2})
	if strings.Contains(got, "first") {
		t.Errorf("MaxEntries kept the oldest:\n%s", got)
	}
	for _, want := range []string{"second", "third"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHistoryDropsFromTheOldestEndAndAlwaysKeepsOne(t *testing.T) {
	t.Parallel()
	entries := []Session{
		{Reply: strings.Repeat("a", 200)},
		{Reply: strings.Repeat("b", 200)},
		{Reply: strings.Repeat("c", 200)},
	}
	got := RenderHistory(entries, HistoryOptions{MaxChars: 250})
	if strings.Contains(got, "aaa") {
		t.Errorf("the oldest entry survived a char budget:\n%s", got)
	}
	if !strings.Contains(got, "ccc") {
		t.Errorf("the newest entry was dropped:\n%s", got)
	}

	// A single entry over budget still renders. A block trimmed to nothing
	// tells the next turn this conversation has no history, which is the
	// one thing it must not conclude.
	solo := RenderHistory([]Session{{Reply: strings.Repeat("d", 900)}}, HistoryOptions{MaxChars: 10})
	if solo == "" {
		t.Error("an over-budget lone entry was dropped, so history read as empty")
	}
}

func TestASessionReadsAsTheSeatsOwnPast(t *testing.T) {
	t.Parallel()
	got := renderSession(Session{
		TurnID:  "0189d4c2-aaaa-bbbb-cccc-ddddddddddd0",
		At:      "2026-08-20T09:00:00Z",
		Trigger: "@alice: can you repost the summary?",
		Intent:  "repost with the fixed link",
		Calls:   "- slack_post({\"channel\":\"C1\"}) → success",
		Reply:   "Reposted with the corrected link.",
	})
	for _, want := range []string{
		"### 2026-08-20T09:00:00Z (turn 0189d4c2)",
		"Triggered by: @alice",
		"You set out to: repost",
		"You called:",
		"You replied: Reposted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestAnUnremarkableEndingIsNotAnnounced(t *testing.T) {
	t.Parallel()
	// Saying "Turn ended: done" on every entry trains the reader to skip
	// the line — which is the line that says a turn FAILED.
	if got := renderSession(Session{Reply: "hi", Decision: "done"}); strings.Contains(got, "Turn ended") {
		t.Errorf("a routine ending was announced:\n%s", got)
	}
	if got := renderSession(Session{Reply: "hi", Decision: "failed"}); !strings.Contains(got, "Turn ended: failed") {
		t.Errorf("a failure was not announced:\n%s", got)
	}
}

func TestAShortTurnIDDoesNotPanic(t *testing.T) {
	t.Parallel()
	// The id is a string the caller supplies. Slicing [:8] blindly panics
	// on anything shorter, and a panic here takes down the turn that was
	// only trying to describe itself.
	if got := renderSession(Session{TurnID: "abc"}); !strings.Contains(got, "turn abc") {
		t.Errorf("a short id did not render: %s", got)
	}
}

func TestAnEntryWithNoTimeStillHasAHeading(t *testing.T) {
	t.Parallel()
	if got := renderSession(Session{Reply: "hi"}); !strings.HasPrefix(got, "### Earlier turn") {
		t.Errorf("a timeless entry lost its heading:\n%s", got)
	}
}
