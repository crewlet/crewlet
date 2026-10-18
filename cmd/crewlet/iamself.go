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
	"os"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
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

// selfMint is one person minting their own token.
type selfMint struct {
	// base is the node's API, dialled; origin is the address the deployment
	// is reached at, stated on the cookie-authenticated requests.
	base, origin string

	login, password, code string

	// body is the mint's own request: grants, label, lifetime.
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
	cookie, restricted, err := m.signIn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if endErr := m.signOut(context.WithoutCancel(ctx), cookie); endErr != nil {
			// NOT `crewlet iam revoke`, nor POST /auth/logout/all: both
			// move the owner's revocation epoch, which withdraws the
			// token this command just minted along with the session.
			fmt.Fprintf(warn, "couldn't sign out the session this command "+
				"opened (%v). It ends on its own after %s unused, or now "+
				"with POST /auth/logout/<lineage>. Don't use `crewlet iam "+
				"revoke` for it: that revokes the new token too.\n",
				endErr, session.Idle)
		}
	}()
	if restricted {
		// A SESSION THAT MAY ONLY ENROL A SECOND FACTOR mints nothing,
		// and asking would only be refused: said here, with the remedy,
		// rather than as the node's 403 on the mint. The deferred
		// sign-out still ends it.
		return nil, errEnrolFirst
	}
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

// errEnrolFirst is a sign-in that succeeded into a session that may only enrol
// a second factor: the deployment requires one and this person holds none.
var errEnrolFirst = errors.New("this company requires two-step verification. " +
	"Set it up in the dashboard, then run this again")

