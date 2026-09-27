package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	for _, start := range e.starts {
		if start.Lineage != lineage {
			continue
		}
		identity.Session = session.LineageRow{Found: true, Epoch: e.counters.Epoch,
			ProvedAt: start.ProvedAt, GroupGrants: start.GroupGrants,
			EnrolmentOnly: start.EnrolmentOnly,
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

func (seatless) Position(context.Context) (uint64, time.Duration, error) {
	return 1, 0, nil
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
// The CONTROL is the same person under `totp: optional`, whose sign-in is a
// whole session from the start. Mutation: drop the mark at the sign-in, or the
// guard's check, and the restricted cookie reaches /iam; replace nothing at
// the enrolment and the old cookie still works.
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
			r := newSignInRigWith(t, func(o *authapi.Options) {
				requiring(o, tc.factor)
				// THE SURFACE READS THE SAME ROWS THE GUARD DOES, as a
				// node's two readers of one estate do.
				o.Sessions = o.Writer.(*estate)
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

			// THE ENROLMENT, both legs, through the restricted session.
			offered, _ := send(t, h, http.MethodPost, auth.PathAuthTOTP, "{}", first)
			var seed struct{ Secret string }
			if err := json.Unmarshal(offered.Body.Bytes(), &seed); err != nil ||
				offered.Code != http.StatusOK || seed.Secret == "" {
				t.Fatalf("the first leg answered %d (%s)", offered.Code, offered.Body)
			}
			code, err := credential.TOTPCode(seed.Secret, credential.TOTPStep(clock))
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
// Every way this surface opens a session on a password — the sign-in, the
// founding, an invitation's redemption — opens one that may only enrol where a
// second factor is required and none was proved. A sign-in that PROVED one is
// whole, and so is a provider's, whose second factor is the provider's own and
// invisible here — asking for one on top would be a factor on top of a factor
// the engine cannot see. Mutation: restrict on the deployment's setting alone
// and the second-factor and provider rows are restricted; skip any of the
// three password routes and its row is whole.
func TestOnlyASignInThatProvedAPasswordAloneIsRestricted(t *testing.T) {
	t.Parallel()
	required := func(o *authapi.Options) { requiring(o, iam.SecondFactorRequired) }
	for _, tc := range []struct {
		name       string
		opened     func(t *testing.T) []iamdomain.SessionStart
		restricted bool
	}{
		{"a password sign-in", func(t *testing.T) []iamdomain.SessionStart {
			r := newSignInRigWith(t, required)
			passwordOnly(r.estate)
			r.login(t, "jane.doe", password, "")
			return r.estate.starts
		}, true},
		{"a sign-in that proved an app code", func(t *testing.T) []iamdomain.SessionStart {
			r := newSignInRigWith(t, required)
			r.login(t, "jane.doe", password, appCode(t, clock))
			return r.estate.starts
		}, false},
		{"the founding", func(t *testing.T) []iamdomain.SessionStart {
			writer := &recordingWriter{}
			mux := bootstrapSurfaceWith(t, []iamdomain.BootstrapCode{{
				ID: codeID(), MintedBy: "node-a", ExpiresAt: clock.Add(time.Hour),
				MintedAt: clock.Add(-time.Minute),
			}}, writer, true, required)
			bootstrapOnce(t, mux, "founder.one")
			return writer.opened()
		}, true},
		{"an invitation's redemption", func(t *testing.T) []iamdomain.SessionStart {
			writer := &recordingWriter{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
				required(o)
				o.Directory = sealedInvitation{}
				o.Sealer = stubSealer{address: "dana@example.com"}
				o.Writer = writer
			}).Routes(mux)
			postJSON(t, mux, "/auth/invite/"+invitationID, map[string]string{
				"secret": invitationSecret, "login": "dana.sre", "name": "Dana",
				"password": "a-perfectly-fine-passphrase"})
			return writer.opened()
		}, true},
		{"a provider sign-in", func(t *testing.T) []iamdomain.SessionStart {
			idp := newProvider(t)
			b := bootstrapFor(t)
			b.API.Auth.Backend = config.AuthBackendOIDC
			b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
			recorder := &sessionRecorder{}
			signInThroughProvider(t, idp, b, func(o *authapi.Options) {
				required(o)
				o.Writer = recorder
			})
			return recorder.opened()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			starts := tc.opened(t)
			if len(starts) != 1 {
				t.Fatalf("opened %d sessions, want 1", len(starts))
			}
			if got := starts[0].EnrolmentOnly; got != tc.restricted {
				t.Errorf("the session opened enrolment-only %v, want %v", got,
					tc.restricted)
			}
		})
	}
}
