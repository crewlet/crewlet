package authapi_test

import (
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
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THREE OUTCOMES STAY THREE, ON EVERY WRITE THIS SURFACE MAKES.
//
// The identity writer used to answer a bare position, and an `unknown`
// outcome — nothing can establish whether the record is on the log — came
// back as a zero position beside a nil error, which every caller read as
// success. So a second factor whose spend may never have landed opened a
// session on a code that still worked, a session start nobody could confirm
// minted a cookie carrying position zero, and every event this surface
// announces was said of writes that may not exist.
//
// Each case below makes ONE write answer unknown and holds the surface to the
// same three things: a 503 with the Retry-After an unknown outcome carries —
// its retry under the same id is the point — and the operation id; nothing built on the write (no session, no cookie, no codes);
// and nothing announced about it.
//
// Mutation: treat an unknown outcome as landed at any one site and its case
// goes red on the status, the session, or the event.
func TestAnUnknownOutcomeIsNeverBuiltOnOrAnnounced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write string
		do    func(t *testing.T, r *signInRig) *httptest.ResponseRecorder
	}{{
		name: "a second factor's spend", write: "SetCredentials",
		do: func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
			return r.signIn(t, "jane.doe", password, appCode(t, clock))
		},
	}, {
		name: "a session's start", write: "OpenSession",
		do: func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
			return r.signIn(t, "jane.doe", password, appCode(t, clock))
		},
	}, {
		name: "signing out everywhere", write: "Revoke",
		do: func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
			return r.asPerson(http.MethodPost, "/auth/logout/all", "")
		},
	}, {
		name: "ending one named session", write: "CloseSession",
		do: func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
			r.estate.owner = r.estate.person.ID
			return r.asPerson(http.MethodPost,
				"/auth/logout/0192f00d-0000-7000-8000-0000000000bb", "")
		},
	}, {
		name: "enrolling an authenticator app", write: "SetCredentials",
		do: func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
			secret, err := credential.NewTOTPSecret()
			if err != nil {
				t.Fatal(err)
			}
			code, err := credential.TOTPCode(secret, credential.TOTPStep(clock))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"secret": secret, "code": code})
			return r.asPerson(http.MethodPost, "/auth/totp", string(body))
		},
	}, {
		name: "regenerating the recovery codes", write: "SetCredentials",
		do: func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
			return r.asPerson(http.MethodPost, "/auth/totp/recovery", "")
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRig(t)
			r.estate.unresolved = map[string]bool{tc.write: true}
			rec := tc.do(t, r)
			if rec.Code != http.StatusServiceUnavailable ||
				rec.Header().Get("Retry-After") != "2" {
				t.Fatalf("answered %d with Retry-After %q (%s), want 503 carrying 2",
					rec.Code, rec.Header().Get("Retry-After"), rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["error"] != string(httpjson.CodeUnavailable) || body["op_id"] == "" ||
				body["op_id"] == nil || body["outcome"] != httpjson.OutcomeUnknown {
				t.Errorf("body = %v, want `unavailable` naming the operation id "+
					"and saying its outcome is unknown", body)
			}
			if _, codes := body["codes"]; codes {
				t.Error("recovery codes nothing can say are stored were handed out")
			}
			if emitted, _ := r.audit.snapshot(); len(emitted) != 0 {
				t.Errorf("announced %v about a write whose outcome is unknown", emitted)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == session.CookieBaseName || c.Name == session.HostCookieName {
					if c.MaxAge >= 0 && c.Value != "" {
						t.Errorf("a session cookie was minted on a write nobody "+
							"can confirm: %s", c.Name)
					}
				}
			}
			if tc.write == "SetCredentials" && strings.HasPrefix(tc.name, "a second") {
				r.estate.mu.Lock()
				opened := len(r.estate.starts)
				r.estate.mu.Unlock()
				if opened != 0 {
					t.Errorf("a session was opened on a spend nobody can confirm")
				}
			}
		})
	}
}

// AN ENROLMENT NOBODY CAN CONFIRM IS NOT A PERSON: NOTHING BUILT ON IT.
//
// The enrolment this surface performs is a sequence — enrol, then spend the
// invitation that authorised it, then sign the new person in — and each step
// after the first was built on an enrolment answered `unknown`. The op id is
// derived from what the caller presented (the invitation), so the answer
// names the id the retry lands under by construction.
//
// Mutation: drop the route's outcome check and it spends the link and
// answers 200.
func TestAnEnrolmentNobodyCanConfirmBuildsNothing(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, rec *httptest.ResponseRecorder, writer *recordingWriter,
		wantOp string) {

		t.Helper()
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("answered %d with Retry-After %q (%s)", rec.Code,
				rec.Header().Get("Retry-After"), rec.Body)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["op_id"] != wantOp {
			t.Errorf("op id %v, want %q — the id a retry of this very "+
				"credential lands under", body["op_id"], wantOp)
		}
		if len(writer.spent) != 0 {
			t.Errorf("spent a credential on an enrolment nobody can confirm: "+
				"%+v", writer.spent)
		}
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge >= 0 && c.Value != "" {
				t.Errorf("minted cookie %s on an unconfirmed enrolment", c.Name)
			}
		}
	}

	t.Run("an invitation", func(t *testing.T) {
		t.Parallel()
		writer := &recordingWriter{unresolved: true}
		mux := http.NewServeMux()
		buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
			o.Directory = liveInvitation{}
			o.Writer = writer
		}).Routes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/auth/invite/"+invitationID, strings.NewReader(
				`{"secret":"`+invitationSecret+`","login":"dana.sre","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
		// DERIVED FROM THE INVITATION, AT ITS INSTANT: the op id a retry
		// re-derives, in the grammar the ledger vouches for a retry by.
		if len(writer.enrolled) != 1 {
			t.Fatalf("enrolments %d, want 1", len(writer.enrolled))
		}
		op := writer.enrolled[0].OpID
		issued, _ := statelog.OpMintedAt(invitationID)
		if at, ok := statelog.OpMintedAt(op); !ok || !at.Equal(issued) {
			t.Errorf("the redemption ran under %q, which does not carry the "+
				"invitation's instant %s", op, issued)
		}
		check(t, rec, writer, op)
	})
}

// asPerson posts one request as the rig's person, signed in with a fresh
// step-up — inside the window, as a proof taken this instant is —
// straight to the surface.
func (r *signInRig) asPerson(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:4711"
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID:    uuid.MustParse(r.estate.person.ID),
		Login: r.estate.person.Login, Kind: iam.KindPerson,
		Stage: iam.StageActive, ReauthAt: clock.Add(time.Hour),
	}))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}
