package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// loginProvider builds a provider over a temp state dir, with a profile whose
// credential files are the ones these tests write.
func loginProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestFakeHarness"},
			"model_args": []any{}, "output": "text",
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
			"volatile_paths":        []any{".fake/sessions"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// writeHostLogin puts a login in a fake human home directory.
func writeHostLogin(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, ".fake", "creds.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The usual starting point: the operator has been running the harness on this box
// for months. Adopting is a COPY, so their own login is never written to by a
// fleet of agents refreshing tokens.
func TestAdoptingAHostLoginCopiesRatherThanMoves(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"personal"}`)

	taken, err := p.AdoptHostLogin(t.Context(), home)
	if err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if len(taken) != 1 {
		t.Fatalf("adopted %v", taken)
	}
	if _, err := os.Stat(filepath.Join(home, ".fake", "creds.json")); err != nil {
		t.Errorf("the operator's own login was moved, not copied: %v", err)
	}
	if !hasLogin(t, p.Workspace()) {
		t.Error("the adopted login is not where seats read it from")
	}
}

// "No login found" must name the directory it looked in, or an operator
// running as a different user than the one who logged in has nothing to go on.
func TestAdoptingWithNoHostLoginSaysWhereItLooked(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	home := t.TempDir()
	_, err := p.AdoptHostLogin(t.Context(), home)
	if err == nil {
		t.Fatal("adopting a login that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), home) {
		t.Errorf("error %q does not name the home it searched", err)
	}
	if !strings.Contains(err.Error(), "--home") {
		t.Errorf("error %q does not offer the flag for a different home", err)
	}
}

// A bundle carries a login onto another host, and must come back byte for
// byte — a credential that round-trips almost correctly is a login that fails
// at the vendor with no explanation.
func TestACredentialBundleRoundTrips(t *testing.T) {
	t.Parallel()
	source := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"exported","refresh":"r1"}`)
	if _, err := source.AdoptHostLogin(t.Context(), home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}

	blob, err := source.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	target := loginProvider(t)
	if err := target.RestoreBundle(t.Context(), blob); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(target.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatalf("the bundle restored nothing: %v", err)
	}
	if string(restored) != `{"token":"exported","refresh":"r1"}` {
		t.Errorf("restored %q", restored)
	}
}

// An archive is an execution surface if it is unpacked on trust, and a bundle
// arrives from the secret store, which every node holding the keyring shares.
func TestARestoredBundleCannotEscapeTheCredentialDirectory(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	for _, name := range []string{
		"../../.ssh/authorized_keys",
		"/etc/passwd",
		"nested/creds.json",
		"unexpected.json",
	} {
		err := p.RestoreBundle(t.Context(), bundleOf(t, p.harness, name, "pwned"))
		if err == nil {
			t.Errorf("RestoreBundle accepted %q", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for %q does not name it: %v", name, err)
		}
	}
	if hasLogin(t, p.Workspace()) {
		t.Error("a refused bundle still wrote a credential")
	}
}

// AN OVER-LARGE BUNDLE IS REFUSED, not clipped. io.LimitReader stops at its cap
// and hands the tar reader a clean io.EOF, which is indistinguishable from the
// end of a whole archive — so a bundle past the cap used to restore whatever
// files fitted and report success, leaving a partial credential directory that
// HasLogin then reads as a login and declines to repair.
func TestAnOversizedBundleIsRefusedRatherThanPartlyRestored(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	err := p.RestoreBundle(t.Context(), bundleOf(t, p.harness, "creds.json", strings.Repeat("x", maxBundle+1)))
	if err == nil {
		t.Fatal("an oversized bundle was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// The command takes the entry's KEY: naming the harness ("custom") sent the
	// operator to `crewlet llm export` for a provider that does not exist.
	if !strings.Contains(err.Error(), "`crewlet llm export sub`") {
		t.Errorf("the refusal does not name the export that replaces it: %v", err)
	}
	if hasLogin(t, p.Workspace()) {
		t.Error("a refused bundle still wrote a credential")
	}
}

// A BUNDLE RESTORES ONLY INTO THE HARNESS IT NAMES. A bundle is keyed by the
// entry — CREWLET_LLM_CLI_<KEY>_CREDENTIALS, or the entry's
// auth.credential_bundle — not by its harness, so an entry moved to another harness
// still restores the bundle it was configured with. Judged by its files' names
// alone, that bundle was refused only where the two harnesses keep different names:
// six keep an `auth.json`, so codex's bundle was
// written into pi's empty login, where it is a credential pi cannot read and,
// as a login already present, turned every later restore away — the right
// bundle included.
//
// Every built-in harness's bundle, restored and imported into every other harness's
// empty login on the same state directory — the move — is refused naming the
// harness it came from, the move and the export that replaces it, and leaves that
// login's directory empty; and it restores into its own harness byte for byte,
// with nothing beside its login files.
//
// Mutation: skip the manifest's check in restoreBundle, and every pair that
// shares a file name restores (codex into pi, among others), and
// every other pair is refused as a file it does not keep rather than as
// another harness's bundle.
func TestABundleRestoresOnlyIntoTheHarnessItNames(t *testing.T) {
	t.Parallel()
	var clis []string
	keptBy := map[string][]string{}
	for _, name := range harnessprofile.BuiltinNames() {
		profile, _ := harnessprofile.Builtin(name)
		names := profileLoginNames(profile)
		if len(names) == 0 {
			continue
		}
		clis = append(clis, name)
		for _, file := range names {
			keptBy[file] = append(keptBy[file], name)
		}
	}
	var shared []string
	for file, by := range keptBy {
		if len(by) > 1 {
			shared = append(shared, fmt.Sprintf("%s (%s)", file, strings.Join(by, ", ")))
		}
	}
	if len(shared) == 0 {
		t.Fatal("the premise: no two built-in harnesses keep a login file under one name, so no " +
			"pair here is one a file name alone would have let through")
	}
	t.Logf("login file names more than one harness keeps: %s", strings.Join(slices.Sorted(slices.Values(shared)), "; "))

	build := func(t *testing.T, dir, agent string) *Provider {
		t.Helper()
		p, err := New(Config{Key: "k", Harness: agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1})
		if err != nil {
			t.Fatalf("New(%s): %v", agent, err)
		}
		return p
	}
	for _, from := range clis {
		t.Run(from, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			source := build(t, dir, from)
			want := map[string]string{}
			for _, file := range profileLoginNames(source.profile) {
				want[file] = fmt.Sprintf(`{"harness":%q,"file":%q}`, from, file)
				if key, ok := source.profile.LoginKeys[file]; ok {
					// A file the harness keeps settings in too is a login
					// only holding its key.
					want[file] = fmt.Sprintf(`{"harness":%q,"file":%q,%q:"placeholder"}`, from, file, key)
				}
				writeFile(t, filepath.Join(source.ws.CredentialsDir(), file), want[file])
			}
			blob, err := source.ExportBundle(t.Context())
			if err != nil {
				t.Fatalf("ExportBundle: %v", err)
			}

			for _, to := range clis {
				if to == from {
					continue
				}
				target := build(t, dir, to)
				for op, restore := range map[string]func(context.Context, string) error{
					"a restore": target.RestoreBundle, "an import": target.ImportBundle,
				} {
					err := restore(t.Context(), blob)
					if !errors.Is(err, ErrForeignBundle) {
						t.Errorf("%s of %s's bundle into %s = %v, want ErrForeignBundle", op, from, to, err)
						continue
					}
					for _, say := range []string{fmt.Sprintf("it names %q", from),
						fmt.Sprintf(`"k" moved to %q`, to), "`crewlet llm export k`"} {
						if !strings.Contains(err.Error(), say) {
							t.Errorf("%s of %s's bundle into %s does not say %q: %v", op, from, to, say, err)
						}
					}
					if got := credentialEntries(t, target); len(got) != 0 {
						t.Errorf("%s of %s's bundle into %s was refused and still wrote %v", op, from, to, got)
					}
				}
			}

			home := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(home) })
			own := build(t, home, from)
			if err := own.RestoreBundle(t.Context(), blob); err != nil {
				t.Fatalf("%s's bundle into %s: %v", from, from, err)
			}
			if got, files := credentialEntries(t, own), slices.Sorted(maps.Keys(want)); !slices.Equal(got, files) {
				t.Errorf("%s's bundle restored %v into its own harness, want exactly its login files %v", from, got, files)
			}
			for file, body := range want {
				got, err := os.ReadFile(filepath.Join(own.ws.CredentialsDir(), file)) //nolint:gosec // the test's own state directory
				if err != nil || string(got) != body {
					t.Errorf("%s's %s restored as %q (%v), want %q", from, file, got, err, body)
				}
			}
		})
	}
}

