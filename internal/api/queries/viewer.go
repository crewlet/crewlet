// Who the caller is, which is the question every personal surface rests on.

package queries

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/org"
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
	}
	seat := s.seatForOperator(operatorID)
	if seat == nil {
		return out, nil
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
	if operatorID == "" || s.Company == nil {
		return nil
	}
	company := s.Company()
	if company == nil {
		return nil
	}
	organization, err := company.Organization()
	if err != nil {
		return nil
	}
	// NIL LOOKUP, so the reference resolves against this process's own
	// environment — which is where Tier B's `${VAR}` pointers are resolved
	// everywhere else in the engine.
	return organization.SeatByOperatorID(operatorID, nil)
}

// viewerHandle is the handle a personal question answers for when the caller
// named none, and the authority check when they named one.
//
// THE SCOPE RULE, in one place because three questions share it: a caller
// reads the seat their own token is bound to, and naming anybody else's handle
// requires an operator credential. Registering these operator-only instead —
// which is what `work_my_work` did — makes the landing screen the most-gated
// screen in the product and the human teammate, who is one of the two readers
// this dashboard is for, fictional.
//
// Returns the handle to read and an error to refuse with.
func (s Sources) viewerHandle(ctx context.Context, asked string) (string, error) {
	operatorID := operatorFrom(ctx)
	own := ""
	if seat := s.seatForOperator(operatorID); seat != nil {
		own = seat.Handle()
	}
	if asked == "" {
		if own == "" {
			// NOT AN AUTHORIZATION FAILURE. Nobody was refused: there is
			// no person to answer about, and the remedy is a line of
			// company configuration rather than a different credential.
			return "", errNoSeat
		}
		return own, nil
	}
	if asked == own {
		return asked, nil
	}
	if operatorID == "" {
		return "", errNotYours
	}
	// An operator reads anybody's: they hold the credential that writes
	// these records through the operator tool server in the first place.
	return asked, nil
}

// viewerPins is the authority rule over the viewer a view STRIP is
// personalised by.
//
// The same check as [Sources.viewerHandle] over a different ABSENCE, which is
// why the named case delegates rather than restating it. A personal question
// is ABOUT somebody, so naming nobody with no seat to fall back on has no
// answer and [errNoSeat] says so. A view strip is about a CONTAINER and the
// viewer only decides whose pins order it, so naming nobody is the SHARED
// strip — a real answer, the documented meaning of an empty
// [tracker.ViewQuery.Viewer], and the one an anonymous or unbound caller must
// keep getting, because the sidebar and the board ask for exactly that.
//
// What is identical is the half that matters. `viewer` selected whose record
// was read and nothing checked it, so a reader could walk the org chart and
// page through every seat's pinned views by handle — the personal record
// `work_person` is scoped for, on a surface `api.allow_anonymous_read` opens.
// A scope rule three of the four personal questions follow is not a rule.
func (s Sources) viewerPins(ctx context.Context, asked string) (string, error) {
	if asked == "" {
		return "", nil
	}
	return s.viewerHandle(ctx, asked)
}
