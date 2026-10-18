package artifactupload

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/objectives"
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

// Releases is the versions of one embedded tree this build serves: the page
// runtime contracts (the tree the artifact origin answers `/_crewlet/runtime/`
// from) or the artifact kit's releases ([artifacts.KitPrefix]).
type Releases interface {
	// Latest is the newest version served, empty when the build serves
	// none.
	Latest() string

	// Serves reports whether an exact version is one this build serves.
	Serves(version string) bool
}

// Meter is where the pipeline's measurements go: the node's metrics recorder.
type Meter interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Observe(name string, d time.Duration, attrs metrics.Attrs)
	Set(name string, v float64, attrs metrics.Attrs)
}

// Objectives is where the pipeline's service objective events go: the node's
// objective meter ([objectives.Meter]), which judges each against its bound.
type Objectives interface {
	Record(n objectives.Name, elapsed time.Duration, answered bool)
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
	Runtimes Releases

	// Kits are the artifact kit releases a page may link, which the
	// publish judges a page's kit links against.
	Kits Releases

	// Meter records the pipeline's measurements; nil records none.
	Meter Meter

	// Objectives counts every create and publish into the publish
	// objectives ([objectives.PublishSuccess], [objectives.PublishApplied]);
	// nil counts none.
	Objectives Objectives

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
	runtimes Releases
	kits     Releases
	meter    Meter
	judged   Objectives
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
	case deps.Kits == nil:
		return nil, errors.New("artifactupload: the kit releases are required: a page's " +
			"links to the kit are judged against them at publish")
	case len(deps.Hosts.Hosts) == 0:
		return nil, errors.New("artifactupload: the CDN allowlist is required: the linter " +
			"holds every page's scripts to it")
	}
	s := &Service{
		objects: deps.Objects, res: deps.Reservations, writes: deps.Writes, reads: deps.Reads,
		presign: deps.Presign, limits: deps.Limits, vendor: deps.Vendor, hosts: deps.Hosts,
		runtimes: deps.Runtimes, kits: deps.Kits, meter: deps.Meter, judged: deps.Objectives, now: deps.Now,
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

// holdUntil is the end of a hold whose bytes are stored no later than last.
func holdUntil(last time.Time) time.Time { return last.Add(holdTail) }

// Report is what a publish found and did beside the version it landed:
// the linter's warnings (its refusals are errors and never reach here), and
// where the version's files came from.
type Report struct {
	Warnings []lint.Finding `json:"warnings,omitempty"`

	// Files counts the version's files by where their bytes came from; nil
	// where the pipeline did not compose the manifest (a rollback, whose
	// files the domain copies from the version it restores).
	Files *FileCounts `json:"files,omitempty"`
}

// FileCounts is one version's files by where their bytes came from.
//
// IT IS COUNTED FROM THE MANIFEST THAT LANDED, never from the upload intent:
// the intent judges a file by the digest of its author's bytes, while a page
// is stored in its linted form, so an unchanged page never matches what the
// artifact holds, is sent, and is then found held once linted. Only the
// manifest knows that.
type FileCounts struct {
	// Stored are the files whose bytes this publish stored as new objects.
	Stored int `json:"stored"`

	// Carried are the files whose bytes the artifact already held, however
	// they arrived: named as carried, or sent and then found held.
	Carried int `json:"carried"`
}
