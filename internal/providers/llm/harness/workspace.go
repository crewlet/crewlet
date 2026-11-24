package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Workspace owns one harness's share of a state directory: the shared login, and a
// home per seat, all under <state dir>/<harness>.
//
// One per (RESOLVED state directory, harness), from a package-level registry:
// entries on the same directory AND harness share it, so per-phase models work
// off one `crewlet llm login`. Two values over one directory would each believe
// they owned the pruning and delete the other entry's live session.
//
// KEYED BY THE HARNESS TOO, ON DISK AND IN THE REGISTRY: harnesses agree on
// nothing a workspace keeps. Keyed by directory alone, a second harness would
// prune a login it cannot read, and the registry's refusal would split the
// fleet (a running node refusing a revision a restarted one applies).
type Workspace struct {
	// stateDir is the resolved state directory, as the entry names it, and
	// root is this harness's share of it, <stateDir>/<harness>, under which every
	// file the workspace keeps lives.
	stateDir string
	root     string
	harness  string
	profile  harnessprofile.Profile
	// credentialSteps observes every write of the shared login this
	// workspace makes ([hostbox.CredentialSteps]): nil in every workspace the
	// engine opens. The kill matrix's writer process sets it before its first
	// write, to park at a step and be killed there (loginset_test.go).
	credentialSteps hostbox.CredentialSteps

	mu sync.Mutex
	// seats is each home's share, by its seat directory: a seat's login home
	// and its key home ([Workspace.AcquireForKey]) are two.
	seats map[string]*seatState
	// retries is when the next sweep may try a seat whose open generation
	// it could not carry out, by seat slug ([Workspace.sweep]). Guarded by mu.
	retries map[string]carryRetry
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
	// held releases the seat's generation lock ([Workspace.lockSeat]), taken
	// by the call that opens a generation and let go by the last call out:
	// non-nil exactly while inflight is above zero.
	held func()

	// turn is held by the one call a home whose harness keeps a daemon runs
	// at a time ([harnessprofile.HomeDaemon]), from before its generation
	// opens until after it closes.
	turn chan struct{}
}

// workspaceKey is a workspace's identity: the resolved state directory and
// the harness that keeps its files there.
type workspaceKey struct{ dir, agent string }

// registry is the process-wide table of workspaces by [workspaceKey].
var registry = struct {
	mu sync.Mutex
	m  map[workspaceKey]*Workspace
}{m: make(map[workspaceKey]*Workspace)}

// Shared returns the workspace of one harness in a state directory, creating it
// once: <stateDir>/<harness>, so two harnesses on one directory never share a file.
//
// The agent names a directory, so it must be one path element — the profile
// table's names all are, and a name that could climb out of the state
// directory or land two harnesses on one subdirectory is refused here rather than
// written under.
func Shared(stateDir, agent string, profile harnessprofile.Profile) (*Workspace, error) {
	key, err := keyOf(stateDir, agent)
	if err != nil {
		return nil, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing, ok := registry.m[key]; ok {
		return existing, nil
	}
	ws := &Workspace{
		stateDir: key.dir,
		root:     filepath.Join(key.dir, key.agent),
		harness:  agent,
		profile:  profile,
		seats:    make(map[string]*seatState),
		retries:  make(map[string]carryRetry),
	}
	registry.m[key] = ws
	return ws, nil
}

// keyOf resolves a state directory and checks the agent names one directory
// inside it.
func keyOf(stateDir, agent string) (workspaceKey, error) {
	// filepath.Base differs from any name holding a separator and from "",
	// which it renders "."; "." and ".." are their own base.
	if agent == "." || agent == ".." || filepath.Base(agent) != agent {
		return workspaceKey{}, fmt.Errorf(
			"harness: agent %q cannot name a directory in state_dir %q: it must be one "+
				"path element", agent, stateDir)
	}
	resolved, err := filepath.Abs(stateDir)
	if err != nil {
		return workspaceKey{}, fmt.Errorf("harness: resolving state_dir %q: %w", stateDir, err)
	}
	return workspaceKey{dir: filepath.Clean(resolved), agent: agent}, nil
}

// forgetWorkspace drops every harness's workspace in a state dir from the
// registry. Tests only: they stand for a process that ended, so that the next
// Shared opens the directory as a fresh process would.
func forgetWorkspace(stateDir string) {
	resolved, err := filepath.Abs(stateDir)
	if err != nil {
		return
	}
	resolved = filepath.Clean(resolved)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for key := range registry.m {
		if key.dir == resolved {
			delete(registry.m, key)
		}
	}
}

// StateDir is the resolved state directory this workspace keeps its harness's
// share of — what the entry's state_dir names, or the one derived for its key.
func (w *Workspace) StateDir() string { return w.stateDir }

// Root is this harness's share of the state directory, <state dir>/<harness>: the
// directory every file the workspace keeps lives under.
func (w *Workspace) Root() string { return w.root }

// capturesDir is where the output files of a child no call owns are made —
// the version probe, the login status, the device sign-in, the token-minting
// command, and on darwin the credential store's commands — made 0700 on first
// use: <root>/captures. Each file is unlinked before its child starts
// ([startChild]), so the directory holds nothing between them, and nothing a
// crash could leave.
func (w *Workspace) capturesDir() (string, error) {
	dir := filepath.Join(w.root, "captures")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("harness: preparing %q for a child's output: %w", dir, err)
	}
	return dir, nil
}

// CredentialsDir is where the shared login lives — ONE per workspace.
func (w *Workspace) CredentialsDir() string { return filepath.Join(w.root, "credentials") }

// SeatDir is one seat's share of the workspace: its login home, and that
// home's cache, lock and open generation.
func (w *Workspace) SeatDir(seat string) string {
	return filepath.Join(w.root, "seats", seatSlug(seat))
}

// KeySeatDir is one seat's KEY HOME for one entry, where that entry's calls
// for the seat that sign in with a key run ([Workspace.AcquireForKey]), beside
// the login homes rather than among them, so a sweep for a login to carry out
// never lists one. Per entry because what a key signs a harness in to may stay
// in its home while a call of the generation runs, and a fallback entry's call
// must not go out on the previous entry's key.
func (w *Workspace) KeySeatDir(seat, entry string) string {
	return filepath.Join(w.root, "key-seats", seatSlug(seat), seatSlug(entry))
}

// loginHome is the home every `crewlet llm` auth command runs the harness in,
// rebuilt from the shared login for each one — see
// [Provider.runInCredentialHome].
func (w *Workspace) loginHome() string { return filepath.Join(w.root, "login-home") }

// loginCache is the login home's XDG_CACHE_HOME, outside the pruned home for
// the reason a seat's cache is.
func (w *Workspace) loginCache() string { return filepath.Join(w.root, "login-cache") }

// The files a workspace keeps BESIDE its homes rather than in one, so that
// neither a bundle (which carries the credentials directory) nor a harness (whose
// HOME is a home) ever reads one as its own.
const (
	// generationFile is a seat's open [generation], in its seat directory.
	generationFile = "generation.json"
	// loginGenerationFile is the login home's open [generation], in the
	// workspace's root. On disk rather than in memory for the reason the
	// login home has a lock: every `crewlet llm` command is a process of its
	// own, so what one left open is the NEXT one's to finish.
	loginGenerationFile = "login-generation.json"
	// seatLockFile is a seat's generation lock, in its seat directory — see
	// [Workspace.lockSeat].
	seatLockFile = "generation.lock"
	// loginHomeLockFile is the login home's lock — see
	// [Workspace.lockLoginHome].
	loginHomeLockFile = "login-home.lock"
	// loginEpochFile is the shared login's OPERATOR EPOCH, in the
	// workspace's root — see [Workspace.bumpLoginEpoch].
	loginEpochFile = "login-epoch"
	// loggedOutFile records that `crewlet llm logout` removed the shared
	// login, in the workspace's root — see [Workspace.markLoggedOut].
	loggedOutFile = "logged-out"
)

// sharedLock is the shared login's lock file ([Workspace.lockShared]): the
// credentials directory's own path plus ".lock", beside it rather than in it so
// that nothing copying the directory carries the lock along. ONE NAME FOR EVERY
// WRITER of that directory — a local sandbox's teardown, writing back a login
// its box refreshed, takes the same file through [hostbox.CredentialLock], and
// so does every lock here, through [hostbox.LockFile] — because a
// compare-and-swap is one only against writers that agree on the lock.
func (w *Workspace) sharedLock() string { return hostbox.CredentialLock(w.CredentialsDir()) }

// slugReadable is how much of a handle a directory name shows before the
// disambiguating digest. Long enough to recognise a seat at a glance, short
// enough that name plus digest stays far inside NAME_MAX.
const slugReadable = 48

// seatSlug makes a seat handle safe as one path element, INJECTIVELY.
//
// Not a hash outright: an operator seeing whose home fills a disk must
// recognise the seat. Not lossy either: the directory is a seat's HOME (session,
// history, credentials), and two seats on one slug would read each other's
// transcripts, which this package's doc says must never happen.
//
// So: the readable form when it represents the handle EXACTLY, otherwise the
// readable prefix plus a digest of the whole handle, so distinct handles cannot
// meet whatever a vendor's API or a founder's YAML supplies.
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

