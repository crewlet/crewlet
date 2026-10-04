package iamapi

import (
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
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

// SeatRow is one human seat of the company and who holds it.
type SeatRow struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	// Unit is the key of the unit the seat sits in, empty at the root.
	Unit string `json:"unit,omitempty"`
	// Holders is everybody the directory binds to the seat, at any stage
	// short of removal — normally one, and empty for a seat nobody holds.
	// More than one is a duplicate a restore left behind, which
	// `GET /iam/check` names.
	Holders []SeatHolding `json:"holders"`
}

// SeatHolding is one person bound to a seat.
type SeatHolding struct {
	Person string    `json:"person"`
	Login  string    `json:"login,omitempty"`
	Stage  iam.Stage `json:"stage,omitempty"`
}

// GetSeats is `GET /iam/seats`: every human seat of the running company and
// who holds it, and with `?unheld=true` only the seats nobody holds.
//
// # A listing, not a report
//
// It answers who sits where, which is what assigning a person to a seat needs.
// What is WRONG with a binding — a seat gone, a duplicate — is
// `GET /iam/check`'s, the one place a problem is reported.
//
// # Held is any stage short of removal
//
// A suspended person still holds their seat, and so does somebody invited to
// it or an enrolment that has reserved it — the same rule a company write is
// held to before it removes a seat. "Unheld" is a seat with nobody at all.
//
// # An unreadable directory is 503, never a list of vacancies
//
// The filter is never applied to what could not be read: answered as "nobody
// holds anything", `?unheld=true` would list every seat in the company under a
// parameter that promised the vacancies.
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
			map[string]string{"hint": "this node runs no company yet, so it has " +
				"no seats to list; import one, or ask a node that has applied it"})
		return
	}
	bindings, err := s.directory.SeatBindings(r.Context())
	if err != nil {
		s.unavailable(w, r, "read the seat bindings", err)
		return
	}
	held := map[string][]SeatHolding{}
	for _, b := range bindings {
		held[b.Seat] = append(held[b.Seat], SeatHolding{
			Person: b.Person, Login: b.Login, Stage: b.Stage,
		})
	}
	rows := []SeatRow{}
	for _, seat := range seats {
		holders := held[seat.Handle]
		if unheld && len(holders) > 0 {
			continue
		}
		if holders == nil {
			holders = []SeatHolding{}
		}
		rows = append(rows, SeatRow{
			Handle: seat.Handle, Name: seat.Name, Unit: seat.Unit, Holders: holders,
		})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"seats": rows})
}
