package engine

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE BOXES CONFIG JUDGES ARE THE BOXES THE ENGINE LAUNCHES.
//
// [config.Company.SeatBoxes] is a second resolution of where each seat's
// executor runs and what its box is given, made before anything is built so
// that validation can refuse a setup file a box would refuse. Its own test
// holds it to a table; this one holds it to the engine, seat by seat: every
// cell a launch of the seat can land in — one per member of its executor chain
// the launch can pick ([executorRunners]), placed through the manager's own
// default as BuildSpec places it — and the steps the engine's runEnvironment
// hands the box. A change to any of them here fails this case, where it would
// otherwise leave validation judging cells and steps no box runs.
//
// The company covers every shape a seat's boxes take: one member falling to the
// catalogue's default, a chain whose fallback lands in another cell, a chain
// whose two members share one cell, a mixed chain whose head no box can run,
// and a person, who runs no turn.
func TestConfigSeatBoxesAreTheBoxesTheEngineLaunches(t *testing.T) {
	t.Parallel()
	doc := fmt.Sprintf(`
name: Acme
providers:
  sandbox:
    fake: true
    default_run_in: direct
    setup:
      - {name: shared, brief: b}
  llm:
    main: {model: claude-golden, state_dir: %q}
    remote: {harness: claude-code, model: sonnet, run_in: e2b, state_dir: %q}
    near: {harness: claude-code, model: haiku, run_in: direct, state_dir: %q}
    boxed: {harness: opencode, model: anthropic/claude-sonnet-5, run_in: container, state_dir: %q}
    codex: {harness: codex, model: gpt-golden, state_dir: %q}
roles:
  - name: Founder
    handle: founder
    kind: human
    contact: {slack_user_id: U0FOUNDER}
  - name: Plain
    handle: plain
    llm: main
    sandbox:
      setup:
        - {name: mine, brief: b}
  - name: Spread
    handle: spread
    llm: [remote, main]
  - name: Shared
    handle: shared
    llm: [main, near]
  - name: Mixed
    handle: mixed
    llm: [codex, boxed]
`, t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir())
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.ValidateRunnable(); err != nil {
		t.Fatalf("the fixture does not run: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("this fixture's company does not build an org: %v", err)
	}
	models, err := buildProviders(cfg, config.EnvOnly(), nil)
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	manager, err := buildSandbox(cfg, config.EnvOnly(), nil, nil)
	if err != nil || manager == nil {
		t.Fatalf("buildSandbox = %v, %v", manager, err)
	}
	c := &Company{Config: cfg, Org: o, Models: models}
	e := &Engine{}
	e.epoch.current.Store(c)

	judged := map[string]map[string]config.SeatBox{}
	for box := range cfg.SeatBoxes() {
		handle := box.Seat.Seat().Handle()
		if judged[handle] == nil {
			judged[handle] = map[string]config.SeatBox{}
		}
		judged[handle][string(box.Cell)] = box
	}
	want := map[string][]string{
		"plain":  {"direct"},
		"spread": {"direct", "e2b"},
		"shared": {"direct"},
		"mixed":  {"container"},
	}
	for role := range cfg.EachRole() {
		handle := role.Seat().Handle()
		boxes := judged[handle]
		seat := o.AgentSeatByHandle(handle)
		if seat == nil {
			// Never spawned, so it launches nothing.
			if len(boxes) != 0 {
				t.Errorf("%s: config judges boxes %v for a seat the engine never runs", handle,
					slices.Sorted(maps.Keys(boxes)))
			}
			continue
		}
		runners, err := executorRunners(c, seat, manager)
		if err != nil {
			t.Fatalf("%s: %v", handle, err)
		}
		launched := map[string]string{}
		for _, m := range runners {
			cell := manager.BuildSpec(sandbox.SpecInput{
				Placement: sandbox.Placement(agentPlacement(c, m.Key)), Harness: "claude-code",
			}).Placement
			if _, seen := launched[string(cell)]; !seen {
				launched[string(cell)] = m.Key
			}
		}
		if got, judgedCells := slices.Sorted(maps.Keys(launched)), slices.Sorted(maps.Keys(boxes)); !slices.Equal(got, judgedCells) {
			t.Errorf("%s: config judges boxes in %v; the engine can launch in %v", handle, judgedCells, got)
			continue
		}
		if got := slices.Sorted(maps.Keys(launched)); !slices.Equal(got, want[handle]) {
			t.Errorf("%s: launches land in %v, want %v: the fixture no longer covers its shape",
				handle, got, want[handle])
		}
		var given []string
		for _, step := range e.runEnvironment(t.Context(), c, seat, seatSandbox(c, handle),
			manager.DefaultSetup()).setup {
			given = append(given, step.Name)
		}
		for cell, box := range boxes {
			if box.Executor != launched[cell] {
				t.Errorf("%s: config's %s box is %q's; the first member the engine launches there is %q",
					handle, cell, box.Executor, launched[cell])
			}
			var judgedSteps []string
			for _, step := range box.Steps {
				judgedSteps = append(judgedSteps, step.Step.Name)
			}
			if !slices.Equal(judgedSteps, given) {
				t.Errorf("%s: config judges the %s box's steps %v; the engine gives it %v",
					handle, cell, judgedSteps, given)
			}
		}
	}
}
