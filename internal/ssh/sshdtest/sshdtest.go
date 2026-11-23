// Package sshdtest runs a real OpenSSH server in a container for the SSH
// integration's tests: a stock distribution image with openssh-server, sudo
// and nothing of Crewlet's, which a test then sets up with the rendered server
// setup alone, exactly as a person would. Its host keys are made when the
// container starts and are thrown away with it; no test reaches any other
// server.
//
// It needs a usable docker or podman. Where neither answers, [Require] skips
// the test, and internal/skipgate declares each skip as an environment one:
// CI's runners have docker, so the suites run there. skipgate matches a
// declared skip by the test's exact name, so a case that runs one server per
// image calls [Require] itself, before its per-image subtests: a skip taken
// inside one of them is a skip of that subtest, which nothing declares, and a
// workstation without a runtime would fail the gate on every image of every
// such case. [Start] calls it too, for a case that starts its one server
// directly.
package sshdtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bounds on the runtime's commands: an image build installs packages over
// the network, and every other command answers in seconds.
const (
	buildTimeout   = 10 * time.Minute
	commandTimeout = time.Minute
)

// Images are the distributions the setup is proven on, by their base image.
var Images = []string{"ubuntu:22.04", "ubuntu:24.04", "debian:12"}

// runtimeName is the container runtime found on PATH, "" for none.
var runtimeName = sync.OnceValue(func() string {
	for _, name := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := exec.CommandContext(ctx, name, "info").Run()
		cancel()
		if err == nil {
			return name
		}
	}
	return ""
})

// dockerfile builds base with sshd ready to start. The package's install
// makes host keys, which would be the same in every container, so they are
// removed and each container makes its own. A container runs no init, so its
// first process keeps sshd running, and a systemctl stands in that restarts it
// and returns once the new one answers, which the setup's own `systemctl
// reload ssh` then calls as it would on a server.
//
// APT RETRIES EACH FETCH FIVE TIMES (aptRetries), set before its first fetch so
// update and install both take it. apt waits 1 s before the first retry and
// doubles the wait, so a fetch rides out about 31 s of a mirror resetting or
// answering 5xx (issue #849), while a mirror that stays down still fails the
// build, its own messages kept in the build's output ([build]).
const dockerfile = `FROM %s
RUN printf 'Acquire::Retries "%d";\n' >/etc/apt/apt.conf.d/80crewlet-retries && \
    apt-get -q update && DEBIAN_FRONTEND=noninteractive apt-get -q install -y --no-install-recommends \
      openssh-server openssh-client sudo procps passwd bsdutils && rm -rf /var/lib/apt/lists/* && \
    rm -f /etc/ssh/ssh_host_* && mkdir -p /run/sshd && \
    printf '%%s\n' '#!/bin/sh' '[ "$1" = reload ] || exit 1' \
      'old=$(cat /run/sshd.pid)' 'kill "$old"' 'while kill -0 "$old" 2>/dev/null; do sleep 0.1; done' \
      'until ssh-keyscan -T 2 127.0.0.1 >/dev/null 2>&1; do sleep 0.1; done' >/usr/local/bin/systemctl && \
    chmod 0755 /usr/local/bin/systemctl
CMD ["sh", "-c", "ssh-keygen -A >/dev/null && while :; do /usr/sbin/sshd -D -e; done"]
`

// aptRetries is how many times apt fetches a file again before it fails the
// build ([dockerfile] says why five).
const aptRetries = 5

// CacheEnv names a directory that keeps the test images between runs, a tar
// each: CI restores it before the suite and saves it after (ci.yml's `test`
// job says why images rather than apt's archives). Unset, as on a workstation,
// an image the runtime does not hold is built.
const CacheEnv = "CREWLET_SSHDTEST_CACHE"

// completeFile, in the cache directory, says it holds a tar of every image of
// [Images] as this tree defines them: what ci.yml saves the directory on.
const completeFile = "complete"

// images provided in this process, by base.
var (
	built   sync.Mutex
	builtAt = map[string]string{}
)

