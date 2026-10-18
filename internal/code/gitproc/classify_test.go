package gitproc_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// fixture is the measured history (gittest.Fixture): commits 1–3 in an
// OLDER pack and commit 4 — adding a large blob — in a NEWER one, main at 4.
// Every case damages its own copy.
type fixture struct{ *gittest.Fixture }

func newFixture(t *testing.T) *fixture { return &fixture{gittest.NewFixture(t)} }

func (f *fixture) tip() string { return f.Tip() }

// bare is a fresh copy: the repository, and the newer pack's path without
// its extension.
func (f *fixture) bare(t *testing.T) (dir, newer string) {
	t.Helper()
	dir, _, newer = f.packed(t)
	return dir, newer
}

// packed is a fresh copy (gittest.Bare) and both its packs' paths without
// their extension.
func (f *fixture) packed(t *testing.T) (dir, older, newer string) {
	t.Helper()
	s := gittest.Bare(t, f.Fixture)
	return s.Dir, s.Older, s.Newer
}

// writable makes a file git wrote read-only writable, for a case to damage.
func writable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func truncate(t *testing.T, path string, size int64) {
	t.Helper()
	writable(t, path)
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

func overwrite(t *testing.T, path string, at int64, b []byte) {
	t.Helper()
	writable(t, path)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
}

func readByte(t *testing.T, path string, at int64) byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body[at]
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// copyOf is a copy of path in a directory of its own, for index-pack.
func copyOf(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// fetchV2 is an upload-pack v2 fetch of want, as a clone sends it.
func fetchV2(dir, want string) gitproc.Cmd {
	return gitproc.Cmd{
		Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir}, Protocol: "version=2",
		Config: serveConfig,
		Stdin:  strings.NewReader(pkt("command=fetch\n") + "0001" + pkt("thin-pack\n") + pkt("want "+want+"\n") + pkt("done\n") + "0000"),
		Class:  gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
	}
}

// fetchV2Progress is fetchV2 asking for progress and the side band, as a
// clone at a terminal does.
func fetchV2Progress(dir, want string) gitproc.Cmd {
	c := fetchV2(dir, want)
	c.Stdin = strings.NewReader(pkt("command=fetch\n") + "0001" + pkt("thin-pack\n") + pkt("ofs-delta\n") +
		pkt("want "+want+"\n") + pkt("done\n") + "0000")
	return c
}

// serveConfig is the cache's serve configuration (cache.ServeConfig) the
// fetches here run under: the multi-pack-index and its bitmap read, partial
// clones' filters admitted. A literal, since a cache is not this package's.
var serveConfig = []gitproc.KV{
	{Key: "core.multiPackIndex", Value: "true"},
	{Key: "pack.useBitmaps", Value: "true"},
	{Key: "uploadpack.allowFilter", Value: "true"},
	{Key: "uploadpackfilter.allow", Value: "false"},
	{Key: "uploadpackfilter.blob:none.allow", Value: "true"},
	{Key: "uploadpackfilter.blob:limit.allow", Value: "true"},
	{Key: "uploadpackfilter.tree.maxDepth", Value: "0"},
	{Key: "uploadpackfilter.object:type.allow", Value: "true"},
	{Key: "uploadpackfilter.combine.allow", Value: "true"},
	{Key: "uploadpack.keepAlive", Value: "5"},
}

func read(dir string, args ...string) gitproc.Cmd {
	return gitproc.Cmd{Dir: dir, Args: args, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
		Config: []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}}}
}

func batchCheck(dir string, ids ...string) gitproc.Cmd {
	c := read(dir, "cat-file", "--batch-check")
	c.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	return c
}

func maintenance(dir string, args ...string) gitproc.Cmd {
	return gitproc.Cmd{Dir: dir, Args: args, Class: gitproc.ClassMaintenance, Deadline: gitproc.ReadDeadline}
}

// indexPack is index-pack over a copy of a pack, run alone: the failure
// receive-pack's own index-pack meets. The class only picks the lane.
func indexPack(t *testing.T, pack string) gitproc.Cmd {
	t.Helper()
	cp := copyOf(t, pack)
	return gitproc.Cmd{Args: []string{"index-pack", "--rev-index", "-o", strings.TrimSuffix(cp, ".pack") + ".idx", cp},
		Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(size(t, cp))}
}

// v is a git version, for the ranges a format is printed over.
func v(major, minor int) gitproc.Version { return gitproc.Version{Major: major, Minor: minor} }

// before reports whether running is older than at.
func before(running, at gitproc.Version) bool { return !running.AtLeast(at) }

// The formats git prints over only part of the floor's range of versions:
// [From, To), To zero for every later git. Every other row is printed by every
// git from the floor (2.40). Measured on 2.40.4, 2.43.0, 2.47.3, 2.48.0 and
// 2.56.0, and from git's source (and the planning pass's 2.55.0) where a
// boundary falls between two of those.
type span struct{ From, To gitproc.Version }

func (s span) holds(running gitproc.Version) bool {
	return running.AtLeast(s.From) && (s.To == (gitproc.Version{}) || before(running, s.To))
}

var printedOver = map[string]span{
	// 2.56 formats the sentence into a buffer and dies with "%s" (the hint).
	`packed object %s (stored in %s) is corrupt`: {v(2, 40), v(2, 56)},
	// Before 2.47 the same damage is a segmentation fault.
	`invalid MIDX object position, MIDX is likely corrupt`: {v(2, 47), gitproc.Version{}},
	// The required chunks, reworded at 2.43.
	`multi-pack-index missing required pack-name chunk`:                   {v(2, 40), v(2, 43)},
	`multi-pack-index missing required OID fanout chunk`:                  {v(2, 40), v(2, 43)},
	`multi-pack-index missing required OID lookup chunk`:                  {v(2, 40), v(2, 43)},
	`multi-pack-index missing required object offsets chunk`:              {v(2, 40), v(2, 43)},
	`multi-pack-index required pack-name chunk missing or corrupted`:      {v(2, 43), gitproc.Version{}},
	`multi-pack-index required OID fanout chunk missing or corrupted`:     {v(2, 43), gitproc.Version{}},
	`multi-pack-index required OID lookup chunk missing or corrupted`:     {v(2, 43), gitproc.Version{}},
	`multi-pack-index required object offsets chunk missing or corrupted`: {v(2, 43), gitproc.Version{}},
	// The graph's fanout chunk, reworded at 2.44; its size checked from 2.43.
	`commit-graph is missing the OID Fanout chunk`:                {v(2, 40), v(2, 44)},
	`commit-graph oid fanout chunk is wrong size`:                 {v(2, 43), gitproc.Version{}},
	`commit-graph required OID fanout chunk missing or corrupted`: {v(2, 44), gitproc.Version{}},
	// A chunk's alignment, checked from 2.43.
	`chunk id %x not %d-byte aligned`: {v(2, 43), gitproc.Version{}},
}

// span is the versions that print format.
func spanOf(format string) span {
	if s, ok := printedOver[format]; ok {
		return s
	}
	return span{v(2, 40), gitproc.Version{}}
}

// result is what a run is held to: the table's formats its events matched
// (spelled as the table spells them), its kind — nil for a run that answered
// — the parts its Damage named (none: no Damage), the hints it recorded, and
// whether a signal ended it.
type result struct {
	rows   []string
	kind   error
	parts  gitproc.Part
	hints  int
	signal bool
}

// arm is one command over one case's damage, and what it is held to on the
// running git.
type arm struct {
	name string
	cmd  func(t *testing.T, d *damaged) gitproc.Cmd
	want func(running gitproc.Version) result
}

// damaged is one case's damaged copy.
type damaged struct {
	f                   *fixture
	p                   *gitproc.Pool
	dir, older, newer   string
	deltaTree, baseTree string // the delta fixture's
	stdin               []byte // a case's pack or push, for a command reading its stdin
}

