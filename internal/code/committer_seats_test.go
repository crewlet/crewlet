package code_test

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// A SEAT'S PUSH THROUGH THE COMMITTER: decided on a node that judges it from
// the chart of its own epoch, and on no other — never on a node confirmed
// behind the company's configuration, nor on one that holds no chart yet,
// each of which hands it to the repository's next committer — and counted on
// its company day by the apply of the record that lands it.

// seatProposal is one push to repo by seat handle of undecided commands, as
// the engine stamps a seat's push: its run, and the way its pack was built.
func seatProposal(repo, handle string, cmds ...code.Command) code.Proposal {
	p := proposal(repo, cmds...)
	p.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: handle,
		Run: code.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(), WorkBackend: "native", WorkKey: "ENG-7"}}
	return p
}

// A COMMITTER BEHIND THE COMPANY'S CONFIGURATION DECIDES NO SEAT'S PUSH
// (M3-O8's hard arm): while its gate answers that the node is confirmed behind
// — shed or stuck — a seat's push is refused code.ErrNotServing, decided by
// nobody and appending nothing, whether it was proposed while the gate was
// shut or queued before and its batch formed after; a person's push is
// decided as ever; and with the gate open again, the seat's push under the
// same push id lands once.
//
// Mutation: drop the gate (behindFor answering "") — the seat's push is
// decided and lands on the node behind, red.
func TestACommitterBehindTheConfigurationDecidesNoSeatsPush(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	var behind atomic.Bool
	current := func(o *code.CommitterOptions) { o.Current = func() bool { return !behind.Load() } }

	c, _ := committerOn(t, n, n.pub, current)
	behind.Store(true)
	seat := seatProposal(repo, "dev", update("refs/heads/seats/dev/x", code.ZeroOID, oidOf("X")))
	before := n.end()
	if res, err := c.Propose(ctx, seat); !errors.Is(err, code.ErrNotServing) ||
		!strings.Contains(err.Error(), "behind the company's configuration") {
		t.Fatalf("a seat's push on a committer behind the configuration = %+v, %v; want ErrNotServing saying so",
			res, err)
	}
	if got := n.end(); got != before {
		t.Fatalf("the log moved from %d to %d for a push nobody decided", before, got)
	}
	requireOK(t, await(t, proposeAsync(ctx, c, proposal(repo, update("refs/heads/feature", code.ZeroOID,
		oidOf("F"))))))
	behind.Store(false)
	requireOK(t, await(t, proposeAsync(ctx, c, seat)))
	if got := n.target(repo, "refs/heads/seats/dev/x"); got != oidOf("X") {
		t.Fatalf("the seat's push, the gate open again, left its ref at %q", got)
	}

	// QUEUED BEFORE THE GATE SHUT, its batch formed after: a person's batch
	// holds the repository's queue at its publish while the seat's push
	// waits behind it, and the gate shuts in between.
	gate := newGate(n.pub)
	held, _ := committerOn(t, n, gate, current)
	first := proposeAsync(ctx, held, proposal(repo, update("refs/heads/held", code.ZeroOID, oidOf("H"))))
	eventually(t, "the first batch at its publish", func() bool { return gate.calls.Load() == 1 })
	queued := seatProposal(repo, "dev", update("refs/heads/seats/dev/y", code.ZeroOID, oidOf("Y")))
	waiting := proposeAsync(ctx, held, queued)
	eventually(t, "the seat's push in the queue", func() bool { return code.CommitterQueued(held, repo) == 1 })
	behind.Store(true)
	gate.open()
	requireOK(t, await(t, first))
	if a := await(t, waiting); !errors.Is(a.err, code.ErrNotServing) {
		t.Fatalf("a seat's push queued before the gate shut and batched after = %+v, %v; want ErrNotServing",
			a.res, a.err)
	}
	if got := n.target(repo, "refs/heads/seats/dev/y"); got != "" {
		t.Fatalf("the seat's push batched behind the gate left its ref at %q", got)
	}
	behind.Store(false)
	requireOK(t, await(t, proposeAsync(ctx, held, queued)))
}

