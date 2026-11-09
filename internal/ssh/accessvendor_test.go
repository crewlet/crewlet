package ssh_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

// company is a document with SSH connected, hosts api-1 and db-2, and the
// SRE seat on at level for hosts.
func company(level string, hosts ...string) *config.Company {
	on := true
	return &config.Company{
		Integrations: config.Integrations{SSH: &config.SSH{
			Enabled:      true,
			Provisioning: &config.SSHProvisioning{CertificateAuthority: "${SSH_CERTIFICATE_AUTHORITY}"},
			Hosts: []config.SSHHost{
				{ID: "api-1", Address: "api-1.example.com", Description: "Production API server"},
				{ID: "db-2", Address: "db-2.example.com"},
			},
		}},
		Roles: []config.Role{
			{Name: "SRE", Handle: "sre", Integrations: config.RoleIntegrations{
				SSH: &config.RoleSSH{Enabled: &on, Level: level, Hosts: hosts}}},
			{Name: "QA", Handle: "qa"},
		},
	}
}

func vendor(t *testing.T) seataccess.Vendor {
	t.Helper()
	v, ok := seataccess.Lookup(integration.KindSSH)
	if !ok {
		t.Fatal("no SSH vendor is registered")
	}
	return v
}

func session(t *testing.T, c *config.Company) seataccess.Session {
	t.Helper()
	s, err := vendor(t).Open(t.Context(), seataccess.OpenInput{Config: c})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// OPERATE IS A STEP-UP: granting it, from off or from read, needs a fresh
// step-up because the vendor marks it as administering; read never does, and
// keeping operate does not.
func TestOperateIsAStepUp(t *testing.T) {
	t.Parallel()
	opts, err := session(t, company("read", "api-1")).Options(t.Context(), seataccess.LaneSSH)
	if err != nil {
		t.Fatal(err)
	}
	off := seataccess.Desired{}
	read := seataccess.Desired{Enabled: true, Level: config.SSHLevelRead, Scope: []string{"api-1"}}
	operate := seataccess.Desired{Enabled: true, Level: config.SSHLevelOperate, Scope: []string{"api-1"}}
	for _, tc := range []struct {
		name          string
		before, after seataccess.Desired
		want          bool
	}{
		{"off to operate", off, operate, true},
		{"read to operate", read, operate, true},
		{"off to read", off, read, false},
		{"operate kept on another host", operate, seataccess.Desired{Enabled: true, Level: config.SSHLevelOperate,
			Scope: []string{"db-2"}}, false},
		{"operate to read", operate, read, false},
	} {
		if got := seataccess.NeedsStepUp(opts, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: step-up %v, want %v", tc.name, got, tc.want)
		}
	}
	if ids := []string{opts.Levels[0].ID, opts.Levels[1].ID}; !slices.Equal(ids, config.SSHLevels()) {
		t.Errorf("levels %v, want %v least first", ids, config.SSHLevels())
	}
}

// NOTHING EXISTS OUTSIDE THE DOCUMENT: no op draws a step, a seat that is on is
// observed holding exactly its setting, and one that is off holds nothing, so
// a plan for any change of level or hosts is empty and the next certificate
// follows the document.
func TestSSHAccessIsTheDocumentAlone(t *testing.T) {
	t.Parallel()
	v := vendor(t)
	cat := v.Catalog()
	if len(cat.Credentials) != 0 || len(cat.Lanes) != 1 || cat.Lanes[0].Issued != sshint.CertificateField ||
		!cat.Lanes[0].ScopeRequired || cat.Lanes[0].Rotatable {
		t.Errorf("catalog %+v", cat)
	}
	for _, op := range []seataccess.Op{seataccess.OpCreate, seataccess.OpEnable, seataccess.OpLevel,
		seataccess.OpScope, seataccess.OpDisable, seataccess.OpRemove, seataccess.OpMint, seataccess.OpRotate} {
		if steps := v.Steps(seataccess.SeatRef{Handle: "sre"}, seataccess.LaneSSH, op,
			seataccess.Desired{}, seataccess.Observed{}); len(steps) != 0 {
			t.Errorf("%s draws %v", op, steps)
		}
	}
	c := company("operate", "db-2", "api-1")
	obs, err := session(t, c).Observe(t.Context(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{{Handle: "sre"}, {Handle: "qa"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sre := obs["sre"][seataccess.LaneSSH]
	if !sre.Exists || !sre.Enabled || sre.Level != "operate" || !slices.Equal(sre.Scope, []string{"api-1", "db-2"}) ||
		sre.AccountID != "crewlet/sre" {
		t.Errorf("the SRE is observed as %+v", sre)
	}
	if qa := obs["qa"][seataccess.LaneSSH]; qa.Exists {
		t.Errorf("QA, with no SSH block, is observed as %+v", qa)
	}
	want, _ := seataccess.DesiredOf(&c.Roles[0], seataccess.LaneSSH)
	plan, err := seataccess.Plan(v, seataccess.SeatInput{
		Seat: seataccess.SeatRef{Handle: "sre"},
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneSSH: want},
		Have: map[seataccess.Lane]seataccess.Observed{seataccess.LaneSSH: {}},
	})
	if err != nil || len(plan) != 0 {
		t.Errorf("turning the SRE on plans %v, %v; want nothing to take", plan, err)
	}
}

// THE REACH IS THE REGISTERED HOSTS, never open, and the options name each
// host by its description.
func TestTheSSHReachIsTheRegisteredHosts(t *testing.T) {
	t.Parallel()
	c := company("read", "api-1")
	opts, err := session(t, c).Options(t.Context(), seataccess.LaneSSH)
	if err != nil {
		t.Fatal(err)
	}
	opts = seataccess.WithReach(opts, vendor(t), c, nil)
	if opts.ScopeOpen || !slices.Equal(opts.Scope, []string{"api-1", "db-2"}) || !opts.ScopeRequired {
		t.Errorf("options %+v", opts)
	}
	if got := opts.ScopeNames["api-1"]; got != "api-1 (Production API server)" {
		t.Errorf("api-1 is named %q", got)
	}
	empty := company("read")
	empty.Integrations.SSH.Hosts = nil
	if r := vendor(t).(seataccess.DocumentReach).Reach(empty, nil, seataccess.LaneSSH); r.Open || len(r.Scope) != 0 {
		t.Errorf("a company with no host reaches %+v", r)
	}
}

// AN INTEGRATION THAT IS OFF, OR HOLDS NO AUTHORITY, IS NOT CONFIGURED, so the
// access pass waits rather than reporting a fault.
func TestAnSSHBlockWithoutAnAuthorityIsNotConfigured(t *testing.T) {
	t.Parallel()
	off := company("read", "api-1")
	off.Integrations.SSH.Enabled = false
	bare := company("read", "api-1")
	bare.Integrations.SSH.Provisioning = nil
	for name, c := range map[string]*config.Company{"off": off, "no authority": bare, "absent": {}} {
		if _, err := vendor(t).Open(t.Context(), seataccess.OpenInput{Config: c}); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
	}
}