// image is base's test image, provided once a process ([provide]).
func image(t *testing.T, base string) string {
	t.Helper()
	built.Lock()
	defer built.Unlock()
	if tag, ok := builtAt[base]; ok {
		return tag
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	tag, err := provide(ctx, runtimeOf(runtimeName()), base, os.Getenv(CacheEnv), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	builtAt[base] = tag
	return tag
}

// runtime is the container runtime by its name, and how one of its commands
// runs: stdin from in where non-nil, answering the combined output.
type runtime struct {
	name string
	run  func(ctx context.Context, in io.Reader, args ...string) ([]byte, error)
}

func runtimeOf(name string) runtime {
	return runtime{name: name, run: func(ctx context.Context, in io.Reader, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		if in != nil {
			cmd.Stdin = in
		}
		return cmd.CombinedOutput()
	}}
}

// textOf is the Dockerfile that defines base's image.
func textOf(base string) string { return fmt.Sprintf(dockerfile, base, aptRetries) }

// tagOf is base's image tag, named by a hash of the text that defines it, so
// an image the runtime or the cache holds under it was built from exactly that
// text, and a changed Dockerfile builds afresh.
func tagOf(base, text string) string {
	sum := sha256.Sum256([]byte(text))
	return "crewlet-sshdtest:" + strings.NewReplacer(":", "-", "/", "-").Replace(base) + "-" +
		hex.EncodeToString(sum[:6])
}

// tarOf is where the cache directory dir keeps tag.
func tarOf(dir, tag string) string {
	return filepath.Join(dir, strings.TrimPrefix(tag, "crewlet-sshdtest:")+".tar")
}

// provide answers base's image, cheapest first: the one the runtime holds
// under its tag; else the one cacheDir keeps, loaded; else one built, then
// saved to cacheDir. With no cacheDir an image is held or built, as before
// there was a cache. The cache is only a shortcut: a tar that will not load is
// built over, and a save that fails is said through logf, never a failure.
func provide(ctx context.Context, rt runtime, base, cacheDir string,
	logf func(string, ...any)) (string, error) {

	text := textOf(base)
	tag := tagOf(base, text)
	if _, err := rt.run(ctx, nil, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	if cacheDir != "" {
		if _, err := os.Stat(tarOf(cacheDir, tag)); err == nil {
			out, err := rt.run(ctx, nil, "load", "-i", tarOf(cacheDir, tag))
			if err == nil {
				return tag, nil
			}
			logf("sshdtest: the cached %s did not load, so it is built: %v\n%s", tag, err, out)
		}
	}
	if err := build(ctx, rt, tag, text); err != nil {
		return "", err
	}
	if cacheDir != "" {
		if err := save(ctx, rt, cacheDir, tag); err != nil {
			logf("%v", err)
		}
	}
	return tag, nil
}

// save writes tag to the cache directory dir, whole or not at all, and marks
// the directory complete once it holds every image of [Images]. A save that
// fails leaves nothing of itself, and its error names [CacheEnv], which chose
// the directory.
func save(ctx context.Context, rt runtime, dir, tag string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sshdtest: the image cache %s (%s): %w", dir, CacheEnv, err)
	}
	path := tarOf(dir, tag)
	next := path + ".next"
	defer func() { _ = os.Remove(next) }()
	if out, err := rt.run(ctx, nil, "save", "-o", next, tag); err != nil {
		return fmt.Errorf("sshdtest: save %s to %s (%s): %w\n%s", tag, dir, CacheEnv, err, out)
	}
	if err := os.Rename(next, path); err != nil {
		return fmt.Errorf("sshdtest: save %s to %s (%s): %w", tag, dir, CacheEnv, err)
	}
	if !keepsEvery(dir) {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, completeFile), nil, 0o644)
}

// keepsEvery reports whether the cache directory dir holds a tar of every
// image of [Images] as this tree defines them.
func keepsEvery(dir string) bool {
	for _, base := range Images {
		if _, err := os.Stat(tarOf(dir, tagOf(base, textOf(base)))); err != nil {
			return false
		}
	}
	return true
}

// build builds the Dockerfile text as tag, and answers an error that carries
// the build's whole output: a failed step's own messages, apt's among them,
// so a mirror that could not be reached reads differently from a package that
// is gone.
//
// NEVER QUIET: docker's -q leaves only "exit code: 100" of a failed step.
// docker's BuildKit is asked for plain progress, which prints every step's
// output whether or not a terminal is attached; podman prints it unasked, and
// takes no such flag.
func build(ctx context.Context, rt runtime, tag, text string) error {
	args := []string{"build", "-t", tag}
	if rt.name == "docker" {
		args = append(args, "--progress=plain")
	}
	if out, err := rt.run(ctx, strings.NewReader(text), append(args, "-")...); err != nil {
		return fmt.Errorf("sshdtest: build %s: %w\n%s", tag, err, out)
	}
	return nil
}

// Server is one running sshd.
type Server struct {
	t         *testing.T
	container string
	// Addr is where the server listens, on this machine's loopback.
	Addr string
	// Image is the base image it runs.
	Image string
}

// Require skips t where no container runtime answers. A case that runs a
// server per image calls it before its subtests, so the skip is of the case
// internal/skipgate declares (see the package doc).
func Require(t *testing.T) {
	t.Helper()
	if runtimeName() == "" {
		t.Skip("sshdtest: needs a usable docker or podman")
	}
}

// Start runs a fresh server on base and removes it when t ends. It skips t
// where no container runtime answers ([Require]).
func Start(t *testing.T, base string) *Server {
	t.Helper()
	Require(t)
	tag := image(t, base)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, runtimeName(), "run", "-d", "--rm", "-p", "127.0.0.1::22", tag).Output()
	if err != nil {
		t.Fatalf("sshdtest: run %s: %v", tag, err)
	}
	s := &Server{t: t, container: strings.TrimSpace(string(out)), Image: base}
	t.Cleanup(func() {
		done, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = exec.CommandContext(done, runtimeName(), "rm", "-f", s.container).Run()
	})
	port, err := exec.CommandContext(ctx, runtimeName(), "port", s.container, "22/tcp").Output()
	if err != nil {
		t.Fatalf("sshdtest: the port of %s: %v", s.container, err)
	}
	s.Addr = strings.TrimSpace(strings.Split(string(port), "\n")[0])
	s.waitForSSHD()
	return s
}

