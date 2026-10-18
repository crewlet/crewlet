package s3obj

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/crewlet/crewlet/internal/objstore"
)

// THE BOOT PROBE, and what it proves.
//
// # Under the prefix, never about the bucket
//
// A node used to check its bucket with HeadBucket, which needs a right over
// the whole bucket (s3:ListBucket). A credential scoped to one company's
// prefix, which is how one bucket is shared between companies without each
// reading the others' files, has no such right, so the check refused exactly
// the credential that makes sharing safe. The probe instead writes, reads back
// and deletes ONE object under `<prefix>probe/<key>`, through the same verbs
// every upload uses: it proves the node can store a file where it will store
// them, which a HeadBucket never did.
//
// # Then whether the provider enforces what a presigned put relies on
//
// A presigned put hands a client a URL it uses itself, and two bindings keep
// it to the bytes the engine agreed to: the signed `x-amz-checksum-sha256`
// makes any put that lands carry exactly the declared bytes, and
// `If-None-Match: *` refuses a second put to the same name, so nobody swaps
// the content after it was verified. Providers differ on both, and some
// accept the headers and ignore them. So the probe asks: a put whose checksum
// does not match its body must be refused `BadDigest`, a matching put must be
// stored, and a second put of the same name under the condition must be
// refused 412. Only a provider that does all three offers direct uploads
// ([Backend.DirectUpload]); any other degrades to every upload streaming
// through the engine, which is today's behaviour, rather than failing the
// boot.
//
// The checksum half alone also decides whether the engine's own single-part
// puts carry the header, so the provider checks what it stores and the audit
// can read the digest back ([Backend.Stat]).
//
// # What fails the boot
//
// Only what an upload would fail on: the probe object cannot be written or
// deleted through the engine's own path. Every other answer is a capability
// the provider lacks, reported in health, never a refusal to start.

// probeNamespace is where the probe's object lives under the prefix: beside
// `files/` and never inside it, so the collector, which judges only names
// under `files/` ([objstore.KeyOfName]), never mistakes it for a file, and a
// probe interrupted before its delete leaves a name nothing else reads.
const probeNamespace = "probe/"

// probeTerm is how long each presigned URL the probe signs is valid: a minute,
// far more than one small request takes and short enough that a URL that
// leaked from a log is worthless.
const probeTerm = time.Minute

// capabilities is what the probe proved of the provider.
type capabilities struct {
	// checksums is a put whose x-amz-checksum-sha256 did not match its
	// body was refused, and one that matched was stored.
	checksums bool
	// conditional is a second put of a held name under If-None-Match: *
	// was refused.
	conditional bool
	// why is the first thing the provider did not do, empty when it did
	// everything.
	why string
}

// direct reports whether a presigned put is safe to offer.
func (c capabilities) direct() bool { return c.checksums && c.conditional }

// probe runs the boot probe: see the file comment.
func (b *Backend) probe(ctx context.Context) (capabilities, error) {
	name := probeNamespace + objstore.KeyAt(time.Now()).String()
	body := []byte("crewlet object store probe " + name)
	// THE PROBE OBJECT IS DELETED HOWEVER THE PROBE ENDS, under a context
	// the caller's cannot end: a boot that gave up is the likeliest moment
	// to leave one.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), callBudget)
		defer cancel()
		_ = b.Delete(cleanup, name)
	}()

	// THE ENGINE'S OWN PATH FIRST: what every upload does, so a credential
	// that cannot write or delete under the prefix fails the boot here,
	// before any capability is judged.
	if err := b.putObject(ctx, name, body, objstore.PutMeta{ContentType: "text/plain"}); err != nil {
		return capabilities{}, fmt.Errorf("s3obj: store a probe object under %q in bucket %q: %w",
			b.prefix, b.bucket, err)
	}
	if err := b.Delete(ctx, name); err != nil {
		return capabilities{}, fmt.Errorf("s3obj: delete the probe object under %q in bucket %q "+
			"(the collector deletes what no file names, and needs s3:DeleteObject): %w",
			b.prefix, b.bucket, err)
	}

	var c capabilities
	digest := objstore.HashOf(body)
	wrong := objstore.HashOf(append([]byte("not "), body...))

	// A BODY THAT DOES NOT MATCH ITS CHECKSUM must be refused.
	status, code, err := b.presignedPut(ctx, name, body, wrong)
	switch {
	case err != nil:
		return capabilities{}, err
	case status == http.StatusForbidden:
		return capabilities{}, fmt.Errorf("s3obj: the bucket %q refused a presigned put under "+
			"%q (%s): the node's credential cannot sign the puts it can make itself", b.bucket,
			b.prefix, code)
	case status == http.StatusBadRequest && (code == "BadDigest" || code == "XAmzContentChecksumMismatch"):
		c.checksums = true
	case status/100 == 2:
		c.why = "the provider stored a body whose x-amz-checksum-sha256 did not match it"
		if err := b.Delete(ctx, name); err != nil {
			return capabilities{}, fmt.Errorf("s3obj: delete the probe object: %w", err)
		}
	default:
		c.why = fmt.Sprintf("the provider answered %d %s to a body whose "+
			"x-amz-checksum-sha256 did not match it, rather than BadDigest", status, code)
	}

	// A MATCHING BODY must be stored, under the condition.
	status, code, err = b.presignedPut(ctx, name, body, digest)
	switch {
	case err != nil:
		return capabilities{}, err
	case status/100 != 2:
		c.checksums = false
		return c.or(fmt.Sprintf("the provider refused a presigned put whose checksum "+
			"matched its body (%d %s)", status, code)), nil
	}

	// A SECOND PUT OF A HELD NAME must be refused by its condition.
	status, code, err = b.presignedPut(ctx, name, body, digest)
	switch {
	case err != nil:
		return capabilities{}, err
	case status == http.StatusPreconditionFailed:
		c.conditional = true
	case status/100 == 2:
		c = c.or("the provider accepted a second put of a name it held, under If-None-Match: *")
	default:
		c = c.or(fmt.Sprintf("the provider answered %d %s to a second put of a name "+
			"it held under If-None-Match: *, rather than 412", status, code))
	}
	return c, nil
}

