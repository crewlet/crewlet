package cache_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE NODE'S FULL READS, BOUNDED (MaxFullRounds): every page of a full read is
// served out of one of the answers a data node gives the whole fleet's reads
// at once, so a node joining with hundreds of repositories asked of it reads
// at most MaxFullRounds of them whole at once, and the rest wait their turn.

// emptyRepo makes the n'th repository of a series no other helper's ids meet:
// in the log, with no refs and no packs — a repository whose first round on a
// node is a full read and nothing else, so a case holds that read alone.
func (w *syncWorld) emptyRepo(n int) string {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("empty-repo-"+strconv.Itoa(n))).String()
	w.log.create(id, "empty-"+strconv.Itoa(n))
	return id
}

// heldReads is repositories a node has never read, each asked of it at once,
// every call their reads make of the log held there until release: what
// holds every one of the node's full-read slots, and lines up the rest.
type heldReads struct {
	repos   []string
	errs    chan error
	release func()

	mu          sync.Mutex
	inLog, most int
}

// holdReads makes n empty repositories of w — the series from first — has c
// sync each linearizably at once, and holds every call they make of the log
// until the answer's release, which also runs when the case ends, before the
// cache's Stop waits for the rounds.
func holdReads(t *testing.T, w *syncWorld, c *cache.Cache, first, n int) *heldReads {
	t.Helper()
	h := &heldReads{errs: make(chan error, n)}
	held := map[string]bool{}
	for i := range n {
		repo := w.emptyRepo(first + i)
		h.repos = append(h.repos, repo)
		held[repo] = true
	}
	gate := make(chan struct{})
	h.release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(h.release)
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if !held[call.Repo] {
				return nil
			}
			h.mu.Lock()
			h.inLog++
			h.most = max(h.most, h.inLog)
			h.mu.Unlock()
			<-gate
			h.mu.Lock()
			h.inLog--
			h.mu.Unlock()
			return nil
		}
	})
	for _, repo := range h.repos {
		go func() {
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err == nil {
				if s.Kind != cache.SyncFull {
					err = fmt.Errorf("the first sync of %s was a %s round, want a full one", repo, s.Kind)
				}
				s.Release()
			}
			h.errs <- err
		}()
	}
	return h
}

// inFlight is how many of the held reads' calls are in the log now, and the
// most that have been at once.
func (h *heldReads) inFlight() (now, most int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inLog, h.most
}

// answered waits for every held read's sync to be answered, and fails the
// case on any that was not answered with its repository.
func (h *heldReads) answered(t *testing.T) {
	t.Helper()
	for range h.repos {
		select {
		case err := <-h.errs:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(hangDetector):
			t.Fatalf("a held read's sync was not answered within %v of its release", hangDetector)
		}
	}
}

