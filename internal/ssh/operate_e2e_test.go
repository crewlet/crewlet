package ssh_test

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/sourcetree"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/ssh/sshdtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// e2eCompany is a company whose sre seat operates api-1; %q is the server's
// host key.
const e2eCompany = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  ssh:
    enabled: true
    provisioning:
      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}
    hosts:
      - {id: api-1, address: api-1.example.com, host_keys: [%s], users: {read: crewlet-read, operate: crewlet-ops}}
roles:
  - name: SRE
    handle: sre
    llm: zulu
    integrations: {ssh: {enabled: true, level: operate, hosts: [api-1]}}
`

// e2eAudit records certificates and commands as the access log is handed them.
type e2eAudit struct {
	mu    sync.Mutex
	certs []sshint.CertificateRecord
	cmds  []accessdomain.NewCommand
}

func (a *e2eAudit) RecordCertificate(_ context.Context, rec sshint.CertificateRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.certs = append(a.certs, rec)
	return nil
}

func (a *e2eAudit) RecordCommand(_ context.Context, _ time.Time, cmd accessdomain.NewCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cmds = append(a.cmds, cmd)
	return nil
}

// e2eServers is the engine's SSH over the document and a runner reaching the
// container.
type e2eServers struct {
	cfg    *config.Company
	ca     *setup.SSHCertificateAuthority
	runner *sshint.Runner
}

func (s *e2eServers) Reach(handle string) config.SSHReach { return s.cfg.SSHReachOf(handle) }

func (s *e2eServers) Read(ctx context.Context, call builtin.SSHCall) (sshint.Result, error) {
	reach := s.Reach(call.Handle)
	h, ok := reach.Host(call.Host)
	if !ok {
		return sshint.Result{}, builtin.ErrSSHUnreached
	}
	return s.runner.Read(ctx, sshint.ReadCall{Use: sshint.Use{CA: s.ca}, Seat: call.Handle, Turn: call.Turn,
		Host: h, Hosts: reach.HostIDs(), Command: call.Command})
}

func (s *e2eServers) operateCall(call builtin.SSHCall) (sshint.OperateCall, error) {
	h, ok := s.Reach(call.Handle).Host(call.Host)
	if !ok {
		return sshint.OperateCall{}, builtin.ErrSSHUnreached
	}
	return sshint.OperateCall{Use: sshint.Use{CA: s.ca}, Seat: call.Handle, Turn: call.Turn,
		HeldCall: call.HeldCall, Approver: call.Approver, Host: h, Command: call.Command, Stdin: call.Stdin}, nil
}

func (s *e2eServers) Operate(ctx context.Context, call builtin.SSHCall) (sshint.Result, error) {
	op, err := s.operateCall(call)
	if err != nil {
		return sshint.Result{}, err
	}
	return s.runner.Operate(ctx, op)
}

func (s *e2eServers) Denied(ctx context.Context, call builtin.SSHCall) error {
	op, err := s.operateCall(call)
	if err != nil {
		return err
	}
	return s.runner.Denied(ctx, op)
}

// e2eDesk records the question a held call parks its run on.
type e2eDesk struct{ asks []sandbox.Ask }

func (d *e2eDesk) RecordAsk(_ context.Context, _, _ string, ask sandbox.Ask) (sandbox.AskOutcome, error) {
	d.asks = append(d.asks, ask)
	return sandbox.AskRecorded, nil
}

func (d *e2eDesk) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}

// noRequesters composes nobody: a turn nobody woke asks for no requester.
type noRequesters struct{}

func (noRequesters) Requester(context.Context, string) (iam.Principal, error) {
	return iam.Principal{}, errors.New("no requester in this test")
}

// A HELD OPERATE CALL APPROVED ONCE RUNS ONCE AGAINST A REAL SERVER: the call
// is held and nothing reaches the server; the approver's Allow once, carrying
// the server binding the question showed, makes the engine run exactly the
// approved command once, on a certificate signed for that held call and
// recorded first, with the command recorded as approved by the approver.
//
// Mutation: make the call before it is approved, and the server's file has a
// line before the approval.
func TestAHeldOperateCallApprovedOnceRunsOnce(t *testing.T) {
	t.Parallel()
	srv := sshdtest.Start(t, sshdtest.Images[1])
	held, err := setup.GenerateSSHCertificateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		t.Fatal(err)
	}
	keys := srv.HostKeys()
	cfg, err := config.ParseCompany(fmt.Appendf(nil, e2eCompany, fmt.Sprintf("%q", keys[0])))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := cfg.Integrations.SSH.Host("api-1")
	script, err := sshint.RenderSetup(host, []string{ca.PublicLine()})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := srv.RunSetup(script); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	chart := orgchart.Of(o)
	audit := &e2eAudit{}
	servers := &e2eServers{cfg: cfg, ca: ca, runner: sshint.NewRunnerDialing(audit, audit, srv.Addr)}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Authorize: builtin.Decide(chart), SSH: builtin.SSHDeps{
		Servers: servers, Requesters: noRequesters{}, Chart: chart}}); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := sandbox.NewCallSeal(cipher)
	desk := &e2eDesk{}
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: o.AgentSeatByHandle("sre"), Org: o,
		Lookups: turnctx.NewLookups()}
	exec := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.SSHOperateTool}).
		WithHolder(builtin.NewCallHold(desk, seal)).ForTurn(turn)
	ctx := turnctx.WithJob(iam.WithPrincipal(t.Context(), turnctx.SeatPrincipal(o, turn.Seat)), "launch-1")
	const marker = "/tmp/crewlet-e2e-ran"
	args := map[string]any{"host": "api-1", "command": "echo ran >> " + marker, "reason": "prove the run"}
	res, err := exec.Execute(ctx, llm.ToolCall{Name: builtin.SSHOperateTool, Arguments: args})
	if err != nil || !res.Held || len(desk.asks) != 1 {
		t.Fatalf("the call answered %+v, %v, asked %d, want it held", res, err, len(desk.asks))
	}
	if out, _ := srv.Exec(nil, "sh", "-c", "cat "+marker+" 2>/dev/null | wc -l"); strings.TrimSpace(out) != "0" {
		t.Fatalf("the server ran the call before it was approved: %q lines", out)
	}
	parked := desk.asks[0].Held
	opened, err := seal.OpenArguments("run-1", *parked)
	if err != nil {
		t.Fatal(err)
	}
	approved := tools.WithApproval(ctx, tools.Approval{By: "ops-lead", HeldID: parked.ID, Binding: parked.Binding})
	made, err := exec.RunApproved(approved, builtin.SSHOperateTool, opened)
	if err != nil || made.Failed {
		t.Fatalf("the approved call answered %+v, %v", made, err)
	}
	if out, _ := srv.Exec(nil, "sh", "-c", "wc -l < "+marker); strings.TrimSpace(out) != "1" {
		t.Fatalf("the approved command ran %q times, want once", strings.TrimSpace(out))
	}
	if len(audit.certs) != 1 || audit.certs[0].Purpose != sshint.PurposeOperate || audit.certs[0].HeldCall != parked.ID ||
		len(audit.cmds) != 1 || audit.cmds[0].Approver != "ops-lead" || !audit.cmds[0].Ran ||
		audit.cmds[0].Decision != accessdomain.DecisionApproved {
		t.Fatalf("recorded certificates %+v and commands %+v", audit.certs, audit.cmds)
	}
}

// NO OPERATE MATERIAL REACHES A BOX: nothing a box is built from or handed
// reaches the package that signs certificates, or the one that holds the
// certificate authority, at any depth. Operate runs in the engine alone.
//
// Mutation: import internal/ssh from anything under internal/sandbox, and the
// walk names the path.
func TestNoOperateMaterialReachesABox(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/"
	root := sourcetree.Root(t)
	ctxt := build.Default
	ctxt.UseAllFiles = true
	banned := map[string]bool{"internal/ssh": true, "internal/setup": true}
	seen := map[string]bool{}
	var walk func(pkg, via string)
	walk = func(pkg, via string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		if banned[pkg] {
			t.Errorf("a box's package reaches %s through %s", pkg, via)
			return
		}
		p, err := ctxt.ImportDir(filepath.Join(root, filepath.FromSlash(pkg)), build.IgnoreVendor)
		if err != nil {
			var none *build.NoGoError
			if errors.As(err, &none) {
				return
			}
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, imp := range p.Imports {
			if rest, ok := strings.CutPrefix(imp, module); ok {
				walk(rest, via+" > "+rest)
			}
		}
	}
	for _, pkg := range []string{"internal/sandbox", "internal/sandbox/executor"} {
		walk(pkg, pkg)
	}
	if !seen["internal/sandbox/executor"] {
		t.Fatal("the walk read none of the box's packages")
	}
}
