package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// requiring is a Tier A whose password backend states `totp`.
func requiring(o *authapi.Options, factor iam.SecondFactor) {
	o.Bootstrap.API.Auth.Local = &config.APILocal{TOTP: factor}
}

// passwordOnly strips a rig's person down to the password: freshly invited,
// the founder, or somebody an administrator reset.
func passwordOnly(e *estate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.person.Credentials = slices.DeleteFunc(e.person.Credentials,
		func(c iamdomain.Credential) bool { return c.Method.SecondFactor() })
}

// Resolve answers a bearer from what this estate was asked to write: the
// session it opened (ended once it was closed) and the person — so a guard
// built over it reads the rows the surface's own writes produced.
func (e *estate) Resolve(_ context.Context, lineage, person string) (
	session.Identity, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	identity := session.Identity{
		Applied: ^uint64(0) >> 1, Generation: e.counters.Generation,
	}
	if person == e.person.ID {
		identity.Person = session.PersonRow{Found: true, Epoch: e.counters.Epoch,
			Stage: e.person.Stage, Login: e.person.Login, Grants: e.person.Grants}
	}
	if e.unapplied {
		// BELOW EVERY START this estate handed out, and holding none of
		// their rows — the node a sign-in answered before it applied.
		identity.Applied = 0
		return identity, nil
	}
	for _, start := range e.starts {
		if start.Lineage != lineage {
			continue
		}
		identity.Session = session.LineageRow{Found: true, Epoch: e.counters.Epoch,
			ProvedAt: start.ProvedAt, EnrolmentOnly: start.EnrolmentOnly,
			Ended: slices.ContainsFunc(e.closes, func(c closedSession) bool {
				return c.lineage == lineage
			})}
	}
	return identity, nil
}

// AwaitApplied is a node that has applied everything.
func (e *estate) AwaitApplied(context.Context, uint64) error { return nil }

// seatless is a chart asked about nobody: the rig's person holds no seat.
type seatless struct{}

func (seatless) Seat(context.Context, string) (session.Seat, bool, error) {
	return session.Seat{}, false, nil
}

// guarded is the sign-in surface and two ordinary routes, all behind a real
// request guard whose session arm reads the rig's own estate — the node this
// package serves on, in miniature.
func guarded(t *testing.T, r *signInRig) http.Handler {
	t.Helper()
	b := bootstrapFor(t)
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: fixtureSigner(t), Directory: r.estate, Applier: r.estate,
		Chart: seatless{}, External: b.API.ExternalBase(), Audit: r.audit,
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("build the session arm: %v", err)
	}
	guard := auth.New(&b).WithSessions(arm)
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	for _, route := range []string{"GET /iam/people", "POST /work/items"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, _ *http.Request) {
			httpjson.Write(w, http.StatusOK, map[string]string{"reached": "yes"})
		})
	}
	return guard.Middleware(mux)
}

// send is one request through h, presenting cookie where there is one, and
// answers the response with the session cookie it set, if any.
func send(t *testing.T, h http.Handler, method, path, body string,
	cookie string) (*httptest.ResponseRecorder, string) {

	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:4711"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if slices.Contains(session.CookieNames, c.Name) && c.Value != "" {
			return rec, c.Value
		}
	}
	return rec, ""
}

// statusOf is the `status` an answer carries.
func statusOf(rec *httptest.ResponseRecorder) any {
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body["status"]
}

