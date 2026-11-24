package harness

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// executorRelease holds the harness a direct box runs, this host's own, to its
// runner's floor and probe (harnessprofile.Executor's Floor and ProbeArgs),
// version being what this host's harness printed. A container box runs its
// image's harness, which the run's own script holds to the floor at each
// run, so there it is a line and never a problem. Nothing for an entry no
// executor chain holds, a harness with no runner, or a runner stating neither.
func (p *Provider) executorRelease(ctx context.Context, opts DiagnoseOptions, version string) (line, problem string) {
	runner, ok := harnessprofile.ExecutorOf(p.harness)
	if !ok || (runner.Floor == "" && len(runner.ProbeArgs) == 0) || len(opts.ExecutorSeats) == 0 ||
		!slices.Contains(opts.ExecutorHarnesses, p.harness) {
		return "", ""
	}
	if opts.Cell != directCell {
		return fmt.Sprintf("release: a box's own %s is held to %s at each run", p.profile.Binary,
			orNone(runner.Floor)), ""
	}
	if version == "" {
		// The version check already says why none was read.
		return "", ""
	}
	if runner.Floor != "" && harnessprofile.Release(version) == "" {
		return "release: " + version, fmt.Sprintf("this host's %s printed %q, which names no release its "+
			"executor runner reads (a bare MAJOR.MINOR.PATCH, never a pre-release), so every run would be "+
			"refused: a direct box runs this host's own, so install a release at or past %s before a seat "+
			"launches on providers.llm.%s", p.profile.Binary, version, runner.Floor, p.key)
	}
	if runner.Floor != "" && !releaseAtLeast(version, runner.Floor) {
		return "release: " + version, fmt.Sprintf("this host's %s is %s, and its executor runner drives %s "+
			"or later: a direct box runs this host's own, so upgrade it before a seat launches on "+
			"providers.llm.%s", p.profile.Binary, version, runner.Floor, p.key)
	}
	if len(runner.ProbeArgs) > 0 {
		exit, err := p.probeExit(ctx, runner.ProbeArgs)
		said := string(p.profile.Binary) + " " + strings.Join(runner.ProbeArgs, " ")
		switch {
		case err != nil:
			return "release: " + version, fmt.Sprintf("`%s` could not be started: %v", said, err)
		case exit != 0:
			return "release: " + version, fmt.Sprintf("`%s` exited %d: this host's %s lacks what its "+
				"executor runner starts, so a direct box's runs on providers.llm.%s would fail; install a "+
				"release at or past %s", said, exit, p.profile.Binary, p.key, orNone(runner.Floor))
		}
	}
	if runner.Floor != "" && !releaseAtLeast(runner.Floor, version) {
		// NEWER THAN MEASURED RUNS, its sessions fresh: only a release the
		// runner was measured on continues a session another box packed.
		return fmt.Sprintf("release: %s, newer than the runner's %s: it runs, each run in a fresh session",
			version, runner.Floor), ""
	}
	return fmt.Sprintf("release: %s, the runner's %s", version, orNone(runner.Floor)), ""
}

// probeExit runs the harness with args as the version probe runs it
// ([Provider.probeVersion]) and answers its exit status.
func (p *Provider) probeExit(ctx context.Context, args []string) (int, error) {
	captures, err := p.ws.capturesDir()
	if err != nil {
		return 0, err
	}
	res, err := run(ctx, invocation{
		binary: p.profile.Binary, args: args, env: p.probeEnv(), timeout: probeTimeout,
		grace: p.termGrace, outDir: captures, setting: p.stateSetting(), hooks: p.hooks, janitor: &p.janitor,
	})
	if err != nil {
		return 0, err
	}
	if why := res.unfinished(); why != "" {
		return 0, fmt.Errorf("it did not finish: %s", why)
	}
	return res.exitCode, nil
}

// releaseAtLeast reports whether version, as a harness printed it, is at or
// past floor, MAJOR.MINOR.PATCH, each read as the run's script reads it
// ([harnessprofile.Release]); one that names no release meets no floor.
func releaseAtLeast(version, floor string) bool {
	have, ok := releaseParts(version)
	if !ok {
		return false
	}
	want, ok := releaseParts(floor)
	if !ok {
		return false
	}
	return slices.Compare(have[:], want[:]) >= 0
}

// releaseParts is the three numbers of the bare release s names, false where
// it names none.
func releaseParts(s string) ([3]int, bool) {
	var out [3]int
	release := harnessprofile.Release(s)
	if release == "" {
		return out, false
	}
	for i, field := range strings.SplitN(release, ".", 3) {
		n, err := strconv.Atoi(field)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
