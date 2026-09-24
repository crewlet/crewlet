// Who the caller is, which is the question every personal surface rests on.

package queries

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The two refusals a personal question makes, told apart because the remedies
// are different: one is a line of company configuration, the other a
// credential.
var (
	errNoSeat = fmt.Errorf("%w: this credential is not bound to a seat — give a human "+
		"seat contact.crewlet_operator_id matching the api.auth token id, or name a handle",
		ErrBadParams)
	errNotYours = fmt.Errorf("%w: reading another seat's record needs an operator credential",
		ErrUnauthorized)
)

// viewer answers who this socket's credential belongs to.
//
// THE FRAME HAS NEVER HAD A VIEWER, and everything personal in the dashboard
// is fiction without one: "my work" picked the alphabetically first seat,
// `work_views` was asked without a viewer so no pinned or personal view could
// exist, the five `preset=` values that resolve against the caller's identity
// were never sent, and an inbox could not be anyone's.
//
// The resolution is a chain of two, and NEITHER STEP IS NEW: Tier A's
// `api.auth.tokens` maps a presented credential to an operator id, and a seat
// binds one of those ids with `contact.crewlet_operator_id`. What was missing
// is a question that walks it.
//
// AN UNBOUND TOKEN IS AN ORDINARY STATE. `org.HumanContact` says so in as many
// words, so this answers the operator id with no seat rather than an error —
// the screen then says what to bind, which is a different thing from a screen
// that looks broken.
func (s Sources) viewer(ctx context.Context, _ Params) (any, error) {
	operatorID := operatorFrom(ctx)
	out := map[string]any{
		"operator_id": operatorID,
		// WHETHER THIS CALLER MAY ASK THE GUARDED QUESTIONS, which is the
		// same test the registry makes, answered once so a screen can draw
		// a locked row rather than discovering the refusal per question.
		"operator": operatorID != "",
		"handle":   "",
		"name":     "",
		"kind":     "",
		// WHAT THIS CALLER MAY DO on the act transport, which admits a
		// person and nobody else (ADR-0024): empty for an anonymous
		// reader and for a token no seat binds, so a screen disables its
		// write controls with the reason rather than offering a press
		// the engine refuses. ALWAYS AN ARRAY, never null, so "may do
		// nothing" is a value a reader can test rather than an absence.
		"acts": []string{},
	}
	seat := s.seatForOperator(operatorID)
	if seat == nil {
		return out, nil
	}
	if s.OperatorActs != nil {
		if acts := s.OperatorActs(); acts != nil {
			out["acts"] = acts
		}
	}
	out["handle"] = seat.Handle()
	out["name"] = seat.Name
	// The zero value is an agent, which is what config.Role's own `kind`
	// defaults to — reporting "" would make the common case look unset.
	kind := seat.Kind
	if kind == "" {
		kind = org.KindAgent
	}
	out["kind"] = string(kind)
	return out, nil
}

// seatForOperator resolves the caller's operator id to a seat, or nil.
func (s Sources) seatForOperator(operatorID string) *org.Role {
	organization := s.organization()
	if operatorID == "" || organization == nil {
		return nil
	}
	// NIL LOOKUP, so the reference resolves against this process's own
	// environment — which is where Tier B's `${VAR}` pointers are resolved
	// everywhere else in the engine.
	return organization.SeatByOperatorID(operatorID, nil)
}

