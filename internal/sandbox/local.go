package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// localLog names the LOCAL backend. The package-neutral logger is in
// protocol.go; binding one var for the whole package stamped the remote
// backend and the shared runtime as "sandbox.local" too.
var localLog = logging.Get("sandbox.local")

// WorkspaceSubdir is where a box's checkout lives, relative to its home: the
// harness's working directory. Kept under the home so container mode
// needs exactly one bind mount.
const WorkspaceSubdir = "workspace"

// ContainerPrefix names every container this backend starts, so a stray box is
// identifiable — and greppable in `docker ps` — without consulting Crewlet.
const ContainerPrefix = "crewlet-sbx-"

// ControlTimeout bounds a CONTROL-PLANE command: mkdir, `docker exec`, the
// liveness probe, and a container box's start — its `docker run`, with the
// pull of an image the host does not hold yet inside it. Never the launched
// job itself, which is detached and deliberately uncapped. Sized for a
// container runtime under load, not for work.
//
// EXPORTED for the one reader outside this package: a scheduled turn's
// default cap (internal/org's DefaultScheduleTimeout) has to outlast what its
// turn spends before its run starts, the box's start among it, and a test in
// internal/engine holds the two in order.
const ControlTimeout = 120 * time.Second

// termGrace is how long a direct box's process group gets between SIGTERM and
// SIGKILL.
const termGrace = 5 * time.Second

// minReapAge floors the orphan reaper's cutoff regardless of the TTL it is
// handed. A spec with a tiny (or zero) timeout would otherwise make every box
// on the host instantly reapable, including ones another engine just created
// and has not yet heart-beaten.
const minReapAge = time.Minute

// BoxRootEnv overrides where local boxes are created.
const BoxRootEnv = "CREWLET_SANDBOX_LOCAL_HOME"

// LocalError reports a local box that could not be created, reached, or torn
// down.
type LocalError struct {
	msg string

	// class is what clears it, where the backend knows ([WhyOf]); empty
	// for a failure of the engine host's own — a directory it could not
	// create — which is the node's fault to report as one.
	class Why
}

func (e *LocalError) Error() string { return e.msg }

func (e *LocalError) why() (Why, bool) { return e.class, e.class != "" }

func localErrorf(format string, args ...any) error {
	return &LocalError{msg: fmt.Sprintf(format, args...)}
}

// localRefusedf is a [LocalError] the backend can class: a container runtime
// that will not start the configured image, or a mount it cannot share.
func localRefusedf(why Why, format string, args ...any) error {
	return &LocalError{msg: fmt.Sprintf(format, args...), class: why}
}

// BoxRoot is the directory local boxes are made under for a company whose
// `providers.sandbox.local.state_dir` is stateDir: the state_dir as written,
// else [DefaultBoxRoot].
//
// ONE RULE for every box the engine makes on its host: a seat's boxes, in
// either local cell ([NewLocal]), and the Dashboard Assistant's confined
// deep-work boxes, which the engine keeps in a directory of their own beneath
// it. The volume an operator gives this directory is the one sized for boxes;
// a box root derived from anything else (the store's directory, which it once
// was for deep work) puts a run's writes on a volume nobody sized for them.
func BoxRoot(stateDir string) string {
	if stateDir == "" {
		return DefaultBoxRoot()
	}
	return stateDir
}

// DefaultBoxRoot is the parent directory local boxes are created under when
// the company names no `providers.sandbox.local.state_dir` ([BoxRoot]).
func DefaultBoxRoot() string {
	if override := strings.TrimSpace(os.Getenv(BoxRootEnv)); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".crewlet", "sandboxes")
}

// ---------------------------------------------------------------------
// box layout
// ---------------------------------------------------------------------

// boxLayout is where one box's files live on the engine host: one directory,
// root, holding TWO SIDES that must never be one.
//
// HOME is the box's side — the harness's HOME, its checkout and its run
// files, and in container mode the only directory bind-mounted into the
// container, so everything under it is written by whatever runs in the box.
// RECORDS is the engine's side: what the engine itself must know about the box
// to manage it, kept beside the home rather than inside it. A box has to be
// SELF-DESCRIBING — Connect is handed nothing but an id, possibly in a
// different process from the one that created it, so anything a reconnect must
// know is on disk rather than in the object Create returned — and that record
// is only worth keeping if the box cannot rewrite it. They used to share the
// home's .crewlet/, where a containerised agent could replace the credential
// map with one naming any host file as the "shared login" to write a box file
// back over at teardown, the env file with a link the next exec's rewrite
// followed out of the box, and either with a named pipe whose open blocked the
// poll, the collection, the pause reaper and the orphan reaper for good.
//
// In direct mode the job runs as the engine's own user with no mount, so it
// can reach the records too; nothing here is a boundary there, which is what
// direct mode is documented to be.
type boxLayout struct {
	id   string
	root string
}

// home is the box's own side: its HOME, and its mount in container mode.
func (l boxLayout) home() string { return filepath.Join(l.root, "home") }

// records is the engine's side, out of the box's reach in container mode.
func (l boxLayout) records() string { return filepath.Join(l.root, "records") }

func (l boxLayout) workspace() string { return filepath.Join(l.home(), WorkspaceSubdir) }

// tmp is the box's own temporary directory: every temp root its processes
// are pointed at ([TempRootEnv]), inside the home so the box stays the one
// place a confined run may write.
func (l boxLayout) tmp() string { return filepath.Join(l.home(), ".tmp") }

// meta is the box's own .crewlet/, where its runner's markers, reports and
// bin directory live: the box's side, never a record.
func (l boxLayout) meta() string { return filepath.Join(l.home(), ".crewlet") }

// metaName is [boxLayout.meta] as a name in the home.
const metaName = ".crewlet"

// openHome is the box's home opened as an os.Root, and EVERY HOST-SIDE ACCESS
// TO A FILE THE BOX HOLDS — a read, a write, a directory made, a file removed
// — is made through one, by a name relative to it.
//
// THE HOME IS THE BOX'S TO WRITE. A container's job writes its whole home, the
// one directory mounted into it, and a confined direct box's job is given
// back its home and nothing else ([Local.confinedRules]) — and both run
// whatever a checkout's scripts say. So any directory in it may be a link by
// the time the engine reaches it, put there before the engine looked or in
// the instant after. A path checked and then opened by name was two walks of
// whatever the names held at each moment; the root walks the name once, a
// component at a time, follows a link only to somewhere beneath itself and
// refuses the rest. What leads outside the box is refused at the access,
// whenever it was put there.
//
// Opened per access rather than held by the box: a box value is made afresh
// by every reconnect and dropped without a teardown, and a held descriptor
// would outlive the directory a teardown removes.
func (l boxLayout) openHome() (*os.Root, error) { return os.OpenRoot(l.home()) }

// openRecords is the box's records opened as an os.Root, which every access to
// one of the engine's records about the box is made through, for the same
// reason as [boxLayout.openHome]: a direct box's job runs as the engine's own
// user, and nothing about where a record lies is taken on trust.
func (l boxLayout) openRecords() (*os.Root, error) { return os.OpenRoot(l.records()) }

// mkdirHome makes rel, and every directory above it, in the box's home,
// through its root.
func (l boxLayout) mkdirHome(rel string) error {
	home, err := l.openHome()
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	return home.MkdirAll(rel, hostbox.DirMode)
}

// pidFile records the detached job's process group so teardown can reach it
// even in a fresh engine that never held the handle.
//
// It holds a [procgroup.Leader], the pid AND its leader's start time, never a
// bare pid: the file is read back by a later process, possibly hours on, and a
// pid alone may by then lead a stranger's group. Signalling that would reach
// somebody else's processes, and probing it would keep a dead box alive.
func (l boxLayout) pidFile() string { return filepath.Join(l.records(), pidRecord) }