// damageCase is one damage and the commands that meet it; drops is
// Damage.Drops() for a derived damage.
type damageCase struct {
	name   string
	setup  func(t *testing.T, d *damaged) // nil: the fixture's two packs
	damage func(t *testing.T, d *damaged)
	drops  gitproc.Part
	arms   []arm
}

func always(r result) func(gitproc.Version) result { return func(gitproc.Version) result { return r } }

func rows(r ...string) []string { return r }

// on is the arm cmd with the same result on every git.
func on(name string, cmd func(t *testing.T, d *damaged) gitproc.Cmd, r result) arm {
	return arm{name: name, cmd: cmd, want: always(r)}
}

func revList(_ *testing.T, d *damaged) gitproc.Cmd {
	return read(d.dir, "rev-list", "--objects", d.f.Tip())
}

func mergeBase(_ *testing.T, d *damaged) gitproc.Cmd {
	c := read(d.dir, "merge-base", "--is-ancestor", d.f.Commits[2], d.f.Tip())
	c.Answers = []int{1}
	return c
}

func catTip(_ *testing.T, d *damaged) gitproc.Cmd { return batchCheck(d.dir, d.f.Tip()) }

func fetchTip(_ *testing.T, d *damaged) gitproc.Cmd { return fetchV2(d.dir, d.f.Tip()) }

func bitmapWalk(_ *testing.T, d *damaged) gitproc.Cmd {
	return read(d.dir, "rev-list", "--objects", "--use-bitmap-index", "--count", "--all")
}

// writeMidx writes the maintainer's multi-pack-index with its bitmap over d's
// packs.
func writeMidx(t *testing.T, d *damaged) {
	t.Helper()
	c := maintenance(d.dir, "multi-pack-index", "write", "--bitmap", "--no-progress")
	c.Config = []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"},
		{Key: "pack.writeBitmapLookupTable", Value: "true"}}
	if _, err := d.p.Run(t.Context(), c); err != nil {
		t.Fatal(err)
	}
}

// writeGraph writes the maintainer's commit-graph over d's refs.
func writeGraph(t *testing.T, d *damaged) {
	t.Helper()
	if _, err := d.p.Run(t.Context(), maintenance(d.dir, "commit-graph", "write", "--reachable", "--no-progress")); err != nil {
		t.Fatal(err)
	}
}

func (d *damaged) graph() string {
	return filepath.Join(d.dir, "objects", "info", "commit-graph")
}

// removePack removes stem's three files.
func removePack(t *testing.T, stem string) {
	t.Helper()
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(stem + ext); err != nil {
			t.Fatal(err)
		}
	}
}

