package artifactdata

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DATASTORE LOG'S SUBJECTS. One subject per artifact store, because the
// store is the unit a batch is atomic over and the unit every write is ordered
// within: a write is SEQUENCED there (no expectation, the applier decides), and
// a schema change is ARBITRATED there (decided in a snapshot of the schema it
// replaces), so the schema change is ordered against every write it judges.

// ObjectKind is what an object on the datastore log is.
//
// A NAMED STRING TYPE whose Valid is false for a kind this build has never
// heard of, and the literal is RETAINED either way: a newer peer's kind is
// filed under its literal by the deferral.
type ObjectKind string

const (
	// KindStore is one artifact's datastore: every write, undo, schema
	// change and purge of it.
	KindStore ObjectKind = "store"

	// KindEviction is a node's eviction from THIS log, or its readmission.
	KindEviction ObjectKind = "eviction"

	// KindGeneration is a reanchor's record, create-only at an expectation
	// of zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append, on ONE subject
	// for the whole domain.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the arbitration anchors
	// below what the log no longer holds — [statelog.TrimmedKind], ADR-0073
	// — on one subject per floor, which the subject's id names. The
	// framework removes the anchors in the transaction that applies it; this
	// domain writes nothing for it, and has the case that says so.
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are the five, and THE ORDER IS LOAD-BEARING: the framework's
// conformance suite publishes the first three a domain declares, so a store's
// write comes first.
var ObjectKinds = []ObjectKind{KindStore, KindEviction, KindGeneration, KindBarrier, KindTrimmed}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind may carry a per-subject
// expectation, and so whether the framework keeps an anchor for it.
//
// THE STORE IS, although its writes carry none: its schema changes and its
// purge are arbitrated, and their expectation must be formed against every
// record on the subject, writes included, so the anchor moves with every one —
// which is also why every store a page ever wrote to keeps an anchor until a
// trimmed record removes it. Only the two framework records keep none: the
// barrier, on one subject for the whole domain, and the trimmed record, which
// REMOVES anchors and whose identical copies are each meant to land, so an
// expectation would refuse every one after the first.
func (k ObjectKind) Arbitrated() bool { return k != KindBarrier && k != KindTrimmed }

// InstallsGate reports a kind whose unknown version must STOP the applier. A
// purge is this log's other gate, by its op on an ordinary store subject.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record is published on.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// StoreSubject names one artifact's datastore.
func StoreSubject(id artifacts.ID) Subject { return Subject{Kind: KindStore, ID: id.String()} }

// EvictionSubject names one node's standing in this log.
func EvictionSubject(nodeID string) Subject { return Subject{Kind: KindEviction, ID: nodeID} }

// GenerationSubject names one generation of the stream.
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

// String renders the subject's own path.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.ArtifactDataLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object. A kind this build
// does not know is accepted here and refused where a record is WRITTEN.
func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("artifactdata: a subject with no kind addresses the log's own prefix")
	}
	if strings.ContainsAny(string(s.Kind), ". \t\n*>") {
		return fmt.Errorf("artifactdata: subject kind %q carries a separator or a wildcard", s.Kind)
	}
	if s.ID == "" && s.Kind != KindBarrier {
		return fmt.Errorf("artifactdata: a %s subject needs an id: only the barrier is a "+
			"kind with exactly one object", s.Kind)
	}
	if strings.ContainsAny(s.ID, " \t\r\n*>/") {
		return fmt.Errorf("artifactdata: subject id %q carries whitespace, a wildcard or a slash", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not write.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("artifactdata: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the datastore log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.ArtifactDataLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// gatedStore is the artifact whose deletion marker gates a record on a
// subject, and false for a subject no marker covers. THE SUBJECT ALONE
// DECIDES, for the publisher's side of the gate, which is asked about another
// node's record with no body.
func gatedStore(kind, id string) (string, bool) {
	if ObjectKind(kind) == KindStore && id != "" {
		return id, true
	}
	return "", false
}
