package estate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// askerSlack is what each request these cases send leaves the serving node
// beyond the reply's margin (code.ReplyMargin): a wait the margin bounds ends
// that far in, and one it does not runs on to the deadline a whole margin
// later — inside every budget a serving node's waits have of their own.
const askerSlack = 400 * time.Millisecond

// askDirectly sends one task read straight to node's subject, carrying floors
// and a deadline askerSlack and a reply's margin away, and answers the reply
// it heard. A node silent until the deadline, or heard from with less than
// half the margin left — an answer its asker may already have given up on —
// fails the case.
func askDirectly(t *testing.T, f *fleet, node string, floors []statelog.Position) reply {
	t.Helper()
	args, err := json.Marshal(tasksArgs{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), code.ReplyMargin+askerSlack)
	defer cancel()
	deadline, _ := ctx.Deadline()
	req, sealed := signed(t, fleetEnvelopes, request{Op: opTasks.spec.name, Args: args,
		Floors: floors, Deadline: deadline, From: "agent-9"})
	replies, err := f.start(t).Ask(ctx, Subject(node), sealed, 1)
	left := time.Until(deadline)
	if err != nil || len(replies) != 1 {
		t.Fatalf("%s answered %d replies (%v) before its asker stopped listening, want one", node,
			len(replies), err)
	}
	if left < code.ReplyMargin/2 {
		t.Fatalf("%s answered %v before its asker stops listening, want about the %v margin — "+
			"a wait that runs to the deadline answers nobody", node, left.Round(time.Millisecond),
			code.ReplyMargin)
	}
	rep, why, err := fleetEnvelopes.openReply(replies[0], req.ID)
	if err != nil || why != "" {
		t.Fatalf("the reply from %s was not believed: %q, %v", node, why, err)
	}
	return rep
}

// A NODE BEHIND ITS ASKER'S FLOOR SAYS SO WHILE ITS ASKER IS STILL LISTENING:
// its wait for the floor ends a reply's margin short of the request's deadline
// rather than at it (statelog.WithReplyMargin), here well inside the floor
// wait's own budget (statelog.ReadBudget). Waited out to the deadline, the
// node's honest `behind` reached nobody, and the asker heard a lagging node as
// a wedged one: asked it last for suspectFor and told the placement it went
// unanswered.
func TestANodeBehindItsAskersFloorSaysSoWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	if code.ReplyMargin+askerSlack >= statelog.ReadBudget {
		t.Fatalf("the premise: the asker's %v must end before the floor wait's own %v would",
			code.ReplyMargin+askerSlack, statelog.ReadBudget)
	}
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.behind = true })

	rep := askDirectly(t, f, "data-a",
		[]statelog.Position{{Stream: trackerStream, Generation: 1, Seq: 5}})
	if rep.Unserved != unservedBehind || !strings.Contains(rep.Detail, "all its asker's attempt left") {
		t.Fatalf("a node behind its asker's floor answered %+v, want it unserved %q naming what "+
			"its asker left", rep, unservedBehind)
	}
}

// A READ'S OWN WAITS END WHILE ITS ASKER IS STILL LISTENING TOO. The node has
// its asker's floor; the read is a linearizable one through a REAL statelog
// reader, whose barrier commits at once and whose position this node never
// reaches — a node that lags the log's end. The reader's wait for it ends a
// reply's margin short of the request's deadline, because the serving node
// marks the request it answers (statelog.WithReplyMargin): refused `behind`,
// and heard.
func TestAReadsOwnWaitsEndWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.levelRead = laggingLinearizableRead(t) })

	rep := askDirectly(t, f, "data-a", nil)
	var refusal *statelog.Refused
	if rep.Err == nil || !errors.As(decodeError(rep.Err), &refusal) ||
		refusal.Code != statelog.RefuseBehind {
		t.Fatalf("a node lagging its read's barrier answered %+v, want the read refused %q", rep,
			statelog.RefuseBehind)
	}
	if !strings.Contains(refusal.Detail, "all its asker's attempt left") {
		t.Errorf("the refusal reads %q, which does not say the asker's time is what ran out",
			refusal.Detail)
	}
}

