package authapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY IDENTITY WRITE THIS SURFACE MAKES CARRIES AN INSTANT ITS LEDGER CAN
// VOUCH FOR.
//
// The publisher vouches for a retry by the instant its operation id carries,
// against the point this node's operation ledger may have lost rows from — and
// a session subject's rows go after an hour ([iamdomain.SessionOpsRetention]).
// Every id here was `session:<lineage>`, `logout:<lineage>`, `totp:<person>:…`
// and the like, which carry no instant and read as minted at the epoch: within
// an hour of the first sweep every sign-in, sign-out and factor change was
// answered `unknown` without being published. And a close derived from the
// session it closes carries the instant that session BEGAN — days ago for a
// week-long one — which the hour's sweep has already passed; so each close is
// held to an instant no earlier than the request's own.
//
// Mutation: derive any of these from the lineage it names, or from a prefix,
// and its row goes red.
func TestEveryIdentityWriteHereCarriesAnInstantItsLedgerCanVouchFor(t *testing.T) {
	t.Parallel()
	floor := clock.Add(-time.Minute)
	for _, c := range []struct {
		name string
		ops  func(t *testing.T) []string
	}{
		{"a sign-in's spend and its session", func(t *testing.T) []string {
			r := newSignInRig(t)
			if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
				t.Fatalf("the sign-in answered %d", got)
			}
			return append(slices.Clone(r.estate.credentialOps), startOps(r.estate)...)
		}},
		{"a step-up's close and its replacement", func(t *testing.T) []string {
			r := newStepUpRig(t, session.RowValid)
			if rec := r.stepUp(t); rec.Code != http.StatusOK {
				t.Fatalf("the step-up answered %d (%s)", rec.Code, rec.Body)
			}
			return append(closeOps(r.estate), startOps(r.estate)...)
		}},
		{"a sign-out", func(t *testing.T) []string {
			r := newStepUpRig(t, session.RowValid)
			req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
			req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: r.cookie})
			rec := httptest.NewRecorder()
			mux := http.NewServeMux()
			r.svc.Routes(mux)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("the sign-out answered %d (%s)", rec.Code, rec.Body)
			}
			return closeOps(r.estate)
		}},
		{"ending one named session", func(t *testing.T) []string {
			r := newSignInRig(t)
			r.estate.owner = r.estate.person.ID
			if rec := r.asPerson(http.MethodPost,
				"/auth/logout/0192f00d-0000-7000-8000-0000000000bb", ""); rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s)", rec.Code, rec.Body)
			}
			return closeOps(r.estate)
		}},
		{"signing out everywhere", func(t *testing.T) []string {
			r := newSignInRig(t)
			if rec := r.asPerson(http.MethodPost, "/auth/logout/all", ""); rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s)", rec.Code, rec.Body)
			}
			return slices.Clone(r.estate.revokeOps)
		}},
		{"enrolling an authenticator app", func(t *testing.T) []string {
			r := newSignInRig(t)
			if rec := enrolApp(t, r); rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s)", rec.Code, rec.Body)
			}
			return slices.Clone(r.estate.credentialOps)
		}},
		{"regenerating the recovery codes", func(t *testing.T) []string {
			r := newSignInRig(t)
			if rec := r.asPerson(http.MethodPost, "/auth/totp/recovery", ""); rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s)", rec.Code, rec.Body)
			}
			return slices.Clone(r.estate.credentialOps)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ops := c.ops(t)
			if len(ops) == 0 {
				t.Fatal("no write was asked for; this case tests nothing")
			}
			for _, op := range ops {
				if err := statelog.CheckCallerOpID(op); err != nil {
					t.Errorf("published under %q, outside the grammar: %v", op, err)
					continue
				}
				if at, _ := statelog.OpMintedAt(op); at.Before(floor) {
					t.Errorf("published under %q, dated %s — before the request "+
						"that made it, which an hour's sweep has already passed",
						op, at)
				}
			}
		})
	}
}

// startOps and closeOps are the operation ids an estate's session starts and
// closes were asked under.
func startOps(e *estate) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.starts))
	for _, s := range e.starts {
		out = append(out, s.OpID)
	}
	return out
}

func closeOps(e *estate) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.closes))
	for _, c := range e.closes {
		out = append(out, c.opID)
	}
	return out
}

// enrolApp posts one authenticator enrolment, with a fresh seed and its code,
// as the rig's person.
func enrolApp(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
	t.Helper()
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
}

