package objstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDirectUnavailable is a direct upload asked of a store whose backend
// offers none: the broker's own bucket (natsobj), which no browser may reach,
// or a bucket whose provider the boot probe could not prove enforces the
// bindings a presigned put relies on ([Store.DirectUpload]). The caller's
// answer is to stream the bytes through the engine instead, never to retry.
var ErrDirectUnavailable = errors.New("objstore: this backend offers no direct upload")

// MaxPresignTerm is the longest a presigned put may stay valid.
//
// AN HOUR. The term has to outlast the slowest transfer of the largest file a
// client sends directly (an artifact file is at most 16 MiB, eight minutes at
// the [MiBPace] floor), and it has to end far inside [RecordWithin], because
// a put that lands after the write naming its key is refused leaves bytes no
// row can ever name. An hour is the bound the upload intent's own term is
// held to ([PresignTerm]) and a twelfth of RecordWithin.
const MaxPresignTerm = time.Hour

// presignBase is the part of [PresignTerm] that does not depend on the size:
// the client's own latency between receiving the URL and starting the put,
// and a retry of a put that failed.
//
// FIFTEEN MINUTES, the figure the upload intent's design states: a browser
// tab that receives several transfers at once starts them one after another,
// and the last of a batch can begin minutes after the first.
const presignBase = 15 * time.Minute

// PresignTerm is how long a presigned put of size bytes is given: fifteen
// minutes and a [MiBPace] for every mebibyte begun, never more than
// [MaxPresignTerm].
func PresignTerm(size int64) time.Duration {
	return min(presignBase+PaceFor(size), MaxPresignTerm)
}

// PresignRequest is what a presigned put is bound to. Every field is signed
// into the URL, so a put carrying any other length, type or content is
// refused by the provider, and a second put to the same URL is refused by its
// if-none-match condition.
type PresignRequest struct {
	// Size is the exact length of the body, in bytes.
	Size int64

	// ContentType is the body's media type, the one the object is served
	// as. Required: an object stored with a type the client chose would be
	// the client deciding how its bytes are read.
	ContentType string

	// SHA256 is the digest the body must hash to.
	SHA256 Hash

	// Expires is how long the URL stays valid, at most [MaxPresignTerm];
	// [PresignTerm] is the term an upload intent gives a file of its size.
	Expires time.Duration
}

// Presigned is a put a client makes itself, straight to the backend.
type Presigned struct {
	// Method is the HTTP method, always PUT.
	Method string `json:"method"`

	// URL is where the body goes, its signature in the query.
	URL string `json:"url"`

	// Headers are the headers the put must carry, each one signed: the
	// length, the type, the digest and the if-none-match condition. A
	// browser sets Content-Length from the body itself, and the signature
	// still binds it.
	Headers map[string]string `json:"headers"`

	// Expires is when the URL stops being accepted, in UTC.
	Expires time.Time `json:"expires"`
}

// Presigner is a backend that can hand a client a put it makes itself.
//
// AN OPTIONAL CAPABILITY, asked of a backend by [Store] rather than added to
// [Backend]: the broker's own bucket cannot offer one (no browser reaches the
// broker), and a method that every such backend answered with a refusal would
// be a verb the conformance suite certifies as broken.
type Presigner interface {
	// PresignPut signs a put of r under name. It is called only once
	// [Presigner.DirectUpload] answered available.
	PresignPut(ctx context.Context, name string, r PresignRequest) (Presigned, error)

	// DirectUpload reports whether the backend's provider was proven to
	// enforce what a presigned put relies on, and why not where it was
	// not.
	DirectUpload() (bool, string)
}

// noPresign is what [Store.DirectUpload] says of a backend with no presigner.
const noPresign = "this backend streams every upload through the engine " +
	"(the broker's own bucket is reachable by the fleet alone)"

// DirectUpload reports whether this store can hand a client a presigned put,
// and why not where it cannot: the reason health reports beside
// `objects.direct_upload`.
func (s *Store) DirectUpload() (bool, string) {
	p, ok := s.backend.(Presigner)
	if !ok {
		return false, noPresign
	}
	return p.DirectUpload()
}

