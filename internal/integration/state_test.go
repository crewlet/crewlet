package integration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// A REFUSED CREDENTIAL IS NOT A WAIT.
//
// Every pass failure used to fold into activating/engine with the detail "the
// last pass could not read this integration", on the reasoning that almost
// every fault is a third-party app briefly unreachable. That reasoning is exactly
// backwards for a refusal: the third-party app answered, and it will answer the same
// way on every subsequent pass until a person changes the credential. The
// screen told an operator the engine was working on it while the only action
// that could fix it was theirs, and the dashboard drew it in the neutral tone
// it reserves for work in flight.
func TestARefusedCredentialIsTheOperatorsToFix(t *testing.T) {
	now := time.Now().UTC()
	refused := fmt.Errorf("jira: GET /rest/api/3/myself: %w",
		Reject(errors.New("401: Client must be authenticated"), 401))

	got, forget := Observe(State{}, KindJira, nil, refused, now)
	if forget {
		t.Fatal("a refused credential forgot the surface; that is reserved for a block that left the company")
	}
	if got.Report.Phase != PhaseUnconfigured {
		t.Errorf("phase = %q, want %q: a credential the third-party app refuses means this deployment cannot talk to the surface at all",
			got.Report.Phase, PhaseUnconfigured)
	}
	if got.Report.Actor != ActorOperator {
		t.Errorf("actor = %q, want %q: no retry fixes a token the third-party app will not accept",
			got.Report.Actor, ActorOperator)
	}
	if len(got.Findings) != 1 || got.Findings[0].Kind != FindingCredentialRejected {
		t.Errorf("findings = %+v, want one %q", got.Findings, FindingCredentialRejected)
	}
	if got.LastError == "" {
		t.Error("the third-party app's own words were dropped; they are what says which credential and why")
	}
}

// And a fault that is NOT a refusal keeps the wait, because that reasoning
// still holds: an unreachable third-party app clears without anybody doing anything.
func TestAnUnreachableVendorIsStillAWait(t *testing.T) {
	now := time.Now().UTC()
	got, _ := Observe(State{}, KindJira, nil, errors.New("dial tcp: i/o timeout"), now)
	if got.Report.Phase != PhaseActivating || got.Report.Actor != ActorEngine {
		t.Errorf("phase/actor = %q/%q, want %q/%q: a transport fault is still the engine's to retry",
			got.Report.Phase, got.Report.Actor, PhaseActivating, ActorEngine)
	}
}

// The status rule is one rule, and the statuses that are NOT refusals matter
// as much as the ones that are: a rate limit and a 5xx both clear on their
// own, and reporting either as the operator's job sends somebody to rotate a
// working credential.
func TestOnlyAuthStatusesAreRefusals(t *testing.T) {
	base := errors.New("boom")
	for _, status := range []int{401, 403} {
		if !errors.Is(Reject(base, status), ErrCredentialRejected) {
			t.Errorf("status %d was not treated as a refusal", status)
		}
	}
	for _, status := range []int{0, 404, 429, 500, 502, 503} {
		if errors.Is(Reject(base, status), ErrCredentialRejected) {
			t.Errorf("status %d was treated as a refusal, but it clears on its own", status)
		}
	}
	if Reject(nil, 401) != nil {
		t.Error("Reject invented an error out of nil")
	}
}

// A TEARDOWN THAT FAILS HOLDS THE SURFACE, rather than letting it drift back
// to looking connected.
//
// The block is still in the company document for the whole of a teardown —
// it is the credential the teardown authenticates with — so a normal pass
// over the same surface would find it configured and report it healthy. If a
// failed teardown did not pin the phase, a third-party app refusing the delete would
// show a connected integration somebody had already asked to remove.
func TestATeardownThatFailsKeepsTheSurfaceDisconnecting(t *testing.T) {
	now := time.Now().UTC()
	start := State{Disconnecting: true, RemoveSeats: true}

	got, forget := ObserveTeardown(start, KindJira, errors.New("jira: 403 on delete"), now)
	if forget {
		t.Fatal("a failed teardown forgot the row; the third-party app still holds what it registered")
	}
	if got.Report.Phase != PhaseDisconnecting {
		t.Errorf("phase = %q, want %q", got.Report.Phase, PhaseDisconnecting)
	}
	if !got.Disconnecting {
		t.Error("the intent was dropped, so the next pass would reconcile it as if connected")
	}
	if !got.RemoveSeats {
		t.Error("the operator's answer to the checkbox was lost between attempts")
	}
	if got.LastError == "" {
		t.Error("the third-party app's own words were dropped; they are what says why it is stuck")
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the backoff is derived from it", got.Attempts)
	}
}