// A COLD NODE RUNS AT MOST ITS FULL ROUNDS AT ONCE: asked for three times as
// many repositories as it reads whole at once, all at the same moment — a node
// joining with hundreds asked of it — the log sees exactly MaxFullRounds full
// reads, the rest wait in line for a slot, the gauge says so, and every one is
// answered once the reads ahead of it end. A caller whose own budget ends
// while its round is in line is answered by that end at once, and its round
// goes on: the repository comes up for whoever asks next, never read twice.
//
// Mutation: hold no slot in readOnce, and every repository's read reaches the
// log at once.
func TestAColdNodeRunsAtMostItsFullRoundsAtOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	const asked = 3 * cache.MaxFullRounds
	h := holdReads(t, w, c, 1, asked)
	eventually(t, "every sync in the log or in line for a slot", func() bool {
		now, _ := h.inFlight()
		_, _, waiting := cache.FullRounds(c)
		return int64(now)+waiting == asked
	})
	now, _ := h.inFlight()
	running, _, waiting := cache.FullRounds(c)
	if now != cache.MaxFullRounds || running != cache.MaxFullRounds || waiting != asked-cache.MaxFullRounds {
		t.Fatalf("asked for %d repositories at once, the log saw %d full reads (%d slots held, %d in line), "+
			"want %d reading and %d in line", asked, now, running, waiting, cache.MaxFullRounds,
			asked-cache.MaxFullRounds)
	}
	for state, want := range map[string]float64{"running": cache.MaxFullRounds, "waiting": asked - cache.MaxFullRounds} {
		if got := gaugeAt(t, w.rec, metrics.CodeCacheFullRounds, "state", state); got != want {
			t.Errorf("crewlet.code.cache.full_rounds{state=%s} = %v, want %v", state, got, want)
		}
	}

	// THE WAIT IS THE CALLER'S: one more repository's caller, whose budget
	// ends while its round is in line.
	late := w.emptyRepo(asked + 1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, err := c.Sync(ctx, late, linearizable)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a sync whose budget ended in line: %v, want its own deadline", err)
	}
	eventually(t, "the late caller's round in line, with its caller gone", func() bool {
		_, _, waiting := cache.FullRounds(c)
		return waiting == asked-cache.MaxFullRounds+1
	})

	h.release()
	h.answered(t)
	eventually(t, "the late caller's round converged with nobody waiting for it", func() bool {
		return cache.Converged(c, late)
	})
	s, err := c.Sync(t.Context(), late, stale)
	if err != nil {
		t.Fatal(err)
	}
	s.Release()
	if s.Kind != cache.SyncNone {
		t.Errorf("a stale sync after the round converged was a %s round, want answered at once", s.Kind)
	}
	if calls := w.log.callsOf(late); len(calls) != 1 {
		t.Errorf("the late repository's rows were read %d times, want once, by the round its caller left", len(calls))
	}

	_, most := h.inFlight()
	running, mostHeld, waiting := cache.FullRounds(c)
	if most != cache.MaxFullRounds || mostHeld != cache.MaxFullRounds {
		t.Errorf("at most %d full reads were in the log at once (%d slots held), want the bound reached and "+
			"never passed: %d", most, mostHeld, cache.MaxFullRounds)
	}
	if running != 0 || waiting != 0 {
		t.Errorf("%d slots held and %d reads in line once every read ended, want none", running, waiting)
	}
	for _, state := range []string{"running", "waiting"} {
		if got := gaugeAt(t, w.rec, metrics.CodeCacheFullRounds, "state", state); got != 0 {
			t.Errorf("crewlet.code.cache.full_rounds{state=%s} = %v once every read ended, want 0", state, got)
		}
	}
}

// A DELTA NEVER WAITS FOR A FULL ROUND'S SLOT: with every slot held by cold
// repositories' full reads and another in line behind them, the sync after a
// push to a repository the node has read whole reads its delta and is
// answered — the round every git node runs after every push is never put
// behind a joining node's full reads.
//
// Mutation: hold a slot for every read, a delta's too, and the delta waits in
// line behind the full reads.
func TestADeltaNeverWaitsForAFullRound(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	warm := w.repo(t, 1)
	w.push(t, warm, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, warm)
	h := holdReads(t, w, c, 1, cache.MaxFullRounds+1)
	eventually(t, "every slot held and one read in line", func() bool {
		running, _, waiting := cache.FullRounds(c)
		return running == cache.MaxFullRounds && waiting == 1
	})

	w.push(t, warm, "refs/heads/main", nil)
	type answer struct {
		s   cache.Synced
		err error
	}
	done := make(chan answer, 1)
	go func() {
		s, err := c.Sync(t.Context(), warm, linearizable)
		done <- answer{s, err}
	}()
	var got answer
	eventually(t, "the delta answered, or in line for a slot", func() bool {
		select {
		case got = <-done:
			return true
		default:
		}
		_, _, waiting := cache.FullRounds(c)
		return waiting > 1
	})
	if got.s.Dir == "" && got.err == nil {
		t.Fatal("the sync after a push waits in line for a full round's slot")
	}
	if got.err != nil {
		t.Fatalf("the sync after a push while every slot is held: %v", got.err)
	}
	got.s.Release()
	if got.s.Kind != cache.SyncDelta {
		t.Errorf("the sync after a push was a %s round, want a delta", got.s.Kind)
	}
	h.release()
	h.answered(t)
}

