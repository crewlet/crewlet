package code_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// countingPublisher is a node's write authority that remembers the subject
// kind of every publish it is asked for — what a reconcile costs, counted.
type countingPublisher struct {
	code.Publisher
	mu    sync.Mutex
	kinds []string
}

func (c *countingPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	c.mu.Lock()
	c.kinds = append(c.kinds, req.Subject.Kind)
	c.mu.Unlock()
	return c.Publisher.Publish(ctx, req)
}

// published is every subject kind asked for so far, sorted.
func (c *countingPublisher) published() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(slices.Values(c.kinds))
}

// countingStore is n's lifecycle store over a publisher that counts.
func countingStore(t *testing.T, n *node) (*code.Store, *countingPublisher) {
	t.Helper()
	counting := &countingPublisher{Publisher: n.pub}
	s, err := code.NewStore(code.StoreOptions{
		Publisher: counting, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, counting
}

// ENSUREREPOS ANSWERS WHAT ENSUREREPO ANSWERS, declaration by declaration —
// an unchanged repository and one this activation already deleted from the
// one read, everything else through EnsureRepo — and publishes for what
// changed alone: the batch read, then a claim for a new name and a claim and
// a settle for changed settings.
func TestEnsureReposAnswersWhatEnsureRepoAnswers(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	kept := n.ensure("kept", described("one"), t1)
	edited := n.ensure("edited", described("one"), t1)
	gone := n.ensure("gone", described("one"), t1)
	a := n.activate(t2)
	if deleted, err := n.store.DeleteRepo(t.Context(), a, gone.RepoID); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	n.drain()

	s, counting := countingStore(t, n)
	before := n.end()
	got, err := s.EnsureRepos(t.Context(), a, []code.Declared{
		{Name: "kept", Settings: described("one")},
		{Name: "edited", Settings: described("two")},
		{Name: "fresh", Settings: described("new")},
		{Name: "gone", Settings: described("one")},
		{Name: "Not A Name", Settings: described("one")},
	})
	if err != nil {
		t.Fatalf("EnsureRepos: %v", err)
	}
	n.drain()
	if len(got) != 5 {
		t.Fatalf("EnsureRepos answered %d declarations of 5", len(got))
	}
	for name, check := range map[int]func(code.EnsureResult) bool{
		0: func(r code.EnsureResult) bool {
			return r.Err == nil && !r.Changed && r.RepoID == kept.RepoID && r.Status == code.RepoStatusActive
		},
		1: func(r code.EnsureResult) bool {
			return r.Err == nil && r.Changed && r.RepoID == edited.RepoID
		},
		2: func(r code.EnsureResult) bool {
			return r.Err == nil && r.Changed && r.RepoID != "" && r.Status == code.RepoStatusActive
		},
		3: func(r code.EnsureResult) bool {
			return r.Err == nil && !r.Changed && r.RepoID == gone.RepoID && r.Status == code.RepoStatusDeleted
		},
		4: func(r code.EnsureResult) bool { return errors.Is(r.Err, code.ErrInvalid) },
	} {
		if !check(got[name]) {
			t.Errorf("declaration %d answered %+v", name, got[name])
		}
	}
	if written := n.end() - before; written != 2 {
		t.Errorf("the reconcile wrote %d records, want 2: a create and a settings change", written)
	}
	if want := []string{string(code.KindCompany), string(code.KindName), string(code.KindName), string(code.KindRepo)}; !slices.Equal(counting.published(), want) {
		t.Errorf("the reconcile published %v, want %v — the batch read, a claim for the "+
			"new name, and a claim and a settle for the changed settings", counting.published(), want)
	}
	if row, _ := n.repo("edited"); row.description != "two" {
		t.Errorf("the edited repository's description is %q, want the declared %q", row.description, "two")
	}
}

// AN ACTIVATION THAT CHANGES NOTHING IS ONE PUBLISH, however many repositories
// it declares: the batch read finds every one as declared — every other one
// naming path owners, which the read takes beside its rules — and no
// repository's own subject is published to — where EnsureRepo spent two
// publishes a repository to find the same.
//
// Mutation: leave the path owners out of the batch read — every repository
// naming them reads as changed and is published to, red.
func TestAnUnchangedActivationIsOnePublish(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var declared []code.Declared
	for i := range 20 {
		name := fmt.Sprintf("repo-%02d", i)
		s := described(name)
		if i%2 == 0 {
			s.PathOwners = []code.PathOwner{
				{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Paths: []string{name + "/**"}},
				{Owner: code.Owner{Kind: code.OwnerUnit, Ref: "ENG"}, Paths: []string{"**/*.go", "docs/**"}},
			}
		}
		n.ensure(name, s, t1)
		declared = append(declared, code.Declared{Name: name, Settings: s})
	}
	a := n.activate(t2)
	n.drain()

	s, counting := countingStore(t, n)
	before := n.end()
	got, err := s.EnsureRepos(t.Context(), a, declared)
	if err != nil {
		t.Fatalf("EnsureRepos: %v", err)
	}
	for i, r := range got {
		if r.Err != nil || r.Changed || r.RepoID == "" {
			t.Fatalf("declaration %d answered %+v, want unchanged", i, r)
		}
	}
	if published := counting.published(); !slices.Equal(published, []string{string(code.KindCompany)}) {
		t.Fatalf("an unchanged activation of %d repositories published %v, want the one "+
			"batch read", len(declared), published)
	}
	if n.end() != before {
		t.Fatalf("an unchanged activation moved the log from %d to %d", before, n.end())
	}
}

// A RECORD THIS NODE CANNOT DECODE ANSWERS AS IT DOES THROUGH ENSUREREPO. The
// batch read's probe covers the whole domain, so a deferred record anywhere
// sends every declaration through EnsureRepo, whose own probe covers that
// repository's paths: one the record covers is refused — never answered
// unchanged from rows the record left stale — and one it does not is
// answered as before.
func TestADeferredRecordIsAnsweredAsEnsureRepoAnswersIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stale := n.ensure("stale", described("one"), t1)
	fine := n.ensure("fine", described("one"), t1)
	a := n.activate(t2)
	n.drain()
	n.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id, version,
		payload, stored_at) VALUES (999999, ?, 'repo', ?, 9, x'7b7d', 0)`,
		"crewlet.code.log.repo."+stale.RepoID, stale.RepoID)
	n.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (999999, ?)`,
		code.RepoPath(stale.RepoID))

	s, counting := countingStore(t, n)
	got, err := s.EnsureRepos(t.Context(), a, []code.Declared{
		{Name: "stale", Settings: described("one")},
		{Name: "fine", Settings: described("one")},
	})
	if err != nil {
		t.Fatalf("EnsureRepos: %v", err)
	}
	var refused *statelog.Unavailable
	if !errors.As(got[0].Err, &refused) || refused.Reason != statelog.ReasonDeferred {
		t.Errorf("the repository a deferred record covers answered %+v, want refused "+
			"deferred, as EnsureRepo refuses it", got[0])
	}
	if got[1].Err != nil || got[1].Changed || got[1].RepoID != fine.RepoID {
		t.Errorf("the repository no deferred record covers answered %+v, want unchanged", got[1])
	}
	if published := counting.published(); len(published) < 3 {
		t.Errorf("the reconcile published %v — a deferred record anywhere sends every "+
			"declaration through EnsureRepo", published)
	}
}

