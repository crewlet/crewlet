package configapi

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
)

// THE SEAT-HOLDER CHECK: a write may not take a human seat out of the company
// while anybody in the identity directory holds it — a person, a service
// account bound to it, or an open invitation onto it.
//
// # Why at the write
//
// A person's seat binding lives in the identity directory and the seat in the
// company document, and the two are written by different gestures. A revision
// that removes a seat a colleague holds — or turns it into an agent seat —
// leaves that person bound to nothing: every request they make is refused
// `403 seat_unavailable`, and `/iam/check` names it once somebody asks. That is
// the ordinary mistake, removing a seat somebody still uses, and the write is
// where it can carry that person's name rather than surface as a finding
// after the fact.
//
// # Any stage short of removal, and an invitation on its way
//
// A suspended person still holds their seat — suspending somebody is not
// giving their seat away. A removal deletes the person's row, so a seat whose
// holder was removed is free.
//
// AN OPEN INVITATION HOLDS ITS SEAT exactly as it holds its address (ADR-0026):
// every person holds a human seat for as long as they exist, so every
// invitation names the one its redemption binds, and nothing else may take
// it while the link is outstanding. Taken out of the company under it, the
// link would be refused at its redemption with nothing to tell its holder why,
// and the administrator who sent it would learn of it only from them. An
// invitation redeemed, cancelled or past its expiry holds nothing, judged at
// this surface's own clock ([Service.now]) — and where a build before
// invitations held their seats left a person and an invitation on one seat, the
// PERSON is named, since the invitation's redemption would be refused for the
// seat they hold.
//
// # Advisory, and what it cannot see
//
// The directory is read before the activation, so a bind or an invitation
// landing between the two is not seen; and an offline `crewlet config import`
// or `activate` writes the store with no directory to ask. Both leave the
// residue `/iam/check` reports as `binding_dangling` — or an invitation whose
// redemption is refused — which one record repairs.
//
// # An unreadable directory refuses, and only a write that needs it
//
// A write that takes no human seat away never asks. One that does and cannot
// read the directory is refused 503 rather than allowed: a removal may not
// proceed on evidence this node does not have, and an outage read as "nobody
// holds it" is exactly the answer the write would act on.

