package boxhelper_test

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE LOCK IS THE RELEASE SOMEBODY READ: tree/package.json names
// @playwright/mcp alone, at internal/browser.PlaywrightMCPRelease, and
// tree/package-lock.json pins it and the two packages it pins exactly at
// PlaywrightCoreRelease, by the registry's own address and a SHA-512, none with
// a flag the package doc refuses or an engines.node above MinNodeMajor. So a
// bump of the lock alone, as Dependabot's is, fails until a person moves them.
// lockProblems says how a tree falls short, and
// TestALockShortOfTheReleaseIsRefused holds each of its checks.
func TestTheLockIsTheReleaseSomebodyRead(t *testing.T) {
	t.Parallel()
	manifest, lock := committedTree(t)
	for _, problem := range lockProblems(manifest, lock, browser.PlaywrightMCPRelease, browser.PlaywrightCoreRelease) {
		t.Error(problem)
	}
}

// committedTree is the bytes of tree/package.json and tree/package-lock.json.
func committedTree(t *testing.T) (manifest, lock []byte) {
	t.Helper()
	dir := filepath.Join(sourcetree.Root(t), "internal", "browser", "boxhelper", "tree")
	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err = os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	return manifest, lock
}

// lockProblems is each way a manifest and its lock fall short of the releases
// somebody read, mcp of @playwright/mcp and core of the two packages it pins,
// in the words of a failure.
func lockProblems(manifestJSON, lockJSON []byte, mcp, core string) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var manifest map[string]any
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return []string{fmt.Sprintf("tree/package.json is no JSON: %v", err)}
	}
	deps := map[string]any{"@playwright/mcp": mcp}
	want := map[string]any{"name": "crewlet-browser-tree", "private": true, "dependencies": deps}
	if !reflect.DeepEqual(manifest, want) {
		fail("tree/package.json is %s, want %s: @playwright/mcp alone, at internal/browser.PlaywrightMCPRelease "+
			"(CONTRIBUTING.md, \"A Playwright MCP bump is read before it lands\")", asJSON(manifest), asJSON(want))
	}

	var lock struct {
		LockfileVersion int                       `json:"lockfileVersion"`
		Packages        map[string]map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(lockJSON, &lock); err != nil {
		return append(problems, fmt.Sprintf("tree/package-lock.json is no lock npm writes: %v", err))
	}
	if lock.LockfileVersion != 3 {
		fail("tree/package-lock.json is lockfileVersion %d, want 3, which every npm from 9 writes", lock.LockfileVersion)
	}
	if root := lock.Packages[""]; !reflect.DeepEqual(root, map[string]any{"name": want["name"], "dependencies": deps}) {
		fail("the lock's root is %s, and package.json names %s: regenerate the lock (CONTRIBUTING.md)", asJSON(root),
			asJSON(deps))
	}
	// Each package the lock may hold: its release and what it pins.
	pinned := map[string]struct {
		version string
		deps    map[string]any
	}{
		"@playwright/mcp": {mcp, map[string]any{"playwright": core, "playwright-core": core}},
		"playwright":      {core, map[string]any{"playwright-core": core}},
		"playwright-core": {core, nil},
	}
	keys := []string{""}
	for name := range pinned {
		keys = append(keys, "node_modules/"+name)
	}
	if got := slices.Sorted(maps.Keys(lock.Packages)); !slices.Equal(got, slices.Sorted(slices.Values(keys))) {
		return append(problems, fmt.Sprintf("the lock holds %q, want exactly %q: @playwright/mcp and the two "+
			"packages it pins, once each, which a bump that splits them (a nested copy) or adds one breaks", got,
			slices.Sorted(slices.Values(keys))))
	}
	integrity := regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
	for _, name := range slices.Sorted(maps.Keys(pinned)) {
		p, entry := pinned[name], lock.Packages["node_modules/"+name]
		if entry["version"] != p.version {
			fail("the lock pins %s %v, and internal/browser names %s: read the release, then move the constant "+
				"(CONTRIBUTING.md, \"A Playwright MCP bump is read before it lands\")", name, entry["version"], p.version)
		}
		if got, want := entry["resolved"], registryAddress(name, p.version); got != want {
			fail("the lock resolves %s at %v, not the registry's own %s: generate it against "+
				"https://registry.npmjs.org/, never a mirror, whose host npm would not rewrite", name, got, want)
		}
		if s, _ := entry["integrity"].(string); !integrity.MatchString(s) {
			fail("the lock's integrity of %s is %q, not one SHA-512, which npm checks every byte against", name, s)
		}
		if got := entry["dependencies"]; !reflect.DeepEqual(got, valueOrNil(p.deps)) {
			fail("the lock's %s depends on %s, want %s", name, asJSON(got), asJSON(p.deps))
		}
		for _, flag := range refusedFlags {
			if v, ok := entry[flag]; ok {
				fail("the lock's %s has %q: %v, which the box helper's tree may not hold (boxhelper's package doc)",
					name, flag, v)
			}
		}
		engines, ok := entry["engines"].(map[string]any)
		if _, present := entry["engines"]; present && !ok {
			fail("the lock's %s has engines %s, not an object npm writes", name, asJSON(entry["engines"]))
		}
		for _, engine := range slices.Sorted(maps.Keys(engines)) {
			spec := engines[engine]
			floor, err := nodeFloor(fmt.Sprint(spec))
			switch {
			case engine != "node":
				fail("the lock's %s asks for engine %s %v, which the box helper does not hold", name, engine, spec)
			case err != nil:
				fail("the lock's %s asks for node %v: %v", name, spec, err)
			case floor > boxhelper.MinNodeMajor:
				fail("the lock's %s asks for node %v, above boxhelper.MinNodeMajor %d, the oldest a box may run: "+
					"raise the floor first, or hold the release back", name, spec, boxhelper.MinNodeMajor)
			}
		}
	}
	return problems
}

