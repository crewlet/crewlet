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
// one a multipart upload of equal parts — so an upload holds a few parts in
// memory and never the object. A body ENDS only where its reader answers
// io.EOF itself ([objstore.Fill]): one cut short fails the put rather than
// being stored as far as it got. A part that fails, or a read, abandons the
// upload, under a context of its own, because an upload nobody completes or
// aborts is invisible to a listing and billed until somebody does — and one a
// process killed mid-upload leaves is what [Backend.Pending] lists for the
// collector to abandon.
//
// # A large upload's parts are in flight together
//
// Up to [partsInFlight] parts of one upload are sent at once, each on a
// connection of its own, while the next is read — and hashed, by the store —
// into a buffer of its own; the parts are independent until the completion
// names them by number. Sent one after another, every part paid a whole round
// trip before the next could start and one connection's throughput carried
// the whole object. The buffers past an upload's first come from ONE ALLOWANCE
// the node's uploads share ([spareParts]), taken only while it has one free,
// so many large uploads at once cost the node a bounded amount of memory and
// each still finishes on the one buffer it holds. A part that fails ends its
// upload AT ONCE — the others in flight cancelled, the body read no further —
// rather than when the upload next needs a buffer, which was after up to
// three more parts of a client's body.
//
// # Every request is bounded
//
// The bucket is reached through the engine's shared transport with no
// client-wide timeout (internal/httpx), which is right only where every
// request is bounded some other way — so every request is: a request that
// moves no body gets [callBudget]; a body sent as one put gets that plus its
// size at [objstore.MiBPace]; a part of an upload gets that plus its size at
// the floor once for every part the upload may keep in flight, because they
// share one link and the floor is the upload's ([partBudget]); and a read is
// bounded by the deadline its caller must carry ([objstore.CheckRead]).
//
// # What it costs the node, measured
//
// `make bench` (BenchmarkBackend, through internal/objstore/objstoretest's
// Bench) against s3fake, at the production [PartBytes] and S3's own page of a
// thousand keys — on four vCPUs of a KVM Intel Xeon at 2.8 GHz WITHOUT SHA
// instructions (it has AES-NI), Go 1.27.2, aws-sdk-go-v2/service/s3 v1.114.0,
// 2026-10-10, two runs on a VM shared with other work:
//
//   - ON LOOPBACK, over https, the NODE'S SIDE of a bucket: the SDK, the
//     signature, TLS and the store's own hashing, with the fake's half of
//     every request on the same CPUs, and none of the provider's — no round
//     trip, no per-prefix request rate, no link. A 64 KiB put took 0.73 ms and
//     larger ones ran at 177–229 MB/s; a whole read — of an object written
//     moments before, held in the fake's memory, as every read figure here
//     is — at 670–1,020 MB/s from 1 MiB up, and 215–245 MB/s verified
//     ([objstore.Store.Open]), which is this CPU's SHA-256 rate; eight
//     verified reads at once at 660–700 MB/s together; a 64 KiB page at a
//     random offset of a 128 MiB object took 0.49 ms — ONE ranged request
//     wherever the page is. A HEAD across ten thousand objects ran 3,200–3,600
//     a second from one goroutine and 19,300–22,700 at 32 in flight, and a
//     listing named 117,000–123,000 objects a second, a thousand to a request.
//   - A ROUND TRIP AWAY — the fake holding every request 20 or 50 ms before it
//     answers (`rtt=`) — every loop of one request at a time is the round
//     trip's, and the node's CPU stops being what it waits on. A HEAD runs 47
//     a second at 20 ms and 19 at 50 for each request in flight: 1,380–1,450
//     and about 610 a second at 32 (what that makes of an audit is
//     internal/objstore/collect's to say, since it decides how many it asks at
//     once). A listing's page costs its round trip and about ten milliseconds
//     of the node's own parsing — 31,400–33,000 names a second at 20 ms,
//     15,600–16,900 at 50 — so a pass over two million objects is one to two
//     minutes. A 64 KiB page at any offset is one request, 21 and 51 ms. And a
//     128 MiB put, eighteen requests, ran at 220–228 MB/s at 20 ms and 153–196
//     at 50 with [partsInFlight] parts in flight — 192–229 on loopback — where
//     one part at a time had run at 121 and 79, and 166 on loopback.
//
// OVER PLAIN HTTP THE SDK SIGNS EVERY BODY: it hashes each byte it uploads to
// put the digest in the signature, where over https it sends UNSIGNED-PAYLOAD
// instead. On this CPU, where the hash is what a put waits on, puts from
// 1 MiB up ran at 105–175 MB/s over http against 177–229 over https; reads
// over http, with no TLS to decrypt, ran at 1.8–2.1 GB/s unverified. Reach a
// bucket over https.
//
// # Measuring a real bucket
//
// BenchmarkBackend's `bucket=real` arm runs only when CREWLET_BENCH_S3_BUCKET
// names a bucket, and otherwise skips, saying so under `go test -v` (which
// `make bench` passes). No real bucket's figures are recorded here: none has
// been measured. The variables are read by the benchmark and never by the
// engine:
//
//   - CREWLET_BENCH_S3_BUCKET, the bucket; setting it turns the arm on.
//   - CREWLET_BENCH_S3_REGION, required with it.
//   - CREWLET_BENCH_S3_ENDPOINT, the S3 API's base URL; empty is Amazon's own.
//   - CREWLET_BENCH_S3_PATH_STYLE, `true` for MinIO and most gateways.
//   - CREWLET_BENCH_S3_PREFIX, what every key is put under; empty is
//     `crewlet-bench/`.
//
// NO CREDENTIAL IS ONE OF THEM: the arm takes the SDK's own chain —
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, AWS_PROFILE, a web identity, the
// instance's role — and honours AWS_CA_BUNDLE as [Open] does. For example:
//
//	CREWLET_BENCH_S3_BUCKET=acme-bench CREWLET_BENCH_S3_REGION=eu-west-1 \
//	  make bench BENCH_PKGS=./internal/objstore/s3obj BENCH_PATTERN=Backend/bucket=real
//
// Every object the arm writes it deletes; a run killed part way can leave
// objects and an unfinished upload under the prefix, and a lifecycle rule
// aborting incomplete uploads removes the second. Run it from inside the
// bucket's region: each case moves as much as the link carries in its
// benchtime, which at the default is gigabytes.
package s3obj

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
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
// of each buffer an upload holds a part in — one per part in flight, so at
// most [partsInFlight] of them, 32 MiB, for one upload however large.
//
// EIGHT MEBIBYTES. S3 takes parts of five at the least, all but the last;
// Cloudflare R2 takes them only all of one size, which is why every part is
// this one; and an upload is ten thousand parts at most — eighty gibibytes,
// eighty times the largest file the tracker keeps. A body under it never
// becomes a multipart upload at all.
const PartBytes = 8 * objstore.MiB

