package configapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A RAISE IS A PERSON'S, ON EVERY PATH THAT WRITES IT (D10).
//
// An agent seat's access to a vendor lives in the company document, so every
// authored write of it can hand an agent an administration role at the vendor:
// the Agents tab's save, and also `PATCH /config`, `PUT /config`, a seat's
// `PUT /config/roles/{id}` and a revert. Each is held to the same rule, the
// one [seataccess.AsksStepUp] states: where the write raises a seat's lane to
// an administration grant, or the vendor's offer cannot say it does not, the
// writer is asked [authz.ActionSetupAccessRaise] on top of the write's own
// verb, which refuses a machine token and asks a person's fresh step-up.
//
// JUDGED FROM THE TWO DOCUMENTS, the revision replaced and the one proposed,
// as admission is, so a write that resends a seat's setting unchanged is never
// a raise and one that adds a seat already at an administration role is.
//
// NOT ASKED of the engine's own writes, which carry no principal (the Agents
// tab's save asks it itself, before it writes), of a check, which writes
// nothing and answers what a save would do in its plan, or of a document the
// operator writes (Enforce mode): there the agent's access is set on its Seat
// object, whose own authority decides it.

// Raises is which agent seats a write of the company document from prior to
// next raises to an administration grant ([seataccess.RaisesBetween]).
// priorRevision is prior's stored revision, empty where there is none. The
// engine's implementation reads each vendor's offer; it fails closed.
type Raises func(ctx context.Context, prior *config.Company, priorRevision string,
	next *config.Company) []seataccess.Raise

// failClosedRaises is [Raises] with no vendor asked: every lane that grants by
// level is unread, so a turn-on or a change of level on a lane the engine
// provisions is a raise. What a surface built without the engine's offers
// judges by.
func failClosedRaises(managed func() bool) Raises {
	return func(_ context.Context, prior *config.Company, _ string,
		next *config.Company) []seataccess.Raise {

		return seataccess.RaisesBetween(prior, next, managed(), func(kind integration.Kind) map[seataccess.Lane]seataccess.Options {
			return seataccess.UnreadOffers(kind,
				next.SeatCredentialMode(string(kind), managed()) != config.SeatCredentialEngine)
		})
	}
}

// RaiseError reports an authored write that raises an agent seat to an
// administration grant, which its caller may not make as they asked: a machine
// token, a proof older than the step-up window, or an assistant's read grant.
type RaiseError struct {
	Decision authz.Decision
	Raises   []seataccess.Raise
}

func (e *RaiseError) Error() string {
	parts := make([]string, 0, len(e.Raises))
	for _, r := range e.Raises {
		parts = append(parts, r.Handle+" on "+string(r.Lane))
	}
	return "configapi: this change raises an agent to an administration role (" +
		strings.Join(parts, ", ") + "): " + string(e.Decision.Reason)
}

// admitRaises refuses next when it raises an agent seat and p may not raise one.
func (s *Service) admitRaises(ctx context.Context, p iam.Principal, b base,
	next *config.Company) error {

	revision := ""
	if b.found {
		revision = b.revision.ID
	}
	raises := s.raises(ctx, b.prior, revision, next)
	if len(raises) == 0 {
		return nil
	}
	d := authz.Decide(ctx, p, authz.ActionSetupAccessRaise,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, time.Now())
	if d.Allowed {
		return nil
	}
	return &RaiseError{Decision: d, Raises: raises}
}

// refuseRaise answers a write refused for its raise: the authority table's own
// refusal for the raise's verb, naming each seat's lane it raises.
func refuseRaise(w http.ResponseWriter, err *RaiseError) {
	d := err.Decision
	if d.Unknown() {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds)
		return
	}
	status, code, detail := authz.RefusalFields(d)
	if detail == nil {
		detail = httpjson.Detail{}
	}
	detail["raises"] = err.Raises
	detail["hint"] = "this change gives an agent an administration role at a vendor, " +
		"which a person decides at a fresh step-up; confirm who you are and send it " +
		"again, or leave the agent at a lower level"
	httpjson.FailWithFields(w, status, code, detail)
}
