package gitproc_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// fixture is one history, packed the way a cache holds it: commits 1–3 in an
// OLDER pack and commit 4 — adding a large blob — in a NEWER one, main at 4.
// Every case damages its own copy.
type fixture struct {
	client  *gittest.Client
	wt      string
	commits [4]string
}

// bigFile is 256 KiB of base64 over a seeded stream, so the bytes, the blob
// and the pack checksums are the same on every run.
func bigFile() string {
	r := rand.New(rand.NewPCG(1, 2))
	raw := make([]byte, 192<<10)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{client: gittest.NewClient(t)}
	f.wt = f.client.Init(t)
	f.commits[0] = f.client.Commit(t, f.wt, map[string]string{"a.txt": "one\n"}, "one")
	f.commits[1] = f.client.Commit(t, f.wt, map[string]string{"a.txt": "two\n"}, "two")
	f.commits[2] = f.client.Commit(t, f.wt, map[string]string{"b.txt": "three\n"}, "three")
	f.commits[3] = f.client.Commit(t, f.wt, map[string]string{"big.txt": bigFile()}, "four")
	return f
}

func (f *fixture) tip() string { return f.commits[3] }

// bare is a fresh copy: the repository, and the newer pack's path without
// its extension.
func (f *fixture) bare(t *testing.T) (dir, newer string) {
	t.Helper()
	dir = gittest.Empty(t)
	gittest.Pack(t, f.client, dir, f.wt, f.commits[2])
	cs := gittest.Pack(t, f.client, dir, f.wt, f.tip(), "^"+f.commits[2])
	gittest.SetRefs(t, dir, map[string]string{"refs/heads/main": f.tip()})
	return dir, filepath.Join(dir, "objects", "pack", "pack-"+cs)
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
		Stdin: strings.NewReader(pkt("command=fetch\n") + "0001" + pkt("thin-pack\n") + pkt("want "+want+"\n") + pkt("done\n") + "0000"),
		Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
	}
}