// daemonStopper runs the harness's own stop command for the daemon it keeps in a
// home ([harnessprofile.HomeDaemon]). The PROVIDER implements it, for the reason
// it implements [loginStore]: running the harness takes its environment.
type daemonStopper interface {
	stopDaemon(ctx context.Context, home, cache string) error
}

// loginStore moves a [harnessprofile.CredentialStore]'s document into and out of a
// home through the harness's own commands.
//
// The PROVIDER implements it, because running the harness takes what only the
// provider holds — the environment its calls are given — while the workspace
// decides WHEN a home is seeded and synced. One interface for the two places
// that need it, a seat generation and the login home, so neither runs the harness
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
	// keyed is a key home's checkout ([Workspace.AcquireForKey]), which
	// carries nothing out.
	keyed bool
	// record is where the seat's open generation is kept on disk.
	record string
	// store is the generation's way back out for a login held in the harness's
	// own database: whichever provider seeded the home exports from it.
	store loginStore
	// stopper stops the home's daemon as the call releases it, nil where the
	// profile declares none or no provider runs the call.
	stopper  daemonStopper
	released bool
}

// Acquire prepares a place for one call to run. The FIRST concurrent call into a
// seat prunes and seeds; a later one shares the home untouched (pruning would
// delete the parent's state mid-call). It OPENS A
// GENERATION under the seat's lock ([Workspace.lockSeat]), held until the last
// call is out, after SWEEPING other seats' open idle generations
// ([Workspace.sweep]: a failed release holds the only copy of a rotated refresh
// token) and finishing its OWN last one ([Workspace.carryOut]); if that cannot
// be carried out the call is refused, classified so the chain moves on.
//
// store imports a login kept in the harness's own database ([loginStore]);
// Release and the sweep export through it, sound as a workspace drives ONE
// HARNESS ([Shared]).
func (w *Workspace) Acquire(ctx context.Context, seat, callID string, store loginStore) (*Checkout, error) {
	dir := w.SeatDir(seat)
	stopper, _ := store.(daemonStopper)
	checkout, err := w.checkOut(ctx, dir, callID, func(state *seatState) error {
		return w.openGeneration(ctx, seatSlug(seat), dir, state, store, stopper)
	})
	if err != nil {
		return nil, err
	}
	checkout.record = filepath.Join(dir, generationFile)
	checkout.store = store
	checkout.stopper = stopper
	return checkout, nil
}

// AcquireForKey prepares a place for one call whose harness signs in with a KEY —
// an entry under auth.mode api-key, or inherit-env handing it the engine's own
// key, in the key home of the seat and the entry ([Workspace.KeySeatDir]).
//
// A HOME APART FROM THE LOGIN'S: a harness may keep what a key signs it in to in
// its login's own file (Cursor Agent writes the session and key to auth.json and
// next prefers that key to the variable). Carried back it would overwrite the
// login of record and sign every subscription seat in with the key. So a key
// home is never seeded with the login and nothing is carried out: each
// generation opens and closes it with no credential file ([Workspace.dropCopies]).
func (w *Workspace) AcquireForKey(
	ctx context.Context, seat, entry, callID string, stopper daemonStopper,
) (*Checkout, error) {
	dir := w.KeySeatDir(seat, entry)
	checkout, err := w.checkOut(ctx, dir, callID, func(state *seatState) error {
		return w.openKeyGeneration(ctx, dir, state, stopper)
	})
	if err != nil {
		return nil, err
	}
	checkout.keyed = true
	checkout.stopper = stopper
	return checkout, nil
}

// checkOut is the part of a checkout the two kinds of home share: the call's
// own working directory, prepared, and the seat's generation opened by open
// when the call is its first, and, for a harness that keeps a daemon for its
// home, the home's one turn, waited for under ctx ([seatState.turn]).
func (w *Workspace) checkOut(
	ctx context.Context, dir, callID string, open func(*seatState) error,
) (_ *Checkout, err error) {
	state := w.seatState(dir)
	home := filepath.Join(dir, "home")
	cache := filepath.Join(dir, "cache")
	work := filepath.Join(dir, "work", seatSlug(callID))

	if w.profile.HomeDaemon != nil {
		select {
		case state.turn <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("harness: waiting for the seat's call in %q to end: %w",
				dir, context.Cause(ctx))
		}
		defer func() {
			if err != nil {
				<-state.turn
			}
		}()
	}

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
	// the order a seed that RUNS THE HARNESS needs: an import is a child whose
	// HOME is this directory and whose working directory is too.
	if err = prepareHome(home, cache); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(work, 0o700); err != nil {
		return nil, fmt.Errorf("harness: preparing %q: %w", work, err)
	}
	if err = ancestorRefusal(w.harness, filepath.Dir(work), w.profile.AncestorFiles); err != nil {
		return nil, err
	}
	// THE CALL'S DIRECTORY IS ITS OWN PROJECT ROOT. Every vendor harness here
	// looks for project files — AGENTS.md, CLAUDE.md, its own rules, skills,
	// hooks, a project config — from its working directory UP to the nearest
	// repository root, so a state_dir inside a checkout handed every seat the
	// instructions of a repository the engine does not control, and some
	// harnesses ran git in it. A `.git` file here ends that walk where the call
	// begins: measured, it confines codex, OpenCode, Kimi Code, Qwen Code,
	// Hermes, Copilot's instruction files and Gemini CLI's GEMINI.md, and it
	// changes nothing else they send. See [harnessprofile.ProjectRootMarker].
	if err = os.WriteFile(filepath.Join(work, harnessprofile.ProjectRootMarker),
		[]byte(harnessprofile.ProjectRootGitdir), 0o600); err != nil {
		return nil, fmt.Errorf("harness: marking %q as the call's own project root: %w", work, err)
	}
	// The working directory is this call's own, so its settings files are
	// written per call rather than per seat generation — and BEFORE a
	// generation is opened, so that nothing can fail between the generation
	// taking its lock and this call being counted into it: a lock taken by a
	// call that then returned an error would be held by a generation nobody
	// ever releases.
	if err = w.seedFiles(work, harnessprofile.SeedWork); err != nil {
		return nil, err
	}
	if state.inflight == 0 {
		if err = open(state); err != nil {
			return nil, err
		}
	}
	state.inflight++
	return &Checkout{Home: home, Cache: cache, Work: work, ws: w, seat: state}, nil
}

// openKeyGeneration is the first call into a key home taking its generation
// lock and emptying the home of every credential file and volatile path —
// whatever an earlier generation's harness wrote down, a process that ended
// mid-call included — before the home's own settings are seeded: the
// profile's, then a key home's own over them
// ([harnessprofile.Profile.KeySeedFiles]). On success the lock is the seat's to
// hold until its last call is out.
func (w *Workspace) openKeyGeneration(ctx context.Context, dir string, state *seatState, stopper daemonStopper) error {
	release, err := w.lockSeat(ctx, dir)
	if err != nil {
		return err
	}
	home := filepath.Join(dir, "home")
	if err := w.stopDaemon(ctx, stopper, home, filepath.Join(dir, "cache")); err != nil {
		release()
		return err
	}
	if err := errors.Join(w.dropCopies(home), w.prune(home), w.seedFiles(home, harnessprofile.SeedHome),
		writeSeeds(home, w.profile.KeySeedFiles)); err != nil {
		release()
		return err
	}
	state.held = release
	return nil
}

// openGeneration is the first call into a seat taking its generation lock,
// sweeping, carrying out its own last generation, and pruning and seeding the
// home — see [Workspace.Acquire]. On success the lock is the seat's to hold
// until its last call is out; on failure it is let go.
func (w *Workspace) openGeneration(
	ctx context.Context, slug, dir string, state *seatState, store loginStore, stopper daemonStopper,
) (err error) {
	home := filepath.Join(dir, "home")
	cache := filepath.Join(dir, "cache")
	record := filepath.Join(dir, generationFile)
	release, err := w.lockSeat(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			release()
		}
	}()
	// BEFORE THE CARRY: a daemon an engine that ended mid-call left would
	// otherwise still write the login being carried out.
	if err = w.stopDaemon(ctx, stopper, home, cache); err != nil {
		return err
	}
	w.sweep(ctx, store, slug)
	carried, err := w.carryOut(ctx, home, cache, store, record, seatRemedy(record))
	if err != nil {
		if ctx.Err() == nil {
			// A failed attempt the sweeps pace themselves by, as their own
			// are — unless it was this call's deadline that ended it, which
			// says nothing about the home.
			w.carryFailed(slug)
		}
		return err
	}
	if carried {
		w.carried(slug)
	}
	if err = w.prune(home); err != nil {
		return err
	}
	gen, err := w.seed(ctx, home, cache, store, syncRefresh)
	if err != nil {
		return err
	}
	// RECORDED ONLY ONCE THE SEED HAS LANDED: a generation is open from here,
	// and a record left by a seed that failed half way would have the next
	// call carry a half-imported database out over the shared login.
	if err = writeGeneration(record, gen); err != nil {
		return err
	}
	state.gen = gen
	state.held = release
	return nil
}

