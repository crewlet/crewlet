// Package s3obj keeps the object store's objects in an S3-compatible bucket:
// Amazon S3, or anything speaking its API (R2, MinIO, GCS interop, B2,
// Ceph's gateway).
//
// # When to take it
//
// The default backend (internal/objstore/natsobj) keeps every object on every
// broker member holding a copy: wrong for a company whose files outgrow one
// member's disk. A bucket has no such ceiling and takes the files off the
// broker's routes, at the price of a dependency the engine does not run and a
// round trip per request.
//
// # One bucket, several companies: only with credentials scoped to a prefix
//
// A prefix keeps two companies' keys from colliding in one bucket, and that is
// all it does. Each company's nodes hold a credential, and a credential that
// reaches the whole bucket reads, overwrites and deletes every other company's
// files as readily as its own. So a bucket is shared only where each
// company's credential is scoped to its own prefix (an IAM policy on
// `arn:aws:s3:::<bucket>/<prefix>*`, a MinIO or SeaweedFS policy on the same
// path), and such a credential has no right over the bucket itself. That is
// why the node checks its bucket with a probe under the prefix (probe.go)
// rather than a HeadBucket, which needs s3:ListBucket on the whole bucket and
// refused exactly the credential that makes sharing safe. One bucket per
// company is the simpler, recommended isolation.
//
// # The AWS SDK, the keys and what the probe proves
//
// The backend uses the AWS SDK because SigV4 and the regional endpoint rules
// are what a hand-rolled client gets wrong. Its credentials are the static
// pair in [Config] or else the SDK's own chain, and `AWS_CA_BUNDLE` is added
// to the roots for the bucket's requests only, which all go through
// internal/httpx and never the SDK's own pool. The open's probe (probe.go)
// stores and deletes an object through the engine's path,
// failing the boot if it cannot, then proves with presigned puts that the
// provider refuses a mismatched `x-amz-checksum-sha256` (BadDigest) and a
// second put under `If-None-Match: *` (412). Only a provider doing both offers
// [Backend.PresignPut] through DirectUpload, and only one proven to check
// checksums gets the header on single-part puts and a digest on [Backend.Stat].
//
// # One object per name, streamed
//
// Each name is the key `<prefix><name>`, and every name the store writes is
// `files/<key>` ([objstore.Key.Name]), a corner of the bucket an empty prefix
// still does not share with another application's UUID-named objects.
//
// LastModified is what [objstore.Info.Written] reports, and AN OBJECT PUT IN
// PARTS CARRIES ITS UPLOAD'S START there, not its completion: Amazon dates an
// object assembled from parts by when its multipart upload was created. That
// contract therefore promises only an instant between the two, the earliest
// the object might have been stored; s3fake dates an upload the same way.
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
// connection of its own, while the next is read (hashed by the store)
// into a buffer of its own; the parts are independent until the completion
// names them by number. Sent one after another, one connection's throughput
// carried the whole object. The buffers past an upload's first come from ONE ALLOWANCE
// the node's uploads share ([spareParts]), taken only while it has one free,
// so many large uploads cost the node bounded memory and each still finishes on
// the one buffer it holds. A failed part ends its upload AT ONCE, the
// others cancelled and the body read no further.
//
// # So are a large read's ranges
//
// A read that may run past one part — a whole object, or the rest of one — is
// read as consecutive part-sized ranges handed out strictly in order: the git
// cache's largest bundle at the tail of a sync, a person's download of a large
// file, a backup's copy of every object. Read as one stream it moved at one
// connection's rate, and a provider limits each connection's. The FIRST range
// is streamed straight through and says how large the object is, so an object
// the first range holds whole — every object of a part or less — costs the one
// request and the memory a read cost before there were ranges. Every later
// range is held to the first one's ETag (If-Match): a bucket written into
// while it was read fails the read rather than stitching two objects
// together, and an object the bucket gives no tag is refused past its first
// range.
//
// A READ IS AS WIDE AS ITS READER. Ranges are read ahead into buffers from the
// SAME allowance the uploads draw on ([spareParts]), never waited for, and
// handed out a piece at a time as they land, so the store's stall watchdog
// sees a moving read moving. A read holds one to begin with and then as many
// as its reader keeps busy, up to [partsInFlight]: more while the reader waits
// on the bucket, one fewer once a range read ahead had landed whole before
// the reader got there. A download paced to a person's link so holds one
// buffer for as long as it runs; a read that finds no buffer free streams
// its ranges one after another, taking one as soon as one is free. EVERY
// RANGE IS ASKED FOR AHEAD of the moment its answer is needed — a buffer
// freeing for it, or the reader reaching it — by twice the bucket's first byte
// at the reader's pace, its answer waiting on its connection meanwhile: asked
// only once its buffer was free, each range left a whole first byte
// between ranges. A range that fails ends
// the ranges after it at once, every range before it still handed out, and
// the read asks for nothing more. Closing a read cancels every range in
// flight, waits for each, and gives its buffers back before it returns.
//
// # Every request is bounded
//
// The bucket is reached through a clone of the engine's shared transport with
// no client-wide timeout (internal/httpx), which is right only where every
// request is bounded some other way — so every request is: a request that
// moves no body gets [callBudget]; a body sent as one put gets that plus its
// size at [objstore.MiBPace]; a part of an upload gets that plus its size at
// the floor once for every part the upload may keep in flight, because they
// share one link and the floor is the upload's ([partBudget]); and a read —
// every range of it — is bounded by the deadline its caller must carry
// ([objstore.CheckRead]).
//
// A REQUEST'S ATTEMPTS END AT ITS DEADLINE, OR A MINUTE INTO NOTHING BUT
// FAILURES: one the bucket throttles, fails with a server error, or cannot be
// reached for is asked again, after a jittered backoff ([fullJitter]), until
// it is answered or its deadline passes — never for a count of attempts, which
// `AWS_MAX_ATTEMPTS` cannot put back — and a request its deadline ends names
// the bucket's last answer beside it. But one the bucket answers with nothing
// but FAILURES — server errors that are no throttle, or no answer at all — is
// given up within [judge.giveUpAfter], a minute from the first ([judge]): at a
// bound drawn for its span of failures between forty seconds and the minute,
// its last attempt sent at that bound, so the requests that began failing
// together do not all ask again in one instant at the minute; and its caller
// is told how long the bucket failed every attempt and what it last answered.
// A throttle answered to the request starts the span again, since a bucket
// asking the node to slow down is up. An upload in parts whose part is given
// up is answered up to [callBudget] later, once its unfinished upload has been
// abandoned — asked once instead, a bucket that recovered inside those seconds
// would hold the parts, billed, for the collector's day of grace. Only a
// refusal asking again cannot change is final at once: a client error, a
// certificate the node does not trust. A READ'S deadline is the longest, hours
// for a large object, so what ends a read whose first request the bucket keeps
// failing is that request's bound or the store's stall watchdog
// ([objstore.Stall]), which watches the open as it watches each piece after,
// whichever comes first, the bucket's last answer named either way.
//
// # A write that stops moving
//
// AN ATTEMPT THAT HAS NOT MOVED FOR [objstore.Stall] — the minute a read
// already waits on its backend, and the minute a request the bucket keeps
// failing is given up within — IS ENDED, whatever its deadline
// ([watchedClient]): a deadline sized for a write's bytes at the floor is
// minutes long — four and a half for a put of up to a part, sixteen and a half
// for a part, five for a completion — and a bucket that stops taking a write
// held the person who sent it for all of them. Each attempt is watched from
// where the SDK hands it to the HTTP client — after the throttle gate and the
// signature, so no wait in line at the gate counts — and a request whose
// attempt was ended is given up at once, by the KIND of its failure, its
// caller told the bucket stopped moving it: it has gone a whole minute with
// nothing moving, which the minute of nothing but failures already meets, and
// asked again it would hold the person another minute.
//
// MOVED IS COUNTED AT THE TRANSPORT, never estimated from a rate: the
// transport taking bytes of the body, the socket's count of bytes the far end
// has not acknowledged changing between two samples five seconds apart, a
// `100 Continue`, the answer's headers, and every byte of an answer the SDK
// reads itself — so a write moving at any pace the node's socket can see is
// never ended, however slow. The body alone is not enough: kernel and bucket
// buffers hold megabytes, so a slow-link write went minutes between two reads
// of its body. The socket alone misses an answer, and a body still being
// copied into an empty buffer. Time spent inside a read of the body is the body's source's
// and is never counted against the bucket.
//
// THE ONE STOP IT CANNOT SEE is a bucket whose SOFTWARE reads a write slower
// than its kernel took it, or a proxy in front of it that buffers an upload
// (nginx's default): what they took and have not read is acknowledged already,
// so nothing the node can see moves while it drains, and a write of which they
// hold more than a minute's reading is ended a minute after the node last saw
// it move (the figures are in the object-store concepts page). No rule the
// node can apply keeps it and still ends a hung bucket; a slow NETWORK in
// front of a healthy bucket is never ended.
//
// THE BUCKET IS SPOKEN TO OVER HTTP/1.1 ALONE ([bucketClient]), so a socket
// is one request's: over HTTP/2 another's acknowledgements would read
// as this one's movement. S3's API is specified over HTTP/1.1. A connection
// whose count cannot be read is never ended by the watch, only by its
// deadline; that never happens on the platforms the engine builds for, and
// where the default transport is one internal/httpx cannot clone (a test
// harness, never the engine's own run) the bucket is spoken to in whatever it
// speaks.
//
// # A throttle slows the whole client
//
// One backend serves every reader and writer of the node's `files/` prefix —
// the git cache's syncs, the collector's audit and deletions, the backup's
// copy, the API's files — so the moment the bucket answers one request with a
// throttle (S3's `503 SlowDown`, a gateway's `429`), the client's [gate]
// slows EVERY request it sends: it lets out at once seven tenths of the
// node's width — the most attempts it has had out at once since its last cut,
// whether or not they were out when the throttle came back, so a throttle
// answered to a lone HEAD between two syncs holds the next sync back no more
// than one answered at its start — the rest waiting in line under their own
// contexts, and one more each round the bucket answers. It is lifted once the
// bucket has gone [throttleQuiet] without a throttle, by a timer the throttle
// sets, whether or not the node sends anything meanwhile.
// A 503 naming a gateway's own failure is no throttle ([isThrottle]): it is
// asked again like any server error and slows nothing else. S3 Express's
// session request — what a directory bucket's every request is signed with,
// asked for by the SDK from inside a request already let out — is never held
// at the gate ([sessionOperation]): held, it would wait behind its own waiter.
//
// THERE IS NO CLIENT-WIDE RETRY QUOTA: the SDK's spent five of five hundred
// tokens a retry, so under a sustained `SlowDown` every request after the
// hundredth retry failed at once. What bounds the requests asking again is
// who sends them: every caller but the API's files is a closed loop of its
// own width, so a retry holds the slot its request held; a person's upload or
// download is bounded by its own deadline, and by the gate once the bucket
// has throttled — and a server error that is no throttle is asked again by
// every request open at the time, each at the backoff, for no longer than a
// minute after its first failure or its deadline, whichever is first
// ([newRetryer]).
//
// # A bucket with no room is full, never failing
//
// Amazon S3 holds whatever it is sent; a self-hosted gateway does not. MinIO
// at its free-space floor or a bucket's quota, and Ceph's gateway out of space
// or past a quota, refuse a write for want of room ([fullCodes], any `507`),
// the same way until an operator makes room. So [Backend.Put] answers such a
// refusal — on the put, a part, the completion or the upload's start — as the
// store having none ([objstore.Full]), naming the provider's room
// ([FullRemedy]); read as the bucket failing, every surface invited a retry of
// the same bytes into the same bucket. The remedy is never the nats bucket's
// "move the files to a bucket", which would send an operator already on one to
// migrate to the store that refused; and the write is not asked again.
//
// # What it counts
//
// Into the node's recorder ([Config.Metrics]), by S3 `operation` — the API's
// own name for each request, a closed set ([operations]) — and never by key,
// bucket or prefix:
//
//   - `crewlet.objects.s3.attempts` counts every attempt the bucket was sent,
//     by what it made of it: `answered`, `throttled`, `failed` (any other
//     answer), `unreached` (none at all), `stalled` (the bucket stopped
//     moving it for a minute: took no more of its body, acknowledged none of
//     it at the node's socket and sent none of its answer; the request is
//     given up) or `ended` (its own context ended it in flight). The session
//     request S3 Express signs with is counted too, and so is an attempt
//     whose session could not be had, as `unreached`.
//   - `crewlet.objects.s3.request.attempts` observes every request once as it
//     ends, at how many attempts it took, by how it ended: `answered`,
//     `given_up` (nothing but failures, for up to a minute, or an attempt
//     stalled for one), `ended` (its deadline or caller, after an attempt),
//     `unsent` (before its first, observed at zero) or `failed` (a failure
//     asking again cannot change).
//   - `crewlet.objects.s3.gate.limit` is how many attempts the gate lets out
//     at once while a throttle holds the node back, zero while nothing does.
//   - `crewlet.objects.s3.gate.waiting` is how many attempts wait at the gate.
//     Both gauges are set only when they change.
//
// The outcomes are read by the gate's own predicates ([isThrottle],
// [answered]), so what the counter calls a throttle is what cut the gate. An
// operation's RETRIES are its attempts less its requests, plus the requests
// counted `unsent`, which is why a request that never got its turn has an
// outcome of its own: the histogram's first bucket holds zero and one together.
//
// # What it costs the node
//
// `make bench` (BenchmarkBackend, through internal/objstore/objstoretest's
// Bench) runs against s3fake at the production [PartBytes]; it measures the
// node's side of a bucket, never a provider's. What its figures fix:
//
//   - A round trip away, a loop of one request at a time is the round trip's,
//     so a large put sends [partsInFlight] parts at once (one part at a time ran
//     at about half the rate at 50 ms).
//   - Against a connection capped near S3's 80 MB/s, a whole read in ranges ran
//     at 219-353 MB/s where one GetObject ran at 80. A range asked for only once
//     its buffer was free left a first byte between ranges, so each is asked
//     ahead. A read holds only as many buffers as its reader keeps busy: four
//     held by a download paced to a person's link took a third of the allowance
//     for hours.
//   - A throttle before a burst (BenchmarkAThrottledBurst) costs the gate 2.7 s
//     against 2.4 s unthrottled, where the SDK's adaptive mode took 9.6 s.
//
// OVER PLAIN HTTP THE SDK SIGNS EVERY BODY, hashing each byte it uploads where
// https sends UNSIGNED-PAYLOAD, and puts ran at 105-175 MB/s against 177-229
// over https on a CPU without SHA instructions. Reach a bucket over https.
//
// # Measuring a real bucket
//
// BenchmarkBackend's `bucket=real` arm, and every Gitlet benchmark's arm over
// the bucket (internal/gitlet/cache's BenchmarkColdSync, internal/gitlet/host's
// BenchmarkPushSmall and BenchmarkCloneCold, internal/gitlet/query's
// BenchmarkCodeRead), run only when CREWLET_BENCH_S3_BUCKET names a bucket, and
// otherwise skip, saying so under `go test -v` (which `make bench` passes).
// The variables are read by s3fake.RealBucket — the one parser of them, which
// every benchmark that measures a real bucket calls, so each reads them
// alike — and never by the engine:
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
// instance's role — and honours AWS_CA_BUNDLE as [Open] does.
//
// Every object the arm writes it deletes; a run killed part way can leave
// objects and an unfinished upload under the prefix, and a lifecycle rule
// aborting incomplete uploads removes the second. Run it from inside the
// bucket's region: each case moves gigabytes at the default benchtime.
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
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
	// Prefix is prepended to every key: an object is
	// `<prefix>files/<key>`. Companies sharing one bucket are kept apart
	// only by credentials scoped to their own prefix; the prefix alone
	// keeps their keys from colliding and nothing more (see the package
	// doc).
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
	// Metrics is the node's recorder, which the client counts its attempts,
	// its requests and its gate into from its first request — the check of
	// the bucket at open (see "What it counts"). Nil counts nothing: a test,
	// the backup's benchmark.
	Metrics *metrics.Recorder
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