// aliveFile is the keepalive stamp — the local counterpart of a remote box's
// TTL.
//
// A directory's mtime does NOT move when files are written inside its
// subdirectories, so the box root's own timestamp is frozen at creation for
// the box's entire life. Anything reading it as "recently used" is reading a
// constant. This file is touched on every SetTimeout, which the waiter calls
// once per poll for exactly the box it is keeping alive.
func (l boxLayout) aliveFile() string { return filepath.Join(l.records(), aliveRecord) }

// credentialsFile records the logins this box was seeded with
// ([seededCredential]), so a reconnect knows which files to write back and
// against what. Without it a reconnected box closes with an empty map and the
// login the harness refreshed mid-run is discarded — and every production
// teardown goes through Connect or Kill, never the object Create returned, so
// that would be every run.
func (l boxLayout) credentialsFile() string { return filepath.Join(l.records(), credentialsRecord) }

// runFile records the run the box was made for ([boxRun]), which is how this
// host tells a box no run names any more from one a run is using ([HostBox]).
func (l boxLayout) runFile() string { return filepath.Join(l.records(), runRecord) }

// envFile is the container runtime's --env-file. See containerBox.envArgs.
func (l boxLayout) envFile() string { return filepath.Join(l.records(), envRecord) }

// witnessFile records the engine process that last saw the box's job running,
// which is how a job its engine's restart killed is told from one that ended
// ([jobEnd]).
func (l boxLayout) witnessFile() string { return filepath.Join(l.records(), witnessRecord) }

// The records' names in the box's records ([boxLayout.openRecords]), each the
// last element of the path its method above names.
const (
	pidRecord         = "box.pid"
	aliveRecord       = "alive"
	credentialsRecord = "credentials.json"
	runRecord         = "run.json"
	envRecord         = "env"
	witnessRecord     = "witness"
)

// seededCredential is one login file a box was seeded with, as the box's
// RECORD keeps it: where on the engine host it came from, and the SHA-256 of
// the very bytes the box was given — "" for a file of the login the shared
// directory did not hold when the box was seeded, which the box was given
// nothing for.
//
// THE SEED IS WHAT A WRITE-BACK IS JUDGED AGAINST. A box's teardown compared
// its copy with the shared login as it was AT TEARDOWN, and an idle box's copy
// differs from that the moment anybody else writes: a run that never refreshed
// put its stale seed back over a login the host had refreshed while it ran —
// OpenCode's export reproduces its import byte for byte, so every box seeded
// with its document did — and the vendor had already rotated that seed's
// refresh token away, logging every seat out at the next expiry.
//
// AN ABSENT FILE IS RECORDED TOO, because a login is the set of its files: a
// write-back is made only while the shared login is still the one the box was
// seeded from, and a file somebody added since is as much a newer login as a
// file somebody changed.
type seededCredential struct {
	Source string `json:"source"`
	Seed   string `json:"seed"`
}

// boxLogin is one login a box is seeded with: the files of one shared
// credential directory, by the path relative to the box's home each is seeded
// at, against its absolute path on the engine host.
type boxLogin struct {
	dir   string
	files map[string]string
}

// names is every file of the login, by its name in the shared directory.
func (b boxLogin) names() []string {
	names := make([]string, 0, len(b.files))
	for _, source := range b.files {
		names = append(names, filepath.Base(source))
	}
	slices.Sort(names)
	return names
}

// loginsOf groups a box's credential files into the LOGINS they are: one per
// shared credential directory, which is the unit every writer of that
// directory writes and every reader reads ([hostbox.WriteCredentials]). A
// profile's credential files all live in one directory, so a box has one —
// but a map naming two is two logins, each read and written whole.
func loginsOf(files map[string]string) []boxLogin {
	byDir := map[string]map[string]string{}
	for relative, source := range files {
		dir := filepath.Dir(source)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][relative] = source
	}
	out := make([]boxLogin, 0, len(byDir))
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		out = append(out, boxLogin{dir: dir, files: byDir[dir]})
	}
	return out
}

// seedCredentials copies the harness login into the box before the harness
// runs, and reports what it seeded the box with: every file of the login, by
// its path relative to the box's home, with the digest of what the box was
// given — and "" for a file the shared directory does not hold, which is not
// seeded, so nothing is ever written back over it.
//
// THE LOGIN IS READ WHOLE, under the shared directory's lock
// ([hostbox.ReadCredentials]): read a file at a time it could straddle a write
// — a box seeded with one sign-in's token beside another's config.toml — and a
// write that stopped part way is finished first rather than read as the half
// it left. And it is seeded WHOLE OR NOT AT ALL: a login the box cannot be
// given every file of is given none, and said, because a harness handed half a
// login fails inside the run in a way nothing outside it explains.
//
// The keys are relative paths from an operator-overridable profile, so they go
// through hostbox.SafeJoin: an entry like "../../etc/cron.d/x" would otherwise
// write a host file every time a box was created.
func seedCredentials(ctx context.Context, l boxLayout, files map[string]string) map[string]seededCredential {
	seeded := map[string]seededCredential{}
	for _, login := range loginsOf(files) {
		got, err := seedLogin(ctx, l, login)
		if err != nil {
			localLog.Warn("local_sandbox_credential_seed_failed",
				"sandbox_id", l.id, "dir", login.dir, "error", err.Error(),
				"detail", "the box was given none of this login, so its run is not signed in by it")
			continue
		}
		maps.Copy(seeded, got)
	}
	return seeded
}

// seedLogin seeds one login into a box — every file the shared directory
// holds of it, or none — and reports what it seeded.
func seedLogin(ctx context.Context, l boxLayout, login boxLogin) (map[string]seededCredential, error) {
	// VALIDATED BEFORE ANYTHING IS READ OR WRITTEN, so a login with one
	// path out of the box is seeded nowhere; WRITTEN through the box's home
	// ([boxLayout.openHome]), where no component of a path leads out of it.
	targets := map[string]string{}
	for relative := range login.files {
		if _, err := hostbox.SafeJoin(l.home(), relative); err != nil {
			return nil, fmt.Errorf("the credential path %q: %w", relative, err)
		}
		targets[relative] = filepath.Clean(relative)
	}
	names := login.names()
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		return nil, fmt.Errorf("two of its files share a name in %s, which keeps one file by each name",
			login.dir)
	}
	unlock, err := hostbox.LockCredentials(ctx, login.dir)
	if err != nil {
		return nil, err
	}
	shared, err := hostbox.ReadCredentials(login.dir, names)
	unlock()
	if err != nil {
		return nil, err
	}
	home, err := l.openHome()
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	seeded := map[string]seededCredential{}
	var written []string
	for _, relative := range slices.Sorted(maps.Keys(login.files)) {
		source := login.files[relative]
		content, present := shared[filepath.Base(source)]
		if !present {
			seeded[relative] = seededCredential{Source: source}
			continue
		}
		dst := targets[relative]
		if err := writeBoxFile(home, dst, content); err != nil {
			for _, name := range written {
				_ = home.Remove(name)
			}
			return nil, fmt.Errorf("seeding %q: %w", relative, err)
		}
		written = append(written, dst)
		// THE SEED'S DIGEST IS OF THE BYTES THE BOX WAS GIVEN: read separately
		// it could describe a newer file than the box's, and the teardown
		// would then excuse a write it should have refused.
		seeded[relative] = seededCredential{Source: source, Seed: digestOf(content)}
	}
	return seeded, nil
}

// writeBoxFile puts one seeded login file at name in a box's home, its parents
// created, as every file of a box is written: replaced whole, every step
// through the home's root ([replaceIn]).
func writeBoxFile(home *os.Root, name string, content []byte) error {
	if err := home.MkdirAll(filepath.Dir(name), hostbox.DirMode); err != nil {
		return err
	}
	return replaceIn(home, name, content)
}

