package ssh

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// SSH's half of per-seat access. A seat's access is its level and its hosts,
// as the company document writes them; its credential is a certificate the
// engine signs at use, never sealed. There is no account anywhere to create,
// level, disable or delete, so every step is empty, and what a seat holds is
// what the document says: turning a seat or the integration off stops the
// next certificate, and one already issued expires within minutes.

func init() { seataccess.Register(accessVendor{}) }

// CertificateField is the issued credential's field: a seat's certificates
// are signed at use and never sealed.
const CertificateField = "CERTIFICATE"

// The two levels' words, as the Agents tab draws them.
const (
	readNote    = "Runs this server's allowlisted read commands through ssh_read. No approval."
	operateNote = "Read, plus any command through ssh_operate, each one approved by a person first."
)

// LevelName is a level's name in the dashboard's words.
func LevelName(level string) string {
	switch level {
	case config.SSHLevelOperate:
		return "Operate"
	default:
		return "Read"
	}
}

// accessVendor is SSH's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindSSH }

// Catalog is SSH's per-seat offer: two fixed levels on the hosts a seat names,
// which it must name. Nothing is sealed or rotated: each certificate is issued
// at use for minutes ([CertificateField]).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind: integration.KindSSH,
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneSSH, Levels: true, Scoped: true, ScopeRequired: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, Issued: CertificateField,
		}},
	}
}

// Steps is empty for every op: nothing exists outside the document to change.
func (accessVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// Reach is every host the company registers, which a seat's hosts must be
// within. Never open: a company with no host reaches none.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil {
		return seataccess.Reach{}
	}
	return seataccess.Reach{Scope: c.Integrations.SSH.HostIDs()}
}

// Open builds a session from the company's SSH block. An integration that is
// absent, off or without its certificate authority is not configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("ssh: %w: no company document", integration.ErrNotConfigured)
	}
	block := in.Config.Integrations.SSH
	if block == nil || !block.Enabled || block.Provisioning == nil ||
		strings.TrimSpace(block.Provisioning.CertificateAuthority) == "" {
		return nil, fmt.Errorf("ssh: %w: integrations.ssh is off or holds no certificate authority",
			integration.ErrNotConfigured)
	}
	return &accessSession{company: in.Config}, nil
}

// accessSession reads the document it was opened on, and nothing else.
type accessSession struct{ company *config.Company }

// Options is the two fixed levels and the registered hosts. Operate administers,
// so granting it needs a step-up.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	levels := []seataccess.Level{
		{ID: config.SSHLevelRead, Name: LevelName(config.SSHLevelRead), Note: readNote},
		{ID: config.SSHLevelOperate, Name: LevelName(config.SSHLevelOperate), Note: operateNote, Administers: true},
	}
	block := s.company.Integrations.SSH
	names := map[string]string{}
	for _, h := range block.Hosts {
		if d := strings.TrimSpace(h.Description); d != "" {
			names[h.ID] = h.ID + " (" + d + ")"
		}
	}
	if len(names) == 0 {
		names = nil
	}
	return seataccess.Options{
		Lane: lane, Levels: levels, Scoped: true, ScopeRequired: true,
		Scope: block.HostIDs(), ScopeLive: true, ScopeNames: names,
	}, nil
}

// Observe is each seat's setting in the document: a seat that is on holds
// exactly what it is granted, since there is nothing outside the document to
// drift. A seat that is off, or left the document, holds nothing.
func (s *accessSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneSSH); ok && d.Enabled {
			identity := KeyIDPrefix(handle)
			observed = seataccess.Observed{
				Exists: true, Enabled: true, Account: identity, AccountID: identity,
				Level: d.Level, Scope: d.Scope,
			}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneSSH: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("ssh: no step %s exists for %s: SSH access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

// KeyIDPrefix is what every certificate a seat is issued names it by in its key
// ID, as servers log it: crewlet/<handle>.
func KeyIDPrefix(handle string) string { return "crewlet/" + handle }
