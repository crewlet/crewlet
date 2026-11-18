package gitproc_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
)

// These benchmarks are the package doc's "Measured" figures and what a pool
// change is compared by (make bench, six counts on each tree, benchstat).
// Every arm but BenchmarkTransfer's copy-path runs REAL git through a real
// pool: a process (fork, exec, environment, the guard's reads, reap, sweep)
// is what is priced, and a stand-in would price a shell. copy-path is the
// stand-in on purpose, to price the copy path with git taken out.

// BenchmarkRun is one git process through the pool, end to end.
//
//   - version: the floor every process pays (slot, environment, fork and
//     exec, procgroup's leader identity, reap, sweep); no repository, no guard.
//   - rev-parse: the floor, the guard's reads of config and commondir, and
//     git reading one object from a pack.
//   - rev-parse-in-scratch: the same in a repository holding only an
//     alternate (a push's scratch), so the guard's SECOND repository counts.
//   - cat-file-batch-check-1000: a thousand ids through [gitproc.Lines], the
//     shape of a fetch's wants check and a cache round's tag peel.
//   - parallel-rev-parse: every read slot busy; ns/op is wall time per
//     process across them, the node's aggregate rate.
func BenchmarkRun(b *testing.B) {
	quiet(b)
	p := realPool(b, gitproc.Options{})

	b.Run("version", func(b *testing.B) {
		c := gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline}
		for b.Loop() {
			if _, err := p.Run(b.Context(), c); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("rev-parse", func(b *testing.B) {
		h := sharedHistory(b)
		benchRevParse(b, p, h.dir, "", h.tip)
	})

	b.Run("rev-parse-in-scratch", func(b *testing.B) {
		h := sharedHistory(b)
		benchRevParse(b, p, scratchOver(b, h.dir), h.dir, h.tip)
	})

	b.Run("cat-file-batch-check-1000", func(b *testing.B) {
		h := sharedHistory(b)
		for b.Loop() {
			answered := 0
			lines := gitproc.NewLines(func(line []byte) error {
				if bytes.HasSuffix(line, []byte(" missing")) {
					return fmt.Errorf("cat-file answered %q", line)
				}
				answered++
				return nil
			})
			_, err := p.Run(b.Context(), gitproc.Cmd{
				Dir: h.dir, Args: []string{"cat-file", "--batch-check", "--buffer"},
				Stdin: strings.NewReader(h.ids), Stdout: lines,
				Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
			})
			if err == nil {
				err = lines.Close()
			}
			if err != nil {
				b.Fatal(err)
			}
			if answered != batchIDs {
				b.Fatalf("cat-file answered %d of the %d ids", answered, batchIDs)
			}
		}
	})

	b.Run("parallel-rev-parse", func(b *testing.B) {
		h := sharedHistory(b)
		c := revParse(h.dir, h.tip)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				res, err := p.Run(b.Context(), c)
				if err == nil && strings.TrimSpace(string(res.Stdout)) != h.tip {
					err = fmt.Errorf("rev-parse answered %q, want %s", res.Stdout, h.tip)
				}
				if err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}

// BenchmarkTransfer is a TRANSFER-lane process streaming the whole history
// through the pool, with a served fetch's deadline and stall watchdog, into a
// writer that only counts. MB/s, in two arms, since a clone is git's work and
// the pool's copy path in series:
//
//   - clone: upload-pack answering a protocol v2 fetch of the tip, as a clone
//     asks; what a node serves a fast client at.
//   - copy-path: a stand-in that cats the same pack, its start included: a
//     LOWER BOUND on the copy path (pipe, 64 KiB reads, metered stream). It
//     must sit far above clone, so git and never the pool bounds a clone:
//     under 200 MB/s on an idle machine is a copy-path finding.
func BenchmarkTransfer(b *testing.B) {
	quiet(b)
	h := sharedHistory(b)

	b.Run("clone", func(b *testing.B) {
		p := realPool(b, gitproc.Options{})
		request := pkt("command=fetch\n") + "0001" + pkt("ofs-delta\n") + pkt("no-progress\n") +
			pkt("want "+h.tip+"\n") + pkt("done\n") + "0000"
		clone := func() int64 {
			return transfer(b, p, gitproc.Cmd{
				Dir: h.dir, Args: []string{"upload-pack", "--stateless-rpc", h.dir}, Protocol: "version=2",
				Stdin: strings.NewReader(request),
			}, h.packBytes)
		}
		// One clone before the timer sizes the figure's unit. Not checked
		// per clone: side-band framing follows pack-objects' writes, so two
		// clones of one pack differ by a few hundred bytes.
		b.SetBytes(clone())
		for b.Loop() {
			clone()
		}
	})

	b.Run("copy-path", func(b *testing.B) {
		p := fakePool(b, `exec cat "$2"`, gitproc.Options{})
		c := gitproc.Cmd{Args: []string{"upload-pack", h.pack}}
		b.SetBytes(h.packBytes)
		for b.Loop() {
			if sent := transfer(b, p, c, h.packBytes); sent != h.packBytes {
				b.Fatalf("the stand-in sent %d bytes of a %d-byte pack", sent, h.packBytes)
			}
		}
	})
}

// transfer runs c as a served transfer of a history whose pack is packBytes
// — the transfer lane, upload-pack's deadline, the stall watchdog — and
// returns what it sent. A response under half the pack fails b: a clone of
// the whole history sends about its pack, and anything far smaller is a
// refusal, which a writer that only counts would price as a fast clone.
func transfer(b *testing.B, p *gitproc.Pool, c gitproc.Cmd, packBytes int64) int64 {
	b.Helper()
	var sent counter
	c.Stdout = &sent
	c.Class, c.Share, c.Deadline, c.Stall = gitproc.ClassTransfer, suiteShare, gitproc.UploadPackDeadline(packBytes), gitproc.TransferStall
	if _, err := p.Run(b.Context(), c); err != nil {
		b.Fatal(err)
	}
	n := sent.n.Load()
	if n < packBytes/2 {
		b.Fatalf("git %s sent %d bytes of a history whose pack is %d", c.Args[0], n, packBytes)
	}
	return n
}

// BenchmarkIndexPack is receive-pack's index-pack of a push over the history's
// pack, [gitproc.PackWorkPerMiB]'s anchor; MB/s of the pack. Its arguments are
// receive-pack's own (GIT_TRACE) less --report-end-of-input and
// --max-input-size, into a scratch over an alternate, not a quarantine
// (neither changes the work); a loose file's index-pack is only a floor. Read
// lane, one thread: the pool lends cores to a receive-pack, never a read, so
// this is a receive-pack on a node with no core free to lend.
//
//   - new-objects: an empty alternate, every pushed object new.
//   - known-objects: the history as alternate, so index-pack reads back and
//     compares every object, as for a push of objects reachable only from
//     refs its client was not shown: the WORST CASE, PackWorkPerMiB's anchor.
func BenchmarkIndexPack(b *testing.B) {
	quiet(b)
	p := realPool(b, gitproc.Options{})
	h := sharedHistory(b)

	b.Run("new-objects", func(b *testing.B) {
		empty := gittest.Empty(b)
		benchReceiveIndex(b, p, h, scratchOver(b, empty), empty)
	})
	b.Run("known-objects", func(b *testing.B) {
		benchReceiveIndex(b, p, h, scratchOver(b, h.dir), h.dir)
	})
}

// benchReceiveIndex is BenchmarkIndexPack's arm: receive-pack's index-pack
// of the history's pack in scratch, the pack it wrote removed after every
// operation with the timer stopped, so each one writes the pack afresh into
// a directory holding nothing else.
func benchReceiveIndex(b *testing.B, p *gitproc.Pool, h *history, scratch, alternate string) {
	b.Helper()
	c := gitproc.Cmd{
		Dir: scratch, Alternate: alternate,
		Args: []string{"index-pack", "--stdin", fmt.Sprintf("--pack_header=%d,%d", h.packVersion, h.packObjects),
			"--keep=receive-pack", "--strict", "--fix-thin"},
		Config: []gitproc.KV{{Key: "pack.writeReverseIndex", Value: "true"}, {Key: "pack.indexVersion", Value: "2"}},
		Class:  gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(h.packBytes),
	}
	// --keep makes index-pack name what it wrote "keep" rather than "pack".
	want := "keep\t" + h.checksum + "\n"
	written := filepath.Join(scratch, "objects", "pack")
	b.SetBytes(h.packBytes)
	for b.Loop() {
		body := packBody(b, h)
		c.Stdin = body
		res, err := p.Run(b.Context(), c)
		_ = body.Close()
		if err != nil {
			b.Fatal(err)
		}
		if got := string(res.Stdout); got != want {
			b.Fatalf("index-pack answered %q, want %q", got, want)
		}
		b.StopTimer()
		removeWritten(b, written, h.checksum)
		b.StartTimer()
	}
}

// packBody is the history's pack after its header, as receive-pack hands it
// on once it has read the header itself: a file, so every Read is bounded.
// The caller closes it.
func packBody(b *testing.B, h *history) *os.File {
	b.Helper()
	f, err := os.Open(h.pack)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := f.Seek(packHeaderBytes, io.SeekStart); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	return f
}

// removeWritten removes what one receive-pack index-pack wrote into dir, and
// fails b unless that is exactly the pack, its index, its reverse index and
// its .keep: a missing .rev would be a figure that skipped the reverse index
// every stored bundle carries, and anything else left behind would be in the
// next operation's way.
func removeWritten(b *testing.B, dir, checksum string) {
	b.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	base := "pack-" + checksum
	if want := []string{base + ".idx", base + ".keep", base + ".pack", base + ".rev"}; !slices.Equal(names, want) {
		b.Fatalf("index-pack wrote %v into %s, want %v", names, dir, want)
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			b.Fatal(err)
		}
	}
}

// revParse peels id in dir: a read of one object.
func revParse(dir, id string) gitproc.Cmd {
	return read(dir, "rev-parse", "--verify", id+"^{commit}")
}

// benchRevParse is BenchmarkRun's rev-parse arms: id peeled in dir.
func benchRevParse(b *testing.B, p *gitproc.Pool, dir, alternate, id string) {
	b.Helper()
	c := revParse(dir, id)
	c.Alternate = alternate
	for b.Loop() {
		res, err := p.Run(b.Context(), c)
		if err != nil {
			b.Fatal(err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != id {
			b.Fatalf("rev-parse in %s answered %q, want %s", dir, got, id)
		}
	}
}

// scratchOver is an empty repository whose one alternate is repo's objects:
// a push's scratch, or an engine write's, as the guard sees one.
func scratchOver(tb testing.TB, repo string) string {
	tb.Helper()
	scratch := gittest.Empty(tb)
	info := filepath.Join(scratch, "objects", "info")
	if err := os.MkdirAll(info, hostbox.DirMode); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(repo, "objects")+"\n"), hostbox.FileMode); err != nil {
		tb.Fatal(err)
	}
	return scratch
}

// counter is a Stdout that keeps nothing and counts what it was given.
type counter struct{ n atomic.Int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// quiet raises the log level to Warn while a benchmark runs: an INFO line
// (code_git_ready, at every New) inside a result line breaks what a
// comparison tool reads (internal/statelog's reason). Warn still prints,
// since a killed process mid-measurement is a finding.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// THE HISTORY every arm that needs a repository reads: historyFiles files of
// seeded words, about historyFileBytes each, then historyRewrites commits each
// replacing a tenth of every file's lines, so its one pack has delta chains
// that deep for index-pack to resolve and pack-objects to reuse. The same
// bytes on every run (seeded words, one committer and date, a one-thread
// pack, whose delta search has one answer); the logged pack checksum shows it.
const (
	historyFiles     = 64
	historyFileBytes = 128 << 10
	historyRewrites  = 32
	// batchIDs is how many of the history's ids cat-file is asked about;
	// the history holds about twice as many objects.
	batchIDs = 1000
)

// history is the built repository and what the arms read of it.
type history struct {
	dir       string // a bare repository the engine's shape, main at tip
	tip       string
	pack      string // the path of its one pack
	checksum  string // that pack's checksum, which index-pack prints
	packBytes int64
	// packVersion and packObjects are the pack's header, which
	// receive-pack reads off the wire and hands index-pack as
	// --pack_header=<version>,<objects>.
	packVersion, packObjects uint32
	ids                      string // the first batchIDs ids rev-list names, a line each
}

// packHeaderBytes is a pack's header: "PACK", then its version and its
// object count, each four bytes in network order.
const packHeaderBytes = 12

// readPackHeader is the version and the object count in the header of the
// pack at path.
func readPackHeader(path string) (version, objects uint32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	var header [packHeaderBytes]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, 0, fmt.Errorf("reading the header of %s: %w", path, err)
	}
	if string(header[:4]) != "PACK" {
		return 0, 0, fmt.Errorf("%s does not begin with a pack's signature: %q", path, header[:4])
	}
	return binary.BigEndian.Uint32(header[4:8]), binary.BigEndian.Uint32(header[8:12]), nil
}

// shared is the history, built ONCE PER TEST BINARY outside every timer: the
// build takes seconds, which per arm or count would outweigh the measuring.
//
// A mutex and a nil check, not sync.OnceValues: the build fails through
// b.Fatalf, whose Goexit inside OnceValues is re-raised as a nil panic that
// kills the binary; here a failed build leaves nothing and the next arm
// retries. root is the binary's own, since a benchmark's directories go when
// it ends; TestMain removes it.
var shared struct {
	mu   sync.Mutex
	root string
	h    *history
}

// sharedHistory is the history, built by the first arm to ask.
func sharedHistory(b *testing.B) *history {
	b.Helper()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.h != nil {
		return shared.h
	}
	if shared.root == "" {
		root, err := binaryDir()
		if err != nil {
			b.Fatal(err)
		}
		shared.root = root
	}
	shared.h = buildHistory(b, filepath.Join(shared.root, "history"))
	return shared.h
}

// binaryDir is a directory that outlives every benchmark of this binary:
// TestMain removes it ([removeShared]). Not a b.TempDir, which goes with the
// benchmark that made it.
func binaryDir() (string, error) {
	return os.MkdirTemp("", "gitproc-bench-*")
}

// removeShared removes what the benchmarks built for the whole binary.
func removeShared() {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.root != "" {
		_ = os.RemoveAll(shared.root)
	}
}

// buildHistory writes the history through git fast-import — straight into a
// pack, since every version of every file as a loose object would put about
// a hundred megabytes on disk to throw away — then packs it the engine's
// way (gittest.Pack: the pack, its version-2 index and its reverse index)
// into an empty repository, and moves that repository to dst.
func buildHistory(b *testing.B, dst string) *history {
	b.Helper()
	began := time.Now()
	client := gittest.NewClient(b)
	wt := client.Init(b)

	stream, feed := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := writeHistory(feed)
		// nil closes it cleanly: fast-import reads its end of file.
		_ = feed.CloseWithError(err)
		written <- err
	}()
	_, stderr, err := client.Try(b, wt, stream, "fast-import", "--quiet")
	// A fast-import that stopped reading would leave the writer blocked
	// on a pipe nobody drains; this unblocks it before it is waited for.
	_ = stream.CloseWithError(errors.New("fast-import has ended"))
	if werr := <-written; werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		b.Fatalf("importing the history: %v\n%s", err, stderr)
	}

	oneThread := client.Env("GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=pack.threads", "GIT_CONFIG_VALUE_0=1")
	bare := gittest.Empty(b)
	checksum := gittest.Pack(b, oneThread, bare, wt, "main")
	tip := strings.TrimSpace(client.Run(b, wt, "rev-parse", "main"))
	gittest.SetRefs(b, bare, map[string]string{"refs/heads/main": tip})

	var ids strings.Builder
	n := 0
	for line := range strings.Lines(client.Run(b, wt, "rev-list", "--objects", "main")) {
		if n == batchIDs {
			break
		}
		id, _, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		ids.WriteString(id + "\n")
		n++
	}
	if n < batchIDs {
		b.Fatalf("the history holds %d objects, fewer than the %d cat-file is asked about", n, batchIDs)
	}

	if err := os.Rename(bare, dst); err != nil {
		b.Fatalf("moving the history to the binary's directory: %v", err)
	}
	pack := filepath.Join(dst, "objects", "pack", "pack-"+checksum+".pack")
	info, err := os.Stat(pack)
	if err != nil {
		b.Fatal(err)
	}
	version, objects, err := readPackHeader(pack)
	if err != nil {
		b.Fatal(err)
	}
	// The checksum is the fixture's identity: two runs that print the same
	// one measured the same bytes.
	b.Logf("the history: %d files of %d KiB, %d rewrites, a %.1f MiB pack %s of %d objects, built in %v",
		historyFiles, historyFileBytes>>10, historyRewrites, float64(info.Size())/(1<<20), checksum, objects,
		time.Since(began).Round(time.Millisecond)) //fixedwait:measure logged with the history it built
	return &history{
		dir: dst, tip: tip, pack: pack, checksum: checksum, packBytes: info.Size(),
		packVersion: version, packObjects: objects, ids: ids.String(),
	}
}

// historyEpoch is the committer date of every commit: the client's own clock,
// 2026-01-01T00:00:00Z.
const historyEpoch = 1767225600

// writeHistory writes the history as a fast-import stream: every version of
// one file before the next file's, each a blob of its own, because
// fast-import deltas a blob against the blob written just before it — so a
// version is stored as a delta of the one before, a chain as deep as the
// history — and then one commit per version naming them.
func writeHistory(w io.Writer) error {
	out := bufio.NewWriterSize(w, 1<<20)
	words := vocabulary()
	versions := historyRewrites + 1
	mark := func(file, version int) int { return 1 + file*versions + version }
	var body strings.Builder
	for file := range historyFiles {
		r := rand.New(rand.NewPCG(uint64(file), 0x6769740a))
		lines := make([]string, 0, historyFileBytes/64)
		for size := 0; size < historyFileBytes; {
			line := sentence(r, words)
			lines = append(lines, line)
			size += len(line)
		}
		for version := range versions {
			if version > 0 {
				for range len(lines) / 10 {
					lines[r.IntN(len(lines))] = sentence(r, words)
				}
			}
			body.Reset()
			for _, line := range lines {
				body.WriteString(line)
			}
			if _, err := fmt.Fprintf(out, "blob\nmark :%d\ndata %d\n%s\n", mark(file, version), body.Len(), body.String()); err != nil {
				return err
			}
		}
	}
	for version := range versions {
		message := fmt.Sprintf("version %d\n", version)
		if _, err := fmt.Fprintf(out, "commit refs/heads/main\ncommitter Crewlet Test <test@example.com> %d +0000\ndata %d\n%s",
			historyEpoch, len(message), message); err != nil {
			return err
		}
		for file := range historyFiles {
			if _, err := fmt.Fprintf(out, "M 100644 :%d f%02d.txt\n", mark(file, version), file); err != nil {
				return err
			}
		}
		if _, err := out.WriteString("\n"); err != nil {
			return err
		}
	}
	return out.Flush()
}

// vocabulary is a thousand and twenty-four lowercase words of two to nine
// letters, the same on every run.
func vocabulary() []string {
	r := rand.New(rand.NewPCG(1, 2))
	words := make([]string, 1024)
	for i := range words {
		word := make([]byte, 2+r.IntN(8))
		for j := range word {
			word[j] = byte('a' + r.IntN(26))
		}
		words[i] = string(word)
	}
	return words
}

// sentence is one line of four to twelve words, ending in a newline.
func sentence(r *rand.Rand, words []string) string {
	n := 4 + r.IntN(9)
	var s strings.Builder
	for i := range n {
		if i > 0 {
			s.WriteByte(' ')
		}
		s.WriteString(words[r.IntN(len(words))])
	}
	s.WriteByte('\n')
	return s.String()
}

// BenchmarkTraceSink is the reader every run's trace2 stream passes through,
// over three streams as git writes them, in a pipe's 64 KiB pieces:
//
//   - healthy: a clone's upload-pack and pack-objects events, the stream
//     every successful run pays for;
//   - damaged: the same clone over a damaged reverse index, pack-objects'
//     position row and upload-pack's abort beside it;
//   - echo: a request's 390 KB echo of control bytes, the longest line a
//     request can make git write, skipped whole.
func BenchmarkTraceSink(b *testing.B) {
	child := func(argv ...string) string {
		return versionLine(2, "2.47.3") + startLine(2, argv...) +
			`{"event":"cmd_name","sid":"` + sidAt(2) + `","thread":"main","name":"pack-objects","hierarchy":"upload-pack/pack-objects"}` + "\n"
	}
	region := func(depth int) string {
		return `{"event":"region_enter","sid":"` + sidAt(depth) + `","thread":"main","nesting":1,"category":"pack-objects","label":"enumerate-objects"}` + "\n" +
			`{"event":"region_leave","sid":"` + sidAt(depth) + `","thread":"main","t_rel":0.000412,"nesting":1,"category":"pack-objects","label":"enumerate-objects"}` + "\n"
	}
	leader := versionLine(1, "2.47.3") + startLine(1, "git", "upload-pack", "--stateless-rpc", "/srv/r.git") +
		`{"event":"cmd_ancestry","sid":"` + sidAt(1) + `","thread":"main","ancestry":["crewlet"]}` + "\n"
	healthy := leader + child("git", "pack-objects", "--revs", "--thin", "--stdout") + region(2) + region(2) +
		exitLine(2, 0) + exitLine(1, 0)
	damaged := leader + child("git", "pack-objects", "--revs", "--thin", "--stdout") + region(2) +
		errorLine(2, "invalid MIDX object position, MIDX is likely corrupt",
			"invalid MIDX object position, MIDX is likely corrupt") + exitLine(2, 128) +
		errorLine(1, "git upload-pack: %s", "git upload-pack: git-pack-objects died with error.") +
		errorLine(1, "%s", "git upload-pack: aborting due to possible repository corruption on the remote side.") +
		exitLine(1, 128)
	echo := leader + errorLine(1, "unexpected line: '%s'", "unexpected line: '"+controlBytes(65000)+"'") +
		exitLine(1, 128)
	for _, tc := range []struct{ name, stream string }{
		{"healthy", healthy}, {"damaged", damaged}, {"echo", echo},
	} {
		b.Run(tc.name, func(b *testing.B) {
			stream := []byte(tc.stream)
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			for b.Loop() {
				gitproc.ReadTrace("/srv/r.git", stream, 64<<10)
			}
		})
	}
}