// profileLoginNames are the file names a profile's credential directory keeps
// a login under: each credential path's, and a credential store's document.
func profileLoginNames(profile harnessprofile.Profile) []string {
	var names []string
	for _, rel := range profile.CredentialPaths {
		names = append(names, filepath.Base(rel))
	}
	if profile.CredentialStore != nil {
		names = append(names, harnessprofile.CredentialStoreDocument)
	}
	return names
}

// A BUNDLE THAT NAMES NO HARNESS IS REFUSED AS ONE THAT NAMES ANOTHER: nothing in
// it says whose its files are, and a file's name is the guess the manifest
// replaced. So is one that names a file or the manifest twice — two answers to
// one question, the last winning by accident — and one that holds no login,
// which an import reported "Restored" over an empty directory. A manifest with
// a key this build does not know still restores: a later build may add one,
// and a node still on this build would otherwise run its entry signed out.
//
// Mutation: drop the refusal of a missing manifest, a repeated name or an
// empty bundle from restoreBundle, and its case is red.
func TestABundleThatDoesNotSayWhoseItIsIsRefused(t *testing.T) {
	t.Parallel()
	creds := bundleFile{name: "creds.json", data: []byte(`{"token":"k-bundled"}`)}
	manifest := func(body string) bundleFile {
		return bundleFile{name: harnessprofile.BundleManifest, data: []byte(body)}
	}
	ours := manifest(`{"harness":"custom"}`)
	for name, tc := range map[string]struct {
		entries []bundleFile
		// foreign is whether it is refused as naming no harness.
		foreign bool
		say     string
	}{
		"no manifest": {
			entries: []bundleFile{creds}, foreign: true,
			say: "it names none (it holds no " + harnessprofile.BundleManifest + ")",
		},
		"a manifest that does not parse": {
			entries: []bundleFile{manifest("custom"), creds}, foreign: true, say: "does not parse",
		},
		"a manifest naming no agent": {
			entries: []bundleFile{manifest(`{}`), creds}, foreign: true, say: "names no agent",
		},
		"two manifests": {
			entries: []bundleFile{ours, creds, manifest(`{"harness":"codex"}`)},
			say:     `holds "` + harnessprofile.BundleManifest + `" twice`,
		},
		"a login file twice": {
			entries: []bundleFile{ours, creds, creds}, say: `holds "creds.json" twice`,
		},
		"no login file": {
			entries: []bundleFile{ours}, say: "holds no \"custom\" login file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for op, restore := range map[string]func(*Provider, string) error{
				"a restore": func(p *Provider, blob string) error { return p.RestoreBundle(t.Context(), blob) },
				"an import": func(p *Provider, blob string) error { return p.ImportBundle(t.Context(), blob) },
			} {
				p := loginProvider(t)
				err := restore(p, rawBundle(t, tc.entries...))
				if err == nil {
					t.Errorf("%s accepted it", op)
					continue
				}
				if errors.Is(err, ErrForeignBundle) != tc.foreign {
					t.Errorf("%s = %v, want ErrForeignBundle %v", op, err, tc.foreign)
				}
				for _, say := range []string{tc.say, "`crewlet llm export sub`"} {
					if !strings.Contains(err.Error(), say) {
						t.Errorf("%s does not say %q: %v", op, say, err)
					}
				}
				if got := credentialEntries(t, p); len(got) != 0 {
					t.Errorf("%s was refused and still wrote %v", op, got)
				}
			}
		})
	}

	p := loginProvider(t)
	if err := p.RestoreBundle(t.Context(), rawBundle(t,
		manifest(`{"harness":"custom","exported_by":"a later build"}`), creds)); err != nil {
		t.Fatalf("a manifest carrying a key this build does not know: %v", err)
	}
	if got := readCreds(t, p); got != `{"token":"k-bundled"}` {
		t.Errorf("a manifest carrying a key this build does not know restored %q", got)
	}
}