// A BATCH REFUSED FOR A REASON EVERY DECLARATION SHARES IS THE CALL'S ERROR
// AND EVERY DECLARATION'S ANSWER, from its one publish. A node whose applier
// is held below the activation's own fence refuses the batch `behind` on that
// fence's session floor — the floor every per-repository publish waits on too
// — so each of them would wait out the same budget for the same refusal: at
// the ten thousand repositories a configuration may declare, hours before the
// reconcile reported what the first publish already knew. The call says so
// once, and each declaration it left undecided carries that same error, so a
// caller logging the call's failure can pass over every one of them. Only a
// declaration that is not one is answered otherwise, by its own refusal.
func TestABatchRefusedBehindIsEveryDeclarationsAnswer(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("kept", described("one"), t1)
	n.ensure("edited", described("one"), t1)
	n.hold()
	a := n.activate(t2)

	s, counting := countingStore(t, n)
	before := n.end()
	got, err := s.EnsureRepos(t.Context(), a, []code.Declared{
		{Name: "kept", Settings: described("one")},
		{Name: "edited", Settings: described("two")},
		{Name: "fresh", Settings: described("new")},
		{Name: "Not A Name", Settings: described("one")},
	})
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonBehind {
		t.Fatalf("EnsureRepos on a node held below its activation's fence = %v, want the "+
			"batch's own refusal, behind, as the call's error", err)
	}
	for i, r := range got[:3] {
		if r.Err != err { //nolint:errorlint // the identical value is the assertion
			t.Errorf("declaration %d on a node held below its activation's fence answered "+
				"%+v, want the call's own error, %v", i, r, err)
		}
	}
	if !errors.Is(got[3].Err, code.ErrInvalid) {
		t.Errorf("a declaration that is not one answered %+v, want ErrInvalid", got[3])
	}
	if published := counting.published(); !slices.Equal(published, []string{string(code.KindCompany)}) {
		t.Errorf("a batch refused behind was followed by %v, want the one batch read — "+
			"every per-repository publish waits on the same floor", published)
	}
	if n.end() != before {
		t.Errorf("a node held below its fence moved the log from %d to %d", before, n.end())
	}
}

