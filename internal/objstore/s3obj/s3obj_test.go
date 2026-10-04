package s3obj_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

func open(t *testing.T, srv *s3fake.Server, prefix string) *s3obj.Backend {
	t.Helper()
	b, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: prefix,
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return b
}

// AN S3 BUCKET PASSES THE SUITE, over the wire the SDK speaks, with listings
// that cross page boundaries. LastModified is to the second, so a re-put is
// asked about a second later.
func TestContract(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, s3fake.Start(t, "files"), "crewlet/")
	}, objstoretest.Options{Granularity: time.Second})
}

// A BUCKET THAT CANNOT BE REACHED FAILS THE BOOT, not the first upload.
func TestAnUnreachableBucketIsRefusedAtOpen(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	_, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "somebody-elses", PathStyle: true,
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err == nil {
		t.Fatal("a bucket the endpoint does not have was opened")
	}
	for name, cfg := range map[string]s3obj.Config{
		"no bucket":  {Region: "us-east-1"},
		"no region":  {Bucket: "files"},
		"half a key": {Bucket: "files", Region: "us-east-1", AccessKeyID: "AKIAEXAMPLE"},
	} {
		if _, err := s3obj.Open(t.Context(), cfg); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// THE PREFIX IS THE COMPANY'S CORNER OF A SHARED BUCKET: every key is under
// it, and a listing never hands the collector a key outside it or one that is
// not a chunk — either would be deleted as garbage.
func TestTheBackendKeepsToItsPrefix(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	ours, theirs := open(t, srv, "acme/"), open(t, srv, "other/")
	data := []byte("a chunk")
	h := objstore.HashOf(data)
	if err := ours.Put(t.Context(), h, data); err != nil {
		t.Fatal(err)
	}
	if err := theirs.Put(t.Context(), objstore.HashOf([]byte("theirs")), []byte("theirs")); err != nil {
		t.Fatal(err)
	}
	if err := ours.Put(t.Context(), objstore.HashOf([]byte("x")), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(srv.Keys(), "acme/"+string(h)) {
		t.Fatalf("the chunk is not under the prefix: %v", srv.Keys())
	}
	var listed []objstore.Hash
	if err := ours.List(t.Context(), func(held objstore.Held) error {
		listed = append(listed, held.Hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || !slices.Contains(listed, h) {
		t.Fatalf("the prefix listed %v", listed)
	}
}