// A bundle is validated WHOLE before anything reaches the disk: the checks
// reject mid-archive, and writing as the loop went left the rejected bundle's
// earlier entries behind.
func TestARejectedBundleLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	// A good entry FIRST, then one the name check refuses.
	blob, err := packBundle(bundleManifest{Harness: p.harness}, []bundleFile{
		{name: "creds.json", data: []byte(`{"token":"real"}`)},
		{name: "../../.ssh/authorized_keys", data: []byte("pwned")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreBundle(t.Context(), blob); err == nil {
		t.Fatal("a bundle holding a path traversal was accepted")
	}
	if got := credentialEntries(t, p); len(got) != 0 {
		t.Errorf("the entry before the refused one was written anyway: %v", got)
	}
}

// A node that has been running holds the FRESHER refresh token; restoring a
// boot-time blob over it is how a fleet logs itself out at the next expiry.
func TestABundleDoesNotOverwriteALiveLogin(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"current"}`)
	if _, err := p.AdoptHostLogin(t.Context(), home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if err := p.RestoreBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"stale"}`)); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"token":"current"}` {
		t.Errorf("a boot-time bundle overwrote the running node's login: %q", got)
	}
	// The operator's form of the same restore SAYS it restored nothing,
	// rather than leaving `crewlet llm import` to print "Restored".
	if err := p.ImportBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"stale"}`)); !errors.Is(err, ErrLoginPresent) {
		t.Errorf("ImportBundle over a live login = %v, want ErrLoginPresent", err)
	}
}

// Exporting nothing must say what to run, not produce an empty archive that
// looks like a working credential in the secret store.
func TestExportingWithNoLoginRefuses(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	_, err := p.ExportBundle(t.Context())
	if err == nil {
		t.Fatal("an empty credential directory exported cleanly")
	}
	if !strings.Contains(err.Error(), "crewlet llm login") {
		t.Errorf("error %q does not say what to run", err)
	}
}

// Logout removes the local credential even when the vendor's own command
// fails: a login the operator believes they removed must not still work.
func TestLogoutRemovesTheCredentialEvenIfTheVendorCommandFails(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"x"}`)
	if _, err := p.AdoptHostLogin(t.Context(), home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	// A logout command that cannot possibly succeed.
	p.profile.LogoutArgs = []string{"-test.run=TestFakeHarness"}
	p.env = fakeChildEnv(map[string]string{"FAKE_EXIT": "3"})

	err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Error("a failing vendor logout was reported as success")
	}
	if hasLogin(t, p.Workspace()) {
		t.Error("the credential survived a logout the operator was told about")
	}
}

// The three harnesses whose login is browser OAuth must say so in a sentence an
// operator can act on, rather than hanging on a prompt or failing obscurely.
func TestAHarnessWithNoCredentialLoginExplainsItself(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	err := p.CredentialLogin(t.Context(), "ops@example.com", "hunter2",
		&bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, ErrNoLoginCommand) {
		t.Fatalf("err = %v, want ErrNoLoginCommand", err)
	}
	for _, want := range []string{"crewlet llm login", "stdin_login", "browser"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
}

// AND IT NAMES ONLY ROUTES THAT RUN. It offered `crewlet llm login` to a harness
// that declares no login subcommand, which that command refuses, and
// `-capture-token` "where the vendor mints a headless token" to every harness,
// which that command refuses for one that mints none — and a token minted
// under an overrides.token_env no store can hold (MY-TOKEN) could be neither
// stored nor referenced. Claude Code as shipped is the control: it has both
// routes, and is told both.
//
// Mutation: offer -capture-token whatever the harness mints, and the custom case
// is told it; by the profile's token variable alone, and the MY-TOKEN case
// is; offer `crewlet llm login KEY` whatever the harness declares, and the custom
// case is.
func TestACredentialLoginRefusalNamesOnlyRoutesThatRun(t *testing.T) {
	t.Parallel()
	claude := func(t *testing.T, overrides map[string]any) *Provider {
		t.Helper()
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		p, err := New(Config{
			Key: "sub", Harness: "claude-code", StateDir: dir, Timeout: time.Second,
			MaxConcurrent: 1, Overrides: overrides,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}
	for _, tc := range []struct {
		name          string
		provider      func(t *testing.T) *Provider
		want, notWant []string
	}{
		{
			name:     "claude-code as shipped",
			provider: func(t *testing.T) *Provider { return claude(t, nil) },
			want: []string{"run `crewlet llm login sub`",
				"CLAUDE_CODE_OAUTH_TOKEN with `crewlet llm login sub -capture-token`"},
		},
		{
			name: "a token variable no store can hold",
			provider: func(t *testing.T) *Provider {
				return claude(t, map[string]any{"token_env": "MY-TOKEN"})
			},
			want:    []string{"run `crewlet llm login sub`"},
			notWant: []string{"capture-token"},
		},
		{
			name:     "a harness with no login subcommand",
			provider: loginProvider,
			want:     []string{"declares no login subcommand", "`crewlet llm login sub -from-host`"},
			notWant:  []string{"run `crewlet llm login sub`", "capture-token"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.provider(t).CredentialLogin(t.Context(), "ops@example.com", "hunter2",
				&bytes.Buffer{}, &bytes.Buffer{})
			if !errors.Is(err, ErrNoLoginCommand) {
				t.Fatalf("err = %v, want ErrNoLoginCommand", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("the refusal names %q, a route that does not run here: %v", bad, err)
				}
			}
		})
	}
}

// A HARNESS THAT MINTS NO TOKEN IS SENT TO THE LOGIN IT HAS. `-capture-token` on
// one told the operator to "run `crewlet llm login`" to broker its own login,
// which a harness declaring no login subcommand refuses in turn. Codex, which has
// one, is the control.
//
// Mutation: name `crewlet llm login KEY` whatever the harness declares, and the
// custom case is told it.
func TestATokenMintRefusalNamesTheLoginTheHarnessHas(t *testing.T) {
	t.Parallel()
	codex := func(t *testing.T) *Provider {
		t.Helper()
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		p, err := New(Config{
			Key: "sub", Harness: "codex", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}
	for _, tc := range []struct {
		name          string
		provider      func(t *testing.T) *Provider
		want, notWant []string
	}{
		{
			name: "codex", provider: codex,
			want: []string{`the "codex" harness mints no headless token`, "run `crewlet llm login sub`"},
		},
		{
			name: "a harness with no login subcommand", provider: loginProvider,
			want:    []string{"declares no login subcommand", "`crewlet llm login sub -from-host`"},
			notWant: []string{"run `crewlet llm login"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.provider(t).CaptureToken(t.Context(), nil, &bytes.Buffer{})
			if !errors.Is(err, ErrNoLoginCommand) {
				t.Fatalf("err = %v, want ErrNoLoginCommand", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("the refusal names %q, a route this harness does not have: %v", bad, err)
				}
			}
		})
	}
}

