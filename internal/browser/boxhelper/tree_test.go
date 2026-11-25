package boxhelper_test

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// fault is a file-system failure the probe stages: every call of method
// after the first after, or times of them where times is set, throws the
// error Node gives for errno, a name; with kill the first such call kills the
// probe; with uid it answers its stat with that owner; with kill holder it
// kills the Chromium the link it reads names, as a browser that dies while
// its launch is read; with kill reading it kills the browser whose command
// line it reads, and answers that command line less --no-sandbox; with blank
// reading it answers a browser's command line as nothing, as the kernel does
// once the browser's main thread exits; with hold it waits for the probe's
// release, then runs.
type fault struct {
	Method       string `json:"method"`
	Errno        string `json:"errno,omitempty"`
	After        int    `json:"after"`
	Times        int    `json:"times,omitempty"`
	Kill         bool   `json:"kill,omitempty"`
	UID          int    `json:"uid,omitempty"`
	KillHolder   bool   `json:"kill_holder,omitempty"`
	KillReading  bool   `json:"kill_reading,omitempty"`
	BlankReading bool   `json:"blank_reading,omitempty"`
	Hold         bool   `json:"hold,omitempty"`
}

// outcome is the probe's answer to install: where the tree is and whether
// that call installed it, or why it is withheld, and each npm it started.
type outcome struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Installed bool   `json:"installed"`
	Reason    string `json:"reason"`
	Cause     string `json:"cause"`
	Detail    string `json:"detail"`
	PIDs      []int  `json:"pids"`
}

// installCommand is the probe's command to install the box's tree, failing as
// fail says, and with abort its signal aborted as npm starts.
func installCommand(box *boxhelpertest.Box, fail *fault, abort bool) any {
	return struct {
		Op     string `json:"op"`
		Folder string `json:"folder"`
		Fail   *fault `json:"fail,omitempty"`
		Abort  bool   `json:"abort,omitempty"`
	}{"install", box.Folder, fail, abort}
}

// install has the box's own step install its tree, failing as fail says.
func install(p *boxhelpertest.Probe, box *boxhelpertest.Box, fail *fault) outcome {
	var got outcome
	p.Do(installCommand(box, fail, false), &got)
	return got
}

// nameOf matches the name config.json gives the box's tree under pw/.
func nameOf(t *testing.T, box *boxhelpertest.Box) *regexp.Regexp {
	t.Helper()
	c := box.Config(t)
	return regexp.MustCompile(`^` + regexp.QuoteMeta(fmt.Sprintf("%v-%v", c["pin"], c["pin_digest"])) + `-l[0-9]+$`)
}

// ready fails t unless got is the tree at the name config.json gives it under
// the box's pw/, installed by that call as now says.
func ready(t *testing.T, box *boxhelpertest.Box, got outcome, now bool) {
	t.Helper()
	if got.Reason != "" || !nameOf(t, box).MatchString(got.Name) ||
		got.Dir != filepath.Join(box.Folder, "pw", got.Name) || got.Installed != now {
		t.Fatalf("the install answered %+v; want the tree at pw/<pin>-<pin digest>-l<layout>, installed %t", got, now)
	}
}

// withheld fails t unless got withholds the browser as playwright_missing
// cause.
func withheld(t *testing.T, got outcome, cause string) {
	t.Helper()
	if got.Reason != "playwright_missing" || got.Cause != cause {
		t.Errorf("the install answered %+v, want playwright_missing %s", got, cause)
	}
}

// inPW is every entry of the box's pw/, none where there is no pw/.
func inPW(t *testing.T, box *boxhelpertest.Box) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(box.Folder, "pw"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// nothingIn fails t unless the box's pw/ holds no entry but want: no tree
// that was not to be published, and nothing an install staged.
func nothingIn(t *testing.T, box *boxhelpertest.Box, want ...string) {
	t.Helper()
	if got := inPW(t, box); !slices.Equal(got, want) {
		t.Errorf("pw/ holds %q, want %q: a tree is published whole or not at all, and nothing staged stays", got, want)
	}
}

// whole fails t unless the tree published at dir is exactly the lock: npm's
// record of what it installed is the lock's packages, its files are the
// committed digest's, nothing in it is a link, and every file and directory
// in it is the box's user's alone, a directory at 0700.
func whole(t *testing.T, dir string) {
	t.Helper()
	if odd := installedOtherwise(t, dir); len(odd) > 0 {
		t.Errorf("the tree is not the lock: %s", strings.Join(odd, "; "))
	}
	digest, err := installedDigest(filepath.Join(dir, "node_modules"))
	if err != nil {
		t.Errorf("the tree: %v", err)
	} else if want := committedInstalled(t); digest != want {
		t.Errorf("the tree's files have the digest %s, and the lock's whole install %s", digest, want)
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch perm := info.Mode().Perm(); {
		case !d.IsDir() && !d.Type().IsRegular():
			t.Errorf("%s is a %s, and a tree holds files and directories alone", path, d.Type())
		case d.IsDir() && perm != 0o700:
			t.Errorf("%s is %v, want 0700, the box's user's alone", path, perm)
		case perm&0o077 != 0:
			t.Errorf("%s is %v: a group or another user may use it", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// relink puts the case's own npm back at the box's bin/npm.
func relink(t *testing.T, box *boxhelpertest.Box, env boxhelpertest.Env) {
	t.Helper()
	npm := filepath.Join(box.Home, "bin", "npm")
	if err := os.Remove(npm); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(env.NPM, npm); err != nil {
		t.Fatal(err)
	}
}

// stopAtEnd stops each of pids' groups at t's end, while its leader is still
// the process it is now, so a stand-in a failed case left behind does not run
// on, and a pid handed out again costs no other group its life.
func stopAtEnd(t *testing.T, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		leader, err := procgroup.Identify(pid)
		if err != nil {
			continue
		}
		t.Cleanup(func() {
			if current, err := leader.Current(); err == nil && current {
				_ = procgroup.Kill(pid)
			}
		})
	}
}

// goneWithin bounds a wait for a killed process to go: past it, the kernel
// holds it, never a slow machine.
const goneWithin = time.Minute

// THE TREE IS INSTALLED WHOLE OR NOT AT ALL: an install killed part way
// leaves nothing under the tree's name, and the next one installs it; an
// installed tree is exactly the lock and its digest, with no link, every file
// and directory the box's user's alone (directories at 0700), so npm wrote
// under umask 077; it is reused with nothing read, even with no npm on the
// box, and installed again once its .complete is gone; and lock files in the
// folder that are edited or missing are digest_mismatch, nothing staged.
//
// Mutations, each red: install in place under the tree's name; reuse a tree
// with no .complete; ask npm its version before the reuse (the row with no
// npm is withheld); drop the umask.
func TestTheTreeIsInstalledWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		t.Run("an install killed part way leaves no tree under its name", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			box.Probe(t).Killed(installCommand(box, &fault{Method: "writeFile", After: 1, Kill: true}, false))
			for _, name := range inPW(t, box) {
				if nameOf(t, box).MatchString(name) {
					t.Errorf("pw/%s is there after an install killed part way: a tree is published whole", name)
				}
			}
			ready(t, box, install(box.Probe(t), box, nil), true)
		})

		t.Run("an installed tree is the lock, reused with nothing read", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			probe := box.Probe(t)
			got := install(probe, box, nil)
			ready(t, box, got, true)
			whole(t, got.Dir)
			nothingIn(t, box, got.Name)
			if info, err := os.Stat(filepath.Join(box.Folder, "pw")); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("pw/ is %v (%v), want 0700", info.Mode().Perm(), err)
			}

			if err := os.Remove(filepath.Join(box.Home, "bin", "npm")); err != nil {
				t.Fatal(err)
			}
			ready(t, box, install(probe, box, nil), false)

			relink(t, box, env)
			if err := os.Remove(filepath.Join(got.Dir, ".complete")); err != nil {
				t.Fatal(err)
			}
			ready(t, box, install(probe, box, nil), true)
		})

		t.Run("lock files edited or missing are not the lock", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			probe := box.Probe(t)
			lock := filepath.Join(box.Folder, "tree-package-lock.json")
			f, err := os.OpenFile(lock, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			withheld(t, install(probe, box, nil), "digest_mismatch")
			box.Write(t)
			if err := os.Remove(filepath.Join(box.Folder, "tree-package.json")); err != nil {
				t.Fatal(err)
			}
			withheld(t, install(probe, box, nil), "digest_mismatch")
			nothingIn(t, box)
			box.Write(t)
			ready(t, box, install(probe, box, nil), true)
		})
	})
}