// partsInFlight is the most parts of one upload sent at once, and the most
// ranges of one read read ahead at once, each in a buffer of its own: a read
// has at most twice this many requests at the bucket.
//
// FOUR, where a bucket a round trip away stops being what an upload waits on
// and the node's hashing starts to be: one part at a time moved 128 MiB at
// 121 MB/s with the bucket 20 ms away (166 on loopback); four overlap the
// round trips (220-228), and more would only queue behind the one goroutine
// that hashes the body (324-353 MB/s, internal/objstore's BenchmarkHashOf).
// For a read, memory holds it at four: 32 MiB of buffers, and three such
// reads hold the node's whole allowance ([spareParts]).
const partsInFlight = 4

// spareParts is the most part buffers the node's uploads hold BEYOND each
// one's first, together with every buffer its reads hold: 96 MiB, four
// uploads or three reads at full width at once.
//
// AN ALLOWANCE, NEVER A QUEUE, and ONE FOR BOTH DIRECTIONS (one node's
// memory): a transfer takes a spare only while one is free and otherwise goes
// on with the buffers it has, so none waits on memory or another transfer.
// TAKEN AS FAST AS IT IS USED: an upload once its part in flight is still on
// its way when the body has filled the next, a read once its reader has
// waited on the bucket ([wideRead.fit]). A slow download holds ONE spare; a
// read taking four regardless let three hold the whole allowance.
const spareParts = 3 * partsInFlight