// AND THE MODE CHOOSES ITS ROUTES BEFORE THE HARNESS DOES. Chosen by the harness alone,
// an api-key entry was told to run the harness's login or, on Claude Code, to mint
// a headless token with `-capture-token` — a login is not what an api-key entry
// signs in with, and the token is one the mode removes from the harness's
// environment and `crewlet llm login` refuses before it mints; an inherit-env
// Claude Code entry was told the same mint, whose token lands in the secret
// store that mode never reads, so the command refuses to store it. api-key is
// now told its key alone, and inherit-env what it forwards, a login and, where
// the harness mints, a token minted to stdout to export. Codex, which mints none,
// is held to the same split. OpenCode under api-key, a pairing every write of
// the configuration refuses and `crewlet llm` still loads, names no key
// variable, so the mode hands its harness nothing and the remedy is the mode's own,
// a login under subscription: it was told that and, in the same sentence, that
// a login is not its sign-in. Codex under api-key with its key variable
// outside the reference grammar (MY=KEY), which leaves ModeSignIn's Variables
// empty too, still keeps a key and is told to name the variable.
//
// Mutation: choose the routes by the harness alone, and every case is red; word
// the opencode case as an api-key entry that keeps its key, and it is red; ask
// Variables alone for a harness that names no key variable, and the MY=KEY case is
// worded as one, losing that a login is not its sign-in.
func TestACredentialLoginRefusalFollowsTheEntrysAuthMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent   string
		mode          AuthMode
		overrides     map[string]any
		want, notWant []string
	}{
		{
			name: "claude-code under api-key", agent: "claude-code", mode: AuthAPIKey,
			want: []string{"auth.mode is api-key",
				"providers.llm.sub.auth.api_key, else ANTHROPIC_API_KEY", "`crewlet secrets set`"},
			notWant: []string{"crewlet llm login sub", "capture-token", "stdin_login"},
		},
		{
			name: "codex under api-key", agent: "codex", mode: AuthAPIKey,
			want: []string{"auth.mode is api-key",
				"providers.llm.sub.auth.api_key, else CODEX_API_KEY", "`crewlet secrets set`"},
			notWant: []string{"crewlet llm login sub", "stdin_login"},
		},
		{
			name: "claude-code under inherit-env", agent: "claude-code", mode: AuthInheritEnv,
			want: []string{"run `crewlet llm login sub`", "auth.mode is inherit-env",
				"CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY from the engine's own process environment",
				"export one in the engine's environment",
				"`crewlet llm login sub -capture-token -print-token`",
				"export it as CLAUDE_CODE_OAUTH_TOKEN in the engine's environment"},
			notWant: []string{"with `crewlet llm login sub -capture-token`"},
		},
		{
			name: "codex under inherit-env", agent: "codex", mode: AuthInheritEnv,
			want: []string{"run `crewlet llm login sub`", "auth.mode is inherit-env",
				"CODEX_API_KEY from the engine's own process environment",
				"export it in the engine's environment"},
			notWant: []string{"capture-token"},
		},
		{
			name: "opencode under api-key, its profile naming no key variable", agent: "opencode",
			mode: AuthAPIKey,
			want: []string{"its auth.mode is api-key, which puts its key in no variable",
				"a pairing every write of the configuration refuses",
				"set auth.mode: subscription, and sign the harness in with a login"},
			notWant: []string{"a login is not this entry's sign-in", "crewlet llm login sub",
				"capture-token", "stdin_login"},
		},
		{
			name: "codex under api-key, its key variable outside the grammar", agent: "codex",
			mode: AuthAPIKey, overrides: map[string]any{"api_key_env": "MY=KEY"},
			want: []string{"a login is not this entry's sign-in",
				"name the variable your build of the harness reads in providers.llm.sub.overrides.api_key_env"},
			notWant: []string{"a pairing every write", "auth.mode: subscription", "crewlet llm login sub",
				"`crewlet secrets set`"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Harness: tc.agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: tc.overrides, Auth: Auth{Mode: tc.mode},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = p.CredentialLogin(t.Context(), "ops@example.com", "hunter2", &bytes.Buffer{}, &bytes.Buffer{})
			if !errors.Is(err, ErrNoLoginCommand) {
				t.Fatalf("err = %v, want ErrNoLoginCommand", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("the refusal names %q, a route this mode does not read: %v", bad, err)
				}
			}
		})
	}
}

// A HARNESS THAT KEEPS NO LOGIN IS SENT TO ITS KEY, not to a login on the host:
// Qwen Code keeps none — both measured builds refuse the OAuth login it once
// kept — so `-from-host` would find nothing to adopt.
func TestBrokeringALoginForAHarnessThatKeepsNoneNamesItsKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{Key: "qwen", Harness: "qwen-code", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p.BrokerLogin(t.Context(), strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, ErrNoLoginCommand) {
		t.Fatalf("err = %v, want ErrNoLoginCommand", err)
	}
	if !strings.Contains(err.Error(), "auth.mode: api-key") || strings.Contains(err.Error(), "-from-host") {
		t.Errorf("the refusal does not send the operator to the key alone: %v", err)
	}
}

// NO BUILT-IN PROFILE SHIPS A CREDENTIAL LOGIN, because no shipped harness has
// one. opencode's did — `auth login --username`, a flag no release of either
// OpenCode major accepts, which printed usage and exited 1 — and a declared
// login a harness cannot serve sends operators down a path that only fails. The
// mechanism stays for an operator whose build does accept one.
func TestNoBuiltinProfileShipsACredentialLogin(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		if name == "custom" {
			continue
		}
		p, err := harnessprofile.Load(name, nil)
		if err != nil {
			t.Fatalf("Load(%q): %v", name, err)
		}
		if p.StdinLogin != nil {
			t.Errorf("%s: ships a stdin_login (%v) its harness cannot serve", name, p.StdinLogin.Args)
		}
	}
}

