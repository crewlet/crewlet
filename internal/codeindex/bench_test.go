package codeindex_test

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// benchFiles is the tip BenchmarkCodeIndex indexes: 120k files, a large
// monorepo's (M6's sizing).
const benchFiles = 120_000

// benchQueries is how many searches the benchmark times for its p50 and
// p99: two hundred, so the p99 is the second slowest.
const benchQueries = 200

// corpusFile is one file of this repository's own tree.
type corpusFile struct {
	path string
	data []byte
}

// benchCorpus is every file of this repository's tree the index would read
// (codeindex.Skip admits it): real source — Go, TypeScript, Markdown, YAML,
// the committed dashboard bundle — rather than text a generator guesses the
// trigrams of. build/ is the suite's own scratch, ignored by git.
func benchCorpus(b *testing.B) []corpusFile {
	root := sourcetree.Root(b)
	var files []corpusFile
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if codeindex.Skip(int64(len(data)), data[:min(len(data), codeindex.BinaryProbeBytes)]) {
			return nil
		}
		files = append(files, corpusFile{path: filepath.ToSlash(rel), data: data})
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if len(files) == 0 {
		b.Fatal("the repository's tree holds no file the index reads")
	}
	return files
}

// blob is the benchmark tip's file i: the corpus's file i mod its length,
// its letters rotated by the copy it is (i over the corpus's length) and its
// digits by the copy over 26 — so every file of the tip is a distinct blob
// whose trigrams are the real file's in number and shape, and each copy
// brings a vocabulary of its own, as a monorepo's teams do, rather than
// postings that repeat one tree's.
func blob(corpus []corpusFile, i int, dst []byte) []byte {
	f := corpus[i%len(corpus)]
	c := i / len(corpus)
	letters, digits := byte(c%26), byte(c/26%10)
	dst = append(dst[:0], f.data...)
	if c == 0 {
		return dst
	}
	for j, ch := range dst {
		switch {
		case 'a' <= ch && ch <= 'z':
			dst[j] = 'a' + (ch-'a'+letters)%26
		case 'A' <= ch && ch <= 'Z':
			dst[j] = 'A' + (ch-'A'+letters)%26
		case '0' <= ch && ch <= '9':
			dst[j] = '0' + (ch-'0'+digits)%10
		}
	}
	return dst
}

// blobID is git's id of a blob holding data.
func blobID(data []byte) codeindex.OID {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var o codeindex.OID
	copy(o[:], h.Sum(nil))
	return o
}

// heap is the bytes the heap holds once collected.
func heap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// BenchmarkCodeIndex measures code search's index at the size it is sized
// for — a tip of 120k files, built from this repository's own tree (blob) —
// and reports what re-derives its provisional figures (M3-O6):
//
//   - build-s/GB: the build's seconds a gigabyte of source, segments written
//     as production writes them (a segment each codeindex.SegmentSourceBytes,
//     merged as codeindex.MergeNewest says after each) into memory, as the
//     cache's place does before it stages the file. The blobs are made in
//     memory where the cache reads them through one cat-file;
//   - index-B/src-B: the index's bytes — its segments and its tip map — a
//     byte of the source it indexes;
//   - seg-peak-MiB: the heap one segment's build holds at its peak, the
//     builder's postings and table with the written segment beside them —
//     what codeindex.SegmentSourceBytes is sized by;
//   - zlib-B/src-B: the tree's own files deflated as a pack deflates an
//     object, a byte of source — the least a copy holding the tip takes,
//     against which CodeIndexShare weighs the index;
//   - query-p50-ms and query-p99-ms: a search's lookups over every segment
//     and its confirmation of the candidates' lines, up to code.MaxSearchHits,
//     for benchQueries literals of the tree's own identifiers, one in four
//     folding case. The cat-file that reads a candidate in production is not
//     timed: a blob is made in memory here.
//
// It is one bounded run: -run '^$' -bench '^BenchmarkCodeIndex$' -benchtime
// 1x -count 1. Its figures are on the machine that ran it, which the commit
// that set the figures names.
func BenchmarkCodeIndex(b *testing.B) {
	corpus := benchCorpus(b)
	ids := make([]codeindex.OID, benchFiles)
	var buf []byte
	for i := range benchFiles {
		buf = blob(corpus, i, buf)
		ids[i] = blobID(buf)
	}
	order := make([]int, benchFiles)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(x, y int) int { return bytes.Compare(ids[x][:], ids[y][:]) })
	// Files of one content are one blob, as in git: an empty file, or one
	// with no letter or digit to rotate, is the same in every copy.
	order = slices.CompactFunc(order, func(x, y int) bool { return ids[x] == ids[y] })
	byID := make(map[codeindex.OID]int, benchFiles)
	for i, id := range ids {
		byID[id] = i
	}

	var (
		segs    []*codeindex.Segment
		sizes   []int64
		source  int64
		elapsed time.Duration
	)
	for b.Loop() {
		segs, sizes, source = nil, nil, 0
		start := time.Now()
		builder := &codeindex.Builder{}
		write := func() {
			if builder.Len() == 0 {
				return
			}
			var out bytes.Buffer
			if err := codeindex.WriteSegment(&out, builder); err != nil {
				b.Fatal(err)
			}
			source += builder.SourceBytes()
			builder = &codeindex.Builder{}
			segs, sizes = append(segs, open(b, out.Bytes())), append(sizes, int64(out.Len()))
			for {
				if !codeindex.MergeNewest(sizes) {
					return
				}
				n := len(segs)
				var merged bytes.Buffer
				if err := codeindex.MergeSegments(&merged, segs[n-2:], nil); err != nil {
					b.Fatal(err)
				}
				segs = append(segs[:n-2], open(b, merged.Bytes()))
				sizes = append(sizes[:n-2], int64(merged.Len()))
			}
		}
		for _, i := range order {
			buf = blob(corpus, i, buf)
			if err := builder.Add(ids[i], buf); err != nil {
				b.Fatal(err)
			}
			if builder.SourceBytes() >= codeindex.SegmentSourceBytes {
				write()
			}
		}
		write()
		elapsed = time.Since(start)
	}

	var indexBytes int64
	for _, n := range sizes {
		indexBytes += n
	}
	entries := make([]codeindex.TipEntry, benchFiles)
	for i := range entries {
		entries[i] = codeindex.TipEntry{Path: strconv.Itoa(i/len(corpus)) + "/" + corpus[i%len(corpus)].path,
			Blob: ids[i]}
	}
	var tipMap countingWriter
	if err := codeindex.WriteTipMap(&tipMap, ids[0], entries); err != nil {
		b.Fatal(err)
	}
	indexBytes += tipMap.n

	b.ReportMetric(float64(benchFiles), "files")
	b.ReportMetric(float64(source)/(1<<30), "src-GiB")
	b.ReportMetric(float64(len(segs)), "segments")
	b.ReportMetric(elapsed.Seconds()/(float64(source)/1e9), "build-s/GB")
	b.ReportMetric(float64(indexBytes)/float64(source), "index-B/src-B")
	b.ReportMetric(float64(segmentPeak(b, corpus, ids, order))/(1<<20), "seg-peak-MiB")

	b.ReportMetric(deflated(b, corpus), "zlib-B/src-B")

	p50, p99 := queryTimes(b, corpus, segs, byID)
	b.ReportMetric(float64(p50.Microseconds())/1e3, "query-p50-ms")
	b.ReportMetric(float64(p99.Microseconds())/1e3, "query-p99-ms")
}