// formatCases is every damage a cache can hold that git names — and the
// commands over it that the engine runs — with what each git says. Each case
// damages its own copy, every byte offset read from the file's own header and
// table of contents.
func formatCases(f *fixture) []damageCase {
	corrupt := func(r ...string) result { return result{rows: r, kind: gitproc.ErrCorrupt, parts: gitproc.PartPack} }
	missing := func(r ...string) result {
		return result{rows: r, kind: gitproc.ErrMissingObject, parts: gitproc.PartObject}
	}
	// A run that answered over damage it reported: exit 0, the damage named.
	answered := func(parts gitproc.Part, r ...string) result { return result{rows: r, parts: parts} }
	midxFatal := func(r ...string) result {
		return result{rows: r, kind: gitproc.ErrCorrupt, parts: gitproc.PartMultiPackIndex}
	}
	packed := `packed object %s (stored in %s) is corrupt`
	notOurs := `git upload-pack: not our ref %s`
	required := func(chunk string) func(gitproc.Version) result {
		return func(running gitproc.Version) result {
			if before(running, v(2, 43)) {
				return midxFatal(`multi-pack-index missing required ` + chunk + ` chunk`)
			}
			return midxFatal(`multi-pack-index required ` + chunk + ` chunk missing or corrupted`)
		}
	}
	midxCase := func(name string, damage func(t *testing.T, d *damaged), drops gitproc.Part, arms ...arm) damageCase {
		return damageCase{name: name, setup: writeMidx, damage: damage, drops: drops, arms: arms}
	}
	graphCase := func(name string, damage func(t *testing.T, d *damaged), drops gitproc.Part, arms ...arm) damageCase {
		return damageCase{name: name, setup: writeGraph, damage: damage, drops: drops, arms: arms}
	}
	// Every reader of a multi-pack-index damage that is fatal for whatever
	// loads the index.
	fatalForAll := func(want func(gitproc.Version) result) []arm {
		return []arm{
			{name: "rev-list", cmd: revList, want: want},
			{name: "merge-base", cmd: mergeBase, want: want},
			{name: "cat-file --batch-check", cmd: catTip, want: want},
			{name: "upload-pack v2", cmd: fetchTip, want: want},
		}
	}
	// Every reader of a damage git reads around: exit 0, the damage named.
	tolerated := func(parts gitproc.Part, r ...string) []arm {
		return []arm{
			on("rev-list", revList, answered(parts, r...)),
			on("cat-file --batch-check", catTip, answered(parts, r...)),
		}
	}
	graphTolerated := func(parts gitproc.Part, r ...string) []arm {
		return []arm{
			on("rev-list", revList, answered(parts, r...)),
			on("merge-base", mergeBase, answered(parts, r...)),
			on("upload-pack v2", fetchTip, answered(parts, r...)),
		}
	}

	return []damageCase{
		{name: "A the newer pack removed", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.PackRemoved, f.Tip()) }, arms: []arm{
			on("rev-list", revList, missing(`bad object %s`)),
			on("cat-file -t", func(_ *testing.T, d *damaged) gitproc.Cmd { return read(d.dir, "cat-file", "-t", f.Tip()) },
				missing(`git cat-file: could not get object info`)),
			on("merge-base", mergeBase, missing(`Not a valid commit name %s`)),
			on("fsck --connectivity-only", func(_ *testing.T, d *damaged) gitproc.Cmd {
				return maintenance(d.dir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
			}, missing(`%s: invalid sha1 pointer %s`)),
			on("upload-pack v2", fetchTip, result{rows: rows(notOurs), kind: gitproc.ErrNotOurRef}),
		}},
		{name: "A2 the older pack removed", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.PackRemoved, f.Commits[2]) }, arms: []arm{
			on("rev-list", revList, missing(`Could not read %s`, `Failed to traverse parents of commit %s`)),
			on("rev-list --topo-order", func(_ *testing.T, d *damaged) gitproc.Cmd {
				return read(d.dir, "rev-list", "--topo-order", f.Tip())
			}, missing(`Could not read %s`)),
			{name: "merge-base of the tip's parent", cmd: func(_ *testing.T, d *damaged) gitproc.Cmd {
				return read(d.dir, "merge-base", f.Tip()+"~1", f.Tip())
			}, want: func(running gitproc.Version) result {
				if running.AtLeast(v(2, 55)) {
					return missing(`Could not read %s`, `Not a valid commit name %s`)
				}
				return missing(`Not a valid commit name %s`)
			}},
			on("upload-pack v2 with progress", func(_ *testing.T, d *damaged) gitproc.Cmd {
				return fetchV2Progress(d.dir, f.Tip())
			}, missing(`Could not read %s`, `Failed to traverse parents of commit %s`)),
		}},
		{name: "B the newer index truncated", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.IdxTruncated, f.Tip()) }, arms: []arm{
			on("rev-list", revList, corrupt(`index file %s is too small`, `bad object %s`)),
			// It also reports "not our ref" for the tip it can no longer
			// find: damage outranks it.
			on("upload-pack v2", fetchTip, corrupt(`index file %s is too small`, notOurs)),
			on("cat-file --batch-check", catTip, answered(gitproc.PartPack, `index file %s is too small`)),
		}},
		{name: "C eight bytes flipped in the newer pack", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.PackFlipped, f.Tip()) }, arms: []arm{
			{name: "cat-file -p", cmd: func(_ *testing.T, d *damaged) gitproc.Cmd {
				return read(d.dir, "cat-file", "-p", f.Tip()+":big.txt")
			}, want: func(running gitproc.Version) result {
				if running.AtLeast(v(2, 56)) {
					r := corrupt(`inflate: %s (%s)`)
					r.hints = 1
					return r
				}
				return corrupt(`inflate: %s (%s)`, packed)
			}},
			on("fsck", func(_ *testing.T, d *damaged) gitproc.Cmd {
				return maintenance(d.dir, "fsck", "--no-dangling", "--no-progress")
			}, corrupt(`%s pack checksum mismatch`, `index CRC mismatch for object %s from %s at offset %lu`,
				`inflate: %s (%s)`, `cannot unpack %s from %s at offset %lu`)),
			on("index-pack", func(t *testing.T, d *damaged) gitproc.Cmd { return indexPack(t, d.newer+".pack") },
				corrupt(`inflate: %s (%s)`, `pack has bad object at offset %lu: %s`)),
			{name: "pack-objects", cmd: func(t *testing.T, d *damaged) gitproc.Cmd {
				c := read(d.dir, "pack-objects", "--revs", "-q", filepath.Join(t.TempDir(), "out"))
				c.Stdin = strings.NewReader(f.Tip() + "\n")
				return c
			}, want: func(running gitproc.Version) result {
				switch {
				case running.AtLeast(v(2, 56)):
					r := corrupt(`bad packed object CRC for %s`)
					r.hints = 1
					return r
				case running.AtLeast(v(2, 55)):
					return corrupt(`bad packed object CRC for %s`, packed)
				}
				return corrupt(`bad packed object CRC for %s`, `unable to read %s`)
			}},
		}},
		{name: "D the newer pack truncated to a third", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.PackTruncated, f.Tip()) }, arms: []arm{
			on("rev-list", revList, corrupt(`packfile %s does not match index`, `bad object %s`)),
			on("index-pack", func(t *testing.T, d *damaged) gitproc.Cmd { return indexPack(t, d.newer+".pack") },
				corrupt(`early EOF`)),
			on("upload-pack v2", fetchTip, corrupt(`packfile %s does not match index`, notOurs)),
		}},
		// THE BITMAP'S POSITIONS, which the reverse index maps: the
		// maintainer's whole multi-pack-index with its bitmap, as a clone
		// reads it, and its reverse index (the RIDX chunk) overwritten —
		// every position it holds past the index's objects. The bitmap itself
		// is left whole: overwritten too, git 2.56 stops at the bitmap first
		// and says only "Out of memory, realloc failed". Before 2.47 git does
		// not check the positions, and dies of a segmentation fault saying
		// nothing a table can read.
		midxCase("E a multi-pack-index whose reverse index is overwritten beside its bitmap", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxRidx) }, gitproc.PartMultiPackIndex,
			arm{name: "rev-list over its bitmap", cmd: bitmapWalk, want: func(running gitproc.Version) result {
				if before(running, gittest.NamesMidxPosition) {
					return result{kind: gitproc.ErrFailed, signal: true}
				}
				return midxFatal(`invalid MIDX object position, MIDX is likely corrupt`)
			}},
			arm{name: "upload-pack v2", cmd: fetchTip, want: func(running gitproc.Version) result {
				if before(running, gittest.NamesMidxPosition) {
					return result{kind: gitproc.ErrFailed}
				}
				return midxFatal(`invalid MIDX object position, MIDX is likely corrupt`)
			}}),
		midxCase("F a multi-pack-index with its signature overwritten", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxSignature) }, gitproc.PartMultiPackIndex,
			on("rev-list", revList, midxFatal(`multi-pack-index signature 0x%08x does not match signature 0x%08x`)),
			on("upload-pack v2", fetchTip, midxFatal(`multi-pack-index signature 0x%08x does not match signature 0x%08x`))),
		midxCase("F2 a multi-pack-index truncated", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxTruncated) },
			gitproc.PartMultiPackIndex, tolerated(gitproc.PartMultiPackIndex, `multi-pack-index file %s is too small`)...),
		midxCase("F3 a multi-pack-index's version", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxVersion) },
			gitproc.PartMultiPackIndex, on("rev-list", revList, midxFatal(`multi-pack-index version %d not recognized`))),
		midxCase("F4 a multi-pack-index's hash version", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxHashVersion) },
			gitproc.PartMultiPackIndex, tolerated(gitproc.PartMultiPackIndex,
				`multi-pack-index hash version %u does not match version %u`)...),
		midxCase("F5 a multi-pack-index counting no chunks", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxNoChunks) },
			gitproc.PartMultiPackIndex|gitproc.PartCommitGraph, tolerated(gitproc.PartChunk, `final chunk has non-zero id %x`)...),
		midxCase("F6 a multi-pack-index's first chunk id zeroed", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxFirstChunkZeroed) }, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			tolerated(gitproc.PartChunk, `terminating chunk id appears earlier than expected`)...),
		midxCase("F7 a multi-pack-index's pack names missing", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxPackNamesMissing) }, gitproc.PartMultiPackIndex, fatalForAll(required("pack-name"))...),
		midxCase("F8 a multi-pack-index's fanout missing", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxFanoutMissing) }, gitproc.PartMultiPackIndex, fatalForAll(required("OID fanout"))...),
		midxCase("F8L a multi-pack-index's lookup missing", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxLookupMissing) }, gitproc.PartMultiPackIndex, fatalForAll(required("OID lookup"))...),
		midxCase("F8O a multi-pack-index's offsets missing", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxOffsetsMissing) }, gitproc.PartMultiPackIndex, fatalForAll(required("object offsets"))...),
		midxCase("F9 a multi-pack-index's pack names out of order", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxPackNamesOrder) }, gitproc.PartMultiPackIndex, fatalForAll(always(midxFatal(`multi-pack-index pack names out of order: '%s' before '%s'`)))...),
		midxCase("F10 a multi-pack-index's chunk offset past its end", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxChunkPastEnd) }, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			tolerated(gitproc.PartChunk, `improper chunk offset(s) %lx and %lx`)...),
		midxCase("F11 a multi-pack-index's chunk misaligned", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxChunkMisaligned) }, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			arm{name: "rev-list", cmd: revList, want: func(running gitproc.Version) result {
				if before(running, v(2, 43)) {
					return answered(gitproc.PartChunk, `improper chunk offset(s) %lx and %lx`)
				}
				return answered(gitproc.PartChunk, `chunk id %x not %d-byte aligned`)
			}}),
		midxCase("F12 a multi-pack-index's chunk named twice", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.MidxChunkTwice) }, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			tolerated(gitproc.PartChunk, `duplicate chunk ID %x found`)...),
		{name: "G the tip commit's compressed bytes flipped", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.EntryBytes, f.Tip()) }, arms: []arm{
			{name: "upload-pack v2", cmd: fetchTip, want: func(running gitproc.Version) result {
				switch {
				case running.AtLeast(v(2, 56)):
					r := corrupt(`inflate: %s (%s)`)
					r.hints = 1
					return r
				case running.AtLeast(v(2, 55)):
					return corrupt(`inflate: %s (%s)`, packed)
				}
				return corrupt(`inflate: %s (%s)`, notOurs)
			}},
			{name: "rev-list", cmd: revList, want: func(running gitproc.Version) result {
				switch {
				case running.AtLeast(v(2, 56)):
					r := corrupt(`inflate: %s (%s)`)
					r.hints = 1
					return r
				case running.AtLeast(v(2, 55)):
					return corrupt(`inflate: %s (%s)`, packed)
				}
				return corrupt(`inflate: %s (%s)`, `bad object %s`)
			}},
		}},
		{name: "G1 the tip commit's type", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.EntryType, f.Tip()) }, arms: []arm{
			{name: "upload-pack v2", cmd: fetchTip, want: unknownType},
			{name: "rev-list", cmd: revList, want: unknownType},
		}},
		{name: "H the big blob in a pack of its own, removed", setup: func(t *testing.T, d *damaged) {
			// The copy again: the older pack, the tip's commit and tree in a
			// pack, and the big blob alone in a third.
			d.dir = gittest.Empty(t)
			gittest.Pack(t, f.Client, d.dir, f.Work, f.Commits[2])
			gittest.PackObjects(t, f.Client, d.dir, f.Work, f.Tip(),
				strings.TrimSpace(f.Client.Run(t, f.Work, "rev-parse", f.Tip()+"^{tree}")))
			only := gittest.PackObjects(t, f.Client, d.dir, f.Work,
				strings.TrimSpace(f.Client.Run(t, f.Work, "rev-parse", f.Tip()+":big.txt")))
			d.newer = filepath.Join(d.dir, "objects", "pack", "pack-"+only)
			gittest.SetRefs(t, d.dir, map[string]string{"refs/heads/main": f.Tip()})
		}, damage: func(t *testing.T, d *damaged) {
			blob := strings.TrimSpace(f.Client.Run(t, f.Work, "rev-parse", f.Tip()+":big.txt"))
			gittest.Damage(t, d.dir, gittest.PackRemoved, blob)
		}, arms: []arm{
			on("upload-pack v2", fetchTip, missing(`unable to read %s`)),
			on("rev-list", revList, missing(`missing %s object '%s'`)),
			// fsck's findings are on its stdout, never a report: its verdict
			// is the check's to read, and names no kind.
			on("fsck --connectivity-only", func(_ *testing.T, d *damaged) gitproc.Cmd {
				return maintenance(d.dir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
			}, result{kind: gitproc.ErrFailed}),
		}},
		graphCase("I a commit-graph's signature", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphSignature) }, gitproc.PartCommitGraph, graphTolerated(gitproc.PartCommitGraph, `commit-graph signature %X does not match signature %X`)...),
		graphCase("I2 a commit-graph truncated", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphTruncated) },
			gitproc.PartCommitGraph, on("rev-list", revList, answered(gitproc.PartCommitGraph, `commit-graph file is too small`))),
		graphCase("I3 a commit-graph's version", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphVersion) },
			gitproc.PartCommitGraph, on("rev-list", revList, answered(gitproc.PartCommitGraph,
				`commit-graph version %X does not match version %X`))),
		graphCase("I4 a commit-graph's hash version", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphHashVersion) },
			gitproc.PartCommitGraph, on("rev-list", revList, answered(gitproc.PartCommitGraph,
				`commit-graph hash version %X does not match version %X`))),
		graphCase("I5 a commit-graph's first chunk id zeroed", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphFirstChunkZeroed) }, gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			graphTolerated(gitproc.PartChunk, `terminating chunk id appears earlier than expected`)...),
		graphCase("I6 a commit-graph counting no chunks", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphNoChunks) },
			gitproc.PartMultiPackIndex|gitproc.PartCommitGraph,
			graphTolerated(gitproc.PartChunk, `final chunk has non-zero id %x`)...),
		graphCase("I7 a commit-graph's first chunk three bytes on", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphChunkShifted) }, gitproc.PartCommitGraph, arm{name: "rev-list", cmd: revList, want: func(running gitproc.Version) result {
			switch {
			case before(running, v(2, 43)):
				// No size check on the fanout before 2.43: a read past it.
				return result{kind: gitproc.ErrFailed, signal: true}
			case before(running, v(2, 44)):
				return answered(gitproc.PartCommitGraph, `commit-graph is missing the OID Fanout chunk`,
					`commit-graph oid fanout chunk is wrong size`)
			}
			return answered(gitproc.PartCommitGraph, `commit-graph oid fanout chunk is wrong size`,
				`commit-graph required OID fanout chunk missing or corrupted`)
		}}),
		graphCase("I8 a commit-graph's parent past its commits", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphParentPast, f.Tip()) }, gitproc.PartCommitGraph, on("rev-list", revList, result{rows: rows(`invalid parent position %u`),
			kind: gitproc.ErrCorrupt, parts: gitproc.PartCommitGraph})),
		// A PARENT THAT IS ANOTHER COMMIT OF THE GRAPH: git answers wrong and
		// says nothing — no table can see it; held so a git that starts
		// naming it is noticed.
		graphCase("I9 a commit-graph's parent pointing at its own commit", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphParentSelf, f.Tip()) }, 0, on("rev-list", revList, result{})),
		graphCase("I10 a commit-graph's fanout missing", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphFanoutMissing) }, gitproc.PartCommitGraph, arm{name: "rev-list", cmd: revList, want: func(running gitproc.Version) result {
			if before(running, v(2, 44)) {
				return answered(gitproc.PartCommitGraph, `commit-graph is missing the OID Fanout chunk`)
			}
			return answered(gitproc.PartCommitGraph, `commit-graph required OID fanout chunk missing or corrupted`)
		}}),
		graphCase("I11 a commit-graph's generation past its overflow", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.GraphGeneration, f.Tip()) }, gitproc.PartCommitGraph, on("rev-list", revList, result{
			rows: rows(`commit-graph requires overflow generation data but has none`),
			kind: gitproc.ErrCorrupt, parts: gitproc.PartCommitGraph})),
		midxCase("K1 a bitmap's signature", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.BitmapSignature) }, gitproc.PartMultiPackIndex, bitmapArms(answered(gitproc.PartMultiPackIndex, `corrupted bitmap index file (wrong header)`))...),
		midxCase("K2 a bitmap truncated", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.BitmapTruncated) },
			gitproc.PartMultiPackIndex, bitmapArms(answered(gitproc.PartMultiPackIndex, `corrupted bitmap index (too small)`))...),
		midxCase("K3 a bitmap's version", func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.BitmapVersion) },
			gitproc.PartMultiPackIndex, bitmapArms(answered(gitproc.PartMultiPackIndex,
				`unsupported version '%d' for bitmap index file`))...),
		{name: "M every index offset past the pack", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.IdxOffsetsPastPack) }, arms: []arm{
			on("rev-list", revList, corrupt(`offset beyond end of packfile (truncated pack?)`)),
			on("upload-pack v2", fetchTip, corrupt(`offset beyond end of packfile (truncated pack?)`)),
			on("cat-file --batch-check", catTip, corrupt(`offset beyond end of packfile (truncated pack?)`)),
		}},
		// A DELTA'S OWN BYTES: its zlib header mangled, so the delta cannot
		// be inflated over a base git read whole.
		{name: "P a delta's compressed data", setup: deltaCopy(f), damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.DeltaData, d.deltaTree) }, arms: []arm{
			{name: "cat-file -p", cmd: catDelta,
				want: deltaRead(`failed to unpack compressed delta at offset %lu from %s`)},
		}},
		// A DELTA'S BASE OFFSET past every object: git cannot even say what
		// type the delta is, and calls the object absent.
		{name: "P2 a delta's base offset past the pack's start", setup: deltaCopy(f), damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.DeltaBase, d.deltaTree) }, arms: []arm{
			on("cat-file -p", catDelta, missing(`Not a valid object name %s`)),
		}},
		// A DELTA'S BASE whose own bytes are damaged: git reads the delta
		// and cannot read what it applies to.
		{name: "P3 a delta base's compressed data", setup: deltaCopy(f), damage: func(t *testing.T, d *damaged) {
			gittest.Damage(t, d.dir, gittest.DeltaBaseData, d.deltaTree, d.baseTree)
		}, arms: []arm{
			{name: "cat-file -p", cmd: catDelta,
				want: deltaRead(`failed to read delta base object %s at offset %lu from %s`)},
		}},
		{name: "R1 every reverse index truncated", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.RevTruncated) },
			arms: revArms(`reverse-index file %s is too small`)},
		{name: "R2 every reverse index's signature", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.RevSignature) }, arms: revArms(`reverse-index file %s has unknown signature`)},
		{name: "R3 every reverse index's version", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.RevVersion) }, arms: revArms(`reverse-index file %s has unsupported version %u`)},
		{name: "R4 every reverse index's hash id", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.RevHashID) }, arms: revArms(`reverse-index file %s has unsupported hash id %u`)},
		{name: "R5 every reverse index four bytes long", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.RevAppended) }, arms: revArms(`reverse-index file %s is corrupt`)},
		{name: "S1 the tip commit's stated size one more", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.EntrySize, f.Tip()) }, arms: []arm{
			{name: "rev-list", cmd: revList, want: sizeOff(missing(`bad object %s`))},
			{name: "merge-base", cmd: mergeBase, want: sizeOff(missing(`Not a valid commit name %s`))},
			{name: "upload-pack v2", cmd: fetchTip, want: sizeOff(result{rows: rows(notOurs), kind: gitproc.ErrNotOurRef})},
		}},
		{name: "S2 the tip commit's checksum zeroed", damage: func(t *testing.T, d *damaged) { gittest.Damage(t, d.dir, gittest.EntryTail, f.Tip()) }, arms: []arm{
			{name: "rev-list", cmd: revList, want: func(running gitproc.Version) result {
				switch {
				case running.AtLeast(v(2, 56)):
					r := corrupt(`inflate: %s (%s)`)
					r.hints = 1
					return r
				case running.AtLeast(v(2, 55)):
					return corrupt(`inflate: %s (%s)`, packed)
				}
				return corrupt(`inflate: %s (%s)`, `bad object %s`)
			}},
		}},
		// WHAT A PUSH'S index-pack SAYS of the CLIENT's pack — kinds the
		// receiver reads as the client's, never the cache's.
		{name: "N1 a thin pack's one delta on an absent base", setup: thinPack(f, 1), arms: []arm{
			on("index-pack --stdin", indexStdin, corrupt(`pack has %d unresolved delta`)),
		}},
		{name: "N2 a thin pack's two deltas on absent bases", setup: thinPack(f, 2), arms: []arm{
			on("index-pack --stdin", indexStdin, corrupt(`pack has %d unresolved deltas`)),
		}},
		{name: "Q the tip commit alone", setup: func(t *testing.T, d *damaged) {
			stdout, stderr, err := f.Client.Try(t, f.Work, strings.NewReader(f.Tip()+"\n"), "pack-objects", "--stdout", "-q")
			if err != nil {
				t.Fatalf("pack-objects of the tip commit: %v\n%s", err, stderr)
			}
			d.dir, d.stdin = gittest.Empty(t), []byte(stdout)
		}, arms: []arm{
			on("index-pack --stdin --strict", func(_ *testing.T, d *damaged) gitproc.Cmd {
				c := indexStdin(nil, d)
				c.Args = append(c.Args, "--strict")
				return c
			}, missing(`did not receive expected object %s`)),
		}},
		{name: "a push whose pack is cut short", setup: func(t *testing.T, d *damaged) {
			stdout, stderr, err := f.Client.Try(t, f.Work, strings.NewReader(f.Tip()+"\n"),
				"pack-objects", "--revs", "--stdout", "-q")
			if err != nil {
				t.Fatalf("pack-objects of the history: %v\n%s", err, stderr)
			}
			d.dir = gittest.Empty(t)
			d.stdin = []byte(pkt(strings.Repeat("0", 40)+" "+f.Tip()+" refs/heads/main\x00report-status\n") +
				"0000" + stdout[:300])
		}, arms: []arm{
			// receive-pack answers in its report and exits 0; the failure is
			// its index-pack's, a child's report on the one stream.
			on("receive-pack", receiveCut, answered(gitproc.PartPack, `early EOF`)),
		}},
		// A READER OF A SPLIT GRAPH WHOSE LAYER WENT — what git prints of a
		// healthy repository when a reader races git's own rewrite of the
		// chain. It is no row, and the reader answers.
		{name: "a commit-graph chain missing a layer", setup: func(t *testing.T, d *damaged) {
			for range 2 {
				if _, err := d.p.Run(t.Context(), maintenance(d.dir, "commit-graph", "write", "--reachable", "--split",
					"--no-progress")); err != nil {
					t.Fatal(err)
				}
			}
		}, damage: func(t *testing.T, d *damaged) {
			layers, _ := filepath.Glob(filepath.Join(d.dir, "objects", "info", "commit-graphs", "graph-*.graph"))
			if len(layers) == 0 {
				t.Fatal("the split write left no layer")
			}
			for _, layer := range layers {
				if err := os.Remove(layer); err != nil {
					t.Fatal(err)
				}
			}
		}, arms: []arm{on("rev-list", revList, result{})}},
	}
}