// callBudget bounds a request that carries no object's bytes: a head, a
// delete, a batch deletion — whose body is at most a thousand keys, a hundred
// kilobytes — a page of a listing, the start or the abandonment of an upload.
//
// THIRTY SECONDS: each is one small request and its answer, a fraction of a
// second against any healthy provider — a batch deletion a second or two.
// A request the bucket throttles or fails is asked again until this deadline
// and never for a count of attempts ([newRetryer]), and thirty seconds is
// about six attempts at the backoff ([fullJitter]: a random share of two,
// four, eight, sixteen and then twenty seconds, half of each on average) —
// while a bucket that has stopped answering ends a request rather than
// holding the handler that sent it.
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
// plus [objstore.MiBPace] a mebibyte of the part [partsInFlight] times over.
//
// THE PART HAS A SHARE OF THE LINK, NOT THE LINK: parts in flight cross one
// link and the floor is the UPLOAD's, so a part given only its own bytes
// misses its deadline once others share it. Counted for EVERY PART AN UPLOAD
// MAY KEEP IN FLIGHT, since parts join as the body arrives: sixteen and a
// half minutes for a full part, four and a half alone. What it still bounds
// is a part that keeps MOVING below its share, one the bucket keeps
// THROTTLING, and one whose connection count cannot be read (sockcount's
// ErrUncountable); failing and stopped parts end sooner ([judge.giveUpAfter]).
func partBudget(size int64) time.Duration { return sendBudget(size * partsInFlight) }

