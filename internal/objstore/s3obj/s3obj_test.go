package s3obj_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"testing/iotest"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
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
// that cross page boundaries and objects long enough to go up in parts.
// LastModified is to the second, so a re-put is asked about a second later.
func TestContract(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, s3fake.Start(t, "files"), "crewlet/")
	}, objstoretest.Options{Granularity: time.Second, Piece: s3obj.PartBytes})
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

// THE PREFIX IS THE COMPANY'S CORNER OF A SHARED BUCKET: every key is put
// under it, and a listing never hands the collector a key outside it.
func TestTheBackendKeepsToItsPrefix(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	ours, theirs := open(t, srv, "acme/"), open(t, srv, "other/")
	for _, name := range []string{"one", "two"} {
		if err := ours.Put(t.Context(), name, bytes.NewReader([]byte(name)), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := theirs.Put(t.Context(), "theirs", bytes.NewReader([]byte("x")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/one", "acme/two", "other/theirs"}; !slices.Equal(srv.Keys(), want) {
		t.Fatalf("the bucket holds %v, want %v", srv.Keys(), want)
	}
	var listed []string
	if err := ours.List(t.Context(), func(info objstore.Info) error {
		listed = append(listed, info.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two"}; !slices.Equal(listed, want) {
		t.Fatalf("the prefix listed %v, want %v", listed, want)
	}
}

// AN OBJECT LONGER THAN A PART GOES UP AS A MULTIPART UPLOAD of equal parts,
// and one that fits a part as a single put — the fake counts which, so a
// backend that sent everything as one request would fail here rather than
// pass every case without uploading a part.
func TestAnObjectLongerThanAPartGoesUpInParts(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	long := bytes.Repeat([]byte("in parts "), (2*s3obj.PartBytes+17)/9+1)
	if err := b.Put(t.Context(), "long", iotest.HalfReader(bytes.NewReader(long)), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("long"); got != 3 {
		t.Fatalf("%d bytes went up in %d parts, want 3 of at most %d", len(long), got, s3obj.PartBytes)
	}
	if err := b.Put(t.Context(), "short", bytes.NewReader([]byte("one request")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("short"); got != 0 {
		t.Fatalf("an object under a part went up in %d parts, want one put", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "long", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, long) {
		t.Fatalf("the multipart object read back %d bytes, %v", len(got), err)
	}
	if len(srv.Uploads()) != 0 {
		t.Fatalf("a completed upload is still open: %v", srv.Uploads())
	}
}

// AN UPLOAD THAT FAILS PART WAY IS ABANDONED — whether the bucket refused a
// part or the body stopped arriving — because an upload nobody completes or
// aborts is billed for its parts and shown by no listing. A body CUT SHORT is
// one that stopped, at a part's end or inside one: read as the end, it was
// completed as an object of the parts that had arrived.
func TestAFailedUploadIsAbandoned(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	body := bytes.Repeat([]byte{7}, 3*s3obj.PartBytes)

	srv.FailPart(2)
	if err := b.Put(t.Context(), "refused", bytes.NewReader(body), objstore.PutMeta{}); err == nil {
		t.Fatal("an upload whose second part was refused succeeded")
	}
	srv.FailPart(0)

	for _, stop := range []struct {
		name string
		at   int
		err  error
	}{
		{"stopped", 3 * s3obj.PartBytes / 2, errors.New("the client went away")},
		{"cut-at-a-part", s3obj.PartBytes, io.ErrUnexpectedEOF},
		{"cut-in-a-part", 3 * s3obj.PartBytes / 2, io.ErrUnexpectedEOF},
	} {
		stopped := io.MultiReader(bytes.NewReader(body[:stop.at]), iotest.ErrReader(stop.err))
		if err := b.Put(t.Context(), stop.name, stopped, objstore.PutMeta{}); !errors.Is(err, stop.err) {
			t.Fatalf("%s: an upload whose body stopped = %v, want the body's error", stop.name, err)
		}
	}
	if open := srv.Uploads(); len(open) != 0 {
		t.Fatalf("failed uploads were left open: %v", open)
	}
	if len(srv.Keys()) != 0 {
		t.Fatalf("failed uploads left objects: %v", srv.Keys())
	}
}

// THE MEDIA TYPE IS STORED WITH THE OBJECT, whichever way it went up, so an
// operator reading the bucket with their own tools sees what the bytes are.
func TestTheContentTypeIsStored(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "acme/")
	client := httpxtest.Pool(t)
	for name, size := range map[string]int{"small": 10, "large": s3obj.PartBytes + 1} {
		if err := b.Put(t.Context(), name, bytes.NewReader(make([]byte, size)),
			objstore.PutMeta{ContentType: "text/csv"}); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/files/acme/"+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "text/csv" {
			t.Errorf("%s is served as %q, want the type it was put with", name, got)
		}
	}
}

// AN OBJECT PUT IN PARTS IS DATED BY ITS UPLOAD'S START, as Amazon dates one —
// which is why objstore.Info's Written is read as the earliest the object
// might have been stored rather than as when it was finished. The body holds
// its last byte back until the clock has crossed into a later second than the
// upload's start, so a backend or a fake dating the completion reports that
// later second and fails here — and a fake that did would let the collector's
// tests meet a gentler S3 than Amazon's.
func TestAnObjectPutInPartsIsDatedByItsUploadsStart(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	before := time.Now().UTC().Truncate(time.Second)
	body := &secondCrossingReader{r: bytes.NewReader(make([]byte, s3obj.PartBytes+1)), at: s3obj.PartBytes}
	if err := b.Put(t.Context(), "long", body, objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("long"); got != 2 {
		t.Fatalf("%d bytes went up in %d parts, want 2: this case is about an object put in parts",
			s3obj.PartBytes+1, got)
	}
	if body.resumed.IsZero() {
		t.Fatal("the upload never asked for the bytes past its first part")
	}
	info, err := b.Stat(t.Context(), "long")
	if err != nil {
		t.Fatal(err)
	}
	if info.Written.Before(before) || !info.Written.Before(body.resumed) {
		t.Fatalf("an object whose upload began in [%v, %v) and finished at or after %v is "+
			"dated %v, want its upload's start", before, body.resumed, body.resumed, info.Written)
	}
	var listed []objstore.Info
	if err := b.List(t.Context(), func(info objstore.Info) error {
		listed = append(listed, info)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Written.Equal(info.Written) {
		t.Fatalf("the listing says %+v, want the dating Stat gave: %v", listed, info.Written)
	}
}

// secondCrossingReader reads r, and on the first read at offset at — once the
// upload holding everything before it has begun — waits for the clock to
// reach a later second than the one it was asked in, recording that second.
type secondCrossingReader struct {
	r       io.Reader
	at      int64
	read    int64
	resumed time.Time
}

func (s *secondCrossingReader) Read(p []byte) (int, error) {
	if s.read < s.at {
		p = p[:min(int64(len(p)), s.at-s.read)]
	} else if s.resumed.IsZero() {
		next := time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)
		time.Sleep(time.Until(next))
		s.resumed = time.Now().UTC().Truncate(time.Second)
	}
	n, err := s.r.Read(p)
	s.read += int64(n)
	return n, err
}