// deflated is the corpus's bytes compressed as an object is in a pack (zlib
// at its default level, 6), a byte of source: the least a copy holding the
// tip's blobs takes, deltas and history aside.
func deflated(b *testing.B, corpus []corpusFile) float64 {
	var out countingWriter
	var src int64
	for _, f := range corpus {
		w, err := flate.NewWriter(&out, 6)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		src += int64(len(f.data))
	}
	return float64(out.n) / float64(src)
}

// open opens a segment the benchmark wrote.
func open(b *testing.B, data []byte) *codeindex.Segment {
	s, err := codeindex.OpenSegment(data)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// countingWriter counts what is written to it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

var _ io.Writer = (*countingWriter)(nil)

// segmentPeak is the heap one segment's build holds at its peak: the
// first codeindex.SegmentSourceBytes of the tip, in id order, added to a
// builder and written beside it into memory, as the cache's place writes it.
func segmentPeak(b *testing.B, corpus []corpusFile, ids []codeindex.OID, order []int) int64 {
	base := heap()
	builder := &codeindex.Builder{}
	var buf []byte
	for _, i := range order {
		buf = blob(corpus, i, buf)
		if err := builder.Add(ids[i], buf); err != nil {
			b.Fatal(err)
		}
		if builder.SourceBytes() >= codeindex.SegmentSourceBytes {
			break
		}
	}
	buf = nil
	var out bytes.Buffer
	if err := codeindex.WriteSegment(&out, builder); err != nil {
		b.Fatal(err)
	}
	peak := heap() - base
	runtime.KeepAlive(builder)
	runtime.KeepAlive(out.Bytes())
	return peak
}

// queryTimes is the p50 and p99 of benchQueries searches of segs: literals
// taken from the unrotated copy's own lines, each an identifier of 6 to 40
// bytes, one in four folding case.
func queryTimes(b *testing.B, corpus []corpusFile, segs []*codeindex.Segment,
	byID map[codeindex.OID]int) (p50, p99 time.Duration) {

	rng := rand.New(rand.NewPCG(1, 2))
	times := make([]time.Duration, 0, benchQueries)
	var buf []byte
	for len(times) < benchQueries {
		text, ok := identifier(rng, corpus[rng.IntN(len(corpus))].data)
		if !ok {
			continue
		}
		plan, err := codeindex.Query{Text: text, Mode: codeindex.ModeLiteral, IgnoreCase: len(times)%4 == 3}.Plan()
		if err != nil {
			b.Fatalf("query %q: %v", text, err)
		}
		start := time.Now()
		hits := 0
	segments:
		for _, s := range segs {
			candidates, err := plan.Candidates(s)
			if err != nil {
				b.Fatal(err)
			}
			for _, id := range candidates {
				buf = blob(corpus, byID[id], buf)
				if _, err := plan.Confirm(b.Context(), buf, func(codeindex.Hit) bool {
					hits++
					return hits < code.MaxSearchHits
				}); err != nil {
					b.Fatal(err)
				}
				if hits >= code.MaxSearchHits {
					break segments
				}
			}
		}
		times = append(times, time.Since(start))
	}
	slices.Sort(times)
	return times[len(times)/2], times[len(times)*99/100]
}

// identifier is a word of one of data's lines — letters, digits and
// underscores, 6 to 40 bytes — chosen at random, or false where the line
// chosen holds none.
func identifier(rng *rand.Rand, data []byte) (string, bool) {
	lines := bytes.Split(data, []byte("\n"))
	line := lines[rng.IntN(len(lines))]
	var words []string
	for _, w := range bytes.FieldsFunc(line, func(r rune) bool {
		return !(r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	}) {
		if 6 <= len(w) && len(w) <= 40 {
			words = append(words, string(w))
		}
	}
	if len(words) == 0 {
		return "", false
	}
	return words[rng.IntN(len(words))], true
}