func read(dir string, args ...string) gitproc.Cmd {
	return gitproc.Cmd{Dir: dir, Args: args, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}
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

// FAILURES ARE CLASSIFIED FROM WHAT REAL GIT SAYS about real damage — every
// row a repository damaged the way a cache can be, and the command that meets
// it. A git that rewords a message fails here rather than turning a damaged
// cache into a 500.
func TestClassifiesWhatRealGitSays(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	type row struct {
		name string
		cmd  func(t *testing.T, dir, newer string) gitproc.Cmd
		want error
	}
	for _, damage := range []struct {
		name   string
		damage func(t *testing.T, dir, newer string)
		rows   []row
	}{
		{"the newer pack removed", func(t *testing.T, _, newer string) {
			for _, ext := range []string{".pack", ".idx", ".rev"} {
				if err := os.Remove(newer + ext); err != nil {
					t.Fatal(err)
				}
			}
		}, []row{
			{"rev-list", func(_ *testing.T, dir, _ string) gitproc.Cmd { return read(dir, "rev-list", "--objects", f.tip()) }, gitproc.ErrMissingObject},
			{"cat-file", func(_ *testing.T, dir, _ string) gitproc.Cmd { return read(dir, "cat-file", "-t", f.tip()) }, gitproc.ErrMissingObject},
			{"merge-base", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				return read(dir, "merge-base", "--is-ancestor", f.commits[2], f.tip())
			}, gitproc.ErrMissingObject},
			{"fsck", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				return maintenance(dir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
			}, gitproc.ErrMissingObject},
			{"upload-pack v2", func(_ *testing.T, dir, _ string) gitproc.Cmd { return fetchV2(dir, f.tip()) }, gitproc.ErrNotOurRef},
		}},
		{"the newer index truncated", func(t *testing.T, _, newer string) { truncate(t, newer+".idx", 100) }, []row{
			{"rev-list", func(_ *testing.T, dir, _ string) gitproc.Cmd { return read(dir, "rev-list", "--objects", f.tip()) }, gitproc.ErrCorrupt},
			// It also prints "not our ref" for the tip it can no longer
			// find: damage outranks it.
			{"upload-pack v2", func(_ *testing.T, dir, _ string) gitproc.Cmd { return fetchV2(dir, f.tip()) }, gitproc.ErrCorrupt},
		}},
		{"eight bytes flipped in the newer pack", func(t *testing.T, _, newer string) {
			overwrite(t, newer+".pack", size(t, newer+".pack")/2, bytes.Repeat([]byte{0xff}, 8))
		}, []row{
			{"cat-file", func(_ *testing.T, dir, _ string) gitproc.Cmd { return read(dir, "cat-file", "-p", f.tip()+":big.txt") }, gitproc.ErrCorrupt},
			{"fsck", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				return maintenance(dir, "fsck", "--no-dangling", "--no-progress")
			}, gitproc.ErrCorrupt},
			{"index-pack", func(t *testing.T, _, newer string) gitproc.Cmd { return indexPack(t, newer+".pack") }, gitproc.ErrCorrupt},
		}},
		{"the newer pack truncated to a third", func(t *testing.T, _, newer string) {
			truncate(t, newer+".pack", size(t, newer+".pack")/3)
		}, []row{
			{"rev-list", func(_ *testing.T, dir, _ string) gitproc.Cmd { return read(dir, "rev-list", "--objects", f.tip()) }, gitproc.ErrCorrupt},
			{"index-pack", func(t *testing.T, _, newer string) gitproc.Cmd { return indexPack(t, newer+".pack") }, gitproc.ErrCorrupt},
		}},
		// THE BITMAP'S POSITIONS, which the reverse index maps: the
		// maintainer's whole multi-pack-index with its bitmap, as a clone
		// reads it, and its reverse index (the RIDX chunk) overwritten —
		// every position it holds past the index's objects. The bitmap
		// itself is left whole: overwritten too, git 2.56 stops at the
		// bitmap first and says only "Out of memory, realloc failed".
		{"a multi-pack-index whose reverse index is overwritten beside its bitmap", func(t *testing.T, dir, _ string) {
			if v := p.Version(); !v.AtLeast(gittest.NamesMidxPosition) {
				t.Skipf("git %s does not check a multi-pack-index's object positions (from %s): a bitmapped "+
					"walk over this damage dies of a segmentation fault, saying nothing a table can read",
					v, gittest.NamesMidxPosition)
			}
			c := maintenance(dir, "multi-pack-index", "write", "--bitmap", "--no-progress")
			c.Config = []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"},
				{Key: "pack.writeBitmapLookupTable", Value: "true"}}
			if _, err := p.Run(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			midx := filepath.Join(dir, "objects", "pack", "multi-pack-index")
			start, end := gittest.MidxChunk(t, midx, "RIDX")
			overwrite(t, midx, start, bytes.Repeat([]byte{0xff}, int(end-start)))
		}, []row{
			{"rev-list over its bitmap", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				c := read(dir, "rev-list", "--objects", "--use-bitmap-index", "--count", "--all")
				c.Config = []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}}
				return c
			}, gitproc.ErrCorrupt},
		}},
		{"a multi-pack-index with its signature overwritten", func(t *testing.T, dir, _ string) {
			c := maintenance(dir, "multi-pack-index", "write", "--no-progress")
			c.Config = []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}}
			if _, err := p.Run(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			overwrite(t, filepath.Join(dir, "objects", "pack", "multi-pack-index"), 0, []byte{0xde, 0xad, 0xbe, 0xef})
		}, []row{
			{"rev-list", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				c := read(dir, "rev-list", "--objects", f.tip())
				c.Config = []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}}
				return c
			}, gitproc.ErrCorrupt},
		}},
		{"no damage", func(*testing.T, string, string) {}, []row{
			{"upload-pack v0 of an absent want", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				return gitproc.Cmd{
					Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir},
					Stdin: strings.NewReader(pkt("want 0123456789012345678901234567890123456789\n") + "0000" + pkt("done\n")),
					Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
				}
			}, gitproc.ErrNotOurRef},
			{"rev-parse of an absent ref", func(_ *testing.T, dir, _ string) gitproc.Cmd {
				return read(dir, "rev-parse", "--verify", "refs/heads/absent")
			}, gitproc.ErrFailed},
		}},
	} {
		for _, r := range damage.rows {
			t.Run(damage.name+"/"+r.name, func(t *testing.T) {
				dir, newer := f.bare(t)
				damage.damage(t, dir, newer)
				c := r.cmd(t, dir, newer)
				res, err := p.Run(t.Context(), c)
				var exit *gitproc.ExitError
				if !errors.As(err, &exit) || !errors.Is(err, r.want) {
					t.Fatalf("%v (stderr %q), want an ExitError of kind %v", err, res.Stderr, r.want)
				}
				if exit.Command != c.Args[0] {
					t.Errorf("ExitError.Command = %q, want %q", exit.Command, c.Args[0])
				}
				if last := lastStderrLine(res.Stderr); last == "" || !strings.Contains(err.Error(), last) {
					t.Errorf("the error %q does not carry git's last line %q", err, last)
				}
			})
		}
	}
}

