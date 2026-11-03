package code_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RETENTION PASS — the code-retention duty's body. Each case runs it
// against the whole path (a real broker, store and applier) on a clock the
// case sets, at a retention of an hour, because what the pass decides is a
// question about the rows its steps read and never about the calendar.

// keep is the retention a case passes unless a longer one is its subject: an
// hour.
const keep = time.Hour

// deleteAt deletes a repository the activation t1 declared, its delete
// applied at the broker instant at — the deleted_at a retention is judged
// from. Every case declares its repositories first: a declaration at t1
// after the fence stands at t2 is a superseded activation.
func (n *node) deleteAt(id string, at time.Time) {
	n.t.Helper()
	n.storedAt = func(uint64, time.Time) time.Time { return at }
	defer func() { n.storedAt = nil }()
	if deleted, err := n.store.DeleteRepo(n.t.Context(), n.activate(t2), id); err != nil || !deleted {
		n.t.Fatalf("DeleteRepo %s = %v, %v", id, deleted, err)
	}
}

// purged collects the names a pass hands its OnPurged, safe for the pass's
// goroutine and the case's.
type purged struct {
	mu    sync.Mutex
	names []string
}

func (p *purged) add(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = append(p.names, name)
	return nil
}

func (p *purged) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.names)
}

// passOver is a retention pass at the harness's clock over a store and a
// reader, handing every purged name to p — under the default scratch policy,
// with no live turn holding anything, and no committer to propose an expiry
// to: the cases built on it make no seat branch, and one that does hands the
// pass a committer of its own (expiryPassOn).
func passOver(s *code.Store, r *code.Reader, p *purged) *code.Retention {
	return &code.Retention{
		Store: s, Reader: r,
		Retention: func() time.Duration { return keep },
		OnPurged:  p.add,
		Policy:    func() code.LifecyclePolicy { return code.ScratchOf(config.Code{}) },
		Holds:     holdsNone,
		Propose:   proposeNone,
		Node:      "node-a",
		Now:       func() time.Time { return harnessAt },
	}
}

// holdsNone is a fleet where no live turn holds anything.
func holdsNone(context.Context, []string) (map[string]bool, error) { return map[string]bool{}, nil }

// proposeNone is a committer no expiry reaches: a case whose pass proposes one
// meets its error in the report's ExpiryFailed.
func proposeNone(context.Context, code.Proposal) (code.ProposeResult, error) {
	return code.ProposeResult{}, errors.New("this case's pass proposes no expiry")
}

// A PASS PURGES WHAT IS PAST RETENTION, AND ONLY THAT: a repository deleted
// longer ago than the retention is purged step by step to its marker and
// handed to OnPurged once; one deleted more recently is left as it was.
func TestAPassPurgesWhatIsPastRetention(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("old", described("old"), t1)
	n.ensure("recent", described("recent"), t1)
	old, recent := n.mustID("old"), n.mustID("recent")
	n.deleteAt(old, harnessAt.Add(-2*keep))
	n.deleteAt(recent, harnessAt.Add(-keep/2))
	// ONE ROW PAST A PART, so the purge takes two steps — a part, then the
	// purge itself — and a pass that stopped after one step would leave the
	// repository purging.
	n.seedRows(old, code.PurgePartRows+1, 0, 0)

	var p purged
	report, err := passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"old"}) || len(report.Failed) != 0 {
		t.Fatalf("the pass reported %+v, want old purged and nothing failed", report)
	}
	if report.Steps != 2 {
		t.Fatalf("the pass took %d steps, want 2: a part of PurgePartRows (%d) and the purge",
			report.Steps, code.PurgePartRows)
	}
	if got := p.got(); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("OnPurged was handed %v, want old once", got)
	}
	if _, found := n.repo("old"); found {
		t.Fatal("old still has a row after the pass purged it")
	}
	if marked := n.count(`SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?`, old); marked != 1 {
		t.Fatalf("old's purge left %d markers, want one", marked)
	}
	if row, found := n.repo("recent"); !found || row.status != string(code.RepoStatusDeleted) {
		t.Fatalf("recent, inside its retention, is %+v (found %v), want it deleted and kept", row, found)
	}
}

// refusingOne is a write authority that refuses every record on one
// repository's subject and passes every other to the real one.
type refusingOne struct {
	code.Publisher
	subject code.Subject
}