// Backend is the objects in a bucket.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string

	// presigner signs the puts a client makes itself, and http is the
	// client the probe makes them through: the bucket's own transport.
	presigner *s3.PresignClient
	http      *http.Client

	// unpaced is the client's API options as the SDK and the configuration
	// gave them, before the client's own steps ([paced]) were added: what
	// every presign is built from ([Backend.withoutAttempts]). Clipped, so
	// nothing appending to it writes beside the options it holds.
	unpaced []func(*middleware.Stack) error

	// caps is what the boot probe proved of the provider, written once
	// by Open and read by every put after.
	caps capabilities

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

	// gate is how many attempts the client lets the bucket hold at once:
	// every request of the node's, slowed together once the bucket has
	// throttled one ([gate]).
	gate *gate

	// judge reads every attempt's answer, and gives up a request the bucket
	// keeps failing within [judge.giveUpAfter] ([judge]) — that bound and the
	// draw of each span's own FIELDS, which nothing in the engine changes, so
	// this package's cases run the rule at a span they can afford, and seeded,
	// as [Backend.partBytes] is for the part.
	judge *judge

	// stall is the client's one stall bound ([stallBound]): how long the
	// judge asks a failing request again, and how long every attempt's
	// watch lets it go without moving ([watchedClient]) — [objstore.Stall],
	// which Open sets and nothing in the engine changes, and a FIELD, read
	// by both through one pointer, so this package's cases shorten both at
	// once (export_test.go) as [Backend.partBytes] is for the part.
	stall stallBound

	// watched is the bucket's HTTP client, every attempt watched.
	watched *watchedClient
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