// AN ANSWER READ OVER DAMAGE SAYS SO, whatever the exit status: cat-file
// --batch-check calls the tip of a pack whose index is truncated `missing`
// and exits 0, only its stderr naming the index, so Result.Damage is what
// tells that answer from an object the repository does not hold — a removed
// pack, which prints nothing. A failed run carries its kind there too, and a
// refusal that is no damage (`not our ref`) carries none.
func TestAResultNamesTheDamageItsProcessRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	batchCheck := func(dir string) gitproc.Cmd {
		c := read(dir, "cat-file", "--batch-check")
		c.Stdin = strings.NewReader(f.tip() + "\n")
		return c
	}
	for _, tc := range []struct {
		name    string
		damage  func(t *testing.T, newer string)
		cmd     func(dir string) gitproc.Cmd
		stdout  string // what the answer says of the tip; "" for a failed run
		want    error  // Result.Damage
		wantErr error
	}{
		{"an index truncated, answered", func(t *testing.T, newer string) { truncate(t, newer+".idx", 100) },
			batchCheck, f.tip() + " missing\n", gitproc.ErrCorrupt, nil},
		{"a pack removed, answered", func(t *testing.T, newer string) {
			for _, ext := range []string{".pack", ".idx", ".rev"} {
				if err := os.Remove(newer + ext); err != nil {
					t.Fatal(err)
				}
			}
		}, batchCheck, f.tip() + " missing\n", nil, nil},
		{"no damage, answered", func(*testing.T, string) {}, batchCheck, f.tip() + " commit ", nil, nil},
		{"an index truncated, failed", func(t *testing.T, newer string) { truncate(t, newer+".idx", 100) },
			func(dir string) gitproc.Cmd { return read(dir, "rev-list", "--objects", f.tip()) }, "",
			gitproc.ErrCorrupt, gitproc.ErrCorrupt},
		{"a want refused, failed", func(*testing.T, string) {}, func(dir string) gitproc.Cmd {
			return gitproc.Cmd{
				Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir},
				Stdin: strings.NewReader(pkt("want 0123456789012345678901234567890123456789\n") + "0000" + pkt("done\n")),
				Class: gitproc.ClassTransfer, Deadline: gitproc.ReadDeadline,
			}
		}, "", nil, gitproc.ErrNotOurRef},
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

func lastStderrLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// A FLIPPED PACK IS SERVED VERBATIM — documented, not a classification: pack
// reuse copies the damaged bytes and no stderr line says so. The day git
// starts checking, this goes red and the cache's verify-on-read assumption is
// revisited.
func TestAFlippedPackIsServedVerbatim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{})
	dir, newer := f.bare(t)
	overwrite(t, newer+".pack", size(t, newer+".pack")/2, bytes.Repeat([]byte{0xff}, 8))
	var sent countingWriter
	c := fetchV2(dir, f.tip())
	c.Stdout = &sent
	res, err := p.Run(t.Context(), c)
	if err != nil || res.Exit != 0 || sent.n == 0 {
		t.Fatalf("a v2 fetch over a flipped pack: exit %d, %d bytes, %v", res.Exit, sent.n, err)
	}
}

// oid is any 40-hex id, for the example lines.
const oid = "0123456789abcdef0123456789abcdef01234567"