// thinPack is a copy holding nothing and a thin pack of n deltas, each on a
// blob of the commit before, which the pack does not carry.
func thinPack(f *fixture, n int) func(t *testing.T, d *damaged) {
	return func(t *testing.T, d *damaged) {
		t.Helper()
		c := f.Client
		wt := c.Init(t)
		files, changed := map[string]string{}, map[string]string{}
		for i := range n {
			name := fmt.Sprintf("big%d.txt", i)
			// Each its own content, so each is a blob of its own and a
			// delta of its own.
			files[name] = name + "\n" + gittest.BigFile()
			changed[name] = "changed\n" + name + "\n" + gittest.BigFile()
		}
		base := c.Commit(t, wt, files, "base")
		tip := c.Commit(t, wt, changed, "changed")
		stdout, stderr, err := c.Try(t, wt, strings.NewReader(tip+"\n^"+base+"\n"),
			"pack-objects", "--revs", "--thin", "--stdout", "-q")
		if err != nil {
			t.Fatalf("a thin pack: %v\n%s", err, stderr)
		}
		d.dir, d.stdin = gittest.Empty(t), []byte(stdout)
	}
}

// indexStdin is index-pack reading d's pack on its stdin, in d's repository:
// what receive-pack's index-pack does with a push's pack.
func indexStdin(_ *testing.T, d *damaged) gitproc.Cmd {
	return gitproc.Cmd{Dir: d.dir, Args: []string{"index-pack", "--stdin"}, Stdin: bytes.NewReader(d.stdin),
		Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(int64(len(d.stdin)))}
}

