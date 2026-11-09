package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/ssh/sshdtest"
)

// Every case here runs against a real sshd in a container, set up only by the
// rendered setup. The cases that prove the setup itself run on every image.

// rig is one server, set up as one host, with the authority it trusts and a
// client that reaches it.
type rig struct {
	srv    *sshdtest.Server
	host   config.SSHHost
	ca     *setup.SSHCertificateAuthority
	client *Client
}

func mintAuthority(t *testing.T) *setup.SSHCertificateAuthority {
	t.Helper()
	held, err := setup.GenerateSSHCertificateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// dialTo reaches addr whatever address the client asks for, as the host's
// registered address would.
func dialTo(addr string) dialer {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

// newRig starts a server on image and, unless bare, runs the rendered setup
// for host id, with mutate applied to the host first.
func newRig(t *testing.T, image, id string, bare bool, mutate func(*config.SSHHost)) rig {
	t.Helper()
	srv := sshdtest.Start(t, image)
	host := config.SSHHost{ID: id, Address: id + ".example.com", HostKeys: srv.HostKeys(),
		Users: config.SSHHostUsers{Read: "crewlet-read", Operate: "crewlet-ops"}}
	if mutate != nil {
		mutate(&host)
	}
	ca := mintAuthority(t)
	r := rig{srv: srv, host: host, ca: ca, client: &Client{timeout: time.Minute, maxOutput: 64 << 10, dial: dialTo(srv.Addr)}}
	if !bare {
		r.setup(t)
	}
	return r
}

func (r rig) setup(t *testing.T) string {
	t.Helper()
	script, err := RenderSetup(r.host, []string{r.ca.PublicLine()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.srv.RunSetup(script)
	if err != nil {
		t.Fatalf("the setup failed on %s: %v\n%s", r.srv.Image, err, out)
	}
	return out
}

func (r rig) read(t *testing.T) Issued {
	t.Helper()
	return r.sign(t, Request{Purpose: PurposeRead, Seat: "sre", Turn: "turn-1", Hosts: []string{r.host.ID},
		Level: config.SSHLevelRead, Now: time.Now()})
}

func (r rig) operate(t *testing.T, command string) Issued {
	t.Helper()
	return r.sign(t, Request{Purpose: PurposeOperate, Seat: "sre", Turn: "turn-1", HeldCall: "call-1",
		Hosts: []string{r.host.ID}, Level: config.SSHLevelOperate, ForceCommand: OperateCommand([]byte(command)),
		Now: time.Now()})
}

func (r rig) sign(t *testing.T, req Request) Issued {
	t.Helper()
	issued, err := Sign(r.ca, req)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func (r rig) run(t *testing.T, user string, auth ssh.Signer, line string, stdin []byte) Result {
	t.Helper()
	res, err := r.client.Run(t.Context(), r.host, user, auth, Command{Line: line, Stdin: stdin})
	if err != nil {
		t.Fatalf("run %q as %s on %s: %v", line, user, r.srv.Image, err)
	}
	return res
}

// eachImage runs f on every image, in parallel.
func eachImage(t *testing.T, f func(t *testing.T, image string)) {
	for _, image := range sshdtest.Images {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			f(t, image)
		})
	}
}

// THE RENDERED SETUP PASSES SSHD AND IS IDEMPOTENT: on every image it applies,
// sshd accepts the result, and running it again rewrites the same files.
func TestTheRenderedSetupPassesSshdAndIsIdempotent(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", false, nil)
		const state = "find /etc/ssh/crewlet /usr/local/sbin/crewlet-gate /etc/ssh/sshd_config.d/60-crewlet.conf " +
			"/etc/sudoers.d -type f -exec sha256sum {} + | sort; getent passwd crewlet-read crewlet-ops | cut -d: -f1,7"
		before, err := r.srv.Exec(nil, "sh", "-c", state)
		if err != nil {
			t.Fatalf("%v\n%s", err, before)
		}
		if out := r.setup(t); !strings.Contains(out, fmt.Sprintf("Crewlet SSH setup %d applied for api-1.", SetupVersion)) {
			t.Errorf("the second run said %q", out)
		}
		after, _ := r.srv.Exec(nil, "sh", "-c", state)
		if after != before || !strings.Contains(after, "crewlet-read:/bin/sh") {
			t.Errorf("a second run changed the setup:\nbefore %s\nafter %s", before, after)
		}
		if out, err := r.srv.Exec(nil, "sshd", "-t"); err != nil {
			t.Errorf("sshd -t: %v %s", err, out)
		}
	})
}

// THE SETUP CHANGES NOTHING FOR OTHER USERS: sshd's effective configuration for
// root and any other user is the same before the setup and after it.
func TestTheSetupChangesNothingForOtherUsers(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", true, nil)
		effective := func() string {
			var b strings.Builder
			for _, user := range []string{"root", "nobody", "deploy"} {
				out, err := r.srv.Exec(nil, "sshd", "-T", "-C", "user="+user+",host=client.example.com,addr=203.0.113.9")
				if err != nil {
					t.Fatalf("sshd -T for %s: %v\n%s", user, err, out)
				}
				b.WriteString(out)
			}
			return b.String()
		}
		before := effective()
		r.setup(t)
		if after := effective(); after != before {
			t.Error("the setup changed sshd's configuration for a user it does not create")
		}
		crewlet, _ := r.srv.Exec(nil, "sshd", "-T", "-C", "user=crewlet-read,host=client.example.com,addr=203.0.113.9")
		if !strings.Contains(crewlet, "forcecommand /usr/local/sbin/crewlet-gate read") {
			t.Errorf("the read user is not forced through the gate:\n%s", crewlet)
		}
	})
}

