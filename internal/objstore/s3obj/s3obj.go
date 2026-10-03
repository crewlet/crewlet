// Package s3obj keeps the object store's chunks in an S3-compatible bucket:
// Amazon S3, or anything speaking its API — Cloudflare R2, MinIO, Google Cloud
// Storage's interoperability endpoint, Backblaze B2, Ceph's gateway.
//
// # When to take it
//
// The default backend (internal/objstore/natsobj) keeps every chunk on every
// broker member holding a copy, which is right for a fleet of three or five
// data nodes and wrong for a company whose files outgrow one member's disk.
// A bucket has no such ceiling, keeps its own copies, and takes the files off
// the broker's routes entirely — at the price of a dependency the engine does
// not run and a network round trip per chunk.
//
// # One object per chunk
//
// Each chunk is the key `<prefix><hash>`. A put of a key already held
// replaces it and moves its LastModified, which is the property
// [objstore.Backend.Put] asks for.
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
	// one company.
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

// Backend is the chunks in a bucket.
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
		// sends a trailing checksum on every put, which providers that are
		// not Amazon refuse or mishandle, and buys nothing here: every
		// chunk is checked against its own SHA-256 name on every read.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	b := &Backend{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("s3obj: reach bucket %q: %w", cfg.Bucket, err)
	}
	return b, nil
}

func (b *Backend) key(h objstore.Hash) string { return b.prefix + string(h) }

// Put implements [objstore.Backend].
func (b *Backend) Put(ctx context.Context, h objstore.Hash, data []byte) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(h)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("s3obj: put %s: %w", h, err)
	}
	return nil
}

// Get implements [objstore.Backend].
func (b *Backend) Get(ctx context.Context, h objstore.Hash) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(h)),
	})
	if missing(err) {
		return nil, objstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("s3obj: get %s: %w", h, err)
	}
	defer func() { _ = out.Body.Close() }()
	// BOUNDED one byte past a chunk: a key that holds more than any chunk
	// can is not a chunk, and reading it whole would be a memory bill
	// somebody else wrote.
	data, err := io.ReadAll(io.LimitReader(out.Body, objstore.ChunkSize+1))
	if err != nil {
		return nil, fmt.Errorf("s3obj: read %s: %w", h, err)
	}
	return data, nil
}

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, h objstore.Hash) (time.Time, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(h)),
	})
	if missing(err) {
		return time.Time{}, objstore.ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("s3obj: stat %s: %w", h, err)
	}
	return aws.ToTime(out.LastModified).UTC(), nil
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, h objstore.Hash) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.key(h)),
	})
	if err != nil && !missing(err) {
		return fmt.Errorf("s3obj: delete %s: %w", h, err)
	}
	return nil
}

// List implements [objstore.Backend]: every key under the prefix, a page at a
// time. A key whose remainder is not a content hash is somebody else's object
// and is not visited — the collector must never be handed a name it would
// delete as garbage.
func (b *Backend) List(ctx context.Context, visit func(objstore.Held) error) error {
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3obj: list %q: %w", b.prefix, err)
		}
		for _, obj := range page.Contents {
			h, err := objstore.ParseHash(strings.TrimPrefix(aws.ToString(obj.Key), b.prefix))
			if err != nil {
				continue
			}
			if err := visit(objstore.Held{Hash: h, Written: aws.ToTime(obj.LastModified).UTC()}); err != nil {
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

// missing reports an answer that the key is not there, under each spelling
// the API uses for it: GetObject's NoSuchKey, and HeadObject's bare 404,
// which carries no body to name a code in.
func missing(err error) bool {
	if err == nil {
		return false
	}
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound")
}