func (r refusingOne) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	if req.Subject.Kind == string(r.subject.Kind) && req.Subject.ID == r.subject.ID {
		return statelog.Result{}, errors.New("the broker refused this record")
	}
	return r.Publisher.Publish(ctx, req)
}

// A FAILING REPOSITORY DOES NOT STOP THE PASS: the first repository due, whose
// every record is refused, is reported failed with the refusal's sentence,
// and the pass goes on to purge the next one.
func TestAFailingRepositoryDoesNotStopThePass(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("stuck", described("stuck"), t1)
	n.ensure("freed", described("freed"), t1)
	stuck, freed := n.mustID("stuck"), n.mustID("freed")
	// stuck is the OLDER deletion, so the pass meets it first.
	n.deleteAt(stuck, harnessAt.Add(-3*keep))
	n.deleteAt(freed, harnessAt.Add(-2*keep))
	s, err := code.NewStore(code.StoreOptions{
		Publisher: refusingOne{Publisher: n.pub, subject: code.RepoSubject(stuck)},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var p purged
	report, err := passOver(s, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"freed"}) || !slices.Equal(p.got(), []string{"freed"}) {
		t.Fatalf("the pass purged %v and handed OnPurged %v, want freed alone", report.Purged, p.got())
	}
	if len(report.Failed) != 1 || !strings.Contains(report.Failed["stuck"], "the broker refused this record") {
		t.Fatalf("the pass reported failures %v, want stuck alone, with its refusal", report.Failed)
	}
	if row, found := n.repo("stuck"); !found || row.status != string(code.RepoStatusDeleted) {
		t.Fatalf("stuck is %+v (found %v), want it deleted, untouched", row, found)
	}
	if _, found := n.repo("freed"); found {
		t.Fatal("freed still has a row after the pass purged it")
	}
}

// A PURGED NAME CAN BE DECLARED AGAIN: the purge releases the name's claim, so
// the next declaration of it creates a NEW repository rather than finding
// the old one.
func TestAPurgedNameCanBeDeclaredAgain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	old := n.mustID("app")
	n.deleteAt(old, harnessAt.Add(-2*keep))
	var p purged
	if report, err := passOver(n.store, n.reader, &p).Pass(t.Context()); err != nil ||
		!slices.Equal(report.Purged, []string{"app"}) {
		t.Fatalf("Pass = %+v, %v; want app purged", report, err)
	}
	got := n.ensure("app", described("again"), t3)
	if !got.Changed || got.RepoID == old || got.Status != code.RepoStatusActive {
		t.Fatalf("declaring the purged name again = %+v, want a new active repository (not %s)",
			got, old)
	}
	if row, found := n.repo("app"); !found || row.id != got.RepoID || row.description != "again" {
		t.Fatalf("app's row is %+v (found %v), want the new repository %s", row, found, got.RepoID)
	}
}

// A REPOSITORY THAT MOVED ON SINCE THE LISTING IS NOT A FAILURE: the pass
// lists what is due from a stale read of its node's rows and every step
// decides again inside its own snapshot, so a repository a later activation
// restored — or restored and deleted again, inside its retention — after the
// listing read it is simply no longer due. Staged on two nodes: the listing
// is read on one held behind the change, the steps decided on one past it.
func TestARepositoryThatMovedOnSinceTheListingIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// moveOn takes the repository past the listing on the node the
		// steps decide on.
		moveOn func(a *node, id string)
		want   code.RepoStatus
	}{{
		name:   "restored",
		moveOn: func(a *node, id string) { a.ensure("app", described("one"), t3) },
		want:   code.RepoStatusActive,
	}, {
		name: "deleted again",
		moveOn: func(a *node, id string) {
			a.ensure("app", described("one"), t3)
			a.storedAt = func(uint64, time.Time) time.Time { return harnessAt.Add(-keep / 2) }
			defer func() { a.storedAt = nil }()
			if deleted, err := a.store.DeleteRepo(t.Context(), a.activate(harnessAt), id); err != nil || !deleted {
				t.Fatalf("delete it again = %v, %v", deleted, err)
			}
		},
		want: code.RepoStatusDeleted,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := newTwoNodes(t)
			a.ensure("app", described("one"), t1)
			id := a.mustID("app")
			old := func(uint64, time.Time) time.Time { return harnessAt.Add(-2 * keep) }
			a.storedAt, b.storedAt = old, old
			if deleted, err := a.store.DeleteRepo(t.Context(), a.activate(t2), id); err != nil || !deleted {
				t.Fatalf("DeleteRepo = %v, %v", deleted, err)
			}
			b.drain()
			a.storedAt, b.storedAt = nil, nil
			b.hold()
			tc.moveOn(a, id)

			var p purged
			report, err := passOver(a.store, b.reader, &p).Pass(t.Context())
			if err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if report.Steps != 0 || len(report.Purged) != 0 || len(report.Failed) != 0 || len(p.got()) != 0 {
				t.Fatalf("the pass reported %+v and handed OnPurged %v, want nothing at all",
					report, p.got())
			}
			if row, found := a.repo("app"); !found || row.id != id || row.status != string(tc.want) {
				t.Fatalf("app is %+v (found %v), want %s kept %s", row, found, id, tc.want)
			}
		})
	}
}

