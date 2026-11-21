package harness

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
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// maxBundle bounds a credential bundle on the way IN, decompressed.
//
// An archive is an execution surface if unpacked on trust: a bundle comes from the
// secret store every keyring node shares, so a compromised row must not exhaust
// memory or disk. One MiB is far above any real login (Claude's is under 2 KB).
//
// A stored bundle meets a SECOND BOUND on different bytes:
// [github.com/crewlet/crewlet/internal/secrets.MaxValueBytes] caps what the store
// keeps (base64 of gzip; `crewlet llm export -secret-store` checks it), this caps
// what a restore unpacks. Neither implies the other (a directory that compresses
// well packs far smaller than it unpacks), and this is not tied to the store's:
// `crewlet llm import` and auth.credential_bundle also bring bundles in.
const maxBundle = 1 << 20

// ErrNoLoginCommand is returned when a profile declares no way to do what
// was asked. Its own error so `crewlet llm login` can print the paragraph
// that explains the vendor's actual login route instead of a bare failure.
var ErrNoLoginCommand = errors.New("harness: this harness has no such login command")

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
// A login that leaves NO login is a failure, even where the harness exited 0: see
// [Provider.loginLeftNothing].
func (p *Provider) BrokerLogin(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	if len(p.profile.LoginArgs) == 0 {
		// NOT "it authenticates on first use", which this said once: a seat
		// never creates a login (see [Workspace.syncCredentialsOut]), and a
		// headless call has nobody to complete a sign-in for it anyway — so
		// the advice to make a call and then look was advice to watch it fail.
		//
		// And not "adopt it from the host" for a harness that keeps no login at
		// all: there is nothing on the host to adopt.
		if !p.profile.KeepsLogin() {
			return fmt.Errorf("%w: the %q harness keeps no login for Crewlet to hold — %s",
				ErrNoLoginCommand, p.harness, p.keyRemedy())
		}
		return fmt.Errorf("%w: the %q harness declares no login subcommand — sign in with "+
			"the harness itself and adopt that login with `crewlet llm login %s -from-host`, "+
			"or give the entry a key",
			ErrNoLoginCommand, p.harness, p.key)
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
// to …" on a nil error, and an operator who backed out of the harness's provider
// picker — or whose harness put the credential somewhere no seat is seeded from —
// was told exactly that about a directory holding nothing, and found out at
// the first call. Every shipped profile with a login command names where that
// login lands (its credential files, or the database its credential store
// exports), so a login that left none there is one no seat would ever have
// been signed in by.
func (p *Provider) loginLeftNothing() error {
	if !p.profile.KeepsLogin() {
		return fmt.Errorf("harness: the %q profile names no place a login is kept — no "+
			"credential_paths and no credential_store — so nothing its login command stores "+
			"can reach a seat; declare where it keeps one under "+
			"providers.llm.%s.overrides", p.harness, p.key)
	}
	return fmt.Errorf("harness: the %q harness exited without leaving a login where this "+
		"entry reads one (%s), so nothing was written to %s — if the sign-in did not "+
		"finish, run `crewlet llm login %s` again",
		p.harness, p.profile.LoginLocation(), p.ws.CredentialsDir(), p.key)
}

// CaptureToken runs the vendor's token-minting command and returns what it
// printed.
//
// Preferred wherever a vendor offers it: a headless token has no credential
// files to sync, no refresh-token rotation to race, and it survives an
// ephemeral container with no persistent volume.
//
// WHERE THE TOKEN PASSES: the command is a credential child, so on linux its
// output is captured in memory and the token reaches no disk; on darwin, which
// has no file in memory, it passes through an unlinked file in the
// workspace's captures directory, whose blocks keep it until they are reused.
func (p *Provider) CaptureToken(ctx context.Context, in io.Reader, errOut io.Writer) (string, error) {
	if len(p.profile.CaptureTokenArgs) == 0 {
		// The login route this harness has ([Provider.loginRemedy]): Gemini CLI,
		// Qwen Code and Grok declare no login subcommand, so "run `crewlet
		// llm login`" sent them to a second refusal.
		return "", fmt.Errorf("%w: the %q harness mints no headless token, so sign it in "+
			"with a login instead: %s", ErrNoLoginCommand, p.harness, p.loginRemedy(nil))
	}
	// MIRRORED WHILE IT IS CAPTURED. A minting command is INTERACTIVE: the harness
	// renders its UI whenever STDIN is a terminal (it is, for the browser sign-in)
	// and writes every byte of it to STDOUT: the sign-in URL, the "paste code
	// here" prompt. Capturing stdout alone left the operator a blank terminal
	// waiting for a code from a URL never shown (measured on Claude Code 2.1.x
	// with a tty on stdin: all UI on stdout, none on stderr).
	//
	// The mirror goes to errOut because `-print-token` writes the token to stdout,
	// and a vendor's UI there would corrupt what the operator pipes into a secret
	// manager. Every byte as read, not every line: the prompt has no line break.
	// ONE READER writes the mirror, so the streams never interleave. A credential
	// file left behind is kept (syncLogin) but not required: it is FOR the token.
	captures, err := p.ws.capturesDir()
	if err != nil {
		return "", err
	}
	command := string(p.profile.Binary) + " " + strings.Join(p.profile.CaptureTokenArgs, " ")
	var printed string
	if _, err := p.inCredentialHome(ctx, syncLogin, errOut,
		func(ctx context.Context, home string, env []string) error {
			// IN THE FOREGROUND, the operator's terminal on its stdin: it
			// reads the operator's keys, so it cannot be in a group of its
			// own ([Provider.runInCredentialHome] says why). Only its own
			// process is ever signalled.
			ch, err := startChild(ctx, childSpec{
				binary: p.profile.Binary, args: p.profile.CaptureTokenArgs, dir: home, env: env,
				stdin: in, outDir: captures, setting: p.stateSetting(),
				credential: true, foreground: true,
				out:   streamSpec{kind: oneAnswer, mirror: errOut},
				err:   streamSpec{kind: readToEnd, mirror: errOut},
				grace: p.termGrace, hooks: p.hooks, janitor: &p.janitor,
			})
			if err != nil {
				return fmt.Errorf("harness: %s: %w", command, err)
			}
			waitErr := <-ch.exited
			res := &rawResult{grace: p.termGrace}
			ended := ch.end(time.Now().Add(p.termGrace))
			got := ch.collect(ended)
			ch.close()
			res.heldStdout, res.cut = ended.held[0], ch.cut.Load()
			if s := ch.shrunk.Load(); s != nil {
				res.shrunk = *s
			}
			switch {
			case res.cut != nil:
				return fmt.Errorf("harness: %s: %s", command, res.unfinished())
			case waitErr != nil:
				return fmt.Errorf("harness: %s: %w", command, waitErr)
			case res.unfinished() != "":
				return fmt.Errorf("harness: %s exited 0, but %s", command, res.unfinished())
			}
			printed = got.out
			return nil
		}); err != nil {
		return "", err
	}
	token := lastPrintedLine(printed)
	if token == "" {
		return "", fmt.Errorf("harness: %q printed no token", p.harness)
	}
	return token, nil
}

// ansiEscape matches the control sequences a terminal UI writes between its
// words: CSI (cursor, colour), OSC (window title, and the hyperlinks a modern
// harness wraps a sign-in URL in), and the bare two-character escapes.
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

// noCredentialLogin is [Provider.CredentialLogin]'s refusal for a harness that
// declares no username/password login, naming the routes this entry has.
//
// THE MODE FIRST, THEN THE HARNESS: the mode decides what the entry reads, in
// the words [Provider.ModeSignIn] keeps for every sign-in message. By harness
// alone, api-key was offered `-capture-token`, a token its mode removes from the
// environment, and inherit-env one whose token lands in a store it never reads.
// So api-key is told its key only (a login would sit beside the key, and which
// the harness bills is the vendor's precedence), except where the profile names
// no key variable: then a login under subscription. inherit-env is told a login
// and a token to export; subscription, a login and a headless token.
// Only routes this harness has ([Provider.loginRemedy], [Provider.MintsHeadlessToken]).
func (p *Provider) noCredentialLogin() error {
	const lead = "%w: the %q harness authenticates through the vendor's browser OAuth flow — " +
		"there is no username/password login to drive"
	declare := fmt.Sprintf(". If your build of this harness does accept a credential, declare it "+
		"under providers.llm.%s.overrides.stdin_login", p.key)
	sign, fromEnv := p.ModeSignIn()
	switch {
	case fromEnv && sign.Mode == AuthAPIKey && len(sign.Variables) == 0 && len(sign.Misnamed) == 0:
		return fmt.Errorf(lead+": %s", ErrNoLoginCommand, p.harness, sign.Missing())
	case fromEnv && sign.Mode == AuthAPIKey:
		return fmt.Errorf(lead+", and a login is not this entry's sign-in: its auth.mode is "+
			"api-key, which %s: %s", ErrNoLoginCommand, p.harness, sign.Reads, sign.Supply)
	case fromEnv:
		mint := ""
		if p.MintsHeadlessToken() {
			mint = fmt.Sprintf(", or mint a headless %s to stdout, storing nothing, with "+
				"`crewlet llm login %s -capture-token -print-token`, and export it as %s in "+
				"the engine's environment", p.profile.TokenEnv, p.key, p.profile.TokenEnv)
		}
		return fmt.Errorf(lead+": %s; or, as its auth.mode is %s, which %s, %s%s%s",
			ErrNoLoginCommand, p.harness, p.loginRemedy(nil), sign.Mode, sign.Reads, sign.Supply,
			mint, declare)
	}
	mint := ""
	if p.MintsHeadlessToken() {
		mint = fmt.Sprintf(", or mint a headless %s with `crewlet llm login %s -capture-token`",
			p.profile.TokenEnv, p.key)
	}
	return fmt.Errorf(lead+": %s%s%s", ErrNoLoginCommand, p.harness, p.loginRemedy(nil), mint, declare)
}

// Logout revokes the login where the harness can, and removes the credential
// files either way: a failed vendor command must not leave a working login on
// disk that an operator believes removed.
//
// A login a failed export left pending is DISCARDED FIRST
// ([Provider.discardLoginHome]): the vendor's command would otherwise carry it
// out over the very login it is about to revoke. What that command leaves is
// carried out as a refresh ([syncRefresh]), which a revoked login never is.
// The removal takes the shared login's lock, so a seat's concurrent refresh
// lands before it and is removed too, or finds the login gone, and moves the
// operator epoch on ([Workspace.bumpLoginEpoch]).
func (p *Provider) Logout(ctx context.Context, out, errOut io.Writer) error {
	if err := p.discardLoginHome(ctx); err != nil {
		return fmt.Errorf("harness: the login was NOT removed: %w", err)
	}
	var vendorErr error
	if len(p.profile.LogoutArgs) > 0 {
		_, vendorErr = p.runInCredentialHome(ctx, syncRefresh, p.profile.LogoutArgs, nil, out, errOut)
	}
	unlock, err := p.ws.lockShared(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("harness: the login was NOT removed: %w", err), vendorErr)
	}
	defer unlock()
	// THE RECORD FIRST ([Workspace.markLoggedOut]), so the automatic restore
	// declines while it stands ([ErrLoggedOut]); otherwise the next boot, apply or
	// `crewlet llm status` restored the login just removed ([Provider.LogoutNotice]
	// tells the operator). A record or epoch that cannot be written still lets the
	// login go.
	//
	// The removal is ONE WRITE of the login less every file of it that is a
	// login ([Workspace.writeLoginLocked]), so a killed logout never leaves one
	// file of it; what is not a login stays ([Workspace.loginIn]). A write that
	// stopped part way cannot be told apart file by file, so the logout REPLACES
	// it whole, install record ([LoginInstall]) included.
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
		return fmt.Errorf("harness: the login was removed from %s, but %q's own logout, "+
			"which revokes it with the vendor, did not finish: %w",
			p.ws.CredentialsDir(), p.harness, vendorErr)
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

// Status asks the harness who it is logged in as, IN WORDS, for a person reading
// `crewlet llm status`: the profile's [harnessprofile.Profile.StatusTextArgs] where
// its status_args print JSON for the engine to read ([Provider.LoginStatus]),
// and status_args themselves where they are already the harness's words.
//
// A status makes no login, so it is carried out as a refresh ([syncRefresh]):
// a home whose harness changed nothing writes nothing, and one whose harness did
// refresh writes only over the login it was seeded from — so asking who the
// entry is signed in as can neither revert a seat's refresh that landed while
// it ran nor bring back a login `crewlet llm logout` removed. (The command's
// restore of a configured bundle before it asks cannot either: it declines
// after a logout, [ErrLoggedOut].)
func (p *Provider) Status(ctx context.Context, out, errOut io.Writer) error {
	if len(p.profile.StatusArgs) == 0 {
		return fmt.Errorf("%w: the %q harness has no status command — "+
			"`crewlet llm doctor` reports what can be observed instead",
			ErrNoLoginCommand, p.harness)
	}
	args := p.profile.StatusTextArgs
	if len(args) == 0 {
		args = p.profile.StatusArgs
	}
	_, err := p.runInCredentialHome(ctx, syncRefresh, args, nil, out, errOut)
	return err
}

// AdoptHostLogin copies the harness's login out of a human's home directory into
// this provider's own, and reports what it took.
//
// A COPY, not a redirect: agents never write the operator's personal file, and
// both copies descend from one refresh token, so a vendor that rotates them can
// log out whichever side refreshes second.
//
// A login in the harness's own database is taken through its export
// ([Provider.exportHostLogin]) FIRST, so a failure adopts nothing. Every file is
// read before any is written; one the home lacks leaves the shared login. Written
// under the shared lock, it ends a logout ([Workspace.clearLoggedOut]) and moves
// the epoch on ([Workspace.bumpLoginEpoch]) so a pending login never overwrites it.
func (p *Provider) AdoptHostLogin(ctx context.Context, home string) ([]string, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("harness: no home directory to adopt a login from "+
				"(%w) — name one with --home", err)
		}
	}
	var document []byte
	if p.profile.CredentialStore != nil {
		var err error
		document, err = p.exportHostLogin(ctx, home)
		if err != nil {
			return nil, fmt.Errorf("harness: reading the %q login in %q: %w", p.harness, home, err)
		}
	}
	login := map[string][]byte{}
	var taken []string
	for _, rel := range p.hostSources() {
		src, err := harnessprofile.UnderRoot(home, rel)
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
		login[harnessprofile.CredentialStoreDocument] = document
		taken = append(taken, p.hostStoreName(home))
	}
	if len(taken) == 0 {
		return nil, fmt.Errorf("harness: no %q login found under %q — log in with the "+
			"vendor's harness first, or name a different home with --home", p.harness, home)
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
		return nil, false, fmt.Errorf("harness: reading %q: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if info.Size() > hostbox.MaxCredentialBytes {
		return nil, false, fmt.Errorf("harness: %q is larger than %d bytes, which is not a login",
			src, hostbox.MaxCredentialBytes)
	}
	content, err := os.ReadFile(src) //nolint:gosec // a path the profile names under the home
	if err != nil {
		return nil, false, fmt.Errorf("harness: reading %q: %w", src, err)
	}
	return content, true, nil
}

// HostLogin reports where this harness's login sits in a human's home directory,
// whether or not it has been adopted.
//
// So that "no sign-in" on a machine where the harness plainly works explains
// itself, rather than sending an operator to look for a bug that is a missing
// `-from-host`.
//
// A login held in the harness's own database is found by asking the harness to export
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
		src, err := harnessprofile.UnderRoot(home, rel)
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

// hostStoreName names a login found in a human's home through the harness's own
// export, for the lines that list where a login was found: there is no file to
// name, so it names the home and the command that read it.
func (p *Provider) hostStoreName(home string) string {
	return fmt.Sprintf("the login %s holds in %s (`%s %s`)", p.harness, home,
		p.profile.Binary, strings.Join(p.profile.CredentialStore.ExportArgs, " "))
}

// ExportBundle packs the credential directory into one base64 blob for the
// encrypted secret store, so an engine rebuilt on every deploy comes up authenticated.
//
// ONLY credential files travel: sessions, history and caches are per-seat state
// that must not be restored onto another node. The manifest NAMES THE HARNESS
// ([harnessprofile.BundleManifest]), since file names cannot say whose they are
// (six keep an `auth.json`), and the restore refuses another's ([ErrForeignBundle]).
// The files are read as ONE login ([Workspace.Login]) so a half-written one is never
// packed, and only those that are a login ([Workspace.loginIn]). The login's INSTALL
// RECORD ([LoginInstall]) rides in the manifest, read under the same lock hold, so a
// restore can order the bundle against a node's login ([Provider.RestoreBundle]).
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
		return "", fmt.Errorf("harness: %q has no login to export — "+
			"run `crewlet llm login %s` first", p.harness, p.key)
	}
	packed := make([]bundleFile, 0, len(login))
	for _, name := range slices.Sorted(maps.Keys(login)) {
		packed = append(packed, bundleFile{name: name, data: login[name]})
	}
	manifest := bundleManifest{Harness: p.harness}
	if !install.IsZero() {
		manifest.Login = &install
	}
	return packBundle(manifest, packed)
}

