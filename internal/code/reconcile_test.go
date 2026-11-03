package code_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// gatedPublisher is a node's write authority that watches the publishes a
// reconcile makes on a repository's own subjects — a name's claim, a
// repository's settings or lifecycle — as they run: how many are in flight,
// the most there ever were, and how many began. Each such publish is HELD
// until hold of them are in flight together (or a moment has passed, for the
// tail of a run with fewer left), so a run that can reach its bound does; and
// refuse, when it answers non-nil for a publish, is that publish's answer in
// place of the broker's. lose, when set, is asked once the broker has answered
// any publish, and where it says so that answer is LOST: the publish answers
// OutcomeUnknown with no error, as statelog does for an append whose context
// ended while it was in flight — the cancelled append is classified by a probe
// on the same dead context, which cannot answer either. The append itself is
// made on a context that cannot end, so its record lands and its decide reads
// the snapshot it would have.
type gatedPublisher struct {
	code.Publisher
	hold   int
	refuse func(ctx context.Context, req statelog.Request) error
	lose   func(ctx context.Context, req statelog.Request) bool

	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
	most     int
	began    int
}

func newGatedPublisher(inner code.Publisher, hold int) *gatedPublisher {
	g := &gatedPublisher{Publisher: inner, hold: hold}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gatedPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	kind := code.ObjectKind(req.Subject.Kind)
	if kind != code.KindName && kind != code.KindRepo {
		return g.answer(ctx, req)
	}
	g.mu.Lock()
	g.inFlight++
	g.began++
	g.most = max(g.most, g.inFlight)
	g.cond.Broadcast()
	deadline := time.Now().Add(200 * time.Millisecond)
	for g.inFlight < g.hold && time.Now().Before(deadline) && ctx.Err() == nil {
		// A TIMED WAIT, so the tail of a run — fewer left than the bound
		// — goes on after a moment rather than for ever.
		wake := time.AfterFunc(10*time.Millisecond, g.cond.Broadcast)
		g.cond.Wait()
		wake.Stop()
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.cond.Broadcast()
		g.mu.Unlock()
	}()
	if g.refuse != nil {
		if err := g.refuse(ctx, req); err != nil {
			return statelog.Result{}, err
		}
	}
	return g.answer(ctx, req)
}

// answer is the broker's answer to req — or, where lose says so once it is
// in, that answer lost.
func (g *gatedPublisher) answer(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	if g.lose == nil {
		return g.Publisher.Publish(ctx, req)
	}
	res, err := g.Publisher.Publish(context.WithoutCancel(ctx), req)
	if err != nil || !g.lose(ctx, req) {
		return res, err
	}
	return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: req.OpID}, nil
}

// counts is the run so far: in flight now, the most ever, and how many began.
func (g *gatedPublisher) counts() (inFlight, most, began int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight, g.most, g.began
}