// digestOf is the SHA-256 of content in [hostbox.Digest]'s form.
func digestOf(content []byte) string {
	digest, _ := hostbox.Digest(bytes.NewReader(content)) // a bytes.Reader does not fail
	return digest
}

// collectCredentials writes a login the run refreshed back to the shared
// directory — a COMPARE-AND-SWAP against what the box was seeded with
// ([writeBackLogin]), one login at a time.
//
// OAuth access tokens expire in hours and most vendors rotate the refresh
// token alongside them, so a box that quietly discards the rewritten file logs
// the whole fleet out at the next expiry — and one that writes back a login it
// did not refresh over one the host did logs the fleet out just the same.
//
// Read through SafeJoin for the same reason the seed is written through it: a
// "../../" key would otherwise copy an arbitrary host file out of the box's
// directory and over the credential store. The map itself is the box's RECORD
// — written by the engine at Create, out of the box's reach — so where a login
// is written back to is the operator's profile and never the box's say.
//
// A TEARDOWN, so it takes [context.WithoutCancel] of its caller's context: the
// teardown is often the cancellation's own consequence, and the only wait in
// it — another writer's lock — has its own bound ([hostbox.CredentialLockWait]).
func collectCredentials(ctx context.Context, l boxLayout, seeded map[string]seededCredential) {
	ctx = context.WithoutCancel(ctx)
	files := map[string]string{}
	for relative, rec := range seeded {
		files[relative] = rec.Source
	}
	for _, login := range loginsOf(files) {
		got, err := writeBackLogin(ctx, l, login, seeded, hostbox.WriteCredentials)
		if err != nil {
			localLog.Warn("local_sandbox_credential_writeback_failed",
				"sandbox_id", l.id, "dir", login.dir, "error", err.Error())
			continue
		}
		for _, relative := range slices.Sorted(maps.Keys(got)) {
			switch outcome := got[relative]; outcome {
			case writeBackWritten:
				localLog.Info("local_sandbox_credential_refreshed", "sandbox_id", l.id, "file", relative)
			case writeBackChanged, writeBackRemoved:
				// WARN, because it is the one trace that a refresh the run
				// made went nowhere; two in a row on one login are worth
				// somebody looking at. It names the file, never what either
				// copy holds.
				localLog.Warn("local_sandbox_credential_superseded",
					"sandbox_id", l.id, "file", relative, "path", seeded[relative].Source,
					"shared", string(outcome), "detail", supersededDetail[outcome])
			}
		}
	}
}

// writeBack is what a teardown did with one login file.
type writeBack string

// supersededDetail is what local_sandbox_credential_superseded says of each
// outcome in which a login the run refreshed was not written back: one
// sentence apiece, because the two leave different things behind — a newer
// login kept, and no login at all.
var supersededDetail = map[writeBack]string{
	writeBackChanged: "the run refreshed this login, but the shared one changed after the box " +
		"was seeded — another seat's refresh, or a new crewlet llm login — so the shared one " +
		"was kept, being the newer",
	writeBackRemoved: "the run refreshed this login, but the shared one was removed after the " +
		"box was seeded — crewlet llm logout — and a box never creates one, so none was written",
}

const (
	// writeBackIdle: the box holds what it was seeded with, or nothing —
	// the run refreshed nothing, so there is nothing to write.
	writeBackIdle writeBack = "idle"
	// writeBackWritten: the box's refreshed login replaced the shared one.
	writeBackWritten writeBack = "written"
	// writeBackHeld: the shared login already holds what the box does.
	writeBackHeld writeBack = "held"
	// writeBackChanged: the shared login moved past the seed while the box
	// ran — another seat's refresh, a new `crewlet llm login` — so it is
	// newer than anything the box has to say, and was kept.
	writeBackChanged writeBack = "changed"
	// writeBackRemoved: the shared login was removed while the box ran —
	// `crewlet llm logout` — and a box never creates one.
	writeBackRemoved writeBack = "removed"
)