// A READ CERTIFICATE RUNS AN ALLOWLISTED COMMAND, and the server logs the
// certificate's key ID and serial beside the sign-in.
func TestAReadCertificateRunsAnAllowlistedCommand(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", false, nil)
		read := r.read(t)
		res := r.run(t, "crewlet-read", read.Signer, "uptime", nil)
		if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "load average") {
			t.Fatalf("uptime: exit %d, %q %q", res.ExitCode, res.Stdout, res.Stderr)
		}
		if res.StdoutBytes != int64(len(res.Stdout)) || res.Truncated || len(res.OutputSHA256) != 64 {
			t.Errorf("result %+v", res)
		}
		want := fmt.Sprintf(`ID crewlet/sre/turn-1 (serial %d)`, read.Certificate.Serial)
		if log := r.srv.Log(); !strings.Contains(log, want) {
			t.Errorf("sshd's log does not name the certificate (%s):\n%s", want, log)
		}
	})
}

// A READ CERTIFICATE CANNOT RUN ANYTHING ELSE: an unlisted command, an
// allowlisted one with an option in a slot that takes a word, and a listed
// binary under another path are each refused by the gate with 126.
func TestAReadCertificateCannotRunAnythingElse(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", false, nil)
		read := r.read(t)
		for _, line := range []string{"id", "cat /etc/shadow", "/usr/bin/systemctl status --all",
			"/tmp/uptime", "systemctl restart ssh", "sh"} {
			if res := r.run(t, "crewlet-read", read.Signer, line, nil); res.ExitCode != 126 {
				t.Errorf("%q ran: exit %d, %q", line, res.ExitCode, res.Stdout)
			}
		}
	})
}

// A READ CERTIFICATE GETS NO SHELL, PTY OR FORWARDING: an interactive session
// is refused by the gate, a PTY by sshd, and a port forward and the agent are
// refused before they reach anything.
func TestAReadCertificateGetsNoShellPtyOrForwarding(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", false, nil)
		client, err := r.client.connect(t.Context(), r.host, "crewlet-read", r.read(t).Signer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close() }()
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		var errOut strings.Builder
		session.Stderr = &errOut
		if err := session.Shell(); err == nil {
			var exit *ssh.ExitError
			if werr := session.Wait(); !errors.As(werr, &exit) || exit.ExitStatus() != 126 {
				t.Errorf("an interactive shell ended %v: %s", werr, errOut.String())
			}
		}
		pty, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := pty.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
			t.Error("sshd granted a PTY")
		}
		if conn, err := client.Dial("tcp", "127.0.0.1:22"); err == nil {
			_ = conn.Close()
			t.Error("sshd forwarded a port")
		}
		agent, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := agent.SendRequest("auth-agent-req@openssh.com", true, nil); ok {
			t.Error("sshd accepted agent forwarding")
		}
	})
}

// AN OPERATE CERTIFICATE RUNS ONLY ITS FORCED COMMAND: the client asks for `id`
// and the server runs the approved command, with the approved stdin, as the
// operate user.
func TestAnOperateCertificateRunsOnlyItsForcedCommand(t *testing.T) {
	t.Parallel()
	eachImage(t, func(t *testing.T, image string) {
		r := newRig(t, image, "api-1", false, nil)
		res := r.run(t, "crewlet-ops", r.operate(t, "echo approved; id -un; cat").Signer, "id", []byte("from stdin"))
		if res.ExitCode != 0 || string(res.Stdout) != "approved\ncrewlet-ops\nfrom stdin" {
			t.Errorf("the server ran %q (exit %d, %q)", res.Stdout, res.ExitCode, res.Stderr)
		}
		if res := r.run(t, "crewlet-ops", r.operate(t, "exit 7").Signer, "true", nil); res.ExitCode != 7 {
			t.Errorf("the approved command's exit status came back as %d", res.ExitCode)
		}
	})
}

