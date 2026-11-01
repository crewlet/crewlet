package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The cases here hold WHEN A LOGIN'S INSTALL RECORD MOVES ([LoginInstall]):
// on every write that installs a login and on no other, and never into
// anything a harness reads.

// installRecord is the record the entry's credential directory holds, a record
// that cannot be read failing the case.
func installRecord(t *testing.T, p *Provider) LoginInstall {
	t.Helper()
	g, err := p.ws.LoginInstall()
	if err != nil {
		t.Fatalf("LoginInstall: %v", err)
	}
	return g
}

// seedRecord gives the entry's login the record g, as a login installed at
// g.At would have left it, so a case can tell a new record from the one
// before it.
func seedRecord(t *testing.T, p *Provider, g LoginInstall) {
	t.Helper()
	unlock, err := p.ws.lockShared(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	login, err := p.ws.readLoginLocked()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ws.installLoginLocked(t.Context(), p.key, login, g); err != nil {
		t.Fatal(err)
	}
}

// recordAt is an install record installed at at, with an id of its own.
func recordAt(t *testing.T, at time.Time) LoginInstall {
	t.Helper()
	g, err := newLoginInstall(at)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// EVERY WRITE THAT INSTALLS A LOGIN WRITES A NEW RECORD: a login command
// carried out, `-from-host`, and an import of a bundle that carries none. Each
// starts from a login holding an older record, so a write that left the record
// as it was, or wrote none, is red.
//
// Mutation: drop the record from the login carry's write, from -from-host's,
// or the fresh record an import of a recordless bundle is given, and that case
// is red.
func TestEveryLoginInstalledWritesANewRecord(t *testing.T) {
	t.Parallel()
	for name, install := range map[string]func(t *testing.T, p *Provider) error{
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
			// An import never replaces a login, so it lands in a directory
			// a logout emptied.
			if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			return p.ImportBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"k-imported"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
			writeCreds(t, p, `{"token":"k-old"}`)
			old := recordAt(t, time.Now().Add(-time.Hour))
			seedRecord(t, p, old)
			before := time.Now()
			if err := install(t, p); err != nil {
				t.Fatalf("installing a login: %v", err)
			}
			got := installRecord(t, p)
			if got.IsZero() || got.ID == old.ID {
				t.Fatalf("the login installed holds record %+v, want a new one in place of %+v", got, old)
			}
			if got.At.Before(before.Add(-time.Second)) || !got.Newer(old) {
				t.Errorf("the new record %+v is not newer than the one it replaced %+v", got, old)
			}
		})
	}
}

// A LOGOUT TAKES THE RECORD WITH THE LOGIN. A record left behind would name a
// login that is gone, and order the next bundle against it.
//
// Mutation: write the logout's removal without the record, and this is red.
func TestALogoutRemovesTheInstallRecord(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, nil)
	writeCreds(t, p, `{"token":"k-old"}`)
	seedRecord(t, p, recordAt(t, time.Now()))
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if got := installRecord(t, p); !got.IsZero() {
		t.Errorf("the logout left the install record %+v", got)
	}
	if got := credentialEntries(t, p); len(got) != 0 {
		t.Errorf("the logout left %v in the credential directory", got)
	}
}

// A REFRESH IS THE SAME SIGN-IN, so the record a seat's refresh is carried
// back under is the one the login was installed with: a refresh that minted a
// record would read as a new sign-in and lift a sit-out the vendor still holds.
//
// Mutation: write the refresh carry through installLoginLocked with a new
// record, and this is red.
func TestARefreshCarriedBackKeepsTheInstallRecord(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("BrokerLogin: %v", err)
	}
	installed := installRecord(t, p)
	if installed.IsZero() {
		t.Fatal("the premise: the login holds no install record")
	}
	co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	refreshFile(t, co, `{"token":"k-refreshed"}`)
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := readCreds(t, p); got != `{"token":"k-refreshed"}` {
		t.Fatalf("the premise: the refresh was not carried back (%q)", got)
	}
	if got := installRecord(t, p); got != installed {
		t.Errorf("a refresh moved the install record from %+v to %+v", installed, got)
	}
}

