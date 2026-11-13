package atlasdomain_test

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// agentCall is a write an agent seat makes, under a fresh operation.
func agentCall(handle string) atlasdomain.Call {
	return atlasdomain.Call{By: iam.Actor{Name: handle, Kind: iam.ActorAgent},
		OpID: statelog.NewOpID(time.Now(), "atlas")}
}

// asAgent is the source the writer replaces with its caller, its turn and the
// evidence the call names: any agent will do.
var asAgent = atlas.AgentSource("whichever", "some-turn", atlas.EvidenceNone)

// observe writes one claim as the agent handle on n, in turn with evidence.
func (n *writeNode) observe(handle, turn string, evidence atlas.Evidence, c atlas.Claim) statelog.Result {
	n.t.Helper()
	res, err := n.writer.Observe(n.t.Context(), agentCall(handle), c,
		atlasdomain.Observation{Turn: turn, Evidence: evidence})
	if err != nil {
		n.t.Fatalf("observe %s's claim on %s: %v", handle, c.Subject, err)
	}
	return res
}

// AN AGENT'S CLAIM LANDS ON EVERY NODE WITH ITS TURN AND ITS EVIDENCE, the
// seat as its source whatever the claim said, so its confidence is read off
// the record on every node alike and chosen by nobody.
//
// Mutation: drop Turn and Evidence from the wire source (sourceOf) and every
// agent's claim reads back as reported, with no turn.
func TestAnAgentsClaimLandsOnEveryNodeWithItsEvidence(t *testing.T) {
	t.Parallel()
	log := openAtlasLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	a.observe("sre", "run-7", atlas.EvidenceCall, must(t)(atlas.Exists(orders, asAgent, writerClock)))
	b.observe("sre", "run-7", atlas.EvidenceFile,
		must(t)(atlas.Says(orders, atlas.FactEngine, "postgres", asAgent, writerClock)))
	a.drain()
	b.drain()
	if got, want := a.dump(), b.dump(); !maps.EqualFunc(got, want, func(x, y []string) bool {
		return strings.Join(x, "\n") == strings.Join(y, "\n")
	}) {
		t.Fatalf("two nodes on one log hold different rows:\n%v\n%v", got, want)
	}
	live := b.live()
	if len(live) != 2 {
		t.Fatalf("%d live claims, want 2", len(live))
	}
	for _, c := range live {
		want := atlas.AgentSource("sre", "run-7", atlas.EvidenceCall)
		if c.Facet.Kind == atlas.FacetFact {
			want.Evidence = atlas.EvidenceFile
		}
		if c.Source != want {
			t.Errorf("the %s claim landed as %+v, want %+v", c.Facet.Kind, c.Source, want)
		}
	}
	e, ok := b.view().Find(orders.ID())
	if !ok || e.Word != atlas.WordLikely || e.State != atlas.StateDiscovered {
		t.Errorf("the entry reads %+v (found %v), want a likely, discovered one", e, ok)
	}
}

// ONLY AN AGENT'S OWN SEAT OBSERVES, in a turn it names: a person's claim is
// a record of their own rank, and the engine records nothing as an agent.
func TestOnlyAnAgentObservesAndNamesItsTurn(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	claim := must(t)(atlas.Exists(orders, asAgent, writerClock))
	seen := atlasdomain.Observation{Turn: "run-1", Evidence: atlas.EvidenceCall}
	for _, kind := range []iam.ActorKind{iam.ActorHuman, iam.ActorOperator, iam.ActorSystem} {
		c := agentCall("ada")
		c.By.Kind = kind
		if _, err := n.writer.Observe(t.Context(), c, claim, seen); !errors.Is(err, atlasdomain.ErrInvalid) {
			t.Errorf("a %s recorded an agent's claim: %v", kind, err)
		}
	}
	for _, bad := range []atlasdomain.Observation{
		{Evidence: atlas.EvidenceCall},
		{Turn: strings.Repeat("r", atlasdomain.MaxTurnBytes+1), Evidence: atlas.EvidenceCall},
		{Turn: "run-1"},
		{Turn: "run-1", Evidence: "hearsay"},
	} {
		if _, err := n.writer.Observe(t.Context(), agentCall("sre"), claim, bad); !errors.Is(err, atlasdomain.ErrInvalid) {
			t.Errorf("an observation %+v was recorded: %v", bad, err)
		}
	}
	n.drain()
	if live := n.live(); len(live) != 0 {
		t.Errorf("a refused observation left %d claims", len(live))
	}
}

