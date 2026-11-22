package boxhelper_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE LOCKED CORE EXPECTS THE CHROMIUM SOMEBODY READ: the browsers.json of the
// playwright-core the suite installed from the lock, the only place the build
// is written, names one chromium, of internal/browser's
// ExpectedChromiumRevision and ExpectedChromiumVersion, with no
// revisionOverrides. Mutations, each red: ExpectedChromiumVersion
// "159.0.8154.7"; ExpectedChromiumRevision "1256".
func TestTheLockedCoreExpectsTheChromiumSomebodyRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(boxhelpertest.Tree(t), "node_modules", "playwright-core", "browsers.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := chromiumOf(body)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if got.Revision != browser.ExpectedChromiumRevision || got.BrowserVersion != browser.ExpectedChromiumVersion {
		t.Errorf("the locked playwright-core expects Chromium %s, revision %s, and internal/browser expects %s, "+
			"revision %s: read the release, then move ExpectedChromiumVersion and ExpectedChromiumRevision "+
			"(CONTRIBUTING.md, \"A Playwright MCP bump is read before it lands\")", got.BrowserVersion, got.Revision,
			browser.ExpectedChromiumVersion, browser.ExpectedChromiumRevision)
	}
}

// browserEntry is one entry of playwright-core's browsers.json.
type browserEntry struct {
	Name              string          `json:"name"`
	Revision          string          `json:"revision"`
	BrowserVersion    string          `json:"browserVersion"`
	RevisionOverrides json.RawMessage `json:"revisionOverrides"`
}

