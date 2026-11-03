package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
)

// LoginState is what a harness's own status command says of the login an entry
// keeps in its credential files.
type LoginState string

const (
	// LoginSignedIn is a status command that exited 0: the harness holds a login
	// it takes for its own.
	LoginSignedIn LoginState = "signed_in"
	// LoginSignedOut is a status command that exited 1, which every shipped
	// harness with a status command answers when it holds no login.
	LoginSignedOut LoginState = "signed_out"
	// LoginUnknown is any other answer: another exit, a command that ran
	// out of time, or one whose output something it left running still held.
	LoginUnknown LoginState = "unknown"
)

// Valid reports whether s is one of the three states.
func (s LoginState) Valid() bool {
	switch s {
	case LoginSignedIn, LoginSignedOut, LoginUnknown:
		return true
	}
	return false
}

// LoginStatus is what the harness said of its login when it was last asked
// ([Provider.LoginStatus]).
type LoginStatus struct {
	// State is the answer, read off the command's exit.
	State LoginState
	// Method is how the harness is signed in, as the harness names it ("claude.ai"),
	// where the profile's status_fields read one out of the command's JSON.
	Method string
	// Account is who the harness is signed in as (an email, an organisation),
	// read the same way, at most [maxStatusAccount] bytes.
	Account string
	// Detail is the first line the harness wrote on stderr for an unknown
	// answer, or what kept it from answering, scrubbed of anything shaped
	// like a credential and at most [maxStatusDetail] bytes. Empty for the
	// other two states.
	Detail string
	// CheckedAt is when the command answered, in UTC.
	CheckedAt time.Time
}

// loginStatusTimeout bounds one status command.
//
// A status command reads the harness's own credential file and prints it; it calls
// no network (Claude Code's `auth status` and Codex's `login status` both answer
// offline, which is the reason a periodic check may run it at all). What it
// costs is the harness's runtime starting, about the half second
// [auxiliaryTimeout] measures for a command of this size, so ten seconds is an
// order of magnitude of headroom on a host already running max_concurrent harnesses,
// and short enough that a wedged one is reported as unknown long before the
// next check is due.
const loginStatusTimeout = 10 * time.Second

// maxStatusAccount bounds [LoginStatus.Account]: an email address is at most
// 254 bytes by its RFC, and every one a person uses is far shorter; 120 keeps
// any real one whole while a harness that printed something else there cannot
// carry a page of it into every place the status is shown.
const maxStatusAccount = 120

// maxStatusMethod bounds [LoginStatus.Method], a short word in every harness
// measured ("claude.ai", "console", "api_key").
const maxStatusMethod = 64

// maxStatusDetail bounds [LoginStatus.Detail]: one sentence of the harness's own,
// which is what a person reading why the answer is unknown needs.
const maxStatusDetail = 200

// LoginStatus asks the harness whether it holds a login, and as whom, by its
// profile's status_args run in the entry's login home ([Provider.Status] is
// the same command in words, for a person).
//
// THE EXIT IS THE ANSWER: 0 is [LoginSignedIn], 1 is [LoginSignedOut], and
// anything else is [LoginUnknown] with the first line the harness wrote on stderr as
// its detail. What the command printed is read only for the profile's
// status_fields, and only where it is JSON: Claude Code's `auth status` prints
// `authMethod` beside `email` and `orgName`, and a harness whose status is a
// sentence has no fields to read, so its answer is the exit alone.
//
// IN THE LOGIN HOME, as `crewlet llm status` runs it, so the harness reads the login
// every seat is seeded from, and as a command that makes no login
// ([syncRefresh]): a harness that refreshed its token while it looked carries the
// refresh back over the login it was seeded from and over nothing else.
//
// AN ERROR IS A STATUS THAT COULD NOT BE ASKED, never an answer: a profile
// with no status command ([ErrNoLoginCommand]), another `crewlet llm` command
// holding the login home, a harness that could not be started, a home that could
// not be prepared or carried out. A command that ran and did not say is
// [LoginUnknown]. Bounded by [loginStatusTimeout].
func (p *Provider) LoginStatus(ctx context.Context) (LoginStatus, error) {
	return p.loginStatus(ctx, loginStatusTimeout)
}

// loginStatus is [Provider.LoginStatus] within a budget of the caller's, which
// the suite shortens to watch one run out.
func (p *Provider) loginStatus(ctx context.Context, budget time.Duration) (LoginStatus, error) {
	if len(p.profile.StatusArgs) == 0 {
		return LoginStatus{}, fmt.Errorf("%w: the %q harness has no status command, so whether it "+
			"holds a login can only be seen by using it", ErrNoLoginCommand, p.harness)
	}
	var res *rawResult
	_, err := p.inCredentialHome(ctx, syncRefresh, io.Discard,
		func(ctx context.Context, home string, env []string) error {
			r, err := run(ctx, invocation{
				binary: p.profile.Binary, args: p.profile.StatusArgs,
				dir: home, env: env, timeout: budget, grace: p.termGrace,
			})
			res = r
			return err
		})
	if err != nil {
		return LoginStatus{}, err
	}
	return p.readLoginStatus(res, budget, time.Now()), nil
}

// readLoginStatus reads the result of a status command run within budget, as
// of at.
func (p *Provider) readLoginStatus(res *rawResult, budget time.Duration, at time.Time) LoginStatus {
	status := LoginStatus{State: LoginUnknown, CheckedAt: at.UTC()}
	switch {
	case res.timedOut:
		status.Detail = fmt.Sprintf("the status command did not answer within %s", budget)
	case res.unfinished() != "":
		status.Detail = clipBytes("the status command "+res.unfinished(), maxStatusDetail)
	case res.exitCode == 0:
		status.State = LoginSignedIn
		status.Method, status.Account = p.statusFields(res.stdout)
	case res.exitCode == 1:
		status.State = LoginSignedOut
	default:
		said := firstLine(ansiEscape.ReplaceAllString(res.stderr, ""))
		status.Detail = clipBytes(fmt.Sprintf("the status command exited %d: %s", res.exitCode,
			redact.Secrets(said)), maxStatusDetail)
	}
	return status
}

// statusFields reads the sign-in method and the account out of a status
// command's JSON by the profile's status_fields, "" for each it cannot find.
func (p *Provider) statusFields(stdout string) (method, account string) {
	fields := p.profile.StatusFields
	if fields == nil {
		return "", ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &doc); err != nil {
		return "", ""
	}
	method, _ = firstString(doc, fields.Method)
	account, _ = firstString(doc, fields.Account)
	return clipBytes(strings.TrimSpace(method), maxStatusMethod),
		clipBytes(strings.TrimSpace(account), maxStatusAccount)
}

// clipBytes cuts s to at most n bytes, at a rune boundary, so a bound never
// leaves half a character behind.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
