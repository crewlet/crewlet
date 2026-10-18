package artifactspike

import (
	"os"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
)

// SPIKE S3: a 64 MiB publish streamed out of a sandbox through the engine's
// transfer leg ([artifactupload.Service.Transfer]) into the object store.
//
// It reports the wall time from the intent to the last byte stored, the rate,
// how much of the object store's own pace allowance ([objstore.PaceFor]) that
// was, how far the heap rose while it ran, and where it stood once the
// broker's caches had expired. The heap is the PROCESS's, and the embedded
// broker's members run in it as they do in a node (the default deployment):
// on the three-member arm it is three members' worth.
//
// The `nats` arms run on the embedded broker (one member, then three). The
// `s3` arm runs against a real S3 endpoint named in the environment, because
// an in-process fake measures the fake: start the reference SeaweedFS
// (`docker compose --profile s3 up -d --wait`, then create the bucket) and set
// CREWLET_SPIKE_S3_ENDPOINT, CREWLET_SPIKE_S3_BUCKET and, where the store
// checks them, CREWLET_SPIKE_S3_ACCESS_KEY_ID and
// CREWLET_SPIKE_S3_SECRET_ACCESS_KEY. Its objects are left under the
// `crewlet-spike/` prefix.
//
// Run: go test -run '^$' -bench S3 -benchtime 1x ./internal/artifactspike

// publishBucketBytes is the nats bucket's reservation in the byte-leg spikes:
// two publishes at three copies of headroom, inside memberStoreBytes beside
// the datastore log.
const publishBucketBytes = 320 << 20

// BenchmarkS3StreamPublish streams one 64 MiB publish into each backend.
func BenchmarkS3StreamPublish(b *testing.B) {
	b.Run("backend=nats/topo=solo", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			br := startBrokers(b, topoSolo)
			streamOnce(b, openBucket(b, br.queues[0], 1, publishBucketBytes))
		}
	})
	b.Run("backend=nats/topo=fleet3", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			br := startBrokers(b, topoFleet)
			streamOnce(b, openBucket(b, br.queues[0], len(br.queues), publishBucketBytes))
		}
	})
	endpoint, bucket := os.Getenv("CREWLET_SPIKE_S3_ENDPOINT"), os.Getenv("CREWLET_SPIKE_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		b.Log("the s3 arm runs against a real endpoint: set CREWLET_SPIKE_S3_ENDPOINT and " +
			"CREWLET_SPIKE_S3_BUCKET (docs/reference/artifact-capacity.md, S3)")
		return
	}
	b.Run("backend=s3", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			region := os.Getenv("CREWLET_SPIKE_S3_REGION")
			if region == "" {
				region = "us-east-1"
			}
			backend, err := s3obj.Open(b.Context(), s3obj.Config{
				Endpoint: endpoint, Region: region, Bucket: bucket, Prefix: "crewlet-spike/",
				PathStyle:       true,
				AccessKeyID:     os.Getenv("CREWLET_SPIKE_S3_ACCESS_KEY_ID"),
				SecretAccessKey: os.Getenv("CREWLET_SPIKE_S3_SECRET_ACCESS_KEY"),
			})
			if err != nil {
				b.Fatalf("open the s3 store: %v", err)
			}
			streamOnce(b, backend)
		}
	})
}

// streamOnce streams one publish into backend and reports it.
func streamOnce(b *testing.B, backend objstore.Backend) {
	b.Helper()
	rig := newUploadRig(b, backend)
	heap := watchHeap(5 * time.Millisecond)
	b.StartTimer()
	elapsed, err := rig.streamPublish(b, 0)
	b.StopTimer()
	if err != nil {
		b.Fatalf("the publish: %v", err)
	}
	growth := heap.growth()
	settled := heap.settled(heapSettle)
	b.ReportMetric(elapsed.Seconds(), "publish-s")
	b.ReportMetric(float64(publishBytes)/mib/elapsed.Seconds(), "MiB/s")
	b.ReportMetric(elapsed.Seconds()/objstore.PaceFor(publishBytes).Seconds(), "share-of-pace")
	b.ReportMetric(float64(growth)/mib, "heap-growth-MiB")
	b.ReportMetric(float64(settled)/mib, "heap-settled-MiB")
}

// heapSettle is how long a spike waits before reading what a publish left on
// the heap: past the broker's message block cache expiry (a few seconds in
// nats-server's file store), so what remains is held rather than cached.
const heapSettle = 15 * time.Second
