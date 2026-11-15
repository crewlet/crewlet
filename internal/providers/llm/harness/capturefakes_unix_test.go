//go:build unix

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// The fake harness's modes for the capture's cases (capture_unix_test.go).
// Every helper a mode starts announces itself through a READY PIPE it is
// handed as its descriptor 3, written once its pid file is down, so the
// harness that started it goes on only once it is there — never after a
// guess at how long that takes.

// fakeWritten is how much [fakeWrite] has written of this process's stdout.
var fakeWritten int

// fakeWrite writes b to stdout, as the harness would — unless FAKE_PIPE_CUT is
// n and stdout is a PIPE: then only the first n bytes of everything it would
// print are written, and the process exits 0 the moment they are, as OpenCode
// 2.0.32 and 2.1.4 were measured doing (Bun's asynchronous writes to a pipe,
// lost at exit). Into a regular file it writes everything.
func fakeWrite(b []byte) {
	n, _ := strconv.Atoi(os.Getenv("FAKE_PIPE_CUT"))
	if info, err := os.Stdout.Stat(); n <= 0 || err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		_, _ = os.Stdout.Write(b)
		return
	}
	b = b[:min(len(b), max(n-fakeWritten, 0))]
	_, _ = os.Stdout.Write(b)
	if fakeWritten += len(b); fakeWritten >= n {
		os.Exit(0)
	}
}

// fakePrint is [fakeWrite] of a string.
func fakePrint(s string) { fakeWrite([]byte(s)) }

// stdioReport is what [fakeReportStdio] records of one invocation.
type stdioReport struct {
	Argv    []string                `json:"argv"`
	Streams map[string]streamReport `json:"streams"`
}

// streamReport is one of an invocation's output descriptors as the child sees
// it.
type streamReport struct {
	Regular bool   `json:"regular"`
	Pipe    bool   `json:"pipe"`
	Links   uint64 `json:"links"`
	Memfd   bool   `json:"memfd"`
	Target  string `json:"target"`
}

// fakeReportStdio writes, into a new file in dir, what this invocation's
// descriptors 1 and 2 are: a regular file or a pipe, how many names the file
// has, and on linux whether it is a file in memory.
func fakeReportStdio(dir string) {
	report := stdioReport{Argv: os.Args[1:], Streams: map[string]streamReport{}}
	for fd, name := range map[int]string{1: "stdout", 2: "stderr"} {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			continue
		}
		target, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		report.Streams[name] = streamReport{
			Regular: st.Mode&unix.S_IFMT == unix.S_IFREG, Pipe: st.Mode&unix.S_IFMT == unix.S_IFIFO,
			Links: uint64(st.Nlink), Memfd: strings.HasPrefix(target, "/memfd:"), Target: target, //nolint:gosec,unconvert // a link count is small, and its type differs by platform
		}
	}
	f, err := os.CreateTemp(dir, "stdio-*.json")
	if err != nil {
		os.Exit(3)
	}
	_ = json.NewEncoder(f).Encode(report)
	_ = f.Close()
}

// startFakeChild re-executes this binary as a helper of the fake harness, with
// env over its own, the given stdout and stderr (nil is the null device), in
// its own session when setsid, and waits for its ready pipe.
func startFakeChild(env []string, stdout, stderr *os.File, setsid bool) {
	ready, signalled, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake harness: no ready pipe:", err)
		os.Exit(1)
	}
	child := exec.Command(os.Args[0], "-test.run=TestFakeHarness") //nolint:gosec // this test binary
	child.Env = append(append(os.Environ(), env...), "FAKE_READY_FD=3")
	child.Stdout, child.Stderr = stdout, stderr
	child.ExtraFiles = []*os.File{signalled}
	if setsid {
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness could not fork:", err)
		os.Exit(1)
	}
	_ = signalled.Close()
	if _, err := ready.Read(make([]byte, 1)); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness: the helper never announced itself:", err)
		os.Exit(1)
	}
	_ = ready.Close()
}

// fakeSignalReady writes this helper's ready byte, when it was handed a ready
// pipe, and lets go of the pipe.
func fakeSignalReady() {
	if os.Getenv("FAKE_READY_FD") == "" {
		return
	}
	ready := os.NewFile(3, "ready")
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
}

// fakeWritePid writes this process's pid to the file the variable name
// names, when it names one.
func fakeWritePid(name string) {
	if path := os.Getenv(name); path != "" {
		_ = os.WriteFile(path, fmt.Appendf(nil, "%d", os.Getpid()), 0o600)
	}
}

