package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

// The SSH tools on this node: what a seat reaches is read off the running
// document at every call, so turning the integration, a seat's access or a
// host off stops the next certificate; the certificate authority is resolved
// at every call and held only in memory.

// SSHToolDeps are the SSH tools' dependencies on this node, or the zero value
// (which serves neither) without an engine.
func SSHToolDeps(e *Engine) builtin.SSHDeps {
	if e == nil {
		return builtin.SSHDeps{}
	}
	return builtin.SSHDeps{Servers: sshServers{engine: e}, Requesters: turnRequesters{engine: e},
		Chart: ChartAuthorityOf(e)}
}

// sshRun is this node's SSH runner, made once.
func (e *Engine) sshRun() *sshint.Runner {
	e.sshRunnerOnce.Do(func() { e.sshRunner = sshint.NewRunner(sshAudit{engine: e}, sshAudit{engine: e}) })
	return e.sshRunner
}

// sshServers is [builtin.SSHServers] on this node.
type sshServers struct{ engine *Engine }

var _ builtin.SSHServers = sshServers{}

// Reach is what handle reaches as the running document says now.
func (s sshServers) Reach(handle string) config.SSHReach {
	c := s.engine.Company()
	if c == nil || c.Config == nil {
		return config.SSHReach{}
	}
	return c.Config.SSHReachOf(handle)
}

// reached is call's host as the seat reaches it now, every host it reaches,
// and what its command is signed and bounded by.
func (s sshServers) reached(ctx context.Context, call builtin.SSHCall) (config.SSHHost, []string, sshint.Use, error) {
	reach := s.Reach(call.Handle)
	h, ok := reach.Host(call.Host)
	if !ok {
		return config.SSHHost{}, nil, sshint.Use{}, fmt.Errorf("%w: %s", builtin.ErrSSHUnreached, call.Host)
	}
	block := s.engine.Company().Config.Integrations.SSH
	use := sshint.Use{Limits: block.Commands, SourceAddresses: block.Provisioning.SourceAddresses}
	value := strings.TrimSpace(s.engine.resolver().Value(block.Provisioning.CertificateAuthority))
	ca, err := setup.ParseSSHCertificateAuthority(value)
	if err != nil {
		// NO AUTHORITY SIGNS: the runner records the command as the
		// engine's error, and the reason is the log's.
		log.ErrorContext(ctx, "ssh_certificate_authority_unusable", "seat", call.Handle, "error", err.Error())
	} else {
		use.CA = ca
	}
	return h, reach.HostIDs(), use, nil
}

// Read runs call as its host's read user.
func (s sshServers) Read(ctx context.Context, call builtin.SSHCall) (sshint.Result, error) {
	h, hosts, use, err := s.reached(ctx, call)
	if err != nil {
		return sshint.Result{}, err
	}
	return s.engine.sshRun().Read(ctx, sshint.ReadCall{Use: use, Seat: call.Handle, Turn: call.Turn,
		Host: h, Hosts: hosts, Command: call.Command})
}

// Operate runs an approved call as its host's operate user.
func (s sshServers) Operate(ctx context.Context, call builtin.SSHCall) (sshint.Result, error) {
	h, _, use, err := s.reached(ctx, call)
	if err != nil {
		return sshint.Result{}, err
	}
	return s.engine.sshRun().Operate(ctx, s.operateCall(call, h, use))
}

// Denied records that a person denied call, whatever the seat reaches now.
func (s sshServers) Denied(ctx context.Context, call builtin.SSHCall) error {
	h := config.SSHHost{ID: call.Host}
	if c := s.engine.Company(); c != nil && c.Config != nil {
		if found, ok := c.Config.Integrations.SSH.Host(call.Host); ok {
			h = found
		}
	}
	return s.engine.sshRun().Denied(ctx, s.operateCall(call, h, sshint.Use{}))
}

func (sshServers) operateCall(call builtin.SSHCall, h config.SSHHost, use sshint.Use) sshint.OperateCall {
	return sshint.OperateCall{Use: use, Seat: call.Handle, Turn: call.Turn, HeldCall: call.HeldCall,
		Approver: call.Approver, Host: h, Command: call.Command, Stdin: call.Stdin}
}
