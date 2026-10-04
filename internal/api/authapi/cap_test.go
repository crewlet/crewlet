package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// capDirectory is the arms' people beside one live invitation, so one surface
// serves every arm of a sign-in, a step-up and a redemption.
type capDirectory struct{ armsDirectory }

func (capDirectory) InvitationByID(ctx context.Context, id string) (
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

// EVERY ARM OF A SIGN-IN WAITS FOR THE VERIFY CAP — AND GIVES UP ITS PLACE WHEN
// IT GOES AWAY.
//
// A sign-in surface is not a roster only while every arm does the same work,
// and the work is one argon2id derivation under the node's verify cap: a
// verification where there is a verifier, and a verification against the
// hasher's dummy where there is none — nobody by that login or address, a
// directory this node cannot read, a person suspended, an enrolment nobody
// finished, a person who holds no password. So with every slot of the cap
// held, every arm waits — a wrong password, each of those, a step-up and an
// invitation's redemption alike. They are then abandoned: each answers 503
// rather than a refusal, and not one counts as a failed attempt, because
// nothing was verified. The control is the cap itself: once the slot frees, a
// sign-in is served.
//
// Mutations: skip the decoy on any arm, or let it derive outside the cap, and
// that arm answers without waiting; wait for a slot without the request's
// context and the abandoned arms never answer.
func TestEverySignInArmWaitsForTheVerifyCap(t *testing.T) {
	t.Parallel()
	hasher := credential.NewHasher(cheap, 1)
	verifier, err := hasher.Hash(t.Context(), rightPassword)
	if err != nil {
		t.Fatal(err)
	}
	audit := &recordingAudit{}
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendLocal
	mux := http.NewServeMux()
	buildWith(t, b, func(o *authapi.Options) {
		o.Hasher = hasher
		o.Directory = capDirectory{armsDirectory{verifier: verifier}}
		o.Audit = audit
	}).Routes(mux)

	release, err := credential.HoldSlot(t.Context(), hasher)
	if err != nil {
		t.Fatal(err)
	}

	gone, abandon := context.WithCancel(t.Context())
	// A SOURCE PER ARM, so no two arms share a pair on the curve and none
	// waits on the curve rather than on the cap.
	source := 0
	from := func() string {
		source++
		return "198.51.100." + strconv.Itoa(source)
	}
	signIn := func(login, password string) *posted {
		return postFrom(gone, mux, "/auth/login", from(),
			map[string]string{"login": login, "password": password}, nil)
	}
	waiting := map[string]*posted{
		"nobody by that login":              signIn("nobody.here", rightPassword),
		"nobody at that address":            signIn("gone@example.com", rightPassword),
		"a directory this node cannot read": signIn("broken.store", rightPassword),
		"a person suspended":                signIn("sam.suspended", rightPassword),
		"a person who holds no password":    signIn("no.password", rightPassword),
		"a wrong password":                  signIn("dana.sre", "not-the-passphrase-at-all"),
		"a step-up": postFrom(gone, mux, "/auth/step-up", from(),
			map[string]string{"password": "not-the-passphrase-at-all"}, &iam.Principal{
				ID: uuid.New(), Login: "dana.sre",
				Kind: iam.KindPerson, Stage: iam.StageActive,
				ReauthAt: clock.Add(time.Hour),
			}),
		"a redemption": postFrom(gone, mux, "/auth/invite/"+invitationID, from(),
			map[string]string{"secret": invitationSecret, "login": "dana.newcomer",
				"name": "Dana", "password": "a-perfectly-fine-passphrase"}, nil),
	}
	for what, p := range waiting {
		if p.answered(200 * time.Millisecond) {
			t.Errorf("%s answered %d %s with every slot of the verify cap held "+
				"— it spent no derivation", what, p.rec.Code, p.rec.Body)
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
	if _, failures := audit.snapshot(); len(failures) != 0 {
		t.Errorf("abandoned attempts counted as %d failed attempts: %+v",
			len(failures), failures)
	}

	// THE CONTROL: the slot frees, and a sign-in is served.
	release()
	served := postFrom(t.Context(), mux, "/auth/login", from(),
		map[string]string{"login": "dana.sre", "password": rightPassword}, nil)
	if !served.answered(30 * time.Second) {
		t.Fatal("with the cap free, a sign-in still waited")
	}
	if served.rec.Code != http.StatusOK {
		t.Errorf("with the cap free, the right password answered %d %s",
			served.rec.Code, served.rec.Body)
	}
}
