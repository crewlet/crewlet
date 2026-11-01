package cliagent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// The cases here hold the SHARED LOGIN TO BEING ONE LOGIN. kimi-code,
// gemini-cli, cursor-agent and hermes each keep a login in two files, and a
// login is only a login while both come from the same sign-in: every writer
// used to replace them one rename at a time, so a writer killed or refused by
// the disk between two renames left half of one, which every presence check,
// seed, export and restore then took for a whole login. Every writer now writes
// the login in one committed step and every reader reads it whole
// ([hostbox.WriteCredentials]); these cases kill the writers, leave a write
// standing part way, and race a reader against a writer.

// twoFiles are the files a two-file login is kept in, by their names in the
// shared directory.
var twoFiles = []string{"auth.json", "config.toml"}

// twoFileLogin is a login of the two-file profile, both files naming one
// sign-in.
func twoFileLogin(signIn string) map[string]string {
	return map[string]string{
		"auth.json":   `{"token":"` + signIn + `"}`,
		"config.toml": `provider = "` + signIn + `"`,
	}
}

// twoFileConfig is a provider over stateDir whose CLI keeps ONE LOGIN IN TWO
// FILES, with a login command that writes both ([fakeStore]'s
// store-two-file-login). A function of the directory rather than of a test,
// because [runLoginWriter] builds the same provider in a process of its own.
func twoFileConfig(stateDir string, env map[string]string) Config {
	run := "-test.run=TestCLIAgentFakeCLI"
	merged := map[string]string{"FAKE_STORE": "1"}
	maps.Copy(merged, env)
	return Config{
		Key: "sub", Agent: "custom", StateDir: stateDir,
		Timeout: 20 * time.Second, MaxConcurrent: 2, Env: fakeChildEnv(merged),
		Auth: Auth{Mode: AuthSubscription},
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{run}, "model_args": []any{},
			"output": "text", "prompt_mode": "stdin",
			"credential_paths":      []any{".fake/auth.json", ".fake/config.toml"},
			"host_credential_paths": []any{".fake/auth.json", ".fake/config.toml"},
			"volatile_paths":        []any{".local/share/fake"},
			"login_args":            []any{run, "store-two-file-login"},
		},
	}
}

