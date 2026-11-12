package ledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
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

// ONE READ CAP SERVES EVERY GROUP OF A RENDER. A brief that tells one round's
// calls under several headings (a continuation's calls after its save and its
// late calls) renders them as groups, and the cap counts the reads of all of
// them together, spent in the order the groups come: a group shows the reads
// the groups before it left, and its own omitted line counts its own reads
// left out. Every write renders, a skipped name never does, and an empty group
// still says "(none)". The reads the groups show are those one list of the
// same calls shows, in the same order.
//
// Mutation: count the reads of each group from zero, and the render shows
// sixteen reads where the cap is twelve. Spend the groups in reverse, or count
// a group's omitted line over every group, and the lines below are wrong.
func TestOneReadCapServesEveryGroup(t *testing.T) {
	t.Parallel()
	read := func(n int) Call { return Call{Name: "get", Args: map[string]any{"n": n}} }
	write := func(group int) Call { return Call{Name: "post", Args: map[string]any{"group": group}} }
	opts := Format([]string{"activate_tool"}, []string{"get"})
	var groups [][]Call
	made := 0
	for g, reads := range []int{8, 8, 3, 2} {
		var calls []Call
		for range reads {
			made++
			calls = append(calls, read(made))
		}
		if g < 3 {
			calls = append(calls, write(g), Call{Name: "activate_tool", Args: map[string]any{"group": g}})
		}
		groups = append(groups, calls)
	}
	groups = append(groups, nil)
	line := func(call Call) string { return FormatCalls([]Call{call}, opts) }

	got := FormatCallGroups(groups, opts)
	if len(got) != len(groups) {
		t.Fatalf("%d renders for %d groups", len(got), len(groups))
	}
	if reads := strings.Count(strings.Join(got, "\n"), "(read)"); reads != MaxReadCalls {
		t.Errorf("the groups show %d reads in all, want the one cap of %d:\n%s", reads, MaxReadCalls,
			strings.Join(got, "\n---\n"))
	}
	for g, want := range []struct {
		shown   []int
		omitted string
	}{
		{shown: []int{1, 8}},
		{shown: []int{9, 12}, omitted: "(+4 further read call(s) omitted)"},
		{omitted: "(+3 further read call(s) omitted)"},
		{omitted: "(+2 further read call(s) omitted)"},
	} {
		for n := 1; n <= 21; n++ {
			shown := len(want.shown) == 2 && n >= want.shown[0] && n <= want.shown[1]
			if strings.Contains(got[g], line(read(n))) != shown {
				t.Errorf("group %d shows read %d: %t, want %t:\n%s", g, n, !shown, shown, got[g])
			}
		}
		if want.omitted == "" && strings.Contains(got[g], "omitted") {
			t.Errorf("group %d counts reads it showed as omitted:\n%s", g, got[g])
		}
		if want.omitted != "" && !strings.Contains(got[g], want.omitted) {
			t.Errorf("group %d does not say %q:\n%s", g, want.omitted, got[g])
		}
		if g < 3 && !strings.Contains(got[g], line(write(g))) {
			t.Errorf("group %d dropped its write to fit the read cap:\n%s", g, got[g])
		}
		if strings.Contains(got[g], "activate_tool") {
			t.Errorf("group %d renders a skipped name:\n%s", g, got[g])
		}
	}
	if got[3] != "- (+2 further read call(s) omitted)" {
		t.Errorf("a group whose every read is past the cap reads %q, want its omitted line alone", got[3])
	}
	if got[4] != noCalls {
		t.Errorf("an empty group reads %q, want %q", got[4], noCalls)
	}

	one := FormatCalls(slices.Concat(groups...), opts)
	for n := 1; n <= 21; n++ {
		if strings.Contains(one, line(read(n))) != strings.Contains(strings.Join(got, "\n"), line(read(n))) {
			t.Errorf("read %d is shown by one of the render and the list of the same calls, not both", n)
		}
	}
	for g, calls := range groups {
		if FormatCalls(calls, opts) != FormatCallGroups([][]Call{calls}, opts)[0] {
			t.Errorf("FormatCalls and the one-group render part on group %d", g)
		}
	}
}

