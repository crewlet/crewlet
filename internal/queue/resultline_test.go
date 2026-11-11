package queue_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// EACH PATH LOGS AN OUTCOME UNDER ITS OWN NAME, and an ack under none.
//
// One delivery's outcome and one partition's are operationally different
// things — a bad event against a bad turn — so the contract names them apart
// ([queue.LogResult], [queue.LogBatchResult]), and a log consumer counting
// one must never be handed the other. The in-memory twin spelled its own
// lines and logged a partition's deferral as `delivery_deferred`.
//
// Mutation: log a partition's deferral as `delivery_deferred`, and the
// partition's row reads a single delivery's name.
func TestEachPathLogsAnOutcomeUnderItsOwnName(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Type: "external_notification"}
	failed := errors.New("the store did not answer")
	single := func(ctx context.Context, l *slog.Logger, r queue.Result) {
		queue.LogResult(ctx, l, "crewlet.notifications.inbound", "notify-inbound", ev, r)
	}
	partition := func(ctx context.Context, l *slog.Logger, r queue.Result) {
		queue.LogBatchResult(ctx, l, "crewlet.notifications.inbound", "notify-inbound",
			"conversation-1", []*events.Event{ev}, r)
	}

	for _, tc := range []struct {
		name   string
		emit   func(context.Context, *slog.Logger, queue.Result)
		result queue.Result
		want   map[string]any
	}{
		{"a failed delivery", single, queue.Nak(failed), map[string]any{
			"msg": "handler_failed", "level": "WARN", "error": failed.Error()}},
		{"a deferred delivery", single, queue.Defer(queue.DeferDraining, "the node drains"), map[string]any{
			"msg": "delivery_deferred", "level": "INFO", "reason": "the node drains"}},
		{"a failed partition", partition, queue.Nak(failed), map[string]any{
			"msg": "batch_handler_failed", "level": "WARN", "error": failed.Error(),
			"batch_key": "conversation-1", "event_count": float64(1)}},
		{"a deferred partition", partition, queue.Defer(queue.DeferDraining, "the node drains"), map[string]any{
			"msg": "batch_delivery_deferred", "level": "INFO", "reason": "the node drains",
			"batch_key": "conversation-1", "event_count": float64(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			tc.emit(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), tc.result)

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("decoding %q: %v", buf.String(), err)
			}
			for key, want := range tc.want {
				if got := line[key]; got != want {
					t.Errorf("%s = %v, want %v, in %s", key, got, want, buf.String())
				}
			}
			for key, want := range map[string]any{"topic": "crewlet.notifications.inbound",
				"group": "notify-inbound", "event_type": "external_notification"} {
				if got := line[key]; got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
		})
	}

	// AND AN ACK IS SILENT on both paths: the overwhelming majority of
	// deliveries, whose line would bury the ones that matter.
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	single(t.Context(), l, queue.Ack())
	partition(t.Context(), l, queue.Ack())
	if buf.Len() != 0 {
		t.Errorf("an ack logged %s", buf.String())
	}
}

// NO BACKEND WRITES ITS OWN OUTCOME LINE, for the reason
// [TestNoBackendWritesItsOwnPanicLine] gives about panics: a line a backend
// spells is a line that drifts. The twin's did, and a partition it deferred
// was logged under a single delivery's name. A backend calls
// [queue.LogResult] or [queue.LogBatchResult], which take the context a line
// is correlated by, so no backend has a reason left to spell one.
//
// Mutation: spell the deferral's line in the twin again
// (`log.InfoContext(ctx, "delivery_deferred", …)`), and its file is named.
func TestNoBackendWritesItsOwnResultLine(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		`"handler_failed"`,
		`"batch_handler_failed"`,
		`"delivery_deferred"`,
		`"batch_delivery_deferred"`,
	}
	for file, raw := range backendSources(t) {
		for _, bad := range forbidden {
			if bytes.Contains(raw, []byte(bad)) {
				t.Errorf("%s spells %s itself; call queue.LogResult / "+
					"queue.LogBatchResult so every backend says the same thing",
					file, strings.Trim(bad, `"`))
			}
		}
	}
}