// receiveCut is receive-pack taking d's push.
func receiveCut(_ *testing.T, d *damaged) gitproc.Cmd {
	indexed := make(chan struct{})
	close(indexed)
	return gitproc.Cmd{Dir: d.dir, Args: []string{"receive-pack", "--stateless-rpc", d.dir},
		Stdin: bytes.NewReader(d.stdin), Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline, Indexed: indexed}
}

// unknownType is what an entry of an unknown type is reported as.
func unknownType(running gitproc.Version) result {
	r := result{rows: rows(`unknown object type %i at offset %lu in %s`), kind: gitproc.ErrCorrupt,
		parts: gitproc.PartPack}
	if running.AtLeast(v(2, 56)) {
		r.hints = 1
		return r
	}
	r.rows = append(r.rows, `packed object %s (stored in %s) is corrupt`)
	return r
}

// sizeOff is S1 on each git: before 2.55 what an object git cannot find is
// reported as (older), 2.55 the corrupt object's own row, and from 2.56 that
// sentence through "%s" alone — no kind, no damage, one hint.
func sizeOff(older result) func(gitproc.Version) result {
	return func(running gitproc.Version) result {
		switch {
		case running.AtLeast(v(2, 56)):
			return result{kind: gitproc.ErrFailed, hints: 1}
		case running.AtLeast(v(2, 55)):
			return result{rows: rows(`packed object %s (stored in %s) is corrupt`), kind: gitproc.ErrCorrupt,
				parts: gitproc.PartPack}
		}
		return older
	}
}

// bitmapArms is a bitmap's damage, met by a clone and by a bitmapped walk.
func bitmapArms(r result) []arm {
	return []arm{on("upload-pack v2", fetchTip, r), on("rev-list over its bitmap", bitmapWalk, r)}
}