// refusedFlags are the lock entry fields a package of the tree may not carry:
// a script --ignore-scripts would skip, a platform or libc that makes the tree
// differ by machine, a flag that lets npm leave the package out (optional,
// dev, devOptional, peer), and a link or a bundled copy, which no registry
// address names.
var refusedFlags = []string{"hasInstallScript", "hasShrinkwrap", "os", "cpu", "libc", "optional", "dev",
	"devOptional", "peer", "link", "inBundle"}

// A LOCK SHORT OF THE RELEASE IS REFUSED, each way: every row changes one thing
// in the committed manifest or lock, or names another release, and
// lockProblems must say what, as it must say nothing of the committed tree.
// The flags are spelled here, not read from refusedFlags, so that list cannot
// lose one unnoticed. Mutations, each red: drop any one of lockProblems'
// checks; a flag fewer in refusedFlags.
func TestALockShortOfTheReleaseIsRefused(t *testing.T) {
	t.Parallel()
	manifestJSON, lockJSON := committedTree(t)
	mcp, core := browser.PlaywrightMCPRelease, browser.PlaywrightCoreRelease
	type tree struct{ manifest, lock map[string]any }
	packages := func(tr tree) map[string]any { return tr.lock["packages"].(map[string]any) }
	entry := func(tr tree, name string) map[string]any {
		return packages(tr)["node_modules/"+name].(map[string]any)
	}
	type row struct {
		name   string
		mcp    string     // the @playwright/mcp release internal/browser names; empty, the committed one
		change func(tree) // nil: the committed tree
		says   string     // a fragment of one problem; empty, no problem at all
	}
	rows := []row{
		{name: "the committed tree"},
		{name: "another @playwright/mcp release named", mcp: mcp + "-other",
			says: "the lock pins @playwright/mcp " + mcp + ", and internal/browser names " + mcp + "-other"},
		{name: "the core at another release", change: func(tr tree) { entry(tr, "playwright-core")["version"] = "0.0.1" },
			says: "the lock pins playwright-core 0.0.1"},
		{name: "a range in package.json",
			change: func(tr tree) { tr.manifest["dependencies"] = map[string]any{"@playwright/mcp": "^" + mcp} },
			says:   "tree/package.json is"},
		{name: "engines in package.json",
			change: func(tr tree) { tr.manifest["engines"] = map[string]any{"node": ">=20"} }, says: "tree/package.json is"},
		{name: "scripts in package.json",
			change: func(tr tree) { tr.manifest["scripts"] = map[string]any{"postinstall": "true"} },
			says:   "tree/package.json is"},
		{name: "another lockfileVersion", change: func(tr tree) { tr.lock["lockfileVersion"] = 2 },
			says: "is lockfileVersion 2, want 3"},
		{name: "a root naming another release", change: func(tr tree) {
			packages(tr)[""].(map[string]any)["dependencies"] = map[string]any{"@playwright/mcp": "0.0.1"}
		}, says: "the lock's root is"},
		{name: "a nested core", change: func(tr tree) {
			packages(tr)["node_modules/@playwright/mcp/node_modules/playwright-core"] = entry(tr, "playwright-core")
		}, says: "the lock holds"},
		{name: "an address on another host", change: func(tr tree) {
			e := entry(tr, "playwright")
			e["resolved"] = strings.Replace(e["resolved"].(string), "registry.npmjs.org", "npm.example.com", 1)
		}, says: "the lock resolves playwright at https://npm.example.com/"},
		{name: "a SHA-256 integrity", change: func(tr tree) {
			entry(tr, "playwright")["integrity"] = "sha256-" + strings.Repeat("A", 43) + "="
		}, says: "the lock's integrity of playwright is"},
		{name: "an edge at another release", change: func(tr tree) {
			entry(tr, "@playwright/mcp")["dependencies"] = map[string]any{"playwright": "0.0.1", "playwright-core": core}
		}, says: "the lock's @playwright/mcp depends on"},
		{name: "engines npm writes no object of", change: func(tr tree) { entry(tr, "playwright-core")["engines"] = ">=20" },
			says: "not an object npm writes"},
		{name: "an engine other than node",
			change: func(tr tree) { entry(tr, "playwright-core")["engines"] = map[string]any{"npm": ">=10"} },
			says:   "asks for engine npm"},
		{name: "an engines.node of no major's floor",
			change: func(tr tree) { entry(tr, "playwright-core")["engines"] = map[string]any{"node": "^20"} },
			says:   "asks for node ^20"},
		{name: "an engines.node above the floor", change: func(tr tree) {
			entry(tr, "playwright-core")["engines"] = map[string]any{"node": fmt.Sprintf(">=%d", boxhelper.MinNodeMajor+2)}
		}, says: "above boxhelper.MinNodeMajor"},
	}
	for _, flag := range []string{"hasInstallScript", "hasShrinkwrap", "os", "cpu", "libc", "optional", "dev",
		"devOptional", "peer", "link", "inBundle"} {
		rows = append(rows, row{name: "an entry with " + flag, change: func(tr tree) { entry(tr, "playwright")[flag] = true },
			says: fmt.Sprintf("the lock's playwright has %q", flag)})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tr tree
			if err := json.Unmarshal(manifestJSON, &tr.manifest); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(lockJSON, &tr.lock); err != nil {
				t.Fatal(err)
			}
			if tc.change != nil {
				tc.change(tr)
			}
			m, l := []byte(asJSON(tr.manifest)), []byte(asJSON(tr.lock))
			problems := lockProblems(m, l, cmp.Or(tc.mcp, mcp), core)
			switch {
			case tc.says == "" && len(problems) > 0:
				t.Errorf("found %q; want no problem", problems)
			case tc.says != "" && !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.says) }):
				t.Errorf("found %q; want one saying %q", problems, tc.says)
			}
		})
	}
}

