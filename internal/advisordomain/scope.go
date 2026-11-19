package advisordomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: every object its apply may write, stated by the writer and
// readable at every version. A run's record writes the per-class table every
// run shares, so a run is scoped to the runs as a whole.
//
//	ad               the whole domain: a gate, an expiry, or an unreadable scope
//	ad/f/<key id>    one finding key, its episodes and their history
//	ad/r             the runs and the newest run of each class
//	ad/w/<week>      one week's report
//	ad/a/<series>    one alarm series and its episodes

const (
	pathDomain  = "ad"
	pathFinding = "f"
	pathRuns    = "r"
	pathReport  = "w"
	pathAlarm   = "a"
)

// TermKind is what a scope term names.
type TermKind string

// The five term kinds.
const (
	// TermFinding is one finding key, by [KeyID].
	TermFinding TermKind = "finding"

	// TermRuns is every run and the per-class table.
	TermRuns TermKind = "runs"

	// TermReport is one week's report.
	TermReport TermKind = "report"

	// TermAlarm is one alarm series, by [AlarmSeriesID].
	TermAlarm TermKind = "alarm"

	// TermDomain is everything on this log.
	TermDomain TermKind = "domain"
)

// TermKinds are the five.
var TermKinds = []TermKind{TermFinding, TermRuns, TermReport, TermAlarm, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope: every record touches
// exactly its subject's object or the whole log.
const MaxScopeTerms = 1

// ScopeTerm is one element of a record's blast radius.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// ID is the key id, week or series the term names; empty for TermRuns
	// and TermDomain.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path. An unknown term kind is the domain:
// the only safe reading of a record this build cannot bound is the widest.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermFinding:
		return join(pathDomain, pathFinding, t.ID)
	case TermRuns:
		return join(pathDomain, pathRuns)
	case TermReport:
		return join(pathDomain, pathReport, t.ID)
	case TermAlarm:
		return join(pathDomain, pathAlarm, t.ID)
	}
	return pathDomain
}

// join builds a scope path; a missing segment is the domain.
func join(segments ...string) string {
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			return pathDomain
		}
	}
	return strings.Join(segments, statelog.ScopeSeparator)
}

// ScopeSet is the complete set of objects a record's apply may write: the
// subject sentinel, or an enumeration. Never empty.
type ScopeSet struct {
	// Subject is true for the sentinel: exactly the object the subject names.
	Subject bool

	// Terms is the enumeration, when the record touches more.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and the rest as an array.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings and reads anything else as the domain,
// never as an error: the envelope pass must survive a newer build's record.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		if sentinel == ScopeSentinel {
			*s = ScopeSet{Subject: true}
			return nil
		}
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		//nolint:nilerr // Widest-on-unreadable is the contract: the record is filed and reprocessed.
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own paths.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// Validate refuses a scope a writer cannot mean.
func (s ScopeSet) Validate() error {
	if s.Subject {
		if len(s.Terms) > 0 {
			return errors.New("advisordomain: a scope is the subject sentinel or an " +
				"enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("advisordomain: a record's scope names nothing")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("advisordomain: a record's scope names %d objects and the cap is %d",
			len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermFinding, TermReport, TermAlarm:
			if t.ID == "" {
				return fmt.Errorf("advisordomain: a %s term names its object", t.Kind)
			}
		case TermRuns, TermDomain:
		default:
			return fmt.Errorf("advisordomain: scope term kind %q is not one this build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindFinding:
		return ScopeTerm{Kind: TermFinding, ID: s.ID}.Path()
	case KindRun:
		return ScopeTerm{Kind: TermRuns}.Path()
	case KindReport:
		return ScopeTerm{Kind: TermReport, ID: s.ID}.Path()
	case KindAlarm:
		return ScopeTerm{Kind: TermAlarm, ID: s.ID}.Path()
	case KindBarrier, KindTrimmed:
		// Neither writes a row of this domain's.
		return statelog.FrameworkScope
	}
	// Every other kind, an unknown one included, is the whole domain.
	return pathDomain
}

// domainScope is the term a log-wide record declares.
func domainScope() ScopeSet { return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}} }

// DomainScope is the closure of a read about every object on the log.
func DomainScope() statelog.ScopeSet { return statelog.ScopeSet{Paths: []string{pathDomain}} }

// KeyScope is the closure of a read about one finding key.
func KeyScope(key string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermFinding, ID: KeyID(key)}.Path()}}
}

// RunsScope is the closure of a read about the runs.
func RunsScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermRuns}.Path()}}
}
