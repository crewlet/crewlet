package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

	// restricted makes every sign-in answer a session that may only enrol
	// a second factor.
	restricted bool

	// mintOutcome is the outcome a mint answers, as a real node's every
	// write answer carries one: `applied` unless a case says otherwise, and
	// nil for an answer carrying none.
	mintOutcome any
}

const selfCookie = "the-session-bearer"

func newSelfNode(t *testing.T) *selfNode {
	t.Helper()
	n := &selfNode{mintCode: http.StatusCreated, mintOutcome: "applied"}
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
			status := "signed_in"
			if n.restricted {
				status = string(httpjson.CodeSecondFactorEnrolmentRequired)
			}
			httpjson.Write(w, http.StatusOK, map[string]string{
				"person": "p-jane", "status": status})
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
		code, outcome := n.mintCode, n.mintOutcome
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
		answer := map[string]any{
			"id": "c-1", "person": "p-jane", "token": "cwl_pat_the-value",
			"grants":     []string{"worklet"},
			"expires_at": "2026-07-01T00:00:00Z",
			"position":   "CREWLET_IAM_LOG@0:7", "op_id": "the-mint-key.k0123456789abcdef",
		}
		if outcome != nil {
			answer["outcome"] = outcome
		}
		httpjson.Write(w, http.StatusCreated, answer)
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
		"-grants", "worklet", "-config", selfConfig(t), "-api", n.server.URL},
		extra...)
	var out, errs bytes.Buffer
	err := runIAM(noEnviron, args, strings.NewReader(stdin), &out, &errs)
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

// A SECOND FACTOR IS THE LINE AFTER THE PASSWORD, and never a flag.
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
}

// A SECOND-FACTOR CODE IS NOT A FLAG, for the password's reason.
//
// One of the two things it may be is a RECOVERY CODE, a single-use secret that
// stays good until it is spent — and a sign-in refused for a mistyped password
// spends nothing, so a code typed as an argument would sit in the shell's
// history and in `ps` still working. So `-code` is an unknown flag, refused
// before anything is read or sent: exactly `-token`'s treatment. Mutation:
// declare the flag again and the command signs in with it.
func TestASecondFactorCodeIsNeverAFlag(t *testing.T) {
	n := newSelfNode(t)
	n.wantCode = "654321"
	_, _, err := runSelf(t, n, "a long pass phrase\n", "-code", "654321")
	if err == nil || !strings.Contains(err.Error(), "-code") {
		t.Fatalf("a code on the command line answered %v, want the flag "+
			"refused as one nothing declares", err)
	}
	if len(n.signIns) != 0 {
		t.Errorf("the command signed in %d times before refusing the flag",
			len(n.signIns))
	}
}

// A REFUSED MINT STILL SIGNS OUT, and says what refused it.
func TestARefusedMintStillSignsOut(t *testing.T) {
	n := newSelfNode(t)
	n.mintCode = http.StatusForbidden
	_, _, err := runSelf(t, n, "a long pass phrase\n")
	if err == nil || !strings.Contains(err.Error(), "Not today.") {
		t.Fatalf("a refused mint answered %v, want the node's refusal", err)
	}
	if strings.Join(n.steps, ",") != "login,mint,logout" {
		t.Errorf("the node saw %v — the session a refused mint opened was left "+
			"open", n.steps)
	}
}

// A PASSWORD ALONE WHERE A SECOND FACTOR IS REQUIRED MINTS NOTHING, SAYS WHY,
// AND STILL SIGNS OUT.
//
// The sign-in succeeds into a session that may only enrol a second factor —
// its answer says `second_factor_enrolment_required` — so a mint would only be
// refused. The command says what to do instead, asks the node for nothing, and
// ends the session it opened. Mutation: ignore the status and the node sees a
// mint; return before the deferred sign-out and it sees no logout.
func TestAPasswordAloneWhereAFactorIsRequiredMintsNothing(t *testing.T) {
	n := newSelfNode(t)
	n.restricted = true
	_, _, err := runSelf(t, n, "a long pass phrase\n")
	if err == nil || !strings.Contains(err.Error(), "two-step verification") {
		t.Fatalf("a restricted sign-in answered %v, want the enrolment named", err)
	}
	if strings.Join(n.steps, ",") != "login,logout" {
		t.Errorf("the node saw %v, want a sign-in and a sign-out and no mint",
			n.steps)
	}
}

// ONE OWNER, NAMED: -login for your own, -person for a service account's.
func TestATokenNamesExactlyOneOwner(t *testing.T) {
	for name, args := range map[string][]string{
		"both":    {"token", "-login", "jane.doe", "-person", "p-1"},
		"neither": {"token"},
	} {
		var out, errs bytes.Buffer
		err := runIAM(noEnviron, args, strings.NewReader(""), &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "-login") ||
			!strings.Contains(err.Error(), "-person") {
			t.Errorf("%s answered %v, want a refusal naming both flags", name, err)
		}
	}
}

