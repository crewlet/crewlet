package sourcetree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// layout writes a tree: each key is a slash path under the root, and the file
// there holds its value.
func layout(t *testing.T, entries map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range entries {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// visited is every path Walk hands the callback, relative to dir and in slash
// form, sorted.
func visited(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := Walk(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	slices.Sort(out)
	return out
}

// A NESTED CHECKOUT IS NEVER ENTERED, whichever shape its `.git` takes.
//
// A linked worktree — what `isolation: worktree` creates — has a `.git` FILE
// pointing back at the repository it was taken from; a clone has a `.git`
// DIRECTORY. Both are a second copy of this tree at another commit, and a
// guard that reads either reports that copy's content as this tree's. Neither
// the directory nor anything under it may reach the callback, however deep it
// sits: the rule is the structure, not the path `.claude/worktrees/`. The
// gitfiles here point at nothing that exists, which git would walk straight
// past; TestAGitEntryThatResolvesNowhereStillMarksANestedCheckout is why that
// does not matter here.
func TestANestedCheckoutIsNeverEntered(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{
		"go.mod":                             "module example.com/m\n",
		"internal/live.go":                   "package live\n",
		".claude/settings.json":              "{}\n",
		".claude/worktrees/wf_1/.git":        "gitdir: /elsewhere/.git/worktrees/wf_1\n",
		".claude/worktrees/wf_1/go.mod":      "module example.com/m\n",
		".claude/worktrees/wf_1/internal/x":  "a stale copy\n",
		"tmp/compare/.git/HEAD":              "ref: refs/heads/main\n",
		"tmp/compare/internal/live.go":       "package live // another commit's\n",
		"deep/under/a/tree/clone/.git":       "gitdir: /elsewhere\n",
		"deep/under/a/tree/clone/stale.go":   "package stale\n",
		"deep/under/a/tree/beside/ours.go":   "package ours\n",
		"deep/under/a/tree/beside/nested/y":  "ours too\n",
		"docs/page.md":                       "# ours\n",
		"docs/submodule-like/.git":           "gitdir: ../.git/modules/x\n",
		"docs/submodule-like/README.md":      "not ours\n",
		"internal/pkg/testdata/fixture.json": "{}\n",
	})

	got := visited(t, root)
	want := []string{
		".claude",
		".claude/settings.json",
		".claude/worktrees",
		"deep",
		"deep/under",
		"deep/under/a",
		"deep/under/a/tree",
		"deep/under/a/tree/beside",
		"deep/under/a/tree/beside/nested",
		"deep/under/a/tree/beside/nested/y",
		"deep/under/a/tree/beside/ours.go",
		"docs",
		"docs/page.md",
		"go.mod",
		"internal",
		"internal/live.go",
		"internal/pkg",
		"internal/pkg/testdata",
		"internal/pkg/testdata/fixture.json",
		"tmp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("walk visited\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"),
			strings.Join(want, "\n\t"))
	}
}

// A `.git` THAT RESOLVES NOWHERE STILL MARKS A NESTED CHECKOUT, which is
// deliberately broader than git's own rule.
//
// git skips only a directory whose `.git` is a valid repository: it walks
// into a DANGLING gitfile — a worktree whose administrative directory was
// pruned or moved — or into an empty `.git`, and would stage what it finds.
// Such a directory is still a copy of this repository at some other commit,
// so the walk steps over it. It can afford to: git never tracks a `.git`
// path, so a fresh checkout holds none and the broader rule skips nothing
// there.
func TestAGitEntryThatResolvesNowhereStillMarksANestedCheckout(t *testing.T) {
	t.Parallel()
	gone := filepath.Join(t.TempDir(), "gone", ".git", "worktrees", "pruned")
	root := layout(t, map[string]string{
		"internal/live.go": "package live\n",
		"pruned/.git":      "gitdir: " + gone + "\n",
		"pruned/stale.go":  "package stale\n",
		"garbled/.git":     "not a gitfile at all\n",
		"garbled/stale.go": "package stale\n",
		"emptied/stale.go": "package stale\n",
	})
	if err := os.Mkdir(filepath.Join(root, "emptied", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := visited(t, root)
	want := []string{"internal", "internal/live.go"}
	if !slices.Equal(got, want) {
		t.Errorf("walk visited %q, want %q — a directory beside a .git of any "+
			"kind is a checkout, whether or not git would call it one", got, want)
	}
}

// THE START'S OWN `.git` IS NOT SOURCE, and the start itself is walked,
// though never handed over (see TestTheStartIsNeverJudgedByTheCallersSkips).
//
// The module root holds a `.git` by definition — a directory in a clone and a
// FILE in a linked worktree, which is what every agent's checkout of this
// repository is — so the nested-checkout rule must never be applied to the
// start, or a walk from the root would read nothing at all. The `.git` entry
// itself is VCS bookkeeping in either shape and is never handed over.
func TestTheStartsOwnGitIsSkippedAndTheStartIsWalked(t *testing.T) {
	t.Parallel()
	for name, git := range map[string]map[string]string{
		"a clone":           {".git/HEAD": "ref: refs/heads/main\n", ".git/objects/ab/cd": "blob"},
		"a linked worktree": {".git": "gitdir: /elsewhere/.git/worktrees/wt\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entries := map[string]string{
				"go.mod":           "module example.com/m\n",
				"internal/live.go": "package live\n",
			}
			for path, body := range git {
				entries[path] = body
			}
			got := visited(t, layout(t, entries))
			want := []string{"go.mod", "internal", "internal/live.go"}
			if !slices.Equal(got, want) {
				t.Errorf("walk visited %q, want %q", got, want)
			}
		})
	}
}

// A DEPENDENCY INSTALL IS NOT SOURCE, wherever it sits.
//
// npm writes node_modules beside the package.json that asked for it, which is
// dashboard/ here and could be anywhere, and .gitignore ignores the name at
// every depth for that reason. A `node_modules` that is a SYMLINK — how pnpm
// lays one out — is skipped too: WalkDir does not follow it, so it would
// otherwise reach a guard as a file that cannot be read.
func TestNodeModulesIsNeverEntered(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{
		"node_modules/react/index.js":           "module.exports = {}\n",
		"dashboard/package.json":                "{}\n",
		"dashboard/node_modules/vite/index.js":  "export {}\n",
		"dashboard/src/main.tsx":                "export {}\n",
		"elsewhere/store/node_modules/.keep":    "",
		"elsewhere/store/real/node_modules.txt": "a FILE that merely starts with the name\n",
	})
	if err := os.Symlink(filepath.Join(root, "node_modules"),
		filepath.Join(root, "dashboard", "src", "node_modules")); err != nil {
		t.Fatal(err)
	}
	got := visited(t, root)
	want := []string{
		"dashboard",
		"dashboard/package.json",
		"dashboard/src",
		"dashboard/src/main.tsx",
		"elsewhere",
		"elsewhere/store",
		"elsewhere/store/real",
		"elsewhere/store/real/node_modules.txt",
	}
	if !slices.Equal(got, want) {
		t.Errorf("walk visited %q, want %q", got, want)
	}
}

