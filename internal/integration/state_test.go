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