// bundleManifest is what a bundle's [harnessprofile.BundleManifest] entry holds.
//
// Decoded with unknown keys IGNORED, not refused: a bundle outlives the build
// that exported it — it sits in the secret store while a fleet upgrades one
// node at a time — so a later build may add a key, and a node still on this
// one that refused it would run the entry signed out over a bundle that is
// its harness's.
type bundleManifest struct {
	// Harness is the harness the bundle was exported from: the provider's
	// harness, which is the directory its files are kept in under the state
	// directory (`<state_dir>/<harness>`, [Shared]) — so an entry that leaves
	// `harness` out, and drives claude-code, writes "claude-code".
	Harness string `json:"harness"`
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
		return "", fmt.Errorf("harness: writing the bundle's manifest: %w", err)
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	entries := append([]bundleFile{{name: harnessprofile.BundleManifest, data: manifest}}, files...)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Mode: 0o600,
			Size: int64(len(entry.data)), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return "", fmt.Errorf("harness: writing the bundle header: %w", err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			return "", fmt.Errorf("harness: writing the bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("harness: closing the bundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("harness: compressing the bundle: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes()), nil
}

// RestoreBundle restores a bundle as the entry's login when it is the NEWEST this
// node knows of (empty directory, or an older install). Not newer is the ordinary
// boot case, no error; [Provider.ImportBundle] is the operator's form.
//
// Validated as an execution surface: the profile's file names, regular files, no
// paths, each once, size capped. It must NAME THIS HARNESS ([ErrForeignBundle]):
// codex and pi both keep an `auth.json`. NEWEST WINS by install record
// ([LoginInstall.Newer]): a DIFFERENT login installed LATER replaces (a refresh keeps
// its record, so a stale token never does); no record counts as zero, restoring only
// into an empty directory. An unreadable bundle replaces nothing and is reported only
// to an empty directory. Its own record is written; the epoch ([Workspace.bumpLoginEpoch])
// moves only if it REPLACES a login. It DECLINES AFTER A LOGOUT ([ErrLoggedOut]).
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
var ErrLoginPresent = errors.New("harness: the entry already has a login, so the bundle was not restored")

// ErrForeignBundle is a bundle that does not name this entry's harness as the one
// it was exported from — another harness's, or one naming none — and so restored
// nothing. The error that wraps it names the harness the bundle does name, and the
// export that replaces it.
var ErrForeignBundle = errors.New("harness: the credential bundle does not name this entry's harness " +
	"as the one it was exported from, so it was not restored")

// ErrLoggedOut is an automatic restore that found the entry logged out by
// `crewlet llm logout` since its last login, and so restored nothing: see
// [Workspace.markLoggedOut]. The error that wraps it names the logout, how to
// sign in again and what retires the bundle.
var ErrLoggedOut = errors.New("harness: the entry was logged out, so its configured credential " +
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
// ONLY WHERE THE ENTRY TAKES ONE: `crewlet llm login` refuses `-capture-token` and
// `-token-stdin` for a harness with no token variable (every built-in but
// claude-code), one outside the reference grammar ([misnamedVariable]), and an
// api-key or inherit-env entry (api-key removes the variable; inherit-env never
// reads the store). So a token is named only under a variable a store can hold
// ([Provider.TokenCanBeStored]), `-capture-token` only where one is minted
// ([Provider.MintsHeadlessToken]), by mode: stored under subscription, exported
// under inherit-env, never under api-key.
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
// <key>` to an entry whose harness declares no login subcommand (qwen-code, grok,
// gemini-cli), and the one remedy offered first was a command that refuses.
// `-from-host` needs a login it can find in a person's home, the harness's own
// export or a file the profile names; an import needs nothing, so it is always
// there.
func (p *Provider) loginRoutes() string {
	var routes []words.Said
	if len(p.profile.LoginArgs) > 0 {
		routes = append(routes, words.Said(fmt.Sprintf("`crewlet llm login %s`", p.key)))
	}
	if p.profile.StdinLogin != nil {
		routes = append(routes, words.Said(fmt.Sprintf("`crewlet llm login %s -username <name> -password-stdin`", p.key)))
	}
	if p.profile.CredentialStore != nil || len(p.hostSources()) > 0 {
		routes = append(routes, words.Said(fmt.Sprintf("`crewlet llm login %s -from-host`", p.key)))
	}
	routes = append(routes, words.Said(fmt.Sprintf("`crewlet llm import %s`", p.key)))
	return words.Or(routes)
}

// LogoutNotice is what `crewlet llm logout` tells the operator about the entry's
// configured credential bundle while the logout's record stands
// ([Workspace.markLoggedOut]): that nothing restores it onto this host until a
// login is written again (a headless token, where one signs the entry in
// ([Provider.headlessTokenRoute]), is not one), that a fresh volume with no record
// still restores it, and what retires it. It is "" for an entry with no bundle and
// while no record stands: a logout that stopped before writing one leaves a
// directory the next restore refills, so saying otherwise would be false.
//
// READ OFF THE RECORD, never off whether [Provider.Logout] returned an error: a vendor
// logout failing after the login was removed still writes the record, and an operator
// revoking a compromised login was told nothing about the bundle held back.
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
	return harnessprofile.BundleVarName(p.key)
}

// retireBundle says what removes the entry's configured bundle, which is what
// a fresh state volume restores.
func (p *Provider) retireBundle() string {
	if p.bundleConfigured {
		return fmt.Sprintf("remove providers.llm.%s.auth.credential_bundle from the company "+
			"document, or the secret it references", p.key)
	}
	name := harnessprofile.BundleVarName(p.key)
	return fmt.Sprintf("remove %s from the secret store (`crewlet secrets unset %s`) and from "+
		"the engine's environment", name, name)
}

// ImportBundle is [Provider.RestoreBundle] for an operator who asked for it, so it
// answers [ErrLoginPresent] where a login is in place rather than nothing.
//
// IT NEVER REPLACES A LOGIN, newer bundle or not: an operator is there to be told,
// and `crewlet llm logout` then the import says this bundle is the one. It writes
// the bundle's install record ([LoginInstall]), a new one if it has none (installed
// now; with none it would count as older than every recorded bundle and be replaced
// at the next boot). The decision is made UNDER THE SHARED LOGIN'S LOCK, with the
// writes: a look-then-restore reported "restored" for a bundle that wrote nothing
// when a login landed between. As an operator's write it moves the epoch on
// ([Workspace.bumpLoginEpoch]) so a pending login is never carried out over it, and
// it is never declined after a logout, which it ends ([Workspace.clearLoggedOut]).
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
		// there, a bundle another harness's left behind refused the very
		// `crewlet llm export` that replaces it, on the node signed in to the
		// new harness. Where nothing is held the refusal is what explains a
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
	// login (a settings file the harness also keeps its login in, holding none),
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
// removes the login keeps, as the harness's record of itself rather than anybody's
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
		return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle is not base64: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle is not gzip: %w", err)
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
		return nil, LoginInstall{}, fmt.Errorf("harness: reading the credential bundle: %w", err)
	}
	if len(decompressed) > maxBundle {
		return nil, LoginInstall{}, fmt.Errorf(
			"harness: the credential bundle is larger than %d bytes decompressed, "+
				"so it was not unpacked — a bundle this size is not a login; "+
				"re-export it with `crewlet llm export %s`", maxBundle, p.key)
	}

	// The SHAPE of each entry is judged as it is read — a link, a path or an
	// oversized file is refused there, whatever else the bundle says — and
	// WHOSE it is once the whole archive is read: the manifest is an entry
	// like any other, and a bundle from another harness is refused as that,
	// naming the move behind it, rather than as whichever of its files came
	// first and is not a name this harness keeps.
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
			return nil, LoginInstall{}, fmt.Errorf("harness: reading the credential bundle: %w", nextErr)
		}
		if header.Typeflag != tar.TypeReg {
			return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle holds %q, which is not a "+
				"regular file", header.Name)
		}
		name := filepath.Base(header.Name)
		if name != header.Name {
			return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle holds %q, which is not a "+
				"%q credential file", header.Name, p.harness)
		}
		data, readErr := io.ReadAll(io.LimitReader(tr, maxBundle+1))
		if readErr != nil {
			return nil, LoginInstall{}, fmt.Errorf("harness: reading %q from the bundle: %w", name, readErr)
		}
		if len(data) > maxBundle {
			return nil, LoginInstall{}, fmt.Errorf("harness: %q in the credential bundle is larger than %d bytes",
				name, maxBundle)
		}
		// ONCE EACH: a name held twice is two answers to one question, and
		// the last of them winning is a choice nothing made — for the
		// manifest, which harness the bundle is.
		if _, twice := staged[name]; twice || (name == harnessprofile.BundleManifest && haveManifest) {
			return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle holds %q twice, so it was not "+
				"unpacked — re-export it with `crewlet llm export %s`", name, p.key)
		}
		if name == harnessprofile.BundleManifest {
			// NEVER WRITTEN: it says whose the files are, and is no file of
			// the login. No profile may keep a login file under this name
			// ([harnessprofile.Profile.Validate]), so it is never one of them.
			manifest, haveManifest = data, true
			continue
		}
		staged[name] = data
		order = append(order, name)
	}
	named, err := p.checkBundleHarness(manifest, haveManifest)
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
			return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle's install record "+
				"cannot be read (%v), so it was not restored: export it again (`crewlet llm export %s`)",
				err, p.key)
		}
		install = *named.Login
	}
	for _, name := range order {
		if !allowed[name] {
			// The bundle is this harness's, by its manifest, and still holds a
			// file this entry does not keep: one exported while the entry's
			// credential_paths said otherwise, or not by `crewlet llm export`.
			return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle holds %q, which is not a %q "+
				"credential file (it keeps %s) — re-export it with `crewlet llm export %s`",
				name, p.harness, strings.Join(slices.Sorted(maps.Keys(allowed)), ", "), p.key)
		}
	}
	if len(staged) == 0 {
		// A bundle `crewlet llm export` writes always holds a login: it
		// refuses an entry with none. One that holds none restored nothing,
		// and `crewlet llm import` said "Restored" over an empty directory.
		return nil, LoginInstall{}, fmt.Errorf("harness: the credential bundle holds no %q login file, so there is "+
			"nothing to restore — export it again (`crewlet llm export %s`) where %q is signed in",
			p.harness, p.key, p.harness)
	}
	return staged, install, nil
}