// A DIRECTORY THAT CANNOT BE PROBED IS AN ERROR, never a guess either way.
//
// Classed as ours, a checkout nobody can see into would have its copies read
// as this tree's; classed as foreign, this tree's own files under it would
// vanish from every guard, which then passes having read less than it thinks.
// Both are the failure this package exists to remove, so the walk stops and
// names the directory. The probe's failure is injected because the natural
// one — a directory without search permission — is no failure at all to a
// test running as root.
func TestAnUnprobeableDirectoryStopsTheWalk(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{"locked/inside.go": "package inside\n"})
	locked := filepath.Join(root, "locked")
	denied := func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }

	var seen []string
	err := walk(root, func(path string, _ fs.DirEntry, err error) error {
		seen = append(seen, path)
		return err
	}, denied)
	if err == nil {
		t.Fatalf("an unprobeable directory was classed rather than reported; "+
			"the callback saw %q", seen)
	}
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), locked) {
		t.Errorf("error %q does not wrap the probe's failure and name %s", err, locked)
	}
	if len(seen) != 0 {
		t.Errorf("the callback saw %q, want nothing before the walk stopped", seen)
	}
}

// A GUARD'S OWN SKIPS STILL WORK, because they are the guard's decisions and
// this package only adds the three every guard shares. SkipDir from a
// directory prunes it, and SkipAll ends the walk without an error.
func TestTheCallersOwnSkipsAreHonoured(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{
		"internal/live.go":    "package live\n",
		"static/bundle.js":    "minified\n",
		"zz/after/the/end.go": "package end\n",
	})
	var got []string
	err := Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && d.Name() == "static":
			return fs.SkipDir
		case rel == "zz":
			return fs.SkipAll
		}
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"internal", "internal/live.go"}
	if !slices.Equal(got, want) {
		t.Errorf("walk visited %q, want %q", got, want)
	}
}