// revArms is a reverse index's damage: git builds the index in memory beside
// it, so every reader answers, and only a pack-objects names it.
func revArms(format string) []arm {
	return []arm{
		on("upload-pack v2", fetchTip, result{rows: rows(format), parts: gitproc.PartPack}),
		on("rev-list", revList, result{}),
	}
}

// deltaCopy replaces the case's copy with one holding a directory of three
// hundred files and a commit that changes one of them, both root trees in ONE
// pack written as a push's pack is (--delta-base-offset: receive-pack
// advertises ofs-delta, so every client sends one), so one tree is stored as
// an OFS_DELTA on the other. Trees rather than blobs because cat-file -p
// STREAMS a blob, asking its size first, and a damaged delta then fails in
// the size's lookup ("delta data unpack-initial failed", which names nothing)
// before any reader resolves it; a tree is read whole, as every walk reads it.
func deltaCopy(f *fixture) func(t *testing.T, d *damaged) {
	return func(t *testing.T, d *damaged) {
		t.Helper()
		c := f.Client
		wt := c.Init(t)
		files := map[string]string{}
		for i := range 300 {
			files[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
		}
		c.Commit(t, wt, files, "base")
		base := strings.TrimSpace(c.Run(t, wt, "rev-parse", "HEAD^{tree}"))
		tip := c.Commit(t, wt, map[string]string{"f150.txt": "changed\n"}, "changed")
		next := strings.TrimSpace(c.Run(t, wt, "rev-parse", "HEAD^{tree}"))
		d.dir = gittest.Empty(t)
		stdout, stderr, err := c.Try(t, wt, strings.NewReader(tip+"\n"), "-c", "pack.writeReverseIndex=true",
			"pack-objects", "--revs", "--delta-base-offset", "-q", filepath.Join(d.dir, "objects", "pack", "pack"))
		if err != nil {
			t.Fatalf("packing the delta's history: %v\n%s", err, stderr)
		}
		cs := strings.TrimSpace(stdout)
		gittest.SetRefs(t, d.dir, map[string]string{"refs/heads/main": tip})
		d.newer = filepath.Join(d.dir, "objects", "pack", "pack-"+cs)
		for _, tree := range []string{base, next} {
			start, _ := gittest.PackEntry(t, d.newer+".idx", tree)
			if typ := readByte(t, d.newer+".pack", start) >> 4 & 7; typ == 6 {
				d.deltaTree = tree
			} else {
				d.baseTree = tree
			}
		}
		if d.deltaTree == "" || d.baseTree == "" {
			t.Fatal("neither root tree is stored as an OFS_DELTA on the other")
		}
	}
}

func catDelta(_ *testing.T, d *damaged) gitproc.Cmd {
	return read(d.dir, "cat-file", "-p", d.deltaTree)
}

// deltaRead is what reading a tree git cannot resolve from its delta reports:
// zlib's own failure and the step of the delta's resolution that met it
// (row). The reader then dies with "not a tree object", which names no
// damage. Before 2.56 no "packed object … is corrupt" follows (a blob's read
// prints it; a tree's does not); from 2.56 it does, through a bare "%s" — a
// hint naming the pack, beside the rows.
func deltaRead(row string) func(gitproc.Version) result {
	return func(running gitproc.Version) result {
		r := result{rows: rows(`inflate: %s (%s)`, row), kind: gitproc.ErrCorrupt, parts: gitproc.PartPack}
		if running.AtLeast(v(2, 56)) {
			r.hints = 1
		}
		return r
	}
}

// tableRows is the formats of the table among a run's formats, spelled as the
// table spells them.
func tableRows(t gitproc.Trace) []string {
	byCanon := map[string]string{}
	for f := range gitproc.FormatTable() {
		byCanon[gitproc.CanonicalFormat(f)] = f
	}
	var out []string
	for _, c := range t.Formats {
		if f, ok := byCanon[c]; ok {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// THE FORMAT TABLE IS WHAT GIT SAYS, BOTH WAYS. Every damage a cache can hold
// that git names, the commands the engine runs over it, run through the pool
// on the git gittest.Require finds: the run's kind, its Damage (kind, parts,
// Derived, Drops), its hints, and the TABLE ROWS its events matched — equal to
// the rows the case expects on THIS git's version, so a row git stops printing
// fails as surely as one it starts printing beside the case's own. And every
// row of the table is expected by some case on every git that prints it, so a
// row nothing provokes fails too. A crash the case expects (a git before the
// check that names it) is held as a crash, never skipped.
//
// Mutations, each red: a row deleted (its case finds a format the table
// lacks), a row's kind or part swapped, a row added that no case provokes, a
// span narrowed or widened, canonicalFormat replaced by the identity (every
// %lu row), PartChunk dropping only the index (the I5 and F5 drops).
func TestTheFormatTableIsWhatGitSays(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	running := p.Version()
	expected := map[string]bool{}
	for _, dc := range formatCases(f) {
		for _, a := range dc.arms {
			want := a.want(running)
			for _, r := range want.rows {
				expected[r] = true
				if !spanOf(r).holds(running) {
					t.Errorf("%s/%s expects %q on git %s, outside the versions the table says print it (%v)",
						dc.name, a.name, r, running, spanOf(r))
				}
			}
			t.Run(dc.name+"/"+a.name, func(t *testing.T) {
				d := &damaged{f: f, p: p}
				d.dir, d.older, d.newer = f.packed(t)
				if dc.setup != nil {
					dc.setup(t, d)
				}
				if dc.damage != nil {
					dc.damage(t, d)
				}
				c := a.cmd(t, d)
				res, err := p.Run(t.Context(), c)
				got := tableRows(res.Trace)
				wantRows := slices.Sorted(slices.Values(want.rows))
				if !slices.Equal(got, wantRows) {
					t.Errorf("git %s's events matched rows %q, want %q (every format: %q; stderr %q)",
						running, got, wantRows, res.Trace.Formats, res.Stderr)
				}
				var exit *gitproc.ExitError
				switch {
				case want.kind == nil && err != nil:
					t.Errorf("%v (formats %q), want an answer", err, res.Trace.Formats)
				case want.kind != nil && (!errors.As(err, &exit) || !errors.Is(exit.Kind, want.kind)):
					t.Errorf("%v (formats %q), want an ExitError of kind %v", err, res.Trace.Formats, want.kind)
				}
				if want.signal != (res.Exit < 0) {
					t.Errorf("exit %d, want a signal %v", res.Exit, want.signal)
				}
				var dmg *gitproc.Damage
				switch {
				case want.parts == 0 && res.Damage != nil:
					t.Errorf("Result.Damage = %v, want none", res.Damage)
				case want.parts != 0 && !errors.As(res.Damage, &dmg):
					t.Errorf("Result.Damage = %v, want damage of %v", res.Damage, want.parts)
				case want.parts != 0:
					wantKind := gitproc.ErrCorrupt
					if want.parts == gitproc.PartObject {
						wantKind = gitproc.ErrMissingObject
					}
					if dmg.Parts != want.parts || !errors.Is(dmg.Kind, wantKind) {
						t.Errorf("Damage = %v of %v, want %v of %v", dmg.Kind, dmg.Parts, wantKind, want.parts)
					}
					if derived := dc.drops != 0; dmg.Derived() != derived || (derived && dmg.Drops() != dc.drops) {
						t.Errorf("Damage is derived %v dropping %v, want derived %v dropping %v",
							dmg.Derived(), dmg.Drops(), derived, dc.drops)
					}
				}
				if len(res.Trace.Hints) != want.hints {
					t.Errorf("hints %v, want %d", res.Trace.Hints, want.hints)
				}
				if exit != nil {
					if last := res.LastLine(); last != "" && !strings.Contains(err.Error(), last) {
						t.Errorf("the error %q does not carry git's last line %q", err, last)
					}
					if dmg != nil && !strings.Contains(err.Error(), "git named: "+dmg.Message[:min(len(dmg.Message), 100)]) {
						t.Errorf("the error %q does not carry the damage git named, %q", err, dmg.Message)
					}
				}
			})
		}
	}
	for format := range gitproc.FormatTable() {
		if spanOf(format).holds(running) && !expected[format] {
			t.Errorf("no case provokes %q on git %s, which prints it: a row nothing provokes is a row nobody "+
				"checked", format, running)
		}
	}
	for format := range printedOver {
		if _, ok := gitproc.FormatTable()[format]; !ok {
			t.Errorf("the spans name %q, which is no row", format)
		}
	}
}

// NO ROW IS A FORMAT A REQUEST WRITES, NOR ONE GIT PRINTS OF A HEALTHY
// REPOSITORY. Every key is canonical once and unique, has a word of its own
// outside its conversions, and is none of the formats a request was measured
// to make upload-pack die with (requestFormats) nor the two a reader racing
// git's own split or incremental write prints.
//
// Mutation: add "%s", `unexpected line: '%s'` or `unable to find all
// commit-graph files` as a row, and this fails.
func TestNoRowIsAFormatARequestWrites(t *testing.T) {
	t.Parallel()
	forbidden := map[string]bool{
		gitproc.CanonicalFormat(`unable to find all commit-graph files`):     true,
		gitproc.CanonicalFormat(`unable to find all multi-pack index files`): true,
	}
	for f := range requestFormats {
		forbidden[gitproc.CanonicalFormat(f)] = true
	}
	seen := map[string]string{}
	for format, row := range gitproc.FormatTable() {
		c := gitproc.CanonicalFormat(format)
		if prev, ok := seen[c]; ok {
			t.Errorf("%q and %q are one canonical format", prev, format)
		}
		seen[c] = format
		if gitproc.CanonicalFormat(c) != c {
			t.Errorf("%q's canonical form %q is not canonical", format, c)
		}
		if forbidden[c] {
			t.Errorf("%q is a row, and a request or a healthy repository makes git print it", format)
		}
		words := strings.FieldsFunc(strings.ReplaceAll(c, "%_", " "), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
		})
		if !slices.ContainsFunc(words, func(w string) bool { return len(w) >= 3 }) {
			t.Errorf("%q has no word of git's own: its argument is all it says", format)
		}
		if row.Kind == nil || (row.Part == 0) != errors.Is(row.Kind, gitproc.ErrNotOurRef) {
			t.Errorf("%q classifies as %v of %v", format, row.Kind, row.Part)
		}
	}
}

// requestFormats is every format a request was measured to make upload-pack
// die with, against a healthy copy: the planning pass's 74 forms on four gits,
// which TestARequestCannotNameTheDamage replays — and two the replay found
// beside them on 2.43 and 2.47, the response-end refusal and the object
// format's mismatch spelled with the newline 2.43's format carries.
var requestFormats = map[string]bool{
	`%s`:                       true,
	`unexpected line: '%s'`:    true,
	`invalid shallow line: %s`: true,
	`Invalid deepen: %s`:       true,
	`Invalid deepen-since: %s`: true,
	`git upload-pack: ambiguous deepen-not: %s`:                            true,
	`git upload-pack: deepen-not is not a ref: %s`:                         true,
	`git upload-pack: expected SHA1 object, got '%s'`:                      true,
	`git upload-pack: protocol error, expected to get object ID, not '%s'`: true,
	`git upload-pack: protocol error, expected to get oid, not '%s'`:       true,
	`invalid command '%s'`:                                                 true,
	`unknown capability '%s'`:                                              true,
	`unknown object format '%s'`:                                           true,
	`command '%s' requested after already requesting command '%s'`:         true,
	`expected flush after fetch arguments`:                                 true,
	`git upload-pack: expected SHA1 list, got '%s'`:                        true,
	`invalid shallow object %s`:                                            true,
	"mismatched object format: server %s; client %s\n":                     true,
	`mismatched object format: server %s; client %s`:                       true,
	`unexpected response end packet`:                                       true,
	`no commits selected for shallow requests`:                             true,
	`protocol error: bad line length character: %.4s`:                      true,
	`the remote end hung up unexpectedly`:                                  true,
}

// THE CANONICAL FORM MATCHES EVERY CONVERSION: a platform's PRIuMAX (lu on
// Linux, ju on macOS, llu elsewhere), a width, a precision, a star — each one
// conversion — while a doubled percent stays, and the text around them is
// untouched.
//
// Mutation: keep length modifiers in the canonical form, and the %ju arm
// fails.
func TestTheCanonicalFormMatchesEveryConversion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`at offset %lu`:               `at offset %_`,
		`at offset %ju`:               `at offset %_`,
		`at offset %llu`:              `at offset %_`,
		`signature 0x%08x`:            `signature 0x%_`,
		`signature %X`:                `signature %_`,
		`character: %.4s`:             `character: %_`,
		`%*s`:                         `%_`,
		`type %i at %d`:               `type %_ at %_`,
		`100%% done`:                  `100%% done`,
		`no conversion`:               `no conversion`,
		`a %% then %s`:                `a %% then %_`,
		`trailing %`:                  `trailing %`,
		`%s: invalid sha1 pointer %s`: `%_: invalid sha1 pointer %_`,
	} {
		if got := gitproc.CanonicalFormat(in); got != want {
			t.Errorf("CanonicalFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if gitproc.CanonicalFormat(`index CRC mismatch for object %s from %s at offset %ju`) !=
		gitproc.CanonicalFormat(`index CRC mismatch for object %s from %s at offset %lu`) {
		t.Error("macOS's PRIuMAX spelling of a row is not the row")
	}
}

// AN ANSWER READ OVER DAMAGE SAYS SO, whatever the exit status: cat-file
// --batch-check calls the tip of a pack whose index is truncated `missing`
// and exits 0, only its report naming the index, so Result.Damage is what
// tells that answer from an object the repository does not hold — a removed
// pack, which reports nothing. A failed run carries its damage there too, and
// a refusal that is no damage (`not our ref`) carries none.
//
// Mutation: compute Result.Damage only for a failed run, and the first arm
// fails.
func TestAResultNamesTheDamageItsProcessRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	for _, tc := range []struct {
		name    string
		damage  func(t *testing.T, newer string)
		cmd     func(dir string) gitproc.Cmd
		stdout  string // what the answer says of the tip; "" for a failed run
		want    error  // Result.Damage
		wantErr error
	}{
		{"an index truncated, answered", func(t *testing.T, newer string) { truncate(t, newer+".idx", 100) },
			func(dir string) gitproc.Cmd { return batchCheck(dir, f.Tip()) }, f.Tip() + " missing\n", gitproc.ErrCorrupt, nil},
		{"a pack removed, answered", func(t *testing.T, newer string) { removePack(t, newer) },
			func(dir string) gitproc.Cmd { return batchCheck(dir, f.Tip()) }, f.Tip() + " missing\n", nil, nil},
		{"no damage, answered", func(*testing.T, string) {},
			func(dir string) gitproc.Cmd { return batchCheck(dir, f.Tip()) }, f.Tip() + " commit ", nil, nil},
		{"an index truncated, failed", func(t *testing.T, newer string) { truncate(t, newer+".idx", 100) },
			func(dir string) gitproc.Cmd { return read(dir, "rev-list", "--objects", f.Tip()) }, "",
			gitproc.ErrCorrupt, gitproc.ErrCorrupt},
		{"a want refused, failed", func(*testing.T, string) {}, func(dir string) gitproc.Cmd {
			return gitproc.Cmd{
				Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir},
				Stdin: strings.NewReader(pkt("want 0123456789012345678901234567890123456789\n") + "0000" + pkt("done\n")),
				Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
			}
		}, "", nil, gitproc.ErrNotOurRef},
		{"an absent ref, failed", func(*testing.T, string) {}, func(dir string) gitproc.Cmd {
			return read(dir, "rev-parse", "--verify", "refs/heads/absent")
		}, "", nil, gitproc.ErrFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, newer := f.bare(t)
			tc.damage(t, newer)
			res, err := p.Run(t.Context(), tc.cmd(dir))
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("%v (stderr %q), want an answer", err, res.Stderr)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("%v (stderr %q), want %v", err, res.Stderr, tc.wantErr)
			}
			if tc.stdout != "" && !strings.HasPrefix(string(res.Stdout), tc.stdout) {
				t.Errorf("answered %q, want %q", res.Stdout, tc.stdout)
			}
			if (res.Damage == nil) != (tc.want == nil) || !errors.Is(res.Damage, tc.want) {
				t.Errorf("Result.Damage = %v (stderr %q), want %v", res.Damage, res.Stderr, tc.want)
			}
		})
	}
}