// twoFileProvider is [twoFileConfig] over a directory of the case's own.
func twoFileProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(twoFileConfig(dir, env))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// writeShared puts files in the shared directory directly, as a login of
// record already there.
func writeShared(t *testing.T, ws *Workspace, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(ws.sharedPath(name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readLogin is [Workspace.Login] as strings, a login that cannot be read
// failing the case.
func readLogin(t *testing.T, ws *Workspace) map[string]string {
	t.Helper()
	login, err := ws.Login(t.Context())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	out := map[string]string{}
	for name, content := range login {
		out[name] = string(content)
	}
	return out
}

// bundleOfFiles builds a bundle of several files.
func bundleOfFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	blob, err := twoFileBundle(files)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// twoFileBundle is [Provider.ExportBundle]'s format over files, for the CLI
// [twoFileConfig] runs: written by [packBundle], the format's one writer.
func twoFileBundle(files map[string]string) (string, error) {
	entries := make([]bundleFile, 0, len(files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		entries = append(entries, bundleFile{name: name, data: []byte(files[name])})
	}
	return packBundle(bundleManifest{Agent: twoFileConfig("", nil).Agent}, entries)
}

// seededHome is what a seat's first call is seeded with: its home's copy of
// each login file, "" for one it was not given.
func seededHome(t *testing.T, p *Provider, seat string) (map[string]string, error) {
	t.Helper()
	co, err := p.ws.Acquire(t.Context(), seat, "call", p)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := co.Release(t.Context()); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()
	out := map[string]string{}
	for _, name := range twoFiles {
		raw, err := os.ReadFile(filepath.Join(co.Home, ".fake", name))
		switch {
		case err == nil:
			out[name] = string(raw)
		case !os.IsNotExist(err):
			t.Fatal(err)
		}
	}
	return out, nil
}

// leaveWriteStanding leaves the shared directory as a writer of after, killed
// between its first file and its second, leaves it over before: before's
// files, after's first file over its own, and the write's journal beside them.
//
// Made by the primitive itself rather than by hand, in a directory of its own
// whose second file a directory stands in the way of, and then moved into
// place: the journal's format is the primitive's to know.
func leaveWriteStanding(t *testing.T, ws *Workspace, before, after map[string]string) {
	t.Helper()
	writeShared(t, ws, before)
	scratch := filepath.Join(t.TempDir(), "credentials")
	names := slices.Sorted(maps.Keys(after))
	if err := os.MkdirAll(filepath.Join(scratch, names[1], "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	login := map[string][]byte{}
	for name, body := range after {
		login[name] = []byte(body)
	}
	var unapplied *hostbox.UnappliedError
	if err := hostbox.WriteCredentials(scratch, names, login); !errors.As(err, &unapplied) {
		t.Fatalf("the premise: a write stopped by its second file = %v", err)
	}
	for from, to := range map[string]string{
		hostbox.CredentialJournal(scratch): hostbox.CredentialJournal(ws.CredentialsDir()),
		filepath.Join(scratch, names[0]):   ws.sharedPath(names[0]),
	} {
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}
}

// EVERY READER FINISHES A WRITE LEFT STANDING, AND NONE READS ITS HALF. A
// writer killed between its two files left the new token beside the old
// config.toml, and the next seat was seeded with that, the doctor called it
// present, an export packed it for every fresh volume, and a restore declined
// to repair it. Each of them reads the login the write committed instead.
//
// Mutation: read the login's files without finishing the write first, and
// the seat is seeded with one sign-in's token beside the other's config.toml.
func TestEveryReaderFinishesALoginWriteLeftStanding(t *testing.T) {
	t.Parallel()
	before, after := twoFileLogin("before"), twoFileLogin("after")
	for name, read := range map[string]func(*testing.T, *Provider) map[string]string{
		"a seat's seed": func(t *testing.T, p *Provider) map[string]string {
			got, err := seededHome(t, p, "dev")
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			return got
		},
		"an export": func(t *testing.T, p *Provider) map[string]string {
			blob, err := p.ExportBundle(t.Context())
			if err != nil {
				t.Fatalf("ExportBundle: %v", err)
			}
			fresh := twoFileProvider(t, nil)
			if err := fresh.RestoreBundle(t.Context(), blob); err != nil {
				t.Fatalf("RestoreBundle: %v", err)
			}
			return readLogin(t, fresh.ws)
		},
		"a restore's look for a login": func(t *testing.T, p *Provider) map[string]string {
			if err := p.ImportBundle(t.Context(), bundleOfFiles(t, twoFileLogin("bundled"))); !errors.Is(err, ErrLoginPresent) {
				t.Errorf("an import over a login a write left standing = %v, want ErrLoginPresent", err)
			}
			return readLogin(t, p.ws)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := twoFileProvider(t, nil)
			leaveWriteStanding(t, p.ws, before, after)
			if got := read(t, p); !maps.Equal(got, after) {
				t.Errorf("%s read %v, want the login the standing write committed %v", name, got, after)
			}
			if _, err := os.Stat(hostbox.CredentialJournal(p.ws.CredentialsDir())); !os.IsNotExist(err) {
				t.Errorf("the standing write was not finished: %v", err)
			}
		})
	}
}

// A LOGOUT DISCARDS A LOGIN WRITE LEFT STANDING, rather than being undone by
// it. A write killed part way is the login of record the moment its journal
// landed, so a logout that removed only the files left that write for the next
// reader to finish — and the login the operator had just removed came back,
// whole, at the next seat's start.
//
// Mutation: remove the login's files without writing the empty login, and the
// standing write puts the login back.
func TestALogoutDiscardsALoginWriteLeftStanding(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	leaveWriteStanding(t, p.ws, twoFileLogin("before"), twoFileLogin("after"))
	if err := p.Logout(t.Context(), io.Discard, io.Discard); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if got := readLogin(t, p.ws); len(got) != 0 {
		t.Errorf("after the logout the shared login is %v, want none", got)
	}
	if got, err := seededHome(t, p, "dev"); err != nil || len(got) != 0 {
		t.Errorf("after the logout a seat was seeded with %v (%v), want nothing", got, err)
	}
}

// A LOGIN THAT CANNOT BE READ WHOLE IS NEVER READ AS A LOGIN. Where a write
// left standing cannot be finished — here a directory stands where its second
// file goes — the doctor and the list say the login is unknown rather than
// present, and no seat is seeded with the file that did land.
//
// Mutation: answer an unreadable login as "no login" in the sign-in, and the
// list says `none`.
func TestALoginThatCannotBeReadWholeIsNeverReadAsOne(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	leaveWriteStanding(t, p.ws, map[string]string{}, twoFileLogin("after"))
	obstacle := filepath.Join(p.ws.sharedPath("config.toml"), "in-the-way")
	if err := os.MkdirAll(obstacle, 0o700); err != nil {
		t.Fatal(err)
	}
	if present, err := p.ws.HasLogin(t.Context()); err == nil {
		t.Fatalf("HasLogin = %v over a write that cannot be finished, want an error", present)
	}
	if got := p.SignInState(t.Context()); got != "unknown" {
		t.Errorf("SignInState = %q, want unknown", got)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	if !strings.HasPrefix(d.Credentials, "unknown") ||
		!slices.ContainsFunc(d.Problems, func(s string) bool { return strings.Contains(s, "could not be read") }) {
		t.Errorf("the doctor said credentials %q, problems %v; want unknown, and why", d.Credentials, d.Problems)
	}
	if got, err := seededHome(t, p, "dev"); err == nil {
		t.Errorf("a seat was seeded with %v from a login that cannot be read whole", got)
	}

	if err := os.RemoveAll(filepath.Dir(obstacle)); err != nil {
		t.Fatal(err)
	}
	got, err := seededHome(t, p, "dev")
	if err != nil {
		t.Fatalf("Acquire once the write could be finished: %v", err)
	}
	if want := twoFileLogin("after"); !maps.Equal(got, want) {
		t.Errorf("the seat was seeded with %v, want %v", got, want)
	}
}

// AN ADOPTED LOGIN IS THE HUMAN'S WHOLE LOGIN. A home holding one of the two
// files is a login of one file, and the other file of the login it replaces is
// no part of it: kept beside the adopted one, it made a login of two sign-ins.
//
// Mutation: write only the files the human's home holds, and the replaced
// login's config.toml survives beside the adopted token.
func TestAnAdoptedLoginKeepsNoFileOfTheOneItReplaced(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	writeShared(t, p.ws, twoFileLogin("replaced"))
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fake", "auth.json"), []byte(`{"token":"human"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AdoptHostLogin(t.Context(), home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if got, want := readLogin(t, p.ws), map[string]string{"auth.json": `{"token":"human"}`}; !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the human's login alone %v", got, want)
	}
}

// A SIGN-IN IS WRITTEN AS THE WHOLE LOGIN IT LEFT. Its CLI renewed the token and
// left config.toml as the command seeded it — or held none — while another
// writer's config.toml landed in the shared directory. Carried out as the files
// it changed alone, the new token was written beside the other writer's
// config.toml, a login of two sign-ins: the later sign-in wins whole, so the
// file it left goes back as it left it, and a file it never held is not kept.
//
// Mutation: carry a login as its changed files over the shared login, and
// both cases hold the other writer's config.toml; carry every file the home
// holds but keep the shared files it never held, and the second still does.
func TestASignInIsWrittenAsTheWholeLoginItLeft(t *testing.T) {
	t.Parallel()
	token := `{"token":"signed-in"}`
	for name, c := range map[string]struct {
		before, want map[string]string
	}{
		"over a login it left a file of": {
			before: twoFileLogin("before"),
			want:   map[string]string{"auth.json": token, "config.toml": twoFileLogin("before")["config.toml"]},
		},
		"into a directory it found empty": {
			before: map[string]string{},
			want:   map[string]string{"auth.json": token},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := twoFileProvider(t, nil)
			writeShared(t, p.ws, c.before)
			p.profile.LoginArgs = []string{"-test.run=TestCLIAgentFakeCLI", "store-token-login"}
			// Another writer's config.toml lands while the sign-in runs.
			p.env = fakeChildEnv(map[string]string{
				"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "signed-in",
				"FAKE_MIDRUN_VERB": "store-token-login",
				"FAKE_MIDRUN_WRITES": midrun(t, map[string]string{
					p.ws.sharedPath("config.toml"): twoFileLogin("other")["config.toml"],
				}),
			})
			if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("BrokerLogin: %v", err)
			}
			if got := readLogin(t, p.ws); !maps.Equal(got, c.want) {
				t.Errorf("the shared login is %v, want the login the sign-in left %v", got, c.want)
			}
		})
	}
}

// A FILE A SIGN-IN REMOVED GOES BACK ONLY FROM THE LOGIN IT WAS SEEDED FROM.
// Its CLI renewed the token and removed config.toml. No CLI's removal reaches
// the shared directory — a CLI that dropped a file is no reason to log the
// fleet out of the login it was seeded with — so the shared login's
// config.toml stays beside the new token while it is still that login's: the
// copy the sign-in was seeded with, or that copy as a seat refreshed it while
// the sign-in ran, which is the same login, newer. A config.toml an operator's
// write put there while the sign-in ran is not that login's: it is another
// sign-in's, and kept it made the shared login one sign-in's token beside
// another's config.toml. It is dropped, as a file the sign-in never held is,
// leaving the login the sign-in left. Bytes cannot tell the last two apart —
// each is a config.toml that is not the seed — so the operator's write is the
// one that moved the operator epoch.
//
// Mutation: carry a login as the files the home holds alone, and the first two
// cases lose config.toml; keep only a copy whose bytes are the seed's, and the
// second loses the seat's refresh; keep whatever the shared directory holds of
// a file the home was seeded with, or read the epoch the seed is judged by when
// the sign-in ends rather than with the seed, and the third keeps the
// operator's config.toml.
func TestAFileASignInRemovedGoesBackOnlyFromTheLoginItWasSeededFrom(t *testing.T) {
	t.Parallel()
	token := `{"token":"signed-in"}`
	before := twoFileLogin("before")
	refreshed := `provider = "before, refreshed"`
	for name, c := range map[string]struct {
		// meanwhile is what lands while the sign-in runs, by path, nil for
		// nothing.
		meanwhile func(ws *Workspace) map[string]string
		want      map[string]string
	}{
		"while the shared copy is the one it was seeded with": {
			want: map[string]string{"auth.json": token, "config.toml": before["config.toml"]},
		},
		"after a seat refreshed the shared copy": {
			meanwhile: func(ws *Workspace) map[string]string {
				return map[string]string{ws.sharedPath("config.toml"): refreshed}
			},
			want: map[string]string{"auth.json": token, "config.toml": refreshed},
		},
		"after an operator replaced the login": {
			meanwhile: func(ws *Workspace) map[string]string {
				other := twoFileLogin("other")
				return map[string]string{
					ws.sharedPath("auth.json"):               other["auth.json"],
					ws.sharedPath("config.toml"):             other["config.toml"],
					filepath.Join(ws.Root(), loginEpochFile): "1\n",
				}
			},
			want: map[string]string{"auth.json": token},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := twoFileProvider(t, nil)
			writeShared(t, p.ws, before)
			p.profile.LoginArgs = []string{"-test.run=TestCLIAgentFakeCLI", "store-token-only-login"}
			env := map[string]string{"FAKE_STORE": "1", "FAKE_LOGIN_KEY": "signed-in"}
			if c.meanwhile != nil {
				env["FAKE_MIDRUN_VERB"] = "store-token-only-login"
				env["FAKE_MIDRUN_WRITES"] = midrun(t, c.meanwhile(p.ws))
			}
			p.env = fakeChildEnv(env)
			if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("BrokerLogin: %v", err)
			}
			if got := readLogin(t, p.ws); !maps.Equal(got, c.want) {
				t.Errorf("the shared login is %v, want %v", got, c.want)
			}
		})
	}
}