// Release returns the seat share, and is safe to call twice.
//
// The LAST call out carries a refreshed login back to the shared directory and
// prunes: vendors rotate the refresh token, so discarding it logs the fleet out
// at the next expiry. A FAILED CARRY PRUNES NOTHING: the home holds the only
// copy, kept with the generation's record for the next generation to carry out
// ([Workspace.sweep]). Either way the lock is let go, telling a sweep it is idle.
//
// ctx bounds the export of a login in the harness's own database. A caller hands
// it [context.WithoutCancel]: a call ended BY cancellation still has a
// refreshed token to write back.
func (c *Checkout) Release(ctx context.Context) error {
	if c == nil || c.released {
		return nil
	}
	c.released = true
	if c.ws.profile.HomeDaemon != nil {
		// Let go after everything below, the generation's lock included.
		defer func() { <-c.seat.turn }()
	}

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
	// The generation ends here whichever way its carry goes: its lock is let
	// go LAST, once the home is either pruned with its record cleared or
	// kept with its record in place, so a sweep that takes the lock next
	// finds one or the other and never a home half way between.
	defer func() {
		if c.seat.held != nil {
			c.seat.held()
			c.seat.held = nil
		}
	}()
	// The daemon goes first, so nothing writes the home while its login is
	// carried out and pruned. One that would not stop is reported and stopped
	// again as the next generation opens.
	stopErr := c.ws.stopDaemon(ctx, c.stopper, c.Home, c.Cache)
	if c.keyed {
		// Nothing to carry: what a key signed the harness in to goes with
		// the generation ([Workspace.AcquireForKey]).
		return errors.Join(workErr, stopErr, c.ws.dropCopies(c.Home), c.ws.prune(c.Home))
	}
	if _, err := c.ws.syncCredentialsOut(ctx, c.Home, c.Cache, c.store, c.seat.gen); err != nil {
		return errors.Join(workErr, stopErr, fmt.Errorf("harness: the login in %q could not be "+
			"carried back to the shared directory, so the home was kept as it is for the "+
			"next generation to carry out: %w", c.Home, err))
	}
	pruneErr := c.ws.prune(c.Home)
	return errors.Join(workErr, stopErr, pruneErr, clearGeneration(c.record))
}

// carryRefreshed carries a login the harness refreshed WHILE ITS CALL GOES ON
// back to the shared directory, moving the generation's baseline (on disk too)
// to the login then shared.
//
// Carried only at release, a token refreshed an hour into a phase would leave
// other seats seeding the rotated-away one, so it is carried at each tool call,
// when the harness waits on the engine rather than writing to its home.
//
// THE BASELINE MOVES WITH THE WRITE, WHOLE: the next carry is judged against the
// login now shared ([Workspace.carry]); on the old one the release would see this
// very write and drop the next refresh as superseded. A carry a newer login
// superseded moves nothing. A key home carries nothing.
func (c *Checkout) carryRefreshed(ctx context.Context) error {
	if c == nil || c.keyed || c.released {
		return nil
	}
	c.seat.mu.Lock()
	defer c.seat.mu.Unlock()
	_, landed, err := c.ws.carryChanged(ctx, c.Home, c.Cache, c.store, c.seat.gen)
	if err != nil || landed == nil {
		return err
	}
	gen := c.seat.gen
	gen.Seeded = landed
	if err := writeGeneration(c.record, gen); err != nil {
		return err
	}
	c.seat.gen = gen
	return nil
}

// seatState returns the share of the home in a seat directory, creating it
// once.
func (w *Workspace) seatState(dir string) *seatState {
	w.mu.Lock()
	defer w.mu.Unlock()
	state, ok := w.seats[dir]
	if !ok {
		state = &seatState{turn: make(chan struct{}, 1)}
		w.seats[dir] = state
	}
	return state
}

// seatTempDir is the directory TMPDIR names, under the seat's home.
//
// The ENGINE'S, not a profile's: every child is handed it, and every harness
// leaves something in it — Claude Code a probe directory on almost every call,
// Qwen Code a compile cache, OpenCode its own scratch — so it is pruned with
// the volatile paths whatever the profile lists, and made again, private and
// empty, for the generation that follows. Kept, a seat's home grew by what
// every call it ever made left behind.
const seatTempDir = "tmp"

// prune deletes the profile's volatile paths, and the engine's temporary
// directory, from a seat home, and leaves that directory in place and empty.
//
// Run only where no call of the seat is in flight — a generation opening, a
// login home being emptied — so nothing that is still writing loses its files.
func (w *Workspace) prune(home string) error {
	for _, rel := range append(slices.Clone(w.profile.VolatilePaths), seatTempDir) {
		target, err := harnessprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("harness: pruning %q: %w", target, err)
		}
	}
	// MADE AGAIN, because a harness handed a TMPDIR that does not exist fails
	// the first temporary file it opens rather than making the directory.
	if err := os.MkdirAll(tempDir(home), 0o700); err != nil {
		return fmt.Errorf("harness: preparing the seat's temporary directory: %w", err)
	}
	return nil
}

// stopDaemon ends the daemon the harness keeps for home ([harnessprofile.HomeDaemon])
// while its pid file says one runs: through the harness's own stop command, and
// then the file must be gone. Nothing to do for a profile with no daemon, or a
// caller with no provider to run the harness (a test's bare workspace).
func (w *Workspace) stopDaemon(ctx context.Context, stopper daemonStopper, home, cache string) error {
	d := w.profile.HomeDaemon
	if d == nil || stopper == nil {
		return nil
	}
	pidFile, err := harnessprofile.UnderRoot(home, d.PIDFile)
	if err != nil {
		return err
	}
	running := func() (bool, error) {
		_, err := os.Stat(pidFile)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	if up, err := running(); err != nil || !up {
		return err
	}
	if err := stopper.stopDaemon(ctx, home, cache); err != nil {
		return fmt.Errorf("harness: stopping the daemon %s keeps for %q: %w", w.harness, home, err)
	}
	if up, err := running(); err != nil || up {
		return errors.Join(err, fmt.Errorf("harness: %s still records a daemon in %s after "+
			"its own stop command (home_daemon.stop_args)", w.harness, pidFile))
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
		target, err := harnessprofile.UnderRoot(home, rel)
		if err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("harness: removing %q: %w", target, err)
		}
	}
	return nil
}

// emptyLoginHome leaves the login home holding no login and no open generation:
// its credential files' copies dropped, its volatile paths — a credential
// store's database among them — pruned, and its record cleared. The caller
// holds the login home's lock.
//
// What a command that carried its login out does on its way out, so the next
// command starts from the login of record; and what discarding a login nothing
// carried out does, for a logout and for a login that replaces it.
func (w *Workspace) emptyLoginHome() error {
	home := w.loginHome()
	return errors.Join(
		w.dropCopies(home),
		w.prune(home),
		clearGeneration(filepath.Join(w.root, loginGenerationFile)))
}

// seed copies the shared login into a home and opens its [generation] under
// mode: the baseline is the SHA-256 of each file copied in and the document
// imported (a missing name is an absent file) and, for a login-making command,
// the operator epoch ([generation.SeededEpoch], [generation.After]).
//
// A COPY, not a symlink: the harness rewrites in place and seven seats would
// share one inode. The login is read whole under the shared lock
// ([Workspace.seedFrom]) or it could straddle a write; the digest is of the very
// bytes written. A login in the harness's database is IMPORTED last, after the
// settings files ([Workspace.importLogin]). A REFUSED IMPORT
// ([*loginRefusedError]) still returns the generation: a login-making command
// judges what it stores against that document ([Provider.runInCredentialHome]).
func (w *Workspace) seed(
	ctx context.Context, home, cache string, store loginStore, mode syncMode,
) (generation, error) {
	login, epoch, err := w.seedFrom(ctx, mode)
	if err != nil {
		return generation{}, err
	}
	seeded := map[string]string{}
	gen := generation{Mode: mode, Seeded: seeded, SeededEpoch: epoch}
	if epoch != nil {
		// The epoch a login command's login is ordered after, should it be
		// carried out by a later command rather than by its own
		// ([generation.After]): as of its seed, and moved to when it finished
		// should its own carry fail. Its own copy, moved on its own.
		after := *epoch
		gen.After = &after
	}
	for _, rel := range w.profile.CredentialPaths {
		name := filepath.Base(rel)
		dst, err := harnessprofile.UnderRoot(home, rel)
		if err != nil {
			return generation{}, err
		}
		content, present := login[name]
		if !present {
			// NO LOGIN YET is not an error here. The call then fails with
			// the harness's own "not authenticated", which names the vendor
			// and is what `crewlet llm doctor` explains — far better than
			// a provider that refused to start.
			//
			// AND THE HOME KEEPS NONE EITHER. A credential file is not
			// volatile, so the copy an earlier generation left is still in
			// the home — and skipping the seed used to leave the harness signed
			// in with it, so a seat went on calling as a login `crewlet llm
			// logout` had removed.
			if err = os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return generation{}, fmt.Errorf("harness: removing the stale login %q: %w", dst, err)
			}
			continue
		}
		// A MIXED FILE IS SEEDED AS ITS LOGIN ALONE ([harnessprofile.FileRule.Login]),
		// so no setting in the shared copy reaches a seat home on the engine
		// host, where nothing sandboxes it (Kimi Code 2.7.2 runs a home's
		// config.toml [[hooks]]); a box is handed the file whole.
		handed, seedErr := w.profile.FileRule(name).Login(content)
		if seedErr != nil {
			return generation{}, fmt.Errorf("harness: seeding %q: %w", dst, seedErr)
		}
		if err = writePrivate(dst, handed); err != nil {
			return generation{}, err
		}
		seeded[name] = digestOf(handed)
	}
	if err := w.seedFiles(home, harnessprofile.SeedHome); err != nil {
		return generation{}, err
	}
	if err := w.importLogin(ctx, home, cache, store, login, seeded); err != nil {
		var refused *loginRefusedError
		if errors.As(err, &refused) {
			return gen, err
		}
		return generation{}, err
	}
	return gen, nil
}

