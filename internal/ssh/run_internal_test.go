package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/ssh/sshdtest"
)

// audit is a recorder of certificates and commands, as the access log is
// handed them.
type audit struct {
	mu      sync.Mutex
	certs   []CertificateRecord
	cmds    []accessdomain.NewCommand
	minted  []time.Time
	certErr error
}

func (a *audit) RecordCertificate(_ context.Context, rec CertificateRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.certErr != nil {
		return a.certErr
	}
	a.certs = append(a.certs, rec)
	return nil
}

func (a *audit) RecordCommand(_ context.Context, minted time.Time, cmd accessdomain.NewCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cmds, a.minted = append(a.cmds, cmd), append(a.minted, minted)
	return nil
}

// runner is a runner reaching the rig's server, recording to a.
func (r rig) runner(a *audit) *Runner {
	run := NewRunner(a, a)
	run.dial = dialTo(r.srv.Addr)
	return run
}

func (r rig) readCall(command string) ReadCall {
	return ReadCall{Use: Use{CA: r.ca}, Seat: "sre", Turn: "turn-1", Host: r.host, Hosts: []string{r.host.ID},
		Command: command}
}

func (r rig) operateCall(command string, stdin []byte) OperateCall {
	return OperateCall{Use: Use{CA: r.ca}, Seat: "sre", Turn: "turn-1", HeldCall: uuid.Must(uuid.NewV7()).String(),
		Approver: "ops-lead", Host: r.host, Command: command, Stdin: stdin}
}

// EVERY CERTIFICATE AND COMMAND IS RECORDED. Two reads in one turn run on one
// read certificate, recorded once before its first use; a read the server's
// gate refuses is recorded as run, with its exit status; an approved operate
// command runs on a certificate signed for its held call alone, recorded
// first, and its record names the approver, the stdin's digest and that
// certificate; a denial is recorded under the held call's own id and time, so
// a denial delivered twice is one record. Nothing records an output's text,
// and the server logs the key ID each certificate carries.
//
// Mutation: sign a read certificate per call, or record a command before
// its certificate, and a case is red.
func TestEveryCertificateAndCommandIsRecorded(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "api-1", false, nil)
	a := &audit{}
	run := r.runner(a)
	for _, line := range []string{"uptime", "/bin/df -h"} {
		if res, err := run.Read(t.Context(), r.readCall(line)); err != nil || res.ExitCode != 0 {
			t.Fatalf("read %q: exit %d, %v", line, res.ExitCode, err)
		}
	}
	if res, err := run.Read(t.Context(), r.readCall("cat /etc/shadow")); err != nil || res.ExitCode != gateRefused {
		t.Fatalf("an unlisted read exited %d, %v, want the gate's %d", res.ExitCode, err, gateRefused)
	}
	if len(a.certs) != 1 || a.certs[0].Purpose != PurposeRead || a.certs[0].Turn != "turn-1" {
		t.Fatalf("three reads in one turn recorded certificates %+v, want one read certificate", a.certs)
	}
	for i, cmd := range a.cmds {
		if cmd.Level != accessdomain.LevelRead || !cmd.Ran || cmd.CertificateSerial != a.certs[0].Serial ||
			cmd.Decision != accessdomain.DecisionNone || cmd.Approver != "" || cmd.OutputSHA256 == "" {
			t.Errorf("read %d is recorded as %+v", i, cmd)
		}
	}

	stdin := []byte("id -un\n")
	op := r.operateCall("sh -s", stdin)
	res, err := run.Operate(t.Context(), op)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "crewlet-ops" {
		t.Fatalf("the approved command printed %q, %v", res.Stdout, err)
	}
	if len(a.certs) != 2 || a.certs[1].Purpose != PurposeOperate || a.certs[1].HeldCall != op.HeldCall {
		t.Fatalf("the operate certificate is recorded as %+v", a.certs)
	}
	sum := sha256.Sum256(stdin)
	got := a.cmds[3]
	if got.Level != accessdomain.LevelOperate || got.Decision != accessdomain.DecisionApproved ||
		got.Approver != "ops-lead" || got.HeldCall != op.HeldCall || got.StdinBytes != int64(len(stdin)) ||
		got.StdinSHA256 != hex.EncodeToString(sum[:]) || got.CertificateSerial != a.certs[1].Serial ||
		got.User != "crewlet-ops" || !got.Ran || got.OutputSHA256 != res.OutputSHA256 {
		t.Fatalf("the operate command is recorded as %+v", got)
	}

	denied := r.operateCall("reboot", nil)
	for range 2 {
		if err := run.Denied(t.Context(), denied); err != nil {
			t.Fatal(err)
		}
	}
	first, second := a.cmds[4], a.cmds[5]
	if first.Decision != accessdomain.DecisionDenied || first.Ran || first.ID != second.ID ||
		!a.minted[4].Equal(a.minted[5]) || len(a.certs) != 2 {
		t.Fatalf("a denial delivered twice is recorded as %+v and %+v, minted %v", first, second, a.minted[4:])
	}
	held, _ := uuid.Parse(denied.HeldCall)
	if sec, nsec := held.Time().UnixTime(); !a.minted[4].Equal(time.Unix(sec, nsec).UTC()) {
		t.Errorf("a denial is minted at %v, want its held call's own time", a.minted[4])
	}
	for _, rec := range a.certs {
		if !strings.Contains(r.srv.Log(), rec.KeyID) {
			t.Errorf("the server did not log the key ID %q", rec.KeyID)
		}
	}
}

