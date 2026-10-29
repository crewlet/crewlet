package cliagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// maxBundle bounds a credential bundle on the way IN, decompressed.
//
// An archive is an execution surface if it is unpacked on trust: a bundle
// arrives from the secret store, which is shared by every node holding the
// keyring, so a compromised row must not be able to exhaust a node's memory
// or fill its disk. One mebibyte is two orders of magnitude above the largest
// real login (a Claude credential file is under 2 KB) and small enough that
// the worst case is nothing.
//
// A BUNDLE KEPT IN THE SECRET STORE MEETS A SECOND BOUND, on different bytes:
// [github.com/crewlet/crewlet/internal/secrets.MaxValueBytes] caps what the
// store keeps, the bundle once packed (base64 of gzip), and `crewlet llm
// export -secret-store` checks a bundle against it before writing one, while
// this caps what a restore unpacks. A stored bundle must pass both, and
// neither implies the other: a directory that compresses well packs far
// smaller than it unpacks. This one is not tied to the store's because the
// store is not the only way in — `crewlet llm import` reads a bundle from
// stdin, and auth.credential_bundle can resolve from the engine's environment.
const maxBundle = 1 << 20

// ErrNoLoginCommand is returned when a profile declares no way to do what
// was asked. Its own error so `crewlet llm login` can print the paragraph
// that explains the vendor's actual login route instead of a bare failure.
var ErrNoLoginCommand = errors.New("cliagent: this CLI has no such login command")

// BrokerLogin runs the vendor's OWN interactive login, attached to the
// operator's terminal.
//
// Crewlet controls only WHERE the credential lands: in this provider's
// isolated credentials directory, separate from the operator's personal login
// on the same machine. It never sees the password, and never drives a
// headless browser through a vendor's login page — that breaks on the
// vendor's next redesign, and it is exactly the kind of re-implementation
// this whole backend exists to avoid.
//
// A login that leaves NO login is a failure, even where the CLI exited 0: see
// [Provider.loginLeftNothing].
func (p *Provider) BrokerLogin(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	if len(p.profile.LoginArgs) == 0 {
		// NOT "it authenticates on first use", which this said once: a seat
		// never creates a login (see [Workspace.syncCredentialsOut]), and a
		// headless call has nobody to complete a sign-in for it anyway — so
		// the advice to make a call and then look was advice to watch it fail.
		//
		// And not "adopt it from the host" for a CLI that keeps no login at
		// all: there is nothing on the host to adopt.
		if !p.profile.KeepsLogin() {
			return fmt.Errorf("%w: the %q CLI keeps no login for Crewlet to hold — %s",
				ErrNoLoginCommand, p.agent, p.keyRemedy())
		}
		return fmt.Errorf("%w: the %q CLI declares no login subcommand — sign in with "+
			"the CLI itself and adopt that login with `crewlet llm login %s -from-host`, "+
			"or give the entry a key",
			ErrNoLoginCommand, p.agent, p.key)
	}
	holds, err := p.runInCredentialHome(ctx, syncLogin, p.profile.LoginArgs, in, out, errOut)
	if err != nil {
		return err
	}
	if !holds {
		return p.loginLeftNothing()
	}
	return nil
}

// loginLeftNothing is the failure of a login command that exited without
// leaving a login anywhere this entry reads one.
//
// A FAILURE, NOT A SUCCESS THAT WROTE NOTHING. Its caller prints "Login written
// to …" on a nil error, and an operator who backed out of the CLI's provider
// picker — or whose CLI put the credential somewhere no seat is seeded from —
// was told exactly that about a directory holding nothing, and found out at
// the first call. Every shipped profile with a login command names where that
// login lands (its credential files, or the database its credential store
// exports), so a login that left none there is one no seat would ever have
// been signed in by.
func (p *Provider) loginLeftNothing() error {
	if !p.profile.KeepsLogin() {
		return fmt.Errorf("cliagent: the %q profile names no place a login is kept — no "+
			"credential_paths and no credential_store — so nothing its login command stores "+
			"can reach a seat; declare where it keeps one under "+
			"providers.llm.%s.overrides", p.agent, p.key)
	}
	return fmt.Errorf("cliagent: the %q CLI exited without leaving a login where this "+
		"entry reads one (%s), so nothing was written to %s — if the sign-in did not "+
		"finish, run `crewlet llm login %s` again",
		p.agent, p.profile.LoginLocation(), p.ws.CredentialsDir(), p.key)
}

// CaptureToken runs the vendor's token-minting command and returns what it
// printed.
//
// Preferred wherever a vendor offers it: a headless token has no credential
// files to sync, no refresh-token rotation to race, and it survives an
// ephemeral container with no persistent volume.
func (p *Provider) CaptureToken(ctx context.Context, in io.Reader, errOut io.Writer) (string, error) {
	if len(p.profile.CaptureTokenArgs) == 0 {
		// The login route this CLI has ([Provider.loginRemedy]): Gemini CLI,
		// Qwen Code and Grok declare no login subcommand, so "run `crewlet
		// llm login`" sent them to a second refusal.
		return "", fmt.Errorf("%w: the %q CLI mints no headless token, so sign it in "+
			"with a login instead: %s", ErrNoLoginCommand, p.agent, p.loginRemedy(nil))
	}
	// MIRRORED WHILE IT IS CAPTURED, and this is the whole difficulty of the
	// command. A minting command is INTERACTIVE — the CLI renders its UI
	// whenever STDIN is a terminal, which it is here, since the operator has
	// to complete a browser sign-in — and it writes every byte of that UI to
	// STDOUT: the sign-in URL, the "paste code here" prompt, all of it.
	// Measured against Claude Code 2.1.263 with a tty on stdin and a pipe on
	// stdout: 1512 bytes to stdout, ZERO to stderr. So capturing stdout into a
	// buffer alone handed the operator a blank terminal that then sat waiting
	// for a code from a URL they were never shown.
	//
	// The mirror goes to errOut rather than stdout because `-print-token`
	// writes the token itself to stdout, and a vendor's UI in that stream
	// would corrupt what the operator is piping into their secret manager.
	//
	// GUARDED, because os/exec promises at most one Write at a time only when
	// Stdout and Stderr are the SAME comparable value. A mirror makes them two
	// different writers over one destination, which is two copier goroutines
	// racing on it — and the loss is silent: measured, the mirrored bytes
	// simply vanished and the operator's terminal went blank again, which is
	// the very bug this is fixing. One mutex, shared by both, restores the
	// guarantee without folding stderr into the buffer the token is read from.
	//
	// A LOGIN COMMAND for the sync that follows it (syncLogin): a minting
	// command is the vendor's sign-in, and one that also leaves a credential
	// file behind has left the operator's own new login. Not one that must
	// leave a login, though — what it is FOR is the token it prints.
	var out bytes.Buffer
	mirror := lockedWriter{mu: new(sync.Mutex), w: errOut}
	if _, err := p.runInCredentialHome(
		ctx, syncLogin, p.profile.CaptureTokenArgs, in, io.MultiWriter(&out, mirror), mirror,
	); err != nil {
		return "", err
	}
	token := lastPrintedLine(out.String())
	if token == "" {
		return "", fmt.Errorf("cliagent: %q printed no token", p.agent)
	}
	return token, nil
}

// lockedWriter serialises the two copier goroutines os/exec runs when neither
// of a child's output streams is a plain file. Copies share the mutex by
// holding the same pointer, so one value guards one destination.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// ansiEscape matches the control sequences a terminal UI writes between its
// words: CSI (cursor, colour), OSC (window title, and the hyperlinks a modern
// CLI wraps a sign-in URL in), and the bare two-character escapes.
var ansiEscape = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC, both terminators
		"|\x1b[@-Z\\\\-_]") // the rest

