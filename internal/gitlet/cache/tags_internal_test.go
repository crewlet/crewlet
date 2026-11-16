package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// testTag is a tag of the given types naming obj, peeling to peeled, dated
// where date is not negative.
func testTag(name string, obj, peeled [20]byte, typ, ptyp byte, date int64) tagInsert {
	f := tagFacts{peeled: peeled, typ: typ, ptyp: ptyp}
	if date >= 0 {
		f.dated, f.date = true, date
	}
	return tagInsert{obj: obj, facts: f, name: name}
}

// randomOID is twenty random bytes of r.
func randomOID(r *rand.Rand) [20]byte {
	var b [20]byte
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// randomTags is n tags of r's drawing: lightweight tags of commits, trees and
// blobs, annotated tags of each and of a tag, some undated, some at 0 and at
// int64's largest, some 245-byte names, and many tags of one commit.
func randomTags(r *rand.Rand, n int) []tagInsert {
	shared := randomOID(r)
	out := make([]tagInsert, 0, n)
	for i := range n {
		name := fmt.Sprintf("v%d.%d", i, r.IntN(1000))
		if r.IntN(10) == 0 {
			name = name + "-" + strings.Repeat("x", tagNameMax-len(name)-1)
		}
		date := int64(r.IntN(1000))
		switch r.IntN(10) {
		case 0:
			date = -1
		case 1:
			date = 0
		case 2:
			date = math.MaxInt64
		}
		obj, peeled := randomOID(r), randomOID(r)
		ptyp := byte(1 + r.IntN(3))
		if r.IntN(4) == 0 {
			peeled, ptyp = shared, typeCommit
		}
		if r.IntN(2) == 0 {
			out = append(out, testTag(name, obj, peeled, typeTag, ptyp, date))
		} else {
			out = append(out, testTag(name, peeled, peeled, ptyp, ptyp, date))
		}
	}
	return out
}

// tagFileOf is the whole build of tags, its bytes.
func tagFileOf(t testing.TB, tags []tagInsert) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := newTagWriter(&b)
	if err == nil {
		err = writeTagFile(slices.Clone(tags), w)
	}
	if err == nil {
		_, err = w.finish()
	}
	if err != nil {
		t.Fatalf("writing %d tags: %v", len(tags), err)
	}
	return b.Bytes()
}