// AND A TEARDOWN THAT SUCCEEDS IS THE END OF THE ROW. Nothing is left to
// reconcile, and the caller removes the block in the same step.
func TestATeardownThatSucceedsForgetsTheSurface(t *testing.T) {
	now := time.Now().UTC()
	got, forget := ObserveTeardown(State{Disconnecting: true}, KindJira, nil, now)
	if !forget {
		t.Fatal("a finished teardown kept the row, so the screen would still show it")
	}
	if got.LastError != "" {
		t.Error("a fault from an earlier attempt survived a pass that succeeded")
	}
}

// TearingDown reads the INTENT, not the phase. They differ for exactly as
// long as it takes the first pass to run, which is the window in which a
// reader would otherwise see a connected integration and press Disconnect
// again.
func TestTheDisconnectIntentIsVisibleBeforeAPassHasRun(t *testing.T) {
	asked := State{Disconnecting: true}
	if !asked.TearingDown() {
		t.Error("a surface asked to disconnect does not report itself tearing down")
	}
	running := State{Report: Report{Phase: PhaseDisconnecting}}
	if !running.TearingDown() {
		t.Error("a surface mid-teardown does not report itself tearing down")
	}
	if (State{Report: Report{Phase: PhaseReady}}).TearingDown() {
		t.Error("a connected surface reports itself tearing down")
	}
}

// A SURFACE BEING TAKEN AWAY REPORTS THE TEARDOWN, NOT THE LAST RECONCILE.
//
// The findings on a row are statements about a world the operator is
// dismantling, and an actor is not owed a step in it. Measured: pressing
// Disconnect on Datadog left the card carrying that pass's "re-enable it at
// Datadog: this pass will not" underneath the word Disconnecting — so the one
// thing on the screen told the operator they had work to do at a third-party
// app they had just asked to be let go of.
//
// [ObserveTeardown] already erases them on every pass after the first, which
// is what makes leaving them on the first a divergence rather than a policy.
func TestAskingForATeardownDropsWhatTheLastPassFound(t *testing.T) {
	t.Parallel()
	settled := State{
		Kind: KindDatadog,
		Findings: []Finding{{
			Kind:   FindingApprovalRequired,
			Detail: "re-enable it at Datadog: this pass will not",
		}},
		LastError:     "the last pass could not read the organization",
		Attempts:      4,
		NextAttemptAt: time.Now().Add(9 * time.Minute),
		Report: Report{
			Phase: PhaseDegraded, Actor: ActorAdmin,
			Detail: "re-enable it at Datadog: this pass will not",
		},
	}

	asker := iam.Actor{Name: "ana.admin", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	got := AskTeardown(settled, KindDatadog, true, asker)

	if len(got.Findings) != 0 {
		t.Errorf("findings = %+v, which describe a world being taken apart", got.Findings)
	}
	if got.LastError != "" {
		t.Errorf("last error = %q, from a pass that is no longer what is happening", got.LastError)
	}
	if got.Report.Phase != PhaseDisconnecting {
		t.Errorf("phase = %s, want disconnecting", got.Report.Phase)
	}
	// NOBODY IS OWED A STEP. The actor decides whether the card draws amber
	// with an alert icon, which over a disconnect asks a person to act on an
	// integration they are removing.
	if got.Report.Actor.WaitsOnAPerson() {
		t.Errorf("actor = %s: a teardown in progress is the engine's work", got.Report.Actor)
	}
	if !got.Disconnecting || !got.RemoveSeats {
		t.Errorf("the intent was not recorded: disconnecting=%v remove_seats=%v",
			got.Disconnecting, got.RemoveSeats)
	}
	// AND WHO ASKED, whole — the author, their kind and the credential —
	// which is what the teardown's writes are recorded under.
	if by, named := got.RequestedBy.Actor(); !named || by != asker {
		t.Errorf("the ask names %+v (%v), want %+v", by, named, asker)
	}
	// DUE NOW, rather than inheriting a backoff nobody asked it to serve.
	if got.Attempts != 0 || !got.NextAttemptAt.IsZero() {
		t.Errorf("attempts=%d next=%v, so the disconnect waits out a backoff",
			got.Attempts, got.NextAttemptAt)
	}
}

// lineCount is a log destination counting the lines written to it.
type lineCount struct {
	mu sync.Mutex
	n  int
}

func (c *lineCount) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += bytes.Count(p, []byte("\n"))
	return len(p), nil
}

