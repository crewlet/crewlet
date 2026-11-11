package accessdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the writer and readable at every version. The paths nest where the rows do,
// so a seat-wide record covers every record of the seat:
//
//	ac                          the whole domain: a gate, an expiry, or an unreadable scope
//	ac/s/<handle>               one seat and every record of it
//	ac/s/<handle>/<k>/<id>      one record of the seat (k: t told, c certificate, m command, v verdict)

const (
	pathDomain = "ac"
	pathSeat   = "s"
)

// objectLetter is the path segment a seat-owned kind sits under.
func objectLetter(k ObjectKind) string {
	switch k {
	case KindTold:
		return "t"
	case KindCertificate:
		return "c"
	case KindCommand:
		return "m"
	case KindVerdict:
		return "v"
	}
	return ""
}

// TermKind is what a scope term names.
type TermKind string

const (
	// TermSeat is one seat, by handle, and every record of it.
	TermSeat TermKind = "seat"

	// TermDomain is everything on this log.
	TermDomain TermKind = "domain"
)

// TermKinds are the two.
var TermKinds = []TermKind{TermSeat, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope: every record touches
// exactly its subject, its seat or the whole log.
const MaxScopeTerms = 1

// ScopeTerm is one element of a record's blast radius.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Seat is the seat's handle, required for TermSeat.
	Seat string `json:"s,omitempty"`
}

// Path renders the term as a scope path. An unknown term kind is the domain:
// the only safe reading of a claim this build cannot bound is the widest.
func (t ScopeTerm) Path() string {
	if t.Kind == TermSeat {
		return join(pathDomain, pathSeat, t.Seat)
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
			return errors.New("accessdomain: a scope is the subject sentinel or an " +
				"enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("accessdomain: a record's scope names nothing")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("accessdomain: a record's scope names %d objects and the cap is %d",
			len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermSeat:
			if t.Seat == "" {
				return errors.New("accessdomain: a seat term names its seat")
			}
		case TermDomain:
		default:
			return fmt.Errorf("accessdomain: scope term kind %q is not one this build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch {
	case s.Kind.seatOwned():
		handle, object, ok := splitOwned(s.ID)
		if !ok {
			return pathDomain
		}
		return join(pathDomain, pathSeat, handle, objectLetter(s.Kind), object)
	case s.Kind == KindSeat:
		return ScopeTerm{Kind: TermSeat, Seat: s.ID}.Path()
	case s.Kind == KindBarrier, s.Kind == KindTrimmed:
		// The framework's own scope: neither writes a row of this domain's.
		return statelog.FrameworkScope
	}
	// A gate, an expiry, a generation and an unknown kind are the whole domain.
	return pathDomain
}

// seatScope is the term a seat-wide record declares.
func seatScope(handle string) ScopeSet {
	return ScopeSet{Terms: []ScopeTerm{{Kind: TermSeat, Seat: handle}}}
}

// domainScope is the term a log-wide record declares.
func domainScope() ScopeSet { return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}} }

// DomainScope is the closure of a read about every seat.
func DomainScope() statelog.ScopeSet { return statelog.ScopeSet{Paths: []string{pathDomain}} }

// SeatScope is the closure of a read about one seat's records.
func SeatScope(handle string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermSeat, Seat: handle}.Path()}}
}
