package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/sandbox/homecache"
)

// A CONTAINER BOX'S HOME, CACHED (#2005): a run's box starts from a copy of
// the home its seat's setup left in a build, once per node and key, rather
// than running that setup again. internal/sandbox/homecache is the cache and
// says why it is a home and not an image; this file is the container cell's
// half: the build, what it is handed, and what is said when it caches less.

// identityFacts are the run environment's variables a build is handed beside
// its steps' own literal env: the engine's facts about the seat, which a
// recipe reads (git's commit identity), never a credential.
var identityFacts = []string{"CREWLET_AGENT_HANDLE", "CREWLET_AGENT_EMAIL"}

// maxEscapedPaths is how many paths a cut names: enough to say what a step
// installed outside the home, short enough for one log line.
const maxEscapedPaths = 16

// HomeMeter is told each build of a cached home that cached fewer steps than
// it was given, by reason ("outside_home", "credential"): the engine's meter
// records them in the node's metrics. Nil records nothing.
type HomeMeter interface {
	HomeCut(reason string)
}

// homeCacheExclude is what of a built home is the box's own, never kept: its
// work tree and its temporary files. A step's tools in .crewlet/bin stay.
var homeCacheExclude = []string{WorkspaceSubdir, ".tmp"}

// homeCache is this backend's cache, under its root beside the boxes and on
// their filesystem, so a build's home is renamed in rather than copied.
func (l *Local) homeCache() (*homecache.Cache, error) {
	return homecache.Open(filepath.Join(l.root, "homes"), homeCacheExclude...)
}

// CreateProvisioned is [Local.Create] for a run's box whose home starts from
// this node's cache of steps ([homecache]): in the container cell, the first
// cached of steps are already in its home, and it says how many. Elsewhere,
// or where the cache cannot answer, it is Create and none.
func (l *Local) CreateProvisioned(ctx context.Context, spec Spec, steps []SetupStep) (Sandbox, int, error) {
	if l.opts.Placement != Container || len(steps) == 0 {
		box, err := l.Create(ctx, spec)
		return box, 0, err
	}
	var cached int
	box, err := l.create(ctx, spec, createOptions{fill: func(home string) error {
		var err error
		cached, err = l.provisionHome(ctx, spec, steps, home)
		return err
	}})
	if err != nil {
		return nil, 0, err
	}
	return box, cached, nil
}

// provisionHome fills home from the cache and says how many of steps it
// holds. A build whose step fails is the setup's failure, as it would have
// been in the box; any other failure of the cache leaves home as it was and
// caches nothing, so the box runs every step.
func (l *Local) provisionHome(ctx context.Context, spec Spec, steps []SetupStep, home string) (int, error) {
	seat := spec.Env[identityFacts[0]]
	cache, err := l.homeCache()
	var image string
	if err == nil {
		image, err = l.imageID(ctx)
	}
	if err != nil {
		localLog.Warn("local_sandbox_home_uncached", "seat", seat, "error", err.Error(),
			"detail", "this box runs every setup step itself")
		return 0, nil
	}
	env := buildEnv(spec.Env, steps)
	in := homecache.Input{
		Owner: seat, Runtime: filepath.Base(l.cli.path), ImageID: image, Env: env,
		Steps: cachedSteps(steps), Withheld: withheld(spec.Env, env), Secrets: spec.Secrets,
	}
	var roots []string
	build := func(ctx context.Context, built []homecache.Step) (homecache.Built, error) {
		b, root, berr := l.buildHome(ctx, env, steps[:len(built)])
		if root != "" {
			roots = append(roots, root)
		}
		return b, berr
	}
	res, err := cache.Provision(ctx, in, build, home)
	for _, root := range roots {
		_ = os.RemoveAll(root) // its home renamed into the cache, or removed with a cut
	}
	for _, cut := range res.Cuts {
		l.sayCut(seat, cut)
	}
	var setupErr *SetupError
	switch {
	case errors.As(err, &setupErr):
		localLog.Warn("local_sandbox_home_build_failed", "seat", seat, "step", setupErr.Step,
			"error", err.Error(), "remedy", "a step that needs a credential the build is not "+
				"handed fails there: mark it every_box, and it runs in each box with the launch's "+
				"environment instead")
		return 0, err
	case err != nil:
		localLog.Warn("local_sandbox_home_uncached", "seat", seat, "error", err.Error(),
			"detail", "this box runs every setup step itself")
		return 0, emptyHome(home)
	}
	if res.Built {
		localLog.Info("local_sandbox_home_cached", "seat", seat, "key", string(res.Key), "steps", res.Prefix)
	}
	return res.Prefix, nil
}

