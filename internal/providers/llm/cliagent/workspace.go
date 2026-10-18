package cliagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// Workspace owns one state directory: the shared login, and a home per seat.
//
// One per RESOLVED state directory, from a package-level registry, because
// entries that point at the same directory are meant to share it — that is
// how per-phase models work off a single `crewlet llm login`. Two Workspace
// values over one directory would each believe they owned the pruning, and
// the opus entry's prune would delete the sonnet entry's live session.
type Workspace struct {
	root    string
	agent   string
	profile cliprofile.Profile

	mu    sync.Mutex
	seats map[string]*seatState

	// sharedMu is the in-process half of the shared login's lock, which
	// every write to the shared directory holds ([Workspace.lockShared]).
	// The other half is an flock, for the `crewlet llm` commands that write
	// the same directory from processes of their own; the mutex is what
	// queues this process's seats behind each other rather than having each
	// poll the file.
	sharedMu sync.Mutex
}

// seatState is one seat's share of a workspace.
type seatState struct {
	// mu guards inflight AND the prune, so a second call entering the seat
	// waits for the first call's seeding rather than racing it into a
	// half-populated home.
	mu       sync.Mutex
	inflight int

	// gen is the open generation — what the seat's home was seeded from —
	// set by the call that seeds the home and read by the last call out,
	// which carries a refreshed login back against it ([generation]).
	gen generation
}

// registry is the process-wide table of workspaces by resolved state dir.
var registry = struct {
	mu sync.Mutex
	m  map[string]*Workspace
}{m: make(map[string]*Workspace)}

// Shared returns the workspace for a state directory, creating it once.
//
// It refuses two DIFFERENT CLIs over one directory rather than serving both:
// two CLIs disagree about which files are credentials and which are
// conversation memory, so each would prune the other's login. Config
// validation catches this at `crewlet validate`; this is the backstop for a
// directory two processes reached by different paths.
func Shared(stateDir, agent string, profile cliprofile.Profile) (*Workspace, error) {
	resolved, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("cli-agent: resolving state_dir %q: %w", stateDir, err)
	}
	resolved = filepath.Clean(resolved)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing, ok := registry.m[resolved]; ok {
		if existing.agent != agent {
			return nil, fmt.Errorf(
				"cli-agent: state_dir %q is already driving %q — two CLIs cannot share "+
					"one directory, because each would prune the other's login; give this "+
					"entry its own cli.state_dir", resolved, existing.agent)
		}
		return existing, nil
	}
	ws := &Workspace{
		root:    resolved,
		agent:   agent,
		profile: profile,
		seats:   make(map[string]*seatState),
	}
	registry.m[resolved] = ws
	return ws, nil
}

// forgetWorkspace drops a state dir from the registry. Tests only: a process
// that reopened one directory as a different CLI is the bug Shared refuses,
// so nothing in the engine has a reason to call it.
func forgetWorkspace(stateDir string) {
	resolved, err := filepath.Abs(stateDir)
	if err != nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.m, filepath.Clean(resolved))
}

// Root is the workspace's state directory.
func (w *Workspace) Root() string { return w.root }

// CredentialsDir is where the shared login lives — ONE per workspace.
func (w *Workspace) CredentialsDir() string { return filepath.Join(w.root, "credentials") }

// SeatDir is one seat's share of the workspace.
func (w *Workspace) SeatDir(seat string) string {
	return filepath.Join(w.root, "seats", seatSlug(seat))
}

// loginHome is the home every `crewlet llm` auth command runs the CLI in,
// rebuilt from the shared login for each one — see
// [Provider.runInCredentialHome].
func (w *Workspace) loginHome() string { return filepath.Join(w.root, "login-home") }

// loginCache is the login home's XDG_CACHE_HOME, outside the pruned home for
// the reason a seat's cache is.
func (w *Workspace) loginCache() string { return filepath.Join(w.root, "login-cache") }

// The files a workspace keeps BESIDE its homes rather than in one, so that
// neither a bundle (which carries the credentials directory) nor a CLI (whose
// HOME is a home) ever reads one as its own.
const (
	// generationFile is a seat's open [generation], in its seat directory.
	generationFile = "generation.json"
	// loginGenerationFile is the login home's open [generation], in the
	// state directory. On disk rather than in memory for the reason the
	// login home has a lock: every `crewlet llm` command is a process of its
	// own, so what one left open is the NEXT one's to finish.
	loginGenerationFile = "login-generation.json"
	// sharedLockFile is the shared login's lock — see
	// [Workspace.lockShared].
	sharedLockFile = "credentials.lock"
	// loginHomeLockFile is the login home's lock — see
	// [Workspace.lockLoginHome].
	loginHomeLockFile = "login-home.lock"
)