// gatedStore is n's lifecycle store over a gated publisher holding hold.
func gatedStore(t *testing.T, n *node, hold int) (*code.Store, *gatedPublisher) {
	t.Helper()
	g := newGatedPublisher(n.pub, hold)
	s, err := code.NewStore(code.StoreOptions{
		Publisher: g, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, g
}

// fresh is count new declarations named prefix-NNN.
func fresh(prefix string, count int) []code.Declared {
	out := make([]code.Declared, count)
	for i := range out {
		name := fmt.Sprintf("%s-%03d", prefix, i)
		out[i] = code.Declared{Name: name, Settings: described(name)}
	}
	return out
}

// WHAT CHANGED IS DECIDED SIDE BY SIDE, AND NEVER PAST THE BOUND.
//
// One repository at a time, a changed activation was one round trip of a
// write a repository — over a minute at the ten thousand a document may
// declare, inside the apply. Each repository is its own subjects, so the
// reconcile decides ReconcileInFlight of them at once: the bound is REACHED
// (a run of three bounds' worth holds that many in flight together) and
// never exceeded, every answer lands in its declaration's place, and nothing
// is in flight when the call returns. DeleteRepos is held to the same.
//
// Mutation: decide one at a time and the bound is never reached; start every
// declaration at once and it is exceeded.
func TestAChangedActivationIsDecidedSideBySide(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	a := n.activate(t1)
	declared := fresh("side", 3*code.ReconcileInFlight)
	s, g := gatedStore(t, n, code.ReconcileInFlight)
	got, err := s.EnsureRepos(t.Context(), a, declared)
	if err != nil {
		t.Fatalf("EnsureRepos: %v", err)
	}
	inFlight, most, _ := g.counts()
	if most != code.ReconcileInFlight || inFlight != 0 {
		t.Fatalf("creating %d repositories held at most %d publishes in flight and %d at its "+
			"return, want %d and none", len(declared), most, inFlight, code.ReconcileInFlight)
	}
	n.drain()
	ids := make([]string, len(got))
	for i, r := range got {
		row, found := n.repo(declared[i].Name)
		if r.Err != nil || !r.Changed || !found || row.id != r.RepoID {
			t.Fatalf("declaration %d (%s) answered %+v, its row %+v (held %v): every answer "+
				"belongs in its declaration's place", i, declared[i].Name, r, row, found)
		}
		ids[i] = r.RepoID
	}

	b := n.activate(t2)
	s, g = gatedStore(t, n, code.ReconcileInFlight)
	deleted, err := s.DeleteRepos(t.Context(), b, ids)
	if err != nil {
		t.Fatalf("DeleteRepos: %v", err)
	}
	inFlight, most, _ = g.counts()
	if most != code.ReconcileInFlight || inFlight != 0 {
		t.Fatalf("deleting %d repositories held at most %d publishes in flight and %d at its "+
			"return, want %d and none", len(ids), most, inFlight, code.ReconcileInFlight)
	}
	n.drain()
	for i, r := range deleted {
		if row, _ := n.repo(declared[i].Name); r.Err != nil || !r.Deleted ||
			row.status != string(code.RepoStatusDeleted) {
			t.Fatalf("deletion %d answered %+v with its row %s", i, r, row.status)
		}
	}
}

// A FAILURE EVERY REPOSITORY SHARES ENDS THE RECONCILE, and a repository's
// own does not.
//
// A log that refuses this node's every write (here log_full, as a log at its
// ceiling refuses) refuses the next repository the same way, so the decisions
// in flight are the last begun: the call answers that failure, once, as its
// error and as the answer of every repository it left undecided — the same
// value, so its caller can pass over each — and returns with nothing in
// flight. A refusal of one repository's own (a record this node cannot decode
// covering its paths alone) is that repository's answer, and the rest are
// decided.
//
// Mutation: treat log_full as a repository's own and every declaration is
// attempted; treat deferred as shared and the reconcile stops at the first.
func TestAFailureEveryRepositorySharesEndsTheReconcile(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	a := n.activate(t1)
	declared := fresh("full", 4*code.ReconcileInFlight)
	s, g := gatedStore(t, n, code.ReconcileInFlight)
	full := &statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "the log is at its ceiling"}
	g.refuse = func(context.Context, statelog.Request) error { return full }
	got, err := s.EnsureRepos(t.Context(), a, declared)
	if !errors.Is(err, full) {
		t.Fatalf("EnsureRepos on a full log = %v, want the refusal as the call's error", err)
	}
	inFlight, _, began := g.counts()
	if inFlight != 0 || began > 2*code.ReconcileInFlight {
		t.Fatalf("a reconcile on a full log began %d of %d decisions and left %d in flight, "+
			"want it ended at the first refusal — at most the ones then in flight, and the "+
			"ones racing it — with none in flight", began, len(declared), inFlight)
	}
	for i, r := range got {
		if r.Err != err { //nolint:errorlint // the identical value is the assertion
			t.Fatalf("declaration %d answered %+v, want the call's own error", i, r)
		}
	}

	b := n.activate(t2)
	s, g = gatedStore(t, n, code.ReconcileInFlight)
	own := fresh("own", 2*code.ReconcileInFlight)
	deferred := &statelog.Unavailable{Reason: statelog.ReasonDeferred, Detail: "a record covers it"}
	g.refuse = func(_ context.Context, req statelog.Request) error {
		if slices.Contains(req.Scope.Paths, code.NamePath(code.NameToken(own[3].Name))) {
			return deferred
		}
		return nil
	}
	got, err = s.EnsureRepos(t.Context(), b, own)
	if err != nil {
		t.Fatalf("EnsureRepos with one repository refused its own way = %v, want the rest "+
			"decided", err)
	}
	for i, r := range got {
		switch {
		case i == 3 && !errors.Is(r.Err, statelog.ErrUnavailable):
			t.Errorf("the refused repository answered %+v, want its own refusal", r)
		case i != 3 && (r.Err != nil || !r.Changed):
			t.Errorf("declaration %d answered %+v, want it created beside the refused one", i, r)
		}
	}
}

// A RECONCILE WHOSE CONTEXT ENDS RETURNS WITH NOTHING IN FLIGHT: the decisions
// running are ended, no more begin, and every one left undecided answers the
// context's end — the apply that asked has gone, a drain or an engine
// stopping, and a decision still running after it would publish for an apply
// nobody is waiting on.
func TestAReconcileWhoseContextEndsLeavesNothingInFlight(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	a := n.activate(t1)
	declared := fresh("gone", 4*code.ReconcileInFlight)
	s, g := gatedStore(t, n, code.ReconcileInFlight)
	ctx, cancel := context.WithCancel(t.Context())
	g.refuse = func(context.Context, statelog.Request) error {
		cancel()
		return nil
	}
	got, err := s.EnsureRepos(ctx, a, declared)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureRepos with its context ended = %v, want the cancellation", err)
	}
	if inFlight, _, began := g.counts(); inFlight != 0 || began > 2*code.ReconcileInFlight {
		t.Fatalf("a cancelled reconcile began %d of %d decisions and left %d in flight",
			began, len(declared), inFlight)
	}
	undecided := 0
	for _, r := range got {
		if r.Err == err { //nolint:errorlint // the identical value is the assertion
			undecided++
		}
	}
	if undecided < len(declared)-2*code.ReconcileInFlight {
		t.Fatalf("%d declarations answer the call's end, want every one it left undecided",
			undecided)
	}
}