// THE GROUPS' PIECES ARE THOSE THEIR RENDER SHOWS. Fitted over the groups'
// concatenation, every payload past its budget the groups show is named, so
// none renders whole, and nothing is named that the one cap leaves out, so no
// rewrite is spent on a read the brief never shows.
//
// Mutation: let the pieces ignore the read cap, as naming them group by group
// under a cap each does, and the read the one cap leaves out of the later group
// is named all the same.
func TestTheGroupsPiecesAreThoseTheirRenderShows(t *testing.T) {
	t.Parallel()
	payload := func(n int) string { return fmt.Sprintf("%03d %s", n, strings.Repeat("p", 400)) }
	opts := Format(nil, []string{"search"})
	var first, second []Call
	for n := range MaxReadCalls {
		first = append(first, Call{Name: "search", Args: map[string]any{"query": payload(n)}})
	}
	second = append(second,
		Call{Name: "search", Args: map[string]any{"query": payload(100)}},
		Call{Name: "post", Args: map[string]any{"text": payload(101)}},
		Call{Name: "post", Failed: true, Result: payload(102)},
	)
	groups := [][]Call{first, second}

	pieces := CallPieces(slices.Concat(groups...), opts)
	opts.Fitted = Fitted{}
	for i, p := range pieces {
		opts.Fitted[p] = fmt.Sprintf("(rewrite %d)", i)
	}
	told := strings.Join(FormatCallGroups(groups, opts), "\n")
	for i := range pieces {
		if !strings.Contains(told, fmt.Sprintf("(rewrite %d)", i)) {
			t.Errorf("piece %.12q is fitted and the groups never show it:\n%s", pieces[i].Text, told)
		}
	}
	if strings.Contains(told, strings.Repeat("p", 400)) {
		t.Errorf("the groups show a payload past its budget whole:\n%s", told)
	}
	if len(pieces) != MaxReadCalls+2 {
		t.Errorf("%d pieces, want the %d shown reads' queries, the post's text and its error", len(pieces),
			MaxReadCalls)
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
	// A detached executor ends the turn's segment and its completion starts
	// a NEW one. Without this round-trip the resumed turn forgets every
	// earlier round and can re-fire its deliveries — the exact bug the
	// ledger exists to prevent, reached by a different road.
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
	// Losing a field costs the next round some of its account; raising would
	// cost it the whole turn.
	var it Iteration
	if err := json.Unmarshal([]byte(`{"iteration":3}`), &it); err != nil {
		t.Fatalf("a sparse row failed to decode: %v", err)
	}
	if it.Iteration != 3 {
		t.Errorf("iteration = %d, want 3", it.Iteration)
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

// AN OBJECT IS WEIGHED BY THE LINE'S OWN JSON, unescaped too. A value that is
// not a string is judged by its JSON, and that JSON is what a rewrite is handed
// and what the line shows when none can be had — so judged by `json.Marshal`'s,
// an object of markup weighed six bytes for every `<`: one the line carries
// whole in fewer bytes than its budget was fitted, and the rewrite was handed
// `\u003c` escapes to decode. Mutation: weigh the value with `json.Marshal`
// and the first half's object becomes a piece, the second half's text escaped.
func TestAnObjectArgumentIsWeighedByTheLinesOwnJSON(t *testing.T) {
	t.Parallel()
	markup := map[string]any{"html": strings.Repeat("<b>", 40)}
	if p, ok := valuePiece(markup, 150); ok {
		t.Errorf("an object whose line is %d bytes was fitted to a budget of 150: %q",
			len(marshal(markup)), p.Text)
	}
	p, ok := valuePiece(map[string]any{"html": strings.Repeat("<b>", 100)}, 150)
	if !ok {
		t.Fatal("an object past its budget was carried whole")
	}
	if strings.Contains(p.Text, `\u003c`) || !strings.Contains(p.Text, "<b><b>") {
		t.Errorf("the piece a rewrite is handed reads %.80q, want the markup as written",
			p.Text)
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
	produced := strings.Repeat("the draft goes on ", 2000) + "\nTHE DRAFT ENDS HERE."
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
