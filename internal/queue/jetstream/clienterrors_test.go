package jetstream

import (
	"bytes"
	"context"
	"errors"
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
// Two halves, for [reconnectWatch]'s reason: the client calls the handler on a
// goroutine of its own, so its answer is asserted directly with a logger the
// case holds, and then that it is the handler every connection this package
// opens is given — the queue's own, a watched second one and an owned one, on
// an embedded broker and through an operator's server's dial.
//
// Mutation: drop the ErrorHandler option from either dial and a row of the
// second half goes red; log nothing, or drop the subscription's subject, and
// the first half does.
func TestTheClientsAsynchronousErrorsReachTheEnginesLogger(t *testing.T) {
	t.Parallel()

	t.Run("it logs what it is handed", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		errs := clientErrors{log: slog.New(slog.NewTextHandler(&logged, nil))}
		errs.reported(nil, &nats.Subscription{Subject: "crewlet.probe.slow"}, nats.ErrSlowConsumer)
		errs.reported(nil, nil, errors.New("nats: authorization violation"))
		errs.reported(nil, nil, nil)
		lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("logged %d lines for two errors and a nil, want 2:\n%s",
				len(lines), logged.String())
		}
		for i, want := range [][]string{
			{"level=WARN", "jetstream_client_error", "subject=crewlet.probe.slow", "slow consumer"},
			{"level=WARN", "jetstream_client_error", "authorization violation"},
		} {
			for _, part := range want {
				if !strings.Contains(lines[i], part) {
					t.Errorf("line %q does not carry %q", lines[i], part)
				}
			}
		}
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