// maxParts is the most parts S3 assembles into one object.
const maxParts = 10_000

// partsInFlight is the most parts of one upload sent at once.
//
// FOUR, which is where a bucket a round trip away stops being what an upload
// waits on and the node's own hashing starts to be. A part costs its round
// trip on top of its bytes, so one part at a time moved a 128 MiB object at
// 121 MB/s with the bucket 20 ms away and 79 at 50, against 166 on loopback
// (the `rtt=` arms of BenchmarkBackend; the package doc has the machine).
// Four in flight overlap the round trips — 220–228 and 153–196 MB/s — and
// more would only queue parts behind the one goroutine that reads and hashes
// the body, which on that CPU, without SHA instructions, hashes 324–353 MB/s
// (internal/objstore's BenchmarkHashOf). It is also four connections'
// throughput from a provider that limits each connection's, and 32 MiB of
// parts held by the largest upload.
const partsInFlight = 4

// spareParts is the most part buffers the node's uploads hold BEYOND the one
// each holds for its own first part — 96 MiB, room for four uploads at full
// width at once.
//
// AN ALLOWANCE, NEVER A QUEUE: an upload takes a spare only while one is free
// and otherwise goes on with the buffers it has, so the fifth large upload at
// once runs a part at a time rather than waiting for memory, and none ever
// waits on another to finish. What bounds the memory is then one part per
// upload in progress — what the uploads before this allowance held — plus
// this.
const spareParts = 3 * partsInFlight

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

// sendBudget bounds a request carrying n bytes that has the link to itself —
// a body that fit one part, sent as one put: [callBudget] for the request and
// [objstore.MiBPace] for every mebibyte of its body.
func sendBudget(n int64) time.Duration { return callBudget + objstore.PaceFor(n) }

