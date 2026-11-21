package boxhelpertest

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// exited is the identity of a process that has exited: reaped, or left a
// zombie until t ends.
func exited(t *testing.T, reap bool) procgroup.Leader {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leader, err := procgroup.Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if reap {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return leader
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	if !procgrouptest.AwaitGone(t, cmd.Process.Pid, time.Minute) {
		t.Fatalf("pid %d, sh -c 'exit 0', did not exit", cmd.Process.Pid)
	}
	return leader
}

// A LEFTOVER HOME IS SWEPT ONLY ONCE ITS TEST BINARY IS GONE: a home whose
// owner file names a process the kernel no longer reports, one that exited and
// waits to be reaped, or a pid running with another start time is removed;
// this binary's own home, which no process group is led by, is kept, and so is
// a home it cannot judge (no owner file, one that names no process) and a
// directory that is no home of Start's.
//
// Mutations, each red: judge the owner by its pid alone (the recycled pid's
// home is kept); by procgroup.Leader.Current (this binary's own home is
// swept); count a zombie as running (its home is kept).
func TestALeftoverHomeIsSweptOnlyOnceItsTestBinaryIsGone(t *testing.T) {
	t.Parallel()
	me, err := self()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	homes := []struct {
		name, owner string
		kept        bool
	}{
		{"cb-this-binary", me.String(), true},
		{"cb-reaped", exited(t, true).String(), false},
		{"cb-zombie", exited(t, false).String(), false},
		{"cb-recycled", procgroup.Leader{PID: me.PID, Start: me.Start + "0"}.String(), false},
		{"cb-unowned", "", true},
		{"cb-garbled", "not a test binary", true},
		{"other-reaped", exited(t, true).String(), true},
	}
	for _, h := range homes {
		if err := os.Mkdir(filepath.Join(dir, h.name), 0o700); err != nil {
			t.Fatal(err)
		}
		if h.owner != "" {
			if err := os.WriteFile(filepath.Join(dir, h.name, ownerFile), []byte(h.owner+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := sweep(dir); err != nil {
		t.Fatal(err)
	}
	for _, h := range homes {
		_, err := os.Stat(filepath.Join(dir, h.name))
		if gone := errors.Is(err, fs.ErrNotExist); gone == h.kept || (err != nil && !gone) {
			t.Errorf("%s, owned by %q: kept %t (%v), want kept %t", h.name, h.owner, !gone, err, h.kept)
		}
	}
}

// npmWait bounds one npm a case runs in a box: an offline install takes about
// half a second and a question of npm's settings less, so a minute is a hang.
const npmWait = time.Minute

// npmIn runs the box's own npm in dir with args, in the box's environment and
// extra after it, and answers what it printed, failing t if it fails.
func npmIn(t *testing.T, b *Box, dir string, extra []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), npmWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(b.Home, "bin", "npm"), args...)
	// npm's pipes close as it exits; a second past that, a child of its still
	// holding them is cut rather than hung.
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, append(b.Environ(), extra...), time.Second
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			err = errors.Join(err, errors.New(string(exit.Stderr)))
		}
		t.Fatalf("the box's npm %s: %v; its stdout:\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// A BOX RUNS ITS NODE'S OWN NPM OVER THE SUITE'S CACHE: Start links the box's
// bin/npm to the case's npm, which runs under the box's own node (the floor's
// npm-cli.js too, its `env node` finding the box's bin alone); copies the
// suite tree's npm cache to the box's ~/.npm; and writes an empty global
// npmrc, which Environ names beside a registry nothing answers at. So the
// box's npm reads that npmrc and never the machine's, and installs the lock
// with no request.
//
// Mutations, each red: Start skipping the cache's copy (the install meets
// the refused registry); Environ naming no NPM_CONFIG_GLOBALCONFIG (the
// box's npmrc unread).
func TestABoxRunsItsNodesOwnNPMOverTheSuitesCache(t *testing.T) {
	t.Parallel()
	EachNode(t, func(t *testing.T, env Env) {
		t.Parallel()
		b := Start(t, env, Options{})
		if got, err := os.Readlink(filepath.Join(b.Home, "bin", "npm")); err != nil || got != env.NPM {
			t.Errorf("the box's bin/npm links to %q (%v), want the case's npm %s", got, err, env.NPM)
		}
		if rc, err := os.ReadFile(b.GlobalNPMRC); err != nil || len(rc) > 0 {
			t.Errorf("the box's global npmrc holds %q (%v), want it empty", rc, err)
		}
		node := versionOf(t, nodeAnswer, env.Node, nodeRemedy, env.Node).version
		var versions map[string]string
		if err := json.Unmarshal([]byte(npmIn(t, b, b.Home, nil, "version", "--json")), &versions); err != nil {
			t.Fatal(err)
		}
		if versions["node"] != node {
			t.Errorf("the box's npm runs under Node %s, and the box's node is %s", versions["node"], node)
		}

		if err := os.WriteFile(b.GlobalNPMRC, []byte("init-author-name=a-box\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(npmIn(t, b, b.Home, nil, "config", "get", "init-author-name")); got != "a-box" {
			t.Errorf("the box's npm reads init-author-name %q, and the box's global npmrc sets a-box: it reads "+
				"another global npmrc", got)
		}
		if err := os.WriteFile(b.GlobalNPMRC, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		dir := t.TempDir()
		for _, name := range []string{"package.json", "package-lock.json"} {
			body, err := os.ReadFile(filepath.Join(Tree(t), name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		npmIn(t, b, dir, nil, "ci", "--ignore-scripts", "--no-bin-links", "--no-audit", "--no-fund",
			"--no-update-notifier", "--logs-max=0")
		entries, err := os.ReadDir(filepath.Join(dir, "node_modules"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if want := []string{".package-lock.json", "@playwright", "playwright", "playwright-core"}; !slices.Equal(names, want) {
			t.Errorf("the box's npm installed %q, want %q", names, want)
		}
	})
}
