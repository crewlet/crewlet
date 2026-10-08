package iamapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// listedInvitations is the `invitations` array of a listing, by id.
func listedInvitations(t *testing.T, got answered) map[string]map[string]any {
	t.Helper()
	rows, ok := got.body["invitations"].([]any)
	if !ok {
		t.Fatalf("the listing carries no invitations array: %v", got.body)
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		view, _ := row.(map[string]any)
		id, _ := view["id"].(string)
		out[id] = view
	}
	return out
}

// THE LISTING IS THE OPEN INVITATIONS, AND `all=true` IS EVERY ONE HELD.
//
// An administrator's first question is "who have we invited who has not
// joined", so the default is the links that still open; the expired and the
// redeemed are the estate's until the sweep collects them, and `all=true` adds
// them, each saying which it is. An address this node's keyring cannot open is
// SEALED, never blank — a blank reads as an invitation sent to nobody. And no
// row carries what opens a link.
//
// The CONTROL is the default listing beside `all=true`. Mutation: drop the
// `all` parameter and the expired and redeemed rows never appear; label by
// [iamdomain.InvitationRow.Spent] alone and the redeemed row reads expired.
func TestTheInvitationListingIsOpenUnlessAllIsAsked(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	verifier := iamdomain.InvitationVerifier("the-link-secret")
	r.directory.invitations = []iamdomain.InvitationRow{
		{ID: "inv-open", Sealed: "sealed", Seat: "founder",
			Grants: []iam.Grant{iam.GrantStateRead}, InvitedBy: "alice.admin",
			ExpiresAt: at.Add(time.Hour), Verifier: verifier},
		{ID: "inv-foreign", Sealed: "foreign", Seat: "sre",
			ExpiresAt: at.Add(time.Hour)},
		{ID: "inv-expired", Sealed: "sealed", Seat: "sre",
			ExpiresAt: at.Add(-time.Hour)},
		{ID: "inv-redeemed", Sealed: "sealed", Seat: "sre",
			ExpiresAt: at.Add(time.Hour), RedeemedAt: at.Add(-time.Minute),
			Person: bob.String()},
		// ISSUED BEFORE EVERY INVITATION NAMED A SEAT: its deadline is
		// ahead and it opens nothing, since its redemption is refused.
		{ID: "inv-seatless", Sealed: "sealed", ExpiresAt: at.Add(time.Hour)},
	}

	open := r.as(administrator(), http.MethodGet, "/iam/invitations", nil)
	if open.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", open.status, open.body)
	}
	if r.directory.invited.All || !r.directory.invited.Now.Equal(at) {
		t.Errorf("the default listing asked %+v, want the open ones at the "+
			"surface's clock", r.directory.invited)
	}
	rows := listedInvitations(t, open)
	if len(rows) != 2 || rows["inv-open"] == nil || rows["inv-foreign"] == nil {
		t.Fatalf("the default listing is %v, want the two open invitations", rows)
	}
	first := rows["inv-open"]
	if first["email"] != "invited@example.com" || first["state"] != "open" ||
		first["seat"] != "founder" || first["invited_by"] != "alice.admin" {
		t.Errorf("the open invitation rendered %v", first)
	}
	if sealed := rows["inv-foreign"]; sealed["sealed"] != true || sealed["email"] != nil {
		t.Errorf("an address the keyring cannot open rendered %v, want sealed "+
			"and no address", sealed)
	}
	raw, _ := json.Marshal(open.body)
	for _, leak := range []string{"the-link-secret", verifier, "verifier",
		"secret", "url"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the listing carries %q: %s", leak, raw)
		}
	}

	every := r.as(administrator(), http.MethodGet, "/iam/invitations?all=true", nil)
	rows = listedInvitations(t, every)
	if len(rows) != 5 {
		t.Fatalf("all=true listed %v, want all five held", rows)
	}
	if rows["inv-expired"]["state"] != "expired" {
		t.Errorf("an aged-out invitation rendered %v", rows["inv-expired"])
	}
	if rows["inv-seatless"]["state"] != "expired" {
		t.Errorf("an invitation naming no seat rendered %v, want one that no "+
			"longer opens", rows["inv-seatless"])
	}
	if redeemed := rows["inv-redeemed"]; redeemed["state"] != "redeemed" ||
		redeemed["person"] != bob.String() {
		t.Errorf("a redeemed invitation rendered %v, want redeemed naming the "+
			"person it created", redeemed)
	}
}