// lastPrintedLine reads the token out of what a minting command printed.
//
// The LAST non-empty line, because these commands print their instructions
// above the token and storing the whole output would store the instructions.
//
// Escape sequences come off first, and \r splits a line like \n does: the same
// interactivity that makes the mirror above necessary means this stream is a
// rendered UI rather than a report, so it carries cursor moves, colour and
// OSC-8 hyperlinks, and it redraws in place. A line that is only decoration
// has to count as EMPTY rather than be stored as a credential — a token has no
// escape sequence in it, and a frame of one is not a token.
func lastPrintedLine(raw string) string {
	cleaned := ansiEscape.ReplaceAllString(raw, "")
	lines := strings.FieldsFunc(cleaned, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// CredentialLogin drives a profile's declared username/password login.
//
// The password goes on STDIN or in a declared environment variable, never on
// argv: argv is visible in `ps` to every user on the host and lands in the
// operator's shell history.
func (p *Provider) CredentialLogin(ctx context.Context, username, password string, out, errOut io.Writer) error {
	login := p.profile.StdinLogin
	if login == nil {
		return p.noCredentialLogin()
	}
	args := make([]string, 0, len(login.Args))
	for _, arg := range login.Args {
		args = append(args, strings.ReplaceAll(arg, "{username}", username))
	}
	stdin := strings.ReplaceAll(login.StdinTemplate, "{password}", password)
	stdin = strings.ReplaceAll(stdin, "{username}", username)

	extra := map[string]string{}
	if login.PasswordEnv != "" {
		extra[login.PasswordEnv] = password
		stdin = ""
	}
	holds, err := p.runInCredentialHome(ctx, syncLogin, args, strings.NewReader(stdin), out, errOut, extra)
	if err != nil {
		return err
	}
	if !holds {
		return p.loginLeftNothing()
	}
	return nil
}

// noCredentialLogin is [Provider.CredentialLogin]'s refusal for a CLI that
// declares no username/password login, naming the routes this entry has.
//
// THE MODE FIRST, THEN THE CLI, since the mode decides what the entry reads at
// all, in the words [Provider.ModeSignIn] keeps for every message that tells an
// operator how to sign such an entry in. Chosen by the CLI alone, an api-key
// entry was offered a login and `-capture-token`, a token the mode removes from
// the CLI's environment and that `crewlet llm login` refuses before it mints;
// and an inherit-env one `-capture-token`, whose token lands in the secret
// store that mode never reads, so the command refuses to store it. So api-key
// is told its key and nothing else, as the doctor tells it: a login in its
// directory would sit in every seat's home beside the key, and which of the
// two the CLI bills is the vendor's precedence rather than the entry's — except
// on a CLI whose profile names no key variable, a pairing every write of the
// configuration refuses, where the mode hands the CLI nothing and its remedy
// is a login under subscription, so the entry is told that and not that a
// login is not its sign-in. inherit-env is told what it forwards, a login,
// which it seeds like every mode, and a minted token only to stdout, to
// export. subscription is told a login and a headless token.
//
// ONLY ROUTES THIS CLI HAS, judged as the doctor judges them: its own login
// command where it declares one ([Provider.loginRemedy]), and -capture-token
// where it mints a headless token ([Provider.MintsHeadlessToken]). Offered
// whatever the CLI declared, both sent an operator of a CLI with neither to a
// second refusal.
func (p *Provider) noCredentialLogin() error {
	const lead = "%w: the %q CLI authenticates through the vendor's browser OAuth flow — " +
		"there is no username/password login to drive"
	declare := fmt.Sprintf(". If your build of this CLI does accept a credential, declare it "+
		"under providers.llm.%s.overrides.stdin_login", p.key)
	sign, fromEnv := p.ModeSignIn()
	switch {
	case fromEnv && sign.Mode == AuthAPIKey && len(sign.Variables) == 0 && len(sign.Misnamed) == 0:
		return fmt.Errorf(lead+": %s", ErrNoLoginCommand, p.agent, sign.Missing())
	case fromEnv && sign.Mode == AuthAPIKey:
		return fmt.Errorf(lead+", and a login is not this entry's sign-in: its auth.mode is "+
			"api-key, which %s: %s", ErrNoLoginCommand, p.agent, sign.Reads, sign.Supply)
	case fromEnv:
		mint := ""
		if p.MintsHeadlessToken() {
			mint = fmt.Sprintf(", or mint a headless %s to stdout, storing nothing, with "+
				"`crewlet llm login %s -capture-token -print-token`, and export it as %s in "+
				"the engine's environment", p.profile.TokenEnv, p.key, p.profile.TokenEnv)
		}
		return fmt.Errorf(lead+": %s; or, as its auth.mode is %s, which %s, %s%s%s",
			ErrNoLoginCommand, p.agent, p.loginRemedy(nil), sign.Mode, sign.Reads, sign.Supply,
			mint, declare)
	}
	mint := ""
	if p.MintsHeadlessToken() {
		mint = fmt.Sprintf(", or mint a headless %s with `crewlet llm login %s -capture-token`",
			p.profile.TokenEnv, p.key)
	}
	return fmt.Errorf(lead+": %s%s%s", ErrNoLoginCommand, p.agent, p.loginRemedy(nil), mint, declare)
}

// Logout revokes the login where the CLI can, and removes the credential
// files either way.
//
// Both halves, and the local half whatever the vendor's command did: a vendor
// command that failed must not leave a working login on disk that an operator
// believes they removed.
//
// A login a failed export left pending in the login home is DISCARDED FIRST
// ([Provider.discardLoginHome]), before the vendor's command runs: that command
// would otherwise begin by carrying the pending login out — over the very login
// it is about to revoke — and a carry that failed again failed the logout over
// a login the logout was discarding anyway.
//
// The vendor's command makes no login, so what it leaves is carried out as a
// refresh ([syncRefresh]) — which a login the CLI has just revoked never is:
// it can only write over the very login it was seeded from, and a home whose
// login is gone writes nothing. The removal takes the shared login's lock, so
// a seat carrying a refresh out at the same moment either lands before it,
// and is removed with the rest, or finds the login gone and keeps it gone; and
// it moves the operator epoch on ([Workspace.bumpLoginEpoch]), so a pending
// login the discard could not reach — another command held the login home —
// is never carried out over the logout later.
//
// AND IT IS DURABLE AGAINST THE CONFIGURED BUNDLE: under the same lock, before
// anything is removed, it records the logout beside the operator epoch
// ([Workspace.markLoggedOut]), and the automatic restore declines while that
// record stands ([ErrLoggedOut]) — otherwise the engine's next boot or apply, or
// the next `crewlet llm status`, restored the very login the operator had just
// removed. A login of record written afterwards clears it. What the operator is
// told about the bundle itself is [Provider.LogoutNotice].
func (p *Provider) Logout(ctx context.Context, out, errOut io.Writer) error {
	if err := p.discardLoginHome(ctx); err != nil {
		return fmt.Errorf("cliagent: the login was NOT removed: %w", err)
	}
	var vendorErr error
	if len(p.profile.LogoutArgs) > 0 {
		_, vendorErr = p.runInCredentialHome(ctx, syncRefresh, p.profile.LogoutArgs, nil, out, errOut)
	}
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("cliagent: the login was NOT removed: %w", err), vendorErr)
	}
	defer unlock()
	// THE RECORD FIRST: a removal that stopped part way leaves a directory
	// the record already keeps the bundle out of, and a record that cannot
	// be written still removes the login, the half an operator relies on.
	// THEN THE EPOCH, before the removal it counts ([Workspace.bumpLoginEpoch])
	// and on the same terms: one that cannot be moved still removes the login.
	//
	// The removal is ONE WRITE of the login less every file of it that is a
	// login ([Workspace.writeLoginLocked]), so a logout killed part way never
	// leaves one file of the login it was removing. What it keeps is what is
	// not a login ([Workspace.loginIn]): a settings file the CLI also keeps
	// its login in, holding none, is the CLI's record of itself rather than
	// anybody's sign-in. A write that stopped part way is the login of record,
	// so it is finished by the read and removed with the rest; one that cannot
	// be finished cannot be told apart file by file, and the logout REPLACES
	// it whole — it removes the login an operator asked removed either way.
	// Its install record goes in the same write ([LoginInstall]): a record
	// left behind would name a login that is gone, and order whatever is
	// restored or written next against it.
	removeErr := p.ws.markLoggedOut()
	if err := p.ws.bumpLoginEpoch(); err != nil {
		removeErr = errors.Join(removeErr, err)
	}
	var kept map[string][]byte
	if current, err := p.ws.readLoginLocked(); err == nil {
		kept = p.ws.notLoginIn(current)
	}
	if err := p.ws.installLoginLocked(ctx, p.key, kept, LoginInstall{}); err != nil {
		removeErr = errors.Join(removeErr, err)
	}
	if removeErr != nil {
		return errors.Join(removeErr, vendorErr)
	}
	if vendorErr != nil {
		return fmt.Errorf("cliagent: the login was removed from %s, but %q's own logout, "+
			"which revokes it with the vendor, did not finish: %w",
			p.ws.CredentialsDir(), p.agent, vendorErr)
	}
	return nil
}