// checkBundleHarness answers [ErrForeignBundle] unless a bundle's manifest names
// this entry's harness — the provider's agent, the same name its files are kept
// under in the state directory, as the one the bundle was exported from, and
// the manifest otherwise.
//
// A bundle naming no harness is refused as one naming another: nothing in it says
// whose its files are, and a file name is the guess this replaces.
func (p *Provider) checkBundleHarness(manifest []byte, found bool) (bundleManifest, error) {
	none := fmt.Sprintf("it holds no %s", harnessprofile.BundleManifest)
	if found {
		var named bundleManifest
		err := json.Unmarshal(manifest, &named)
		switch {
		case err != nil:
			none = fmt.Sprintf("its %s does not parse: %v", harnessprofile.BundleManifest, err)
		case named.Harness == "":
			none = fmt.Sprintf("its %s names no agent", harnessprofile.BundleManifest)
		case named.Harness == p.harness:
			return named, nil
		default:
			// A BUNDLE IS KEYED BY THE ENTRY (CREWLET_LLM_CLI_<KEY>_CREDENTIALS,
			// or the entry's own auth.credential_bundle), not by its harness, so
			// the bundle an entry was configured with before it moved to
			// another harness is still the one restored after the move — refused
			// at every boot and apply until it is replaced, which is the
			// remedy to name.
			return bundleManifest{}, fmt.Errorf("%w: it names %q, and %q runs %q; a bundle restores only into "+
				"the harness it was exported from, so one exported before %q moved to %q, or for "+
				"another entry, is refused until it is replaced: sign %q in and export its "+
				"bundle again (`crewlet llm export %s`)",
				ErrForeignBundle, named.Harness, p.key, p.harness, p.key, p.harness, p.harness, p.key)
		}
	}
	return bundleManifest{}, fmt.Errorf("%w: it names none (%s), and a bundle restores only into the harness it was "+
		"exported from, so one that cannot say which is refused until it is replaced: sign %q "+
		"in and export its bundle again (`crewlet llm export %s`)",
		ErrForeignBundle, none, p.harness, p.key)
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

// runInCredentialHome runs one of the harness's own auth commands in the LOGIN HOME,
// seeded from and carried back to the shared credential directory (a seat's home is
// pruned, which would delete a login brokered into it), and reports whether the home
// held a login afterwards. mode: [syncLogin] MAKES a login (the one kind that may
// create the shared login, written over whatever it holds), [syncRefresh] only USES one.
//
// It holds the login home's lock ([Workspace.lockLoginHome]): a second command
// pruning the home deleted the first's browser-flow database. It sweeps NO SEAT'S HOME
// ([Workspace.sweep]), which the engine does at each seat's first call. It is, with
// [startChild], the one place that starts a harness whose output is NOT CAPTURED: stdout
// and stderr are the operator's terminal, handed down as *os.File with no pipe
// (childoutput_gate_test.go names this exception); [Provider.CaptureToken] captures.
func (p *Provider) runInCredentialHome(
	ctx context.Context, mode syncMode, args []string,
	in io.Reader, out, errOut io.Writer, extra ...map[string]string,
) (bool, error) {
	return p.inCredentialHome(ctx, mode, errOut, func(ctx context.Context, home string, env []string) error {
		cmd := exec.CommandContext(ctx, string(p.profile.Binary), args...) //nolint:gosec // args come from a validated profile
		cmd.Dir = home
		cmd.Env = env
		cmd.Stdin = in
		cmd.Stdout = out
		cmd.Stderr = errOut
		// NO procgroup.Set HERE, deliberately: an interactive login has to read the
		// operator's terminal, and a process in its own group is not in the
		// tty's FOREGROUND group, so it cannot read stdin and would be stopped
		// with SIGTTIN the moment it tried. The prompt is the whole point of
		// this command. (A command with no terminal to read, the device
		// sign-in and the login status, runs in a group of its own:
		// [DeviceLogin], [Provider.LoginStatus].)
		//
		// WaitDelay still applies. Without it, cancelling this context killed
		// the harness while a forked helper kept the inherited pipes open, and Wait
		// blocked on EOF that was never coming: an operator's Ctrl+C left the
		// command hanging.
		cmd.WaitDelay = p.termGrace
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("harness: %s %s: %w", p.profile.Binary, strings.Join(args, " "), err)
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
		return false, fmt.Errorf("harness: preparing %q: %w", p.ws.CredentialsDir(), err)
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
		log.WarnContext(ctx, "harness_login_discarded", "provider", p.key, "agent", p.harness,
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
	// so a login the harness keeps in its own database is imported into an
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
		// A LOGIN REPLACES A SHARED LOGIN THE HARNESS CANNOT READ, for the reason
		// it replaces one it cannot carry out: the refusal names `crewlet llm
		// login` as the remedy, and a login that stopped at the same refusal
		// left the operator in a loop with the fleet signed out. It starts
		// from an empty home, and the refused document stays the baseline
		// ([Workspace.seed]), so the login it stores is written over it.
		// Said on the terminal and in the log, because what was refused is
		// gone once the login lands. A status and a vendor's own logout still
		// refuse: neither makes a login to put in its place.
		log.WarnContext(ctx, "harness_shared_login_refused", "provider", p.key, "agent", p.harness,
			"path", refused.path, "error", refused.err)
		if errOut != nil {
			_, _ = fmt.Fprintf(errOut, "crewlet: the harness refused to import the shared login %s "+
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

	// Carried out even on failure: a login that authenticated and then failed a later
	// step still wrote a credential worth keeping, and on Ctrl+C too, which is why the
	// export does not inherit the cancellation.
	//
	// WITHOUT THE EPOCH IT IS ORDERED AFTER ([generation.After]), which orders only a
	// login carried out LATER: this one is the operator's sign-in finishing, written
	// over whatever landed while it ran, so the later of two sign-ins wins. The record
	// on disk keeps it for the next command should this carry fail. The epoch it was
	// SEEDED at stays ([generation.SeededEpoch]): it decides only what becomes of a
	// file the sign-in removed.
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
		return holds, errors.Join(fmt.Errorf("harness: the login in %q could not be carried "+
			"back to %s, so it was kept there, and the next `crewlet llm` command that runs "+
			"the harness for %q carries it out before anything else: %w",
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