// writeBackLogin writes the files of one login the run CHANGED back over the
// shared login, but only while the shared login is STILL, as a whole, the one
// the box was seeded with — and reports what became of each file the run
// changed.
//
// TWO CONDITIONS, and each is a different lost login. A box whose copy still
// holds its seed refreshed nothing, and writing it back would revert whatever
// the host wrote meanwhile. A shared login that no longer holds the seed —
// any file of it, or a file it did not hold — was rewritten by somebody else:
// a refresh of its own, which rotated the refresh token the box's copy
// descends from, or a new login an operator made — and the box's copy is the
// older of the two. Judged a file at a time, a refresh of one file was written
// beside another writer's newer other file, a login no harness wrote. So the
// comparison and the write are one step, under the shared directory's lock
// ([hostbox.LockCredentials]), which every writer of it takes, and the write
// is ONE WRITE of the whole login ([hostbox.WriteCredentials]), so a teardown
// killed part way never leaves half of it.
//
// A file the box was not seeded with is never written: a box never creates a
// login, nor a file of one.
//
// The write goes through commit, which is [hostbox.WriteCredentials] in every
// box: see [loginCommit].
func writeBackLogin(
	ctx context.Context, l boxLayout, login boxLogin, seeded map[string]seededCredential, commit loginCommit,
) (map[string]writeBack, error) {
	got := map[string]writeBack{}
	changed := map[string][]byte{}
	byName := map[string]string{}
	home, err := l.openHome()
	switch {
	case absent(err):
		// A HOME THAT IS GONE holds no refresh, as a file that is gone
		// holds none: there is nothing to write back.
		return got, nil
	case err != nil:
		return nil, err
	}
	defer func() { _ = home.Close() }()
	for _, relative := range slices.Sorted(maps.Keys(login.files)) {
		rec := seeded[relative]
		name := filepath.Base(rec.Source)
		byName[name] = relative
		if rec.Seed == "" {
			continue
		}
		// READ THROUGH THE HOME'S ROOT, after the run that had the home to
		// itself: whatever it made of the directories on the way, the read
		// stays in the box ([boxLayout.openHome]). A refusal is said as the
		// escape it is when the path, as it stands, leads out.
		content, present, readErr := readBoxFile(home, filepath.Clean(relative))
		if readErr != nil {
			if _, check := hostbox.SafeJoin(l.home(), relative); check != nil {
				return nil, fmt.Errorf("the credential path %q: %w", relative, check)
			}
			return nil, readErr
		}
		if !present || digestOf(content) == rec.Seed {
			got[relative] = writeBackIdle
			continue
		}
		changed[name] = content
	}
	if len(changed) == 0 {
		return got, nil
	}
	verdict := func(outcome writeBack) map[string]writeBack {
		for name := range changed {
			got[byName[name]] = outcome
		}
		return got
	}
	unlock, err := hostbox.LockCredentials(ctx, login.dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	names := login.names()
	current, err := hostbox.ReadCredentials(login.dir, names)
	if err != nil {
		return nil, fmt.Errorf("reading the shared login: %w", err)
	}
	next := maps.Clone(current)
	maps.Copy(next, changed)
	if maps.EqualFunc(next, current, bytes.Equal) {
		return verdict(writeBackHeld), nil
	}
	for relative, rec := range login.files {
		shared, present := current[filepath.Base(rec)]
		seed := seeded[relative].Seed
		if (seed == "") == present || (present && digestOf(shared) != seed) {
			if len(current) == 0 {
				return verdict(writeBackRemoved), nil
			}
			return verdict(writeBackChanged), nil
		}
	}
	err = commit(login.dir, names, next)
	if !hostbox.Committed(err) {
		return nil, err
	}
	if err != nil {
		// THE REFRESH IS THE LOGIN OF RECORD — its journal landed, and every
		// reader puts it in place before it reads — so it was written, and
		// the step that could not be finished now is said.
		localLog.Warn("local_sandbox_credential_unapplied", "sandbox_id", l.id, "dir", login.dir,
			"error", err.Error())
	}
	return verdict(writeBackWritten), nil
}

// loginCommit is how a write-back puts a whole login in the shared directory:
// [hostbox.WriteCredentials], in every box.
//
// A PARAMETER rather than a call written in, for the one failure after which
// the refresh HAS been written — its journal landed, and a file the disk then
// refused is put in place by the next reader — which a write-back has to report
// as written. Nothing a test can leave in the shared directory beforehand
// reaches that failure: the write-back reads the login under the same hold of
// the lock first, and the read refuses whatever would stop the write. So a test
// hands in a commit that puts the obstacle down between the two.
type loginCommit func(dir string, names []string, login map[string][]byte) error

// readBoxFile reads one login file a box holds at name in its home, reporting
// false where it holds none.
//
// OPENED ONCE, as a box's files are read ([openRegularIn]): a file checked by
// its path and then opened by it again is two reads of whatever the path names
// at each moment, and the box's side is the one that names it. So a pipe there
// answers at once as what it is rather than blocking a teardown, a link is
// refused rather than followed, no directory on the way leads out of the box,
// and what is written back is the file that was read. Larger than
// [hostbox.MaxCredentialBytes], it is refused: the shared login would not hold
// it, and a box is not handed the engine's memory.
func readBoxFile(home *os.Root, name string) ([]byte, bool, error) {
	src := filepath.Join(home.Name(), name)
	in, err := openRegularIn(home, name)
	switch {
	case absent(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	defer func() { _ = in.Close() }()
	content, err := io.ReadAll(io.LimitReader(in, hostbox.MaxCredentialBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", src, err)
	}
	if len(content) > hostbox.MaxCredentialBytes {
		return nil, false, fmt.Errorf("%s is larger than %d bytes, which is not a login",
			src, hostbox.MaxCredentialBytes)
	}
	return content, true, nil
}

// recordCredentialMap persists what the box was seeded with so Connect can
// write a refreshed login back.
//
// It RETURNS its failure rather than logging it, even though the caller only
// logs: the previous shape wrote the WriteFile error into an `if err :=`
// binding that shadowed the one being checked, so the warning this function
// exists to emit could never fire and no test could have caught it. A
// returned error is the shape a test can assert on.
func recordCredentialMap(l boxLayout, seeded map[string]seededCredential) error {
	if len(seeded) == 0 {
		return nil
	}
	blob, err := json.Marshal(seeded)
	if err != nil {
		return fmt.Errorf("encoding the credential map: %w", err)
	}
	if err := os.MkdirAll(l.records(), hostbox.DirMode); err != nil {
		return fmt.Errorf("preparing %q: %w", l.records(), err)
	}
	if err := writeRecord(l, credentialsRecord, blob); err != nil {
		return fmt.Errorf("writing %q: %w", l.credentialsFile(), err)
	}
	return nil
}

// readCredentialMap is what the box was seeded with, as recorded at Create:
// nil when nothing was.
//
// A RECORD THAT CANNOT BE USED IS SAID, and read as no map: every reconnect
// reads it before it answers — the poll's, the collection's, the pause
// reaper's and the orphan reaper's — and none of them may fail over a list of
// logins to write back, so what is lost is the write-back of a refreshed
// login, which the warning names.
func readCredentialMap(l boxLayout) map[string]seededCredential {
	blob, err := readRecord(l, credentialsRecord)
	if absent(err) {
		return nil
	}
	if err == nil {
		var seeded map[string]seededCredential
		if err = json.Unmarshal(blob, &seeded); err == nil {
			return seeded
		}
	}
	localLog.Warn("local_sandbox_credential_map_unreadable", "sandbox_id", l.id,
		"path", l.credentialsFile(), "error", err.Error(),
		"detail", "the box's record of the logins it was seeded with could not be used, so a "+
			"login the run refreshed is not written back to the shared credential directory")
	return nil
}

// maxRecordBytes bounds one read of a box's record. The largest the engine
// writes is the credential map — a harness profile's few credential files, each a
// relative path, a host path and a 64-character digest — which is a few
// hundred bytes; a mebibyte is over a hundred and twenty entries at the
// longest paths a filesystem takes, so a record past it is not one this
// engine wrote and is not read whole into memory to find that out.
const maxRecordBytes = 1 << 20

// readRecord reads the engine's record called name about a box, the way every
// file under a box's directory is read on the engine host: through the root
// it lies under ([boxLayout.openRecords]), and only a REGULAR FILE WHERE IT
// LIES ([openRegularIn]), so a record that is a named pipe answers at once as
// what it is rather than blocking whoever asked, and a link is refused rather
// than followed. An absent record answers [fs.ErrNotExist].
func readRecord(l boxLayout, name string) ([]byte, error) {
	path := filepath.Join(l.records(), name)
	records, err := l.openRecords()
	if err != nil {
		return nil, err
	}
	defer func() { _ = records.Close() }()
	f, err := openRegularIn(records, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: path, Err: err}
	}
	if len(raw) > maxRecordBytes {
		return nil, fmt.Errorf("%s is past %d KiB, which no record this engine writes is, "+
			"so it was not read", path, maxRecordBytes>>10)
	}
	return raw, nil
}

// touchAlive refreshes the box's keepalive stamp, through the box's records
// ([boxLayout.openRecords]). Never fatal.
func touchAlive(l boxLayout) {
	if err := os.MkdirAll(l.records(), hostbox.DirMode); err != nil {
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
		return
	}
	records, err := l.openRecords()
	if err != nil {
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
		return
	}
	defer func() { _ = records.Close() }()
	now := time.Now()
	if records.Chtimes(aliveRecord, now, now) == nil {
		return
	}
	// The stamp is made, empty, where there is none yet: a keepalive that
	// did not land is a box the reaper will treat as an orphan, so the
	// failure is worth the same line as the records' open.
	if err := replaceIn(records, aliveRecord, nil); err != nil {
		localLog.Debug("local_sandbox_keepalive_unwritable", "sandbox_id", l.id, "error", err.Error())
	}
}

// recordLeader writes the detached job's group identity into the box's
// records.
func recordLeader(l boxLayout, leader procgroup.Leader) error {
	if err := os.MkdirAll(l.records(), hostbox.DirMode); err != nil {
		return err
	}
	return writeRecord(l, pidRecord, []byte(leader.String()))
}

// writeRecord puts blob at the engine's record called name about a box,
// through the box's records ([boxLayout.openRecords]): replaced whole, and
// only over a regular file where it lies ([replaceIn]), so a reader — another
// goroutine's poll, the next engine after a restart, the runtime's CLI reading
// the env file — finds the record before the write or after it, never half of
// one. The records directory must exist.
func writeRecord(l boxLayout, name string, blob []byte) error {
	records, err := l.openRecords()
	if err != nil {
		return err
	}
	defer func() { _ = records.Close() }()
	return replaceIn(records, name, blob)
}

// jobGroup is the box's recorded job group when that group is still the job,
// and false when there is none to act on.
//
// THREE ANSWERS, and the caller decides which way the third fails. False with
// a nil error is definitive: no job was recorded, the record is not one this
// engine wrote, or the group it names has ended or now belongs to a later
// process. An error means the kernel could not be asked, which neither proves
// the job alive nor licenses a signal: the orphan reaper keeps the directory on
// it, and every signalling path withholds its signal.
//
// A box with no record started no job. A record that exists and cannot be
// read — something other than a regular file at its path included, read the
// way every record is ([readRecord]) — is the unknown answer, like a kernel
// record that cannot be read.
//
// A record that does not parse is logged and read as no job. Only a direct
// box has one, and its job runs as the engine's own user beside its records,
// so the file's content is input rather than a fact; and a bare pid written
// there is exactly the identity with no start time [procgroup.ParseLeader]
// refuses.
func jobGroup(l boxLayout) (procgroup.Leader, bool, error) {
	leader, found, err := readJobRecord(l)
	if err != nil || !found {
		return leader, false, err
	}
	current, err := leader.Current()
	if err != nil {
		return leader, false, err
	}
	return leader, current, nil
}

// readJobRecord is the job identity a box recorded, false when it recorded
// none it can use. See [jobGroup] for which failures are which answer.
func readJobRecord(l boxLayout) (procgroup.Leader, bool, error) {
	raw, err := readRecord(l, pidRecord)
	if absent(err) {
		return procgroup.Leader{}, false, nil
	}
	if err != nil {
		return procgroup.Leader{}, false, fmt.Errorf("reading the job record %s: %w", l.pidFile(), err)
	}
	leader, err := procgroup.ParseLeader(strings.TrimSpace(string(raw)))
	if err != nil {
		localLog.Warn("local_sandbox_job_record_unreadable", "sandbox_id", l.id,
			"path", l.pidFile(), "error", err.Error(),
			"detail", "the box's job record is not a process group identity, so no signal "+
				"is sent through it and the box counts as having no running job")
		return procgroup.Leader{}, false, nil
	}
	return leader, true, nil
}

// signalJob sends one signal to the box's job group, but only to a group that
// is still the job.
//
// Every signal a box sends goes through here, so none of them can reach a
// recycled pid. Reports whether the group was current, which is what tells a
// teardown whether there is anything to wait for.
func signalJob(l boxLayout, what string, send func(pid int) error) bool {
	leader, current, err := jobGroup(l)
	if err != nil {
		localLog.Warn("local_sandbox_signal_withheld", "sandbox_id", l.id, "signal", what,
			"pgid", leader.PID, "error", err.Error(),
			"detail", "the kernel's record of the job's process group could not be read, so "+
				"the signal is not sent: it may no longer be this box's job")
		return false
	}
	if !current {
		return false
	}
	logSignal(what, leader.PID, send(leader.PID))
	return true
}

// ---------------------------------------------------------------------
// provider
// ---------------------------------------------------------------------

// LocalOptions configures a [Local].
type LocalOptions struct {
	// Placement is Direct (default) or Container. It is the ONE thing that
	// differs between the two local cells, which is why one backend serves
	// both: everything else (the box root, the reap, the process group,
	// the box's bin directory) is identical.
	Placement Placement

	// StateDir is where boxes are created: [BoxRoot] of it.
	StateDir string

	// Image is the container image. REQUIRED for Container, and it must have
	// the harness installed: there is no sensible default, and a box
	// without the harness fails only once an agent tries to use it.
	Image string

	// Runtime is "auto" (default; docker then podman), "docker" or "podman".
	Runtime string

	// Network is passed to the runtime's --network.
	Network string

	// RunArgs are extra arguments spliced into the container run.
	RunArgs []string

	// RuntimeEnv is what the operator DECLARED for the container runtime's
	// own harness, its references already resolved — laid over the fixed rule
	// every runtime call carries ([hostbox.ContainerRuntime]), for what that
	// rule cannot know a runtime's helpers read: the cloud role, profile or
	// key file a registry credential helper pulls the image with.
	// It never reaches a box: that is `role.sandbox.env`.
	RuntimeEnv map[string]string

	// Confine runs every command a Direct box's job starts inside Host's
	// boundary, kept from Confine's paths, every other box and the box's
	// own records (see confine.go). Nil is the unconfined Direct cell a
	// seat's code work runs in. Both or neither: a confinement with no
	// mechanism to enforce it is refused at construction, never run open.
	Confine *Confinement
	Host    *HostConfinement
}

// Local mints sandboxes on the engine host, directly or in a container.
//
// The flagship backend: it exists so a role can do real code work with the
// harness the operator already logged in to — no account, no API key, no
// token minting. The run reads the same credential directory `crewlet llm
// login` wrote.
type Local struct {
	opts LocalOptions
	root string
	cli  containerCLI // the resolved runtime; its path is "" in direct mode
}

var _ Provider = (*Local)(nil)

// NewLocal validates the options and resolves the container runtime once,
// rather than on every Create.
func NewLocal(opts LocalOptions) (*Local, error) {
	if opts.Placement == "" {
		opts.Placement = Direct
	}
	if opts.Placement != Direct && opts.Placement != Container {
		return nil, localErrorf("the local backend runs %q or %q, not %q — a "+
			"remote placement is another backend's",
			Direct, Container, opts.Placement)
	}
	// Refused at CONSTRUCTION, not at the first Create: an operator whose
	// platform cannot run this backend should be told when the config is
	// applied, not when an agent first tries to do code work.
	if !localSupported {
		return nil, localErrorf("%s", unsupportedReason)
	}
	switch {
	case opts.Confine != nil && opts.Placement != Direct:
		return nil, localErrorf("a confined local box is a %q box: %q runs in its own "+
			"container already", Direct, opts.Placement)
	case (opts.Confine == nil) != (opts.Host == nil):
		return nil, localErrorf("a confined local box needs both what it is kept from and " +
			"the mechanism that keeps it; one was given without the other")
	}
	root := BoxRoot(opts.StateDir)
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, localErrorf("providers.sandbox.local.state_dir %q is not a usable path: %v", root, err)
	}
	local := &Local{opts: opts, root: abs}
	if opts.Placement == Container {
		if strings.TrimSpace(opts.Image) == "" {
			return nil, localErrorf("run_in %q requires providers.sandbox.local.image, an image "+
				"that has the harness installed — there is no sensible default, and a "+
				"box without the harness fails only once an agent tries to use it", Container)
		}
		var path string
		if path, err = ResolveContainerRuntime(opts.Runtime); err != nil {
			return nil, err
		}
		// CLONED, so the options map a caller built — and may reuse for the
		// next apply — is never the one every runtime call reads.
		local.cli = containerCLI{path: path, declared: maps.Clone(opts.RuntimeEnv)}
	}
	return local, nil
}

// Kind names the backend, for logs and the operator surface.
func (l *Local) Kind() string { return "local" }

// layout is where one box's files live.
//
// AN EMPTY ID IS REFUSED, and this is not defensive tidiness: joining "" gives
// the boxes DIRECTORY ITSELF, so an empty id produces a "box" whose root is
// the parent of every box on the host — one whose Close or Kill would
// os.RemoveAll every other box's checkout. It is reachable: a run's row exists
// before its box is attached, so a poll landing in that window asks to connect
// to "".
func (l *Local) layout(id string) (boxLayout, error) {
	if strings.TrimSpace(id) == "" {
		return boxLayout{}, localErrorf("local sandbox: an empty sandbox id names no box " +
			"(it would resolve to the directory holding every box)")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		// Ids are minted here and never come from config, but the same
		// join is what a traversal would exploit, and the check is one
		// comparison on a path that is about to be deleted from.
		return boxLayout{}, localErrorf("local sandbox: %q is not a usable box id", id)
	}
	return boxLayout{id: id, root: filepath.Join(l.root, "boxes", id)}, nil
}

func (l *Local) containerName(id string) string { return ContainerPrefix + id }

// confinedRules are one box's confinement, nil for an unconfined backend.
//
// EVERY BOX ON THIS BACKEND IS DENIED, its own records included, and only its
// home is given back: one person's run must never read another's checkout,
// and the records hold the box's credential map and its job's identity, which
// are the engine's side of the box.
func (l *Local) confinedRules(layout boxLayout) (*boxConfine, error) {
	if l.opts.Confine == nil {
		return nil, nil
	}
	deny, err := resolveAll(append(slices.Clone(l.opts.Confine.Deny), l.root))
	if err != nil {
		return nil, localErrorf("local sandbox %s cannot be confined: %v", layout.id, err)
	}
	read, err := resolveAll(l.opts.Confine.Read)
	if err != nil {
		return nil, localErrorf("local sandbox %s cannot be confined: %v", layout.id, err)
	}
	home, err := resolvePath(layout.home())
	if err != nil {
		return nil, localErrorf("local sandbox %s cannot be confined: %v", layout.id, err)
	}
	return &boxConfine{host: l.opts.Host, rules: confineRules{deny: deny, read: read, write: home}}, nil
}

// Create mints a box and, in container mode, starts the container behind it.
func (l *Local) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	id := uuid.NewString()
	id = strings.ReplaceAll(id, "-", "")[:16]
	layout, err := l.layout(id)
	if err != nil {
		return nil, err
	}

	// THE BOX'S OWN DIRECTORIES BY PATH — its root, its home and its
	// records are the roots every later access is made through — and what
	// is IN the home through the home ([boxLayout.mkdirHome]).
	if err = os.MkdirAll(layout.home(), hostbox.DirMode); err != nil {
		return nil, localErrorf("could not create local sandbox %s at %s: %v", id, layout.root, err)
	}
	if err = layout.mkdirHome(WorkspaceSubdir); err != nil {
		return nil, localErrorf("could not create local sandbox %s at %s: %v", id, layout.root, err)
	}
	// MkdirAll honours umask, so the mode is asserted rather than requested:
	// a box holds a credential and must not be group- or world-readable on a
	// host that runs other services.
	if err = os.Chmod(layout.root, hostbox.DirMode); err != nil {
		return nil, localErrorf("could not secure local sandbox %s at %s: %v", id, layout.root, err)
	}
	if err = layout.mkdirHome(filepath.Base(layout.tmp())); err != nil {
		return nil, localErrorf("could not create local sandbox %s tmpdir: %v", id, err)
	}
	if err = os.MkdirAll(layout.records(), hostbox.DirMode); err != nil {
		return nil, localErrorf("could not create local sandbox %s records at %s: %v",
			id, layout.records(), err)
	}

	l.reapOrphans(ctx, time.Duration(spec.TimeoutSec)*time.Second)
	seeded := seedCredentials(ctx, layout, spec.CredentialFiles)
	// The box records what it was seeded with and stamps itself alive before
	// anyone can reap it.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := recordCredentialMap(layout, seeded); err != nil {
		// The map names paths and digests, not secrets, but losing it means
		// a refreshed login is never written back to the shared directory —
		// worth saying out loud, and not worth failing the box for.
		localLog.Warn("local_sandbox_credential_map_unwritable", "sandbox_id", layout.id, "error", err.Error())
	}
	if err = recordRun(layout, boxRun{TurnID: spec.TurnID, Placement: l.opts.Placement}); err != nil {
		// LOST, the box is one its host's waiter cannot tell is still a
		// run's, so it is never reclaimed by it — only, once nothing runs
		// in it, by the orphan reaper. Worth saying; not worth the run.
		localLog.Warn("local_sandbox_run_unrecorded", "sandbox_id", layout.id, "error", err.Error())
	}
	touchAlive(layout)

	if l.opts.Placement == Direct {
		localLog.Info("local_sandbox_created", "sandbox_id", id, "placement", string(Direct), "home", layout.home())
		confine, confineErr := l.confinedRules(layout)
		if confineErr != nil {
			_ = os.RemoveAll(layout.root)
			return nil, confineErr
		}
		return &directBox{layout: layout, env: spec.Env, credentials: seeded, confine: confine}, nil
	}

	name := l.containerName(id)
	result, err := runHost(ctx, l.cli.command(0, l.runArgv(ctx, layout, name)...))
	if err != nil || result.ExitCode != 0 {
		_ = os.RemoveAll(layout.root)
		if cancelledBy(ctx) {
			// THIS NODE CANCELLED THE LAUNCH, which says nothing about the
			// image: classed as one, a drain was told as a setup to fix.
			return nil, fmt.Errorf("sandbox: starting container sandbox %s was "+
				"abandoned: %w", id, ctx.Err())
		}
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		if err != nil {
			detail = err.Error()
		}
		// THE WHOLE DETAIL. This is the container runtime's own account of
		// why the image would not start — a missing platform, a pull
		// failure, a mount that does not exist — and the operator reading
		// it has no other copy. A 400-byte cut removed the second half of
		// exactly the messages worth reading.
		//
		// CLASSED AS CONFIGURATION, which every one of those is — the image,
		// the runtime, its daemon, the flags — and which a pull failing on a
		// network blip is not quite: telling which from the runtime's words
		// is the reading of text this package does not do, and a person
		// looking at the host is what both need when it does not clear.
		return nil, localRefusedf(WhyConfiguration,
			"could not start sandbox container from image %q: %s", l.opts.Image, detail)
	}
	box := &containerBox{
		layout: layout, cli: l.cli, container: name,
		env: spec.Env, credentials: seeded,
	}
	// PROVEN BEFORE THE BOX IS HANDED OUT. Both properties this mode rests
	// on fail silently and only much later — see verifyMount — so the box is
	// torn down here rather than returned to do half its job.
	if err := verifyMount(ctx, box, layout); err != nil {
		_, _ = runHost(context.WithoutCancel(ctx), l.cli.command(0, "rm", "-f", name))
		_ = os.RemoveAll(layout.root)
		return nil, err
	}
	localLog.Info("local_sandbox_created",
		"sandbox_id", id, "placement", string(Container), "image", l.opts.Image, "container", name)
	return box, nil
}