// discardLoginHome empties the login home of a login a command left there and
// never carried out — what a failed export leaves for the next command to
// write — so that a logout is not followed by the next `status` carrying the
// login it removed back into the shared directory.
//
// A home another command is using is left to it: that command is a login in
// progress, an operator's own new sign-in, which a logout that came first does
// not undo — its result is written when it finishes. Whatever it began by
// carrying out loses to the logout's epoch ([Workspace.bumpLoginEpoch]).
func (p *Provider) discardLoginHome(ctx context.Context) error {
	unlock, err := p.ws.lockLoginHome(ctx)
	var busy *loginHomeBusyError
	if errors.As(err, &busy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	return p.ws.emptyLoginHome()
}

// Status asks the CLI who it is logged in as, IN WORDS, for a person reading
// `crewlet llm status`: the profile's [cliprofile.Profile.StatusTextArgs] where
// its status_args print JSON for the engine to read ([Provider.LoginStatus]),
// and status_args themselves where they are already the CLI's words.
//
// A status makes no login, so it is carried out as a refresh ([syncRefresh]):
// a home whose CLI changed nothing writes nothing, and one whose CLI did
// refresh writes only over the login it was seeded from — so asking who the
// entry is signed in as can neither revert a seat's refresh that landed while
// it ran nor bring back a login `crewlet llm logout` removed. (The command's
// restore of a configured bundle before it asks cannot either: it declines
// after a logout, [ErrLoggedOut].)
func (p *Provider) Status(ctx context.Context, out, errOut io.Writer) error {
	if len(p.profile.StatusArgs) == 0 {
		return fmt.Errorf("%w: the %q CLI has no status command — "+
			"`crewlet llm doctor` reports what can be observed instead",
			ErrNoLoginCommand, p.agent)
	}
	args := p.profile.StatusTextArgs
	if len(args) == 0 {
		args = p.profile.StatusArgs
	}
	_, err := p.runInCredentialHome(ctx, syncRefresh, args, nil, out, errOut)
	return err
}

// AdoptHostLogin copies the CLI's login out of a human's home directory into
// this provider's own, and reports what it took.
//
// A COPY, not a redirect: agents never write into the operator's personal
// credential file, so a fleet refreshing a token mid-session is not a
// surprise the operator gets handed. The cost is that both copies then
// descend from one refresh token, and a vendor that rotates refresh tokens
// can log out whichever side refreshes second — which is why the caller says
// so, and recommends a headless token where the vendor mints one.
//
// A login the CLI keeps in a database of its own is taken the only way it can
// leave one, through the CLI's own export run against that home
// ([Provider.exportHostLogin]) — run FIRST, before anything is written, so an
// export that fails adopts nothing rather than half a login.
//
// EVERY FILE IS READ BEFORE ANYTHING IS WRITTEN, and the login it makes is the
// human's whole: a file the human's home does not hold is removed from the
// shared login rather than kept beside the ones adopted, which would make a
// login of two sign-ins.
//
// It is the operator's own new login, so it is written over whatever the
// shared directory holds, under the shared login's lock: a seat carrying out a
// refresh of the old login at the same moment then finds the login moved and
// keeps this one ([syncRefresh]). And it moves the operator epoch on
// ([Workspace.bumpLoginEpoch]), so a login an earlier `crewlet llm login` left
// pending in the login home — its export failed — is never carried out over
// this one by the next command. A login of record, so a logout before it is
// over ([Workspace.clearLoggedOut]).
func (p *Provider) AdoptHostLogin(ctx context.Context, home string) ([]string, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cliagent: no home directory to adopt a login from "+
				"(%w) — name one with --home", err)
		}
	}
	var document []byte
	if p.profile.CredentialStore != nil {
		var err error
		document, err = p.exportHostLogin(ctx, home)
		if err != nil {
			return nil, fmt.Errorf("cliagent: reading the %q login in %q: %w", p.agent, home, err)
		}
	}
	login := map[string][]byte{}
	var taken []string
	for _, rel := range p.hostSources() {
		src, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return nil, err
		}
		content, present, err := readHostFile(src)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		login[filepath.Base(rel)] = content
		taken = append(taken, src)
	}
	if holdsLogin(document) {
		login[cliprofile.CredentialStoreDocument] = document
		taken = append(taken, p.hostStoreName(home))
	}
	if len(taken) == 0 {
		return nil, fmt.Errorf("cliagent: no %q login found under %q — log in with the "+
			"vendor's CLI first, or name a different home with --home", p.agent, home)
	}
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// THE EPOCH FIRST, before the write it counts ([Workspace.bumpLoginEpoch]):
	// one that cannot be moved writes nothing, since a login of record that
	// landed with no epoch behind it reads as no operator's write at all.
	if err = p.ws.bumpLoginEpoch(); err != nil {
		return nil, err
	}
	// THE HOST'S LOGIN, WHOLE, AS ONE WRITE: every file it holds, and none of
	// the files it does not — a file of the login it replaces left beside it
	// would make a login of two sign-ins — put in place in one step a seat
	// starting its generation now reads before or after, never part way,
	// with a new install record ([LoginInstall]), since it is a login
	// installed now, and newer than any bundle exported before it.
	install, err := newLoginInstall(time.Now())
	if err != nil {
		return nil, err
	}
	if err := p.ws.installLoginLocked(ctx, p.key, login, install); err != nil {
		return nil, err
	}
	if err := p.ws.clearLoggedOut(); err != nil {
		return nil, err
	}
	return taken, nil
}

// readHostFile reads one file of a login a human's home holds, reporting false
// where there is none, or where what is there is not a regular file — not a
// login file this command could adopt. One larger than
// [hostbox.MaxCredentialBytes] is refused rather than adopted: the shared login
// would refuse to hold it.
func readHostFile(src string) ([]byte, bool, error) {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cliagent: reading %q: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if info.Size() > hostbox.MaxCredentialBytes {
		return nil, false, fmt.Errorf("cliagent: %q is larger than %d bytes, which is not a login",
			src, hostbox.MaxCredentialBytes)
	}
	content, err := os.ReadFile(src) //nolint:gosec // a path the profile names under the home
	if err != nil {
		return nil, false, fmt.Errorf("cliagent: reading %q: %w", src, err)
	}
	return content, true, nil
}

// HostLogin reports where this CLI's login sits in a human's home directory,
// whether or not it has been adopted.
//
// So that "no sign-in" on a machine where the CLI plainly works explains
// itself, rather than sending an operator to look for a bug that is a missing
// `-from-host`.
//
// A login held in the CLI's own database is found by asking the CLI to export
// it — the one way to tell an empty database from one holding a login — and an
// export that fails reports nothing rather than an error: this is a hint on a
// report already explaining that nothing signs the entry in.
func (p *Provider) HostLogin(ctx context.Context, home string) []string {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil
		}
	}
	var found []string
	for _, rel := range p.hostSources() {
		src, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			continue
		}
		if info, err := os.Stat(src); err == nil && info.Mode().IsRegular() {
			found = append(found, src)
		}
	}
	if p.profile.CredentialStore != nil {
		if document, err := p.exportHostLogin(ctx, home); err == nil && holdsLogin(document) {
			found = append(found, p.hostStoreName(home))
		}
	}
	return found
}

