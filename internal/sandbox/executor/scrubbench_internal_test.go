package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkTheScrubAtThePlaintextBound scrubs a session at [scrubPlainMax],
// both readings of the tar, from a file on disk, as the take does: one string
// to the bound (a PDF's base64), a million short lines of about 160 bytes, a
// loaded-tools record whose tiny definitions all lose their line, and the
// shapes a harness writes that set SessionScrubWait: a tool result's string of
// `pwd=x `, one of `pwd=` lines torn by a take mid-write, and a tool result's
// file of `pwd=x` lines. Each reports s/scrub, the plaintext and archive in
// MiB, and held-MiB, the heap in use after a collection at every sixty-fourth
// read less the heap before ([heapSampler]); the package doc's "The scrub's
// bounds, and what it holds" has the figures.
//
//	go test -run '^$' -bench '^BenchmarkTheScrubAtThePlaintextBound$' -benchtime 1x -timeout 60m
func BenchmarkTheScrubAtThePlaintextBound(b *testing.B) {
	bounds := plainBounds(scrubPlainMax)
	// Room for the tar's header and its end, and the line's own words.
	body := int(bounds.plain) - 64<<10
	s := newScrubber([]string{longValue})
	for _, bc := range []struct {
		name   string
		member string
		write  func(w io.Writer, r *rand.Rand) error
	}{
		{"one_string", benchTranscript, func(w io.Writer, r *rand.Rand) error {
			if _, err := io.WriteString(w, `{"type":"user","message":{"content":[{"type":"tool_result","content":"`); err != nil {
				return err
			}
			if err := writeBase64Blocks(w, r, body-200); err != nil {
				return err
			}
			_, err := io.WriteString(w, `"}]}}`+"\n")
			return err
		}},
		{"a_million_short_lines", benchTranscript, func(w io.Writer, r *rand.Rand) error {
			words := strings.Fields("the a file read write ok done test build go run error line value key session")
			for i := range 1_000_000 {
				var line strings.Builder
				fmt.Fprintf(&line, `{"type":"assistant","uuid":"u-%d","message":{"content":[{"type":"text","text":"`, i)
				for range 12 {
					line.WriteString(words[r.IntN(len(words))])
					line.WriteByte(' ')
				}
				line.WriteString(`"}]},"api_key":"k-` + fmt.Sprint(i) + `"}` + "\n")
				if _, err := io.WriteString(w, line.String()); err != nil {
					return err
				}
			}
			return nil
		}},
		{"tiny_definitions", benchTranscript, func(w io.Writer, _ *rand.Rand) error {
			head := `{"parentUuid":null,"note":"` + longValue + `","attachment":{"type":"deferred_tools_record","entries":[1`
			if _, err := io.WriteString(w, head); err != nil {
				return err
			}
			chunk := strings.Repeat(",1", 32<<10)
			for n := len(head); n < body-len(chunk); n += len(chunk) {
				if _, err := io.WriteString(w, chunk); err != nil {
					return err
				}
			}
			_, err := io.WriteString(w, `]},"type":"attachment"}`+"\n")
			return err
		}},
		// THE SHAPES THAT SET THE WAIT: short assignments the redaction weighs
		// at every `=`, in a tool result's string, in one torn at its last line
		// as a take mid-write finds it, and in a tool result's file.
		{"pwd_spaced_string", benchTranscript, func(w io.Writer, _ *rand.Rand) error {
			return benchOneString(w, body, "pwd=x ")
		}},
		{"pwd_escaped_torn", benchTranscript, func(w io.Writer, _ *rand.Rand) error {
			if err := benchOneString(w, body-64, "pwd=\n"); err != nil {
				return err
			}
			_, err := io.WriteString(w, `{"type":"user","message":`)
			return err
		}},
		{"pwd_lines_file", "projects/p/s/tool-results/out.txt", func(w io.Writer, _ *rand.Rand) error {
			return benchRepeatTo(w, body, "pwd=x\n")
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "archive.tar.gz")
			plain, gzipped := writeBenchArchive(b, file, bc.member, bc.write)
			if plain > bounds.plain {
				b.Fatalf("the case's plaintext is %d bytes, past the %d one reading reads", plain, bounds.plain)
			}
			scrub := func(first io.Reader, reopen func() (io.ReadCloser, error)) error {
				return scrubTar(first, reopen, io.Discard, s, bounds)
			}
			runtime.GC()
			for b.Loop() {
				f, err := os.Open(file)
				if err != nil {
					b.Fatal(err)
				}
				err = scrub(f, func() (io.ReadCloser, error) { return os.Open(file) })
				_ = f.Close()
				if err != nil {
					b.Fatalf("the scrub refused the case: %v", err)
				}
			}
			b.ReportMetric(float64(b.Elapsed())/float64(b.N)/float64(time.Second), "s/scrub")
			b.StopTimer()
			// REPORTED AFTER THE LOOP, whose first call resets every metric
			// reported before it.
			b.ReportMetric(float64(plain)/(1<<20), "plain-MiB")
			b.ReportMetric(float64(gzipped)/(1<<20), "archive-MiB")
			b.ReportMetric(float64(benchHeld(b, file, scrub))/(1<<20), "held-MiB")
			b.Logf("against SessionScrubWait (%s): %.1f s a scrub", sandbox.SessionScrubWait,
				float64(b.Elapsed())/float64(b.N)/float64(time.Second))
		})
	}
}

