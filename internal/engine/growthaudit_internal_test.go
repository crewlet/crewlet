package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// routedGrowthAudit is a growth audit on a node holding no data, which reaches
// served through the router.
func routedGrowthAudit(t *testing.T, served estate.LocalBackends) GrowthAudit {
	t.Helper()
	broker := memory.NewBroker()
	started := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	envelopes := routerEnvelopes(t)
	stop, err := estate.Serve(t.Context(), started(), estate.ServerOptions{Taken: signedasktest.NewLedger(),
		Self: "data-1", Local: served, Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "seats-1", Queue: started(), Placement: oneDataNode{}, Git: noGitletNodes{},
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return GrowthAudit{engine: &Engine{router: router}}
}

// A NODE WITHOUT DATA RECORDS EVERY GROWTH STEP THROUGH A DATA NODE, each
// published text as its digest alone: a product's calls run wherever its seat
// is held, and the words it publishes never leave the node in the clear.
//
// Mutation: hand a text change to the writer as it is and the reply crosses
// the router; record a step without a router or an access log and a stateless
// node's acts are off the audit.
func TestANodeWithoutDataRecordsItsGrowthStepsThroughADataNode(t *testing.T) {
	t.Parallel()
	served := &receivedAccess{}
	audit := routedGrowthAudit(t, served)
	const reply = "We fixed the crash in build 4417-ledger, thank you"
	held := uuid.NewString()
	ask := growth.Record{Product: "fakeads", Connection: "eu", Account: "act_1", Target: "review-9",
		Tool: "fakeads_reply", Class: growth.ClassPublic, Handle: "growth", HeldCall: held,
		Operation: strings.Repeat("ab", 32),
		Changes:   []growth.RecordChange{{Field: "reply", Label: "Reply", After: reply, Text: true}}}
	if first, err := audit.Asked(t.Context(), ask); err != nil || !first {
		t.Fatalf("an ask from a node holding no data: %v, %v", first, err)
	}
	allowed := ask
	allowed.Answerer = "ops-lead"
	if first, err := audit.Verdict(t.Context(), growth.StepAllowed, allowed); err != nil || !first {
		t.Fatalf("an allow from a node holding no data: %v, %v", first, err)
	}
	if _, err := audit.Verdict(t.Context(), growth.StepApplied, ask); err == nil {
		t.Error("an apply was recorded as an answer")
	}
	served.mu.Lock()
	got := append([]accessdomain.NewGrowth(nil), served.growth...)
	served.mu.Unlock()
	if len(got) != 2 || got[0].Step != growth.StepAsked || got[1].Step != growth.StepAllowed ||
		got[1].Answerer != "ops-lead" {
		t.Fatalf("the data node received %+v, want the ask and the allow", got)
	}
	sum := sha256.Sum256([]byte(reply))
	text := got[0].Changes[0]
	if text.After != "" || !text.Text || text.AfterSHA256 != hex.EncodeToString(sum[:]) || text.AfterBytes != len(reply) {
		t.Errorf("the published text crossed the router as %+v", text)
	}
	if _, err := (GrowthAudit{engine: &Engine{}}).Made(t.Context(), ask); err == nil {
		t.Error("a node with neither an access log nor a router recorded a growth step")
	}
}

// recordedAccess answers every growth write as recorded already.
type recordedAccess struct{ receivedAccess }

func (a *recordedAccess) For(context.Context, string) (estate.Backend, bool) {
	return estate.Backend{Access: estate.AccessHalf{Writer: a}}, true
}

func (a *recordedAccess) Growth(context.Context, accessdomain.Call, accessdomain.NewGrowth) (statelog.Result, error) {
	return statelog.Result{}, accessdomain.ErrRecorded
}

// A STEP WHOSE PHASE IS RECORDED ALREADY IS NOT THE FIRST, and is no error: an
// allow after the engine's lapse says first false, so a product does not act
// on it.
//
// Mutation: answer ErrRecorded as an error and a retried record fails a call
// that landed; answer it as first and a product applies a lapsed act.
func TestAGrowthStepRecordedAlreadyIsNotTheFirst(t *testing.T) {
	t.Parallel()
	audit := routedGrowthAudit(t, &recordedAccess{})
	first, err := audit.Verdict(t.Context(), growth.StepAllowed, growth.Record{Product: "fakeads", Connection: "eu",
		Account: "act_1", Tool: "fakeads_raise_budget", Class: growth.ClassSpend, Handle: "growth",
		HeldCall: uuid.NewString(), Answerer: "ops-lead", Operation: strings.Repeat("ab", 32)})
	if err != nil || first {
		t.Errorf("an allow after another answer says %v, %v; want not the first and no error", first, err)
	}
}

// An act's lease outlives everything its apply does under it, which a product
// cuts at growth.ApplyBound, so no apply acts after its lease lapsed.
//
// Mutation: set growthActLeaseTTL at or below growth.ApplyBound, and this fails.
func TestAGrowthActsLeaseOutlivesItsApply(t *testing.T) {
	t.Parallel()
	if growthActLeaseTTL <= growth.ApplyBound {
		t.Errorf("the lease lives %s and an apply runs up to %s", growthActLeaseTTL, growth.ApplyBound)
	}
}