// hostSources are the files a human's own login is kept in.
func (p *Provider) hostSources() []string {
	if len(p.profile.HostCredentialPaths) > 0 {
		return p.profile.HostCredentialPaths
	}
	return p.profile.CredentialPaths
}

// hostStoreName names a login found in a human's home through the CLI's own
// export, for the lines that list where a login was found: there is no file to
// name, so it names the home and the command that read it.
func (p *Provider) hostStoreName(home string) string {
	return fmt.Sprintf("the login %s holds in %s (`%s %s`)", p.agent, home,
		p.profile.Binary, strings.Join(p.profile.CredentialStore.ExportArgs, " "))
}

// ExportBundle packs the credential directory into one base64 blob for the
// encrypted secret store, so an engine rebuilt on every deploy comes up
// already authenticated.
//
// ONLY the credential files travel. Sessions, history and caches are never
// in a bundle: they are per-seat conversation state, and shipping them across
// hosts would restore one seat's transcripts onto another node's disk.
//
// And the bundle NAMES THE CLI it was exported from, in a manifest beside the
// files ([cliprofile.BundleManifest]): a bundle is keyed by the entry, which
// can move to another CLI, and the files' names cannot say whose they are —
// six CLIs keep an `auth.json` — so the restore reads the CLI from the
// manifest and refuses every other ([ErrForeignBundle]).
//
// The files are read as ONE login ([Workspace.Login]), so a bundle never packs
// a write that stopped part way — which every fresh volume restoring it would
// then hold. And only the files of it that are a login travel
// ([Workspace.loginIn]): a settings file the CLI also keeps its login in,
// holding none, is the CLI's record of itself on this host.
//
// And it CARRIES THE LOGIN'S INSTALL RECORD ([LoginInstall]) in its manifest,
// read in the same hold of the shared login's lock as the files, so it is the
// record that login was committed with: the restore orders the bundle's login
// against the one a node holds by it ([Provider.RestoreBundle]). A login with
// no record exports a bundle with none.
func (p *Provider) ExportBundle(ctx context.Context) (string, error) {
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return "", err
	}
	files, install, err := p.ws.loginAndInstallLocked()
	unlock()
	if err != nil {
		return "", err
	}
	login := p.ws.loginIn(files)
	if len(login) == 0 {
		return "", fmt.Errorf("cliagent: %q has no login to export — "+
			"run `crewlet llm login %s` first", p.agent, p.key)
	}
	packed := make([]bundleFile, 0, len(login))
	for _, name := range slices.Sorted(maps.Keys(login)) {
		packed = append(packed, bundleFile{name: name, data: login[name]})
	}
	manifest := bundleManifest{Agent: p.agent}
	if !install.IsZero() {
		manifest.Login = &install
	}
	return packBundle(manifest, packed)
}

// bundleManifest is what a bundle's [cliprofile.BundleManifest] entry holds.
//
// Decoded with unknown keys IGNORED, not refused: a bundle outlives the build
// that exported it — it sits in the secret store while a fleet upgrades one
// node at a time — so a later build may add a key, and a node still on this
// one that refused it would run the entry signed out over a bundle that is
// its CLI's.
type bundleManifest struct {
	// Agent is the CLI the bundle was exported from: the provider's agent,
	// which is the directory its files are kept in under the state directory
	// (`<state_dir>/<agent>`, [Shared]) — so an entry that leaves `agent`
	// out, and drives claude-code, writes "claude-code".
	Agent string `json:"agent"`
	// Login is the install record of the login the bundle carries
	// ([LoginInstall]), nil for a login exported with none. It is how a node
	// tells a newer login from the one it holds ([Provider.RestoreBundle]),
	// and the record every node restoring the bundle then holds, so each of
	// them answers the same [SignInID] for it.
	Login *LoginInstall `json:"login,omitempty"`
}

// bundleFile is one login file in a bundle, under its file name.
type bundleFile struct {
	name string
	data []byte
}

