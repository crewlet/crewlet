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
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// localLog names the LOCAL backend; the package-neutral logger is in
// protocol.go, so the shared runtime is not stamped "sandbox.local".
var localLog = logging.Get("sandbox.local")

// WorkspaceSubdir is where a box's checkout lives, relative to its home: the
// harness's working directory. Kept under the home so container mode
// needs exactly one bind mount.
const WorkspaceSubdir = "workspace"

// ContainerPrefix names every container this backend starts, so a stray box is
// identifiable — and greppable in `docker ps` — without consulting Crewlet.
const ContainerPrefix = "crewlet-sbx-"

// ControlTimeout bounds a CONTROL-PLANE command: mkdir, `docker exec`, the
// liveness probe, and a container box's start (its `docker run`, including
// the pull of an image the host lacks). Never the detached launched job,
// which is deliberately uncapped. Sized for a container runtime under load.
//
// Exported for internal/org's DefaultScheduleTimeout, which must outlast what
// a turn spends before its run starts, the box's start among it; a test in
// internal/engine holds the two in order.
const ControlTimeout = 120 * time.Second

// minReapAge floors the orphan reaper's cutoff whatever grace it is handed: a
// tiny one would otherwise make every box on the host reapable at once,
// including ones another engine just created.
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
// ONE RULE for every box the engine makes on its host: a seat's boxes in
// either local cell ([NewLocal]). This is the volume an operator sizes for
// boxes; a root derived from anything else puts a run's writes on a volume
// nobody sized for them.
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
// HOME is the box's side: the harness's HOME, checkout and run files, and in
// container mode the only directory bind-mounted, so whatever runs in the box
// writes all of it. RECORDS is the engine's side, beside the home: what a
// reconnect needs (Connect gets only an id, maybe in another process), worth
// keeping only if the box cannot rewrite it. A box that could would name any
// host file as the login to write back over, plant a link the next env rewrite
// follows out, or leave a pipe whose open blocks every reader.
//
// In direct mode (engine's user, no mount) nothing here is a boundary.
type boxLayout struct {
	id   string
	root string
	// credentialSteps observes this box's teardown login write-back
	// ([hostbox.CredentialSteps]): its backend's ([Local.credentialSteps]), nil
	// in production. On the layout because the layout is the one value every
	// teardown holds (Close, Kill, the orphan reaper, a reconnected box).
	credentialSteps hostbox.CredentialSteps
	// statTaken observes each read's open once the one stat that sizes it
	// and that its change check compares with is taken ([boxLayout.opened]):
	// nil in production, a test's writer growing the file there.
	statTaken func(f *os.File)
}

// home is the box's own side: its HOME, and its mount in container mode.
func (l boxLayout) home() string { return filepath.Join(l.root, "home") }

// records is the engine's side, out of the box's reach in container mode.
func (l boxLayout) records() string { return filepath.Join(l.root, "records") }

func (l boxLayout) workspace() string { return filepath.Join(l.home(), WorkspaceSubdir) }

// tmp is the box's own temporary directory: every temp root its processes
// are pointed at ([TempRootEnv]), inside the home so a run's temporary files
// stay in its box.
func (l boxLayout) tmp() string { return filepath.Join(l.home(), ".tmp") }

// meta is the box's own .crewlet/, where its runner's markers, reports and
// bin directory live: the box's side, never a record.
func (l boxLayout) meta() string { return filepath.Join(l.home(), ".crewlet") }

// metaName is [boxLayout.meta] as a name in the home.
const metaName = ".crewlet"

// openHome is the box's home opened as an os.Root; EVERY HOST-SIDE ACCESS TO
// A FILE THE BOX HOLDS (read, write, mkdir, remove) goes through one, by a
// name relative to it.
//
// THE HOME IS THE BOX'S TO WRITE: a container's job writes its whole mounted
// home, a direct job its whole home, and both run a checkout's scripts. So any directory in it may become a link
// at any moment; a path checked then opened by name is two walks, while the
// root walks it once and refuses a link leading outside it.
//
// Opened per access, not held: a box value is remade by every reconnect and
// dropped without teardown, and a held descriptor would outlive the directory.
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

// aliveFile is the keepalive stamp the orphan reaper reads ([lastSeen]) of a box
// that records neither a run nor a check ([recordedRun]): one whose record did
// not land.
//
// A directory's mtime does NOT move on writes inside its subdirectories, so
// the box root's timestamp is its creation time for life. This file is touched
// on every [Sandbox.Touch] (the waiter's, once per poll for the box it keeps
// alive) and by a launch for the box it is provisioning, which has no job yet
// and only the stamp keeps from another launch's orphan sweep.
func (l boxLayout) aliveFile() string { return filepath.Join(l.records(), aliveRecord) }

