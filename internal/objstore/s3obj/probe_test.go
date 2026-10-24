package s3obj_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A CREDENTIAL SCOPED TO ONE PREFIX OPENS THE BUCKET: the probe asks nothing
// about the bucket itself, which is what lets companies share one bucket
// without each holding a credential that reaches the others' files.
func TestACredentialScopedToItsPrefixOpensTheBucket(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.AllowOnly("acme/")
	b := open(t, srv, "acme/")
	if ok, why := b.DirectUpload(); !ok {
		t.Fatalf("a provider enforcing both bindings offers no direct upload: %s", why)
	}
	if err := b.Put(t.Context(), "files/one", bytes.NewReader([]byte("one")), objstore.PutMeta{}); err != nil {
		t.Fatalf("a put under the prefix: %v", err)
	}
	_, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "other/",
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err == nil {
		t.Fatal("a node whose credential cannot write under its prefix opened the bucket")
	}
}

// THE PROBE LEAVES NOTHING BEHIND, on a provider that enforces everything
// and on one that enforces nothing.
func TestTheProbeLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	for name, lax := range map[string]bool{"strict": false, "lax": true} {
		srv := s3fake.Start(t, "files")
		if lax {
			srv.IgnoreChecksums()
			srv.IgnoreConditionalPuts()
		}
		open(t, srv, "acme/")
		if keys := srv.Keys(); len(keys) != 0 {
			t.Errorf("%s: the probe left %v in the bucket", name, keys)
		}
	}
}

// A BACKEND THAT IGNORES CHECKSUMS GETS NO DIRECT UPLOADS, and neither does
// one that ignores the condition: either would let a client swap the bytes
// after the engine verified them.
func TestABackendThatIgnoresChecksumsGetsNoDirectUploads(t *testing.T) {
	t.Parallel()
	for name, cripple := range map[string]func(*s3fake.Server){
		"x-amz-checksum-sha256": (*s3fake.Server).IgnoreChecksums,
		"If-None-Match":         (*s3fake.Server).IgnoreConditionalPuts,
	} {
		srv := s3fake.Start(t, "files")
		cripple(srv)
		b := open(t, srv, "")
		ok, why := b.DirectUpload()
		if ok || !strings.Contains(why, name) {
			t.Errorf("a provider ignoring %s offers direct upload %v (%q)", name, ok, why)
		}
		store, err := objstore.NewStore(b)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.PresignPut(t.Context(), store.Mint(), presignOf([]byte("x")))
		if !errors.Is(err, objstore.ErrDirectUnavailable) {
			t.Errorf("a provider ignoring %s signed a put: %v", name, err)
		}
	}
}

func presignOf(body []byte) objstore.PresignRequest {
	return objstore.PresignRequest{Size: int64(len(body)), ContentType: "text/plain",
		SHA256: objstore.HashOf(body), Expires: 5 * time.Minute}
}

// putSigned makes a presigned put of body the way a client does.
func putSigned(t *testing.T, p objstore.Presigned, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		if k != "Content-Length" {
			req.Header.Set(k, v)
		}
	}
	// A POOL OF THIS TEST'S OWN, for [httpxtest]'s reason: every
	// httptest.Server.Close in the binary sweeps the process-global pool.
	resp, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A SECOND PUT TO A PRESIGNED URL IS REFUSED, and so is a put of other bytes:
// the URL stores exactly the declared bytes once.
func TestASecondPutToAPresignedURLIsRefused(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	store, err := objstore.NewStore(open(t, srv, "acme/"))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("the bytes the engine agreed to")
	k := store.Mint()
	signed, err := store.PresignPut(t.Context(), k, presignOf(body))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if got := putSigned(t, signed, []byte("other bytes of the same len!!")); got != http.StatusBadRequest {
		t.Fatalf("a put of other bytes answered %d, want 400", got)
	}
	if got := putSigned(t, signed, body); got != http.StatusOK {
		t.Fatalf("the agreed put answered %d", got)
	}
	if got := putSigned(t, signed, body); got != http.StatusPreconditionFailed {
		t.Fatalf("a second put answered %d, want 412", got)
	}
	rc, err := store.Open(t.Context(), objstore.Object{Key: k, Hash: objstore.HashOf(body), Size: int64(len(body))})
	if err != nil {
		t.Fatalf("the presigned object does not read back verified: %v", err)
	}
	_ = rc.Close()
	if got := srv.Keys(); len(got) != 1 || got[0] != "acme/"+k.Name() {
		t.Fatalf("the bucket holds %v, want only the minted key", got)
	}
}