// slugReadable is how much of a handle a directory name shows before the
// disambiguating digest. Long enough to recognise a seat at a glance, short
// enough that name plus digest stays far inside NAME_MAX.
const slugReadable = 48

// seatSlug makes a seat handle safe as one path element, INJECTIVELY.
//
// Not a hash outright: an operator looking at a state directory to see whose
// home is filling a disk needs to recognise the seat. But not a lossy
// rendering either — this directory is a seat's HOME, holding its coding-CLI
// session, its history and its credentials, and two seats that land on one
// slug get one home. That is the first thing this package's doc says must
// never happen: seven seats on one subscription reading each other's
// transcripts.
//
// The old rendering could collide two ways. Characters outside the safe set
// all folded to '-', and the result was cut at 64 — so any two handles
// sharing a 64-character prefix shared a home. Nothing bounded a handle's
// length, so that was reachable from a config a founder could write.
//
// So: the readable form when it represents the handle EXACTLY, and otherwise
// the readable prefix plus a digest of the whole handle. Distinct handles
// cannot meet, whatever a vendor's API or a founder's YAML supplies.
func seatSlug(seat string) string {
	seat = strings.TrimSpace(seat)
	var b strings.Builder
	for _, r := range strings.ToLower(seat) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	// FAITHFUL means the slug IS the handle, compared against the raw input
	// rather than a lowered copy: case-folding is a collision too, and
	// "sarah-chen" and "SARAH-CHEN" would otherwise share a home. Only an
	// exact match — nothing folded, nothing trimmed, nothing cut — lets two
	// inputs be told apart by the slug alone. Every handle org.ValidHandle
	// admits is already in this form, so the common case renders bare.
	if slug == seat && slug != "" && len(slug) <= slugReadable {
		return slug
	}
	sum := sha256.Sum256([]byte(seat))
	if len(slug) > slugReadable {
		slug = slug[:slugReadable]
	}
	if slug == "" {
		slug = "seat"
	}
	return slug + "-" + hex.EncodeToString(sum[:])[:16]
}

// loginStore moves a [cliprofile.CredentialStore]'s document into and out of a
// home through the CLI's own commands.
//
// The PROVIDER implements it, because running the CLI takes what only the
// provider holds — the environment its calls are given — while the workspace
// decides WHEN a home is seeded and synced. One interface for the two places
// that need it, a seat generation and the login home, so neither runs the CLI
// its own way.
type loginStore interface {
	// importLogin stores the document's logins in home's database.
	importLogin(ctx context.Context, home, cache string, document []byte) error
	// exportLogin prints every login home's database holds.
	exportLogin(ctx context.Context, home, cache string) ([]byte, error)
	// providerKey names the provider in the log lines a sync writes, since
	// several providers can share one workspace and only the store knows
	// which of them is releasing a home.
	providerKey() string
}

// Checkout is one call's place to run, and the seat share it borrows.
type Checkout struct {
	// Home is HOME for the child, and where every XDG and vendor
	// relocation variable points.
	Home string
	// Cache is XDG_CACHE_HOME — warm across calls, and holds no
	// conversation, so it is deliberately outside the pruned home.
	Cache string
	// Work is the empty per-call working directory, removed on release.
	Work string

	ws   *Workspace
	seat *seatState
	// record is where the seat's open generation is kept on disk.
	record string
	// store is the generation's way back out for a login held in the CLI's
	// own database: whichever provider seeded the home exports from it.
	store    loginStore
	released bool
}

