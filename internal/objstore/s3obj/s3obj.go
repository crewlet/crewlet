// Package s3obj keeps the object store's objects in an S3-compatible bucket:
// Amazon S3, or anything speaking its API — Cloudflare R2, MinIO, Google Cloud
// Storage's interoperability endpoint, Backblaze B2, Ceph's gateway.
//
// # When to take it
//
// The default backend (internal/objstore/natsobj) keeps every object on every
// broker member holding a copy, which is right for a fleet of three or five
// data nodes and wrong for a company whose files outgrow one member's disk.
// A bucket has no such ceiling, keeps its own copies, and takes the files off
// the broker's routes entirely — at the price of a dependency the engine does
// not run and a network round trip per request.
//
// # One object per name, streamed
//
// Each name is the key `<prefix><name>` — and every name the store writes is
// `files/<key>` ([objstore.Key.Name]), so an object of the engine's sits at
// `<prefix>files/<key>`, under a corner of the bucket an empty prefix still
// does not share with another application's own UUID-named objects.
//
// LastModified is what [objstore.Info.Written] reports, and AN OBJECT PUT IN
// PARTS CARRIES ITS UPLOAD'S START there, not its completion: Amazon dates an
// object assembled from parts by when its multipart upload was created —
// after the put began, and for a large object hours before anybody could read
// it. It is why that contract promises only an instant between the two, to
// be read as the earliest the object might have been stored and never as when
// it was finished; s3fake dates a completed upload the same way, so nothing
// tested against it meets a gentler S3 than Amazon's.
//
// A put STREAMS without knowing its length, which S3 can only take in parts:
// a body that ends inside its first [PartBytes] is one PutObject, and a longer
// one a multipart upload of equal parts sent one after another through ONE
// buffer — so an upload holds a part in memory and never the object. A body
// ENDS only where its reader answers io.EOF itself ([objstore.Fill]): one cut
// short fails the put rather than being stored as far as it got. A part that
// fails, or a read, abandons the upload, under a context of its own, because
// an upload nobody completes or aborts is invisible to a listing and billed
// until somebody does.
//
// # Every request is bounded
//
// The bucket is reached through the engine's shared transport with no
// client-wide timeout (internal/httpx), which is right only where every
// request is bounded some other way — so every request is: a request that
// moves no body gets [callBudget], one that carries a part gets that plus the
// part's size at [objstore.MiBPace], and a read is bounded by the deadline its
// caller must carry ([objstore.CheckRead]).
package s3obj

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Config is where the bucket is and how to reach it.
type Config struct {
	// Endpoint is the S3 API's base URL; empty is Amazon's own for the
	// region.
	Endpoint string
	// Region is the bucket's region. Required by the signature even where
	// the provider ignores it (R2 takes `auto`, MinIO `us-east-1`).
	Region string
	// Bucket is the bucket's name.
	Bucket string
	// Prefix is prepended to every key, so a bucket can hold more than
	// one company: an object is `<prefix>files/<key>`.
	Prefix string
	// PathStyle addresses the bucket in the path rather than the host
	// name — what MinIO and most self-hosted gateways need.
	PathStyle bool
	// AccessKeyID and SecretAccessKey are the credentials. Both empty
	// takes the SDK's own chain instead — the environment, a shared
	// profile, a web identity, or the instance's role — which is how a
	// node on AWS should hold none at all.
	AccessKeyID     string
	SecretAccessKey string
}

// PartBytes is the size of every part of a multipart upload but its last, and
// the most one upload holds in memory.
//
// EIGHT MEBIBYTES. S3 takes parts of five at the least, all but the last;
// Cloudflare R2 takes them only all of one size, which is why every part is
// this one; and an upload is ten thousand parts at most — eighty gibibytes,
// eighty times the largest file the tracker keeps. A body under it never
// becomes a multipart upload at all.
const PartBytes = 8 * objstore.MiB

