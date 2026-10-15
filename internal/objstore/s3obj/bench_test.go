package s3obj_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// BenchmarkBackend measures an S3 bucket through the suite every backend runs,
// at the production [s3obj.PartBytes], so a 16 MiB object goes up in two parts
// and a 128 MiB one in sixteen, as they would against a real bucket.
//
// AGAINST THE FAKE (`bucket=fake`) on loopback, at S3's own page of a
// thousand keys, every request counted:
//
//   - `scheme=https` is the NODE'S SIDE of a bucket — the SDK, the
//     signature, TLS and the store's own hashing, all on this machine's CPU —
//     and none of the provider's: no round trip, no per-prefix request rate,
//     no link.
//   - `scheme=http` is the same where the SDK hashes every body it sends to
//     sign it — what a MinIO reached without TLS costs.
//   - `scheme=https/rtt=…` is the node's side with a provider's ROUND TRIP
//     added to every request ([s3fake.Server.Latency]): 20 ms, a bucket in
//     the node's own region on a good day, and 50 ms, one under load or a
//     region away. The round trip is what bounds every request-at-a-time
//     loop — a multipart upload's parts, a listing's pages, an audit's
//     stats — and loopback hides it entirely. It charges no bandwidth.
//   - `scheme=https/bw=80MB/…/read=…` is the reads alone
//     ([objstoretest.BenchReads]) from a bucket that writes every GetObject
//     body — an object's bytes — at no more than 80 MB/s
//     ([s3fake.Server.ConnBytesPerSecond]): a provider's ceiling on one
//     connection, about what one S3 connection carries. Each is read three
//     ways: in ranges with the node's spare part buffers free (`read=wide`,
//     as wide as the reader keeps busy), in ranges with every spare already
//     taken (`read=narrow`, so the read streams range after range, the next
//     one's request on its way while the one before is read), and as ONE
//     GetObject a read (`read=stream`, how every read was made before a
//     large one was read in ranges). And each at loopback's first byte and
//     at a provider's 20 and 50 ms (`rtt=`), because a read in ranges pays a
//     first byte a range where one stream paid it once, and only a first
//     byte as long as a provider's shows whether the ranges' requests are
//     sent early enough to hide it. Side by side they are what reading a
//     large object in ranges buys against a provider that limits each
//     connection, which loopback's gigabytes a second hide.
//
// AGAINST A REAL BUCKET (`bucket=real`) only when CREWLET_BENCH_S3_BUCKET
// names one, configured by the variables the package doc lists; otherwise the
// arm skips, saying so under `go test -v` (which `make bench` passes). Every
// case puts its objects under a prefix of its own and [objstoretest.Clear]s
// it when the case ends, since a real bucket outlives the run.
func BenchmarkBackend(b *testing.B) {
	for _, arm := range []struct {
		name string
		tls  bool
		rtt  time.Duration
	}{
		{"bucket=fake/scheme=https", true, 0},
		{"bucket=fake/scheme=http", false, 0},
		{"bucket=fake/scheme=https/rtt=20ms", true, 20 * time.Millisecond},
		{"bucket=fake/scheme=https/rtt=50ms", true, 50 * time.Millisecond},
	} {
		b.Run(arm.name, func(b *testing.B) {
			objstoretest.Bench(b, func(b *testing.B) objstoretest.BenchBackend {
				var srv *s3fake.Server
				if arm.tls {
					srv = s3fake.StartTLS(b, "files")
					b.Setenv("AWS_CA_BUNDLE", trustBundle(b, srv))
				} else {
					srv = s3fake.Start(b, "files")
				}
				srv.PageSize = s3PageSize
				srv.Latency = arm.rtt
				return objstoretest.BenchBackend{Backend: openFake(b, srv, "crewlet/"),
					Requests: srv.Requests}
			})
		})
	}
	for _, rtt := range []time.Duration{0, 20 * time.Millisecond, 50 * time.Millisecond} {
		for _, read := range []string{"wide", "narrow", "stream"} {
			name := "bucket=fake/scheme=https/bw=80MB"
			if rtt > 0 {
				name += "/rtt=" + rtt.String()
			}
			b.Run(name+"/read="+read, func(b *testing.B) {
				objstoretest.BenchReads(b, func(b *testing.B) objstoretest.BenchBackend {
					srv := s3fake.StartTLS(b, "files")
					b.Setenv("AWS_CA_BUNDLE", trustBundle(b, srv))
					srv.PageSize = s3PageSize
					srv.Latency = rtt
					srv.ConnBytesPerSecond = connBytesPerSecond
					backend := openFake(b, srv, "crewlet/")
					switch read {
					case "narrow":
						b.Cleanup(s3obj.TakeSpares(backend))
					case "stream":
						return objstoretest.BenchBackend{Backend: s3obj.OneStream(backend), Requests: srv.Requests}
					}
					return objstoretest.BenchBackend{Backend: backend, Requests: srv.Requests}
				})
			})
		}
	}
	b.Run("bucket=real", func(b *testing.B) {
		cfg, ok := s3fake.RealBucket(b)
		if !ok {
			b.Skip("CREWLET_BENCH_S3_BUCKET names no bucket, so no real bucket is measured; " +
				"internal/objstore/s3obj's package doc lists the variables that name one")
		}
		objstoretest.Bench(b, func(b *testing.B) objstoretest.BenchBackend {
			// A PREFIX OF THE CASE'S OWN, so a case starts from an empty
			// store whatever an earlier one — or a run killed part way —
			// left, and its listing counts its own objects alone.
			own := cfg
			own.Prefix = cfg.Prefix + objstore.KeyAt(time.Now()).String() + "/"
			backend, err := s3obj.Open(b.Context(), own)
			if err != nil {
				b.Fatalf("open bucket %s: %v", own.Bucket, err)
			}
			b.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(b.Context()), clearBudget)
				defer cancel()
				if err := objstoretest.Clear(ctx, backend); err != nil {
					b.Errorf("clear %s in bucket %s: %v", own.Prefix, own.Bucket, err)
				}
			})
			return objstoretest.BenchBackend{Backend: backend}
		})
	})
}