// THE OPERATOR EPOCH TELLS A SEAT'S REFRESH FROM ANOTHER SIGN-IN, written by the
// writers themselves rather than by hand: a sign-in seeded from one login, whose
// CLI renewed the token and removed config.toml, is carried out after a real
// writer replaced the shared config.toml while it ran. A seat's refresh moves no
// epoch and is the seeded login, newer, so its config.toml stays beside the new
// token; `-from-host`, and a logout followed by an import, move it, so theirs is
// another sign-in's and is dropped. Judged by bytes alone, the seat's refresh
// was dropped with them, and a sign-in that happened to overlap a seat's call
// logged the fleet out of the file it removed.
//
// Mutation: drop every removed file's copy that is not the seed's, or seed a
// sign-in with no epoch, and the seat's case loses config.toml; keep every copy
// whatever the epoch says, and the other two keep the other sign-in's
// config.toml; move no epoch in -from-host, and its case does; in neither a
// logout nor an import, and the last does.
func TestTheOperatorEpochTellsASeatsRefreshFromAnotherSignIn(t *testing.T) {
	t.Parallel()
	token := `{"token":"signed-in"}`
	for name, c := range map[string]struct {
		meanwhile func(t *testing.T, p *Provider)
		want      map[string]string
	}{
		"a seat's refresh": {
			meanwhile: func(t *testing.T, p *Provider) {
				co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				writeFile(t, filepath.Join(co.Home, ".fake", "config.toml"), `provider = "before, refreshed"`)
				if err := co.Release(t.Context()); err != nil {
					t.Fatalf("Release: %v", err)
				}
				if got := readLogin(t, p.ws)["config.toml"]; got != `provider = "before, refreshed"` {
					t.Fatalf("the premise: the seat's refresh landed = %q", got)
				}
			},
			want: map[string]string{"auth.json": token, "config.toml": `provider = "before, refreshed"`},
		},
		"-from-host": {
			meanwhile: func(t *testing.T, p *Provider) {
				home := t.TempDir()
				for name, body := range twoFileLogin("human") {
					writeFile(t, filepath.Join(home, ".fake", name), body)
				}
				if _, err := p.AdoptHostLogin(t.Context(), home); err != nil {
					t.Fatalf("AdoptHostLogin: %v", err)
				}
			},
			want: map[string]string{"auth.json": token},
		},
		"a logout, then an import": {
			meanwhile: func(t *testing.T, p *Provider) {
				if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
					t.Fatalf("Logout: %v", err)
				}
				if err := p.ImportBundle(t.Context(), bundleOfFiles(t, twoFileLogin("imported"))); err != nil {
					t.Fatalf("ImportBundle: %v", err)
				}
			},
			want: map[string]string{"auth.json": token},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := twoFileProvider(t, nil)
			writeShared(t, p.ws, twoFileLogin("before"))
			home, cache := t.TempDir(), t.TempDir()
			gen, err := p.ws.seed(t.Context(), home, cache, p, syncLogin)
			if err != nil {
				t.Fatalf("seeding the sign-in: %v", err)
			}
			// Carried out by its own command as it ends, as
			// [Provider.runInCredentialHome] carries one: not ordered after
			// any epoch, but judged by the one it was seeded at.
			gen.After = nil
			c.meanwhile(t, p)
			// The sign-in's CLI renews the token and removes config.toml.
			writeFile(t, filepath.Join(home, ".fake", "auth.json"), token)
			if err := os.Remove(filepath.Join(home, ".fake", "config.toml")); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ws.syncCredentialsOut(t.Context(), home, cache, p, gen); err != nil {
				t.Fatalf("carrying the sign-in out: %v", err)
			}
			if got := readLogin(t, p.ws); !maps.Equal(got, c.want) {
				t.Errorf("the shared login is %v, want %v", got, c.want)
			}
		})
	}
}

