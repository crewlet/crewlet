package queue_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// The contract says every backend must emit the same name for the same
// situation. Nothing connected that sentence to the backends, and all three
// drifted: `memory` logged a recovered listener panic as
// `publish_listener_failed` with the value under `error`, while `jetstream`
// and `pulsar` logged `publish_listener_panicked` with it under `panic`; the
// stream side additionally keyed the topic as `subject` on those two and
// `topic` on the third.
//
// The reason it survived is that the in-memory twin is the one the tests run
// on, so the suite watched the divergent spelling go past on every run. These
// two tests are what would have caught it: one pins the wire shape of the
// contract's own line, the other refuses a backend that writes its own.

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
			// `panic`, not `error`: a recovered value is whatever was
			// passed to panic(), and the key is what tells an operator
			// the callback crashed rather than returned badly.
			if got := line["panic"]; got != "boom" {
				t.Errorf("panic = %v, want \"boom\"", got)
			}
			if _, bad := line["error"]; bad {
				t.Error("a recovered panic must not be reported under `error`")
			}
			// `topic` on every backend — `subject` is NATS's word, and
			// two backends used to leak it into the contract's vocabulary.
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

// TestNoBackendWritesItsOwnPanicLine is the guard that would have caught the
// drift. A backend must call the contract's helper rather than spell the
// event itself, because a hand-written line is how three backends ended up
// with three names for one situation.
func TestNoBackendWritesItsOwnPanicLine(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}

	// The names a backend must not emit directly — the two the contract
	// owns, plus the two spellings the backends had drifted onto, so a
	// revert to either is caught rather than silently re-landing.
	forbidden := []string{
		`"publish_listener_panicked"`,
		`"stream_handler_panicked"`,
		`"publish_listener_failed"`,
		`"stream_handler_failed"`,
	}

	for _, backend := range []string{"memory", "jetstream", "pulsar"} {
		entries, err := os.ReadDir(filepath.Join(dir, backend))
		if err != nil {
			t.Fatalf("reading backend %s: %v", backend, err)
		}
		var scanned int
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") {
				continue
			}
			scanned++
			raw, err := os.ReadFile(filepath.Join(dir, backend, name))
			if err != nil {
				t.Fatalf("reading %s/%s: %v", backend, name, err)
			}
			for _, bad := range forbidden {
				if bytes.Contains(raw, []byte(bad)) {
					t.Errorf("%s/%s spells %s itself; call "+
						"queue.LogListenerPanic / queue.LogStreamHandlerPanic "+
						"so every backend says the same thing",
						backend, name, bad)
				}
			}
		}
		// A backend whose files stopped being found would pass this test
		// by reading nothing at all.
		if scanned == 0 {
			t.Errorf("scanned no source files for backend %s", backend)
		}
	}
}

// The contract's helpers must not format anything the caller did not ask
// for: a nil event is what a backend has when the panic happened before the
// event was decoded, and it must still produce a line rather than a nil
// dereference.
func TestAPanicLineSurvivesANilEvent(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	queue.LogListenerPanic(log, "t", nil, context.Canceled)
	queue.LogStreamHandlerPanic(log, "t", nil, context.Canceled)
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 2 {
		t.Fatalf("want two lines, got %d: %s", lines, buf.String())
	}
}