// waitForSSHD waits until sshd has written its host keys and listens: the
// container's own shell polls, bounded by the context.
func (s *Server) waitForSSHD() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.exec(ctx, nil, "sh", "-c",
		"until test -s /run/sshd.pid && ssh-keyscan -T 2 127.0.0.1 >/dev/null 2>&1; do sleep 0.2; done")
	if err != nil {
		s.t.Fatalf("sshdtest: sshd did not start in %s: %v\n%s", s.container, err, out)
	}
}

// Exec runs argv in the container as root, stdin from in, and answers its
// combined output and error.
func (s *Server) Exec(in []byte, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.exec(ctx, in, argv...)
}

func (s *Server) exec(ctx context.Context, in []byte, argv ...string) (string, error) {
	args := append([]string{"exec", "-i", s.container}, argv...)
	cmd := exec.CommandContext(ctx, runtimeName(), args...)
	if in != nil {
		cmd.Stdin = bytes.NewReader(in)
	} else {
		cmd.Stdin = strings.NewReader("")
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HostKeys is the server's public host key lines, as a person reads them off
// its console.
func (s *Server) HostKeys() []string {
	s.t.Helper()
	out, err := s.Exec(nil, "sh", "-c", "cat /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ecdsa_key.pub")
	if err != nil {
		s.t.Fatalf("sshdtest: read host keys: %v\n%s", err, out)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			lines = append(lines, f[0]+" "+f[1])
		}
	}
	return lines
}

// RunSetup runs script as root through sh -s, as a person pastes it.
func (s *Server) RunSetup(script string) (string, error) {
	return s.Exec([]byte(script), "sh", "-s")
}

// Log is sshd's own output so far, where it logs each key ID and serial.
func (s *Server) Log() string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, runtimeName(), "logs", s.container).CombinedOutput()
	return string(out)
}
