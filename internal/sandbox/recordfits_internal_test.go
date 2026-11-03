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

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
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
	rec := collectedRecord(PendingRun{}, LaunchRecord{}, got, time.Now())
	for where, refs := range map[string][]string{
		"the result": got.DeliveredRefs, "the record": rec.DeliveredRefs,
		"the run's report": {runReport(PendingRun{}, got)},
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

	text := runReport(PendingRun{}, got)
	if !strings.Contains(text, fmt.Sprintf("(and %d more its report names", got.DeliveredRefsElided)) {
		t.Errorf("the run's report does not say the list is short: %.300q", text)
	}
	rec := collectedRecord(PendingRun{}, LaunchRecord{}, got, time.Now())
	if rec.DeliveredRefsElided != got.DeliveredRefsElided || len(rec.DeliveredRefs) != len(got.DeliveredRefs) {
		t.Errorf("the record lists %d and counts %d", len(rec.DeliveredRefs), rec.DeliveredRefsElided)
	}
}

// REFS THE RECORD LISTS NONE OF ARE STILL SAID. A ref longer than the bound is
// past it on its own, so a record can list nothing and count what the run
// named; the run's report then said nothing of it at all, and the run read to
// its reviewer as one that delivered nothing, while the record and the
// dashboard both counted the ref.
func TestTheRunsReportSaysRefsTheRecordListsNoneOf(t *testing.T) {
	t.Parallel()
	long := "https://h/a/b/pull/1?x=" + strings.Repeat("y", MaxDeliveredRefBytes)
	got := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{long}})
	if len(got.DeliveredRefs) != 0 || got.DeliveredRefsElided != 1 {
		t.Fatalf("listed %d and counted %d more; want none listed and the one ref counted",
			len(got.DeliveredRefs), got.DeliveredRefsElided)
	}
	text := runReport(PendingRun{}, got)
	if !strings.Contains(text, "Delivered: 1 ref(s) its report names, not listed here") {
		t.Errorf("the report does not say the ref it does not list: %q", text)
	}

	// A run that named no ref says none.
	text = runReport(PendingRun{}, Result{Success: true, Text: "Outcome: succeeded"})
	if strings.Contains(text, "Delivered:") {
		t.Errorf("a run that named no ref reports one: %q", text)
	}
}

// A REF THE RECORD WOULD RENAME IS COUNTED, NEVER LISTED. Every reader of the
// refs reads them through JSON — the launch's own record, the run's report —
// where a byte that is not UTF-8 is replaced: listed, such a ref arrived as
// another, a URL the run never delivered, and refs that differ only there
// arrived as one. A URL holds whatever bytes a report puts in it, on a
// `Delivered:` line or in the host a scrape reads, so it is judged here, where
// every runner's refs pass.
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
	raw, err := json.Marshal(collectedRecord(PendingRun{}, LaunchRecord{}, got, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var rec types.ExecutorCollected
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.DeliveredRefs, want) || rec.DeliveredRefsElided != 3 {
		t.Errorf("the record arrives listing %q and counting %d; want %q and 3",
			rec.DeliveredRefs, rec.DeliveredRefsElided, want)
	}

	// And the report says the list is short, never naming a ref nobody
	// delivered.
	if text := runReport(PendingRun{}, got); !utf8.ValidString(text) ||
		!strings.Contains(text, "Delivered: wip/t1, https://h/a/b/pull/1\uFFFD (and 3 more its report names") {
		t.Errorf("the run's report reads %q", text)
	}

	// A RUN WHOSE ONLY REF IS ONE lists none, and its report still says it
	// delivered one.
	alone := (&Coordinator{}).fitResult(t.Context(), PendingRun{},
		Result{Success: true, DeliveredRefs: []string{"https://h/a/b/pull/1\xff"}})
	if text := runReport(PendingRun{}, alone); len(alone.DeliveredRefs) != 0 || alone.DeliveredRefsElided != 1 ||
		!strings.Contains(text, "Delivered: 1 ref(s) its report names, not listed here") {
		t.Errorf("a run whose only ref is not UTF-8: listed %q, counted %d, its report reads %q",
			alone.DeliveredRefs, alone.DeliveredRefsElided, text)
	}
}

