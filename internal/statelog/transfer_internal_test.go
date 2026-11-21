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
// Each wait (the ask's flush, the fetch's flush, the wait for a chunk) is a
// child of the caller's context bounded by this package, so both ends fail
// alike through it. The caller's must come back promptly, wrapping its error:
// that is how the join tells a node being stopped from a fleet with nothing to
// give. The package's must come back NAMED, never as DeadlineExceeded (a
// deadline the caller never set). A real broker confirms a flush at once, so
// this one stops answering.
//
// THE CALLER'S END IS CUED BY THE BROKER seeing the wait (fakeBroker's
// waiting), never timed: a clock races the steps before it, naming one.
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
				_, err := fetchArtefact(ctx, nc, Offer{Fetch: SubjectFetchPrefix + "donor"}, dest, waitingFor(bound))
				return err
			},
			step: "flush the fetch", own: "did not confirm the fetch request",
		},
		{
			name: "the wait for a chunk", confirm: true, production: TransferChunkWait,
			run: func(ctx context.Context, nc *nats.Conn, bound time.Duration, dest string) error {
				_, err := fetchArtefact(ctx, nc, Offer{Fetch: SubjectFetchPrefix + "donor"}, dest, waitingFor(bound))
				return err
			},
			step: "receive a chunk", own: "the donor stopped sending",
		},
	}
	for _, w := range waits {
		t.Run(w.name+"/its own bound", func(t *testing.T) {
			t.Parallel()
			nc, _ := fakeBroker(t, w.confirm)
			started := time.Now()
			err := w.run(t.Context(), nc, ownBound, filepath.Join(t.TempDir(), "part"))
			took := time.Since(started)
			if err == nil {
				t.Fatal("a wait nothing ended reported success")
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("the package's own bound came back as a context error (%v), "+
					"which a caller reads as its own context ending, and it did not", err)
			}
			if !strings.Contains(err.Error(), w.own) || !strings.Contains(err.Error(), ownBound.String()) {
				t.Fatalf("the error does not say %q and name the %s bound: %v",
					w.own, ownBound, err)
			}
			// Well inside the client's ten-second flush timeout, which
			// an unbounded flush would sit out instead.
			if took > 5*time.Second {
				t.Fatalf("a %s bound took %s to end the wait", ownBound, took)
			}
		})
		ends := []struct {
			name string
			// caller returns the caller's context and what ends it.
			caller func(t *testing.T) (context.Context, func())
			want   error
		}{
			{"the caller's deadline", func(t *testing.T) (context.Context, func()) {
				ctx := &deadlineOnCue{Context: t.Context(), passed: make(chan struct{})}
				return ctx, ctx.pass
			}, context.DeadlineExceeded},
			{"the caller cancelling", func(t *testing.T) (context.Context, func()) {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				return ctx, cancel
			}, context.Canceled},
		}
		for _, end := range ends {
			t.Run(w.name+"/"+end.name, func(t *testing.T) {
				t.Parallel()
				nc, waiting := fakeBroker(t, w.confirm)
				ctx, endCaller := end.caller(t)
				var ended time.Time
				returned := make(chan struct{})
				go func() {
					select {
					case <-waiting:
						ended = time.Now()
						endCaller()
					case <-returned:
					}
				}()
				err := w.run(ctx, nc, w.production, filepath.Join(t.TempDir(), "part"))
				close(returned)
				if !errors.Is(err, end.want) {
					t.Fatalf("a wait ended by %s returned %v, want it wrapped: "+
						"anything else is the caller's own end reported as somebody "+
						"else's", end.name, err)
				}
				if !strings.Contains(err.Error(), w.step) {
					t.Fatalf("the error does not name the step it ended (%q): %v", w.step, err)
				}
				// The wrapped end proves the cue fired, so ended is set.
				// Far inside the production bound and the client's ten seconds.
				if took := time.Since(ended); took > 3*time.Second {
					t.Fatalf("a wait took %s to return once its caller ended it: it "+
						"sat out a bound the caller cannot end", took)
				}
			})
		}
	}
}

// A COPY THIS NODE CANNOT WRITE IS ITS OWN FAULT, AND A DONOR THAT STOPS IS NOT.
//
// The join files a failure by whose it is ([nodeFault]): the offer's moves on
// to the next donor; this node's ends the join, since every other artefact
// would land on the same disk. The fetch draws the line: a destination this
// node cannot create is its own, a donor that stops after one chunk the offer's.
func TestACopyThisNodeCannotWriteIsItsOwnFault(t *testing.T) {
	t.Parallel()
	nc, _ := fakeBroker(t, true)
	offer := Offer{Fetch: SubjectFetchPrefix + "donor"}

	_, err := fetchArtefact(t.Context(), nc, offer,
		filepath.Join(t.TempDir(), "no-such-directory", "part"), waitingFor(time.Second))
	if fault := (*nodeFault)(nil); !errors.As(err, &fault) {
		t.Fatalf("a copy this node could not create came back as %v, not as this "+
			"node's own fault — the join would blame the donor and fetch every "+
			"other offer to the same disk", err)
	}

	_, err = fetchArtefact(t.Context(), nc, offer,
		filepath.Join(t.TempDir(), "part"), waitingFor(200*time.Millisecond))
	if err == nil {
		t.Fatal("a donor that stopped after one chunk reported a complete transfer")
	}
	if fault := (*nodeFault)(nil); errors.As(err, &fault) {
		t.Fatalf("a donor that stopped sending came back as this node's own "+
			"fault (%v) — the join would end there rather than try the next offer", err)
	}
}

