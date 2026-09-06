package phase_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

type stub struct{ model string }

func (s stub) Model() string { return s.model }
func (s stub) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{Model: s.model}, nil
}

func reg(t *testing.T, keys ...string) *phase.Registry {
	t.Helper()
	entries := make([]phase.Entry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, phase.Entry{Key: k, Provider: stub{model: k}})
	}
	r, err := phase.NewRegistry(entries)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// chainKeys returns a checker bound to t. A plain helper taking (t, members,
// err) cannot be called as chainKeys(t)(r.Chain(...)) — Go only spreads a
// multi-value call into a signature it matches exactly — and splitting every
// call site into two statements would bury the assertion.
func chainKeys(t *testing.T) func([]chain.Member, error) []string {
	t.Helper()
	return func(members []chain.Member, err error) []string {
		if err != nil {
			t.Fatalf("Chain: %v", err)
		}
		if len(members) == 0 {
			t.Fatal("Chain returned an empty chain; a nil error is supposed to guarantee one member")
		}
		out := make([]string, 0, len(members))
		for _, m := range members {
			if m.Provider == nil {
				t.Fatalf("member %q carries no provider", m.Key)
			}
			out = append(out, m.Key)
		}
		return out
	}
}

func TestAPerPhaseChainWinsOverTheRoleDefault(t *testing.T) {
	t.Parallel()
	r := reg(t, "default", "fast", "big")
	role := &org.Role{Name: "CTO", LLM: org.ProviderKeys{"default"}, LLMReview: org.ProviderKeys{"big", "fast"}}

	if got := chainKeys(t)(r.Chain(role, phase.Review)); !slices.Equal(got, []string{"big", "fast"}) {
		t.Errorf("review chain = %v, want [big fast]", got)
	}
	// The counterfactual: a phase with no override takes the role default.
	// Without it the assertion above passes for a resolver that ignores the
	// role default entirely. The executor is that phase BY CONSTRUCTION —
	// `llm` is its chain and it has no field of its own.
	if got := chainKeys(t)(r.Chain(role, phase.Execute)); !slices.Equal(got, []string{"default"}) {
		t.Errorf("execute chain = %v, want [default]", got)
	}
}

func TestARoleNamingNothingLandsOnDefault(t *testing.T) {
	t.Parallel()
	r := reg(t, "fast", "default")
	got := chainKeys(t)(r.Chain(&org.Role{Name: "CEO"}, phase.Review))
	if !slices.Equal(got, []string{"default"}) {
		t.Errorf("chain = %v, want [default] — the named key beats config order", got)
	}
}

func TestWithNoDefaultKeyTheFirstConfiguredProviderWins(t *testing.T) {
	t.Parallel()
	// CONFIG ORDER, not map order. Over a Go map this answer is randomised
	// per call, so two seats booted from one config would run on different
	// models and one seat would change model on restart.
	r := reg(t, "alpha", "beta", "gamma")
	first := chainKeys(t)(r.Chain(&org.Role{Name: "CEO"}, phase.Execute))
	if !slices.Equal(first, []string{"alpha"}) {
		t.Fatalf("chain = %v, want [alpha]", first)
	}
	for range 50 {
		if got := chainKeys(t)(r.Chain(&org.Role{Name: "CEO"}, phase.Execute)); !slices.Equal(got, first) {
			t.Fatalf("resolution is not stable: %v then %v", first, got)
		}
	}
}

func TestAnUnknownKeyIsDroppedNotFatal(t *testing.T) {
	t.Parallel()
	// A miss must not take the turn down — the fallback is what keeps a
	// seat running. It is rejected at config validation instead, which is
	// the only place it can be caught before tokens are spent.
	r := reg(t, "default", "fast")
	role := &org.Role{Name: "CTO", LLMReview: org.ProviderKeys{"claude-sonet", "fast"}}
	if got := chainKeys(t)(r.Chain(role, phase.Review)); !slices.Equal(got, []string{"fast"}) {
		t.Errorf("chain = %v, want the surviving key [fast]", got)
	}
	// Every key missing falls all the way through to "default".
	role.LLMReview = org.ProviderKeys{"nope", "also-nope"}
	if got := chainKeys(t)(r.Chain(role, phase.Review)); !slices.Equal(got, []string{"default"}) {
		t.Errorf("chain = %v, want [default]", got)
	}
}

func TestARepeatedKeyIsCollapsed(t *testing.T) {
	t.Parallel()
	// A chain that retries the same provider twice is not a fallback, it is
	// a retry — and one the provider's own retry policy already owns.
	r := reg(t, "fast", "default")
	role := &org.Role{Name: "CTO", LLMReview: org.ProviderKeys{"fast", "fast", "default"}}
	if got := chainKeys(t)(r.Chain(role, phase.Review)); !slices.Equal(got, []string{"fast", "default"}) {
		t.Errorf("chain = %v, want [fast default]", got)
	}
}

