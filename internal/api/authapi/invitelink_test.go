package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

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
		return iamdomain.InvitationRow{Vouched: true}, nil
	}
	return d.sealedInvitation.InvitationByID(ctx, id)
}

// unvouchedInvitation is [issuedInvitation] on a node whose rows cannot vouch
// for what they answer ([iamdomain.InvitationRow.Vouched]).
type unvouchedInvitation struct{ issuedInvitation }

func (d unvouchedInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := d.issuedInvitation.InvitationByID(ctx, id)
	row.Vouched = false
	return row, err
}

// AN INVITATION THIS NODE CANNOT VOUCH FOR IS NO DEAD LINK, for the reset
// link's reason ([TestALinkThisNodeCannotVouchForIsNoDeadLink]): an id these
// rows do not hold and a real id with a wrong secret are both 503 and neither
// is counted, while the live link is served — the CONTROL. Mutation: drop the
// vouch from the view and the unknown id is a counted 410.
func TestAnInvitationThisNodeCannotVouchForIsNoDeadLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, id, secret string
		status           int
	}{
		{"the live link (the control)", invitationID, invitationSecret, http.StatusOK},
		{"an id these rows do not hold", uuid.Must(uuid.NewV7()).String(),
			invitationSecret, http.StatusServiceUnavailable},
		{"a secret that is not the link's", invitationID, "not-the-links-secret",
			http.StatusServiceUnavailable},
	} {
		audit := &recordingAudit{}
		mux := http.NewServeMux()
		buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
			o.Directory = unvouchedInvitation{issuedInvitation{id: invitationID}}
			o.Sealer = stubSealer{address: "dana@example.com"}
			o.Audit = audit
		}).Routes(mux)
		req := httptest.NewRequest(http.MethodGet, "/auth/invite/"+tc.id, nil)
		req.Header.Set(secretHeader, tc.secret)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s: the view answered %d, want %d: %s", tc.name, rec.Code,
				tc.status, rec.Body)
			continue
		}
		if _, failures := audit.snapshot(); len(failures) != 0 {
			t.Errorf("%s: counted %v on a node that could not say", tc.name, failures)
		}
	}
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
		o.Directory = sealedInvitation{}
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
	if len(writer.enrolled) != 0 || len(writer.opened()) != 0 {
		t.Fatalf("rendering the invitation wrote %d enrolments and %d "+
			"sessions — a GET spent it", len(writer.enrolled),
			len(writer.opened()))
	}

	// THE CONTROL: the POST is the person, and its record spends the link
	// — binding the seat the invitation names and presenting the link's
	// secret to the record.
	redeemed := postJSON(t, mux, "/auth/invite/"+invitationID, map[string]string{
		"secret": invitationSecret, "login": "dana.sre", "name": "Dana",
		"password": "a-perfectly-fine-passphrase"})
	if redeemed.Code != http.StatusOK {
		t.Fatalf("the redemption answered %d (%s)", redeemed.Code, redeemed.Body)
	}
	if len(writer.enrolled) != 1 || writer.enrolled[0].Invitation != invitationID {
		t.Fatalf("the redemption wrote %+v, want one enrolment naming the "+
			"invitation it spends", writer.enrolled)
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
// by the redemption's own record; the person holding the link can do nothing
// about it but ask for a new one, and the holder is none of their business.
// Mutation: drop the seat arm and the refusal says the LOGIN is taken, which
// sends them choosing names that will never help.
func TestARedemptionWhoseSeatWasTakenSaysSo(t *testing.T) {
	t.Parallel()
	const holder = "018f3a9c-0000-7000-8000-0000000000a2"
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = sealedInvitation{}
		o.Sealer = stubSealer{address: "dana@example.com"}
		o.Writer = refusingWriter{err: &iamdomain.ErrTaken{
			Field: iamdomain.UniqueSeat, Value: "eng-lead", Person: holder}}
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

// AN INVITATION NAMES WHO SENT IT AS THE CHART NAMES THEM.
//
// A person bound to a seat writes as the seat, so the invitation records its
// author by the seat's handle, and the page said "founder invited you" to
// somebody who knows that person as "Jane Founder". A machine — a Tier A
// token, a service account — is named by nobody: "token:founder invited you"
// told the company's first person nothing they could recognise. The CONTROL
// is an author the chart does not hold, named as the record holds it.
// Mutation: answer the recorded author and the seat's name is never shown and
// the machine is named.
func TestAnInvitationNamesWhoSentItAsTheChartNamesThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		seats  companySeats
		author string
		want   string
	}{
		{"a seat the chart holds", companySeats{"founder": {Handle: "founder",
			Kind: session.SeatKindHuman, Name: "Jane Founder"}}, "", "Jane Founder"},
		{"a Tier A token", companySeats{}, "token:founder", ""},
		{"a seat it does not (the control)", companySeats{}, "", "founder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory = authoredInvitation{author: tc.author}
				o.Seats = tc.seats
				o.Sealer = stubSealer{address: "dana@example.com"}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, viewInvite(invitationID))
			var view struct {
				InvitedBy string `json:"invited_by"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil ||
				rec.Code != http.StatusOK || view.InvitedBy != tc.want {
				t.Errorf("the view answered %d naming %q (%v), want %q", rec.Code,
					view.InvitedBy, err, tc.want)
			}
		})
	}
}

// authoredInvitation is [sealedInvitation] issued by author, or by its own
// seat author where author is empty.
type authoredInvitation struct {
	sealedInvitation
	author string
}

func (a authoredInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := a.sealedInvitation.InvitationByID(ctx, id)
	if a.author != "" {
		row.InvitedBy = a.author
	}
	return row, err
}

// unreadableSeats is a chart this node cannot read: every lookup fails.
type unreadableSeats struct{}

func (unreadableSeats) Seat(context.Context, string) (session.Seat, bool, error) {
	return session.Seat{}, false, errors.New("this node is applying a revision")
}

// EVERY INVITATION'S VIEW NAMES ITS SEAT, AS THE CHART HOLDS IT NOW.
//
// A person holds a human seat for as long as they exist (ADR-0026), so every
// invitation holds one and the seat is part of what its holder agrees to: the
// view carries it on EVERY answer, never only where the chart still names it.
// A seat the chart has retired since the issue — removed, or made an agent's —
// is `seat: {}`, present with no handle, which is how the screen says the
// redemption will be refused before anybody types a password. A chart this
// node cannot read shows the stored handle alone: the page is a courtesy, and
// the redemption asks again. The CONTROL is the seat as the chart names it.
//
// Mutation: answer nil for a seat the chart no longer holds and the key is
// absent; drop the kind check and an agent's seat is offered to a person.
func TestEveryInvitationViewNamesItsSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seats session.Chart
		want  map[string]any
	}{
		{"a human seat the chart holds (the control)", engLead,
			map[string]any{"handle": "eng-lead", "name": "Engineering lead"}},
		{"a seat the chart made an agent's", companySeats{"eng-lead": {
			Handle: "eng-lead", Kind: "agent", Name: "Engineering lead"}},
			map[string]any{}},
		{"a seat the chart no longer holds", companySeats{}, map[string]any{}},
		{"a chart this node cannot read", unreadableSeats{},
			map[string]any{"handle": "eng-lead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory = sealedInvitation{}
				o.Seats = tc.seats
				o.Sealer = stubSealer{address: "dana@example.com"}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, viewInvite(invitationID))
			if rec.Code != http.StatusOK {
				t.Fatalf("the view answered %d: %s", rec.Code, rec.Body)
			}
			var view map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			seat, present := view["seat"]
			if !present {
				t.Fatalf("the view carries no seat: %s", rec.Body)
			}
			if got, _ := seat.(map[string]any); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("the view shows seat %v, want %v", seat, tc.want)
			}
		})
	}
}

// seatlessInvitation is [sealedInvitation] issued before every invitation held
// a seat: it names none.
type seatlessInvitation struct{ sealedInvitation }

func (seatlessInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := sealedInvitation{}.InvitationByID(ctx, id)
	row.Seat = ""
	return row, err
}

// AN INVITATION NAMING NO SEAT IS A DEAD LINK, on the view and the redemption.
//
// One issued before every invitation held a seat creates nobody — a person
// holds a human seat for as long as they exist, and the redemption's own
// record refuses a link naming none — so it is answered the one 410 every dead
// link gets, and on the VIEW, before anybody chooses a password at a form
// whose every submission would be refused. NEVER a 400 asking for a seat: the
// invitee left nothing out and the form has no field to put one in; what they
// need is a new link. It is the link's holder presenting the link's own
// secret, so it is not a failed attempt. The CONTROL is the same link onto a
// seat, which opens.
//
// Mutation: read such a link as open and the view renders, and the redemption
// reaches the record.
func TestAnInvitationNamingNoSeatIsADeadLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		directory authapi.Directory
		status    int
	}{
		{"a link onto a seat (the control)", sealedInvitation{}, http.StatusOK},
		{"a link naming no seat", seatlessInvitation{}, http.StatusGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			audit := &recordingAudit{}
			writer := &recordingWriter{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory, o.Writer, o.Audit = tc.directory, writer, audit
				o.Seats = engLead
				o.Sealer = stubSealer{address: "dana@example.com"}
			}).Routes(mux)

			viewed := httptest.NewRecorder()
			mux.ServeHTTP(viewed, viewInvite(invitationID))
			redeemed := postJSON(t, mux, "/auth/invite/"+invitationID,
				map[string]string{"secret": invitationSecret, "login": "dana.sre",
					"name": "Dana", "password": "a-perfectly-fine-passphrase"})
			for route, rec := range map[string]*httptest.ResponseRecorder{
				"the view": viewed, "the redemption": redeemed} {
				if rec.Code != tc.status {
					t.Errorf("%s answered %d, want %d: %s", route, rec.Code,
						tc.status, rec.Body)
				}
				if tc.status == http.StatusGone &&
					codeOf(t, rec) != string(httpjson.CodeInviteSpent) {
					t.Errorf("%s answered %s, want the one refusal every dead "+
						"link gets", route, rec.Body)
				}
			}
			if tc.status == http.StatusGone && len(writer.enrolled) != 0 {
				t.Errorf("a link naming no seat reached the record: %+v",
					writer.enrolled)
			}
			if _, failures := audit.snapshot(); len(failures) != 0 {
				t.Errorf("the link's own holder was counted as a guesser: %v",
					failures)
			}
		})
	}
}
