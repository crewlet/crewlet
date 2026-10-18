package cliagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

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
}

// seatState is one seat's share of a workspace.
type seatState struct {
	// mu guards inflight AND the prune, so a second call entering the seat
	// waits for the first call's seeding rather than racing it into a
	// half-populated home.
	mu       sync.Mutex
	inflight int
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
// store imports a login the CLI keeps in its own database ([loginStore]); it is
// asked only by a profile that declares a [cliprofile.CredentialStore], and the
// generation's [Checkout.Release] exports through the same one.
func (w *Workspace) Acquire(ctx context.Context, seat, callID string, store loginStore) (*Checkout, error) {
	state := w.seatState(seat)
	dir := w.SeatDir(seat)
	home := filepath.Join(dir, "home")
	cache := filepath.Join(dir, "cache")
	work := filepath.Join(dir, "work", seatSlug(callID))

	state.mu.Lock()
	defer state.mu.Unlock()

	// The directories exist BEFORE the seed rather than after it, which is
	// the order a seed that RUNS THE CLI needs: an import is a child whose
	// HOME is this directory and whose working directory is too.
	for _, d := range []string{home, cache, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("cli-agent: preparing %q: %w", d, err)
		}
	}
	if state.inflight == 0 {
		if err := w.prune(home); err != nil {
			return nil, err
		}
		if err := w.seed(ctx, home, cache, store); err != nil {
			// The call is not run, and it will not have a Release to
			// clean up after it: its scratch directory goes here, and
			// whatever the seed got as far as goes with the next prune.
			_ = os.RemoveAll(work)
			return nil, err
		}
	}
	// The working directory is this call's own, so its settings files
	// are written per call rather than per seat generation.
	if err := w.seedFiles(work, cliprofile.SeedWork); err != nil {
		return nil, err
	}
	state.inflight++
	return &Checkout{Home: home, Cache: cache, Work: work, ws: w, seat: state, store: store}, nil
}

// Release returns the seat share, and is safe to call twice.
//
// The LAST call out of a seat syncs a refreshed credential back to the shared
// directory and prunes. Syncing back matters more than it looks: OAuth access
// tokens expire in hours and most vendors rotate the refresh token with them,
// so discarding the file the CLI rewrote logs the whole fleet out at the next
// expiry.
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
	// A SEAT NEVER CREATES A LOGIN, it only refreshes one: written back only
	// over a credential the shared directory still holds, so a seat that was
	// mid-call while `crewlet llm logout` ran cannot put the login back.
	syncErr := c.ws.syncCredentialsOut(ctx, c.Home, c.Cache, c.store, false)
	pruneErr := c.ws.prune(c.Home)
	for _, err := range []error{workErr, syncErr, pruneErr} {
		if err != nil {
			return err
		}
	}
	return nil
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