// A REQUIRED SECOND FACTOR IS ENROLLED BEFORE ANYTHING ELSE.
//
// `api.auth.local.totp: required` promised that nobody acts on a password
// alone, and nothing read it: a person holding only a password signed in and
// reached every route. Now their sign-in succeeds into a session that says so
// (`status: second_factor_enrolment_required`), which the guard refuses /iam
// and /work to; `GET /auth/session` says the same; enrolling an authenticator
// through that very session REPLACES it with a whole one — the restricted
// session ended, the new cookie on the enrolment's answer — and the whole one
// reaches both routes, while the restricted cookie reaches nothing again.
//
// And the replacement keeps the restricted session's PROOF: the enrolment's
// code proves possession of a seed that session was handed a moment earlier,
// not who holds it, so dating the replacement at the enrolment handed whoever
// held the restricted cookie a fresh sensitive window.
//
// The CONTROL is the same person under `totp: optional`, whose sign-in is a
// whole session from the start. Mutation: drop the mark at the sign-in, or the
// guard's check, and the restricted cookie reaches /iam; replace nothing at
// the enrolment and the old cookie still works; date the replacement at the
// enrolment and its proof is five minutes younger than the password's.
func TestARequiredSecondFactorIsEnrolledBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		factor     iam.SecondFactor
		restricted bool
	}{
		{"required", iam.SecondFactorRequired, true},
		{"optional (the control)", iam.SecondFactorOptional, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// THE SURFACE'S CLOCK MOVES between the sign-in and the
			// enrolment, so a replacement dated at either instant says
			// which.
			wall := &ticking{at: clock}
			r := newSignInRigWith(t, func(o *authapi.Options) {
				requiring(o, tc.factor)
				// THE SURFACE READS THE SAME ROWS THE GUARD DOES, as a
				// node's two readers of one estate do.
				o.Sessions = o.Writer.(*estate)
				o.Now = wall.now
			})
			passwordOnly(r.estate)
			h := guarded(t, r)

			login, _ := json.Marshal(map[string]string{
				"login": "jane.doe", "password": password,
			})
			signedIn, first := send(t, h, http.MethodPost, auth.PathAuthLogin,
				string(login), "")
			if signedIn.Code != http.StatusOK || first == "" {
				t.Fatalf("the sign-in answered %d (%s)", signedIn.Code, signedIn.Body)
			}
			want := "signed_in"
			if tc.restricted {
				want = string(httpjson.CodeSecondFactorEnrolmentRequired)
			}
			if got := statusOf(signedIn); got != want {
				t.Errorf("the sign-in answered status %v, want %s", got, want)
			}
			reached, _ := send(t, h, http.MethodGet, auth.PathAuthSession, "", first)
			if got := statusOf(reached); got != want {
				t.Errorf("GET /auth/session answered status %v, want %s", got, want)
			}
			for _, route := range [][2]string{
				{http.MethodGet, "/iam/people"}, {http.MethodPost, "/work/items"},
			} {
				rec, _ := send(t, h, route[0], route[1], "{}", first)
				if tc.restricted && rec.Code != http.StatusForbidden {
					t.Errorf("an enrolment-only session reached %s %s (%d)",
						route[0], route[1], rec.Code)
				}
				if !tc.restricted && rec.Code != http.StatusOK {
					t.Errorf("a whole session was refused %s %s (%d: %s)",
						route[0], route[1], rec.Code, rec.Body)
				}
			}
			if !tc.restricted {
				return
			}

			// THE ENROLMENT, both legs, through the restricted session —
			// five minutes on, well inside the sensitive window the
			// password's proof opened.
			wall.advance(5 * time.Minute)
			offered, _ := send(t, h, http.MethodPost, auth.PathAuthTOTP, "{}", first)
			var seed struct{ Secret string }
			if err := json.Unmarshal(offered.Body.Bytes(), &seed); err != nil ||
				offered.Code != http.StatusOK || seed.Secret == "" {
				t.Fatalf("the first leg answered %d (%s)", offered.Code, offered.Body)
			}
			code, err := credential.TOTPCode(seed.Secret, credential.TOTPStep(wall.now()))
			if err != nil {
				t.Fatal(err)
			}
			proof, _ := json.Marshal(map[string]string{"secret": seed.Secret, "code": code})
			enrolled, second := send(t, h, http.MethodPost, auth.PathAuthTOTP,
				string(proof), first)
			if enrolled.Code != http.StatusOK || second == "" || second == first {
				t.Fatalf("the enrolment answered %d with cookie %t (%s), want a "+
					"replacement session", enrolled.Code, second != "", enrolled.Body)
			}
			var answer struct {
				Status  string
				Session struct{ Status string }
			}
			_ = json.Unmarshal(enrolled.Body.Bytes(), &answer)
			if answer.Status != "enrolled" || answer.Session.Status != "signed_in" {
				t.Errorf("the enrolment answered %s, want enrolled with a signed-in "+
					"session beside it", enrolled.Body)
			}
			r.estate.mu.Lock()
			starts := slices.Clone(r.estate.starts)
			r.estate.mu.Unlock()
			if len(starts) != 2 {
				t.Fatalf("opened %d sessions, want the sign-in's and its replacement",
					len(starts))
			}
			if !starts[1].ProvedAt.Equal(starts[0].ProvedAt) {
				t.Errorf("the replacement was proved at %s, want the restricted "+
					"session's %s — the enrolment's code proves a seed, not a person",
					starts[1].ProvedAt, starts[0].ProvedAt)
			}
			for _, route := range [][2]string{
				{http.MethodGet, "/iam/people"}, {http.MethodPost, "/work/items"},
			} {
				if rec, _ := send(t, h, route[0], route[1], "{}", second); rec.Code != http.StatusOK {
					t.Errorf("the session the enrolment opened was refused %s %s "+
						"(%d: %s)", route[0], route[1], rec.Code, rec.Body)
				}
				if rec, _ := send(t, h, route[0], route[1], "{}", first); rec.Code != http.StatusUnauthorized {
					t.Errorf("the restricted session still answers %s %s (%d) "+
						"after the enrolment replaced it", route[0], route[1], rec.Code)
				}
			}
		})
	}
}

