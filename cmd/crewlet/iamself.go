package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// `crewlet iam token -login` — minting YOUR OWN machine token.
//
// # Why this one command signs in
//
// A person's machine token is theirs alone to mint: whoever mints one is shown
// its value and it acts as its owner, so internal/iamdomain refuses a mint on a
// person's account by anybody else, an administrator included. What mints one
// is the person's own SESSION — and this CLI otherwise authenticates with
// CREWLET_API_TOKEN and nothing else, which is either the deployment's Tier A
// token (it owns no tokens) or a machine token (which may not mint another).
// So for this one request it signs in exactly as the dashboard does: the login,
// a password read from the terminal without echo or from the first line of
// standard input, and a second-factor code where the person holds one, read the
// same way; then the mint, carrying the cookie that answered; then a logout, so
// the session it opened ends with the command rather than living on, in
// nobody's browser, for its absolute lifetime.
//
// # Why it states an Origin here and nowhere else
//
// A cookie-authenticated write is refused without an Origin the deployment is
// reached at (internal/api/auth's cross-site check): a browser always sends
// one, so a request carrying a cookie and none did not come from the browser
// the cookie was issued to. This client IS the party the cookie was issued
// to, so it states the deployment's own origin — `api.external_url`'s. A
// bearer request states none, which the same check admits.
//
// # What it cannot do
//
// A deployment that signs in through an identity provider serves no password
// route, and a provider's round trip needs a browser. There the mint is the
// same request made from a session the browser holds, and this command says
// so.

// selfMint is one person minting their own token.
type selfMint struct {
	// base is the node's API, dialled; origin is the address the deployment
	// is reached at, stated on the cookie-authenticated requests.
	base, origin string

	login, password, code string

	// body is the mint's own request: grants, reach, label, lifetime.
	body map[string]any

	http *http.Client
}

// errSecondFactor is a sign-in whose first factor checked out and whose person
// holds a second one the request did not carry.
var errSecondFactor = errors.New("this person holds a second factor")

// mint signs in, mints, and signs out, answering what the mint answered.
//
// THE SIGN-OUT RUNS WHATEVER THE MINT ANSWERED, on a context the caller's
// cancellation does not reach: a session left open because the mint failed is
// the one this command promised not to leave. A sign-out that does not land is
// reported on warn, with what to do, and never replaces the mint's answer.
func (m selfMint) mint(ctx context.Context, warn io.Writer) (map[string]any, error) {
	cookie, err := m.signIn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := m.signOut(context.WithoutCancel(ctx), cookie); err != nil {
			// NOT `crewlet iam revoke`, nor POST /auth/logout/all: both
			// move the owner's revocation epoch, which withdraws the
			// token this command just minted along with the session.
			fmt.Fprintf(warn, "the session this command opened could not be "+
				"ended (%v): it lapses on its own after %s unused, or end it "+
				"by name with POST /auth/logout/<lineage> (`crewlet iam "+
				"sessions <your id>` lists it) — not with `crewlet iam "+
				"revoke`, which would withdraw the new token too\n",
				err, session.Idle)
		}
	}()
	answer, status, raw, err := m.send(ctx, http.MethodPost, "/iam/credentials",
		cookie, m.body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return answer, selfRefusal(status, answer, raw)
	}
	return answer, nil
}

