package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/iam"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
)

// sshAuditor is the engine's own name on every SSH record it writes.
var sshAuditor = iam.Actor{Name: "ssh", Kind: iam.ActorSystem}

// errNoAccessLog is a node that neither keeps the access log nor reaches one
// that does, so it signs nothing.
var errNoAccessLog = errors.New("engine: this node reaches no access log, so it signs no SSH certificate")

// sshWriter is the access log's SSH writes as this node reaches them.
type sshWriter interface {
	Issued(ctx context.Context, c accessdomain.Call, n accessdomain.NewCertificate) (statelog.Result, error)
	Ran(ctx context.Context, c accessdomain.Call, n accessdomain.NewCommand) (statelog.Result, error)
	Verdict(ctx context.Context, c accessdomain.Call, n accessdomain.NewVerdict) (statelog.Result, error)
}

// writer is this node's own access log where it keeps one, and otherwise a
// data node's through the router, a command's text and a verdict's words
// sealed here before they leave: a seat's executor, and so its SSH calls and
// its delegated verdicts, run wherever its seat is held (ADR-0103, ADR-0125).
func (a sshAudit) writer() (sshWriter, error) {
	if c := a.engine.core.Load(); c != nil && c.accessWriter != nil {
		return c.accessWriter, nil
	}
	if a.engine.router == nil {
		return nil, errNoAccessLog
	}
	sealer, err := accessdomain.NewSealer(a.engine.cipher)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoAccessLog, err)
	}
	return a.engine.router.AccessWrites(sealer), nil
}

// sshAudit records each SSH certificate the engine signs on the access log,
// before the certificate is used: a certificate whose record did not land, or
// whose landing is unknown, is never presented to a server.
type sshAudit struct{ engine *Engine }

var _ sshint.Recorder = sshAudit{}

// RecordCertificate writes rec as the access log's certificate record.
func (a sshAudit) RecordCertificate(ctx context.Context, rec sshint.CertificateRecord) error {
	w, err := a.writer()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := w.Issued(ctx, accessdomain.Call{
		By: sshAuditor, OpID: accessdomain.IssuedOpID(now, rec.Seat, rec.Serial),
	}, accessdomain.NewCertificate{
		Serial: rec.Serial, KeyID: rec.KeyID, Handle: rec.Seat, Turn: rec.Turn, HeldCall: rec.HeldCall,
		Purpose: accessdomain.CertificatePurpose(rec.Purpose), HostIDs: rec.HostIDs, Principals: rec.Principals,
		ValidAfter: rec.ValidAfter, ValidBefore: rec.ValidBefore,
		PublicKeySHA256: rec.PublicKeySHA256, CASHA256: rec.CASHA256, Node: a.engine.nodeID(),
	})
	return accessLanded(res, err, fmt.Sprintf("certificate %016x", rec.Serial))
}

var _ sshint.CommandRecorder = sshAudit{}

// RecordCommand writes cmd as the access log's command record, its text
// sealed to it.
func (a sshAudit) RecordCommand(ctx context.Context, minted time.Time, cmd accessdomain.NewCommand) error {
	w, err := a.writer()
	if err != nil {
		return err
	}
	res, err := w.Ran(ctx, accessdomain.Call{
		By: sshAuditor, OpID: accessdomain.RanOpID(minted, cmd.Handle, cmd.ID),
	}, cmd)
	return accessLanded(res, err, "command "+cmd.ID)
}

// RecordVerdict writes v as the access log's verdict record, what it decided
// and its giver's words sealed to it, under the operation derived from minted:
// the instant the deciding work began, so a retry of one decision lands once.
// first is false, with no error, where v's giver already has a verdict on the
// request: the earlier one stands and v is not recorded, so v must not act.
// A record whose landing is not known is an error, as every SSH record's is.
func (a sshAudit) RecordVerdict(ctx context.Context, minted time.Time,
	v accessdomain.NewVerdict) (first bool, err error) {

	w, err := a.writer()
	if err != nil {
		return false, err
	}
	res, err := w.Verdict(ctx, accessdomain.Call{
		By: sshAuditor, OpID: accessdomain.VerdictOpID(minted, v.Handle, v.Request, v.By),
	}, v)
	return verdictLanded(res, err, v.Request)
}

// verdictLanded is whether a verdict's record landed as its giver's first on
// request: false with no error where an earlier one stands, and an error where
// whether it landed is not known, which must not act either.
func verdictLanded(res statelog.Result, err error, request string) (first bool, _ error) {
	if errors.Is(err, accessdomain.ErrRecorded) {
		return false, nil
	}
	if landed := accessLanded(res, err, "the verdict on "+request); landed != nil {
		return false, landed
	}
	return true, nil
}

// accessLanded is nil where what's record was applied or is pending, and otherwise
// why it is not known to have landed.
func accessLanded(res statelog.Result, err error, what string) error {
	switch {
	case err != nil:
		return fmt.Errorf("engine: record %s: %w", what, err)
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		return nil
	}
	return fmt.Errorf("engine: whether %s's record landed is %s", what, res.Outcome)
}

// SSHAccess is the setup surface's half of Test connection and the SSH audit
// on this node, its verdicts on delegated requests included: the client and
// the access log's two sides.
func (e *Engine) SSHAccess() SSHAccessService { return SSHAccessService{engine: e} }

// SSHAccessService is [Engine.SSHAccess].
type SSHAccessService struct{ engine *Engine }

// TestConnection runs Test connection, recording each certificate first.
func (s SSHAccessService) TestConnection(ctx context.Context, in sshint.TestInput) []sshint.TestStep {
	return sshint.NewClient(nil).TestConnection(ctx, in, sshAudit(s))
}

// Certificates pages the certificates the engine signed.
func (s SSHAccessService) Certificates(ctx context.Context, q accessdomain.AuditQuery,
	fresh statelog.Freshness) (accessdomain.CertificatePage, error) {
	r := s.engine.AccessReader()
	if r == nil {
		return accessdomain.CertificatePage{}, errNoAccessLog
	}
	return r.Certificates(ctx, q, fresh)
}

// Commands pages the commands the engine ran or a person denied.
func (s SSHAccessService) Commands(ctx context.Context, q accessdomain.AuditQuery,
	fresh statelog.Freshness) (accessdomain.CommandPage, error) {
	r := s.engine.AccessReader()
	if r == nil {
		return accessdomain.CommandPage{}, errNoAccessLog
	}
	return r.Commands(ctx, q, fresh)
}

// Verdicts pages the verdicts on delegated requests.
func (s SSHAccessService) Verdicts(ctx context.Context, q accessdomain.VerdictQuery,
	fresh statelog.Freshness) (accessdomain.VerdictPage, error) {
	r := s.engine.AccessReader()
	if r == nil {
		return accessdomain.VerdictPage{}, errNoAccessLog
	}
	return r.Verdicts(ctx, q, fresh)
}

// OpenVerdict opens what a verdict decided and its giver's reason and findings
// under this node's keyring, with the one sealer the node built.
func (s SSHAccessService) OpenVerdict(row accessdomain.VerdictRow) (subject, reason string,
	findings []string, err error) {
	c := s.engine.core.Load()
	if c == nil || c.accessSealer == nil {
		return "", "", nil, errNoAccessLog
	}
	return row.Open(c.accessSealer)
}

// OpenCommand opens a command's text under this node's keyring, with the one
// sealer the node built.
func (s SSHAccessService) OpenCommand(row accessdomain.CommandRow) (string, error) {
	c := s.engine.core.Load()
	if c == nil || c.accessSealer == nil {
		return "", errNoAccessLog
	}
	return row.Open(c.accessSealer)
}
