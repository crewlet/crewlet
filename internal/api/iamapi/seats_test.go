package iamapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// seatsRig is a surface over a company with four human seats — one held by an
// active person, one by a suspended one, one an open invitation holds and one
// nothing holds — beside a binding to a seat the company does not hold and an
// invitation onto the vacant seat that aged out.
func seatsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, func(o *iamapi.Options) {
		o.Seats = fakeSeats{seats: []session.Seat{
			{Handle: "founder", Name: "Founder", Kind: "human"},
			{Handle: "ops-lead", Name: "Ops Lead", Kind: "human", Unit: "ops"},
			{Handle: "support", Name: "Support", Kind: "human", Unit: "ops"},
			{Handle: "sales", Name: "Sales", Kind: "human"},
		}}
	})
	r.directory.bindings = []iamdomain.SeatBinding{
		{Person: alice.String(), Login: "alice.admin", Kind: iam.KindPerson,
			Seat: "founder", Stage: iam.StageActive,
			NameSealed: []byte("sealed-name")},
		{Person: bob.String(), Login: "bob.sre", Kind: iam.KindPerson,
			Seat: "ops-lead", Stage: iam.StageSuspended,
			NameSealed: []byte("sealed-name")},
		{Person: "p-gone", Login: "gone.person", Kind: iam.KindPerson,
			Seat: "removed-seat", Stage: iam.StageActive},
	}
	r.directory.seatInvites = []iamdomain.SeatInvitation{
		{Invitation: "inv-open", Seat: "support", Sealed: "sealed",
			InvitedBy: "founder", CreatedAt: at.Add(-time.Hour),
			ExpiresAt: at.Add(time.Hour)},
		// AGED OUT AT THE SURFACE'S CLOCK, and open at any later one —
		// which is what a surface asking at the wrong clock would hold
		// against the seat.
		{Invitation: "inv-lapsed", Seat: "sales", Sealed: "sealed",
			InvitedBy: "founder", CreatedAt: at.Add(-200 * time.Hour),
			ExpiresAt: at.Add(-time.Minute)},
	}
	return r
}

// seatsOf is the listing's rows keyed by handle.
func seatsOf(t *testing.T, got answered) map[string]map[string]any {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/seats = %d, want 200: %v", got.status, got.body)
	}
	rows, _ := got.body["seats"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		out[seat["handle"].(string)] = seat
	}
	return out
}

// part is one half of a row — its holder or its invitation — or nil.
func part(row map[string]any, name string) map[string]any {
	held, _ := row[name].(map[string]any)
	return held
}

// THE SEAT LISTING IS EVERY HUMAN SEAT OF THE RUNNING COMPANY AND WHAT HOLDS IT.
//
// It is what creating, inviting or moving a person onto a seat reads: a seat a
// suspended person holds is held — suspending somebody is not giving their seat
// away — and so is a seat an OPEN invitation names, which nothing else may take
// until it is redeemed, cancelled or lapses. An invitation that has aged out at
// this node's clock holds nothing. A binding to a seat the company does not hold
// lists nothing, since the listing is of seats; `GET /iam/check` is where that
// binding is reported. The `unheld` filter is the seats nothing holds — no
// holder and no invitation — which are the ones a create, an invitation or a
// move may name.
//
// Mutation: list a seat an invitation holds as unheld, or ask the directory at
// any clock but the surface's own, and the support or sales rows go red; drop
// a holder's kind and the founder's does.
func TestIamSeatsListsHumanSeatsAndWhatHoldsThem(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)

	all := seatsOf(t, r.as(administrator(), http.MethodGet, "/iam/seats", nil))
	if !r.directory.claimedAt.Equal(at) {
		t.Errorf("the claims were asked open at %s, want this node's clock %s",
			r.directory.claimedAt, at)
	}
	founder := part(all["founder"], "holder")
	if len(all) != 4 || founder["login"] != "alice.admin" ||
		founder["kind"] != string(iam.KindPerson) ||
		part(all["ops-lead"], "holder")["login"] != "bob.sre" ||
		part(all["ops-lead"], "holder")["stage"] != string(iam.StageSuspended) {
		t.Errorf("the listing = %v, want four seats: the founder's and the ops "+
			"lead's held by their people, each with its kind", all)
	}
	invited := part(all["support"], "invitation")
	if part(all["support"], "holder") != nil || invited["id"] != "inv-open" ||
		invited["email"] != "invited@example.com" ||
		invited["invited_by"] != "founder" || invited["expires_at"] == nil {
		t.Errorf("support = %v, want held by its open invitation, its address "+
			"opened", all["support"])
	}
	if part(all["sales"], "holder") != nil || part(all["sales"], "invitation") != nil {
		t.Errorf("sales = %v, want nothing holding it: its invitation aged out",
			all["sales"])
	}
	raw, _ := json.Marshal(all)
	for _, leak := range []string{"url", "secret", "verifier", "#/invite/"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the listing carries %q: %s", leak, raw)
		}
	}
	if _, listed := all["removed-seat"]; listed {
		t.Error("a binding to a seat the company does not hold was listed as a seat")
	}

	unheld := seatsOf(t, r.as(administrator(), http.MethodGet, "/iam/seats?unheld=true", nil))
	if _, listed := unheld["sales"]; len(unheld) != 1 || !listed {
		t.Errorf("?unheld=true = %v, want sales alone — a suspended person "+
			"still holds their seat, and an open invitation holds its own", unheld)
	}

	if got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=maybe", nil); got.status != http.StatusBadRequest {
		t.Errorf("?unheld=maybe = %d, want 400", got.status)
	}
}

