package configapi

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
)

// THE SEAT-HOLDER CHECK: a write may not take a human seat out of the company
// while somebody in the identity directory is bound to it.
//
// # Why at the write
//
// A person's seat binding lives in the identity directory and the seat in the
// company document, and the two are written by different gestures. A revision
// that removes a seat a colleague is bound to — or turns it into an agent seat
// — leaves that person bound to nothing: every request they make is refused
// `403 seat_unavailable`, and the dangling-binding alarm names it a minute
// later. That is the ordinary mistake, removing a seat somebody still uses,
// and the write is where it can carry that person's name rather than surface
// as an alarm.
//
// # Any stage short of removal
//
// A suspended person still holds their seat — suspending somebody is not
// giving their seat away — and so do an invited one and a reservation an
// enrolment in flight has made. A removal deletes the person's row, so a seat
// whose holder was removed is free.
//
// # Advisory, and what it cannot see
//
// The directory is read before the activation, so a bind landing between the
// two is not seen; and an offline `crewlet config import` or `activate` writes
// the store with no directory to ask. Both leave the residue
// `iam_binding_dangling` reports, which one record repairs.
//
// # An unreadable directory refuses, and only a write that needs it
//
// A write that takes no human seat away never asks. One that does and cannot
// read the directory is refused 503 rather than allowed: a removal may not
// proceed on evidence this node does not have, and an outage read as "nobody
// holds it" is exactly the answer the write would act on.

// SeatHolder is a person the identity directory binds to a seat, as a refusal
// names them.
type SeatHolder struct {
	// Person is their id, which a route that unbinds or removes them takes.
	Person string `json:"person"`
	// Login is the name they sign in with; empty for a reservation an
	// enrolment has not finished.
	Login string `json:"login,omitempty"`
	// Stage is how far through enrolment they are; empty for a reservation.
	Stage iam.Stage `json:"stage,omitempty"`
}

// Holders answers who the identity directory binds to each of seats, at any
// stage short of removal, keyed by the seat's handle. A seat nobody holds is
// absent. An error is UNKNOWN, never "nobody".
//
// A FUNCTION RATHER THAN THE DIRECTORY, because this surface needs one
// question of it, and the directory's own types are the identity estate's.
type Holders func(ctx context.Context, seats []string) (map[string][]SeatHolder, error)

// DirectoryHolders is [Holders] over this node's identity directory, or nil
// when there is none — which [New] refuses by name rather than serving a
// surface that removes a held seat with nothing said.
func DirectoryHolders(directory *iamdomain.Reader) Holders {
	if directory == nil {
		return nil
	}
	return func(ctx context.Context, seats []string) (map[string][]SeatHolder, error) {
		held, err := directory.HoldersOf(ctx, seats)
		if err != nil {
			return nil, err
		}
		out := make(map[string][]SeatHolder, len(held))
		for seat, bindings := range held {
			for _, b := range bindings {
				out[seat] = append(out[seat], SeatHolder{
					Person: b.Person, Login: b.Login, Stage: b.Stage,
				})
			}
		}
		return out, nil
	}
}

// SeatHeldError reports a write that would take a human seat out of the
// company while somebody is bound to it. Held names every such seat and
// everybody who holds it.
type SeatHeldError struct {
	Held map[string][]SeatHolder
}

func (e *SeatHeldError) Error() string {
	seats := make([]string, 0, len(e.Held))
	for seat, holders := range e.Held {
		names := make([]string, 0, len(holders))
		for _, h := range holders {
			names = append(names, h.name())
		}
		seats = append(seats, seat+" (held by "+strings.Join(names, ", ")+")")
	}
	slices.Sort(seats)
	return "configapi: this write removes a human seat somebody is bound to: " +
		strings.Join(seats, "; ")
}

// name is how a refusal's sentence names a holder: their login, or their id
// where a reservation has none yet.
func (h SeatHolder) name() string {
	if h.Login != "" {
		return h.Login
	}
	return h.Person
}

// HoldersUnavailableError reports a write that takes a human seat away on a
// node that could not read who holds it.
type HoldersUnavailableError struct {
	Seats []string
	Err   error
}

func (e *HoldersUnavailableError) Error() string {
	return fmt.Sprintf("configapi: this write removes the human seat(s) %s, and "+
		"the identity directory could not be read to see whether anybody holds "+
		"them: %v", strings.Join(e.Seats, ", "), e.Err)
}

func (e *HoldersUnavailableError) Unwrap() error { return e.Err }

// checkHeldSeats refuses next when it takes a human seat of prior away while
// somebody holds it — see the file's header.
func (s *Service) checkHeldSeats(ctx context.Context, prior, next *config.Company) error {
	leaving := leavingHumanSeats(prior, next)
	if len(leaving) == 0 {
		return nil
	}
	held, err := s.holders(ctx, leaving)
	if err != nil {
		return &HoldersUnavailableError{Seats: leaving, Err: err}
	}
	refused := map[string][]SeatHolder{}
	for _, seat := range leaving {
		if holders := held[seat]; len(holders) > 0 {
			refused[seat] = holders
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &SeatHeldError{Held: refused}
}

// leavingHumanSeats is every human seat of prior that next does not hold as a
// human seat — removed, or turned into an agent seat — by handle, sorted.
//
// BY HANDLE, because a seat's handle is its identity and a binding names it:
// a seat renamed for display keeps its handle and is not leaving, and a
// handle that disappears is a seat that did.
func leavingHumanSeats(prior, next *config.Company) []string {
	if prior == nil {
		return nil
	}
	stays := map[string]bool{}
	if next != nil {
		for role := range next.EachRole() {
			if role.Kind == org.KindHuman {
				stays[role.IdentityKey()] = true
			}
		}
	}
	var leaving []string
	for role := range prior.EachRole() {
		if handle := role.IdentityKey(); role.Kind == org.KindHuman && !stays[handle] {
			leaving = append(leaving, handle)
		}
	}
	slices.Sort(leaving)
	return slices.Compact(leaving)
}