// A TREE NPM LEFT SHORT IS NEVER PUBLISHED: npm answers 0 having installed
// other than the lock (a file it could not write is only a warning to it),
// so an install whose files are not the lock's whole install, a file
// missing, one cut short, or one more, is install_failed, naming the
// digest its files have, with nothing published and nothing staged. A
// stand-in runs the box's own npm, then alters what it installed.
//
// Mutation: skip the installed digest (each short tree is published).
func TestATreeNPMLeftShortIsNeverPublished(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct{ name, alter string }{
			{"a file npm left out", `rm "$DIR/node_modules/playwright-core/lib/webp_codec.wasm"`},
			{"a file npm cut short", `f="$DIR/node_modules/playwright-core/browsers.json"; ` +
				`head -c 64 "$f" >"$f.cut" && mv "$f.cut" "$f"`},
			{"a file npm never installed", `echo stray >"$DIR/node_modules/playwright-core/stray.js"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
				box.StandInNPM(t, "11.0.0", `"$REAL_NPM" "$@" || exit $?`+"\n"+tc.alter)
				got := install(box.Probe(t), box, nil)
				withheld(t, got, "install_failed")
				if !strings.Contains(got.Detail, "digest") {
					t.Errorf("the refusal says %q, which names no digest of the files npm installed", got.Detail)
				}
				nothingIn(t, box)
			})
		}
	})
}

// A BOX THAT CANNOT TAKE THE TREE SAYS WHY: a disk that fills, or a spent
// quota, is no_space; a read-only file system, a pw/ the box may not create
// in, one of another user it cannot hold, or a publish refused is unwritable,
// each playwright_missing. None leaves a tree or what it staged, so the next
// install, the fault gone, installs the tree. A disk no case can fill, a quota
// and a file system no case can make read-only are staged by the probe, as
// the errors Node gives for them (a quota's code is no EDQUOT: libuv names
// none). The first rename is npm's record's, the second the publish.
//
// Mutations, each red: read ENOSPC as unwritable; tell a quota by its code;
// leave what a failure staged.
func TestABoxThatCannotTakeTheTreeSaysWhy(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			fail   fault
			cause  string
			havePW bool
		}{
			{"a disk that fills", fault{Method: "writeFile", Errno: "ENOSPC"}, "no_space", false},
			{"a spent quota", fault{Method: "writeFile", Errno: "EDQUOT"}, "no_space", false},
			{"a read-only file system", fault{Method: "writeFile", Errno: "EROFS"}, "unwritable", false},
			{"a pw/ the box may not create in", fault{Method: "mkdtemp", Errno: "EACCES"}, "unwritable", false},
			{"a pw/ of another user", fault{Method: "chmod", Errno: "EPERM"}, "unwritable", true},
			{"a publish refused", fault{Method: "rename", Errno: "EACCES", After: 1}, "unwritable", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
				if tc.havePW {
					if err := os.Mkdir(filepath.Join(box.Folder, "pw"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				probe := box.Probe(t)
				withheld(t, install(probe, box, &tc.fail), tc.cause)
				nothingIn(t, box)
				ready(t, box, install(probe, box, nil), true)
			})
		}
	})
}

// AN INSTALLED TREE LOADS ONE PLAYWRIGHT CORE FOR ITS SERVER: required as the
// helper requires it, @playwright/mcp from the tree and playwright-core
// through mcp's own resolver, an installed tree hands over mcp's
// createConnection and the core's chromium.launchPersistentContext, and loads
// exactly one coreBundle.js, mcp's own, even with a second core nested under
// mcp, so a context the core launches is one the server accepts. On the
// floor's Node as on the machine's: the tree is loaded, never just installed.
//
// Mutation: require the core from the tree's root (two bundles load).
func TestAnInstalledTreeLoadsOnePlaywrightCoreForItsServer(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		tree := install(probe, box, nil)
		ready(t, box, tree, true)
		modules := filepath.Join(tree.Dir, "node_modules")
		nested := filepath.Join(modules, "@playwright", "mcp", "node_modules", "playwright-core")
		if err := os.CopyFS(nested, os.DirFS(filepath.Join(modules, "playwright-core"))); err != nil {
			t.Fatal(err)
		}
		var got struct {
			CreateConnection        string   `json:"create_connection"`
			LaunchPersistentContext string   `json:"launch_persistent_context"`
			Bundles                 []string `json:"bundles"`
		}
		probe.Do(struct {
			Op  string `json:"op"`
			Dir string `json:"dir"`
		}{"load", tree.Dir}, &got)
		resolved, err := filepath.EvalSymlinks(nested)
		if err != nil {
			t.Fatal(err)
		}
		bundle := filepath.Join(resolved, "lib", "coreBundle.js")
		if got.CreateConnection != "function" || got.LaunchPersistentContext != "function" ||
			!slices.Equal(got.Bundles, []string{bundle}) {
			t.Errorf("the installed tree loaded %+v, want both functions and the one bundle %s", got, bundle)
		}
	})
}

// hostile is every npm setting that, set by a box, would change what npm ci
// installs or what it does beside installing, each with the flag on the
// helper's command line that defeats it.
var hostile = []struct{ key, value string }{
	{"dry-run", "true"}, {"package-lock-only", "true"}, {"install-strategy", "linked"}, {"global", "true"},
	{"location", "global"}, {"prefer-online", "true"}, {"replace-registry-host", "never"}, {"bin-links", "true"},
	{"ignore-scripts", "false"}, {"loglevel", "an-unknown-level"}, {"audit", "true"}, {"fund", "true"},
	{"update-notifier", "true"}, {"workspaces", "true"}, {"logs-max", "10"}, {"progress", "true"},
	{"fetch-retries", "5"},
}

// lockPaths is each path the lock resolves a package's tarball at, as a
// request for it reads.
func lockPaths(t *testing.T) []string {
	t.Helper()
	_, lock := committedTree(t)
	var l struct {
		Packages map[string]struct {
			Resolved string `json:"resolved"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for key, p := range l.Packages {
		if key != "" {
			paths = append(paths, "GET "+strings.TrimPrefix(p.Resolved, "https://registry.npmjs.org"))
		}
	}
	slices.Sort(paths)
	return paths
}

// THE BOX'S NPM CONFIGURATION CANNOT CHANGE THE TREE: under every hostile
// setting, set once in the environment and once in the box's ~/.npmrc, a cold
// install from a stand-in registry is exactly the lock and its digest, with no
// link, the registry asked for the three tarballs and nothing else, and no
// log in the box's npm cache. Real egress is refused (a proxy on a closed
// port, the registry exempt), so no setting reaches the network.
//
// Mutations, each red: drop any one of --no-dry-run, --no-package-lock-only,
// --install-strategy=hoisted, --no-bin-links, --no-global, --location=user,
// --replace-registry-host=npmjs, --loglevel=error, --no-audit, --logs-max=0.
// The other flags change nothing here; the command line's case holds them.
func TestTheBoxsNPMConfigurationCannotChangeTheTree(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, where := range []string{"the environment", "the box's npmrc"} {
			t.Run(where, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone})
				registry := boxhelpertest.Registry(t, boxhelpertest.Serve)
				extra := []string{"NPM_CONFIG_REGISTRY=" + registry.URL, "NPM_CONFIG_HTTPS_PROXY=http://127.0.0.1:1",
					"NPM_CONFIG_PROXY=http://127.0.0.1:1", "NPM_CONFIG_NOPROXY=127.0.0.1"}
				var rc strings.Builder
				for _, s := range hostile {
					if where == "the environment" {
						extra = append(extra, "NPM_CONFIG_"+strings.ToUpper(strings.ReplaceAll(s.key, "-", "_"))+"="+s.value)
					} else {
						rc.WriteString(s.key + "=" + s.value + "\n")
					}
				}
				if err := os.WriteFile(filepath.Join(box.Home, ".npmrc"), []byte(rc.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				got := install(box.Probe(t, extra...), box, nil)
				ready(t, box, got, true)
				whole(t, got.Dir)
				if requests := slices.Sorted(slices.Values(registry.Requests())); !slices.Equal(requests, lockPaths(t)) {
					t.Errorf("the registry was asked %q, want exactly the lock's tarballs %q", requests, lockPaths(t))
				}
				if logs, err := os.ReadDir(filepath.Join(box.Home, ".npm", "_logs")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("npm logged into the box's cache: %v (%v)", logs, err)
				}
			})
		}
	})
}

