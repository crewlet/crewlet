package engine

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE BOXES CONFIG JUDGES ARE THE BOXES THE ENGINE LAUNCHES.
//
// [config.Company.SeatBoxes] is a second resolution of where each seat's work
// runs and what its box is given, made before anything is built so that
// validation can refuse a setup file a box would refuse. Its own test holds it
// to a table; this one holds it to the engine, seat by seat: whether the seat's
// executor is an agent-mode launcher, which is what keeps run_sandbox off its
// surface; the cell that launcher carries, or the run_sandbox gate's, each
// resolved through the manager's own default, as BuildSpec resolves it at
// launch; and the steps the engine's runEnvironment hands the box. A change to
// any of them here fails this case, where it would otherwise leave validation
// judging cells and steps no box runs.
//
// The company covers every shape a seat's boxes take: a gate naming its cell,
// a gate falling to the catalogue's default, a gate that is off, an agent-mode
// seat with no gate, one whose gate says `self`, one whose gate names a cell of
// its own (which a write refuses and a stored revision may still carry, so it
// is read without validation), and a person, who runs no turn.
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
    main:
      model: claude-golden
    sub: {agent: claude-code, model: sonnet, mode: agent, run_in: e2b, state_dir: %[1]q}
    deflt: {agent: claude-code, model: sonnet, mode: agent, state_dir: %[1]q}
roles:
  - name: Founder
    handle: founder
    kind: human
    contact: {slack_user_id: U0FOUNDER}
  - name: Named
    handle: named
    llm: main
    sandbox:
      enabled: true
      run_in: container
      setup:
        - {name: mine, brief: b}
  - name: Silent
    handle: silent
    llm: main
    sandbox: {enabled: true}
  - name: Idle
    handle: idle
    llm: main
    sandbox: {enabled: false}
  - name: Bare
    handle: bare
    llm: deflt
  - name: Self
    handle: self
    llm: sub
    sandbox:
      enabled: true
      run_in: self
      setup:
        - {name: own, brief: b}
  - name: Elsewhere
    handle: elsewhere
    llm: sub
    sandbox:
      enabled: true
      run_in: container
      setup:
        - {name: kept, brief: b}
`, t.TempDir())
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("this fixture's company does not build an org: %v", err)
	}
	models, err := buildProviders(cfg, config.EnvOnly())
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	manager, err := buildSandbox(cfg, config.EnvOnly(), nil)
	if err != nil || manager == nil {
		t.Fatalf("buildSandbox = %v, %v", manager, err)
	}
	c := &Company{Config: cfg, Org: o, Models: models}
	e := &Engine{}
	e.epoch.current.Store(c)

	judged := map[string]config.SeatBox{}
	for box := range cfg.SeatBoxes() {
		judged[box.Seat.Seat().Handle()] = box
	}
	var boxed int
	for role := range cfg.EachRole() {
		handle := role.Seat().Handle()
		box, has := judged[handle]
		seat := o.AgentSeatByHandle(handle)
		if seat == nil {
			// Never spawned, so it launches nothing of either kind.
			if has {
				t.Errorf("%s: config judges a box in %s for a seat the engine never runs", handle, box.Cell)
			}
			continue
		}
		gate := seatSandbox(c, handle)
		var (
			launches bool
			executor bool
			cell     sandbox.Placement
		)
		switch launcher := c.executorRuntime(e, &turnctx.Turn{RunID: "t-" + handle}, seat); {
		case launcher != nil:
			// The executor's own box, every turn; and the runner offers
			// such a seat no run_sandbox, its launcher being present.
			launches, executor = true, true
			cell = manager.BuildSpec(sandbox.SpecInput{Placement: launcher.placement}).Placement
		case gate != nil && gate.Enabled && config.Placement(gate.RunIn) != config.PlacementSelf:
			// The runner offers run_sandbox to a seat with no launcher and
			// an enabled gate, and the launch refuses only `self`.
			launches = true
			cell = manager.BuildSpec(sandbox.SpecInput{Placement: sandbox.Placement(gate.RunIn)}).Placement
		}
		if has != launches {
			t.Errorf("%s: config judges a box: %v; the engine launches one: %v", handle, has, launches)
			continue
		}
		if !launches {
			continue
		}
		boxed++
		if (box.Executor != "") != executor {
			t.Errorf("%s: config's box is the executor's: %v; the engine's: %v", handle, box.Executor != "", executor)
		}
		if string(box.Cell) != string(cell) {
			t.Errorf("%s: config places the box in %q; the engine launches it in %q", handle, box.Cell, cell)
		}
		var judgedSteps, given []string
		for _, step := range box.Steps {
			judgedSteps = append(judgedSteps, step.Step.Name)
		}
		for _, step := range e.runEnvironment(t.Context(), e.Company(),
			e.Company().Org.AgentSeatByHandle(handle), gate, manager.DefaultSetup()).setup {
			given = append(given, step.Name)
		}
		if !slices.Equal(judgedSteps, given) {
			t.Errorf("%s: config judges steps %v; the engine gives the box %v", handle, judgedSteps, given)
		}
	}
	// Every shape above but the person and the gate that is off launches a
	// box, so a fixture that stopped building them proves nothing.
	if boxed != 5 {
		t.Errorf("%d seats launched a box, want 5: the fixture no longer covers every shape", boxed)
	}
}
