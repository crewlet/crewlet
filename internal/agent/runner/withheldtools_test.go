package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WITHHELD TOOL IS OFFERED ON NO PHASE.
//
// The registry is the epoch's, shared by every seat, and a tool only some
// seats may call — fleet_health, which a role's grant opens — is registered
// for all of them; the engine names what this seat's surfaces leave out. The
// executor's run is not offered it, cannot activate it over the bridge, and
// its prompt does not list it to discover; the reviewer is not offered it; the
// onboarding pass's catalogue does not list it. Every other tool stays.
//
// Mutation: build any phase's surface from the registry whole rather than the
// seat's snapshot, and that phase offers or lists fleet_health.
func TestAWithheldToolIsOfferedOnNoPhase(t *testing.T) {
	t.Parallel()
	withheld := []string{"fleet_health", "code_status"}
	launcher := &playLauncher{}
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		launcher: launcher,
		register: func(t *testing.T, reg *tools.Registry) {
			for _, name := range withheld {
				if err := reg.Register(stubTool{name: name}, tools.OriginBuiltin); err != nil {
					t.Fatalf("Register: %v", err)
				}
			}
		},
		withheld: withheld,
	})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := launcher.last(t)
	offered := toolNames(req.Surface.ToolDefs())
	for _, name := range withheld {
		if slices.Contains(offered, name) {
			t.Errorf("the executor's run was offered %s", name)
		}
		if req.Surface.Activate(name) {
			t.Errorf("the executor's run activated %s over the bridge", name)
		}
		if strings.Contains(req.System, name) {
			t.Errorf("the executor's catalogue lists %s to discover", name)
		}
	}
	if !slices.Contains(offered, "lookup_colleague") {
		t.Error("the executor's run lost lookup_colleague, which nothing withholds")
	}

	if _, err := r.Review(context.Background(), 1, workFor("posted the summary"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	reviewed := prov.offeredFor("review")
	if len(reviewed) == 0 {
		t.Fatal("the review phase made no request")
	}
	for i, names := range reviewed {
		for _, name := range withheld {
			if slices.Contains(names, name) {
				t.Errorf("review request %d was offered %s", i+1, name)
			}
		}
	}

	onboard := marking()
	reg := tools.NewRegistry()
	for _, name := range []string{"reflect_and_persist", "mark_onboarded", "fleet_health"} {
		if err := reg.Register(stubTool{name: name, out: "ok"}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: onboard}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	pass, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models, Launcher: &playLauncher{}, Sessions: sessions,
		Onboarding: runner.Onboarding{Markers: &markers{claimHeld: true}, Latch: runner.NewLatch(),
			Rounds: 3},
		Withheld: []string{"fleet_health"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, err := pass.Onboard(context.Background()); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if system := systemOf(t, onboard, "onboarding"); strings.Contains(system, "fleet_health") {
		t.Error("the onboarding pass's catalogue lists fleet_health")
	}
	for i, names := range onboard.offeredFor("onboarding") {
		if slices.Contains(names, "fleet_health") {
			t.Errorf("onboarding request %d was offered fleet_health", i+1)
		}
	}
}