// partBudget bounds the request carrying one part of size bytes: [callBudget]
// for the request, and [objstore.MiBPace] for every mebibyte of the part
// [partsInFlight] times over.
//
// THE PART HAS A SHARE OF THE LINK, NOT THE LINK: an upload's parts in flight
// cross one link to the bucket together, and the floor is the UPLOAD's — the
// slowest an object may cross a link, which one part at a time held it to. A
// part given only its own bytes at the floor, on a link the upload crosses at
// the floor, misses its deadline once others share it, and the upload is
// abandoned where one part at a time would have finished.
//
// EVERY PART AN UPLOAD MAY KEEP IN FLIGHT, not the parts in flight when it is
// sent: an upload sends parts behind one already on its way — its first goes
// alone and three more join it as fast as the body arrives — and each one
// that joins slows the parts before it for the rest of their way. A part's
// share is at worst one in [partsInFlight] while the link divides fairly
// between the connections carrying them, which is what it is given. What it
// costs is the notice a part that has stopped moving gets: sixteen and a half
// minutes for a full part, where the link to itself was four and a half.
func partBudget(size int64) time.Duration { return sendBudget(size * partsInFlight) }

// Backend is the objects in a bucket.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string

	// partBytes is the size of every part of a multipart upload but its
	// last: [PartBytes], which Open sets and nothing in the engine changes.
	//
	// A FIELD, and the one every use reads, so this package's own tests can
	// run the multipart path at a size they can afford (export_test.go):
	// every branch of it — one put below a part, equal parts above, the
	// abort at or inside a part — turns on sizes RELATIVE to the part, and at
	// eight mebibytes the cases moved hundreds of mebibytes through a fake
	// bucket to reach them. A use that read the constant instead would run
	// at one size under test and another in production.
	partBytes int

	// parts recycles part buffers of partBytes between uploads, so a node
	// storing many small objects does not allocate a part for each.
	//
	// A BUFFER GOES BACK ONLY AFTER A PUT THAT SUCCEEDED. A request that
	// failed may still have its body read by the transport after it returns
	// — the standard library says so of RoundTrip — and a buffer handed
	// straight to the next upload would be written under that read.
	parts *sync.Pool

	// spare is the node's allowance of part buffers beyond each upload's
	// first ([spareParts]): a token in it is one taken.
	spare chan struct{}
}

// cutParts sizes every part but the last at n bytes, and the buffers that
// hold them.
func (b *Backend) cutParts(n int) {
	b.partBytes = n
	b.parts = &sync.Pool{New: func() any {
		buf := make([]byte, n)
		return &buf
	}}
	b.spare = make(chan struct{}, spareParts)
}

// takeSpare takes one buffer of the node's spare allowance, or answers false
// when none is free — never waiting for one.
func (b *Backend) takeSpare() bool {
	select {
	case b.spare <- struct{}{}:
		return true
	default:
		return false
	}
}

// returnSpares gives n buffers back to the node's spare allowance.
//
// AT THE UPLOAD'S END, whatever became of it, because the allowance counts
// memory rather than vouching for it: a buffer a failed part's request may
// still be read from is dropped rather than pooled, and is the collector's
// once the transport lets go — a moment over the allowance, never a buffer
// written under a read.
func (b *Backend) returnSpares(n int) {
	for range n {
		<-b.spare
	}
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
	b.cutParts(PartBytes)
	headCtx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	if _, err := client.HeadBucket(headCtx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("s3obj: reach bucket %q: %w", cfg.Bucket, err)
	}
	return b, nil
}

func (b *Backend) key(name string) string { return b.prefix + name }