// runArgv is the container-run command line for one box, after the binary.
//
// Separated from Create so the argument order is assertable without a running
// daemon: what an operator's own run_args may override is a function of where
// they are spliced, and nothing else says it.
func (l *Local) runArgv(ctx context.Context, layout boxLayout, name string) []string {
	argv := []string{
		"run", "-d",
		"--name", name,
		"-v", layout.home() + ":" + DefaultHome,
		"-w", DefaultHome + "/" + WorkspaceSubdir,
		"-e", "HOME=" + DefaultHome,
		// A reaping PID 1. A launch's detached job is backgrounded inside an
		// `exec`, so without an init that reaps it, a finished job lingers as
		// a zombie — which `kill -0` reports as ALIVE, hanging the runner's
		// completion check on a job that already died.
		"--init",
	}
	argv = append(argv, containerUserArgs(ctx, l.cli)...)
	// The engine host by name, for the addresses the engine hands the box
	// in its own network ([EngineAddress]).
	argv = append(argv, l.cli.hostAliasArgs()...)
	if l.opts.Network != "" {
		argv = append(argv, "--network", l.opts.Network)
	}
	// AHEAD OF THE ENGINE FOR THE KERNEL'S OOM KILLER ([boxOOMScore]), for
	// every process in the box and not only its first: both runtimes start an
	// `exec` from the container's own process specification, the value
	// included (Docker's daemon/exec.go, Podman's
	// libpod/oci_conmon_exec_common.go), so the launched job and every setup
	// command take it too. Neither raises it unasked: Docker runs a container
	// at 0, and Podman at its own harness's value, which is the engine's. A
	// container on the engine's own kernel competes with the engine for the
	// host's memory; one in a VM of its own (Docker Desktop) is merely ordered
	// within it. Before run_args, so an operator's own --oom-score-adj wins.
	if score, ok := boxOOMScore(); ok {
		argv = append(argv, "--oom-score-adj", strconv.Itoa(score))
	}
	// LAST, so an operator's own flags win. Both runtimes take the final
	// occurrence of a repeated flag, which makes run_args the escape hatch
	// for an image that genuinely needs its own user (`--user 0:0`) without
	// a config knob whose only job is to undo the line above.
	argv = append(argv, l.opts.RunArgs...)
	// PID 1 exists only to hold the namespace open; every command is an
	// `exec` into it, which is what makes a launch's detached job survive the
	// turn that started it.
	return append(argv, l.opts.Image, "sleep", "infinity")
}