// THE RECORD NAMES A SESSION AND A RELEASE OF A HARNESS'S SHAPES ALONE. Both
// are the box's words, which a runner holds to those shapes as it reads the
// box, and the record holds them again as it is published — trusting no
// runner, nor a host of another build, to have done so, as it holds its two
// reasons: one of any other shape, or past its bound, is none on the record,
// never cut, since a shortened id or release names another one.
//
// Mutation: carry either as the runner handed it, hold it to its length alone
// or its characters alone, or cut it to its bound, and a case goes red.
func TestTheRecordNamesASessionAndAReleaseOfTheirShapesAlone(t *testing.T) {
	t.Parallel()
	const uuid = "9a8b7c6d-0000-4000-8000-000000000001"
	widestID := strings.Repeat("s", executorkeep.SessionIDMaxBytes)
	widestRelease := strings.Repeat("1", executorkeep.HarnessVersionMaxBytes-len(".0.0")) + ".0.0"
	for _, tc := range []struct{ session, release, wantSession, wantRelease string }{
		{uuid, "2.1.310", uuid, "2.1.310"},
		{"ses_01J9ZQ3W7K", "2.0.32", "ses_01J9ZQ3W7K", "2.0.32"},
		{widestID, widestRelease, widestID, widestRelease},
		{widestID + "s", "1" + widestRelease, "", ""},
		{"../../.credentials.json", "2.1.310 (Claude Code)", "", ""},
		{"a session", "2.1", "", ""},
		{"s\n" + uuid, "v2.1.310", "", ""},
		{"", "2..310", "", ""},
	} {
		rec := collectedRecord(PendingRun{}, LaunchRecord{},
			Result{SessionID: tc.session, HarnessVersion: tc.release}, time.Now())
		if rec.HarnessSession != tc.wantSession || rec.HarnessVersion != tc.wantRelease {
			t.Errorf("a box that named session %.40q (%d bytes) and release %.40q (%d bytes) is recorded "+
				"as %.40q and %.40q; want %.40q and %.40q", tc.session, len(tc.session), tc.release,
				len(tc.release), rec.HarnessSession, rec.HarnessVersion, tc.wantSession, tc.wantRelease)
		}
	}
}

// A PIECE IS MEASURED AS THE RECORD CARRIES IT: REDACTED. A redaction can
// lengthen a text — a password's value written in pieces between escaped
// breaks becomes a marker a piece, seven times its bytes — and the record is
// redacted as it is published, so a report or a failure measured before its
// last redaction rode the record at up to seven times its bound: a runner's
// that redacted nothing, which the coordinator trusts no runner not to hand
// over, and a model's rewrite, which no runner ever saw. Each is redacted as
// it is fitted, a rewrite before it is measured, and one the redaction puts
// past the bound is refused as a long one is: whole lines of the redacted text
// stand in its place.
//
// Mutation: fit the report or the failure before redacting it, or measure the
// rewrite before redacting it, and a case goes red.
func TestAPieceIsMeasuredAsTheRecordCarriesIt(t *testing.T) {
	t.Parallel()
	// LINES OF A VALUE IN PIECES, just inside the bound as written and seven
	// times it redacted, in lines a fallback can keep whole.
	line := "pwd=" + strings.Repeat(`x\n`, 20) + "\n"
	grows := strings.Repeat(line, MaxRunTextBytes/len(line))
	if redacted := redact.Secrets(grows); len(redacted) < 4*MaxRunTextBytes {
		t.Fatalf("the case's text redacts to %d bytes, which is not far past the %d-byte bound", len(redacted),
			MaxRunTextBytes)
	}
	controls := strings.Repeat(strings.Repeat("\x01", 99)+"\n", 2*MaxRunTextBytes/100+1)
	failure := strings.Repeat(strings.Repeat("\x01", 99)+"\n", MaxCondenseBytes/100+2)
	noModel := &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
		return "", errors.New("no model")
	}}
	// A REWRITE OF THAT SHAPE, inside the budget as the model wrote it.
	growingRewrite := &fakeCondenser{answer: func(_ RunPart, _ string, budget int) (string, error) {
		return grows[:budget/len(line)*len(line)], nil
	}}
	for name, tc := range map[string]struct {
		condense Condenser
		result   Result
	}{
		"a runner that redacted nothing": {noModel, Result{Text: grows, Error: grows}},
		"a rewrite a redaction grows":    {growingRewrite, Result{Text: controls, Error: failure}},
	} {
		c := &Coordinator{condense: tc.condense}
		rec := collectedRecord(PendingRun{}, LaunchRecord{}, c.fitResult(t.Context(), PendingRun{}, tc.result),
			time.Now())
		// THE NOTE saying what was left out is not counted against the bound
		// ([wholeLines]); a KiB holds it.
		for piece, text := range map[string]string{"report": rec.Response, "failure": rec.Error} {
			if len(text) > MaxRunTextBytes+1<<10 {
				t.Errorf("%s: the record's %s is %d bytes, past the %d it is held to", name, piece, len(text),
					MaxRunTextBytes)
			}
		}
	}
}

