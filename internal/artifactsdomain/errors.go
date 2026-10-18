package artifactsdomain

import (
	"errors"
	"fmt"
	"strings"
)

// The sentinels. Each is a fact a caller acts on differently, which is why they
// are separate.
var (
	// ErrNotFound reports an artifact, a version or an asset that does not
	// exist (or never did, which a caller with no relation is told too).
	ErrNotFound = errors.New("artifactsdomain: no such artifact")

	// ErrTrashed reports a write to an artifact in the trash: restore it
	// first.
	ErrTrashed = errors.New("artifactsdomain: the artifact is in the trash")

	// ErrPurged reports an artifact a purge destroyed for good.
	ErrPurged = errors.New("artifactsdomain: the artifact was purged")

	// ErrSourceTaken reports a source address another of the owner's
	// artifacts holds: republish to that artifact, or choose another key.
	ErrSourceTaken = errors.New("artifactsdomain: that source address is held by " +
		"another artifact of the same owner")

	// ErrAgentCompanyShare reports an agent widening a share to the whole
	// company, which only a person does (D10).
	ErrAgentCompanyShare = errors.New("artifactsdomain: an agent never shares " +
		"an artifact with the whole company; share it with seats or units, or ask " +
		"a person to widen it")

	// ErrAboveOwnRole reports a share granting a role above the sharer's own.
	ErrAboveOwnRole = errors.New("artifactsdomain: a share may not grant a role " +
		"above the sharer's own")

	// ErrLabelTaken reports a label another version of the artifact holds,
	// or a label past the labelled versions an artifact keeps.
	ErrLabelTaken = errors.New("artifactsdomain: another version of this " +
		"artifact holds that label")

	// ErrInvalid reports a write whose input the domain refuses, naming the
	// field and what to change.
	ErrInvalid = errors.New("artifactsdomain: invalid")

	// ErrUploadStale reports an uploaded object whose key was minted longer
	// than objstore.RecordWithin before the publish was decided: upload the
	// file again, since the collector may already be reclaiming it.
	ErrUploadStale = errors.New("artifactsdomain: an uploaded object is too old " +
		"to name")

	// ErrConflict reports a write that lost its race on the artifact too
	// many times: somebody else keeps changing it, so read it again.
	ErrConflict = errors.New("artifactsdomain: the artifact kept changing under " +
		"this write")

	// ErrBadCursor refuses a cursor this reader did not mint.
	ErrBadCursor = errors.New("artifactsdomain: that cursor is not one this " +
		"listing minted")
)

// VersionMoved is a publish based on a head that has moved (412
// `version_moved`): it carries the live head and its version, so the caller
// can merge onto them and publish again with the live number as its base.
type VersionMoved struct {
	Head Head
	Live Version
}

func (e *VersionMoved) Error() string {
	return fmt.Sprintf("artifactsdomain: artifact %s is at version %d and this "+
		"publish was based on an older one: merge the change onto version %d and "+
		"publish again based on it", e.Head.ID, e.Head.HeadNumber, e.Head.HeadNumber)
}

// CarriedObjectGone is a publish carrying files this artifact no longer names
// (409 `carried_object_gone`): upload those paths again.
type CarriedObjectGone struct {
	Paths []string
}

func (e *CarriedObjectGone) Error() string {
	return fmt.Sprintf("artifactsdomain: this artifact no longer holds the bytes "+
		"of %s; upload them again rather than carrying them", strings.Join(e.Paths, ", "))
}

// QuotaExceeded is a write the company's plan quota refuses (413
// `quota_exceeded`), naming the Tier A key an operator raises.
type QuotaExceeded struct {
	Limit int64
	Usage int64
	Key   string
}

func (e *QuotaExceeded) Error() string {
	return fmt.Sprintf("artifactsdomain: this write would pass the company's "+
		"quota: %d in use against a limit of %d (%s); remove or purge artifacts, "+
		"or ask the operator to raise %s", e.Usage, e.Limit, e.Key, e.Key)
}
