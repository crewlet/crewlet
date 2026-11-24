package main

import (
	"os"
	"path/filepath"
	"strings"
)

// cloneFinished reports whether the checkout at root is a finished clone, one
// a firing may claim: its index written, no index.lock, and HEAD resolving to
// a commit. git 2.40.4 and 2.56.0 alike create .git, then a HEAD naming a
// branch that does not exist yet, fetch, hold index.lock for the whole
// checkout (the work tree's files appear meanwhile) and write the index last
// (measured): claimed earlier, a checkout would be handed once without the
// instructions and skills it was still receiving. A repository with no commit
// is never claimed; it has nothing a clone brings. A .git file (a worktree, a
// separated git directory) names the directory these live in. A reftable
// repository's HEAD names refs/heads/.invalid for good, so there a non-empty
// reftable/tables.list stands for it (written before the checkout begins on
// git 2.56.0, measured).
func cloneFinished(root string) bool {
	gitDir := filepath.Join(root, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.Mode().IsRegular() {
		raw, err := readBounded(gitDir, 4096)
		if err != nil {
			return false
		}
		named, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
		if !ok {
			return false
		}
		if !filepath.IsAbs(named) {
			named = filepath.Join(root, named)
		}
		gitDir = named
	}
	if _, err := os.Stat(filepath.Join(gitDir, "index")); err != nil {
		return false
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "index.lock")); err == nil {
		return false
	}
	if reftable(gitDir) {
		return tablesListed(gitDir)
	}
	return headResolves(gitDir)
}

// gitDirs is gitDir and the common directory its commondir names, where a
// worktree's refs and config live.
func gitDirs(gitDir string) []string {
	dirs := []string{gitDir}
	if common, err := readBounded(filepath.Join(gitDir, "commondir"), 4096); err == nil {
		dir := strings.TrimSpace(string(common))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitDir, dir)
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// reftable reports whether gitDir's repository keeps its refs in reftable:
// extensions.refStorage set to reftable in its config, the common
// directory's for a worktree.
func reftable(gitDir string) bool {
	for _, dir := range gitDirs(gitDir) {
		raw, err := readBounded(filepath.Join(dir, "config"), maxGitConfig)
		if err != nil {
			continue
		}
		section := ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				section = strings.ToLower(strings.Trim(line, "[] \t"))
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if ok && section == "extensions" && strings.EqualFold(strings.TrimSpace(key), "refstorage") {
				return strings.EqualFold(strings.TrimSpace(value), "reftable")
			}
		}
	}
	return false
}

// tablesListed reports whether a reftable repository's tables.list names a
// table, in gitDir or its common directory.
func tablesListed(gitDir string) bool {
	for _, dir := range gitDirs(gitDir) {
		if info, err := os.Stat(filepath.Join(dir, "reftable", "tables.list")); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// maxGitConfig is the most of a git config file read for its refStorage:
// 1 MiB, far past a clone's few hundred bytes.
const maxGitConfig = 1 << 20

// headResolves reports whether gitDir's HEAD names a commit: an object name,
// or a ref that exists loose or packed, in gitDir or in the common directory
// a worktree's commondir names.
func headResolves(gitDir string) bool {
	raw, err := readBounded(filepath.Join(gitDir, "HEAD"), 4096)
	if err != nil {
		return false
	}
	head := strings.TrimSpace(string(raw))
	ref, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return (len(head) == 40 || len(head) == 64) && strings.Trim(head, "0123456789abcdef") == ""
	}
	for _, dir := range gitDirs(gitDir) {
		if info, err := os.Stat(filepath.Join(dir, ref)); err == nil && info.Mode().IsRegular() {
			return true
		}
		if packed, err := readBounded(filepath.Join(dir, "packed-refs"), maxPackedRefs); err == nil {
			for _, line := range strings.Split(string(packed), "\n") {
				if _, name, ok := strings.Cut(line, " "); ok && name == ref {
					return true
				}
			}
		}
	}
	return false
}

// maxPackedRefs is the most of a packed-refs file the check reads: 16 MiB, a
// hundred thousand refs at their longest, far past a clone's (a fresh clone
// packs its remote branches and tags there).
const maxPackedRefs = 16 << 20
