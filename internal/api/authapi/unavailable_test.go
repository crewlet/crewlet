package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY 503 THIS SURFACE ANSWERS SAYS WHETHER TO COME BACK, BECAUSE NOTHING
// HERE SPELLS ONE ITSELF OR PICKS ITS HINT WITHOUT ITS CAUSE.
//
// A 503 that does not say whether to come back is indistinguishable to a
// client from a node that is down for good, and the sign-in surface is where a
// client most needs to tell them apart: a browser that cannot, gives up on the
// one page that would have let the person back in. Ten sites answered a bare
// 503. The rule is held structurally — no file in this package names the
// status, so the one writer that decides the header with it,
// httpjson.Unavailable, is the only way to answer one: a Retry-After where
// waiting clears the cause, and none, deliberately, where it cannot — because
// a behavioural case per site would have to provoke ten different failures and
// would still say nothing about the eleventh.
//
// AND NO FILE NAMES THE BARE HINT, auth.RetryIdentitySeconds: every site asks
// auth.RetryIdentity with what caused the 503, nil where nothing did. Every
// site took the constant, so a write the state log refused for good — too
// large, a full log, an evicted node — told a client to come back in two
// seconds; the refusal's own rule answers those with no header, which is the
// answer.
func TestEveryUnavailableAnswerHereSaysWhetherToComeBack(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "http" &&
					n.Sel.Name == "StatusServiceUnavailable" {
					t.Errorf("%s spells a 503 itself; answer it with "+
						"httpjson.Unavailable, which carries the Retry-After",
						fset.Position(n.Pos()))
				}
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "auth" &&
					n.Sel.Name == "RetryIdentitySeconds" {
					t.Errorf("%s takes the identity hint whatever caused the "+
						"503; ask auth.RetryIdentity with the cause, nil where "+
						"there is none", fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind == token.INT && n.Value == "503" {
					t.Errorf("%s spells a 503 as a literal; answer it with "+
						"httpjson.Unavailable", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	// THE CONTROL: a glob that matched nothing would pass every rule.
	if checked < 5 {
		t.Fatalf("checked %d source files, which is not this package", checked)
	}
}

// failingRevoke is a writer whose revocation cannot be recorded.
type failingRevoke struct{ stubWriter }

func (failingRevoke) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return statelog.Result{}, errors.New("the broker is unreachable")
}

// AND ONE SITE, END TO END: signing out everywhere on a node that cannot
// record it is the unknown arm, with the hint a client retries on — not a bare
// 503 a browser reads as the engine being gone.
func TestASignOutEverywhereThatCannotLandSaysWhenToRetry(t *testing.T) {
	t.Parallel()
	svc := buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Writer = failingRevoke{}
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout/all", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID:    uuid.MustParse("0192f00d-0000-7000-8000-00000000000a"),
		Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("answered %d with Retry-After %q, want 503 carrying 2",
			rec.Code, rec.Header().Get("Retry-After"))
	}
}

// refusingRevoke is a writer whose revocation the state log refused.
type refusingRevoke struct {
	stubWriter
	reason statelog.Reason
}

func (w refusingRevoke) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return statelog.Result{}, fmt.Errorf("iamdomain: publish: %w",
		&statelog.Unavailable{Reason: w.reason, Detail: "what the refusal is about"})
}

// AND A SIGN-OUT EVERYWHERE THE LOG REFUSED FOR GOOD SAYS SO BY CARRYING NO
// HINT. A full log or a record too large answers the same retry the same, and
// the identity hint told a browser to come back in two seconds for ever; the
// refusal's own rule answers it with no Retry-After, as /worklet does.
// A node that is behind is the control, and keeps the hint. Each is told in
// the state log's sentence for its reason ([statelog.ConditionSentence]),
// whose advice agrees with that hint, and never the refusal's own detail: one
// fixed sentence asked all three to be tried again in a moment.
//
// Mutation: answer the fixed sentence again and the full log's and the large
// record's rows go red.
func TestASignOutEverywhereTheLogRefusedSaysWhetherToRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason statelog.Reason
		retry  string
	}{
		{statelog.ReasonLogFull, ""},
		{statelog.ReasonRecordTooLarge, ""},
		{statelog.ReasonBehind, "2"},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			svc := buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Writer = refusingRevoke{reason: tc.reason}
			})
			mux := http.NewServeMux()
			svc.Routes(mux)
			req := httptest.NewRequest(http.MethodPost, "/auth/logout/all", nil)
			req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
				ID:    uuid.MustParse("0192f00d-0000-7000-8000-00000000000a"),
				Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
			}))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable ||
				rec.Header().Get("Retry-After") != tc.retry {
				t.Errorf("a %s refusal answered %d with Retry-After %q, want 503 "+
					"with %q", tc.reason, rec.Code, rec.Header().Get("Retry-After"),
					tc.retry)
			}
			want, _ := statelog.ConditionSentence(&statelog.Unavailable{Reason: tc.reason})
			if body := rec.Body.String(); !strings.Contains(body, want) ||
				strings.Contains(body, "what the refusal is about") {
				t.Errorf("a %s refusal answered %s, want the state log's sentence %q "+
					"and nothing of the refusal's detail", tc.reason, body, want)
			}
		})
	}
}