// A CERTIFICATE FOR ONE HOST IS REFUSED BY ANOTHER: its principal names its
// host, which the other server's principals file does not.
func TestACertificateForOneHostIsRefusedByAnother(t *testing.T) {
	t.Parallel()
	api := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	db := newRig(t, sshdtest.Images[1], "db-2", true, nil)
	db.ca = api.ca
	db.setup(t)
	if _, err := db.client.Run(t.Context(), db.host, "crewlet-read", api.read(t).Signer, Command{Line: "uptime"}); err == nil {
		t.Error("db-2 accepted a certificate for api-1")
	}
	if res := db.run(t, "crewlet-read", db.read(t).Signer, "uptime", nil); res.ExitCode != 0 {
		t.Errorf("db-2 refused its own certificate: exit %d", res.ExitCode)
	}
}

// AN EXPIRED CERTIFICATE, A PLAIN KEY AND THE WRONG PRINCIPAL ARE REFUSED: each
// fails to sign in, while the control certificate signs in.
func TestAnExpiredCertificateIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	expired := r.sign(t, Request{Purpose: PurposeRead, Seat: "sre", Turn: "turn-1", Hosts: []string{"api-1"},
		Level: config.SSHLevelRead, Now: time.Now().Add(-time.Hour)})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		user string
		auth ssh.Signer
	}{
		"an expired certificate":             {"crewlet-read", expired.Signer},
		"a plain key":                        {"crewlet-read", plain},
		"a read certificate as operate":      {"crewlet-ops", r.read(t).Signer},
		"an operate certificate as the read": {"crewlet-read", r.operate(t, "id").Signer},
		"a read certificate as root":         {"root", r.read(t).Signer},
	} {
		if _, err := r.client.Run(t.Context(), r.host, tc.user, tc.auth, Command{Line: "uptime"}); err == nil {
			t.Errorf("%s signed in", name)
		}
	}
	if res := r.run(t, "crewlet-read", r.read(t).Signer, "uptime", nil); res.ExitCode != 0 {
		t.Errorf("the control certificate failed: exit %d", res.ExitCode)
	}
}

// AN UNPINNED HOST KEY IS REFUSED BEFORE AUTHENTICATION: a host whose pinned
// key is not the server's is never handed a certificate.
func TestAnUnpinnedHostKeyIsRefusedBeforeAuthentication(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	other := newRig(t, sshdtest.Images[1], "api-1", true, nil)
	r.host.HostKeys = other.host.HostKeys
	_, err := r.client.Run(t.Context(), r.host, "crewlet-read", r.read(t).Signer, Command{Line: "uptime"})
	if !errors.Is(err, ErrHostKey) {
		t.Fatalf("an unpinned host key: %v", err)
	}
	if log := r.srv.Log(); strings.Contains(log, "crewlet/sre") || strings.Contains(log, "Failed publickey") {
		t.Errorf("the server saw a sign-in:\n%s", log)
	}
}

// fakeRecorder records the certificates a test signs.
type fakeRecorder struct {
	mu   sync.Mutex
	recs []CertificateRecord
	fail error
}

func (f *fakeRecorder) RecordCertificate(_ context.Context, rec CertificateRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.recs = append(f.recs, rec)
	return nil
}