// or is c with why recorded, unless an earlier reason already was: the
// first thing the provider did not do is the one health names.
func (c capabilities) or(why string) capabilities {
	if c.why == "" {
		c.why = why
	}
	return c
}

// presignedPut signs a put of body under name bound to digest, makes it, and
// answers the status and the error code the provider gave. An error is a put
// that got no answer at all, which fails the boot: a bucket that does not
// answer cannot store a file either.
func (b *Backend) presignedPut(ctx context.Context, name string, body []byte,
	digest objstore.Hash) (int, string, error) {

	signed, err := b.PresignPut(ctx, name, objstore.PresignRequest{
		Size: int64(len(body)), ContentType: "text/plain", SHA256: digest, Expires: probeTerm,
	})
	if err != nil {
		return 0, "", fmt.Errorf("s3obj: sign the probe's put: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, signed.Method, signed.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("s3obj: build the probe's put: %w", err)
	}
	for k, v := range signed.Headers {
		if http.CanonicalHeaderKey(k) == "Content-Length" {
			continue // the request's own length, which NewRequest set from the body
		}
		req.Header.Set(k, v)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("s3obj: reach bucket %q with the probe's put: %w", b.bucket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, errorCode(resp.Body), nil
}

// errorCode is the `<Code>` of an S3 error body, or "" for a body that is not
// one. Read through a small window: an error body is a sentence.
func errorCode(r io.Reader) string {
	var e struct {
		Code string `xml:"Code"`
	}
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || xml.Unmarshal(raw, &e) != nil {
		return ""
	}
	return e.Code
}

// maxDirectBytes is the largest body one presigned put may carry: S3's own
// ceiling on a single PUT, five gibibytes. A larger one would be signed and
// then refused by the provider after the client sent it.
const maxDirectBytes = 5 << 30

// PresignPut implements [objstore.Presigner]: a PUT of name signed with the
// exact length, the type, the SHA-256 and If-None-Match: *, so the provider
// refuses any other body and any second put. [objstore.Store] calls it only
// once [Backend.DirectUpload] answered available; the probe calls it to find
// out.
func (b *Backend) PresignPut(ctx context.Context, name string, r objstore.PresignRequest) (objstore.Presigned, error) {
	switch {
	case r.Size <= 0:
		// AN EMPTY BODY SIGNS NO TYPE: the SDK drops Content-Type from a
		// request of no length, so the binding would be one short.
		return objstore.Presigned{}, fmt.Errorf("%w: an empty body is stored through the engine",
			objstore.ErrDirectUnavailable)
	case r.Size > maxDirectBytes:
		return objstore.Presigned{}, fmt.Errorf("%w: %d bytes is past the %d one presigned put "+
			"may carry", objstore.ErrDirectUnavailable, r.Size, int64(maxDirectBytes))
	}
	raw, err := hex.DecodeString(string(r.SHA256))
	if err != nil || len(raw) != sha256.Size {
		return objstore.Presigned{}, fmt.Errorf("s3obj: presign %s: %w: %q", name, objstore.ErrBadHash, r.SHA256)
	}
	out, err := b.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(b.bucket),
		Key:            aws.String(b.key(name)),
		ContentLength:  aws.Int64(r.Size),
		ContentType:    aws.String(r.ContentType),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(raw)),
		IfNoneMatch:    aws.String("*"),
	}, s3.WithPresignExpires(r.Expires))
	if err != nil {
		return objstore.Presigned{}, fmt.Errorf("s3obj: presign a put of %s: %w", name, err)
	}
	headers := make(map[string]string, len(out.SignedHeader))
	for k, v := range out.SignedHeader {
		// THE HOST IS THE URL'S, and a client that set it would be setting
		// a header no browser lets a page set.
		if http.CanonicalHeaderKey(k) == "Host" || len(v) == 0 {
			continue
		}
		headers[http.CanonicalHeaderKey(k)] = v[0]
	}
	for _, want := range []string{"Content-Length", "Content-Type", "If-None-Match", "X-Amz-Checksum-Sha256"} {
		if _, ok := headers[want]; !ok {
			return objstore.Presigned{}, fmt.Errorf("s3obj: the presigned put of %s does not "+
				"sign %s, and a put without it could store other bytes", name, want)
		}
	}
	return objstore.Presigned{Method: out.Method, URL: out.URL, Headers: headers,
		Expires: time.Now().Add(r.Expires).UTC()}, nil
}

// DirectUpload implements [objstore.Presigner]: what the boot probe proved.
func (b *Backend) DirectUpload() (bool, string) {
	if b.caps.direct() {
		return true, ""
	}
	return false, b.caps.why
}

// checksumOf is the SHA-256 of b as S3 spells one: base64 of the raw digest.
func checksumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// digestOf reads a full-object checksum S3 answered back into the object
// store's spelling, or "" where it answered none or a composite one: an
// object assembled from parts carries `<checksum>-<parts>`, a checksum of the
// parts' checksums, which says nothing about the whole content's digest.
func digestOf(checksum *string, kind string) objstore.Hash {
	value := aws.ToString(checksum)
	if value == "" || strings.Contains(value, "-") || (kind != "" && kind != "FULL_OBJECT") {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != sha256.Size {
		return ""
	}
	return objstore.Hash(hex.EncodeToString(raw))
}