// Open builds the client and runs the boot probe under the prefix (see
// probe.go): a credential that cannot store and delete an object there fails
// the boot rather than the first upload, and what the provider enforces
// decides whether direct uploads are offered.
//
// The probe's own requests are the client's like any other: one the bucket
// refuses — a wrong key, a bucket that is not there — fails the boot at once,
// and one it throttles, fails or does not answer is asked again within its
// budget, as every request is ([newRetryer]).
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	return open(ctx, cfg, fullJitter{}, systemClock)
}

// clock is the time the client keeps: the instant its [gate] measures its
// quiet minute from ([throttleQuiet]) and its watches date movements on
// ([watchedClient]), and the timer that lifts the gate when the minute ends.
type clock struct {
	now   func() time.Time
	after func(d time.Duration, f func())
}

// systemClock is the wall clock and its timers. The timer is discarded once
// armed: a Backend has no Close, and a lift timer exists only while its gate
// is engaged — at most a minute after the bucket's last throttle — so there is
// nothing to stop.
var systemClock = clock{
	now:   time.Now,
	after: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
}

// open is [Open] with the client's backoff and the clock its [gate] keeps
// given, so this package's cases can ask a bucket again in milliseconds and
// move a clock past a quiet minute rather than wait one out (export_test.go).
func open(ctx context.Context, cfg Config, backoff retry.BackoffDelayer, c clock) (*Backend, error) {
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
	b := &Backend{bucket: cfg.Bucket, prefix: cfg.Prefix, stall: newStallBound(objstore.Stall)}
	count := counted{rec: cfg.Metrics}
	b.gate = newGate(c, count)
	// ONE RETRYER, read by the SDK's retry step and by the judge alike, so
	// what the judge counts as a failure is exactly an answer the retry step
	// would ask again.
	retryer := newRetryer(backoff)
	b.judge = &judge{now: c.now, retryer: retryer, stall: &b.stall, count: count}
	b.watched = watching(httpClient, &b.stall, c.now)
	b.client = s3.NewFromConfig(base, func(o *s3.Options) {
		// THE BUCKET IS REACHED THROUGH THE ENGINE'S OWN TRANSPORT
		// (internal/httpx), never a pool of the SDK's — every attempt
		// WATCHED, so one the bucket stops moving ends a stall after it
		// last moved ([watchedClient]).
		o.HTTPClient = b.watched
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
		// NOTHING COUNTS ATTEMPTS ([newRetryer]). RetryMaxAttempts is
		// zeroed as well as the retryer's own count, because the SDK
		// copies AWS_MAX_ATTEMPTS — or a shared profile's max_attempts —
		// into it and, after every option here has run, wraps the
		// retryer in that count: an operator's environment would
		// silently put back the cap that failed requests a bucket would
		// have answered.
		o.Retryer = retryer
		o.RetryMaxAttempts = 0
		// A THROTTLE SLOWS THE WHOLE CLIENT ([gate]), a request the bucket
		// keeps failing is given up within a minute ([judge]), and a
		// request its deadline ends names the bucket's last answer — each
		// counted into the node's recorder as it happens. Every presign is
		// built from the options before these ([Backend.withoutAttempts]).
		b.unpaced = slices.Clip(slices.Clone(o.APIOptions))
		o.APIOptions = append(o.APIOptions, paced(follows(count, &b.stall), b.gate, b.judge))
	})
	b.presigner = s3.NewPresignClient(b.client)
	b.http = httpClient
	b.cutParts(PartBytes)
	caps, err := b.probe(ctx)
	if err != nil {
		return nil, err
	}
	b.caps = caps
	return b, nil
}

