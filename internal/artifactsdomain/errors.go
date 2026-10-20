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

	// ErrTrashed reports a write to an artifact in the trash, or a read of
	// its content (its files, its datastore), which stops being served the
	// moment it is trashed: restore it first.
	ErrTrashed = errors.New("artifactsdomain: the artifact is in the trash")

	// ErrPurged reports an artifact a purge destroyed for good.
	ErrPurged = errors.New("artifactsdomain: the artifact was purged")

	// ErrPurging reports a restore of an artifact whose purge has begun: its
	// data may already be gone for good, so it is never brought back, and
	// the purge finishes on its retry, an administrator's purge or the trash
	// reaper's next pass.
	ErrPurging = errors.New("artifactsdomain: the artifact is being purged and " +
		"cannot be restored")

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

	// ErrFolderNotFound reports a folder that does not exist, was removed,
	// or that the caller cannot see: one answer for all three, so a folder's
	// existence is never disclosed to a caller it is not visible to.
	ErrFolderNotFound = errors.New("artifactsdomain: no such folder")

	// ErrFolderNameTaken reports a folder name another folder in the same
	// place already holds, compared case-insensitively.
	ErrFolderNameTaken = errors.New("artifactsdomain: a folder in that place " +
		"already has that name")

	// ErrFolderCycle reports a folder moved into itself or a folder inside
	// it.
	ErrFolderCycle = errors.New("artifactsdomain: a folder cannot move into " +
		"itself or a folder inside it")

	// ErrFolderDepth reports a create or a move that would place a folder
	// deeper than folders may go (artifacts.MaxFolderDepth).
	ErrFolderDepth = errors.New("artifactsdomain: that would place a folder " +
		"deeper than folders may go")

	// ErrFolderLimit reports a create past the folders a company holds
	// (artifacts.MaxFolders) or one folder holds (artifacts.MaxFolderChildren).
	ErrFolderLimit = errors.New("artifactsdomain: there are already as many " +
		"folders there as there may be")

	// ErrBrandContrast reports an adoption whose text colour does not reach
	// brandformat.MinTextContrast on its light ground: the one contrast pair
	// that refuses rather than warns.
	ErrBrandContrast = errors.New("artifactsdomain: the brand's text colour " +
		"does not contrast enough with its light ground to be read")

	// ErrBrandIncomplete reports an adoption of a brand that leaves out a
	// field an adopted brand holds (brandformat.AdoptionFields), which a
	// draft may.
	ErrBrandIncomplete = errors.New("artifactsdomain: the brand leaves out a field an " +
		"adopted brand holds")
)

// FolderAmbiguous is a folder path that names more than one folder the caller
// can see (409 `folder_ambiguous`): every candidate, so the caller can choose
// one by id.
type FolderAmbiguous struct {
	Path       string
	Candidates []Folder
}

func (e *FolderAmbiguous) Error() string {
	ids := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		ids[i] = c.ID
	}
	return fmt.Sprintf("artifactsdomain: %q names %d folders you can see (%s); "+
		"name the one you mean by its id", e.Path, len(e.Candidates), strings.Join(ids, ", "))
}

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

// QuotaExceeded is a write the company's plan quota refuses (409
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
