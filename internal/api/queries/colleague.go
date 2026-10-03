// Resolving a name a person typed to a seat, with the engine's own tiers.

package queries

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/iam"
)

// ColleagueQueryMax bounds the text `colleague` resolves, in bytes.
//
// A NAME, not a paragraph: the longest thing a person types to find a seat is
// a role name with a word either side ("the senior platform engineer"), and
// the command palette sends whatever is in its box once typing settles. 200
// bytes is several times the longest role name a company writes, and small
// enough that the fuzzy tier's ratio against every seat stays a
// sub-millisecond read on the largest chart this engine is built for.
const ColleagueQueryMax = 200

// colleagueRow is one candidate on the wire.
type colleagueRow struct {
	Handle string `json:"handle"`
	// Why is the tier that found it, as a person reads it
	// ([colleague.Method.Why]).
	Why string `json:"why"`
}

// colleagueAnswer is the answer: one seat, or an honest list, never a guess.
type colleagueAnswer struct {
	// Match is the seat the text names when exactly ONE does, and null
	// otherwise — including when several do, which is the case a screen
	// must never settle by taking the first row.
	Match *colleagueRow `json:"match"`
	// Candidates is every seat the text could name, best tier first. It
	// holds the match too when there is one, so a list drawn from it is the
	// same list either way; EMPTY means nothing matched, which reads
	// differently from ambiguity ("try another spelling" against "say
	// which of these").
	Candidates []colleagueRow `json:"candidates"`
}

// colleague resolves free text to a seat through the SAME four tiers an
// agent's `lookup_colleague` and `a2a_ask` resolve through.
//
// ONE RESOLVER, because the question is the same one: a person typing "swe"
// into the palette to hand a task over and an agent typing it to ask for help
// both mean whoever the company calls that, and two rankings would send them
// to different seats. It never picks between candidates — `match` is set only
// when the text names exactly one seat — because work handed to the wrong
// colleague on the strength of a suggestion was guessed at, and the engine
// does not guess (`agent/colleague`'s rule).
//
// THE CHAT-ID TIER NEEDS THE CONFIGURATION READ. A seat's contact identities
// are read only through the grant the company document and the chart's runtime
// half are read under (organization-model.md, "What anyone can read about a
// seat"), and an exact-id tier answered to anybody holding the board's read
// would be an oracle for them: paste an id, learn whose it is. So a caller
// without `config:read` resolves over handles and names alone, and one holding
// it over everything the agent's lookup reads.
//
// AND A WITHHELD PERSON IS WITHHELD HERE TOO: the corpus is built with the
// identity directory's withholding ([Sources.WithheldContacts]), exactly as a
// seat's own lookup is, so a person the directory has suspended is not offered
// as somebody to hand work to from a screen while every agent is told they
// cannot be reached.
func (s Sources) colleague(ctx context.Context, p Params) (any, error) {
	text := strings.TrimSpace(p.String("q"))
	if text == "" {
		return nil, badParams("q", "", nil)
	}
	if len(text) > ColleagueQueryMax {
		return nil, fmt.Errorf("%w: q is %d bytes, and a colleague is found by a name of at "+
			"most %d — send the name, not the sentence around it",
			ErrBadParams, len(text), ColleagueQueryMax)
	}
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "colleague")
	}
	var withheld func(handle string) bool
	if s.WithheldContacts != nil {
		withheld = s.WithheldContacts()
	}
	corpus := builtin.Corpus(s.organization(), withheld)
	if !principal.Can(iam.GrantConfigRead) {
		for i := range corpus {
			corpus[i].External = nil
		}
	}
	found := colleague.Resolve(text, corpus)
	out := colleagueAnswer{Candidates: make([]colleagueRow, 0, len(found))}
	for _, c := range found {
		out.Candidates = append(out.Candidates, colleagueRow{Handle: c.Seat.Handle, Why: c.Method.Why()})
	}
	if len(out.Candidates) == 1 {
		match := out.Candidates[0]
		out.Match = &match
	}
	return out, nil
}
