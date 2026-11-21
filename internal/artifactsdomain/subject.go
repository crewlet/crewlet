package artifactsdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ARTIFACTS LOG'S SUBJECTS: a record is conditioned on its subject's last
// sequence, so the subject is the arbitration unit and the concurrency design.
//   - Every op adding or removing an object reference rides the ARTIFACT's
//     subject, so the broker orders every carry against every removal.
//   - A republish-in-place address is a create-only SOURCE subject: two first
//     publishes of one source must contend, and two fresh artifact ids never do.
//   - An asset (never carried) and a comment thread (ADR-0154) ride subjects
//     under the artifact: uploads never push publishes into compare-and-set.
//   - A READER's pins and opened marks are one person's: their own subject.
//   - The folder tree and the brand are ONE subject each, with no id: every
//     folder write reads the whole tree, and two adoptions must contend. A
//     MOVE rides its artifact's subject, naming its folders in scope.

// ObjectKind is what an object on the artifacts log is.
//
// Valid is false for a kind this build has never heard of, but the literal is
// RETAINED: the deferral of a newer peer's record forms a scope term from it.
type ObjectKind string

const (
	// KindArtifact is one artifact's head: every write to an artifact that
	// is not an asset or one reader's own state, a create without a source
	// key included.
	KindArtifact ObjectKind = "artifact"

	// KindAsset is one file in an artifact's asset store. Its id is the
	// artifact's id, a dot and the asset's, so the deletion gate reads the
	// artifact from the subject alone.
	KindAsset ObjectKind = "asset"

	// KindReader is one person's own state about artifacts: pins, opened
	// marks and the audit row an elevated read leaves. Its id is the seat
	// handle (or machine login) the state is kept under.
	KindReader ObjectKind = "reader"

	// KindSource is an owner's hold on one republish-in-place address. Its
	// id is the owner, a dot and the address's token, and the one op on it
	// is a create.
	KindSource ObjectKind = "source"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	// It installs a gate, so its version is pinned for ever.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. Like [KindTrimmed], it writes no row of this
	// domain's on any node.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record removing the arbitration anchors below
	// what the log still holds ([statelog.TrimmedKind], ADR-0080), on one
	// subject per floor, which its id names. The framework removes the anchors
	// as it applies it; this domain writes nothing for it.
	KindTrimmed ObjectKind = statelog.TrimmedKind

	// KindFolder is the company's folder tree, ONE subject with no id:
	// every folder's create, rename, move and removal.
	KindFolder ObjectKind = "folder"

	// KindBrand is the company's adopted brand, ONE subject with no id: an
	// adoption and a clear.
	KindBrand ObjectKind = "brand"

	// KindComment is one comment thread on an artifact (ADR-0154). Its id is
	// `<artifact>.<thread>`, so the deletion gate reads the artifact from it.
	KindComment ObjectKind = "comment"
)

