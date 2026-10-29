package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/queue"
)

// A REF IS REDACTED BEFORE ANYTHING READS IT. It is whatever word a report
// put on its `Delivered:` line, and a box holds the seat's credentials — so
// the coordinator, which trusts no runner to have redacted what it hands back,
// redacts each before the record, the run's report and the bound that
// measures them.
func TestADeliveredRefIsRedactedBeforeItIsRecorded(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("c", 36)
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{"wip/t1", secret}})
	rec := runPhase(PendingRun{}, LaunchRecord{}, got, time.Now())
	for where, refs := range map[string][]string{
		"the result": got.DeliveredRefs, "the record": rec.DeliveredRefs,
		"the run's report": {runReport(got)},
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

	text := runReport(got)
	if !strings.Contains(text, fmt.Sprintf("(and %d more its report names", got.DeliveredRefsElided)) {
		t.Errorf("the run's report does not say the list is short: %.300q", text)
	}
	rec := runPhase(PendingRun{}, LaunchRecord{}, got, time.Now())
	if rec.DeliveredRefsElided != got.DeliveredRefsElided || len(rec.DeliveredRefs) != len(got.DeliveredRefs) {
		t.Errorf("the record lists %d and counts %d", len(rec.DeliveredRefs), rec.DeliveredRefsElided)
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
	// inside that first, as the longest failure is). A larger input fits to
	// the same record and only costs the redactions it passes through. The
	// question is the row's, which the ask held to [decision.MaxQuestion]
	// when it was recorded, so it is at that bound, in bytes JSON escapes.
	line := strings.Repeat("\x01", 99) + "\n"
	twice := strings.Repeat(line, 2*MaxRunTextBytes/len(line)+1)
	failure := strings.Repeat(line, MaxCondenseBytes/len(line)+2)
	refs := make([]string, 0, 2000)
	for i := range 2000 {
		refs = append(refs, fmt.Sprintf("https://github.com/acme/api/pull/%d?x=%s", i, strings.Repeat("<", 400)))
	}
	asking := PendingRun{Question: strings.Repeat("<", decision.MaxQuestion)}
	c := &Coordinator{condense: &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
		return "", errors.New("no model")
	}}}
	for name, tc := range map[string]struct {
		run    PendingRun
		result Result
	}{
		"a failed run":    {PendingRun{}, Result{Text: twice, Error: failure, Transcript: twice, DeliveredRefs: refs}},
		"a run that asks": {asking, Result{Text: twice, Transcript: twice, DeliveredRefs: refs}},
	} {
		fitted := c.fitResult(t.Context(), tc.run, tc.result)
		raw, err := json.Marshal(runPhase(tc.run, LaunchRecord{}, fitted, time.Now()))
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