// Acquire prepares a place for one call to run.
//
// The FIRST concurrent call into a seat prunes and seeds; a second call
// arriving while the first is still running shares the same home untouched.
// Batched sub-agents belong to the same agent, so sharing memory between an
// agent and its own sub-agents is harmless by definition — and pruning under
// them would delete the parent's state mid-call.
//
// BEFORE IT PRUNES, the first call finishes a generation the seat's last one
// left open ([Workspace.carryOut]): a release whose export failed, or a
// process that ended mid-call, leaves the home holding a login its CLI may
// have refreshed — and with most vendors rotating the refresh token, that
// home is the only place the working one exists. A seat whose old login
// cannot be carried out does not run on it, and does not discard it: the call
// is refused, and the next call tries again.
//
// store imports a login the CLI keeps in its own database ([loginStore]); it is
// asked only by a profile that declares a [cliprofile.CredentialStore], and the
// generation's [Checkout.Release] exports through the same one.
func (w *Workspace) Acquire(ctx context.Context, seat, callID string, store loginStore) (_ *Checkout, err error) {
	state := w.seatState(seat)
	dir := w.SeatDir(seat)
	home := filepath.Join(dir, "home")
	cache := filepath.Join(dir, "cache")
	work := filepath.Join(dir, "work", seatSlug(callID))
	record := filepath.Join(dir, generationFile)

	state.mu.Lock()
	defer state.mu.Unlock()

	// ONE CLEANUP FOR EVERY WAY OUT. Until this returns a Checkout the call
	// has nothing to Release, so a scratch directory an error path left
	// behind is one nothing ever removes — one per failing call, for as long
	// as the failure lasts, which is the unbounded growth Release exists to
	// prevent. Whatever a failed seed got as far as goes with the next prune.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(work)
		}
	}()

	// The directories exist BEFORE the seed rather than after it, which is
	// the order a seed that RUNS THE CLI needs: an import is a child whose
	// HOME is this directory and whose working directory is too.
	for _, d := range []string{home, cache, work} {
		if err = os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("cli-agent: preparing %q: %w", d, err)
		}
	}
	if state.inflight == 0 {
		if err = w.carryOut(ctx, home, cache, store, record); err != nil {
			return nil, err
		}
		if err = w.prune(home); err != nil {
			return nil, err
		}
		var seeded map[string]string
		if seeded, err = w.seed(ctx, home, cache, store); err != nil {
			return nil, err
		}
		// RECORDED ONLY ONCE THE SEED HAS LANDED: a generation is open from
		// here, and a record left by a seed that failed half way would have
		// the next call carry a half-imported database out over the shared
		// login.
		gen := generation{Mode: syncRefresh, Seeded: seeded}
		if err = writeGeneration(record, gen); err != nil {
			return nil, err
		}
		state.gen = gen
	}
	// The working directory is this call's own, so its settings files
	// are written per call rather than per seat generation.
	if err = w.seedFiles(work, cliprofile.SeedWork); err != nil {
		return nil, err
	}
	state.inflight++
	return &Checkout{
		Home: home, Cache: cache, Work: work,
		ws: w, seat: state, record: record, store: store,
	}, nil
}

// Release returns the seat share, and is safe to call twice.
//
// The LAST call out of a seat carries a refreshed login back to the shared
// directory and prunes. Carrying it back matters more than it looks: OAuth
// access tokens expire in hours and most vendors rotate the refresh token with
// them, so discarding the login the CLI rewrote logs the whole fleet out at the
// next expiry.
//
// A FAILED CARRY PRUNES NOTHING. The home then holds the only copy of what the
// call refreshed, so it is kept as it is, with the generation's record beside
// it, for the seat's next call to carry out before it prunes — see
// [Workspace.Acquire].
//
// ctx bounds the export a login held in the CLI's own database leaves through.
// A caller hands it [context.WithoutCancel] of its own: a release is a teardown,
// and a call that ended BECAUSE its context was cancelled still has a refreshed
// token to write back.
func (c *Checkout) Release(ctx context.Context) error {
	if c == nil || c.released {
		return nil
	}
	c.released = true

	// The scratch directory goes first and unconditionally: it is this
	// call's alone, and leaving it behind on an error path is how a state
	// directory grows without bound.
	workErr := os.RemoveAll(c.Work)

	c.seat.mu.Lock()
	defer c.seat.mu.Unlock()
	c.seat.inflight--
	if c.seat.inflight > 0 {
		return workErr
	}
	if _, err := c.ws.syncCredentialsOut(ctx, c.Home, c.Cache, c.store, c.seat.gen); err != nil {
		return errors.Join(workErr, fmt.Errorf("cli-agent: the login in %q could not be "+
			"carried back to the shared directory, so the home was kept as it is for the "+
			"seat's next call to carry out: %w", c.Home, err))
	}
	pruneErr := c.ws.prune(c.Home)
	return errors.Join(workErr, pruneErr, clearGeneration(c.record))
}

