package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHEN A TURN'S OPERATION IDS CAN FIRST HAVE BEEN MINTED, from the trigger to
// the writer.
//
// A write in a turn derives its operation id from the turn, so a redelivered
// turn re-derives the ids its first attempt minted, and a node that adopted a
// snapshot since answers a write stamped before the adoption from what the
// adoption brought. A write stamped with its own call's instant — later than
// the first attempt's — reads as newer than an adoption the first attempt
// predates, and is decided a second time. So the instant is fixed where the
// turn is described, carried on the turn's context, and handed to every writer
// a seat's tools are given.

// instantCompany is a company with one seat, for describing its turns.
func instantCompany() *Company {
	o := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	o.Normalize()
	return &Company{Config: &config.Company{}, Org: o}
}

// A DISPATCHED TURN CARRIES ITS EARLIEST TRIGGER'S INSTANT, and a turn with no
// unit of work its own start.
//
// The work key is derived from the partition's events, every attempt at the
// work was woken by them, and none can have minted anything before they
// existed — so the earliest of them bounds the first attempt's ids where the
// dispatch's own instant, on a redelivery, does not. An event with no
// timestamp says nothing and is passed over. With no work key the run is the
// seed, and nothing but this run derives its ids.
//
// Mutation: describe the turn with its own start whatever its events, or hand
// the runner a context without the instant, and this fails.
func TestADispatchedTurnCarriesItsEarliestTriggersInstant(t *testing.T) {
	t.Parallel()
	company := instantCompany()
	earliest := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	partition := []*events.Event{
		{Timestamp: earliest.Add(time.Minute)},
		nil,
		{},
		{Timestamp: earliest},
	}

	tel := (&Engine{}).describeTurn(t.Context(), company, Request{
		Handle: "eng", RunID: "run-2", WorkKey: "wk-1", Events: partition,
	})
	got := tel.runnerTurn(company, 0, nil, "", turn.NoReply()).Context
	if got == nil {
		t.Fatal("the dispatched turn carries no context")
	}
	if !got.TriggeredAt.Equal(earliest) {
		t.Fatalf("a redelivered turn's context carries %v, want its earliest "+
			"trigger's %v", got.TriggeredAt, earliest)
	}

	before := time.Now().UTC()
	tel = (&Engine{}).describeTurn(t.Context(), company, Request{
		Handle: "eng", RunID: "run-3", Events: partition,
	})
	after := time.Now().UTC()
	got = tel.runnerTurn(company, 0, nil, "", turn.NoReply()).Context
	if got.TriggeredAt.Before(before) || got.TriggeredAt.After(after) {
		t.Errorf("a turn with no work key carries %v, want its own start, "+
			"between %v and %v", got.TriggeredAt, before, after)
	}
}

// A RESUMED TURN CARRIES THE INSTANT ITS ROW RECORDS, the one the resumer
// derives it from, and not the run's first launch alone.
//
// The resume re-enters the turn that launched the run with no trigger to
// re-read, and a write it made before the suspend and makes again after the
// resume derives the same id: the launch bounds only what the turn minted
// after it. The row carries the launching turn's own instant for that, and
// the resumed turn's context — which is what its tools hand every writer —
// has to carry it.
//
// Mutation: describe the resumed turn with the run's first launch, and its
// writes read as minted after an adoption the launching turn predates.
func TestAResumedTurnCarriesTheInstantItsRowRecords(t *testing.T) {
	t.Parallel()
	company := instantCompany()
	triggered := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	run := sandbox.PendingRun{
		TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "eng",
		TriggeredAt: triggered, CreatedAt: triggered.Add(10 * time.Minute),
	}
	in := resumeInput{
		Company: company, Run: run,
		// AS THE RESUMER BUILDS IT, off the same row.
		Turn: &turnctx.Turn{
			RunID: run.TurnID, WorkKey: run.UnitOfWork(),
			TriggeredAt: run.TriggerInstant(),
			Seat:        company.Org.AgentSeatByHandle("eng"), Org: company.Org,
		},
	}

	tel := (&Engine{}).describeResume(t.Context(), company, in)
	got := tel.runnerTurn(company, 0, nil, "", turn.NoReply()).Context
	if got == nil {
		t.Fatal("the resumed turn carries no context")
	}
	if !got.TriggeredAt.Equal(triggered) {
		t.Fatalf("the resumed turn's context carries %v, want the launching "+
			"turn's %v off the row — describeResume stamps something other "+
			"than the instant the resumer derived", got.TriggeredAt, triggered)
	}
}

// EVERY WRITER A SEAT'S TOOLS ARE HANDED CARRIES THE TURN'S MINT INSTANT.
//
// The tracker's write authority takes the instant as provenance, one writer
// per actor, and a seat's tools reach it in four shapes — the item writer, the
// project writer, the dependency sequence and the merge sequence. A shape
// that dropped it would stamp every write of its kind with the call's own
// instant, which is the second decision above for exactly that kind of write.
//
// Compared whole against the writer the provenance names, and against one
// without the instant, so the comparison is seen to turn on it.
//
// Mutation: leave MintedAt out of any one of the four, and this fails.
func TestEveryWriterASeatIsHandedCarriesTheMintInstant(t *testing.T) {
	t.Parallel()
	base := &tracker.Writer{}
	e := &Engine{native: &native{trackerReader: &tracker.Reader{}, writer: base}}
	deps := e.workDeps(instantCompany())
	actor := builtin.Actor{
		Handle: "eng", Kind: tracker.AuthorAgent, TurnID: "run-1",
		Chain: []string{"pm"}, MintedAt: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC),
	}
	want := base.As(actor.Handle, actor.Kind, tracker.Provenance{
		TurnID: actor.TurnID, Chain: actor.Chain, MintedAt: actor.MintedAt,
	})
	unstamped := base.As(actor.Handle, actor.Kind, tracker.Provenance{
		TurnID: actor.TurnID, Chain: actor.Chain,
	})
	for name, shape := range map[string]func(builtin.Actor) any{
		"the item writer":         func(a builtin.Actor) any { return deps.Writer(a) },
		"the project writer":      func(a builtin.Actor) any { return deps.ProjectWriter(a) },
		"the dependency sequence": func(a builtin.Actor) any { return deps.Dependencies(a) },
		"the merge sequence":      func(a builtin.Actor) any { return deps.Merges(a) },
	} {
		got, ok := shape(actor).(*tracker.Writer)
		if !ok {
			t.Errorf("%s is not the tracker's writer: %T", name, shape(actor))
			continue
		}
		if !reflect.DeepEqual(got, want) || reflect.DeepEqual(got, unstamped) {
			t.Errorf("%s was derived without the turn's mint instant %v",
				name, actor.MintedAt)
		}
	}
}