// A FLIPPED PACK IS SERVED VERBATIM — documented, not a classification: pack
// reuse copies the damaged bytes and git reports nothing. The day git starts
// checking, this goes red and the cache's verify-on-read assumption is
// revisited.
func TestAFlippedPackIsServedVerbatim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	dir, newer := f.bare(t)
	overwrite(t, newer+".pack", size(t, newer+".pack")/2, bytes.Repeat([]byte{0xff}, 8))
	var sent countingWriter
	c := fetchV2(dir, f.Tip())
	c.Stdout = &sent
	res, err := p.Run(t.Context(), c)
	if err != nil || res.Exit != 0 || sent.n == 0 || res.Trace.Errors != 0 {
		t.Fatalf("a v2 fetch over a flipped pack: exit %d, %d bytes, %d error events, %v",
			res.Exit, sent.n, res.Trace.Errors, err)
	}
}

// AN EXIT ERROR IS EVERY KIND ITS EVENTS NAMED. A merge-base over an old id
// this company never held, in a copy whose commit-graph git reads around:
// its events name the graph's chunk (ErrCorrupt) and the id it could not
// find (ErrMissingObject). The Kind is the higher, and errors.Is reaches
// both — so a force check asking "was the old commit missing?" is told yes —
// and no kind the run did not name.
//
// Mutation: Is over Kind alone, and the ErrMissingObject arm is red.
func TestAnExitErrorIsEveryKindItsEventsNamed(t *testing.T) {
	t.Parallel()
	stream := versionLine(1, "2.47.3") + startLine(1, "git", "merge-base", "--is-ancestor") +
		errorLine(1, `terminating chunk id appears earlier than expected`,
			`terminating chunk id appears earlier than expected`) +
		errorLine(1, `Not a valid commit name %s`, "Not a valid commit name "+strings.Repeat("ab", 20)) +
		exitLine(1, 128)
	tr, kind, damage := gitproc.ReadTrace("/srv/r.git", []byte(stream), 0)
	err := error(&gitproc.ExitError{Command: "merge-base", Kind: kind,
		Result: gitproc.Result{Exit: 128, Damage: damage, Trace: tr}})
	if !errors.Is(kind, gitproc.ErrCorrupt) {
		t.Errorf("Kind = %v, want ErrCorrupt, the higher", kind)
	}
	for _, tc := range []struct {
		kind error
		want bool
	}{
		{gitproc.ErrCorrupt, true},
		{gitproc.ErrMissingObject, true},
		{gitproc.ErrNotOurRef, false},
		{gitproc.ErrFailed, false},
	} {
		if got := errors.Is(err, tc.kind); got != tc.want {
			t.Errorf("errors.Is(err, %v) = %v, want %v", tc.kind, got, tc.want)
		}
	}
	var dmg *gitproc.Damage
	if !errors.As(err, &dmg) || dmg.Parts != gitproc.PartChunk || !dmg.Derived() {
		t.Errorf("the run's damage is %v, want the derived chunk damage its first event named", damage)
	}
}

