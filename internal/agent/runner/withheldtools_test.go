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
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WITHHELD TOOL IS OFFERED ON NO PHASE.
//
// The registry is the epoch's, shared by every seat, and a tool only some
// seats may call — fleet_health, which a role's grant opens — is registered
// for all of them; the engine names what this seat's surfaces leave out. The
// executor is not offered it and its prompt does not list it to discover, even
// when the model asks to activate it; the reviewer is not offered it; the
// onboarding pass's catalogue does not list it. Every other tool stays.
//
// Mutation: build any phase's surface from the registry whole rather than the
// seat's snapshot, and that phase offers or lists fleet_health.
func TestAWithheldToolIsOfferedOnNoPhase(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		execute: []llm.Completion{activate("fleet_health"), submitWork(t)},
		review:  []llm.Completion{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{
		register: func(t *testing.T, reg *tools.Registry) {
			for _, name := range []string{"fleet_health", "code_status"} {
				if err := reg.Register(stubTool{name: name}, tools.OriginBuiltin); err != nil {
					t.Fatalf("Register: %v", err)
				}
			}
		},
		withheld: []string{"code_status", "fleet_health"},
	})
	w, _, err := r.Execute(context.Background(), 1, "", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Review(context.Background(), 1, w, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	for _, which := range []string{"execute", "review"} {
		requests := prov.requestsFor(which)
		if len(requests) == 0 {
			t.Fatalf("the %s phase made no request", which)
		}
		for i, req := range requests {
			offered := toolNames(req.Tools)
			for _, withheld := range []string{"fleet_health", "code_status"} {
				if slices.Contains(offered, withheld) {
					t.Errorf("%s round %d was offered %s", which, i+1, withheld)
				}
			}
			if which == "execute" && !slices.Contains(offered, "lookup_colleague") {
				t.Errorf("execute round %d lost lookup_colleague, which nothing withholds", i+1)
			}
		}
	}
	if system := systemOf(t, prov, "execute"); strings.Contains(system, "fleet_health") {
		t.Error("the executor's catalogue lists fleet_health to discover")
	}

	onboard := &scriptedProvider{onboarding: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "mark_onboarded",
			Arguments: map[string]any{"notes": "read the pages"}}}},
	}}
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
		Registry: reg, Models: models, Caps: runner.Caps{ExecutorRounds: 2},
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
}
