package objstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// signer is a backend that presigns, and says what it was asked to sign.
type signer struct {
	*memobj.Backend
	available bool
	why       string
	asked     []string
}

func (s *signer) DirectUpload() (bool, string) { return s.available, s.why }

func (s *signer) PresignPut(_ context.Context, name string, r objstore.PresignRequest) (objstore.Presigned, error) {
	s.asked = append(s.asked, name)
	return objstore.Presigned{Method: "PUT", URL: "https://bucket.example.com/" + name,
		Headers: map[string]string{"Content-Type": r.ContentType}}, nil
}

func validRequest() objstore.PresignRequest {
	return objstore.PresignRequest{Size: 12, ContentType: "text/plain",
		SHA256: objstore.HashOf([]byte("hello, world")), Expires: objstore.PresignTerm(12)}
}

// A BACKEND THAT CANNOT PRESIGN IS ASKED NOTHING, and the refusal is the one
// sentinel a caller branches on to stream the bytes through the engine
// instead: the broker's own bucket advertises no presign.
func TestABackendWithoutAPresignerOffersNoDirectUpload(t *testing.T) {
	t.Parallel()
	s := newStore(t, memobj.New())
	if ok, why := s.DirectUpload(); ok || why == "" {
		t.Fatalf("a backend with no presigner reports direct upload %v (%q)", ok, why)
	}
	_, err := s.PresignPut(t.Context(), s.Mint(), validRequest())
	if !errors.Is(err, objstore.ErrDirectUnavailable) {
		t.Fatalf("a presign on a backend with none answered %v, want ErrDirectUnavailable", err)
	}
}

// A PRESIGNER WHOSE PROVIDER WAS NOT PROVEN is not asked to sign: an
// unproven provider may store bytes that do not match the digest, or a second
// put to the same URL.
func TestAnUnprovenPresignerIsNeverAskedToSign(t *testing.T) {
	t.Parallel()
	b := &signer{Backend: memobj.New(), why: "the provider ignored if-none-match"}
	s := newStore(t, b)
	_, err := s.PresignPut(t.Context(), s.Mint(), validRequest())
	if !errors.Is(err, objstore.ErrDirectUnavailable) || !strings.Contains(err.Error(), b.why) {
		t.Fatalf("an unproven presigner answered %v, want ErrDirectUnavailable naming why", err)
	}
	if len(b.asked) != 0 {
		t.Fatalf("an unproven presigner was asked to sign %v", b.asked)
	}
}

// A DIRECT UPLOAD NAMES ONLY A KEY THE ENGINE MINTED: the presigned name is
// the key's own, under the store's namespace, and every binding the client
// could loosen is refused before the backend is asked.
func TestADirectUploadNamesOnlyAKeyTheEngineMinted(t *testing.T) {
	t.Parallel()
	b := &signer{Backend: memobj.New(), available: true}
	s := newStore(t, b)
	k := s.Mint()
	if _, err := s.PresignPut(t.Context(), k, validRequest()); err != nil {
		t.Fatalf("a valid presign was refused: %v", err)
	}
	if len(b.asked) != 1 || b.asked[0] != k.Name() {
		t.Fatalf("the backend was asked to sign %v, want only %s", b.asked, k.Name())
	}
	refused := map[string]func(*objstore.PresignRequest){
		"a negative size":       func(r *objstore.PresignRequest) { r.Size = -1 },
		"no media type":         func(r *objstore.PresignRequest) { r.ContentType = " " },
		"a two-line media type": func(r *objstore.PresignRequest) { r.ContentType = "text/plain\r\nx: y" },
		"a digest that is not":  func(r *objstore.PresignRequest) { r.SHA256 = "abc" },
		"no term":               func(r *objstore.PresignRequest) { r.Expires = 0 },
		"a term past an hour":   func(r *objstore.PresignRequest) { r.Expires = objstore.MaxPresignTerm + time.Second },
	}
	for name, mutate := range refused {
		r := validRequest()
		mutate(&r)
		if _, err := s.PresignPut(t.Context(), s.Mint(), r); err == nil {
			t.Errorf("a presign with %s was signed", name)
		}
	}
	if _, err := s.PresignPut(t.Context(), objstore.Key{}, validRequest()); err == nil {
		t.Error("a presign naming no key was signed")
	}
	if len(b.asked) != 1 {
		t.Fatalf("refused presigns still reached the backend: %v", b.asked)
	}
}