// EVERYBODY SEES WHO HOLDS EACH SEAT, AND ONLY AN ADMIN SEES THEIR LOGIN.
//
// The org chart is everybody's overview (ADR-0033), and so is who sits in each
// of its human seats — drawn by the name they go by. What identifies their
// ACCOUNT — the directory id, the login a sign-in form takes, how far through
// enrolment they are — is the directory's: an admin reads it, and so does the
// holder about themselves. An OPEN INVITATION carries the address it was sent
// to, so it is an admin's alone, and so is `?unheld=true`, the helper every
// admin gesture that puts somebody on a seat picks from — refused anybody else
// naming admin. A name this keyring cannot open is `sealed`, never an empty
// name a screen would draw as a vacant seat.
//
// Mutations: send the login to every reader and the colleague's view of the
// founder goes red; drop the holder-about-themselves arm and bob's own row
// does; send the invitation to everybody and support's does.
func TestEverybodySeesWhoHoldsEachSeatAndOnlyAdminSeesTheirLogin(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)

	// A COLLEAGUE — every app, no admin, holding the ops lead's seat.
	seen := seatsOf(t, r.as(ordinary(), http.MethodGet, "/iam/seats", nil))
	founder := part(seen["founder"], "holder")
	if founder["name"] != "Opened Name" || founder["kind"] != string(iam.KindPerson) {
		t.Errorf("a colleague sees the founder's seat held by %v, want her "+
			"name and kind", founder)
	}
	for _, account := range []string{"person", "login", "stage"} {
		if _, shown := founder[account]; shown {
			t.Errorf("a colleague is sent the founder's %s: %v", account, founder)
		}
	}
	own := part(seen["ops-lead"], "holder")
	if own["person"] != bob.String() || own["login"] != "bob.sre" ||
		own["stage"] != string(iam.StageSuspended) {
		t.Errorf("bob's own seat reads %v, want his own account beside his name",
			own)
	}
	if part(seen["support"], "invitation") != nil {
		t.Errorf("a colleague is sent the open invitation on support: %v",
			seen["support"])
	}
	raw, _ := json.Marshal(seen)
	if strings.Contains(string(raw), "invited@example.com") ||
		strings.Contains(string(raw), "alice.admin") {
		t.Errorf("a colleague's listing carries an address or another's "+
			"login: %s", raw)
	}

	// AN ADMIN sees the account and the invitation.
	all := seatsOf(t, r.as(administrator(), http.MethodGet, "/iam/seats", nil))
	if held := part(all["founder"], "holder"); held["login"] != "alice.admin" ||
		held["person"] != alice.String() || held["stage"] != string(iam.StageActive) {
		t.Errorf("an admin sees the founder as %v, want her account", held)
	}
	if part(all["support"], "invitation") == nil {
		t.Errorf("an admin is not sent the open invitation on support: %v",
			all["support"])
	}

	// THE VACANCY HELPER IS AN ADMIN'S.
	got := r.as(ordinary(), http.MethodGet, "/iam/seats?unheld=true", nil)
	if grants, _ := got.body["grants"].([]any); got.status != http.StatusForbidden ||
		len(grants) != 1 || grants[0] != string(iam.GrantAdmin) {
		t.Errorf("?unheld=true for a colleague = %d %v, want 403 naming admin",
			got.status, got.body)
	}

	// A NAME THIS KEYRING CANNOT OPEN is sealed, never vacant.
	locked := newRig(t, func(o *iamapi.Options) {
		o.Opener = failingOpener{person: alice.String(),
			err: errors.New("no key on this ring")}
	})
	locked.directory.bindings = []iamdomain.SeatBinding{{Person: alice.String(),
		Login: "alice.admin", Kind: iam.KindPerson, Seat: "founder",
		Stage: iam.StageActive, NameSealed: []byte("sealed-name")}}
	held := part(seatsOf(t, locked.as(ordinary(), http.MethodGet, "/iam/seats",
		nil))["founder"], "holder")
	if held["sealed"] != true || held["name"] != nil || held["kind"] != string(iam.KindPerson) {
		t.Errorf("a name this keyring cannot open reads %v, want sealed with "+
			"its kind and no name", held)
	}
}