// npmFlags is every flag the helper's npm ci carries but the bounds, as the
// package doc lists them.
var npmFlags = []string{"--ignore-scripts", "--install-strategy=hoisted", "--no-bin-links", "--no-workspaces",
	"--no-global", "--location=user", "--no-dry-run", "--no-package-lock-only", "--no-prefer-online",
	"--replace-registry-host=npmjs", "--no-audit", "--no-fund", "--no-update-notifier", "--no-progress",
	"--logs-max=0", "--loglevel=error", "--json"}

// NPM RUNS WITH EVERY FLAG AND BOUND ON ITS COMMAND LINE: the box's npm is
// asked its version with --loglevel=error, then run as `npm ci` with exactly
// the helper's flags and each bound at config.json's value (the case's own,
// so none is spelled in the helper), in the directory it installs into beside
// the tree's name, with no --cache, --offline or --registry, which would take
// the box's own cache or registry from it, nor --prefix, which would take its
// global npmrc. A stand-in records each command line and where npm ci ran,
// then runs the box's npm.
//
// Mutations, each red: any flag dropped; a bound read from anywhere but
// config.json; a --cache of the helper's own added; --prefix added.
func TestNPMRunsWithEveryFlagAndBoundOnItsCommandLine(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Config: map[string]any{
			"install": map[string]any{"timeout_ms": 61_000, "fetch_timeout_ms": 21_000, "fetch_retries": 2,
				"fetch_retry_wait_ms": 2_100},
		}})
		box.StandInNPM(t, "11.0.0", `printf '%s' "$DIR" >"$HOME/npm-ran-in"; exec "$REAL_NPM" "$@"`)
		got := install(box.Probe(t), box, nil)
		ready(t, box, got, true)
		calls := box.NPMCalls(t)
		if len(calls) != 2 || !slices.Equal(calls[0], []string{"--version", "--loglevel=error"}) {
			t.Fatalf("npm was run as %q, want --version --loglevel=error, then ci", calls)
		}
		ci := calls[1]
		ran, err := os.ReadFile(filepath.Join(box.Home, "npm-ran-in"))
		// Both read physically: a temporary directory may lie behind a link.
		dir, dirErr := filepath.EvalSymlinks(got.Dir)
		if len(ci) == 0 || ci[0] != "ci" || err != nil || dirErr != nil ||
			!regexp.MustCompile(`^`+regexp.QuoteMeta(dir)+`\.[^/]+$`).Match(ran) {
			t.Fatalf("npm ci was run as %q in %q (%v, %v), want ci in a directory of its own beside %s", ci, ran,
				err, dirErr, got.Dir)
		}
		rest := slices.Clone(ci[1:])
		want := slices.Concat(npmFlags, []string{"--fetch-timeout=21000", "--fetch-retries=2",
			"--fetch-retry-mintimeout=2100", "--fetch-retry-maxtimeout=2100"})
		slices.Sort(rest)
		slices.Sort(want)
		if !slices.Equal(rest, want) {
			t.Errorf("npm ci carried %q, want exactly %q", rest, want)
		}
	})
}

