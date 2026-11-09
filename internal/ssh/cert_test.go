package ssh_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

func authority(t *testing.T) *setup.SSHCertificateAuthority {
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

// A CERTIFICATE CARRIES THE LOCKED FIELDS AND NOTHING ELSE: a user certificate
// signed by the authority, a non-zero serial, the key ID naming its holder,
// one principal per host at the level, valid from a minute back for its
// purpose's span, no extension at all, a force-command only where it binds
// one, and source-address only where the company sets it.
func TestACertificateCarriesTheLockedFields(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	approved := []byte("systemctl restart api; echo done")
	for _, tc := range []struct {
		name       string
		req        sshint.Request
		keyID      string
		principals []string
		valid      time.Duration
		options    map[string]string
	}{
		{"read", sshint.Request{Purpose: sshint.PurposeRead, Seat: "sre", Turn: "turn-1",
			Hosts: []string{"db-2", "api-1", "api-1"}, Level: config.SSHLevelRead, Now: now},
			"crewlet/sre/turn-1", []string{"crewlet.api-1.read", "crewlet.db-2.read"}, 15 * time.Minute,
			map[string]string{}},
		{"read in a box", sshint.Request{Purpose: sshint.PurposeBoxRead, Seat: "sre", Turn: "turn-1",
			Hosts: []string{"api-1"}, Level: config.SSHLevelRead, Now: now, SourceAddresses: []string{"203.0.113.7/32"}},
			"crewlet/sre/turn-1", []string{"crewlet.api-1.read"}, 30 * time.Minute,
			map[string]string{"source-address": "203.0.113.7/32"}},
		{"operate", sshint.Request{Purpose: sshint.PurposeOperate, Seat: "sre", Turn: "turn-1", HeldCall: "call-9",
			Hosts: []string{"api-1"}, Level: config.SSHLevelOperate, ForceCommand: sshint.OperateCommand(approved),
			SourceAddresses: []string{" 203.0.113.7/32", "2001:db8::/32 "}, Now: now},
			"crewlet/sre/turn-1/call-9", []string{"crewlet.api-1.operate"}, 5 * time.Minute,
			map[string]string{"force-command": "/usr/local/sbin/crewlet-gate operate " +
				base64.StdEncoding.EncodeToString(approved), "source-address": "203.0.113.7/32,2001:db8::/32"}},
		{"test", sshint.Request{Purpose: sshint.PurposeTest, Person: "ops-lead", Nonce: "0011aabb",
			Hosts: []string{"api-1"}, Level: config.SSHLevelOperate, ForceCommand: sshint.GateVersionCommand, Now: now},
			"crewlet/test/ops-lead/0011aabb", []string{"crewlet.api-1.operate"}, 2 * time.Minute,
			map[string]string{"force-command": "/usr/local/sbin/crewlet-gate version"}},
	} {
		issued, err := sshint.Sign(ca, tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c := issued.Certificate
		checker := ssh.CertChecker{IsUserAuthority: func(auth ssh.PublicKey) bool {
			return string(auth.Marshal()) == string(ca.Signer.PublicKey().Marshal())
		}, Clock: func() time.Time { return now }, SupportedCriticalOptions: []string{"force-command", "source-address"}}
		for _, p := range tc.principals {
			if err := checker.CheckCert(p, c); err != nil {
				t.Errorf("%s: the authority does not vouch for %s: %v", tc.name, p, err)
			}
		}
		switch {
		case c.CertType != ssh.UserCert || c.Serial == 0 || c.KeyId != tc.keyID:
			t.Errorf("%s: type %d, serial %d, key ID %q", tc.name, c.CertType, c.Serial, c.KeyId)
		case !slices.Equal(c.ValidPrincipals, tc.principals):
			t.Errorf("%s: principals %v, want %v", tc.name, c.ValidPrincipals, tc.principals)
		case time.Unix(int64(c.ValidAfter), 0).UTC() != now.Add(-time.Minute) ||
			time.Unix(int64(c.ValidBefore), 0).UTC() != now.Add(tc.valid):
			t.Errorf("%s: valid %d to %d", tc.name, c.ValidAfter, c.ValidBefore)
		case len(c.Extensions) != 0:
			t.Errorf("%s: extensions %v, want none", tc.name, c.Extensions)
		case len(c.CriticalOptions) != len(tc.options):
			t.Errorf("%s: options %v, want %v", tc.name, c.CriticalOptions, tc.options)
		}
		for k, v := range tc.options {
			if c.CriticalOptions[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.name, k, c.CriticalOptions[k], v)
			}
		}
		rec := issued.Record
		if rec.Serial != c.Serial || rec.KeyID != c.KeyId || rec.CASHA256 != ca.Fingerprint() ||
			rec.PublicKeySHA256 != ssh.FingerprintSHA256(c.Key) || rec.Purpose != tc.req.Purpose {
			t.Errorf("%s: record %+v", tc.name, rec)
		}
		if issued.Signer.PublicKey().Type() != ssh.CertAlgoED25519v01 {
			t.Errorf("%s: authenticates as %s", tc.name, issued.Signer.PublicKey().Type())
		}
	}
}

// A REQUEST OUTSIDE THE LOCKED SHAPES IS NOT SIGNED: operate on two hosts or
// at read, a read certificate binding a command, an operate one binding
// anything but the gate handed one command, a certificate of any purpose at
// operate without the gate (a test binds only the version), a key ID part
// holding a slash, no host, and a source that is no address range.
func TestARequestOutsideTheLockedShapesIsNotSigned(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	now := time.Now()
	ok := sshint.Request{Purpose: sshint.PurposeOperate, Seat: "sre", Turn: "t", HeldCall: "c",
		Hosts: []string{"api-1"}, Level: config.SSHLevelOperate, ForceCommand: sshint.OperateCommand([]byte("id")), Now: now}
	for name, mutate := range map[string]func(*sshint.Request){
		"operate on two hosts":   func(r *sshint.Request) { r.Hosts = []string{"api-1", "db-2"} },
		"operate at read":        func(r *sshint.Request) { r.Level = config.SSHLevelRead },
		"operate without a gate": func(r *sshint.Request) { r.ForceCommand = "id" },
		"operate with no call":   func(r *sshint.Request) { r.HeldCall = "" },
		"a seat with a slash":    func(r *sshint.Request) { r.Seat = "sre/other" },
		"no host":                func(r *sshint.Request) { r.Hosts = nil },
		"a host that is no id":   func(r *sshint.Request) { r.Hosts = []string{"api.1"} },
		"read binding a command": func(r *sshint.Request) {
			r.Purpose, r.Level, r.HeldCall = sshint.PurposeRead, config.SSHLevelRead, ""
		},
		"a two-line command":            func(r *sshint.Request) { r.ForceCommand += "\nid" },
		"operate with an empty command": func(r *sshint.Request) { r.ForceCommand = sshint.OperateCommand(nil) },
		"operate with a payload that is no base64": func(r *sshint.Request) {
			r.ForceCommand = "/usr/local/sbin/crewlet-gate operate id;id"
		},
		"a test at operate binding nothing": func(r *sshint.Request) {
			r.Purpose, r.Person, r.Nonce, r.Seat, r.Turn, r.HeldCall, r.ForceCommand =
				sshint.PurposeTest, "ops-lead", "01", "", "", "", ""
		},
		"a test at operate binding a command": func(r *sshint.Request) {
			r.Purpose, r.Person, r.Nonce, r.Seat, r.Turn, r.HeldCall =
				sshint.PurposeTest, "ops-lead", "01", "", "", ""
		},
		"a test at read binding a command": func(r *sshint.Request) {
			r.Purpose, r.Person, r.Nonce, r.Seat, r.Turn, r.HeldCall, r.Level, r.ForceCommand =
				sshint.PurposeTest, "ops-lead", "01", "", "", "", config.SSHLevelRead, sshint.GateVersionCommand
		},
		"a source that is no range": func(r *sshint.Request) { r.SourceAddresses = []string{"203.0.113.7"} },
	} {
		req := ok
		mutate(&req)
		if _, err := sshint.Sign(ca, req); !errors.Is(err, sshint.ErrRequest) {
			t.Errorf("%s: %v, want a refused request", name, err)
		}
	}
	if _, err := sshint.Sign(ca, ok); err != nil {
		t.Errorf("the control request was refused: %v", err)
	}
}

// THE SETUP IS RENDERED FROM THE DOCUMENT AND NOTHING ELSE: both authority
// lines during a rotation, the host's principals and users, the container
// reads only where turned on, a host's own allowlist in place of the default,
// no operate user or sudo on a read-only host, and a refusal for any value a
// script run as root must not carry.
func TestTheSetupIsRenderedFromTheHostAlone(t *testing.T) {
	t.Parallel()
	current, next := authority(t).PublicLine(), authority(t).PublicLine()
	host := config.SSHHost{ID: "api-1", Users: config.SSHHostUsers{Read: "crewlet-read", Operate: "crewlet-ops"}}
	script, err := sshint.RenderSetup(host, []string{current, next})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{current + "\n" + next + "\nCREWLET_CA", "HOST_ID=api-1", "OPS_USER=crewlet-ops",
		"Match User crewlet-read,crewlet-ops", "Match User crewlet-read\n    ForceCommand /usr/local/sbin/crewlet-gate read",
		"crewlet-gate 2:", "/usr/bin/journalctl --no-pager -u * --since=*", "rm -f /etc/sudoers.d/crewlet-read"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	if strings.Contains(script, "docker") {
		t.Error("a host without containers is given the container reads")
	}
	again, _ := sshint.RenderSetup(host, []string{current, next})
	if again != script {
		t.Error("two renders of one host differ")
	}
	readOnly := host
	readOnly.Users.Operate, readOnly.Containers = "", true
	ro, err := sshint.RenderSetup(readOnly, []string{current})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ro, "OPS_USER") || strings.Contains(ro, "NOPASSWD: ALL") || !strings.Contains(ro, "rm -f /etc/sudoers.d/crewlet-ops") ||
		!strings.Contains(ro, "/usr/bin/sudo -n /usr/bin/docker logs --tail * *") || !strings.Contains(ro, "Match User crewlet-read\n") {
		t.Errorf("a read-only host with containers renders:\n%s", ro)
	}
	own := host
	own.ReadAllowlist = []string{"/usr/bin/uptime", "/usr/bin/tail -n * /var/log/app.log"}
	custom, err := sshint.RenderSetup(own, []string{current})
	if err != nil || !strings.Contains(custom, "/usr/bin/tail -n * /var/log/app.log\nCREWLET_ALLOW") ||
		strings.Contains(custom, "/usr/bin/free -m") {
		t.Errorf("a host's own allowlist renders %v:\n%s", err, custom)
	}
	for name, mutate := range map[string]func(*config.SSHHost, *[]string){
		"a host id with a space":  func(h *config.SSHHost, _ *[]string) { h.ID = "api 1" },
		"a user with a quote":     func(h *config.SSHHost, _ *[]string) { h.Users.Read = "x'y" },
		"one user for both":       func(h *config.SSHHost, _ *[]string) { h.Users.Operate = h.Users.Read },
		"no authority":            func(_ *config.SSHHost, ca *[]string) { *ca = nil },
		"an authority of 2 lines": func(_ *config.SSHHost, ca *[]string) { *ca = []string{current + "\nCREWLET_CA"} },
		"a rule with a shell":     func(h *config.SSHHost, _ *[]string) { h.ReadAllowlist = []string{"/bin/sh -c id;id"} },
		"a relative rule":         func(h *config.SSHHost, _ *[]string) { h.ReadAllowlist = []string{"uptime"} },
	} {
		h, cas := host, []string{current}
		mutate(&h, &cas)
		if _, err := sshint.RenderSetup(h, cas); !errors.Is(err, sshint.ErrUnrenderable) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}
