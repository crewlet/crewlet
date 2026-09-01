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
	"strings"
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
func (p *Provider) BrokerLogin(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	if len(p.profile.LoginArgs) == 0 {
		return fmt.Errorf("%w: %q authenticates on first use — run "+
			"`crewlet llm doctor` after a call to confirm the login took",
			ErrNoLoginCommand, p.agent)
	}
	return p.runInCredentialHome(ctx, p.profile.LoginArgs, in, out, errOut)
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
	var out bytes.Buffer
	if err := p.runInCredentialHome(ctx, p.profile.CaptureTokenArgs, in, &out, errOut); err != nil {
		return "", err
	}
	// The LAST non-empty line: these commands print instructions above the
	// token, and taking the whole output would store the instructions.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if token := strings.TrimSpace(lines[i]); token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf("cliagent: %q printed no token", p.agent)
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
			"`crewlet llm login --capture-token` where the vendor mints a headless "+
			"token. If your build of this CLI does accept a credential, declare it "+
			"under providers.llm.%s.cli.overrides.stdin_login",
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
	return p.runInCredentialHome(ctx, args, strings.NewReader(stdin), out, errOut, extra)
}

// Logout revokes the login where the CLI can, and removes the credential
// files either way.
//
// Both halves, and the local half unconditionally: a vendor command that
// failed must not leave a working login on disk that an operator believes
// they removed.
func (p *Provider) Logout(ctx context.Context, out, errOut io.Writer) error {
	var vendorErr error
	if len(p.profile.LogoutArgs) > 0 {
		vendorErr = p.runInCredentialHome(ctx, p.profile.LogoutArgs, nil, out, errOut)
	}
	var removeErr error
	for _, path := range p.ws.LoginFiles() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			removeErr = fmt.Errorf("cliagent: removing %q: %w", path, err)
		}
	}
	if removeErr != nil {
		return removeErr
	}
	return vendorErr
}

// Status asks the CLI who it is logged in as.
func (p *Provider) Status(ctx context.Context, out, errOut io.Writer) error {
	if len(p.profile.StatusArgs) == 0 {
		return fmt.Errorf("%w: the %q CLI has no status command — "+
			"`crewlet llm doctor` reports what can be observed instead",
			ErrNoLoginCommand, p.agent)
	}
	return p.runInCredentialHome(ctx, p.profile.StatusArgs, nil, out, errOut)
}

// AdoptHostLogin copies the CLI's credential files out of a human's home
// directory into this provider's own, and reports what it took.
//
// A COPY, not a redirect: agents never write into the operator's personal
// credential file, so a fleet refreshing a token mid-session is not a
// surprise the operator gets handed. The cost is that both copies then
// descend from one refresh token, and a vendor that rotates refresh tokens
// can log out whichever side refreshes second — which is why the caller says
// so, and recommends a headless token where the vendor mints one.
func (p *Provider) AdoptHostLogin(home string) ([]string, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cliagent: no home directory to adopt a login from "+
				"(%w) — name one with --home", err)
		}
	}
	sources := p.profile.HostCredentialPaths
	if len(sources) == 0 {
		sources = p.profile.CredentialPaths
	}
	dest := p.ws.CredentialsDir()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, fmt.Errorf("cliagent: preparing %q: %w", dest, err)
	}
	var taken []string
	for _, rel := range sources {
		src, err := underRoot(home, rel)
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
		if err := copyFile(src, filepath.Join(dest, filepath.Base(rel))); err != nil {
			return nil, err
		}
		taken = append(taken, src)
	}
	if len(taken) == 0 {
		return nil, fmt.Errorf("cliagent: no %q login found under %q — log in with the "+
			"vendor's CLI first, or name a different home with --home", p.agent, home)
	}
	return taken, nil
}

// HostLogin reports where this CLI's login sits in a human's home directory,
// whether or not it has been adopted.
//
// So that "no login" on a machine where the CLI plainly works explains
// itself, rather than sending an operator to look for a bug that is a missing
// `--from-host`.
func (p *Provider) HostLogin(home string) []string {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil
		}
	}
	sources := p.profile.HostCredentialPaths
	if len(sources) == 0 {
		sources = p.profile.CredentialPaths
	}
	var found []string
	for _, rel := range sources {
		src, err := underRoot(home, rel)
		if err != nil {
			continue
		}
		if info, err := os.Stat(src); err == nil && info.Mode().IsRegular() {
			found = append(found, src)
		}
	}
	return found
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
// fleet logs itself out.
func (p *Provider) RestoreBundle(blob string) error {
	dest := p.ws.CredentialsDir()
	if p.ws.HasLogin() {
		return nil
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
	if err := os.MkdirAll(dest, 0o700); err != nil {
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
				"re-export it with `crewlet llm export %s`", maxBundle, p.agent)
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
		if name != header.Name || !allowed[name] {
			return fmt.Errorf("cliagent: the credential bundle holds %q, which is not a "+
				"%q credential file", header.Name, p.agent)
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

	for name, data := range staged {
		if err := os.WriteFile(filepath.Join(dest, name), data, 0o600); err != nil {
			return fmt.Errorf("cliagent: writing %q: %w", name, err)
		}
	}
	return nil
}

// runInCredentialHome runs one of the CLI's own auth commands with its home
// pointed at the SHARED credential directory rather than a seat's.
//
// A login has to write where every seat's checkout is seeded FROM, and a seat
// home is pruned; brokering a login into one would delete the credential
// moments later.
func (p *Provider) runInCredentialHome(
	ctx context.Context, args []string,
	in io.Reader, out, errOut io.Writer, extra ...map[string]string,
) error {
	home := filepath.Join(p.ws.Root(), "login-home")
	dest := p.ws.CredentialsDir()
	for _, dir := range []string{home, dest} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cliagent: preparing %q: %w", dir, err)
		}
	}
	// The login home is seeded from and synced back to the shared
	// directory exactly like a seat's, so re-running a login updates the
	// one credential every seat reads.
	if err := p.ws.seed(home); err != nil {
		return err
	}
	env := map[string]string{}
	for k, v := range p.env {
		env[k] = v
	}
	for _, more := range extra {
		for k, v := range more {
			env[k] = v
		}
	}
	checkout := &Checkout{Home: home, Cache: filepath.Join(p.ws.Root(), "login-cache")}
	if err := os.MkdirAll(checkout.Cache, 0o700); err != nil {
		return fmt.Errorf("cliagent: preparing %q: %w", checkout.Cache, err)
	}

	cmd := exec.CommandContext(ctx, p.profile.Binary, args...) //nolint:gosec // args come from a validated profile
	cmd.Dir = home
	cmd.Env = buildEnv(p.profile, checkout, env, p.auth)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	runErr := cmd.Run()

	// Synced back even on failure: a login that authenticated and then
	// failed a later step still wrote a credential worth keeping.
	if err := p.ws.syncCredentialsOut(home); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("cliagent: %s %s: %w", p.profile.Binary, strings.Join(args, " "), runErr)
	}
	return nil
}