// seatState returns a seat's share, creating it once.
func (w *Workspace) seatState(seat string) *seatState {
	w.mu.Lock()
	defer w.mu.Unlock()
	state, ok := w.seats[seatSlug(seat)]
	if !ok {
		state = &seatState{}
		w.seats[seatSlug(seat)] = state
	}
	return state
}

// prune deletes the profile's volatile paths from a seat home.
func (w *Workspace) prune(home string) error {
	for _, rel := range w.profile.VolatilePaths {
		target, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("cli-agent: pruning %q: %w", target, err)
		}
	}
	return nil
}

// dropCopies deletes a home's copy of every credential file.
//
// For the LOGIN HOME, once its login has been carried out: the shared
// directory holds the login of record, and a second copy left in the login
// home is one a later command would find there and carry back — which is how
// a login `crewlet llm logout` removed came back on the next `status`.
func (w *Workspace) dropCopies(home string) error {
	for _, rel := range w.profile.CredentialPaths {
		target, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cli-agent: removing %q: %w", target, err)
		}
	}
	return nil
}

// seed copies the shared login into a home, and reports what it seeded the
// home from — the [generation]'s baseline: the SHA-256 of each shared
// credential file copied in and of the document imported, by name, a name
// left out being a file that was absent.
//
// A COPY rather than a symlink into the shared directory: the CLI rewrites
// this file in place when it refreshes, and seven seats sharing one inode
// would have seven writers on it. The copy back out is [syncCredentialsOut],
// which runs once, when the seat's last call finishes.
//
// Each file is read ONCE, and the digest is of the very bytes written into
// the home: a digest read separately could describe a newer file than the one
// the CLI started from, and the baseline would then excuse a write it should
// have refused.
//
// A login held in the CLI's own database is IMPORTED last, once the settings
// files are in place, because the import is the CLI itself starting in this
// home and reading them: see [Workspace.importLogin].
func (w *Workspace) seed(ctx context.Context, home, cache string, store loginStore) (map[string]string, error) {
	shared := w.CredentialsDir()
	seeded := map[string]string{}
	for _, rel := range w.profile.CredentialPaths {
		name := filepath.Base(rel)
		dst, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return nil, err
		}
		content, present, err := readShared(filepath.Join(shared, name))
		if err != nil {
			return nil, err
		}
		if !present {
			// NO LOGIN YET is not an error here. The call then fails with
			// the CLI's own "not authenticated", which names the vendor
			// and is what `crewlet llm doctor` explains — far better than
			// a provider that refused to start.
			//
			// AND THE HOME KEEPS NONE EITHER. A credential file is not
			// volatile, so the copy an earlier generation left is still in
			// the home — and skipping the seed used to leave the CLI signed
			// in with it, so a seat went on calling as a login `crewlet llm
			// logout` had removed.
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("cli-agent: removing the stale login %q: %w", dst, err)
			}
			continue
		}
		if err := writePrivate(dst, content); err != nil {
			return nil, err
		}
		seeded[name] = digestOf(content)
	}
	if err := w.seedFiles(home, cliprofile.SeedHome); err != nil {
		return nil, err
	}
	if err := w.importLogin(ctx, home, cache, store, seeded); err != nil {
		return nil, err
	}
	return seeded, nil
}

// importLogin puts a [cliprofile.CredentialStore]'s document into a home's
// database, for a profile that declares one, and records the document it read
// in seeded.
//
// INTO A DATABASE THE PRUNE HAS JUST EMPTIED, which is not incidental: the CLI
// skips a credential id its database already holds rather than updating it, so
// importing a refreshed document over an older database would keep the older
// token. Both callers prune first.
func (w *Workspace) importLogin(
	ctx context.Context, home, cache string, store loginStore, seeded map[string]string,
) error {
	if w.profile.CredentialStore == nil {
		return nil
	}
	path := w.documentPath()
	document, present, err := readShared(path)
	if err != nil {
		return err
	}
	if !present {
		// No login yet, exactly as for a credential file above.
		return nil
	}
	seeded[cliprofile.CredentialStoreDocument] = digestOf(document)
	if !holdsLogin(document) {
		return nil
	}
	if store == nil {
		// Every caller in this package hands one; a nil here is a caller
		// that would otherwise run the seat with no login while the shared
		// directory plainly holds one.
		return fmt.Errorf("cli-agent: %q keeps its login in its own database, and this "+
			"home was prepared with no way to import %q into it", w.agent, path)
	}
	if err := store.importLogin(ctx, home, cache, document); err != nil {
		return fmt.Errorf("cli-agent: importing the shared login %q: %w — if the "+
			"document is damaged, sign in again with `crewlet llm login`", path, err)
	}
	return nil
}

