package iamdomain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// cancel withdraws one invitation through the rig's writer and applies the
// result.
func cancel(t *testing.T, rig *writeRig, id string) (statelog.Result, error) {
	t.Helper()
	var result statelog.Result
	err := rig.draining(func() error {
		var err error
		result, err = rig.writer.CancelInvitation(t.Context(), id,
			operationKey(), "sent to the wrong address")
		return err
	})
	return result, err
}

// A CANCELLED INVITATION OPENS NOTHING, AND ITS ADDRESS AND ITS SEAT ARE FREE
// AT ONCE.
//
// The record deletes the row, so a link to it resolves exactly as an id nobody
// issued does — the zero row every refusal of a dead link is decided on — and
// the address and the seat it held are free for a new invitation under a new
// key the moment it lands. The cancellation is on the trail, filed under the
// address as the issue was.
//
// The CONTROL is the same two invitations with no cancellation, which the
// address and the seat each refuse as held by the first. Mutation: delete
// nothing in the apply and the second invitations are refused; keep the row
// and the link still opens.
func TestACancelledInvitationOpensNothingAndFreesItsAddressAndItsSeat(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{true, false} {
		rig := newWriteRig(t)
		first, err := inviteFor(t, rig, "priya@example.com",
			rig.vacantSeat("platform-lead"))
		if err != nil {
			t.Fatalf("invite: %v", err)
		}
		if cancelled {
			result, err := cancel(t, rig, first.ID)
			if err != nil || result.Outcome != statelog.OutcomeApplied {
				t.Fatalf("cancel: (%+v, %v)", result, err)
			}
			rig.drain()
			row, err := rig.invitationRow(t, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.ID != "" || row.Admits(first.Secret) {
				t.Errorf("the cancelled invitation still resolves: %+v", row)
			}
			trail := rig.column(`SELECT object_kind || ':' || object_id FROM
				iam_history WHERE op = 'cancel'`)
			if len(trail) != 1 || trail[0] != "email:"+blindOf(t, "priya@example.com") {
				t.Errorf("the cancellation's trail row is %v, want one filed "+
					"under the address", trail)
			}
		}
		for _, again := range []struct {
			what, address, seat string
			field               iamdomain.Unique
		}{
			{"its address, onto another seat", "priya@example.com",
				rig.vacantSeat("data-lead"), iamdomain.UniqueEmail},
			{"its seat, for another address", "dana@example.com",
				"platform-lead", iamdomain.UniqueSeat},
		} {
			_, err = inviteFor(t, rig, again.address, again.seat)
			var taken *iamdomain.ErrTaken
			switch {
			case cancelled && err != nil:
				t.Errorf("a new invitation to a cancelled one's %s was "+
					"refused: %v", again.what, err)
			case !cancelled && (!errors.As(err, &taken) ||
				taken.Field != again.field || taken.Invitation != first.ID):
				t.Errorf("with no cancellation an invitation to %s answered "+
					"%v, want it held by the first — the control", again.what, err)
			}
		}
	}
}

// AN INVITATION THAT AGED OUT HOLDS NOTHING, swept or not.
//
// Open is one predicate — not redeemed, not aged out — and the sweep that
// collects the row runs a week after its deadline, so between the two an
// expired invitation is a row that must hold neither its address nor its seat:
// held, an administrator who let a link lapse could not create its person on
// the seat for a week, for a link that opens nothing.
//
// The CONTROL is the same create while the invitation is still open, refused
// naming it.
func TestAnInvitationThatAgedOutHoldsNothing(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued := issueOnto(t, rig, "priya@example.com", seat, brokerAt.Add(time.Hour))
	create := func(w *iamdomain.Writer, op string, expires time.Time) error {
		return rig.during(func() error {
			_, err := w.Create(t.Context(), iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
				Stage: iam.StageActive, Name: "Priya Shah",
				Email: "priya@example.com", Login: "priya.shah", Seat: seat,
				LinkExpiresAt: expires, OpID: op, Reason: "a hire",
			})
			return err
		})
	}
	var taken *iamdomain.ErrTaken
	if err := create(rig.writer, "op-open", firstLinkExpiry); !errors.As(err, &taken) ||
		taken.Invitation != issued.ID {
		t.Fatalf("a create beside the open invitation answered %v, want it "+
			"refused naming %s — the control", err, issued.ID)
	}
	later := rig.writer.As(principalNamed("ana.admin", iam.KindPerson, iam.AllGrants))
	at := brokerAt.Add(2 * time.Hour)
	later.Now = func() time.Time { return at }
	if err := create(later, "op-lapsed", at.Add(time.Hour)); err != nil {
		t.Fatalf("a create onto the seat and the address of an invitation that "+
			"aged out was refused: %v", err)
	}
	if rows := rig.column(`SELECT id FROM iam_invites WHERE id = ?`,
		issued.ID); len(rows) != 1 {
		t.Errorf("the aged-out invitation's row is %v — the case is about a row "+
			"the sweep has not collected", rows)
	}
}

