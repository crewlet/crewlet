package configapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A PERSON'S OWN AVAILABILITY — the one change to the company document a
// member key may make.
//
// Everything else on this surface is ADMIN (ADR-0031): the document is how
// the engine is run. A seat's `availability` is not. It is a line a person
// writes about themselves — "CET mornings; replies within ~4h" — rendered
// into every roster a colleague reads before they wait on them, and the one
// person who knows it was the one who could not change it without asking an
// admin to edit the company for them. So `PUT` and `PATCH
// /config/roles/{handle}` are mounted at member reach, and a caller below
// admin is held, IN THE WRITE, to two rules:
//
//   - THEIR OWN SEAT: the one whose `contact.crewlet_operator_id` names their
//     key, in the revision the write is built on. Judged in the draft rather
//     than before it, so an admin unlinking the key between the request and
//     the commit cannot be raced: the write lands on the revision it was
//     judged against or is refused as raced.
//   - ITS AVAILABILITY ALONE: the seat the write leaves is the seat it found
//     with only `availability` moved. Judged on the RESULT rather than on the
//     request's keys, so a PUT and a PATCH answer to one rule, and a body
//     restating a field exactly as it stands changes nothing and is admitted.
//
// Contact ids, the email and `crewlet_operator_id` itself stay the admin's:
// they are how the engine attributes a person and reaches them, and a key
// that could rewrite its own binding could bind itself to somebody else's
// seat.

// ErrNotYourSeat reports a member's write to a seat their key is not linked
// to — or from a key no seat links at all.
var ErrNotYourSeat = errors.New("configapi: a member key changes only the seat it is linked to")

// ErrBeyondAvailability reports a member's write that changes more of their
// seat than its availability.
var ErrBeyondAvailability = errors.New("configapi: a member key changes only its seat's availability")

// selfService is the rule a write by a caller below admin answers to.
type selfService struct {
	// operatorID is the caller's key.
	operatorID string
	// env resolves a `${VAR}` a seat's binding names, through this node's
	// own chain — the one its guard binds a key to a person through.
	env org.EnvLookup
}

// selfServiceOf is the rule a request's write answers to: nil for an admin,
// whose write is the document's to judge and nobody's else.
func (s *Service) selfServiceOf(r *http.Request) *selfService {
	caller := auth.PrincipalFrom(r.Context())
	if caller.IsAdmin() {
		return nil
	}
	return &selfService{operatorID: caller.ID, env: s.env}
}

// owns reports whether the seat at id is the one the caller's key is linked
// to in company, as [ErrNotYourSeat] when it is not.
func (rule *selfService) owns(company *config.Company, kind, id string) error {
	if kind != EntityRoles {
		return fmt.Errorf("%w: a member key changes nothing in %s", ErrNotYourSeat, kind)
	}
	organization, err := company.Organization()
	if err != nil {
		return fmt.Errorf("configapi: read the chart the write is built on: %w", err)
	}
	seat := organization.SeatByOperatorID(rule.operatorID, rule.env)
	switch {
	case seat == nil:
		return fmt.Errorf("%w: no seat names the key %q in contact.crewlet_operator_id",
			ErrNotYourSeat, rule.operatorID)
	case seat.Handle() != id:
		return fmt.Errorf("%w: the key %q is linked to %q, and this write is to %q",
			ErrNotYourSeat, rule.operatorID, seat.Handle(), id)
	}
	return nil
}

// onlyAvailability reports whether a seat write moved nothing but the seat's
// availability, as [ErrBeyondAvailability] when it moved more.
//
// before is the seat as the write found it, ENCODED; after is the seat the
// write leaves. The found seat is READ AGAIN through the reader the write's
// body went through ([decodeEntity]) before the two are compared, so a field
// the reader spells differently from the stored struct — a derived default,
// a mapping collapsed — is not a change the caller made.
func onlyAvailability(before []byte, after any) error {
	now, ok := after.(*config.Role)
	if !ok {
		return fmt.Errorf("%w: the write left no seat", ErrBeyondAvailability)
	}
	was, err := decodeEntity[config.Role](asText(before), nil)
	if err != nil {
		return fmt.Errorf("configapi: read the seat the write found: %w", err)
	}
	moved := *now
	was.Availability, moved.Availability = "", ""
	a, err := json.Marshal(was)
	if err != nil {
		return fmt.Errorf("configapi: encode the seat the write found: %w", err)
	}
	b, err := json.Marshal(moved)
	if err != nil {
		return fmt.Errorf("configapi: encode the seat the write leaves: %w", err)
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("%w: this write changes more of %q than `availability`",
			ErrBeyondAvailability, now.IdentityKey())
	}
	return nil
}

// refuseSelfService answers a member's write the rule refused, and reports
// whether it did.
//
// 403 `forbidden`: the key was accepted and the request is well formed, and
// no retry with this key changes the answer. The hint says who can.
func refuseSelfService(w http.ResponseWriter, err error) bool {
	var hint string
	switch {
	case errors.Is(err, ErrNotYourSeat):
		hint = "a member key changes the availability of its own seat — the one " +
			"whose contact.crewlet_operator_id names it — and nothing else here; " +
			"an admin changes anybody else's"
	case errors.Is(err, ErrBeyondAvailability):
		hint = "send only `availability` — PATCH /config/roles/{handle} with " +
			"{\"availability\": \"…\"}. Contact ids, the email, the operator " +
			"binding and everything else on a seat are an admin's to change"
	default:
		return false
	}
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeForbidden, map[string]any{
		"detail": err.Error(), "hint": hint,
	})
	return true
}
