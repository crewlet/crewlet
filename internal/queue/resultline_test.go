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
// A FAILURE NAMES WHOSE IT IS, the message's or its node's own
// ([queue.NakNode]), as the backend reads it when it decides where the message
// is kept.
//
// Mutation: log a partition's deferral as `delivery_deferred`, and the
// partition's row reads a single delivery's name; name every failure the
// message's, and a node's own reads as poison.
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
			"msg": "handler_failed", "level": "WARN", "error": failed.Error(), "fault": "message"}},
		{"a delivery its node failed", single, queue.NakNode(failed), map[string]any{
			"msg": "handler_failed", "level": "WARN", "error": failed.Error(), "fault": "node"}},
		{"a deferred delivery", single, queue.Defer(queue.DeferDraining, "the node drains"), map[string]any{
			"msg": "delivery_deferred", "level": "INFO", "reason": "the node drains"}},
		{"a failed partition", partition, queue.Nak(failed), map[string]any{
			"msg": "batch_handler_failed", "level": "WARN", "error": failed.Error(),
			"batch_key": "conversation-1", "event_count": float64(1), "fault": "message"}},
		{"a partition its node failed", partition, queue.NakNode(failed), map[string]any{
			"msg": "batch_handler_failed", "level": "WARN", "error": failed.Error(),
			"batch_key": "conversation-1", "event_count": float64(1), "fault": "node"}},
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

// A PASS IS LOGGED AS A PASS, NEVER AS A FAILURE, on both paths.
//
// A pass hands a message to a peer that can read it — an older build's
// ordinary minutes during a roll — and nothing failed. Logged as the failure
// path's `handler_failed` at Warn, every roll would read as a fleet failing
// its mail, and an operator would go looking for a fault in the one outcome
// that is none. So each path names it in its own words, at Info, with why it
// was passed and what the node met.
//
// Mutation: log a pass in LogResult's Nak arm, and a delivery's pass reads
// `handler_failed` at Warn; drop LogBatchResult's pass arm, and a passed
// partition logs nothing.
func TestAPassIsLoggedAsAPassNeverAsAFailure(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Type: "external_notification"}
	met := errors.New("worklet: the record is version 9 and this build reads 8")
	for _, tc := range []struct {
		name string
		emit func(context.Context, *slog.Logger, queue.Result)
		want map[string]any
	}{
		{"a delivery", func(ctx context.Context, l *slog.Logger, r queue.Result) {
			queue.LogResult(ctx, l, "crewlet.notifications.inbound", "notify-inbound", ev, r)
		}, map[string]any{"msg": "delivery_passed"}},
		{"a partition", func(ctx context.Context, l *slog.Logger, r queue.Result) {
			queue.LogBatchResult(ctx, l, "crewlet.notifications.inbound", "notify-inbound",
				"conversation-1", []*events.Event{ev}, r)
		}, map[string]any{"msg": "batch_delivery_passed", "batch_key": "conversation-1",
			"event_count": float64(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			tc.emit(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)),
				queue.Pass("newer_record", met))

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("decoding %q: %v", buf.String(), err)
			}
			want := map[string]any{"level": "INFO", "reason": "newer_record", "error": met.Error(),
				"topic": "crewlet.notifications.inbound", "group": "notify-inbound",
				"event_type": "external_notification"}
			for key, value := range tc.want {
				want[key] = value
			}
			for key, value := range want {
				if got := line[key]; got != value {
					t.Errorf("%s = %v, want %v, in %s", key, got, value, buf.String())
				}
			}
		})
	}
}

// EVERY OUTCOME RENDERS ITS OWN NAME, because String is what a line reads an
// outcome by and its fallback is "ack": an outcome it does not name would read
// as a delivery that was settled.
//
// Mutation: drop the pass case from Outcome.String, and a pass renders as
// "ack".
func TestEveryOutcomeRendersItsOwnName(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[queue.Outcome]string{
		queue.OutcomeAck: "ack", queue.OutcomeNak: "nak",
		queue.OutcomeDefer: "defer", queue.OutcomePass: "pass",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("outcome %d renders %q, want %q", int(outcome), got, want)
		}
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
		`"delivery_passed"`,
		`"batch_delivery_passed"`,
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

// A DEAD LETTER IS ONE LINE, NAMING WHERE ITS COPY IS KEPT AND WHAT SPENT IT.
//
// Both backends log a message whose budget a return spent through
// [queue.LogDeadLettered], so the line is held here once: its name, its level,
// how many deliveries the message had, the return that spent it and why, and
// the subject its copy went to — which tells an operator whether it is poison
// or a delivery no member handled, waiting for a reader that can.
//
// Mutation: name the subject by the pair's poison whatever the message's
// failures, and a pass's line sends an operator to the wrong subject; drop the
// reason, and a hand-back's line no longer says what blocked it.
func TestADeadLetterIsOneLineNamingWhereItsCopyIsKept(t *testing.T) {
	t.Parallel()
	const topic, group = "crewlet.notifications.inbound", "notify-inbound"
	ev := &events.Event{Type: "external_notification"}
	for _, tc := range []struct {
		spent  queue.Outcome
		reason string
		failed bool
	}{
		{queue.OutcomeNak, "", true},
		{queue.OutcomeNak, "", false},
		{queue.OutcomePass, "newer_record", false},
		{queue.OutcomeDefer, "shedding", false},
		{queue.OutcomeDefer, "shedding", true},
	} {
		var buf bytes.Buffer
		queue.LogDeadLettered(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), topic, group,
			ev, 25, tc.spent, tc.reason, tc.failed)
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("decoding %q: %v", buf.String(), err)
		}
		want := map[string]any{"msg": "dead_lettered", "level": "ERROR", "topic": topic,
			"group": group, "event_type": "external_notification", "deliveries": float64(25),
			"returned": tc.spent.String(), "reason": tc.reason,
			"subject": queue.RetiredTo(topic, group, tc.failed)}
		for key, value := range want {
			if got := line[key]; got != value {
				t.Errorf("a %s's dead letter logs %s = %v, want %v, in %s", tc.spent, key, got,
					value, buf.String())
			}
		}
	}
}

// NO BACKEND WRITES ITS OWN DEAD-LETTER LINE, for
// [TestNoBackendWritesItsOwnResultLine]'s reason: the twin spelled its own,
// `event_dead_lettered` with a count of redeliveries, where the broker logged
// `dead_lettered` with its deliveries, and the line that said a copy was kept
// bare beside its pass was spelled once in each backend. A backend calls
// [queue.LogDeadLettered], which takes the context a line is correlated by.
//
// Mutation: spell the twin's line again (`log.ErrorContext(ctx,
// "event_dead_lettered", …)`), and its file is named.
func TestNoBackendWritesItsOwnDeadLetterLine(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		`"dead_lettered"`,
		`"event_dead_lettered"`,
		`"dead_letter_unmarked"`,
	}
	for file, raw := range backendSources(t) {
		for _, bad := range forbidden {
			if bytes.Contains(raw, []byte(bad)) {
				t.Errorf("%s spells %s itself; call queue.LogDeadLettered so every "+
					"backend says the same thing", file, strings.Trim(bad, `"`))
			}
		}
	}
}
