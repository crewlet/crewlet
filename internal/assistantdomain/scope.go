package assistantdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE is the complete set of objects its apply may write, stated
// by the writer and readable at every version: a node that cannot decode a
// record defers it under its scope, which later reads and writes probe. Paths
// nest as the rows do (a turn and a proposal under their conversation, whose
// activity they move), so a deferred turn record blocks a conversation read
// and a deferred purge blocks a write to its turns. The letter `c` is unique
// among domains, so a deferral row names its domain.
//
//	c                         the whole domain: a gate, or an unreadable scope
//	c/x/<conversation>        one conversation and everything in it
//	c/x/<conversation>/t/<t>  one turn of it
//	c/x/<conversation>/p/<p>  one proposal made in it

const (
	pathDomain       = "c"
	pathConversation = "x"
	pathTurn         = "t"
	pathProposal     = "p"
)

// TermKind is what a scope term names.
type TermKind string

const (
	// TermConversation is one conversation by id.
	TermConversation TermKind = "conversation"

	// TermTurn is one turn, within its conversation.
	TermTurn TermKind = "turn"

	// TermProposal is one proposal, within its conversation.
	TermProposal TermKind = "proposal"

	// TermDomain is everything on this log: what a gate names and what an
	// unreadable scope reads as.
	TermDomain TermKind = "domain"
)

// TermKinds are the four.
var TermKinds = []TermKind{TermConversation, TermTurn, TermProposal, TermDomain}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// MaxScopeTerms is the cap on a record's declared scope: no record names more
// than one object besides its subject, so the cap catches a writer's mistake
// rather than bounding a real record.
const MaxScopeTerms = 2

// ScopeTerm is one element of a record's blast radius. It names an object,
// nothing about the operation, so a build that does not know the op reads it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Conversation is the conversation a turn or a proposal lives in.
	// Required for TermTurn and TermProposal and empty for every other.
	Conversation string `json:"c,omitempty"`

	// ID is the conversation's, the turn's or the proposal's id. Empty for
	// TermDomain.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermConversation:
		return join(pathDomain, pathConversation, t.ID)
	case TermTurn:
		return join(pathDomain, pathConversation, t.Conversation, pathTurn, t.ID)
	case TermProposal:
		return join(pathDomain, pathConversation, t.Conversation, pathProposal, t.ID)
	case TermDomain:
		return pathDomain
	}
	// An unknown term kind is the domain, never skipped: an unbounded claim
	// is safe only read as the widest.
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
// Never empty: exactly-the-subject encodes as the one-byte sentinel, so the
// field is never absent by accident, and an absent or unreadable scope reads
// as the domain term, never "just the subject".
type ScopeSet struct {
	// Subject is true for the sentinel: the record touches exactly its
	// subject's object.
	Subject bool

	// Terms enumerates the objects when the record touches more than that.
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

// UnmarshalJSON accepts both encodings, and reads anything else as the domain,
// never an error: it runs on a record a newer build wrote, and an error would
// take the whole envelope pass down.
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
		// doc above.
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
			return errors.New("assistantdomain: a scope is either the subject " +
				"sentinel or an enumeration, and this one is both")
		}
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("assistantdomain: a record's scope names nothing, which " +
			"would say it makes no read stale: the one claim a record this build " +
			"may not be able to decode must never make")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("assistantdomain: a record's scope names %d objects and "+
			"the cap is %d", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermConversation:
			if t.ID == "" {
				return errors.New("assistantdomain: a conversation term names nothing")
			}
		case TermTurn, TermProposal:
			if t.ID == "" || t.Conversation == "" {
				return fmt.Errorf("assistantdomain: a %s term names its "+
					"conversation and itself", t.Kind)
			}
		case TermDomain:
		default:
			return fmt.Errorf("assistantdomain: scope term kind %q is not one this "+
				"build writes", t.Kind)
		}
	}
	return nil
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject) string {
	switch s.Kind {
	case KindConversation:
		return ScopeTerm{Kind: TermConversation, ID: s.ID}.Path()
	case KindTurn, KindProposal:
		conversation, child, ok := splitChild(s.ID)
		if !ok {
			// Unparseable: the domain, not a narrower guess.
			return pathDomain
		}
		kind := TermTurn
		if s.Kind == KindProposal {
			kind = TermProposal
		}
		return ScopeTerm{Kind: kind, Conversation: conversation, ID: child}.Path()
	}
	if s.Kind == KindBarrier || s.Kind == KindTrimmed {
		// Neither framework record writes this domain's rows, so neither
		// may intersect any read's closure.
		return statelog.FrameworkScope
	}
	// A gate, or a kind this build does not know: the whole domain.
	return pathDomain
}

// DomainScope is the closure of a read about every conversation: a person's
// listing and the retention reaper's question.
func DomainScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}
}

// ConversationScope is the closure of a read about one conversation and
// everything in it.
func ConversationScope(conversation string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermConversation, ID: conversation}.Path(),
	}}
}