// packBundle writes a bundle: its manifest, then each file, as one base64 gzip
// tar. The format's ONE writer: [Provider.ExportBundle] writes
// through it, and so does every bundle this package's suite builds as an
// export would, so the suite cannot hold the restore to a format the export
// does not write.
func packBundle(m bundleManifest, files []bundleFile) (string, error) {
	manifest, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("cliagent: writing the bundle's manifest: %w", err)
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	entries := append([]bundleFile{{name: cliprofile.BundleManifest, data: manifest}}, files...)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Mode: 0o600,
			Size: int64(len(entry.data)), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return "", fmt.Errorf("cliagent: writing the bundle header: %w", err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			return "", fmt.Errorf("cliagent: writing the bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("cliagent: closing the bundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("cliagent: compressing the bundle: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes()), nil
}

// RestoreBundle restores a bundle as the entry's login when it is the NEWEST
// login this node knows of: into a credential directory holding no login, or
// over one installed before it.
//
// Validated on the way in, because an archive is an execution surface if it
// is unpacked on trust: only the profile's own credential file names, regular
// files only, no paths, each name once, and a total size cap. A bundle naming
// "../../.ssh/authorized_keys" is the attack this refuses by construction.
//
// And only a bundle that NAMES THIS ENTRY'S CLI as the one it was exported
// from ([ErrForeignBundle]). The file names alone cannot tell: a bundle is
// keyed by the entry, and an entry moved from codex to pi kept a bundle whose
// `auth.json` is a name pi keeps too — restored, it was written into pi's
// empty login as a credential pi cannot read, and as a login already present
// it turned away every later restore, the right bundle included.
//
// THE NEWEST LOGIN WINS, by the logins' install records ([LoginInstall]). The
// bundle replaces a login the directory holds only when the bundle carries a
// record that is a DIFFERENT login installed LATER ([LoginInstall.Newer]); a
// login with no record counts as installed at the zero instant, older than any
// recorded bundle. Restored only into an empty directory, as it once was, a
// fresh sign-in exported to the store (a device sign-in made through another
// node, a re-login after a revoked one) never reached a node that had ever held
// a login; restored whenever it differed, a boot-time blob would overwrite a
// login made on this node after it, and a refresh token the node had rotated
// since, which is how a fleet logs itself out. The record decides between the
// two: a refresh keeps its login's record, so a bundle of the login a node
// already holds is never newer than it, however stale its token.
//
// A BUNDLE WITH NO RECORD restores only into a directory holding no login:
// nothing in it says it is newer than anything, so it replaces nothing. Its
// login is then held with no record, and the first recorded bundle replaces it.
// A bundle that cannot be read at all (another CLI's, a broken archive) is no
// newer login either: where a login is held it stands and nothing is said,
// and only a directory holding none reports the refusal that leaves it
// signed out.
//
// The bundle's OWN record is written with it, never a new one: every node
// restoring one bundle then holds one record, so a second restore of it is no
// newer and writes nothing, and each node answers one [SignInID] for one login.
//
// NOT AN OPERATOR'S WRITE, unlike the import, where it fills an empty
// directory with the login the deployment was configured with: it leaves the
// operator epoch where it is ([Workspace.bumpLoginEpoch]), so a login an
// operator made on this node whose export failed, which is why the directory is
// empty, is still carried out over it by the next command. One that REPLACES a
// login moves the epoch as an operator's write does: it writes over a login
// exactly as `-from-host` does, and a sign-in running meanwhile, or a login a
// failed export left pending, must not read it as a refresh of the login it
// began from.
//
// And it DECLINES AFTER A LOGOUT, answering [ErrLoggedOut], whatever the
// bundle's record: the directory a logout empties is one an operator emptied,
// and refilling it from the deployment's configuration undid the logout at the
// next boot, apply or `status` ([Workspace.markLoggedOut]).
//
// A bundle that is not newer is the ordinary case at every boot and not an
// error; [Provider.ImportBundle] is the operator's form, which says so.
func (p *Provider) RestoreBundle(ctx context.Context, blob string) error {
	if err := p.restoreBundle(ctx, blob, false); err != nil && !errors.Is(err, ErrLoginPresent) {
		return err
	}
	return nil
}

// RestoreConfiguredBundle restores the entry's own configured bundle
// ([Config.CredentialBundle]) by [Provider.RestoreBundle], and does nothing for
// an entry that configures none.
//
// Called by whoever builds the entry to RUN it — the engine, for every epoch it
// builds, and the `crewlet llm` commands, which answer as that engine would —
// and never by [New], whose callers include `crewlet validate`. A failure is the
// caller's to report and never a refusal to build: a node with a broken bundle
// can still be logged in by hand, and taking a company down over one blob is
// the worse outcome. An entry `crewlet llm logout` emptied answers
// [ErrLoggedOut], which is no failure of the bundle and is reported as what it
// is: the logout, and how to sign in again.
func (p *Provider) RestoreConfiguredBundle(ctx context.Context) error {
	if p.bundle == "" {
		return nil
	}
	return p.RestoreBundle(ctx, p.bundle)
}

// ErrLoginPresent is a bundle import that found the entry already signed in,
// and so restored nothing.
var ErrLoginPresent = errors.New("cliagent: the entry already has a login, so the bundle was not restored")

// ErrForeignBundle is a bundle that does not name this entry's CLI as the one
// it was exported from — another CLI's, or one naming none — and so restored
// nothing. The error that wraps it names the CLI the bundle does name, and the
// export that replaces it.
var ErrForeignBundle = errors.New("cliagent: the credential bundle does not name this entry's CLI " +
	"as the one it was exported from, so it was not restored")

// ErrLoggedOut is an automatic restore that found the entry logged out by
// `crewlet llm logout` since its last login, and so restored nothing: see
// [Workspace.markLoggedOut]. The error that wraps it names the logout, how to
// sign in again and what retires the bundle.
var ErrLoggedOut = errors.New("cliagent: the entry was logged out, so its configured credential " +
	"bundle was not restored")

// loggedOutError is [ErrLoggedOut] for this entry.
//
// It names the three commands that WRITE A LOGIN into the credential directory,
// which is what ends a logout, and, for an entry a headless token signs in
// ([Provider.headlessTokenRoute]), says that a token does not: a token signs
// the entry in through its environment and writes nothing there, so after
// `-token-stdin`, or a `-capture-token` whose sign-in left no login file, the
// logout stands and the bundle stays out.
func (p *Provider) loggedOutError() error {
	token := ""
	if p.headlessTokenRoute() != "" {
		token = " (a headless token signs the entry in without ending the logout)"
	}
	return fmt.Errorf("%w: `crewlet llm logout` removed the login in %s, and the bundle stays out "+
		"until a login is written there again — sign in with %s%s; a state directory with no "+
		"record of the logout (a fresh volume) restores it, so to retire the bundle %s",
		ErrLoggedOut, p.ws.CredentialsDir(), p.loginRoutes(), token, p.retireBundle())
}

// headlessTokenRoute is how a headless token signs this entry in, worded for
// the messages that say one does without ending a logout
// ([Provider.LogoutNotice], [Provider.loggedOutError]), or "" where none does.
//
// ONLY WHERE THE ENTRY TAKES ONE. Said of every entry, it named
// `-capture-token` and `-token-stdin` to a CLI whose profile names no token
// variable — codex, like every built-in one but claude-code — which `crewlet
// llm login` refuses both for; to one whose token variable is outside the
// reference grammar ([misnamedVariable]), under which a token could be neither
// stored nor referenced; and to an api-key or inherit-env entry, whose mode
// that command refuses a stored token for: api-key removes the variable from
// the CLI's environment, and inherit-env forwards it from the engine's own and
// never reads the secret store. So a token is named only under a variable a
// store can hold ([Provider.TokenCanBeStored]), `-capture-token` only where the
// CLI mints one ([Provider.MintsHeadlessToken]), and by the mode: stored under
// subscription, exported under inherit-env, and never under api-key.
func (p *Provider) headlessTokenRoute() string {
	if !p.TokenCanBeStored() {
		return ""
	}
	sign, fromEnv := p.ModeSignIn()
	switch {
	case !fromEnv && p.MintsHeadlessToken():
		return "a headless token from `-capture-token` or `-token-stdin`"
	case !fromEnv:
		return "a headless token from `-token-stdin`"
	case sign.Mode == AuthInheritEnv && p.MintsHeadlessToken():
		return fmt.Sprintf("a headless %s exported in the engine's environment, which "+
			"`-capture-token -print-token` mints,", p.profile.TokenEnv)
	case sign.Mode == AuthInheritEnv:
		return fmt.Sprintf("a headless %s exported in the engine's environment", p.profile.TokenEnv)
	}
	return ""
}

// loginRoutes names the commands that write a login into this entry's
// credential directory — what ends a logout — as THIS PROFILE offers them.
//
// From the profile, never a fixed list: a fixed list named `crewlet llm login
// <key>` to an entry whose CLI declares no login subcommand (qwen-code, grok,
// gemini-cli), and the one remedy offered first was a command that refuses.
// `-from-host` needs a login it can find in a person's home, the CLI's own
// export or a file the profile names; an import needs nothing, so it is always
// there.
func (p *Provider) loginRoutes() string {
	var routes []string
	if len(p.profile.LoginArgs) > 0 {
		routes = append(routes, fmt.Sprintf("`crewlet llm login %s`", p.key))
	}
	if p.profile.StdinLogin != nil {
		routes = append(routes, fmt.Sprintf("`crewlet llm login %s -username <name> -password-stdin`", p.key))
	}
	if p.profile.CredentialStore != nil || len(p.hostSources()) > 0 {
		routes = append(routes, fmt.Sprintf("`crewlet llm login %s -from-host`", p.key))
	}
	routes = append(routes, fmt.Sprintf("`crewlet llm import %s`", p.key))
	if len(routes) == 1 {
		return routes[0]
	}
	return strings.Join(routes[:len(routes)-1], ", ") + " or " + routes[len(routes)-1]
}

// LogoutNotice is what `crewlet llm logout` tells the operator about the
// entry's configured credential bundle while the logout's record stands
// ([Workspace.markLoggedOut]): that nothing restores the bundle onto this host
// until a login is written again — which a headless token, where one signs the
// entry in ([Provider.headlessTokenRoute]), is not — that a
// state directory with no record of the logout, a fresh volume, still does,
// and what retires it. It is "" for an entry that configures no bundle, and
// while no record stands: a logout that stopped before writing one leaves a
// directory the next restore does refill, and saying otherwise would be false.
//
// READ OFF THE RECORD, never off whether [Provider.Logout] returned an error: a
// vendor logout that fails after the login was removed fails the command with
// the record written all the same, and the operator revoking a compromised
// login — the case the notice is for — was told nothing about the bundle now
// held back, nor about what retires it for a fresh volume. An error is a record
// that cannot be read, which the restore would answer with too.
func (p *Provider) LogoutNotice() (string, error) {
	if p.bundle == "" {
		return "", nil
	}
	if out, err := p.ws.loggedOut(); err != nil || !out {
		return "", err
	}
	routes := p.loginRoutes()
	if token := p.headlessTokenRoute(); token != "" {
		routes += "; " + token + " signs it in without ending this"
	}
	return fmt.Sprintf("Its configured credential bundle (%s) is not restored onto this host until "+
		"a login is written into its credential directory again (%s) — not at the engine's "+
		"next boot or apply, and not by `crewlet llm status`, `export`, `doctor` or `list`. A state "+
		"directory with no record of this logout, such as a fresh volume, still restores it; to "+
		"retire the bundle, %s.",
		p.bundleSource(), routes, p.retireBundle()), nil
}

// bundleSource names where the entry's configured bundle came from.
func (p *Provider) bundleSource() string {
	if p.bundleConfigured {
		return fmt.Sprintf("providers.llm.%s.auth.credential_bundle", p.key)
	}
	return cliprofile.BundleVarName(p.key)
}

// retireBundle says what removes the entry's configured bundle, which is what
// a fresh state volume restores.
func (p *Provider) retireBundle() string {
	if p.bundleConfigured {
		return fmt.Sprintf("remove providers.llm.%s.auth.credential_bundle from the company "+
			"document, or the secret it references", p.key)
	}
	name := cliprofile.BundleVarName(p.key)
	return fmt.Sprintf("remove %s from the secret store (`crewlet secrets unset %s`) and from "+
		"the engine's environment", name, name)
}

// ImportBundle is [Provider.RestoreBundle] for an operator who asked for it,
// and so it answers [ErrLoginPresent] where a login is already in place rather
// than nothing.
//
// IT NEVER REPLACES A LOGIN, newer bundle or not, unlike the automatic restore:
// an operator is there to be told, and `crewlet llm logout` then the import is
// the way to say that this bundle, and not the login in place, is the one. The
// automatic restore has nobody to ask, so it decides by the install records.
//
// It writes the bundle's install record ([LoginInstall]), as the restore does,
// so the node that imports a bundle and every node that restores it hold one
// record for one login; a bundle that carries none is given a new one, since an
// operator installed it now, and with none it would count as older than every
// recorded bundle and be replaced by the next boot's restore.
//
// DECIDED UNDER THE SHARED LOGIN'S LOCK, with the writes: a caller that looked
// for a login first and then restored was told "restored" whenever a login
// landed between the two, about a bundle that had written nothing.
//
// An operator's write of the login of record, so it moves the operator epoch
// on ([Workspace.bumpLoginEpoch]): a login an earlier `crewlet llm login` left
// pending in the login home is never carried out over the bundle afterwards.
// And it is never declined after a logout, which it ends
// ([Workspace.clearLoggedOut]): an import is how an operator replaces a login
// they logged out.
func (p *Provider) ImportBundle(ctx context.Context, blob string) error {
	return p.restoreBundle(ctx, blob, true)
}

// restoreBundle is [Provider.RestoreBundle] and [Provider.ImportBundle], which
// differ only in whether the restore is the operator's own write of the login
// of record.
func (p *Provider) restoreBundle(ctx context.Context, blob string, operator bool) error {
	if operator {
		// The operator's form answers a login already in place before it
		// reads the bundle: it never replaces one (see ImportBundle).
		present, err := p.ws.HasLogin(ctx)
		if err != nil {
			return err
		}
		if present {
			return ErrLoginPresent
		}
	}
	if err := p.declineAfterLogout(operator); err != nil {
		return err
	}
	staged, bundled, err := p.stageBundle(blob)
	if err != nil {
		// A BUNDLE THAT CANNOT BE READ IS NO NEWER LOGIN: a node that holds
		// one keeps it, as the engine runs it, and the restore says nothing,
		// as it did when a bundle reached only an empty directory. Reported
		// there, a bundle another CLI's left behind refused the very
		// `crewlet llm export` that replaces it, on the node signed in to the
		// new CLI. Where nothing is held the refusal is what explains a
		// signed-out entry, so it is reported.
		if present, hasErr := p.ws.HasLogin(ctx); !operator && hasErr == nil && present {
			return ErrLoginPresent
		}
		return err
	}

	// DECIDED UNDER THE SHARED LOGIN'S LOCK, with the writes: a login that
	// landed while the bundle was being validated is read here with the
	// record it was committed with, and is the fresher one unless the bundle
	// is newer than it, as a logout that landed meanwhile is the newer
	// decision.
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, held, err := p.ws.loginAndInstallLocked()
	if err != nil {
		return err
	}
	replacing := len(p.ws.loginIn(current)) > 0
	if replacing && (operator || !bundled.Newer(held)) {
		return ErrLoginPresent
	}
	if err = p.declineAfterLogout(operator); err != nil {
		return err
	}
	install := bundled
	if operator && install.IsZero() {
		// AN OPERATOR'S IMPORT OF A BUNDLE WITH NO RECORD is still a login
		// installed now, by somebody who chose it: left with no record, it
		// counted as older than any recorded bundle, and the next boot's
		// restore of a configured one undid the import.
		if install, err = newLoginInstall(time.Now()); err != nil {
			return err
		}
	}
	if operator || replacing {
		// An operator's own login of record, or a restore that writes over a
		// login as one does, counted BEFORE it is written
		// ([Workspace.bumpLoginEpoch]): an epoch that cannot be moved writes
		// nothing.
		if err = p.ws.bumpLoginEpoch(); err != nil {
			return err
		}
	}
	// THE BUNDLE'S LOGIN WHOLE, beside the files of the directory that are no
	// login (a settings file the CLI also keeps its login in, holding none),
	// which a bundle never carries ([Provider.ExportBundle]): a file of the
	// login it replaces left beside it would make a login of two sign-ins.
	next := p.ws.notLoginIn(current)
	maps.Copy(next, staged)
	if err = p.ws.installLoginLocked(ctx, p.key, next, install); err != nil {
		return err
	}
	if operator {
		// And a logout before it is over.
		return p.ws.clearLoggedOut()
	}
	return nil
}

// notLoginIn is the part of files, the shared login as read, that is NOT a
// login ([Workspace.loginIn]): the settings files a write that replaces or
// removes the login keeps, as the CLI's record of itself rather than anybody's
// sign-in.
func (w *Workspace) notLoginIn(files map[string][]byte) map[string][]byte {
	out := maps.Clone(files)
	if out == nil {
		out = map[string][]byte{}
	}
	maps.DeleteFunc(out, w.profile.HoldsLogin)
	return out
}

// stageBundle unpacks a bundle into the login it holds, by file name, and the
// install record its manifest carries (zero for none), writing nothing.
//
// VALIDATED WHOLE, THEN WRITTEN WHOLE. Every entry is checked and held before
// anything reaches the disk, because the checks reject a bundle mid-archive:
// writing as the loop went left the rejected bundle's earlier files behind,
// and HasLogin reads a directory with any file of a login in it as one — so a
// refused restore could leave a half-populated home that the next boot
// declines to repair. The write itself is ONE STEP for the same reason
// ([Workspace.installLoginLocked]): written a file at a time, a restore killed or
// refused by the disk between two of them left exactly that half.
func (p *Provider) stageBundle(blob string) (map[string][]byte, LoginInstall, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle is not base64: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle is not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	allowed := map[string]bool{}
	for _, name := range p.ws.loginNames() {
		allowed[name] = true
	}

	// REFUSED PAST THE CAP, not clipped to it. io.LimitReader stops at its
	// limit and hands the tar reader a clean io.EOF, which the loop below
	// cannot tell from the end of a whole archive — so an over-large bundle
	// used to restore whatever files fitted and report success. Reading one
	// byte past the cap is what makes "too large" a distinct answer from
	// "finished".
	decompressed, err := io.ReadAll(io.LimitReader(zr, maxBundle+1))
	if err != nil {
		return nil, LoginInstall{}, fmt.Errorf("cliagent: reading the credential bundle: %w", err)
	}
	if len(decompressed) > maxBundle {
		return nil, LoginInstall{}, fmt.Errorf(
			"cliagent: the credential bundle is larger than %d bytes decompressed, "+
				"so it was not unpacked — a bundle this size is not a login; "+
				"re-export it with `crewlet llm export %s`", maxBundle, p.key)
	}

	// The SHAPE of each entry is judged as it is read — a link, a path or an
	// oversized file is refused there, whatever else the bundle says — and
	// WHOSE it is once the whole archive is read: the manifest is an entry
	// like any other, and a bundle from another CLI is refused as that,
	// naming the move behind it, rather than as whichever of its files came
	// first and is not a name this CLI keeps.
	var (
		manifest     []byte
		haveManifest bool
		order        []string
	)
	staged := make(map[string][]byte, len(allowed))
	tr := tar.NewReader(bytes.NewReader(decompressed))
	for {
		header, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: reading the credential bundle: %w", nextErr)
		}
		if header.Typeflag != tar.TypeReg {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"regular file", header.Name)
		}
		name := filepath.Base(header.Name)
		if name != header.Name {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"%q credential file", header.Name, p.agent)
		}
		data, readErr := io.ReadAll(io.LimitReader(tr, maxBundle+1))
		if readErr != nil {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: reading %q from the bundle: %w", name, readErr)
		}
		if len(data) > maxBundle {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: %q in the credential bundle is larger than %d bytes",
				name, maxBundle)
		}
		// ONCE EACH: a name held twice is two answers to one question, and
		// the last of them winning is a choice nothing made — for the
		// manifest, which CLI the bundle is.
		if _, twice := staged[name]; twice || (name == cliprofile.BundleManifest && haveManifest) {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle holds %q twice, so it was not "+
				"unpacked — re-export it with `crewlet llm export %s`", name, p.key)
		}
		if name == cliprofile.BundleManifest {
			// NEVER WRITTEN: it says whose the files are, and is no file of
			// the login. No profile may keep a login file under this name
			// ([cliprofile.Profile.Validate]), so it is never one of them.
			manifest, haveManifest = data, true
			continue
		}
		staged[name] = data
		order = append(order, name)
	}
	named, err := p.checkBundleCLI(manifest, haveManifest)
	if err != nil {
		return nil, LoginInstall{}, err
	}
	var install LoginInstall
	if named.Login != nil {
		// A RECORD THAT CANNOT BE ORDERED IS REFUSED, not read as none: a
		// bundle that says it carries a record and carries no usable one is
		// not one `crewlet llm export` wrote, and taken as recordless it
		// would quietly lose to every login it was exported to replace.
		if err := named.Login.check(); err != nil {
			return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle's install record "+
				"cannot be read (%v), so it was not restored: export it again (`crewlet llm export %s`)",
				err, p.key)
		}
		install = *named.Login
	}
	for _, name := range order {
		if !allowed[name] {
			// The bundle is this CLI's, by its manifest, and still holds a
			// file this entry does not keep: one exported while the entry's
			// credential_paths said otherwise, or not by `crewlet llm export`.
			return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle holds %q, which is not a %q "+
				"credential file (it keeps %s) — re-export it with `crewlet llm export %s`",
				name, p.agent, strings.Join(slices.Sorted(maps.Keys(allowed)), ", "), p.key)
		}
	}
	if len(staged) == 0 {
		// A bundle `crewlet llm export` writes always holds a login: it
		// refuses an entry with none. One that holds none restored nothing,
		// and `crewlet llm import` said "Restored" over an empty directory.
		return nil, LoginInstall{}, fmt.Errorf("cliagent: the credential bundle holds no %q login file, so there is "+
			"nothing to restore — export it again (`crewlet llm export %s`) where %q is signed in",
			p.agent, p.key, p.agent)
	}
	return staged, install, nil
}