// ONLY A SIGN-IN THAT PROVED A PASSWORD AND NOTHING ELSE IS RESTRICTED.
//
// Every way this surface opens a session on a password — the sign-in, an
// invitation's redemption, and a password step-up — opens one that
// may only enrol where a second factor is required and none was proved, and
// SAYS so in its answer's `status`, which is how a client learns to render the
// enrolment rather than a broken first screen. The step-up matters most: it is
// one of the routes an enrolment-only session reaches, and it opens a
// replacement — which, left whole, would turn a password alone into a whole
// session in one request. A sign-in that PROVED a factor is whole. Mutation:
// restrict on the deployment's setting alone and the second-factor row is
// restricted; skip any of the three password routes — `if how.stepUp { return
// false }` in enrolmentOnly included — and its row is whole; answer
// `signed_in` whatever was opened and the restricted rows' statuses are wrong.
func TestOnlyASignInThatProvedAPasswordAloneIsRestricted(t *testing.T) {
	t.Parallel()
	required := func(o *authapi.Options) { requiring(o, iam.SecondFactorRequired) }
	for _, tc := range []struct {
		name string
		// opened is every session the gesture opened and the `status`
		// its answer carried.
		opened     func(t *testing.T) ([]iamdomain.SessionStart, any)
		restricted bool
	}{
		{"a password sign-in", func(t *testing.T) ([]iamdomain.SessionStart, any) {
			r := newSignInRigWith(t, required)
			passwordOnly(r.estate)
			rec := r.signIn(t, "jane.doe", password, "")
			return r.estate.starts, statusOf(rec)
		}, true},
		{"a sign-in that proved an app code", func(t *testing.T) ([]iamdomain.SessionStart, any) {
			r := newSignInRigWith(t, required)
			rec := r.signIn(t, "jane.doe", password, appCode(t, clock))
			return r.estate.starts, statusOf(rec)
		}, false},
		{"a password step-up from an enrolment-only session",
			func(t *testing.T) ([]iamdomain.SessionStart, any) {
				r := newStepUpRigWith(t, session.RowValid, required, true)
				passwordOnly(r.estate)
				rec := r.stepUp(t)
				return r.estate.starts, statusOf(rec)
			}, true},
		{"an invitation's redemption", func(t *testing.T) ([]iamdomain.SessionStart, any) {
			writer := &recordingWriter{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				required(o)
				o.Directory = sealedInvitation{}
				o.Sealer = stubSealer{address: "dana@example.com"}
				o.Writer = writer
			}).Routes(mux)
			rec := postJSON(t, mux, "/auth/invite/"+invitationID, map[string]string{
				"secret": invitationSecret, "login": "dana.sre", "name": "Dana",
				"password": "a-perfectly-fine-passphrase"})
			return writer.opened(), statusOf(rec)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			starts, status := tc.opened(t)
			if len(starts) != 1 {
				t.Fatalf("opened %d sessions, want 1", len(starts))
			}
			if got := starts[0].EnrolmentOnly; got != tc.restricted {
				t.Errorf("the session opened enrolment-only %v, want %v", got,
					tc.restricted)
			}
			want := "signed_in"
			if tc.restricted {
				want = string(httpjson.CodeSecondFactorEnrolmentRequired)
			}
			if status != want {
				t.Errorf("the answer's status is %v, want %s", status, want)
			}
		})
	}
}

