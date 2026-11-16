package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SOLO EMBEDDED NODE RAISES ITS BUCKET AT BOOT: a larger figure is applied,
// one past what the logs and the coordination floor leave is kept and raised as
// the alarm, and a smaller one never lowers it.
func TestASoloNodeRaisesItsBucketAtBoot(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)

	boot := func(figure int64) (int64, statelog.Reading) {
		t.Helper()
		b.Store.Objects.NatsMaxBytes = figure
		back, err := OpenBackends(t.Context(), &b, nil, nil)
		if err != nil {
			t.Fatalf("OpenBackends at %d: %v", figure, err)
		}
		e, err := New(t.Context(), Options{Bootstrap: &b, Backends: back})
		if err != nil {
			back.Close(context.WithoutCancel(t.Context()))
			t.Fatalf("boot at %d: %v", figure, err)
		}
		defer func() {
			e.Stop(context.WithoutCancel(t.Context()))
			back.Close(context.WithoutCancel(t.Context()))
		}()
		usage, err := back.Objects.(*natsobj.Backend).Usage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var r statelog.Reading
		e.objectsReading(time.Now(), &r)
		return usage.MaxBytes, r
	}

	if held, _ := boot(gib); held != gib {
		t.Fatalf("the first boot created the bucket at %d, want %d", held, gib)
	}
	held, r := boot(2 * gib)
	if held != 2*gib || r.ObjectsBucketWanted != 0 {
		t.Fatalf("a boot at 2 GiB left the bucket at %d (refused: %q), want it raised", held, r.ObjectsBucketWhy)
	}
	// INSIDE WHAT TIER A ACCEPTS, and past what the logs leave beside the floor.
	held, r = boot(12 * gib)
	if held != 2*gib {
		t.Fatalf("a raise past the broker's limit left the bucket at %d, want the %d kept", held, 2*gib)
	}
	alarm, fired := findAlarm(statelog.Evaluate(r), statelog.KindObjectsBucketRaiseRefused)
	if !fired {
		t.Fatalf("a refused raise raised no alarm; the reading was %+v", r)
	}
	for _, want := range []string{"12.0 GiB", "2.0 GiB", "coordination records keep"} {
		if !strings.Contains(alarm.Detail, want) {
			t.Errorf("the alarm says %q, which does not say %q", alarm.Detail, want)
		}
	}
	if held, r = boot(gib); held != 2*gib || r.ObjectsBucketWanted != 0 {
		t.Fatalf("a boot at a smaller figure left the bucket at %d (refused: %q), want the %d kept "+
			"and nothing raised", held, r.ObjectsBucketWhy, 2*gib)
	}
}

// A RAISE LEAVES THE COORDINATION FLOOR: it fits only where the broker's room
// less the floor holds the difference, and a refusal names the most that fits.
func TestARaiseLeavesTheCoordinationFloor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		broker any
		want   int64
		says   string
	}{
		{"a raise that leaves the floor", budgetOf{Limit: 10 * gib, Committed: 6 * gib}, 4 * gib, ""},
		{"a raise into the floor", budgetOf{Limit: 10 * gib, Committed: 6 * gib}, 4*gib + 1,
			"the most it can be raised to is 4 GiB"},
		{"a broker with nothing past the floor", budgetOf{Limit: 10 * gib, Committed: 9 * gib}, 2 * gib,
			"no raise fits"},
		{"a broker that states no limit", budgetOf{Limit: -1, Source: jetstream.BudgetUnstated}, 2 * gib,
			"states no limit"},
		{"a broker whose room cannot be read", unreadBudget{}, 2 * gib, "could not be read"},
		{"a queue with no room to read", struct{}{}, 2 * gib, "no room it can read"},
	} {
		why := objectsRoom(t.Context(), c.broker, gib, c.want)
		switch {
		case c.says == "" && why != "":
			t.Errorf("%s: refused (%s), want it to fit", c.name, why)
		case c.says != "" && !strings.Contains(why, c.says):
			t.Errorf("%s: answered %q, want it to say %q", c.name, why, c.says)
		}
	}
}

// A REFUSED RAISE TOUCHES NOTHING, a smaller figure never lowers the bucket,
// and a node without `data` never raises it.
func TestOnlyADataNodeRaisesAndOnlyWhereItFits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		roles   []string
		held    int64
		figure  int64
		room    budgetOf
		raised  bool
		refused bool
	}{
		{"a data node with room", nil, gib, 2 * gib, budgetOf{Limit: 10 * gib, Committed: 2 * gib}, true, false},
		{"a data node without room", nil, gib, 9 * gib, budgetOf{Limit: 10 * gib, Committed: 2 * gib}, false, true},
		{"a smaller figure", nil, 3 * gib, 2 * gib, budgetOf{Limit: 10 * gib}, false, false},
		{"a node without data", []string{"seats"}, gib, 2 * gib, budgetOf{Limit: 10 * gib}, false, false},
	} {
		b := testBootstrap(t)
		b.Node.Roles = c.roles
		b.Store.Objects.NatsMaxBytes = c.figure
		bucket := &fleetBucket{held: c.held}
		out := &Backends{Objects: bucket, Queue: roomyQueue{budgetOf: c.room}, Coord: coordmemory.New()}
		refused := raiseObjects(t.Context(), &b, out, "node-a")
		if got := len(bucket.history) > 0; got != c.raised {
			t.Errorf("%s: raised %v (history %v), want %v", c.name, got, bucket.history, c.raised)
		}
		if (refused != nil) != c.refused {
			t.Errorf("%s: refused %+v, want refused %v", c.name, refused, c.refused)
		}
		if refused != nil && (refused.Wanted != c.figure || refused.Kept != c.held) {
			t.Errorf("%s: the refusal says %+v, want the %d figure and the %d kept", c.name, refused,
				c.figure, c.held)
		}
	}
}

