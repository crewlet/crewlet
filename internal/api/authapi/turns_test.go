package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// invitingEstate is the sign-in rig's estate holding one live invitation too,
// so one surface serves a sign-in, a step-up and a redemption.
type invitingEstate struct{ *estate }

func (invitingEstate) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	return liveInvitation{}.InvitationByID(ctx, id)
}

// posted is one request served in the background, and what it answered.
type posted struct {
	rec  *httptest.ResponseRecorder
	done chan struct{}
}

// postFrom serves one POST from source in the background, on ctx, as the
// principal given (nil for an unguarded route).
func postFrom(ctx context.Context, mux *http.ServeMux, path, source string,
	body any, principal *iam.Principal) *posted {

	raw, _ := json.Marshal(body)
	if principal != nil {
		ctx = iam.WithPrincipal(ctx, *principal)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path,
		strings.NewReader(string(raw)))
	req.RemoteAddr = source + ":4711"
	p := &posted{rec: httptest.NewRecorder(), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		mux.ServeHTTP(p.rec, req)
	}()
	return p
}

// answered reports whether the request has answered within a moment — long
// enough, race detector included, for one that was not waiting on anything to
// have finished a derivation at the rig's cost.
func (p *posted) answered(within time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(within):
		return false
	}
}

// A SIGN-IN WAITS FOR ITS OWN ADDRESS'S TURN AND NOBODY ELSE'S — AND GIVES IT
// UP WHEN IT GOES AWAY.
//
// Every derivation a request causes is asked for in the turn of the address it
// came from, one turn per address at the verify cap, so one address's flood
// queues behind itself rather than in front of everybody (internal/iam/
// credential's turns.go). That holds only if each route hands the hasher the
// request's own source. Here one address's turn is held, and from it a wrong
// password, an unknown login, a step-up and an invitation's redemption all
// wait — while a sign-in from another address is served at once. The waiting
// requests are then abandoned: each answers 503 rather than a refusal, and not
// one counts as a failed attempt, because nothing was verified.
//
// Mutation: hand the hasher any fixed source — nobody's — from one of the four
// and that request does not wait for the held turn.
func TestASignInWaitsForItsOwnAddressesTurn(t *testing.T) {
	t.Parallel()
	const held, other = "203.0.113.9", "198.51.100.40"
	// TWO SLOTS, so the held turn leaves one free: a request that waits is
	// waiting for its source's turn, and not for the cap.
	hasher := credential.NewHasher(cheap, 2)
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Hasher = hasher
		o.Directory = invitingEstate{o.Directory.(*estate)}
	})
	mux := http.NewServeMux()
	r.svc.Routes(mux)

	release, err := credential.HoldTurn(t.Context(), hasher, held)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	gone, abandon := context.WithCancel(t.Context())
	person := r.estate.person
	waiting := map[string]*posted{
		"a wrong password": postFrom(gone, mux, "/auth/login", held,
			map[string]string{"login": person.Login, "password": "not-the-passphrase-at-all"}, nil),
		"an unknown login": postFrom(gone, mux, "/auth/login", held,
			map[string]string{"login": "nobody.here", "password": password}, nil),
		"a step-up": postFrom(gone, mux, "/auth/step-up", held,
			map[string]string{"password": "not-the-passphrase-at-all"}, &iam.Principal{
				ID: uuid.MustParse(person.ID), Login: person.Login,
				Kind: iam.KindPerson, Stage: iam.StageActive,
				ReauthAt: clock.Add(time.Hour), SensitiveReauthAt: clock.Add(15 * time.Minute),
			}),
		"a redemption": postFrom(gone, mux, "/auth/invite/"+invitationID, held,
			map[string]string{"secret": invitationSecret, "login": "dana.sre",
				"name": "Dana", "password": "a-perfectly-fine-passphrase"}, nil),
	}

	served := postFrom(t.Context(), mux, "/auth/login", other,
		map[string]string{"login": person.Login, "password": password}, nil)
	if !served.answered(30 * time.Second) {
		t.Fatal("a sign-in from another address waited for the held turn")
	}
	if served.rec.Code != http.StatusUnauthorized ||
		codeOf(t, served.rec) != string(httpjson.CodeSecondFactorRequired) {
		t.Fatalf("the other address's sign-in answered %d %s, want the "+
			"second-factor prompt its right password earns", served.rec.Code,
			served.rec.Body)
	}
	for what, p := range waiting {
		if p.answered(200 * time.Millisecond) {
			t.Errorf("%s from the held address answered %d %s without waiting "+
				"for its address's turn", what, p.rec.Code, p.rec.Body)
		}
	}

	abandon()
	for what, p := range waiting {
		if !p.answered(30 * time.Second) {
			t.Fatalf("%s went on waiting after its request went away", what)
		}
		if p.rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s abandoned in the queue answered %d %s, want 503 — "+
				"nothing was verified, so nothing was refused", what,
				p.rec.Code, p.rec.Body)
		}
	}
	if _, failures := r.audit.snapshot(); len(failures) != 0 {
		t.Errorf("abandoned attempts counted as %d failed attempts: %+v",
			len(failures), failures)
	}
}