// A WRITE THAT LANDED AS A COPY THIS CALL CANNOT PROVE IS ITS OWN IS NOT BUILT
// ON.
//
// The framework answers a retry of an operation that had already landed from
// its ledger, before this call's decide runs ([statelog.Result.Collapsed]) —
// so what the decide computed describes a decision nothing published: a second
// factor's verdict, a factor or a set of recovery codes this call formed, the
// counters a session's bearer carries. Each is answered as the unknown it is
// to this call: 503 with the operation, no session, no cookie, no codes, and
// nothing announced.
//
// Mutation: read any of these as landed and its row opens a session, shows the
// codes or announces the factor.
func TestACollapsedWriteIsNeverBuiltOn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, write string
		do          func(t *testing.T, r *signInRig) *httptest.ResponseRecorder
	}{
		{"a second factor's spend", "SetCredentials",
			func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.signIn(t, "jane.doe", password, appCode(t, clock))
			}},
		{"a session's start", "OpenSession",
			func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.signIn(t, "jane.doe", password, appCode(t, clock))
			}},
		{"an authenticator's enrolment", "SetCredentials", enrolApp},
		{"a set of recovery codes", "SetCredentials",
			func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.asPerson(http.MethodPost, "/auth/totp/recovery", "")
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRig(t)
			r.estate.collapsed = map[string]bool{c.write: true}
			rec := c.do(t, r)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusServiceUnavailable || body["op_id"] == nil ||
				body["op_id"] == "" {
				t.Fatalf("answered %d %v, want 503 naming the operation",
					rec.Code, body)
			}
			// AND IT SAYS WHAT IS SO: the write landed, so "cannot
			// establish whether it landed" — or, for a start the domain
			// refused as a copy, "could not make that change" — would
			// send a client to find out what it could have been told.
			if detail, _ := body["detail"].(string); !strings.Contains(detail,
				"landed as a copy") {
				t.Errorf("the answer says %q, not that the change landed as a "+
					"copy this call cannot prove is its own", detail)
			}
			if _, codes := body["codes"]; codes {
				t.Error("recovery codes nothing proves are stored were handed out")
			}
			for _, cookie := range rec.Result().Cookies() {
				if cookie.MaxAge >= 0 && cookie.Value != "" {
					t.Errorf("a session cookie %s was minted on a collapsed write",
						cookie.Name)
				}
			}
			if emitted, _ := r.audit.snapshot(); len(emitted) != 0 {
				t.Errorf("announced %v about a write this call cannot prove "+
					"is its own", emitted)
			}
			if c.name == "a second factor's spend" && len(startOps(r.estate)) != 0 {
				t.Error("a session was opened on a spend whose verdict is " +
					"another round's")
			}
		})
	}
}

// AN UNKNOWN THIS NODE'S LEDGER CANNOT VOUCH FOR SENDS THE CALLER ELSEWHERE.
//
// Asked here again it answers the same way until the write reaches this node,
// so it carries no Retry-After — which a client obeys by hammering this node —
// and says `unvouched`. The control is a lost acknowledgement, which the same
// request here settles, and which says when.
//
// Mutation: drop the unvouched arm and the first row carries a Retry-After.
func TestAnUnvouchedUnknownSendsTheCallerElsewhere(t *testing.T) {
	t.Parallel()
	for _, unvouched := range []bool{true, false} {
		r := newSignInRig(t)
		r.estate.unresolved = map[string]bool{"Revoke": true}
		r.estate.unvouched = unvouched
		rec := r.asPerson(http.MethodPost, "/auth/logout/all", "")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		retry := rec.Header().Get("Retry-After")
		switch {
		case rec.Code != http.StatusServiceUnavailable || body["op_id"] == nil:
			t.Errorf("unvouched %v: answered %d %v, want 503 naming the "+
				"operation", unvouched, rec.Code, body)
		case unvouched && (retry != "" || body["unvouched"] != true):
			t.Errorf("an unvouched unknown answered Retry-After %q, unvouched "+
				"%v", retry, body["unvouched"])
		case !unvouched && (retry != "2" || body["unvouched"] != nil):
			t.Errorf("a lost acknowledgement answered Retry-After %q, "+
				"unvouched %v", retry, body["unvouched"])
		}
	}
}