// chromiumOf is the one chromium entry of a browsers.json, or an error saying
// why there is not exactly one plain one: revisionOverrides rename the
// chromium-<revision> directory Playwright looks for on some platforms, so the
// revision named would not be the build a box looks for.
func chromiumOf(body []byte) (browserEntry, error) {
	var doc struct {
		Browsers []browserEntry `json:"browsers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return browserEntry{}, err
	}
	var found []browserEntry
	for _, b := range doc.Browsers {
		if b.Name == "chromium" {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		return browserEntry{}, fmt.Errorf("it names chromium %d times, where once is the build playwright-core "+
			"expects", len(found))
	}
	if len(found[0].RevisionOverrides) > 0 && string(found[0].RevisionOverrides) != "null" {
		return browserEntry{}, fmt.Errorf("its chromium has revisionOverrides %s, which rename the build Playwright "+
			"looks for by platform", found[0].RevisionOverrides)
	}
	return found[0], nil
}

// A BROWSERS.JSON IS READ FOR ONE PLAIN CHROMIUM OR REFUSED: none, two, or one
// whose revision moves by platform is no single build to expect. Mutations,
// each red: take the first chromium of two; ignore revisionOverrides.
func TestABrowsersJSONNamesOnePlainChromiumOrIsRefused(t *testing.T) {
	t.Parallel()
	const chromium = `{"name": "chromium", "revision": "1255", "browserVersion": "159.0.8154.6"}`
	const webkit = `{"name": "webkit", "revision": "2395", "revisionOverrides": {"mac14": "2251"}}`
	for _, tc := range []struct{ name, browsers, refusal string }{
		{"one chromium beside other browsers", chromium + "," + webkit, ""},
		{"no chromium", webkit, "names chromium 0 times"},
		{"two chromiums", chromium + "," + chromium, "names chromium 2 times"},
		{"a chromium whose revision moves by platform",
			`{"name": "chromium", "revision": "1255", "revisionOverrides": {"mac14": "1200"}}`, "revisionOverrides"},
	} {
		got, err := chromiumOf([]byte(`{"browsers": [` + tc.browsers + `]}`))
		switch {
		case tc.refusal == "" && (err != nil || got.Revision != "1255" || got.BrowserVersion != "159.0.8154.6"):
			t.Errorf("%s: read %+v, %v; want revision 1255 of 159.0.8154.6", tc.name, got, err)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: read %+v, %v; want a refusal saying %q", tc.name, got, err, tc.refusal)
		}
	}
}

// installTimeout bounds one offline install: it takes about half a second, so
// a minute is a hang, never a slow machine.
const installTimeout = time.Minute

// THE LOCK INSTALLS WITH EACH NODE'S OWN NPM, offline, from the npm cache of
// the suite's tree: the machine's npm under the machine's node, both held to
// the floor, and the floor's under the floor's node, each `npm ci` into a
// directory of its own, with a refused registry and none of the developer's
// npm settings, ends 0 having installed exactly the lock's packages, so a box
// on the oldest Node and npm the helper supports installs the lock it is
// handed. Mutations, each red: node.sh keeping no npm (FloorNPM fails); a
// tarball deleted from the cache's copy, or an integrity flipped in the lock
// installed, which an offline npm looks a tarball up by (ENOTCACHED).
func TestTheLockInstallsWithEachNodesOwnNpm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		npm  func(testing.TB) []string // the command that runs that npm
	}{
		// The machine's npm runs under whichever node PATH finds, so that node is
		// held to the floor too.
		{"newest", func(t testing.TB) []string { boxhelpertest.Node(t); return []string{boxhelpertest.NPM(t)} }},
		{"floor", func(t testing.TB) []string { return []string{boxhelpertest.Floor(t), boxhelpertest.FloorNPM(t)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := boxhelpertest.Tree(t)
			npm := tc.npm(t)
			dir, home := t.TempDir(), t.TempDir()
			cache := filepath.Join(t.TempDir(), "npm-cache")
			if err := os.CopyFS(cache, os.DirFS(filepath.Join(tree, "npm-cache"))); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"package.json", "package-lock.json"} {
				body, err := os.ReadFile(filepath.Join(tree, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Two npmrc files, empty and distinct: npm refuses to read one file as both.
			userrc, globalrc := filepath.Join(home, ".npmrc"), filepath.Join(home, "global-npmrc")
			for _, rc := range []string{userrc, globalrc} {
				if err := os.WriteFile(rc, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, npm[0], slices.Concat(npm[1:], []string{"ci", "--prefix", dir,
				"--cache", cache, "--offline", "--registry", "http://127.0.0.1:1/", "--userconfig", userrc,
				"--globalconfig", globalrc, "--ignore-scripts", "--install-strategy=hoisted", "--no-bin-links",
				"--no-audit", "--no-fund", "--no-update-notifier"})...)
			cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
				name, _, _ := strings.Cut(kv, "=")
				return name == "NODE_OPTIONS" || name == "HOME" || strings.HasPrefix(strings.ToLower(name), "npm_config_")
			}), "HOME="+home)
			cmd.WaitDelay = 5 * time.Second
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s ci, offline from the tree's npm cache: %v (%v), saying:\n%s", strings.Join(npm, " "), err,
					ctx.Err(), out)
			}
			if odd := installedOtherwise(t, dir); len(odd) > 0 {
				t.Errorf("%s ci installed other than the lock: %s", strings.Join(npm, " "), strings.Join(odd, "; "))
			}
		})
	}
}

// installedOtherwise is how the packages npm recorded installing in dir
// (node_modules/.package-lock.json) differ from dir's lock: each key either
// lacks, or whose version, address or SHA-512 differs.
func installedOtherwise(t *testing.T, dir string) []string {
	t.Helper()
	read := func(path string) map[string]map[string]any {
		var lock struct {
			Packages map[string]map[string]any `json:"packages"`
		}
		readJSON(t, path, &lock)
		return lock.Packages
	}
	want, got := read(filepath.Join(dir, "package-lock.json")), read(filepath.Join(dir, "node_modules", ".package-lock.json"))
	delete(want, "")
	keys := map[string]bool{}
	for key := range want {
		keys[key] = true
	}
	for key := range got {
		keys[key] = true
	}
	var odd []string
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		for _, field := range []string{"version", "resolved", "integrity"} {
			if w, g := want[key][field], got[key][field]; w == nil || w != g {
				odd = append(odd, fmt.Sprintf("%s's %s: locked %v, installed %v", key, field, w, g))
			}
		}
	}
	return odd
}

// THE INSTALLED DIGEST NAMES THE FILES THE LOCK INSTALLS: tree/installed.sha256
// is one line, the digest of the files npm installed under the suite tree's
// node_modules (boxhelpertest.Tree) as a box's install computes it before it
// publishes a tree (js/tree.mjs's installedDigest): the hex SHA-256 over one
// line per regular file, npm's own record aside, <path>\0<hex SHA-256>\n, the
// path relative and slash-separated, in byte order. This is the definition's
// second implementation, which holds the box's: every install case checks a
// box's tree against the same file. A bump moves it, and this names the value.
//
// Mutations, each red: one digit of the file changed; the digest here taken
// over the paths without the files' bytes.
func TestTheInstalledDigestNamesTheFilesTheLockInstalls(t *testing.T) {
	t.Parallel()
	committed := committedInstalled(t)
	tree := boxhelpertest.Tree(t)
	got, err := installedDigest(filepath.Join(tree, "node_modules"))
	if err != nil {
		t.Fatalf("the suite's tree, %s: %v", tree, err)
	}
	if got != committed {
		t.Errorf("the files the lock installs (the suite's tree, %s) have the digest %s, and "+
			"internal/browser/boxhelper/tree/installed.sha256 names %s: write %s there (CONTRIBUTING.md, \"A "+
			"Playwright MCP bump is read before it lands\")", tree, got, committed, got)
	}
}

// committedInstalled is the digest tree/installed.sha256 names, failing t
// unless the file is that one line: 64 lowercase hex digits and a newline.
func committedInstalled(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "internal", "browser", "boxhelper", "tree", "installed.sha256")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(raw) {
		t.Fatalf("%s holds %q, and it is one line: the digest's 64 lowercase hex digits", path, raw)
	}
	return strings.TrimSuffix(string(raw), "\n")
}

// installedDigest is the digest of the files under nodeModules, in Go, as
// js/tree.mjs's installedDigest defines it; anything there that is neither a
// file nor a directory, which no whole install holds, is an error naming it.
func installedDigest(nodeModules string) (string, error) {
	var lines [][]byte
	err := filepath.WalkDir(nodeModules, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(nodeModules, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is a %s, neither a file nor a directory", rel, d.Type())
		case rel == ".package-lock.json":
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		lines = append(lines, []byte(rel+"\x00"+hex.EncodeToString(sum[:])+"\n"))
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.SortFunc(lines, bytes.Compare)
	sum := sha256.Sum256(bytes.Join(lines, nil))
	return hex.EncodeToString(sum[:]), nil
}
