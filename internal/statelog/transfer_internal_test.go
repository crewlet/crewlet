package statelog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// EVERY BOUNDED WAIT IN A TRANSFER SAYS WHOSE END IT REACHED.
//
// Each wait — the ask's flush, the fetch's flush, the wait for a chunk — is a
// child of the caller's context with a limit of this package's own, so both
// ends fail through the child and look alike there. The caller's end must come
// back wrapping the caller's own error, promptly, because that is how the join
// tells a node being stopped from a fleet with nothing to give. The package's
// own bound must come back NAMED and not as DeadlineExceeded, because in the
// chain that reads as the caller's deadline having passed when the caller set
// none — and before the flushes took a context at all, a plain Flush held a
// stopping node for the client's own ten seconds and answered `nats: timeout`.
//
// A real broker confirms a flush in microseconds, so no case against one can
// reach either flush's end; this one talks to a broker that stops answering.
func TestEveryTransferWaitSaysWhoseEndItReached(t *testing.T) {
	t.Parallel()
	const ownBound = 200 * time.Millisecond
	waits := []struct {
		name string
		// confirm is whether the broker answers the client's flushes:
		// without it the flush is the wait that ends, with it the
		// flush returns and the wait for a chunk is.
		confirm bool
		// production is the bound the engine's own call runs under.
		production time.Duration
		run        func(ctx context.Context, nc *nats.Conn, bound time.Duration, dest string) error
		// step is what the caller's end names; own is what the
		// package's bound names.
		step, own string
	}{
		{
			name: "the ask's flush", production: OfferWindow,
			run: func(ctx context.Context, nc *nats.Conn, bound time.Duration, _ string) error {
				_, err := CollectOffers(ctx, nc, OfferRequest{NodeID: "joiner"}, bound)
				return err
			},
			step: "flush the offer request", own: "did not confirm the offer request",
		},
		{
			name: "the fetch's flush", production: TransferChunkWait,
			run: func(ctx context.Context, nc *nats.Conn, bound time.Duration, dest string) error {
				_, err := fetchArtefact(ctx, nc, Offer{Fetch: SubjectFetchPrefix + "donor"}, dest, bound)
				return err
			},
			step: "flush the fetch", own: "did not confirm the fetch request",
		},
		{
			name: "the wait for a chunk", confirm: true, production: TransferChunkWait,
			run: func(ctx context.Context, nc *nats.Conn, bound time.Duration, dest string) error {
				_, err := fetchArtefact(ctx, nc, Offer{Fetch: SubjectFetchPrefix + "donor"}, dest, bound)
				return err
			},
			step: "receive a chunk", own: "the donor stopped sending",
		},
	}
	for _, w := range waits {
		t.Run(w.name+"/its own bound", func(t *testing.T) {
			t.Parallel()
			nc := fakeBroker(t, w.confirm)
			started := time.Now()
			err := w.run(t.Context(), nc, ownBound, filepath.Join(t.TempDir(), "part"))
			took := time.Since(started)
			if err == nil {
				t.Fatal("a wait nothing ended reported success")
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("the package's own bound came back as a context error (%v) — "+
					"read by a caller, that is its own context ending, which it did not", err)
			}
			if !strings.Contains(err.Error(), w.own) || !strings.Contains(err.Error(), ownBound.String()) {
				t.Fatalf("the error does not say %q and name the %s bound: %v",
					w.own, ownBound, err)
			}
			// Well inside the client's own ten-second flush timeout,
			// which is what a flush bounded by nothing it was given
			// runs into instead.
			if took > 5*time.Second {
				t.Fatalf("a %s bound took %s to end the wait", ownBound, took)
			}
		})
		ends := []struct {
			name string
			ctx  func(t *testing.T) context.Context
			want error
		}{
			{"the caller's deadline", func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			}, context.DeadlineExceeded},
			{"the caller cancelling", func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(100*time.Millisecond, cancel)
				t.Cleanup(cancel)
				return ctx
			}, context.Canceled},
		}
		for _, end := range ends {
			t.Run(w.name+"/"+end.name, func(t *testing.T) {
				t.Parallel()
				nc := fakeBroker(t, w.confirm)
				started := time.Now()
				err := w.run(end.ctx(t), nc, w.production, filepath.Join(t.TempDir(), "part"))
				took := time.Since(started)
				if !errors.Is(err, end.want) {
					t.Fatalf("a wait ended by %s returned %v, want it wrapped — "+
						"anything else is the caller's own end reported as somebody "+
						"else's", end.name, err)
				}
				if !strings.Contains(err.Error(), w.step) {
					t.Fatalf("the error does not name the step it ended (%q): %v", w.step, err)
				}
				// Far inside both the production bound and the client's
				// ten seconds: the caller ended it at 100ms.
				if took > 3*time.Second {
					t.Fatalf("a wait whose caller ended it at 100ms took %s — it sat "+
						"out a bound the caller cannot end", took)
				}
			})
		}
	}
}

// fakeBroker is just enough of a NATS server to hold one client connection:
// it sends INFO, answers the connect handshake's PING, reads everything the
// client sends after that and delivers nothing.
//
// With confirm set it answers every later PING too — a broker that confirms
// each flush and has nobody to route a message to. Without it, it stops
// answering while the connection stays up, which is what a half-open TCP
// connection to a wedged server looks like to a client: nothing closes, so
// nothing but a bound ends the wait.
func fakeBroker(t *testing.T, confirm bool) *nats.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveFake(conn, confirm)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	nc, err := nats.Connect("nats://"+ln.Addr().String(),
		nats.NoReconnect(), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the fake broker: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// serveFake speaks the server half of the NATS text protocol, as far as a
// client needs it to stay connected.
func serveFake(conn net.Conn, confirm bool) {
	defer func() { _ = conn.Close() }()
	port := conn.LocalAddr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"server_name\":\"fake\","+
		"\"version\":\"2.10.0\",\"proto\":1,\"host\":\"127.0.0.1\",\"port\":%d,"+
		"\"headers\":true,\"max_payload\":1048576}\r\n", port); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	handshake := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PING":
			if handshake || confirm {
				if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
					return
				}
			}
			handshake = false
		case "PUB", "HPUB":
			// THE PAYLOAD IS SKIPPED BY ITS DECLARED SIZE, never read as
			// lines: a payload is bytes, and one that happened to hold
			// a line reading PING would otherwise be answered.
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, r, int64(size)+2); err != nil {
				return
			}
		}
	}
}