// checkBundleCLI answers [ErrForeignBundle] unless a bundle's manifest names
// this entry's CLI — the provider's agent, the same name its files are kept
// under in the state directory, as the one the bundle was exported from, and
// the manifest otherwise.
//
// A bundle naming no CLI is refused as one naming another: nothing in it says
// whose its files are, and a file name is the guess this replaces.
func (p *Provider) checkBundleCLI(manifest []byte, found bool) (bundleManifest, error) {
	none := fmt.Sprintf("it holds no %s", cliprofile.BundleManifest)
	if found {
		var named bundleManifest
		err := json.Unmarshal(manifest, &named)
		switch {
		case err != nil:
			none = fmt.Sprintf("its %s does not parse: %v", cliprofile.BundleManifest, err)
		case named.Agent == "":
			none = fmt.Sprintf("its %s names no agent", cliprofile.BundleManifest)
		case named.Agent == p.agent:
			return named, nil
		default:
			// A BUNDLE IS KEYED BY THE ENTRY (CREWLET_LLM_CLI_<KEY>_CREDENTIALS,
			// or the entry's own auth.credential_bundle), not by its CLI, so
			// the bundle an entry was configured with before it moved to
			// another CLI is still the one restored after the move — refused
			// at every boot and apply until it is replaced, which is the
			// remedy to name.
			return bundleManifest{}, fmt.Errorf("%w: it names %q, and %q runs %q; a bundle restores only into "+
				"the CLI it was exported from, so one exported before %q moved to %q, or for "+
				"another entry, is refused until it is replaced: sign %q in and export its "+
				"bundle again (`crewlet llm export %s`)",
				ErrForeignBundle, named.Agent, p.key, p.agent, p.key, p.agent, p.agent, p.key)
		}
	}
	return bundleManifest{}, fmt.Errorf("%w: it names none (%s), and a bundle restores only into the CLI it was "+
		"exported from, so one that cannot say which is refused until it is replaced: sign %q "+
		"in and export its bundle again (`crewlet llm export %s`)",
		ErrForeignBundle, none, p.agent, p.key)
}