// seedFiles writes the profile's settings files for one scope into root.
//
// Written on EVERY seed rather than once: the CLI may rewrite its own
// settings file during a call, and a vendor's tool policy that quietly
// changed under a seat is the hole these files exist to close. Verbatim at
// 0600, parents created, rooted the same way prune's targets are.
func (w *Workspace) seedFiles(root string, scope cliprofile.SeedScope) error {
	for _, f := range w.profile.SeedFiles {
		if f.Scope() != scope {
			continue
		}
		dst, err := cliprofile.UnderRoot(root, f.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o600); err != nil {
			return fmt.Errorf("cli-agent: writing the seeded settings file %q: %w", dst, err)
		}
	}
	return nil
}

// syncMode is how a home's changed login is carried back to the shared
// directory — which depends on whether the command that ran there was meant to
// MAKE a login or only to use one.
type syncMode string

const (
	// syncRefresh is a seat's generation, and every auth command that makes
	// no login (`status`, a vendor's own logout). Its CLI can only have
	// REFRESHED the login it was seeded from, so the write is a
	// COMPARE-AND-SWAP against that login: made only while the shared
	// directory still holds exactly what the home was seeded from, and never
	// where the shared directory held nothing. Anything else is somebody
	// else's newer write — another seat's refresh, an operator's new login,
	// a logout — and the newer one is kept.
	syncRefresh syncMode = "refresh"
	// syncLogin is a command that MAKES a login: `crewlet llm login`, a
	// credential login, a token capture. What its CLI stored is written
	// whenever it differs from what the command started with, OVER whatever
	// the shared directory holds by then — an operator's new login has to
	// win over a refresh a seat wrote during a browser flow that took
	// minutes — and it is the one mode that may create the shared login.
	syncLogin syncMode = "login"
)

// Valid reports whether m is one of the two modes.
func (m syncMode) Valid() bool { return m == syncRefresh || m == syncLogin }

// generation is one home's OPEN GENERATION: how a login its CLI changed is
// carried out, and what the home's login was seeded from, which is what a
// changed one is told from an unchanged one by — and, for a refresh, what the
// shared directory must still hold for the write to be made.
//
// It is written beside the home when the seed lands and removed once the login
// has been carried out, so a record still there when the next generation
// begins is a generation whose login was never carried out — a failed
// release, or a process that ended mid-call — and is carried out first.
type generation struct {
	// Mode is how the login is carried out.
	Mode syncMode `json:"mode"`
	// Seeded is the SHA-256, hex, of each shared login file the home was
	// seeded from, by its name in the shared directory: every credential
	// file copied in and the credential store's document. A name absent
	// here was absent then.
	Seeded map[string]string `json:"seeded"`
}

// readGeneration reads an open generation's record, and reports false where
// there is none.
func readGeneration(path string) (generation, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if os.IsNotExist(err) {
		return generation{}, false, nil
	}
	if err != nil {
		return generation{}, false, fmt.Errorf("cli-agent: reading %q: %w", path, err)
	}
	var gen generation
	if err := json.Unmarshal(raw, &gen); err != nil || !gen.Mode.Valid() {
		// REFUSED, not skipped: skipping would prune a home that may hold
		// the only copy of a refreshed login, which is exactly what the
		// record exists to prevent. Removing it is the operator's call,
		// and the message says so.
		return generation{}, false, fmt.Errorf("cli-agent: %q does not record a generation "+
			"this build can read, so the home beside it may hold a login nothing has "+
			"carried out — remove the file to discard that login and let the next call "+
			"start from the shared one", path)
	}
	return gen, true, nil
}

// writeGeneration records an open generation, replacing the file whole so a
// crash mid-write leaves the previous record or this one and never half.
func writeGeneration(path string, gen generation) error {
	raw, err := json.Marshal(gen)
	if err != nil {
		return fmt.Errorf("cli-agent: encoding %q: %w", path, err)
	}
	if err := hostbox.ReplaceFile(path, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("cli-agent: recording the open generation in %q: %w", path, err)
	}
	return nil
}

// clearGeneration removes a generation's record once its login is carried out.
func clearGeneration(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cli-agent: closing the generation recorded in %q: %w", path, err)
	}
	return nil
}

