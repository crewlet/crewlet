package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE NODE'S UNMANNED-ROLE CHECK ASKS THE CURRENT COMPANY WHETHER IT NEEDS GIT.
// The engine hands the seat host its answer through the node
// (seat.Config.RoleNeeded, by way of node.Config): a fleet whose nodes declare
// their roles without git is missing nothing while its company hosts no code,
// and is missing git from the activation that turns code on. Each link of that
// chain is a field a constructor copies, and one left out made the seat host
// need every role — fleet_role_unmanned for git on every such fleet — while the
// check's own tests, which build a host by hand, still passed.
//
// Mutation: drop RoleNeeded from the engine's node.Config, or from the node's
// seat.Config, and the first assertion goes red.
func TestTheFleetRoleCheckAsksTheCurrentCompanyForGit(t *testing.T) {
	t.Parallel()
	p := planeFor(t, newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Node.Roles = []string{"data", "ingress", "seats", "workers"}
	})}))
	host := p.engine.Node().Host()
	if host.NeedsRole(placement.RoleGit) {
		t.Fatal("the seat host needs git for a company that hosts no code: the engine's " +
			"answer did not reach it")
	}

	p.activate(t.Context(), t, companyDoc+"code:\n  native: true\n")
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the revision hosting code: %v", err)
	}
	for _, role := range placement.Vocabulary() {
		if !host.NeedsRole(role) {
			t.Errorf("the seat host does not need %s for a company that hosts code", role)
		}
	}
}
