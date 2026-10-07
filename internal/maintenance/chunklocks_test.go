package maintenance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/maintenance"
)

// chunkLocks is a coordination store holding the chunk locks' bucket until it
// is retired.
type chunkLocks struct {
	present bool
	retires int
}

func (c *chunkLocks) RetireChunkLocks(context.Context) (bool, error) {
	c.retires++
	was := c.present
	c.present = false
	return was, nil
}

// era is the chunk era as a census answered it.
type era struct {
	over bool
	err  error
}

func (e era) Over(context.Context) (bool, error) { return e.over, e.err }

// THE CHUNK LOCKS OUTLIVE EVERY NODE THAT TAKES THEM.
//
// A build that kept files in chunks fails a write of a chunk the store holds
// when its lock cannot be taken, so while the chunk era is open the job has no
// work; once it is over, the first tick deletes the bucket and every later
// tick finds nothing.
func TestTheChunkLocksAreRetiredOnlyOnceTheChunkEraIsOver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		era     era
		retired bool
	}{
		{"a node still writes chunks", era{}, false},
		{"no node writes chunks", era{over: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bucket := &chunkLocks{present: true}
			w := newWorker(t, maintenance.Options{
				Now: fixed(base), Jobs: maintenance.RetiredChunkLockJobs(bucket, tc.era),
			})
			swept, err := w.Tick(t.Context())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if got := !bucket.present; got != tc.retired {
				t.Fatalf("retired = %v, want %v", got, tc.retired)
			}
			if tc.retired && swept["retired_chunk_locks"] != 1 {
				t.Fatalf("swept = %v, want the retirement reported once", swept)
			}
			if !tc.retired && bucket.retires != 0 {
				t.Fatalf("the retirement was attempted %d time(s) while a node "+
					"still writes chunks", bucket.retires)
			}
			swept, err = w.Tick(t.Context())
			if err != nil || swept["retired_chunk_locks"] != 0 {
				t.Fatalf("the second tick = (%v, %v), want nothing retired", swept, err)
			}
		})
	}
}

// AN ERA THAT CANNOT BE READ IS NOT "NOT YET": the job does nothing — a
// deletion cannot be taken back — and the tick says it could not tell.
func TestAnUnreadableChunkEraRetiresNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	bucket := &chunkLocks{present: true}
	w := newWorker(t, maintenance.Options{
		Now:  fixed(base),
		Jobs: maintenance.RetiredChunkLockJobs(bucket, era{err: errors.New("the census is unreadable")}),
	})
	if _, err := w.Tick(t.Context()); err == nil {
		t.Fatal("an era that could not be read was reported as no work")
	}
	if !bucket.present || bucket.retires != 0 {
		t.Fatal("the chunk locks were retired on an era nobody could read")
	}
}

// NOTHING TO RETIRE FROM, OR NOTHING TO GATE ON, IS NO JOB.
func TestNoStoreOrNoEraContributesNoChunkLockRetirement(t *testing.T) {
	t.Parallel()
	if jobs := maintenance.RetiredChunkLockJobs(nil, era{}); jobs != nil {
		t.Fatalf("jobs = %v with no store", jobs)
	}
	if jobs := maintenance.RetiredChunkLockJobs(&chunkLocks{}, nil); jobs != nil {
		t.Fatalf("jobs = %v with no era", jobs)
	}
}