// SeatHolder is the ONE thing holding a human seat, as a refusal names it: a
// person or a service account bound to it, or an open invitation onto it.
//
// NEVER THE INVITATION'S ADDRESS: a `/config` writer may be a unit's lead who
// holds no grant over the identity directory, and what they need in order to
// act is that the seat is spoken for and by which invitation — cancelling it
// is somebody holding `people:manage`'s gesture, by its id.
type SeatHolder struct {
	// Person is the holder's id, which the route that moves, unbinds or
	// removes them takes. Empty where an invitation holds the seat.
	Person string `json:"person,omitempty"`
	// Kind is a person or a service account, which decides the remedy: a
	// person holds a human seat for as long as they exist, so they are
	// MOVED to another one or removed, and only a service account is
	// unbound.
	Kind iam.Kind `json:"kind,omitempty"`
	// Login is the name they sign in with.
	Login string `json:"login,omitempty"`
	// Stage is how far through enrolment they are.
	Stage iam.Stage `json:"stage,omitempty"`

	// Invitation is the id of the open invitation holding the seat, which
	// `DELETE /iam/invitations/{id}` cancels — set only where nobody is
	// bound to it.
	Invitation string `json:"invitation,omitempty"`
	// ExpiresAt is when that invitation lapses and frees the seat on its
	// own.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// held names the holder as a refusal's sentence does.
func (h SeatHolder) held() string {
	switch {
	case h.Invitation != "":
		return "open invitation " + h.Invitation
	case h.Kind == iam.KindMachine:
		return "service account " + cmp.Or(h.Login, h.Person)
	}
	return cmp.Or(h.Login, h.Person)
}

// Holders answers what holds each of seats at now, keyed by the seat's handle
// — one holder per seat ([SeatHolder]): whoever the identity directory binds
// to it at any stage short of removal, or else an invitation open at now. A
// seat nothing holds is absent. An error is UNKNOWN, never "nobody".
//
// A FUNCTION RATHER THAN THE DIRECTORY, because this surface needs one
// question of it, and the directory's own types are the identity estate's.
// NOW IS THE CALLER'S, because whether an invitation is still open is a
// question about an instant and the directory keeps no clock: the surface
// passes its own ([Service.now]), so a case pinning it pins the answer.
type Holders func(ctx context.Context, seats []string, now time.Time) (
	map[string]SeatHolder, error)

// DirectoryHolders is [Holders] over this node's identity directory, or nil
// when there is none — which [New] refuses by name rather than serving a
// surface that removes a held seat with nothing said.
//
// ONE SNAPSHOT of the bindings and the open invitations
// ([iamdomain.Reader.SeatClaims]), because a redemption spends its invitation
// and binds its person in one record: read apart, one landing between the two
// reads showed its seat as held by neither, and the write it was asked about
// took the seat from the person who had just arrived.
func DirectoryHolders(directory *iamdomain.Reader) Holders {
	if directory == nil {
		return nil
	}
	return claimHolders(directory.SeatClaims)
}

// claimHolders is [Holders] over any one-snapshot read of the seat claims —
// the directory's, or a case's — folded by [holdersOf].
func claimHolders(read func(context.Context, time.Time) (iamdomain.SeatClaims,
	error)) Holders {

	return func(ctx context.Context, seats []string, now time.Time) (
		map[string]SeatHolder, error) {

		claims, err := read(ctx, now)
		if err != nil {
			return nil, err
		}
		return holdersOf(claims, seats)
	}
}

// holdersOf folds one snapshot of the seat claims to what holds each of seats.
//
// IT ANSWERS ABOUT THE SEATS ASKED AND NO OTHERS: the claims are narrowed to
// them BEFORE they are folded. The fold is the directory's
// ([iamdomain.ClaimsBySeat]), so two rows bound to one seat are the unknown arm
// — but only for a seat this write takes away. Folded whole, a residue on any
// seat in the company refused every write that removed a human seat, the
// seats nobody holds included, for a duplicate the write never touched.
//
// Two open invitations on one seat are the newest. A PERSON WINS where a
// person and an invitation both claim a seat — a residue of the build before
// invitations held their seats — because what the write takes from is the
// person; the invitation's redemption is refused for the seat they hold either
// way.
func holdersOf(claims iamdomain.SeatClaims, seats []string) (
	map[string]SeatHolder, error) {

	asked := make(map[string]bool, len(seats))
	for _, seat := range seats {
		asked[seat] = true
	}
	narrowed := iamdomain.SeatClaims{}
	for _, b := range claims.Bindings {
		if asked[b.Seat] {
			narrowed.Bindings = append(narrowed.Bindings, b)
		}
	}
	for _, inv := range claims.Invitations {
		if asked[inv.Seat] {
			narrowed.Invitations = append(narrowed.Invitations, inv)
		}
	}
	bySeat, err := iamdomain.ClaimsBySeat(narrowed)
	if err != nil {
		return nil, err
	}
	out := make(map[string]SeatHolder, len(seats))
	for _, seat := range seats {
		claim, ok := bySeat[seat]
		switch {
		case !ok:
		case claim.Holder != nil:
			b := claim.Holder
			out[seat] = SeatHolder{Person: b.Person, Kind: b.Kind,
				Login: b.Login, Stage: b.Stage}
		case claim.Invitation != nil:
			out[seat] = SeatHolder{Invitation: claim.Invitation.Invitation,
				ExpiresAt: claim.Invitation.ExpiresAt}
		}
	}
	return out, nil
}

// SeatHeldError reports a write that would take a human seat out of the
// company while something holds it. Held names every such seat and what holds
// it.
type SeatHeldError struct {
	Held map[string]SeatHolder
}

func (e *SeatHeldError) Error() string {
	seats := make([]string, 0, len(e.Held))
	for seat, h := range e.Held {
		seats = append(seats, seat+" (held by "+h.held()+")")
	}
	slices.Sort(seats)
	return "configapi: this write removes a human seat that is still held: " +
		strings.Join(seats, "; ")
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
// anything holds it — see the file's header.
func (s *Service) checkHeldSeats(ctx context.Context, prior, next *config.Company) error {
	leaving := leavingHumanSeats(prior, next)
	if len(leaving) == 0 {
		return nil
	}
	held, err := s.holders(ctx, leaving, s.now())
	if err != nil {
		return &HoldersUnavailableError{Seats: leaving, Err: err}
	}
	refused := map[string]SeatHolder{}
	for _, seat := range leaving {
		if h, ok := held[seat]; ok {
			refused[seat] = h
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