// Connect reattaches to an existing box by id, live or paused.
//
// It AUTO-RESUMES, matching a remote provider: a paused box the coordinator
// reattaches to must be runnable immediately. That is exactly why the pause
// reaper must use Kill instead.
func (l *Local) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	box, err := l.reattach(sandboxID)
	if err != nil {
		return nil, err
	}
	switch b := box.(type) {
	case *directBox:
		b.resume()
	case *containerBox:
		b.unpause(ctx)
	}
	return box, nil
}

// Attach implements [Provider]: the box as it is, never resumed.
//
// A DIRECT BOX IS READ WHILE STOPPED. Its files are the engine host's own and
// its liveness probe asks the kernel about a host pid, neither of which needs
// the job to run, so a paused direct box is handed back paused.
//
// A CONTAINER IS REFUSED WHILE PAUSED. Its files are read on the host side of
// the mount too, but its liveness probe is an exec into it, which a paused
// container refuses — so a reader handed one would report a finished job as
// unreadable. Whether it is paused is the runtime's answer, read without
// changing it; a runtime that cannot say is not taken for a pause, since the
// files are still readable and the probe's own error is the honest report.
func (l *Local) Attach(ctx context.Context, sandboxID string) (Sandbox, error) {
	box, err := l.reattach(sandboxID)
	if err != nil {
		return nil, err
	}
	if b, ok := box.(*containerBox); ok && b.paused(ctx) {
		return nil, boxPaused{localErrorf("container sandbox %q is paused, and a paused container "+
			"cannot be read without unpausing it", sandboxID)}
	}
	return box, nil
}