// A SEAT AN INVITATION HOLDS WHOSE ADDRESS THIS KEYRING CANNOT OPEN IS LISTED
// SEALED, as the invitation listing lists it — never with an empty address,
// which would read as an invitation sent to nobody, and never taking the
// listing down over one row.
func TestIamSeatsListsAnUnopenableInvitationAsSealed(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)
	r.directory.seatInvites[0].Sealed = "foreign"
	invited := part(seatsOf(t, r.as(administrator(), http.MethodGet,
		"/iam/seats", nil))["support"], "invitation")
	if invited["sealed"] != true || invited["email"] != nil {
		t.Errorf("an address this keyring cannot open listed as %v, want sealed",
			invited)
	}
}

// AN UNREADABLE DIRECTORY IS 503, NEVER A LIST OF VACANCIES — and so is a seat
// two rows bind, and a node with no company says so rather than listing none.
//
// Read as "nobody holds anything", `?unheld=true` would list every seat in the
// company under a parameter that promised the vacancies. Two rows binding one
// seat — a record this node retained, or a restore — leave this node unable
// to say who holds it, so another node answers rather than this one picking.
// Two OPEN INVITATIONS on one seat do not: a build before invitations held their
// seats may have left two, and the newest is listed. The control is the same
// request against a directory that reads.
//
// Mutation: answer the first of two holders and the duplicate reads 200.
func TestIamSeatsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)
	r.directory.claimsErr = errors.New("the identity estate is not open")
	got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=true", nil)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") == "" {
		t.Errorf("an unreadable directory = %d (Retry-After %q), want 503 with a "+
			"hint: %v", got.status, got.header.Get("Retry-After"), got.body)
	}
	r.directory.claimsErr = nil
	if got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=true", nil); got.status != http.StatusOK {
		t.Errorf("the control: a directory that reads = %d, want 200", got.status)
	}
	r.directory.seatInvites = append(r.directory.seatInvites, iamdomain.SeatInvitation{
		Invitation: "inv-newer", Seat: "support", Sealed: "sealed",
		CreatedAt: at, ExpiresAt: at.Add(2 * time.Hour)})
	if got := part(seatsOf(t, r.as(administrator(), http.MethodGet, "/iam/seats",
		nil))["support"], "invitation"); got["id"] != "inv-newer" {
		t.Errorf("a seat two open invitations name lists %v, want the newest", got)
	}
	r.directory.bindings = append(r.directory.bindings, iamdomain.SeatBinding{
		Person: "p-twin", Login: "twin.person", Kind: iam.KindPerson, Seat: "founder",
		Stage: iam.StageActive})
	if got := r.as(administrator(), http.MethodGet, "/iam/seats", nil); got.status != http.StatusServiceUnavailable {
		t.Errorf("a seat two rows bind = %d %v, want 503", got.status, got.body)
	}

	none := newRig(t, func(o *iamapi.Options) { o.Seats = fakeSeats{missing: true} })
	if got := none.as(administrator(), http.MethodGet, "/iam/seats", nil); got.status != http.StatusConflict ||
		got.body["error"] != "no_active_revision" {
		t.Errorf("a node with no company = %d %v, want 409 no_active_revision",
			got.status, got.body)
	}
}