// s3PageSize is how many keys one page of a listing holds: S3's own, the most
// ListObjectsV2 answers and what it answers when asked for no other number.
// The fake's default is a handful, so that a test's few dozen objects cross
// pages; a listing measured at that page would make about 140 times the
// requests S3 does.
const s3PageSize = 1000

// connBytesPerSecond is the most one connection carries in the `bw=` arms:
// 80 MB/s, the low end of the 80–90 MB/s one S3 connection carries — the
// figure S3's own guidance answers with byte-range fetches in parallel.
const connBytesPerSecond = 80_000_000

// clearBudget bounds clearing a real bucket's case prefix: the largest holds a
// hundred thousand objects, deleted 64 at a time — about half a minute at a
// 20 ms round trip — and ten minutes is a hang detector rather than a pace.
const clearBudget = 10 * time.Minute

// BenchmarkUploadBesideSlowReads measures a 128 MiB upload with downloads
// paced to a slow client reading large objects on the same node beside it
// (`slowreads=3`), and with none (`slowreads=0`), from a bucket a 20 ms round
// trip away sending each connection 80 MB/s.
//
// THE ONE ALLOWANCE, shared: an upload's parts beyond its first and a read's
// ranges read ahead draw on the same spare buffers ([s3obj.SpareParts]), and a
// read that held its full width whatever its reader's pace left an upload
// beside three slow downloads one part at a time. The two arms side by side
// are what a read as wide as its reader keeps for everybody else, and
// `spares/read` is how many buffers each slow download held once it was past
// its first range.
func BenchmarkUploadBesideSlowReads(b *testing.B) {
	for _, readers := range []int{0, slowReads} {
		b.Run(fmt.Sprintf("bucket=fake/scheme=https/bw=80MB/rtt=20ms/slowreads=%d", readers), func(b *testing.B) {
			srv := s3fake.StartTLS(b, "files")
			b.Setenv("AWS_CA_BUNDLE", trustBundle(b, srv))
			srv.Latency = 20 * time.Millisecond
			srv.ConnBytesPerSecond = connBytesPerSecond
			backend := openFake(b, srv, "crewlet/")
			body := seeded(128<<20, 13)

			ctx, stop := context.WithCancel(b.Context())
			var (
				going sync.WaitGroup
				past  atomic.Int64
			)
			b.Cleanup(func() {
				stop()
				going.Wait()
			})
			if readers > 0 {
				if err := backend.Put(ctx, "downloaded", bytes.NewReader(body[:slowReadBytes]),
					objstore.PutMeta{}); err != nil {
					b.Fatal(err)
				}
			}
			for range readers {
				going.Go(func() { slowRead(ctx, b, backend, &past) })
			}
			for past.Load() < int64(readers) {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			held := s3obj.SparesHeld(backend)

			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				if err := backend.Put(ctx, "uploaded", bytes.NewReader(body), objstore.PutMeta{}); err != nil {
					b.Fatal(err)
				}
			}
			if readers > 0 {
				b.ReportMetric(float64(held)/float64(readers), "spares/read")
			}
		})
	}
}

