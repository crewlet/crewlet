package compact_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// heard is a node's event stream as an expunge's commit announces itself on
// it: every code_expunged kept, with the subject it was published on.
type heard struct {
	mu       sync.Mutex
	expunged []types.CodeExpunged
	subjects []string
}

func (h *heard) Publish(_ context.Context, subject string, ev *events.Event) error {
	if p, ok := ev.Data.(*types.CodeExpunged); ok {
		h.mu.Lock()
		h.expunged = append(h.expunged, *p)
		h.subjects = append(h.subjects, subject)
		h.mu.Unlock()
	}
	return nil
}

// all is every expunge's account so far, and the subjects each went out on.
func (h *heard) all() ([]types.CodeExpunged, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.expunged), slices.Clone(h.subjects)
}

// storeHold is how long after its commit the store may keep what an expunge
// dropped on a healthy fleet: the retired packs' grace, then the retention
// duty's sweep and the collector's pass, each at the most its interval away.
const storeHold = code.RetireGrace + code.RetentionInterval + collect.CollectInterval

// EVERY COMMITTED EXPUNGE IS ANNOUNCED ONCE: a person's purge served and a
// cycle the maintainer's weekly schedule began each end in exactly one
// code_expunged on the node's event stream, published on the type's own
// subject once the commit has applied — naming the repository, the purge it
// served (none for the schedule), the expunge, what it dropped, the commit's
// position and until when the store still holds the dropped bytes — so the
// event log keeps every expunge where the repository's detail keeps only its
// latest.
//
// Mutation: drop the announcement from the commit's applied path, and both
// cases hear none; announce from the begin as well, and both hear two.
func TestEveryCommittedExpungeIsAnnouncedOnce(t *testing.T) {
	t.Parallel()

	t.Run("a person's purge", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.push(4 << 10)
		x.leak("feature")
		x.push(4 << 10)
		x.purge()
		request := x.mustOwe().Request
		h := &heard{}
		// The fixture's clock: the wall's, a publication debounce ahead.
		lo := time.Now().Add(compact.PublishDebounce)
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Events = h })
		x.served(c)
		hi := time.Now().Add(compact.PublishDebounce)
		x.heardOne(h, request, lo, hi)
	})

	t.Run("a weekly cycle", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.push(96 << 10)
		x.leakBytes("big", 64<<10)
		h := &heard{}
		// past's clock, a grace and an hour ahead of the wall's.
		ahead := code.PruneGrace + time.Hour
		c, _, _ := x.compactorOf(func(o *compact.Options) {
			o.Now = func() time.Time { return time.Now().Add(ahead) }
			o.Events = h
		})
		compact.ExpungeFrom(c, 1)
		cand, ok := x.weekly(codetest.RepoName, time.Now().Add(ahead-code.ExpungeInterval))
		if !ok {
			t.Fatalf("the weekly arm names no %s, never expunged", codetest.RepoName)
		}
		lo := time.Now().Add(ahead)
		if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeApplied {
			t.Fatalf("a weekly cycle of a repository past a quarter unreachable answered %s, %v; want applied",
				out, err)
		}
		hi := time.Now().Add(ahead)
		x.heardOne(h, "", lo, hi)
	})
}

// heardOne fails the case unless h heard exactly one code_expunged, on its
// type's subject, for codetest.RepoName serving request ("" for the weekly
// schedule), dropping something, at a position, and holding the store to
// storeHold after a commit the compactor's clock read between lo and hi.
func (x *fixture) heardOne(h *heard, request string, lo, hi time.Time) {
	x.t.Helper()
	got, subjects := h.all()
	if len(got) != 1 {
		x.t.Fatalf("the expunge was announced %d times, want once: %+v", len(got), got)
	}
	ev := got[0]
	if want := topics.Event("code_expunged"); subjects[0] != want {
		x.t.Errorf("code_expunged went out on %q, want %q", subjects[0], want)
	}
	repo := x.rows().Repo
	if ev.RepoID != repo.ID || ev.RepoName != codetest.RepoName {
		x.t.Errorf("the event names repository %q (%s), want %q (%s)", ev.RepoName, ev.RepoID,
			codetest.RepoName, repo.ID)
	}
	if ev.Request != request {
		x.t.Errorf("the event names request %q, want %q", ev.Request, request)
	}
	if ev.Expunge == "" || ev.Position == "" {
		x.t.Errorf("the event names expunge %q at position %q, want both", ev.Expunge, ev.Position)
	}
	if ev.DroppedObjects <= 0 || ev.DroppedBytes <= 0 {
		x.t.Errorf("the event says the commit dropped %d objects, %d bytes; want what the leak left",
			ev.DroppedObjects, ev.DroppedBytes)
	}
	if ev.StoreUntil.Before(lo.Add(storeHold)) || ev.StoreUntil.After(hi.Add(storeHold)) {
		x.t.Errorf("the event holds the store until %s, want %s after the commit, between %s and %s",
			ev.StoreUntil, storeHold, lo.Add(storeHold), hi.Add(storeHold))
	}
}