// maxParts is the most parts S3 assembles into one object.
const maxParts = 10_000

// callBudget bounds a request that carries no body of its own: a head, a
// delete, a page of a listing, the start or the abandonment of an upload.
//
// THIRTY SECONDS: each is one small request and its answer, a fraction of a
// second against any healthy provider, and thirty is room for the SDK's own
// retries of one that failed — while a bucket that has stopped answering ends
// a request rather than holding the handler that sent it.
const callBudget = 30 * time.Second

// completeBudget bounds the request that assembles a multipart upload.
//
// FIVE MINUTES, not [callBudget]: Amazon documents that completing an upload
// can take several minutes for a large object, and keeps the connection alive
// with whitespace while it works.
const completeBudget = 5 * time.Minute

// sendBudget bounds a request carrying n bytes: [callBudget] for the request
// and [objstore.MiBPace] for every mebibyte of its body.
func sendBudget(n int64) time.Duration { return callBudget + objstore.PaceFor(n) }

// parts recycles part buffers between uploads, so a node storing many small
// objects does not allocate eight mebibytes for each.
//
// A BUFFER GOES BACK ONLY AFTER A PUT THAT SUCCEEDED. A request that failed
// may still have its body read by the transport after it returns — the
// standard library says so of RoundTrip — and a buffer handed straight to the
// next upload would be written under that read.
var parts = sync.Pool{New: func() any {
	b := make([]byte, PartBytes)
	return &b
}}

// Backend is the objects in a bucket.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// Open builds the client and checks the bucket is reachable with these
// credentials, so a wrong key fails the boot rather than the first upload.
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Bucket == "" || cfg.Region == "" {
		return nil, errors.New("s3obj: an S3 store needs a bucket and a region")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, errors.New("s3obj: an access key id and a secret access key are given " +
			"together or not at all")
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	// THE SDK'S OWN CLIENT DISCOVERS CREDENTIALS — the instance's role, a
	// web identity, a container's endpoint — because those requests are
	// the SDK's protocol rather than the engine's, and it accepts only its
	// own client type once an AWS_CA_BUNDLE is set.
	base, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3obj: load the S3 credentials: %w", err)
	}
	httpClient, err := bucketClient(os.Getenv("AWS_CA_BUNDLE"))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(base, func(o *s3.Options) {
		// THE BUCKET IS REACHED THROUGH THE ENGINE'S OWN TRANSPORT
		// (internal/httpx), never a pool of the SDK's.
		o.HTTPClient = httpClient
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
		// CHECKSUMS ONLY WHERE AN OPERATION REQUIRES ONE. The SDK's default
		// sends a trailing checksum on every put and every part, which
		// providers that are not Amazon refuse or mishandle, and buys
		// nothing here: every object is checked against the SHA-256 its
		// row records on every read (objstore.Store), which no part's
		// checksum could stand in for.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	b := &Backend{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}
	headCtx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	if _, err := client.HeadBucket(headCtx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("s3obj: reach bucket %q: %w", cfg.Bucket, err)
	}
	return b, nil
}

func (b *Backend) key(name string) string { return b.prefix + name }

// Put implements [objstore.Backend].
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) (err error) {
	r = objstore.ContextReader(ctx, r)
	buf := parts.Get().(*[]byte)
	defer func() {
		if err == nil {
			parts.Put(buf)
		}
	}()
	n, end, err := objstore.Fill(r, *buf)
	switch {
	case err != nil:
		return fmt.Errorf("s3obj: read %s: %w", name, err)
	case end:
		return b.putObject(ctx, name, (*buf)[:n], m)
	}
	return b.putParts(ctx, name, r, *buf, m)
}

// contentType is a put's media type as the API takes it: nil records none.
func contentType(m objstore.PutMeta) *string {
	if m.ContentType == "" {
		return nil
	}
	return aws.String(m.ContentType)
}

