package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// A PASS FLUSHES TWICE, once after its vendor's company-level work and once
// after its seats' access sync, and the second flush's seals reach this node
// too: it asks for a re-activation again, which the republisher's window turns
// into the trailing run. So does a value an off cleared: a seat turned off must
// not go on starting its tool with the credential its off revoked.
func TestEveryFlushAfterASealOrAClearRebuilds(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)
	armed := func() bool {
		e.republish.mu.Lock()
		defer e.republish.mu.Unlock()
		return e.republish.pending != nil
	}

	sink := &refreshingSink{TokenSink: &failingFlushSink{}, engine: e}
	ctx := t.Context()
	if err := sink.Record(ctx, "JIRA_AGENT_TOKEN", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	_ = sink.Flush(ctx)
	w.awaitReloads(t, 1)
	if err := sink.Record(ctx, "DATADOG_APP_KEY_SRE", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	_ = sink.Flush(ctx)
	if !armed() {
		t.Fatal("a second flush after a seal asked for no re-activation")
	}
	if err := sink.Forget(ctx, "DATADOG_APP_KEY_SRE"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !sink.changed {
		t.Fatal("a cleared value left the snapshot marked current")
	}
	_ = sink.Flush(ctx)
	if sink.changed {
		t.Fatal("a flush left the snapshot marked stale")
	}
}

// A SURFACE WITH NO PER-SEAT ACCESS keeps its own pass's answer: the seat half
// is not configured there, which is not a fault.
func TestWithSeatAccessKeepsTheCompanyAnswerWhereNoVendorIsRegistered(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	want := []integration.Finding{{Kind: integration.FindingIngressPending, Detail: "hooks pending"}}
	got, err := e.withSeatAccess(t.Context(), integration.KindJira, setup.PassInput{},
		func(context.Context, setup.PassInput) ([]integration.Finding, error) { return want, nil })
	if err != nil || len(got) != 1 || got[0].Detail != "hooks pending" {
		t.Fatalf("withSeatAccess = %+v, %v; want the company findings as they are", got, err)
	}
	boom := errors.New("vendor unreachable")
	if _, err := e.withSeatAccess(t.Context(), integration.KindJira, setup.PassInput{},
		func(context.Context, setup.PassInput) ([]integration.Finding, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("a company-level fault = %v, want it raised", err)
	}
	if _, err := e.seatAccessPlan(nil, integration.KindJira); !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("seatAccessPlan with no vendor = %v, want ErrNotConfigured", err)
	}
}
