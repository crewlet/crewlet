package cliagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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
	if len(p.profile.CredentialPaths) == 0 && p.profile.CredentialStore == nil {
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
		return "", fmt.Errorf("%w: the %q CLI mints no headless token — "+
			"run `crewlet llm login` to broker its own login instead",
			ErrNoLoginCommand, p.agent)
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
		return fmt.Errorf("%w: the %q CLI authenticates through the vendor's browser "+
			"OAuth flow — there is no username/password login to drive. Run "+
			"`crewlet llm login` (which brokers that flow), or "+
			"`crewlet llm login -capture-token` where the vendor mints a headless "+
			"token. If your build of this CLI does accept a credential, declare it "+
			"under providers.llm.%s.overrides.stdin_login",
			ErrNoLoginCommand, p.agent, p.key)
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
	removeErr := p.ws.markLoggedOut()
	for _, path := range p.ws.LoginFiles() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			removeErr = errors.Join(removeErr, fmt.Errorf("cliagent: removing %q: %w", path, err))
		}
	}
	if err := p.ws.bumpLoginEpoch(); err != nil {
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

// Status asks the CLI who it is logged in as.
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
	_, err := p.runInCredentialHome(ctx, syncRefresh, p.profile.StatusArgs, nil, out, errOut)
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
	dest := p.ws.CredentialsDir()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, fmt.Errorf("cliagent: preparing %q: %w", dest, err)
	}
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var taken []string
	for _, rel := range p.hostSources() {
		src, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cliagent: reading %q: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		// REPLACED rather than rewritten in place: a seat starting its
		// generation now is seeded from this very file.
		if _, err := hostbox.CopyFileAtomic(src, filepath.Join(dest, filepath.Base(rel))); err != nil {
			return nil, fmt.Errorf("cliagent: copying %q: %w", src, err)
		}
		taken = append(taken, src)
	}
	if holdsLogin(document) {
		if err := hostbox.ReplaceFile(p.ws.documentPath(), bytes.NewReader(document)); err != nil {
			return nil, fmt.Errorf("cliagent: writing %q: %w", p.ws.documentPath(), err)
		}
		taken = append(taken, p.hostStoreName(home))
	}
	if len(taken) == 0 {
		return nil, fmt.Errorf("cliagent: no %q login found under %q — log in with the "+
			"vendor's CLI first, or name a different home with --home", p.agent, home)
	}
	if err := p.ws.clearLoggedOut(); err != nil {
		return nil, err
	}
	if err := p.ws.bumpLoginEpoch(); err != nil {
		return nil, err
	}
	return taken, nil
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
func (p *Provider) ExportBundle() (string, error) {
	files := p.ws.LoginFiles()
	if len(files) == 0 {
		return "", fmt.Errorf("cliagent: %q has no login to export — "+
			"run `crewlet llm login %s` first", p.agent, p.key)
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // path came from the workspace
		if err != nil {
			return "", fmt.Errorf("cliagent: reading %q: %w", path, err)
		}
		header := &tar.Header{
			Name: filepath.Base(path), Mode: 0o600,
			Size: int64(len(data)), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return "", fmt.Errorf("cliagent: writing the bundle header: %w", err)
		}
		if _, err := tw.Write(data); err != nil {
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

// RestoreBundle unpacks a bundle into an EMPTY credential directory.
//
// Validated on the way in, because an archive is an execution surface if it
// is unpacked on trust: only the profile's own credential file names, regular
// files only, no paths, and a total size cap. A bundle naming
// "../../.ssh/authorized_keys" is the attack this refuses by construction.
//
// It refuses to overwrite an existing login: a node that has been running has
// the fresher refresh token, and restoring a boot-time blob over it is how a
// fleet logs itself out. At boot that is the ordinary case and not an error;
// [Provider.ImportBundle] is the operator's form, which says so.
//
// NOT AN OPERATOR'S WRITE, unlike the import: it fills an empty directory with
// the login the deployment was configured with, so it leaves the operator
// epoch where it is ([Workspace.bumpLoginEpoch]) — and a login an operator made
// on this node whose export failed, which is why the directory is empty, is
// still carried out over it by the next command, being the newer of the two.
//
// And it DECLINES AFTER A LOGOUT, answering [ErrLoggedOut]: the directory a
// logout empties is one an operator emptied, and refilling it from the
// deployment's configuration undid the logout at the next boot, apply or
// `status` ([Workspace.markLoggedOut]).
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

// ErrLoggedOut is an automatic restore that found the entry logged out by
// `crewlet llm logout` since its last login, and so restored nothing: see
// [Workspace.markLoggedOut]. The error that wraps it names the logout, how to
// sign in again and what retires the bundle.
var ErrLoggedOut = errors.New("cliagent: the entry was logged out, so its configured credential " +
	"bundle was not restored")

// loggedOutError is [ErrLoggedOut] for this entry.
//
// It names the three commands that WRITE A LOGIN into the credential directory,
// which is what ends a logout, and says that a headless token does not: a
// token signs the entry in through its environment and writes nothing there,
// so after `-token-stdin`, or a `-capture-token` whose sign-in left no login
// file, the logout stands and the bundle stays out.
func (p *Provider) loggedOutError() error {
	return fmt.Errorf("%w: `crewlet llm logout` removed the login in %s, and the bundle stays out "+
		"until a login is written there again — sign in with %s (a headless token signs the "+
		"entry in without ending the logout); a state directory with no record of the logout "+
		"(a fresh volume) restores it, so to retire the bundle %s",
		ErrLoggedOut, p.ws.CredentialsDir(), p.loginRoutes(), p.retireBundle())
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
// until a login is written again — which a headless token is not — that a
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
	return fmt.Sprintf("Its configured credential bundle (%s) is not restored onto this host until "+
		"a login is written into its credential directory again (%s; a headless token from "+
		"`-capture-token` or `-token-stdin` signs it in without ending this) — not at the engine's "+
		"next boot or apply, and not by `crewlet llm status`, `export`, `doctor` or `list`. A state "+
		"directory with no record of this logout, such as a fresh volume, still restores it; to "+
		"retire the bundle, %s.",
		p.bundleSource(), p.loginRoutes(), p.retireBundle()), nil
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
	dest := p.ws.CredentialsDir()
	if p.ws.HasLogin() {
		return ErrLoginPresent
	}
	if err := p.declineAfterLogout(operator); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		return fmt.Errorf("cliagent: the credential bundle is not base64: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("cliagent: the credential bundle is not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	allowed := map[string]bool{}
	for _, rel := range p.profile.CredentialPaths {
		allowed[filepath.Base(rel)] = true
	}
	if p.profile.CredentialStore != nil {
		allowed[cliprofile.CredentialStoreDocument] = true
	}
	if err = os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("cliagent: preparing %q: %w", dest, err)
	}

	// REFUSED PAST THE CAP, not clipped to it. io.LimitReader stops at its
	// limit and hands the tar reader a clean io.EOF, which the loop below
	// cannot tell from the end of a whole archive — so an over-large bundle
	// used to restore whatever files fitted and report success. Reading one
	// byte past the cap is what makes "too large" a distinct answer from
	// "finished".
	decompressed, err := io.ReadAll(io.LimitReader(zr, maxBundle+1))
	if err != nil {
		return fmt.Errorf("cliagent: reading the credential bundle: %w", err)
	}
	if len(decompressed) > maxBundle {
		return fmt.Errorf(
			"cliagent: the credential bundle is larger than %d bytes decompressed, "+
				"so it was not unpacked — a bundle this size is not a login; "+
				"re-export it with `crewlet llm export %s`", maxBundle, p.key)
	}

	// VALIDATED WHOLE, THEN WRITTEN. Every entry is checked and held before
	// anything reaches the disk, because the checks below reject a bundle
	// mid-archive: writing as the loop went left the rejected bundle's
	// earlier files behind, and HasLogin reads a directory with any
	// credential in it as a login — so a refused restore could leave a
	// half-populated home that the next boot declines to repair.
	staged := make(map[string][]byte, len(allowed))
	tr := tar.NewReader(bytes.NewReader(decompressed))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("cliagent: reading the credential bundle: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"regular file", header.Name)
		}
		name := filepath.Base(header.Name)
		if name != header.Name {
			return fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"%q credential file", header.Name, p.agent)
		}
		if !allowed[name] {
			// A PLAIN NAME THIS CLI DOES NOT KEEP is, short of a forged
			// bundle, ANOTHER CLI'S LOGIN. A bundle is keyed by the entry
			// (CREWLET_LLM_CLI_<KEY>_CREDENTIALS, or the entry's own
			// auth.credential_bundle), not by its CLI, so the bundle an entry
			// was configured with before it moved to another CLI is still
			// the one restored after the move — refused at every boot and
			// apply until it is replaced, which is the remedy to name.
			return fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"%q credential file — a bundle restores only into the CLI it was exported "+
				"from, so one exported before %q moved to %q is refused until it is replaced: "+
				"sign %q in and export its bundle again (`crewlet llm export %s`)",
				header.Name, p.agent, p.key, p.agent, p.agent, p.key)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBundle+1))
		if err != nil {
			return fmt.Errorf("cliagent: reading %q from the bundle: %w", name, err)
		}
		if len(data) > maxBundle {
			return fmt.Errorf("cliagent: %q in the credential bundle is larger than %d bytes",
				name, maxBundle)
		}
		staged[name] = data
	}

	// THE EMPTINESS ASKED AGAIN UNDER THE SHARED LOGIN'S LOCK, with the
	// writes: the check above is the cheap path for a node that already has
	// a login, and a login that landed while the bundle was being validated
	// is the fresher one — as a logout that landed meanwhile is the newer
	// decision.
	unlock, lockErr := p.ws.lockShared(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	if p.ws.HasLogin() {
		return ErrLoginPresent
	}
	if err := p.declineAfterLogout(operator); err != nil {
		return err
	}
	for name, data := range staged {
		if err := hostbox.ReplaceFile(filepath.Join(dest, name), bytes.NewReader(data)); err != nil {
			return fmt.Errorf("cliagent: writing %q: %w", name, err)
		}
	}
	if operator {
		// The operator's own login of record, so a logout before it is over.
		if err := p.ws.clearLoggedOut(); err != nil {
			return err
		}
		return p.ws.bumpLoginEpoch()
	}
	return nil
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
	home := p.ws.loginHome()
	cache := p.ws.loginCache()
	record := filepath.Join(p.ws.Root(), loginGenerationFile)
	for _, dir := range []string{home, cache, p.ws.CredentialsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("cliagent: preparing %q: %w", dir, err)
		}
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
	seeded, err := p.ws.seed(ctx, home, cache, p)
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
	gen := generation{Mode: mode, Seeded: seeded}
	if mode == syncLogin {
		// The epoch this login is ordered after, should it be carried out by
		// a later command rather than by this one ([generation.After]) — as
		// of now, and moved to when it finished should its carry fail below.
		epoch, err := p.ws.loginEpoch()
		if err != nil {
			return false, err
		}
		gen.After = &epoch
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

	cmd := exec.CommandContext(ctx, p.profile.Binary, args...) //nolint:gosec // args come from a validated profile
	cmd.Dir = home
	cmd.Env = buildEnv(p.profile, checkout, env, p.auth)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	// NO procgroup.Set HERE, deliberately, and it is the one child in this
	// package that does without: an interactive login has to read the
	// operator's terminal, and a process in its own group is not in the
	// tty's FOREGROUND group — so it cannot read stdin and would be stopped
	// with SIGTTIN the moment it tried. The prompt is the whole point of
	// this command.
	//
	// WaitDelay still applies. Without it, cancelling this context killed
	// the CLI while a forked helper kept the inherited pipes open, and Wait
	// blocked on EOF that was never coming — an operator's Ctrl+C left the
	// command hanging.
	cmd.WaitDelay = p.termGrace
	runErr := cmd.Run()
	if runErr != nil {
		runErr = fmt.Errorf("cliagent: %s %s: %w", p.profile.Binary, strings.Join(args, " "), runErr)
	}

	// Carried out even on failure: a login that authenticated and then
	// failed a later step still wrote a credential worth keeping — and on an
	// operator's Ctrl+C too, which is why the export does not inherit the
	// cancellation that ended the command.
	//
	// WITHOUT THE EPOCH, which orders only a login carried out LATER: the one
	// carried out now is the operator's sign-in finishing, and it is written
	// over whatever landed while it ran, an operator's other write included —
	// the later of two sign-ins wins. The record on disk keeps it, for the
	// next command should this carry fail.
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
			// on outranks it. Where the epoch cannot be read now, the one
			// read when the command began stays on the record — which errs
			// towards the operator's other write, never towards this one.
			if epoch, err := p.ws.loginEpoch(); err == nil {
				gen.After = &epoch
				recordErr = writeGeneration(record, gen)
			}
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