// viewerParty is who a personal question answers about when the caller named
// nobody, the authority check when they named somebody, and BOTH IDENTITIES
// that person's rows may carry.
//
// # The scope rule, in one place because four questions share it
//
// A caller reads the seat their own token is bound to, and naming anybody
// else's handle requires an operator credential. Registering these
// operator-only instead — which is what `work_my_work` did — makes the landing
// screen the most-gated screen in the product and the human teammate, who is
// one of the two readers this dashboard is for, fictional.
//
// # And the party, which is what the reader needs rather than a handle
//
// A write made through somebody's own credential is attributed to the TOKEN,
// not to their seat, and deliberately so: a tracker whose author field is
// chosen by the writer is not an audit trail (see internal/api/operator). The
// consequence is that one person's rows carry two names — `jane-founder` on
// what a colleague assigned them, `founder` on everything their own assistant
// filed — so a personal read asked about one of them answered nothing. A
// founder who was reporter and watcher on eleven items opened My work and
// found seven empty tabs.
//
// # The party belongs to the person ASKED ABOUT, not to the caller
//
// An operator reading a report's day gets that report's own alias, resolved
// from the chart, rather than the credential in their own hand: whose two
// names these are is a fact about the seat, and the caller's token has nothing
// to do with it. That it is the caller's own id in the ordinary case falls out
// of the same lookup rather than being a second rule.
//
// Returns the party to read and an error to refuse with.
func (s Sources) viewerParty(ctx context.Context, asked string) (tracker.Party, error) {
	operatorID := operatorFrom(ctx)
	own := ""
	if seat := s.seatForOperator(operatorID); seat != nil {
		own = seat.Handle()
	}
	switch {
	case asked == "":
		if own == "" {
			// NOT AN AUTHORIZATION FAILURE. Nobody was refused: there
			// is no person to answer about, and the remedy is a line
			// of company configuration rather than a different
			// credential.
			return tracker.Party{}, errNoSeat
		}
		return s.partyOf(own), nil
	case asked == own:
		return s.partyOf(asked), nil
	case operatorID == "":
		return tracker.Party{}, errNotYours
	}
	// An operator reads anybody's: they hold the credential that writes
	// these records through the operator tool server in the first place.
	return s.partyOf(asked), nil
}

// partyOf is one seat and the credential bound to it.
//
// NIL LOOKUP, so a `${VAR}` binding resolves against this process's own
// environment — which is where every other consumer of `contact` resolves one,
// including [Sources.seatForOperator] on the way in. The two directions go
// through the same resolution in `org`, so a company cannot be bound for one
// and unbound for the other.
//
// A SEAT THAT IS NOT IN THE CHART IS STILL A PARTY. An operator naming a
// handle that no seat holds — a person who has left, a handle on old rows —
// gets that handle alone rather than a refusal: the rows are what the question
// is about, and this lookup only ever ADDS an alias.
func (s Sources) partyOf(handle string) tracker.Party {
	party := tracker.Party{Handle: handle}
	organization := s.organization()
	if organization == nil {
		return party
	}
	party.OperatorID = organization.SeatByHandle(handle).ResolvedOperatorID(nil)
	return party
}

// viewerHandle is [Sources.viewerParty] for a question that answers about a
// seat and reads no tracker row — the conversation ledger, whose rows are
// written by the SEAT's own turns and carry no credential's name.
func (s Sources) viewerHandle(ctx context.Context, asked string) (string, error) {
	party, err := s.viewerParty(ctx, asked)
	if err != nil {
		return "", err
	}
	return party.Handle, nil
}

// viewerPins is the authority rule over the party a view STRIP is
// personalised by.
//
// The same check as [Sources.viewerParty] over a different ABSENCE, which is
// why the named case delegates rather than restating it. A personal question
// is ABOUT somebody, so naming nobody with no seat to fall back on has no
// answer and [errNoSeat] says so. A view strip is about a CONTAINER and the
// viewer only decides whose pins order it, so naming nobody is the SHARED
// strip — a real answer, the documented meaning of an unnamed
// [tracker.ViewQuery.Viewer], and the one an anonymous or unbound caller must
// keep getting, because the sidebar and the board ask for exactly that.
//
// What is identical is the half that matters. `viewer` selected whose record
// was read and nothing checked it, so a reader could walk the org chart and
// page through every seat's pinned views by handle — the personal record
// `work_person` is scoped for, on a surface `api.allow_anonymous_read` opens.
// A scope rule three of the four personal questions follow is not a rule.
//
// AND IT CARRIES BOTH NAMES, because a saved view and a pin are both written
// through the person's own credential: owned by the token's id, asked for
// under the seat's, so a founder's own strip came back with neither.
func (s Sources) viewerPins(ctx context.Context, asked string) (tracker.Party, error) {
	if asked == "" {
		return tracker.Party{}, nil
	}
	return s.viewerParty(ctx, asked)
}
