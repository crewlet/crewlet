package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
)

// silentServer is an in-process SSH server that signs anybody in, accepts every
// session and request, and never sends an exit status: a hung gate.
func silentServer(t *testing.T) (string, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return &ssh.Permissions{}, nil
	}}
	cfg.AddHostKey(signer)
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					_, creqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for r := range creqs {
							if r.WantReply {
								_ = r.Reply(true, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

type nopRecorder struct{}

func (nopRecorder) RecordCertificate(context.Context, CertificateRecord) error { return nil }

// A TEST CONNECTION STEP THAT NEVER ENDS IS CLOSED AT ITS LIMIT, OR WHEN ITS
// REQUEST ENDS: a server that accepts the session and never answers holds
// neither the request nor the connection.
//
// Mutation: run a step without its timer, and the case waits forever.
func TestATestStepThatNeverEndsIsClosedAtItsLimit(t *testing.T) {
	t.Parallel()
	addr, hostKey := silentServer(t)
	host := config.SSHHost{ID: "api-1", Address: "api-1.example.com", HostKeys: []string{hostKey},
		Users: config.SSHHostUsers{Read: "crewlet-read"}}
	in := TestInput{Host: host, CA: mintAuthority(t), Person: "ops-lead", Now: time.Now()}
	for name, c := range map[string]struct {
		step    time.Duration
		ctxWait time.Duration
	}{
		"its own limit":     {step: 300 * time.Millisecond, ctxWait: time.Minute},
		"the request's end": {step: time.Minute, ctxWait: 300 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &Client{timeout: time.Minute, maxOutput: 64 << 10, dial: dialTo(addr), step: c.step}
			ctx, cancel := context.WithTimeout(t.Context(), c.ctxWait)
			defer cancel()
			// RETURNING AT ALL IS THE CLAIM: a step left unbounded never does.
			steps := client.TestConnection(ctx, in, nopRecorder{})
			last := steps[len(steps)-1]
			if last.ID != StepGate || last.OK {
				t.Fatalf("the steps are %+v, want the gate step failed", steps)
			}
		})
	}
}

// A SCAN STOPS AT THE FIRST ADDRESS THAT DOES NOT ANSWER: every other
// algorithm would wait as long again.
func TestAScanStopsWhereNobodyAnswers(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	refused := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("connection refused")
	}
	if _, err := scan(t.Context(), refused, "api-1.example.com", 22); !errors.Is(err, ErrNoHostKey) {
		t.Fatalf("the scan answered %v", err)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("the scan dialled %d times, want once", n)
	}
}

// THE GATE LOGS ONE LINE PER COMMAND: an operate command as its base64, and any
// other text with its line breaks flattened, so no command writes a log line
// of its own.
func TestTheGateLogsOneLinePerCommand(t *testing.T) {
	t.Parallel()
	g := newLoggedGate(t)
	payload := base64.StdEncoding.EncodeToString([]byte("echo a\necho b"))
	g.run(t, nil, 0, "operate", payload)
	g.run(t, []string{"SSH_ORIGINAL_COMMAND=uptime\nreboot"}, GateRefused, "read")
	records := g.records(t)
	if len(records) != 2 || !strings.Contains(records[0], "cmd_b64="+payload) {
		t.Fatalf("the gate logged %q", records)
	}
	for _, r := range records {
		if strings.ContainsAny(r, "\r\n") {
			t.Errorf("a log line carries a line break: %q", r)
		}
	}
}

// A PROGRAM A CASE WRITES RUNS THE MOMENT writeExecutable HANDS IT OVER, with
// goroutines of this process forking beside it: a fork made while the file's
// descriptor is open hands its child a copy, and Linux refuses to exec a file
// held open for writing (ETXTBSY). The gate's say swallows that refusal from
// its logger, so a gate case saw it only as a record missing.
//
// Mutation: take the fork lock out of writeExecutable and programs are refused.
func TestAProgramACaseWritesRunsTheMomentItIsWritten(t *testing.T) {
	t.Parallel()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for range 8 {
		forkers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = exec.Command("/bin/true").Run() //nolint:noctx // a fork, not a subject
				}
			}
		})
	}
	t.Cleanup(func() { close(stop); forkers.Wait() })

	var probes sync.WaitGroup
	refused := make(chan error, 64)
	for range 64 {
		probes.Go(func() {
			path := filepath.Join(t.TempDir(), "logger")
			writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
			if err := exec.Command(path).Run(); err != nil { //nolint:noctx // the probe itself
				refused <- err
			}
		})
	}
	probes.Wait()
	close(refused)
	if n := len(refused); n > 0 {
		t.Fatalf("%d of 64 programs could not be run the moment they were written (%v); a "+
			"gate case reads each as a command its gate never logged", n, <-refused)
	}
}