// racingRevoke is a writer whose revocation lost its race: the person's record
// kept changing under it until the framework gave up.
type racingRevoke struct{ stubWriter }

func (racingRevoke) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return statelog.Result{}, fmt.Errorf("iamdomain: publish: %w: "+
		"crewlet.iam.person.0192f00d-0000-7000-8000-00000000000a kept changing under "+
		"this write", statelog.ErrConflict)
}

// A WRITE THAT LOST ITS RACE IS `409 stale`, AS ON /iam AND /worklet, IN THE STATE
// LOG'S WORDS FOR A PERSON, AND NEVER THE 503 OF A NODE THAT COULD NOT.
//
// Nothing about the request or the node was wrong: the record it wrote kept
// changing under it, and the same gesture made again decides on a fresh
// snapshot and lands — which is `stale`, carrying no Retry-After since no wait
// is what clears it. This surface answered it as every other failed write,
// "This node couldn't make that change. Try again in a moment.", which sent a
// person to wait out a node with nothing wrong with it, while the API
// reference already said a lost race here is `409 stale`. The framework's own
// words name the subject the race was lost on, a broker path with the
// person's id in it, and never reach the answer.
//
// Mutation: drop the lost-race arm from failWrite and the case goes red on
// the status.
func TestASignOutEverywhereThatLostItsRaceIsStale(t *testing.T) {
	t.Parallel()
	svc := buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Writer = racingRevoke{}
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout/all", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID:    uuid.MustParse("0192f00d-0000-7000-8000-00000000000a"),
		Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
	}
	if rec.Code != http.StatusConflict || rec.Header().Get("Retry-After") != "" {
		t.Errorf("a lost race answered %d with Retry-After %q, want 409 and none",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	if body["error"] != "stale" || body["detail"] != statelog.LostRace || body["op_id"] == "" {
		t.Errorf("a lost race is told %v, want `stale` in the state log's words "+
			"with the operation it was published under", body)
	}
	if strings.Contains(rec.Body.String(), "0192f00d") ||
		strings.Contains(rec.Body.String(), "crewlet.") {
		t.Errorf("a lost race is told %s, which names the subject", rec.Body)
	}
}

// A LOST RACE IS TOLD IN THE WORDS OF WHAT WAS ASKED: AN EDIT IN THE STATE
// LOG'S, A GESTURE IN THIS SURFACE'S OWN.
//
// statelog.LostRace ends "look at it again, then make the change again if it
// still needs making", which is advice about an account somebody edits — a
// sign-out everywhere, an authenticator's enrolment. A person at the sign-in
// form, or redeeming a link, has nothing to look at and changed nothing: what
// clears it is signing in again. Every write here took the edit's words, so a
// sign-in whose session or second factor's spend lost its race told the person
// to look at something that is not there.
//
// Mutation: answer every lost race through writeFailed and the sign-in rows
// go red on the sentence.
func TestALostRaceIsToldInTheWordsOfWhatWasAsked(t *testing.T) {
	t.Parallel()
	raced := fmt.Errorf("iamdomain: publish: %w: crewlet.iam.person."+
		"0192f00d-0000-7000-8000-00000000000a kept changing under this write",
		statelog.ErrConflict)
	signIn := func(t *testing.T, r *signInRig) *httptest.ResponseRecorder {
		return r.signIn(t, "jane.doe", password, appCode(t, clock))
	}
	for _, c := range []struct {
		name, write, want string
		do                func(t *testing.T, r *signInRig) *httptest.ResponseRecorder
	}{
		{"a sign-in's session", "OpenSession", authapi.GestureRaced, signIn},
		{"a sign-in's second factor", "SetCredentials", authapi.GestureRaced, signIn},
		{"signing out everywhere", "Revoke", statelog.LostRace,
			func(_ *testing.T, r *signInRig) *httptest.ResponseRecorder {
				return r.asPerson(http.MethodPost, "/auth/logout/all", "")
			}},
		{"enrolling an authenticator app", "SetCredentials", statelog.LostRace,
			enrolApp},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRig(t)
			r.estate.refuse = map[string]error{c.write: raced}
			rec := c.do(t, r)
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
			}
			if rec.Code != http.StatusConflict || body["error"] != "stale" ||
				body["op_id"] == "" || rec.Header().Get("Retry-After") != "" {
				t.Fatalf("answered %d %v with Retry-After %q, want 409 stale "+
					"naming the operation and none", rec.Code, body,
					rec.Header().Get("Retry-After"))
			}
			if body["detail"] != c.want {
				t.Errorf("a lost race is told %q, want %q", body["detail"], c.want)
			}
			if strings.Contains(rec.Body.String(), "0192f00d") {
				t.Errorf("a lost race is told %s, which names the subject", rec.Body)
			}
		})
	}
}

