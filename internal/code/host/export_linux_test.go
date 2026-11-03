//go:build linux

package host

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// GitPeak is the most memory one git process s ran held while its output was
// read: Peak is its VmHWM, the high-water mark of its resident memory in
// bytes, read from /proc each time a piece of its output reached the caller;
// Wrote is how many bytes of output there were; and Found is whether the
// process was found at its first output. One that had ended by then — an
// output small enough to sit in the pipe whole — was never read, and its
// Peak says nothing.
type GitPeak struct {
	Args  []string
	Peak  int64
	Wrote int64
	Found bool
}

// PeakGit has s watch every READ-lane git process it hands the pool from now
// on whose output a caller of s reads (Cmd.Stdout) — the scans of a push
// among them — and answers a reader of what each held at most, in order. Set
// before the request it is to watch. Linux alone: it reads /proc.
func PeakGit(s *Server) func() []GitPeak {
	var (
		mu    sync.Mutex
		peaks []GitPeak
	)
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		if c.Stdout == nil || c.Dir == "" || c.Class != gitproc.ClassRead {
			return run(ctx, c)
		}
		w := &peakWriter{w: c.Stdout, dir: c.Dir, args: c.Args}
		c.Stdout = w
		res, err := run(ctx, c)
		mu.Lock()
		peaks = append(peaks, GitPeak{Args: slices.Clone(c.Args), Peak: w.peak, Wrote: w.wrote, Found: w.pid != 0})
		mu.Unlock()
		return res, err
	}
	return func() []GitPeak {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(peaks)
	}
}

// peakWriter is a Cmd.Stdout reading the high-water mark of the process
// writing to it each time it is written, before it hands the bytes on: while
// a Write runs, the process is either done writing or held writing to a full
// pipe, and every output larger than the pipe and the pool's read of it
// holds it alive for the first.
type peakWriter struct {
	w           io.Writer
	dir         string
	args        []string
	looked      bool
	pid         int
	peak, wrote int64
}

func (p *peakWriter) Write(b []byte) (int, error) {
	if !p.looked {
		p.looked = true
		p.pid = gitPID(p.dir, p.args)
	}
	if p.pid != 0 {
		if hwm, ok := vmHWM(p.pid); ok {
			p.peak = max(p.peak, hwm)
		}
	}
	p.wrote += int64(len(b))
	return p.w.Write(b)
}

// gitPID is the process running in dir — the pool runs git with its
// repository as its working directory — whose arguments end with args, or 0
// for none.
func gitPID(dir string, args []string) int {
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err != nil || cwd != want {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		var argv []string
		for _, a := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
			argv = append(argv, string(a))
		}
		if len(argv) > len(args) && slices.Equal(argv[len(argv)-len(args):], args) {
			return pid
		}
	}
	return 0
}

// vmHWM is the high-water mark of pid's resident memory, in bytes, as
// /proc/<pid>/status states it.
func vmHWM(pid int) (int64, bool) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		rest, ok := bytes.CutPrefix(lines.Bytes(), []byte("VmHWM:"))
		if !ok {
			continue
		}
		kib, err := strconv.ParseInt(string(bytes.TrimSuffix(bytes.TrimSpace(rest), []byte(" kB"))), 10, 64)
		if err != nil {
			return 0, false
		}
		return kib << 10, true
	}
	return 0, false
}