// AN AGENT NEVER SETS MEANING, AN OWNER OR A DEPENDENCY, AND NEVER SAYS TWO
// ENTRIES ARE THE SAME, whatever evidence it names: the writer refuses each
// before anything is published.
//
// Mutation: drop the agent arms of atlas.Claim.Validate and each is recorded.
func TestAnAgentNeverRecordsWhatAPersonDecides(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	seen := atlasdomain.Observation{Turn: "run-1", Evidence: atlas.EvidenceCall}
	person := atlas.PersonSource("ada")
	for name, claim := range map[string]atlas.Claim{
		"environment": must(t)(atlas.Says(checkout, atlas.FactEnvironment, "production", person, writerClock)),
		"importance":  must(t)(atlas.Says(checkout, atlas.FactImportance, "critical", person, writerClock)),
		"purpose":     must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", person, writerClock)),
		"owner": must(t)(atlas.Names(checkout, atlas.RelationOwnedBy, atlas.ExternalSeat, "ada", person,
			writerClock)),
		"dependency":   must(t)(atlas.Relates(checkout, atlas.RelationDependsOn, orders, person, writerClock)),
		"the same":     must(t)(atlas.Same(web, cloudWeb, person, writerClock)),
		"not the same": must(t)(atlas.Distinct(web, cloudWeb, person, writerClock)),
	} {
		if _, err := n.writer.Observe(t.Context(), agentCall("sre"), claim, seen); !errors.Is(err, atlasdomain.ErrInvalid) {
			t.Errorf("an agent recorded %s: %v", name, err)
		}
	}
	n.drain()
	if live := n.live(); len(live) != 0 {
		t.Errorf("a refused claim left %d claims", len(live))
	}
}

// AN AGENT RETIRES NOTHING: its dispute of an entry a person stands behind is
// recorded beside the person's word, the entry stays, and no row marks it
// retired, so the retention never takes it.
//
// Mutation: let applyClaim retire on any denied exists claim and the entry's
// row is marked retired by the agent's dispute.
func TestAnAgentsDisputeRetiresNothingOnTheLog(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	n.record("ada", must(t)(atlas.Exists(checkout, asPerson, writerClock)))
	gone := must(t)(atlas.Dispute(atlas.Claim{Subject: checkout, Facet: atlas.Facet{Kind: atlas.FacetExists}},
		asAgent, writerClock))
	n.observe("sre", "run-9", atlas.EvidenceCall, gone)
	n.drain()
	e, ok := n.view().Find(checkout.ID())
	if !ok || e.State != atlas.StateConfirmed {
		t.Fatalf("after an agent's dispute the entry reads %+v (found %v), want it confirmed", e, ok)
	}
	row, found, err := n.reader().Entry(t.Context(), checkout.ID().String(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || !found || !row.Retired.IsZero() {
		t.Errorf("after an agent's dispute the entry's row reads %+v (found %v, %v), want it not retired",
			row, found, err)
	}
}

// A VALUE OR A KEY SHAPED LIKE A CREDENTIAL IS REFUSED FROM AN AGENT TOO, and
// the refusal never repeats it.
func TestAnAgentsCredentialShapedValueIsRefusedAtTheWriter(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	seen := atlasdomain.Observation{Turn: "run-1", Evidence: atlas.EvidenceCall}
	secret := tokenHost()
	forged := atlas.Claim{Subject: atlas.Ref{Kind: atlas.KindHostname, Key: secret},
		Facet: atlas.Facet{Kind: atlas.FacetExists}, Source: asAgent, ObservedAt: writerClock}
	_, err := n.writer.Observe(t.Context(), agentCall("sre"), forged, seen)
	if !errors.Is(err, atlas.ErrCredential) {
		t.Fatalf("a key shaped like a credential: %v, want ErrCredential", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal repeats the key: %v", err)
	}
}