// carryOut finishes a generation an earlier call or command left open, before
// the prune that would discard what its home holds.
//
// Under the record's own mode and baseline, exactly as the release it stands in
// for would have — so carrying out a home whose CLI changed nothing writes
// nothing, a login the shared directory has since moved past is not written
// over it, and a home whose export holds no login is carried out by doing
// nothing. Only then is the record removed and the home free to prune; a carry
// that fails again leaves both for the next attempt.
func (w *Workspace) carryOut(ctx context.Context, home, cache string, store loginStore, record string) error {
	gen, open, err := readGeneration(record)
	if err != nil || !open {
		return err
	}
	if _, err := w.syncCredentialsOut(ctx, home, cache, store, gen); err != nil {
		return fmt.Errorf("cli-agent: the login an earlier generation left in %q has not been "+
			"carried back to the shared directory, and it may be the only copy of a token "+
			"the CLI refreshed, so it was kept and the CLI was not started on top of it: %w",
			home, err)
	}
	return clearGeneration(record)
}

// syncCredentialsOut carries a login a home's CLI CHANGED back to the shared
// directory, and reports whether the home holds a login at all.
//
// CHANGED IS JUDGED AGAINST THE GENERATION'S BASELINE, never against the shared
// directory as it is now. Compared with the shared file, an idle home — whose
// export reproduces its import byte for byte — differs from it the moment
// anybody else has written a newer login, and writing it back REVERTED that
// write: another seat's refresh, carrying a refresh token the vendor had
// already rotated away, or an operator's new login, which the next expiry then
// logged the whole fleet out of. So a home that holds what it was seeded with
// writes nothing at all, whatever the shared directory holds.
//
// What did change is written by [Workspace.carry], under the shared login's
// lock, by the generation's [syncMode]: a refresh only over the login it was
// seeded from, a login over anything. A login that lost to a newer one is
// logged as `cli_agent_login_superseded` and dropped.
//
// Neither kind of home deletes anything from the shared directory: a credential
// file a call removed, or a database exporting no login — what a CLI that
// dropped a credential it could not refresh leaves — is not a login worth
// keeping over the shared one.
//
// Everything is READ before anything is written, the export included, so an
// export that fails writes none of the credential files beside it: a carry is
// retried whole ([Workspace.carryOut]), and a half-written one would be read
// by the retry as somebody else's newer write.
//
// Every write REPLACES the file rather than rewriting it in place, through a
// temporary file and a rename: the shared file is what every other seat's next
// generation is seeded from, and a seed reading it mid-write took a
// half-written credential — or an empty one — into a seat.
func (w *Workspace) syncCredentialsOut(
	ctx context.Context, home, cache string, store loginStore, gen generation,
) (bool, error) {
	type held struct {
		name, dst string
		content   []byte
	}
	var changed []held
	holds := false
	shared := w.CredentialsDir()
	for _, rel := range w.profile.CredentialPaths {
		src, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return false, err
		}
		content, err := os.ReadFile(src) //nolint:gosec // path is rooted by cliprofile.UnderRoot
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("cli-agent: reading the refreshed login %q: %w", src, err)
		}
		holds = true
		name := filepath.Base(rel)
		if digestOf(content) != gen.Seeded[name] {
			changed = append(changed, held{name: name, dst: filepath.Join(shared, name), content: content})
		}
	}
	if w.profile.CredentialStore != nil {
		if store == nil {
			return false, fmt.Errorf("cli-agent: %q keeps its login in its own database, and this "+
				"home was released with no way to export it", w.agent)
		}
		document, err := store.exportLogin(ctx, home, cache)
		if err != nil {
			return false, fmt.Errorf("cli-agent: exporting the login from %q: %w", home, err)
		}
		if holdsLogin(document) {
			holds = true
			if digestOf(document) != gen.Seeded[cliprofile.CredentialStoreDocument] {
				changed = append(changed, held{
					name: cliprofile.CredentialStoreDocument, dst: w.documentPath(), content: document,
				})
			}
		}
	}
	if len(changed) == 0 {
		return holds, nil
	}
	who := ""
	if store != nil {
		who = store.providerKey()
	}
	unlock, err := w.lockShared(ctx)
	if err != nil {
		return holds, err
	}
	defer unlock()
	for _, h := range changed {
		if err := w.carry(ctx, who, gen, h.name, h.dst, h.content); err != nil {
			return holds, err
		}
	}
	return holds, nil
}

