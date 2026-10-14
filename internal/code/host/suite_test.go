package host_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// TestTheHostPassesTheSuite runs the code host's suite over one data node with
// each object store backend: the in-memory twin, and S3 through an in-process
// bucket speaking its wire — the backend whose puts go in parts and whose
// reads go in ranges.
func TestTheHostPassesTheSuite(t *testing.T) {
	t.Run("memobj", func(t *testing.T) {
		codetest.Run(t, codetest.Solo(codetest.SoloOptions{}, func(*testing.T) objstore.Backend {
			return memobj.New()
		}))
	})
	t.Run("s3fake", func(t *testing.T) {
		codetest.Run(t, codetest.Solo(codetest.SoloOptions{}, s3Backend))
	})
}

// s3Backend is a fresh in-process S3 bucket, through the production backend.
func s3Backend(t *testing.T) objstore.Backend {
	t.Helper()
	srv := s3fake.Start(t, "files")
	b, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		t.Fatalf("opening the S3 fake: %v", err)
	}
	return b
}
