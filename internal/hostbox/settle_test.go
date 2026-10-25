package hostbox_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// probeScript answers the probe argument by exiting at once, the shape every
// caller of Settle writes, so settling it never runs a body.
const probeScript = "#!/bin/sh\n[ \"${1-}\" = --probe ] && exit 0\nexit 0\n"

// A SCRIPT WRITTEN WHILE OTHER GOROUTINES FORK EXECS ONCE IT HAS SETTLED.
//
// The window is every fork that happened while os.WriteFile held the file
// open: each such child holds the write descriptor until its own exec closes
// it. Eight goroutines forking /bin/true in a loop keep that window open as
// often as a busy suite does (17 failures in 200 execs without the wait), and
// the exec AFTER Settle is the assertion: Settle's promise is that once one
// exec gets through, every later one does.
func TestAFreshlyWrittenScriptSettles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for range 8 {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command("/bin/true").Run()
			}
		}()
	}
	defer func() {
		close(stop)
		forkers.Wait()
	}()

	for i := range 200 {
		path := filepath.Join(dir, fmt.Sprintf("script-%d", i))
		if err := os.WriteFile(path, []byte(probeScript), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := hostbox.Settle(t.Context(), path, "--probe"); err != nil {
			t.Fatalf("settling script %d: %v", i, err)
		}
		if err := exec.Command(path).Run(); err != nil {
			t.Fatalf("script %d settled and then failed to exec: %v", i, err)
		}
	}
}

// A FILE THAT CAN NEVER BE EXEC'D IS REFUSED, never reported settled: only an
// exec that STARTED got through, so a hook written without its exec bit, at
// a wrong path or with no interpreter line fails here, naming the file, and
// not at the first push as somebody else's error.
func TestAFileThatCannotBeExecutedIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		write func(path string) error
		want  error
	}{
		"no exec bit": {
			write: func(path string) error { return os.WriteFile(path, []byte(probeScript), 0o600) },
			want:  fs.ErrPermission,
		},
		"no file": {
			write: func(string) error { return nil },
			want:  fs.ErrNotExist,
		},
		"no interpreter line": {
			write: func(path string) error { return os.WriteFile(path, []byte("\x7fELF but not really\n"), 0o700) },
			want:  syscall.ENOEXEC,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := tc.write(path); err != nil {
				t.Fatal(err)
			}
			err := hostbox.Settle(t.Context(), path, "--probe")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("Settle = %v, want an error naming %s and wrapping %v", err, path, tc.want)
			}
		})
	}
}

// THE PROBE SEES ONLY THE PASSTHROUGH ALLOWLIST, never the engine's own
// environment — which holds the org's chat token, the store's DSN and the
// API keys — because a script settled with no probe of its own runs its body.
func TestTheProbeSeesOnlyThePassthroughEnvironment(t *testing.T) {
	t.Setenv("CREWLET_SECRET_X", "sk-ant-not-for-a-probe")
	dir := t.TempDir()
	path, out := filepath.Join(dir, "script"), filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nenv > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hostbox.Settle(t.Context(), path, out); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, name := range hostbox.PassthroughEnv {
		allowed[name] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		name, _, _ := strings.Cut(line, "=")
		switch name {
		case "PWD", "OLDPWD", "SHLVL", "_": // what the shell itself sets
			continue
		}
		if !allowed[name] {
			t.Errorf("the probe saw %s, which is not on the passthrough allowlist", name)
		}
	}
	if path := os.Getenv("PATH"); path != "" && !strings.Contains(string(body), "PATH="+path+"\n") {
		t.Errorf("the probe did not see the engine's PATH:\n%s", body)
	}
}

// A FILE SOMETHING REALLY HOLDS OPEN FOR WRITING IS REPORTED, never waited on
// for ever: the context's error when it ends first, ErrStillBusy once every
// attempt has failed.
func TestAScriptHeldOpenForWritingIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(path, []byte(probeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := hostbox.Settle(ctx, path, "--probe"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settle under a one-second context = %v, want the context's deadline", err)
	}

	previous := hostbox.SetSettleAttempts(3)
	t.Cleanup(func() { hostbox.SetSettleAttempts(previous) })
	if err := hostbox.Settle(t.Context(), path, "--probe"); !errors.Is(err, hostbox.ErrStillBusy) {
		t.Fatalf("Settle with every attempt refused = %v, want ErrStillBusy", err)
	}
}
