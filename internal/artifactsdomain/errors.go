package artifactsdomain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/statelog"
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

	// ErrLabelTaken reports a label another version of the artifact holds: a
	// COLLISION, so the same label lands on this version once that one gives
	// it up, and another label lands now.
	ErrLabelTaken = errors.New("artifactsdomain: another version has that label")

	// ErrLabelsFull reports a label for a version when the artifact already
	// keeps [artifacts.MaxLabels] labelled versions: the ARTIFACT'S STATE,
	// its own labels at their cap, so the same label lands once one is
	// removed. Its own sentinel beside [ErrLabelTaken] because the two send
	// a caller to different places — another label for a collision, a
	// removal for a full set — and one sentinel for both told a caller
	// choosing a label to choose another when no label would land.
	ErrLabelsFull = errors.New("artifactsdomain: every label this artifact " +
		"keeps is in use")

	// ErrExists reports a create of an artifact or an asset that is already
	// there: a COLLISION with what an earlier write made, which is the same
	// create made again after its operation ledger forgot it, or a create
	// that raced another onto one id. Read it rather than make it again. Its
	// own sentinel rather than [ErrInvalid] (nothing about the request is
	// malformed) or the state log's bare one (which every surface but the
	// tools answered as this node's fault).
	ErrExists = errors.New("artifactsdomain: what this write creates is already there")

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
// can see (422 `folder_ambiguous`): every candidate, so the caller can choose
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

// Sentence is a refusal this package wrote, as a person and a model read it:
// the sentence, without the package names and the class word a Go error
// carries in front of it.
//
// A REFUSAL HERE OPENS WITH ITS SENTINEL ("artifactsdomain: invalid: kind
// \"x\" is not one of …"), and one about a manifest, a path or an audience
// carries the refusal of the package that decided it after that
// ("artifactsdomain: invalid: manifest: invalid: worklet: the path …"). The
// package names say where it came from and the class word what [errors.Is]
// finds, and neither is anything a reader acts on: stripped of the package
// names alone, a refusal read "publish_artifact refused that: invalid: kind
// …", the class word mid-sentence. Both go, wherever they sit, and every word
// after them stays.
//
// THE ONE PLACE THE ARTIFACTS' WORDS ARE CUT, beside [ErrInvalid]: the tools
// and the artifacts API each kept a list of package names to strip, and
// neither list knew the class words. The publish pipeline's own refusals
// carry its names too, which internal/artifactupload's Sentence cuts before
// this one.
//
// AND NEVER AN ID THE READER DID NOT CHOOSE. A refusal about the artifact
// itself — not there, in the trash, being purged, purged, moved past a base, a
// version or an asset it does not keep, a restore of one that is not in the
// trash, an asset store at its cap — carries the artifact's id for the log
// ("the artifact was purged: 0192f…"), and a person reads it on a screen that
// already shows the artifact, or as the answer to a link; the id goes, the
// sentence stays, and the error a log prints keeps it ([CutArtifactID],
// [VersionMoved]).
//
// AND THE FRAMEWORK'S OWN THREE IN ITS WORDS — a lost race ([ErrConflict] wraps
// it), a create of what is there ([AlreadyExists] carries it) and a write a
// deletion keeps deleted — as every surface that asks the state log first
// tells them ([statelog.Sentence]), so a caller asking only this one never
// prints a subject the framework met.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	text := err.Error()
	var moved *VersionMoved
	if errors.As(err, &moved) {
		text = strings.Replace(text, moved.Error(), moved.sentence(), 1)
	}
	for _, about := range aboutTheArtifact {
		text = CutArtifactID(text, about)
	}
	for _, cut := range sentenceCuts {
		text = strings.ReplaceAll(text, cut, "")
	}
	return text
}

// aboutTheArtifact are the refusals that name the artifact they are about by
// its id, after the sentinel's own sentence: `<sentinel>: <id>`, or
// `<sentinel>: <id>: <a sentence of its own>`.
var aboutTheArtifact = []error{ErrNotFound, ErrTrashed, ErrPurging, ErrPurged, ErrInvalid}

// CutArtifactID is what a person reads of a refusal whose error names, for the
// log, the artifact the reader is already looking at: text with the artifact
// id that follows sentinel's own words cut, and text unchanged where no id
// follows them.
//
// TWO SHAPES. `<sentinel>: <id>` is told as the sentinel's words ("the
// artifact was purged"). `<sentinel>: <id>: <sentence>` is told as the
// sentence alone, which says more than the sentinel and would contradict it
// beside it: a version the artifact does not keep is ErrNotFound, whose own
// words are "no such artifact".
//
// Exported for the datastore's purge
// ([github.com/crewlet/crewlet/internal/artifactdata.ErrGone]), which names
// its artifact the same way.
func CutArtifactID(text string, sentinel error) string {
	lead := sentinel.Error() + ": "
	at := strings.Index(text, lead)
	if at < 0 {
		return text
	}
	rest := text[at+len(lead):]
	id, after, more := strings.Cut(rest, ":")
	if _, err := artifacts.ParseID(id); err != nil {
		return text
	}
	if !more {
		return text[:at] + sentinel.Error()
	}
	return text[:at] + strings.TrimPrefix(after, " ")
}

// sentenceCuts are what [Sentence] removes, the class words before the package
// names they open with: this package's own, and those of the packages whose
// refusals this one carries — a manifest's, an artifact id's, a label's or an
// audience's ("not an audience: \"nobody\" is not …", whose sentence names
// what it is not on its own), a path's, which the manifest holds to Worklet's
// one file-path rule, and a folder's name ([foldername.Check]).
var sentenceCuts = []string{
	ErrInvalid.Error() + ": ", manifest.ErrInvalid.Error() + ": ",
	artifacts.ErrInvalidAudience.Error() + ": ", artifacts.ErrInvalidID.Error() + ": ",
	artifacts.ErrInvalidLabel.Error() + ": ", foldername.ErrInvalid.Error() + ": ",
	"artifactsdomain: ", "manifest: ", "artifacts: ", "worklet: ", "foldername: ",
}

// AlreadyExists is [ErrExists] naming What was already there. THE STATE LOG'S
// [statelog.ErrExists] IS IN ITS CHAIN AND NOT IN ITS WORDS: what the
// framework says names the subject's broker path, while a create of what is
// already there is one refusal on every domain, and a surface telling a
// person one tells it in the framework's words for a person
// ([statelog.Sentence]).
//
// EXPORTED, WITH ITS FIELD, because it crosses the estate's wire: a node with
// no data asks a data node to write, and the wire rebuilds an error type from
// its exported fields and its registration (internal/estate's typedKinds),
// so an unexported one would arrive as its message alone.
type AlreadyExists struct{ What string }

func (e *AlreadyExists) Error() string   { return ErrExists.Error() + ": " + e.What }
func (e *AlreadyExists) Unwrap() []error { return []error{ErrExists, statelog.ErrExists} }

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

// sentence is the refusal as a person reads it ([Sentence]): the error less
// the artifact's id, which the screen the publish came from already shows and
// the answer carries as `head`.
func (e *VersionMoved) sentence() string {
	return fmt.Sprintf("this artifact is at version %d, and this publish was "+
		"based on an older one. Merge the change onto version %d and publish "+
		"again based on it", e.Head.HeadNumber, e.Head.HeadNumber)
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
