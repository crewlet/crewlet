package artifactsdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ARTIFACTS LOG'S SUBJECTS: what a record arbitrates over.
//
// Every change is one RECORD published to the subject of the object it
// contends for and conditioned on that object's last sequence there. The
// subject is the ARBITRATION UNIT, so the choice of subject per operation is
// the concurrency design:
//
//   - Every op that adds or removes an object reference (a publish, a
//     rollback, a retention prune, a purge) is on the ARTIFACT's own subject,
//     so the broker orders every carry against every removal and the applier's
//     carry check never fires for a well-behaved writer.
//   - The republish-in-place address is a SOURCE subject, create-only, because
//     two writers publishing the same source for the first time must contend
//     there: two fresh artifact ids never would.
//   - An asset rides a subject of its own beneath its artifact, because an
//     asset is never carried and an upload storm must not push publishes into
//     compare-and-set rounds.
//   - A reader's pins and opened marks ride the READER's subject, because they
//     are one person's state and contend with nothing else.
//   - The company's folder tree is ONE subject (`folder`, no id), because
//     every folder write reads the whole tree: sibling names, the ancestor
//     chain a move must not close into a cycle, the depth stamp, the live
//     count. One subject makes the writer's snapshot the one the broker
//     arbitrates, as the identity directory's one subject does. An
//     artifact's MOVE rides the artifact's own subject, ordered against its
//     publishes, and names the folders it reads in its scope.
//   - The company's adopted brand is ONE subject (`brand`, no id): two
//     administrators adopting at once contend there, and the later one is
//     decided against the earlier one's adoption.

// ObjectKind is what an object on the artifacts log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of, and the literal is RETAINED either way: a newer peer publishes a
// kind this build does not know, and the deferral this build files it under
// forms a scope term out of that literal.
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
	// One of the two kinds that install a gate, which is why its version is
	// pinned for ever.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain. One of the two kinds that write no row of this
	// domain's on any node; the other is [KindTrimmed].
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds — [statelog.TrimmedKind], ADR-0073
	// — on one subject per floor, which the subject's id names. The
	// framework removes the anchors in the transaction that applies it; this
	// domain writes nothing for it, and has the case that says so.
	KindTrimmed ObjectKind = statelog.TrimmedKind

	// KindFolder is the company's folder tree, ONE subject with no id:
	// every folder's create, rename, move and removal.
	KindFolder ObjectKind = "folder"

	// KindBrand is the company's adopted brand, ONE subject with no id: an
	// adoption and a clear.
	KindBrand ObjectKind = "brand"
)

// KindComment is RESERVED for the anchored comments a later build adds: no
// op, no payload and no table in this build, and NOT in [ObjectKinds], so a
// record of it is refused where it is written and a newer peer's is filed
// under the literal like any kind this build does not know.
const KindComment ObjectKind = "comment"

// ObjectKinds are the ten, and THE ORDER IS LOAD-BEARING.
//
// The framework's conformance suite publishes the FIRST THREE a domain
// declares, twice each, in order, so the declaration decides what the suite
// certifies. These three are a real sequence: an artifact's create, an asset
// added to it, and a reader pinning it. An asset or a pin of an artifact no
// create wrote is a record the applier refuses, so any other order would
// certify a refusal.
//
// A KIND ADDED LATER IS APPENDED, never inserted: the folder tree and the
// brand came after the base format, so a build that predates them retains
// their records ([versionedFields] stamps each kind at its version).
var ObjectKinds = []ObjectKind{
	KindArtifact, KindAsset, KindReader, KindSource, KindEviction, KindGeneration,
	KindBarrier, KindTrimmed, KindFolder, KindBrand,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// EIGHT OF TEN DO. The barrier shares one subject across the whole domain, so
// an expectation there would serialise every linearizable read behind every
// other one. A trimmed record REMOVES anchors, and identical ones are each
// meant to land, so an expectation would refuse every one after the first and
// an anchor of its own would be the one row it exists not to leave behind. A
// reader's `opened` and `read_elevated` are additive writes on an arbitrated
// kind: they carry no expectation of their own, and they move the anchor a pin
// is arbitrated against, which costs a pin at most one round.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier && k != KindTrimmed }

// InstallsGate reports a kind whose unknown version must STOP the applier
// rather than be filed for later. A purge is this log's other gate, and it is
// one by its OP on an ordinary artifact subject ([RecordEnvelope.InstallsGate]).
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
// THE ARTIFACT LEADS, before a dot, so the deletion gate reads the artifact
// from the subject without decoding a payload: the publisher's side of the gate
// is asked about another node's record with no body at all ([Gates.GatedAt]).
func AssetSubject(artifact artifacts.ID, asset string) Subject {
	return Subject{Kind: KindAsset, ID: artifact.String() + "." + asset}
}

// ReaderSubject names one person's own state, by the name it is kept under
// (iam.RecordOwner).
func ReaderSubject(record string) Subject { return Subject{Kind: KindReader, ID: record} }

// SourceSubject names an owner's hold on one republish-in-place address.
//
// # Why the address is a TOKEN
//
// A source key is a caller's own string (a path, a URL, a slug), and a subject
// is a broker path that may carry no space, dot or wildcard. So the address is
// arbitrated on a fixed-width digest of the key ([SourceToken]); the record's
// payload carries the owner and the key themselves, and the applier recomputes
// the subject from them and refuses a record that arbitrated on one address
// while claiming another.
func SourceSubject(owner, sourceKey string) Subject {
	return Subject{Kind: KindSource, ID: owner + "." + SourceToken(sourceKey)}
}

// SourceToken is the address a source key is arbitrated on: the first sixteen
// bytes of SHA-256 over the key, in lowercase hex.
//
// THE KEY IS NOT NORMALISED: a source is an exact string a caller repeats (a
// file path, a project slug), and folding case would make two distinct
// sources one address.
func SourceToken(sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(sum[:16])
}

// SplitSourceID takes a source subject's id apart into the owner and the
// token. The token is fixed-width hex with no dot, so the cut is at the LAST
// dot and an owner that itself carries one still reads back whole.
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
// A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED HERE. It is refused where a
// record is WRITTEN ([Subject.writable]) and accepted where one is READ, which
// is the asymmetry the two-pass decode exists for.
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

// writable refuses a subject this build may not WRITE: an unknown kind, and
// the reserved comment kind among them.
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
// THE SUBJECT ALONE DECIDES, for the reason [Gates.GatedAt] gives: the
// publisher's side is asked about another node's record with no body. An
// artifact subject names its artifact and an asset subject leads with it; a
// source subject and a reader subject name one only in their payloads, so they
// are gated on nothing and their applies refuse an artifact the marker holds
// instead. ONE FUNCTION, asked by [Applier.Gated] and [Gates.GatedAt] alike,
// because the framework holds the two to one answer.
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
	}
	return "", false
}