// imageCache is a copy of the suite tree's npm cache outside the box's home,
// read-only as an image's may be, less the tarball of skip if it is not "",
// and named by the box's global npmrc, as an image names its own. It answers
// the cache and a digest of everything in it.
func imageCache(t *testing.T, box *boxhelpertest.Box, skip string) (string, string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "npm-cache")
	if err := os.CopyFS(cache, os.DirFS(filepath.Join(boxhelpertest.Tree(t), "npm-cache"))); err != nil {
		t.Fatal(err)
	}
	if skip != "" {
		if err := os.Remove(cacheContent(t, cache, skip)); err != nil {
			t.Fatal(err)
		}
	}
	modes := func(file, dir fs.FileMode) {
		err := filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return os.Chmod(path, dir)
			}
			return os.Chmod(path, file)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	modes(0o444, 0o555)
	// t.TempDir's removal needs the directories writable again.
	t.Cleanup(func() { modes(0o644, 0o755) })
	if err := os.WriteFile(box.GlobalNPMRC, []byte("cache="+cache+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cache, cacheDigest(t, cache)
}

// cacheContent is where an npm cache keeps the tarball of the lock's package
// name: cacache's content-v2/sha512/<h[0:2]>/<h[2:4]>/<h[4:]>, with h its
// integrity's SHA-512 in hex.
func cacheContent(t *testing.T, cache, name string) string {
	t.Helper()
	_, lock := committedTree(t)
	var l struct {
		Packages map[string]struct {
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		t.Fatal(err)
	}
	h := sha512Hex(t, l.Packages["node_modules/"+name].Integrity)
	return filepath.Join(cache, "_cacache", "content-v2", "sha512", h[:2], h[2:4], h[4:])
}

// sha512Hex is a lock's integrity, sha512-<base64>, as the digest's hex.
func sha512Hex(t *testing.T, integrity string) string {
	t.Helper()
	sum, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha512-"))
	if err != nil || !strings.HasPrefix(integrity, "sha512-") || len(sum) != sha512.Size {
		t.Fatalf("the lock's integrity %q is not one SHA-512: %v", integrity, err)
	}
	return hex.EncodeToString(sum)
}

// cacheDigest is every entry under cache with its mode and bytes, in one
// string, so two readings differ wherever anything in it changed.
func cacheDigest(t *testing.T, cache string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v", path, info.Mode())
		if d.Type().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", body)
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A WARM CACHE INSTALLS WITH NO REQUEST: from the box's own npm cache, even
// with prefer-online in its settings, and from an image's read-only cache
// outside the home, named by its global npmrc, the tree installs with no
// request to a registry that would serve it, and the image's cache is left
// as it was; the same cache missing the core's tarball is unwritable, since
// npm writes what it fetches into the cache it reads, which is why an image's
// cache is writable by any user (the package doc).
//
// Mutations, each red: drop --no-prefer-online; pass a --cache of the
// helper's own; read npm's EACCES as install_failed; pass a --prefix (npm
// then reads its global npmrc from there).
func TestAWarmCacheInstallsWithNoRequest(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		t.Run("the box's own cache, preferring online", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			registry := boxhelpertest.Registry(t, boxhelpertest.Serve)
			got := install(box.Probe(t, "NPM_CONFIG_REGISTRY="+registry.URL, "NPM_CONFIG_PREFER_ONLINE=true"), box, nil)
			ready(t, box, got, true)
			if requests := registry.Requests(); len(requests) > 0 {
				t.Errorf("a box with the tree's packages in its cache asked the registry %q", requests)
			}
		})
		t.Run("an image's read-only cache", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone})
			cache, before := imageCache(t, box, "")
			registry := boxhelpertest.Registry(t, boxhelpertest.Serve)
			ready(t, box, install(box.Probe(t, "NPM_CONFIG_REGISTRY="+registry.URL), box, nil), true)
			if requests := registry.Requests(); len(requests) > 0 {
				t.Errorf("a box whose image's cache holds the tree's packages asked the registry %q", requests)
			}
			if cacheDigest(t, cache) != before {
				t.Errorf("the image's cache %s changed under the install", cache)
			}
		})
		t.Run("an image's read-only cache missing a tarball", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone})
			imageCache(t, box, "playwright-core")
			registry := boxhelpertest.Registry(t, boxhelpertest.Serve)
			withheld(t, install(box.Probe(t, "NPM_CONFIG_REGISTRY="+registry.URL), box, nil), "unwritable")
			nothingIn(t, box)
		})
	})
}