// A DECISION STILL RUNNING WHEN ANOTHER MEETS A FAILURE EVERY REPOSITORY
// SHARES IS ENDED, not waited out: a publish on a node whose applier has
// stalled waits its whole budget for an answer the failure already gives, and
// a reconcile that waited for each one in flight would hold the apply for it.
// Here every decision but the first stalls until its context ends — the call
// returns once the first has failed, not when the stalls would.
//
// Mutation: record the failure without ending the decisions in flight and
// the call waits out every stall.
func TestADecisionInFlightIsEndedByAFailureEveryRepositoryShares(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	a := n.activate(t1)
	declared := fresh("stall", 2*code.ReconcileInFlight)
	s, g := gatedStore(t, n, code.ReconcileInFlight)
	full := &statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "the log is at its ceiling"}
	first := code.NamePath(code.NameToken(declared[0].Name))
	const stall = 20 * time.Second
	g.refuse = func(ctx context.Context, req statelog.Request) error {
		if slices.Contains(req.Scope.Paths, first) {
			return full
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stall):
			return nil
		}
	}
	began := time.Now()
	_, err := s.EnsureRepos(t.Context(), a, declared)
	if !errors.Is(err, full) {
		t.Fatalf("EnsureRepos = %v, want the shared refusal", err)
	}
	if took := time.Since(began); took > stall/2 {
		t.Fatalf("the reconcile took %v, waiting out the decisions in flight rather than "+
			"ending them at the refusal every repository shares", took)
	}
	if inFlight, _, _ := g.counts(); inFlight != 0 {
		t.Fatalf("%d decisions in flight at the call's return", inFlight)
	}
}