// THE WHOLE RECORD FITS. Every piece of text a box wrote that rides the
// launch's own record is at its bound, in the worst bytes JSON can be handed —
// each one escaped six-fold — and the record still clears the queue's ceiling,
// so the launch's only spend record is never refused for what its run wrote.
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
	// THE TWO REASONS FAR PAST THEIR KiB: one line of control bytes a MiB
	// long, as a box's own agent can make the session record's line the
	// fall-back reason is read from — and as a runner, or a host of another
	// build, that did not hold it would hand it over. Held as the record is
	// published, each is at its widest whatever it came as; carried whole,
	// either one puts the record past the ceiling. AND THE SESSION AND THE
	// RELEASE THE BOX NAMED, the same MiB each, as such a runner would hand
	// them over too: identifiers, which the record refuses rather than cuts
	// when they are not of a harness's shapes; carried whole, either one puts
	// the record past the ceiling as well.
	reason := strings.Repeat("\x01", 1<<20)
	kept := &SessionKept{NotKept: reason}
	c := &Coordinator{condense: &fakeCondenser{answer: func(RunPart, string, int) (string, error) {
		return "", errors.New("no model")
	}}}
	for name, tc := range map[string]struct {
		run    PendingRun
		result Result
	}{
		"a failed run": {PendingRun{}, Result{Text: twice, Error: failure, Transcript: twice, DeliveredRefs: refs,
			FellBack: reason, Kept: kept, SessionID: reason, HarnessVersion: reason}},
		"a run that asks": {asking, Result{Text: twice, Transcript: twice, DeliveredRefs: refs,
			FellBack: reason, Kept: kept, SessionID: reason, HarnessVersion: reason}},
		"a run that pushed": {PendingRun{}, Result{Text: twice, Error: failure, Transcript: twice, Pushed: pushed,
			PushedElided: 1 << 30, FellBack: reason, Kept: kept, SessionID: reason, HarnessVersion: reason}},
		"a run whose pushes are unknown": {PendingRun{}, Result{Text: twice, Error: failure, Transcript: twice,
			DeliveredRefs: refs, PushedUnread: unread, FellBack: reason, Kept: kept, SessionID: reason,
			HarnessVersion: reason}},
	} {
		fitted := c.fitResult(t.Context(), tc.run, tc.result)
		rec := collectedRecord(tc.run, LaunchRecord{}, fitted, time.Now())
		if len(rec.HarnessSessionFellBack) > MaxFellBackBytes ||
			len(rec.SessionNotKept) > executorkeep.UnkeptReasonMaxBytes {
			t.Errorf("%s: the record's reasons are %d and %d bytes, past the KiB each is held to", name,
				len(rec.HarnessSessionFellBack), len(rec.SessionNotKept))
		}
		if rec.HarnessSession != "" || rec.HarnessVersion != "" {
			t.Errorf("%s: the record names a session of %d bytes and a release of %d, where neither is "+
				"of a harness's shape", name, len(rec.HarnessSession), len(rec.HarnessVersion))
		}
		raw, err := json.Marshal(rec)
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