// THE RECORD REACHES NO HARNESS: a seat's home is seeded from the login's own
// files, a box from the paths SandboxCredentials names, and a bundle packs the
// login's files with the record in its manifest. A record handed to a harness is a
// file in its home the vendor never wrote, and one packed as a file would be
// restored as a credential file no profile keeps.
//
// Mutation: add the record to the login's names (loginNames), and every half
// of this is red.
func TestTheInstallRecordReachesNoHarness(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-login"})
	if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("BrokerLogin: %v", err)
	}
	if installRecord(t, p).IsZero() {
		t.Fatal("the premise: the login holds no install record")
	}

	co, err := p.ws.Acquire(t.Context(), "dev", "call", p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = co.Release(t.Context()) }()
	err = filepath.WalkDir(co.Home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == loginInstallFile {
			t.Errorf("the seat's home holds the install record at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for rel, source := range p.SandboxCredentials() {
		if filepath.Base(rel) == loginInstallFile || filepath.Base(source) == loginInstallFile {
			t.Errorf("a box is seeded with the install record (%s from %s)", rel, source)
		}
	}

	blob, err := p.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	for _, name := range bundleEntries(t, blob) {
		if name == loginInstallFile {
			t.Errorf("the bundle packs the install record as a file")
		}
	}
}

// bundleEntries names every entry of a bundle, in order.
func bundleEntries(t *testing.T, blob string) []string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// installedLoginOf is a bundle of the `custom` harness holding one creds.json of
// body, carrying the install record g, as another node's export of a login
// installed with g writes one.
func installedLoginOf(t *testing.T, body string, g LoginInstall) string {
	t.Helper()
	blob, err := packBundle(bundleManifest{Harness: "custom", Login: &g},
		[]bundleFile{{name: "creds.json", data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// THE NEWEST LOGIN WINS (B3). A bundle carries its login's install record, and
// the automatic restore replaces the login a node holds when the bundle's is a
// different login installed later, so a re-login exported to the store
// reaches a node that already held a login, and never when it is older, so a
// boot-time blob never overwrites a login made on this node after it. The
// bundle's own record is what the node then holds, so a second restore of it is
// no newer and every node restoring it holds one record.
//
// Mutation: restore only into an empty directory (red: "newer replaces"); drop
// the Newer comparison (red: "older keeps"); write a fresh record in place of
// the bundle's (red: "the bundle's record").
func TestTheNewestLoginWinsARestore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	t.Run("newer replaces", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, nil)
		writeCreds(t, p, `{"token":"k-held"}`)
		seedRecord(t, p, recordAt(t, now.Add(-time.Hour)))
		bundled := recordAt(t, now)
		if err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k-newer"}`, bundled)); err != nil {
			t.Fatalf("RestoreBundle: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-newer"}` {
			t.Fatalf("a newer bundle left the older login in place: %q", got)
		}
		if got := installRecord(t, p); got != bundled {
			t.Errorf("the restore holds record %+v, want the bundle's own %+v", got, bundled)
		}
		// Restored again, it is the login already held, and writes nothing.
		writeCreds(t, p, `{"token":"k-newer-refreshed"}`)
		if err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k-newer"}`, bundled)); err != nil {
			t.Fatalf("RestoreBundle again: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-newer-refreshed"}` {
			t.Errorf("the same bundle restored twice wrote over the refreshed login: %q", got)
		}
	})
	t.Run("older keeps", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, map[string]string{"FAKE_LOGIN_KEY": "k-local"})
		bundled := recordAt(t, now.Add(-time.Hour))
		// A fresh local login, later than the bundle.
		if err := p.BrokerLogin(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("BrokerLogin: %v", err)
		}
		held := installRecord(t, p)
		if err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k-older"}`, bundled)); err != nil {
			t.Fatalf("RestoreBundle: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-local"}` {
			t.Errorf("an older bundle overwrote a fresh local login: %q", got)
		}
		if got := installRecord(t, p); got != held {
			t.Errorf("an older bundle moved the record from %+v to %+v", held, got)
		}
	})
	t.Run("a login with no record", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, nil)
		writeCreds(t, p, `{"token":"k-unrecorded"}`)
		bundled := recordAt(t, now)
		if err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k-recorded"}`, bundled)); err != nil {
			t.Fatalf("RestoreBundle: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-recorded"}` {
			t.Errorf("a recorded bundle left a login with no record in place: %q", got)
		}
	})
	t.Run("a bundle with no record", func(t *testing.T) {
		t.Parallel()
		p := fileProvider(t, nil)
		writeCreds(t, p, `{"token":"k-held"}`)
		if err := p.RestoreBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"k-unrecorded"}`)); err != nil {
			t.Fatalf("RestoreBundle: %v", err)
		}
		if got := readCreds(t, p); got != `{"token":"k-held"}` {
			t.Errorf("a bundle that says nothing of when it was installed replaced a login: %q", got)
		}
		// Into an empty directory it restores, and with no record.
		empty := fileProvider(t, nil)
		if err := empty.RestoreBundle(t.Context(), bundleOf(t, p.harness, "creds.json", `{"token":"k-unrecorded"}`)); err != nil {
			t.Fatalf("RestoreBundle into an empty directory: %v", err)
		}
		if got := readCreds(t, empty); got != `{"token":"k-unrecorded"}` {
			t.Errorf("a bundle with no record did not restore into an empty directory: %q", got)
		}
		if got := installRecord(t, empty); !got.IsZero() {
			t.Errorf("a bundle with no record was given one on restore: %+v", got)
		}
	})
}