// seedFrom reads the login a home is seeded from and, for a command that MAKES a
// login ([syncLogin]), the operator epoch beside it, in ONE HOLD of the shared
// login's lock ([generation.SeededEpoch]): every writer that moves the epoch
// moves it in the hold its write is made in ([Workspace.bumpLoginEpoch]), so the
// two are read as of one instant. A seat's never needs the epoch, and is not
// refused over an epoch it would never read.
func (w *Workspace) seedFrom(ctx context.Context, mode syncMode) (map[string][]byte, *uint64, error) {
	unlock, err := w.lockShared(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	login, err := w.readLoginLocked()
	if err != nil || mode != syncLogin {
		return login, nil, err
	}
	epoch, err := w.loginEpoch()
	if err != nil {
		return nil, nil, err
	}
	return login, &epoch, nil
}

// importLogin puts a [harnessprofile.CredentialStore]'s document, as the login seed
// read it, into a home's database, for a profile that declares one, and
// records the document in seeded.
//
// INTO A DATABASE THE PRUNE HAS JUST EMPTIED, which is not incidental: the harness
// skips a credential id its database already holds rather than updating it, so
// importing a refreshed document over an older database would keep the older
// token. Both callers prune first.
func (w *Workspace) importLogin(
	ctx context.Context, home, cache string, store loginStore, login map[string][]byte, seeded map[string]string,
) error {
	if w.profile.CredentialStore == nil {
		return nil
	}
	path := w.documentPath()
	document, present := login[harnessprofile.CredentialStoreDocument]
	if !present {
		// No login yet, exactly as for a credential file above.
		return nil
	}
	seeded[harnessprofile.CredentialStoreDocument] = digestOf(document)
	if !holdsLogin(document) {
		return nil
	}
	if store == nil {
		// Every caller in this package hands one; a nil here is a caller
		// that would otherwise run the seat with no login while the shared
		// directory plainly holds one.
		return fmt.Errorf("harness: %q keeps its login in its own database, and this "+
			"home was prepared with no way to import %q into it", w.harness, path)
	}
	if err := store.importLogin(ctx, home, cache, document); err != nil {
		var exit *auxiliaryExitError
		if errors.As(err, &exit) {
			return &loginRefusedError{key: store.providerKey(), path: path, err: err}
		}
		return fmt.Errorf("harness: importing the shared login %q: %w", path, err)
	}
	return nil
}

// loginRefusedError is a shared login the harness ran and REFUSED to import: a
// document it cannot read, which the same call made again would be refused
// identically — what [acquireFailureKind] reads as an unusable login.
//
// It NAMES THE WAYS OUT, every one of which works on a document the harness cannot
// read: a login replaces it ([Provider.runInCredentialHome]), `-from-host`
// adopts a working one over it without importing it, and a logout removes it.
// A bundle exported by another build of the harness — one whose shapes this build
// decodes strictly — is the usual way to get here.
type loginRefusedError struct {
	key, path string
	err       error
}

func (e *loginRefusedError) Error() string {
	return fmt.Sprintf("harness: the harness refused to import the shared login %q: %v — sign "+
		"in again with `crewlet llm login %s`, which replaces it, adopt a login that works "+
		"with `crewlet llm login %s -from-host`, or remove it with `crewlet llm logout %s`",
		e.path, e.err, e.key, e.key, e.key)
}

func (e *loginRefusedError) Unwrap() error { return e.err }

// acquireFailureKind classifies a seat that could not be made ready to run:
//   - A command that ran out of time (the auxiliary bound or the call's own
//     deadline) is a TIMEOUT, as in [Provider.completion]: a busy host, so the
//     chain may try another member and the credential is not benched.
//   - A shared login the harness refused to import is AUTH: unusable until
//     somebody signs in again, so the chain moves on, and the next call tries
//     the login again so `crewlet llm login` mends it.
//   - Any other command that ran and did not succeed (non-zero, or exit 0
//     without being seen to its end) is a SERVER failure, like a vendor 5xx; the
//     login is fine. The export carrying out a previous login is the usual one.
//   - Everything else is a filesystem failure or an unstartable harness: FATAL.
func acquireFailureKind(err error) llm.ErrorKind {
	var refused *loginRefusedError
	var exit *auxiliaryExitError
	var unfinished *auxiliaryUnfinishedError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return llm.KindTimeout
	case errors.As(err, &refused):
		return llm.KindAuth
	case errors.As(err, &exit), errors.As(err, &unfinished):
		return llm.KindServer
	default:
		return llm.KindFatal
	}
}

// seedFiles writes the profile's settings files for one scope into root.
//
// Written on EVERY seed rather than once: the harness may rewrite its own
// settings file during a call, and a vendor's tool policy that quietly
// changed under a seat is the hole these files exist to close. Verbatim at
// 0600, parents created, rooted the same way prune's targets are.
func (w *Workspace) seedFiles(root string, scope harnessprofile.SeedScope) error {
	var files []harnessprofile.SeedFile
	for _, f := range w.profile.SeedFiles {
		if f.Scope() == scope {
			files = append(files, f)
		}
	}
	return writeSeeds(root, files)
}