// putObject stores a body that fit one part in one request.
func (b *Backend) putObject(ctx context.Context, name string, data []byte, m objstore.PutMeta) error {
	_, err := call(ctx, sendBudget(int64(len(data))), func(ctx context.Context) (*s3.PutObjectOutput, error) {
		return b.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(b.key(name)),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data))),
			ContentType:   contentType(m),
		})
	})
	if err != nil {
		return fmt.Errorf("s3obj: put %s: %w", name, err)
	}
	return nil
}

// putParts streams a body longer than one part as a multipart upload whose
// first part is already in buf, reading each next part into the same buffer
// once the one before it is stored.
//
// EVERY FAILURE ABANDONS THE UPLOAD, under a context the caller's cannot end:
// the failure being cleaned up after is often the caller giving up, and an
// abort inheriting that context would abort nothing.
func (b *Backend) putParts(ctx context.Context, name string, r io.Reader, buf []byte,
	m objstore.PutMeta) (err error) {

	key := b.key(name)
	started, err := call(ctx, callBudget, func(ctx context.Context) (*s3.CreateMultipartUploadOutput, error) {
		return b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key), ContentType: contentType(m),
		})
	})
	if err != nil {
		return fmt.Errorf("s3obj: start the upload of %s: %w", name, err)
	}
	upload := started.UploadId
	defer func() {
		if err == nil {
			return
		}
		abort, cancel := context.WithTimeout(context.WithoutCancel(ctx), callBudget)
		defer cancel()
		_, aerr := b.client.AbortMultipartUpload(abort, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: upload,
		})
		if aerr != nil && !missing(aerr) {
			err = fmt.Errorf("%w (and its upload %s could not be abandoned, so the "+
				"bucket bills for its parts until it is: %v)", err, aws.ToString(upload), aerr)
		}
	}()

	var done []types.CompletedPart
	size, last := len(buf), false
	for number := int32(1); size > 0; number++ {
		if number > maxParts {
			return fmt.Errorf("s3obj: %s is longer than the %d parts of %d bytes one "+
				"upload can hold", name, maxParts, PartBytes)
		}
		part := buf[:size]
		sent, perr := call(ctx, sendBudget(int64(size)),
			func(ctx context.Context) (*s3.UploadPartOutput, error) {
				return b.client.UploadPart(ctx, &s3.UploadPartInput{
					Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: upload,
					PartNumber: aws.Int32(number), Body: bytes.NewReader(part),
					ContentLength: aws.Int64(int64(size)),
				})
			})
		if perr != nil {
			return fmt.Errorf("s3obj: store part %d of %s: %w", number, name, perr)
		}
		done = append(done, types.CompletedPart{ETag: sent.ETag, PartNumber: aws.Int32(number)})
		if last {
			break
		}
		var rerr error
		if size, last, rerr = objstore.Fill(r, buf); rerr != nil {
			return fmt.Errorf("s3obj: read %s: %w", name, rerr)
		}
	}
	_, err = call(ctx, completeBudget, func(ctx context.Context) (*s3.CompleteMultipartUploadOutput, error) {
		return b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: upload,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: done},
		})
	})
	if err != nil {
		return fmt.Errorf("s3obj: complete the upload of %s: %w", name, err)
	}
	return nil
}

// call runs one request under its own budget.
func call[T any](ctx context.Context, budget time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return fn(ctx)
}

