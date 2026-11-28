package estate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// budgetMeter records what a data node's answer budget holds, and is closed
// freed once a hold it saw is given back whole.
type budgetMeter struct {
	mu    sync.Mutex
	held  int64
	freed chan struct{}
	once  sync.Once
}

func (m *budgetMeter) AnswerWaited(queue.AnswerKind, queue.AnswerOutcome, time.Duration) {}

func (m *budgetMeter) AnswerHeld(bytes int64) {
	m.mu.Lock()
	was := m.held
	m.held = bytes
	m.mu.Unlock()
	if was > 0 && bytes == 0 {
		m.once.Do(func() { close(m.freed) })
	}
}

func (m *budgetMeter) bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held
}

// weighedReads is a data node's access reader recording what the node's answer
// budget held as each read ran.
type weighedReads struct {
	AccessReader
	meter *budgetMeter
	mu    *sync.Mutex
	saw   map[string]int64
}

func (w weighedReads) record(read string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.saw[read] = w.meter.bytes()
}

func (w weighedReads) Commands(context.Context, accessdomain.AuditQuery,
	statelog.Freshness) (accessdomain.CommandPage, error) {
	w.record("commands")
	return accessdomain.CommandPage{}, nil
}

func (w weighedReads) SealedKeys(context.Context) (accessdomain.SealedCount, error) {
	w.record("sealed_keys")
	return accessdomain.SealedCount{}, nil
}

// weighedCopy serves the access reader from every copy.
type weighedCopy struct{ reads weighedReads }

func (c weighedCopy) For(context.Context, string) (Backend, bool) {
	return Backend{
		Access:  AccessHalf{Reader: c.reads},
		Reached: func(context.Context, statelog.Position) error { return nil },
		Answers: func(context.Context) bool { return true },
	}, true
}

// A PAGE HOLDS ITS WEIGHT OF THE NODE'S ANSWER BUDGET UNTIL ITS REPLY IS
// WRITTEN, AND ONE VALUE HOLDS NONE: a routed rows read runs while the data
// node holds five times the bytes it declared, given back once its reply is
// written; a read answering one value runs holding nothing, the request and
// its record cap its bound.
//
// Mutations, each red: hold nothing for a page; hold for one value; drop the
// multiple. (Dropping the handler's release changes nothing: in an answer the
// reply's write gives the hold back, queuetest's case.)
func TestAPageHoldsItsWeightUntilItsReplyAndOneValueNone(t *testing.T) {
	t.Parallel()
	meter := &budgetMeter{freed: make(chan struct{})}
	reads := weighedReads{meter: meter, mu: &sync.Mutex{}, saw: map[string]int64{}}
	f := &fleet{broker: memory.NewBroker()}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{Taken: signedasktest.NewLedger(), Self: "data-a", Local: weighedCopy{reads: reads},
		Envelopes: fleetEnvelopes, Answers: queue.NewAnswerBudget(meter)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: f.start(t),
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Git: &fakePlacement{},
		Session: NewSession(), Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AccessReads().SealedKeys(t.Context()); err != nil {
		t.Fatalf("sealed keys: %v", err)
	}
	if _, err := r.AccessReads().Commands(t.Context(), accessdomain.AuditQuery{},
		statelog.Freshness{Level: statelog.ReadStale}); err != nil {
		t.Fatalf("commands: %v", err)
	}
	reads.mu.Lock()
	saw := map[string]int64{"commands": reads.saw["commands"], "sealed_keys": reads.saw["sealed_keys"]}
	reads.mu.Unlock()
	if want := int64(answerEncodeMultiple * statelog.AnswerBytes); saw["commands"] != want {
		t.Errorf("a page of commands read while the budget held %d bytes, want five times its %d: %d",
			saw["commands"], statelog.AnswerBytes, want)
	}
	if saw["sealed_keys"] != 0 {
		t.Errorf("one value read while the budget held %d bytes, want none", saw["sealed_keys"])
	}
	wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-meter.freed:
	case <-wait.Done():
		t.Fatal("the page's reply was written and the budget never gave its hold back")
	}
}

// A READ WEIGHS ITS DECLARED SHAPE: a page five times the bytes it is read or
// cut to, a capped list five times its count by its row bound, one value and
// an entry per key asked nothing.
//
// Mutations, each red: weigh a capped list by its row bound alone; weigh one
// value; drop the multiple.
func TestAReadWeighsItsDeclaredShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		shape answerShape
		want  int64
	}{
		{answerShape{kind: answerRows, bytes: 1000}, 5000},
		{answerShape{kind: answerOwnBound, bytes: 1000}, 5000},
		{answerShape{kind: answerCapped, rows: 10, bytes: 100}, 5000},
		{answerShape{kind: answerOne}, 0},
		{answerShape{kind: answerPerAsked}, 0},
	} {
		if got := tc.shape.weight(); got != tc.want {
			t.Errorf("a %s answer of %+v weighs %d, want %d", tc.shape.kind, tc.shape, got, tc.want)
		}
	}
}
