package s3obj_test

import (
	"context"
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

// clearBudget bounds clearing a real bucket's case prefix: the largest holds a
// hundred thousand objects, deleted 64 at a time — about half a minute at a
// 20 ms round trip — and ten minutes is a hang detector rather than a pace.
const clearBudget = 10 * time.Minute