// A RESTRICTED SESSION IS RESTRICTED BEFORE ANY NODE HAS APPLIED IT.
//
// Every sign-in answers before any node applies the session it opened
// (`NoWait`), and a node below the bearer's start position serves reads on the
// bearer alone. The restriction lived only on the session's row, so on that
// node — the one that answered the sign-in included — the first requests after
// a password sign-in were served whole, and `GET /auth/session` said
// `signed_in`. The sign-in mints the restriction into the bearer now, and both
// the guard and the session route read it there.
//
// The CONTROL is the same sign-in under `totp: optional`, whose session is
// whole on the same node. Mutation: mint the bearer without the mark and the
// restricted row reaches /iam and reports itself signed in.
func TestARestrictedSessionIsRestrictedBeforeAnyNodeHasAppliedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		factor     iam.SecondFactor
		restricted bool
	}{
		{"required", iam.SecondFactorRequired, true},
		{"optional (the control)", iam.SecondFactorOptional, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRigWith(t, func(o *authapi.Options) {
				requiring(o, tc.factor)
				o.Sessions = o.Writer.(*estate)
			})
			passwordOnly(r.estate)
			r.estate.unapplied = true
			h := guarded(t, r)

			login, _ := json.Marshal(map[string]string{
				"login": "jane.doe", "password": password,
			})
			signedIn, cookie := send(t, h, http.MethodPost, auth.PathAuthLogin,
				string(login), "")
			if signedIn.Code != http.StatusOK || cookie == "" {
				t.Fatalf("the sign-in answered %d (%s)", signedIn.Code, signedIn.Body)
			}
			want := "signed_in"
			if tc.restricted {
				want = string(httpjson.CodeSecondFactorEnrolmentRequired)
			}
			reached, _ := send(t, h, http.MethodGet, auth.PathAuthSession, "", cookie)
			if reached.Code != http.StatusOK || statusOf(reached) != want {
				t.Errorf("GET /auth/session on a node that has not applied the "+
					"session answered %d status %v, want 200 %s", reached.Code,
					statusOf(reached), want)
			}
			rec, _ := send(t, h, http.MethodGet, "/iam/people", "", cookie)
			if tc.restricted && rec.Code != http.StatusForbidden {
				t.Errorf("a restricted session reached GET /iam/people on a node "+
					"that has not applied it (%d)", rec.Code)
			}
			if !tc.restricted && rec.Code != http.StatusOK {
				t.Errorf("a whole session was refused GET /iam/people (%d: %s)",
					rec.Code, rec.Body)
			}
		})
	}
}

