package accessdomain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ACCESS LOG'S SUBJECTS. Every audit record is its own object, created once
// on a subject naming its seat and its own id, so a retried write lands once and
// a second write for the same turn, serial or command is refused as existing.

// ObjectKind is what an object on the access log is. A kind this build has never
// heard of is retained as its literal, so a newer peer's records are deferred.
type ObjectKind string

const (
	// KindTold is what one turn of a seat was told about its access.
	KindTold ObjectKind = "told"

	// KindSeat is one seat: the subject of its removal, which forgets what it
	// was told.
	KindSeat ObjectKind = "seat"

	// KindEviction is a node's eviction from this log, or its readmission.
	KindEviction ObjectKind = "eviction"

	// KindCertificate is one SSH certificate the engine signed.
	KindCertificate ObjectKind = "certificate"

	// KindCommand is one SSH command the engine ran, or refused on a
	// person's denial.
	KindCommand ObjectKind = "command"

	// KindExpiry is the retention of one hour of records.
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
// sequence (a told record, its seat's removal, a node's eviction).
var ObjectKinds = []ObjectKind{
	KindTold, KindSeat, KindEviction, KindCertificate, KindCommand, KindExpiry,
	KindGeneration, KindBarrier, KindTrimmed,
}

// Valid reports whether a kind off the wire is one this build knows.
func (k ObjectKind) Valid() bool { return slices.Contains(ObjectKinds, k) }

// Arbitrated reports whether writes on this kind carry a per-subject
// expectation: every kind but the barrier, which one subject serves for the
// whole log, and the trimmed record, whose identical copies each land.
func (k ObjectKind) Arbitrated() bool {
	return k.Valid() && k != KindBarrier && k != KindTrimmed
}

// InstallsGate reports a kind whose unknown version must stop the applier: the
// eviction alone.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// seatOwned reports a kind whose subject id is a seat's handle, a dot and the
// object's own id.
func (k ObjectKind) seatOwned() bool {
	return k == KindTold || k == KindCertificate || k == KindCommand
}

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// ToldSubject names what turn of seat handle was told.
func ToldSubject(handle, turn string) Subject {
	return Subject{Kind: KindTold, ID: handle + "." + turn}
}

// CertificateSubject names one certificate by its seat and serial.
func CertificateSubject(handle string, serial uint64) Subject {
	return Subject{Kind: KindCertificate, ID: handle + "." + SerialString(serial)}
}

// CommandSubject names one command by its seat and id.
func CommandSubject(handle, id string) Subject {
	return Subject{Kind: KindCommand, ID: handle + "." + id}
}

// SeatSubject names one seat's hold on what it was told.
func SeatSubject(handle string) Subject { return Subject{Kind: KindSeat, ID: handle} }

// ExpirySubject names the retention of the hour starting at hour.
func ExpirySubject(hour time.Time) Subject {
	return Subject{Kind: KindExpiry, ID: Bucket(hour)}
}

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

// SerialString is a certificate serial as subjects, rows and the audit spell
// it: sixteen lowercase hexadecimal digits.
func SerialString(serial uint64) string { return fmt.Sprintf("%016x", serial) }

// bucketLayout is an expiry bucket's id: an hour in UTC, with no dot.
const bucketLayout = "2006-01-02T15"

// Bucket is the id of the retention hour t falls in.
func Bucket(t time.Time) string { return t.UTC().Truncate(time.Hour).Format(bucketLayout) }

// ParseBucket reads a bucket id back as the hour it starts.
func ParseBucket(id string) (time.Time, error) {
	at, err := time.ParseInLocation(bucketLayout, id, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q is not a retention hour such as 2026-01-02T15",
			ErrInvalid, id)
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
func (s Subject) Wire() string { return topics.AccessLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object. A kind this build
// does not know is accepted here and refused where a record is written.
func (s Subject) Validate() error {
	switch {
	case s.Kind == "":
		return fmt.Errorf("accessdomain: a subject with no kind addresses the log's own prefix")
	case strings.ContainsAny(string(s.Kind), ". \t\n*>"):
		return fmt.Errorf("accessdomain: subject kind %q carries a separator or a wildcard", s.Kind)
	case s.ID == "" && s.Kind != KindBarrier:
		return fmt.Errorf("accessdomain: a %s subject needs an id", s.Kind)
	case strings.ContainsAny(s.ID, " \t\r\n*>/"):
		return fmt.Errorf("accessdomain: subject id %q carries whitespace, a wildcard or a slash", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not write.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("accessdomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the access log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.AccessLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}

// splitOwned takes a seat-owned subject's id apart at its first dot: a seat's
// handle holds no dot.
func splitOwned(id string) (handle, object string, ok bool) {
	handle, object, ok = strings.Cut(id, ".")
	return handle, object, ok && handle != "" && object != ""
}
