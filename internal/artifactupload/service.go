package artifactupload

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Reservations is the fleet's account of reserved bytes, as this package uses
// it: internal/coord's UploadReservations.
type Reservations interface {
	UploadLedger(ctx context.Context) ([]byte, uint64, error)
	SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (uint64, error)
	UploadRecord(ctx context.Context, upload string) ([]byte, uint64, error)
	SwapUploadRecord(ctx context.Context, upload string, value []byte, revision uint64) (uint64, error)
	DeleteUploadRecord(ctx context.Context, upload string, revision uint64) (bool, error)
}

// Writes is the artifacts domain's write side as the pipeline calls it, this
// node's own or a data node's through the estate router.
type Writes interface {
	Create(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.CreateInput) (artifactsdomain.Published, error)
	Publish(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.PublishInput) (artifactsdomain.Published, error)
	AddAsset(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.AssetInput) (artifactsdomain.Asset, artifactsdomain.Written, error)
}

// Reads is the artifacts domain's read side as the pipeline asks it.
type Reads interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (artifactsdomain.Head, statelog.Position, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	HeldObjects(ctx context.Context, id artifacts.ID, hashes []objstore.Hash) (map[objstore.Hash]objstore.Object, error)
	Usage(ctx context.Context) (artifactsdomain.Usage, error)
}

// Presigner signs a put a client makes straight to the bucket: the object
// store, where its backend proved it enforces what a presigned put relies on.
type Presigner interface {
	PresignPut(ctx context.Context, k objstore.Key, r objstore.PresignRequest) (objstore.Presigned, error)
}

// Runtimes is the page runtime contracts this build serves (the embedded
// runtime tree the artifact origin answers `/_crewlet/runtime/` from).
type Runtimes interface {
	// Latest is the newest contract served, empty when the build serves
	// none.
	Latest() string

	// Serves reports whether an exact contract is one this build serves.
	Serves(contract string) bool
}

// Meter is where the pipeline's measurements go: the node's metrics recorder.
type Meter interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Observe(name string, d time.Duration, attrs metrics.Attrs)
	Set(name string, v float64, attrs metrics.Attrs)
}

// Deps is what a [Service] is built from.
type Deps struct {
	// Objects is the fleet's object store, the one every byte lands in.
	Objects *objstore.Store

	// Reservations is the fleet's account of reserved bytes.
	Reservations Reservations

	// Writes and Reads are the artifacts domain, local or routed.
	Writes Writes
	Reads  Reads

	// Presign is nil unless the object store's backend proved direct
	// upload (an S3 bucket whose boot probe passed): every transfer is then
	// streamed through the engine.
	Presign Presigner

	// Limits are the Tier A quotas in force on this node.
	Limits func() artifacts.Limits

	// Vendor copies allowlisted CDN files; nil turns copying off, and a
	// page referencing one is refused.
	Vendor *vendor.Cache

	// Hosts is the CDN allowlist the linter holds pages to.
	Hosts lint.Allowlist

	// Runtimes are the runtime contracts a page may pin.
	Runtimes Runtimes

	// Meter records the pipeline's measurements; nil records none.
	Meter Meter

	// Now is the clock every reservation and key is judged by; nil is the
	// wall clock.
	Now func() time.Time
}

// Service is the upload and publish pipeline: one per node.
type Service struct {
	objects  *objstore.Store
	res      Reservations
	writes   Writes
	reads    Reads
	presign  Presigner
	limits   func() artifacts.Limits
	vendor   *vendor.Cache
	hosts    lint.Allowlist
	runtimes Runtimes
	meter    Meter
	now      func() time.Time
	turns    *turnGuard
}

// New builds the pipeline.
func New(deps Deps) (*Service, error) {
	switch {
	case deps.Objects == nil:
		return nil, errors.New("artifactupload: the object store is required: every byte " +
			"an artifact holds is an object")
	case deps.Reservations == nil:
		return nil, errors.New("artifactupload: the reservation store is required: no byte " +
			"moves before it is reserved")
	case deps.Writes == nil || deps.Reads == nil:
		return nil, errors.New("artifactupload: the artifacts domain's reads and writes are " +
			"required: a version is decided there")
	case deps.Limits == nil:
		return nil, errors.New("artifactupload: the quota limits are required: every " +
			"reservation and record is held to them")
	case deps.Runtimes == nil:
		return nil, errors.New("artifactupload: the runtime contracts are required: a page " +
			"pins one at publish")
	case len(deps.Hosts.Hosts) == 0:
		return nil, errors.New("artifactupload: the CDN allowlist is required: the linter " +
			"holds every page's scripts to it")
	}
	s := &Service{
		objects: deps.Objects, res: deps.Reservations, writes: deps.Writes, reads: deps.Reads,
		presign: deps.Presign, limits: deps.Limits, vendor: deps.Vendor, hosts: deps.Hosts,
		runtimes: deps.Runtimes, meter: deps.Meter, now: deps.Now,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	s.turns = newTurnGuard(s.now)
	return s, nil
}

// MaxOutstandingUploads is how many uploads one principal may hold
// reservations for at once, and a company [artifacts.CompanyPendingFactor]
// times as many.
//
// SIXTY-FOUR: eight times the eight full publishes the pending byte bound
// allows a principal, so no honest client meets it, while a client opening
// small uploads in a loop is refused long before the ledger of every hold (one
// coordination record the company shares) grows past a few tens of kilobytes.
const MaxOutstandingUploads = 64

// The hold a reservation keeps: until the object collector could have deleted
// every byte it answers for. A key is collected once both its minting and its
// storing are past the collector's grace, at the next pass; a pass already
// listing when that instant comes may not reach the key until the one after.
// So a hold lasts from the last instant a byte it answers for can be stored,
// through the grace, and two passes.
const holdTail = collect.PendingGrace + 2*collect.CollectInterval

// MaxHold is the longest a reservation holds bytes past its last write: the
// longest transfer window an intent is given, the pace of its largest file
// begun at the window's end, and [holdTail]. The coordination store keeps a
// reservation's records at least this long after their last write.
const MaxHold = objstore.MaxPresignTerm + time.Duration((artifacts.MaxTextFileBytes+objstore.MiB-1)/objstore.MiB)*objstore.MiBPace + holdTail

// intentBase is the shortest transfer window an intent is given: the term a
// presigned put of an empty file gets (objstore.PresignTerm).
const intentBase = 15 * time.Minute

// holdUntil is the end of a hold whose bytes are stored no later than last.
func holdUntil(last time.Time) time.Time { return last.Add(holdTail) }

// Report is what the linter found: refusals never reach here (they are
// errors); warnings ride on success.
type Report struct {
	Warnings []lint.Finding `json:"warnings,omitempty"`
}