// credentialsFile records the logins this box was seeded with
// ([seededCredential]), so a reconnect knows which files to write back and
// against what. Every production teardown goes through Connect or Kill, never
// the object Create returned, so without it every run's refreshed login would
// be discarded.
func (l boxLayout) credentialsFile() string { return filepath.Join(l.records(), credentialsRecord) }

// runFile records the run or check the box was made for ([boxRun]), which is
// how this host tells a box nothing names any more from one in use ([HostBox],
// [CheckBox]).
func (l boxLayout) runFile() string { return filepath.Join(l.records(), runRecord) }

// envFile is the container runtime's --env-file. See containerBox.envArgs.
func (l boxLayout) envFile() string { return filepath.Join(l.records(), envRecord) }

// groupFile records a container box's job's process group IN ITS CONTAINER, as
// the exec that started it read it off the kernel ([containerBox.StartBackground]):
// `<group> <handle>`, so [containerBox.KillJob] can end every process of the
// job it names and nothing else. Only the exec knows the group: the handle is
// the job's first process, which leads none. A record of the engine's, out of
// the job's reach, since what is signalled through it is read back from it.
func (l boxLayout) groupFile() string { return filepath.Join(l.records(), groupRecord) }

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
	groupRecord       = "job.group"
)

// seededCredential is one login file a box was seeded with, as the box's
// RECORD keeps it: its source on the engine host, the SHA-256 of the bytes the
// box was given ("" for a file the shared login did not hold then), and its
// rule ([Spec.CredentialRules]), by which a mixed file's seed is its login's
// fields alone.
//
// THE SEED IS WHAT A WRITE-BACK IS JUDGED AGAINST, not the shared login at
// teardown: an idle box's copy differs from that once anybody else writes, and
// putting a stale seed back over a login the host refreshed meanwhile, whose
// refresh token the vendor had already rotated away, logs every seat out. An
// absent file is recorded too: a file added since is a newer login.
type seededCredential struct {
	Source string                  `json:"source"`
	Seed   string                  `json:"seed"`
	Rule   harnessprofile.FileRule `json:"rule"`
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
// runs, and reports what it seeded: every file of the login by its path
// relative to the home, with the digest of what the box was given, and "" for
// a file the shared directory lacks, which is not seeded and so never written
// back over.
//
// Each login is READ WHOLE under the shared directory's lock
// ([hostbox.ReadCredentials]), so a seed never straddles a write, and seeded
// WHOLE OR NOT AT ALL ([seedLogin]): a harness handed half a login fails inside
// the run in a way nothing outside explains. Keys come from an
// operator-overridable profile, so they go through hostbox.SafeJoin: a
// "../../etc/cron.d/x" would otherwise write a host file.
func seedCredentials(
	ctx context.Context, l boxLayout, files map[string]string, rules map[string]harnessprofile.FileRule,
) map[string]seededCredential {
	seeded := map[string]seededCredential{}
	for _, login := range loginsOf(files) {
		got, err := seedLogin(ctx, l, login, rules)
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

// seedLogin seeds one login into a box, every file the shared directory
// holds of it or none, and reports what it seeded. Each file is handed whole,
// a mixed one's settings with its login, and held to its login alone
// ([harnessprofile.FileRule.Login]); one that cannot be read for it fails the
// whole login.
func seedLogin(
	ctx context.Context, l boxLayout, login boxLogin, rules map[string]harnessprofile.FileRule,
) (map[string]seededCredential, error) {
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
		// A FILE WITH NO RULE carries nothing back: its zero rule names no
		// field and no store; one with a rule is read by its shared name.
		rule, ruled := rules[relative]
		if ruled {
			rule.Name = filepath.Base(source)
		}
		content, present := shared[filepath.Base(source)]
		if !present {
			seeded[relative] = seededCredential{Source: source, Rule: rule}
			continue
		}
		held, err := rule.Login(content)
		if err == nil {
			err = writeBoxFile(home, targets[relative], content)
		}
		if err != nil {
			for _, name := range written {
				_ = home.Remove(name)
			}
			return nil, fmt.Errorf("seeding %q: %w", relative, err)
		}
		written = append(written, targets[relative])
		// THE SEED'S DIGEST IS OF THE LOGIN THE BOX WAS GIVEN: read separately
		// it could describe a newer file than the box's, and the teardown
		// would then excuse a write it should have refused.
		seeded[relative] = seededCredential{Source: source, Seed: digestOf(held), Rule: rule}
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

// seedDigest is the digest of the login content holds, a copy of the file
// ([harnessprofile.FileRule.Login]): two copies whose login is one have one,
// whatever settings beside it differ. A copy that cannot be read for its login
// has "", no seed's.
func seedDigest(rule harnessprofile.FileRule, content []byte) string {
	seed, err := rule.Login(content)
	if err != nil {
		return ""
	}
	return digestOf(seed)
}

// digestOf is the SHA-256 of content in [hostbox.Digest]'s form.
func digestOf(content []byte) string {
	digest, _ := hostbox.Digest(bytes.NewReader(content)) // a bytes.Reader does not fail
	return digest
}

// collectCredentials writes each login the run refreshed back to the shared
// directory, a COMPARE-AND-SWAP against the seed ([writeBackLogin]).
//
// Most vendors rotate the refresh token with the hours-long access token, so
// discarding a refresh logs the fleet out at the next expiry, and writing back
// a login the box did not refresh over one the host did logs it out the same.
// Reads stay in the box's home ([readBoxFile]), so a "../../" key cannot copy
// a host file over the credential store, and the map is the box's RECORD, out
// of its reach: where a login is written is the operator's profile's say.
//
// A TEARDOWN, so it takes [context.WithoutCancel]; its one wait, another
// writer's lock, has its own bound ([hostbox.CredentialLockWait]).
func collectCredentials(ctx context.Context, l boxLayout, seeded map[string]seededCredential) {
	ctx = context.WithoutCancel(ctx)
	files := map[string]string{}
	for relative, rec := range seeded {
		files[relative] = rec.Source
	}
	commit := func(dir string, names []string, next map[string][]byte) error {
		return hostbox.WriteCredentials(dir, names, next, l.credentialSteps)
	}
	for _, login := range loginsOf(files) {
		got, err := writeBackLogin(ctx, l, login, seeded, commit)
		if err != nil {
			localLog.Warn("local_sandbox_credential_writeback_failed",
				"sandbox_id", l.id, "dir", login.dir, "error", err.Error())
			continue
		}
		for _, relative := range slices.Sorted(maps.Keys(got)) {
			switch outcome := got[relative]; outcome {
			case writeBackWritten:
				localLog.Info("local_sandbox_credential_refreshed", "sandbox_id", l.id, "file", relative)
			case writeBackChanged, writeBackRemoved, writeBackOtherAccount, writeBackUnreadable:
				// WARN: the one trace that a refresh the run made went nowhere;
				// two in a row on one login are worth a look. It names the file,
				// never what either copy holds.
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
// sentence apiece, because each leaves a different thing behind.
var supersededDetail = map[writeBack]string{
	writeBackUnreadable: "the box's copy of this login could not be read, so nothing of it was " +
		"written back, and the shared one was kept",
	writeBackOtherAccount: "the box's copy of this login names another account than the shared " +
		"one in its account fields or named token, so it was not written back, and the shared one was kept",
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
	// writeBackUnreadable: a file of the box's copy could not be read, so
	// nothing of it can be told apart, and the shared one was kept.
	writeBackUnreadable writeBack = "unreadable"
	// writeBackOtherAccount: the box's copy names another account than the
	// shared login ([harnessprofile.FileRule.SameAccount]), so it is no refresh of
	// that login, and the shared one was kept.
	writeBackOtherAccount writeBack = "other_account"
)

// writeBackLogin writes the files of one login the run CHANGED back over the
// shared login, only while the shared login is STILL, as a whole, the one the
// box was seeded with, and reports what became of each changed file.
//
// TWO CONDITIONS, each a different lost login: a copy still holding its seed
// refreshed nothing, and writing it back reverts the host's writes since; a
// shared login no longer holding the seed (any file of it, or one it lacked)
// was rewritten by another refresh or a new login, and is the newer. So the
// compare and ONE WRITE of the whole login (never half, even if killed) are
// one step under [hostbox.LockCredentials], which every writer takes. A file
// the box was not seeded with is never written. commit: see [loginCommit].
func writeBackLogin(
	ctx context.Context, l boxLayout, login boxLogin, seeded map[string]seededCredential, commit loginCommit,
) (map[string]writeBack, error) {
	got := map[string]writeBack{}
	changed := map[string][]byte{}
	// held is the box's copy of every seeded file it still holds, which the
	// account check reads whole, changed or not.
	held := map[string][]byte{}
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
		if present {
			held[relative] = content
		}
		if !present || seedDigest(rec.Rule, content) == rec.Seed {
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
	// THE SHARED LOGIN STILL THE SEED, judged before anything is carried: a
	// login another writer moved or removed is newer than any refresh here.
	for relative, rec := range login.files {
		shared, present := current[filepath.Base(rec)]
		seed := seeded[relative].Seed
		if (seed == "") == present || (present && seedDigest(seeded[relative].Rule, shared) != seed) {
			if len(current) == 0 {
				return verdict(writeBackRemoved), nil
			}
			return verdict(writeBackChanged), nil
		}
	}
	// ONLY TOKENS ARE CARRIED ([harnessprofile.FileRule.Carry]): a file's
	// refresh keys, never an endpoint, a key the run minted or a setting it
	// wrote, and a store's document held to the logins it was handed. A file
	// that then is the shared one changed nothing.
	for _, name := range slices.Sorted(maps.Keys(changed)) {
		kept, carryErr := seeded[byName[name]].Rule.Carry(changed[name], current[name])
		if carryErr != nil {
			// A COPY THAT CANNOT BE READ carries nothing: the shared login
			// stays, said as unreadable, never with the parser's words, which
			// may quote the file.
			return verdict(writeBackUnreadable), nil
		}
		if bytes.Equal(kept, current[name]) {
			delete(changed, name)
			got[byName[name]] = writeBackIdle
			continue
		}
		changed[name] = kept
	}
	if len(changed) == 0 {
		return got, nil
	}
	next := maps.Clone(current)
	maps.Copy(next, changed)
	if maps.EqualFunc(next, current, bytes.Equal) {
		return verdict(writeBackHeld), nil
	}
	// A COPY WHOSE ACCOUNT FIELDS OR NAMED TOKEN NAME ANOTHER ACCOUNT is not
	// written back ([harnessprofile.FileRule.SameAccount]): the whole login
	// stays as it is. Asked of EVERY file of the box's copy, changed or not,
	// against the login it was seeded with, which current is here; a file
	// holding them that the box dropped names none.
	for _, relative := range slices.Sorted(maps.Keys(login.files)) {
		rec := seeded[relative]
		if rec.Seed == "" || len(rec.Rule.IdentityKeys) == 0 {
			continue
		}
		copied, present := held[relative]
		if !present || !rec.Rule.SameAccount(copied, current[filepath.Base(login.files[relative])]) {
			return verdict(writeBackOtherAccount), nil
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
// [hostbox.WriteCredentials] with the layout's observer
// ([boxLayout.credentialSteps]), in every box.
//
// A PARAMETER for the one failure after which the refresh HAS been written
// (its journal landed; the next reader puts a refused file in place), which a
// write-back must report as written. No obstacle a test leaves beforehand
// reaches it, since the read under the same lock refuses whatever would stop
// the write, so a test hands in a commit that puts one down between the two.
type loginCommit func(dir string, names []string, login map[string][]byte) error

// readBoxFile reads one login file at name in a box's home, false where it
// holds none.
//
// OPENED ONCE ([openRegularIn]), since the box's side names the path: a pipe
// answers at once rather than blocking a teardown, a link is refused, no
// directory on the way leads out, and what is written back is what was read.
// Past [hostbox.MaxCredentialBytes] it is refused: the shared login would not
// hold it, and a box is not handed the engine's memory.
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
// write a refreshed login back. It returns its failure, though the caller
// only logs it, so a test can assert on it.
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
// A RECORD THAT CANNOT BE USED IS SAID and read as no map: every reconnect
// (the poll, the collection, both reapers) reads it and none may fail over
// it; what is lost is a refreshed login's write-back, which the warning names.
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
// writes, the credential map, is a few hundred bytes; a mebibyte holds over a
// hundred and twenty entries at a filesystem's longest paths, so a record past
// it is not this engine's and is not read whole to find that out.
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

// removeRecord removes the engine's record called name about a box, through the
// box's records ([boxLayout.openRecords]); one that is not there is nothing.
func removeRecord(l boxLayout, name string) error {
	records, err := l.openRecords()
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = records.Close() }()
	if err := records.Remove(name); err != nil && !absent(err) {
		return err
	}
	return nil
}

// containerJobGroup is the in-container process group a container box
// recorded for the job pid names as it started ([boxLayout.groupFile]), false
// where there is none: an older build's job, a runtime whose exec led no
// session of its own ([containerBox.StartBackground]), another job's record,
// or an unreadable one, which is said. A job with none is killed through its
// first process's group ([killFirstProcessGroup]).
func containerJobGroup(l boxLayout, pid int) (int, bool) {
	raw, err := readRecord(l, groupRecord)
	if absent(err) {
		return 0, false
	}
	if err != nil {
		localLog.Warn("local_sandbox_job_group_unreadable", "sandbox_id", l.id, "path", l.groupFile(),
			"error", err.Error(), "detail", "the job's process group in its container could not be read, "+
				"so its kill reads the group off the job's first process instead")
		return 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, false
	}
	group, groupErr := strconv.Atoi(fields[0])
	job, jobErr := strconv.Atoi(fields[1])
	if groupErr != nil || jobErr != nil || group < 2 || job != pid {
		return 0, false
	}
	return group, true
}

// killJobGroup ends a direct box's job group the way its teardown always has, and
// leaves the box and its files ([Sandbox.KillJob]): SIGCONT first — a stopped
// process never runs again to handle SIGTERM — then SIGTERM, up to
// [JobStopGrace] for the group to go, SIGKILL, and up to [JobStopGrace] more
// for the kernel to take it ([JobKillWait]).
//
// Each signal re-reads the job's identity ([signalJob]) rather than reusing one
// reading: the group may end between two of them, and its pid is then free to
// be reissued to a stranger before the next.
func killJobGroup(ctx context.Context, l boxLayout) {
	if signalJob(l, "continue", procgroup.Continue) {
		signalJob(l, "terminate", procgroup.Terminate)
		awaitJobExit(ctx, l)
		// Whether or not it went: SIGKILL reaches nothing on a group that
		// is already gone, and the alternative is a tree left running
		// because the grace expired.
		if signalJob(l, "kill", procgroup.Kill) {
			// Waited for again: a teardown's removal of the box races the
			// dying wrapper's last writes exactly as Kill's does, and a read
			// after a kill reads what the wrapper last wrote.
			awaitJobExit(ctx, l)
		}
	}
}

// jobGroup is the box's recorded job group when that group is still the job,
// and false when there is none to act on.
//
// THREE ANSWERS. False with a nil error is definitive: no record (no job
// started), a record this engine did not write, or a group that has ended or
// passed to a later process. An error means the record ([readRecord], a
// non-regular file included) or the kernel could not be read: it neither
// proves the job alive nor licenses a signal, so the orphan reaper keeps the
// box and every signal is withheld. A record that does not parse is logged and
// read as no job: a direct job runs as the engine's user beside its records,
// so the content is input, and a bare pid lacks the start time
// [procgroup.ParseLeader] requires.
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
	// differs between the two cells, which is why one backend serves both:
	// everything else (the box root, the reap, the process group, the box's
	// bin directory) is identical.
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

	// RuntimeEnv is what the operator DECLARED for the container runtime's own
	// harness, references resolved, laid over the fixed rule every runtime call
	// carries ([hostbox.ContainerRuntime]): what a registry credential helper
	// reads to pull the image (a cloud role, profile or key file). It never
	// reaches a box: that is `role.sandbox.env`.
	RuntimeEnv map[string]string

	// HostGrace is the orphan reaper's stamp cutoff ([Local.reapOrphans]): the
	// manager's [Manager.HostGrace], one value for every box on this root
	// whichever box's Create runs the sweep. Zero takes [DefaultHostGrace].
	HostGrace time.Duration
}

// Local mints sandboxes on the engine host, directly or in a container.
//
// It exists so a role can do real code work with the harness the operator
// already logged in to — no account, no API key, no token minting. The run
// reads the same credential directory `crewlet llm login` wrote.
type Local struct {
	opts LocalOptions
	root string
	cli  containerCLI // the resolved runtime; its path is "" in direct mode
	// credentialSteps observes every login write-back this backend's boxes
	// make ([hostbox.CredentialSteps]): nil in production, set by the suite
	// before a box is made (export_test.go's ObserveCredentialWrites), and
	// carried to each teardown on the box's layout.
	credentialSteps hostbox.CredentialSteps
}

var _ Provider = (*Local)(nil)

// NewLocal validates the options and resolves the container runtime once,
// rather than on every Create.
func NewLocal(opts LocalOptions) (*Local, error) {
	if opts.Placement == "" {
		opts.Placement = Direct
	}
	if opts.HostGrace == 0 {
		opts.HostGrace = DefaultHostGrace
	}
	if opts.Placement != Direct && opts.Placement != Container {
		return nil, localErrorf("the local backend runs %q or %q, not %q",
			Direct, Container, opts.Placement)
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

// layout is where one box's files live.
//
// AN EMPTY ID IS REFUSED: joining "" gives the boxes directory itself, whose
// Close or Kill would os.RemoveAll every box on the host. It is reachable: a
// run's row exists before its box is attached, so a poll in that window
// connects to "".
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
	return boxLayout{id: id, root: filepath.Join(l.root, "boxes", id), credentialSteps: l.credentialSteps}, nil
}

func (l *Local) containerName(id string) string { return ContainerPrefix + id }

// EngineHost is the name the engine host answers to inside every box this
// backend makes, known before one exists ([Manager.EngineHost]): a
// container's is its runtime's alias, as the box itself answers
// ([containerBox.EngineHost]); a direct box shares the host's network, "".
func (l *Local) EngineHost() string {
	if l.opts.Placement != Container {
		return ""
	}
	return l.cli.hostAlias()
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

	l.reapOrphans(ctx, l.opts.HostGrace)
	seeded := seedCredentials(ctx, layout, spec.CredentialFiles, spec.CredentialRules)
	// The box records what it was seeded with and stamps itself alive before
	// anyone can reap it.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := recordCredentialMap(layout, seeded); err != nil {
		// The map names paths and digests, not secrets, but losing it means
		// a refreshed login is never written back to the shared directory —
		// worth saying out loud, and not worth failing the box for.
		localLog.Warn("local_sandbox_credential_map_unwritable", "sandbox_id", layout.id, "error", err.Error())
	}
	if err = recordRun(layout, boxRun{TurnID: spec.TurnID, Check: spec.Check, Placement: l.opts.Placement}); err != nil {
		// LOST, the box is one its host's waiter or check runner cannot tell
		// is still in use, so neither reclaims it — only, once nothing runs
		// in it, the orphan reaper. Worth saying; not worth the box.
		localLog.Warn("local_sandbox_run_unrecorded", "sandbox_id", layout.id, "error", err.Error())
	}
	touchAlive(layout)

	if l.opts.Placement == Direct {
		localLog.Info("local_sandbox_created", "sandbox_id", id, "placement", string(Direct), "home", layout.home())
		return &directBox{layout: layout, env: spec.Env, credentials: seeded}, nil
	}

	name := l.containerName(id)
	result, err := runHost(ctx, l.cli.command(0, l.runArgv(ctx, layout, name)...))
	if err != nil || result.ExitCode != 0 {
		// A RUN THAT FAILED MAY STILL HAVE MADE ITS CONTAINER, created and never
		// started or started as its CLI was cut off, which only this box's record
		// names: removed with the box, or keeping it ([containerBox.discard]).
		(&containerBox{layout: layout, cli: l.cli, container: name}).discard(ctx)
		if cancelledBy(ctx) {
			// THIS NODE CANCELLED THE LAUNCH, which says nothing about the image:
			// classed as an image failure, a drain would read as a setup to fix.
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
		// THE WHOLE DETAIL: the runtime's own account of why the image would not
		// start (platform, pull, mount), of which the operator has no other copy.
		//
		// CLASSED BY WHETHER THE IMAGE IS HERE, asked of the runtime rather than
		// read from its words: present and refused is the configuration's; absent is
		// a failed pull, which a retry may outlast.
		return nil, localRefusedf(startFailure(ctx, l.cli, l.opts.Image),
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
		// The caller hears the mount's failure; a container left is discard's.
		box.discard(ctx)
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
	// AHEAD OF THE ENGINE FOR THE KERNEL'S OOM KILLER ([boxOOMScore]), for every
	// process in the box: both runtimes start an `exec` from the container's
	// process spec, this value included (Docker's daemon/exec.go, Podman's
	// libpod/oci_conmon_exec_common.go). Neither raises it unasked: Docker runs a
	// container at 0, Podman at its harness's value, the engine's. It orders
	// the box against the engine only on a shared kernel; in a VM (Docker
	// Desktop) it orders within the VM. Before run_args, so an operator's own
	// --oom-score-adj wins.
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
// It AUTO-RESUMES, as [Provider.Connect] promises: a paused box the
// coordinator reattaches to must be runnable immediately. That is exactly why
// the pause reaper must use Kill instead.
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
// A DIRECT BOX IS READ WHILE STOPPED: its files are the host's and its probe
// asks the kernel about a host pid, so a paused direct box is handed back
// paused. A PAUSED CONTAINER IS REFUSED: its probe is an exec, which a paused
// container refuses, so a reader would report a finished job as unreadable.
// Paused is the runtime's answer, read without changing it; a runtime that
// cannot say is not taken for a pause, since the files stay readable and the
// probe's own error is the honest report.
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
		return &directBox{layout: layout, credentials: credentials}, nil
	}
	return &containerBox{
		layout: layout, cli: l.cli,
		container: l.containerName(sandboxID), credentials: credentials,
	}, nil
}

// Kill terminates a box by id WITHOUT resuming it.
//
// The pause reaper's primitive: Connect resumes, so reclaiming a paused box
// through it would restart the work purely to stop it. A box already gone is
// not an error; a container its runtime keeps is, and the box is left whole,
// its record with it, for the caller's next reclaim ([containerBox.remove]).
func (l *Local) Kill(ctx context.Context, sandboxID string) error {
	layout, err := l.layout(sandboxID)
	if err != nil {
		// Nothing to reclaim, and refusing is what stops the join below
		// resolving to the directory that holds every box.
		localLog.Warn("local_sandbox_kill_refused", "sandbox_id", sandboxID, "error", err.Error())
		return nil
	}
	if l.opts.Placement == Container {
		box := &containerBox{layout: layout, cli: l.cli, container: l.containerName(sandboxID)}
		if err := box.remove(ctx); err != nil {
			return err
		}
	} else if signalJob(layout, "kill", procgroup.Kill) {
		// SIGKILL alone, with NO SIGCONT first: SIGKILL takes a stopped process
		// directly, and waking the group would let a paused run execute again on
		// its way out, which this method exists to prevent.
		//
		// And WAIT for it to go: the kernel takes the process down moments after
		// delivery, and until then the wrapper still writes its exit code and
		// marker, so removing the tree under it fails non-empty and leaves the box
		// for a later reclaim.
		awaitJobExit(ctx, layout)
	}
	// A teardown like any other: the box may hold a login the run refreshed,
	// and collecting it needs no resume. Nothing in the box runs after SIGKILL or
	// `docker rm -f`, so a harness keeping its login in a database has it on disk
	// only as far as its runner exported it, which is why such a runner exports
	// while the run works (executor's OpenCode).
	collectCredentials(ctx, layout, readCredentialMap(layout))
	removeBox(layout)
	localLog.Debug("local_sandbox_killed", "sandbox_id", sandboxID)
	return nil
}

// removeBox deletes a box's directory, saying so when it cannot: a box that
// survives its teardown holds the seeded login and the run's writes, and only
// a later reclaim, its host's sweep or a create's orphan reaper, would clean it.
//
// THROUGH THE ROOT OF THE DIRECTORY HOLDING EVERY BOX, by the box's name, so
// no link in the box leads the removal anywhere else.
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
// Bounded by [JobStopGrace] and by the caller's context: a group that will not go
// is not a reason to hold a teardown open forever, and the caller proceeds
// either way. A record the kernel will not give counts as still there, so the
// wait runs its course rather than racing a removal against a live tree.
func awaitJobExit(ctx context.Context, l boxLayout) {
	deadline := time.Now().Add(JobStopGrace)
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

// boxRun is a box's record of what it was made for — a run's turn, or a
// check's attempt ([Spec.Check]), never both — and the cell it is in: which of
// this host's two backends reaches it, since both keep their boxes under one
// root.
type boxRun struct {
	TurnID    string    `json:"turn_id,omitempty"`
	Check     string    `json:"check,omitempty"`
	Placement Placement `json:"placement"`
}

// recordRun writes the box's record of what it was made for. A box made for
// neither a turn nor a check records nothing.
func recordRun(l boxLayout, run boxRun) error {
	if run.TurnID == "" && run.Check == "" {
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

// recordedRun is what a box records it was made for ([recordRun]), and false
// where it records nothing it can use: no record, or one unreadable or naming
// not exactly one of a turn and a check in a known cell, which the error says.
// THE ONE READING, so the boxes its host's sweep reclaims ([Local.RunBoxes]),
// those its node's check runner reclaims ([Local.CheckBoxes]) and those the
// orphan reaper judges ([Local.reapOrphans]) part the host's boxes.
func recordedRun(l boxLayout) (boxRun, bool, error) {
	blob, err := readRecord(l, runRecord)
	if absent(err) {
		return boxRun{}, false, nil
	}
	var run boxRun
	if err == nil {
		err = json.Unmarshal(blob, &run)
	}
	if err == nil && ((run.TurnID == "") == (run.Check == "") || !run.Placement.Valid()) {
		err = fmt.Errorf("the record names turn %q and check %q in cell %q", run.TurnID, run.Check, run.Placement)
	}
	if err != nil {
		return boxRun{}, false, err
	}
	return run, true, nil
}

// HostBox is one box on this node's host and the run it was made for.
type HostBox struct {
	SandboxID string
	TurnID    string
	Placement Placement
}

// CheckBox is one box on this node's host and the check attempt it was made
// for, as its caller spelled it ([Spec.Check]).
type CheckBox struct {
	SandboxID string
	Check     string
	Placement Placement
}

// RunBoxes lists the boxes under this backend's root that record the run
// they were made for ([boxRun]), each with the cell its record names.
//
// BOTH CELLS' BOXES, since both keep them under one root: each answers
// for the cell its own record names, never for the backend that listed it.
// A box with no record of its run — one a launch did not make, or one whose
// record was lost — is not listed: nothing can say whose it is, and the
// orphan reaper is what bounds it ([Local.reapOrphans]). A box whose record
// cannot be read is said and not listed, for the same reason.
func (l *Local) RunBoxes() ([]HostBox, error) {
	records, err := l.recordedBoxes(func(layout boxLayout, err error) {
		localLog.Warn("local_sandbox_run_unreadable", "sandbox_id", layout.id,
			"path", layout.runFile(), "error", err.Error(),
			"detail", "the box's record of its run could not be used, so this host cannot "+
				"tell whether a run still names it; the orphan reaper bounds it")
	})
	var out []HostBox
	for _, r := range records {
		if r.run.TurnID != "" {
			out = append(out, HostBox{SandboxID: r.layout.id, TurnID: r.run.TurnID, Placement: r.run.Placement})
		}
	}
	return out, err
}

// CheckBoxes lists the boxes under this backend's root that record the check
// attempt they were made for ([boxRun]), each with the cell its record names,
// as [Local.RunBoxes] lists a run's. A box whose record cannot be used is not
// listed, and not said again: RunBoxes says it, and it is the orphan reaper's.
func (l *Local) CheckBoxes() ([]CheckBox, error) {
	records, err := l.recordedBoxes(nil)
	var out []CheckBox
	for _, r := range records {
		if r.run.Check != "" {
			out = append(out, CheckBox{SandboxID: r.layout.id, Check: r.run.Check, Placement: r.run.Placement})
		}
	}
	return out, err
}

// recordedBox is a box under this backend's root and what it records it was
// made for.
type recordedBox struct {
	layout boxLayout
	run    boxRun
}

// recordedBoxes is every box under this backend's root with a record it can
// use ([recordedRun]), handing each whose record it cannot use to unusable
// where that is set; an error is a root that could not be listed.
func (l *Local) recordedBoxes(unusable func(boxLayout, error)) ([]recordedBox, error) {
	entries, err := os.ReadDir(filepath.Join(l.root, "boxes"))
	switch {
	case absent(err):
		return nil, nil
	case err != nil:
		return nil, localErrorf("listing the boxes under %s: %v", l.root, err)
	}
	var out []recordedBox
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layout, err := l.layout(entry.Name())
		if err != nil {
			continue
		}
		run, recorded, err := recordedRun(layout)
		if err != nil && unusable != nil {
			unusable(layout, err)
		}
		if recorded {
			out = append(out, recordedBox{layout: layout, run: run})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------
// orphan reaping
// ---------------------------------------------------------------------

// reapOrphans removes the box directories left behind that record neither a
// run nor a check ([recordedRun]), bounded by the host grace
// ([LocalOptions.HostGrace]). A recorded box is its host's sweep's
// ([Waiter.sweep]) or its node's check runner's ([CheckBox]), each asking a
// row: a PARKED run's box has an exited job and an untouched stamp, and is
// kept to its pause TTL, past a grace that would have reaped it.
//
// Any other box is reaped only when it fails BOTH questions. IN USE? A live
// process group still the box's own job (direct) or an existing container,
// asked of the OS. ABANDONED? Its stamp ([boxLayout.aliveFile]) untouched for a
// whole grace. Age alone is not orphanhood: the root's mtime is its birth
// ([lastSeen]); reaping on it would delete a long run's checkout under its job.
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
		layout := boxLayout{
			id: entry.Name(), root: filepath.Join(boxes, entry.Name()), credentialSteps: l.credentialSteps,
		}
		if !lastSeen(layout).Before(cutoff) {
			continue
		}
		if _, recorded, _ := recordedRun(layout); recorded {
			// Its host's sweep's or its check runner's, whatever its job and
			// stamp say.
			continue
		}
		candidates = append(candidates, layout)
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
		// WRAPPED, with the detail: the runtime down, unreachable or refusing are
		// three different operator actions.
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

// startFailure classes a container that would not start from image: the
// configuration's when the runtime holds the image and refused to run it, the
// image source's when the image is not here, so the failure was fetching it.
func startFailure(ctx context.Context, cli containerCLI, image string) Why {
	result, err := runHost(ctx, cli.command(0, "image", "inspect", image))
	if err == nil && result.ExitCode == 0 {
		return WhyConfiguration
	}
	return WhyProviderUnavailable
}
