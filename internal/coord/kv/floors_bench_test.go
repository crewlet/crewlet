package kv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// floorDomains are the five domains a node registers, each with a floor row:
// what a reader that names every domain at once ([readFloorsOf] in the engine)
// asks about.
var floorDomains = []string{"tracker", "pages", "code", "vectors", "usage"}

// BenchmarkAFloorRead measures one read of the published trim floors in three
// shapes: key (ONE domain's floor, the write fence's read on every write at an
// expectation of zero); five-keys (every registered domain's key at once, one
// goroutine each, as the heartbeat, readiness verdict and trim read); listing
// (the floor class listed whole, through the register's walk).
//
// It runs on a member (client in the broker's process) and a leaf (across its
// link), and reports ms-p50, ms-p99 and api-requests/op counted off `$JS.>`,
// the cost an idle broker hides: a listing's consumer create and delete are
// each a metadata-group proposal on a clustered broker, where a key read is
// one request to the stream's leader. A read that fails fails the benchmark.
func BenchmarkAFloorRead(b *testing.B) {
	for _, topology := range []struct {
		name   string
		reach  Reach
		client func(b *testing.B) jetstream.JetStream
	}{
		{"member", InProcess, func(b *testing.B) jetstream.JetStream {
			_, nc := embeddedServer(b)
			return jsOf(nc)
		}},
		{"leaf", AcrossLink, func(b *testing.B) jetstream.JetStream {
			return leafClientOf(b, leafMember(b), "leaf")
		}},
	} {
		for _, shape := range []string{"key", "five-keys", "listing"} {
			b.Run(topology.name+"/"+shape, func(b *testing.B) {
				benchmarkFloorRead(b, topology.client(b), topology.reach, shape)
			})
		}
	}
}

func benchmarkFloorRead(b *testing.B, client jetstream.JetStream, reach Reach, shape string) {
	ctx := context.Background()
	fleet := openFleetVia(b, client, reach, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	for _, domain := range floorDomains {
		if err := fleet.PutFloor(ctx, coord.TrimFloor{
			Domain: domain, Generation: 1, TrimTo: 40, Floor: 40,
			At: time.Now().UTC(), By: "node-a",
		}); err != nil {
			b.Fatalf("PutFloor(%s): %v", domain, err)
		}
	}
	// The register's other classes, so a listing filters rather than walks a
	// bucket of floors alone.
	for i := range 5 {
		if err := fleet.PutPositions(ctx, coord.NodePositions{
			NodeID: fmt.Sprintf("node-%d", i), At: time.Now().UTC(),
		}); err != nil {
			b.Fatalf("PutPositions: %v", err)
		}
	}
	read := floorReadOf(fleet, shape)
	if err := read(ctx); err != nil {
		b.Fatalf("the first %s read: %v", shape, err)
	}

	tap := apiTap(b, client.Conn())
	took := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for range b.N {
		start := time.Now()
		if err := read(ctx); err != nil {
			b.Fatalf("a %s read: %v", shape, err)
		}
		took = append(took, time.Since(start))
	}
	b.StopTimer()
	requests, consumers := tap()
	slices.Sort(took)
	at := func(q float64) time.Duration { return took[int(q*float64(len(took)-1))] }
	b.ReportMetric(float64(at(0.5).Microseconds())/1000, "ms-p50")
	b.ReportMetric(float64(at(0.99).Microseconds())/1000, "ms-p99")
	b.ReportMetric(float64(requests)/float64(b.N), "api-requests/op")
	b.ReportMetric(float64(consumers)/float64(b.N), "consumer-creates+deletes/op")
}

// floorReadOf is one read of the floors in the named shape.
func floorReadOf(fleet *FleetStore, shape string) func(context.Context) error {
	switch shape {
	case "key":
		return func(ctx context.Context) error {
			_, found, err := fleet.Floor(ctx, "tracker")
			if err == nil && !found {
				err = errors.New("the tracker's floor read as absent")
			}
			return err
		}
	case "five-keys":
		return func(ctx context.Context) error {
			var (
				wg   sync.WaitGroup
				errs = make([]error, len(floorDomains))
			)
			for i, domain := range floorDomains {
				wg.Go(func() { _, _, errs[i] = fleet.Floor(ctx, domain) })
			}
			wg.Wait()
			return errors.Join(errs...)
		}
	default:
		return func(ctx context.Context) error {
			n := 0
			err := fleet.eachPositionKey(ctx, coord.FloorClass, "the trim floors",
				func(string, []byte, time.Time) error { n++; return nil })
			if err == nil && n != len(floorDomains) {
				err = fmt.Errorf("the listing named %d floors, want %d", n, len(floorDomains))
			}
			return err
		}
	}
}

// apiTap counts every JetStream API request nc sends from now on, and the
// consumer creates and deletes among them (metadata-group proposals on a
// clustered broker). A connection receives what it publishes, so a `$JS.>`
// subscription sees each request, and a flush queues every one already sent
// before the count is read.
func apiTap(b *testing.B, nc *nats.Conn) func() (requests, consumers int) {
	b.Helper()
	sub, err := nc.SubscribeSync("$JS.>")
	if err != nil {
		b.Fatalf("subscribe the tap: %v", err)
	}
	if err := sub.SetPendingLimits(-1, -1); err != nil {
		b.Fatalf("lift the tap's limits: %v", err)
	}
	if err := nc.Flush(); err != nil {
		b.Fatalf("flush the tap's subscription: %v", err)
	}
	return func() (requests, consumers int) {
		defer func() { _ = sub.Unsubscribe() }()
		if err := nc.Flush(); err != nil {
			b.Fatalf("flush the tap: %v", err)
		}
		for {
			msg, err := sub.NextMsg(100 * time.Millisecond)
			if errors.Is(err, nats.ErrTimeout) {
				return requests, consumers
			}
			if err != nil {
				b.Fatalf("read the tap: %v", err)
			}
			verb, ok := apiRequest(msg.Subject)
			if !ok {
				continue
			}
			requests++
			if strings.HasPrefix(verb, "CONSUMER.CREATE.") ||
				strings.HasPrefix(verb, "CONSUMER.DURABLE.CREATE.") ||
				strings.HasPrefix(verb, "CONSUMER.DELETE.") {
				consumers++
			}
		}
	}
}

// apiRequest is the verb of a JetStream API request subject — what follows
// `$JS.API.` on the account's own API, or `$JS.<domain>.API.` on a domain's —
// and false for anything else under the prefix, an acknowledgement or an
// advisory.
func apiRequest(subject string) (string, bool) {
	tokens := strings.Split(subject, ".")
	switch {
	case len(tokens) > 2 && tokens[0] == "$JS" && tokens[1] == "API":
		return strings.Join(tokens[2:], "."), true
	case len(tokens) > 3 && tokens[0] == "$JS" && tokens[2] == "API" && tokens[1] != "EVENT":
		return strings.Join(tokens[3:], "."), true
	}
	return "", false
}
