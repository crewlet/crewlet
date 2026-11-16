package main

// # The CPU each package used
//
// A -timings file names each package's CPU beside its wall time, because the
// parallel half's runners are CPU-bound and internal/solo/partition deals that
// half by CPU: a package's wall time there grows with whatever shared the
// machine with it. go test reports no package's CPU, only its own process
// tree's, so the gate measures it where it can be read exactly — at each test
// binary, through go test's own -exec.
//
// With -timings, the gate hands the `go test` it runs `-exec '<itself> exec'`,
// and go test then starts every package's test binary as `skipgate exec
// <binary> <args…>`, in the package's source directory. That mode runs the
// binary on the same standard streams, hands it the signals go test sends
// (SIGQUIT among them, which a test binary answers with every goroutine's stack
// when go test gives up on it), and once it exits appends one line — the
// directory and the CPU the binary and every process it waited for used
// (os.ProcessState's UserTime and SystemTime: the git and the brokers a case
// starts are inside its package's figure) — to the file the gate named in
// [cpuFileVar]. It exits as the binary did.
//
// The gate reads that file once go test has exited and names each directory by
// its import path, under the module the gate runs at the root of
// ([importPathOf]).
//
// An estimate came first, and was wrong in the way that mattered: the run's CPU
// shared out by each package's share of its tests' busy time charged a test
// that sat waiting on git or a timer for CPU it never used, so the CPU-heavy
// packages read light, and five shards dealt equal estimates used 1,117 to
// 2,484 CPU-seconds, the one holding internal/engine ending the run.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuFileVar names the file the gate's exec mode appends each test binary's
// CPU to, set on the go test the gate runs and inherited by every binary go
// test starts through -exec.
const cpuFileVar = "CREWLET_SKIPGATE_CPU"

// execMode is the first argument that makes this program go test's -exec
// wrapper rather than the gate.
const execMode = "exec"

// runExec is `skipgate exec <binary> <args…>`: run the test binary, hand it
// go test's signals, record its CPU, and answer its exit status.
func runExec(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "skipgate exec: no test binary to run")
		return 2
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // go test hands the wrapper its own test binary
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "skipgate exec: start %s: %v\n", argv[0], err)
		return 127
	}
	exited := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-signals:
				_ = cmd.Process.Signal(s)
			case <-exited:
				return
			}
		}
	}()
	waitErr := cmd.Wait()
	close(exited)

	ps := cmd.ProcessState
	if ps == nil {
		fmt.Fprintf(os.Stderr, "skipgate exec: %s: %v\n", argv[0], waitErr)
		return 1
	}
	if err := recordCPU(os.Getenv(cpuFileVar), ps.UserTime()+ps.SystemTime()); err != nil {
		fmt.Fprintf(os.Stderr, "skipgate exec: record the CPU %s used: %v\n", argv[0], err)
	}
	if code := ps.ExitCode(); code >= 0 {
		return code
	}
	// Ended by a signal: a failure go test reports as the binary's own.
	return 1
}

// recordCPU appends the working directory and used to the file named path, as
// one write, so the lines of binaries go test runs at once never interleave.
// No file named is a go test the gate is not measuring: nothing is recorded.
func recordCPU(path string, used time.Duration) error {
	if path == "" {
		return nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(dir + "\t" + strconv.FormatFloat(used.Seconds(), 'f', 3, 64) + "\n")
	return errors.Join(werr, f.Close())
}

// withExec is argv, a `go test` command, handed this program as its -exec
// wrapper: `-exec '<self> exec'`, right after `test`. A command that is not a
// go test, or that names its own -exec, is refused rather than measured some
// other way: the CPU a -timings file names is the wrapper's to read.
func withExec(argv []string, self string) ([]string, error) {
	if len(argv) < 2 || argv[1] != "test" || filepath.Base(argv[0]) != "go" {
		return nil, fmt.Errorf("-timings measures each package's CPU through go test's -exec, and %q is no "+
			"`go test` command", strings.Join(argv, " "))
	}
	for _, a := range argv[2:] {
		if a == "-exec" || a == "--exec" || strings.HasPrefix(a, "-exec=") || strings.HasPrefix(a, "--exec=") {
			return nil, errors.New("-timings measures each package's CPU through go test's -exec, " +
				"and the command names an -exec of its own")
		}
	}
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv[0], argv[1], "-exec", "'"+strings.ReplaceAll(self, "'", `'\''`)+"' "+execMode)
	return append(out, argv[2:]...), nil
}

// readCPU reads the lines the exec mode appended — a directory and seconds —
// into each package's CPU by import path, a package's lines summed (a binary
// run more than once is one package's CPU more than once). A line it cannot
// read is refused, since only the exec mode writes the file.
func readCPU(path, root, module string) (map[string]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		dir, secs, ok := strings.Cut(sc.Text(), "\t")
		cpu, err := strconv.ParseFloat(secs, 64)
		if !ok || err != nil || cpu < 0 {
			return nil, fmt.Errorf("%s:%d: %q is not a directory and its CPU", path, line, sc.Text())
		}
		pkg, err := importPathOf(dir, root, module)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out[pkg] += cpu
	}
	return out, sc.Err()
}

// importPathOf is the import path of the package in dir, under the module
// module rooted at root.
func importPathOf(dir, root, module string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the module at %s", dir, root)
	}
	if rel == "." {
		return module, nil
	}
	return module + "/" + filepath.ToSlash(rel), nil
}

// moduleAt is the module path go.mod in root declares: the gate runs at the
// module's root, as the Makefile runs it, to name a directory by its package.
func moduleAt(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("-timings names each package by its import path, read from go.mod "+
			"where the gate runs (run it at the module's root): %w", err)
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("%s declares no module", filepath.Join(root, "go.mod"))
}