// PresignPut signs a put of the object k names, bound to r, for a client to
// make itself.
//
// # The engine still mints the key
//
// k is a key this store minted ([Store.Mint]) for an upload the caller has
// reserved: the client never chooses a name, which is what keeps a key
// "named only by the write that uploaded it" (ADR-0027) when the write is not
// the engine's. And the URL has to stop working before the key is too old to
// be named: a put landing after [RecordWithin] stores bytes no row can name,
// so a term reaching past it is refused here rather than signed.
//
// # And it still verifies the bytes
//
// What a presigned put stores is checked again before any row names it, by a
// read through [Store.Open] against the declared digest: the provider's own
// checksum check is what keeps a client from swapping the bytes after the
// read, never a substitute for it.
func (s *Store) PresignPut(ctx context.Context, k Key, r PresignRequest) (Presigned, error) {
	if err := s.checkPresign(k, r); err != nil {
		return Presigned{}, err
	}
	p, ok := s.backend.(Presigner)
	if !ok {
		return Presigned{}, fmt.Errorf("%w: %s", ErrDirectUnavailable, noPresign)
	}
	if available, why := p.DirectUpload(); !available {
		return Presigned{}, fmt.Errorf("%w: %s", ErrDirectUnavailable, why)
	}
	signed, err := p.PresignPut(ctx, k.Name(), r)
	if err != nil {
		return Presigned{}, fmt.Errorf("objstore: presign a put of %s: %w", k, err)
	}
	return signed, nil
}

// checkPresign refuses a request no backend should sign.
func (s *Store) checkPresign(k Key, r PresignRequest) error {
	switch {
	case k.IsZero():
		return errors.New("objstore: a presigned put names no key")
	case r.Size < 0:
		return fmt.Errorf("objstore: a presigned put of %d bytes", r.Size)
	case strings.TrimSpace(r.ContentType) == "" || strings.ContainsAny(r.ContentType, "\r\n"):
		return fmt.Errorf("objstore: a presigned put of %s needs a media type, "+
			"one line long (got %q)", k, r.ContentType)
	case !r.SHA256.Valid():
		return fmt.Errorf("objstore: a presigned put of %s: %w: %q", k, ErrBadHash, r.SHA256)
	case r.Expires <= 0 || r.Expires > MaxPresignTerm:
		return fmt.Errorf("objstore: a presigned put of %s valid for %v; a term is "+
			"positive and at most %v", k, r.Expires, MaxPresignTerm)
	}
	if last := k.Minted().Add(RecordWithin); s.now().Add(r.Expires).After(last) {
		return fmt.Errorf("objstore: a presigned put of %s would be accepted until %s, "+
			"after %s, when no write may name its key any more: mint a fresh key",
			k, s.now().Add(r.Expires).UTC().Format(time.RFC3339),
			last.UTC().Format(time.RFC3339))
	}
	return nil
}

// Mint mints a key for an upload that does not pass through [Store.Put]: a
// presigned put, whose bytes the client sends itself. On the store's own
// clock, as every key Put mints is, because that instant is what the write
// naming the key and the collector both judge its age by.
func (s *Store) Mint() Key { return KeyAt(s.now()) }

// Reserver is a backend that holds a byte reservation of its own and can say
// how much of it its objects take: the fleet's own bucket on the broker,
// whose reservation is carved out of what every coordination record shares.
// A provider's bucket reserves nothing and is not one.
type Reserver interface {
	Usage(ctx context.Context) (Usage, error)
}

// Usage answers how much of the backend's byte reservation its objects take,
// and false where the backend holds none.
func (s *Store) Usage(ctx context.Context) (Usage, bool, error) {
	r, ok := s.backend.(Reserver)
	if !ok {
		return Usage{}, false, nil
	}
	u, err := r.Usage(ctx)
	if err != nil {
		return Usage{}, true, fmt.Errorf("objstore: read the backend's usage: %w", err)
	}
	return u, true, nil
}