// carry is one changed login file's write, made under the shared login's lock
// so that the comparison and the replacement are one step against every other
// writer — a seat in this process, or a `crewlet llm` command in another.
func (w *Workspace) carry(ctx context.Context, who string, gen generation, name, dst string, content []byte) error {
	current, present, err := readShared(dst)
	if err != nil {
		return err
	}
	if present && bytes.Equal(current, content) {
		// Already there: two homes that refreshed to the same login, or a
		// carry repeated after a write that landed before it failed.
		return nil
	}
	switch gen.Mode {
	case syncLogin:
	case syncRefresh:
		base, seededFrom := gen.Seeded[name]
		switch {
		case !seededFrom:
			// A REFRESH NEVER CREATES A LOGIN. Nothing in the shared
			// directory when the home was seeded means nobody had logged
			// this entry in — or somebody had logged it out — and a login
			// a seat's call happened to leave behind is not the
			// operator's to inherit. If somebody has signed in since,
			// theirs is the login, and this one is no refresh of it.
			return nil
		case !present:
			w.superseded(ctx, who, dst, "removed")
			return nil
		case digestOf(current) != base:
			w.superseded(ctx, who, dst, "changed")
			return nil
		}
	default:
		return fmt.Errorf("cli-agent: carrying %q out under an unknown mode %q", dst, gen.Mode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(dst), err)
	}
	if err := hostbox.ReplaceFile(dst, bytes.NewReader(content)); err != nil {
		return fmt.Errorf("cli-agent: writing the refreshed login %q: %w", dst, err)
	}
	return nil
}

// superseded logs a refreshed login dropped because the shared one moved past
// it: WARN rather than debug, because it is the one trace that a refresh — or
// a token a seat's call minted — went nowhere, and two of them in a row on
// one entry are worth somebody looking at. It names the file and never what
// either copy holds.
func (w *Workspace) superseded(ctx context.Context, who, dst, how string) {
	log.WarnContext(ctx, "cli_agent_login_superseded", "provider", who, "agent", w.agent,
		"path", dst, "shared", how)
}

// sharedLockWait bounds how long a writer of the shared login waits for
// another to finish with it.
//
// What the lock guards is a read and a rename of a file under a mebibyte
// ([maxBundle] bounds a whole login), a few milliseconds even on a loaded
// disk, so ten seconds is three orders of magnitude of headroom: only a holder
// that has STOPPED RUNNING — a `crewlet llm` command suspended at a terminal
// mid-write — reaches it, and then the honest answer is an error naming the
// lock rather than a seat's release that never returns. A holder that crashed
// holds nothing: the lock is the open descriptor's, and the kernel drops it
// with the process.
const sharedLockWait = 10 * time.Second

// lockPoll is how often a writer waiting on another's lock asks again. A lock
// held for milliseconds is free within one or two polls, and a waiter
// sleeping between them costs nothing.
const lockPoll = 10 * time.Millisecond

// lockShared takes the shared login's lock for one compare-and-swap, and hands
// back its release.
//
// TWO HALVES because there are two kinds of writer: this process's own seats,
// queued on [Workspace.sharedMu], and the `crewlet llm` commands, each a
// process of its own, which only an flock on a file both can open reaches.
// Every write to the shared directory takes it — a seat's carry, a login's,
// `-from-host`, a restored bundle and a logout's removal — because a
// compare-and-swap is only one against writers that agree to the same lock.
func (w *Workspace) lockShared(ctx context.Context) (func(), error) {
	w.sharedMu.Lock()
	path := filepath.Join(w.root, sharedLockFile)
	release, held, err := flockFile(ctx, path, sharedLockWait)
	if err == nil && !held {
		err = &lockWaitError{path: path, wait: sharedLockWait}
	}
	if err != nil {
		w.sharedMu.Unlock()
		return nil, err
	}
	return func() {
		release()
		w.sharedMu.Unlock()
	}, nil
}

// lockLoginHome takes the login home's lock for the whole of one auth command,
// or reports that another command holds it.
//
// Held from the leading prune to the trailing one, because everything between
// them is the login home: a second command pruning it ran `RemoveAll` under a
// login still waiting on its browser flow, which then stored its credential in
// a database nothing could find again and was told it had written a login.
//
// NOT WAITED ON. The holder may be a person part way through a sign-in, for
// minutes, and a `crewlet llm status` that sat silent behind it would read as
// a wedged command; saying so at once is the answer an operator can act on.
func (w *Workspace) lockLoginHome(ctx context.Context) (func(), error) {
	path := filepath.Join(w.root, loginHomeLockFile)
	release, held, err := flockFile(ctx, path, 0)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, &loginHomeBusyError{agent: w.agent, root: w.root, lock: path}
	}
	return release, nil
}