// writeSeeds writes settings files into root, in order — so a later one at a
// path replaces an earlier one's content there.
func writeSeeds(root string, files []harnessprofile.SeedFile) error {
	for _, f := range files {
		dst, err := harnessprofile.UnderRoot(root, f.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("harness: preparing %q: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o600); err != nil {
			return fmt.Errorf("harness: writing the seeded settings file %q: %w", dst, err)
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
	// no login (`status`, a vendor's own logout). Its harness can only have
	// REFRESHED the login it was seeded from, so the write is a
	// COMPARE-AND-SWAP against that login, WHOLE: made only while the shared
	// directory still holds exactly what the home was seeded from — every
	// file, and every absence — and never where the shared directory held
	// nothing. Anything else is somebody else's newer write — another seat's
	// refresh, an operator's new login, a logout — and the newer one is
	// kept ([Workspace.carry]).
	syncRefresh syncMode = "refresh"
	// syncLogin is a command that MAKES a login: `crewlet llm login`, a
	// credential login, a token capture. What its harness stored is written
	// whenever it differs from what the command started with, OVER whatever
	// the shared directory holds by then — an operator's new login has to
	// win over a refresh a seat wrote during a browser flow that took
	// minutes — as the WHOLE login its sign-in left, never one file of it
	// beside another sign-in's ([Workspace.carry]); and it is the one mode
	// that may create the shared login.
	syncLogin syncMode = "login"
)

// Valid reports whether m is one of the two modes.
func (m syncMode) Valid() bool { return m == syncRefresh || m == syncLogin }

// generation is one home's OPEN GENERATION: how a login its harness changed is
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
	// After is the shared login's OPERATOR EPOCH ([Workspace.bumpLoginEpoch])
	// as of when a [syncLogin] generation's own carry failed, read under the
	// shared login's lock ([Workspace.orderPendingLogin]) — or as of its seed
	// ([generation.SeededEpoch]), where the lock could not be taken or the
	// epoch read at the failure — set on the record of every login command and
	// read only when that record is CARRIED OUT LATER ([Workspace.carryOut]). A
	// command's own carry is made the moment it ends, and an operator's login
	// is written then over whatever is there, the later of two sign-ins
	// winning; a login that did not land then is ordered by this instead:
	// written only while no operator has replaced the login of record since it
	// finished — a `-from-host`, an import, a logout — and never over one.
	After *uint64 `json:"after,omitempty"`
	// SeededEpoch is the shared login's OPERATOR EPOCH as of the seed, read
	// in the SAME HOLD of the shared login's lock as the login the home was
	// seeded from, and set on the record of every [syncLogin] generation. It
	// is what tells a file the sign-in removed and a seat has since REFRESHED
	// from one another sign-in has since REPLACED, which no comparison of
	// bytes can ([Workspace.carry]): while the epoch is still this one, every
	// write the shared login took since the seed was a refresh of the login
	// the home was seeded from. Read outside that hold, an operator's write
	// landing between the two reads would be counted as having come before
	// the seed. A record without it vouches for no write since its seed.
	SeededEpoch *uint64 `json:"seeded_epoch,omitempty"`
}

// readGeneration reads an open generation's record, and reports false where
// there is none.
func readGeneration(path string) (generation, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if os.IsNotExist(err) {
		return generation{}, false, nil
	}
	if err != nil {
		return generation{}, false, fmt.Errorf("harness: reading %q: %w", path, err)
	}
	var gen generation
	if err := json.Unmarshal(raw, &gen); err != nil || !gen.Mode.Valid() {
		// REFUSED, not skipped: skipping would prune a home that may hold
		// the only copy of a refreshed login, which is exactly what the
		// record exists to prevent. Removing it is the operator's call,
		// and the message says so.
		return generation{}, false, fmt.Errorf("harness: %q does not record a generation "+
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
		return fmt.Errorf("harness: encoding %q: %w", path, err)
	}
	if err := hostbox.ReplaceFile(path, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("harness: recording the open generation in %q: %w", path, err)
	}
	return nil
}

// clearGeneration removes a generation's record once its login is carried out.
func clearGeneration(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("harness: closing the generation recorded in %q: %w", path, err)
	}
	return nil
}

// carryOut finishes a generation an earlier call or command left open, before
// the prune that would discard what its home holds, and reports whether there
// was one.
//
// Under the record's own mode and baseline, exactly as the release it stands in
// for would have — so carrying out a home whose harness changed nothing writes
// nothing, a login the shared directory has since moved past is not written
// over it, and a home whose export holds no login is carried out by doing
// nothing. Only then is the record removed and the home free to prune; a carry
// that fails again leaves both for the next attempt, and the
// [*carryOutError] it answers names remedy — how an operator discards what is
// kept, for a carry that will never succeed.
func (w *Workspace) carryOut(
	ctx context.Context, home, cache string, store loginStore, record, remedy string,
) (bool, error) {
	gen, open, err := readGeneration(record)
	if err != nil || !open {
		return false, err
	}
	if _, err := w.syncCredentialsOut(ctx, home, cache, store, gen); err != nil {
		return false, &carryOutError{home: home, remedy: remedy, err: err}
	}
	return true, clearGeneration(record)
}

// carryOutError is an open generation whose login could not be carried out, so
// its home was kept as it is.
//
// It NAMES THE WAY OUT, because a carry that fails for good — a harness upgrade that
// broke its export, a login past what an export may print — refuses the seat, or
// the auth command, every time it is asked: what discards the kept login is the
// one thing the person reading this needs and cannot otherwise find.
type carryOutError struct {
	home, remedy string
	err          error
}

func (e *carryOutError) Error() string {
	return fmt.Sprintf("harness: the login an earlier generation left in %q has not been "+
		"carried back to the shared directory, and it may be the only copy of a token the "+
		"harness refreshed, so it was kept and the harness was not started on top of it — it is "+
		"tried again next time; %s: %v", e.home, e.remedy, e.err)
}

func (e *carryOutError) Unwrap() error { return e.err }

// seatRemedy is how an operator discards a seat's kept login.
func seatRemedy(record string) string {
	return fmt.Sprintf("if it can never be carried out, remove %s to discard it, and the "+
		"seat's next call starts from the shared login", record)
}

// loginHomeRemedy is how an operator discards the login home's kept login.
func loginHomeRemedy(record string) string {
	return fmt.Sprintf("if it can never be carried out, `crewlet llm login` signs in afresh "+
		"and discards it, as `crewlet llm logout` does, or remove %s to discard it", record)
}

// carryRetry is how a sweep paces its attempts on one seat it could not carry
// out: failures in a row, and when it may try again.
type carryRetry struct {
	failures int
	next     time.Time
}

// carryRetryFloor and carryRetryCeiling bound a sweep's backoff on a seat whose
// carry-out keeps failing.
//
// The FIRST sweep after a release's own carry failed is immediate: other seats
// starting now would otherwise be seeded with a rotated-away token. Then the
// attempts double from a minute, how long a transient cause (a timed-out export
// on a busy host, a memory kill) takes to clear, to half an hour, because each
// attempt is an export of up to [auxiliaryTimeout] inside some OTHER seat's
// call and a permanent failure must not cost every seat's first call that long:
// one attempt per half-hour per wedged seat, while the seat's own next call
// still tries every time ([Workspace.Acquire]).
const (
	carryRetryFloor   = time.Minute
	carryRetryCeiling = 30 * time.Minute
)

// sweepDue reports whether a sweep may try a seat now.
func (w *Workspace) sweepDue(slug string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.retries[slug]
	return !ok || !time.Now().Before(r.next)
}

// carried forgets a seat's failures once its generation is carried out.
func (w *Workspace) carried(slug string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.retries, slug)
}

// carryFailed counts one more failed carry-out of a seat's open generation and
// reports how long a sweep now leaves it.
func (w *Workspace) carryFailed(slug string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.retries[slug]
	r.failures++
	wait := carryRetryFloor
	for i := 1; i < r.failures && wait < carryRetryCeiling; i++ {
		wait *= 2
	}
	wait = min(wait, carryRetryCeiling)
	r.next = time.Now().Add(wait)
	w.retries[slug] = r
	return wait
}

// sweep carries out every OTHER seat's open idle generation (a failed release,
// or a process that ended mid-call) before the calling seat seeds.
//
// IDLE IS THE SEAT'S GENERATION LOCK, taken without waiting: a takeable flock
// means no call runs in the home, in this process or another.
// BEST EFFORT: a failed carry is logged (`harness_carry_out_failed`), paced
// ([carryRetryFloor]) and retried later, and the caller seeds regardless;
// refusing would turn one seat's broken export into every seat's outage. SEATS
// ARE LISTED, NEVER GLOBBED: the state directory is the operator's path and a
// pattern reads `[`, `*`, `?` as syntax. A listing failing but for a missing
// directory is logged (`harness_sweep_failed`).
func (w *Workspace) sweep(ctx context.Context, store loginStore, own string) {
	who := ""
	if store != nil {
		who = store.providerKey()
	}
	seats := filepath.Join(w.root, "seats")
	entries, err := os.ReadDir(seats)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WarnContext(ctx, "harness_sweep_failed", "provider", who, "agent", w.harness,
				"error", err)
		}
		return
	}
	for _, entry := range entries {
		slug := entry.Name()
		if !entry.IsDir() || slug == own || !w.sweepDue(slug) {
			continue
		}
		dir := filepath.Join(seats, slug)
		record := filepath.Join(dir, generationFile)
		if _, err := os.Lstat(record); os.IsNotExist(err) {
			// Nothing open; any other failure to look is the carry's to
			// report, which reads the same record.
			continue
		}
		if ctx.Err() != nil {
			return
		}
		carried, err := w.sweepOne(ctx, store, dir, record)
		switch {
		case err == nil:
			if carried {
				w.carried(slug)
			}
		case ctx.Err() != nil:
			// The calling seat's own deadline, which says nothing about this
			// seat's home: counted, it would pace a carry that never ran.
			return
		default:
			log.WarnContext(ctx, "harness_carry_out_failed", "provider", who, "agent", w.harness,
				"seat", slug, "retry_in", w.carryFailed(slug).String(), "error", err)
		}
	}
}

