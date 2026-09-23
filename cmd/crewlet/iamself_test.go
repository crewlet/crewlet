package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// selfNode is a node serving the three routes `iam token -login` walks, behind
// the REAL cross-site check configured for an external URL the test server is
// not — so a cookie request that stated no origin, or the dialled one, is
// refused exactly as a deployment behind a proxy refuses it.
type selfNode struct {
	mu        sync.Mutex
	server    *httptest.Server
	wantCode  string
	mintCode  int
	steps     []string
	signIns   []map[string]string
	mintAuth  string
	mintQuery string
	mintBody  map[string]any
}

const selfCookie = "the-session-bearer"

func newSelfNode(t *testing.T) *selfNode {
	t.Helper()
	n := &selfNode{mintCode: http.StatusCreated}
	var boot config.Bootstrap
	boot.API.ExternalURL = "https://crewlet.example.com"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+auth.PathAuthLogin, func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		n.mu.Lock()
		n.steps = append(n.steps, "login")
		n.signIns = append(n.signIns, in)
		n.mu.Unlock()
		switch {
		case in["login"] != "jane.doe" || in["password"] != "a long pass phrase":
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
		case n.wantCode != "" && in["code"] == "":
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSecondFactorRequired)
		case n.wantCode != "" && in["code"] != n.wantCode:
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
		default:
			http.SetCookie(w, &http.Cookie{Name: session.CookieBaseName,
				Value: selfCookie, HttpOnly: true})
			httpjson.Write(w, http.StatusOK, map[string]string{"person": "p-jane"})
		}
	})
	signedIn := func(r *http.Request) bool {
		c, err := r.Cookie(session.CookieBaseName)
		return err == nil && c.Value == selfCookie
	}
	mux.HandleFunc("POST /iam/credentials", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.steps = append(n.steps, "mint")
		n.mintAuth, n.mintQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&n.mintBody)
		code := n.mintCode
		n.mu.Unlock()
		if !signedIn(r) {
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		if code != http.StatusCreated {
			httpjson.FailWith(w, code, httpjson.CodeUnauthorized,
				map[string]string{"detail": "not today"})
			return
		}
		httpjson.Write(w, http.StatusCreated, map[string]any{
			"id": "c-1", "person": "p-jane", "token": "cwl_pat_the-value",
			"grants": []string{"state:read"}, "colleague": "read",
			"expires_at": "2026-07-01T00:00:00Z",
		})
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if signedIn(r) {
			n.steps = append(n.steps, "logout")
		}
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
	})
	n.server = httptest.NewServer(auth.NewCSRF(&boot).Middleware(mux))
	t.Cleanup(n.server.Close)
	return n
}

// selfConfig is a Tier A config naming the same external URL the node's check
// admits, which is the origin a cookie request has to state.
func selfConfig(t *testing.T) string {
	t.Helper()
	path := bootstrapWithKeyring(t, "k1")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("api:\n  external_url: https://crewlet.example.com\n"); err != nil {
		t.Fatal(err)
	}
	return path
}

// runSelf runs `crewlet iam token -login jane.doe` with stdin piped in.
func runSelf(t *testing.T, n *selfNode, stdin string, extra ...string) (
	string, string, error) {

	t.Helper()
	args := append([]string{"token", "-login", "jane.doe", "-label", "laptop",
		"-grants", "state:read", "-config", selfConfig(t), "-api", n.server.URL},
		extra...)
	var out, errs bytes.Buffer
	err := runIAM(args, strings.NewReader(stdin), &out, &errs)
	return out.String(), errs.String(), err
}

// YOUR OWN TOKEN IS MINTED FROM YOUR OWN SESSION, and the session ends with
// the command.
//
// A person's token is theirs alone to mint, and the CLI otherwise holds only
// CREWLET_API_TOKEN — which is the deployment's (it owns no tokens) or a token
// (which may not mint). So `-login` signs in for the one request: the password
// off standard input, the mint carrying the cookie that answered and NO
// bearer, the deployment's own origin stated so the node's cross-site check
// admits a cookie write, and a sign-out after. Mutation: drop the Origin and
// the mint is refused as cross-site; drop the deferred sign-out and the node
// sees no logout.
func TestYourOwnTokenIsMintedFromYourOwnSession(t *testing.T) {
	n := newSelfNode(t)
	t.Setenv(apiTokenEnv, "")
	out, errs, err := runSelf(t, n, "a long pass phrase\n")
	if err != nil {
		t.Fatalf("iam token -login: %v\n%s", err, errs)
	}
	if strings.Join(n.steps, ",") != "login,mint,logout" {
		t.Errorf("the node saw %v, want a sign-in, the mint and a sign-out", n.steps)
	}
	if n.mintAuth != "" || n.mintQuery != "" {
		t.Errorf("the mint carried Authorization %q and query %q — your own "+
			"token names nobody and presents no bearer", n.mintAuth, n.mintQuery)
	}
	if n.mintBody["label"] != "laptop" {
		t.Errorf("the mint's body was %v", n.mintBody)
	}
	if !strings.Contains(out, "cwl_pat_the-value") {
		t.Errorf("the value was not printed:\n%s", out)
	}
	if strings.Contains(out+errs, "a long pass phrase") {
		t.Error("the password was echoed")
	}
}

// A SECOND FACTOR IS THE LINE AFTER THE PASSWORD, or the -code flag.
//
// The first factor checks out and the node asks for the code; the command
// reads it once and signs in again. Mutation: stop reading the second line and
// the sign-in is refused for want of a code.
func TestASecondFactorIsAskedForOnce(t *testing.T) {
	n := newSelfNode(t)
	n.wantCode = "123456"
	if _, errs, err := runSelf(t, n, "a long pass phrase\n123456\n"); err != nil {
		t.Fatalf("a piped code: %v\n%s", err, errs)
	}
	if len(n.signIns) != 2 || n.signIns[1]["code"] != "123456" {
		t.Errorf("the sign-ins carried %v, want a second one with the code", n.signIns)
	}

	flagged := newSelfNode(t)
	flagged.wantCode = "654321"
	if _, errs, err := runSelf(t, flagged, "a long pass phrase\n",
		"-code", "654321"); err != nil {
		t.Fatalf("a flagged code: %v\n%s", err, errs)
	}
	if len(flagged.signIns) != 1 {
		t.Errorf("a code given up front took %d sign-ins, want one", len(flagged.signIns))
	}
}

// A REFUSED MINT STILL SIGNS OUT, and says what refused it.
func TestARefusedMintStillSignsOut(t *testing.T) {
	n := newSelfNode(t)
	n.mintCode = http.StatusForbidden
	_, _, err := runSelf(t, n, "a long pass phrase\n")
	if err == nil || !strings.Contains(err.Error(), "not today") {
		t.Fatalf("a refused mint answered %v, want the node's refusal", err)
	}
	if strings.Join(n.steps, ",") != "login,mint,logout" {
		t.Errorf("the node saw %v — the session a refused mint opened was left "+
			"open", n.steps)
	}
}

// ONE OWNER, NAMED: -login for your own, -person for a service account's.
func TestATokenNamesExactlyOneOwner(t *testing.T) {
	for name, args := range map[string][]string{
		"both":    {"token", "-login", "jane.doe", "-person", "p-1"},
		"neither": {"token"},
	} {
		var out, errs bytes.Buffer
		err := runIAM(args, strings.NewReader(""), &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "-login") ||
			!strings.Contains(err.Error(), "-person") {
			t.Errorf("%s answered %v, want a refusal naming both flags", name, err)
		}
	}
}
