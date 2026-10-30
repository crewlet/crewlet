package configapi

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
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
// AN OPEN INVITATION HOLDS ITS SEAT exactly as it holds its address (ADR-0032):
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
// A write that takes no human seat away never asks, except a whole document
// replacing an unsealed revision, which can't tell what it takes away (below).
// One that does and cannot
// read the directory is refused 503 rather than allowed: a removal may not
// proceed on evidence this node does not have, and an outage read as "nobody
// holds it" is exactly the answer the write would act on.
//
// # A document replacing what nobody can read asks about every seat
//
// The seats a write takes away are the human seats of the revision it
// replaces that its own document does not keep. A whole document replacing a
// revision stored without a seal reads nothing from it (apply.go), so it has
// no such list: it asks instead about EVERY seat its document does not keep
// as a human seat, and only the directory knows which of those anybody holds.
// So it always asks, and a held seat it drops is refused exactly as a removal
// is. Judged against nothing, the check would answer "nothing leaves" and the
// replacement would take every seat it dropped from whoever holds it.

// SeatHolder is the ONE thing holding a human seat, as a refusal names it: a
// person or a service account bound to it, or an open invitation onto it.
//
// NEVER THE INVITATION'S ADDRESS: a `/config` writer may be a unit's lead who
// is no admin and reads nobody's directory row, and what they need in order to
// act is that the seat is spoken for and by which invitation — cancelling it
// is an admin's gesture, by its id.
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
		return "an open invitation (" + h.Invitation + ")"
	case h.Kind == iam.KindMachine:
		return "service account " + cmp.Or(h.Login, h.Person)
	}
	return cmp.Or(h.Login, h.Person)
}

// Holders answers what holds each seat the asked predicate admits at now,
// keyed by the seat's handle — one holder per seat ([SeatHolder]): whoever the
// identity directory binds to it at any stage short of removal, or else an
// invitation open at now. A seat nothing holds, and a seat the predicate does
// not admit, is absent. An error is UNKNOWN, never "nobody".
//
// A FUNCTION RATHER THAN THE DIRECTORY, because this surface needs one
// question of it, and the directory's own types are the identity estate's.
// THE SEATS ASKED ARE A PREDICATE rather than a list, because one write asks
// about seats it cannot list: a whole document replacing a revision stored
// without a seal asks about every seat its own document does not keep as a
// human seat, and only the directory knows which of those anybody holds (see
// this file's header).
// NOW IS THE CALLER'S, because whether an invitation is still open is a
// question about an instant and the directory keeps no clock: the surface
// passes its own ([Service.now]), so a case pinning it pins the answer.
type Holders func(ctx context.Context, asked func(seat string) bool, now time.Time) (
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

	return func(ctx context.Context, asked func(string) bool, now time.Time) (
		map[string]SeatHolder, error) {

		claims, err := read(ctx, now)
		if err != nil {
			return nil, err
		}
		return holdersOf(claims, asked)
	}
}