// registryAddress is where npm's registry serves name's tarball of version:
// <registry>/<name>/-/<name without its scope>-<version>.tgz.
func registryAddress(name, version string) string {
	_, base, _ := strings.Cut(name, "/")
	if base == "" {
		base = name
	}
	return "https://registry.npmjs.org/" + name + "/-/" + base + "-" + version + ".tgz"
}

// nodeFloor is the major an engines.node range starts at, for a range of the one
// form a floor can be read off: >=N, or >=N.0 or >=N.0.0. Any other is an
// error naming it, since >=22.5 or ^20 asks for more than a major.
func nodeFloor(spec string) (int, error) {
	m := regexp.MustCompile(`^>=([0-9]+)(\.0(\.0)?)?$`).FindStringSubmatch(spec)
	if m == nil {
		return 0, fmt.Errorf("%q is no range of the form >=N, from a major's first release, which is all the "+
			"suite can hold to boxhelper.MinNodeMajor", spec)
	}
	return strconv.Atoi(m[1])
}

// THE ENGINES RANGE IS READ OF ONE FORM ALONE: a floor of a major's first
// release, which is all MinNodeMajor can be compared with. Mutation: accept any
// leading >=N -> >=22.5 and ">=20 <24" read as floors (red).
func TestAnEnginesRangeIsReadAsAMajorFloorOrRefused(t *testing.T) {
	t.Parallel()
	for spec, want := range map[string]int{">=20": 20, ">=22.0": 22, ">=22.0.0": 22, ">=18": 18} {
		if got, err := nodeFloor(spec); err != nil || got != want {
			t.Errorf("nodeFloor(%q) = %d, %v; want %d", spec, got, err, want)
		}
	}
	for _, spec := range []string{"^20", ">=22.5", ">=20 <24", ">= 20", "20.x", "*", "", ">=20.0.1", "<=24"} {
		if got, err := nodeFloor(spec); err == nil {
			t.Errorf("nodeFloor(%q) = %d; want it refused as no floor of a major", spec, got)
		}
	}
}

