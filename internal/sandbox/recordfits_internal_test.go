package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// A QUESTION PAST ITS BOUND IS CONDENSED, as the question it is: it travels on
// the run's record, its coordination row, the park's announcement, a person's
// banner and the resumed executor's every later round, and a run's ask file
// was read whole up to 32 MiB.
func TestAQuestionPastItsBoundIsCondensedAsAQuestion(t *testing.T) {
	t.Parallel()
	condenser := &fakeCondenser{answer: func(part RunPart, _ string, _ int) (string, error) {
		return "(condensed) which branch, main or release/2?", nil
	}}
	c := &Coordinator{condense: condenser}
	question := "Which branch should the fix target?\n" + strings.Repeat("context line\n", 4000)
	got := c.fitResult(t.Context(), PendingRun{AgentHandle: "dev", TurnID: "t1"},
		Result{NeedsInput: true, Question: question, AskTo: "team"})
	if got.Question != "(condensed) which branch, main or release/2?" || !got.NeedsInput {
		t.Errorf("question = %q, needs input %v; want the rewrite, still asked", got.Question, got.NeedsInput)
	}
	want := condenseCall{"dev", "t1", PartQuestion, len(question), MaxQuestionBytes}
	if len(condenser.calls) != 1 || condenser.calls[0] != want {
		t.Errorf("condense calls = %+v; want %+v", condenser.calls, want)
	}

	// Within the bound, nobody is asked to rewrite it.
	short := c.fitResult(t.Context(), PendingRun{}, Result{NeedsInput: true, Question: "main or release?"})
	if short.Question != "main or release?" || len(condenser.calls) != 1 {
		t.Errorf("a short question changed or was sent to a model: %q", short.Question)
	}
}

// NO MODEL, NO CUT: whole lines from the question's start, the rest counted —
// and a question whose first line alone is past the bound is NOT ASKED, since
// whole lines leave nobody anything to answer and a fragment would be a
// question the agent never asked.
func TestAnUncondensedQuestionKeepsWholeLinesOrIsNotAsked(t *testing.T) {
	t.Parallel()
	c := &Coordinator{}
	question := "Which branch should the fix target?\n" + strings.Repeat("context line\n", 4000)
	got := c.fitResult(t.Context(), PendingRun{}, Result{NeedsInput: true, Question: question, AskTo: "team"})
	if !got.NeedsInput || !strings.HasPrefix(got.Question, "Which branch should the fix target?\n") ||
		!strings.Contains(got.Question, "later line(s)") || !strings.Contains(got.Question, "16 KiB") {
		t.Errorf("an uncondensed question = %.80q … %q", got.Question, got.Question[max(0, len(got.Question)-120):])
	}
	if len(got.Question) > MaxQuestionBytes+256 {
		t.Errorf("the question is %d bytes, past its bound and its note", len(got.Question))
	}

	oneLine := strings.Repeat("why ", MaxQuestionBytes)
	refused := c.fitResult(t.Context(), PendingRun{},
		Result{NeedsInput: true, Question: oneLine, AskTo: "team", Text: "Outcome: blocked"})
	if refused.NeedsInput || refused.Question != "" || refused.Success {
		t.Errorf("a question no line of which fits: needs input %v, question %d bytes, success %v",
			refused.NeedsInput, len(refused.Question), refused.Success)
	}
	if !strings.Contains(refused.Error, "nobody was asked it") || refused.Text != "Outcome: blocked" {
		t.Errorf("the run does not say why it was not parked: error %q, report %q", refused.Error, refused.Text)
	}

	// THE REFUSAL IS INSIDE THE FAILURE'S BOUND, beside a failure already at
	// it: the question is fitted first, so the failure that carries the
	// refusal is fitted after. Fitted the other way round, the refusal rode
	// past the bound the record's arithmetic counts on.
	//
	// Mutation: fit the question after the failure, and this goes red.
	full := strings.Repeat("a failing line\n", MaxRunTextBytes/len("a failing line\n")+1)
	refused = c.fitResult(t.Context(), PendingRun{},
		Result{NeedsInput: true, Question: oneLine, Error: full})
	if !strings.HasPrefix(refused.Error, "the question the coding agent asked") {
		t.Errorf("the failure lost the refusal it begins with: %.120q", refused.Error)
	}
	// The bound counts content; the note standing for what was left out
	// is not counted against it ([wholeLines]).
	content := refused.Error
	if i := strings.Index(content, "\n("); i >= 0 {
		if j := strings.Index(content[i+1:], ")\n"); j >= 0 {
			content = content[:i+1] + content[i+1+j+2:]
		}
	}
	if len(content) > MaxRunTextBytes {
		t.Errorf("the failure carrying the refusal is %d bytes of content, past its %d", len(content), MaxRunTextBytes)
	}
}

