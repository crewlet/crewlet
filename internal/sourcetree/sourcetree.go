// Package sourcetree is how anything that reads this repository's own files
// finds them: the module root, and a walk that never enters a directory that
// is not part of this tree.
//
// # Why a gate walks the tree at all
//
// The structural guards — a withdrawn name, a hand-built env assignment, a
// statement on a pool that can be nil, a record anchored at an authority —
// mostly assert an ABSENCE, and the only way to assert one is to read
// everything. So what "everything" means is part of every one of those
// guards, and it is the part none of them is about. The formatter asks the
// same question, which is why [internal/sourcetree/gofiles] exists.
//
// # What is not part of this tree
//
// Three things, and they are the same three wherever a walk starts:
//
//   - VCS METADATA: any entry named `.git` — the repository's own directory,
//     or the FILE a linked worktree carries in its place. It is git's
//     bookkeeping, not source.
//   - A NESTED CHECKOUT: any directory BELOW the walk's start that holds a
//     `.git` entry of its own, file or directory, whether or not it resolves
//     to a repository. This repository is developed with Claude Code's
//     `isolation: worktree`, which puts a full checkout of it under
//     `.claude/worktrees/` (the reason .gitignore ignores that path), and
//     each one is a copy of every file here, taken at some other commit. Read
//     as this tree's, it breaks a guard in BOTH directions. One asserting an
//     absence fails on a clean tree: the withdrawn-vocabulary gate reported
//     the withdrawn names at its own list, in the nested copy of the file
//     that lists them, and the ADR gate reported the nested copy of the
//     record template as citing a record nobody wrote, because it excuses the
//     template by its path in THIS tree. One asserting a presence passes on a
//     stale copy: the ADR gate's check that an Enforced-by test exists finds
//     a deleted test in any checkout taken before it went.
//   - A DEPENDENCY INSTALL: any entry named `node_modules`. npm writes it,
//     .gitignore ignores it wherever it appears, and nothing in it is ours.
//
// Everything else a guard skips — static/'s committed bundle, dist/, schema/,
// testdata, the guard's own package — is THAT GUARD's decision, because one
// gate's noise is another's subject, and it stays at the guard. It reaches
// only the entries BELOW the start, because [Walk] never hands the start over:
// the start's name is wherever the checkout happens to live.
//
// # A nested checkout is recognised by its structure, not by its name
//
// A name list would say `.claude` and miss the clone somebody drops into
// tmp/ to compare against, which is the same hazard with nothing to call it.
// So the rule is structural, and DELIBERATELY BROADER THAN GIT'S: any `.git`
// entry marks a nested checkout, whether or not it resolves to a repository.
// git stops only at a directory whose `.git` is a valid repository (it warns
// about an "embedded git repository" rather than adding its files); a
// DANGLING gitfile — a worktree whose administrative directory was pruned or
// moved — or an empty `.git` directory it walks straight into, and would stage
// what it finds there. This walk steps over those too, because such a
// directory is still a copy of this repository taken at some other commit,
// which is exactly what a guard must not read; and telling a live repository
// from a dead one would mean a second copy of git's repository discovery here.
//
// Broader is the safe direction, and it costs nothing where it matters: git
// never tracks a path with a `.git` component, so a fresh checkout — CI's —
// holds no nested `.git` entry at all, and a walk there reads every committed
// file. The only files it skips that git would not are ones lying beside a
// `.git` somebody left on disk. And `go build ./...` does not compile a
// checkout of this module either, for its own reason: the checkout carries a
// go.mod, and a pattern stops at a module boundary.
//
// # The disk, and not `git ls-files`
//
// Asking git for the tracked files would sidestep all of this, and it would
// also miss a file nobody has added yet: the compiler reads what is on disk,
// tracked or not, so a guard over the index passes a violation until the
// commit that tracks it — which is precisely the commit the guard exists to
// stop.
//
// # One implementation, for the reason clientsource gives
//
// Twenty-one walks over this tree each decided for themselves what it was —
// ten with a skip list of their own — and the one that stepped over
// `.claude/worktrees/` did it by skipping every dot-directory, a NAME that
// protects nothing cloned anywhere else. Every test that read a file outside
// its own package found the module root for itself — most by counting `..` up
// from where it sat, two by walking up from the working directory — and four
// spelled out where the dashboard's source is, relative to wherever they
// happened to sit. That is [internal/clientsource]'s argument exactly: N
// copies of "which files are ours" are N chances for one to start reading what
// the others skip, and a gate that reads more than it thinks reports findings
// that are not there, or a pass it did not earn. A normal package rather than
// a test file, because a _test.go file cannot be imported by another package's
// tests, and every guard in the tree needs this.
package sourcetree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Root returns the directory holding this module's go.mod, and fails t when
// it cannot be found.
//
// Located from THIS package's compiled-in source path rather than from the
// working directory: `go test` runs a binary in its package's directory, but
// a binary run any other way does not, and a root found from the working
// directory would then silently be some other tree. It walks UP to the nearest
// go.mod rather than counting a fixed number of `..`, because the nearest
// go.mod is the definition of the module a package belongs to — and a counted
// depth is a number every copy had to get right for itself.
//
// So ANY path a test takes out of its own package starts here: a page under
// docs/, the examples, the schemas, the dashboard's build, another package's
// sources, or the directory `go list ./...` must run in. Two anchors are not
// this one, each for a reason of its own: the package's own directory — its
// testdata, its own sources — because `go test` runs every test binary there
// by contract, so a path relative to it has no depth to count; and the working
// tree's top level that static's merge test asks git for, because
// `git check-attr` resolves paths against exactly that.
func Root(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("sourcetree: cannot locate this package's own source file")
	}
	root, err := moduleRoot(file)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// moduleRoot is the nearest directory above file that holds a go.mod, with
