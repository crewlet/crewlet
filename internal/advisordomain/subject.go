package advisordomain

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADVISOR LOG'S SUBJECTS (package doc, "Subjects"): a finding key is one
// subject, arbitrated on its own version, so one key has at most one non-closed
// episode; a run, a report, an alarm series and a day's retention are each one.

// ObjectKind is what an object on the advisor log is. A kind this build has
// never heard of is retained as its literal, so a newer peer's records are
// deferred.
type ObjectKind string

const (
	// KindFinding is one finding key, by [KeyID]: every record about every
	// episode of the key.
	KindFinding ObjectKind = "finding"

	// KindRun is one run of the Advisor, by its uuid: a person's request and
	// the pass's record.
	KindRun ObjectKind = "run"

	// KindReport is one weekly report, by its ISO week ([IsWeek]).
	KindReport ObjectKind = "report"

	// KindAlarm is one alarm series, by [AlarmSeriesID]: its episodes.
	KindAlarm ObjectKind = "alarm"

	// KindRetention is one UTC day's expiry, by the day ([Day]).
	KindRetention ObjectKind = "retention"

	// KindEviction is a node's eviction from this log, or its readmission.
	KindEviction ObjectKind = "eviction"

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
// suite publishes the first three, twice each, in order, and they are a real
// sequence (a finding opened, the run that found it recorded, the week's
// report written).
var ObjectKinds = []ObjectKind{
	KindFinding, KindRun, KindReport, KindAlarm, KindRetention, KindEviction, KindGeneration,
	KindBarrier, KindTrimmed,
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
// eviction, this log's only gate.
func (k ObjectKind) InstallsGate() bool { return k == KindEviction }

// Subject is the object a record arbitrates over.
type Subject struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i,omitempty"`
}

// keyNamespace and seriesNamespace are the name spaces a finding key's and an
// alarm series' ids are derived in. Fixed for good: another would give every
// key a second subject.
var (
	keyNamespace    = uuid.MustParse("6f1c2b9e-84d3-4a57-b0e6-3d92a7c15f48")
	seriesNamespace = uuid.MustParse("a83e5d17-2c4b-4f90-9d61-58b7e0c3a2f9")
)

// KeyID is a finding key's subject id: a UUIDv5 of the key, since a key holds
// `.`, `:` and `/` and so never goes on the wire itself.
func KeyID(key string) string { return uuid.NewSHA1(keyNamespace, []byte(key)).String() }

// AlarmSeriesID is one alarm series' subject id: a UUIDv5 of the node, the
// alarm's kind and its instance.
func AlarmSeriesID(node, kind, instance string) string {
	return uuid.NewSHA1(seriesNamespace, []byte(node+"\x00"+kind+"\x00"+instance)).String()
}

// FindingSubject names one finding key: the subject of every record about it.
func FindingSubject(key string) Subject { return Subject{Kind: KindFinding, ID: KeyID(key)} }

// RunSubject names one run by its uuid.
func RunSubject(run string) Subject { return Subject{Kind: KindRun, ID: run} }

// ReportSubject names one week's report.
func ReportSubject(week string) Subject { return Subject{Kind: KindReport, ID: week} }

// AlarmSubject names one alarm series.
func AlarmSubject(node, kind, instance string) Subject {
	return Subject{Kind: KindAlarm, ID: AlarmSeriesID(node, kind, instance)}
}

// RetentionSubject names the expiry of the UTC day day falls in.
func RetentionSubject(day time.Time) Subject { return Subject{Kind: KindRetention, ID: Day(day)} }

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

// IsUUID reports whether s is a uuid in its canonical lowercase form, as every
// episode, run and derived id is spelled.
func IsUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// dayLayout is a retention subject's id: a UTC day, with no dot.
const dayLayout = time.DateOnly

// Day is the id of the UTC day t falls in, such as 2026-11-19.
func Day(t time.Time) string { return t.UTC().Format(dayLayout) }

// weekPattern is an ISO week's one spelling: the ISO year, `-W` and the week.
var weekPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([0-9]{4})-W([0-9]{2})$`)
})

// IsWeek reports whether s names an ISO week that exists, such as 2026-W47.
func IsWeek(s string) bool {
	m := weekPattern().FindStringSubmatch(s)
	if m == nil {
		return false
	}
	year, _ := strconv.Atoi(m[1])
	week, _ := strconv.Atoi(m[2])
	// December 28th always falls in a year's last ISO week.
	_, last := time.Date(year, time.December, 28, 0, 0, 0, 0, time.UTC).ISOWeek()
	return week >= 1 && week <= last
}

// Week is the ISO week t falls in, as a report names it.
func Week(t time.Time) string {
	year, week := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// String renders the subject's own path.
func (s Subject) String() string {
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + "." + s.ID
}

// Wire is the full subject the record is published to.
func (s Subject) Wire() string { return topics.AdvisorLogSubject(string(s.Kind), s.ID) }

// framework is the subject as the framework names one.
func (s Subject) framework() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID}
}

// Validate refuses a subject that cannot address an object. A kind this build
// does not know is accepted here and refused where a record is written.
func (s Subject) Validate() error {
	switch {
	case s.Kind == "":
		return fmt.Errorf("advisordomain: a subject with no kind addresses the log's own prefix")
	case strings.ContainsAny(string(s.Kind), ". \t\n*>"):
		return fmt.Errorf("advisordomain: subject kind %q carries a separator or a wildcard", s.Kind)
	case s.ID == "" && s.Kind != KindBarrier:
		return fmt.Errorf("advisordomain: a %s subject needs an id", s.Kind)
	case strings.ContainsAny(s.ID, " \t\r\n*>/"):
		return fmt.Errorf("advisordomain: subject id %q carries whitespace, a wildcard or a slash", s.ID)
	}
	return nil
}

// writable refuses a subject this build may not write.
func (s Subject) writable() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("advisordomain: %s is not a kind this build writes", s.Kind)
	}
	return nil
}

// ParseSubject recovers a subject from a wire subject on the advisor log.
func ParseSubject(wire string) (Subject, bool) {
	kind, id, ok := topics.AdvisorLogPath(wire)
	if !ok {
		return Subject{}, false
	}
	return Subject{Kind: ObjectKind(kind), ID: id}, true
}