// A DELETED REPOSITORY A LATER ACTIVATION DECLARES AGAIN IS RESTORED, through
// EnsureRepo: the batch read answers a deletion only for the activation that
// made it or an older one.
func TestEnsureReposRestoresWhatALaterActivationDeclaresAgain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	gone := n.ensure("gone", described("one"), t1)
	if deleted, err := n.store.DeleteRepo(t.Context(), n.activate(t2), gone.RepoID); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	n.drain()
	got, err := n.store.EnsureRepos(t.Context(), n.activate(t3), []code.Declared{
		{Name: "gone", Settings: described("one")},
	})
	if err != nil || got[0].Err != nil || !got[0].Changed || got[0].Status != code.RepoStatusActive {
		t.Fatalf("a later activation declaring a deleted repository again = %+v, %v, want "+
			"it restored", got, err)
	}
	n.drain()
	if row, _ := n.repo("gone"); row.status != string(code.RepoStatusActive) {
		t.Fatalf("the restored repository's status is %q", row.status)
	}
}

// AN OLDER ACTIVATION'S RECONCILE IS SUPERSEDED, the call and every declaration
// in it — never answered unchanged from a snapshot a later fence stands in, and
// from the batch's one publish, since every per-repository decide reads that
// same fence first and answers the same — and an activation Activate never made
// fails the call.
func TestEnsureReposIsSupersededByALaterActivation(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stale := n.activate(t1)
	// STEADY IS WHAT t1 DECLARED AND t2 DID NOT CHANGE, so its row still
	// carries t1 — no activation re-stamps an unchanged repository — and only
	// the fence says a later activation owns it.
	if _, err := n.store.EnsureRepo(t.Context(), stale, code.Declared{
		Name: "steady", Settings: described("same"),
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	n.drain()
	n.ensure("app", described("new"), t2)
	s, counting := countingStore(t, n)
	before := n.end()
	got, err := s.EnsureRepos(t.Context(), stale, []code.Declared{
		{Name: "app", Settings: described("new")},
		{Name: "steady", Settings: described("same")},
		{Name: "fresh", Settings: described("new")},
	})
	if !errors.Is(err, code.ErrSuperseded) {
		t.Fatalf("EnsureRepos at a superseded activation = %v, want ErrSuperseded as the "+
			"call's error: every declaration shares it", err)
	}
	for i, r := range got {
		if !errors.Is(r.Err, code.ErrSuperseded) {
			t.Errorf("an older activation's reconcile of declaration %d answered %+v, want "+
				"ErrSuperseded — as EnsureRepo answers it, rows the later activation "+
				"left as they were included", i, r)
		}
	}
	if n.end() != before {
		t.Fatalf("the older activation wrote %d record(s)", n.end()-before)
	}
	if published := counting.published(); !slices.Equal(published, []string{string(code.KindCompany)}) {
		t.Errorf("a superseded reconcile published %v, want the one batch read", published)
	}
	if _, err := n.store.EnsureRepos(t.Context(), code.Activation{}, nil); !errors.Is(err, code.ErrNoActivation) {
		t.Fatalf("an activation Activate never made = %v, want ErrNoActivation", err)
	}
}
