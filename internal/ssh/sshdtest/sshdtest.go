// Package sshdtest runs a real OpenSSH server in a container for the SSH
// integration's tests: a stock distribution image with openssh-server, sudo
// and nothing of Crewlet's, which a test then sets up with the rendered server
// setup alone, exactly as a person would. Its host keys are made when the
// container starts and are thrown away with it; no test reaches any other
// server.
//
// It needs a usable docker or podman. Where neither answers, [Start] skips the
// test, and internal/skipgate declares each skip as an environment one: CI's
// runners have docker, so the suites run there.
package sshdtest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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
const dockerfile = `FROM %s
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      openssh-server openssh-client sudo procps passwd bsdutils >/dev/null && rm -rf /var/lib/apt/lists/* && \
    rm -f /etc/ssh/ssh_host_* && mkdir -p /run/sshd && \
    printf '%%s\n' '#!/bin/sh' '[ "$1" = reload ] || exit 1' \
      'old=$(cat /run/sshd.pid)' 'kill "$old"' 'while kill -0 "$old" 2>/dev/null; do sleep 0.1; done' \
      'until ssh-keyscan -T 2 127.0.0.1 >/dev/null 2>&1; do sleep 0.1; done' >/usr/local/bin/systemctl && \
    chmod 0755 /usr/local/bin/systemctl
CMD ["sh", "-c", "ssh-keygen -A >/dev/null && while :; do /usr/sbin/sshd -D -e; done"]
`

// images built in this process, by base.
var (
	built   sync.Mutex
	builtAt = map[string]string{}
)

// image is base's test image, built once a process.
func image(t *testing.T, base string) string {
	t.Helper()
	built.Lock()
	defer built.Unlock()
	if tag, ok := builtAt[base]; ok {
		return tag
	}
	tag := "crewlet-sshdtest:" + strings.NewReplacer(":", "-", "/", "-").Replace(base)
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeName(), "build", "-q", "-t", tag, "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(dockerfile, base))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sshdtest: build %s: %v\n%s", tag, err, out)
	}
	builtAt[base] = tag
	return tag
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

// Start runs a fresh server on base and removes it when t ends. It skips t
// where no container runtime answers.
func Start(t *testing.T, base string) *Server {
	t.Helper()
	if runtimeName() == "" {
		t.Skip("sshdtest: needs a usable docker or podman")
	}
	tag := image(t, base)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, runtimeName(), "run", "-d", "--rm", "-p", "127.0.0.1::22", tag).Output()
	if err != nil {
		t.Fatalf("sshdtest: run %s: %v", tag, err)
	}
	s := &Server{t: t, container: strings.TrimSpace(string(out)), Image: base}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		_ = exec.CommandContext(ctx, runtimeName(), "rm", "-f", s.container).Run()
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