// writeTags writes raw at a new path in dir, answering it.
func writeTags(t testing.TB, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tags")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// listed is tags in the listing's order, as entries.
func listed(tags []tagInsert) []TagEntry {
	sorted := slices.Clone(tags)
	slices.SortFunc(sorted, func(a, b tagInsert) int {
		return compareTagKeys(a.facts.date, a.name, b.facts.date, b.name)
	})
	out := make([]TagEntry, len(sorted))
	for i, s := range sorted {
		r := tagRecord{obj: s.obj, facts: s.facts, name: []byte(s.name)}
		out[i] = r.entry()
	}
	return out
}

// rowsOf is every row of x past after.
func rowsOf(t testing.TB, x *TagIndex, after *TagKey) []TagEntry {
	t.Helper()
	var out []TagEntry
	if err := x.Rows(after, func(e TagEntry) bool { out = append(out, e); return true }); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	return out
}

// countingReaderAt counts the reads of r.
type countingReaderAt struct {
	r     io.ReaderAt
	reads atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	return c.r.ReadAt(p, off)
}

// A TAG FILE READS BACK WHAT IT HOLDS from any key, and refuses one it did not write.
//
// Mutation: write the commit records unsorted → Commit misses, red.
func TestATagFileReadsBackWhatItHolds(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 1000} {
		t.Run(fmt.Sprintf("tags=%d", n), func(t *testing.T) {
			t.Parallel()
			// Each subtest draws from its own source: a rand.Rand is not
			// safe for concurrent use, and these subtests run in parallel.
			r := rand.New(rand.NewPCG(7, uint64(n)))
			tags := randomTags(r, n)
			raw := tagFileOf(t, tags)
			path := writeTags(t, raw)
			x, err := openTagIndex(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatalf("opening %d tags: %v", n, err)
			}
			want := listed(tags)
			if got := rowsOf(t, x, nil); !slices.Equal(got, want) {
				t.Fatalf("the rows of %d tags are not what they hold", n)
			}
			if x.Len() != n {
				t.Errorf("Len = %d, want %d", x.Len(), n)
			}
			for i := range min(n, 200) {
				after := &TagKey{Date: want[i].Date, Ref: want[i].Ref}
				if got := rowsOf(t, x, after); !slices.Equal(got, want[i+1:]) {
					t.Errorf("the rows past %s are %d, want the %d after it", want[i].Ref, len(got), n-i-1)
				}
				// A key no tag has, just past this one's name.
				between := &TagKey{Date: want[i].Date, Ref: want[i].Ref + "\x00"}
				if got := rowsOf(t, x, between); !slices.Equal(got, want[i+1:]) {
					t.Errorf("the rows past a key after %s are %d, want %d", want[i].Ref, len(got), n-i-1)
				}
			}
			if err := scanTagFile(path, tagScan{}); err != nil {
				t.Errorf("reading %d tags whole: %v", n, err)
			}
			// Every tagged commit's tags, newest first, from the index.
			byCommit := map[string][]string{}
			for _, e := range want {
				if e.PeeledType == "commit" {
					byCommit[e.Peeled] = append(byCommit[e.Peeled], e.Ref)
				}
			}
			for commit, refs := range byCommit {
				got, more, err := x.Commit(commit, 8)
				if err != nil {
					t.Fatalf("Commit(%s): %v", commit, err)
				}
				if take := min(8, len(refs)); !slices.Equal(got, refs[:take]) || more != len(refs)-take {
					t.Errorf("Commit(%s) = %v and %d more, want %v and %d", commit, got, more, refs[:take],
						len(refs)-take)
				}
			}
		})
	}

	t.Run("twelve tags of one commit", func(t *testing.T) {
		t.Parallel()
		r := rand.New(rand.NewPCG(7, 12))
		commit := randomOID(r)
		var tags []tagInsert
		for i := range 12 {
			tags = append(tags, testTag(fmt.Sprintf("t%02d", i), commit, commit, typeCommit, typeCommit, int64(i)))
		}
		raw := tagFileOf(t, tags)
		x, err := openTagIndex(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		got, more, err := x.Commit(hex.EncodeToString(commit[:]), 8)
		want := []string{"refs/tags/t11", "refs/tags/t10", "refs/tags/t09", "refs/tags/t08", "refs/tags/t07",
			"refs/tags/t06", "refs/tags/t05", "refs/tags/t04"}
		if err != nil || !slices.Equal(got, want) || more != 4 {
			t.Errorf("Commit = %v and %d more (%v), want %v and 4", got, more, err, want)
		}
	})

	t.Run("not ours", func(t *testing.T) {
		t.Parallel()
		r := rand.New(rand.NewPCG(7, 13))
		commit := randomOID(r)
		tags := []tagInsert{
			testTag("a", commit, commit, typeCommit, typeCommit, 5),
			testTag("b", randomOID(r), commit, typeTag, typeCommit, 3),
		}
		good := tagFileOf(t, tags)
		c, err := readTagCounts(bytes.NewReader(good), int64(len(good)))
		if err != nil {
			t.Fatal(err)
		}
		offsets := tagsHeadBytes + c.records
		// resealed is raw with its trailer made its bytes' again, so only
		// the defect it was given stands.
		resealed := func(raw []byte) []byte {
			sum := sha256.Sum256(raw[:len(raw)-sha256.Size])
			copy(raw[len(raw)-sha256.Size:], sum[:])
			return raw
		}
		edit := func(f func(raw []byte) []byte) []byte { return f(slices.Clone(good)) }
		for _, bad := range []struct {
			name string
			raw  []byte
		}{
			{"cut short", good[:len(good)-1]},
			{"a byte appended", append(slices.Clone(good), 0)},
			{"a count past its size", edit(func(b []byte) []byte {
				binary.BigEndian.PutUint32(b[len(b)-tagsTailBytes:], uint32(c.n+1))
				return resealed(b)
			})},
			{"an offset outside the records", edit(func(b []byte) []byte {
				binary.BigEndian.PutUint32(b[offsets:], uint32(offsets+1))
				return resealed(b)
			})},
			{"a name that is no ref", edit(func(b []byte) []byte {
				b[tagsHeadBytes+tagFixedBytes] = '~'
				return resealed(b)
			})},
			{"a type nibble of 7", edit(func(b []byte) []byte {
				b[tagsHeadBytes+40] = 7<<4 | 1
				return resealed(b)
			})},
			{"a magic of CTAX", edit(func(b []byte) []byte { copy(b, "CTAX"); return resealed(b) })},
			{"a format of 2", edit(func(b []byte) []byte {
				binary.BigEndian.PutUint32(b[4:], 2)
				return resealed(b)
			})},
		} {
			path := writeTags(t, bad.raw)
			_, err := TagRows(path)
			if !errors.Is(err, errTagsMalformed) {
				t.Errorf("%s: reading it whole = %v, want errTagsMalformed", bad.name, err)
			}
			if err := servedTags(bad.raw, hex.EncodeToString(commit[:])); !errors.Is(err, errTagsMalformed) {
				t.Errorf("%s: serving it = %v, want errTagsMalformed", bad.name, err)
			}
		}
	})
}

// servedTags is the first error opening raw, listing it and naming commit's tags
// meets.
func servedTags(raw []byte, commit string) error {
	x, err := openTagIndex(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return err
	}
	if err = x.Rows(nil, func(TagEntry) bool { return true }); err != nil {
		return err
	}
	_, _, err = x.Commit(commit, 8)
	return err
}

// A COMMIT'S TAGS ARE FOUND IN BOUNDED READS however many tags it has.
//
// Mutation: count more by walking the run → red.
func TestACommitsTagsAreFoundInBoundedReads(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	commit := randomOID(r)
	var tags []tagInsert
	for i := range 20_000 {
		c := randomOID(r)
		if i%2 == 0 {
			c = commit
		}
		tags = append(tags, testTag(fmt.Sprintf("t%05d", i), c, c, typeCommit, typeCommit, int64(i)))
	}
	raw := tagFileOf(t, tags)
	counting := &countingReaderAt{r: bytes.NewReader(raw)}
	x, err := openTagIndex(counting, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	counting.reads.Store(0)
	refs, more, err := x.Commit(hex.EncodeToString(commit[:]), 8)
	if err != nil || len(refs) != 8 || more != 9_992 || refs[0] != "refs/tags/t19998" {
		t.Fatalf("Commit = %v and %d more (%v), want the 8 newest and 9992", refs, more, err)
	}
	bound := 2*bits.Len(uint(x.m)) + 16
	if got := counting.reads.Load(); got > int64(bound) {
		t.Errorf("Commit read %d times, past the %d two searches of %d records and 8 tags take", got, bound, x.m)
	}
}

// A TAG MOVE ALLOCATES FOR WHAT MOVED, and merges to a whole build's bytes.
//
// Mutation: decode the old file into entries → allocations grow, red.
func TestATagMoveAllocatesForWhatMoved(t *testing.T) { //nolint:paralleltest // AllocsPerRun refuses a parallel test
	r := rand.New(rand.NewPCG(3, 4))
	allocs := map[int]float64{}
	for _, n := range []int{1_000, 10_000} {
		tags := randomTags(r, n)
		path := writeTags(t, tagFileOf(t, tags))
		// The move: tags[0] to another commit, dated newest.
		c := randomOID(r)
		moved := testTag(tags[0].name, c, c, typeCommit, typeCommit, 5_000)
		moves := []tagMove{{name: moved.name, oid: hex.EncodeToString(c[:])}}
		var merged bytes.Buffer
		merge := func(out io.Writer) {
			w, err := newTagWriter(out)
			if err == nil {
				err = mergeTagFile(path, moves, []tagInsert{moved}, w)
			}
			if err == nil {
				_, err = w.finish()
			}
			if err != nil {
				t.Fatalf("merging one move into %d tags: %v", n, err)
			}
		}
		merge(&merged)
		after := slices.Concat([]tagInsert{moved}, tags[1:])
		if !bytes.Equal(merged.Bytes(), tagFileOf(t, after)) {
			t.Errorf("the merge of one move into %d tags is not the whole build's bytes", n)
		}
		allocs[n] = testing.AllocsPerRun(5, func() { merge(io.Discard) })
	}
	if a, b := allocs[1_000], allocs[10_000]; b > a+8 {
		t.Errorf("a move into 10 000 tags allocates %v times and into 1 000 %v: the merge allocates for the file", b, a)
	}
}

// A TAG STEP'S FAILURE IS THE ROUND'S ONLY WHERE THE ROUND ANSWERS IT.
//
// Mutation: answer false for a missing object → red.
func TestATagStepsFailureIsTheRoundsOnlyWhereTheRoundAnswersIt(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	hinted := &gitproc.ExitError{Command: "cat-file", Kind: errors.New("exit 128")}
	hinted.Result.Trace.Hints = []gitproc.Hint{{Pack: "pack-x"}}
	for _, c := range []struct {
		name  string
		ctx   context.Context
		err   error
		round bool
	}{
		{"corrupt", t.Context(), &gitproc.Damage{Kind: gitproc.ErrCorrupt, Message: "bad"}, true},
		{"missing", t.Context(), fmt.Errorf("x: %w", gitproc.ErrMissingObject), true},
		{"shape", t.Context(), gitproc.ErrRepoShape, true},
		{"a hint", t.Context(), hinted, true},
		{"not ours", t.Context(), fmt.Errorf("x: %w", errRefsNotOurs), true},
		{"the round's end", ended, errors.New("killed"), true},
		{"a parse", t.Context(), errors.New("cache: log answered \"x\""), false},
		{"another exit", t.Context(), &gitproc.ExitError{Command: "log", Kind: errors.New("exit 1")}, false},
		{"the knob", t.Context(), errors.New("the knob"), false},
	} {
		if got := tagStepEnds(c.ctx, c.err); got != c.round {
			t.Errorf("%s: the round's = %t, want %t", c.name, got, c.round)
		}
	}
}