// declineAfterLogout answers [ErrLoggedOut] for an automatic restore while a
// logout's record stands ([Workspace.markLoggedOut]). An operator's import is
// never declined: it is the write that ends the logout.
func (p *Provider) declineAfterLogout(operator bool) error {
	if operator {
		return nil
	}
	out, err := p.ws.loggedOut()
	if err != nil {
		return err
	}
	if out {
		return p.loggedOutError()
	}
	return nil
}

// runInCredentialHome runs one of the CLI's own auth commands in the LOGIN
// HOME, which is seeded from and carried back to the shared credential
// directory rather than a seat's, and reports whether the home held a login
// once the command was done.
//
// A login has to write where every seat's checkout is seeded FROM, and a seat
// home is pruned; brokering a login into one would delete the credential
// moments later.
//
// mode is whether the command MAKES a login ([syncLogin]: login, a capture, a
// credential login — the one kind of command that may create the shared login,
// and whose result is written over whatever the shared directory holds by
// then) or only USES one ([syncRefresh]: status, a vendor's own logout).
//
// The whole of it holds the login home's lock ([Workspace.lockLoginHome]): it
// prunes the home on the way in and out, and a second command doing that under
// a first's browser flow deleted the database the first was about to store
// its login in.
//
// It sweeps NO SEAT'S HOME, though a seat's generation can be left open just
// as this home's can ([Workspace.sweep]): the engine sweeps those at every
// seat's first call, and a command doing it too would pay a CLI export per
// kept seat home — for a home kept because its export keeps failing, up to
// [auxiliaryTimeout] each, on every `crewlet llm status` — for a seed whose only
// readers are a login, which replaces it, and a status, which reads it.
func (p *Provider) runInCredentialHome(
	ctx context.Context, mode syncMode, args []string,
	in io.Reader, out, errOut io.Writer, extra ...map[string]string,
) (bool, error) {
	return p.inCredentialHome(ctx, mode, errOut, func(ctx context.Context, home string, env []string) error {
		cmd := exec.CommandContext(ctx, p.profile.Binary, args...) //nolint:gosec // args come from a validated profile
		cmd.Dir = home
		cmd.Env = env
		cmd.Stdin = in
		cmd.Stdout = out
		cmd.Stderr = errOut
		// NO procgroup.Set HERE, deliberately, and it is the one child in this
		// package that does without: an interactive login has to read the
		// operator's terminal, and a process in its own group is not in the
		// tty's FOREGROUND group, so it cannot read stdin and would be stopped
		// with SIGTTIN the moment it tried. The prompt is the whole point of
		// this command. (A command with no terminal to read, the device
		// sign-in and the login status, runs in a group of its own:
		// [DeviceLogin], [Provider.LoginStatus].)
		//
		// WaitDelay still applies. Without it, cancelling this context killed
		// the CLI while a forked helper kept the inherited pipes open, and Wait
		// blocked on EOF that was never coming: an operator's Ctrl+C left the
		// command hanging.
		cmd.WaitDelay = p.termGrace
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("cliagent: %s %s: %w", p.profile.Binary, strings.Join(args, " "), err)
		}
		return nil
	}, extra...)
}