// A COMMITTER WITH NO CHART HANDS A SEAT'S PUSH ON: a node that has installed
// no epoch yet holds no chart to judge a seat's grant by, so in a batch of a
// person's push and a seat's, the seat's is refused code.ErrNotServing — a
// refusal about the node, which the router hands to the repository's next
// committer — and decided by nobody, while the person's lands. Never the
// seat's refused not_permitted by a chart nobody read, which would record it.
//
// Mutation: decide the batch whole with the empty rules — the decide refuses
// its caller's bug, and the person's push fails with it, red.
func TestAChartlessCommitterHandsASeatsPushOn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate, func(o *code.CommitterOptions) {
		o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{}, false }
	})
	first := proposeAsync(ctx, c, proposal(repo, update("refs/heads/held", code.ZeroOID, oidOf("H"))))
	eventually(t, "the first batch at its publish", func() bool { return gate.calls.Load() == 1 })
	person := proposeAsync(ctx, c, proposal(repo, update("refs/heads/person", code.ZeroOID, oidOf("P"))))
	seat := proposeAsync(ctx, c, seatProposal(repo, "dev", update("refs/heads/seats/dev/x", code.ZeroOID,
		oidOf("X"))))
	eventually(t, "both pushes in the queue", func() bool { return code.CommitterQueued(c, repo) == 2 })
	gate.open()
	requireOK(t, await(t, first))
	requireOK(t, await(t, person))
	if a := await(t, seat); !errors.Is(a.err, code.ErrNotServing) || !strings.Contains(a.err.Error(), "no chart") {
		t.Fatalf("a seat's push on a committer with no chart = %+v, %v; want ErrNotServing saying it holds none",
			a.res, a.err)
	}
	if got := n.target(repo, "refs/heads/seats/dev/x"); got != "" {
		t.Fatalf("the seat's push left its ref at %q on a committer that judged nothing", got)
	}
}

// A SEAT'S PUSH LANDS, COUNTED ON ITS COMPANY DAY: a full_access seat of the
// owning unit's push is decided by its grant on the committer's chart and
// lands — its row naming the seat as its actor — and its record's apply counts
// it on the day the committer stamped; past code.limits.pushes_per_seat_day,
// the seat's next push is refused push_quota whole, appending nothing, while a
// person's push beside it lands, counted nowhere.
//
// Mutation: stamp no day on a seat's entry — the apply counts nothing, and the
// second push lands past the quota, red.
func TestASeatsPushLandsCountedOnItsDay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
		o.Limits = func() code.Limits {
			l := code.DefaultLimits()
			l.PushesPerSeatDay = 1
			return l
		}
	})
	a := await(t, proposeAsync(ctx, c, seatProposal(repo, "dev", update("refs/heads/seats/dev/x", code.ZeroOID,
		oidOf("X")))))
	requireOK(t, a)
	if got := n.count(`SELECT pushes FROM code_seat_days WHERE seat = 'dev' AND day = '2031-04-02'`); got != 1 {
		t.Fatalf("the seat's push is counted %d times on its day, want once", got)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE actor = 'dev' AND actor_kind = 'agent'`); got != 1 {
		t.Fatalf("%d push rows name the seat as their actor, want its one push", got)
	}
	before := n.end()
	over := await(t, proposeAsync(ctx, c, seatProposal(repo, "dev", update("refs/heads/seats/dev/y",
		code.ZeroOID, oidOf("Y")))))
	if over.err != nil || over.res.Answer.Refused != code.RefusalPushQuota || over.res.Outcome != "" ||
		!strings.Contains(over.res.Answer.Detail, "2031-04-03T00:00:00Z") {
		t.Fatalf("the seat's push past its quota = %+v, %v; want push_quota, decided nowhere, naming when the "+
			"count starts again", over.res, over.err)
	}
	if got := n.end(); got != before {
		t.Fatalf("a push refused push_quota moved the log from %d to %d: the refusal is no record's", before, got)
	}
	requireOK(t, await(t, proposeAsync(ctx, c, proposal(repo, update("refs/heads/person", code.ZeroOID,
		oidOf("P"))))))
	if got := n.count(`SELECT COUNT(*) FROM code_seat_days WHERE seat != 'dev'`); got != 0 {
		t.Fatalf("a person's push is counted on %d seats' days", got)
	}
}