// Get implements [objstore.Backend].
//
// A WHOLE OBJECT IS ASKED FOR WITH NO RANGE: a range is refused on an empty
// object, every range there is, so a read of one that carried `bytes=0-`
// would fail where it should answer nothing.
func (b *Backend) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	in := &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(b.key(name))}
	switch {
	case n > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	case off > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", off))
	}
	out, err := b.client.GetObject(ctx, in)
	switch {
	case missing(err):
		return nil, objstore.ErrNotFound
	case status(err) == http.StatusRequestedRangeNotSatisfiable:
		// A RANGE THAT BEGINS AT OR PAST THE END: nothing to read,
		// which the contract says is an empty stream.
		return io.NopCloser(bytes.NewReader(nil)), nil
	case err != nil:
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	// THE STREAM ANSWERS ctx AT EVERY READ, not only once the transport
	// notices the request was cancelled: a body the transport had already
	// buffered would otherwise be handed out after the caller gave up.
	return struct {
		io.Reader
		io.Closer
	}{objstore.ContextReader(ctx, out.Body), out.Body}, nil
}

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	out, err := call(ctx, callBudget, func(ctx context.Context) (*s3.HeadObjectOutput, error) {
		return b.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(b.bucket), Key: aws.String(b.key(name)),
		})
	})
	if missing(err) {
		return objstore.Info{}, objstore.ErrNotFound
	}
	if err != nil {
		return objstore.Info{}, fmt.Errorf("s3obj: stat %s: %w", name, err)
	}
	return objstore.Info{Name: name, Size: aws.ToInt64(out.ContentLength),
		Written: aws.ToTime(out.LastModified).UTC()}, nil
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, name string) error {
	_, err := call(ctx, callBudget, func(ctx context.Context) (*s3.DeleteObjectOutput, error) {
		return b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(b.bucket), Key: aws.String(b.key(name)),
		})
	})
	if err != nil && !missing(err) {
		return fmt.Errorf("s3obj: delete %s: %w", name, err)
	}
	return nil
}

// List implements [objstore.Backend]: every key under the prefix, a page at a
// time, each named by what follows the prefix — verbatim, since what a name
// means is not this backend's to judge.
func (b *Backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.prefix),
	})
	for pages.HasMorePages() {
		page, err := call(ctx, callBudget, func(ctx context.Context) (*s3.ListObjectsV2Output, error) {
			return pages.NextPage(ctx)
		})
		if err != nil {
			return fmt.Errorf("s3obj: list %q: %w", b.prefix, err)
		}
		for _, obj := range page.Contents {
			if err := visit(objstore.Info{
				Name:    strings.TrimPrefix(aws.ToString(obj.Key), b.prefix),
				Size:    aws.ToInt64(obj.Size),
				Written: aws.ToTime(obj.LastModified).UTC(),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// bucketClient is the client the bucket is reached through: the engine's
// shared one, or — where the operator set AWS_CA_BUNDLE, the SDK's own setting
// for a bucket behind a private certificate authority — a clone of its
// transport that also trusts that bundle. The system roots (SSL_CERT_FILE,
// SSL_CERT_DIR) are trusted either way.
func bucketClient(bundle string) (*http.Client, error) {
	if bundle == "" {
		return httpx.Client(0), nil
	}
	pem, err := os.ReadFile(bundle)
	if err != nil {
		return nil, fmt.Errorf("s3obj: read AWS_CA_BUNDLE %s: %w", bundle, err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("s3obj: AWS_CA_BUNDLE %s holds no PEM certificate", bundle)
	}
	return httpx.Customized(0, func(t *http.Transport) {
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		t.TLSClientConfig.RootCAs = roots
	}), nil
}

// missing reports an answer that the key — or an upload being abandoned — is
// not there, under each spelling the API uses for it: GetObject's NoSuchKey,
// HeadObject's bare 404, which carries no body to name a code in, and an
// abort's NoSuchUpload.
func missing(err error) bool {
	if err == nil {
		return false
	}
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	var noUpload *types.NoSuchUpload
	if errors.As(err, &noKey) || errors.As(err, &notFound) || errors.As(err, &noUpload) {
		return true
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.ErrorCode() {
	case "NoSuchKey", "NotFound", "NoSuchUpload":
		return true
	}
	return false
}

// status is the HTTP status an error was answered with, 0 for one that never
// reached an answer.
func status(err error) int {
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}
