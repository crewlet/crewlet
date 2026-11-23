package sandbox

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func catalogue(t *testing.T, opts ManagerOptions) (*Manager, error) {
	t.Helper()
	opts.Runners = map[string]Runner{"claude-code": NewFakeRunner("claude-code")}
	return NewManager(opts)
}

// A CATALOGUE WITH MORE THAN ONE CELL AND NO DEFAULT RESOLVES NOTHING BY
// ITSELF, and is not refused: it is the shape of a company whose every
// executor entry names its own cell.
//
// It must never resolve a run naming none by map order, which is randomised
// per run: the seat would move between the engine user's process tree and a
// container across restarts with nothing saying so. Config validation refuses the silent seat;
// this is the backstop for a row or a spec built by hand.
func TestACatalogueWithNoDefaultNeverPicksOne(t *testing.T) {
	t.Parallel()
	m, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{
		Direct:    NewFakeProvider(),
		Container: NewFakeProvider(),
	}})
	if err != nil {
		t.Fatalf("a catalogue whose callers name their cells was refused: %v", err)
	}
	if got := m.DefaultPlacement(); got != "" {
		t.Fatalf("default placement = %q, want none", got)
	}
	if spec := m.BuildSpec(SpecInput{}); spec.Placement != "" {
		t.Errorf("BuildSpec guessed %q for a run that named no cell", spec.Placement)
	}
	_, err = m.Provider("")
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("a run naming no cell was resolved by map order: %v", err)
	}
	if !strings.Contains(err.Error(), "default_run_in") || !strings.Contains(err.Error(), "run_in") {
		t.Errorf("the refusal does not name the fields that settle it: %v", err)
	}
	// The cells that ARE named resolve as ever.
	for _, placement := range []Placement{Direct, Container} {
		if _, err := m.Provider(placement); err != nil {
			t.Errorf("Provider(%q): %v", placement, err)
		}
	}
}

// ONE BACKEND IS NOT A CHOICE, so nothing is being decided for the operator
// and the default resolves. A rule that refused this too would make the
// simplest catalogue unwritable.
func TestASingleBackendNeedsNoDefault(t *testing.T) {
	t.Parallel()
	m, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{
		Container: NewFakeProvider(),
	}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.DefaultPlacement() != Container {
		t.Fatalf("default placement = %q, want %q", m.DefaultPlacement(), Container)
	}
}

// A DEFAULT NAMING A BACKEND NOBODY CONFIGURED IS REFUSED AT CONSTRUCTION:
// left to the first launch, it fails hours after the config was applied,
// inside a turn that has already spent its rounds.
func TestADefaultWithNoBackendIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	_, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: NewFakeProvider()},
		DefaultPlacement: Container,
	})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("a default with no backend was accepted: %v", err)
	}
}

// A PLACEMENT THIS BUILD DOES NOT KNOW IS A VALUE, NOT A PANIC: it fails the
// one run ([Placement.Valid]).
func TestAPlacementThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()
	_, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{
		"firecracker": NewFakeProvider(),
	}})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("an unknown placement was accepted: %v", err)
	}
	if got := Placement("firecracker").Valid(); got {
		t.Error("an unknown placement reports itself valid")
	}
	for _, p := range Placements {
		if !p.Valid() {
			t.Errorf("%q is in the closed set and reports itself invalid", p)
		}
	}
}

// EACH CELL REACHES ITS OWN BACKEND, so two seats of one company run in
// different places rather than wherever the last provider built happened to be.
func TestEachPlacementReachesItsOwnBackend(t *testing.T) {
	t.Parallel()
	local, container := NewFakeProvider(), NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: local, Container: container},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, tc := range []struct {
		placement Placement
		want      Provider
	}{
		{Direct, local},
		{Container, container},
		// Empty is the seat that named none, which is the default.
		{"", local},
	} {
		got, err := m.Provider(tc.placement)
		if err != nil {
			t.Fatalf("Provider(%q): %v", tc.placement, err)
		}
		if got != tc.want {
			t.Errorf("placement %q reached the wrong backend", tc.placement)
		}
	}
	// AN ERROR, NOT A NIL, for a cell this company did not configure: the
	// caller reaches it holding a run row, and a nil it turned into "the box
	// is gone" would abandon a job that is still running.
	directOnly, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{Direct: local}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := directOnly.Provider(Container); err == nil {
		t.Fatal("an unconfigured placement answered with a backend")
	}
	if got := directOnly.Placements(); !slices.Equal(got, []Placement{Direct}) {
		t.Errorf("Placements() = %v, want the one configured cell", got)
	}
	if got := m.Placements(); !slices.Equal(got, []Placement{Direct, Container}) {
		t.Errorf("Placements() = %v, want the configured two in closed-set order", got)
	}
}