// A PURGE AN EARLIER PASS FINISHED IS HANDED ON AGAIN, and costs no record:
// a pass whose listing was read before the purge's last record applied steps
// the repository once, finds its marker and writes nothing — and still
// reports it purged and hands its name to OnPurged, which is why OnPurged
// must be safe to call twice for one name.
func TestAPurgeAnEarlierPassFinishedIsHandedOnAgain(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	a.ensure("app", described("one"), t1)
	old := func(uint64, time.Time) time.Time { return harnessAt.Add(-2 * keep) }
	a.storedAt, b.storedAt = old, old
	a.deleteAt(a.mustID("app"), harnessAt.Add(-2*keep))
	b.drain()
	a.storedAt, b.storedAt = nil, nil
	b.hold()
	var first purged
	if report, err := passOver(a.store, a.reader, &first).Pass(t.Context()); err != nil ||
		!slices.Equal(report.Purged, []string{"app"}) {
		t.Fatalf("the first pass = %+v, %v; want app purged", report, err)
	}

	before := a.end()
	var again purged
	report, err := passOver(a.store, b.reader, &again).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"app"}) || report.Steps != 1 || len(report.Failed) != 0 ||
		!slices.Equal(again.got(), []string{"app"}) || a.end() != before {
		t.Fatalf("a pass listing the purged repository reported %+v, handed OnPurged %v and "+
			"moved the log %d → %d; want app handed on with one step and nothing written",
			report, again.got(), before, a.end())
	}
}

// cancelling is a write authority that ends a context once its first record
// has been answered — a pass whose wall clock ran out between two steps.
type cancelling struct {
	code.Publisher
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelling) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := c.Publisher.Publish(ctx, req)
	c.once.Do(c.cancel)
	return res, err
}