// A URL NEVER OUTLIVES THE KEY'S RECORD WINDOW: a put landing after
// RecordWithin stores bytes no row may name, so a key minted near the end of
// its window is refused rather than signed.
func TestAPresignedPutEndsBeforeItsKeyCanNoLongerBeNamed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	b := &signer{Backend: memobj.New(), available: true}
	s, err := objstore.NewStoreAt(b, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	old := objstore.KeyAt(now.Add(-objstore.RecordWithin + 10*time.Minute))
	r := validRequest()
	r.Expires = 20 * time.Minute
	if _, err := s.PresignPut(t.Context(), old, r); err == nil {
		t.Fatal("a presign accepted past its key's record window was signed")
	}
	r.Expires = 5 * time.Minute
	if _, err := s.PresignPut(t.Context(), old, r); err != nil {
		t.Fatalf("a presign ending inside its key's window was refused: %v", err)
	}
}

// THE TERM GROWS WITH THE SIZE AND STOPS AT THE CEILING.
func TestThePresignTermIsBounded(t *testing.T) {
	t.Parallel()
	if got := objstore.PresignTerm(0); got != 15*time.Minute {
		t.Errorf("an empty body is given %v, want fifteen minutes", got)
	}
	if got := objstore.PresignTerm(16 * objstore.MiB); got != 15*time.Minute+16*objstore.MiBPace {
		t.Errorf("a 16 MiB body is given %v", got)
	}
	if got := objstore.PresignTerm(1 << 30); got != objstore.MaxPresignTerm {
		t.Errorf("a gibibyte is given %v, want the %v ceiling", got, objstore.MaxPresignTerm)
	}
}

// A BACKEND WITH NO RESERVATION REPORTS NONE, rather than a usage of zero
// against a ceiling of zero that an alarm would read as full.
func TestABackendWithNoReservationReportsNone(t *testing.T) {
	t.Parallel()
	_, held, err := newStore(t, memobj.New()).Usage(t.Context())
	if err != nil || held {
		t.Fatalf("a backend with no reservation answered held %v, %v", held, err)
	}
}

// A RESERVED UPLOAD IS STORED UNDER THE KEY ITS RESERVATION MINTED: the object
// a streamed put answers names that key, and reads back verified, so the key a
// reservation answers for and the object a row names are one.
func TestAPutUnderAMintedKeyStoresThatKey(t *testing.T) {
	t.Parallel()
	s := newStore(t, memobj.New())
	k := s.Mint()
	body := []byte("reserved before it moved")
	obj, err := s.PutMinted(t.Context(), k, strings.NewReader(string(body)), int64(len(body)),
		objstore.PutMeta{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("a put under a minted key was refused: %v", err)
	}
	if obj.Key != k || obj.Hash != objstore.HashOf(body) || obj.Size != int64(len(body)) {
		t.Fatalf("the put answered %+v, want key %s and the body's digest and size", obj, k)
	}
	if _, err := s.Stat(t.Context(), k); err != nil {
		t.Fatalf("the object is not under its minted key: %v", err)
	}
	if _, err := s.PutMinted(t.Context(), objstore.Key{}, strings.NewReader("x"), 1,
		objstore.PutMeta{}); err == nil {
		t.Fatal("a put naming no key was stored")
	}
}

// A KEY TOO OLD TO BE NAMED IS REFUSED BEFORE A BYTE IS READ: bytes stored
// under it could never be recorded and would wait for the collector with
// nothing reserving them.
func TestAPutUnderAStaleMintedKeyReadsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	backend := memobj.New()
	s, err := objstore.NewStoreAt(backend, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	stale := objstore.KeyAt(now.Add(-objstore.RecordWithin - time.Minute))
	body := &countingReader{r: strings.NewReader("late bytes")}
	if _, err := s.PutMinted(t.Context(), stale, body, 64, objstore.PutMeta{}); err == nil {
		t.Fatal("a put under a key past its record window was stored")
	}
	if body.n != 0 {
		t.Fatalf("a refused put read %d bytes first", body.n)
	}
	if _, err := s.Stat(t.Context(), stale); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a refused put left an object behind: %v", err)
	}
}

// countingReader counts what a put read.
type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
