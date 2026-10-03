package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// seatedInvitation is [sealedInvitation] binding a seat by its handle, which
// the view names as the running company calls it.
type seatedInvitation struct{ sealedInvitation }

func (seatedInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := sealedInvitation{}.InvitationByID(ctx, id)
	row.Seat = "eng-lead"
	return row, err
}

// companySeats is the running company's seats, as the view asks it.
type companySeats map[string]session.Seat

func (c companySeats) Seat(_ context.Context, handle string) (session.Seat, bool, error) {
	seat, found := c[handle]
	return seat, found, nil
}

// engLead is the company holding the seat the invitation binds.
var engLead = companySeats{"eng-lead": {Handle: "eng-lead",
	Kind: session.SeatKindHuman, Name: "Engineering lead"}}

// issuedInvitation is [sealedInvitation] under one id and nothing under any
// other, so a case can present an id nobody issued beside one somebody did.
type issuedInvitation struct {
	sealedInvitation
	id string
}

func (d issuedInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	if id != d.id {
		return iamdomain.InvitationRow{}, nil
	}
	return d.sealedInvitation.InvitationByID(ctx, id)
}

// A GET ON AN INVITE RENDERS AND NEVER SPENDS.
//
// A link is followed by things that are not the person it was sent to — a
// mail client prefetching, a scanner opening every URL in a message, a chat app
// building a preview card — and every one of them is a GET. So the view
// answers what the form renders from, however often it is asked, and writes
// nothing: no enrolment, no spend, no session and no cookie. The POST is the
// person, and it is the one that spends.
//
// Mutation: enrol (or spend) from the view and the writer records it; the
// control — the POST — spending nothing would mean the recorder saw nothing
// either way.
func TestAGetOnAnInviteRendersAndNeverSpends(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = seatedInvitation{}
		o.Seats = engLead
		o.Sealer = stubSealer{address: "dana@example.com"}
		o.Writer = writer
	}).Routes(mux)

	for range 3 {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, viewInvite(invitationID))
		if rec.Code != http.StatusOK {
			t.Fatalf("the view answered %d (%s)", rec.Code, rec.Body)
		}
		if cookies := rec.Result().Cookies(); len(cookies) != 0 {
			t.Errorf("the view set %d cookies — a GET opened a session", len(cookies))
		}
		var view struct {
			Email string `json:"email"`
			Login string `json:"login"`
			Seat  *struct {
				Handle string `json:"handle"`
				Name   string `json:"name"`
			} `json:"seat"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode the view: %v", err)
		}
		if view.Email != "dana@example.com" || view.Login != "dana.example" {
			t.Errorf("the view renders (%q, %q), want the invited address and "+
				"the login it proposes", view.Email, view.Login)
		}
		if view.Seat == nil || view.Seat.Handle != "eng-lead" ||
			view.Seat.Name != "Engineering lead" {
			t.Errorf("the view shows seat %+v, want the seat it binds as the "+
				"running company calls it", view.Seat)
		}
	}
	if len(writer.enrolled) != 0 || len(writer.spent) != 0 || len(writer.opened()) != 0 {
		t.Fatalf("rendering the invitation wrote %d enrolments, %d spends and "+
			"%d sessions — a GET spent it", len(writer.enrolled),
			len(writer.spent), len(writer.opened()))
	}

	// THE CONTROL: the POST is the person, and it spends — binding the
	// seat the invitation names and presenting the link's secret to the
	// record.
	redeemed := postJSON(t, mux, "/auth/invite/"+invitationID, map[string]string{
		"secret": invitationSecret, "login": "dana.sre", "name": "Dana",
		"password": "a-perfectly-fine-passphrase"})
	if redeemed.Code != http.StatusOK {
		t.Fatalf("the redemption answered %d (%s)", redeemed.Code, redeemed.Body)
	}
	if len(writer.enrolled) != 1 || len(writer.spent) != 1 {
		t.Fatalf("the redemption wrote %d enrolments and %d spends, want one "+
			"of each", len(writer.enrolled), len(writer.spent))
	}
	enrolled := writer.enrolled[0]
	if enrolled.Seat != "eng-lead" || enrolled.InvitationSecret != invitationSecret {
		t.Errorf("the enrolment binds seat %q presenting secret %q, want the "+
			"invitation's own seat by its identity and the link's secret",
			enrolled.Seat, enrolled.InvitationSecret)
	}
	var answer struct {
		Seat string `json:"seat"`
	}
	_ = json.Unmarshal(redeemed.Body.Bytes(), &answer)
	if answer.Seat != "eng-lead" {
		t.Errorf("the sign-in answered seat %q, want the seat the person now holds",
			answer.Seat)
	}
}

// A LINK'S SECRET IS WHAT OPENS IT, AND A WRONG ONE IS AN ABSENT INVITATION.
//
// The id is in every snapshot, backup and access log, so presenting it opens
// nothing: the view and the redemption each check the secret the link carries
// beside it. And each refuses a missing or wrong one
// with EXACTLY the answer an id nobody issued gets — the same status, the same
// bytes, counted as a failed attempt — because told apart, a
// guessed secret against a leaked id would say the id exists.
//
// Mutation: drop the secret check from the lookup and every wrong-secret arm
// renders or redeems; answer it with a code of its own and the bytes differ.
func TestALinksSecretIsWhatOpensIt(t *testing.T) {
	t.Parallel()
	const nobodyIssued = "018f3a9c-4d2e-7000-8000-000000000bad"
	routes := []struct {
		name  string
		build func(id, secret string) *http.Request
	}{
		{"the view", func(id, secret string) *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/auth/invite/"+id, nil)
			if secret != "" {
				r.Header.Set(secretHeader, secret)
			}
			return r
		}},
		{"the redemption", func(id, secret string) *http.Request {
			raw, _ := json.Marshal(map[string]string{"secret": secret,
				"login": "dana.sre", "name": "Dana",
				"password": "a-perfectly-fine-passphrase"})
			return httptest.NewRequest(http.MethodPost, "/auth/invite/"+id,
				strings.NewReader(string(raw)))
		}},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			audit := &recordingAudit{}
			writer := &recordingWriter{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory = issuedInvitation{id: invitationID}
				o.Sealer = stubSealer{address: "dana@example.com"}
				o.Writer = writer
				o.Audit = audit
			}).Routes(mux)
			ask := func(id, secret string, source int) *httptest.ResponseRecorder {
				r := route.build(id, secret)
				r.RemoteAddr = "198.51.100." + strconv.Itoa(source) + ":5100"
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, r)
				return rec
			}
			absent := ask(nobodyIssued, invitationSecret, 1)
			if absent.Code != http.StatusGone {
				t.Fatalf("an id nobody issued answered %d (%s)", absent.Code, absent.Body)
			}
			for i, wrong := range []string{"", "not-the-secret", invitationID,
				iamdomain.InvitationVerifier(invitationSecret)} {
				rec := ask(invitationID, wrong, i+2)
				if rec.Code != absent.Code || rec.Body.String() != absent.Body.String() {
					t.Errorf("secret %q answered %d %s, want exactly what an id "+
						"nobody issued answers: %d %s", wrong, rec.Code, rec.Body,
						absent.Code, absent.Body)
				}
			}
			_, failures := audit.snapshot()
			counted := 0
			for _, f := range failures {
				if f.Method == types.FailInvite && !f.Throttled {
					counted++
				}
			}
			if counted != 5 {
				t.Errorf("the trail tallied %d refused links, want 5 — a wrong "+
					"secret is a failed attempt like an absent id", counted)
			}
			if len(writer.enrolled) != 0 {
				t.Errorf("a link without its secret enrolled %+v", writer.enrolled)
			}
		})
	}
}

// A REDEMPTION THE SEAT'S NEW HOLDER REFUSES SAYS SO, and names nobody.
//
// A seat bound to a colleague between the issue and the redemption is refused
// by the record's own claim when the two race; the person holding the link can
// do nothing about it but ask for a new one, and the holder is none of their
// business. Mutation: drop the seat arm and the refusal says the ADDRESS is
// taken, which sends them looking for an account they do not have.
func TestARedemptionWhoseSeatWasTakenSaysSo(t *testing.T) {
	t.Parallel()
	const holder = "018f3a9c-0000-7000-8000-0000000000a2"
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = seatedInvitation{}
		o.Sealer = stubSealer{address: "dana@example.com"}
		o.Writer = refusingWriter{err: &iamdomain.ErrClaimed{
			Kind: iamdomain.KindSeat, Token: "eng-lead", Holder: holder}}
	}).Routes(mux)
	rec := postJSON(t, mux, "/auth/invite/"+invitationID, map[string]string{
		"secret": invitationSecret, "login": "dana.sre", "name": "Dana",
		"password": "a-perfectly-fine-passphrase"})
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "seat this invitation binds") {
		t.Errorf("answered %d %s, want 409 saying the seat is taken", rec.Code,
			rec.Body)
	}
	if strings.Contains(rec.Body.String(), holder) {
		t.Errorf("the refusal names the seat's holder: %s", rec.Body)
	}
}