// THE COMMAND LINE STATES THE ORIGIN A BROWSER WOULD, by the one rule the
// server compares origins by (auth.Origin), rather than a copy of its own
// that kept the host's case and a default port as written.
//
// Mutation: build the origin from the parsed scheme and host again and the
// first case keeps `Crewlet.Example.com:443`.
func TestTheCommandLineStatesTheOriginABrowserWould(t *testing.T) {
	t.Parallel()
	const dialled = "http://127.0.0.1:8080"
	for _, tc := range []struct{ external, want string }{
		{"https://Crewlet.Example.com:443/app", "https://crewlet.example.com"},
		{"", dialled},
	} {
		boot := config.DefaultBootstrap()
		boot.API.ExternalURL = tc.external
		if got := reachedAt(&boot, dialled); got != tc.want {
			t.Errorf("reachedAt with api.external_url %q = %q, want %q", tc.external, got, tc.want)
		}
	}
}

// A PIPED PASSWORD, OR THE CODE AFTER IT, IS READ NO FURTHER THAN ANY SIGN-IN
// CARRIES, and refused before anything is sent.
//
// Each was the next line of standard input, read to its end: a file piped by
// mistake was read up to its first line ending, so a blob with none was read
// whole into memory, and a stream with no line ending was a sign-in that
// never began. A sign-in's whole body is at most
// [authapi.MaxLoginBodyBytes], so no password or code is longer; the line is
// refused at that bound, having read at most one buffer of the line reader
// past it (bufio's default, 4096 bytes), and the node sees nothing for a
// password and only the first sign-in for a code.
//
// Mutation: read the line with ReadString again, and the stream with no
// ending runs into its reader's guard and is answered as a failed read.
func TestAPipedPasswordOrCodeIsReadNoFurtherThanAnySignInCarries(t *testing.T) {
	const most = maxPipedPasswordBytes + len("\r\n") + 4096
	for _, tc := range []struct {
		name     string
		before   string
		wantCode string
		signIns  int
	}{
		{name: "the password", signIns: 0},
		{name: "the second-factor code", before: "a long pass phrase\n", wantCode: "123456", signIns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newSelfNode(t)
			n.wantCode = tc.wantCode
			endless := &endlessReader{}
			args := []string{"token", "-login", "jane.doe", "-label", "laptop",
				"-grants", "worklet", "-config", selfConfig(t), "-api", n.server.URL}
			var out, errs bytes.Buffer
			err := runIAM(noEnviron, args, io.MultiReader(strings.NewReader(tc.before), endless),
				&out, &errs)
			if !errors.Is(err, errPipedPasswordTooLong) {
				t.Fatalf("a line with no end answered %v, want errPipedPasswordTooLong", err)
			}
			if endless.read > most {
				t.Errorf("read %d bytes of the stream to refuse it, want at most %d", endless.read, most)
			}
			if len(n.signIns) != tc.signIns {
				t.Errorf("the node saw %d sign-in(s), want %d", len(n.signIns), tc.signIns)
			}
		})
	}
}

// A PASSWORD AT THE BOUND IS A PASSWORD, on either reader, and a byte past it
// is not: the bound refuses a pipe pointed at the wrong thing, never a
// password a sign-in could carry. A line ending, `\n` or `\r\n`, is not part
// of the password; the last line may have none, and the code is the line
// after the password.
//
// So the bound is the password's, measured once the ending is gone: a byte
// past it is refused whatever ending follows, or none. Held to the raw bytes
// with room for `\r\n`, a password a byte past the bound passed with `\n` or
// with no ending, and only `\r\n` after it was refused.
//
// Mutation: drop the length check after the ending is trimmed, and the
// password a byte past the bound with `\n` or with no ending is read back on
// both readers.
func TestAPasswordAtTheBoundIsReadWholeAndOnePastItIsNot(t *testing.T) {
	t.Parallel()
	atLimit := strings.Repeat("p", maxPipedPasswordBytes)
	for _, ending := range []string{"", "\n", "\r\n"} {
		if got, err := readPipedPassword(strings.NewReader(atLimit + ending)); err != nil || got != atLimit {
			t.Errorf("readPipedPassword of the bound and %q: %d bytes, %v", ending, len(got), err)
		}
		next := "123456"
		if ending == "" {
			next = ""
		}
		lines := bufio.NewReader(strings.NewReader(atLimit + ending + next))
		if got, err := readPiped(lines); err != nil || got != atLimit {
			t.Errorf("readPiped of the bound and %q: %d bytes, %v", ending, len(got), err)
		}
		if next == "" {
			continue
		}
		if got, err := readPiped(lines); err != nil || got != next {
			t.Errorf("the line after a password at the bound read as %q, %v", got, err)
		}
	}
	for _, ending := range []string{"", "\n", "\r\n"} {
		past := atLimit + "p" + ending
		if got, err := readPipedPassword(strings.NewReader(past)); !errors.Is(err, errPipedPasswordTooLong) {
			t.Errorf("readPipedPassword a byte past the bound and %q: %d bytes, %v", ending, len(got), err)
		}
		if got, err := readPiped(bufio.NewReader(strings.NewReader(past))); !errors.Is(err, errPipedPasswordTooLong) {
			t.Errorf("readPiped a byte past the bound and %q: %d bytes, %v", ending, len(got), err)
		}
	}
	endless := &endlessReader{}
	if _, err := readPipedPassword(endless); !errors.Is(err, errPipedPasswordTooLong) {
		t.Fatalf("readPipedPassword of a stream that never ends answered %v", err)
	}
	if most := maxPipedPasswordBytes + len("\r\n") + 1; endless.read > most {
		t.Errorf("readPipedPassword read %d bytes of the stream, want at most %d", endless.read, most)
	}
}
