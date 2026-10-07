package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

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
	if got := RenderIterations(nil, nil, nil); got != "" {
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
	}}, []string{"activate_tool"}, nil)

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
	got := RenderIterations([]Iteration{{Iteration: 1, Intent: "triage"}}, nil, nil)
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
	}}, []string{"activate_tool"}, nil)

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
	if RenderIterations([]Iteration{got}, nil, nil) != RenderIterations([]Iteration{want}, nil, nil) {
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

func TestNoHistoryRendersNothing(t *testing.T) {
	t.Parallel()
	if got := RenderHistory(nil, HistoryOptions{}); got != "" {
		t.Errorf("an empty history rendered %q", got)
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
		"### 2026-08-20T09:00:00Z (turn 0189d4c2-aaaa-bbbb-cccc-ddddddddddd0)",
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

func TestAnEntryWithNoTimeStillHasAHeading(t *testing.T) {
	t.Parallel()
	if got := renderSession(Session{Reply: "hi"}); !strings.HasPrefix(got, "### Earlier turn") {
		t.Errorf("a timeless entry lost its heading:\n%s", got)
	}
}

// A LEDGER LINE IS READ, by the model every round and by a person on the seat
// screen. `json.Marshal` escapes `<`, `>` and `&` for an HTML document, and
// neither audience is one: a URL argument rendered `?a=1\\u0026b=2` costs six
// bytes of the budget this package exists to spend well, and reads as noise.
func TestRenderedArgumentsAreNotEscapedForHTML(t *testing.T) {
	t.Parallel()
	got := renderArgs(map[string]any{
		"url": "https://example.com/s?a=1&b=2",
		"md":  "<b>x</b>",
	}, FormatOptions{})
	if strings.Contains(got, `\u0026`) || strings.Contains(got, `\u003c`) {
		t.Errorf("renderArgs = %s, want the characters rather than their escapes", got)
	}
	if !strings.Contains(got, "?a=1&b=2") || !strings.Contains(got, "<b>x</b>") {
		t.Errorf("renderArgs = %s, want both arguments verbatim", got)
	}
	// STILL ONE LINE. A ledger is joined with newlines, so a stray one from
	// the encoder would split a call across two entries.
	if strings.Contains(got, "\n") {
		t.Errorf("renderArgs = %q, want one line", got)
	}
	// STILL JSON.
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("renderArgs produced something that is not JSON: %v", err)
	}
}

// A PAYLOAD IS FITTED AND THE DISCRIMINATOR IS NOT. The bug the per-value
// design prevents is still the one that matters — a line that kept the body
// but lost `channel` hides which of two posts happened — so the identifier is
// never a piece, and the body the caller rewrote is what renders in its place.
func TestAPayloadIsFittedAndTheDiscriminatorIsNot(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("prose ", 400)
	calls := []Call{{Name: "slack_post", Args: map[string]any{"channel": "C0ENGINEERING", "text": body}}}
	opts := Format(nil, nil)
	pieces := CallPieces(calls, opts)
	if len(pieces) != 1 || pieces[0].Text != body || pieces[0].Kind != PieceArgument {
		t.Fatalf("pieces = %+v, want the body alone", pieces)
	}
	opts.Fitted = Fitted{pieces[0]: "(condensed) a long note to the engineering channel"}
	got := FormatCalls(calls, opts)
	if !strings.Contains(got, "C0ENGINEERING") {
		t.Errorf("the discriminating argument was lost:\n%s", got)
	}
	if !strings.Contains(got, "(condensed) a long note to the engineering channel") {
		t.Errorf("the rewrite did not render in the body's place:\n%s", got)
	}
	if strings.Contains(got, body) {
		t.Error("the whole body rendered although a rewrite was supplied")
	}
}

// AN UNFITTED PAYLOAD RENDERS WHOLE — NEVER CUT. A caller that could not have
// a payload rewritten gets prompt weight, not a fragment reading as the
// payload; this is the mutation that would quietly reinstate the old trim.
func TestAnUnfittedPayloadRendersWholeNeverCut(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("prose ", 400)
	errDoc := "<html>" + strings.Repeat("gateway ", 200) + "missing scope chat:write</html>"
	got := FormatCalls([]Call{
		{Name: "slack_post", Args: map[string]any{"text": body}},
		{Name: "slack_post", Args: map[string]any{"text": "hi"}, Failed: true, Result: errDoc},
	}, Format(nil, nil))
	if !strings.Contains(got, body) {
		t.Error("an unfitted argument was cut")
	}
	if !strings.Contains(got, "missing scope chat:write") {
		t.Error("an unfitted error was cut — the line naming the cause is at its end")
	}
	if strings.Contains(got, "…") {
		t.Errorf("a render with nothing fitted still marked a cut:\n%s", got)
	}
}

// AN IDENTIFIER IS JUDGED IN CHARACTERS, so a channel name in a script whose
// characters are three bytes each is never handed to a model to paraphrase.
func TestAMultiByteIdentifierIsNeverAPiece(t *testing.T) {
	t.Parallel()
	channel := strings.Repeat("営", 150) // 150 runes, 450 bytes
	pieces := CallPieces([]Call{{Name: "post", Args: map[string]any{"channel": channel}}}, Format(nil, nil))
	if len(pieces) != 0 {
		t.Fatalf("a %d-rune identifier became a piece: %+v", 150, pieces)
	}
}

// A FAILED CALL'S ERROR IS A PIECE; A SUCCESSFUL CALL'S RESULT IS NOT CARRIED
// AT ALL, and a skipped meta-tool contributes nothing to fit.
func TestCallPiecesNamesExactlyWhatARenderShows(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("e", 900)
	pieces := CallPieces([]Call{
		{Name: "activate_tool", Args: map[string]any{"name": long}},
		{Name: "get", Result: long},
		{Name: "post", Failed: true, Result: long},
		{Name: "post", Failed: true, Result: long},
	}, Format([]string{"activate_tool"}, nil))
	if len(pieces) != 1 || pieces[0].Kind != PieceError {
		t.Fatalf("pieces = %+v, want the one failed result, once", pieces)
	}
}

// A NON-STRING VALUE PAST THE BUDGET IS A PIECE OVER ITS JSON; under it, it
// keeps its native type.
func TestALargeObjectArgumentIsFittedAsItsJSON(t *testing.T) {
	t.Parallel()
	big := map[string]any{"rows": strings.Split(strings.Repeat("row,", 100), ",")}
	pieces := CallPieces([]Call{{Name: "upsert", Args: map[string]any{"payload": big, "n": 3}}}, Format(nil, nil))
	if len(pieces) != 1 || !strings.HasPrefix(pieces[0].Text, `{"rows":`) {
		t.Fatalf("pieces = %+v", pieces)
	}
}

// A PRIOR ROUND'S OUTPUT IS FITTED AS A WHOLE, deliverable included — and
// with nothing fitted it renders whole rather than from either end.
func TestAPriorRoundsOutputIsFittedNotCut(t *testing.T) {
	t.Parallel()
	produced := "<think>" + strings.Repeat("reasoning ", 2000) + "</think>\nTHE DRAFT ENDS HERE."
	records := []Iteration{{Iteration: 1, Text: produced}}
	pieces := IterationPieces(records, nil)
	if len(pieces) != 1 || pieces[0].Kind != PieceProduced || pieces[0].Limit != RenderedArtifactLimit {
		t.Fatalf("pieces = %+v", pieces)
	}
	fitted := RenderIterations(records, nil, Fitted{pieces[0]: "the draft, condensed"})
	if !strings.Contains(fitted, "Produced: the draft, condensed") {
		t.Errorf("the rewrite did not render:\n%s", fitted)
	}
	whole := RenderIterations(records, nil, nil)
	if !strings.Contains(whole, produced) {
		t.Error("an unfitted round output was cut")
	}
	if short := RenderIterations([]Iteration{{Iteration: 1, Text: "short"}}, nil, nil); !strings.Contains(short, "Produced: short") {
		t.Errorf("a short round was altered: %q", short)
	}
}

// THE RECORD IS VERBATIM, apart from the payloads the caller fitted — asserted
// on the write path, where the distinction is permanent: this row is the
// store's only record of the turn.
func TestBuildSessionKeepsStructureWholeAndFitsOnlyPayloads(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("z", 6000)
	in := SessionInput{
		Trigger: long, Intent: long,
		Reply: long, Delivered: true, CompletedWork: long,
		Calls: []Call{{Name: "slack_post", Args: map[string]any{"text": long, "channel": "C1"}}},
	}
	pieces := SessionPieces(in)
	if len(pieces) != 1 || pieces[0].Text != long {
		t.Fatalf("pieces = %+v", pieces)
	}
	in.Fitted = Fitted{pieces[0]: "(condensed) a status update"}
	got := BuildSession(in)
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
	if !strings.Contains(got.Calls, "(condensed) a status update") || !strings.Contains(got.Calls, "C1") {
		t.Errorf("the session row lost the rewrite or the identifier:\n%s", got.Calls)
	}
	if un := BuildSession(SessionInput{Reply: long}); un.Unsent != long {
		t.Errorf("unsent was cut at write time: %d runes of %d",
			utf8.RuneCountInString(un.Unsent), utf8.RuneCountInString(long))
	}
}

// THE NEWEST ENTRIES VERBATIM, THE OLDER ONES CONDENSED: a history past its
// bound keeps the turns a follow-up is answering whole, and gives the rest to
// the caller as one block to rewrite.
func TestSplitHistoryKeepsTheNewestWholeAndOverflowsTheOldest(t *testing.T) {
	t.Parallel()
	entries := make([]Session, 6)
	for i := range entries {
		entries[i] = Session{TurnID: itoa(i), Reply: strings.Repeat(string(rune('a'+i)), 5000)}
	}
	overflow, kept := SplitHistory(entries, InjectedMaxChars)
	if len(overflow) == 0 || len(kept) == 0 || len(overflow)+len(kept) != len(entries) {
		t.Fatalf("split %d / %d of %d", len(overflow), len(kept), len(entries))
	}
	if kept[len(kept)-1].TurnID != "5" || overflow[0].TurnID != "0" {
		t.Fatalf("the split is not oldest-overflow, newest-kept")
	}
	if len(RenderSessions(kept)) > InjectedMaxChars-InjectedMaxChars/EarlierShare {
		t.Errorf("the verbatim half took the condensed account's share")
	}
	if over, all := SplitHistory(entries[:1], InjectedMaxChars); len(over) != 0 || len(all) != 1 {
		t.Error("a history within its bound was split")
	}
	// The newest always survives whole, however long.
	solo := []Session{{Reply: strings.Repeat("d", InjectedMaxChars*2)}}
	if over, all := SplitHistory(solo, InjectedMaxChars); len(over) != 0 || len(all) != 1 {
		t.Error("the only entry was overflowed, so the turn reads as having no history")
	}
}

// A CONDENSED ACCOUNT IS ANNOUNCED AS ONE, and where none could be had the
// block says how many entries it left out — never silently shorter.
func TestRenderHistorySaysWhatItCondensedOrLeftOut(t *testing.T) {
	t.Parallel()
	entries := make([]Session, 6)
	for i := range entries {
		entries[i] = Session{TurnID: itoa(i), Reply: strings.Repeat("z", 5000)}
	}
	overflow, _ := SplitHistory(entries, InjectedMaxChars)
	condensed := RenderHistory(entries, HistoryOptions{MaxChars: InjectedMaxChars, Earlier: "you posted the plan to #eng"})
	if !strings.Contains(condensed, "Earlier in this conversation ("+itoa(len(overflow))+" turn(s), condensed)") ||
		!strings.Contains(condensed, "you posted the plan to #eng") {
		t.Errorf("the condensed account was not rendered as one:\n%s", condensed[:300])
	}
	if strings.Contains(condensed, "are not shown") {
		t.Error("a condensed history also claimed its turns were not shown")
	}
	dropped := RenderHistory(entries, HistoryOptions{MaxChars: InjectedMaxChars})
	if !strings.Contains(dropped, itoa(len(overflow))+" earlier turn(s) in this conversation are not shown") {
		t.Errorf("entries were left out silently:\n%s", dropped[:300])
	}
	// WHOLE ENTRIES: nothing inside an entry is ever cut.
	if strings.Contains(dropped, "…") || strings.Count(dropped, strings.Repeat("z", 5000)) != len(entries)-len(overflow) {
		t.Error("an entry was cut rather than kept whole")
	}
}