// AN OPERATOR'S WRITE IS COUNTED BEFORE IT LANDS. Every reader of the operator
// epoch takes an unmoved one as proof that no operator's write landed — a login
// a failed export kept is carried out over the shared one, and a file a sign-in
// removed goes back as the shared login holds it — so the epoch moves before the
// write it counts. Moved after, a write killed between the two, or kept by a
// power loss that lost the epoch, landed uncounted. Neither can be stopped on
// cue here, so the order is read off an epoch that cannot be moved: an
// operator's write behind it writes nothing. A logout is the exception, by
// design: what an operator relies on it for is the removal, so it removes the
// login it could not count, and says so.
//
// Mutation: move the epoch after the write in -from-host or in an import, and
// that case writes its login though its epoch could not move; move none in a
// logout, and it reports a removal it did not count as counted.
func TestAnOperatorsWriteIsCountedBeforeItLands(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		before map[string]string
		write  func(t *testing.T, p *Provider) error
		want   map[string]string
	}{
		"-from-host": {
			before: twoFileLogin("before"),
			write: func(t *testing.T, p *Provider) error {
				home := t.TempDir()
				for name, body := range twoFileLogin("human") {
					writeFile(t, filepath.Join(home, ".fake", name), body)
				}
				_, err := p.AdoptHostLogin(t.Context(), home)
				return err
			},
			want: twoFileLogin("before"),
		},
		"an import": {
			before: map[string]string{},
			write: func(t *testing.T, p *Provider) error {
				return p.ImportBundle(t.Context(), bundleOfFiles(t, twoFileLogin("imported")))
			},
			want: map[string]string{},
		},
		"a logout": {
			before: twoFileLogin("before"),
			write: func(t *testing.T, p *Provider) error {
				return p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
			},
			want: map[string]string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := twoFileProvider(t, nil)
			writeShared(t, p.ws, c.before)
			// An epoch that can be neither read nor replaced: a directory
			// with something in it.
			if err := os.MkdirAll(filepath.Join(p.ws.Root(), loginEpochFile, "in-the-way"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := c.write(t, p); err == nil {
				t.Fatal("a write whose epoch could not be moved was reported as counted")
			}
			if got := readLogin(t, p.ws); !maps.Equal(got, c.want) {
				t.Errorf("the shared login is %v, want %v", got, c.want)
			}
		})
	}
}