// A REF IS REDACTED BEFORE ANYTHING READS IT. It is whatever word a report
// put on its `Delivered:` line, and a box holds the seat's credentials — so
// the coordinator, which trusts no runner to have redacted what it hands back,
// redacts each before the record, the resumed executor's text and the bound
// that measures them.
func TestADeliveredRefIsRedactedBeforeItIsRecorded(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("c", 36)
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{"wip/t1", secret}})
	rec := runPhase(PendingRun{}, LaunchRecord{}, got, time.Now())
	for where, refs := range map[string][]string{
		"the result": got.DeliveredRefs, "the record": rec.DeliveredRefs,
		"the resumed text": {resumeText(PendingRun{}, got)},
	} {
		if strings.Contains(strings.Join(refs, " "), secret) {
			t.Errorf("a credential survived in %s: %q", where, refs)
		}
	}
	if len(got.DeliveredRefs) != 2 || got.DeliveredRefs[0] != "wip/t1" {
		t.Errorf("refs = %q, want both, the ordinary one as it was", got.DeliveredRefs)
	}
}

// THE REFS ARE DEDUPLICATED AND BOUNDED, the rest counted. They are read from
// the whole report with no count to them, so a report that listed pull
// requests put every one of them on the record, as many times as it named
// them.
func TestDeliveredRefsAreDedupedAndBoundedWithTheRestCounted(t *testing.T) {
	t.Parallel()
	var refs []string
	for i := range 2000 {
		refs = append(refs, fmt.Sprintf("https://github.com/acme/api/pull/%d", i))
		refs = append(refs, fmt.Sprintf("https://github.com/acme/api/pull/%d", i)) // named twice
	}
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{}, Result{Success: true, DeliveredRefs: refs})

	size := 0
	seen := map[string]bool{}
	for _, ref := range got.DeliveredRefs {
		if seen[ref] {
			t.Fatalf("%s is listed twice", ref)
		}
		seen[ref] = true
		size += len(ref)
	}
	if size > MaxDeliveredRefBytes || got.DeliveredRefs[0] != refs[0] {
		t.Errorf("listed %d bytes of refs from %q; want the first found, within %d",
			size, got.DeliveredRefs[0], MaxDeliveredRefBytes)
	}
	if len(got.DeliveredRefs)+got.DeliveredRefsElided != 2000 {
		t.Errorf("listed %d and counted %d more; want the 2000 distinct refs accounted for",
			len(got.DeliveredRefs), got.DeliveredRefsElided)
	}

	text := resumeText(PendingRun{}, got)
	if !strings.Contains(text, fmt.Sprintf("(and %d more its report names", got.DeliveredRefsElided)) {
		t.Errorf("the resumed executor is not told the list is short: %.300q", text)
	}
	rec := runPhase(PendingRun{}, LaunchRecord{}, got, time.Now())
	if rec.DeliveredRefsElided != got.DeliveredRefsElided || len(rec.DeliveredRefs) != len(got.DeliveredRefs) {
		t.Errorf("the record lists %d and counts %d", len(rec.DeliveredRefs), rec.DeliveredRefsElided)
	}
}

