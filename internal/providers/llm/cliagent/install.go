package cliagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// A LOGIN'S INSTALL RECORD names one login of an entry's credential files: which
// sign-in a refreshed token still belongs to, and which of two logins is the
// newer.
//
// Two questions need it, and the files themselves answer neither:
//
//   - WHICH SIGN-IN. A sit-out belongs to the account the CLI is signed in as
//     ([SignInID]), and for a login kept in files that account changes only
//     when a person signs in again. The files change far more often: the CLI
//     refreshes its token and the engine carries the refresh back
//     (workspace.go). Keyed by the files' bytes, every refresh would read as a
//     new account and lift a sit-out the vendor still holds.
//   - WHICH IS NEWER. A credential bundle in the secret store carries the
//     login another node signed in with. Restored only into an empty
//     directory, it never replaced the stale login a node already held, so a
//     fresh sign-in reached no node that had ever had one. Restored whenever
//     it differs, it would overwrite a login made on this node after it.
//
// So every engine write that INSTALLS a login writes a record: a login command
// carried out (`crewlet llm login`, a credential login, a token capture that
// left a login file, a device sign-in), `-from-host` and an import mint a new
// one; a bundle restore writes the BUNDLE's, so every node restoring one bundle
// holds one record for it and answers one [SignInID]. A logout removes it, and
// a refresh written back leaves it alone. (A record and not a "generation":
// that word is a seat's open checkout in this package, workspace.go.)
//
// WRITTEN IN THE SAME COMMITTED STEP AS THE LOGIN ([Workspace.installLoginLocked]),
// one journal of the shared directory's write over the login's files and this
// one. Written beside it as a second step, a write killed between the two left
// a new login under the old login's record, which named it as the sign-in it
// replaced; and a carry retried whole after its login landed would mint a
// second record for one sign-in.
//
// It lives beside the login in credentials/ and is not one of the profile's
// credential paths: a seat's seed, a box's seed and a bundle's files are each
// read by those names alone, so no CLI is ever handed it. A bundle carries it
// in its manifest instead ([bundleManifest]).

// loginInstallFile is the install record's file in the credentials directory.
const loginInstallFile = cliprofile.LoginInstallFile

// LoginInstall is one login's install record: a random id and when the login
// was installed. The zero value is "no install record": a directory with no login,
// or one written before install records existed, which counts as older than any
// install record ([LoginInstall.Newer]).
type LoginInstall struct {
	// ID is 128 random bits in hex.
	ID string `json:"id"`
	// At is when the login was installed, in UTC.
	At time.Time `json:"at"`
}

// IsZero reports whether g names no install record.
func (g LoginInstall) IsZero() bool { return g.ID == "" }

// Newer reports whether g should replace held: a different login installed
// later. Equal ids are one login however their instants read, so a bundle
// restored twice is restored once.
func (g LoginInstall) Newer(held LoginInstall) bool {
	return !g.IsZero() && g.ID != held.ID && g.At.After(held.At)
}

// newLoginInstall mints the install record of a login installed at now.
func newLoginInstall(now time.Time) (LoginInstall, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return LoginInstall{}, fmt.Errorf("cli-agent: minting a login install record: %w", err)
	}
	return LoginInstall{ID: hex.EncodeToString(id[:]), At: now.UTC()}, nil
}

// check refuses an install record no install wrote: an id that is not 128 bits
// in hex, or no instant. Asked of a bundle's, the one record read from outside
// this host: a record that cannot be ordered against another cannot say which
// login is the newer. The one on disk was written here, so it is read as it
// stands.
func (g LoginInstall) check() error {
	if len(g.ID) != 32 || strings.Trim(g.ID, "0123456789abcdef") != "" {
		return fmt.Errorf("its id %q is not 128 bits in lowercase hex", g.ID)
	}
	if g.At.IsZero() {
		return errors.New("it names no instant")
	}
	return nil
}

// LoginInstall is the install record of the login the credentials directory
// holds, zero when it holds none or one installed before install records existed.
//
// Read without the shared login's lock: the file is replaced whole, so a reader
// sees one install record or the other. A write committed and not yet put in
// place (a disk that refused a file) is read as the record before it until the
// next reader of the login finishes it; a caller that has to agree with the
// login it read takes the lock and reads both in one hold
// ([Workspace.loginAndInstallLocked]).
func (w *Workspace) LoginInstall() (LoginInstall, error) {
	path := filepath.Join(w.CredentialsDir(), loginInstallFile)
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if errors.Is(err, fs.ErrNotExist) {
		return LoginInstall{}, nil
	}
	if err != nil {
		return LoginInstall{}, fmt.Errorf("cli-agent: reading the login's install record %q: %w", path, err)
	}
	var g LoginInstall
	err = json.Unmarshal(raw, &g)
	if err == nil && g.IsZero() {
		err = errors.New("it names no login")
	}
	if err != nil {
		return LoginInstall{}, fmt.Errorf("cli-agent: %q does not hold a login install record "+
			"this build can read; remove it, and the login counts as older than any other: %w", path, err)
	}
	return g, nil
}

// loginAndInstallLocked reads the shared login and its install record in one
// hold of the shared login's lock the caller holds, so the record is the one
// committed with the login read ([Workspace.readLoginLocked] finishes a write
// left standing before either is read).
func (w *Workspace) loginAndInstallLocked() (map[string][]byte, LoginInstall, error) {
	login, err := w.readLoginLocked()
	if err != nil {
		return nil, LoginInstall{}, err
	}
	g, err := w.LoginInstall()
	if err != nil {
		return nil, LoginInstall{}, err
	}
	return login, g, nil
}

// installLoginLocked makes the shared login EXACTLY login, as
// [Workspace.writeLoginLocked] does, and its install record g, in the SAME
// committed step: the record is one more name of the journal's write. A zero g
// removes the record with the login it named, which is a logout's write and a
// restore of a bundle that carries none.
//
// Under the shared login's lock the caller holds.
func (w *Workspace) installLoginLocked(ctx context.Context, who string, login map[string][]byte, g LoginInstall) error {
	files := maps.Clone(login)
	if files == nil {
		files = map[string][]byte{}
	}
	if !g.IsZero() {
		raw, err := json.Marshal(g)
		if err != nil {
			return fmt.Errorf("cli-agent: encoding the login's install record: %w", err)
		}
		files[loginInstallFile] = append(raw, '\n')
	}
	return w.commitLoginLocked(ctx, who, append(w.loginNames(), loginInstallFile), files)
}