// seed copies the shared login into a seat home.
//
// A COPY rather than a symlink into the shared directory: the CLI rewrites
// this file in place when it refreshes, and seven seats sharing one inode
// would have seven writers on it. The copy back out is [syncCredentialsOut],
// which runs once, when the seat's last call finishes.
//
// A login held in the CLI's own database is IMPORTED last, once the settings
// files are in place, because the import is the CLI itself starting in this
// home and reading them: see [Workspace.importLogin].
func (w *Workspace) seed(ctx context.Context, home, cache string, store loginStore) error {
	shared := w.CredentialsDir()
	for _, rel := range w.profile.CredentialPaths {
		src := filepath.Join(shared, filepath.Base(rel))
		info, err := os.Stat(src)
		if os.IsNotExist(err) {
			// No login yet is not an error here. The call then fails
			// with the CLI's own "not authenticated", which names the
			// vendor and is what `crewlet llm doctor` explains — far
			// better than a provider that refused to start.
			continue
		}
		if err != nil {
			return fmt.Errorf("cli-agent: reading the shared login %q: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cli-agent: shared login %q is not a regular file", src)
		}
		dst, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	if err := w.seedFiles(home, cliprofile.SeedHome); err != nil {
		return err
	}
	return w.importLogin(ctx, home, cache, store)
}

// importLogin puts a [cliprofile.CredentialStore]'s document into a home's
// database, for a profile that declares one.
//
// INTO A DATABASE THE PRUNE HAS JUST EMPTIED, which is not incidental: the CLI
// skips a credential id its database already holds rather than updating it, so
// importing a refreshed document over an older database would keep the older
// token. Both callers prune first.
func (w *Workspace) importLogin(ctx context.Context, home, cache string, store loginStore) error {
	if w.profile.CredentialStore == nil {
		return nil
	}
	path := w.documentPath()
	document, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if os.IsNotExist(err) {
		// No login yet, exactly as for a credential file above.
		return nil
	}
	if err != nil {
		return fmt.Errorf("cli-agent: reading the shared login %q: %w", path, err)
	}
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

// syncCredentialsOut copies a refreshed login back to the shared directory.
//
// Only when the content actually changed, so an idle seat does not rewrite
// the shared file — and two seats that both refreshed can still race, exactly
// as two terminals running the vendor's CLI would. A headless token has no
// refresh file and sidesteps this entirely, which is why the docs recommend
// one wherever the vendor mints one.
//
// create says whether a login the shared directory does not hold may be
// written there. Only the login home passes true: it is where `crewlet llm
// login` puts a login, so what it holds afterwards IS the login. A SEAT
// passes false and only ever refreshes one, because the shared directory
// losing a file while a seat was mid-call is `crewlet llm logout` having run
// — and writing the seat's copy back, as this used to, put the login an
// operator had just removed back on disk with nothing to say so.
//
// Every write REPLACES the file rather than rewriting it in place, through a
// temporary file and a rename: the shared file is what every other seat's
// next generation is seeded from, and a seed reading it mid-write took a
// half-written credential — or an empty one — into a seat.
func (w *Workspace) syncCredentialsOut(ctx context.Context, home, cache string, store loginStore, create bool) error {
	shared := w.CredentialsDir()
	for _, rel := range w.profile.CredentialPaths {
		src, err := cliprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		updated, err := os.ReadFile(src) //nolint:gosec // path is rooted by cliprofile.UnderRoot
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cli-agent: reading the refreshed login %q: %w", src, err)
		}
		if err := w.replaceShared(filepath.Join(shared, filepath.Base(rel)), updated, create); err != nil {
			return err
		}
	}
	if w.profile.CredentialStore == nil {
		return nil
	}
	if store == nil {
		return fmt.Errorf("cli-agent: %q keeps its login in its own database, and this "+
			"home was released with no way to export it", w.agent)
	}
	document, err := store.exportLogin(ctx, home, cache)
	if err != nil {
		return fmt.Errorf("cli-agent: exporting the login from %q: %w", home, err)
	}
	// A database holding NO login is never written over one. It is what a
	// home whose import found no document exports, and what a CLI that
	// dropped a credential it could no longer refresh exports — and in
	// either case the shared login is the better one to keep, exactly as a
	// credential file a call deleted is not deleted from the shared
	// directory above.
	if !holdsLogin(document) {
		return nil
	}
	return w.replaceShared(w.documentPath(), document, create)
}

// replaceShared puts content at dst in the shared directory when it differs
// from what is there — and, unless create, only over a file that is there.
func (w *Workspace) replaceShared(dst string, content []byte, create bool) error {
	current, err := os.ReadFile(dst) //nolint:gosec // path is inside the state dir
	switch {
	case err == nil && bytes.Equal(current, content):
		return nil
	case os.IsNotExist(err) && !create:
		return nil
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("cli-agent: reading the shared login %q: %w", dst, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(dst), err)
	}
	if err := hostbox.ReplaceFile(dst, bytes.NewReader(content)); err != nil {
		return fmt.Errorf("cli-agent: writing the refreshed login %q: %w", dst, err)
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

// copyFile copies one regular file, creating its parent, at 0600.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // callers root both paths
	if err != nil {
		return fmt.Errorf("cli-agent: reading %q: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("cli-agent: writing %q: %w", dst, err)
	}
	return nil
}