// bundleOf builds a one-file bundle with an arbitrary entry name, naming agent
// as the harness it was exported from, through the export's own writer.
func bundleOf(t *testing.T, agent, name, body string) string {
	t.Helper()
	blob, err := packBundle(bundleManifest{Harness: agent}, []bundleFile{{name: name, data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// rawBundle packs entries exactly as given, with no manifest of its own: a
// bundle no export writes, for the cases that refuse one.
func rawBundle(t *testing.T, entries ...bundleFile) string {
	t.Helper()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o600, Size: int64(len(e.data)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes())
}

// credentialEntries names everything in the entry's credential directory,
// whatever it is — nil where the directory holds nothing or does not exist.
// Read from the directory rather than through [Workspace.Login], which reads
// only the names the profile keeps: a manifest written there would be
// invisible to it.
func credentialEntries(t *testing.T, p *Provider) []string {
	t.Helper()
	entries, err := os.ReadDir(p.ws.CredentialsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An auth command must be a SUBCOMMAND the harness runs and exits from, never a
// prompt typed at an interactive session.
//
// `claude` takes a bare argument as its prompt, so the `["/login"]` this
// profile shipped never invoked a login command at all: it opened a session
// carrying `/login` as the first message. Against the empty login-home
// `runInCredentialHome` hands it, that session ran its own first-run sign-in
// and THEN replayed the queued `/login`, so an operator who had just
// authenticated was asked to authenticate again — and was then left in a REPL
// that never exits, having to interrupt a login that had already succeeded.
// A leading "/" is how that mistake is spelled for every harness here.
func TestNoProfileBrokersAnAuthCommandAsAPrompt(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		if name == "custom" {
			continue
		}
		p, err := harnessprofile.Load(name, nil)
		if err != nil {
			t.Fatalf("Load(%q): %v", name, err)
		}
		for label, args := range map[string][]string{
			"login_args":         p.LoginArgs,
			"status_args":        p.StatusArgs,
			"logout_args":        p.LogoutArgs,
			"capture_token_args": p.CaptureTokenArgs,
		} {
			if len(args) > 0 && strings.HasPrefix(args[0], "/") {
				t.Errorf("%s: %s = %q — a slash command is a PROMPT to an interactive "+
					"session, not a subcommand that exits", name, label, args)
			}
		}
	}
}

// The docs tell every operator to run `crewlet llm status` and
// `crewlet llm logout`, and the flagship profile has to answer both.
//
// It answered neither: with no status_args the command failed with "this harness
// has no status command" for a harness that has one (now JSON for the engine's
// reading, [Provider.LoginStatus], and --text for the person reading
// `crewlet llm status`), and with no logout_args
// `logout` deleted the local files while leaving the session live at the
// vendor — an operator who believed they had revoked a login had not.
func TestTheClaudeProfileCanBrokerInspectAndRevokeItsLogin(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for label, want := range map[string][]string{
		"login_args":       {"auth", "login", "--claudeai"},
		"status_args":      {"auth", "status"},
		"status_text_args": {"auth", "status", "--text"},
		"logout_args":      {"auth", "logout"},
	} {
		got := map[string][]string{
			"login_args": p.LoginArgs, "status_args": p.StatusArgs,
			"status_text_args": p.StatusTextArgs, "logout_args": p.LogoutArgs,
		}[label]
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("claude-code %s = %q, want %q", label, got, want)
		}
	}
}

// A minting command is INTERACTIVE, and the operator has to see it.
//
// Claude Code renders its whole sign-in UI — the OAuth URL and the "paste code
// here" prompt — on STDOUT whenever stdin is a terminal, and writes nothing at
// all to stderr. Capturing stdout into a buffer and showing the operator only
// stderr therefore left them at a blank terminal, waiting to paste a code from
// a URL they had never been shown, on the route the docs call preferred.
func TestCapturingATokenStillShowsTheOperatorTheVendorsPrompts(t *testing.T) {
	t.Parallel()
	ui := "Browser didn't open? Use the url below to sign in\nhttps://example.com/oauth?code=true\n"
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": ui + "sk-ant-oat01-EXAMPLE\n"},
		map[string]any{"capture_token_args": []any{"-test.run=TestFakeHarness"},
			"token_env": "FAKE_TOKEN"})
	var seen bytes.Buffer
	token, err := p.CaptureToken(t.Context(), nil, &seen)
	if err != nil {
		t.Fatalf("CaptureToken: %v", err)
	}
	if token != "sk-ant-oat01-EXAMPLE" {
		t.Errorf("token = %q", token)
	}
	if !strings.Contains(seen.String(), "https://example.com/oauth?code=true") {
		t.Errorf("the operator never saw the sign-in URL; they saw: %q", seen.String())
	}
}

// The captured stream is a rendered UI, not a report: it carries colour,
// cursor moves, OSC-8 hyperlinks and \r redraws. A frame of decoration must
// read as empty rather than be stored as the company's credential.
func TestATerminalFrameIsNotMistakenForAToken(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ raw, want string }{
		"trailing colour reset":  {"instructions\n\x1b[32msk-ant-oat01-A\x1b[0m\n", "sk-ant-oat01-A"},
		"osc-8 hyperlink after":  {"sk-ant-oat01-B\n\x1b]8;;https://example.com\x07\x1b]8;;\x07\n", "sk-ant-oat01-B"},
		"redraw with \\r":        {"waiting...\rsk-ant-oat01-C", "sk-ant-oat01-C"},
		"decoration only at end": {"sk-ant-oat01-D\n\x1b[?25h\x1b[2K\n", "sk-ant-oat01-D"},
		"nothing but decoration": {"\x1b[2K\x1b[?25h\n", ""},
	}
	for name, c := range cases {
		if got := lastPrintedLine(c.raw); got != c.want {
			t.Errorf("%s: lastPrintedLine(%q) = %q, want %q", name, c.raw, got, c.want)
		}
	}
}

// Markers are matched VERBATIM against the harness's own words, so a sentinel the
// installed binary never emits can never fire — and the failure is silent
// until somebody hits their cap.
//
// Both of the claude profile's original sentinels were in that state: neither
// "Claude AI usage limit reached" (with its pipe-and-epoch reset field) nor
// "OAuth token has expired" occurs anywhere in Claude Code 2.x. A spent
// subscription therefore classified FATAL instead of RATE_LIMIT, so the
// fallback chain never carried the seat onto another model.
func TestTheClaudeProfilesMarkersAreStringsTheHarnessStillEmits(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The pipe-and-epoch reset retired with the 1.x wording that carried it:
	// the live line names a time of day and its zone, read as `clock`.
	for _, m := range p.LimitMarkers {
		if m.Reset.Unit == harnessprofile.ResetEpoch {
			t.Errorf("limit marker %q still reads a 1.x epoch reset: %+v", m.Sentinel, m)
		}
	}
	for _, dead := range []string{"Claude AI usage limit reached", "OAuth token has expired"} {
		for _, m := range p.LimitMarkers {
			if strings.Contains(m.Sentinel, dead) {
				t.Errorf("limit_markers still carries the retired sentinel %q", dead)
			}
		}
		for _, m := range p.AuthMarkers {
			if strings.Contains(m.Sentinel, dead) {
				t.Errorf("auth_markers still carries the retired sentinel %q", dead)
			}
		}
	}
	for _, want := range []string{"Usage limit reached", `"type":"rate_limit_error"`} {
		if !slices.ContainsFunc(p.LimitMarkers, func(m harnessprofile.LimitMarker) bool { return m.Sentinel == want }) {
			t.Errorf("limit_markers lacks %q: %+v", want, p.LimitMarkers)
		}
	}
	for _, want := range []string{"Invalid API key", "OAuth token has been revoked", "Please run /login"} {
		if !slices.ContainsFunc(p.AuthMarkers, func(m harnessprofile.AuthMarker) bool { return m.Sentinel == want }) {
			t.Errorf("auth_markers lacks %q: %+v", want, p.AuthMarkers)
		}
	}
}

// BUILDING AN ENTRY WRITES NOTHING. [New] is what `crewlet validate` reaches,
// and it used to restore a configured bundle into the state directory — under
// the shared login's lock — so validating a document wrote a login onto the
// host. The restore is [Provider.RestoreConfiguredBundle], made by whoever runs
// the entry, and an entry with no bundle configured restores nothing.
func TestAConfiguredBundleIsRestoredOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	build := func(bundle string) *Provider {
		p, err := New(Config{
			Key: "sub", Harness: "custom", StateDir: dir,
			Timeout: time.Second, MaxConcurrent: 1, CredentialBundle: bundle,
			Overrides: map[string]any{
				"binary": os.Args[0], "complete_args": []any{"-test.run=TestFakeHarness"},
				"model_args": []any{}, "output": "text",
				"credential_paths": []any{".fake/creds.json"},
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}

	if err := build("").RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("an entry with no bundle: %v", err)
	}
	p := build(bundleOf(t, "custom", "creds.json", `{"token":"configured"}`))
	if hasLogin(t, p.Workspace()) {
		t.Fatal("building the entry restored its bundle — `crewlet validate` would write a login onto the host")
	}
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("RestoreConfiguredBundle: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json"))
	if err != nil || string(restored) != `{"token":"configured"}` {
		t.Fatalf("restored %q (%v), want the configured bundle's login", restored, err)
	}
	// A login already in place is the running node's, and fresher: a second
	// restore leaves it and is no error.
	if err := os.WriteFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json"),
		[]byte(`{"token":"refreshed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("a restore over a login: %v", err)
	}
	if kept, _ := os.ReadFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json")); string(kept) != `{"token":"refreshed"}` {
		t.Errorf("a restore replaced the running login with the bundle: %q", kept)
	}
}

// A LOGOUT IS NOT UNDONE BY THE CONFIGURED BUNDLE. The directory a logout
// empties is exactly what a bundle restores into, so the next restore — the
// one `crewlet llm status` makes before it asks, and the engine's at its next
// boot or apply — put back the very login the operator had just removed, a
// compromised one typically, and the logout lasted until the next command.
// Now the restore declines, and says why in words an operator can act on —
// among them how to retire the bundle for good, `crewlet secrets unset`.
//
// Mutation: drop the record from Logout, or the check from restoreBundle, and
// the login comes back here.
func TestALogoutIsNotUndoneByTheConfiguredBundle(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	// A harness that takes a headless token, as Claude Code does, so the decline
	// and the notice have a token to say does not end the logout: they name
	// one only for an entry it signs in ([Provider.headlessTokenRoute]). It
	// declares no minting command, so `-token-stdin` is the one route.
	p.profile.TokenEnv = "FAKE_HARNESS_TOKEN"
	p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("the boot's restore: %v", err)
	}
	if !hasLogin(t, p.Workspace()) {
		t.Fatal("the premise: the bundle restored nothing")
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	err := p.RestoreConfiguredBundle(t.Context())
	if !errors.Is(err, ErrLoggedOut) {
		t.Errorf("the restore after a logout = %v, want ErrLoggedOut", err)
	}
	if hasLogin(t, p.Workspace()) {
		t.Fatalf("the restore after a logout brought the login back: %v", loginOnDisk(t, p.Workspace()))
	}
	for _, want := range []string{"`crewlet llm login sub -from-host`", "`crewlet llm import sub`",
		"a headless token signs the entry in without ending the logout",
		"fresh volume", "crewlet secrets unset " + harnessprofile.BundleVarName("sub")} {
		if err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("the decline does not name %q: %v", want, err)
		}
	}
	// This profile declares no login subcommand, so the decline does not send
	// the operator to one: `crewlet llm login sub` would only refuse.
	if err != nil && strings.Contains(err.Error(), "`crewlet llm login sub`") {
		t.Errorf("the decline names a brokered login the profile does not have: %v", err)
	}
	// Recorded in the state directory, where no bundle reaches it.
	if _, statErr := os.Stat(filepath.Join(p.Workspace().Root(), loggedOutFile)); statErr != nil {
		t.Errorf("the logout left no record in the state directory: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(p.Workspace().CredentialsDir(), loggedOutFile)); !os.IsNotExist(statErr) {
		t.Errorf("the logout's record is in the credentials directory, where a bundle carries it (%v)", statErr)
	}
	// What the operator is told about the bundle, naming where it came
	// from: the conventional secret here, the field where one is set — and
	// that a headless token, which writes no login there, does not end it.
	notice := logoutNotice(t, p)
	for _, want := range []string{harnessprofile.BundleVarName("sub"), "fresh volume", "`crewlet llm login sub -from-host`",
		"`-token-stdin` signs it in without ending this"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the logout notice does not name %q: %s", want, notice)
		}
	}
	if strings.Contains(notice, "`crewlet llm login sub`") {
		t.Errorf("the notice names a brokered login the profile does not have: %s", notice)
	}
	p.bundleConfigured = true
	if notice := logoutNotice(t, p); !strings.Contains(notice, "providers.llm.sub.auth.credential_bundle") {
		t.Errorf("the notice for a bundle named in the document does not name the field: %s", notice)
	}
	p.bundle = ""
	if notice := logoutNotice(t, p); notice != "" {
		t.Errorf("an entry with no bundle was told about one: %s", notice)
	}
}

// THE WAY BACK FROM A LOGOUT IS WHAT THE PROFILE OFFERS. The decline and the
// notice once named `crewlet llm login <key>` to every entry, including ones
// whose harness declares no login subcommand (qwen-code, grok, gemini-cli), so the
// first remedy an operator tried refused. Each route is named exactly when the
// profile has what it needs, and an import, which needs nothing, always is.
//
// Mutation: name any route unconditionally, and its "absent" case is red.
func TestALogoutsRemedyNamesOnlyTheLoginsTheProfileHas(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit        func(p *harnessprofile.Profile)
		want, never []string
	}{
		"a login subcommand": {
			edit: func(p *harnessprofile.Profile) { p.LoginArgs = []string{"login"} },
			want: []string{"`crewlet llm login sub`", "`crewlet llm login sub -from-host`", "`crewlet llm import sub`"},
		},
		"a credential login": {
			edit:  func(p *harnessprofile.Profile) { p.StdinLogin = &harnessprofile.StdinLogin{Args: []string{"login"}} },
			want:  []string{"`crewlet llm login sub -username <name> -password-stdin`", "`crewlet llm import sub`"},
			never: []string{"`crewlet llm login sub`"},
		},
		"nothing a person's home holds": {
			edit: func(p *harnessprofile.Profile) {
				p.HostCredentialPaths, p.CredentialPaths = nil, nil
			},
			want:  []string{"`crewlet llm import sub`"},
			never: []string{"`crewlet llm login sub`", "-from-host"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := loginProvider(t)
			tc.edit(&p.profile)
			routes := p.loginRoutes()
			for _, want := range tc.want {
				if !strings.Contains(routes, want) {
					t.Errorf("the routes %q do not name %q", routes, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(routes, never) {
					t.Errorf("the routes %q name %q, which this profile does not offer", routes, never)
				}
			}
		})
	}
}

// THE LOGOUT NOTICE SPEAKS FOR THE RECORD, never for whether Logout succeeded:
// a vendor logout that fails after the login is removed fails the command with
// the record written, and the operator was then told nothing about the bundle
// it now holds back — while a directory with no record is one the next restore
// refills, about which the notice would be false.
//
// Mutation: answer the notice for any entry with a bundle, without reading the
// record, and the case before the logout is red; drop the record from Logout,
// and the case after a failed vendor logout is.
func TestTheLogoutNoticeSpeaksForTheRecord(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
	if notice := logoutNotice(t, p); notice != "" {
		t.Errorf("an entry nobody logged out was told its bundle is held back: %s", notice)
	}
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("the boot's restore: %v", err)
	}
	// The vendor's own logout fails, after which the login is removed and the
	// logout recorded all the same.
	p.profile.Binary = "sh"
	p.profile.LogoutArgs = []string{"-c", "exit 3"}
	err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("the premise: Logout = %v, want the vendor's logout reported unfinished", err)
	}
	if hasLogin(t, p.Workspace()) {
		t.Fatalf("the premise: a failed vendor logout left the login: %v", loginOnDisk(t, p.Workspace()))
	}
	if notice := logoutNotice(t, p); !strings.Contains(notice, harnessprofile.BundleVarName("sub")) {
		t.Errorf("after a logout whose vendor half failed, the notice says nothing of the bundle "+
			"it holds back: %q", notice)
	}
}

// A HEADLESS TOKEN DOES NOT END A LOGOUT; A LOGIN THE CAPTURE LEFT DOES. What
// ends a logout is a login written into the credential directory, and a token
// is not one: it signs the entry in through its environment. So a
// `-capture-token` that only printed a token leaves the logout standing and the
// bundle out — as the docs, the decline and the logout's notice all say — while
// one whose sign-in also left a login file has written the login of record and
// ended it like any other login.
//
// Mutation: clear the record on every carry of a login command, whether or not
// it wrote anything, and the first half is red; drop the clear from the carry,
// and the second is.
func TestAHeadlessTokenDoesNotEndALogout(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("the boot's restore: %v", err)
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	p.profile.Binary = "sh"

	p.profile.CaptureTokenArgs = []string{"-c", "echo sk-ant-oat01-PRINTED"}
	if token, err := p.CaptureToken(t.Context(), nil, &bytes.Buffer{}); err != nil || token != "sk-ant-oat01-PRINTED" {
		t.Fatalf("CaptureToken = %q, %v", token, err)
	}
	if err := p.RestoreConfiguredBundle(t.Context()); !errors.Is(err, ErrLoggedOut) {
		t.Errorf("a token-only capture ended the logout: the restore = %v, want ErrLoggedOut", err)
	}
	if hasLogin(t, p.Workspace()) {
		t.Fatalf("a token-only capture brought the bundle back: %v", loginOnDisk(t, p.Workspace()))
	}

	p.profile.CaptureTokenArgs = []string{"-c",
		`mkdir -p .fake && printf '{"token":"captured"}' > .fake/creds.json && echo sk-ant-oat01-FILED`}
	if token, err := p.CaptureToken(t.Context(), nil, &bytes.Buffer{}); err != nil || token != "sk-ant-oat01-FILED" {
		t.Fatalf("CaptureToken = %q, %v", token, err)
	}
	if got, err := os.ReadFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json")); err != nil ||
		string(got) != `{"token":"captured"}` {
		t.Fatalf("the premise: the capture's login file was not carried out (%q, %v)", got, err)
	}
	// The login of record is the capture's now; emptied later, the directory
	// is one the bundle restores into again.
	if err := os.Remove(filepath.Join(p.Workspace().CredentialsDir(), "creds.json")); err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Errorf("after a capture that left a login, the restore = %v, want the logout over", err)
	}
}

// logoutNotice is p's logout notice, failing the test on a record it cannot
// read.
func logoutNotice(t *testing.T, p *Provider) string {
	t.Helper()
	notice, err := p.LogoutNotice()
	if err != nil {
		t.Fatalf("LogoutNotice: %v", err)
	}
	return notice
}

// A LOGGED-OUT ENTRY IS TOLD OF A HEADLESS TOKEN ONLY WHERE ONE SIGNS IT IN.
// The logout's notice told every entry with a bundle that "a headless token
// from `-capture-token` or `-token-stdin` signs it in without ending this",
// and the restore's refusal that a headless token signs it in: codex, whose
// profile names no token variable, takes none, and `crewlet llm login`
// refuses both commands for it; under a token variable outside the reference
// grammar (MY-TOKEN) a token could be neither stored nor referenced; api-key
// removes the token variable from the harness's environment; and inherit-env
// never reads the secret store a token would be stored in, so the token that
// signs it in is one exported in the engine's environment. A harness with a token
// variable and no minting command is told `-token-stdin` alone. Claude Code
// as shipped is the control: it mints a token and reads a stored one, and is
// told both commands.
//
// Mutation: name both commands whatever the entry takes, and every case but
// the control is red; name both wherever a token is read at all, and the
// inherit-env and no-minting cases are.
func TestALoggedOutEntryIsToldOfAHeadlessTokenOnlyWhereOneSignsItIn(t *testing.T) {
	t.Parallel()
	none := []string{"headless", "-capture-token", "-token-stdin"}
	for _, tc := range []struct {
		name, agent string
		mode        AuthMode
		overrides   map[string]any
		// want and notWant are judged on the notice; refusal is whether the
		// restore's refusal says a headless token signs the entry in.
		want, notWant []string
		refusal       bool
	}{
		{
			name: "claude-code as shipped", agent: "claude-code",
			want:    []string{"a headless token from `-capture-token` or `-token-stdin` signs it in"},
			refusal: true,
		},
		{name: "codex", agent: "codex", notWant: none},
		{
			name: "a token variable no store can hold", agent: "claude-code",
			overrides: map[string]any{"token_env": "MY-TOKEN"}, notWant: none,
		},
		{name: "claude-code under api-key", agent: "claude-code", mode: AuthAPIKey, notWant: none},
		{
			name: "claude-code under inherit-env", agent: "claude-code", mode: AuthInheritEnv,
			want: []string{"a headless CLAUDE_CODE_OAUTH_TOKEN exported in the engine's environment, " +
				"which `-capture-token -print-token` mints, signs it in"},
			notWant: []string{"-token-stdin", "from `-capture-token`"},
			refusal: true,
		},
		{
			name: "a harness with no minting command", agent: "claude-code",
			overrides: map[string]any{"capture_token_args": []any{}},
			want:      []string{"a headless token from `-token-stdin` signs it in"},
			notWant:   []string{"-capture-token"},
			refusal:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Harness: tc.agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: tc.overrides, Auth: Auth{Mode: tc.mode},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
			// The vendor's own logout, if the profile has one, is a command
			// that succeeds and does nothing.
			p.profile.Binary = "true"
			if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			notice := logoutNotice(t, p)
			if !strings.Contains(notice, harnessprofile.BundleVarName("sub")) {
				t.Fatalf("the premise: the notice names no bundle: %q", notice)
			}
			for _, want := range tc.want {
				if !strings.Contains(notice, want) {
					t.Errorf("the notice does not say %q: %s", want, notice)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(notice, bad) {
					t.Errorf("the notice says %q, a token route this entry does not have: %s", bad, notice)
				}
			}
			err = p.RestoreConfiguredBundle(t.Context())
			if !errors.Is(err, ErrLoggedOut) {
				t.Fatalf("the restore after a logout = %v, want ErrLoggedOut", err)
			}
			if said := strings.Contains(err.Error(), "a headless token signs the entry in"); said != tc.refusal {
				t.Errorf("the refusal says a headless token signs the entry in: %v, want %v: %v",
					said, tc.refusal, err)
			}
		})
	}
}

// A LOGIN OF RECORD ENDS A LOGOUT. Whichever way the operator signs the entry
// in again — the vendor's login, -from-host, an import — the logout is over,
// and a directory left empty afterwards is restored from the bundle as it was
// before the logout.
//
// Mutation: drop the clearing from any one of the three, and its case is red.
func TestALoginOfRecordEndsALogout(t *testing.T) {
	t.Parallel()
	for name, login := range map[string]func(t *testing.T, p *Provider) error{
		"login": func(t *testing.T, p *Provider) error {
			return p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{})
		},
		"from host": func(t *testing.T, p *Provider) error {
			home := t.TempDir()
			writeHostLogin(t, home, `{"token":"k-host"}`)
			_, err := p.AdoptHostLogin(t.Context(), home)
			return err
		},
		"import": func(t *testing.T, p *Provider) error {
			return p.ImportBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"k-imported"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
			p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
			writeCreds(t, p, `{"token":"k-old"}`)
			if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			if err := login(t, p); err != nil {
				t.Fatalf("signing in again: %v", err)
			}
			if out, err := p.ws.loggedOut(); err != nil || out {
				t.Fatalf("the logout still stands after a login of record (%v, %v)", out, err)
			}
			// The directory empties some other way — an operator clearing it
			// by hand — and the bundle restores into it as it always did.
			for _, path := range loginOnDisk(t, p.ws) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
				t.Fatalf("the restore once the logout was over: %v", err)
			}
			if got := readCreds(t, p); got != `{"token":"k-bundled"}` {
				t.Errorf("the restore once the logout was over left %q, want the bundle's login", got)
			}
		})
	}
}
