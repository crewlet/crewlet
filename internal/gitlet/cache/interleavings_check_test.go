package cache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE CHECK'S OPERATIONS in an interleaving: damage a previous process left,
// and a file replaced while the check hashes it.

// heldFile picks a held repository and one of its index files (fileOf),
// answering the repository, its incarnation's directory and the file's path
// relative to it, or "" when the cache holds nothing.
func (in *interleaving) heldFile(packs, tags bool) (repo, dir, rel string) {
	if repo = in.heldRepo(); repo == "" {
		return "", "", ""
	}
	dir = in.w.dir(in.t, in.c, repo)
	if rel = in.fileOf(dir, packs, tags); rel == "" {
		return "", "", ""
	}
	return repo, dir, rel
}

// heldRepo picks a repository the cache holds, or "" when it holds none.
func (in *interleaving) heldRepo() string {
	held := cache.Repos(in.c)
	var repos []string
	for _, r := range in.repos {
		if _, ok := held[r]; ok {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 {
		return ""
	}
	return repos[in.rng.IntN(len(repos))]
}

// fileOf picks an index file of the incarnation at dir, answering its path
// relative to dir or "" when it holds none: a pack's .idx, .rev or .bitmap,
// the multi-pack-index, a chained copy's layer and chain files, the
// commit-graph; with packs a .pack too, with tags the tag index. The chained
// directories are walked because a chained copy holds no own-mode derived
// file, so drawing from the pack directory alone never damages one.
func (in *interleaving) fileOf(dir string, packs, tags bool) string {
	var files []string
	for _, name := range names(in.t, filepath.Join(dir, "objects", "pack")) {
		if strings.HasSuffix(name, ".idx") || strings.HasSuffix(name, ".rev") || strings.HasSuffix(name, ".bitmap") ||
			name == "multi-pack-index" || packs && strings.HasSuffix(name, ".pack") {
			files = append(files, filepath.Join("objects", "pack", name))
		}
	}
	for _, chained := range []string{filepath.Join("objects", "pack", "multi-pack-index.d"),
		filepath.Join("objects", "info", "commit-graphs")} {
		if !statOK(in.t, filepath.Join(dir, chained)) {
			continue
		}
		for _, name := range names(in.t, filepath.Join(dir, chained)) {
			files = append(files, filepath.Join(chained, name))
		}
	}
	if statOK(in.t, filepath.Join(dir, "objects", "info", "commit-graph")) {
		files = append(files, filepath.Join("objects", "info", "commit-graph"))
	}
	if tags && statOK(in.t, filepath.Join(dir, cache.TagsRel)) {
		files = append(files, filepath.FromSlash(cache.TagsRel))
	}
	if len(files) == 0 {
		return ""
	}
	slices.Sort(files)
	return files[in.rng.IntN(len(files))]
}

// damageAndRestart is damage a previous process left: the cache stops, a
// byte of a held index file is flipped in place (its identity kept, as media
// damage keeps it), and the directory boots again. The new process vouches for
// nothing, so its first round must find and repair the file before answering.
func (in *interleaving) damageAndRestart(string) string {
	repo, dir, rel := in.heldFile(false, true)
	in.c.Stop(in.t.Context())
	detail := "nothing held"
	if rel != "" {
		flipInPlace(in.t, filepath.Join(dir, rel))
		detail = fmt.Sprintf("%s of repo-%d flipped", filepath.Base(rel), slices.Index(in.repos, repo)+1)
	}
	in.c = in.w.open(in.t, in.options)
	return detail
}

// replaceWhileHashed renames a same-bytes copy over a held index file (so no
// vouch holds it), then runs a full round during whose hash another copy is
// renamed in (hashed): that hash gives no verdict, the round repairs nothing,
// and the next full round hashes the successor.
func (in *interleaving) replaceWhileHashed(string) string {
	repo, dir, rel := in.heldFile(false, false)
	if rel == "" {
		return "nothing held"
	}
	path := filepath.Join(dir, rel)
	copyOver(in.t, path)
	in.swapMu.Lock()
	in.swap = path
	in.swapMu.Unlock()
	cache.Reread(in.c, cache.RebuildStream)
	s := in.syncAt(repo, linearizable)
	s.Release()
	in.swapMu.Lock()
	in.swap = ""
	in.swapMu.Unlock()
	return fmt.Sprintf("%s of repo-%d", filepath.Base(rel), slices.Index(in.repos, repo)+1)
}

// hashed is told every piece the check hashes; it renames over the file
// replaceWhileHashed armed, once, while the hash holds it open.
func (in *interleaving) hashed(path string, held bool) {
	in.swapMu.Lock()
	defer in.swapMu.Unlock()
	if held && path == in.swap {
		copyOver(in.t, path)
		in.swap = ""
	}
}

// noteChecks records the outcomes the checks reached: damage a first round
// found, a pack reinstalled in place, a derived file dropped, a layer file
// refetched or a chain file rewritten, and a verdict withheld for a file
// replaced while hashed.
func (in *interleaving) noteChecks() {
	if counterOf(in.w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "first", "outcome": "damaged"}) > 0 {
		in.saw("a first round's check found damage")
	}
	if counter(in.w.rec, metrics.CodeCacheChecks, "outcome", "replaced") > 0 {
		in.saw("a verdict withheld")
	}
	if len(in.w.logs.lines(in.t, "code_cache_pack_reinstalled")) > 0 {
		in.saw("a pack reinstalled in place")
	}
	for _, l := range in.w.logs.lines(in.t, "code_cache_check_damaged") {
		switch l["action"] {
		case "dropped":
			in.saw("a derived file dropped by its check")
		case "refetched":
			in.saw("a layer's file refetched by its check")
		case "rewritten":
			in.saw("a chain file rewritten by its check")
		}
	}
}

// checkSound fails the run when any file of repo's incarnation at dir fails
// its checksum: whatever damage the run left, the scrub found and the round
// before repaired.
func (in *interleaving) checkSound(step int, repo, dir string) {
	bad, err := cache.Unsound(dir)
	if err != nil {
		in.t.Fatalf("after step %d, hashing repo-%d's index files: %v", step, slices.Index(in.repos, repo)+1, err)
	}
	if len(bad) > 0 {
		in.t.Fatalf("after step %d, repo-%d's %v fail their trailers after a linearizable sync",
			step, slices.Index(in.repos, repo)+1, bad)
	}
}

// counterOf is a counter's total in rec over the samples carrying every one
// of attrs.
func counterOf(rec *metrics.Recorder, name string, attrs map[string]string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range attrs {
			match = match && s.Attrs[k] == v
		}
		if match {
			n += s.Total
		}
	}
	return n
}

// flipInPlace inverts a middle byte of the file at path through its own
// inode and restores its mode and modification time, so a vouch by identity
// cannot tell it was written.
func flipInPlace(t interface {
	Helper()
	Fatal(...any)
}, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	at := info.Size() / 2
	if _, err := f.ReadAt(b, at); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Time{}, info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// copyOver renames a copy of the file at path in over it: the same bytes,
// another identity.
func copyOver(t interface {
	Helper()
	Fatal(...any)
}, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	staged := path + ".copy"
	if err := os.WriteFile(staged, raw, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
}