// examples is every pattern of the table with the line it was taken from:
// git 2.43's own messages, the ones TestClassifiesWhatRealGitSays provokes
// among them.
var examples = []struct {
	kind    error
	pattern string
	line    string
}{
	{gitproc.ErrCorrupt, `is corrupt`, "fatal: packed object " + oid + " (stored in /c/objects/pack/pack-1.pack) is corrupt"},
	{gitproc.ErrCorrupt, `bad packed object`, "error: bad packed object CRC for " + oid},
	{gitproc.ErrCorrupt, `does not match index`, "error: packfile /c/objects/pack/pack-1.pack does not match index"},
	{gitproc.ErrCorrupt, `index file .* is too small`, "error: index file /c/objects/pack/pack-1.idx is too small"},
	{gitproc.ErrCorrupt, `unable to read index`, "fatal: unable to read index"},
	{gitproc.ErrCorrupt, `inflate`, "fatal: serious inflate inconsistency"},
	{gitproc.ErrCorrupt, `pack has bad object`, "fatal: pack has bad object at offset 285: inflate returned -5"},
	{gitproc.ErrCorrupt, `pack has [0-9]+ unresolved delta`, "fatal: pack has 3 unresolved deltas"},
	{gitproc.ErrCorrupt, `early EOF`, "fatal: early EOF"},
	{gitproc.ErrCorrupt, `pack checksum mismatch`, "error: /c/objects/pack/pack-1.pack pack checksum mismatch"},
	{gitproc.ErrCorrupt, `CRC mismatch`, "error: index CRC mismatch for object " + oid + " from /c/objects/pack/pack-1.pack at offset 285"},
	{gitproc.ErrCorrupt, `cannot unpack`, "error: cannot unpack " + oid + " from /c/objects/pack/pack-1.pack at offset 285"},
	{gitproc.ErrCorrupt, `^(error|fatal): .*multi-pack-index`, "fatal: multi-pack-index signature 0xdeadbeef does not match signature 0x4d494458"},
	// git 2.47's and later's — one line, each half a pattern.
	{gitproc.ErrCorrupt, `MIDX is likely corrupt`, "fatal: invalid MIDX object position, MIDX is likely corrupt"},
	{gitproc.ErrCorrupt, `invalid MIDX object position`, "fatal: invalid MIDX object position, MIDX is likely corrupt"},
	{gitproc.ErrCorrupt, `^(error|fatal): .*commit-graph`, "error: commit-graph signature 0 does not match signature 43475048"},
	{gitproc.ErrMissingObject, `bad object [0-9a-f]{40}`, "fatal: bad object " + oid},
	{gitproc.ErrMissingObject, `missing (blob|tree|commit|tag) [0-9a-f]{40}`, "missing blob " + oid},
	{gitproc.ErrMissingObject, `invalid sha1 pointer [0-9a-f]{40}`, "error: refs/heads/main: invalid sha1 pointer " + oid},
	{gitproc.ErrMissingObject, `broken link from`, "broken link from    tree " + oid},
	{gitproc.ErrMissingObject, `(unable to|Could not) read [0-9a-f]{40}`, "error: Could not read " + oid},
	{gitproc.ErrMissingObject, `Not a valid (commit|object) name [0-9a-f]{40}`, "fatal: Not a valid commit name " + oid},
	{gitproc.ErrMissingObject, `could not get object info`, "fatal: git cat-file: could not get object info"},
	{gitproc.ErrMissingObject, `Failed to traverse parents of commit [0-9a-f]{40}`, "fatal: Failed to traverse parents of commit " + oid},
	{gitproc.ErrMissingObject, `did not receive expected object [0-9a-f]{40}`, "fatal: did not receive expected object " + oid},
	{gitproc.ErrNotOurRef, `not our ref [0-9a-f]{40}`, "fatal: git upload-pack: not our ref " + oid},
}

// THE TABLE MATCHES ITS OWN LINES: every pattern against the line it was taken
// from, read through the same compiled table a run classifies with — so a
// regexp edit that stops matching its example is red before any repository is
// damaged — and both lists name the same patterns.
func TestTheTableMatchesItsLines(t *testing.T) {
	t.Parallel()
	table := gitproc.ClassifyTable()
	var listed []string
	for kind, patterns := range table {
		for _, pattern := range patterns {
			listed = append(listed, kind.Error()+" "+pattern)
		}
	}
	var exampled []string
	for _, e := range examples {
		exampled = append(exampled, e.kind.Error()+" "+e.pattern)
		if !regexp.MustCompile(e.pattern).MatchString(e.line) {
			t.Errorf("%s does not match its own line %q", e.pattern, e.line)
		}
		if got := gitproc.ClassifyLine(e.line); !errors.Is(got, e.kind) {
			t.Errorf("%q classifies as %v, want %v", e.line, got, e.kind)
		}
		var w gitproc.Words
		if w.Add([]byte(e.line)); !errors.Is(w.Kind(), e.kind) || w.Named() != e.line {
			t.Errorf("%q read as git's words elsewhere is %v named by %q, want %v named by it",
				e.line, w.Kind(), w.Named(), e.kind)
		}
		if strings.Contains(e.pattern, "[0-9a-f]") && !regexp.MustCompile(`\[0-9a-f\]\{40\}`).MatchString(e.pattern) {
			t.Errorf("%s matches an object id looser than its 40 hex digits", e.pattern)
		}
	}
	slices.Sort(listed)
	slices.Sort(exampled)
	if !slices.Equal(listed, exampled) {
		t.Fatalf("the table and its examples name different patterns:\n table   %q\n example %q", listed, exampled)
	}
	if got := gitproc.ClassifyLine("fatal: something else entirely"); !errors.Is(got, gitproc.ErrFailed) {
		t.Errorf("an unknown line classifies as %v, want ErrFailed", got)
	}
}