// signIn posts the credentials and answers the session cookie that came back,
// and whether that session may only enrol a second factor.
func (m selfMint) signIn(ctx context.Context) (*http.Cookie, bool, error) {
	body := map[string]any{"login": m.login, "password": m.password}
	if m.code != "" {
		body["code"] = m.code
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.base+auth.PathAuthLogin, bytes.NewReader(encoded))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("reach %s: %w", m.base+auth.PathAuthLogin, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, false, fmt.Errorf("read the sign-in response: %w", err)
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	code, _ := answer["error"].(string)
	switch {
	case resp.StatusCode == http.StatusOK:
	case code == string(httpjson.CodeSecondFactorRequired):
		return nil, false, errSecondFactor
	case code == string(httpjson.CodeSignInRefused):
		// ONE SENTENCE FOR EVERY MISTAKE, because the node gives one
		// answer for every mistake: telling a caller which part was wrong
		// is telling a stranger who works here.
		return nil, false, errors.New("sign-in failed. Check your login, " +
			"password and code (the node doesn't say which was wrong)")
	default:
		return nil, false, selfRefusal(resp.StatusCode, answer, raw)
	}
	// THE SESSION'S OWN STATUS, which is how a sign-in that succeeded on a
	// password alone, where a second factor is required, says it may do
	// nothing but enrol one.
	status, _ := answer["status"].(string)
	restricted := status == string(httpjson.CodeSecondFactorEnrolmentRequired)
	for _, c := range resp.Cookies() {
		if slices.Contains(session.CookieNames, c.Name) && c.Value != "" {
			return &http.Cookie{Name: c.Name, Value: c.Value}, restricted, nil
		}
	}
	return nil, false, errors.New("the node signed you in but sent no " +
		"session cookie, so no token can be created")
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
	detail := answerWords(answer).said()
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
	code, err := readSecret(stdin, in, stderr, "Authenticator or recovery code: ")
	if err != nil {
		return nil, fmt.Errorf("this account uses two-step verification. Type "+
			"the code when asked, or pipe it after the password (%w)", err)
	}
	if m.code = strings.TrimSpace(code); m.code == "" {
		return nil, errors.New("this account uses two-step verification. " +
			"Type the code or a recovery code when asked, or pipe it after " +
			"the password")
	}
	return m.mint(ctx, stderr)
}

// reachedAt is the origin a browser would state for this deployment:
// `api.external_url`'s, or the dialled base where none is set, spelled by the
// rule the server compares origins by ([auth.Origin]).
func reachedAt(boot *config.Bootstrap, base string) string {
	for _, candidate := range []string{boot.API.ExternalBase(), base} {
		if origin := auth.Origin(candidate); origin != "" {
			return origin
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
//
// NO LONGER THAN A PASSWORD IS ([maxPipedPasswordBytes]) once that ending is
// gone ([pipedPassword]), and a line that runs past the bound and the longest
// ending is refused without reading the rest: what is piped in is whatever
// the operator pointed at it, and a line read to its end was a file read up
// to its first line ending — the whole of a blob with none, into memory — or,
// from a stream with no line ending, a read that never finished. That read
// limit is checked a buffer at a time, so a line refused unread is read at
// most one buffer past it.
func readPiped(lines *bufio.Reader) (string, error) {
	const what = "a password, or the second-factor code after one,"
	var line []byte
	for {
		chunk, err := lines.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxPipedLineBytes {
			return "", pipedPasswordOverran(what)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// The line goes on past the reader's buffer: read the next
			// part of it, which the read limit above still holds.
			continue
		case err == nil:
		case errors.Is(err, io.EOF) && len(line) > 0:
			// A LAST LINE WITH NO ENDING arrives together with io.EOF,
			// and it is still the line that was typed.
		default:
			return "", err
		}
		return pipedPassword(line, what)
	}
}

// readPipedPassword is the password piped to `crewlet llm login -username U
// -password-stdin`: everything piped in, without the line ending a pipe adds,
// held to [maxPipedPasswordBytes] as a line piped to `crewlet iam token
// -login` is, and refused without reading the rest past that bound and the
// longest ending, for the same reason ([readPiped]).
func readPipedPassword(r io.Reader) (string, error) {
	const what = "a password"
	raw, err := io.ReadAll(io.LimitReader(r, int64(maxPipedLineBytes)+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxPipedLineBytes {
		return "", pipedPasswordOverran(what)
	}
	return pipedPassword(raw, what)
}

// pipedPassword is what was piped in as the password, or code, it carries:
// without the line ending a pipe adds, and refused when what is left is
// longer than [maxPipedPasswordBytes].
//
// MEASURED ONCE THE ENDING IS GONE, because the ending is the pipe's and not
// the password's: held to the raw bytes, the bound has to allow for `\r\n`,
// and then a password passes that is past it by as many bytes as its own
// ending falls short of `\r\n` — one with `\n`, two with none.
func pipedPassword(raw []byte, what string) (string, error) {
	password := strings.TrimRight(string(raw), "\r\n")
	if len(password) > maxPipedPasswordBytes {
		return "", fmt.Errorf("%w: what arrived is %d bytes without its line ending; %s is at "+
			"most %d bytes (%d KiB) here — check what is piped in", errPipedPasswordTooLong,
			len(password), what, maxPipedPasswordBytes, maxPipedPasswordBytes>>10)
	}
	return password, nil
}

// maxPipedPasswordBytes bounds a password piped to a command that signs in
// with it, and the second-factor code piped after one, each measured without
// the line ending a pipe adds ([pipedPassword]).
//
// THE ENGINE'S OWN SIGN-IN SETS IT: [authapi.MaxLoginBodyBytes] is the whole
// body a sign-in reads — the login, the password and a code — so no password
// longer than that signs anybody in to this engine, and `crewlet iam token
// -login` has no reason to read one further. A vendor's username and password
// login (`crewlet llm login -username U -password-stdin`) takes the same kind
// of secret, typed or generated, and is held to the same bound: four
// kibibytes is an order of magnitude above any passphrase — 256 characters,
// [config.MaxPasswordLength], is already the longest floor a deployment may
// set — and a token is not a password: `-token-stdin` takes one, to the size
// every store keeps.
//
// NOT [secrets.MaxValueBytes], which a value piped to be stored is read to: a
// password is never stored, so the size every store keeps says nothing about
// it, and that limit is sized for a PEM key or a service account's JSON.
const maxPipedPasswordBytes = authapi.MaxLoginBodyBytes

// maxPipedLineBytes is how much is read for one piped password, or code,
// before it is refused unread: the password at its bound and the longest line
// ending a pipe adds, `\r\n`. It is the READ's limit and never the
// password's, which [pipedPassword] measures without the ending.
const maxPipedLineBytes = maxPipedPasswordBytes + len("\r\n")

// errPipedPasswordTooLong is a piped password, or code, longer than any
// sign-in carries. Its own sentinel because it is the caller's mistake: what
// fixes it is piping in the right thing, never a retry.
var errPipedPasswordTooLong = errors.New("more arrived on stdin than any password is")

// pipedPasswordOverran is [errPipedPasswordTooLong] for a pipe that ran past
// [maxPipedLineBytes] and was not read further: it names the bound it met,
// and never the bytes that met it.
func pipedPasswordOverran(what string) error {
	return fmt.Errorf("%w: more than %d bytes arrived, so the rest was not read; %s "+
		"is at most %d bytes (%d KiB) here — check what is piped in", errPipedPasswordTooLong,
		maxPipedLineBytes, what, maxPipedPasswordBytes, maxPipedPasswordBytes>>10)
}
