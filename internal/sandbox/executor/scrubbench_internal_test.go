package executor

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkTheScrubAtThePlaintextBound scrubs a session at the plaintext one
// reading of an archive at its bound reads (256 MiB), both readings of the
// tar, from a file on disk, as the take does: one string to the bound (a whole
// PDF's base64), a million short lines of about 160 bytes, and one
// loaded-tools record whose list of tiny definitions all lose their line (the
// plan's bits at their most). Each reports s/scrub, the plaintext and archive
// in MiB, and held-MiB, the heap in use after a collection at every
// sixty-fourth read less the heap before ([heapSampler]). The figures, against
// SessionScrubWait and scrubHeldBytes, are in the package doc's "The scrub's
// bounds, and what it holds".
//
//	go test -run '^$' -bench '^BenchmarkTheScrubAtThePlaintextBound$' -benchtime 1x -timeout 30m
func BenchmarkTheScrubAtThePlaintextBound(b *testing.B) {
	bounds := boundsFor(executorkeep.SessionMaxBytes)
	// Room for the tar's header and its end, and the line's own words.
	body := int(bounds.plain) - 64<<10
	s := newScrubber([]string{longValue})
	for _, bc := range []struct {
		name  string
		write func(w io.Writer, r *rand.Rand) error
	}{
		{"one_string", func(w io.Writer, r *rand.Rand) error {
			if _, err := io.WriteString(w, `{"type":"user","message":{"content":[{"type":"tool_result","content":"`); err != nil {
				return err
			}
			if err := writeBase64Blocks(w, r, body-200); err != nil {
				return err
			}
			_, err := io.WriteString(w, `"}]}}`+"\n")
			return err
		}},
		{"a_million_short_lines", func(w io.Writer, r *rand.Rand) error {
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
		{"tiny_definitions", func(w io.Writer, _ *rand.Rand) error {
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
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "archive.tar.gz")
			plain, gzipped := writeBenchArchive(b, file, bc.write)
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
// written, so gzip shrinks it well past the eight times a session must to
// reach the plaintext's bound from the archive's.
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

// writeBenchArchive writes, to file, a gzipped tar of one transcript whose
// body write writes, and answers its plaintext and its size.
func writeBenchArchive(b *testing.B, file string, write func(io.Writer, *rand.Rand) error) (int64, int64) {
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
	if err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "projects/p/s.jsonl", Mode: 0o600,
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