// reattach is a box by id, rebuilt from its own directory, with nothing about
// its job changed.
func (l *Local) reattach(sandboxID string) (Sandbox, error) {
	layout, err := l.layout(sandboxID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(layout.root)
	switch {
	case errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()):
		return nil, boxGone{localErrorf("local sandbox %q is gone (its box directory %s no longer "+
			"exists on this host) — a local box lives on the engine host that made it, the host "+
			"was rebuilt, or the box was reaped", sandboxID, layout.root)}
	case err != nil:
		// NOT "gone": a directory that could not be inspected may well
		// still hold a running job, and answering it as gone would settle
		// a run whose box is merely unreadable for the moment.
		return nil, localErrorf("local sandbox %q could not be inspected at %s: %v", sandboxID, layout.root, err)
	}
	// Rebuild the credential map from the box's own record, so a reconnected
	// teardown still writes a refreshed login back.
	credentials := readCredentialMap(layout)
	if l.opts.Placement == Direct {
		confine, err := l.confinedRules(layout)
		if err != nil {
			return nil, err
		}
		return &directBox{layout: layout, credentials: credentials, confine: confine}, nil
	}
	return &containerBox{
		layout: layout, cli: l.cli,
		container: l.containerName(sandboxID), credentials: credentials,
	}, nil
}

// Kill terminates a box by id WITHOUT resuming it.
//
// The pause reaper's primitive: Connect resumes, so reclaiming a paused box
// through it would restart the work purely to stop it. Best-effort — a box
// that is already gone is not an error.
func (l *Local) Kill(ctx context.Context, sandboxID string) error {
	layout, err := l.layout(sandboxID)
	if err != nil {
		// Nothing to reclaim, and refusing is what stops the join below
		// resolving to the directory that holds every box.
		localLog.Warn("local_sandbox_kill_refused", "sandbox_id", sandboxID, "error", err.Error())
		return nil
	}
	if l.opts.Placement == Container {
		_, _ = runHost(ctx, l.cli.command(0, "rm", "-f", l.containerName(sandboxID)))
	} else if signalJob(layout, "kill", procgroup.Kill) {
		// SIGKILL alone, with NO SIGCONT first. A stopped process is killed
		// by SIGKILL directly — the signal cannot be caught, blocked or
		// ignored, so it needs no scheduling to take effect. Waking the
		// group first would let a paused run execute again, briefly, on its
		// way out: this method exists precisely so a reaped clarification
		// pause is never resumed, and the SIGCONT that Close needs (a
		// stopped process really does never run to handle SIGTERM) would
		// quietly defeat that here.
		//
		// And WAIT for it to actually go. SIGKILL is delivered, not
		// applied: the kernel takes the process down some moments later,
		// and until it does the harness's wrapper is still writing
		// its exit code and marker into the box. Removing the tree under
		// those writes fails with the directory non-empty — a failure the
		// removal below would report and nothing could act on, leaving a
		// box for the orphan reaper to find hours later.
		awaitJobExit(ctx, layout)
	}
	// Kill is a teardown like any other, and the box on disk may hold a login
	// the run refreshed before it was reclaimed — collecting it needs no
	// resume, which is the one thing this method must not do. What is on disk
	// is all there is: nothing in the box runs again after a SIGKILL or a
	// `docker rm -f`. A harness that keeps its login in a file has rewritten that
	// file itself; one that keeps it in a database of its own has it on disk
	// only as far as its runner exported it, which is why such a runner
	// exports while the run works rather than only at its end (see
	// executor's OpenCode).
	collectCredentials(ctx, layout, readCredentialMap(layout))
	removeBox(layout)
	localLog.Debug("local_sandbox_killed", "sandbox_id", sandboxID)
	return nil
}

// removeBox deletes a box's directory, saying so when it cannot.
//
// A box that survives its own teardown is not harmless: it holds the seeded
// login and whatever the run wrote, and the only thing that will ever clean it
// up is the orphan reaper on some later create. Reported rather than swallowed
// so the operator learns of it now instead of finding the directory later.
//
// THROUGH THE ROOT OF THE DIRECTORY HOLDING EVERY BOX, by the box's own name:
// nothing in the box — a link where a directory was — leads the removal of
// its contents anywhere else.
func removeBox(l boxLayout) {
	err := func() error {
		boxes, err := os.OpenRoot(filepath.Dir(l.root))
		if err != nil {
			if absent(err) {
				return nil
			}
			return err
		}
		defer func() { _ = boxes.Close() }()
		return boxes.RemoveAll(filepath.Base(l.root))
	}()
	if err != nil {
		localLog.Warn("local_sandbox_not_removed", "sandbox_id", l.id,
			"path", l.root, "error", err.Error())
	}
}

// awaitJobExit waits for a signalled job group to actually be gone.
//
// Bounded by [termGrace] and by the caller's context: a group that will not go
// is not a reason to hold a teardown open forever, and the caller proceeds
// either way. A record the kernel will not give counts as still there, so the
// wait runs its course rather than racing a removal against a live tree.
func awaitJobExit(ctx context.Context, l boxLayout) {
	deadline := time.Now().Add(termGrace)
	for time.Now().Before(deadline) {
		if _, current, err := jobGroup(l); err == nil && !current {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(exitPollInterval):
		}
	}
}

// exitPollInterval is how often a teardown asks whether a signalled group has
// gone. Short, because the answer is usually yes on the first ask and the
// caller is holding a turn open.
const exitPollInterval = 20 * time.Millisecond

// ---------------------------------------------------------------------
// the boxes on this host, and whose they are
// ---------------------------------------------------------------------

// boxRun is a box's record of the run it was made for: the turn, and the cell
// it is in — which of this host's two backends reaches it, since both keep
// their boxes under one root.
type boxRun struct {
	TurnID    string    `json:"turn_id"`
	Placement Placement `json:"placement"`
}

