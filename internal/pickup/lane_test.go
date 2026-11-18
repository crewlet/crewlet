package pickup_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A BUSY SEAT'S NEXT TURN IS ITS URGENT ITEM (ADR-0159): handed while a turn
// runs on the first of thirty queued assignment wakes, the pickup is the very
// next turn. Mutation: skip the lane between partitions, and it runs last.
func TestAnUrgentPickupIsTheBusySeatsNextTurn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	topic, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	// HELD while the backlog queues, so one drain takes all of it.
	const hold = "backlog"
	if err := q.PauseTopic(ctx, topic, group, hold); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	const backlog = 30
	for i := range backlog {
		wake := events.New(types.TaskAssigned{Agent: "engineer", RoleName: "Engineer",
			Description: fmt.Sprintf("WRK-%d", i)}, events.TraceContext{})
		if err := q.Publish(ctx, topic, wake); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	var mu sync.Mutex
	var turns [][]*events.Event
	started, release, ran := make(chan struct{}), make(chan struct{}), make(chan struct{}, backlog+1)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	err := q.SubscribeBatch(ctx, topic, group, func(_ context.Context, evs []*events.Event) queue.Result {
		mu.Lock()
		turns = append(turns, evs)
		first := len(turns) == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		ran <- struct{}{}
		return queue.Ack()
	}, nil, queue.NewBatchOptions(0, backlog))
	if err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}
	// Off the test's goroutine: the twin runs the drain on whoever resumes it.
	go func() { _ = q.ResumeTopic(context.WithoutCancel(ctx), topic, group, hold) }()
	<-started

	f := newFleet()
	fire := urgent("fire", worklet.StatusInProgress, 40)
	fire.Why = types.PickupInProgress
	h := idleHolder()
	h.c.Dispatching, h.c.MailWaiting, h.c.IdleBefore = true, true, false
	p := &pickup.Picker{Holder: h, Ready: &tracker{items: []worklet.ReadyItem{fire}},
		Ledger: f, Claims: f, Publisher: q, Now: f.clock, Cooldown: 10 * time.Minute}
	out := pass(p)
	if !out.Woken {
		t.Fatalf("a busy seat holding urgent work in progress = %+v, want it handed", out)
	}

	releaseOnce.Do(func() { close(release) })
	<-ran
	<-ran
	mu.Lock()
	next := turns[1]
	mu.Unlock()
	if len(next) != 1 || next[0].ID != out.Wake {
		types := make([]string, 0, len(next))
		for _, ev := range next {
			types = append(types, ev.Type)
		}
		t.Fatalf("the seat's next turn ran on %v, want the urgent pickup ahead of the %d "+
			"assignment wakes its drain still held", types, backlog-1)
	}
}