func TestSandboxTakesItsOwnKeyThenTheRoleDefault(t *testing.T) {
	t.Parallel()
	// Sandboxed work IS this seat's own work, done somewhere else. There is
	// no executor key to inherit — `llm` is the executor's chain — so its
	// own key overrides and the role default is the only fallback.
	r := reg(t, "default", "coder")
	role := &org.Role{Name: "Eng", LLM: org.ProviderKeys{"default"}}

	if got := chainKeys(t)(r.Chain(role, phase.Sandbox)); !slices.Equal(got, []string{"default"}) {
		t.Errorf("sandbox chain = %v, want the role default [default]", got)
	}
	role.LLMSandbox = org.ProviderKeys{"coder"}
	if got := chainKeys(t)(r.Chain(role, phase.Sandbox)); !slices.Equal(got, []string{"coder"}) {
		t.Errorf("sandbox chain = %v, want its own [coder]", got)
	}
}

func TestEveryPhaseReadsItsOwnField(t *testing.T) {
	t.Parallel()
	// A switch that returns the wrong field for one phase is invisible: the
	// seat still runs, on the wrong model. Each phase gets a uniquely-named
	// provider so a crossed wire has somewhere to show up.
	r := reg(t, "default", "r", "s", "a", "j", "sb")
	role := &org.Role{
		Name: "CTO", LLM: org.ProviderKeys{"default"},
		LLMReview: org.ProviderKeys{"r"}, LLMSubagent: org.ProviderKeys{"s"},
		LLMAuxiliary: org.ProviderKeys{"a"}, LLMJudge: org.ProviderKeys{"j"},
		LLMSandbox: org.ProviderKeys{"sb"},
	}
	// The executor has no field of its own: `llm` IS its chain, so it must
	// resolve to the role default here rather than to any satellite's key.
	want := map[phase.Phase]string{
		phase.Execute: "default", phase.Review: "r",
		phase.Subagent: "s", phase.Auxiliary: "a", phase.Judge: "j",
		phase.Sandbox: "sb",
		// ONBOARDING HAS NO FIELD OF ITS OWN and resolves to the role
		// default — asserted rather than omitted, so adding an
		// `llm_onboarding:` without wiring it here fails loudly.
		phase.Onboarding: "default",
	}
	if len(want) != len(phase.All) {
		t.Fatalf("this test covers %d phases, phase.All has %d — a new phase "+
			"needs a case here or its wiring is unasserted", len(want), len(phase.All))
	}
	for _, ph := range phase.All {
		if got := chainKeys(t)(r.Chain(role, ph)); !slices.Equal(got, []string{want[ph]}) {
			t.Errorf("%s chain = %v, want [%s]", ph, got, want[ph])
		}
	}
}

func TestAnEmptyRegistryIsRefusedAtBuildTime(t *testing.T) {
	t.Parallel()
	// Not at the first turn, where it reports as a nil provider deep in a
	// phase and names neither the seat nor the config.
	if _, err := phase.NewRegistry(nil); err == nil {
		t.Error("an empty registry built without error")
	}
	for name, entries := range map[string][]phase.Entry{
		"no key":      {{Provider: stub{}}},
		"no provider": {{Key: "default"}},
		"duplicate":   {{Key: "a", Provider: stub{}}, {Key: "a", Provider: stub{}}},
	} {
		if _, err := phase.NewRegistry(entries); err == nil {
			t.Errorf("%s: built without error", name)
		}
	}
}

func TestChainRefusesANilRole(t *testing.T) {
	t.Parallel()
	// A nil role must report itself, not panic inside a field read: the
	// caller is the turn engine and a panic there takes the node's whole
	// handler down.
	if _, err := reg(t, "default").Chain(nil, phase.Execute); err == nil {
		t.Error("a nil role resolved without error")
	}
}

func TestHeadIsTheChainsFirstMember(t *testing.T) {
	t.Parallel()
	r := reg(t, "default", "fast")
	role := &org.Role{Name: "CTO", LLMReview: org.ProviderKeys{"fast", "default"}}
	head, err := r.Head(role, phase.Review)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Key != "fast" {
		t.Errorf("head = %q, want fast", head.Key)
	}
}

func TestKeysAndHasReportTheConfiguredSet(t *testing.T) {
	t.Parallel()
	r := reg(t, "beta", "alpha")
	if got := r.Keys(); !slices.Equal(got, []string{"beta", "alpha"}) {
		t.Errorf("Keys = %v, want config order [beta alpha]", got)
	}
	// The returned slice must not be the registry's own: a caller sorting
	// it for display would silently reorder resolution's last resort.
	got := r.Keys()
	slices.Sort(got)
	if after := r.Keys(); !slices.Equal(after, []string{"beta", "alpha"}) {
		t.Errorf("Keys aliases internal state: after a caller sorted it, %v", after)
	}
	if !r.Has("alpha") || r.Has("gamma") {
		t.Error("Has misreports the configured set")
	}
}

func TestRoleKeysIsTheRawDeclarationValidationNeeds(t *testing.T) {
	t.Parallel()
	// Resolution and validation ask different questions of one field. The
	// validator must see what the operator WROTE — including a key the
	// fallback would quietly survive — so this returns the declaration with
	// no registry and no fallback applied.
	role := &org.Role{Name: "CTO", LLMReview: org.ProviderKeys{"typo"}}
	if got := phase.RoleKeys(role, phase.Review); !slices.Equal(got, org.ProviderKeys{"typo"}) {
		t.Errorf("RoleKeys = %v, want the raw [typo]", got)
	}
	if got := phase.RoleKeys(role, phase.Subagent); len(got) != 0 {
		t.Errorf("RoleKeys = %v, want nothing for an undeclared phase", got)
	}
}