// Put implements [objstore.Backend].
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	r = objstore.ContextReader(ctx, r)
	buf := b.parts.Get().(*[]byte)
	n, end, err := objstore.Fill(r, *buf)
	switch {
	case err != nil:
		return fmt.Errorf("s3obj: read %s: %w", name, err)
	case !end:
		// THE UPLOAD OWNS THE BUFFER from here, beside the others it
		// takes, and gives them all back when it succeeds.
		return b.putParts(ctx, name, r, buf, m)
	}
	if err := b.putObject(ctx, name, (*buf)[:n], m); err != nil {
		return err
	}
	b.parts.Put(buf)
	return nil
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
// first part is already in first. Up to [partsInFlight] parts are sent at
// once, each from a buffer of its own, while the next is read into another:
// one whose part has landed, a spare from the node's allowance while one is
// free, or — with neither — the buffer of the next part to land, once it has.
//
// A PART THAT FAILS ENDS THE UPLOAD AT ONCE: its sender ends the upload's
// context with the failure as the cause, which cancels every other part in
// flight and stops the read of the body at its next piece, and nothing is
// sent once the upload has ended. An upload that heard of a failure only when
// it next needed a buffer read the client's body on and sent up to three more
// parts — minutes of a slow client's body, and three more part buffers held
// — for an upload already lost, and told its caller last.
//
// EVERY FAILURE ABANDONS THE UPLOAD, under a context the caller's cannot end:
// the failure being cleaned up after is often the caller giving up, and an
// abort inheriting that context would abort nothing. And only ONCE EVERY PART
// SENT HAS ANSWERED, the rest cancelled first: S3 documents that a part still
// uploading when its upload is aborted may land anyway, holding storage no
// listing shows until somebody aborts the upload a second time.
func (b *Backend) putParts(ctx context.Context, name string, r io.Reader, first *[]byte,
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

	// parts is the life of the upload's part requests and of its read of the
	// body, ended by the first part that fails — that part's failure its
	// cause — or by the caller's own context.
	parts, end := context.WithCancelCause(ctx)
	r = objstore.ContextReader(parts, r)
	// BUFFERED FOR EVERY PART THAT CAN BE IN FLIGHT, so a sender never
	// waits to be heard and always returns.
	landed := make(chan sentPart, partsInFlight)
	var (
		held     = []*[]byte{first} // every buffer this upload holds
		idle     []*[]byte          // held buffers whose part has landed
		inFlight int
		spares   int
		done     []types.CompletedPart
	)
	defer func() {
		end(nil)
		for ; inFlight > 0; inFlight-- {
			<-landed
		}
		b.returnSpares(spares)
		if err == nil {
			for _, buf := range held {
				b.parts.Put(buf)
			}
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

	// ended is why the upload stopped before its body did: the first part
	// that failed, or the caller giving up.
	ended := func() error {
		cause := context.Cause(parts)
		if _, failed := errors.AsType[*partError](cause); failed {
			return cause
		}
		return fmt.Errorf("s3obj: put %s: %w", name, cause)
	}
	// send puts part number on its way from the first size bytes of buf, its
	// answer handed to landed.
	send := func(number int32, buf *[]byte, size int) {
		inFlight++
		go func() {
			sent, serr := call(parts, partBudget(int64(size)), func(ctx context.Context) (*s3.UploadPartOutput, error) {
				return b.client.UploadPart(ctx, &s3.UploadPartInput{
					Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: upload,
					PartNumber: aws.Int32(number), Body: bytes.NewReader((*buf)[:size]),
					ContentLength: aws.Int64(int64(size)),
				})
			})
			p := sentPart{number: number, buf: buf, err: serr}
			if serr != nil {
				// THE UPLOAD ENDS BEFORE THE ANSWER LANDS, so whatever
				// hears of the end first — the read it stops, or the loop
				// taking this answer or a cancelled sibling's — finds this
				// failure, or an earlier one, as its cause.
				end(&partError{number: number, name: name, err: serr})
			} else {
				p.etag = sent.ETag
			}
			landed <- p
		}()
	}
	// settle takes one part's answer: its tag for the completion and its
	// buffer for a later part — or, from a part that failed, the upload's end.
	settle := func(p sentPart) error {
		inFlight--
		if p.err != nil {
			return ended()
		}
		done = append(done, types.CompletedPart{ETag: p.etag, PartNumber: aws.Int32(p.number)})
		idle = append(idle, p.buf)
		return nil
	}
	// reap settles every part that has answered, waiting for none: a landed
	// part's buffer is filled next rather than a spare taken from the node,
	// and a part that failed ends the upload before its body is read on.
	reap := func() error {
		for {
			select {
			case p := <-landed:
				if lerr := settle(p); lerr != nil {
					return lerr
				}
			default:
				return nil
			}
		}
	}

	buf, size, last := first, len(*first), false
	for number := int32(1); ; number++ {
		if number > maxParts {
			return fmt.Errorf("s3obj: %s is longer than the %d parts of %d bytes one "+
				"upload can hold", name, maxParts, b.partBytes)
		}
		if parts.Err() != nil {
			return ended()
		}
		send(number, buf, size)
		if last {
			break
		}
		if lerr := reap(); lerr != nil {
			return lerr
		}
		for buf = nil; buf == nil; {
			switch {
			case len(idle) > 0:
				buf, idle = idle[len(idle)-1], idle[:len(idle)-1]
			case len(held) < partsInFlight && b.takeSpare():
				spares++
				buf = b.parts.Get().(*[]byte)
				held = append(held, buf)
			default:
				if lerr := settle(<-landed); lerr != nil {
					return lerr
				}
			}
		}
		var rerr error
		if size, last, rerr = objstore.Fill(r, *buf); rerr != nil {
			if parts.Err() != nil {
				return ended()
			}
			return fmt.Errorf("s3obj: read %s: %w", name, rerr)
		}
		if size == 0 {
			// THE BODY ENDED WHERE A PART DID, so there is no part
			// left to send.
			break
		}
	}
	for inFlight > 0 {
		if lerr := settle(<-landed); lerr != nil {
			return lerr
		}
	}
	// IN THE ORDER OF THEIR NUMBERS, which a completion must name them in,
	// where they landed in whatever order their requests answered.
	slices.SortFunc(done, func(a, b types.CompletedPart) int {
		return cmp.Compare(aws.ToInt32(a.PartNumber), aws.ToInt32(b.PartNumber))
	})
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

// sentPart is one part's answer: its number, the tag the bucket gave it, and
// the buffer it was sent from.
type sentPart struct {
	number int32
	etag   *string
	buf    *[]byte
	err    error
}

// partError is a part of an upload that failed: the cause the upload's
// context is ended with, so whatever hears of the end reports this part
// rather than a sibling the end cancelled.
type partError struct {
	number int32
	name   string
	err    error
}

func (e *partError) Error() string {
	return fmt.Sprintf("s3obj: store part %d of %s: %v", e.number, e.name, e.err)
}

func (e *partError) Unwrap() error { return e.err }

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
	var undated undatedCount
	for pages.HasMorePages() {
		page, err := call(ctx, callBudget, func(ctx context.Context) (*s3.ListObjectsV2Output, error) {
			return pages.NextPage(ctx)
		})
		if err != nil {
			return fmt.Errorf("s3obj: list %q: %w", b.prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.LastModified == nil {
				undated.add(aws.ToString(obj.Key))
				continue
			}
			if err := visit(objstore.Info{
				Name:    strings.TrimPrefix(aws.ToString(obj.Key), b.prefix),
				Size:    aws.ToInt64(obj.Size),
				Written: obj.LastModified.UTC(),
			}); err != nil {
				return err
			}
		}
	}
	return undated.err("objects", "LastModified",
		"an S3-compatible gateway must date the objects it lists for the collector to delete one")
}

// ListRange implements [objstore.Ranged]: ListObjectsV2 from StartAfter, a
// page of a thousand keys at a time, until a key passes through.
//
// IN THE BYTE ORDER OF THE KEYS, which is how a general-purpose bucket lists
// them — and with the prefix common to every key, the order of the names. A
// DIRECTORY BUCKET (S3 Express One Zone, whose names end in `--x-s3`) lists in
// no order and takes no StartAfter, so it answers [objstore.ErrUnordered]
// without a request, and its caller asks about each object instead.
//
// The page that passes through is read whole, up to a thousand keys past it:
// S3 has no end to a range, so ending one costs at most that. A range holding
// more names than a page costs a request for each page it spans, which is why
// the audit cuts its ranges short of one.
func (b *Backend) ListRange(ctx context.Context, after, through string, visit func(objstore.Info) error) error {
	if strings.HasSuffix(b.bucket, directoryBucketSuffix) {
		return fmt.Errorf("%w: %q is a directory bucket, which lists in no order",
			objstore.ErrUnordered, b.bucket)
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(b.bucket), Prefix: aws.String(b.prefix)}
	if after != "" {
		in.StartAfter = aws.String(b.key(after))
	}
	last := b.key(through)
	pages := s3.NewListObjectsV2Paginator(b.client, in)
	var undated undatedCount
	for pages.HasMorePages() {
		page, err := call(ctx, callBudget, func(ctx context.Context) (*s3.ListObjectsV2Output, error) {
			return pages.NextPage(ctx)
		})
		if err != nil {
			return fmt.Errorf("s3obj: list %q after %q: %w", b.prefix, after, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if key > last {
				return undated.err("objects", "LastModified",
					"an S3-compatible gateway must date the objects it lists")
			}
			if obj.LastModified == nil {
				undated.add(key)
				continue
			}
			if err := visit(objstore.Info{
				Name:    strings.TrimPrefix(key, b.prefix),
				Size:    aws.ToInt64(obj.Size),
				Written: obj.LastModified.UTC(),
			}); err != nil {
				return err
			}
		}
		// A PAGE ENDING ON THE RANGE'S LAST NAME ends the range: asking for
		// the next page to learn so is a request for nothing. A range holding
		// fewer names than a page — the audit cuts its ranges to — ends inside
		// its first page, on the first key past it.
		if n := len(page.Contents); n > 0 && aws.ToString(page.Contents[n-1].Key) >= last {
			break
		}
	}
	return undated.err("objects", "LastModified",
		"an S3-compatible gateway must date the objects it lists")
}

// directoryBucketSuffix is what every S3 Express One Zone bucket's name ends
// in — which is how one is known without a request, and how the SDK itself
// knows to sign its requests with S3 Express's own session.
const directoryBucketSuffix = "--x-s3"

// undatedCount is the entries one listing left out because the bucket gave
// no date for them — see [objstore.ErrUndated] for why they are left out
// rather than handed on dated at the zero time.
type undatedCount struct {
	n     int
	first string
}

func (u *undatedCount) add(key string) {
	if u.n == 0 {
		u.first = key
	}
	u.n++
}

// err is the listing's answer once everything it could date was handed on:
// nil, or [objstore.ErrUndated] naming how many entries of what went without
// the field, and the first of them.
func (u undatedCount) err(what, field, remedy string) error {
	if u.n == 0 {
		return nil
	}
	return fmt.Errorf("%w: the bucket answered %d %s with no %s (the first %q), "+
		"which the engine leaves alone because it cannot judge their age; %s",
		objstore.ErrUndated, u.n, what, field, u.first, remedy)
}

// Pending implements [objstore.Backend]: every multipart upload begun under
// the prefix and neither completed nor aborted, a page at a time, each named
// by what follows the prefix and dated by when it was begun.
//
// THE BUCKET KEEPS THEM, AND BILLS FOR THEM, UNTIL SOMEBODY ABORTS THEM: a put
// abandons its own upload when it fails ([Backend.Put]), so one is left only
// where the process died mid-upload — and then nothing else will ever find it,
// since no listing of the bucket's objects shows an upload. A bucket's own
// lifecycle rule for incomplete uploads does the same where an operator set
// one; this does not rely on one having been.
//
// It needs `s3:ListBucketMultipartUploads` on the bucket, which an identity
// granted only the object verbs lacks — the listing then fails, and says so.
func (b *Backend) Pending(ctx context.Context, visit func(objstore.Pending) error) error {
	pages := s3.NewListMultipartUploadsPaginator(b.client, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.prefix),
	})
	var undated undatedCount
	for pages.HasMorePages() {
		page, err := call(ctx, callBudget, func(ctx context.Context) (*s3.ListMultipartUploadsOutput, error) {
			return pages.NextPage(ctx)
		})
		if err != nil {
			return fmt.Errorf("s3obj: list the uploads under %q: %w", b.prefix, err)
		}
		for _, u := range page.Uploads {
			// NO DATE IS NOT "LONG AGO": handed on at the zero time, an
			// upload still in flight would be abandoned under its put.
			if u.Initiated == nil {
				undated.add(aws.ToString(u.Key))
				continue
			}
			if err := visit(objstore.Pending{
				Name:    strings.TrimPrefix(aws.ToString(u.Key), b.prefix),
				ID:      aws.ToString(u.UploadId),
				Started: u.Initiated.UTC(),
			}); err != nil {
				return err
			}
		}
	}
	return undated.err("uploads", "Initiated",
		"give the bucket a lifecycle rule that aborts incomplete multipart uploads "+
			"(AbortIncompleteMultipartUpload), or use a gateway that dates them")
}

// Abandon implements [objstore.Backend]: an abort of the upload, which deletes
// every part it holds. An upload that is gone — completed, or aborted already
// — answers NoSuchUpload, which is what abandoning it was for. It needs
// `s3:AbortMultipartUpload`.
func (b *Backend) Abandon(ctx context.Context, p objstore.Pending) error {
	if p.ID == "" {
		return fmt.Errorf("s3obj: abandon %q: an upload names no upload id", p.Name)
	}
	_, err := call(ctx, callBudget, func(ctx context.Context) (*s3.AbortMultipartUploadOutput, error) {
		return b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(b.bucket), Key: aws.String(b.key(p.Name)), UploadId: aws.String(p.ID),
		})
	})
	if err != nil && !missing(err) {
		return fmt.Errorf("s3obj: abandon the upload %s of %s: %w", p.ID, p.Name, err)
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
