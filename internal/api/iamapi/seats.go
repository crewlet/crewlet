package iamapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
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

// SeatHolding is one principal bound to a seat.
type SeatHolding struct {
	Person string `json:"person"`
	// Kind says which remedy a seat that must be freed takes: a person is
	// moved to another human seat or removed, a service account unbound.
	Kind  iam.Kind  `json:"kind"`
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
				map[string]string{"detail": "unheld is true or false, not " +
					strconv.Quote(raw)})
			return
		}
		unheld = parsed
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
			row.Holder = &SeatHolding{Person: b.Person, Kind: b.Kind,
				Login: b.Login, Stage: b.Stage}
		}
		if inv := claim.Invitation; inv != nil {
			view := &SeatInvitation{ID: inv.Invitation, InvitedBy: inv.InvitedBy,
				ExpiresAt: inv.ExpiresAt}
			view.Email, view.Sealed = s.openInvitation(r, inv.Invitation, inv.Sealed)
			row.Invitation = view
		}
		rows = append(rows, row)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"seats": rows})
}

// noCompanyHint is what a node running no company says about a seat — the
// listing's, and every gesture's that names one ([iamdomain.ErrNoCompany]):
// what clears it is a company declaring a human seat, which no wait supplies.
const noCompanyHint = "this node runs no company yet, so it has no human " +
	"seat to list or to put anybody on: import or create one that declares a " +
	"human seat, then invite or create the person onto it — or ask a node " +
	"that has applied it"