// A REFRESH IS WRITTEN ONLY OVER THE WHOLE LOGIN IT WAS SEEDED FROM. A seat
// refreshed the token while somebody else rewrote the config.toml beside it:
// judged a file at a time the token's file still held its seed, so the
// refresh was written — and the shared login became one token beside another
// writer's config.toml, a login no CLI wrote. The shared login moved past the
// seed, so the newer one is kept whole.
//
// Mutation: compare each changed file with its own seed alone, and the
// refreshed token lands beside the other writer's config.toml.
func TestARefreshIsNeverWrittenBesideAnotherWritersFile(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	writeShared(t, p.ws, twoFileLogin("seed"))
	co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	writeShared(t, p.ws, map[string]string{"config.toml": `provider = "another writer"`})
	if err := os.WriteFile(filepath.Join(co.Home, ".fake", "auth.json"), []byte(`{"token":"refreshed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	want := map[string]string{"auth.json": `{"token":"seed"}`, "config.toml": `provider = "another writer"`}
	if got := readLogin(t, p.ws); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the newer one kept whole %v", got, want)
	}
}

// A LOGIN CARRIED AGAIN ENDS THE LOGOUT ITS FIRST CARRY DID NOT. A login
// command's carry wrote its login and then could not clear the logout's
// record, so the carry is retried whole — and the retry finds its login
// already in place. That is a login of record written all the same, so the
// logout is over and a directory emptied later is one a configured bundle
// restores into again. Answered as a carry with nothing to write, the retry
// left the logout standing for good.
//
// Mutation: return before the logout's clearing when the shared login already
// holds the home's login, and the record still stands after the retry.
func TestALoginCarriedAgainEndsTheLogout(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	home := t.TempDir()
	for name, body := range twoFileLogin("new") {
		path := filepath.Join(home, ".fake", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gen := generation{Mode: syncLogin, Seeded: map[string]string{}}
	// The logout's record as one its clearing cannot remove: a directory
	// with something in it.
	record := filepath.Join(p.ws.root, loggedOutFile)
	if err := os.MkdirAll(filepath.Join(record, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil, gen); err == nil {
		t.Fatal("the premise: a carry that could not clear the logout = nil, want its failure")
	}
	if got := readLogin(t, p.ws); !maps.Equal(got, twoFileLogin("new")) {
		t.Fatalf("the premise: the first carry's login landed = %v", got)
	}
	// The record as a logout writes it, which the retry can remove.
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	if err := p.ws.markLoggedOut(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil, gen); err != nil {
		t.Fatalf("the retried carry: %v", err)
	}
	if out, err := p.ws.loggedOut(); err != nil || out {
		t.Errorf("the logout still stands after its login of record was carried again (%v, %v)", out, err)
	}
	if got := readLogin(t, p.ws); !maps.Equal(got, twoFileLogin("new")) {
		t.Errorf("the retried carry left the shared login %v, want %v", got, twoFileLogin("new"))
	}
}

// A SEED NEVER READS A WRITE IN PROGRESS. Another process writes the two
// logins in turn, under the lock every writer takes, while seats start: each
// is seeded with one login or the other, never a file of each. Read a file at
// a time without the lock, a seat starting while the writer was between its
// two files took the token of one sign-in and the config.toml of the other.
//
// The files are large — 64 KiB each — so that the moment
// between a writer's two files, which is what a reader without the lock
// stumbles into, is long enough to be met.
//
// Mutation: read the login's files without the lock and without finishing a
// write left standing, and a seat is seeded with half of each.
func TestASeedNeverReadsAWriteInProgress(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	pad := strings.Repeat("x", 64<<10)
	logins := []map[string]string{}
	for _, signIn := range []string{"first", "second"} {
		login := map[string]string{}
		for name, body := range twoFileLogin(signIn) {
			login[name] = body + "\n# " + pad
		}
		logins = append(logins, login)
	}
	whose := func(got map[string]string) map[string]string {
		out := map[string]string{}
		for name, body := range got {
			out[name] = "neither"
			for i, login := range logins {
				if body == login[name] {
					out[name] = []string{"first", "second"}[i]
				}
			}
		}
		return out
	}
	writeShared(t, p.ws, logins[0])

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	written := make(chan error, 1)
	go func() {
		written <- func() error {
			for i := 0; ctx.Err() == nil; i++ {
				// Another process's writer: its own descriptor on the lock.
				release, err := hostbox.LockCredentials(ctx, p.ws.CredentialsDir())
				if err != nil {
					return nil //nolint:nilerr // the case is over
				}
				login := map[string][]byte{}
				for name, body := range logins[i%2] {
					login[name] = []byte(body)
				}
				err = hostbox.WriteCredentials(p.ws.CredentialsDir(), p.ws.loginNames(), login)
				release()
				if err != nil {
					return err
				}
				// Let go for longer than a seat waiting for the lock waits
				// between two looks at it, so every seat gets its turn.
				time.Sleep(12*time.Millisecond + time.Duration(rand.IntN(4000))*time.Microsecond)
			}
			return nil
		}()
	}()
	for i := range 60 {
		got, err := seededHome(t, p, "dev")
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if !maps.Equal(got, logins[0]) && !maps.Equal(got, logins[1]) {
			t.Fatalf("seat %d was seeded with files from %v — not one whole login", i, whose(got))
		}
	}
	cancel()
	if err := <-written; err != nil {
		t.Fatalf("the writer: %v", err)
	}
}

// loginWriterEnv makes this test binary a WRITER of a shared login rather than
// the suite, by the operation it names: see [runLoginWriter].
const loginWriterEnv = "CREWLET_CLIAGENT_LOGIN_WRITER"

// The writer's inputs, beside [loginWriterEnv].
const (
	writerStateEnv = "CREWLET_CLIAGENT_LOGIN_WRITER_STATE"
	writerHomesEnv = "CREWLET_CLIAGENT_LOGIN_WRITER_HOMES"
)

// runLoginWriter is a process of its own — a `crewlet llm` command, an engine
// running seats — writing the shared login over and over by one of its
// writers, for [TestNoWriterKilledPartWayLeavesHalfALogin] to kill at any
// instant. It says "ready" once its first write has landed.
//
//   - import-logout: an import of one login, then a logout, in turn;
//   - adopt: `-from-host` from each of two homes in turn;
//   - carry: a seat seeded with one login whose CLI rewrote it to the other,
//     carried back by its release, in turn.
func runLoginWriter(op string) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx := context.Background()
	p, err := New(twoFileConfig(os.Getenv(writerStateEnv), nil))
	if err != nil {
		fail(err)
	}
	logins := []map[string]string{twoFileLogin("first"), twoFileLogin("second")}
	var step func(i int) error
	switch op {
	case "import-logout":
		blob, err := twoFileBundle(logins[0])
		if err != nil {
			fail(err)
		}
		step = func(i int) error {
			if i%2 == 0 {
				return p.ImportBundle(ctx, blob)
			}
			return p.Logout(ctx, io.Discard, io.Discard)
		}
	case "adopt":
		homes := filepath.SplitList(os.Getenv(writerHomesEnv))
		step = func(i int) error {
			_, err := p.AdoptHostLogin(ctx, homes[i%2])
			return err
		}
	case "carry":
		blob, err := twoFileBundle(logins[1])
		if err != nil {
			fail(err)
		}
		if err := p.ImportBundle(ctx, blob); err != nil {
			fail(err)
		}
		step = func(i int) error {
			co, err := p.ws.Acquire(ctx, "dev", fmt.Sprint(i), p)
			if err != nil {
				return err
			}
			for name, body := range logins[i%2] {
				if err := os.WriteFile(filepath.Join(co.Home, ".fake", name), []byte(body), 0o600); err != nil {
					return err
				}
			}
			return co.Release(ctx)
		}
	default:
		fail(fmt.Errorf("no writer %q", op))
	}
	for i := 0; ; i++ {
		if err := step(i); err != nil {
			fail(err)
		}
		if i == 0 {
			fmt.Println("ready")
		}
	}
}

// NO WRITER KILLED PART WAY LEAVES HALF A LOGIN. Each writer of the shared
// login — an import and a logout, `-from-host`, a seat's carry — runs in a
// process of its own, writing over and over, and is killed at a random moment,
// again and again; what the directory holds is then read as the next process
// reads it: every time, one whole login. Written a file at a time, a kill
// between two renames left one sign-in's token beside another's config.toml,
// or one file of a login the logout was removing. Most kills land inside a
// write, which is counted, so a run in which none did certifies nothing and
// fails.
//
// Mutation: write the login a file at a time in any one writer — the restore,
// the logout, `-from-host` or the carry — and its case finds half a login.
func TestNoWriterKilledPartWayLeavesHalfALogin(t *testing.T) {
	t.Parallel()
	first, second := twoFileLogin("first"), twoFileLogin("second")
	homes := []string{t.TempDir(), t.TempDir()}
	for i, login := range []map[string]string{first, second} {
		if err := os.MkdirAll(filepath.Join(homes[i], ".fake"), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range login {
			if err := os.WriteFile(filepath.Join(homes[i], ".fake", name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for op, whole := range map[string][]map[string]string{
		"import-logout": {{}, first},
		"adopt":         {first, second},
		"carry":         {first, second},
	} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			const trials = 30
			midWrite := 0
			for trial := range trials {
				state := t.TempDir()
				killWriterAtRandom(t, op, state, homes)
				p, err := New(twoFileConfig(state, nil))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if _, err := os.Stat(hostbox.CredentialJournal(p.ws.CredentialsDir())); err == nil {
					midWrite++
				}
				got := readLogin(t, p.ws)
				forgetWorkspace(state)
				if !slices.ContainsFunc(whole, func(w map[string]string) bool { return maps.Equal(got, w) }) {
					t.Fatalf("trial %d: %s killed part way left %v — not one whole login", trial, op, got)
				}
			}
			t.Logf("%d of %d kills landed inside a write", midWrite, trials)
			if midWrite == 0 {
				t.Fatalf("none of %d kills landed inside a write, so the case certified nothing", trials)
			}
		})
	}
}

// killWriterAtRandom starts [runLoginWriter] over state, waits for its first
// write, and kills it at a random moment of the writes that follow.
func killWriterAtRandom(t *testing.T, op, state string, homes []string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), loginWriterEnv+"="+op, writerStateEnv+"="+state,
		writerHomesEnv+"="+strings.Join(homes, string(filepath.ListSeparator)))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the %s writer did not start (%q, %v): %s", op, line, err, stderr.String())
	}
	time.Sleep(time.Duration(rand.IntN(30_000)) * time.Microsecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

// A LOGIN WRITE THAT COMMITTED IS WRITTEN, though the disk refused one of its
// files: its journal landed, so it is the login of record and the next reader
// puts it in place — and the steps that follow a write follow it. Reported as
// a write that failed, `-from-host` left the logout standing, so the configured
// bundle stayed held back from a directory holding a login; and a logout
// reported the login it had removed as still there. (The operator epoch has
// moved either way: each moves it before its write ([Workspace.bumpLoginEpoch]).)
//
// Each is a writer that reads nothing first, so something in the way of one
// shared file stops the write after its journal lands — a writer that reads
// the login first is refused by the same obstacle before it writes.
//
// NOT PARALLEL: it reads the process's log.
//
// Mutation: answer a write that committed and could not be put in place as a
// failure in writeLoginLocked, and both cases are red.
func TestALoginWriteThatCommittedIsWrittenThoughAFileWasRefused(t *testing.T) {
	logs := captureLogs(t)
	inTheWay := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(path, "in-the-way"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	epoch := func(t *testing.T, p *Provider) uint64 {
		t.Helper()
		e, err := p.ws.loginEpoch()
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	loggedOut := func(t *testing.T, p *Provider) bool {
		t.Helper()
		out, err := p.ws.loggedOut()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("-from-host", func(t *testing.T) {
		logs.Reset()
		p := twoFileProvider(t, nil)
		// The record as a logout writes it: under the shared login's lock,
		// whose file is what brings the CLI's share of the state directory
		// into being on a fresh one.
		unlock, err := p.ws.lockShared(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		err = p.ws.markLoggedOut()
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		before := epoch(t, p)
		home := t.TempDir()
		for name, body := range twoFileLogin("human") {
			writeFile(t, filepath.Join(home, ".fake", name), body)
		}
		obstacle := p.ws.sharedPath("config.toml")
		inTheWay(t, obstacle)
		if _, err := p.AdoptHostLogin(t.Context(), home); err != nil {
			t.Fatalf("an adoption whose write committed was reported as failed: %v", err)
		}
		if !strings.Contains(logs.String(), "cli_agent_login_unapplied") {
			t.Errorf("a login not yet put in place was not said: %q", logs.String())
		}
		if loggedOut(t, p) {
			t.Error("the adopted login of record left the logout standing")
		}
		if epoch(t, p) == before {
			t.Error("the adopted login of record left the operator epoch where it was")
		}
		if err := os.RemoveAll(obstacle); err != nil {
			t.Fatal(err)
		}
		if got, want := readLogin(t, p.ws), twoFileLogin("human"); !maps.Equal(got, want) {
			t.Errorf("the next reader read %v, want the login the write committed %v", got, want)
		}
	})

	t.Run("logout", func(t *testing.T) {
		logs.Reset()
		p := twoFileProvider(t, nil)
		writeShared(t, p.ws, map[string]string{"config.toml": twoFileLogin("before")["config.toml"]})
		before := epoch(t, p)
		// In the way of the removal sorted first, so the other is still on
		// disk until a reader finishes the logout.
		obstacle := p.ws.sharedPath("auth.json")
		inTheWay(t, obstacle)
		if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("a logout whose removal committed was reported as failed: %v", err)
		}
		if !strings.Contains(logs.String(), "cli_agent_login_unapplied") {
			t.Errorf("a removal not yet put in place was not said: %q", logs.String())
		}
		if !loggedOut(t, p) || epoch(t, p) == before {
			t.Error("the logout did not record itself and move the operator epoch on")
		}
		if err := os.RemoveAll(obstacle); err != nil {
			t.Fatal(err)
		}
		if got := readLogin(t, p.ws); len(got) != 0 {
			t.Errorf("the next reader read %v, want the empty login the logout committed", got)
		}
	})
}

// A RUN'S BASELINE NEVER BECOMES A LOGIN ITS HOME DOES NOT HOLD. Mid-run, a
// seat's home changed only a file a refresh never carries — one it was not
// seeded with — while another writer moved the shared login past the seed.
// The carry has nothing to write, and the shared login is not this home's:
// taken for its new baseline, the run's later refresh of its own stale token
// passed the compare-and-swap at release and reverted the newer login.
//
// Mutation: answer the shared login as the baseline whenever a carry has
// nothing to write, and the release writes the stale refresh over the newer
// token.
func TestARunsBaselineNeverBecomesALoginItsHomeDoesNotHold(t *testing.T) {
	t.Parallel()
	p := twoFileProvider(t, nil)
	writeShared(t, p.ws, map[string]string{"auth.json": `{"token":"seed"}`})
	co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	writeShared(t, p.ws, map[string]string{"auth.json": `{"token":"newer"}`})
	home := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(co.Home, ".fake", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home("config.toml", `provider = "this home's own"`)
	if err := co.carryRefreshed(t.Context()); err != nil {
		t.Fatalf("the mid-run carry: %v", err)
	}
	home("auth.json", `{"token":"refreshed from the seed"}`)
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	want := map[string]string{"auth.json": `{"token":"newer"}`}
	if got := readLogin(t, p.ws); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the newer one kept %v", got, want)
	}
}
