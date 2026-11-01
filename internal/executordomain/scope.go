package executordomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the WRITER and readable at every version.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows one thing about a path, that it nests left to right, so the alphabet
// is chosen so the paths NEST where the rows do: a conversation sits under its
// seat, so a seat-wide record (a capture that may release any of the seat's
// ended sessions, a seat's removal) covers every conversation of it, and a
// deferred one blocks a read of any of them.
//
//	e                        the whole domain: a gate, or an unreadable scope
//	e/s/<handle>             one seat and every conversation of it
//	e/s/<handle>/c/<digest>  one conversation of the seat
//
// The domain letter is `e`, distinct from every other domain's: a path is what
// an operator reads out of a deferral row when a fleet is stuck, and two
// domains whose paths looked alike would make that row ambiguous exactly when
// it matters.

const (
	pathDomain       = "e"
	pathSeat         = "s"
	pathConversation = "c"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermSeat is one seat, by handle, and every conversation of it.
	TermSeat TermKind = "seat"

	// TermConversation is one conversation, within its seat.
	TermConversation TermKind = "conversation"

	// TermDomain is everything on this log: what a gate names and what an
	// unreadable scope reads as.
	TermDomain TermKind = "domain"
)

// TermKinds are the three.
var TermKinds = []TermKind{TermSeat, TermConversation, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope.
//
// ONE: every record here touches exactly its subject or exactly its seat. The
// cap is a writer's mistake caught rather than a bound a writer has to handle.
const MaxScopeTerms = 1

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names an object and nothing about the operation.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Seat is the seat's handle. Required for TermSeat and TermConversation.
	Seat string `json:"s,omitempty"`

	// Conversation is the conversation's digest ([ConversationID]).
	// Required for TermConversation and empty for every other.
	Conversation string `json:"c,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermSeat:
		return join(pathDomain, pathSeat, t.Seat)
	case TermConversation:
		return join(pathDomain, pathSeat, t.Seat, pathConversation, t.Conversation)
	case TermDomain:
		return pathDomain
	}
	// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM: a term this
	// build cannot interpret is a claim about a blast radius it cannot
	// bound, and the only safe reading of an unbounded claim is the widest.
	return pathDomain
}

// join builds a scope path from its segments. A MISSING SEGMENT IS THE DOMAIN
// rather than a path with a hole in it.
func join(segments ...string) string {
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			return pathDomain
		}
	}
	return strings.Join(segments, statelog.ScopeSeparator)
}

// ScopeSet is the COMPLETE set of objects a record's apply may write.
//
// NEVER EMPTY. The exactly-the-subject case encodes as the one-byte sentinel,
// so the field can never be absent by accident. An absent or unreadable scope
// on a record this build cannot decode is the DOMAIN term, never "just the
// subject".
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Terms is the enumeration, when the record touches more than its own
	// subject.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set: this runs on a node reading a record a newer build wrote, and an
// error here would take the whole envelope pass down with it.
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
		// paragraph above: returning the decode error would fail the
		// envelope pass that exists so such a record can be filed, gated
		// and reprocessed rather than dropped.
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
			return errors.New("executordomain: a scope is either the subject " +
				"sentinel or an enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("executordomain: a record's scope names nothing, which " +
			"would say it makes no read stale: the one claim a record this build " +
			"may not be able to decode must never make")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("executordomain: a record's scope names %d objects and "+
			"the cap is %d", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermSeat:
			if t.Seat == "" || t.Conversation != "" {
				return errors.New("executordomain: a seat term names its seat and " +
					"nothing else")
			}
		case TermConversation:
			if t.Seat == "" || t.Conversation == "" {
				return errors.New("executordomain: a conversation term names its " +
					"seat and itself")
			}
		case TermDomain:
		default:
			return fmt.Errorf("executordomain: scope term kind %q is not one this "+
				"build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindConversation:
		handle, conversation, ok := splitConversation(s.ID)
		if !ok {
			// AN UNPARSEABLE CONVERSATION SUBJECT IS THE DOMAIN: a
			// narrower guess would be a claim the record never made.
			return pathDomain
		}
		return ScopeTerm{Kind: TermConversation, Seat: handle, Conversation: conversation}.Path()
	case KindSeat:
		return ScopeTerm{Kind: TermSeat, Seat: s.ID}.Path()
	case KindBarrier:
		// THE FRAMEWORK'S OWN SCOPE, so a barrier never intersects any
		// read's closure.
		return statelog.BarrierScope
	}
	// A GATE IS ABOUT THE WHOLE DOMAIN, and so is a kind this build does
	// not know: anything narrower would be a claim the record never made.
	return pathDomain
}

// seatScope is the term a seat-wide record declares.
func seatScope(handle string) ScopeSet {
	return ScopeSet{Terms: []ScopeTerm{{Kind: TermSeat, Seat: handle}}}
}

// DomainScope is the closure of a read about every seat: the retention duty's
// nomination, the collector's pin, the census of sealed keys.
func DomainScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}
}

// SeatScope is the closure of a read about one seat and every conversation of
// it.
func SeatScope(handle string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermSeat, Seat: handle}.Path()}}
}

// ConversationScope is the closure of a read about one conversation of a seat.
func ConversationScope(handle, conversationKey, turn string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermConversation, Seat: handle,
		Conversation: ConversationID(conversationKey, turn)}.Path()}}
}
