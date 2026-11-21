package boxhelper_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// requireCall is a CommonJS require of a literal module name.
var requireCall = regexp.MustCompile(`\brequire\(\s*['"]([^'"]+)['"]\s*\)`)

// serverLoads is why an installed tree is not the three packages the lock
// pins with the server loading the core alone, or nil: every module
// @playwright/mcp's entry (index.js) requires is playwright-core's; mcp
// depends on playwright and playwright-core alone, each at the core's
// release; playwright on playwright-core alone, at it; and the core on
// nothing. manifests is each package's package.json by its name.
func serverLoads(index []byte, manifests map[string][]byte) error {
	for _, m := range requireCall.FindAllSubmatch(index, -1) {
		if name := string(m[1]); name != "playwright-core" && !strings.HasPrefix(name, "playwright-core/") {
			return fmt.Errorf("@playwright/mcp's index.js requires %q, and its server loads playwright-core alone", name)
		}
	}
	core := browser.PlaywrightCoreRelease
	for name, want := range map[string]map[string]string{
		"@playwright/mcp": {"playwright": core, "playwright-core": core},
		"playwright":      {"playwright-core": core},
		"playwright-core": nil,
	} {
		var pkg struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if err := json.Unmarshal(manifests[name], &pkg); err != nil {
			return fmt.Errorf("%s's package.json: %w", name, err)
		}
		if len(pkg.Dependencies) == 0 {
			pkg.Dependencies = nil
		}
		if !maps.Equal(pkg.Dependencies, want) {
			return fmt.Errorf("%s depends on %v, and the tree is %v of it: the lock's three packages, the core "+
				"depending on nothing", name, pkg.Dependencies, want)
		}
	}
	return nil
}

// THE TREE IS THREE PACKAGES AND THE SERVER LOADS CORE ALONE: in the tree the
// suite installed from the lock (boxhelpertest.Tree), @playwright/mcp's entry
// requires playwright-core alone, mcp depends on playwright and the core at
// the core's release, playwright on the core alone, and the core on nothing,
// so the server the helper embeds runs on the core it loads, and playwright,
// the test runner, is installed and never loaded. Copies altered as a release
// breaking that would be are refused.
//
// Mutations, each red: read no require of the entry (the entry requiring
// playwright passes); read no dependency of the core (the core with one
// passes).
func TestTheTreeIsThreePackagesAndTheServerLoadsCoreAlone(t *testing.T) {
	t.Parallel()
	modules := filepath.Join(boxhelpertest.Tree(t), "node_modules")
	read := func(rel string) []byte {
		body, err := os.ReadFile(filepath.Join(modules, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	index := read("@playwright/mcp/index.js")
	manifests := map[string][]byte{}
	for _, name := range []string{"@playwright/mcp", "playwright", "playwright-core"} {
		manifests[name] = read(name + "/package.json")
	}
	if err := serverLoads(index, manifests); err != nil {
		t.Fatalf("the installed tree: %v", err)
	}
	withDependency := func(manifest []byte, name string) []byte {
		var m map[string]any
		if err := json.Unmarshal(manifest, &m); err != nil {
			t.Fatal(err)
		}
		deps, _ := m["dependencies"].(map[string]any)
		if deps == nil {
			deps = map[string]any{}
		}
		deps[name] = "1.0.0"
		m["dependencies"] = deps
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	altered := func(name string, manifest []byte) map[string][]byte {
		c := maps.Clone(manifests)
		c[name] = manifest
		return c
	}
	for _, tc := range []struct {
		name      string
		index     []byte
		manifests map[string][]byte
	}{
		{"an entry that requires the test runner", append(slices.Clone(index), "\nrequire('playwright')\n"...), manifests},
		{"a core with a dependency", index, altered("playwright-core",
			withDependency(manifests["playwright-core"], "chromium-bidi"))},
		{"a server with a fourth package", index, altered("@playwright/mcp", withDependency(manifests["@playwright/mcp"], "ws"))},
		{"a test runner with a second dependency", index, altered("playwright",
			withDependency(manifests["playwright"], "fsevents"))},
	} {
		if err := serverLoads(tc.index, tc.manifests); err == nil {
			t.Errorf("%s passes as the three packages the lock pins", tc.name)
		}
	}
}

// moduleTimer is a reading of time or a timer a module could make outside its
// clock: a global setTimeout, setInterval, clearTimeout or clearInterval,
// Date.now(), a new Date of now, performance.now(), AbortSignal.timeout(), or
// a timeout option, with which an API such as execFile runs a timer of its
// own.
var moduleTimer = regexp.MustCompile(`(^|[^.\w])(setTimeout|setInterval|clearTimeout|clearInterval)\s*\(|` +
	`\bDate\.now\s*\(|\bnew Date\(\s*\)|\bperformance\.now\s*\(|\bAbortSignal\.timeout\s*\(|\btimeout\s*:`)

// THE HELPER'S TIMERS ARE ITS CLOCK'S: every module reads time and sets
// timers through the clock it is handed, and only common.mjs's real clock
// reaches Node's own, so the probe, which may hand a module a clock of its
// own, drives every bound a module decides by; a timer a module set by itself,
// or handed an API as its timeout option, would escape it, and a case of that
// bound would wait on the wall clock. A program is asked its version through
// common.mjs's askVersion, which kills it on the clock.
//
// Mutations, each red: set install's bound with the global setTimeout; bound
// npm's --version with execFile's timeout option.
func TestTheHelpersTimersAreItsClock(t *testing.T) {
	t.Parallel()
	for _, f := range boxhelper.Files() {
		for i, line := range strings.Split(string(f.Body), "\n") {
			if !moduleTimer.MatchString(line) {
				continue
			}
			if f.Name == "common.mjs" && regexp.MustCompile(`^  (now|setTimeout|clearTimeout): `).MatchString(line) {
				continue
			}
			t.Errorf("%s:%d reads time or sets a timer outside its clock: %s", f.Name, i+1, strings.TrimSpace(line))
		}
	}
}