// credentialCommand runs one auth command in the login home, given the home
// and the whole environment the command runs with, and answers how it ended.
type credentialCommand func(ctx context.Context, home string, env []string) error

// inCredentialHome is [Provider.runInCredentialHome] for any way of running the
// command: everything around it (the lock, the carry of what an earlier
// command left, the prune, the seed, the carry back and the prune after) is
// the same for a command attached to the operator's terminal and for one whose
// output the engine reads, so it is written once.
func (p *Provider) inCredentialHome(
	ctx context.Context, mode syncMode, errOut io.Writer, command credentialCommand,
	extra ...map[string]string,
) (bool, error) {
	home := p.ws.loginHome()
	cache := p.ws.loginCache()
	record := filepath.Join(p.ws.Root(), loginGenerationFile)
	if err := prepareHome(home, cache); err != nil {
		return false, err
	}
	if err := os.MkdirAll(p.ws.CredentialsDir(), 0o700); err != nil {
		return false, fmt.Errorf("cliagent: preparing %q: %w", p.ws.CredentialsDir(), err)
	}
	unlock, lockErr := p.ws.lockLoginHome(ctx)
	if lockErr != nil {
		return false, lockErr
	}
	defer unlock()
	// A COMMAND THAT DID NOT FINISH CARRYING ITS LOGIN OUT is finished
	// first, before the prune that would discard what it left: a login whose
	// export failed after the operator completed the browser flow, or one
	// whose process was killed before its export ran, is still in this home's
	// database and nowhere else. Its record is on disk because this command
	// is a process of its own, and the one that left it is gone.
	if _, err := p.ws.carryOut(ctx, home, cache, p, record, loginHomeRemedy(record)); err != nil {
		if mode != syncLogin {
			return false, err
		}
		// A LOGIN REPLACES WHAT IT CANNOT CARRY OUT. Whatever is kept here
		// — an earlier sign-in, a status's refresh — this command is about to
		// write a newer login over, so refusing it would leave the one
		// command that repairs a broken carry unable to run: an export that
		// fails for good wedged `crewlet llm login` behind its own leftovers.
		// It is said, on the operator's terminal and in the log, because the
		// kept login is gone with it.
		log.WarnContext(ctx, "cli_agent_login_discarded", "provider", p.key, "agent", p.agent,
			"error", err)
		if errOut != nil {
			_, _ = fmt.Fprintf(errOut, "crewlet: discarding the login an earlier command left "+
				"in %s, which could not be carried out (%v); signing in afresh replaces it\n", home, err)
		}
		if err := p.ws.emptyLoginHome(); err != nil {
			return false, err
		}
	}
	// The login home is seeded from and carried back to the shared
	// directory exactly like a seat's, so re-running a login updates the
	// one credential every seat reads — and PRUNED first exactly like one,
	// so a login the CLI keeps in its own database is imported into an
	// empty one rather than skipped by a database still holding an older
	// copy of the same credential.
	if err := p.ws.prune(home); err != nil {
		return false, err
	}
	gen, err := p.ws.seed(ctx, home, cache, p, mode)
	var refused *loginRefusedError
	switch {
	case err == nil:
	case mode == syncLogin && errors.As(err, &refused):
		// A LOGIN REPLACES A SHARED LOGIN THE CLI CANNOT READ, for the reason
		// it replaces one it cannot carry out: the refusal names `crewlet llm
		// login` as the remedy, and a login that stopped at the same refusal
		// left the operator in a loop with the fleet signed out. It starts
		// from an empty home, and the refused document stays the baseline
		// ([Workspace.seed]), so the login it stores is written over it.
		// Said on the terminal and in the log, because what was refused is
		// gone once the login lands. A status and a vendor's own logout still
		// refuse: neither makes a login to put in its place.
		log.WarnContext(ctx, "cli_agent_shared_login_refused", "provider", p.key, "agent", p.agent,
			"path", refused.path, "error", refused.err)
		if errOut != nil {
			_, _ = fmt.Fprintf(errOut, "crewlet: the CLI refused to import the shared login %s "+
				"(%v), so it cannot be read; signing in afresh replaces it\n", refused.path, refused.err)
		}
		if emptyErr := p.ws.emptyLoginHome(); emptyErr != nil {
			return false, emptyErr
		}
	default:
		return false, err
	}
	if err := writeGeneration(record, gen); err != nil {
		return false, err
	}
	checkout := &Checkout{Home: home, Cache: cache}
	env := map[string]string{}
	for k, v := range p.env {
		env[k] = v
	}
	for _, more := range extra {
		for k, v := range more {
			env[k] = v
		}
	}

	runErr := command(ctx, home, buildEnv(p.profile, checkout, env, p.auth))

	// Carried out even on failure: a login that authenticated and then
	// failed a later step still wrote a credential worth keeping — and on an
	// operator's Ctrl+C too, which is why the export does not inherit the
	// cancellation that ended the command.
	//
	// WITHOUT THE EPOCH IT IS ORDERED AFTER ([generation.After]), which orders
	// only a login carried out LATER: the one carried out now is the
	// operator's sign-in finishing, and it is written over whatever landed
	// while it ran, an operator's other write included — the later of two
	// sign-ins wins. The record on disk keeps it, for the next command should
	// this carry fail. The epoch it was SEEDED at stays
	// ([generation.SeededEpoch]): it decides only what becomes of a file the
	// sign-in removed.
	now := gen
	now.After = nil
	holds, syncErr := p.ws.syncCredentialsOut(context.WithoutCancel(ctx), home, cache, p, now)
	if syncErr != nil {
		// NOT PRUNED, and the record stays: the home is now the only place
		// the login this command made may exist, and the next `crewlet llm`
		// command carries it out before it prunes anything.
		var recordErr error
		if mode == syncLogin {
			// ORDERED BY WHEN THE SIGN-IN FINISHED, as the carry that just
			// failed would have been: an operator's write that landed while
			// the browser flow was open is one this login, finishing later,
			// would have been written over, and only a write made from here
			// on outranks it. "Here" is an epoch read UNDER THE SHARED
			// LOGIN'S LOCK, which an operator's write moves the epoch in
			// before it writes ([Workspace.orderPendingLogin]): read without
			// it, between the two, this login was ordered after a write that
			// landed after it. Where the lock cannot be taken or the epoch
			// read, the one read with the seed stays on the record — which
			// errs towards the operator's other write, never towards this one.
			// Not cancelled with the command, as its carry is not.
			recordErr = p.ws.orderPendingLogin(context.WithoutCancel(ctx), record, gen)
		}
		return holds, errors.Join(fmt.Errorf("cliagent: the login in %q could not be carried "+
			"back to %s, so it was kept there, and the next `crewlet llm` command that runs "+
			"the CLI for %q carries it out before anything else: %w",
			home, p.ws.CredentialsDir(), p.key, syncErr), recordErr, runErr)
	}
	// Emptied after as well, so the login home keeps NO second copy of the
	// login — a credential file or a database: the shared directory is the
	// login of record, and the next command here starts afresh from it. A
	// copy left behind is one a later command would carry back, which is how
	// a login `crewlet llm logout` removed came back on the next `status`.
	if err := p.ws.emptyLoginHome(); err != nil {
		return holds, err
	}
	return holds, runErr
}