// A DECISION THE RECONCILE'S END LEFT UNANSWERED IS PART OF THAT END, never a
// failure of its own repository.
//
// A reconcile is ended on purpose — a newer activation's request, or the
// engine stopping — while decisions are in flight, each mid-append. statelog
// answers such an append OutcomeUnknown with no error (the cancelled append is
// classified by a probe on the same dead context), and a decision that read
// that as its own repository's unknown outcome was reported as a repository
// the reconcile could not apply: an ERROR line apiece beside the one line that
// said the reconcile was superseded. Here every decision in flight has its
// record appended, then the call's context is ended, and every answer is lost:
// the call's error is the context's end, and every declaration — those in
// flight and, where there were more, those never begun — answers that same
// value, so a caller that says the ending once says nothing else.
//
// Both shapes: as many declarations as are decided at once, so none is left
// to begin (where the call used to answer no error at all), and twice that.
//
// Mutation: keep the answer of a decision that failed once the run had ended,
// and have an unknown outcome not say its context ended, and the lost answers
// come back as each repository's own.
func TestADecisionTheReconcilesEndLeftUnansweredIsPartOfIt(t *testing.T) {
	t.Parallel()
	for _, count := range []int{code.ReconcileInFlight, 2 * code.ReconcileInFlight} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			a := n.activate(t1)
			declared := fresh("lost", count)
			s, g := gatedStore(t, n, code.ReconcileInFlight)
			ctx, end := context.WithCancelCause(t.Context())
			var answered atomic.Int32
			g.lose = func(ctx context.Context, req statelog.Request) bool {
				if code.ObjectKind(req.Subject.Kind) != code.KindName {
					return false
				}
				// THE LAST OF THE DECISIONS IN FLIGHT ENDS THE RUN, so
				// every one of them is mid-append when it ends.
				if answered.Add(1) == int32(code.ReconcileInFlight) {
					end(fmt.Errorf("%w: a newer activation asked", code.ErrSuperseded))
				}
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Error("the reconcile's context never ended")
				}
				return true
			}
			got, err := s.EnsureRepos(ctx, a, declared)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("EnsureRepos ended mid-append = %v, want the context's end as the "+
					"call's error", err)
			}
			if inFlight, _, _ := g.counts(); inFlight != 0 {
				t.Fatalf("%d decisions in flight at the call's return", inFlight)
			}
			for i, r := range got {
				if r.Err != err { //nolint:errorlint // the identical value is the assertion
					t.Errorf("declaration %d answered %+v, want the call's own error: a decision "+
						"the call's end left unanswered is no failure of its repository", i, r)
				}
			}
		})
	}
}

// THE RUN'S END ANSWERS FOR WHAT IT ENDED, AND FOR NOTHING ELSE.
//
// decideEach answers the failure that ended its run and which decisions that
// failure is the answer of: every one never begun, every one that met it, and
// every one that FAILED once the run had ended — whatever its error says,
// since a failure met on a dead context is the context's doing — while a
// decision that succeeded after the end keeps its answer: it decided its
// repository. Both ways a run ends: its context, and a failure every
// repository shares.
//
// Mutation: mark only the decisions whose error is itself a shared failure
// and the odd decisions here keep their own failures; mark every decision in
// flight at the end and the even ones lose their successes.
func TestTheRunsEndAnswersForWhatItEnded(t *testing.T) {
	t.Parallel()
	const n = 8
	full := &statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "the log is at its ceiling"}
	for _, tc := range []struct {
		name string
		// shared is the failure decision 0 meets once every other
		// decision is in flight; nil has decision 0 end the context then.
		shared error
	}{
		{"its context", nil},
		{"a failure every repository shares", full},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var begun atomic.Int32
			all := make(chan struct{})
			decide := func(run context.Context, k int) error {
				if begun.Add(1) == n {
					close(all)
				}
				if k == 0 {
					<-all
					if tc.shared != nil {
						return tc.shared
					}
					cancel()
				}
				<-run.Done()
				if k%2 == 1 {
					return errors.New("a failure that names no context")
				}
				return nil
			}
			ended, err := code.DecideEach(ctx, n, decide)
			want := tc.shared
			if want == nil {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("decideEach = %v, want %v", err, want)
			}
			if len(ended) != n {
				t.Fatalf("decideEach answered for %d decisions, want %d", len(ended), n)
			}
			for k, e := range ended {
				switch {
				case k == 0 && tc.shared != nil && !e:
					t.Errorf("decision 0 met the shared failure and keeps its own answer")
				case k%2 == 1 && !e:
					t.Errorf("decision %d failed once the run had ended and keeps its own answer", k)
				case k%2 == 0 && (k > 0 || tc.shared == nil) && e:
					t.Errorf("decision %d succeeded once the run had ended and the end answers "+
						"for it: it decided its repository", k)
				}
			}
		})
	}
}