// BenchmarkAThrottledBurst measures what one throttle costs a burst: five
// hundred heads sent by thirty-two loops — a git node's sync of five hundred
// packs, near enough — to a bucket answering each 150 ms after it arrives, the
// middle of S3's own first byte, from a FRESH client, as at boot and as after
// its gate was lifted for a quiet minute.
//
//   - `throttle=none` throttles nothing, which is the burst's floor: sixteen
//     rounds of a first byte.
//   - `throttle=first` answers the burst's first request with a `SlowDown`,
//     which cuts the gate to seven tenths of the thirty-two out when it comes
//     back.
//   - `throttle=quiet` answers a lone head with a `SlowDown` BEFORE the burst,
//     the client having run one round of thirty-two and then gone quiet —
//     what a node between two syncs meets when a fleet sits at the prefix's
//     rate and the bucket throttles requests spread across it — so the cut
//     comes back with one attempt out of a node that runs thirty-two.
//   - `throttle=open` answers the client's check at open with a `SlowDown`:
//     a node throttled before it has had more than one attempt out, which
//     has no width to keep and climbs from one.
func BenchmarkAThrottledBurst(b *testing.B) {
	for _, throttle := range []string{"none", "first", "quiet", "open"} {
		b.Run("bucket=fake/rtt=150ms/throttle="+throttle, func(b *testing.B) {
			srv := s3fake.Start(b, "files")
			srv.Put("k", []byte("an object"))
			srv.Latency = burstFirstByte
			for b.Loop() {
				b.StopTimer()
				if throttle == "open" {
					srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
				}
				backend := openFake(b, srv, "")
				switch throttle {
				case "first":
					srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
				case "quiet":
					heads(b, backend, burstLoops)
					srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
					if _, err := backend.Stat(b.Context(), "k"); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				heads(b, backend, burstHeads)
			}
		})
	}
}

// heads sends n heads of one object through backend from burstLoops loops,
// each sending its next as its last is answered.
func heads(b *testing.B, backend *s3obj.Backend, n int64) {
	var (
		left  atomic.Int64
		loops sync.WaitGroup
	)
	left.Store(n)
	for range burstLoops {
		loops.Go(func() {
			for left.Add(-1) >= 0 {
				if _, err := backend.Stat(b.Context(), "k"); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	loops.Wait()
}

// burstHeads, burstLoops and burstFirstByte are BenchmarkAThrottledBurst's
// burst: five hundred heads by thirty-two loops, each answered 150 ms late.
const (
	burstHeads     = 500
	burstLoops     = 32
	burstFirstByte = 150 * time.Millisecond
)

// slowReads is how many slow downloads BenchmarkUploadBesideSlowReads runs
// beside its upload: three, the most that held the node's whole allowance
// when a read took its full width whatever its reader's pace — three reads of
// [s3obj.PartsInFlight] are [s3obj.SpareParts].
const slowReads = 3

// slowReadBytes is how large each slow download's object is, and
// slowReadRate how fast it reads it: four mebibytes a second, a person's
// link and a twentieth of what the bucket sends one connection, so a 64 MiB
// object lasts the benchmark at any benchtime that measures anything.
const (
	slowReadBytes = 64 << 20
	slowReadRate  = 4 << 20
)

// slowRead reads the downloaded object over and over at slowReadRate until
// ctx ends, counting itself in past once it has been handed its first range.
func slowRead(ctx context.Context, b *testing.B, backend *s3obj.Backend, past *atomic.Int64) {
	const step = 64 << 10
	buf := make([]byte, step)
	counted := false
	for ctx.Err() == nil {
		rctx, cancel := context.WithTimeout(ctx, objstore.ReadBudget(slowReadBytes))
		r, err := backend.Get(rctx, "downloaded", 0, -1)
		if err != nil {
			cancel()
			if ctx.Err() == nil {
				b.Error(err)
			}
			return
		}
		for read := 0; ctx.Err() == nil; read += step {
			if _, err := io.ReadFull(r, buf); err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					b.Error(err)
				}
				break
			}
			if !counted && read >= s3obj.PartBytes {
				counted = true
				past.Add(1)
			}
			time.Sleep(time.Second * step / slowReadRate)
		}
		_ = r.Close()
		cancel()
	}
}