// REFS THE RECORD LISTS NONE OF ARE STILL SAID. A ref longer than the bound is
// past it on its own, so a record can list nothing and count what the run
// named; the resumed executor's text then said nothing of it at all, and the
// run read to the executor as one that delivered nothing, while the record and
// the dashboard both counted the ref.
func TestTheResumedExecutorIsToldOfRefsTheRecordListsNoneOf(t *testing.T) {
	t.Parallel()
	long := "https://h/a/b/pull/1?x=" + strings.Repeat("y", MaxDeliveredRefBytes)
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{long}})
	if len(got.DeliveredRefs) != 0 || got.DeliveredRefsElided != 1 {
		t.Fatalf("listed %d and counted %d more; want none listed and the one ref counted",
			len(got.DeliveredRefs), got.DeliveredRefsElided)
	}
	text := resumeText(PendingRun{}, got)
	if !strings.Contains(text, "Delivered: 1 ref(s) its report names, not listed here") {
		t.Errorf("the resumed executor is not told of the ref it is not shown: %q", text)
	}

	// A run that named no ref is told of none.
	text = resumeText(PendingRun{}, Result{Success: true, Text: "Outcome: succeeded"})
	if strings.Contains(text, "Delivered:") {
		t.Errorf("a run that named no ref is told of one: %q", text)
	}
}

// A REF THE RECORD WOULD RENAME IS COUNTED, NEVER LISTED. Every reader of the
// refs reads them through JSON — the run's phase record, the resumed
// executor's text — where a byte that is not UTF-8 is replaced: listed, such
// a ref arrived as another, a URL the run never delivered, and refs that
// differ only there arrived as one. A URL holds whatever bytes a report puts
// in it, on a `Delivered:` line or in the host a scrape reads, so it is
// judged here, where every runner's refs pass.
func TestADeliveredRefThatIsNotUTF8IsCountedNeverListed(t *testing.T) {
	t.Parallel()
	refs := []string{
		"https://h/a/b/pull/1\xff", // a `Delivered:` line's, as the report wrote it
		"wip/t1",
		"https://h\xff/a/b/pull/2",   // a host the scrape read
		"https://h/a/b/pull/1\xfe",   // another ref, which JSON would make the first: U+FFFD for either byte
		"https://h/a/b/pull/1\xff",   // the first, named again: still one ref
		"https://h/a/b/pull/1\uFFFD", // what JSON makes of both, written as UTF-8: a ref of its own
	}
	want := []string{"wip/t1", "https://h/a/b/pull/1\uFFFD"}
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{}, Result{Success: true, DeliveredRefs: refs})
	if !slices.Equal(got.DeliveredRefs, want) || got.DeliveredRefsElided != 3 {
		t.Errorf("listed %q and counted %d more; want %q and the 3 distinct refs that are not UTF-8",
			got.DeliveredRefs, got.DeliveredRefsElided, want)
	}

	// WHAT IS LISTED ARRIVES AS ITSELF, on the record a reader decodes.
	raw, err := json.Marshal(runPhase(PendingRun{}, LaunchRecord{}, got, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var rec types.AgentPhaseCompleted
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.DeliveredRefs, want) || rec.DeliveredRefsElided != 3 {
		t.Errorf("the record arrives listing %q and counting %d; want %q and 3",
			rec.DeliveredRefs, rec.DeliveredRefsElided, want)
	}

	// And the resumed executor is told the list is short, not handed a ref
	// nobody delivered.
	if text := resumeText(PendingRun{}, got); !utf8.ValidString(text) ||
		!strings.Contains(text, "Delivered: wip/t1, https://h/a/b/pull/1\uFFFD (and 3 more its report names") {
		t.Errorf("the resumed executor reads %q", text)
	}

	// A RUN WHOSE ONLY REF IS ONE lists none, and the executor is still told
	// it delivered one.
	alone := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{"https://h/a/b/pull/1\xff"}})
	if text := resumeText(PendingRun{}, alone); len(alone.DeliveredRefs) != 0 || alone.DeliveredRefsElided != 1 ||
		!strings.Contains(text, "Delivered: 1 ref(s) its report names, not listed here") {
		t.Errorf("a run whose only ref is not UTF-8: listed %q, counted %d, the executor reads %q",
			alone.DeliveredRefs, alone.DeliveredRefsElided, text)
	}
}