// A REGISTRY THAT FAILS NAMES ITS CAUSE: with nothing in the box's cache, a
// mirror lacking the release is registry_refused; a registry failing on its
// side, silent, stopping part way through a tarball, refusing the connection
// or answering https in plain HTTP (OpenSSL's ERR_SSL_WRONG_VERSION_NUMBER),
// and npm told to stay offline, are registry_unreachable; a tarball of other
// bytes is integrity_mismatch, refused by the lock's SHA-512 or, at its start,
// by zlib. Each is npm's own code, from its --json answer, and none leaves a
// tree or what it staged. The fetch bound (1 s) and retry wait (0.1 s) are the
// silent rows' subject.
//
// Mutations, each red: drop --json (every cause reads install_failed); read
// npm's status, not its code; read OpenSSL's codes or Z_DATA_ERROR as none.
func TestARegistryThatFailsNamesItsCause(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			mode    boxhelpertest.Mode
			refused bool     // the box's own refused registry, no stand-in
			https   bool     // the stand-in named by https:// at its plain port
			extra   []string // the box's npm settings
			cause   string
		}{
			{name: "a mirror lacking the release", mode: boxhelpertest.NotFound, cause: "registry_refused"},
			{name: "a registry failing on its side", mode: boxhelpertest.Fail, cause: "registry_unreachable"},
			{name: "a registry that never answers", mode: boxhelpertest.Hang, cause: "registry_unreachable"},
			{name: "a tarball that stops part way", mode: boxhelpertest.Stall, cause: "registry_unreachable"},
			{name: "a refused connection", refused: true, cause: "registry_unreachable"},
			{name: "an https address answered in plain HTTP", mode: boxhelpertest.Serve, https: true,
				cause: "registry_unreachable"},
			{name: "an npm told to stay offline", refused: true, extra: []string{"NPM_CONFIG_OFFLINE=true"},
				cause: "registry_unreachable"},
			{name: "a tarball of other bytes", mode: boxhelpertest.Corrupt, cause: "integrity_mismatch"},
			{name: "a tarball that does not inflate", mode: boxhelpertest.CorruptStream, cause: "integrity_mismatch"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone,
					Config: map[string]any{"install": map[string]any{"fetch_timeout_ms": 1_000, "fetch_retry_wait_ms": 100}}})
				extra := tc.extra
				if !tc.refused {
					registry := boxhelpertest.Registry(t, tc.mode).URL
					if tc.https {
						registry = "https://" + strings.TrimPrefix(registry, "http://")
					}
					extra = append(extra, "NPM_CONFIG_REGISTRY="+registry)
				}
				withheld(t, install(box.Probe(t, extra...), box, nil), tc.cause)
				nothingIn(t, box)
			})
		}
	})
}

// AN INSTALL PAST ITS BOUND IS STOPPED: npm against a registry that never
// answers, with its own fetch bound past the probe's minute, is stopped at
// install.timeout_ms (2 s, the case's subject) with TERM, and killed with its
// group stop_grace_ms (1 s) later, since npm defers its exit to a fetch
// outstanding: the answer is timeout, npm's group is gone and nothing is
// staged. An abort, a caller's deadline, kills an npm that ignores TERM at
// once, under a bound no case outlives.
//
// Mutations, each red: TERM without the KILL (npm runs past the probe's
// minute); an abort that sends TERM.
func TestAnInstallPastItsBoundIsStopped(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		t.Run("npm past install.timeout_ms", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone,
				Config: map[string]any{"install": map[string]any{"timeout_ms": 2_000, "fetch_timeout_ms": 600_000},
					"stop_grace_ms": 1_000}})
			registry := boxhelpertest.Registry(t, boxhelpertest.Hang)
			got := install(box.Probe(t, "NPM_CONFIG_REGISTRY="+registry.URL), box, nil)
			stopAtEnd(t, got.PIDs...)
			withheld(t, got, "timeout")
			for _, pid := range got.PIDs {
				if !procgrouptest.AwaitGone(t, pid, goneWithin) {
					t.Errorf("npm, pid %d, outlived its install's bound by %s", pid, goneWithin)
				}
			}
			nothingIn(t, box)
		})
		t.Run("npm when its caller aborts the install", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Config: map[string]any{
				"install": map[string]any{"timeout_ms": 3_600_000}}})
			// It ignores TERM, and stops by itself ten minutes on, past every
			// wait a case makes, if a failed case leaves it.
			box.StandInNPM(t, "11.0.0", "trap '' TERM\nexec sleep 600")
			var got outcome
			box.Probe(t).Do(installCommand(box, nil, true), &got)
			stopAtEnd(t, got.PIDs...)
			withheld(t, got, "timeout")
			if !strings.Contains(got.Detail, "deadline") || len(got.PIDs) != 1 {
				t.Errorf("the install answered %+v, want the deadline named and the one npm it started", got)
			}
			for _, pid := range got.PIDs {
				if !procgrouptest.AwaitGone(t, pid, goneWithin) {
					t.Errorf("npm, pid %d, outlived the abort by %s", pid, goneWithin)
				}
			}
			nothingIn(t, box)
		})
	})
}

// NPM IS REAPED BEFORE ITS DIRECTORY GOES: an npm that outlives its TERM, as a
// stand-in does that, on TERM, waits for the directory it runs in to go and
// then writes into it again by that absolute path, leaves nothing in
// pw/ once the install has answered and the stand-in is gone, since the
// helper kills npm's group and waits for it before it removes what npm was
// installing into. A helper that removed it sooner would see it made again,
// well inside the grace.
//
// Mutation: answer at the TERM, before npm is reaped (its directory goes
// while it runs).
func TestNPMIsReapedBeforeItsDirectoryGoes(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Config: map[string]any{
			"install": map[string]any{"timeout_ms": 1_000}, "stop_grace_ms": 1_000}})
		// It looks for its directory's removal every 50 ms, a twentieth of the
		// grace, so one before the KILL is seen inside it; and it stops by
		// itself ten minutes on, past every wait a case makes, if a failed
		// case leaves it.
		box.StandInNPM(t, "11.0.0", `trap 'while [ -e "$DIR" ]; do sleep 0.05; done; `+
			`mkdir -p "$DIR/node_modules/late"; exit 0' TERM`+"\ni=0\n"+
			`while [ "$i" -lt 600 ]; do sleep 1; i=$((i + 1)); done`)
		got := install(box.Probe(t), box, nil)
		stopAtEnd(t, got.PIDs...)
		withheld(t, got, "timeout")
		for _, pid := range got.PIDs {
			if !procgrouptest.AwaitGone(t, pid, goneWithin) {
				t.Fatalf("npm, pid %d, outlived its install by %s", pid, goneWithin)
			}
		}
		nothingIn(t, box)
	})
}