// A PASS OF ANY SIZE IS LOGGED WHOLE IN LINEAR WORK: nothing bounds a
// company's seats, and every pass past the record's bound is logged whole
// beside its fit. 10,000 agents' own findings, each past a bound and each
// past the first cut by the record, are each logged once, as cut, reading a
// few lengths and marks apiece: asking whether one was cut reads one mark,
// never every cut finding.
//
// Mutation: ask it by walking the marks (as slices.Contains walked the cut
// indexes), and the marks read grow with the square of the findings.
func TestAPassOfAnySizeIsLoggedWholeInLinearWork(t *testing.T) {
	lines := &lineCount{}
	logging.Configure(slog.LevelInfo, logging.FormatText, lines)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	findings := []Finding{{Kind: FindingCredentialMissing}}
	for i := range 10_000 {
		findings = append(findings, Finding{Kind: FindingApprovalRequired, Seat: true,
			Subject: fmt.Sprintf("agent-%06d", i), Detail: strings.Repeat("d", MaxDetailLength+100)})
	}
	l := logWhole(t.Context(), KindGitHub, findings, nil)
	cut := 0
	for _, c := range l.cut[1:] {
		if c {
			cut++
		}
	}
	if cut != len(findings)-1 {
		t.Fatalf("premise: the record cut %d of the %d findings past the first, want every one", cut,
			len(findings)-1)
	}
	if lines.n != len(findings)-1 {
		t.Errorf("%d lines logged of %d findings cut, want each once", lines.n, len(findings)-1)
	}
	if most := 5 * len(findings); l.read > most {
		t.Errorf("logging the pass read %d lengths and marks of %d findings, want at most %d", l.read,
			len(findings), most)
	}
}

// WHAT A ROW SHORTENS IS LEFT WHOLE ON THE LOG. The records were the only
// place that text was written, so what their bounds left out was lost: a
// validation list's later fields, the scope a refusal named after its first
// sentence, a remedy quoting two long names, a link past what a record keeps.
// Every field [bound] cuts or drops is logged whole, and a finding the record
// cuts or drops is logged as the pass made it, never as cut, each once.
//
// Mutation: log only a finding's detail, log a cut finding from the bounded
// list, or log one both bound and the record cut twice, and a check fails.
func TestWhatARowShortensIsLoggedWhole(t *testing.T) {
	var out bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })

	long := strings.Repeat("field is required; ", 40) + "missing scope admin:org"
	fault := errors.New(strings.Repeat("upstream said no; ", 200) + "rotate the key")
	findings := []Finding{
		{Kind: FindingApprovalRequired, Subject: "sre-lead", Detail: long},
		{Kind: FindingApprovalRequired, Subject: "eng-lead", Detail: "short"},
		{Kind: FindingApprovalRequired, Subject: "ops-lead", Remedy: strings.Repeat("r", 600) + "REMEDY-TAIL",
			Subjects:  []string{strings.Repeat("s", 600) + "SUBJ-TAIL"},
			ActionURL: "https://example.com/" + strings.Repeat("u", 3000) + "URL-TAIL"},
	}
	// ENOUGH OF THE VENDOR'S OWN, each past the bound, that the record cuts them.
	for i := range 300 {
		findings = append(findings, Finding{Kind: FindingGrantPending, Subject: fmt.Sprintf("seat-%03d", i),
			Remedy: strings.Repeat("v", 600) + fmt.Sprintf("DROPPED-TAIL-%03d", i)})
	}
	LogWhole(t.Context(), KindGitHub, findings, fault)

	got := out.String()
	lines := func(event, tail string) int {
		n := 0
		for _, l := range strings.Split(got, "\n") {
			if strings.Contains(l, tail) && (event == "" || strings.Contains(l, event)) {
				n++
			}
		}
		return n
	}
	line := func(event, tail string) bool { return lines(event, tail) > 0 }
	for _, tail := range []string{"missing scope admin:org", "REMEDY-TAIL", "SUBJ-TAIL", "URL-TAIL"} {
		if !line("integration_finding_shortened", tail) {
			t.Errorf("a finding past the row's bound was not logged whole: its line lacks %q", tail)
		}
	}
	if line("integration_finding_shortened", "eng-lead") {
		t.Error("a finding within the bound was logged as shortened")
	}
	if !line("integration_finding_unkept", "DROPPED-TAIL-299") {
		t.Error("a finding the record cut was not logged as the pass made it")
	}
	for _, tail := range []string{"DROPPED-TAIL-299", "REMEDY-TAIL", "missing scope admin:org"} {
		if n := lines("", tail); n != 1 {
			t.Errorf("%q is on %d lines of the log, want one", tail, n)
		}
	}
	if !line("integration_fault_shortened", "rotate the key") {
		t.Errorf("a fault past the row's bound was not logged whole:\n%.400s", got)
	}
}