// ObjectKinds are the eleven, and THE ORDER IS LOAD-BEARING: the framework's
// conformance suite publishes the FIRST THREE a domain declares, twice each,
// in order. They are a real sequence (an artifact's create, an asset added to
// it, a reader pinning it); in any other order the applier refuses an asset or
// pin of an artifact no create wrote, and the suite certifies a refusal. A kind
// added later is APPENDED, never inserted, and [versionedFields] stamps it at
// its version so an older build retains its records.
var ObjectKinds = []ObjectKind{
	KindArtifact, KindAsset, KindReader, KindSource, KindEviction, KindGeneration,
	KindBarrier, KindTrimmed, KindFolder, KindBrand, KindComment,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// NINE OF ELEVEN DO. The barrier's one domain-wide subject would serialise
// every linearizable read behind every other. A trimmed record REMOVES anchors
// and identical ones must each land, so an expectation would refuse all but the
// first, and its own anchor would be the row it exists not to leave. A reader's
// `opened` and `read_elevated` carry no expectation but move the anchor a pin
// is arbitrated against, costing a pin at most one round.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier && k != KindTrimmed }

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later. A purge, the other gate, is one by its OP
// ([RecordEnvelope.InstallsGate]).
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// ArtifactSubject names one artifact by its id.
func ArtifactSubject(id artifacts.ID) Subject {
	return Subject{Kind: KindArtifact, ID: id.String()}
}

// AssetSubject names one asset of one artifact.
//
// THE ARTIFACT LEADS, before a dot, so the deletion gate reads it from the
// subject: the publisher's side is asked about another node's record with no
// body at all ([Gates.GatedAt]).
func AssetSubject(artifact artifacts.ID, asset string) Subject {
	return Subject{Kind: KindAsset, ID: artifact.String() + "." + asset}
}

// ReaderSubject names one person's own state, by the name it is kept under
// (iam.RecordOwner).
func ReaderSubject(record string) Subject { return Subject{Kind: KindReader, ID: record} }

// SourceSubject names an owner's hold on one republish-in-place address. A
// source key is a caller's string and a subject may carry no space, dot or
// wildcard, so the address is the key's digest ([SourceToken]). The payload
// carries the owner and the key, and the applier refuses a record whose
// subject they do not recompute to.
func SourceSubject(owner, sourceKey string) Subject {
	return Subject{Kind: KindSource, ID: owner + "." + SourceToken(sourceKey)}
}

// SourceToken is the address a source key is arbitrated on: the first sixteen
// bytes of SHA-256 over the key, in lowercase hex.
//
// THE KEY IS NOT NORMALISED: a source is an exact string a caller repeats, and
// folding case would make two distinct sources one address.
func SourceToken(sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(sum[:16])
}

// SplitSourceID takes a source subject's id apart into the owner and the
// token. The token has no dot, so the cut is at the LAST dot and an owner
// carrying one reads back whole.
func SplitSourceID(id string) (owner, token string, err error) {
	cut := strings.LastIndex(id, ".")
	if cut <= 0 || cut == len(id)-1 {
		return "", "", fmt.Errorf("artifactsdomain: %q is not a source id: a "+
			"source's id is its owner, a dot and the source key's token", id)
	}
	return id[:cut], id[cut+1:], nil
}

// SplitAssetID takes an asset subject's id apart into the artifact and the
// asset. Neither an artifact id nor an asset id carries a dot, so the cut is at
// the first.
func SplitAssetID(id string) (artifact, asset string, err error) {
	artifact, asset, ok := strings.Cut(id, ".")
	if !ok || artifact == "" || asset == "" {
		return "", "", fmt.Errorf("artifactsdomain: %q is not an asset id: an "+
			"asset's id is its artifact's id, a dot and its own", id)
	}
	return artifact, asset, nil
}

// CommentSubject names one comment thread of one artifact. The artifact leads,
// for [AssetSubject]'s reason: the deletion gate reads it from the subject.
func CommentSubject(artifact artifacts.ID, thread uuid.UUID) Subject {
	return Subject{Kind: KindComment, ID: artifact.String() + "." + thread.String()}
}

// SplitCommentID takes a comment subject's id apart into the artifact and the
// thread, cut at the first dot as an asset's is.
func SplitCommentID(id string) (artifact, thread string, err error) {
	artifact, thread, ok := strings.Cut(id, ".")
	if !ok || artifact == "" || thread == "" {
		return "", "", fmt.Errorf("artifactsdomain: %q is not a comment thread id: "+
			"a thread's id is its artifact's id, a dot and its own", id)
	}
	return artifact, thread, nil
}

// EvictionSubject names one node's standing in this log, so an eviction and
// the readmission that inverts it arbitrate against each other and nothing
// else.
func EvictionSubject(nodeID string) Subject { return Subject{Kind: KindEviction, ID: nodeID} }

// GenerationSubject names one generation of the stream, which a reanchor
// claims create-only.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: strconv.FormatUint(uint64(gen), 10)}
}

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's ONE spelling of it ([statelog.TrimmedSubject]): the framework
// reads the floor back from the id.
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// FolderSubject is the company's folder tree: one subject for every folder.
func FolderSubject() Subject { return Subject{Kind: KindFolder} }

// BrandSubject is the company's adopted brand.
func BrandSubject() Subject { return Subject{Kind: KindBrand} }

// Singleton reports a kind with exactly one object, whose subject carries no
// id: the barrier, the folder tree and the brand.
func (k ObjectKind) Singleton() bool {
	return k == KindBarrier || k == KindFolder || k == KindBrand
}

// String renders the subject's own path: what the framework appends to the
// domain's subject prefix, and what a log line names.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.ArtifactsLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object.
//
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE: it is refused where a
// record is WRITTEN ([Subject.writable]) and accepted where one is READ, the
// asymmetry the two-pass decode exists for.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("artifactsdomain: a subject with no kind addresses the " +
			"log's own prefix, which no applier has a case for")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("artifactsdomain: subject kind %q carries a separator "+
			"or a wildcard, so the kind and the id could not be told apart", s.Kind)
	}
	if s.ID == "" && !s.Kind.Singleton() {
		return fmt.Errorf("artifactsdomain: a %s subject needs an id: only the "+
			"barrier, the folder tree and the brand are kinds with exactly one "+
			"object", s.Kind)
	}
	if s.ID != "" && (s.Kind == KindFolder || s.Kind == KindBrand) {
		return fmt.Errorf("artifactsdomain: a %s subject carries no id: the kind "+
			"has exactly one object, and %q would be a second subject arbitrating "+
			"the same rows", s.Kind, s.ID)
	}
	if strings.ContainsAny(s.ID, " \t\r\n*>/") {
		return fmt.Errorf("artifactsdomain: subject id %q carries whitespace, a "+
			"wildcard or a slash, which the broker or the scope alphabet would "+
			"read as something else", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not WRITE: a kind it does not
// know.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("artifactsdomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the artifacts log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.ArtifactsLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// gatedArtifact is the artifact whose deletion marker gates a record on subj,
// and false for a subject no artifact's marker covers.
//
// THE SUBJECT ALONE DECIDES, for [Gates.GatedAt]'s reason. An artifact
// subject names its artifact, and an asset's and a comment thread's lead with
// it; a source or reader subject names one only in its payload, so it is gated
// on nothing and its apply refuses an artifact the marker holds instead. ONE
// FUNCTION for [Applier.Gated] and [Gates.GatedAt]: the framework holds the two
// to one answer.
func gatedArtifact(kind, id string) (string, bool) {
	switch ObjectKind(kind) {
	case KindArtifact:
		return id, id != ""
	case KindAsset:
		artifact, _, err := SplitAssetID(id)
		if err != nil {
			return "", false
		}
		return artifact, true
	case KindComment:
		artifact, _, err := SplitCommentID(id)
		if err != nil {
			return "", false
		}
		return artifact, true
	}
	return "", false
}