// sweepOne carries out one idle seat's open generation under its lock, and
// reports false where the seat was busy or had nothing open.
func (w *Workspace) sweepOne(ctx context.Context, store loginStore, dir, record string) (bool, error) {
	release, err := hostbox.LockFile(ctx, filepath.Join(dir, seatLockFile), 0, seatHolder)
	var busy *hostbox.LockWaitError
	if errors.As(err, &busy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()
	return w.carryOut(ctx, filepath.Join(dir, "home"), filepath.Join(dir, "cache"),
		store, record, seatRemedy(record))
}

// syncCredentialsOut carries a login a home's harness CHANGED back to the shared
// directory, and reports whether the home holds a login at all.
//
// CHANGED IS JUDGED AGAINST THE GENERATION'S BASELINE, never the shared
// directory as it is now: an idle home differs from a newer shared login, and
// writing it back would REVERT another seat's refresh or an operator's login.
// It is decided under the shared lock by [syncMode] ([Workspace.carry]); a loser
// is logged `harness_login_superseded`. Neither kind deletes the shared SEEDED
// COPY of a file its harness REMOVED. Everything is READ before anything is
// written, so a failed export writes nothing and the carry retries whole
// ([Workspace.carryOut]); the write is ONE ([Workspace.writeLoginLocked]), as a
// kill between files would leave two sign-ins.
func (w *Workspace) syncCredentialsOut(
	ctx context.Context, home, cache string, store loginStore, gen generation,
) (bool, error) {
	holds, _, err := w.carryChanged(ctx, home, cache, store, gen)
	return holds, err
}

// carryChanged is [Workspace.syncCredentialsOut], reporting besides the digests
// of the login the shared directory holds once the carry is made, by file name
// — when that is this home's login, written or found there already — and nil
// when a newer login superseded it or the home changed nothing: the baseline a
// generation that goes on running carries out from next
// ([Checkout.carryRefreshed]).
func (w *Workspace) carryChanged(
	ctx context.Context, home, cache string, store loginStore, gen generation,
) (bool, map[string]string, error) {
	// held is every file of the login the home holds; changed is those of
	// them that differ from what the home was seeded with; raw is held as the
	// home holds it, which the account check reads ([Workspace.carry]).
	held := map[string][]byte{}
	changed := map[string][]byte{}
	raw := map[string][]byte{}
	holds := false
	for _, rel := range w.profile.CredentialPaths {
		src, err := harnessprofile.UnderRoot(home, rel)
		if err != nil {
			return false, nil, err
		}
		content, err := os.ReadFile(src) //nolint:gosec // path is rooted by harnessprofile.UnderRoot
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, nil, fmt.Errorf("harness: reading the refreshed login %q: %w", src, err)
		}
		name := filepath.Base(rel)
		held[name] = content
		raw[name] = content
		// A settings file the harness also keeps its login in is carried with
		// the rest, but answers "holds a login" only holding one
		// ([harnessprofile.Profile.HoldsLogin]): a login command that left
		// just the harness's own record of itself signed nobody in.
		if w.profile.HoldsLogin(name, content) {
			holds = true
		}
		if w.seedDigest(name, content) != gen.Seeded[name] {
			changed[name] = content
		}
	}
	if w.profile.CredentialStore != nil {
		if store == nil {
			return false, nil, fmt.Errorf("harness: %q keeps its login in its own database, and this "+
				"home was released with no way to export it", w.harness)
		}
		document, err := store.exportLogin(ctx, home, cache)
		if err != nil {
			return false, nil, fmt.Errorf("harness: exporting the login from %q: %w", home, err)
		}
		if holdsLogin(document) {
			holds = true
			held[harnessprofile.CredentialStoreDocument] = document
			raw[harnessprofile.CredentialStoreDocument] = document
			if digestOf(document) != gen.Seeded[harnessprofile.CredentialStoreDocument] {
				changed[harnessprofile.CredentialStoreDocument] = document
			}
		}
	}
	if len(changed) == 0 {
		return holds, nil, nil
	}
	who := ""
	if store != nil {
		who = store.providerKey()
	}
	unlock, err := w.lockShared(ctx)
	if err != nil {
		return holds, nil, err
	}
	defer unlock()
	if gen.Mode == syncLogin && gen.After != nil {
		// A LOGIN CARRIED OUT AFTER ITS COMMAND ENDED, which an operator's
		// own later write of the login of record outranks: see
		// [generation.After].
		epoch, epochErr := w.loginEpoch()
		if epochErr != nil {
			return holds, nil, epochErr
		}
		if epoch != *gen.After {
			for _, name := range slices.Sorted(maps.Keys(changed)) {
				w.superseded(ctx, who, w.sharedPath(name), "replaced")
			}
			return holds, nil, nil
		}
	}
	current, err := w.readLoginLocked()
	if err != nil {
		return holds, nil, err
	}
	// THE SHARED LOGIN STILL THE SEED, judged before anything is carried, as a
	// box's teardown judges it: a newer login is said as moved, whatever the
	// home's copy holds.
	if w.movedPastSeed(ctx, who, gen, current, changed) {
		return holds, nil, nil
	}
	if gen.Mode == syncRefresh {
		// ONLY TOKENS ARE CARRIED ([harnessprofile.FileRule.Carry]): a file's
		// refresh keys, never an endpoint, a key a call minted or a setting the
		// harness wrote, and a file whose login is then its seed changed
		// nothing. A copy that cannot be read carries nothing, the shared login
		// kept. Read here, under the lock, against the login a refresh is a
		// compare-and-swap with.
		for _, name := range slices.Sorted(maps.Keys(held)) {
			kept, carryErr := w.profile.KeepLogin(name, held[name], current[name])
			if carryErr != nil {
				w.superseded(ctx, who, w.sharedPath(name), "unreadable")
				return holds, nil, nil
			}
			held[name] = kept
			if _, ok := changed[name]; !ok {
				continue
			}
			if w.seedDigest(name, kept) == gen.Seeded[name] {
				delete(changed, name)
			} else {
				changed[name] = kept
			}
		}
	}
	login, write, err := w.carry(ctx, who, gen, current, held, changed, raw)
	if err != nil {
		return holds, nil, err
	}
	// NOTHING TO WRITE is not nothing to do. A refresh a newer login
	// superseded stops here, but a login whose files the shared login
	// already holds — a carry retried whole because a step after its write
	// failed — IS the login of record, written, and still ends the logout
	// below: answered as a carry with nothing left to do, the retry left the
	// logout standing for good.
	switch {
	case write && gen.Mode == syncLogin:
		// A LOGIN INSTALLED, so a new install record, committed with it
		// ([LoginInstall]). Only on a write: nothing to write is a carry
		// retried after its login landed, and that login landed with its
		// record in the same step, so minting another would name one
		// sign-in twice. A sign-in that left only a settings file holding
		// no login installed none, and its write keeps no record either.
		var install LoginInstall
		if len(w.loginIn(login)) > 0 {
			var err error
			if install, err = newLoginInstall(time.Now()); err != nil {
				return holds, nil, err
			}
		}
		if err := w.installLoginLocked(ctx, who, login, install); err != nil {
			return holds, nil, err
		}
	case write:
		// A refresh, the same sign-in: its record stays.
		if err := w.writeLoginLocked(ctx, who, login); err != nil {
			return holds, nil, err
		}
	}
	if gen.Mode == syncLogin {
		// A LOGIN OF RECORD, written: a logout before it is over, and a
		// configured bundle restores again into a directory left empty later
		// ([Workspace.markLoggedOut]). Under the lock, with the write.
		if err := w.clearLoggedOut(); err != nil {
			return holds, nil, err
		}
	}
	if login == nil {
		return holds, nil, nil
	}
	return holds, w.seedDigests(login), nil
}

// carry decides, under the shared lock, what a home's changed login makes of
// the shared one (current), by the generation's [syncMode]:
//   - a LOGIN is written as the WHOLE LOGIN ITS SIGN-IN LEFT and NO FILE THE HOME
//     NEVER HELD (another sign-in's). A seeded file the home dropped is NOT
//     REMOVED ([Workspace.syncCredentialsOut]) and goes back as shared while that
//     is still the seeded login, refreshed; once an operator's write replaced it
//     it is ANOTHER SIGN-IN'S and dropped, told by the OPERATOR EPOCH
//     ([Workspace.onlyRefreshedSince]);
//   - a REFRESH is a COMPARE-AND-SWAP OF THE WHOLE LOGIN: only while the shared
//     login is EXACTLY the seeded one, of seeded files only, of its account.
//
// It answers the login then shared and whether to write it.
func (w *Workspace) carry(
	ctx context.Context, who string, gen generation, current, held, changed, raw map[string][]byte,
) (map[string][]byte, bool, error) {
	next := maps.Clone(current)
	switch gen.Mode {
	case syncLogin:
		next = maps.Clone(held)
		// refreshed is whether only refreshes have written the shared login
		// since the seed, read once and only for a removed file whose copy is
		// not the seeded one.
		var refreshed *bool
		for _, name := range slices.Sorted(maps.Keys(current)) {
			content := current[name]
			if _, holds := held[name]; holds {
				continue
			}
			seed, seeded := gen.Seeded[name]
			if !seeded {
				continue
			}
			if w.seedDigest(name, content) != seed {
				if refreshed == nil {
					only, err := w.onlyRefreshedSince(gen)
					if err != nil {
						return nil, false, err
					}
					refreshed = &only
				}
				if !*refreshed {
					continue
				}
			}
			next[name] = content
		}
	case syncRefresh:
		for name, content := range changed {
			if _, seeded := gen.Seeded[name]; seeded {
				next[name] = content
			}
		}
	default:
		return nil, false, fmt.Errorf("harness: carrying a login out under an unknown mode %q", gen.Mode)
	}
	if maps.EqualFunc(next, current, bytes.Equal) {
		// NOTHING TO WRITE, and the shared login is this home's only where
		// the home holds exactly it — its refresh already there, or a carry
		// repeated after its write landed. A home that changed only files a
		// refresh never carries holds the seed beside a newer writer's
		// login, which is no baseline of its own: taken for one, its next
		// refresh would pass the compare-and-swap and revert that login.
		if homeHolds(gen, current, held) {
			return current, false, nil
		}
		return nil, false, nil
	}
	if w.movedPastSeed(ctx, who, gen, current, changed) {
		return nil, false, nil
	}
	if gen.Mode == syncRefresh {
		// A COPY WHOSE ACCOUNT FIELDS OR NAMED TOKEN NAME ANOTHER ACCOUNT is not
		// carried ([harnessprofile.FileRule.SameAccount]): the whole login stays
		// as it is. Asked of EVERY seeded file, changed or not, against current,
		// the seeded login here; a file holding them that the home dropped
		// names none.
		var other []string
		for _, name := range slices.Sorted(maps.Keys(gen.Seeded)) {
			rule := w.profile.FileRule(name)
			if len(rule.IdentityKeys) == 0 {
				continue
			}
			if copied, present := raw[name]; !present || !rule.SameAccount(copied, current[name]) {
				other = append(other, name)
			}
		}
		for _, name := range other {
			w.superseded(ctx, who, w.sharedPath(name), "other_account")
		}
		if len(other) > 0 {
			return nil, false, nil
		}
	}
	return next, true, nil
}