// A START THAT DOES NOT EXIST IS AN ERROR THE CALLER SEES, exactly as with
// filepath.WalkDir, rather than a walk over nothing — a guard walking a moved
// directory must fail, not certify an empty tree.
func TestAMissingStartReachesTheCallback(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "moved")
	called := false
	err := Walk(missing, func(path string, _ fs.DirEntry, err error) error {
		called = true
		if path != missing {
			t.Errorf("error reported for %s, want the start %s", path, missing)
		}
		return err
	})
	if !called {
		t.Error("the callback never heard that the start is missing")
	}
	if err == nil {
		t.Error("walking a missing start returned no error")
	}
}

// THE START IS NEVER JUDGED BY THE CALLER'S SKIPS, because its name is
// wherever somebody put the checkout.
//
// The guards skip directories by NAME — dist/, static/, vendor/, a
// dot-directory — and those rules are written for what lies below the start.
// Handed the start, a guard that skips dist/ skipped a whole checkout living
// at ../dist, and since it asserts an absence it passed having read nothing.
// So the start is walked and never handed over, while the same callback still
// prunes a directory of that name further down.
func TestTheStartIsNeverJudgedByTheCallersSkips(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"dist", "static", "vendor", ".crewlet"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := filepath.Join(layout(t, map[string]string{
				name + "/internal/live.go":             "package live\n",
				name + "/internal/" + name + "/gen.go": "package gen\n",
			}), name)
			var got []string
			err := Walk(start, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && d.Name() == name {
					return fs.SkipDir
				}
				rel, err := filepath.Rel(start, path)
				if err != nil {
					return err
				}
				got = append(got, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			want := []string{"internal", "internal/live.go"}
			if !slices.Equal(got, want) {
				t.Errorf("a walk started at a directory named %s visited %q, want %q",
					name, got, want)
			}
		})
	}
}

// A START THAT IS NOT A DIRECTORY IS AN ERROR THE CALLER SEES, a symbolic link
// to one included.
//
// filepath.WalkDir hands such a start to the callback once, as an entry that
// is not a directory, and stops: it does not follow a link at its start. Every
// guard answers a non-directory it has no use for with nil, so a guard started
// at a link to a checkout read nothing and passed. The callback hears an error
// instead, and the walk returns it.
func TestAStartThatIsNotADirectoryReachesTheCallback(t *testing.T) {
	t.Parallel()
	tree := layout(t, map[string]string{
		"go.mod":           "module example.com/m\n",
		"internal/live.go": "package live\n",
	})
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	for name, start := range map[string]string{
		"a symbolic link to a checkout": link,
		"a file":                        filepath.Join(tree, "go.mod"),
	} {
		var heard []string
		err := Walk(start, func(path string, _ fs.DirEntry, err error) error {
			if err == nil {
				t.Errorf("%s: %s was handed over with no error", name, path)
			}
			heard = append(heard, path)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("%s: Walk returned %v, want the refusal", name, err)
		}
		if !slices.Equal(heard, []string{start}) {
			t.Errorf("%s: the callback heard %q, want only the start", name, heard)
		}
	}
}

// THE ROOT IS THE MODULE THIS PACKAGE IS COMPILED IN, which is the one
// question every guard asks before it walks anything.
func TestRootIsTheModuleHoldingThisPackage(t *testing.T) {
	t.Parallel()
	root := Root(t)
	for _, want := range []string{"go.mod", filepath.Join("internal", "sourcetree", "sourcetree.go")} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("Root() = %s, which holds no %s: %v", root, want, err)
		}
	}
}

