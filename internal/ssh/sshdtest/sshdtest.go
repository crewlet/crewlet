// Package sshdtest runs a real OpenSSH server in a container for the SSH
// integration's tests: a stock distribution image with openssh-server, sudo
// and nothing of Crewlet's, which a test then sets up with the rendered server
// setup alone, exactly as a person would. Its host keys are made when the
// container starts and are thrown away with it; no test reaches any other
// server. Its images are internal/imagetest's, which builds and keeps them.
//
// It needs a usable docker or podman, as imagetest does, and skipgate matches
// a declared skip by the test's exact name. So a case that runs one server per
// image calls [Require] itself, before its per-image subtests: a skip taken
// inside one of them is a skip of that subtest, which nothing declares, and a
// workstation without a runtime would fail the gate on every image of every
// such case. [Start] calls it too, for a case that starts its one server
// directly.
package sshdtest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/imagetest"
)

// commandTimeout bounds each of the runtime's own commands a server takes
// (run, port, rm, logs), every one of which answers in seconds.
const commandTimeout = time.Minute

// Images are the distributions the setup is proven on, by their base image.
var Images = []string{"ubuntu:22.04", "ubuntu:24.04", "debian:12"}

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
// build, its own messages kept in the build's error (internal/imagetest).
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

// imageOf is base's test image, its tag crewlet-sshdtest:<base>-<hash>.
func imageOf(base string) imagetest.Image {
	return imagetest.Image{Repo: "crewlet-sshdtest", Name: base,
		Text: fmt.Sprintf(dockerfile, base, aptRetries), CacheEnv: CacheEnv}
}

// image is base's test image as provide answers it, with the cache then marked
// complete if it keeps every image ([markComplete]). Start hands it
// [imagetest.Provide]; a case hands its own, to watch the mark without a
// runtime. A mark that fails is logged, as a save that fails is.
func image(t *testing.T, base string, provide func(testing.TB, imagetest.Image) string) string {
	t.Helper()
	tag := provide(t, imageOf(base))
	dir := os.Getenv(CacheEnv)
	if err := markComplete(dir); err != nil {
		t.Logf("sshdtest: the image cache %s (%s): %v", dir, CacheEnv, err)
	}
	return tag
}

// markComplete writes completeFile to the cache directory dir once it keeps a
// tar of every image of [Images], and does nothing where dir is "".
func markComplete(dir string) error {
	if dir == "" || !keepsEvery(dir) {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, completeFile), nil, 0o644)
}

// keepsEvery reports whether the cache directory dir keeps a tar of every
// image of [Images] as this tree defines them.
func keepsEvery(dir string) bool {
	for _, base := range Images {
		if _, err := os.Stat(imageOf(base).Tar(dir)); err != nil {
			return false
		}
	}
	return true
}

// Server is one running sshd.
type Server struct {
	t         *testing.T
	runtime   string
	container string
	// Addr is where the server listens, on this machine's loopback.
	Addr string
	// Image is the base image it runs.
	Image string
}

// Require skips t where no container runtime answers ([imagetest.Require]). A
// case that runs a server per image calls it before its subtests, so the skip
// is of the case internal/skipgate declares (see the package doc).
func Require(t *testing.T) {
	t.Helper()
	imagetest.Require(t)
}

// Start runs a fresh server on base and removes it when t ends. It skips t
// where no container runtime answers ([Require]).
func Start(t *testing.T, base string) *Server {
	t.Helper()
	runtime := imagetest.Require(t)
	tag := image(t, base, imagetest.Provide)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, runtime, "run", "-d", "--rm", "-p", "127.0.0.1::22", tag).Output()
	if err != nil {
		t.Fatalf("sshdtest: run %s: %v", tag, err)
	}
	s := &Server{t: t, runtime: runtime, container: strings.TrimSpace(string(out)), Image: base}
	t.Cleanup(func() {
		done, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = exec.CommandContext(done, s.runtime, "rm", "-f", s.container).Run()
	})
	port, err := exec.CommandContext(ctx, s.runtime, "port", s.container, "22/tcp").Output()
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
	cmd := exec.CommandContext(ctx, s.runtime, args...)
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
	out, _ := exec.CommandContext(ctx, s.runtime, "logs", s.container).CombinedOutput()
	return string(out)
}