// TEST CONNECTION REPORTS EACH STEP: a server set up passes all six, each test
// certificate recorded before use; one not set up passes the reach and host
// key steps and stops at the read sign-in; and one whose certificates cannot be
// recorded is never handed one.
func TestTestConnectionReportsEachStep(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	rec := &fakeRecorder{}
	steps := r.client.TestConnection(t.Context(), TestInput{Host: r.host, CA: r.ca, Person: "ops-lead", Now: time.Now()}, rec)
	ids := []string{StepReach, StepHostKey, StepReadAuth, StepGate, StepAllowlist, StepOperate}
	if len(steps) != len(ids) {
		t.Fatalf("steps %+v", steps)
	}
	for i, s := range steps {
		if s.ID != ids[i] || !s.OK {
			t.Errorf("step %d = %+v, want %s ok", i, s, ids[i])
		}
	}
	if len(rec.recs) != 2 || rec.recs[0].Purpose != PurposeTest || !strings.HasPrefix(rec.recs[0].KeyID, "crewlet/test/ops-lead/") {
		t.Errorf("recorded %+v", rec.recs)
	}
	bare := newRig(t, sshdtest.Images[1], "api-1", true, nil)
	steps = bare.client.TestConnection(t.Context(), TestInput{Host: bare.host, CA: bare.ca, Person: "ops-lead", Now: time.Now()}, &fakeRecorder{})
	if len(steps) != 3 || !steps[0].OK || !steps[1].OK || steps[2].OK || steps[2].ID != StepReadAuth {
		t.Errorf("a server not set up: %+v", steps)
	}
	steps = r.client.TestConnection(t.Context(), TestInput{Host: r.host, CA: r.ca, Person: "ops-lead", Now: time.Now()},
		&fakeRecorder{fail: errors.New("the audit is down")})
	if len(steps) != 2 || steps[1].OK || !strings.Contains(steps[1].Detail, "could not be recorded") {
		t.Errorf("an unrecorded certificate: %+v", steps)
	}
}

// THE GATE REFUSES SHELL CHARACTERS AND OPTION INJECTION: substitutions,
// separators, pipes, quotes and redirections are refused before any rule is
// read, an option where a rule takes a word is refused, and an option value a
// rule allows runs.
func TestTheGateRefusesShellCharactersAndOptionInjection(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	read := r.read(t)
	for _, line := range []string{"uptime; id", "uptime && id", "uptime | sh", "echo $(id)", "echo `id`",
		"uptime > /tmp/x", "systemctl status 'ssh'", "journalctl --no-pager -u ssh -n --output=json",
		"systemctl status -H attacker.example.com", "journalctl --no-pager -u ssh -n 5 extra"} {
		if res := r.run(t, "crewlet-read", read.Signer, line, nil); res.ExitCode != 126 {
			t.Errorf("%q: exit %d, %q", line, res.ExitCode, res.Stdout)
		}
	}
	if res := r.run(t, "crewlet-read", read.Signer, "journalctl --no-pager -u ssh --since=-1h", nil); res.ExitCode == 126 {
		t.Errorf("an allowed option value was refused: %q", res.Stderr)
	}
}

// THE GATE TAKES A PROGRAM'S BARE NAME AFTER SUDO -N ON A REAL SERVER: with the
// container reads on, `sudo -n docker ps` and `sudo -n docker logs --tail 50
// <container>` pass the gate (sudo then answers that this image has no docker,
// which is not the gate's refusal), and a path in the program's place, an
// option the rule does not name and another docker verb are refused with 126.
func TestTheGateTakesAProgramsBareNameAfterSudoOnARealServer(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, func(h *config.SSHHost) { h.Containers = true })
	read := r.read(t)
	for _, line := range []string{"sudo -n docker ps", "sudo -n docker logs --tail 50 api-backend-production",
		"/usr/bin/sudo -n /usr/bin/docker ps"} {
		if res := r.run(t, "crewlet-read", read.Signer, line, nil); res.ExitCode == GateRefused ||
			strings.Contains(string(res.Stderr), "crewlet-gate: refused") {
			t.Errorf("%q was refused by the gate: exit %d, %q", line, res.ExitCode, res.Stderr)
		}
	}
	for _, line := range []string{"sudo -n ./docker ps", "sudo -n /tmp/docker ps", "sudo -n docker ps -a",
		"sudo -n docker exec api-backend-production sh"} {
		if res := r.run(t, "crewlet-read", read.Signer, line, nil); res.ExitCode != GateRefused {
			t.Errorf("%q: exit %d, %q; want the gate's refusal", line, res.ExitCode, res.Stderr)
		}
	}
}

// THE SCAN READS EACH HOST KEY THE SERVER HOLDS, each a line a person can pin,
// and never signs in.
func TestTheScanReadsEachHostKey(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", true, nil)
	keys, err := scan(t.Context(), dialTo(r.srv.Addr), "api-1.example.com", 22)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range keys {
		got[k.Line] = true
		if k.Problem != "" && k.Type != ssh.KeyAlgoRSA {
			t.Errorf("%s cannot be pinned: %s", k.Type, k.Problem)
		}
	}
	for _, line := range r.srv.HostKeys() {
		if !got[line] {
			t.Errorf("the scan missed %s; read %+v", line, keys)
		}
	}
	if log := r.srv.Log(); strings.Contains(log, "Accepted") || strings.Contains(log, "Failed") {
		t.Errorf("the scan tried to sign in:\n%s", log)
	}
}