// writeBase64Blocks writes n bytes of base64 made of blocks drawn at random
// from four random ones, each within gzip's window of the last time it was
// written, so gzip shrinks it well past what a session must to reach the
// plaintext's bound from the archive's.
func writeBase64Blocks(w io.Writer, r *rand.Rand, n int) error {
	blocks := make([]string, 4)
	for i := range blocks {
		blocks[i] = base64Run(r, 4096)
	}
	for written := 0; written < n; {
		block := blocks[r.IntN(len(blocks))][:min(4096, n-written)]
		if _, err := io.WriteString(w, block); err != nil {
			return err
		}
		written += len(block)
	}
	return nil
}

// benchTranscript is the member a benchmark's session is its main thread in.
const benchTranscript = "projects/p/s.jsonl"

// benchOneString writes one Claude Code user line whose tool result is line,
// JSON-escaped, repeated to about n bytes.
func benchOneString(w io.Writer, n int, line string) error {
	quoted, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(w, `{"type":"user","message":{"content":[{"type":"tool_result",`+
		`"tool_use_id":"toolu_01","content":"`); err != nil {
		return err
	}
	if err = benchRepeatTo(w, n-200, string(quoted[1:len(quoted)-1])); err != nil {
		return err
	}
	_, err = io.WriteString(w, `"}]}}`+"\n")
	return err
}

// benchRepeatTo writes s repeated to at most n bytes.
func benchRepeatTo(w io.Writer, n int, s string) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	for written := 0; written+len(s) <= n; written += len(s) {
		if _, err := bw.WriteString(s); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// writeBenchArchive writes, to file, a gzipped tar of one member, named
// member, whose body write writes, and answers its plaintext and its size.
func writeBenchArchive(b *testing.B, file, member string, write func(io.Writer, *rand.Rand) error) (int64, int64) {
	b.Helper()
	body := filepath.Join(filepath.Dir(file), "body")
	f, err := os.Create(body)
	if err != nil {
		b.Fatal(err)
	}
	if err = write(f, rand.New(rand.NewPCG(1170, 8))); err != nil {
		b.Fatal(err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		b.Fatal(err)
	}
	out, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	zw := gzip.NewWriter(out)
	tw := tar.NewWriter(zw)
	if err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: member, Mode: 0o600,
		Size: size}); err != nil {
		b.Fatal(err)
	}
	if _, err = io.Copy(tw, f); err != nil {
		b.Fatal(err)
	}
	_ = f.Close()
	if err = tw.Close(); err != nil {
		b.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		b.Fatal(err)
	}
	gzipped, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		b.Fatal(err)
	}
	_ = out.Close()
	_ = os.Remove(body)
	return size + 3*512, gzipped
}