// A PRESIGNED PUT SIGNS ITS LENGTH, TYPE, DIGEST AND CONDITION, so a client
// can loosen none of them, and hands back no Host header to set.
func TestAPresignedPutSignsEveryBinding(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	store, err := objstore.NewStore(open(t, srv, ""))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello")
	signed, err := store.PresignPut(t.Context(), store.Mint(), presignOf(body))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	headers := u.Query().Get("X-Amz-SignedHeaders")
	for _, want := range []string{"content-length", "content-type", "if-none-match", "x-amz-checksum-sha256"} {
		if !strings.Contains(headers, want) {
			t.Errorf("the URL signs %q, missing %s", headers, want)
		}
	}
	sum := sha256.Sum256(body)
	if got := signed.Headers["X-Amz-Checksum-Sha256"]; got != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Errorf("the checksum header is %q", got)
	}
	if _, set := signed.Headers["Host"]; set {
		t.Error("the presigned put asks the client to set Host")
	}
	if signed.Method != http.MethodPut || signed.Expires.Before(time.Now()) {
		t.Errorf("signed %s expiring %v", signed.Method, signed.Expires)
	}
	if _, err := store.PresignPut(t.Context(), store.Mint(), presignOf(nil)); !errors.Is(err, objstore.ErrDirectUnavailable) {
		t.Errorf("an empty body was signed: %v", err)
	}
}

// A SINGLE-PART PUT CARRIES ITS CHECKSUM WHERE THE PROVIDER WAS PROVEN TO
// CHECK ONE, and a stat reads the digest back; on a provider that was not, it
// carries none and a stat claims none.
func TestASinglePartPutCarriesItsChecksumWhereProven(t *testing.T) {
	t.Parallel()
	body := []byte("a file the provider checks")
	for name, lax := range map[string]bool{"proven": false, "unproven": true} {
		srv := s3fake.Start(t, "files")
		if lax {
			srv.IgnoreChecksums()
		}
		b := open(t, srv, "")
		if err := b.Put(t.Context(), "files/x", bytes.NewReader(body), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
		sent := srv.PutHeaders("files/x").Get("X-Amz-Checksum-Sha256")
		info, err := b.Stat(t.Context(), "files/x")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case !lax && (sent == "" || info.Digest != objstore.HashOf(body)):
			t.Errorf("%s: the put sent checksum %q and the stat read digest %q", name, sent, info.Digest)
		case lax && (sent != "" || info.Digest != ""):
			t.Errorf("%s: the put sent checksum %q and the stat read digest %q", name, sent, info.Digest)
		}
	}
}

// A PRESIGN IS NO REQUEST OF THE CLIENT'S: it sends nothing, so none of the
// client's own steps — the throttle gate, the judge, the request it follows —
// is on it, nothing of it is counted, and the URL it hands back stores the
// bytes it was bound to.
//
// The SDK builds a presign's stack without the retry step the client's own
// steps stand beside ([s3obj] paced), so a presign built on the client's
// options failed — and with it the boot probe, which presigns, so no S3
// backend opened at all.
func TestAPresignIsNoRequestOfTheClients(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	srv := s3fake.Start(t, "files")
	store, err := objstore.NewStore(openCounted(t, srv, rec, nil, s3obj.Clock{}))
	if err != nil {
		t.Fatal(err)
	}
	// counted is every series of the two counters a request moves: an
	// attempt's total, and a request's count and attempts.
	counted := func() map[string]string {
		out := map[string]string{}
		for _, name := range []string{metrics.ObjectsS3Attempts, metrics.ObjectsS3RequestAttempts} {
			for key, s := range reading(rec, name) {
				out[name+" "+key] = fmt.Sprintf("total %d, count %d, sum %g", s.Total, s.Count, s.Sum)
			}
		}
		return out
	}
	before := counted()
	if len(before) == 0 {
		t.Fatal("the open's probe counted no request: the counters this case compares read nothing")
	}
	body := []byte("bytes a client puts itself")
	k := store.Mint()
	signed, err := store.PresignPut(t.Context(), k, presignOf(body))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if after := counted(); !reflect.DeepEqual(after, before) {
		t.Errorf("a presign was counted as the client's request:\nbefore %v\nafter  %v", before, after)
	}
	if got := putSigned(t, signed, body); got != http.StatusOK {
		t.Fatalf("the presigned put answered %d", got)
	}
	rc, err := store.Open(t.Context(), objstore.Object{Key: k, Hash: objstore.HashOf(body), Size: int64(len(body))})
	if err != nil {
		t.Fatalf("the presigned object does not read back verified: %v", err)
	}
	_ = rc.Close()
}