// A NARROWED CHUNK IS THE ONE FILE ITS PROCESS LOADED. A chunk-format report
// names neither derived file, so a repair of it drops both (Damage.Drops); a
// caller whose process loads only one of the two says so (Damage.Narrow),
// and the chunk is then that file's — every other part kept, the damage's
// kind, format and message unchanged, and the damage it was made from left
// as it was. A process that may load both, a narrowing that names neither,
// and a damage naming no chunk are answered as they are.
//
// Mutations: narrow to the index whatever the process loads (the graph's arm
// is red); drop the chunk without adding the file (the narrowed damage drops
// nothing).
func TestANarrowedChunkIsTheOneFileItsProcessLoaded(t *testing.T) {
	t.Parallel()
	midx, graph := gitproc.PartMultiPackIndex, gitproc.PartCommitGraph
	for _, tc := range []struct {
		name         string
		parts, loads gitproc.Part
		want         gitproc.Part
	}{
		{"a chunk, the index alone loaded", gitproc.PartChunk, midx, midx},
		{"a chunk, the graph alone loaded", gitproc.PartChunk, graph, graph},
		{"a chunk, both loaded", gitproc.PartChunk, midx | graph, gitproc.PartChunk},
		{"a chunk, neither named", gitproc.PartChunk, 0, gitproc.PartChunk},
		{"a chunk beside a pack", gitproc.PartChunk | gitproc.PartPack, midx, midx | gitproc.PartPack},
		{"no chunk", graph, midx, graph},
	} {
		d := &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: tc.parts, Format: "f", Message: "m"}
		n := d.Narrow(tc.loads)
		if n.Parts != tc.want || !errors.Is(n.Kind, gitproc.ErrCorrupt) || n.Format != "f" || n.Message != "m" {
			t.Errorf("%s: Narrow = %+v, want parts %v and the rest unchanged", tc.name, n, tc.want)
		}
		if d.Parts != tc.parts {
			t.Errorf("%s: Narrow changed the damage it was made from to %v", tc.name, d.Parts)
		}
		if tc.want&gitproc.PartChunk == 0 && tc.parts&gitproc.PartChunk != 0 && n.Drops() != tc.loads {
			t.Errorf("%s: the narrowed damage drops %v, want only %v", tc.name, n.Drops(), tc.loads)
		}
	}
	if (*gitproc.Damage)(nil).Narrow(midx) != nil {
		t.Error("a nil damage narrowed is not nil")
	}
}

// ONLY A PEEL LOADS THE COMMIT-GRAPH: the premise the cache's wants check
// narrows its chunk reports by (cache.Missing, Damage.Narrow). A cat-file
// asked about object ids alone reads them through the packs' indexes and the
// multi-pack-index and parses no commit, so a commit-graph whose table of
// contents is damaged (I5′) is never read — no event; the same cat-file
// asked to peel the id ("<id>^{}", as the cache peels its tags) parses the
// commit and reports the graph's chunk. A git that began loading the graph
// for an id alone would turn the first arm red, and the wants check's
// narrowing with it.
func TestOnlyAPeelLoadsTheCommitGraph(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	for _, tc := range []struct {
		name, input string
		chunk       bool
	}{
		{"an object id alone", f.Tip() + "\n", false},
		{"an object id peeled", f.Tip() + "^{}\n", true},
	} {
		d := &damaged{f: f, p: p}
		d.dir, d.older, d.newer = f.packed(t)
		writeGraph(t, d)
		overwrite(t, d.graph(), gittest.ChunkRow(gittest.GraphHeader, 0), []byte{0, 0, 0, 0})
		res, err := p.Run(t.Context(), gitproc.Cmd{
			Dir: d.dir, Args: []string{"cat-file", "--batch-check=%(objectname)", "--buffer"}, Config: serveConfig,
			Stdin: strings.NewReader(tc.input), Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
		})
		if err != nil || strings.TrimSpace(string(res.Stdout)) != f.Tip() {
			t.Fatalf("%s: cat-file over a damaged graph: %q, %v; want the id answered", tc.name, res.Stdout, err)
		}
		var dmg *gitproc.Damage
		named := errors.As(res.Damage, &dmg) && dmg.Parts == gitproc.PartChunk
		if named != tc.chunk {
			t.Errorf("%s: the run named %v (formats %q); want the graph's chunk named %v", tc.name, res.Damage,
				res.Trace.Formats, tc.chunk)
		}
	}
}
