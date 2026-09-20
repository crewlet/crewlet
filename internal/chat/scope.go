package chat

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
// a room must block every write in that room, and a record deferred on one
// room must NOT block a write to another.

// The scope path alphabet.
//
// The domain letter is `c`, where the tracker's is `t` and the wiki's is `p`.
// Scope tables are per domain and a collision across two of them is
// structurally impossible — but a path is the string an operator reads out of
// a deferral row when a fleet is stuck, and two domains whose paths were
// indistinguishable would make that row ambiguous exactly when it matters.
const (
	pathDomain  = "c"
	pathChannel = "c"
	pathName    = "n"
)

// TermKind is what a scope term names.
//
// THERE ARE THREE, AND THERE IS NO PER-MESSAGE TERM. That absence is the
// package's central correctness property rather than an omission, and it is
// enforced here by there being no alphabet for one: a message's apply mints a
// per-channel sequence from LOG ORDER, so a post that declared itself as its
// own object would let a node holding a deferred post go on applying later
// posts in that room and hand them the sequence the deferred one should have
// had. That node's rows then disagree with every other node's for ever, with
// no inverse that repairs it. A message's scope is its CHANNEL, so the
// deferral blocks the room — see the package doc.
type TermKind string

const (
	// TermChannel is one room and EVERYTHING IN IT: its state, its
	// membership, its history and every message ever posted there.
	TermChannel TermKind = "channel"

	// TermName is an ADDRESS — the company's hold on one channel name,
	// named by the same token the subject arbitrates on.
	//
	// ITS OWN TERM rather than a path under the room it names, because a
	// create writes both and the claim outlives nothing: at the moment the
	// record is written the room does not exist yet, so a path under it
	// would be a containment claim about an object no node has.
	TermName TermKind = "name"

	// TermDomain is everything on this domain's log. The fallback for an
	// unreadable scope, and what a gate names.
	TermDomain TermKind = "domain"
)

// TermKinds are the three.
var TermKinds = []TermKind{TermChannel, TermName, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope.
//
// SIXTEEN, matching the wiki's, and the fan-out it covers is smaller still:
// the widest record here is a create, which names the address it claimed and
// the room it made — two terms — and everything else names one. A writer whose
// affected set would exceed this EMITS THE COVERING TERM INSTEAD: a channel
// term already covers every message, member and history row in that room, and
// the domain term covers every channel. So the field is bounded by
// construction rather than by a cap a writer can hit and then have to handle,
// and a record that hits it is a writer enumerating what containment already
// said.
const MaxScopeTerms = 16

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names a room, an address or the domain and nothing
// about the operation that produced it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// ID is the channel's id or the name's TOKEN. Empty for TermDomain,
	// which names everything.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermChannel:
		return join(pathDomain, pathChannel, t.ID)
	case TermName:
		return join(pathDomain, pathName, t.ID)
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
//
// # Why the sentinel carries nothing beside itself
//
// The wiki's sentinel has to carry a CONTAINER, because a page's subject is
// its uuid and which space it lives in is a mutable column only the writer
// knows. Nothing here is in that position: a channel subject's id is the room,
// a message subject's id is ALSO the room, and a name subject's id is the
// address token. Every chat subject already contains everything its path needs,
// which is the same property ruling that a message is channel-scoped — so a
// field here would be a second copy of the channel id that could disagree with
// the first.
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Terms is the enumeration, when the record touches more than its own
	// subject — which here is the create, and nothing else this build
	// writes.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case — every message ever posted — is one byte on the
// wire.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the only
// honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it —
// the record would then be undecodable rather than merely unreadable, and an
// undecodable record yields no subject to file it under and can only be
// dropped.
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
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT, per the
		// paragraph above: this runs on a node decoding a record a newer
		// build wrote, and returning the decode error here would fail the
		// envelope pass that exists precisely so such a record can be
		// filed, gated and reprocessed rather than dropped.
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Validate refuses a scope a writer could not have meant.
//
// It runs where a record is WRITTEN and never where one is read: a scope off
// the wire has already been resolved to the widest term by [ScopeSet.UnmarshalJSON],
// and refusing it here would turn a newer peer's record into a decode failure.
func (s ScopeSet) Validate() error {
	if s.Subject {
		if len(s.Terms) != 0 {
			return invalid("scope", "the exactly-the-subject sentinel is set "+
				"and %d term(s) are enumerated: the two say different things "+
				"about the same record", len(s.Terms))
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return invalid("scope", "a scope names nothing, which is the one claim "+
			"a record may not make — a record that touches exactly its own "+
			"subject sets Subject instead")
	}
	if len(s.Terms) > MaxScopeTerms {
		return invalid("scope", "a record enumerates %d terms and the maximum "+
			"is %d — emit the covering term instead: a %s term already covers "+
			"every message, member and history row in that room",
			len(s.Terms), MaxScopeTerms, TermChannel)
	}
	for _, t := range s.Terms {
		if !t.Kind.Valid() {
			return invalid("scope", "%q is not a term kind this build writes — "+
				"an unknown kind resolves to the whole domain, which is not "+
				"something a writer states on purpose", t.Kind)
		}
		if t.Kind != TermDomain && t.ID == "" {
			return invalid("scope", "a %s term names no id, and an empty "+
				"segment is dropped from the path — the record would resolve "+
				"to the whole domain rather than to the object it names", t.Kind)
		}
		if strings.Contains(t.ID, statelog.ScopeSeparator) {
			return invalid("scope", "term id %q contains %q, which is the path "+
				"separator containment splits on — it would file this record "+
				"under a path no probe for it reaches", t.ID,
				statelog.ScopeSeparator)
		}
	}
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

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindChannel:
		return ScopeTerm{Kind: TermChannel, ID: s.ID}.Path()
	case KindMessage:
		// A MESSAGE'S PATH IS ITS ROOM'S, and it is the SAME path a
		// channel record resolves to rather than one nested under it.
		// The subject's id already IS the channel id, so nothing is
		// derived and nothing can drift; see [TermKind] for what a
		// per-message path would cost.
		return ScopeTerm{Kind: TermChannel, ID: s.ID}.Path()
	case KindChannelName:
		return ScopeTerm{Kind: TermName, ID: s.ID}.Path()
	case KindBarrier:
		// THE FRAMEWORK'S OWN SCOPE, so a barrier never intersects any
		// read's closure. A barrier writes no row anywhere, so a scope
		// that intersected anything would make every linearizable read
		// wait behind every other.
		return statelog.BarrierScope
	default:
		// A GATE IS ABOUT THE WHOLE DOMAIN — it licenses or drops
		// records on every subject — and so is a generation. A KIND
		// THIS BUILD DOES NOT KNOW lands here too, and the domain is
		// the only honest answer for it: narrowing an unknown kind to
		// the room its id happens to look like would be a containment
		// claim the record never made.
		return pathDomain
	}
}