func (b *Backend) key(name string) string { return b.prefix + name }

// Put implements [objstore.Backend].
//
// A WRITE THE PROVIDER REFUSES FOR ROOM ([noRoom]) — the PutObject, any part
// of a multipart upload, or its completion, the upload abandoned as for every
// failure — is answered as the store having none ([objstore.Full]), naming the
// room this bucket needs ([FullRemedy]): never as the bucket failing, which
// every surface answers with "try again", into a bucket that refuses the same
// write the same way until an operator makes room.
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	err := b.put(ctx, name, r, m)
	if noRoom(err) {
		return &objstore.Full{Remedy: FullRemedy, Err: err}
	}
	return err
}

// FullRemedy is the room a bucket its provider refused a write for room needs
// ([objstore.Full.Remedy]), made on the provider's side — space on the storage
// it keeps the bucket on, or a quota it holds the bucket to — or by the
// collector, removing what nothing names.
//
// THE PROVIDER'S ROOM, NEVER THE NATS BUCKET'S: moving the files to a bucket,
// or raising a broker's reservation, are nothing an operator already on a
// bucket can do, and told them, one would migrate to the store that refused.
const FullRemedy = "an operator makes room in the bucket — frees space on the storage its " +
	"provider keeps it on, or raises the quota the provider holds it to — or lets the " +
	"collector remove what nothing names"

// put stores one object, from a body of any length ([Backend.Put]).
func (b *Backend) put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
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
//
// WITH ITS SHA-256 AS A HEADER where the probe proved the provider checks
// one: the provider then refuses a body damaged on the way, and keeps the
// digest the audit and the migration read back ([Backend.Stat]). A header,
// never the SDK's trailing checksum, which providers that are not Amazon
// refuse or mishandle; and only where proven, so a provider that rejects the
// header degrades to a put with none rather than to no put at all.
func (b *Backend) putObject(ctx context.Context, name string, data []byte, m objstore.PutMeta) error {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(b.key(name)),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   contentType(m),
	}
	if b.caps.checksums {
		in.ChecksumSHA256 = aws.String(checksumOf(data))
	}
	_, err := call(ctx, sendBudget(int64(len(data))), func(ctx context.Context) (*s3.PutObjectOutput, error) {
		// A FRESH READER PER ATTEMPT, so the SDK's retry of a failed
		// request sends the body from its start.
		in.Body = bytes.NewReader(data)
		return b.client.PutObject(ctx, in)
	})
	if err != nil {
		return fmt.Errorf("s3obj: put %s: %w", name, err)
	}
	return nil
}