// A DELTA THE LOG ANSWERS IN FULL IS A FULL READ FROM ITS SECOND PAGE: page 1
// decides, and was asked as a delta's, so it waits for nothing — but every
// page after it is a whole repository's, and waits for a slot as a full
// round's does.
//
// Mutation: hold no slot once page 1 answers in full, and the read's next
// page reaches the log while every slot is held.
func TestADeltaTheLogAnswersInFullHoldsASlotFromItsSecondPage(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.log.set(func(f *fakeLog) { f.pageRows = 1 })
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	// A ref made and deleted since the copy's position, its tombstone swept:
	// no delta from there can carry the deletion, so the log answers in full.
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": tip})
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
	w.log.sweep(repo)
	h := holdReads(t, w, c, 1, cache.MaxFullRounds)
	eventually(t, "every slot held", func() bool {
		running, _, _ := cache.FullRounds(c)
		return running == cache.MaxFullRounds
	})

	before := len(w.log.callsOf(repo))
	type answer struct {
		s   cache.Synced
		err error
	}
	done := make(chan answer, 1)
	go func() {
		s, err := c.Sync(t.Context(), repo, linearizable)
		done <- answer{s, err}
	}()
	var got answer
	eventually(t, "the read answered, or in line for a slot", func() bool {
		select {
		case got = <-done:
			return true
		default:
		}
		_, _, waiting := cache.FullRounds(c)
		return waiting == 1
	})
	if got.s.Dir != "" || got.err != nil {
		t.Fatalf("a delta the log answered in full read every page while every slot was held (%v)", got.err)
	}
	calls := w.log.callsOf(repo)[before:]
	if len(calls) != 1 || calls[0].Page != 1 || calls[0].Since.IsZero() {
		t.Fatalf("the log saw %+v of the read before it waited, want its delta's page 1 alone", calls)
	}

	h.release()
	h.answered(t)
	select {
	case got = <-done:
	case <-time.After(hangDetector):
		t.Fatalf("the read was not answered within %v of the slots' release", hangDetector)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	got.s.Release()
	if got.s.Kind != cache.SyncFull {
		t.Errorf("a delta the log answered in full was a %s round, want a full one", got.s.Kind)
	}
}

// A NODE STOPPING ENDS EVERY WAIT FOR A SLOT: a round in line for one is the
// cache's own work, which Stop ends, its callers told the cache stopped
// rather than left waiting for a read nobody will make.
func TestAStoppingNodeEndsEveryWaitForAFullRound(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	cache.SetMaxFullRounds(c, 1)
	h := holdReads(t, w, c, 1, 1)
	eventually(t, "the node's one slot held", func() bool {
		running, _, _ := cache.FullRounds(c)
		return running == 1
	})
	errc := make(chan error, 1)
	go func() {
		_, err := c.Sync(t.Context(), w.emptyRepo(2), linearizable)
		errc <- err
	}()
	eventually(t, "a read in line", func() bool {
		_, _, waiting := cache.FullRounds(c)
		return waiting == 1
	})

	stopped := make(chan struct{})
	go func() {
		c.Stop(context.Background())
		close(stopped)
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, cache.ErrStopped) {
			t.Errorf("a sync whose round was in line when the node stopped: %v, want ErrStopped", err)
		}
	case <-time.After(hangDetector):
		t.Fatalf("a round in line for a slot outlived the node's stop by %v", hangDetector)
	}
	if _, _, waiting := cache.FullRounds(c); waiting != 0 {
		t.Errorf("%d reads in line after the stop, want none", waiting)
	}
	h.release()
	select {
	case <-stopped:
	case <-time.After(hangDetector):
		t.Fatalf("Stop did not return within %v of the held read's end", hangDetector)
	}
}