// benchHeld is what one more scrub of file held at most, read as
// [TestTheScrubHoldsNoMoreThanItsStatedFigure] reads it.
func benchHeld(b *testing.B, file string, scrub func(io.Reader, func() (io.ReadCloser, error)) error) uint64 {
	b.Helper()
	archive, err := os.ReadFile(file)
	if err != nil {
		b.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	most := before.HeapAlloc
	reopen := func() (io.ReadCloser, error) { return io.NopCloser(&heapSampler{b: archive, most: &most}), nil }
	if err = scrub(&heapSampler{b: archive, most: &most}, reopen); err != nil {
		b.Fatal(err)
	}
	return most - before.HeapAlloc
}

// BenchmarkASecondTakeOfAPDFSession takes a session whose main thread holds a
// whole-PDF read at Claude Code's 20 MiB limit (its base64 twice, [benchPDF]
// bytes each, of random bytes as a PDF's compressed streams read) beside
// ordinary lines, scrubbed of three values, both readings of the tar from
// memory: with no memo, as a process's first take of it, and as its second.
// Each reports s/take and the memo's answers a take; the figures are in the
// package doc's "A text a take found unchanged is not redacted again".
//
//	GOMAXPROCS=2 go test -run '^$' -bench '^BenchmarkASecondTakeOfAPDFSession$' -benchtime 3x -count 3
func BenchmarkASecondTakeOfAPDFSession(b *testing.B) {
	r := rand.New(rand.NewPCG(1170, 6))
	raw := make([]byte, benchPDF/4*3)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	data := "JVBERi0xLjcK" + base64.StdEncoding.EncodeToString(raw)[:benchPDF-12]
	var thread strings.Builder
	for i := range 200 {
		fmt.Fprintf(&thread, `{"type":"assistant","uuid":"a-%d","message":{"content":[{"type":"text","text":"reading the file %d"}]}}`+"\n", i, i)
	}
	thread.WriteString(benchPDFLine("a-199", "u-1", "toolu_01", "2026-11-19T10:00:00.000Z", data) + "\n")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "projects/p/s.jsonl", Mode: 0o600, Size: int64(thread.Len()),
		Typeflag: tar.TypeReg}); err != nil {
		b.Fatal(err)
	}
	if _, err := io.WriteString(tw, thread.String()); err != nil {
		b.Fatal(err)
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		b.Fatal(err)
	}
	archive := buf.Bytes()
	s := newScrubber([]string{"engine-value-one-0001", "engine-value-two-0002", "engine-value-three-03"})
	take := func(b *testing.B, memo *scrubMemo) *memoUse {
		use := &memoUse{memo: memo, scope: "agent"}
		if err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), io.Discard, s,
			testBounds.remembering(use)); err != nil {
			b.Fatalf("the take: %v", err)
		}
		return use
	}
	warmed := newScrubMemo()
	for _, bc := range []struct {
		name string
		memo func() *scrubMemo
	}{
		{"memo_off", func() *scrubMemo { return nil }},
		{"first", newScrubMemo},
		{"second", func() *scrubMemo { return warmed }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if bc.name == "second" && warmed.all.Len() == 0 {
				take(b, warmed)
			}
			hits := 0
			b.ResetTimer()
			for range b.N {
				hits += take(b, bc.memo()).n.hits
			}
			b.StopTimer()
			b.ReportMetric(b.Elapsed().Seconds()/float64(b.N), "s/take")
			b.ReportMetric(float64(hits)/float64(b.N), "hits/take")
		})
	}
}