// A WRITE NAMING A PERSON THE DIRECTORY REMOVED IS NEVER A 503.
//
// The domain refuses a record about a removed person ([statelog.ReasonDeleted])
// — a removal that landed between the read a gesture was decided on and its
// record — and nothing will ever write them again. Answered as the generic
// unavailability it was, a client was told to come back for a person who no
// longer exists. Each way in answers what it already answers for somebody who
// is not there: the sign-in's generic refusal, a session that has ended, a
// session already ended, a link that has stopped working.
//
// Mutation: drop any route's removal arm and its row answers 503.
func TestAWriteNamingARemovedPersonIsNeverA503(t *testing.T) {
	t.Parallel()
	gone := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "that person was removed"}
	for _, c := range []struct {
		name, write string
		do          func(t *testing.T, r *signInRig) *httptest.ResponseRecorder
		status      int
		code        httpjson.Code
	}{
		{"a sign-in's session", "OpenSession",
			func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.signIn(t, "jane.doe", password, appCode(t, clock))
			}, http.StatusUnauthorized, httpjson.CodeSignInRefused},
		{"a sign-in's spend", "SetCredentials",
			func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.signIn(t, "jane.doe", password, appCode(t, clock))
			}, http.StatusUnauthorized, httpjson.CodeSignInRefused},
		{"signing out everywhere", "Revoke",
			func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.asPerson(http.MethodPost, "/auth/logout/all", "")
			}, http.StatusUnauthorized, httpjson.CodeSessionRevoked},
		{"enrolling an authenticator app", "SetCredentials", enrolApp,
			http.StatusUnauthorized, httpjson.CodeSessionRevoked},
		{"ending one named session", "CloseSession",
			func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
				r.estate.owner = r.estate.person.ID
				return r.asPerson(http.MethodPost,
					"/auth/logout/0192f00d-0000-7000-8000-0000000000bb", "")
			}, http.StatusOK, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRig(t)
			r.estate.refuse = map[string]error{c.write: gone}
			rec := c.do(t, r)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.status || (c.code != "" && body["error"] != string(c.code)) {
				t.Errorf("answered %d %v, want %d %s", rec.Code, body, c.status, c.code)
			}
			// A SPEND REFUSED OVER A REMOVAL IS A FAILED SIGN-IN LIKE
			// EVERY OTHER, counted in the trail's tally through the one
			// refusal every arm takes.
			if c.name == "a sign-in's spend" {
				if _, failures := r.audit.snapshot(); len(failures) != 1 {
					t.Errorf("the refused sign-in was counted %d times, want "+
						"once: %+v", len(failures), failures)
				}
			}
		})
	}
	t.Run("a step-up's close", func(t *testing.T) {
		t.Parallel()
		r := newStepUpRig(t, session.RowValid)
		r.estate.refuse = map[string]error{"CloseSession": gone}
		rec := r.stepUp(t)
		if rec.Code != http.StatusUnauthorized ||
			!strings.Contains(rec.Body.String(), string(httpjson.CodeSessionRevoked)) {
			t.Errorf("answered %d %s, want 401 session_revoked", rec.Code, rec.Body)
		}
	})
	for name, refusal := range map[string]error{
		"an invitation's redemption":                      gone,
		"an invitation whose operation names another one": fmt.Errorf("%w: %w", iamdomain.ErrOperationReused, &statelog.Unavailable{Reason: statelog.ReasonOpReused}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &recordingWriter{refusals: []error{refusal}}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory = liveInvitation{}
				o.Writer = writer
			}).Routes(mux)
			if got := redeem(t, mux, "dana.sre"); got != http.StatusGone {
				t.Errorf("answered %d, want 410: the link has stopped working", got)
			}
		})
	}
}

// A VERIFIER ANOTHER REWRITE ALREADY MOVED IS NOT WRITTEN AGAIN.
//
// A rewrite's operation is minted when it runs, so two sign-ins presenting one
// stale verifier are two operations — where they used to share an id derived
// from the verifier, which carried no instant the ledger could vouch for. The
// second must then publish nothing: its snapshot holds the verifier the first
// wrote, and a set that changes nothing is a record nobody needs.
//
// Mutation: let the rewrite publish when it swapped nothing and the estate
// stores the set its snapshot read.
func TestAVerifierAnotherRewriteMovedIsNotWrittenAgain(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	r.estate.mu.Lock()
	r.estate.person.Credentials = r.estate.person.Credentials[:1] // a password alone
	r.estate.mu.Unlock()
	before := storeVerifierAt(t, r.estate, cheaper)
	r.estate.before = func(held []iamdomain.Credential) []iamdomain.Credential {
		out := slices.Clone(held)
		out[0].Verifier = "moved-by-another-rewrite"
		return out
	}
	if got := r.login(t, "jane.doe", password, ""); got != http.StatusOK {
		t.Fatalf("the sign-in answered %d", got)
	}
	r.svc.Stop(t.Context())
	r.estate.mu.Lock()
	defer r.estate.mu.Unlock()
	if !slices.ContainsFunc(r.estate.credentialOps, isRehash) {
		t.Fatal("no rewrite was asked for; this case tests nothing")
	}
	if got := r.estate.person.Credentials[0].Verifier; got != before {
		t.Errorf("the estate holds %q, want the %q it held: a rewrite that "+
			"swapped nothing published its snapshot's set", got, before)
	}
}