// sayCut logs and counts a build that cached less than it was given.
func (l *Local) sayCut(seat string, cut homecache.Cut) {
	reason := "outside_home"
	if cut.Secret != "" {
		reason = "credential"
		localLog.Error("local_sandbox_home_refused", "seat", seat, "path", cut.Secret,
			"detail", "a cached home's build left a value the launch hands the box as a "+
				"credential, which the build is never handed; nothing of this seat's setup is "+
				"cached, and every step runs in each box until the setup or the image changes")
	} else {
		localLog.Warn("local_sandbox_home_cut", "seat", seat, "step", cut.Name, "paths", cut.Paths,
			"remedy", "a cached home keeps only the home: install what this step adds outside "+
				"it in providers.sandbox.local.image, or mark the step every_box so the build "+
				"skips it; until then it and every later step run in each box")
	}
	if l.opts.Homes != nil {
		l.opts.Homes.HomeCut(reason)
	}
}

// buildHome runs steps in a build box made for them and hands back its home
// with its container gone, and the box's directory for the caller to remove
// once the cache has taken the home. After each step the container's changes
// outside the home and /tmp are read: the first step with any ends the build
// ([homecache.Built.Escaped]).
func (l *Local) buildHome(ctx context.Context, env map[string]string, steps []SetupStep) (homecache.Built, string, error) {
	made, err := l.create(ctx, Spec{Placement: Container, Env: env}, createOptions{build: true})
	if err != nil {
		return homecache.Built{}, "", err
	}
	box, ok := made.(*containerBox)
	if !ok {
		return homecache.Built{}, "", fmt.Errorf("sandbox: a build box is a container's, not a %T", made)
	}
	stop := keepProvisioning(ctx, box)
	built, err := l.runBuild(ctx, box, env, steps)
	stop()
	if rerr := box.remove(context.WithoutCancel(ctx)); rerr != nil && err == nil {
		err = rerr
	}
	if err != nil {
		return homecache.Built{}, box.layout.root, err
	}
	built.Home = box.layout.home()
	return built, box.layout.root, nil
}

func (l *Local) runBuild(ctx context.Context, box *containerBox, env map[string]string, steps []SetupStep) (homecache.Built, error) {
	before, err := l.changed(ctx, box.container)
	if err != nil {
		return homecache.Built{}, err
	}
	for i, step := range steps {
		if err = ApplySetup(ctx, box, []SetupStep{step}, env); err != nil {
			return homecache.Built{}, err
		}
		after, derr := l.changed(ctx, box.container)
		if derr != nil {
			return homecache.Built{}, derr
		}
		if outside := escaped(before, after); len(outside) > 0 {
			return homecache.Built{Escaped: &homecache.Cut{Step: i, Name: step.Name, Paths: outside}}, nil
		}
		before = after
	}
	return homecache.Built{}, nil
}

// changed is every path the runtime reports changed in container since it
// started from its image (`diff`: "A /path", "C /path", "D /path").
func (l *Local) changed(ctx context.Context, container string) (map[string]bool, error) {
	res, err := runHost(ctx, l.cli.command(0, "diff", container))
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: reading what a build changed in its container: %w", err)
	}
	paths := map[string]bool{}
	for line := range strings.Lines(res.Stdout) {
		if _, path, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			paths[path] = true
		}
	}
	return paths, nil
}

