package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// Test connection: each thing the server setup puts in place, proven against
// the server with certificates signed for the press alone, in the order a
// person fixes them. A step that fails stops the steps that depend on it.

// The steps, by id, in order.
const (
	StepReach     = "reach"
	StepHostKey   = "host_key"
	StepReadAuth  = "read_auth"
	StepGate      = "gate_version"
	StepAllowlist = "allowlist"
	StepOperate   = "operate_binding"
)

// TestStep is one step's result.
type TestStep struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Recorder keeps the audit's record of every certificate before it is used.
type Recorder interface {
	RecordCertificate(ctx context.Context, rec CertificateRecord) error
}

// TestInput is one press of Test connection.
type TestInput struct {
	Host config.SSHHost
	// CA signs the press's certificates: the current authority, or the
	// staged one a rotation tests.
	CA              *setup.SSHCertificateAuthority
	Person          string
	SourceAddresses []string
	Now             time.Time
}

// unlistedProbe is a command no allowlist names, which the gate must refuse.
const unlistedProbe = "/usr/local/sbin/crewlet-gate-unlisted-probe"

// GateRefused is the exit status the gate refuses a command with.
const GateRefused = 126

// TestConnection runs the steps against in.Host, recording each certificate
// it signs before using it.
func (c *Client) TestConnection(ctx context.Context, in TestInput, rec Recorder) []TestStep {
	var steps []TestStep
	add := func(id string, ok bool, detail string) bool {
		steps = append(steps, TestStep{ID: id, OK: ok, Detail: detail})
		return ok
	}
	h := in.Host
	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.PortOrDefault()))
	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	conn, err := c.dial(dialCtx, "tcp", addr)
	cancel()
	if !add(StepReach, err == nil, reachDetail(addr, err)) {
		return steps
	}
	_ = conn.Close()
	read, err := c.testCertificate(ctx, in, config.SSHLevelRead, "", rec)
	if err != nil {
		add(StepHostKey, false, "not tried: "+err.Error())
		return steps
	}
	client, err := c.connect(ctx, h, h.Users.Read, read.Signer)
	switch {
	case errors.Is(err, ErrHostKey):
		add(StepHostKey, false, "The server presented a host key that is not pinned for "+h.ID+
			". Compare its fingerprint on the server's console, then update the host's keys.")
		return steps
	case err != nil:
		add(StepHostKey, true, "The server presented a pinned host key.")
		add(StepReadAuth, false, "The server refused the read certificate as "+h.Users.Read+
			": run the server setup again, so it trusts this authority and the principal "+
			Principal(h.ID, config.SSHLevelRead)+".")
		return steps
	}
	defer func() { _ = client.Close() }()
	add(StepHostKey, true, "The server presented a pinned host key.")
	add(StepReadAuth, true, "The server accepted the read certificate as "+h.Users.Read+".")
	out, code, err := c.runOn(ctx, client, "crewlet-gate version")
	if !add(StepGate, err == nil && code == 0 && strings.TrimSpace(out) == GateVersionLine(),
		gateDetail(out, code, err)) {
		return steps
	}
	_, code, err = c.runOn(ctx, client, unlistedProbe)
	if !add(StepAllowlist, err == nil && code == GateRefused, allowlistDetail(code, err)) {
		return steps
	}
	if h.Users.Operate == "" {
		return steps
	}
	op, err := c.testCertificate(ctx, in, config.SSHLevelOperate, GateVersionCommand, rec)
	if err != nil {
		add(StepOperate, false, "not tried: "+err.Error())
		return steps
	}
	opClient, err := c.connect(ctx, h, h.Users.Operate, op.Signer)
	if err != nil {
		add(StepOperate, false, "The server refused the operate certificate as "+h.Users.Operate+
			": run the server setup again, so it accepts the principal "+Principal(h.ID, config.SSHLevelOperate)+".")
		return steps
	}
	defer func() { _ = opClient.Close() }()
	// THE CLIENT ASKS FOR `id`; a server that honours the certificate runs
	// its force-command instead, and answers the gate's version.
	out, code, err = c.runOn(ctx, opClient, "id")
	add(StepOperate, err == nil && code == 0 && strings.TrimSpace(out) == GateVersionLine(), operateDetail(out, err))
	return steps
}

// testCertificate signs and records one test certificate.
func (c *Client) testCertificate(ctx context.Context, in TestInput, level, force string, rec Recorder) (Issued, error) {
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails
	issued, err := Sign(in.CA, Request{
		Purpose: PurposeTest, Person: in.Person, Nonce: hex.EncodeToString(nonce),
		Hosts: []string{in.Host.ID}, Level: level, ForceCommand: force,
		SourceAddresses: in.SourceAddresses, Now: in.Now,
	})
	if err != nil {
		return Issued{}, err
	}
	if err := rec.RecordCertificate(ctx, issued.Record); err != nil {
		return Issued{}, fmt.Errorf("the certificate could not be recorded, so it was not used: %w", err)
	}
	return issued, nil
}

// runOn runs one command in a session of its own, with no stdin, its output
// bounded to what a step reads. A step that outlasts its limit or its request
// closes the connection, so a server that never answers holds nothing open.
func (c *Client) runOn(ctx context.Context, client *ssh.Client, line string) (string, int, error) {
	out := newCapped(4 << 10)
	done := make(chan error, 1)
	go func() {
		session, err := client.NewSession()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = session.Close() }()
		session.Stdout, session.Stderr = out, out
		done <- session.Run(line)
	}()
	timer := time.NewTimer(c.stepLimit())
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		_ = client.Close()
		return out.text(), 0, errors.Join(fmt.Errorf("ssh: the server did not finish %q within %s", line,
			c.stepLimit()), <-done)
	case <-ctx.Done():
		_ = client.Close()
		return out.text(), 0, errors.Join(ctx.Err(), <-done)
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return out.text(), exit.ExitStatus(), nil
	}
	return out.text(), 0, err
}

func reachDetail(addr string, err error) string {
	if err != nil {
		return "Could not reach " + addr + " within " + DialTimeout.String() + ": " + err.Error() +
			". Let the engine's address through the server's firewall."
	}
	return "Reached " + addr + "."
}

func gateDetail(out string, code int, err error) string {
	switch {
	case err != nil:
		return "The read user's session failed: " + err.Error()
	case code == 0 && strings.TrimSpace(out) == GateVersionLine():
		return "The gate answers " + GateVersionLine() + ", the version this engine renders."
	}
	return fmt.Sprintf("The gate did not answer %s (exit %d): run the server setup again.", GateVersionLine(), code)
}

func allowlistDetail(code int, err error) string {
	switch {
	case err != nil:
		return "The read user's session failed: " + err.Error()
	case code == GateRefused:
		return "A command off the allowlist was refused."
	}
	return fmt.Sprintf("A command off the allowlist was not refused (exit %d): the read user's commands are "+
		"not forced through the gate. Run the server setup again.", code)
}

func operateDetail(out string, err error) string {
	switch {
	case err != nil:
		return "The operate user's session failed: " + err.Error()
	case strings.TrimSpace(out) == GateVersionLine():
		return "The server ran the certificate's command, not the one the client asked for."
	}
	return "The server ran the client's command instead of the certificate's: do not grant operate on this " +
		"server until its setup is run again."
}