// A PASS STOPS WHEN ITS CONTEXT ENDS, BETWEEN STEPS, and the next resumes
// where it stopped: what it did is reported beside the context's error, a
// repository it was purging is not a failure, and the purge it left
// half-done is completed by the next pass.
func TestAPassStopsBetweenStepsAndTheNextResumes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("big", described("big"), t1)
	n.ensure("next", described("next"), t1)
	big, next := n.mustID("big"), n.mustID("next")
	n.deleteAt(big, harnessAt.Add(-3*keep))
	n.deleteAt(next, harnessAt.Add(-2*keep))
	n.seedRows(big, code.PurgePartRows+1, 0, 0)

	ended, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancel()
	var p purged
	if report, err := passOver(n.store, n.reader, &p).Pass(ended); !errors.Is(err, context.Canceled) ||
		report.Steps != 0 {
		t.Fatalf("a pass on an ended context = %+v, %v; want nothing done and its error", report, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := code.NewStore(code.StoreOptions{
		Publisher: &cancelling{Publisher: n.pub, cancel: cancel},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	report, err := passOver(s, n.reader, &p).Pass(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pass = %v, want the context's ending", err)
	}
	if report.Steps != 1 || len(report.Purged) != 0 || len(report.Failed) != 0 || len(p.got()) != 0 {
		t.Fatalf("the cut pass reported %+v and handed OnPurged %v, want its one step and "+
			"nothing failed", report, p.got())
	}
	if row, found := n.repo("big"); !found || row.status != string(code.RepoStatusPurging) {
		t.Fatalf("big is %+v (found %v), want it purging after one part", row, found)
	}
	if _, found := n.repo("next"); !found {
		t.Fatal("next was purged by a pass whose context had ended")
	}

	report, err = passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("the next Pass: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"big", "next"}) || report.Steps != 2 ||
		!slices.Equal(p.got(), []string{"big", "next"}) {
		t.Fatalf("the next pass reported %+v and handed OnPurged %v, want big's last step "+
			"and next's one", report, p.got())
	}
}

// A PASS REFUSES WHAT IT CANNOT WORK WITHOUT, by name, before it reads a row:
// a pass missing its store, its reader or the epoch's retention, or handed a
// retention that is none, has nothing it could decide.
func TestAPassRefusesWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	for _, tc := range []struct {
		name string
		edit func(r *code.Retention)
		want string
	}{
		{"no store", func(r *code.Retention) { r.Store = nil }, "store"},
		{"no reader", func(r *code.Retention) { r.Reader = nil }, "reader"},
		{"no retention", func(r *code.Retention) { r.Retention = nil }, "code.deleted_retention_days"},
		{"a zero retention", func(r *code.Retention) {
			r.Retention = func() time.Duration { return 0 }
		}, "code.deleted_retention_days"},
		{"no scratch policy", func(r *code.Retention) { r.Policy = nil }, "code.scratch"},
		// NEVER AN EXPIRY THAT COULD NOT ASK: one would delete a scratch
		// repository a parked run is waiting to come back to.
		{"no holds", func(r *code.Retention) { r.Holds = nil }, "holds"},
		// THE REF EXPIRY DELETES THROUGH A COMMITTER, naming its node, or not
		// at all.
		{"no committer", func(r *code.Retention) { r.Propose = nil }, "committer"},
		{"no node", func(r *code.Retention) { r.Node = " " }, "node's id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p purged
			r := passOver(n.store, n.reader, &p)
			tc.edit(r)
			if _, err := r.Pass(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Pass = %v, want a refusal naming %s", err, tc.want)
			}
		})
	}
}

// holdingAfter is a write authority that holds its node's applier once its
// first record has been answered — a node whose applier fell behind between
// two steps of one purge, so the purge's last record resolves pending.
type holdingAfter struct {
	code.Publisher
	node *node
	once sync.Once
}

func (h *holdingAfter) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := h.Publisher.Publish(ctx, req)
	h.once.Do(h.node.hold)
	return res, err
}

// A PURGE IS HANDED ON ONLY ONCE THIS NODE'S ROWS HOLD IT. Its last record
// can resolve pending — durable on the log, not yet applied here — and
// OnPurged's re-declaration decides from this node's rows, where the
// repository would still read purging and the name would be refused. So the
// pass holds the name, reports it unhanded, and the next pass hands it on
// once this node has applied the purge: OnPurged never sees the row.
//
// Mutation: hand the name on whatever the last record's outcome, and OnPurged
// meets the row still purging.
func TestAPurgeIsHandedOnOnlyOnceThisNodesRowsHoldIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("big", described("big"), t1)
	big := n.mustID("big")
	n.deleteAt(big, harnessAt.Add(-2*keep))
	n.seedRows(big, code.PurgePartRows+1, 0, 0)
	s, err := code.NewStore(code.StoreOptions{
		Publisher: &holdingAfter{Publisher: n.pub, node: n},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var p purged
	r := passOver(s, n.reader, &p)
	r.OnPurged = func(ctx context.Context, name string) error {
		var status string
		err := n.db.Reader().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT status FROM code_repos WHERE name = ?`, name).
				Scan(&status)
		})
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("OnPurged was handed %s while this node's rows still hold it %s (%v)",
				name, status, err)
		}
		return p.add(ctx, name)
	}

	report, err := r.Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if report.Steps != 2 || len(report.Purged) != 0 || len(report.Failed) != 0 ||
		!slices.Equal(report.Unhanded, []string{"big"}) || len(p.got()) != 0 {
		t.Fatalf("the pass whose last record resolved pending reported %+v and handed OnPurged "+
			"%v, want its two steps, big held unhanded and nothing handed on", report, p.got())
	}
	if row, found := n.repo("big"); !found || row.status != string(code.RepoStatusPurging) {
		t.Fatalf("the premise: big is %+v (found %v) in this node's rows, want it still purging",
			row, found)
	}

	// STILL BEHIND, the next pass lists big from its stale rows and steps
	// nothing: the purge is done on the log, and a step would only meet the
	// record already there.
	before := n.end()
	report, err = r.Pass(t.Context())
	if err != nil {
		t.Fatalf("the pass still behind: %v", err)
	}
	if report.Steps != 0 || len(report.Failed) != 0 || len(report.Purged) != 0 ||
		!slices.Equal(report.Unhanded, []string{"big"}) || n.end() != before {
		t.Fatalf("a pass on a node still behind big's purge reported %+v and moved the log "+
			"%d → %d; want big still held and nothing stepped", report, before, n.end())
	}

	n.release()
	report, err = r.Pass(t.Context())
	if err != nil {
		t.Fatalf("the next Pass: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"big"}) || report.Steps != 0 || len(report.Unhanded) != 0 ||
		!slices.Equal(p.got(), []string{"big"}) {
		t.Fatalf("the next pass, the purge applied, reported %+v and handed OnPurged %v; want "+
			"big handed on with no step", report, p.got())
	}
}

// A NAME THE PASS COULD NOT HAND ON IS HANDED ON BY THE NEXT: OnPurged refused
// it, or the pass's context ended between the purge's last record and the
// hand-on. Either way the purge is done and the name is held, and the next
// pass gives it to OnPurged before it steps anything.
func TestANameThePassCouldNotHandOnIsHandedOnByTheNext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// first is the first pass, which purges app and does not hand it on.
		first func(t *testing.T, n *node, r *code.Retention) (code.RetentionReport, error)
	}{{
		name: "OnPurged refused it",
		first: func(t *testing.T, _ *node, r *code.Retention) (code.RetentionReport, error) {
			take := r.OnPurged
			r.OnPurged = func(context.Context, string) error {
				return errors.New("the reconcile could not run")
			}
			defer func() { r.OnPurged = take }()
			return r.Pass(t.Context())
		},
	}, {
		name: "the context ended first",
		first: func(t *testing.T, n *node, r *code.Retention) (code.RetentionReport, error) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := code.NewStore(code.StoreOptions{
				Publisher: &cancelling{Publisher: n.pub, cancel: cancel},
				DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
			})
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			store := r.Store
			r.Store = s
			defer func() { r.Store = store }()
			return r.Pass(ctx)
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.ensure("app", described("app"), t1)
			n.deleteAt(n.mustID("app"), harnessAt.Add(-2*keep))
			var p purged
			r := passOver(n.store, n.reader, &p)

			report, _ := tc.first(t, n, r)
			if report.Steps != 1 || len(report.Purged) != 0 || !slices.Equal(report.Unhanded, []string{"app"}) {
				t.Fatalf("the first pass reported %+v, want app purged in one step and held", report)
			}
			if _, found := n.repo("app"); found {
				t.Fatal("the premise: app still has a row after its purge")
			}
			report, err := r.Pass(t.Context())
			if err != nil {
				t.Fatalf("the next Pass: %v", err)
			}
			if !slices.Equal(report.Purged, []string{"app"}) || report.Steps != 0 ||
				len(report.Unhanded) != 0 || !slices.Equal(p.got(), []string{"app"}) {
				t.Fatalf("the next pass reported %+v and handed OnPurged %v; want app handed on "+
					"with no step", report, p.got())
			}
		})
	}
}

// A PURGE ONCE BEGUN IS FINISHED WHATEVER THE RETENTION. A pass cut short
// leaves a repository purging — half its rows gone, its name held, no restore
// — and the next pass may run at a LONGER retention: a revision lengthened
// it, or the duty moved to a node on another epoch. That retention saves only
// a repository still merely deleted; the purging one is listed and finished,
// and its name handed on.
//
// Mutation: list purging repositories only past the retention, as deleted
// ones are, and big stays purging.
func TestAPurgeOnceBegunIsFinishedWhateverTheRetention(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("big", described("big"), t1)
	n.ensure("kept", described("kept"), t1)
	big, kept := n.mustID("big"), n.mustID("kept")
	n.deleteAt(big, harnessAt.Add(-2*keep))
	n.deleteAt(kept, harnessAt.Add(-2*keep))
	n.seedRows(big, code.PurgePartRows+1, 0, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := code.NewStore(code.StoreOptions{
		Publisher: &cancelling{Publisher: n.pub, cancel: cancel},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var p purged
	if report, err := passOver(s, n.reader, &p).Pass(ctx); !errors.Is(err, context.Canceled) ||
		report.Steps != 1 {
		t.Fatalf("the cut pass = %+v, %v; want one step and the context's ending", report, err)
	}
	if row, found := n.repo("big"); !found || row.status != string(code.RepoStatusPurging) {
		t.Fatalf("the premise: big is %+v (found %v), want it purging after one part", row, found)
	}

	longer := passOver(n.store, n.reader, &p)
	longer.Retention = func() time.Duration { return 3 * keep }
	report, err := longer.Pass(t.Context())
	if err != nil {
		t.Fatalf("the pass at the longer retention: %v", err)
	}
	if !slices.Equal(report.Purged, []string{"big"}) || !slices.Equal(p.got(), []string{"big"}) {
		t.Fatalf("the pass at a retention of %v reported %+v and handed OnPurged %v; want big, "+
			"purging, finished and handed on", 3*keep, report, p.got())
	}
	if row, found := n.repo("kept"); !found || row.status != string(code.RepoStatusDeleted) {
		t.Fatalf("kept, merely deleted and inside the longer retention, is %+v (found %v); "+
			"want it saved", row, found)
	}
}

// THE LISTING IS READ A PAGE AT A TIME, IN THE ORDER IT IS STEPPED: every
// purging repository first, whatever its deletion's age, then the deleted
// ones oldest first — read here a repository a page, so every boundary
// between pages and between the two phases is crossed — and each page
// resumes AFTER the one before, so a repository that failed is not listed
// again in the same pass.
//
// Mutation: the deleted phase first, and the order changes; a page resuming
// at its phase's first row, and the pass steps the failed repository until
// its budget runs out.
func TestTheListingIsReadAPageAtATimeInTheOrderItIsStepped(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	for _, name := range []string{"newest", "oldest", "stuck", "middle", "inside"} {
		n.ensure(name, described(name), t1)
	}
	n.deleteAt(n.mustID("newest"), harnessAt.Add(-2*keep))
	n.deleteAt(n.mustID("oldest"), harnessAt.Add(-5*keep))
	n.deleteAt(n.mustID("stuck"), harnessAt.Add(-4*keep))
	n.deleteAt(n.mustID("middle"), harnessAt.Add(-3*keep))
	n.deleteAt(n.mustID("inside"), harnessAt.Add(-keep/2))
	// NEWEST IS PURGING, the youngest deletion due, which only a purging
	// phase ahead of the deleted one puts first.
	n.seedRows(n.mustID("newest"), code.PurgePartRows+1, 0, 0)
	if _, err := n.store.PurgeStep(t.Context(), n.mustID("newest"), keep); err != nil {
		t.Fatalf("PurgeStep: %v", err)
	}
	s, err := code.NewStore(code.StoreOptions{
		Publisher: refusingOne{Publisher: n.pub, subject: code.RepoSubject(n.mustID("stuck"))},
		DB:        n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var p purged
	r := passOver(s, n.reader, &p)
	// A BUDGET FAR PAST WHAT FIVE STEPS TAKE, so a pass that listed stuck
	// again and again ends here rather than in a minute.
	code.SetRetentionPace(r, nil, nil, 10*time.Second, 1)
	report, err := r.Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if want := []string{"newest", "oldest", "middle"}; !slices.Equal(report.Purged, want) ||
		len(report.Failed) != 1 || report.Failed["stuck"] == "" || report.Remain {
		t.Fatalf("the pass purged %v, failed %v (remain %v); want %v: the purge begun first, "+
			"then the deleted oldest first, stuck failed once, and inside its retention nothing",
			report.Purged, report.Failed, report.Remain, want)
	}
	if _, found := n.repo("inside"); !found {
		t.Fatal("inside, deleted inside the retention, was purged")
	}
}

// laneClock is a pass's clock and timer and a write authority that takes a
// fixed time a step on that clock — the pacing and the budget staged exactly,
// with no wall clock in them.
type laneClock struct {
	code.Publisher
	step time.Duration

	mu    sync.Mutex
	at    time.Time
	slept []time.Duration
}

func (c *laneClock) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := c.Publisher.Publish(ctx, req)
	c.mu.Lock()
	c.at = c.at.Add(c.step)
	c.mu.Unlock()
	return res, err
}

func (c *laneClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *laneClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.at = c.at.Add(d)
	return ctx.Err()
}

// A PASS PACES ITS STEPS AND STOPS AT ITS BUDGET, and the next resumes: after
// each step it waits RetentionIdlePerStep times as long as the step took — a
// third of the lane — and it takes no step whose turn would come at or after
// its budget's end. Staged on a clock only the steps and the waits move:
// steps of 30 ms in a budget of 200 ms begin at 0, 90 and 180 ms, and the
// fourth's turn at 270 ms is past it — the pass reports Remain, with no
// error, and the next pass takes the last step.
//
// Mutation: no wait after a step, and the pass takes all four in its budget;
// no budget, and it does too.
func TestAPassPacesItsStepsAndStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("big", described("big"), t1)
	big := n.mustID("big")
	n.deleteAt(big, harnessAt.Add(-2*keep))
	// THREE PARTS AND THE PURGE: four steps.
	n.seedRows(big, 3*code.PurgePartRows+1, 0, 0)
	const step, budget = 30 * time.Millisecond, 200 * time.Millisecond
	lane := &laneClock{Publisher: n.pub, step: step, at: harnessAt}
	s, err := code.NewStore(code.StoreOptions{
		Publisher: lane, DB: n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var p purged
	r := passOver(s, n.reader, &p)
	code.SetRetentionPace(r, lane.now, lane.sleep, budget, 0)
	report, err := r.Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wait := time.Duration(float64(step) * code.RetentionIdlePerStep)
	if report.Steps != 3 || !report.Remain || len(report.Purged) != 0 {
		t.Fatalf("the pass reported %+v, want three steps and Remain: steps of %v waiting %v "+
			"after each fit three in %v", report, step, wait, budget)
	}
	if got := lane.slept; !slices.Equal(got, []time.Duration{wait, wait}) {
		t.Fatalf("the pass waited %v between its steps, want %v twice", got, wait)
	}

	report, err = passOver(n.store, n.reader, &p).Pass(t.Context())
	if err != nil {
		t.Fatalf("the next Pass: %v", err)
	}
	if report.Steps != 1 || report.Remain || !slices.Equal(report.Purged, []string{"big"}) {
		t.Fatalf("the next pass reported %+v, want big's last step and big purged", report)
	}
}

// spans is a write authority that records when each of its records was
// asked for and answered.
type spans struct {
	code.Publisher
	mu   sync.Mutex
	took [][2]time.Time
}

func (s *spans) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	began := time.Now()
	res, err := s.Publisher.Publish(ctx, req)
	s.mu.Lock()
	s.took = append(s.took, [2]time.Time{began, time.Now()})
	s.mu.Unlock()
	return res, err
}

// A PASS HOLDS AT MOST A THIRD OF THE WRITE LANE, measured on the whole path
// and the wall clock: between one step's record answering and the next being
// asked for, the pass waits at least twice as long as the record took —
// whatever this machine's apply costs, since the wait is measured from it.
// What share of the pass's wall clock its records held is logged.
//
// Mutation: no wait after a step, and the records follow one another at once.
func TestAPassHoldsAtMostAThirdOfTheWriteLane(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("big", described("big"), t1)
	big := n.mustID("big")
	n.deleteAt(big, harnessAt.Add(-2*keep))
	n.seedRows(big, 4*code.PurgePartRows+1, 0, 0)
	timed := &spans{Publisher: n.pub}
	s, err := code.NewStore(code.StoreOptions{
		Publisher: timed, DB: n.db.Reader(), NodeID: n.id, Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var p purged
	if report, err := passOver(s, n.reader, &p).Pass(t.Context()); err != nil || report.Steps != 5 {
		t.Fatalf("Pass = %+v, %v; want four parts and the purge", report, err)
	}
	var busy time.Duration
	for i, span := range timed.took {
		took := span[1].Sub(span[0])
		busy += took
		if i+1 == len(timed.took) {
			break
		}
		want := time.Duration(float64(took) * code.RetentionIdlePerStep)
		if gap := timed.took[i+1][0].Sub(span[1]); gap < want {
			t.Errorf("step %d's record took %v and the next was asked for %v later, want at "+
				"least %v", i+1, took, gap, want)
		}
	}
	wall := timed.took[len(timed.took)-1][1].Sub(timed.took[0][0])
	t.Logf("the pass's records held the lane %v of its %v: %.0f%% over %d steps",
		busy, wall, 100*float64(busy)/float64(wall), len(timed.took))
}