// fakeLateGrandchild is a launcher that EXITS BEFORE THE RUNTIME IT STARTED HAS
// WRITTEN THE ANSWER: it prints FAKE_STDOUT, starts a helper in its own group
// with its stdout, and exits 0. The helper ("writer") waits for SIGUSR1, then
// prints FAKE_STDOUT_LATE and exits.
func fakeLateGrandchild() {
	if os.Getenv("FAKE_LATE_GRANDCHILD") == "writer" {
		fakeWritePid("FAKE_PIDFILE")
		usr1 := make(chan os.Signal, 1)
		signal.Notify(usr1, syscall.SIGUSR1)
		fakeSignalReady()
		go func() {
			<-usr1
			fmt.Print(os.Getenv("FAKE_STDOUT_LATE"))
			os.Exit(0)
		}()
		procgrouptest.Hold()
	}
	fakeWritePid("FAKE_HARNESS_PIDFILE")
	fmt.Print(os.Getenv("FAKE_STDOUT"))
	startFakeChild([]string{"FAKE_LATE_GRANDCHILD=writer"}, os.Stdout, nil, false)
	os.Exit(0)
}

// fakeIdleHelper is a harness that leaves a helper in its group holding NONE
// of its streams — its stdin, stdout and stderr the null device — deaf to
// SIGTERM, and answers and exits 0.
func fakeIdleHelper() {
	startFakeChild([]string{"FAKE_IDLE_HELPER=", "FAKE_GRANDCHILD=1"}, nil, nil, false)
	fmt.Print(os.Getenv("FAKE_STDOUT"))
	os.Exit(0)
}

// fakeFlood is a harness that writes FAKE_FLOOD_MIB mebibytes (33 unless set)
// of FAKE_FLOOD_LINE lines to the stream FAKE_FLOOD names, after the line
// FAKE_FLOOD_FIRST on stdout where set, a mebibyte at a write, paced by
// FAKE_FLOOD_PACE_MS where set; then FAKE_FLOOD_AFTER on the same stream.
// With FAKE_FLOOD_HOLD it then holds, beside a SIGTERM-deaf helper in its
// group (FAKE_PIDFILE), and a SIGTERM writes FAKE_TERM_MARKER and eight
// mebibytes more; otherwise it prints FAKE_STDOUT and exits 0.
func fakeFlood() {
	stream := os.Stdout
	if os.Getenv("FAKE_FLOOD") == "stderr" {
		stream = os.Stderr
	}
	chunk := floodChunk(os.Getenv("FAKE_FLOOD_LINE"))
	fakeWritePid("FAKE_HARNESS_PIDFILE")
	hold := os.Getenv("FAKE_FLOOD_HOLD") == "1"
	if hold {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		go func() {
			<-term
			_ = os.WriteFile(os.Getenv("FAKE_TERM_MARKER"), []byte("terminated"), 0o600)
			for range 8 {
				_, _ = stream.Write(chunk)
			}
		}()
		startFakeChild([]string{"FAKE_FLOOD=", "FAKE_GRANDCHILD=1"}, nil, nil, false)
	}
	if first := os.Getenv("FAKE_FLOOD_FIRST"); first != "" {
		fmt.Println(first)
	}
	mib, err := strconv.Atoi(os.Getenv("FAKE_FLOOD_MIB"))
	if err != nil {
		mib = 33
	}
	pace, _ := strconv.Atoi(os.Getenv("FAKE_FLOOD_PACE_MS"))
	for range mib {
		_, _ = stream.Write(chunk)
		if pace > 0 {
			time.Sleep(time.Duration(pace) * time.Millisecond) //fixedwait:simulated a harness writing its record at a rate the watcher's tick sees
		}
	}
	fmt.Fprint(stream, os.Getenv("FAKE_FLOOD_AFTER"))
	if hold {
		procgrouptest.Hold()
	}
	fmt.Print(os.Getenv("FAKE_STDOUT"))
	os.Exit(0)
}

// floodChunk is one mebibyte of whole lines of line — of x where line is
// empty — each ended by a line break.
func floodChunk(line string) []byte {
	if line == "" {
		line = strings.Repeat("x", 1023)
	}
	return []byte(strings.Repeat(line+"\n", (1<<20)/(len(line)+1)))
}

// fakeWriteOnSignal is an escaped holder's writing: on each SIGUSR1 (usr1) it writes
// a mebibyte to its stdout, reads how many bytes the file's blocks then take,
// and writes that into dir as ack-<n>, n counting from one.
func fakeWriteOnSignal(dir string, usr1 <-chan os.Signal) {
	chunk := floodChunk("")
	for n := 1; ; n++ {
		<-usr1
		_, _ = os.Stdout.Write(chunk)
		var st unix.Stat_t
		_ = unix.Fstat(1, &st)
		tmp := filepath.Join(dir, fmt.Sprintf(".ack-%d", n))
		_ = os.WriteFile(tmp, fmt.Appendf(nil, "%d", st.Blocks*512), 0o600)
		_ = os.Rename(tmp, filepath.Join(dir, fmt.Sprintf("ack-%d", n)))
	}
}