// AND A TOKEN EXCHANGE WHOSE SESSION LOST ITS RACE IS A GESTURE TOO: the
// caller sent a bearer and changed nothing, so it is told to exchange again,
// never to look at something again.
//
// Mutation: answer the exchange's failure through writeFailed and the case
// goes red on the sentence.
func TestATokenExchangeThatLostItsRaceIsToldAsAGesture(t *testing.T) {
	t.Parallel()
	r := newExchangeRig(t)
	r.estate.openErr = fmt.Errorf("iamdomain: publish: %w: crewlet.iam."+
		"session kept changing under this write", statelog.ErrConflict)
	req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	req.Header.Set("Authorization", "Bearer "+opsValue)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
	}
	if rec.Code != http.StatusConflict || body["error"] != "stale" ||
		body["op_id"] == "" || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("answered %d %v with Retry-After %q, want 409 stale naming "+
			"the operation and none", rec.Code, body, rec.Header().Get("Retry-After"))
	}
	if body["detail"] != authapi.GestureRaced {
		t.Errorf("a lost race is told %q, want %q", body["detail"], authapi.GestureRaced)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" && c.MaxAge >= 0 {
			t.Errorf("a cookie %s was minted on a session that never started", c.Name)
		}
	}
}

// A NEW PASSWORD WHOSE RECORD LOST ITS RACE IS TOLD BY WHAT SET IT: a reset
// link's spend is a gesture, made again from the link, and a person changing
// their own password from their account is making an edit, told in the state
// log's words. Both are the one record (SetPassword), so only the route can
// tell them apart.
//
// Mutation: answer the reset's failure through writeFailed and its row goes
// red on the sentence; answer the change's through gestureFailed and its row
// does.
func TestANewPasswordThatLostItsRaceIsToldByWhatSetIt(t *testing.T) {
	t.Parallel()
	raced := fmt.Errorf("iamdomain: publish: %w: crewlet.iam.person."+
		"0192f00d-0000-7000-8000-00000000000a kept changing under this write",
		statelog.ErrConflict)
	for _, c := range []struct {
		name, want string
		do         func(t *testing.T, r *signInRig, h http.Handler) *httptest.ResponseRecorder
	}{
		{"a reset link's spend", authapi.GestureRaced,
			func(t *testing.T, r *signInRig, h http.Handler) *httptest.ResponseRecorder {
				id, secret := withResetLink(t, r.estate, nil)
				r.estate.mu.Lock()
				r.estate.refuse = map[string]error{"SetPassword": raced}
				r.estate.mu.Unlock()
				rec, _ := spendReset(t, h, id, secret, newPassword)
				return rec
			}},
		{"a change from the account", statelog.LostRace,
			func(t *testing.T, r *signInRig, h http.Handler) *httptest.ResponseRecorder {
				here := signedIn(t, h)
				r.estate.mu.Lock()
				r.estate.refuse = map[string]error{"SetPassword": raced}
				r.estate.mu.Unlock()
				rec, _ := send(t, h, http.MethodPost, "/auth/password",
					changeBody(password, newPassword), here)
				return rec
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, h := passwordRig(t)
			rec := c.do(t, r, h)
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
			}
			if rec.Code != http.StatusConflict || body["error"] != "stale" ||
				body["op_id"] == "" {
				t.Fatalf("answered %d %v, want 409 stale naming the operation",
					rec.Code, body)
			}
			if body["detail"] != c.want {
				t.Errorf("a lost race is told %q, want %q", body["detail"], c.want)
			}
		})
	}
}