// putParts streams a body longer than one part as a multipart upload whose
// first part is already in first. Up to [partsInFlight] parts are sent at
// once from buffers of their own while the next is read into one whose part
// has landed, a spare from the allowance, or the next part to land's buffer.
//
// A PART THAT FAILS ENDS THE UPLOAD AT ONCE: its sender ends the upload's
// context with the failure as cause, cancelling every part in flight and the
// read of the body; hearing of it only at the next buffer would read on and
// send three more parts for a lost upload. EVERY FAILURE ABANDONS THE UPLOAD
// under a context the caller's cannot end (the caller giving up is often the
// failure), and only ONCE EVERY PART SENT HAS ANSWERED: S3 may land a part
// still uploading when its upload is aborted, holding storage no listing shows.
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
// A READ OF A PART OR LESS — a page of a file — is ONE ranged request. A read
// that may run past a part — a whole object, or the rest of one — is read as
// consecutive part-sized ranges, up to [partsInFlight] at once and handed out
// in order ([Backend.getWide]), whose first range alone answers any object
// that fits it. A range that begins at or past the end is refused by S3 —
// an empty object's whole read included, since every range of one is — and
// answered as the empty stream the contract says it is.
func (b *Backend) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	if n < 0 || n > int64(b.partBytes) {
		return b.getWide(ctx, name, off, n)
	}
	out, err := b.client.GetObject(ctx, b.rangeInput(name, off, n, nil))
	switch {
	case missing(err):
		return nil, objstore.ErrNotFound
	case status(err) == http.StatusRequestedRangeNotSatisfiable:
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

// Stat implements [objstore.Backend], with the object's whole-content digest
// where the provider keeps one: only where the probe proved it checks
// checksums, since a single-part put carries one only then, and never for an
// object assembled from parts ([digestOf]).
func (b *Backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	in := &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(b.key(name))}
	if b.caps.checksums {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := call(ctx, callBudget, func(ctx context.Context) (*s3.HeadObjectOutput, error) {
		return b.client.HeadObject(ctx, in)
	})
	if missing(err) {
		return objstore.Info{}, objstore.ErrNotFound
	}
	if err != nil {
		return objstore.Info{}, fmt.Errorf("s3obj: stat %s: %w", name, err)
	}
	return objstore.Info{Name: name, Size: aws.ToInt64(out.ContentLength),
		Written: aws.ToTime(out.LastModified).UTC(),
		Digest:  digestOf(out.ChecksumSHA256, string(out.ChecksumType))}, nil
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

// DeleteBatch implements [objstore.BatchDeleter]: one DeleteObjects request of
// up to [objstore.MaxDeleteBatch] keys, QUIET, so its answer names only the
// keys it refused — a key that is not there is one it answers deleted.
//
// CONTENT-MD5 BESIDE THE SDK'S OWN CHECKSUM: the API once required an MD5 of
// this body and S3-compatible gateways implement that as written.
//
// A REQUEST THE BUCKET REFUSES AS A WHOLE (a client error but a throttle, or
// 501) is [objstore.ErrNoBatch]: it removed nothing and the gateway lacks the
// verb (GCS's XML API has no multi-object delete), so the caller deletes each
// name. A throttle or server error is reported as the bucket failing: a
// request per key would answer a prefix that said to slow down.
func (b *Backend) DeleteBatch(ctx context.Context, names []string) error {
	if len(names) > objstore.MaxDeleteBatch {
		return fmt.Errorf("s3obj: a batch deletion of %d names, past the %d one DeleteObjects takes",
			len(names), objstore.MaxDeleteBatch)
	}
	if len(names) == 0 {
		return nil
	}
	objects := make([]types.ObjectIdentifier, len(names))
	for i, name := range names {
		objects[i] = types.ObjectIdentifier{Key: aws.String(b.key(name))}
	}
	out, err := call(ctx, callBudget, func(ctx context.Context) (*s3.DeleteObjectsOutput, error) {
		return b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}, s3.WithAPIOptions(smithyhttp.AddContentChecksumMiddleware))
	})
	if err != nil {
		if code := status(err); (code >= 400 && code < 500 && code != http.StatusTooManyRequests) ||
			code == http.StatusNotImplemented {
			return fmt.Errorf("s3obj: delete %d objects: %w: %w", len(names), objstore.ErrNoBatch, err)
		}
		return fmt.Errorf("s3obj: delete %d objects: %w", len(names), err)
	}
	failed := map[string]error{}
	for _, e := range out.Errors {
		if aws.ToString(e.Code) == "NoSuchKey" {
			continue // not there: gone, as Delete reads it
		}
		name := strings.TrimPrefix(aws.ToString(e.Key), b.prefix)
		failed[name] = fmt.Errorf("s3obj: delete %s: %s: %s", name, aws.ToString(e.Code), aws.ToString(e.Message))
	}
	if len(failed) > 0 {
		return &objstore.DeleteBatchError{Failed: failed}
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
// them, so with the common prefix, the order of the names. A DIRECTORY BUCKET
// (S3 Express One Zone, names ending `--x-s3`) lists in no order and takes no
// StartAfter, so it answers [objstore.ErrUnordered] without a request and its
// caller asks about each object instead.
//
// The page that passes through is read whole, up to a thousand keys past it:
// S3 has no end to a range. A range spanning pages costs a request each,
// which is why the audit cuts its ranges short of one.
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
// abandons its own failed upload ([Backend.Put]), so one is left only where
// the process died mid-upload, and no listing of objects shows it. A bucket
// lifecycle rule may do the same; this does not rely on one. It needs
// `s3:ListBucketMultipartUploads`, which an identity granted only the object
// verbs lacks: the listing then fails, and says so.
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

// bucketClient is the client the bucket is reached through: a clone of the
// engine's shared transport (internal/httpx) that speaks HTTP/1.1 ALONE and
// also trusts AWS_CA_BUNDLE where set, beside the system roots.
//
// HTTP/1.1, SO A SOCKET IS ONE REQUEST'S: every attempt is watched at its own
// socket ([watchedClient]); over HTTP/2 another request's acknowledgements
// would read as this one's movement, so a stalled stream would never be ended
// while its siblings moved. The cost is four parts on four connections, not
// four streams on one. The pool keeps httpx.MaxIdleConnsPerHost (32), the
// width the git cache, backup and collector are held to. Where the default
// transport is not one httpx can clone (a test harness) the client is the
// shared one, and a stall sharing a socket is ended only by its deadline.
func bucketClient(bundle string) (*http.Client, error) {
	var roots *x509.CertPool
	if bundle != "" {
		pem, err := os.ReadFile(bundle)
		if err != nil {
			return nil, fmt.Errorf("s3obj: read AWS_CA_BUNDLE %s: %w", bundle, err)
		}
		if roots, err = x509.SystemCertPool(); err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("s3obj: AWS_CA_BUNDLE %s holds no PEM certificate", bundle)
		}
	}
	return httpx.Customized(0, func(t *http.Transport) {
		t.Protocols = new(http.Protocols)
		t.Protocols.SetHTTP1(true)
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		// AND OFFERS HTTP/1.1 ALONE in the TLS handshake: the clone carries
		// the shared transport's own TLS configuration, whose protocols
		// list names `h2` once that transport has spoken HTTP/2 anywhere,
		// and a transport set to HTTP/1.1 that still offers `h2` is answered
		// in HTTP/2 by a gateway that takes it — every request then reads
		// the gateway's frames as a malformed HTTP/1.1 answer.
		t.TLSClientConfig.NextProtos = []string{"http/1.1"}
		if roots != nil {
			t.TLSClientConfig.RootCAs = roots
		}
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

// fullCodes is every error code an S3-compatible provider refuses a write
// with for want of room, read from each provider's own source: MinIO's
// `XMinioStorageFull` (507, its drives at their free-space floor) and
// `XMinioAdminBucketQuotaExceeded` (400, the bucket's quota spent;
// cmd/api-errors.go), and Ceph's gateway's `InsufficientCapacity` (507,
// ENOSPC or EDQUOT) and `QuotaExceeded` (403, a user's or a bucket's quota;
// src/rgw/rgw_common.cc). Amazon S3 itself has none: a bucket there holds
// whatever it is sent.
//
// BY CODE AS WELL AS BY STATUS, because two of them come as a 400 and a 403,
// which name a hundred other refusals; and a code a gateway answers under
// another status is still the provider saying it has no room.
var fullCodes = map[string]struct{}{
	"XMinioStorageFull":              {},
	"XMinioAdminBucketQuotaExceeded": {},
	"InsufficientCapacity":           {},
	"QuotaExceeded":                  {},
}

// noRoom reports a write the bucket's provider refused for room: any `507
// Insufficient Storage` — HTTP's own status for a server that cannot store
// what a request needs, whatever code a gateway names beside it — or one of
// [fullCodes].
//
// NEVER ASKED AGAIN, by the SDK's own rules: it retries a 500, 502, 503 or
// 504, its throttle codes and a failure to send, and none of these is one —
// so a full bucket is answered once, and the store's caller is told so at the
// first refusal rather than a minute of them later.
func noRoom(err error) bool {
	if err == nil {
		return false
	}
	if status(err) == http.StatusInsufficientStorage {
		return true
	}
	if api, ok := errors.AsType[smithy.APIError](err); ok {
		_, full := fullCodes[api.ErrorCode()]
		return full
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
