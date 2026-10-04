package iamapi_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// seatsRig is a surface over a company with three human seats — one held by
// an active person, one by a suspended one, one by nobody — and a binding to
// a seat the company does not hold.
func seatsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, func(o *iamapi.Options) {
		o.Seats = fakeSeats{seats: []session.Seat{
			{Handle: "founder", Name: "Founder", Kind: "human"},
			{Handle: "ops-lead", Name: "Ops Lead", Kind: "human", Unit: "ops"},
			{Handle: "support", Name: "Support", Kind: "human", Unit: "ops"},
		}}
	})
	r.directory.bindings = []iamdomain.SeatBinding{
		{Person: alice.String(), Login: "alice.admin", Seat: "founder", Stage: iam.StageActive},
		{Person: bob.String(), Login: "bob.sre", Seat: "ops-lead", Stage: iam.StageSuspended},
		{Person: "p-gone", Login: "gone.person", Seat: "removed-seat", Stage: iam.StageActive},
	}
	return r
}

// seatsOf is the listing's rows keyed by handle, with each row's holder — nil
// for a seat nobody holds.
func seatsOf(t *testing.T, got answered) map[string]map[string]any {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/seats = %d, want 200: %v", got.status, got.body)
	}
	rows, _ := got.body["seats"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		holder, _ := seat["holder"].(map[string]any)
		out[seat["handle"].(string)] = holder
	}
	return out
}

// THE SEAT LISTING IS EVERY HUMAN SEAT OF THE RUNNING COMPANY AND WHO HOLDS IT.
//
// It is what assigning a person to a seat reads: a seat a suspended person
// holds is held — suspending somebody is not giving their seat away — and a
// binding to a seat the company does not hold lists nothing, since the listing
// is of seats; `GET /iam/check` is where that binding is reported. The
// `unheld` filter is the seats nobody holds at all.
func TestIamSeatsListsHumanSeatsAndWhoHoldsThem(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)

	all := seatsOf(t, r.as(administrator(), http.MethodGet, "/iam/seats", nil))
	if len(all) != 3 || all["founder"]["login"] != "alice.admin" ||
		all["ops-lead"]["login"] != "bob.sre" ||
		all["ops-lead"]["stage"] != string(iam.StageSuspended) ||
		all["support"] != nil {
		t.Errorf("the listing = %v, want three seats: the founder's and the ops "+
			"lead's held, support's not", all)
	}
	if _, listed := all["removed-seat"]; listed {
		t.Error("a binding to a seat the company does not hold was listed as a seat")
	}

	unheld := seatsOf(t, r.as(auditor(), http.MethodGet, "/iam/seats?unheld=true", nil))
	if _, listed := unheld["support"]; len(unheld) != 1 || !listed {
		t.Errorf("?unheld=true = %v, want support alone — a suspended "+
			"person still holds their seat", unheld)
	}

	if got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=maybe", nil); got.status != http.StatusBadRequest {
		t.Errorf("?unheld=maybe = %d, want 400", got.status)
	}
}

// AN UNREADABLE DIRECTORY IS 503, NEVER A LIST OF VACANCIES — and so is a seat
// two rows bind, and a node with no company says so rather than listing none.
//
// Read as "nobody holds anything", `?unheld=true` would list every seat in the
// company under a parameter that promised the vacancies. Two rows binding one
// seat — a record this node retained, or a restore — leave this node unable
// to say who holds it, so another node answers rather than this one picking.
// The control is the same request against a directory that reads.
//
// Mutation: answer the first of two holders and the duplicate reads 200.
func TestIamSeatsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	r := seatsRig(t)
	r.directory.bindingsErr = errors.New("the identity estate is not open")
	got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=true", nil)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") == "" {
		t.Errorf("an unreadable directory = %d (Retry-After %q), want 503 with a "+
			"hint: %v", got.status, got.header.Get("Retry-After"), got.body)
	}
	r.directory.bindingsErr = nil
	if got := r.as(administrator(), http.MethodGet, "/iam/seats?unheld=true", nil); got.status != http.StatusOK {
		t.Errorf("the control: a directory that reads = %d, want 200", got.status)
	}
	r.directory.bindings = append(r.directory.bindings, iamdomain.SeatBinding{
		Person: "p-twin", Login: "twin.person", Seat: "founder", Stage: iam.StageActive})
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
