package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// THE CLIENT'S ASYNCHRONOUS ERRORS REACH THE ENGINE'S LOGGER, ON EVERY
// CONNECTION.
//
// A connection given no handler gets the client's default, which writes each
// one to the process's raw stderr with fmt: a line outside every handler the
// engine configured — never in logging.file, never at a level, a bare line in
// the middle of a JSON stream — and the only place the authorization refusal
// before a connection's close for good was ever said.
//
// Three halves. The client calls the handler on a goroutine of its own, so its
// answer is asserted directly with a logger the case holds, as
// [reconnectWatch]'s is. Then that it reads NOTHING off the subscription it is
// handed: the client rewrites a JetStream ordered consumer's Subject on every
// reset under a lock it does not export, and reports that consumer's errors on
// the same subscription from its dispatcher — every coordination-store walk is
// such a consumer — so a handler that read the field raced the client, and the
// writer below is that reset, which the race detector every suite here runs
// under reports against any read. And then that it is the handler every
// connection this package opens is given — the queue's own, a watched second
// one and an owned one, on an embedded broker and through an operator's
// server's dial.
//
// Mutation: drop the ErrorHandler option from either dial and a row of the
// third half goes red; log nothing and the first half does; read the
// subscription's Subject again and the first half names a reset's inbox while
// the race detector fails the second.
func TestTheClientsAsynchronousErrorsReachTheEnginesLogger(t *testing.T) {
	t.Parallel()

	t.Run("it logs what it is handed", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		errs := clientErrors{log: slog.New(slog.NewTextHandler(&logged, nil))}
		const inbox = "_INBOX.a-reset-moved-it-here"
		errs.reported(nil, &nats.Subscription{Subject: inbox}, nats.ErrSlowConsumer)
		errs.reported(nil, nil, errors.New("nats: authorization violation"))
		errs.reported(nil, nil, nil)
		lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("logged %d lines for two errors and a nil, want 2:\n%s",
				len(lines), logged.String())
		}
		for i, want := range [][]string{
			{"level=WARN", "jetstream_client_error", "slow consumer"},
			{"level=WARN", "jetstream_client_error", "authorization violation"},
		} {
			for _, part := range want {
				if !strings.Contains(lines[i], part) {
					t.Errorf("line %q does not carry %q", lines[i], part)
				}
			}
		}
		if strings.Contains(lines[0], inbox) {
			t.Errorf("line %q names the subscription's Subject, a field the client "+
				"rewrites under a lock this package cannot take", lines[0])
		}
	})

	t.Run("it reads nothing a reset rewrites", func(t *testing.T) {
		t.Parallel()
		errs := clientErrors{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		sub := &nats.Subscription{Subject: "_INBOX.before-the-reset"}
		const resets = 1000
		reset := make(chan struct{})
		go func() {
			defer close(reset)
			// AS resetOrderedConsumer DOES, on its own goroutine — under
			// the subscription's lock there, which is unexported, so no
			// reader outside the client can be ordered against it.
			for i := range resets {
				sub.Subject = fmt.Sprintf("_INBOX.reset-%d", i)
			}
		}()
		for range resets {
			errs.reported(nil, sub, nats.ErrConsumerNotActive)
		}
		<-reset
	})

	t.Run("it is the handler every connection is given", func(t *testing.T) {
		t.Parallel()
		embedded, err := StartServer(t.Context(), Config{StoreDir: t.TempDir()})
		if err != nil {
			t.Fatalf("StartServer: %v", err)
		}
		t.Cleanup(embedded.Shutdown)
		q, err := embedded.Client(t.Context())
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		watched, err := q.DialWatched()
		if err != nil {
			t.Fatalf("DialWatched: %v", err)
		}
		t.Cleanup(watched.Close)
		owned, err := q.DialOwned()
		if err != nil {
			t.Fatalf("DialOwned: %v", err)
		}
		t.Cleanup(owned.Close)
		external := newQueueWith(t, Config{URL: externaltest.Start(t, queue.MaxPayloadBytes).URL()})

		want := reflect.ValueOf(clientErrors{}.reported).Pointer()
		for name, nc := range map[string]*nats.Conn{
			"the queue's own":            q.Conn(),
			"a watched second one":       watched,
			"an owned second one":        owned,
			"an operator's server's one": external.Conn(),
		} {
			installed := nc.Opts.AsyncErrorCB
			if installed == nil || reflect.ValueOf(installed).Pointer() != want {
				t.Errorf("%s connection reports its asynchronous errors through "+
					"something other than clientErrors.reported — the client's "+
					"default writes them to raw stderr", name)
			}
		}
	})
}
