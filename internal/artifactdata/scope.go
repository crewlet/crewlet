package artifactdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the writer and readable at every version, so a node that cannot decode a
// record files the deferral under it and a later read or write probes it.
//
// The alphabet is two levels, because every record here writes exactly one
// store or is a gate about the whole log:
//
//	d             the whole domain: a gate, or an unreadable scope
//	d/s/<artifact> one artifact's datastore, everything in it
//
// The domain letter is `d`, distinct from every other domain's (`t`, `p`, `i`,
// `a`), so a deferral row an operator reads names its log unambiguously.

const (
	pathDomain = "d"
	pathStore  = "s"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermStore is one artifact's datastore, by the artifact's id.
	TermStore TermKind = "store"

	// TermDomain is everything on this log.
	TermDomain TermKind = "domain"
)

// TermKinds are the two.
var TermKinds = []TermKind{TermStore, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// ScopeTerm is one element of a record's blast radius.
type ScopeTerm struct {
	Kind TermKind `json:"k"`
	ID   string   `json:"i,omitempty"`
}

// Path renders the term as a scope path. An unknown kind, or a store term
// naming nothing, is the WHOLE DOMAIN: a claim this build cannot bound is read
// at its widest.
func (t ScopeTerm) Path() string {
	if t.Kind == TermStore && strings.TrimSpace(t.ID) != "" {
		return statelog.ScopePath(pathDomain, pathStore, t.ID)
	}
	return pathDomain
}

// ScopeSet is the COMPLETE set of objects a record's apply may write. Every
// record this build writes touches exactly its subject, which encodes as the
// one-byte sentinel; a newer build may enumerate terms.
type ScopeSet struct {
	Subject bool
	Terms   []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and anything else as an
// array.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings and reads anything else as the domain:
// this runs on a record a newer build may have written, and an error would
// take the whole envelope pass down with it.
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
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT: the envelope
		// pass exists so such a record can be filed rather than dropped.
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
			return errors.New("artifactdata: a scope is either the subject sentinel or " +
				"an enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("artifactdata: a record's scope names nothing, which would " +
			"say it makes no read stale")
	}
	for _, t := range s.Terms {
		if !t.Kind.Valid() {
			return fmt.Errorf("artifactdata: scope term kind %q is not one this build writes", t.Kind)
		}
		if t.Kind == TermStore && t.ID == "" {
			return errors.New("artifactdata: a store term names no store")
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindStore:
		return ScopeTerm{Kind: TermStore, ID: s.ID}.Path()
	case KindBarrier:
		return statelog.BarrierScope
	}
	return pathDomain
}

// StoreScope is the closure of a read about one artifact's datastore.
func StoreScope(id string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermStore, ID: id}.Path()}}
}

// DomainScope is the closure of a read about every store: the company's
// datastore usage.
func DomainScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}
}