// sleeper starts a process in a group of its own that runs until t's end,
// standing for an npm a dead helper left, or a process its pid went to: its
// ten minutes outlast every case.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// identify is pid's start as the box's helper reads it, failing t unless the
// helper reads pid as running.
func identify(t *testing.T, probe *boxhelpertest.Probe, pid int) string {
	t.Helper()
	var got struct {
		Alive bool   `json:"alive"`
		Start string `json:"start"`
	}
	probe.Do(struct {
		Op  string `json:"op"`
		PID int    `json:"pid"`
	}{"identify", pid}, &got)
	if !got.Alive || got.Start == "" {
		t.Fatalf("the helper reads pid %d as %+v, want it running with a start", pid, got)
	}
	return got.Start
}

// plant writes a record of an npm that pid and start name into a directory
// beside the box's tree, as an install a dead helper left, and answers its
// name.
func plant(t *testing.T, box *boxhelpertest.Box, pid int, start string) string {
	t.Helper()
	c := box.Config(t)
	return plantAs(t, box, fmt.Sprintf("%v-%v-l1.left", c["pin"], c["pin_digest"]), pid, start)
}

// plantAs is plant into pw/'s directory name.
func plantAs(t *testing.T, box *boxhelpertest.Box, name string, pid int, start string) string {
	t.Helper()
	dir := filepath.Join(box.Folder, "pw", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"pid": pid, "start": start})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// AN INSTALL A DEAD HELPER LEFT RUNNING IS STOPPED: npm runs detached, so it
// outlives a helper killed outright, and the next install, before anything
// else, kills the group its record names while that pid is still the process
// recorded, then removes its directory: one left by a helper that died mid
// install, before the tree is installed; one beside a published tree, before
// that tree is reused; and a record whose pid now runs a process of another
// start, which is left running while the directory goes.
//
// Mutations, each red: skip the reap; reap after the reuse check (the row
// beside a published tree); judge by the pid alone (the bystander is killed).
func TestAnInstallADeadHelperLeftRunningIsStopped(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		t.Run("left by a helper that died mid install", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			// It waits for its record, which lands milliseconds after its
			// spawn, looking every 10 ms; kills its probe, as SIGKILL would a
			// helper; and stops by itself ten minutes on, past every wait a
			// case makes, if a failed case leaves it.
			box.StandInNPM(t, "11.0.0", `while [ ! -e "$DIR/install.json" ]; do sleep 0.01; done`+
				"\nkill -KILL $PPID\nexec sleep 600")
			box.Probe(t).Killed(installCommand(box, nil, false))
			left := inPW(t, box)
			if len(left) != 1 {
				t.Fatalf("pw/ holds %q after the helper died, want the one directory npm installs into", left)
			}
			raw, err := os.ReadFile(filepath.Join(box.Folder, "pw", left[0], "install.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				PID int `json:"pid"`
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatalf("its record %q: %v", raw, err)
			}
			stopAtEnd(t, record.PID)
			relink(t, box, env)
			got := install(box.Probe(t), box, nil)
			ready(t, box, got, true)
			if !procgrouptest.AwaitGone(t, record.PID, goneWithin) {
				t.Errorf("the npm a dead helper left, pid %d, still runs after the next install", record.PID)
			}
			nothingIn(t, box, got.Name)
		})
		t.Run("beside a published tree", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			probe := box.Probe(t)
			tree := install(probe, box, nil)
			ready(t, box, tree, true)
			orphan := sleeper(t)
			plant(t, box, orphan, identify(t, probe, orphan))
			ready(t, box, install(probe, box, nil), false)
			if !procgrouptest.AwaitGone(t, orphan, goneWithin) {
				t.Errorf("the npm a dead helper left beside the tree, pid %d, still runs after a reuse", orphan)
			}
			nothingIn(t, box, tree.Name)
		})
		t.Run("a record whose pid went to another process", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			probe := box.Probe(t)
			bystander := sleeper(t)
			plant(t, box, bystander, identify(t, probe, bystander)+"0")
			tree := install(probe, box, nil)
			ready(t, box, tree, true)
			if running, err := procgrouptest.Running(bystander); err != nil || !running {
				t.Errorf("pid %d, running another process than the record names, was stopped (%v)", bystander, err)
			}
			nothingIn(t, box, tree.Name)
		})
	})
}

// THE SWEEP LEAVES AN INSTALL WHOSE NPM STILL RUNS: once the browser is up,
// ensure's sweep removes every entry of pw/ but the tree, another release's
// tree and what installs left among them, but not one whose record names an
// npm the kernel still reports with the start recorded, as an npm the reap
// could not kill (the kernel holds it) leaves its directory: npm is reaped
// before its directory goes, so that one waits for the next reap. A record
// naming a pid gone, or a pid now another process's, is swept.
//
// Mutation: sweep every entry but the tree (the running npm's directory goes).
func TestTheSweepLeavesAnInstallWhoseNPMStillRuns(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		tree := install(probe, box, nil)
		ready(t, box, tree, true)
		ended := exec.Command("true")
		if err := ended.Run(); err != nil {
			t.Fatal(err)
		}
		npm := sleeper(t)
		running := plantAs(t, box, tree.Name+".running", npm, identify(t, probe, npm))
		swept := []string{
			plantAs(t, box, tree.Name+".gone", ended.Process.Pid, "a start of no process"),
			plantAs(t, box, tree.Name+".recycled", npm, identify(t, probe, npm)+"0"),
			"0.0.1-000000000000-l1",
		}
		if err := os.Mkdir(filepath.Join(box.Folder, "pw", swept[2]), 0o700); err != nil {
			t.Fatal(err)
		}
		var got struct{ Removed []string }
		probe.Do(map[string]any{"op": "sweep", "folder": box.Folder, "name": tree.Name}, &got)
		slices.Sort(got.Removed)
		slices.Sort(swept)
		if !slices.Equal(got.Removed, swept) {
			t.Errorf("the sweep removed %q, want %q", got.Removed, swept)
		}
		nothingIn(t, box, tree.Name, running)
	})
}

