package cliagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	profile Profile

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
func Shared(stateDir, agent string, profile Profile) (*Workspace, error) {
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

	ws       *Workspace
	seat     *seatState
	released bool
}

// Acquire prepares a place for one call to run.
//
// The FIRST concurrent call into a seat prunes and seeds; a second call
// arriving while the first is still running shares the same home untouched.
// Batched sub-agents belong to the same agent, so sharing memory between an
// agent and its own sub-agents is harmless by definition — and pruning under
// them would delete the parent's state mid-call.
func (w *Workspace) Acquire(seat, callID string) (*Checkout, error) {
	state := w.seatState(seat)
	dir := w.SeatDir(seat)
	home := filepath.Join(dir, "home")
	cache := filepath.Join(dir, "cache")
	work := filepath.Join(dir, "work", seatSlug(callID))

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.inflight == 0 {
		if err := w.prune(home); err != nil {
			return nil, err
		}
		if err := w.seed(home); err != nil {
			return nil, err
		}
	}
	for _, d := range []string{home, cache, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("cli-agent: preparing %q: %w", d, err)
		}
	}
	state.inflight++
	return &Checkout{Home: home, Cache: cache, Work: work, ws: w, seat: state}, nil
}

// Release returns the seat share, and is safe to call twice.
//
// The LAST call out of a seat syncs a refreshed credential back to the shared
// directory and prunes. Syncing back matters more than it looks: OAuth access
// tokens expire in hours and most vendors rotate the refresh token with them,
// so discarding the file the CLI rewrote logs the whole fleet out at the next
// expiry.
func (c *Checkout) Release() error {
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
	syncErr := c.ws.syncCredentialsOut(c.Home)
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
		target, err := underRoot(home, rel)
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
func (w *Workspace) seed(home string) error {
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
		dst, err := underRoot(home, rel)
		if err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
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
func (w *Workspace) syncCredentialsOut(home string) error {
	shared := w.CredentialsDir()
	for _, rel := range w.profile.CredentialPaths {
		src, err := underRoot(home, rel)
		if err != nil {
			return err
		}
		updated, err := os.ReadFile(src) //nolint:gosec // path is rooted by underRoot
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cli-agent: reading the refreshed login %q: %w", src, err)
		}
		dst := filepath.Join(shared, filepath.Base(rel))
		current, err := os.ReadFile(dst) //nolint:gosec // path is inside the state dir
		if err == nil && string(current) == string(updated) {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cli-agent: reading the shared login %q: %w", dst, err)
		}
		if err := os.MkdirAll(shared, 0o700); err != nil {
			return fmt.Errorf("cli-agent: preparing %q: %w", shared, err)
		}
		if err := os.WriteFile(dst, updated, 0o600); err != nil {
			return fmt.Errorf("cli-agent: writing the refreshed login %q: %w", dst, err)
		}
	}
	return nil
}

// HasLogin reports whether the shared directory holds any credential file.
func (w *Workspace) HasLogin() bool {
	return len(w.LoginFiles()) > 0
}

// LoginFiles lists the credential files present in the shared directory,
// as absolute paths, sorted.
func (w *Workspace) LoginFiles() []string {
	shared := w.CredentialsDir()
	var out []string
	for _, rel := range w.profile.CredentialPaths {
		path := filepath.Join(shared, filepath.Base(rel))
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// underRoot joins a profile-declared relative path onto a root and refuses
// one that escapes it.
//
// Profile paths are operator-overridable, so "../../.ssh" is reachable from
// config — and prune() calls RemoveAll on whatever this returns.
func underRoot(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("cli-agent: path %q must be relative to the seat home", rel)
	}
	joined := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("cli-agent: path %q escapes the seat home", rel)
	}
	return joined, nil
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