// laggingLinearizableRead is a linearizable tracker read through a real
// statelog reader on a node that never reaches the log's end: its barrier
// commits at once and its applier never gets there, so the read waits for the
// barrier's position until something ends the wait and is then refused
// `behind`. The read is refused before it opens a transaction, so its
// database is one no read may touch.
func laggingLinearizableRead(t *testing.T) func(context.Context) error {
	t.Helper()
	return laggingReadOn(t, committedAppender{}, nil)
}

// laggingReadOn is [laggingLinearizableRead] appending its barriers to log,
// and counting into waits — where it is not nil — every wait it begins with
// time left to spend ([laggingWaiter]), so a case can count both across the
// nodes of a fleet.
func laggingReadOn(t *testing.T, log statelog.Appender, waits *atomic.Int64) func(context.Context) error {
	t.Helper()
	domain := tracker.Domain{}
	spec := domain.Stream()
	reserve, err := statelog.NewReserve(spec,
		func(context.Context) (statelog.Usage, error) { return statelog.Usage{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	// SIGNED UNDER THE SUITE'S FLEET KEYRING, as every barrier is: the read
	// index refuses to be built without a signer.
	signer, err := statelog.NewSigner(domain.Name(), fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(env statelog.Envelope) ([]byte, error) { return json.Marshal(env) }
	// THREE MEMBERS, a fleet's log, though no barrier here fails to commit:
	// what the count decides is only what an unconfirmed one is refused as.
	threeMembers := func() int { return 3 }
	index, err := statelog.NewReadIndex(domain, spec, log, threeMembers, signer, reserve, encode,
		func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := statelog.Position{Stream: spec.Name, Generation: 1, Seq: 1}
	reader, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: domain, Spec: spec, Mode: statelog.ModeNormal,
		DB: untouchedDB{t}, Index: index, Waiter: laggingWaiter{at: at, waits: waits},
		Health: func() (statelog.Health, error) {
			lag, first, floor := uint64(0), uint64(1), uint64(0)
			return statelog.Health{Position: at, AppliedThrough: at.Seq, Drained: true,
				Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:   &lag, FirstSeq: &first, TrimFloor: &floor}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context) error {
		_, err := reader.Read(ctx, statelog.Query{Level: statelog.ReadLinearizable,
			Scope: statelog.ScopeSet{Paths: []string{"t/c/ENG"}}},
			func(*sql.Tx) error { return nil })
		return err
	}
}

// committedAppender is a broker that commits every barrier at once.
type committedAppender struct{}

func (committedAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return 7, false, nil
}

func (committedAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// laggingWaiter is an applier that never reaches what a read waits for. waits,
// where it is not nil, counts every wait begun with time left to spend — each
// runs to its end, since nothing is ever reached — and none begun with none: a
// read that a node asked before it already waited out waits no time.
type laggingWaiter struct {
	at    statelog.Position
	waits *atomic.Int64
}

func (w laggingWaiter) Reached() statelog.Position { return w.at }

func (w laggingWaiter) WaitReached(ctx context.Context, _ statelog.Position) error {
	w.count(ctx)
	<-ctx.Done()
	return ctx.Err()
}

func (w laggingWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, _ statelog.Position) error {
	w.count(ctx)
	<-ctx.Done()
	return ctx.Err()
}

func (w laggingWaiter) count(ctx context.Context) {
	if w.waits != nil && ctx.Err() == nil {
		w.waits.Add(1)
	}
}

// untouchedDB is a database no read may open a transaction on.
type untouchedDB struct{ t *testing.T }

func (d untouchedDB) Read(context.Context, func(*sql.Tx) error) error {
	d.t.Error("a read refused before its transaction opened one")
	return errors.New("no transaction here")
}