// THE REGISTRY'S OWN ADDRESS, and no other, is a locked package's: npm sends it
// to the registry or mirror a box names, and a mirror's own address, which it
// does not rewrite, would tie every box to that mirror. Mutation: compare the
// host alone -> the wrong path or version passes (red).
func TestALockedAddressIsTheRegistrysOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version, resolved string
		ok                      bool
	}{
		{"@playwright/mcp", "0.0.86", "https://registry.npmjs.org/@playwright/mcp/-/mcp-0.0.86.tgz", true},
		{"playwright-core", "1.66.0", "https://registry.npmjs.org/playwright-core/-/playwright-core-1.66.0.tgz", true},
		{"@playwright/mcp", "0.0.86", "https://npm.example.com/@playwright/mcp/-/mcp-0.0.86.tgz", false},
		{"@playwright/mcp", "0.0.86", "http://registry.npmjs.org/@playwright/mcp/-/mcp-0.0.86.tgz", false},
		{"@playwright/mcp", "0.0.86", "https://registry.npmjs.org/@playwright/mcp/-/mcp-0.0.85.tgz", false},
		{"@playwright/mcp", "0.0.86", "https://registry.npmjs.org/@playwright/mcp/-/@playwright/mcp-0.0.86.tgz", false},
		{"playwright-core", "1.66.0", "https://registry.npmjs.org/api/npm/playwright-core/-/playwright-core-1.66.0.tgz", false},
	} {
		if got := registryAddress(tc.name, tc.version) == tc.resolved; got != tc.ok {
			t.Errorf("%s %s at %s read as the registry's own: %v, want %v", tc.name, tc.version, tc.resolved, got, tc.ok)
		}
	}
}

// readJSON decodes the file at path into v, or fails t naming it.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			t.Fatalf("%s is no JSON at byte %d: %v", path, syntax.Offset, err)
		}
		t.Fatalf("%s: %v", path, err)
	}
}

// asJSON is v as JSON, for a failure's words.
func asJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// valueOrNil is m as encoding/json decodes it into an any: nil when empty, so a
// package that pins nothing compares equal to an entry with no dependencies.
func valueOrNil(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
