package atlasdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ATLAS LOG'S SUBJECTS. An entry is one subject, so every person's claim on
// it arbitrates there: a person's edit over an edit they did not see is refused
// on the entry's own version, and two entries never serialize behind each
// other. A purged value is a subject of its own, named by its digest.

// ObjectKind is what an object on the atlas log is. A kind this build has never
// heard of is retained as its literal, so a newer peer's records are deferred.
type ObjectKind string

const (
	// KindEntry is one entry of the atlas, by its id ([atlas.Ref.ID]): the
	// subject of every claim on it and of its purge.
	KindEntry ObjectKind = "entry"

	// KindValue is one value, by the digest of its text ([Digest]): the
	// subject of its purge, which erases it wherever it appears.
	KindValue ObjectKind = "value"

	// KindEviction is a node's eviction from this log, or its readmission.
	KindEviction ObjectKind = "eviction"

	// KindExpiry is the retention of one hour of history.
	KindExpiry ObjectKind = "expiry"

	// KindGeneration is a reanchor's record, create-only at an expectation of
	// zero.
	KindGeneration ObjectKind = "generation"

	// KindBarrier is the read index's payload-free append on one subject.
	KindBarrier ObjectKind = "barrier"

	// KindTrimmed is the trim's record that removes the anchors below a floor
	// ([statelog.TrimmedKind], ADR-0080).
	KindTrimmed ObjectKind = statelog.TrimmedKind
)

// ObjectKinds are every kind, and THE ORDER IS LOAD-BEARING: the conformance
// suite publishes the first three, twice each, in order, and these are a real
// sequence (a claim on an entry, a value's purge that erases it, a node's
// eviction).
var ObjectKinds = []ObjectKind{
	KindEntry, KindValue, KindEviction, KindExpiry, KindGeneration, KindBarrier, KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation: every kind but the barrier, which one subject serves for the
// whole log, and the trimmed record, whose identical copies each land.
func (k ObjectKind) Arbitrated() bool {
	return k.Valid() && k != KindBarrier && k != KindTrimmed
}

// InstallsGate reports a kind whose every record installs a gate: the
// eviction. A purge is the log's other gate, by its op on an entry or a value.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Purgeable reports a kind whose object a purge erases and marks for good.
func (k ObjectKind) Purgeable() bool { return k == KindEntry || k == KindValue }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// EntrySubject names one entry by its ref: the subject of every claim on it.
func EntrySubject(r atlas.Ref) Subject { return Subject{Kind: KindEntry, ID: r.ID().String()} }

// ValueSubject names one value by the digest of its text.
func ValueSubject(text string) Subject { return Subject{Kind: KindValue, ID: Digest(text)} }

// ExpirySubject names the retention of the hour starting at hour.
func ExpirySubject(hour time.Time) Subject { return Subject{Kind: KindExpiry, ID: Bucket(hour)} }

// EvictionSubject names one node's standing in this log.
func EvictionSubject(nodeID string) Subject { return Subject{Kind: KindEviction, ID: nodeID} }

// BarrierSubject is the read index's one subject.
func BarrierSubject() Subject { return Subject{Kind: KindBarrier} }

// TrimmedSubject names the trimmed record whose floor is floor, in the
// framework's one spelling of it.
func TrimmedSubject(floor statelog.Position) Subject {
	return Subject{Kind: KindTrimmed, ID: statelog.TrimmedSubject(floor).ID}
}

// GenerationSubject names one generation of the stream.
func GenerationSubject(gen uint32) Subject {
	return Subject{Kind: KindGeneration, ID: strconv.FormatUint(uint64(gen), 10)}
}

// Digest is a value's name on the log and in every row that marks it: the
// SHA-256 of its text, in lowercase hex. A purge record and its marker carry
// this and never the text, so erasing a value does not write it down again.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// digestPattern is a digest's one spelling.
var digestPattern = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{64}$`) })

// IsDigest reports whether s is spelled as [Digest] spells one.
func IsDigest(s string) bool { return digestPattern().MatchString(s) }

// IsEntryID reports whether s is spelled as every entry id is: a uuid in its
// canonical lowercase form.
func IsEntryID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// bucketLayout is an expiry bucket's id: an hour in UTC, with no dot.
const bucketLayout = "2006-01-02T15"

// Bucket is the id of the retention hour t falls in.
func Bucket(t time.Time) string { return t.UTC().Truncate(time.Hour).Format(bucketLayout) }

// ParseBucket reads a bucket id back as the hour it starts.
func ParseBucket(id string) (time.Time, error) {
	at, err := time.ParseInLocation(bucketLayout, id, time.UTC)
	if err != nil {
		return time.Time{}, invalid("%q is not a retention hour such as 2026-01-02T15", id)
	}
	return at, nil
}

// String renders the subject's own path.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.AtlasLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object. A kind this build
// does not know is accepted here and refused where a record is written.
func (s Subject) Validate() error {
	switch {
	case s.Kind == "":
		return fmt.Errorf("atlasdomain: a subject with no kind addresses the log's own prefix")
	case strings.ContainsAny(string(s.Kind), ". \t\n*>"):
		return fmt.Errorf("atlasdomain: subject kind %q carries a separator or a wildcard", s.Kind)
	case s.ID == "" && s.Kind != KindBarrier:
		return fmt.Errorf("atlasdomain: a %s subject needs an id", s.Kind)
	case strings.ContainsAny(s.ID, " \t\r\n*>/"):
		return fmt.Errorf("atlasdomain: subject id %q carries whitespace, a wildcard or a slash", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not write.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("atlasdomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the atlas log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.AtlasLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