// DATA NODES WITH DIFFERENT FIGURES END AT THE LARGEST, IN EITHER ORDER, and
// never overlap: a node finding another's raise in flight waits on its lease and
// reads the bucket again. Without the lease the two raises overlap.
func TestDataNodesWithDifferentFiguresEndAtTheLargest(t *testing.T) {
	t.Parallel()
	node := func(figure int64) *config.Bootstrap {
		b := testBootstrap(t)
		b.Store.Objects.NatsMaxBytes = figure
		return &b
	}
	room := roomyQueue{budgetOf: budgetOf{Limit: 100 * gib, Committed: gib}}

	t.Run("one after the other", func(t *testing.T) {
		t.Parallel()
		for _, order := range [][2]int64{{2 * gib, 3 * gib}, {3 * gib, 2 * gib}} {
			bucket, leases := &fleetBucket{held: gib}, coordmemory.New()
			for i, figure := range order {
				out := &Backends{Objects: bucket, Queue: room, Coord: leases}
				raiseObjects(t.Context(), node(figure), out, []string{"node-a", "node-b"}[i])
			}
			if bucket.held != 3*gib || !bucket.neverLowered() {
				t.Errorf("figures %v in turn left %d (history %v), want the larger, never lowered",
					order, bucket.held, bucket.history)
			}
		}
	})
	t.Run("at once", func(t *testing.T) {
		t.Parallel()
		leases := &refusalsSeen{Backend: coordmemory.New(), refused: make(chan struct{})}
		bucket := &fleetBucket{held: gib, entered: make(chan struct{}), gate: leases.refused}
		first := make(chan *objectsRaise, 1)
		go func() {
			first <- raiseObjects(t.Context(), node(3*gib), &Backends{Objects: bucket, Queue: room, Coord: leases}, "node-a")
		}()
		<-bucket.entered
		second := raiseObjects(t.Context(), node(2*gib), &Backends{Objects: bucket, Queue: room, Coord: leases}, "node-b")
		if r := <-first; r != nil || second != nil {
			t.Fatalf("the raises were refused: %+v, %+v", r, second)
		}
		if bucket.overlapped || bucket.held != 3*gib || !bucket.neverLowered() {
			t.Fatalf("two nodes raising at once left %d (history %v, overlapped %v), want the larger, "+
				"one raise at a time", bucket.held, bucket.history, bucket.overlapped)
		}
	})
}

// fleetBucket is a bucket several nodes raise, recording every reservation
// written. Where entered is set, the first raise waits inside, between its read
// and its write, until gate closes or a second raise comes in beside it.
type fleetBucket struct {
	objstore.Backend

	mu         sync.Mutex
	held       int64
	history    []int64
	inside     int
	overlapped bool
	entered    chan struct{}
	gate       <-chan struct{}
	beside     chan struct{}
}

func (b *fleetBucket) Usage(context.Context) (objstore.Usage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return objstore.Usage{MaxBytes: b.held}, nil
}

func (b *fleetBucket) Raise(_ context.Context, maxBytes int64) (int64, error) {
	b.mu.Lock()
	read := b.held
	b.inside++
	wait := b.entered != nil && b.inside == 1 && len(b.history) == 0
	if wait {
		b.beside = make(chan struct{})
	}
	if b.inside > 1 {
		b.overlapped = true
		close(b.beside)
	}
	beside := b.beside
	b.mu.Unlock()
	if wait {
		close(b.entered)
		select {
		case <-b.gate:
		case <-beside:
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inside--
	if read >= maxBytes {
		return read, nil
	}
	b.held = maxBytes
	b.history = append(b.history, maxBytes)
	return maxBytes, nil
}

func (b *fleetBucket) neverLowered() bool {
	for i := 1; i < len(b.history); i++ {
		if b.history[i] < b.history[i-1] {
			return false
		}
	}
	return true
}

// refusalsSeen closes refused the first time a lease is refused as held.
type refusalsSeen struct {
	coord.Backend
	once    sync.Once
	refused chan struct{}
}

func (r *refusalsSeen) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (
	*coord.Lease, coord.Refusal, error) {
	lease, refusal, err := r.Backend.TryAcquire(ctx, resource, opts)
	if lease == nil && err == nil {
		r.once.Do(func() { close(r.refused) })
	}
	return lease, refusal, err
}

// roomyQueue is a queue whose only answer is its broker's room.
type roomyQueue struct {
	queue.EventQueue
	budgetOf
}

// findAlarm is the alarm of kind among alarms.
func findAlarm(alarms []statelog.Alarm, kind statelog.Kind) (statelog.Alarm, bool) {
	for _, a := range alarms {
		if a.Kind == kind {
			return a, true
		}
	}
	return statelog.Alarm{}, false
}