// THE WHOLE RECORD FITS. Every piece of text a box wrote that rides the run's
// phase record is at its bound, in the worst bytes JSON can be handed — each
// one escaped six-fold — and the record still clears the queue's ceiling, so
// the run's only spend record is never refused for what its run wrote.
func TestTheWholeRecordFitsTheQueuesCeiling(t *testing.T) {
	t.Parallel()
	// LINES of control bytes, so the whole-line fallback keeps each piece
	// right up to its bound — \u0001 on the wire, six bytes for one.
	//
	// EACH PIECE JUST PAST WHAT IT IS HELD TO, which is where its fitted form
	// is at its largest: the report and the transcript twice the record's
	// bound, the failure past what one condensation reads (so it is brought
	// inside that first, as the longest failure is), the question twice its
	// own. A larger input fits to the same record and only costs the
	// redactions it passes through.
	line := strings.Repeat("\x01", 99) + "\n"
	twice := strings.Repeat(line, 2*MaxRunTextBytes/len(line)+1)
	failure := strings.Repeat(line, MaxCondenseBytes/len(line)+2)
	refs := make([]string, 0, 2000)
	for i := range 2000 {
		refs = append(refs, fmt.Sprintf("https://github.com/acme/api/pull/%d?x=%s", i, strings.Repeat("<", 400)))
	}
	question := strings.Repeat("\x01\n", MaxQuestionBytes)
	// THE PUSH RECORD AT ITS WORST, which is one of two: every ref the record
	// can list is a name of `<` — each six bytes on the wire — beside its two
	// whole object ids, the refs the run's own lines leave nothing of; or the
	// record unread, the reason for it past the bound an error is held to.
	pushed := make([]PushedRef, MaxPushedRefs)
	for i := range pushed {
		pushed[i] = PushedRef{Repo: "web", Ref: fmt.Sprintf("refs/heads/%d/%s", i, strings.Repeat("<", 230)),
			Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40), Pushes: 1 << 30}
	}
	unread := strings.Repeat("<", 1<<20)
	c := &Coordinator{condense: &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
		return "", errors.New("no model")
	}}}
	for name, result := range map[string]Result{
		"a failed run":    {Text: twice, Error: failure, Transcript: twice, DeliveredRefs: refs},
		"a run that asks": {Text: twice, Transcript: twice, DeliveredRefs: refs, NeedsInput: true, Question: question},
		"a run that pushed": {Text: twice, Error: failure, Transcript: twice, Pushed: pushed,
			PushedElided: 1 << 30},
		"a run whose pushes are unknown": {Text: twice, Error: failure, Transcript: twice,
			DeliveredRefs: refs, PushedUnread: unread},
	} {
		fitted := c.fitResult(t.Context(), PendingRun{}, result)
		raw, err := json.Marshal(runPhase(PendingRun{}, LaunchRecord{}, fitted, time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		// Room left for the envelope the payload rides in.
		if len(raw) > queue.MaxPayloadBytes-(64<<10) {
			t.Errorf("%s: the record is %d bytes, against the queue's %d", name, len(raw), queue.MaxPayloadBytes)
		}
		if len(raw) < 3<<20 {
			t.Errorf("%s: the record is only %d bytes, so the case is not at the bounds it claims", name, len(raw))
		}
	}
}