// waitingFor is the production pace with each chunk's wait bounded by wait.
func waitingFor(wait time.Duration) transferPace {
	return transferPace{wait: wait, report: TransferReportEvery, log: loggerOr(nil)}
}

// deadlineOnCue is a caller's context whose deadline passes when pass is
// called, not at a time (a clock races the code before the wait under test).
// The waits observe only Done and Err, through a child that the standard
// library ends with this parent's error.
type deadlineOnCue struct {
	context.Context
	passed chan struct{}
}

func (c *deadlineOnCue) pass() { close(c.passed) }

func (c *deadlineOnCue) Done() <-chan struct{} { return c.passed }

func (c *deadlineOnCue) Err() error {
	select {
	case <-c.passed:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// fakeCreditSubject is the reply subject on the one chunk the fake broker
// sends, so the credit the client returns for it is recognisable.
const fakeCreditSubject = "fake.credit"

// fakeBroker is just enough of a NATS server for one client: INFO, the
// handshake's PONG, and one chunk to each fetch.
//
// With confirm it answers every later PING too (a donor that sends one chunk
// and stops). Without it, it goes silent with the connection up, as a
// half-open connection to a wedged server does: only a bound ends the wait.
//
// waiting receives once the client blocks in a wait the broker can see: an
// unconfirmed flush (its PING), or, with confirm, the wait for the chunk after
// the one it credited.
func fakeBroker(t *testing.T, confirm bool) (nc *nats.Conn, waiting <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cue := make(chan struct{}, 1)
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
				serveFake(conn, confirm, cue)
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

	nc, err = nats.Connect("nats://"+ln.Addr().String(),
		nats.NoReconnect(), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the fake broker: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc, cue
}

// serveFake speaks the server half of the NATS text protocol, as far as a
// client needs it to stay connected and to receive one chunk of a fetch.
// It signals cue, without blocking, as fakeBroker's waiting describes.
func serveFake(conn net.Conn, confirm bool, cue chan<- struct{}) {
	defer func() { _ = conn.Close() }()
	signal := func() {
		select {
		case cue <- struct{}{}:
		default:
		}
	}
	port := conn.LocalAddr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"server_name\":\"fake\","+
		"\"version\":\"2.10.0\",\"proto\":1,\"host\":\"127.0.0.1\",\"port\":%d,"+
		"\"headers\":true,\"max_payload\":1048576}\r\n", port); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	handshake := true
	// sids maps each subject the client subscribed to onto its sid, so a
	// fetch's deliver subject can be routed to.
	sids := map[string]string{}
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
			if !handshake && !confirm {
				signal()
			} else if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
				return
			}
			handshake = false
		case "SUB":
			// SUB <subject> [queue] <sid>
			if len(fields) < 3 {
				return
			}
			sids[fields[1]] = fields[len(fields)-1]
		case "PUB", "HPUB":
			// THE PAYLOAD IS READ BY ITS DECLARED SIZE, never as lines,
			// or a payload holding a line reading PING would be answered.
			//
			// PUB <subject> [reply] <size>
			// HPUB <subject> [reply] <header size> <total size>
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil || len(fields) < 3 {
				return
			}
			headers := 0
			if strings.EqualFold(fields[0], "HPUB") {
				if headers, err = strconv.Atoi(fields[len(fields)-2]); err != nil ||
					headers > size {
					return
				}
			}
			payload := make([]byte, size+2)
			if _, err := io.ReadFull(r, payload); err != nil {
				return
			}
			switch subject := fields[1]; {
			case subject == fakeCreditSubject:
				signal()
			case strings.HasPrefix(subject, SubjectFetchPrefix):
				// The fetch's body is its deliver subject, subscribed
				// before the fetch was published.
				deliver := string(payload[headers:size])
				sid, ok := sids[deliver]
				if !ok {
					return
				}
				const chunk = "chunk"
				if _, err := fmt.Fprintf(conn, "MSG %s %s %s %d\r\n%s\r\n",
					deliver, sid, fakeCreditSubject, len(chunk), chunk); err != nil {
					return
				}
			}
		}
	}
}