// recordRun writes the box's record of its run. A box made for no turn — one
// a caller other than a launch made — records nothing.
func recordRun(l boxLayout, run boxRun) error {
	if run.TurnID == "" {
		return nil
	}
	blob, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("encoding the box's run: %w", err)
	}
	if err := writeRecord(l, runRecord, blob); err != nil {
		return fmt.Errorf("writing %q: %w", l.runFile(), err)
	}
	return nil
}

// HostBox is one box on this node's host and the run it was made for.
type HostBox struct {
	SandboxID string
	TurnID    string
	Placement Placement
}

// RunBoxes lists the boxes under this backend's root that record the run
// they were made for ([boxRun]), each with the cell its record names.
//
// BOTH LOCAL CELLS' BOXES, since both keep them under one root: each answers
// for the cell its own record names, never for the backend that listed it.
// A box with no record of its run — one a launch did not make, or one whose
// record was lost — is not listed: nothing can say whose it is, and the
// orphan reaper is what bounds it ([Local.reapOrphans]). A box whose record
// cannot be read is said and not listed, for the same reason.
func (l *Local) RunBoxes() ([]HostBox, error) {
	entries, err := os.ReadDir(filepath.Join(l.root, "boxes"))
	switch {
	case absent(err):
		return nil, nil
	case err != nil:
		return nil, localErrorf("listing the boxes under %s: %v", l.root, err)
	}
	var out []HostBox
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layout, err := l.layout(entry.Name())
		if err != nil {
			continue
		}
		blob, err := readRecord(layout, runRecord)
		if absent(err) {
			continue
		}
		var run boxRun
		if err == nil {
			err = json.Unmarshal(blob, &run)
		}
		if err == nil && (run.TurnID == "" || !run.Placement.OnEngineHost()) {
			err = fmt.Errorf("the record names turn %q in cell %q", run.TurnID, run.Placement)
		}
		if err != nil {
			localLog.Warn("local_sandbox_run_unreadable", "sandbox_id", layout.id,
				"path", layout.runFile(), "error", err.Error(),
				"detail", "the box's record of its run could not be used, so this host cannot "+
					"tell whether a run still names it; the orphan reaper bounds it")
			continue
		}
		out = append(out, HostBox{SandboxID: layout.id, TurnID: run.TurnID, Placement: run.Placement})
	}
	return out, nil
}

// ---------------------------------------------------------------------
// orphan reaping
// ---------------------------------------------------------------------

// reapOrphans removes box directories an engine crash left behind.
//
// Teardown normally deletes a box, so anything here outlived the engine that
// made it. Bounded by the box TTL the caller already configures rather than a
// knob of its own — that value is exactly "how long a box may outlive its
// engine".
//
// AGE ALONE DOES NOT MEAN ORPHANED, and reading it off the directory's mtime
// does not even mean age. A directory's mtime moves only when an entry is
// added or removed directly in it, and a box's root entries are all made at
// Create — so the root mtime is the box's birth time, frozen, however busy the
// harness inside workspace/ is. Reaping on it deleted the checkout, the
// seeded credentials and the job record of every run that lasted longer than
// the TTL, while its process tree kept going: without the pid file nothing
// could ever kill it, so the job became an unkillable orphan writing into a
// directory that no longer existed.
//
// So the two questions are asked separately. IS IT IN USE? A live process
// group that is still the box's own job (direct) or an existing container,
// checked against the OS rather than inferred. HAS IT BEEN ABANDONED? The
// keepalive stamp the waiter refreshes on every poll, which is what SetTimeout
// means on this backend. A box is reaped only when it fails BOTH: nothing
// running, and nothing has touched it for a whole TTL.
//
// A paused box is deliberately covered by the first: SIGSTOPped processes stop
// being heart-beaten but stay alive, and bounding THAT wait belongs to the
// clarification pause TTL, which already owns it.
func (l *Local) reapOrphans(ctx context.Context, olderThan time.Duration) {
	boxes := filepath.Join(l.root, "boxes")
	entries, err := os.ReadDir(boxes)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-max(minReapAge, olderThan))
	var candidates []boxLayout
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layout := boxLayout{id: entry.Name(), root: filepath.Join(boxes, entry.Name())}
		if lastSeen(layout).Before(cutoff) {
			candidates = append(candidates, layout)
		}
	}
	if len(candidates) == 0 {
		// The common case, and the reason age is tested first: this runs on
		// Create, which an agent is waiting on, so a fleet with nothing stale
		// pays no liveness call at all.
		return
	}

	var live map[string]bool
	if l.opts.Placement == Container {
		if live, err = l.liveContainers(ctx); err != nil {
			// Cannot tell what is alive, so reap nothing: deleting a live
			// box's checkout is unrecoverable, a lingering directory is not.
			return
		}
	}
	for _, layout := range candidates {
		if l.boxIsAlive(layout, live) {
			continue
		}
		collectCredentials(ctx, layout, readCredentialMap(layout))
		removeBox(layout)
		localLog.Info("local_sandbox_orphan_reaped", "sandbox_id", layout.id)
	}
}

// lastSeen is when this box was last known to be in use: the keepalive stamp
// when there is one, else the directory's own mtime — which is the box's
// CREATION time and nothing else.
func lastSeen(l boxLayout) time.Time {
	if records, err := l.openRecords(); err == nil {
		info, err := records.Stat(aliveRecord)
		_ = records.Close()
		if err == nil {
			return info.ModTime()
		}
	}
	if info, err := os.Stat(l.root); err == nil {
		return info.ModTime()
	}
	// Vanished mid-scan. Report it as brand new so this pass leaves it alone
	// rather than racing whatever is deleting it.
	return time.Now()
}

// boxIsAlive reports whether this box still has something running in it.
func (l *Local) boxIsAlive(layout boxLayout, live map[string]bool) bool {
	if l.opts.Placement == Container {
		return live[l.containerName(layout.id)]
	}
	_, current, err := jobGroup(layout)
	if err != nil {
		// Cannot tell, so keep it: deleting a live box's checkout is
		// unrecoverable, a lingering directory is not.
		localLog.Warn("local_sandbox_reap_liveness_unknown", "sandbox_id", layout.id,
			"error", err.Error())
		return true
	}
	return current
}

// liveContainers names every crewlet container this runtime still has.
//
// One listing per reap, rather than an inspect per box: the reap runs on the
// Create path, which an agent is waiting on.
func (l *Local) liveContainers(ctx context.Context) (map[string]bool, error) {
	result, err := runHost(ctx, l.cli.command(0,
		"ps", "-a", "--filter", "name="+ContainerPrefix, "--format", "{{.Names}}"))
	if err != nil || result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if err != nil {
			detail = err.Error()
		}
		localLog.Warn("local_sandbox_reap_listing_failed", "error", detail)
		// WRAPPED, and carrying the detail. The reaper reads this to decide
		// which boxes are live, so a listing failure that reported only
		// "container listing failed" left the caller unable to say whether
		// the runtime was down, unreachable or refusing — three different
		// operator actions behind one string.
		if err != nil {
			return nil, fmt.Errorf("listing sandbox containers: %w", err)
		}
		return nil, localErrorf("listing sandbox containers failed with exit %d: %s",
			result.ExitCode, detail)
	}
	names := map[string]bool{}
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names[name] = true
		}
	}
	return names, nil
}

// logSignal reports a signal that did not reach its process group.
//
// Every caller is on a teardown or a pause path, where there is nothing to
// return the failure to and nothing useful to do about it — but a kill that
// silently did not land leaves a harness running on the engine host,
// which is the one outcome an operator has to be able to find. An empty group
// is not a failure and never reaches here: procgroup answers ESRCH as success.
func logSignal(what string, pid int, err error) {
	if err == nil {
		return
	}
	localLog.Warn("local_sandbox_signal_failed", "signal", what, "pgid", pid, "error", err.Error())
}