// A PASSWORD NEVER ENROLS OVER A SECOND FACTOR.
//
// An enrolment-only session is proved by a password alone, and its proof is
// fresh for the sensitive window, so it satisfied everything an enrolment asks
// of a whole session. Somebody who knew the password of a person holding no
// factor could sign in restricted, wait for that person to enrol their own
// authenticator, and then enrol THEIRS through the still-live restricted
// session: the owner's factor replaced, the owner locked out, and a whole
// session handed over for it. Once the person holds a factor, a session that
// proved only a password is refused — decided in the snapshot the factor would
// land on — and the stored factor is untouched.
//
// The CONTROL is a whole session, proved with the factor, replacing its own
// factor: that is what enrolling one again is for, so the refusal is the
// restriction's and not the factor's. Mutation: drop the check from the
// enrolment's Apply and the first row enrols the stranger's seed and opens a
// whole session.
func TestAPasswordNeverEnrolsOverASecondFactor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restricted bool
	}{
		{"an enrolment-only session, a factor enrolled since it opened", true},
		{"a whole session replacing its own factor (the control)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRigWith(t, func(o *authapi.Options) {
				requiring(o, iam.SecondFactorRequired)
				o.Sessions = o.Writer.(*estate)
			})
			code := appCode(t, clock)
			if tc.restricted {
				passwordOnly(r.estate)
				code = ""
			}
			h := guarded(t, r)
			login, _ := json.Marshal(map[string]string{
				"login": "jane.doe", "password": password, "code": code,
			})
			signedIn, cookie := send(t, h, http.MethodPost, auth.PathAuthLogin,
				string(login), "")
			if signedIn.Code != http.StatusOK || cookie == "" {
				t.Fatalf("the sign-in answered %d (%s)", signedIn.Code, signedIn.Body)
			}
			if tc.restricted {
				// THE OWNER ENROLS THEIR OWN, from somewhere else, while
				// the restricted session is still live.
				r.estate.mu.Lock()
				r.estate.person.Credentials = append(r.estate.person.Credentials,
					iamdomain.Credential{ID: "owners-app", Method: iamdomain.MethodTOTP,
						Verifier: sealedSeed(t, r.estate.person.ID, "owners-app", totpSeed)})
				r.estate.mu.Unlock()
			}
			r.estate.mu.Lock()
			before := slices.Clone(r.estate.person.Credentials)
			opened := len(r.estate.starts)
			r.estate.mu.Unlock()

			// THE STRANGER'S AUTHENTICATOR, both legs.
			offered, _ := send(t, h, http.MethodPost, auth.PathAuthTOTP, "{}", cookie)
			var seed struct{ Secret string }
			if err := json.Unmarshal(offered.Body.Bytes(), &seed); err != nil ||
				offered.Code != http.StatusOK || seed.Secret == "" {
				t.Fatalf("the first leg answered %d (%s)", offered.Code, offered.Body)
			}
			theirs, err := credential.TOTPCode(seed.Secret, credential.TOTPStep(clock))
			if err != nil {
				t.Fatal(err)
			}
			proof, _ := json.Marshal(map[string]string{"secret": seed.Secret, "code": theirs})
			enrolled, replacement := send(t, h, http.MethodPost, auth.PathAuthTOTP,
				string(proof), cookie)

			r.estate.mu.Lock()
			after := slices.Clone(r.estate.person.Credentials)
			starts := len(r.estate.starts)
			r.estate.mu.Unlock()
			if !tc.restricted {
				if enrolled.Code != http.StatusOK || slices.EqualFunc(before, after,
					func(a, b iamdomain.Credential) bool { return a.ID == b.ID }) {
					t.Errorf("a whole session could not replace its own factor: "+
						"%d (%s)", enrolled.Code, enrolled.Body)
				}
				return
			}
			var refusal map[string]any
			_ = json.Unmarshal(enrolled.Body.Bytes(), &refusal)
			if enrolled.Code != http.StatusForbidden ||
				refusal["error"] != string(httpjson.CodeSecondFactorRequired) {
				t.Errorf("a password-only session enrolling over the owner's "+
					"factor answered %d %v, want 403 %s", enrolled.Code, refusal,
					httpjson.CodeSecondFactorRequired)
			}
			if !slices.EqualFunc(before, after, func(a, b iamdomain.Credential) bool {
				return a.ID == b.ID && a.Verifier == b.Verifier
			}) {
				t.Errorf("the stored credentials moved from %+v to %+v", before, after)
			}
			if starts != opened || replacement != "" {
				t.Errorf("opened %d sessions and set cookie %t for a refused "+
					"enrolment", starts-opened, replacement != "")
			}
		})
	}
}

// ticking is a clock a case moves.
type ticking struct {
	mu sync.Mutex
	at time.Time
}

func (c *ticking) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *ticking) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}
