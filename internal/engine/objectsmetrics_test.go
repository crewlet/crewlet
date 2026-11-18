package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// s3Bootstrap is a node's Tier A whose object store is a bucket on srv, path
// style, with the embedded stream in the case's own directory. The keys are
// STATIC: without them the SDK's chain probes the machine for credentials
// (the instance metadata endpoint among them) before its first request.
func s3Bootstrap(t *testing.T, srv *s3fake.Server) *config.Bootstrap {
	t.Helper()
	return bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Store.Objects = config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
			Endpoint: srv.URL, Region: "us-east-1", Bucket: srv.Bucket(), PathStyle: true,
			AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
		}}
	})
}

// answeredProbePuts is rec's count of answered PutObject attempts
// (`crewlet.objects.s3.attempts{operation=PutObject,outcome=answered}`), zero
// where absent. Opening a node puts no object but the bucket probe's (s3obj's
// probe.go), so this counts that probe.
func answeredProbePuts(rec *metrics.Recorder) uint64 {
	for _, s := range rec.Read() {
		if s.Name == metrics.ObjectsS3Attempts && s.Attrs["operation"] == "PutObject" &&
			s.Attrs["outcome"] == "answered" {
			return s.Total
		}
	}
	return 0
}

// THE NODE'S RECORDER REACHES ITS BUCKET through every hop: engine.New hands
// Options.Metrics to OpenBackends, which hands it to the broker's opening and
// on to the object store's, whose bucket probe is the first attempt counted. A
// recorder dropped at any hop publishes none of the four S3 series, which
// reads as a bucket that never failed anybody;
// TestTheObjectStoresBucketCountsIntoTheNodesRecorder covers the last hop only.
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
		if n := answeredProbePuts(rec); n != 1 {
			t.Fatalf("the recorder OpenBackends was handed holds %d answered probe puts, want "+
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
		if n := answeredProbePuts(rec); n != 1 {
			t.Fatalf("the engine's recorder holds %d answered probe puts, want the 1 its "+
				"object store's opening made: %v", n, rec.Read())
		}
	})
}