// escaped is what after holds and before does not, outside the home and
// /tmp, sorted and at most [maxEscapedPaths].
func escaped(before, after map[string]bool) []string {
	var out []string
	for path := range after {
		if before[path] || within(path, DefaultHome) || within(path, "/tmp") {
			continue
		}
		out = append(out, path)
	}
	slices.Sort(out)
	return out[:min(len(out), maxEscapedPaths)]
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// imageID is the box image's ID, which keys a cached home: a tag moves when
// it is pulled again, and its homes with it. An image this node lacks is
// pulled first, as the box's own run would pull it (run's default
// --pull=missing): read before anything ran it, a fresh node has no ID to key
// its first box by, which would then run every step itself.
func (l *Local) imageID(ctx context.Context) (string, error) {
	id, err := l.inspectImage(ctx)
	if err == nil {
		return id, nil
	}
	res, perr := runHost(ctx, l.cli.command(0, "pull", l.opts.Image))
	if perr == nil && res.ExitCode != 0 {
		perr = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if perr != nil {
		return "", fmt.Errorf("sandbox: image %q is not on this node (%w) and could not be pulled: %w",
			l.opts.Image, err, perr)
	}
	return l.inspectImage(ctx)
}

// inspectImage is the ID of the box image as this node holds it.
func (l *Local) inspectImage(ctx context.Context) (string, error) {
	res, err := runHost(ctx, l.cli.command(0, "image", "inspect", "--format", "{{.Id}}", l.opts.Image))
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if id := strings.TrimSpace(res.Stdout); err == nil && id != "" {
		return id, nil
	}
	if err == nil {
		err = errors.New("the runtime named no ID")
	}
	return "", fmt.Errorf("sandbox: reading the ID of image %q: %w", l.opts.Image, err)
}

// buildEnv is what a build is handed of a launch's env: the identity facts,
// and each step's env value that is literal text and is the one the launch
// carries (a seat's own env may replace it with a reference).
func buildEnv(env map[string]string, steps []SetupStep) map[string]string {
	out := map[string]string{}
	for _, name := range identityFacts {
		if v, ok := env[name]; ok {
			out[name] = v
		}
	}
	for _, step := range steps {
		for key, value := range step.Env {
			if !envref.Has(value) && env[key] == value {
				out[key] = value
			}
		}
	}
	return out
}

// withheld is every variable of env the build is not handed.
func withheld(env, build map[string]string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if _, handed := build[key]; !handed {
			out = append(out, key)
		}
	}
	return out
}

func cachedSteps(steps []SetupStep) []homecache.Step {
	out := make([]homecache.Step, 0, len(steps))
	for _, s := range steps {
		out = append(out, homecache.Step{
			Name: s.Name, Files: s.Files, Commands: s.Commands, Env: s.Env,
			TimeoutSeconds: s.TimeoutSeconds, EveryBox: s.EveryBox,
		})
	}
	return out
}

// keepStamped stamps layout's box alive at once and every [provisionRefresh]
// until the returned stop, which waits out a stamp in flight: a box whose home
// is filled for minutes (its seat's build) is no orphan meanwhile.
func keepStamped(layout boxLayout) (stop func()) {
	touchAlive(layout)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(provisionRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				touchAlive(layout)
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}

// emptyHome removes what a failed copy left in home, but the box's own
// directories, so the box starts as if nothing were cached.
func emptyHome(home string) error {
	entries, err := os.ReadDir(home)
	if err != nil {
		return fmt.Errorf("sandbox: clearing a box's home after its cached copy failed: %w", err)
	}
	for _, e := range entries {
		if slices.Contains(homeCacheExclude, e.Name()) {
			continue
		}
		if err = os.RemoveAll(filepath.Join(home, e.Name())); err != nil {
			return fmt.Errorf("sandbox: clearing a box's home after its cached copy failed: %w", err)
		}
	}
	return nil
}

// CollectHomes removes from this node's cache every home no seat of owners
// holds, and what only those held ([homecache.Cache.Collect]). The engine
// calls it as a revision goes live, with the seats it runs in this cell.
func (l *Local) CollectHomes(owners []string) {
	if l.opts.Placement != Container {
		return
	}
	cache, err := l.homeCache()
	if err == nil {
		var removed []homecache.Key
		removed, err = cache.Collect(owners)
		if len(removed) > 0 {
			localLog.Info("local_sandbox_homes_collected", "homes", len(removed))
		}
	}
	if err != nil {
		localLog.Warn("local_sandbox_homes_uncollected", "error", err.Error(),
			"detail", "the next revision's collection tries again")
	}
}