// every symbolic link in its path resolved.
//
// RESOLVED, because the root is where every guard's walk starts and a walk
// does not follow a symbolic link at its start (see [Walk]). The compiled-in
// path is spelled the way the go command was run, so a checkout reached
// through a link — ~/src/crewlet pointing at a bigger disk — compiled in the
// link, and every guard walking from it read nothing: the vocabulary gate,
// which counts what it scans, failed on "scanned 0 files", and every gate that
// does not count passed.
func moduleRoot(file string) (string, error) {
	dir, err := findModuleRoot(file, os.Stat)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("sourcetree: resolving the module root %s: %w", dir, err)
	}
	return resolved, nil
}

// findModuleRoot is [moduleRoot] with the probe for a go.mod supplied. It is
// [os.Stat] everywhere but this package's own tests, whose refusals would
// otherwise depend on the machine: whether any directory above a temporary
// one holds a go.mod is a fact about where TMPDIR points, and a TMPDIR inside
// a checkout has this module's go.mod above every one of them.
func findModuleRoot(file string, stat func(string) (fs.FileInfo, error)) (string, error) {
	if !filepath.IsAbs(file) {
		// -trimpath rewrites every compiled-in path to its import path, so
		// there is no directory on this machine to walk up from.
		return "", fmt.Errorf("sourcetree: this package was compiled as %q, "+
			"which is not a path on this machine — a -trimpath build carries "+
			"no source tree to find, so run the gate under plain `go test`", file)
	}
	for dir := filepath.Dir(file); ; {
		_, err := stat(filepath.Join(dir, "go.mod"))
		switch {
		case err == nil:
			return dir, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("sourcetree: looking for the module root "+
				"at %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("sourcetree: no go.mod in any directory "+
				"above %s, so %s is not inside a module", file, file)
		}
		dir = parent
	}
}

// Walk walks the tree rooted at dir as [filepath.WalkDir] does — the same
// lexical order, the same error contract, and fn's own [fs.SkipDir] and
// [fs.SkipAll] meaning what they mean there — except that nothing outside
// this repository's tree, as the package doc defines it, is entered or handed
// to fn, and neither is dir itself.
//
// THE START IS WALKED BUT NEVER HANDED TO fn, except to say that it cannot be
// walked: it is missing, it cannot be read, or it is not a directory. Its name
// is wherever somebody put the checkout, and the guards judge the directories
// they are handed by NAME — dist/, static/, schema/, vendor/, their own
// package, a dot-directory — so a start handed over was judged by rules
// written for the entries below it. A checkout at ../dist was skipped whole by
// every gate that steps over dist/, and each of those that asserts an absence
// without counting what it read passed having read nothing. Nor is the start
// judged by this package's own rules: the module root holds a `.git` of its
// own by definition. A caller ends a walk early by returning [fs.SkipAll] from
// any entry below it.
//
// A START THAT IS NOT A DIRECTORY IS AN ERROR fn hears, a symbolic link to one
// included. WalkDir hands such a start to fn once and stops — it does not
// follow a link at the start — so a guard started at one read nothing and
// passed. [Root] resolves its links so that its callers never start at one.
func Walk(dir string, fn fs.WalkDirFunc) error {
	return walk(dir, fn, os.Lstat)
}

// walk is [Walk] with the probe for a nested checkout's `.git` supplied. It is
// [os.Lstat] everywhere but this package's own tests, which hand it the
// failure a test running as root cannot provoke.
func walk(dir string, fn fs.WalkDirFunc, lstat func(string) (fs.FileInfo, error)) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			// The start could not be read, or a directory this walk already
			// judged to be ours could not: either way fn hears it unjudged.
			return fn(path, d, err)
		case path == dir && d.IsDir():
			return nil
		case path == dir:
			return fn(path, d, fmt.Errorf("sourcetree: the walk's start %s is "+
				"not a directory, so there is no tree under it to read — a "+
				"symbolic link is not followed, so start at what it points to", path))
		}
		outside, err := outsideTree(path, d, lstat)
		switch {
		case err != nil:
			return err
		case !outside:
			return fn(path, d, nil)
		case d.IsDir():
			return fs.SkipDir
		}
		// SkipDir from a FILE would abandon the rest of its directory, so a
		// `.git` file or a `node_modules` symlink is simply not reported.
		return nil
	})
}

// outsideTree reports whether one entry below a walk's start is not part of
// this repository's tree.
func outsideTree(path string, d fs.DirEntry, lstat func(string) (fs.FileInfo, error)) (bool, error) {
	switch d.Name() {
	case ".git", "node_modules":
		return true, nil
	}
	if !d.IsDir() {
		return false, nil
	}
	_, err := lstat(filepath.Join(path, ".git"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	// NOT a guess in either direction: reading it as ours would scan a
	// checkout's copies, and reading it as foreign would hide this tree's
	// own files from every guard. The walk stops and says which directory.
	return false, fmt.Errorf("sourcetree: cannot tell whether %s is a nested "+
		"checkout, so nothing under it can be classed as this tree's or not: %w",
		path, err)
}
