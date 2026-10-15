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

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
)

// The benchmarks below are the figures the package doc's "Measured" section
// records, and the ones a change to the pool is compared by (make bench, six
// counts on each tree, benchstat). Every arm but one runs REAL git through a
// real pool, because what is being priced is a process — its fork, its exec,
// its environment, the guard's reads of the repository and its alternate,
// the reap and the sweep — and a stand-in would price a shell instead. The
// one stand-in, BenchmarkTransfer's copy-path, is there for exactly that
// difference: to price the pool's copy path with git taken out of it.

// BenchmarkRun is one git process through the pool, end to end.
//
//   - version is the floor every process pays whatever it does: the slot,
//     the built environment, fork and exec, procgroup's identity of the
//     leader, the reap and the sweep. No repository, so no guard.
//   - rev-parse peels the history's tip in the history itself: the floor
//     plus the repository guard's reads of its config and commondir, and git
//     opening a pack to read one object.
//   - rev-parse-in-scratch is the same command in a repository holding
//     nothing but an alternate naming the history — a push's scratch, an
//     advertisement view — so the guard's SECOND repository (the alternate,
//     resolved and checked) is in the figure, and git reads the object
//     through it.
//   - cat-file-batch-check-1000 is one cat-file --batch-check --buffer over
//     a thousand of the history's ids, its answers streamed through
//     [gitproc.Lines] — the shape of a fetch's want list checked against a
//     cache before upload-pack runs, and of a cache round's peel of its
//     tags.
//   - parallel-rev-parse is rev-parse from as many goroutines as the read
//     lane has slots (GOMAXPROCS of each), so every slot is busy: ns/op is
//     wall time per process across all of them, the node's aggregate rate.
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
// through the pool — with the deadline and the stall watchdog a served fetch
// carries — into a writer that only counts. MB/s, in two arms, because a
// clone's rate is two things in series and only one of them is the pool's:
//
//   - clone is upload-pack answering a protocol v2 fetch of the tip, as a
//     clone asks it: git's own work — pack-objects counting the history and
//     reusing the stored pack, upload-pack framing it into side-band — and
//     the pool's copy path behind it. What a node serves a fast client at.
//   - copy-path is a stand-in git that writes the same pack with cat, so
//     git's work is out of the figure. What is left is the pool's copy path
//     — its pipe, its 64 KiB reads, the metered stream — and the stand-in's
//     own start on every operation (a shell exec'ing cat: the process floor
//     BenchmarkRun/version prices), so the figure is a LOWER BOUND on the
//     copy path. It must sit far above the clone arm, so that a clone is
//     bounded by git and never by the pool: under 200 MB/s on an idle
//     machine is a finding in the copy path, not a number to record.
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
		// One clone before the timer sizes the figure's unit. Not held
		// equal across clones: the side-band frames git cuts a pack into
		// follow how pack-objects' writes reach upload-pack, so two clones
		// of one pack differ by a few hundred bytes of framing in nineteen
		// megabytes.
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
	c.Class, c.Deadline, c.Stall = gitproc.ClassTransfer, gitproc.UploadPackDeadline(packBytes), gitproc.TransferStall
	if _, err := p.Run(b.Context(), c); err != nil {
		b.Fatal(err)
	}
	n := sent.n.Load()
	if n < packBytes/2 {
		b.Fatalf("git %s sent %d bytes of a history whose pack is %d", c.Args[0], n, packBytes)
	}
	return n
}