// loggedGate is the gate in a directory of its own, with a logger on its PATH
// that appends each record it is handed to a file, ended by a record separator.
type loggedGate struct{ dir, gate, logged string }

func newLoggedGate(t *testing.T) loggedGate {
	t.Helper()
	dir := t.TempDir()
	g := loggedGate{dir: dir, gate: filepath.Join(dir, "gate.sh"), logged: filepath.Join(dir, "logged")}
	if err := os.WriteFile(g.gate, []byte(gateScript), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dir, "logger"),
		"#!/bin/sh\nfor a; do last=$a; done\nprintf '%s\\036' \"$last\" >> \"$LOGGED\"\n")
	return g
}

// run runs the gate with args through /bin/sh, as sshd's forced command does,
// and fails the case on any exit but want, naming what the gate said.
func (g loggedGate) run(t *testing.T, env []string, want int, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{g.gate}, args...)...)
	cmd.Env = append([]string{"PATH=" + g.dir + ":/usr/bin:/bin", "LOGGED=" + g.logged}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("the gate %q could not run: %v", args, err)
	}
	if code != want {
		t.Fatalf("the gate %q exited %d, want %d: %s", args, code, want, out)
	}
}

// records is every record the gate's logger was handed, in order.
func (g loggedGate) records(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(g.logged)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x1e"), "\x1e")
}

// writeExecutable writes a program a case is about to exec, where no process
// this one forks can still hold it open: a child forked while os.WriteFile's
// descriptor is open inherits it until its own exec, and Linux refuses
// (ETXTBSY) to exec a file held open for writing. A fork takes
// syscall.ForkLock for writing, so holding it for reading keeps every fork out
// of that window. Every program this package's cases exec goes through it.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own program, to be run
		t.Fatal(err)
	}
}

// THE GATE TAKES A PROGRAM'S BARE NAME AFTER SUDO -N, AND NOWHERE ELSE: a rule
// running /usr/bin/docker through `sudo -n` admits `sudo -n docker ps`, which
// sudo resolves through its secure_path and holds to the sudoers line naming
// /usr/bin/docker; a path in that place is matched whole, so `./docker` and
// `/tmp/docker` are refused; every other word, a path argument included, is
// matched exactly. Run against the gate's own text, its allowlist moved and its
// exec said rather than made, so nothing runs on this machine.
//
// Mutation: match the program's base name for any word after sudo, and
// `sudo -n /tmp/docker ps` runs.
func TestTheGateTakesAProgramsBareNameAfterSudo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	allowlist := filepath.Join(dir, "read-allowlist")
	rules := "/usr/bin/uptime\n/usr/bin/sudo -n /usr/bin/docker ps\n/usr/bin/sudo -n /usr/bin/docker logs --tail * *\n" +
		"/usr/bin/tail -n * /var/log/syslog\n"
	gate := strings.Replace(gateScript, "ALLOWLIST=/etc/ssh/crewlet/read-allowlist", "ALLOWLIST="+allowlist, 1)
	gate = strings.Replace(gate, `exec "$bin" "$@"`, `echo "ran $bin $*"; exit 0`, 1)
	if !strings.Contains(gate, "ALLOWLIST="+allowlist) || !strings.Contains(gate, "ran $bin") {
		t.Fatal("the gate's allowlist or exec line moved; this case no longer reads it")
	}
	script := filepath.Join(dir, "gate.sh")
	for path, body := range map[string]string{allowlist: rules, script: gate} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(dir, "logger"), "#!/bin/sh\nexit 0\n")
	read := func(line string) (string, int) {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", script, "read")
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "SSH_ORIGINAL_COMMAND=" + line}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		return string(out), 0
	}
	for line, ran := range map[string]string{
		"sudo -n docker ps":                         "ran /usr/bin/sudo -n docker ps",
		"sudo -n docker logs --tail 50 api-backend": "ran /usr/bin/sudo -n docker logs --tail 50 api-backend",
		"/usr/bin/sudo -n /usr/bin/docker ps":       "ran /usr/bin/sudo -n /usr/bin/docker ps",
		"uptime":                                    "ran /usr/bin/uptime",
		"tail -n 20 /var/log/syslog":                "ran /usr/bin/tail -n 20 /var/log/syslog",
	} {
		if out, code := read(line); code != 0 || strings.TrimSpace(out) != ran {
			t.Errorf("%q: exit %d, %q; want %q", line, code, out, ran)
		}
	}
	for _, line := range []string{"sudo -n ./docker ps", "sudo -n /tmp/docker ps", "sudo -n docker ps -a",
		"sudo -n docker exec api sh", "sudo docker ps", "sudo -n bin/docker ps", "tail -n 20 syslog"} {
		if out, code := read(line); code != GateRefused {
			t.Errorf("%q: exit %d, %q; want refused", line, code, out)
		}
	}
}
