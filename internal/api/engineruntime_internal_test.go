package api

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// offeredTool is a builtin by its name alone, which is all an offer reads of
// one.
type offeredTool struct{ name string }

func (t offeredTool) Name() string             { return t.name }
func (t offeredTool) Description() string      { return "what " + t.name + " does" }
func (offeredTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (offeredTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// A BUILTIN OFFER WITHHOLDS FROM THE SNAPSHOT ITS ROWS CAME FROM. The rows are
// the snapshot's builtins and the discovery pair, and what each agent is
// withheld is the engine's one decision taken against that same snapshot: a
// builtin registered after the offer was read is neither listed nor withheld,
// so no agent is counted against a tool its rows do not carry. lookup_colleague
// is everybody's; fleet_health needs the health grant, which sre's role carries
// and dev's does not. The discovery pair is in the rows and in no registry, so
// nothing withholds it.
//
// Mutation: carry no withholding; decide every seat as sre (dev is withheld
// nothing); or build the rows without the pair — red each way.
func TestABuiltinOfferWithholdsFromTheSnapshotItsRowsCameFrom(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - {name: SRE, handle: sre, health_tools: true}
  - {name: Dev, handle: dev}
`))
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	for _, name := range []string{builtin.LookupColleagueTool, builtin.FleetHealthTool} {
		if err := reg.Register(offeredTool{name}, tools.OriginBuiltin); err != nil {
			t.Fatal(err)
		}
	}
	offer := builtinOffer(reg.Snapshot())
	if err := reg.Register(offeredTool{builtin.CodeStatusTool}, tools.OriginBuiltin); err != nil {
		t.Fatal(err)
	}

	rows := make([]string, 0, len(offer.Rows))
	for _, row := range offer.Rows {
		rows = append(rows, row.Name)
	}
	slices.Sort(rows)
	want := []string{builtin.FleetHealthTool, builtin.LookupColleagueTool, toolname.Activate,
		toolname.ListMCPServerTools}
	slices.Sort(want)
	if !slices.Equal(rows, want) {
		t.Errorf("the offer's rows are %v, want the snapshot's two builtins and the discovery pair %v", rows, want)
	}
	if offer.Withheld == nil {
		t.Fatal("the offer carries no withholding, so every agent would be counted every built-in")
	}
	for handle, want := range map[string][]string{"dev": {builtin.FleetHealthTool}, "sre": nil} {
		if got := offer.Withheld(t.Context(), o, o.AgentSeatByHandle(handle)); !slices.Equal(got, want) {
			t.Errorf("%s is withheld %v, want %v", handle, got, want)
		}
	}
}
