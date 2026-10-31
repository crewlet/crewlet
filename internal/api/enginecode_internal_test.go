package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A RUNNING ENGINE'S CODE SURFACE TELLS A BOX WHY ONLY BY THE FLEET'S KEYRING.
// [EngineCode]'s VerifyBox is the seam a deployment's box routes ask on a node
// serving none of the code host ([App.boxGitRoute], [App.boxCodeRoute]); the
// route suites drive a fake of it, so this holds the adapter itself, over a
// real engine: a git node whose company hosts no code — no code host built,
// every node such a route answers for. A coding run's token and a check's,
// each minted over the keyring the node booted on, as any other node of its
// fleet mints them, are told the reason — the company hosts no code, 404, in
// the words a person's /git/ is told — on a box's git route and on a check's
// box's read alike; the same tokens minted over another fleet's keyring, and
// each of this fleet's presented under the other's user name, are answered the
// box admission's own challenge, in its bytes.
//
// Mutations: `return !host.VerifyBox(…)` in the adapter — every unsigned
// request is told the reason, the leak M3-decisions U8 closed, and every
// signed one is challenged, red; `return false` — a signed box is challenged
// ("this run token is not valid here") where it should be told the reason it
// acts on (on a 503, retry), red; the run tokens handed for the check tokens (or the engine
// building either with the code host rather than at New) — the signed check
// token is challenged, red.
func TestTheEnginesCodeSurfaceTellsABoxWhyOnlyByTheFleetsKeyring(t *testing.T) {
	t.Parallel()
	e, boot := bootNode(t, "")
	if _, hosts := e.CodeHost(); hosts {
		t.Fatal("the premise: a company hosting no code built a code host")
	}
	if why := e.CodeUnserved(); !errors.Is(why, engine.ErrCodeNotHosted) {
		t.Fatalf("the premise: the node serves no code host because %v, want %v", why,
			engine.ErrCodeNotHosted)
	}
	surface := EngineCode(e)
	a := &App{code: surface}

	fleet := boot.Secrets.TokenMaterial()
	boxes, checks := tokensOver(t, fleet)
	otherBoxes, otherChecks := tokensOver(t, runtoken.OneKey("k", "another fleet's key"))
	run := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	check := host.CheckRunRef{Repo: "web", Attempt: code.CheckKey{RepoID: uuid.NewString(),
		OID: strings.Repeat("ab", 20), Target: "refs/heads/main", Name: "unit", Attempt: 1}}
	presented := []struct {
		name, user, token string
		signed            bool
	}{
		{"a run's token this fleet signed", host.BoxUser, boxes.Mint(run), true},
		{"a check's token this fleet signed", host.CheckUser, checks.Mint(check, time.Minute), true},
		{"a run's token another fleet signed", host.BoxUser, otherBoxes.Mint(run), false},
		{"a check's token another fleet signed", host.CheckUser, otherChecks.Mint(check, time.Minute), false},
		{"this fleet's run token as a check's", host.CheckUser, boxes.Mint(run), false},
		{"this fleet's check token as a run's", host.BoxUser, checks.Mint(check, time.Minute), false},
	}
	routes := []struct {
		name string
		h    http.Handler
		path string
	}{
		{"a box's git", a.boxGitRoute(GitRoutes.BoxInfoRefs), "/box/git/web.git/info/refs?service=git-upload-pack"},
		{"a check's box's read", a.boxCodeRoute(code.GitTree), "/box/code/web/tree"},
	}

	// WHAT A PERSON IS TOLD on /git/ by a node whose company hosts no code.
	told := httptest.NewRecorder()
	unservedGit(told, engine.ErrCodeNotHosted)
	if told.Code != http.StatusNotFound {
		t.Fatalf("the premise: a company hosting no code answers %d, want 404", told.Code)
	}
	for _, p := range presented {
		if got := surface.VerifyBox(p.user, p.token); got != p.signed {
			t.Errorf("VerifyBox of %s = %t, want %t", p.name, got, p.signed)
		}
		for _, route := range routes {
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, route.path, nil)
				r.SetBasicAuth(p.user, p.token)
				return r
			}
			want := told
			if !p.signed {
				want = httptest.NewRecorder()
				if !host.ChallengeBox(want, request(), func(string, string) bool { return false },
					slog.New(slog.DiscardHandler)) {
					t.Fatalf("%s: the box admission's credential step admits it", p.name)
				}
			}
			rec := httptest.NewRecorder()
			route.h.ServeHTTP(rec, request())
			if got, w := answerOf(rec), answerOf(want); got != w {
				t.Errorf("%s on %s answered\n  %s\nwant\n  %s", p.name, route.name, got, w)
			}
		}
	}
}

// tokensOver is a coding box's token signer and a check's box's, both over
// keyring, as every node of the fleet that holds it builds them.
func tokensOver(t *testing.T, keyring runtoken.Material) (*host.BoxTokens, *host.CheckTokens) {
	t.Helper()
	boxes, err := host.NewBoxTokens(keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := host.NewCheckTokens(keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return boxes, checks
}

// answerOf is what a client of a git route is shown: its status and the three
// headers a git route's answer can carry, then its words.
func answerOf(rec *httptest.ResponseRecorder) string {
	return fmt.Sprintf("%d [%s] [%s] [%s] %q", rec.Code, rec.Header().Get("Content-Type"),
		rec.Header().Get("WWW-Authenticate"), rec.Header().Get("Retry-After"), rec.Body.String())
}