// A COMMAND THAT DID NOT RUN IS RECORDED BY WHAT STOPPED IT, in the audit's
// closed words: a server nobody reaches, a host key that is not pinned, a
// server that refuses the certificate, a certificate whose record did not
// land (which no server ever sees), and a command stopped at the company's
// timeout, which ran.
func TestACommandThatDidNotRunIsRecordedByWhatStoppedIt(t *testing.T) {
	t.Parallel()
	set := newRig(t, sshdtest.Images[0], "api-1", false, nil)
	bare := newRig(t, sshdtest.Images[0], "api-1", true, nil)
	other := newRig(t, sshdtest.Images[0], "api-1", true, nil)
	wrongKey := set
	wrongKey.host.HostKeys = other.srv.HostKeys()
	for _, c := range []struct {
		name    string
		rig     rig
		dial    dialer
		certErr error
		want    accessdomain.CommandFailure
	}{
		{name: "nobody answers", rig: set, dial: dialTo("127.0.0.1:1"), want: accessdomain.FailureUnreachable},
		{name: "an unpinned host key", rig: wrongKey, want: accessdomain.FailureHostKeyRefused},
		{name: "a server that trusts no authority", rig: bare, want: accessdomain.FailureAuthenticationRefused},
		{name: "a certificate whose record did not land", rig: set, certErr: errors.New("log down"),
			want: accessdomain.FailureEngineError},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &audit{certErr: c.certErr}
			run := c.rig.runner(a)
			if c.dial != nil {
				run.dial = c.dial
			}
			_, err := run.Read(t.Context(), c.rig.readCall("uptime"))
			if FailureOf(err) != c.want {
				t.Fatalf("the read failed as %q (%v), want %q", FailureOf(err), err, c.want)
			}
			if len(a.cmds) != 1 || a.cmds[0].Ran || a.cmds[0].Failure != c.want {
				t.Fatalf("the command is recorded as %+v, want not run, %q", a.cmds, c.want)
			}
		})
	}
	a := &audit{}
	op := set.operateCall("sleep 30", nil)
	op.Limits = &config.SSHCommands{TimeoutSeconds: 1}
	res, err := set.runner(a).Operate(t.Context(), op)
	if err != nil || !res.TimedOut || a.cmds[0].Failure != accessdomain.FailureTimedOut || !a.cmds[0].Ran {
		t.Fatalf("a command past the timeout answered %+v, %v and is recorded as %+v", res, err, a.cmds)
	}
}