// A REDEEMED INVITATION IS NOT CANCELLED, AND NEITHER IS ONE NOBODY ISSUED.
//
// A redeemed one created somebody, and the refusal names them: what undoes it
// is removing that person. An id the estate does not hold is its own refusal.
// Neither publishes anything. Mutation: drop the redeemed check and the
// redeemed invitation's record lands.
func TestOnlyAnUnredeemedInvitationIsCancelled(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	seat := rig.vacantSeat("platform-lead")
	issued, err := inviteFor(t, rig, "sam@example.com", seat)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, err = redeemAs(t, rig, issued, "sam@example.com", issued.Secret, seat); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rig.drain()
	person := rig.column(`SELECT person_id FROM iam_invites WHERE id = ?`, issued.ID)
	before, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, err = cancel(t, rig, issued.ID)
	var redeemed *iamdomain.InvitationRedeemed
	if !errors.As(err, &redeemed) || len(person) != 1 || redeemed.Person != person[0] {
		t.Errorf("cancelling a redeemed invitation answered %v, want a refusal "+
			"naming %v", err, person)
	}
	if _, err = cancel(t, rig, "0192f00d-0000-7000-8000-00000000dead"); !errors.Is(err,
		iamdomain.ErrNoInvitation) {
		t.Errorf("cancelling an id nobody issued answered %v", err)
	}
	if after, _ := rig.log.End(t.Context()); after != before {
		t.Errorf("the refused cancellations moved the log from %d to %d",
			before, after)
	}
}

// THE LISTING IS THE OPEN INVITATIONS UNLESS IT IS ASKED FOR ALL, AND IT PAGES.
//
// Open is the issue's own predicate — not redeemed, a deadline still ahead —
// judged at the instant the caller names; `All` adds every one the estate
// holds; and a page one row short of the rest says where the next starts. The
// CONTROL is the All listing beside the open one at the same instant.
// Mutation: drop the redeemed term from the predicate and the redeemed
// invitation is listed as open.
func TestTheReaderListsOpenInvitationsUnlessAskedForAll(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	open, err := inviteFor(t, rig, "open@example.com", rig.vacantSeat("open-seat"))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	spent, err := inviteFor(t, rig, "spent@example.com", rig.vacantSeat("spent-seat"))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, err = redeemAs(t, rig, spent, "spent@example.com", spent.Secret,
		"spent-seat"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rig.drain()
	reader := rig.reader(t)
	ids := func(q iamdomain.InvitationsQuery) []string {
		t.Helper()
		page, err := reader.Invitations(t.Context(), q)
		if err != nil {
			t.Fatalf("Invitations(%+v): %v", q, err)
		}
		var out []string
		for _, row := range page.Invitations {
			out = append(out, row.ID)
		}
		return out
	}
	now := brokerAt
	if got := ids(iamdomain.InvitationsQuery{Now: now}); len(got) != 1 ||
		got[0] != open.ID {
		t.Errorf("the open listing is %v, want only %s", got, open.ID)
	}
	if got := ids(iamdomain.InvitationsQuery{Now: now, All: true}); len(got) != 2 {
		t.Errorf("the whole listing is %v, want both", got)
	}
	// PAST THE WEEK, the open one has aged out and is listed only as all.
	later := now.Add(200 * time.Hour)
	if got := ids(iamdomain.InvitationsQuery{Now: later}); len(got) != 0 {
		t.Errorf("past every deadline the open listing is %v", got)
	}
	page, err := reader.Invitations(t.Context(),
		iamdomain.InvitationsQuery{Now: now, All: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Invitations) != 1 || page.Next != page.Invitations[0].ID {
		t.Fatalf("a page of one is %+v, want one row and a cursor at it", page)
	}
	rest := ids(iamdomain.InvitationsQuery{Now: now, All: true, After: page.Next})
	if len(rest) != 1 || rest[0] == page.Next {
		t.Errorf("the page after %s is %v", page.Next, rest)
	}
}
