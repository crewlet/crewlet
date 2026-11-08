package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// Agents' commands. A read runs on the seat's read certificate for its turn,
// signed once and reused while it is valid; an operate command runs on a
// certificate signed for that one approved call, whose force-command is the
// approved command. Every certificate is recorded before it is presented, and
// every command once it ran, failed or was denied.

// CommandRecorder records each command an agent's call ran or a person denied,
// under an operation minted at minted: the same call recorded twice is one
// record.
type CommandRecorder interface {
	RecordCommand(ctx context.Context, minted time.Time, cmd accessdomain.NewCommand) error
}

// ErrUnrecorded is a command that ran but whose record did not land: its
// result is still the call's, since the server already ran it.
var ErrUnrecorded = errors.New("ssh: the command ran, and its record did not land")

// Use is what every command is signed and bounded by.
type Use struct {
	CA              *setup.SSHCertificateAuthority
	Limits          *config.SSHCommands
	SourceAddresses []string
}

// ReadCall is one read: Command on Host as its read user.
type ReadCall struct {
	Use
	Seat, Turn string
	Host       config.SSHHost
	// Hosts is every host the seat reads, its read certificate's principals.
	Hosts   []string
	Command string
}

// OperateCall is one approved operate command: Command, with Stdin, on Host as
// its operate user, by the held call a person answered.
type OperateCall struct {
	Use
	Seat, Turn, HeldCall, Approver string
	Host                           config.SSHHost
	Command                        string
	Stdin                          []byte
}

// readReuse is how long before its end a read certificate is signed again:
// one only needs to be valid when the server checks it, at sign-in.
const readReuse = 2 * time.Minute

// readKey is one turn's read certificate: who, which turn, under which
// authority, reaching which hosts from where.
type readKey struct{ seat, turn, ca, reach string }

// Runner runs agents' commands. Its one state is the read certificates it
// signed, in memory until they end.
type Runner struct {
	certs    Recorder
	commands CommandRecorder
	dial     dialer
	now      func() time.Time

	mu    sync.Mutex
	reads map[readKey]Issued
}

// NewRunner is a runner that records certificates to certs and commands to
// commands.
func NewRunner(certs Recorder, commands CommandRecorder) *Runner {
	return &Runner{certs: certs, commands: commands, dial: guardedDial, now: time.Now,
		reads: map[readKey]Issued{}}
}

func (r *Runner) client(limits *config.SSHCommands) *Client {
	return &Client{timeout: limits.Timeout(), maxOutput: limits.MaxOutput(), dial: r.dial}
}

// Read runs call, on the seat's read certificate for its turn.
func (r *Runner) Read(ctx context.Context, call ReadCall) (Result, error) {
	cmd := accessdomain.NewCommand{
		ID: uuid.Must(uuid.NewV7()).String(), Handle: call.Seat, Turn: call.Turn,
		Host: call.Host.ID, User: call.Host.Users.Read, Level: accessdomain.LevelRead, Command: call.Command,
	}
	minted := r.now().UTC()
	issued, err := r.readCertificate(ctx, call)
	if err != nil {
		return Result{}, r.unrun(ctx, minted, cmd, err)
	}
	cmd.CertificateSerial = issued.Record.Serial
	return r.run(ctx, minted, cmd, call.Use, call.Host, call.Host.Users.Read, issued, Command{Line: call.Command})
}

