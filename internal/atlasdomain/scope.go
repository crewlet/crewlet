package atlasdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the writer and readable at every version. A claim writes its own entry's rows
// alone; a purge reaches every entry that names what it erases, so it is the
// whole domain, as an expiry and a gate are.
//
//	at                 the whole domain: a gate, a purge, an expiry, or an unreadable scope
//	at/e/<entry id>    one entry and every claim on it

const (
	pathDomain = "at"
	pathEntry  = "e"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermEntry is one entry, by id, and every claim on it.
	TermEntry TermKind = "entry"

	// TermDomain is everything on this log.
	TermDomain TermKind = "domain"
)

// TermKinds are the two.
var TermKinds = []TermKind{TermEntry, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope: every record touches
// exactly its subject, its entry or the whole log.
const MaxScopeTerms = 1

// ScopeTerm is one element of a record's blast radius.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Entry is the entry's id, required for TermEntry.
	Entry string `json:"e,omitempty"`
}

// Path renders the term as a scope path. An unknown term kind is the domain:
// the only safe reading of a claim this build cannot bound is the widest.
func (t ScopeTerm) Path() string {
	if t.Kind == TermEntry {
		return join(pathDomain, pathEntry, t.Entry)
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
			return errors.New("atlasdomain: a scope is the subject sentinel or an " +
				"enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("atlasdomain: a record's scope names nothing")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("atlasdomain: a record's scope names %d objects and the cap is %d",
			len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermEntry:
			if t.Entry == "" {
				return errors.New("atlasdomain: an entry term names its entry")
			}
		case TermDomain:
		default:
			return fmt.Errorf("atlasdomain: scope term kind %q is not one this build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindEntry:
		return ScopeTerm{Kind: TermEntry, Entry: s.ID}.Path()
	case KindBarrier, KindTrimmed:
		// The framework's own scope: neither writes a row of this domain's.
		return statelog.FrameworkScope
	}
	// A value, a gate, an expiry, a generation and an unknown kind are the
	// whole domain.
	return pathDomain
}

// domainScope is the term a log-wide record declares.
func domainScope() ScopeSet { return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}} }

// DomainScope is the closure of a read about every entry.
func DomainScope() statelog.ScopeSet { return statelog.ScopeSet{Paths: []string{pathDomain}} }

// EntryScope is the closure of a read about one entry's own claims.
func EntryScope(id string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermEntry, Entry: id}.Path()}}
}
