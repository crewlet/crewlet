package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

func probeEvent(conv string) *events.Event {
	return &events.Event{
		ID:        uuid.New(),
		Type:      "probe",
		Timestamp: time.Now().UTC(),
		Source:    "probe",
		Payload:   map[string]any{"conv": conv},
	}
}

func probeKey(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if c, ok := ev.Payload["conv"].(string); ok {
		return c
	}
	return ""
}

func runProbe(t *testing.T, name string, block func(q *memory.Queue, topic, group string)) {
	t.Helper()
	ctx := context.Background()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = q.Stop(ctx) }()

	topic, group := "probe."+name, "grp"
	var mu sync.Mutex
	var seen []string
	err := q.SubscribeBatch(ctx, topic, group,
		func(_ context.Context, evs []*events.Event) queue.Result {
			mu.Lock()
			seen = append(seen, probeKey(evs[0]))
			first := len(seen) == 1
			mu.Unlock()
			if first {
				block(q, topic, group)
			}
			return queue.Ack()
		}, probeKey, queue.DefaultBatchOptions())
	if err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}

	// Fill one chunk: hold, publish a/b/c, release.
	if err := q.PauseTopic(ctx, topic, group, "fill"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	for _, c := range []string{"a", "b", "c"} {
		if err := q.Publish(ctx, topic, probeEvent(c)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if err := q.ResumeTopic(ctx, topic, group, "fill"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	t.Logf("PROBE %s: partitions handled = %v, backlog = %d", name, got, len(q.Backlog(topic, group)))
}

func TestZZProbe(t *testing.T) {
	runProbe(t, "hold", func(q *memory.Queue, topic, group string) {
		if err := q.PauseTopic(context.Background(), topic, group, "park"); err != nil {
			t.Errorf("PauseTopic: %v", err)
		}
	})
	runProbe(t, "pausedelivery", func(q *memory.Queue, _, _ string) {
		if err := q.PauseDelivery(context.Background()); err != nil {
			t.Errorf("PauseDelivery: %v", err)
		}
	})
	runProbe(t, "detach", func(q *memory.Queue, topic, group string) {
		if _, err := q.Detach(context.Background(), topic, group); err != nil {
			t.Errorf("Detach: %v", err)
		}
	})
}