// holdersOf folds one snapshot of the seat claims to what holds each seat
// asked admits.
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
func holdersOf(claims iamdomain.SeatClaims, asked func(string) bool) (
	map[string]SeatHolder, error) {

	narrowed := iamdomain.SeatClaims{}
	for _, b := range claims.Bindings {
		if asked(b.Seat) {
			narrowed.Bindings = append(narrowed.Bindings, b)
		}
	}
	for _, inv := range claims.Invitations {
		if asked(inv.Seat) {
			narrowed.Invitations = append(narrowed.Invitations, inv)
		}
	}
	bySeat, err := iamdomain.ClaimsBySeat(narrowed)
	if err != nil {
		return nil, err
	}
	out := make(map[string]SeatHolder, len(bySeat))
	for seat, claim := range bySeat {
		switch {
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
	// Replacing is a whole document replacing a revision nothing was read
	// from ([base.unsealed]). It cannot say the seats were in the revision it
	// replaces, only that its own document does not keep them, a seat no
	// revision of this company names any more included; and keeping a seat
	// in the document is the one remedy that needs no company running here,
	// where moving a person to another human seat does.
	Replacing bool
}

func (e *SeatHeldError) Error() string {
	seats := make([]words.Said, 0, len(e.Held))
	for seat, h := range e.Held {
		seats = append(seats, words.Said(seat+" (held by "+h.held()+")"))
	}
	slices.Sort(seats)
	if e.Replacing {
		return "this document doesn't keep these human seats, which somebody " +
			"holds: " + words.And(seats)
	}
	return "this change removes human seats that somebody still holds: " +
		words.And(seats)
}

// HoldersUnavailableError reports a write that takes a human seat away, or a
// whole document replacing an unsealed revision, on a node that could not
// read who holds the seats it asked about.
type HoldersUnavailableError struct {
	// Seats are the human seats the write takes away, by handle, sorted;
	// nil for a write Replacing a revision nothing was read from.
	Seats []string
	// Replacing is a whole document replacing a revision nothing was read
	// from ([base.unsealed]), which cannot list the seats it takes away and
	// asked about every seat its document does not keep (see this file's
	// header).
	Replacing bool
	Err       error
}

func (e *HoldersUnavailableError) Error() string {
	if e.Replacing {
		return fmt.Sprintf("configapi: this change replaces a revision this node "+
			"can't read, so it asks who holds every seat its document doesn't keep "+
			"as a human seat, and this node can't read that right now: %v", e.Err)
	}
	return fmt.Sprintf("configapi: this change removes the human seats %s, "+
		"and this node can't check who holds them right now: %v",
		words.And(e.Seats), e.Err)
}

func (e *HoldersUnavailableError) Unwrap() error { return e.Err }

// checkHeldSeats refuses next when it takes a human seat of the revision it
// replaces away while anything holds it — see the file's header. Over a base
// nothing was read from ([base.unsealed]) every seat next does not keep as a
// human seat is asked about, since which of them the replaced revision held is
// what nobody can read.
func (s *Service) checkHeldSeats(ctx context.Context, b base, next *config.Company) error {
	keeps := humanSeats(next)
	asked := func(seat string) bool { return !keeps[seat] }
	var leaving []string
	if !b.unsealed {
		leaving = leavingHumanSeats(b.prior, keeps)
		if len(leaving) == 0 {
			return nil
		}
		asked = func(seat string) bool {
			_, found := slices.BinarySearch(leaving, seat)
			return found
		}
	}
	held, err := s.holders(ctx, asked, s.now())
	if err != nil {
		return &HoldersUnavailableError{Seats: leaving, Replacing: b.unsealed, Err: err}
	}
	refused := map[string]SeatHolder{}
	for seat, h := range held {
		// ONLY WHAT WAS ASKED, whatever the directory answered: a seat
		// next keeps is no seat this write takes from anybody.
		if asked(seat) {
			refused[seat] = h
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &SeatHeldError{Held: refused, Replacing: b.unsealed}
}

// humanSeats is every handle next holds as a human seat.
//
// BY HANDLE, because a seat's handle is its identity and a binding names it:
// a seat renamed for display keeps its handle and is not leaving, and a
// handle that disappears is a seat that did.
func humanSeats(next *config.Company) map[string]bool {
	keeps := map[string]bool{}
	if next == nil {
		return keeps
	}
	for role := range next.EachRole() {
		if role.Kind == org.KindHuman {
			keeps[role.IdentityKey()] = true
		}
	}
	return keeps
}

// leavingHumanSeats is every human seat of prior that keeps does not hold —
// removed, or turned into an agent seat — by handle, sorted.
func leavingHumanSeats(prior *config.Company, keeps map[string]bool) []string {
	if prior == nil {
		return nil
	}
	var leaving []string
	for role := range prior.EachRole() {
		if handle := role.IdentityKey(); role.Kind == org.KindHuman && !keeps[handle] {
			leaving = append(leaving, handle)
		}
	}
	slices.Sort(leaving)
	return slices.Compact(leaving)
}
