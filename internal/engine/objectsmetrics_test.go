package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// s3Bootstrap is a node's Tier A for a case whose object store is a bucket on
// srv: path style, with STATIC keys — with none, the SDK's own chain would
// look for credentials on the machine, the instance's metadata endpoint among
// them, before its first request — and the embedded stream in the case's own
// directory.
func s3Bootstrap(t *testing.T, srv *s3fake.Server) *config.Bootstrap {
	t.Helper()
	return bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Store.Objects = config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
			Endpoint: srv.URL, Region: "us-east-1", Bucket: srv.Bucket(), PathStyle: true,
			AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
		}}
	})
}

// answeredBucketChecks is how many attempts of the bucket's check at open rec
// holds as answered: `crewlet.objects.s3.attempts{operation=HeadBucket,
// outcome=answered}`, zero where the series is absent.
func answeredBucketChecks(rec *metrics.Recorder) uint64 {
	for _, s := range rec.Read() {
		if s.Name == metrics.ObjectsS3Attempts && s.Attrs["operation"] == "HeadBucket" &&
			s.Attrs["outcome"] == "answered" {
			return s.Total
		}
	}
	return 0
}

// THE NODE'S RECORDER REACHES ITS BUCKET, from the engine's own constructor
// down: engine.New hands Options.Metrics to OpenBackends, OpenBackends to the
// broker's opening, and that to the object store's — and an `s3` node's
// check of its bucket at open is the first attempt it counts. A recorder
// dropped at any hop is a node publishing none of the four S3 series, which
// reads exactly as a bucket that never throttled or failed anybody; the one
// test of the last hop alone (TestTheObjectStoresBucketCountsIntoTheNodesRecorder)
// passes whatever the hops above it pass.
func TestTheNodesRecorderReachesItsBucket(t *testing.T) {
	t.Parallel()
	t.Run("through OpenBackends", func(t *testing.T) {
		t.Parallel()
		rec, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}
		back, err := engine.OpenBackends(t.Context(), s3Bootstrap(t, s3fake.Start(t, "files")),
			parsedCompany(t, companyDoc), rec)
		if err != nil {
			t.Fatalf("OpenBackends: %v", err)
		}
		t.Cleanup(func() { back.Close(context.Background()) })
		if n := answeredBucketChecks(rec); n != 1 {
			t.Fatalf("the recorder OpenBackends was handed holds %d answered checks of the bucket, want "+
				"the 1 the object store's opening made: %v", n, rec.Read())
		}
	})
	t.Run("through engine.New", func(t *testing.T) {
		t.Parallel()
		rec, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.New(t.Context(), engine.Options{
			Bootstrap: s3Bootstrap(t, s3fake.Start(t, "files")),
			Company:   parsedCompany(t, companyDoc),
			Metrics:   rec,
		})
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		t.Cleanup(func() { e.Stop(context.Background()) })
		if n := answeredBucketChecks(rec); n != 1 {
			t.Fatalf("the engine's recorder holds %d answered checks of the bucket, want the 1 its "+
				"object store's opening made: %v", n, rec.Read())
		}
	})
}