// A LINE NAMING THE MULTI-PACK-INDEX IS TOLD APART from other corruption:
// every such line is [gitproc.ErrCorrupt] and names the index, while a pack's
// or an object's damage is corrupt but names no index — which is what lets the
// cache repair a damaged index IN PLACE (drop and rewrite it) rather than read
// the whole repository again. The predicate is bounded to the first MaxLine
// bytes as the classifier is.
func TestNamesMultiPackIndex(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"fatal: invalid MIDX object position, MIDX is likely corrupt",
		"fatal: multi-pack-index signature 0xdeadbeef does not match signature 0x4d494458",
		"error: multi-pack-index file /c/objects/pack/multi-pack-index is too small",
	} {
		if !gitproc.NamesMultiPackIndex(line) {
			t.Errorf("%q names the multi-pack-index, read as not", line)
		}
		if got := gitproc.ClassifyLine(line); !errors.Is(got, gitproc.ErrCorrupt) {
			t.Errorf("%q classifies as %v, want ErrCorrupt", line, got)
		}
	}
	for _, line := range []string{
		"fatal: packed object " + oid + " (stored in /c/objects/pack/pack-1.pack) is corrupt",
		"fatal: early EOF",
		"error: commit-graph signature 0 does not match signature 43475048",
		"fatal: bad object " + oid,
	} {
		if gitproc.NamesMultiPackIndex(line) {
			t.Errorf("%q does not name the multi-pack-index, read as naming it", line)
		}
	}
	long := strings.Repeat("x", gitproc.MaxLine)
	if gitproc.NamesMultiPackIndex(long + "MIDX is likely corrupt") {
		t.Error("damage past the first MaxLine bytes names the index, want bounded out")
	}
}

// GIT'S WORDS READ ELSEWHERE THAN ITS STDERR are classified as its stderr is —
// the highest kind any line names winning, damage outranking the want it
// explains — and remember the first line that named the winner, which is what
// a log quotes: upload-pack relays its pack-objects' words in its answer's
// side band, and a fetch's damage is named nowhere else.
func TestWordsKeepTheHighestKindAndTheLineThatNamedIt(t *testing.T) {
	t.Parallel()
	const (
		progress = "Enumerating objects: 12, done."
		midx     = "fatal: invalid MIDX object position, MIDX is likely corrupt"
		short    = "error: index file /c/objects/pack/pack-1.idx is too small"
		missing  = "fatal: bad object " + oid
		notOurs  = "fatal: git upload-pack: not our ref " + oid
	)
	past := strings.Repeat("x", gitproc.MaxLine)
	for _, tc := range []struct {
		name   string
		lines  []string
		kind   error
		damage bool
		named  string
	}{
		{"nothing read", nil, gitproc.ErrFailed, false, ""},
		{"progress alone", []string{progress, "Counting objects: 33% (1/3)"}, gitproc.ErrFailed, false, ""},
		{"a refused want", []string{progress, notOurs}, gitproc.ErrNotOurRef, false, notOurs},
		{"damage after the want it explains", []string{notOurs, short}, gitproc.ErrCorrupt, true, short},
		{"damage outranks a missing object", []string{missing, midx, short}, gitproc.ErrCorrupt, true, midx},
		{"a missing object", []string{progress, missing, missing + " again"}, gitproc.ErrMissingObject, true, missing},
		{"damage in a line's first MaxLine bytes", []string{midx + past}, gitproc.ErrCorrupt, true,
			(midx + past)[:gitproc.MaxLine]},
		{"damage past a line's first MaxLine bytes", []string{past + midx}, gitproc.ErrFailed, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w gitproc.Words
			for _, l := range tc.lines {
				w.Add([]byte(l))
			}
			if !errors.Is(w.Kind(), tc.kind) {
				t.Errorf("Kind = %v, want %v", w.Kind(), tc.kind)
			}
			if got := w.Damage(); (got != nil) != tc.damage || (tc.damage && !errors.Is(got, tc.kind)) {
				t.Errorf("Damage = %v, want damage %v", got, tc.damage)
			}
			if w.Named() != tc.named {
				t.Errorf("Named = %.80q, want %.80q", w.Named(), tc.named)
			}
		})
	}
}
