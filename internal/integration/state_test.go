package integration

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A REFUSED CREDENTIAL IS NOT A WAIT.
//
// Every pass failure used to fold into activating/engine with the detail "the
// last pass could not read this integration", on the reasoning that almost
// every fault is a vendor briefly unreachable. That reasoning is exactly
// backwards for a refusal: the vendor answered, and it will answer the same
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
		t.Errorf("phase = %q, want %q: a credential the vendor refuses means this deployment cannot talk to the surface at all",
			got.Report.Phase, PhaseUnconfigured)
	}
	if got.Report.Actor != ActorOperator {
		t.Errorf("actor = %q, want %q: no retry fixes a token the vendor will not accept",
			got.Report.Actor, ActorOperator)
	}
	if len(got.Findings) != 1 || got.Findings[0].Kind != FindingCredentialRejected {
		t.Errorf("findings = %+v, want one %q", got.Findings, FindingCredentialRejected)
	}
	if got.LastError == "" {
		t.Error("the vendor's own words were dropped; they are what says which credential and why")
	}
}

// And a fault that is NOT a refusal keeps the wait, because that reasoning
// still holds: an unreachable vendor clears without anybody doing anything.
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
// failed teardown did not pin the phase, a vendor refusing the delete would
// show a connected integration somebody had already asked to remove.
func TestATeardownThatFailsKeepsTheSurfaceDisconnecting(t *testing.T) {
	now := time.Now().UTC()
	start := State{Disconnecting: true, RemoveSeats: true}

	got, forget := ObserveTeardown(start, KindJira, errors.New("jira: 403 on delete"), now)
	if forget {
		t.Fatal("a failed teardown forgot the row; the vendor still holds what it registered")
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
		t.Error("the vendor's own words were dropped; they are what says why it is stuck")
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