// signIn posts the credentials and answers the session cookie that came back.
func (m selfMint) signIn(ctx context.Context) (*http.Cookie, error) {
	body := map[string]any{"login": m.login, "password": m.password}
	if m.code != "" {
		body["code"] = m.code
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.base+auth.PathAuthLogin, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", m.base+auth.PathAuthLogin, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, fmt.Errorf("read the sign-in's answer: %w", err)
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	code, _ := answer["error"].(string)
	switch {
	case resp.StatusCode == http.StatusOK:
	case code == string(httpjson.CodeSecondFactorRequired):
		return nil, errSecondFactor
	case code == string(httpjson.CodeSignInRefused):
		// ONE SENTENCE FOR EVERY MISTAKE, because the node gives one
		// answer for every mistake: telling a caller which part was wrong
		// is telling a stranger who works here.
		return nil, errors.New("the node refused the sign-in: check the " +
			"login, the password and any second-factor code — it answers " +
			"the same way whichever was wrong")
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("this deployment does not sign in with " +
			"passwords, so a terminal cannot sign in to it: mint your token " +
			"from a signed-in browser session (POST /iam/credentials with no " +
			"?person=)")
	default:
		return nil, selfRefusal(resp.StatusCode, answer, raw)
	}
	for _, c := range resp.Cookies() {
		if slices.Contains(session.CookieNames, c.Name) && c.Value != "" {
			return &http.Cookie{Name: c.Name, Value: c.Value}, nil
		}
	}
	return nil, errors.New("the node answered the sign-in without a " +
		"session cookie, so there is nothing to mint with")
}

// signOut ends the session this command opened.
func (m selfMint) signOut(ctx context.Context, cookie *http.Cookie) error {
	_, status, raw, err := m.send(ctx, http.MethodPost, "/auth/logout", cookie, nil)
	switch {
	case err != nil:
		return err
	case status >= 400:
		return fmt.Errorf("the node answered %d: %s", status,
			strings.TrimSpace(string(raw)))
	}
	return nil
}

// send is one cookie-authenticated round trip.
func (m selfMint) send(ctx context.Context, method, path string,
	cookie *http.Cookie, body map[string]any) (map[string]any, int, []byte, error) {

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, payload)
	if err != nil {
		return nil, 0, nil, err
	}
	req.AddCookie(cookie)
	req.Header.Set("Origin", m.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("reach %s: %w", m.base+path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read the answer from %s: %w",
			m.base+path, err)
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	return answer, resp.StatusCode, raw, nil
}

// selfRefusal is a refusal on this path, as the sentence an operator acts on.
//
// NOT [iamRefusal]: that one reads a 401 as a problem with CREWLET_API_TOKEN,
// which this path never sent.
func selfRefusal(status int, answer map[string]any, raw []byte) error {
	code, _ := answer["error"].(string)
	detail, _ := answer["detail"].(string)
	if detail == "" {
		detail, _ = answer["message"].(string)
	}
	switch {
	case code != "" && detail != "":
		return fmt.Errorf("%s: %s", code, detail)
	case code != "":
		return errors.New(code)
	case len(raw) > 0:
		return fmt.Errorf("the node answered %d: %s", status,
			strings.TrimSpace(string(raw)))
	}
	return fmt.Errorf("the node answered %d", status)
}

// mintOwnToken is `crewlet iam token -login`: it reads what signing in needs,
// signs in, mints, and signs out.
//
// NEITHER THE PASSWORD NOR THE SECOND FACTOR COMES FROM A FLAG, for
// CREWLET_API_TOKEN's reason: a value on a command line lands in shell history
// and in `ps`. The second factor is held to that as much as the password is,
// because one of the two things it may be is a RECOVERY CODE — a single-use
// secret that stays good until it is spent, and a sign-in refused for a
// mistyped password spends nothing, so a code typed as an argument would sit
// in the shell's history still working. Each is read from the terminal without
// echo, or — piped — as the next line of standard input: the password first,
// then the code, asked for only once the first factor checks out.
func mintOwnToken(ctx context.Context, boot *config.Bootstrap, apiURL,
	login string, body map[string]any, stdin io.Reader,
	stderr io.Writer) (map[string]any, error) {

	base, err := nodeBaseURL(boot, apiURL, "the company's identity directory")
	if err != nil {
		return nil, err
	}
	in := bufio.NewReader(stdin)
	password, err := readSecret(stdin, in, stderr, "Password for "+login+": ")
	if err != nil {
		return nil, fmt.Errorf("read the password: %w", err)
	}
	m := selfMint{
		base: base, origin: reachedAt(boot, base), login: login,
		password: password, body: body, http: httpx.Client(apiTimeout),
	}
	answer, err := m.mint(ctx, stderr)
	if !errors.Is(err, errSecondFactor) {
		return answer, err
	}
	// ASKED FOR, ONCE: the first factor checked out and the person holds a
	// second, so the code is the one thing missing.
	code, err := readSecret(stdin, in, stderr, "Second-factor code: ")
	if err != nil {
		return nil, fmt.Errorf("this person holds a second factor, and none "+
			"was given: type it when asked, or pipe it as the line after the "+
			"password (%w)", err)
	}
	if m.code = strings.TrimSpace(code); m.code == "" {
		return nil, errors.New("this person holds a second factor, and none " +
			"was given: type the current six digits or a recovery code when " +
			"asked, or pipe it as the line after the password")
	}
	return m.mint(ctx, stderr)
}

// reachedAt is the origin a browser would state for this deployment:
// `api.external_url`'s, or the dialled base where none is set.
func reachedAt(boot *config.Bootstrap, base string) string {
	for _, candidate := range []string{boot.API.ExternalBase(), base} {
		parsed, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && parsed.Scheme != "" && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
	}
	return base
}

// readSecret reads one secret: from the terminal without echo when standard
// input is one, or the next line of whatever was piped in.
func readSecret(stdin io.Reader, lines *bufio.Reader, prompt io.Writer,
	ask string) (string, error) {

	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(prompt, ask)
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		return string(raw), err
	}
	return readPiped(lines)
}

// readPiped is the next line of input, without its line ending; the last line
// may end with none.
func readPiped(lines *bufio.Reader) (string, error) {
	line, err := lines.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