// THE NEAREST go.mod IS THE MODULE, which is how the go command defines the
// module a package belongs to — so a checkout nested inside another answers
// its OWN root, and a counted depth has nothing left to get wrong.
func TestModuleRootIsTheNearestGoModAbove(t *testing.T) {
	t.Parallel()
	root := layout(t, map[string]string{
		"go.mod":                              "module example.com/outer\n",
		"internal/sourcetree/sourcetree.go":   "package sourcetree\n",
		"nested/go.mod":                       "module example.com/inner\n",
		"nested/internal/sourcetree/tree.go":  "package sourcetree\n",
		"nested/internal/deeper/still/src.go": "package still\n",
	})
	for file, want := range map[string]string{
		"internal/sourcetree/sourcetree.go":   root,
		"nested/internal/sourcetree/tree.go":  filepath.Join(root, "nested"),
		"nested/internal/deeper/still/src.go": filepath.Join(root, "nested"),
	} {
		got, err := findModuleRoot(filepath.Join(root, filepath.FromSlash(file)), os.Stat)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if got != want {
			t.Errorf("%s: module root = %s, want %s", file, got, want)
		}
	}
}

// A ROOT REACHED THROUGH A SYMBOLIC LINK IS RESOLVED, because the root is
// where every guard's walk starts and a walk does not follow a link at its
// start.
//
// The compiled-in path is spelled the way the go command was run, so a
// checkout reached as a link — ~/src/crewlet pointing at a bigger disk —
// compiled in the link, and every guard walking from it read nothing: the
// vocabulary gate, which counts, failed on "scanned 0 files", and the gates
// that do not count passed.
func TestARootReachedThroughALinkIsResolved(t *testing.T) {
	t.Parallel()
	tree := layout(t, map[string]string{
		"go.mod":                            "module example.com/m\n",
		"internal/sourcetree/sourcetree.go": "package sourcetree\n",
	})
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	got, err := moduleRoot(filepath.Join(link, "internal", "sourcetree", "sourcetree.go"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(tree)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("module root = %s, want %s, which %s points to", got, want, link)
	}
	if files := visited(t, got); !slices.Contains(files, "internal/sourcetree/sourcetree.go") {
		t.Errorf("a walk from the module root read %q, which misses the package "+
			"it was found from", files)
	}
}

// NO ROOT IS AN ERROR, never a guess at one: a guard handed the wrong
// directory walks the wrong tree and reports on it with full confidence.
//
// The probe is supplied rather than the disk's, because the refusal is the
// subject and the disk would make it the machine's: a tree built under
// t.TempDir() has no go.mod above it only while TMPDIR lies outside every
// module, and with TMPDIR inside a checkout this case found that checkout's
// go.mod and failed on a tree nothing was wrong with.
func TestNoModuleRootIsAnError(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "a", "b", "c.go")
	absent := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	denied := func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	for name, tc := range map[string]struct {
		file  string
		stat  func(string) (fs.FileInfo, error)
		says  string
		wraps error
	}{
		// -trimpath compiles in the import path, which names no directory
		// on this machine — and resolved against the working directory it
		// could name the wrong one, so the refusal says which build did it.
		"a trimmed path": {
			file: "github.com/crewlet/crewlet/internal/sourcetree/sourcetree.go",
			stat: absent, says: "-trimpath",
		},
		// A file with no go.mod anywhere above it is not in a module.
		"no go.mod above": {file: file, stat: absent, says: "no go.mod"},
		// A go.mod that cannot be probed is neither there nor absent, and
		// stepping past it would answer some module further up.
		"an unprobeable directory": {
			file: file, stat: denied,
			says: "looking for the module root", wraps: fs.ErrPermission,
		},
	} {
		got, err := findModuleRoot(tc.file, tc.stat)
		if err == nil {
			t.Errorf("%s: module root = %s, want an error", name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: error %q does not say %q", name, err, tc.says)
		}
		if tc.wraps != nil && !errors.Is(err, tc.wraps) {
			t.Errorf("%s: error %q does not wrap %v", name, err, tc.wraps)
		}
	}
}