// BenchmarkIndexPack is the index-pack receive-pack runs over every push
// before its hook, run as receive-pack runs it, over the history's pack — so
// it is what the receiving node spends a core a mebibyte on, which
// [gitproc.PackWorkPerMiB] is anchored to. MB/s is of the pack.
//
// THE COMMAND IS RECEIVE-PACK'S OWN, as GIT_TRACE shows a push under the
// receive configuration starting it: the pack on stdin after the header
// receive-pack has already read (--stdin --pack_header), every object
// checked (--strict, which receive.fsckObjects asks for), thin bases
// completed from the repository (--fix-thin; this pack has none to
// complete) and a .keep beside the pack (--keep), with the receive
// configuration's version-2 index and reverse index — all of it written
// into a scratch whose alternate is a repository, at the base
// configuration's one thread.
//
// NOT index-pack over a file outside any repository: that is the delta
// resolution alone — no stdin to write out, no object checked, nothing to
// look an object up in — and on the machine the package doc names it took
// 1.9 s where known-objects below takes 2.5, so a figure taken from it is a
// floor under the work a push costs, not that work. Two of receive-pack's arguments are left out,
// neither of which changes the work: --report-end-of-input (one NUL for the
// side-band) and --max-input-size (a bound this pack is far under). And it
// writes into the scratch itself rather than into receive-pack's
// quarantine, which is one empty object directory more in front of the same
// alternate.
//
//   - new-objects: the scratch's alternate is an empty repository, so every
//     pushed object is new to it — a push of work the repository has not
//     seen, every lookup a miss.
//   - known-objects: the scratch's alternate is the history itself, so every
//     pushed object is already there, and index-pack reads each one back and
//     compares it with what arrived (its collision check). A client leaves
//     out of a push only what the refs it was shown reach, so a push carries
//     objects its repository already holds whenever they are reachable only
//     from a ref the client was not shown or no longer has. The WORST CASE,
//     and the arm PackWorkPerMiB is anchored to.
//
// READ-lane: the class only picks the lane here. The pool lends cores to a
// receive-pack and never to a read, so this is the one thread a receive-pack
// runs its index-pack with on a node that has no core free to lend.
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
// a push's scratch, as the guard sees one.
func scratchOver(b *testing.B, repo string) string {
	b.Helper()
	scratch := gittest.Empty(b)
	info := filepath.Join(scratch, "objects", "info")
	if err := os.MkdirAll(info, hostbox.DirMode); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(repo, "objects")+"\n"), hostbox.FileMode); err != nil {
		b.Fatal(err)
	}
	return scratch
}

// counter is a Stdout that keeps nothing and counts what it was given.
type counter struct{ n atomic.Int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// quiet raises the process's log level to Warn while a benchmark runs, for
// internal/statelog's reason: an INFO line (code_git_ready, at every New)
// landing inside a result line breaks the line a comparison tool reads. Warn
// and above still print, because a killed process during a measurement is a
// finding. Put back to the level the logging package starts a process at.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// THE HISTORY every arm that needs a repository reads: an initial commit of
// historyFiles files of seeded words, about historyFileBytes each, then
// historyRewrites commits that each replace a tenth of every file's lines —
// so its one pack carries real delta chains, historyRewrites deep, which is
// what index-pack resolves and pack-objects reuses. The same bytes on every
// machine and every run: every word is drawn from seeded streams, every
// commit carries one committer and one date, and the pack is written by one
// thread, whose delta search is the one with a single answer — the pack's
// checksum, logged when the history is built, says so of any two runs.
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

// shared is the history, built ONCE PER TEST BINARY outside every timer
// rather than once per arm or per count: building it takes seconds — every
// version of every file generated, hashed and deltified, then packed — and
// the arms that read it would otherwise spend more of a run building it than
// measuring git.
//
// A mutex and a nil check rather than sync.OnceValues, because the build
// fails through the benchmark's own b.Fatalf (gittest's helpers do), and a
// Goexit inside OnceValues' function is re-raised as a nil panic that takes
// the whole binary down rather than failing the one benchmark; here a failed
// build leaves nothing behind, and the next arm to ask builds it again.
//
// root is a directory of the BINARY's own, because every directory a
// benchmark makes is removed when that benchmark ends: the build moves the
// repository there, and TestMain removes it once every benchmark has run.
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
		time.Since(began).Round(time.Millisecond))
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
