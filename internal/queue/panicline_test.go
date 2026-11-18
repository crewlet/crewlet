package queue_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// Every backend must emit the same name for the same situation, and a suite
// run on the in-memory twin cannot see a backend's divergent spelling: one
// test pins the wire shape of the contract's own panic line, the other refuses
// a backend that writes its own.

func TestAPanicLineNamesThePanicAndTheTopic(t *testing.T) {
	ev := &events.Event{Type: "task.created"}

	for _, tc := range []struct {
		name  string
		emit  func(*slog.Logger)
		event string
	}{
		{"listener", func(l *slog.Logger) {
			queue.LogListenerPanic(l, "crewlet.agent.alice.inbox", ev, "boom")
		}, "publish_listener_panicked"},
		{"stream", func(l *slog.Logger) {
			queue.LogStreamHandlerPanic(l, "crewlet.agent.alice.inbox", ev, "boom")
		}, "stream_handler_panicked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(slog.New(slog.NewJSONHandler(&buf, nil)))

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("decoding %q: %v", buf.String(), err)
			}
			if got := line["msg"]; got != tc.event {
				t.Errorf("event name = %v, want %q", got, tc.event)
			}
			// `panic`, not `error`: the key tells an operator the
			// callback crashed rather than returned badly.
			if got := line["panic"]; got != "boom" {
				t.Errorf("panic = %v, want \"boom\"", got)
			}
			if _, bad := line["error"]; bad {
				t.Error("a recovered panic must not be reported under `error`")
			}
			// `topic` on every backend: `subject` is NATS's word, not
			// the contract's.
			if got := line["topic"]; got != "crewlet.agent.alice.inbox" {
				t.Errorf("topic = %v, want the topic", got)
			}
			if _, bad := line["subject"]; bad {
				t.Error("the topic key is `topic`, never `subject`")
			}
			if got := line["event_type"]; got != "task.created" {
				t.Errorf("event_type = %v, want task.created", got)
			}
			if line["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR — nothing redelivers behind this",
					line["level"])
			}
		})
	}
}

// TestNoBackendWritesItsOwnPanicLine refuses a backend spelling the event
// itself rather than calling the contract's helper: a hand-written line
// drifts into a second name for one situation.
func TestNoBackendWritesItsOwnPanicLine(t *testing.T) {
	// The two names the contract owns, plus the two drifted spellings, so
	// a revert to either is caught.
	forbidden := []string{
		`"publish_listener_panicked"`,
		`"stream_handler_panicked"`,
		`"publish_listener_failed"`,
		`"stream_handler_failed"`,
	}

	for file, raw := range backendSources(t) {
		for _, bad := range forbidden {
			if bytes.Contains(raw, []byte(bad)) {
				t.Errorf("%s spells %s itself; call "+
					"queue.LogListenerPanic / queue.LogStreamHandlerPanic "+
					"so every backend says the same thing", file, bad)
			}
		}
	}
}

// A nil event (a panic before the event was decoded) must still produce a
// line rather than a nil dereference.
func TestAPanicLineSurvivesANilEvent(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	queue.LogListenerPanic(log, "t", nil, context.Canceled)
	queue.LogStreamHandlerPanic(log, "t", nil, context.Canceled)
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 2 {
		t.Fatalf("want two lines, got %d: %s", lines, buf.String())
	}
}

// AND THE LINE CARRIES THE STACK, from the frame that actually panicked: a
// value alone does not say which handler failed. The helpers are called from
// inside the recovering deferred function, whose frame is not yet popped, so
// debug.Stack() still reaches the panicking function. Asserted by panicking in
// a named function and looking for it.
func TestAPanicLineCarriesTheStackOfWhatPanicked(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	func() {
		defer func() {
			if r := recover(); r != nil {
				queue.LogListenerPanic(logger, "crewlet.agent.alice.inbox",
					&events.Event{Type: "task.created"}, r)
			}
		}()
		panicFromANamedFrame()
	}()

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	stack, _ := line["stack"].(string)
	if stack == "" {
		t.Fatalf("the panic line carries no stack: %s", buf.String())
	}
	if !strings.Contains(stack, "panicFromANamedFrame") {
		t.Errorf("the stack does not reach the frame that panicked:\n%s", stack)
	}
}

// panicFromANamedFrame exists to be findable in a stack trace.
func panicFromANamedFrame() { panic("boom") }
