package iamapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// Seats is the company this node runs, as `GET /iam/seats` reads it.
//
// THE RUNNING COMPANY and nothing beside it: the seats a person can be bound
// to are the human seats of the configuration this node applied, which is
// what every request a bound person makes is resolved against.
type Seats interface {
	// HumanSeats is every human seat of the running company, and false on
	// a node that runs no company yet.
	HumanSeats() ([]session.Seat, bool)
}

// SeatRow is one human seat of the company and what holds it.
type SeatRow struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	// Unit is the key of the unit the seat sits in, empty at the root.
	Unit string `json:"unit,omitempty"`
	// Holder is the principal the directory binds to the seat — a person,
	// or a service account acting as it — at any stage short of removal,
	// and absent for a seat nobody holds.
	Holder *SeatHolding `json:"holder,omitempty"`
	// Invitation is the OPEN invitation that names the seat, and absent
	// where none does. It holds the seat as surely as a holder would.
	Invitation *SeatInvitation `json:"invitation,omitempty"`
}

// SeatHolding is one principal bound to a seat, as one reader may see it.
//
// THE ORG CHART IS EVERYBODY'S, and so is who sits in each of its human seats:
// a colleague is drawn by the name they go by. What identifies their ACCOUNT —
// the directory id, the login they sign in with, how far through enrolment
// they are — is the directory's, read by an admin and by the holder about
// themselves ([Service.GetSeats]); everybody else is sent the name and the
// kind and nothing a sign-in form would take.
type SeatHolding struct {
	// Person is the directory id: an admin's and the holder's own.
	Person string `json:"person,omitempty"`
	// Name is the name the holder goes by, opened on this node's keyring;
	// Sealed says it could not be, so a screen draws "name unavailable"
	// rather than a seat nobody holds.
	Name   string `json:"name,omitempty"`
	Sealed bool   `json:"sealed,omitempty"`
	// Kind says which remedy a seat that must be freed takes: a person is
	// moved to another human seat or removed, a service account unbound.
	Kind iam.Kind `json:"kind"`
	// Login and Stage are an admin's and the holder's own, as Person is.
	Login string    `json:"login,omitempty"`
	Stage iam.Stage `json:"stage,omitempty"`
}