// readCertificate is the seat's read certificate for its turn: the one signed
// earlier while it stays valid, otherwise a new one, recorded first.
func (r *Runner) readCertificate(ctx context.Context, call ReadCall) (Issued, error) {
	hosts := hostIDs(call.Hosts)
	if !slices.Contains(hosts, call.Host.ID) {
		return Issued{}, fmt.Errorf("%w: the seat does not read %s", ErrRequest, call.Host.ID)
	}
	if call.CA == nil {
		return Issued{}, fmt.Errorf("%w: no authority", ErrRequest)
	}
	key := readKey{seat: call.Seat, turn: call.Turn, ca: call.CA.Fingerprint(),
		reach: strings.Join(hosts, ",") + "|" + strings.Join(call.SourceAddresses, ",")}
	now := r.now()
	r.mu.Lock()
	issued, ok := r.reads[key]
	r.mu.Unlock()
	if ok && now.Before(issued.Record.ValidBefore.Add(-readReuse)) {
		return issued, nil
	}
	issued, err := Sign(call.CA, Request{Purpose: PurposeRead, Seat: call.Seat, Turn: call.Turn,
		Hosts: hosts, Level: config.SSHLevelRead, SourceAddresses: call.SourceAddresses, Now: now})
	if err != nil {
		return Issued{}, err
	}
	if err := r.certs.RecordCertificate(ctx, issued.Record); err != nil {
		return Issued{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.reads {
		if !now.Before(v.Record.ValidBefore) {
			delete(r.reads, k)
		}
	}
	r.reads[key] = issued
	return issued, nil
}

// Operate runs an approved operate command on a certificate signed for it
// alone, recorded first.
func (r *Runner) Operate(ctx context.Context, call OperateCall) (Result, error) {
	minted, cmd := r.operateRecord(call)
	cmd.Decision = accessdomain.DecisionApproved
	if call.Host.Users.Operate == "" || call.CA == nil {
		return Result{}, r.unrun(ctx, minted, cmd,
			fmt.Errorf("%w: %s has no operate user or no authority signs", ErrRequest, call.Host.ID))
	}
	issued, err := Sign(call.CA, Request{Purpose: PurposeOperate, Seat: call.Seat, Turn: call.Turn,
		HeldCall: call.HeldCall, Hosts: []string{call.Host.ID}, Level: config.SSHLevelOperate,
		ForceCommand: OperateCommand([]byte(call.Command)), SourceAddresses: call.SourceAddresses, Now: r.now()})
	if err == nil {
		err = r.certs.RecordCertificate(ctx, issued.Record)
	}
	if err != nil {
		return Result{}, r.unrun(ctx, minted, cmd, err)
	}
	cmd.CertificateSerial = issued.Record.Serial
	return r.run(ctx, minted, cmd, call.Use, call.Host, call.Host.Users.Operate, issued,
		Command{Line: call.Command, Stdin: call.Stdin})
}

// Denied records that a person denied call.
func (r *Runner) Denied(ctx context.Context, call OperateCall) error {
	minted, cmd := r.operateRecord(call)
	cmd.Decision = accessdomain.DecisionDenied
	return r.commands.RecordCommand(ctx, minted, cmd)
}

// operateRecord is call's command record before it is decided. Its id is the
// held call's own, derived, so an answer delivered twice records once.
func (r *Runner) operateRecord(call OperateCall) (time.Time, accessdomain.NewCommand) {
	minted := r.now().UTC()
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("crewlet.ssh.operate/"+call.HeldCall))
	if held, err := uuid.Parse(call.HeldCall); err == nil && held.Version() == 7 {
		sec, nsec := held.Time().UnixTime()
		minted = time.Unix(sec, nsec).UTC()
	}
	cmd := accessdomain.NewCommand{
		ID: id.String(), Handle: call.Seat, Turn: call.Turn, HeldCall: call.HeldCall,
		Host: call.Host.ID, User: call.Host.Users.Operate, Level: accessdomain.LevelOperate,
		Command: call.Command, Approver: call.Approver,
	}
	if len(call.Stdin) > 0 {
		sum := sha256.Sum256(call.Stdin)
		cmd.StdinBytes, cmd.StdinSHA256 = int64(len(call.Stdin)), hex.EncodeToString(sum[:])
	}
	return minted, cmd
}

// run runs line on h as user and records what it did.
func (r *Runner) run(ctx context.Context, minted time.Time, cmd accessdomain.NewCommand, use Use,
	h config.SSHHost, user string, issued Issued, line Command) (Result, error) {

	res, runErr := r.client(use.Limits).Run(ctx, h, user, issued.Signer, line)
	cmd.Ran, cmd.ExitCode, cmd.Duration = res.Started, res.ExitCode, res.Duration
	cmd.StdoutBytes, cmd.StderrBytes, cmd.Truncated = res.StdoutBytes, res.StderrBytes, res.Truncated
	if res.Started {
		cmd.OutputSHA256 = res.OutputSHA256
	}
	switch {
	case runErr != nil:
		cmd.Failure = FailureOf(runErr)
	case res.TimedOut:
		cmd.Failure = accessdomain.FailureTimedOut
	}
	recErr := r.commands.RecordCommand(ctx, minted, cmd)
	switch {
	case runErr != nil:
		return res, errors.Join(runErr, recErr)
	case recErr != nil:
		return res, fmt.Errorf("%w: %w", ErrUnrecorded, recErr)
	}
	return res, nil
}

// unrun records a command that never reached its server, and answers why.
func (r *Runner) unrun(ctx context.Context, minted time.Time, cmd accessdomain.NewCommand, err error) error {
	cmd.Failure = accessdomain.FailureEngineError
	return failed(accessdomain.FailureEngineError, errors.Join(err, r.commands.RecordCommand(ctx, minted, cmd)))
}
