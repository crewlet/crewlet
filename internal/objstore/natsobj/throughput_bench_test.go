package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// BenchmarkALinksTransfers measures what a node without `data` pays for
// moving its transfers across its leaf link inside the link's one window: a
// download alone, an upload alone, and the two at once, through a leaf whose
// link to its member carries a given rate and round trip each way
// ([linktest.Distant]) — the floor every leg is promised, a fast distant link
// at 100 Mbit/s over round trips of 100 ms and 20 ms — and on loopback, as an
// in-cluster seat pod's leaf link is. It reports each transfer's MB/s.
//
// A window that moves one piece a round trip runs at a piece over the round
// trip on a fast distant link, whatever its rate: the price natsobj's package
// doc states, "What one window costs".
func BenchmarkALinksTransfers(b *testing.B) {
	floor := int64(objstore.MiB) * int64(time.Second) / int64(objstore.MiBPace)
	const fast = 100_000_000 / 8
	for _, l := range []struct {
		name string
		rate int64
		rtt  time.Duration
		size int
	}{
		{"floor", floor, 0, 512 << 10},
		{"100Mbit-rtt100ms", fast, 100 * time.Millisecond, 4 << 20},
		{"100Mbit-rtt20ms", fast, 20 * time.Millisecond, 8 << 20},
		{"loopback", 0, 0, 64 << 20},
	} {
		b.Run("link="+l.name, func(b *testing.B) {
			leaf, member := benchLeaf(b, l.rate, l.rtt)
			// RANDOM BYTES BOTH WAYS: a leaf's link compresses, and a
			// patterned object crosses it at a fraction of its size.
			down := make([]byte, l.size)
			if _, err := rand.Read(down); err != nil {
				b.Fatal(err)
			}
			if err := member.Put(b.Context(), "download", bytes.NewReader(down), objstore.PutMeta{}); err != nil {
				b.Fatal(err)
			}
			up := make([]byte, l.size)
			if _, err := rand.Read(up); err != nil {
				b.Fatal(err)
			}
			get := func(ctx context.Context) error {
				// THE DEADLINE A READ MUST CARRY, deciding nothing.
				ctx, cancel := context.WithTimeout(ctx, time.Hour)
				defer cancel()
				r, err := leaf.Get(ctx, "download", 0, -1)
				if err != nil {
					return err
				}
				defer r.Close()
				n, err := io.Copy(io.Discard, r)
				if err == nil && n != int64(l.size) {
					err = fmt.Errorf("read %d bytes of %d", n, l.size)
				}
				return err
			}
			put := func(ctx context.Context, i int) error {
				return leaf.Put(ctx, "upload-"+strconv.Itoa(i), bytes.NewReader(up), objstore.PutMeta{})
			}
			for _, op := range []string{"get", "put", "get+put"} {
				b.Run("op="+op, func(b *testing.B) {
					var took [2]time.Duration
					i := 0
					for b.Loop() {
						i++
						ctx := b.Context()
						var wg sync.WaitGroup
						errs := make([]error, 2)
						timed := func(k int, f func() error) {
							began := time.Now()
							errs[k] = f()
							took[k] += time.Since(began) //fixedwait:measure a benchmark's transfer time, which it reports and decides nothing by
						}
						if op != "put" {
							wg.Go(func() { timed(0, func() error { return get(ctx) }) })
						}
						if op != "get" {
							wg.Go(func() { timed(1, func() error { return put(ctx, i) }) })
						}
						wg.Wait()
						for _, err := range errs {
							if err != nil {
								b.Fatal(err)
							}
						}
					}
					for k, name := range []string{"get-MB/s", "put-MB/s"} {
						if took[k] > 0 {
							b.ReportMetric(float64(i*l.size)/took[k].Seconds()/1e6, name)
						}
					}
				})
			}
		})
	}
}

// benchLeaf is a bucket opened across a leaf's link, as the engine opens a
// node without `data`'s, and one on the member's own client: the link carries
// rate bytes a second each way with a round trip of rtt, or runs on loopback
// at rate zero.
func benchLeaf(b *testing.B, rate int64, rtt time.Duration) (*natsobj.Backend, *natsobj.Backend) {
	b.Helper()
	member, err := js.StartServer(b.Context(), js.Config{ServerName: "member", SyncAlways: true,
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(b),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: linkFleetAuth()})
	if err != nil {
		b.Fatalf("start the member: %v", err)
	}
	b.Cleanup(member.Shutdown)
	port := member.LeafPort()
	var link *linktest.Link
	if rate > 0 {
		link = linktest.Distant(b, fmt.Sprintf("127.0.0.1:%d", port), rate)
		var p int
		if _, err := fmt.Sscanf(link.Addr(), "127.0.0.1:%d", &p); err != nil {
			b.Fatal(err)
		}
		port = p
	}
	leafJS := client(b, startLeaf(b, port))
	awaitLink(b, leafJS)
	if link != nil {
		link.Lag(rtt / 2)
	}
	own := openWith(b, client(b, member), natsobj.Config{MaxBytes: benchCeiling, Replicas: 1})
	across := openWith(b, leafJS, natsobj.Config{MaxBytes: benchCeiling, Replicas: 1, AcrossLink: true,
		Window: jsapi.NewLinkWindow()})
	return across, own
}