// INVITATIONS ARE READ LIKE THE DIRECTORY AND WITHDRAWN LIKE ANY DIRECTORY
// WRITE.
//
// An auditor reads them — who could reach this company is the audit question —
// and may not cancel one; somebody holding neither grant reads nothing. The
// CONTROL is the administrator, admitted to both. Mutation: mount the listing
// on the write verb and the auditor is refused; mount the cancel on the read
// verb and the auditor cancels.
func TestInvitationsAreReadLikeTheDirectoryAndWithdrawnLikeAWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		who          iam.Principal
		read, cancel int
	}{
		{"an administrator (the control)", administrator(), http.StatusOK, http.StatusOK},
		{"an auditor", auditor(), http.StatusOK, http.StatusForbidden},
		{"somebody holding neither grant", ordinary(), http.StatusForbidden,
			http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if got := r.as(tc.who, http.MethodGet, "/iam/invitations", nil); got.status != tc.read {
				t.Errorf("the listing answered %d, want %d", got.status, tc.read)
			}
			got := r.as(tc.who, http.MethodDelete, "/iam/invitations/inv-1", nil)
			if got.status != tc.cancel {
				t.Errorf("the cancel answered %d, want %d (body %v)", got.status,
					tc.cancel, got.body)
			}
			if tc.cancel != http.StatusOK && len(r.writer.calls) > 0 {
				t.Errorf("a refused cancel reached the writer: %v", r.writer.calls)
			}
		})
	}
}

// A CANCELLATION ANSWERS WHAT ITS RECORD DECIDED, AND IS ANNOUNCED ONLY ONCE IT
// LANDED.
//
// An id the estate does not hold is 404; a redeemed invitation is 409 `stale`
// NAMING the person it created, because what undoes it is removing them, in a
// sentence of the surface's own rather than the domain's error, which carries
// its package and raw ids into a dialog that shows a detail verbatim; a landed
// one is 200 and one `iam_invitation_cancelled` carrying the invitation and
// whoever withdrew it — never an address; an unknown one is 503 and announces
// nothing. Mutation: announce before the outcome is read and the unknown row
// emits; answer the domain's error as the detail, or `bad_params` as the code,
// and the redeemed row goes red.
func TestACancellationAnswersWhatItsRecordDecided(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refuse  error
		outcome statelog.Outcome
		status  int
		event   bool
	}{
		{"it landed (the control)", nil, "", http.StatusOK, true},
		{"nobody issued it", iamdomain.ErrNoInvitation, "", http.StatusNotFound, false},
		{"it was redeemed", &iamdomain.InvitationRedeemed{ID: "inv-1",
			Person: bob.String()}, "", http.StatusConflict, false},
		{"nothing can say", nil, statelog.OutcomeUnknown,
			http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if tc.refuse != nil {
				r.writer.refusals = map[string]error{"cancel": tc.refuse}
			}
			if tc.outcome != "" {
				r.writer.outcomes = map[string]statelog.Outcome{"cancel": tc.outcome}
			}
			got := r.as(administrator(), http.MethodDelete,
				"/iam/invitations/inv-1?reason=sent+to+the+wrong+address", nil)
			if got.status != tc.status {
				t.Fatalf("answered %d, want %d (body %v)", got.status, tc.status,
					got.body)
			}
			if tc.status == http.StatusConflict {
				if got.body["person"] != bob.String() {
					t.Errorf("the refusal of a redeemed invitation does not name "+
						"the person it created: %v", got.body)
				}
				detail, _ := got.body["detail"].(string)
				if got.body["error"] != "stale" ||
					!strings.Contains(detail, "already been redeemed") ||
					strings.Contains(detail, "iamdomain") ||
					strings.Contains(detail, bob.String()) {
					t.Errorf("a redeemed invitation is refused %v, want `stale` "+
						"saying it was redeemed in words of the surface's own", got.body)
				}
			}
			if !tc.event {
				if seen := r.audit.all(); len(seen) != 0 {
					t.Errorf("announced %v about a cancellation that did not land", seen)
				}
				return
			}
			event := only[types.IAMInvitationCancelled](t, r.audit)
			if event.Invitation != "inv-1" || event.By != "founder" ||
				event.Reason != "sent to the wrong address" {
				t.Errorf("announced %+v", event)
			}
		})
	}
}