// SeatInvitation is one open invitation as the seat it names reports it.
//
// NO LINK, NO SECRET, NO VERIFIER, for [invitationView]'s reason — the link was
// shown once, by the issue — and the address opened on this node's keyring
// exactly as the invitation listing opens it, `sealed` where it cannot be.
type SeatInvitation struct {
	ID        string    `json:"id"`
	Email     string    `json:"email,omitempty"`
	Sealed    bool      `json:"sealed,omitempty"`
	InvitedBy string    `json:"invited_by,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GetSeats is `GET /iam/seats`: every human seat of the running company and
// what holds it, and with `?unheld=true` only the seats nothing holds.
//
// # Everybody's, masked per reader
//
// The org chart is everybody's overview (ADR-0033), so the route admits every
// resolved principal (`iam.seats.read`) and each row is shaped here for whoever
// asked: a holder's NAME and kind to everybody; their directory id, login and
// stage to an admin and to the holder about themselves; an OPEN INVITATION,
// which carries the address it was sent to, to an admin alone; and
// `?unheld=true` — the helper a create, an invitation or a move picks a seat
// from, all of them admin's gestures — refused anybody else with the
// permission that would admit them.
//
// # A listing, not a report
//
// It answers who sits where, which is what creating or inviting a person onto
// a seat, and moving one, needs. What is WRONG with a binding — a seat gone —
// is `GET /iam/check`'s, the one place a problem is reported.
//
// # Held is a binding at any stage short of removal, or an open invitation
//
// A suspended person still holds their seat — suspending somebody is not
// giving it away — and an OPEN invitation (not redeemed, not aged out at this
// node's clock) holds the seat it names until it is redeemed, cancelled or
// lapses, the same rules a company write is held to before it removes a seat
// and an invitation or a create is held to before it takes one. "Unheld" is a
// seat with neither, which is exactly a seat a create, an invitation or a move
// may name. A seat a holder and an invitation both claim — only a build before
// invitations held their seats leaves that — lists both.
//
// # An unreadable directory is 503, never a list of vacancies
//
// The filter is never applied to what could not be read: answered as "nobody
// holds anything", `?unheld=true` would list every seat in the company under a
// parameter that promised the vacancies. A seat two rows bind — a record this
// node retained, or a restore — is the same answer, since this node cannot say
// which of them holds it ([iamdomain.ClaimsBySeat]).
func (s *Service) GetSeats(w http.ResponseWriter, r *http.Request) {
	unheld := false
	if raw := r.URL.Query().Get("unheld"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				map[string]string{"detail": "unheld must be true or false, " +
					"not " + strconv.Quote(raw) + "."})
			return
		}
		unheld = parsed
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	admin := principal.Can(iam.GrantAdmin)
	if unheld && !admin {
		// THE CREATE-INVITE-MOVE HELPER IS ADMIN'S, as all three gestures
		// are: a vacancy list is where an administrator puts somebody, and
		// told to anybody else it is a list of which seats an invitation
		// is out for. Answered as the authority table answers a missing
		// permission ([authz.RefusalFields]).
		status, code, detail := authz.RefusalFields(authz.Decision{
			Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantAdmin}})
		httpjson.FailWithFields(w, status, code, detail)
		return
	}
	seats, running := s.seats.HumanSeats()
	if !running {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{"hint": noCompanyHint})
		return
	}
	// ONE SNAPSHOT OF BOTH HALVES, judged open at this node's clock — see
	// [Directory.SeatClaims].
	claims, err := s.directory.SeatClaims(r.Context(), s.now())
	if err != nil {
		s.unavailable(w, r, "read the seat claims", err)
		return
	}
	held, err := iamdomain.ClaimsBySeat(claims)
	if err != nil {
		s.unavailable(w, r, "read the seat claims", err)
		return
	}
	rows := []SeatRow{}
	for _, seat := range seats {
		row := SeatRow{Handle: seat.Handle, Name: seat.Name, Unit: seat.Unit}
		claim := held[seat.Handle]
		if unheld && (claim.Holder != nil || claim.Invitation != nil) {
			continue
		}
		if b := claim.Holder; b != nil {
			row.Holder = s.holding(r, principal, admin, b)
		}
		if inv := claim.Invitation; inv != nil && admin {
			view := &SeatInvitation{ID: inv.Invitation, InvitedBy: inv.InvitedBy,
				ExpiresAt: inv.ExpiresAt}
			view.Email, view.Sealed = s.openInvitation(r, inv.Invitation, inv.Sealed)
			row.Invitation = view
		}
		rows = append(rows, row)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"seats": rows})
}

// holding is one seat's holder as reader p sees it: the name everybody, the
// account an admin and the holder themselves — see [SeatHolding].
func (s *Service) holding(r *http.Request, p iam.Principal, admin bool,
	b *iamdomain.SeatBinding) *SeatHolding {

	out := &SeatHolding{Kind: b.Kind}
	var opened bool
	out.Name, opened = s.open(r.Context(), b.Person, iamdomain.FieldName, b.NameSealed)
	out.Sealed = !opened
	if admin || p.ID.String() == b.Person {
		out.Person, out.Login, out.Stage = b.Person, b.Login, b.Stage
	}
	return out
}

// noCompanyHint is what a node running no company says about a seat — the
// listing's, and every gesture's that names one ([iamdomain.ErrNoCompany]):
// what clears it is a company declaring a human seat, which no wait supplies.
const noCompanyHint = "This node has no company yet. Import one with a human " +
	"seat, then add the person to it."