// THE SEAT'S CELL SURVIVES THE OVERLAY, and a seat that names none inherits
// the company's — resolved at LAUNCH, so a catalogue change reaches every seat
// that wrote nothing without rewriting their blocks.
func TestTheSeatsPlacementOverlaysTheCompanyDefault(t *testing.T) {
	t.Parallel()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: NewFakeProvider(), Container: NewFakeProvider()},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.BuildSpec(SpecInput{}).Placement; got != Direct {
		t.Errorf("a silent seat runs in %q, want the company default %q", got, Direct)
	}
	if got := m.BuildSpec(SpecInput{Placement: Container}).Placement; got != Container {
		t.Errorf("a seat's own cell was overwritten: got %q, want %q", got, Container)
	}
}

// A RUN RECONNECTS THROUGH THE BACKEND THAT CREATED IT, read off its own row:
// why the placement is persisted rather than re-derived ([Placement]).
func TestARunReconnectsThroughTheBackendThatCreatedIt(t *testing.T) {
	t.Parallel()
	local, container := NewFakeProvider(), NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: local, Container: container},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	spec := m.BuildSpec(SpecInput{Placement: Container, Harness: "claude-code"})
	box, _, err := m.Acquire(t.Context(), spec, nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if container.Box(box.ID()) == nil {
		t.Fatalf("the box was created on the wrong backend")
	}
	// The row's placement, which is what a later turn actually holds.
	run := PendingRun{Placement: string(Container), SandboxID: box.ID(), Harness: "claude-code"}
	again, _, err := m.Reconnect(t.Context(), Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if again.ID() != box.ID() {
		t.Fatalf("reconnected to %q, want the run's own box %q", again.ID(), box.ID())
	}
	// A row that named the wrong cell finds nothing, which is exactly the
	// abandoned-run failure the persisted field prevents.
	if _, _, err := m.Reconnect(t.Context(), Direct, box.ID(), "claude-code"); err == nil {
		t.Fatal("the direct backend handed back a container's box")
	}
}

// A CALLER THAT NAMES NO CELL GETS THE CATALOGUE DEFAULT, resolved into the
// spec rather than left empty — so the row a launch writes from it names the
// cell its box is in, and a reconnect through that row finds the box.
func TestASpecNamingNoCellTakesTheDefault(t *testing.T) {
	t.Parallel()
	local := NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{Direct: local}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	spec := m.BuildSpec(SpecInput{Harness: "claude-code"})
	if spec.Placement != Direct {
		t.Fatalf("a spec naming no cell resolved to %q, want the catalogue default %q",
			spec.Placement, Direct)
	}
	box, _, err := m.Acquire(t.Context(), spec, nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	run := PendingRun{SandboxID: box.ID(), Harness: "claude-code", Placement: string(spec.Placement)}
	if _, _, err := m.Reconnect(t.Context(), Placement(run.Placement), run.SandboxID, run.Harness); err != nil {
		t.Fatalf("the row a launch writes could not reconnect: %v", err)
	}
}

// THE ROW REMEMBERS WHERE THE RUN IS, written with the row and before the box
// exists, never re-derived: the process collecting the run may be another node
// reading a configuration applied since, and a row that forgot its cell
// reconnects through the default's backend, abandoning a still-running
// container job as gone.
func TestALaunchRecordsWhereTheRunIs(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	container := NewFakeProvider()
	manager, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: rig.provider, Container: container},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.runners = map[string]Runner{"claude-code": rig.runner}
	rig.manager = manager

	req := launchReq("t1")
	req.Spec = manager.BuildSpec(SpecInput{Placement: Container, Harness: "claude-code"})
	res, err := rig.launchVia(t.Context(), manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("Get: %v (found %v)", err, found)
	}
	if run.Placement != string(Container) {
		t.Fatalf("the row records placement %q, want %q — a run that forgets "+
			"its cell reconnects through the wrong backend and is abandoned "+
			"as gone", run.Placement, Container)
	}
	if container.Box(res.SandboxID) == nil {
		t.Fatalf("the box was created outside the cell the row names")
	}
	// And the row is enough on its own to get back to the box, which is all
	// a completion turn in another process ever has.
	box, _, err := manager.Reconnect(t.Context(), Placement(run.Placement),
		run.SandboxID, run.Harness)
	if err != nil {
		t.Fatalf("reconnect from the row alone: %v", err)
	}
	if box.ID() != res.SandboxID {
		t.Fatalf("reconnected to %q, want %q", box.ID(), res.SandboxID)
	}
}