// loginHomeBusyError is the login home's lock held by another command.
type loginHomeBusyError struct{ agent, root, lock string }

func (e *loginHomeBusyError) Error() string {
	return fmt.Sprintf("cli-agent: another `crewlet llm` command is using the %q login home "+
		"under %q (its lock %s is held) — wait for it to finish, then run this again",
		e.agent, e.root, e.lock)
}

// lockWaitError is a lock another writer held for longer than the wait: a
// TIMEOUT, so it unwraps to [context.DeadlineExceeded].
type lockWaitError struct {
	path string
	wait time.Duration
}

func (e *lockWaitError) Error() string {
	return fmt.Sprintf("cli-agent: the shared login's lock %s was held by another writer "+
		"for longer than %s — a `crewlet llm` command stopped part way through (a "+
		"suspended terminal) holds it until it exits", e.path, e.wait)
}

func (e *lockWaitError) Unwrap() error { return context.DeadlineExceeded }

// flockFile takes an exclusive flock on path, creating it, waiting up to wait
// for another holder to let go — not at all for zero — and reports false
// where it is still held at the end.
//
// flock rather than fcntl for the reason internal/store gives: an fcntl lock
// is dropped when ANY descriptor for the file is closed in the process. And a
// fresh descriptor per call, because flock locks belong to the open file
// description — two opens in one process exclude each other exactly as two
// processes do, which is what the login home's lock relies on.
func flockFile(ctx context.Context, path string, wait time.Duration) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path is inside the state dir
	if err != nil {
		return nil, false, fmt.Errorf("cli-agent: opening the lock %q: %w", path, err)
	}
	fd := int(f.Fd())
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, false, fmt.Errorf("cli-agent: locking %q: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, false, fmt.Errorf("cli-agent: waiting for the lock %q: %w", path, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// readShared reads one shared login file, reporting false where there is none.
func readShared(path string) ([]byte, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cli-agent: reading the shared login %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("cli-agent: shared login %q is not a regular file", path)
	}
	content, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if os.IsNotExist(err) {
		// Removed between the two reads — a logout landing — which is
		// exactly what it would have read a moment later.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cli-agent: reading the shared login %q: %w", path, err)
	}
	return content, true, nil
}

// digestOf is the SHA-256 of content in hex, [hostbox.FileDigest]'s form.
func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// writePrivate writes content at dst, 0600, creating its parent.
func writePrivate(dst string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return fmt.Errorf("cli-agent: writing %q: %w", dst, err)
	}
	return nil
}

// documentPath is where the shared directory keeps a
// [cliprofile.CredentialStore]'s document.
func (w *Workspace) documentPath() string {
	return filepath.Join(w.CredentialsDir(), cliprofile.CredentialStoreDocument)
}

// holdsLogin reports whether an exported document carries any login at all.
//
// The document's format is the CLI's, so this reads only the one shape every
// such export shares when it is empty — nothing, or an empty JSON array,
// object or null — and takes anything else as a login. Guessing harder would
// mean parsing a vendor's credential format here, and a wrong guess in that
// direction discards somebody's login.
func holdsLogin(document []byte) bool {
	trimmed := bytes.TrimSpace(document)
	if len(trimmed) == 0 {
		return false
	}
	var empty any
	if err := json.Unmarshal(trimmed, &empty); err != nil {
		return true
	}
	switch v := empty.(type) {
	case nil:
		return false
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// HasLogin reports whether the shared directory holds any credential file.
func (w *Workspace) HasLogin() bool {
	return len(w.LoginFiles()) > 0
}

// LoginFiles lists the credential files present in the shared directory,
// as absolute paths, sorted — a [cliprofile.CredentialStore]'s document among
// them, which travels in a bundle and is removed by a logout exactly as a
// credential file is.
func (w *Workspace) LoginFiles() []string {
	shared := w.CredentialsDir()
	var candidates []string
	for _, rel := range w.profile.CredentialPaths {
		candidates = append(candidates, filepath.Join(shared, filepath.Base(rel)))
	}
	if w.profile.CredentialStore != nil {
		candidates = append(candidates, w.documentPath())
	}
	var out []string
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}
