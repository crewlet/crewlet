package backup

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// modelObjects is the large model's object count, which the benchmark's
// projection of a first copy is taken over.
const modelObjects = 2_100_000

// modelMix is the large model's objects by size — about 2.1 million holding
// about 1.15 TB, a mean near 550 KB — as a mix whose mean is that: mostly
// small files, a long tail of large ones. A share in hundredths of the objects
// of each size.
var modelMix = []struct {
	size  int
	share int
}{
	{16 << 10, 62},
	{512 << 10, 28},
	{2 << 20, 9},
	{20 << 20, 1},
}

// BenchmarkCarry measures what the bound on a backup's reads of an S3 bucket
// ([backupFetchConcurrency]) buys a FIRST copy: objects and bytes a second
// carried into a backup through the real path — the S3 backend over the fake
// bucket, the store's verifier, the write and fsync of each object — at the
// production bound, at half of it and at twice it, over the model's mix of
// sizes, with the fake answering every request a provider's first byte away
// ([s3fake.Server.Latency]): none, which is this host's own ceiling; 20 ms, a
// bucket in the node's region on a good day; and 100 ms, one under load.
//
// WHAT IT MEASURES is the carry's depth, which is what the bound decides: on
// loopback a body moves at memory speed once its first byte arrives, so the
// rate here is the latency-bound one until the host's own work on each object
// bounds it instead, and a real link's bandwidth only lowers it further.
// `model-first-copy-h` projects the large model's 2.1 million objects at the
// measured rate — a floor on a first copy's length, which a slower link
// stretches, and so on how long the trim hold it spans lasts.
//
//	go test -run '^$' -bench BenchmarkCarry -benchtime 3x ./internal/backup/
func BenchmarkCarry(b *testing.B) {
	const objects = 200
	var sizes []int
	for _, m := range modelMix {
		for range objects * m.share / 100 {
			sizes = append(sizes, m.size)
		}
	}
	// INTERLEAVED, deterministically, so the large objects are not all out
	// at once at the end of a run.
	rng := rand.New(rand.NewPCG(1, 2))
	rng.Shuffle(len(sizes), func(i, j int) { sizes[i], sizes[j] = sizes[j], sizes[i] })
	body := make([]byte, modelMix[len(modelMix)-1].size)
	for i := range body {
		body[i] = byte(rng.Uint32())
	}

	for _, firstByte := range []time.Duration{0, 20 * time.Millisecond, 100 * time.Millisecond} {
		for _, bound := range []int{backupFetchConcurrency / 2, backupFetchConcurrency, 2 * backupFetchConcurrency} {
			b.Run(fmt.Sprintf("first_byte=%s/bound=%d", firstByte, bound), func(b *testing.B) {
				srv := s3fake.Start(b, "files")
				backend, err := s3obj.Open(b.Context(), s3obj.Config{
					Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "crewlet/",
					PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
				})
				if err != nil {
					b.Fatal(err)
				}
				store, err := objstore.NewStore(backend)
				if err != nil {
					b.Fatal(err)
				}
				refs := make([]reference, 0, len(sizes))
				var total int64
				for i, n := range sizes {
					o, err := store.Put(b.Context(), bytes.NewReader(body[:n]), int64(n), objstore.PutMeta{})
					if err != nil {
						b.Fatal(err)
					}
					refs = append(refs, reference{Object: o, NamedBy: fmt.Sprintf("ENG/%04d.bin", i)})
					total += int64(n)
				}
				// THE ROUND TRIP FROM HERE ON, so the seeding above is
				// not slowed by it.
				srv.Latency = firstByte
				base := b.TempDir()
				run := 0
				for b.Loop() {
					run++
					p := &objectPass{objs: &Objects{Open: store.Open},
						root: filepath.Join(base, strconv.Itoa(run))}
					next := 0
					err := bounded(b.Context(), bound, func(context.Context) (reference, bool, error) {
						if next == len(refs) {
							return reference{}, false, nil
						}
						next++
						return refs[next-1], true, nil
					}, func(ctx context.Context, ref reference) error { return p.place(ctx, ref, false) })
					if err != nil || p.art == nil || p.art.Objects != len(refs) {
						b.Fatalf("the carry = %+v, %v; want every object carried", p.art, err)
					}
					if err := os.RemoveAll(p.root); err != nil {
						b.Fatal(err)
					}
				}
				rate := float64(len(refs)*b.N) / b.Elapsed().Seconds()
				b.ReportMetric(rate, "objects/s")
				b.ReportMetric(float64(total)*float64(b.N)/1e6/b.Elapsed().Seconds(), "MB/s")
				b.ReportMetric(modelObjects/rate/3600, "model-first-copy-h")
			})
		}
	}
}