// movedPastSeed reports whether a refresh's shared login, current, is no longer
// the seed: somebody else's newer write (another seat's refresh, an operator's
// login, a logout), which is kept and said for each seeded file the home
// changed.
func (w *Workspace) movedPastSeed(
	ctx context.Context, who string, gen generation, current, changed map[string][]byte,
) bool {
	if gen.Mode != syncRefresh || maps.Equal(w.seedDigests(current), gen.Seeded) {
		return false
	}
	for _, name := range slices.Sorted(maps.Keys(changed)) {
		if _, seeded := gen.Seeded[name]; !seeded {
			continue
		}
		how := "changed"
		if len(w.loginIn(current)) == 0 {
			how = "removed"
		}
		w.superseded(ctx, who, w.sharedPath(name), how)
	}
	return true
}

// homeHolds reports whether a home holds exactly the login given: every file
// the generation was seeded with and every file of the login, each present in
// both with the same bytes or absent from both.
func homeHolds(gen generation, login, held map[string][]byte) bool {
	names := map[string]bool{}
	for name := range gen.Seeded {
		names[name] = true
	}
	for name := range login {
		names[name] = true
	}
	for name := range names {
		have, holds := held[name]
		want, in := login[name]
		if holds != in || !bytes.Equal(have, want) {
			return false
		}
	}
	return true
}

// onlyRefreshedSince reports, under the shared lock the caller holds, whether
// every write since a login command's home was seeded was a REFRESH of its
// seed: whether the epoch is still [generation.SeededEpoch].
//
// A seat's or box's refresh moves no epoch and is a compare-and-swap of the
// WHOLE login, so a chain of them is the seed, refreshed. Every other writer
// moves it first ([Workspace.bumpLoginEpoch]), a bundle restore only when it
// REPLACES a login ([Provider.RestoreBundle]); another login command is
// refused meanwhile ([Workspace.lockLoginHome]). A record without a seeded
// epoch vouches for nothing: the shared copy is dropped, losing a file rather
// than mixing two sign-ins'.
func (w *Workspace) onlyRefreshedSince(gen generation) (bool, error) {
	if gen.SeededEpoch == nil {
		return false, nil
	}
	epoch, err := w.loginEpoch()
	if err != nil {
		return false, err
	}
	return epoch == *gen.SeededEpoch, nil
}

// orderPendingLogin records on a login command's open generation the epoch its
// login is ordered after ([generation.After]) now its own carry failed: a later
// command carries it out, landing only while no operator has written since.
//
// READ IN A HOLD OF ITS OWN of the shared lock, as the seed's is
// ([Workspace.seedFrom]): operators move the epoch FIRST, so an unlocked read
// could fall between epoch and write and order the login after a `-from-host`
// that lands later. A lock not taken, or an unreadable epoch, leaves the seed's
// epoch ([Workspace.seed]), erring towards the operator; only an unwritable
// record is an error.
func (w *Workspace) orderPendingLogin(ctx context.Context, record string, gen generation) error {
	if epoch, err := w.lockedLoginEpoch(ctx); err == nil {
		gen.After = &epoch
		return writeGeneration(record, gen)
	}
	return nil
}

// lockedLoginEpoch reads the operator epoch ([Workspace.loginEpoch]), taking the
// shared login's lock for that read alone.
func (w *Workspace) lockedLoginEpoch(ctx context.Context) (uint64, error) {
	unlock, err := w.lockShared(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return w.loginEpoch()
}

// superseded logs a changed login dropped because the shared one moved past
// it: WARN rather than debug, because it is the one trace that a refresh — or
// a token a seat's call minted, or a login a failed export left pending — went
// nowhere, and two of them in a row on one entry are worth somebody looking at.
// It names the file and never what either copy holds, and how the shared login
// moved: "changed" or "removed" since a refresh was seeded, or "replaced" by an
// operator since a pending login began.
func (w *Workspace) superseded(ctx context.Context, who, dst, how string) {
	log.WarnContext(ctx, "harness_login_superseded", "provider", who, "agent", w.harness,
		"path", dst, "shared", how)
}

// lockShared takes the shared login's lock for one compare-and-swap or read.
//
// ONE LOCK FOR EVERY TAKER: an flock ([hostbox.LockCredentials],
// [Workspace.sharedLock]) that seats, `crewlet llm` commands and a sandbox's
// teardown take for EVERY write (a compare-and-swap only holds against writers
// sharing it) and EVERY READ ([Workspace.Login]): a login is a set of files.
//
// NO IN-PROCESS MUTEX IN FRONT OF IT. An flock belongs to the open file
// description, so takers here exclude each other like processes, each waiting
// on its OWN context and [hostbox.CredentialLockWait]. A queued mutex waited on
// neither: the k-th seat behind a stopped command waited about k times as long.
func (w *Workspace) lockShared(ctx context.Context) (func(), error) {
	return hostbox.LockCredentials(ctx, w.CredentialsDir())
}

// seatLockWait bounds how long the first call into a seat waits for the seat's
// generation lock ([Workspace.lockSeat]).
//
// Held by somebody else only for a SWEEP's carry-out of this seat (one export,
// [auxiliaryTimeout]; one wait on the shared login's lock,
// [hostbox.CredentialLockWait]; one write of a few files each under
// [hostbox.MaxCredentialBytes], where a second is ample) or by ANOTHER PROCESS
// running this seat's generation, an engine that lost the lease and still
// finishes a call, which no affordable wait outlasts. So the bound is the
// sweep's, and the second case is refused as a timeout the fallback chain moves
// past rather than run in a home another harness is using.
const seatLockWait = auxiliaryTimeout + hostbox.CredentialLockWait + time.Second

// lockSeat takes a seat's GENERATION LOCK, and hands back its release.
//
// Held for a generation's whole life — from the first call in, which prunes and
// seeds, to the last call out, which carries the login back — by the
// [seatState] rather than by any one call. It is what lets anything else know
// the home is idle without asking this process: a sweep takes it without
// waiting ([Workspace.sweep]), and an flock is one per open file description, so
// a sweep in this process and one in another exclude a running generation
// alike, and a generation whose process died holds nothing.
func (w *Workspace) lockSeat(ctx context.Context, dir string) (func(), error) {
	return hostbox.LockFile(ctx, filepath.Join(dir, seatLockFile), seatLockWait, seatHolder)
}

// seatHolder is who keeps a seat's generation lock past [seatLockWait]: a
// sweep's carry-out is bounded by it, so only another process running a call in
// the seat's home.
const seatHolder = "another process running a call in this seat's home — an engine still " +
	"finishing a call after its lease on the seat moved"

// lockLoginHome takes the login home's lock for the whole of one auth command,
// or reports that another command holds it.
//
// Held from the leading prune to the trailing one: a second command's
// `RemoveAll` under a login awaiting its browser flow strands the credential it
// then stores. ONE PER WORKSPACE (state directory and harness), not per entry.
//
// NOT WAITED ON. The holder may be a person mid sign-in for minutes, and a
// silent `crewlet llm status` would read as wedged; refusing at once is the
// answer an operator can act on.
func (w *Workspace) lockLoginHome(ctx context.Context) (func(), error) {
	path := filepath.Join(w.root, loginHomeLockFile)
	release, err := hostbox.LockFile(ctx, path, 0, "another `crewlet llm` command")
	var busy *hostbox.LockWaitError
	if errors.As(err, &busy) {
		return nil, &loginHomeBusyError{agent: w.harness, root: w.root, lock: path}
	}
	return release, err
}

// loginHomeBusyError is the login home's lock held by another command.
type loginHomeBusyError struct{ agent, root, lock string }

func (e *loginHomeBusyError) Error() string {
	return fmt.Sprintf("harness: another `crewlet llm` command is using the %q login home "+
		"under %q (its lock %s is held) — wait for it to finish, then run this again",
		e.agent, e.root, e.lock)
}

// loginEpoch reads the shared login's OPERATOR EPOCH, zero where none has been
// written — see [Workspace.bumpLoginEpoch] — under the shared login's lock the
// caller holds: an operator's write moves the epoch first and writes second in
// one hold, so a read that does not exclude the hold can land between the two
// ([Workspace.orderPendingLogin]).
func (w *Workspace) loginEpoch() (uint64, error) {
	path := filepath.Join(w.root, loginEpochFile)
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the state dir
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("harness: reading %q: %w", path, err)
	}
	epoch, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("harness: %q does not hold an epoch this build can read — "+
			"remove it, and a login a failed export left pending is written over whatever "+
			"the shared directory holds: %w", path, err)
	}
	return epoch, nil
}