// THE HELPER NEVER SIGNALS INIT OR EVERY PROCESS: kill(2) reads a group
// signal to pid 1 as one to every process the box's user may signal, and pid
// 0 as the sender's own group, so the helper's signal reaches nobody at a pid
// of 1 or less, alone or as a group, while a live group it may signal is
// sent one; and the reap reads a record naming such a pid, which any process
// of the box's user can plant beside the tree, as naming no process. Every
// signal sent is 0, which kills nothing even with the guard gone.
//
// Mutations, each red: signal a pid of 1 or less (each refused row is sent);
// read a record naming pid 1 as naming it.
func TestTheHelperNeverSignalsInitOrEveryProcess(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Cache: boxhelpertest.CacheNone})
		probe := box.Probe(t)
		live := sleeper(t)
		for _, tc := range []struct {
			pid   int
			group bool
			sent  bool
		}{
			{1, true, false}, {1, false, false}, {0, true, false}, {0, false, false}, {-1, false, false},
			{live, true, true}, {live, false, true},
		} {
			var got struct {
				Sent bool `json:"sent"`
			}
			probe.Do(struct {
				Op    string `json:"op"`
				PID   int    `json:"pid"`
				Sig   int    `json:"sig"`
				Group bool   `json:"group"`
			}{"signal", tc.pid, 0, tc.group}, &got)
			if got.Sent != tc.sent {
				t.Errorf("signal 0 to pid %d, as a group %t, was sent %t, want %t", tc.pid, tc.group, got.Sent, tc.sent)
			}
		}
		for _, tc := range []struct {
			body string
			pid  int // 0: the record names no process
		}{
			{`{"pid":4242,"start":"boot:7"}`, 4242},
			{`{"pid":1,"start":"boot:7"}`, 0},
			{`{"pid":0,"start":"boot:7"}`, 0},
			{`{"pid":-4242,"start":"boot:7"}`, 0},
			{`{"pid":"4242","start":"boot:7"}`, 0},
			{`{"pid":4242}`, 0},
			{`not a record`, 0},
		} {
			var got struct {
				PID   int    `json:"pid"`
				Start string `json:"start"`
			}
			probe.Do(struct {
				Op   string `json:"op"`
				Body string `json:"body"`
			}{"record", tc.body}, &got)
			start := ""
			if tc.pid != 0 {
				start = "boot:7"
			}
			if got.PID != tc.pid || got.Start != start {
				t.Errorf("the reap reads the record %s as %+v, want pid %d", tc.body, got, tc.pid)
			}
		}
	})
}

