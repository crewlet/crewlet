package pages

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the WRITER and readable at every version.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows exactly one thing about a scope path — that it is a hierarchy written
// left to right with a separator, from which it computes CONTAINMENT — so
// everything this file is for rests on the paths NESTING: a record deferred on
// a container must block a write to a page inside it, and a record deferred on
// one page must NOT block a write to its neighbour.

// ContainerlessSpace is the container a record names when its object lives at
// the top of the knowledge base rather than in a space.
//
// A REAL NAME rather than an empty one, so "everything outside a container" is
// a term a writer can state and a probe can match, and so no path in the
// alphabet has a hole in the middle of it.
const ContainerlessSpace = "loose"

// The scope path alphabet.
//
// The domain letter is `p` rather than the tracker's `t`. Scope tables are per
// domain and a collision across two of them is structurally impossible — but
// a path is the string an operator reads out of a deferral row when a fleet is
// stuck, and two domains whose paths were indistinguishable would make that
// row ambiguous exactly when it matters.
const (
	pathDomain    = "p"
	pathContainer = "c"
	pathObject    = "o"
	pathTitle     = "n"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermObject is one page by id, WITHIN its container.
	TermObject TermKind = "object"

	// TermTitle is an ADDRESS — a container's hold on one title, named by
	// the same token the subject arbitrates on — because a rename touches
	// two of them and neither is a page.
	TermTitle TermKind = "title"

	// TermContainer is a space key, or [ContainerlessSpace].
	TermContainer TermKind = "container"

	// TermDomain is everything on this domain's log. The fallback for an
	// unreadable scope, and what a gate names.
	TermDomain TermKind = "domain"
)

// TermKinds are the four.
var TermKinds = []TermKind{TermObject, TermTitle, TermContainer, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope.
//
// SIXTEEN, and it is smaller than the tracker's sixty-four because the fan-out
// it has to cover is smaller: the widest record here is a rename, which names
// two titles and a page, and the next widest is a container purge, which names
// the container and nothing else because a container term COVERS every page
// under it. A writer whose affected set would exceed this emits the covering
// container term instead, so the field is bounded by construction rather than
// by a cap a writer can hit and then have to handle.
const MaxScopeTerms = 16

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names a page, a title, a container or the domain and
// nothing about the operation that produced it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Container is the space key an object or a title lives in, or
	// [ContainerlessSpace]. Required for TermObject and TermTitle, empty
	// for the other two, and never guessed: see the alphabet above.
	Container string `json:"c,omitempty"`

	// ID is the page's uuid, the normalised title or the container's key.
	// Empty for TermDomain, which names everything.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermObject:
		return join(pathDomain, pathContainer, t.container(), pathObject, t.ID)
	case TermTitle:
		return join(pathDomain, pathContainer, t.container(), pathTitle, t.ID)
	case TermContainer:
		// THE CONTAINER TERM'S OWN NAME IS ITS ID, not its Container
		// field: reading the field here would resolve every container
		// term to the containerless space and make a space-wide
		// deferral cover the whole knowledge base.
		key := t.ID
		if key == "" {
			key = t.container()
		}
		return join(pathDomain, pathContainer, key)
	case TermDomain:
		return pathDomain
	}
	// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM. A term this
	// build cannot interpret is a claim about a blast radius it cannot
	// bound, and the only safe reading of an unbounded claim is the widest
	// one. Dropping it would silently narrow a newer peer's scope to
	// whatever this build happened to recognise.
	return pathDomain
}

// container is the term's container, defaulting to the containerless space.
func (t ScopeTerm) container() string {
	if t.Container == "" {
		return ContainerlessSpace
	}
	return t.Container
}

// join builds a scope path from its segments, skipping empty ones so a missing
// id can never produce a path with a hole in it.
func join(segments ...string) string {
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, statelog.ScopeSeparator)
}

// ScopeSet is the COMPLETE set of objects a record's apply may write.
//
// NEVER EMPTY. The exactly-the-subject case — almost every record — encodes as
// the one-byte sentinel, so the field can never be absent by accident. An
// absent or unreadable scope on a record this build cannot decode is the
// DOMAIN term, never "just the subject": assuming a V1 convention holds for a
// V2 operation is the bug this field exists to close.
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Container is the space the subject lives in, and rides the sentinel.
	//
	// # Why the container is ON THE RECORD and not derived from the subject
	//
	// A page's subject is its uuid, and which container it lives in is a
	// fact about the row — one that CHANGES, because a page can be moved
	// between spaces. So the path a record's scope resolves to cannot be
	// computed from the subject alone, and the only party that knows it at
	// the moment the record is written is the writer.
	//
	// Leaving it out is silent rather than wrong-looking: every page record
	// would file its deferral under the containerless space while every
	// space-scoped read probed its own, and the containment probe would
	// simply never match.
	Container string

	// Terms is the enumeration, when the record touches more than its own
	// subject. A container rides the sentinel and never an enumeration:
	// every term already states its own.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
//
// A container is appended to the sentinel with the path separator — "s/ENG" —
// which keeps the containerless case at one byte and costs a space key on the
// records that have one. The separator cannot appear inside a container: a
// space key is upper-case and slug-shaped, checked where a container is
// created.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		if s.Container != "" {
			return json.Marshal(ScopeSentinel + statelog.ScopeSeparator + s.Container)
		}
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the only
// honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		head, container, _ := strings.Cut(sentinel, statelog.ScopeSeparator)
		if head == ScopeSentinel {
			*s = ScopeSet{Subject: true, Container: container}
			return nil
		}
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own paths.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject, s.Container)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject, container string) string {
	switch s.Kind {
	case KindContainer:
		// A CONTAINER'S OWN PATH IS THE CONTAINER, which is what makes
		// a deferred settings edit block every write inside that space
		// — which is exactly what archiving one is.
		return ScopeTerm{Kind: TermContainer, ID: s.ID}.Path()
	case KindTitle:
		space, token, err := SplitTitleID(s.ID)
		if err != nil {
			// AN UNPARSEABLE TITLE SUBJECT IS THE DOMAIN. It came off
			// the wire from a build whose composition this one does
			// not know, and a narrower guess would be a claim the
			// record never made.
			return pathDomain
		}
		// THE TOKEN, not the title: a scope path is compared for
		// equality and containment and never read back, and the title
		// is prose a path separator can appear inside.
		return ScopeTerm{Kind: TermTitle, Container: space, ID: token}.Path()
	case KindBarrier:
		// THE FRAMEWORK'S OWN SCOPE, so a barrier never intersects any
		// read's closure. A barrier writes no row anywhere, so a scope
		// that intersected anything would make every linearizable read
		// wait behind every other.
		return statelog.BarrierScope
	case KindGeneration, KindEviction:
		// A GATE IS ABOUT THE WHOLE DOMAIN. It licenses or drops
		// records on every subject, so anything narrower would be a
		// claim the record does not make.
		return pathDomain
	default:
		return ScopeTerm{Kind: TermObject, Container: container, ID: s.ID}.Path()
	}
}