// AN UNKNOWN OUTCOME ON A CONTEXT THAT ENDED SAYS IT ENDED.
//
// Every write the lifecycle store makes — the fence, a create, a settings
// change, a deletion, a purge step — answers an outcome nobody could establish
// as an error, and statelog answers one for an append its context ended in the
// middle of. That error wraps the context's end, so whatever reads it — a
// reconcile ended on purpose, a retention pass stopping — reads the ending
// rather than a write of its own that failed.
//
// Mutation: drop the context's end from any one write's unknown error and its
// row goes red.
func TestAnUnknownOutcomeOnAnEndedContextSaysItEnded(t *testing.T) {
	t.Parallel()
	// existing is a repository web the activation t1 declared, and the
	// activation t2 a write about it is made at.
	existing := func(n *node) (code.Activation, string) {
		n.ensure("web", described("one"), t1)
		return n.activate(t2), n.mustID("web")
	}
	for _, tc := range []struct {
		name string
		kind code.ObjectKind
		// setup is what the write needs, made through the node's own store;
		// write is the one write whose answer is lost.
		setup func(n *node) (code.Activation, string)
		write func(ctx context.Context, s *code.Store, a code.Activation, id string) error
	}{
		{"the fence", code.KindCompany,
			func(*node) (code.Activation, string) { return code.Activation{}, "" },
			func(ctx context.Context, s *code.Store, _ code.Activation, _ string) error {
				_, err := s.Activate(ctx, t3)
				return err
			}},
		{"a create", code.KindName,
			func(n *node) (code.Activation, string) { return n.activate(t1), "" },
			func(ctx context.Context, s *code.Store, a code.Activation, _ string) error {
				_, err := s.EnsureRepo(ctx, a, code.Declared{Name: "fresh", Settings: described("x")})
				return err
			}},
		{"a settings change", code.KindRepo, existing,
			func(ctx context.Context, s *code.Store, a code.Activation, _ string) error {
				_, err := s.EnsureRepo(ctx, a, code.Declared{Name: "web", Settings: described("two")})
				return err
			}},
		{"a deletion", code.KindRepo, existing,
			func(ctx context.Context, s *code.Store, a code.Activation, id string) error {
				_, err := s.DeleteRepo(ctx, a, id)
				return err
			}},
		{"a purge step", code.KindRepo,
			func(n *node) (code.Activation, string) {
				_, id := existing(n)
				n.deleteAt(id, harnessAt.Add(-48*time.Hour))
				return code.Activation{}, id
			},
			func(ctx context.Context, s *code.Store, _ code.Activation, id string) error {
				_, err := s.PurgeStep(ctx, id, time.Hour)
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			a, id := tc.setup(n)
			s, g := gatedStore(t, n, 1)
			ctx, end := context.WithCancel(t.Context())
			defer end()
			g.lose = func(_ context.Context, req statelog.Request) bool {
				if code.ObjectKind(req.Subject.Kind) != tc.kind {
					return false
				}
				end()
				return true
			}
			if err := tc.write(ctx, s, a, id); !errors.Is(err, context.Canceled) {
				t.Fatalf("a write whose answer its context's end lost = %v, want an error "+
					"that says the context ended", err)
			}
		})
	}
}