// A LOGOUT STILL WINS OVER EVERY BUNDLE, the newest included: the directory an
// operator emptied stays empty until a login is written there again.
//
// Mutation: drop the logout check from the restore, and this is red.
func TestALogoutOutranksANewerBundle(t *testing.T) {
	t.Parallel()
	p := fileProvider(t, nil)
	writeCreds(t, p, `{"token":"k-held"}`)
	seedRecord(t, p, recordAt(t, time.Now().Add(-time.Hour)))
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k-newest"}`, recordAt(t, time.Now())))
	if !errors.Is(err, ErrLoggedOut) {
		t.Errorf("RestoreBundle after a logout = %v, want ErrLoggedOut", err)
	}
	if got := readCreds(t, p); got != "" {
		t.Errorf("a newer bundle undid the logout: %q", got)
	}
}

// A BUNDLE THAT REPLACES A LOGIN REPLACES ALL OF IT, and moves the operator
// epoch as a write over a login does: a file of the login it replaced kept
// beside it would make a login of two sign-ins, and an unmoved epoch would let
// a sign-in running meanwhile read the bundle's files as a refresh of its own.
//
// Mutation: restore over the current login's files rather than its settings
// alone, or skip the epoch on a replacing restore, and this is red.
func TestABundleThatReplacesALoginReplacesItWhole(t *testing.T) {
	t.Parallel()
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "true", "complete_args": []any{"-p"}, "output": "text",
			"credential_paths": []any{".fake/auth.json", ".fake/config.toml"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetWorkspace(p.ws.StateDir()) })
	dir := p.ws.CredentialsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"auth.json": `{"token":"k-held"}`, "config.toml": `provider = "held"`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := p.ws.loginEpoch()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := packBundle(bundleManifest{Harness: "custom", Login: new(recordAt(t, time.Now()))},
		[]bundleFile{{name: "auth.json", data: []byte(`{"token":"k-bundled"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RestoreBundle(t.Context(), blob); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("the replaced login's config.toml was kept beside the bundle's token (%v)", err)
	}
	if after, err := p.ws.loginEpoch(); err != nil || after == epoch {
		t.Errorf("a restore that replaced a login left the operator epoch at %d (%v)", after, err)
	}
}

// A BUNDLE'S RECORD THAT CANNOT BE ORDERED IS REFUSED, not read as none: taken
// as recordless it would quietly lose to every login it was exported to replace.
//
// Mutation: skip the record's check, and this is red.
func TestABundleWhoseRecordCannotBeOrderedIsRefused(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]LoginInstall{
		"no instant": {ID: "0123456789abcdef0123456789abcdef"},
		"not an id":  {ID: "one", At: time.Now()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fileProvider(t, nil)
			err := p.RestoreBundle(t.Context(), installedLoginOf(t, `{"token":"k"}`, g))
			if err == nil {
				t.Fatal("a bundle with an unreadable install record was restored")
			}
			if got := readCreds(t, p); got != "" {
				t.Errorf("the refused bundle wrote %q", got)
			}
		})
	}
}

// A BUNDLE THAT CANNOT BE READ IS NO NEWER LOGIN. A node signed in to the
// entry's new harness keeps its login when the configured bundle is still the old
// harness's, and the restore says nothing, so `crewlet llm export`, which restores
// first and is the very command that replaces that bundle, is not refused over
// it; a node holding no login is told, since the refusal is why it runs signed
// out.
//
// Mutation: report a bundle that cannot be staged whatever the directory holds,
// and the first half is red.
func TestABundleThatCannotBeReadKeepsTheLoginHeld(t *testing.T) {
	t.Parallel()
	foreign := bundleOf(t, "codex", "auth.json", `{"token":"k-codex"}`)
	p := fileProvider(t, nil)
	writeCreds(t, p, `{"token":"k-held"}`)
	if err := p.RestoreBundle(t.Context(), foreign); err != nil {
		t.Errorf("RestoreBundle of another harness's bundle over a held login = %v, want nothing said", err)
	}
	if got := readCreds(t, p); got != `{"token":"k-held"}` {
		t.Errorf("the held login is %q", got)
	}
	empty := fileProvider(t, nil)
	if err := empty.RestoreBundle(t.Context(), foreign); !errors.Is(err, ErrForeignBundle) {
		t.Errorf("RestoreBundle of another harness's bundle into an empty directory = %v, want ErrForeignBundle", err)
	}
}