// A BOX WITHOUT A USABLE NPM SAYS WHY: no npm on the box's PATH, or one it
// cannot run, whether asked its version or then started for npm ci, is
// npm_missing; an npm 9, an npm 9 prerelease, or one that answers --version
// with no version is npm_too_old, the majors compared as numbers; an npm 12
// prerelease is of its major and installs; and the box's own npm under a log
// level it does not know still answers its version, which it is asked with
// --loglevel=error, and installs.
//
// Mutations, each red: compare majors as strings; drop the prerelease's tag
// from the version's grammar; drop --loglevel=error from the version's
// question; read EACCES as npm_too_old when asked the version, or as
// install_failed when npm ci starts.
func TestABoxWithoutAUsableNPMSaysWhy(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			set   func(t *testing.T, box *boxhelpertest.Box)
			extra []string
			cause string // "" installs
		}{
			{"no npm", func(t *testing.T, box *boxhelpertest.Box) {
				if err := os.Remove(filepath.Join(box.Home, "bin", "npm")); err != nil {
					t.Fatal(err)
				}
			}, nil, "npm_missing"},
			{"an npm the box cannot run", func(t *testing.T, box *boxhelpertest.Box) {
				npm := filepath.Join(box.Home, "bin", "npm")
				if err := os.Remove(npm); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(npm, []byte("#!/bin/sh\necho 11.0.0\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}, nil, "npm_missing"},
			// It answers its version, then makes itself an npm the box cannot
			// run, so npm ci's spawn meets EACCES: the spawn error's mapping,
			// not the version's. The box's PATH is its bin alone, so chmod is
			// found on the system's after it.
			{"an npm the box cannot run once it answered its version", func(t *testing.T, box *boxhelpertest.Box) {
				npm := filepath.Join(box.Home, "bin", "npm")
				if err := os.Remove(npm); err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/sh\nPATH=\"$PATH:/usr/bin:/bin\"\nchmod 0644 \"$0\"\necho 11.0.0\n"
				if err := os.WriteFile(npm, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}, nil, "npm_missing"},
			{"an npm 9", func(t *testing.T, box *boxhelpertest.Box) { box.StandInNPM(t, "9.2.0", "exit 1") }, nil,
				"npm_too_old"},
			{"an npm 9 prerelease", func(t *testing.T, box *boxhelpertest.Box) {
				box.StandInNPM(t, "9.0.0-pre.1", "exit 1")
			}, nil, "npm_too_old"},
			{"an answer that is no version", func(t *testing.T, box *boxhelpertest.Box) {
				box.StandInNPM(t, "not a version", "exit 1")
			}, nil, "npm_too_old"},
			{"an npm 12 prerelease", func(t *testing.T, box *boxhelpertest.Box) {
				box.StandInNPM(t, "12.0.0-pre.1", `exec "$REAL_NPM" "$@"`)
			}, nil, ""},
			{"a log level npm does not know", func(*testing.T, *boxhelpertest.Box) {},
				[]string{"NPM_CONFIG_LOGLEVEL=an-unknown-level"}, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
				tc.set(t, box)
				got := install(box.Probe(t, tc.extra...), box, nil)
				if tc.cause == "" {
					ready(t, box, got, true)
					return
				}
				withheld(t, got, tc.cause)
				nothingIn(t, box)
			})
		}
	})
}

// npmError is npm's --json answer to a failure with code, as npm 10 and 11
// both print one: an object holding error's code, summary and detail, indented
// by two spaces (both read on a refused, a missing and a corrupt tarball).
func npmError(code string) string {
	body, _ := json.MarshalIndent(map[string]any{"error": map[string]any{"code": code,
		"summary": "npm's words for " + code, "detail": ""}}, "", "  ")
	return string(body)
}

// NPM'S FAILURES READ AS THEIR CAUSES: npm's --json answer is read for its
// code alone, each standing for the cause the package doc gives it (every
// OpenSSL code, EPROTO and Node's refusals of a certificate but OUT_OF_MEM are
// TLS failing, registry_unreachable; zlib's Z_DATA_ERROR, like EINTEGRITY, a
// tarball that is not the published one), and one that is no JSON, a code npm
// gives no meaning (UNKNOWN, as a spent quota reads) or none is install_failed.
// A stand-in that answers a code with exit 1 is that code's cause, and one
// that answers 0 having installed nothing is install_failed, nothing published.
//
// Mutations, each red: read E429 as registry_refused; EINTEGRITY, Z_DATA_ERROR,
// an OpenSSL code, EPROTO or a certificate refusal as install_failed; every
// ERR_ code, or OUT_OF_MEM, as registry_unreachable; publish on npm's status 0.
func TestNPMsFailuresReadAsTheirCauses(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		causes := map[string]string{"EINTEGRITY": "integrity_mismatch", "Z_DATA_ERROR": "integrity_mismatch",
			"E408": "registry_unreachable", "E429": "registry_unreachable", "E500": "registry_unreachable",
			"E503": "registry_unreachable",
			"E401": "registry_refused", "E403": "registry_refused", "E404": "registry_refused",
			"E407": "registry_refused", "EOTP": "registry_refused", "EAUTHIP": "registry_refused",
			"ENOSPC": "no_space", "EACCES": "unwritable", "EPERM": "unwritable", "EROFS": "unwritable",
			"UNKNOWN": "install_failed", "ECIGLOBAL": "install_failed", "E301": "install_failed",
			"ERR_INVALID_URL": "install_failed", "OUT_OF_MEM": "install_failed"}
		for _, code := range []string{"ECONNREFUSED", "ECONNRESET", "ENOTFOUND", "EAI_AGAIN", "ETIMEDOUT",
			"ENETUNREACH", "EHOSTUNREACH", "ENETDOWN", "ECONNABORTED", "EPIPE", "FETCH_ERROR", "ENOTCACHED",
			"EIDLETIMEOUT", "ECONNECTIONTIMEOUT", "ERESPONSETIMEOUT", "ETRANSFERTIMEOUT", "EINVALIDPROXY",
			"EMAXREDIRECT", "EINVALIDREDIRECT", "ENOREDIRECT", "ERR_TLS_CERT_ALTNAME_INVALID", "EPROTO",
			"ERR_SSL_WRONG_VERSION_NUMBER", "ERR_SSL_SSLV3_ALERT_HANDSHAKE_FAILURE",
			"ERR_SSL_TLSV1_ALERT_PROTOCOL_VERSION", "UNABLE_TO_GET_ISSUER_CERT", "UNABLE_TO_GET_CRL",
			"UNABLE_TO_DECRYPT_CERT_SIGNATURE", "UNABLE_TO_DECRYPT_CRL_SIGNATURE", "UNABLE_TO_DECODE_ISSUER_PUBLIC_KEY",
			"CERT_SIGNATURE_FAILURE", "CRL_SIGNATURE_FAILURE", "CERT_NOT_YET_VALID", "CERT_HAS_EXPIRED",
			"CRL_NOT_YET_VALID", "CRL_HAS_EXPIRED", "ERROR_IN_CERT_NOT_BEFORE_FIELD", "ERROR_IN_CERT_NOT_AFTER_FIELD",
			"ERROR_IN_CRL_LAST_UPDATE_FIELD", "ERROR_IN_CRL_NEXT_UPDATE_FIELD", "DEPTH_ZERO_SELF_SIGNED_CERT",
			"SELF_SIGNED_CERT_IN_CHAIN", "UNABLE_TO_GET_ISSUER_CERT_LOCALLY", "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
			"CERT_CHAIN_TOO_LONG", "CERT_REVOKED", "INVALID_CA", "PATH_LENGTH_EXCEEDED", "INVALID_PURPOSE",
			"CERT_UNTRUSTED", "CERT_REJECTED", "HOSTNAME_MISMATCH"} {
			causes[code] = "registry_unreachable"
		}
		answers := map[string]string{"no JSON": "npm error code E404", "no code": `{"error":{"summary":"no code"}}`,
			"a code that is no text": `{"error":{"code":404}}`}
		for code := range causes {
			answers[code] = npmError(code)
		}
		for what, stdout := range answers {
			want, ok := causes[what]
			if !ok {
				want = "install_failed"
			}
			var got struct {
				Code  string `json:"code"`
				Cause string `json:"cause"`
			}
			probe.Do(struct {
				Op     string `json:"op"`
				Stdout string `json:"stdout"`
			}{"npm_cause", stdout}, &got)
			if got.Cause != want {
				t.Errorf("npm's answer %s reads as %+v, want %s", what, got, want)
			}
		}

		for _, tc := range []struct{ name, body, cause string }{
			{"ENOSPC with exit 1", "echo '{\"error\":{\"code\":\"ENOSPC\"}}'\nexit 1", "no_space"},
			{"EACCES with exit 1", "echo '{\"error\":{\"code\":\"EACCES\"}}'\nexit 1", "unwritable"},
			{"exit 0 with nothing installed", "exit 0", "install_failed"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
				box.StandInNPM(t, "11.0.0", tc.body)
				withheld(t, install(box.Probe(t), box, nil), tc.cause)
				nothingIn(t, box)
			})
		}
	})
}