// bumpLoginEpoch moves the OPERATOR EPOCH on, under the shared login's lock the
// caller holds, BEFORE the write it counts.
//
// It counts operator writes that are not a login command's own (`-from-host`,
// an import, a logout, a bundle restore over a login), not a seat's refresh. An
// unmoved epoch proves no operator write landed, for a LATE write
// ([generation.After]) and a file a sign-in REMOVED
// ([Workspace.onlyRefreshedSince]).
//
// So it moves FIRST and DURABLY (file synced, then its directory): an epoch
// over a failed write errs towards the operator; a write with no epoch behind
// it would read as no write at all.
func (w *Workspace) bumpLoginEpoch() error {
	epoch, err := w.loginEpoch()
	if err != nil {
		return err
	}
	path := filepath.Join(w.root, loginEpochFile)
	if err := hostbox.ReplaceFile(path, strings.NewReader(strconv.FormatUint(epoch+1, 10)+"\n")); err != nil {
		return fmt.Errorf("harness: recording the login's epoch in %q: %w", path, err)
	}
	if err := hostbox.SyncDir(w.root); err != nil {
		return fmt.Errorf("harness: recording the login's epoch in %q: %w", path, err)
	}
	return nil
}

// markLoggedOut records that `crewlet llm logout` removed the login of record,
// under the shared login's lock the caller holds, so that the AUTOMATIC restore
// of a configured bundle ([Provider.RestoreBundle]) declines until a login is
// written again ([Workspace.clearLoggedOut]).
//
// A RECORD, because the empty directory a logout leaves is exactly what a
// bundle restores into: without it the next boot, apply or `crewlet llm status`
// would put back the login just revoked. IN THE WORKSPACE'S ROOT, outside
// credentials/, so no bundle or box carries it and a fresh state volume holds
// none, so a container rebuilt on every deploy still restores its bundle.
func (w *Workspace) markLoggedOut() error {
	path := filepath.Join(w.root, loggedOutFile)
	if err := hostbox.ReplaceFile(path, strings.NewReader(loggedOutNote)); err != nil {
		return fmt.Errorf("harness: recording the logout in %q: %w", path, err)
	}
	return nil
}

// loggedOutNote is what the logout's record says to an operator who finds it.
// Nothing reads it back: the record is its existence.
const loggedOutNote = "`crewlet llm logout` removed the login kept here; a configured " +
	"credential bundle is not restored into it until a login is written again\n"

// clearLoggedOut removes the logout's record ([Workspace.markLoggedOut]), under
// the shared login's lock the caller holds, as a login of record is written.
func (w *Workspace) clearLoggedOut() error {
	path := filepath.Join(w.root, loggedOutFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("harness: clearing the logout recorded in %q: %w", path, err)
	}
	return nil
}

// loggedOut reports whether the logout's record stands
// ([Workspace.markLoggedOut]).
func (w *Workspace) loggedOut() (bool, error) {
	path := filepath.Join(w.root, loggedOutFile)
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("harness: reading the logout's record %q: %w", path, err)
	}
}

// digestOf is the SHA-256 of content in hex, [hostbox.Digest]'s form.
func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// seedDigests is [Workspace.seedDigest] of every file of a login, by name — a
// [generation]'s baseline's form, so a login read now compares with the one a
// home was seeded from, on its login alone.
func (w *Workspace) seedDigests(login map[string][]byte) map[string]string {
	out := make(map[string]string, len(login))
	for name, content := range login {
		out[name] = w.seedDigest(name, content)
	}
	return out
}

// seedDigest is the digest of the login content holds, a copy of the file
// named name ([harnessprofile.FileRule.Login]): two copies whose login is one
// have one, whatever settings beside it differ. A copy that cannot be read for
// its login has "", no seed's.
func (w *Workspace) seedDigest(name string, content []byte) string {
	seed, err := w.profile.FileRule(name).Login(content)
	if err != nil {
		return ""
	}
	return digestOf(seed)
}

// writePrivate writes content at dst, 0600, creating its parent.
func writePrivate(dst string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("harness: preparing %q: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return fmt.Errorf("harness: writing %q: %w", dst, err)
	}
	return nil
}

// documentPath is where the shared directory keeps a
// [harnessprofile.CredentialStore]'s document.
func (w *Workspace) documentPath() string {
	return filepath.Join(w.CredentialsDir(), harnessprofile.CredentialStoreDocument)
}

// holdsLogin reports whether an exported document carries any login at all.
//
// The document's format is the harness's, so this reads only the one shape every
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

// loginNames is every file the shared login may hold, by its name in the
// shared directory: each credential file's, and a
// [harnessprofile.CredentialStore]'s document — which travels in a bundle, is
// removed by a logout and is written in one step with the rest exactly as a
// credential file is.
func (w *Workspace) loginNames() []string {
	names := make([]string, 0, len(w.profile.CredentialPaths)+1)
	for _, rel := range w.profile.CredentialPaths {
		names = append(names, filepath.Base(rel))
	}
	if w.profile.CredentialStore != nil {
		names = append(names, harnessprofile.CredentialStoreDocument)
	}
	return names
}

// sharedPath is where the shared directory keeps the login file name.
func (w *Workspace) sharedPath(name string) string { return filepath.Join(w.CredentialsDir(), name) }

// Login reads the shared login — ONE login, every file it holds by name — under
// the shared login's lock: see [Workspace.readLoginLocked]. A settings file
// holding no login is among them, as the harness keeps it; [Workspace.loginIn] is
// the part that signs anybody in.
func (w *Workspace) Login(ctx context.Context) (map[string][]byte, error) {
	unlock, err := w.lockShared(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return w.readLoginLocked()
}

// readLoginLocked reads the shared login under the shared login's lock the
// caller holds: every file it holds, by name, from ONE committed write.
//
// A LOGIN IS A SET OF FILES, and the shared directory is written a set at a
// time ([hostbox.WriteCredentials]): a write that stopped part way — a process
// killed between two files, a disk that refused the second — is finished
// before anything is read, so every reader answers with the login before that
// write or the one after it, and never with half of each. One that cannot be
// finished is an error, which a reader reports rather than reading the half
// it left as a login: the doctor and `crewlet llm list` say so, a seat is
// refused, and a restore declines nothing it cannot see.
func (w *Workspace) readLoginLocked() (map[string][]byte, error) {
	login, err := hostbox.ReadCredentials(w.CredentialsDir(), w.loginNames())
	if err != nil {
		return nil, fmt.Errorf("harness: reading the shared login in %s: %w", w.CredentialsDir(), err)
	}
	return login, nil
}

// writeLoginLocked makes the shared login EXACTLY login (every file written,
// every other removed) in ONE committed step, under the shared lock the caller
// holds ([hostbox.WriteCredentials]).
//
// A write that COMMITTED but could not be put in place at once is written: every
// reader puts it in place first ([Workspace.readLoginLocked]), so it is logged
// (`harness_login_unapplied`), not failed, and the caller's later steps follow.
//
// THE INSTALL RECORD IS LEFT AS IT IS ([LoginInstall]): this writes a refresh of
// the same sign-in. A write that installs or removes one is
// [Workspace.installLoginLocked].
func (w *Workspace) writeLoginLocked(ctx context.Context, who string, login map[string][]byte) error {
	return w.commitLoginLocked(ctx, who, w.loginNames(), login)
}

// commitLoginLocked is the one write of the shared directory: files over names,
// as [hostbox.WriteCredentials] makes it, a write that committed and could not
// be put in place at once logged and answered as written.
func (w *Workspace) commitLoginLocked(ctx context.Context, who string, names []string, files map[string][]byte) error {
	err := hostbox.WriteCredentials(w.CredentialsDir(), names, files, w.credentialSteps)
	if err != nil && hostbox.Committed(err) {
		log.WarnContext(ctx, "harness_login_unapplied", "provider", who, "agent", w.harness,
			"path", w.CredentialsDir(), "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("harness: writing the shared login in %s: %w", w.CredentialsDir(), err)
	}
	return nil
}

// HasLogin reports whether the shared login holds a login — any file of it
// that is one ([Workspace.loginIn]) — or an error where it cannot be read.
//
// ANY ONE FILE IS ENOUGH, deliberately: hermes keeps API keys in one file and
// an OAuth pool in the other and an entry may use either alone, and gemini-cli
// signs in without the account file beside its token. What makes one file a
// whole login rather than half of two is that the directory is written a login
// at a time, not a count of its files.
func (w *Workspace) HasLogin(ctx context.Context) (bool, error) {
	login, err := w.Login(ctx)
	if err != nil {
		return false, err
	}
	return len(w.loginIn(login)) > 0, nil
}

// loginIn is the part of files — the shared login as [Workspace.Login] reads
// it, by name — that is a login: every file but a settings file the harness also
// keeps its login in that holds none, by its profile's
// [harnessprofile.Profile.HoldsLogin]. Copilot writes `firstLaunchAt` into the
// config.json its plaintext login lives in on its first start, signed in or
// not, and Cursor Agent's cli-config.json is written on every run.
//
// Such a file is still the login's — read, seeded into a seat's home, carried
// out and written with the rest, as the harness keeps it. It is just not a login:
// not reported as one, exported in a bundle, removed by a logout, nor in a
// bundle's way. Counted as one, the doctor reported a sign-in nobody made, and
// a configured bundle — the real login — was refused as a login present.
func (w *Workspace) loginIn(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, content := range files {
		if w.profile.HoldsLogin(name, content) {
			out[name] = content
		}
	}
	return out
}
