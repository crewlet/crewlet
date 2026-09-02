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
	opts.DefaultCodingAgent = "claude-code"
	return NewManager(opts)
}

// A CATALOGUE WITH MORE THAN ONE CELL AND NO DEFAULT RESOLVES NOTHING BY
// ITSELF — and is not refused, because it is the shape a company takes when
// every seat and every agent-mode entry names its own cell, which is what the
// catalogue exists for.
//
// What it must never do is resolve a run that names none by taking whichever
// key the map happened to yield: map order is randomised per run, so the
// company would run that seat on the engine host one restart and in a remote
// box the next, with nothing in the config changing and nothing anywhere
// saying which. Config validation refuses the silent seat; this is the belt
// to that brace for a row or a spec built by hand.
func TestACatalogueWithNoDefaultNeverPicksOne(t *testing.T) {
	t.Parallel()
	m, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{
		Direct: NewFakeProvider(),
		E2B:    NewFakeProvider(),
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
	for _, placement := range []Placement{Direct, E2B} {
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
		E2B: NewFakeProvider(),
	}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.DefaultPlacement() != E2B {
		t.Fatalf("default placement = %q, want %q", m.DefaultPlacement(), E2B)
	}
}

// A DEFAULT NAMING A BACKEND NOBODY CONFIGURED IS THE FAILURE THIS RESHAPE
// EXISTS TO MAKE IMPOSSIBLE, and it is caught at construction: left to the
// first coding run, it fails hours after the config was applied, inside a turn
// that has already spent its rounds.
func TestADefaultWithNoBackendIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	_, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: NewFakeProvider()},
		DefaultPlacement: E2B,
	})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("a default with no backend was accepted: %v", err)
	}
}

// A VALUE OFF THE WIRE IS A VALUE, NOT A PANIC: a placement arrives on a
// pending run's row written by a peer that may be a newer build, and a row
// from the future must fail the one run rather than the node reading it.
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

// EACH CELL REACHES ITS OWN BACKEND. The whole point of the catalogue is that
// two seats on one company run in different places, and a manager that handed
// every caller one provider would put them both wherever the last one built
// happened to be.
func TestEachPlacementReachesItsOwnBackend(t *testing.T) {
	t.Parallel()
	local, remote := NewFakeProvider(), NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: local, E2B: remote},
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
		{E2B, remote},
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
	if _, err := m.Provider(Container); err == nil {
		t.Fatal("an unconfigured placement answered with a backend")
	}
	if got := m.Placements(); !slices.Equal(got, []Placement{Direct, E2B}) {
		t.Errorf("Placements() = %v, want the configured two in closed-set order", got)
	}
}

// THE SEAT'S CELL SURVIVES THE OVERLAY, and a seat that names none inherits
// the company's — resolved at LAUNCH, so a catalogue change reaches every seat
// that wrote nothing without rewriting their blocks.
func TestTheSeatsPlacementOverlaysTheCompanyDefault(t *testing.T) {
	t.Parallel()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: NewFakeProvider(), E2B: NewFakeProvider()},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.BuildSpec(SpecInput{}).Placement; got != Direct {
		t.Errorf("a silent seat runs in %q, want the company default %q", got, Direct)
	}
	if got := m.BuildSpec(SpecInput{Placement: E2B}).Placement; got != E2B {
		t.Errorf("a seat's own cell was overwritten: got %q, want %q", got, E2B)
	}
}

// A RUN RECONNECTS THROUGH THE BACKEND THAT CREATED IT, read off its own row.
//
// This is the load-bearing reason the placement is persisted rather than
// re-derived: the completion turn may be a different process on a different
// node, days later, with the company configuration applied again in between.
// Reconnecting to a remote box through the local backend does not error
// usefully — it reports a box that has vanished, so a job that is still
// running is abandoned as gone.
func TestARunReconnectsThroughTheBackendThatCreatedIt(t *testing.T) {
	t.Parallel()
	local, remote := NewFakeProvider(), NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: local, E2B: remote},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	spec := m.BuildSpec(SpecInput{Placement: E2B})
	box, _, err := m.Acquire(t.Context(), spec, nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if remote.Box(box.ID()) == nil {
		t.Fatalf("the box was created on the wrong backend")
	}
	// The row's placement, which is what a later turn actually holds.
	run := PendingRun{Placement: string(E2B), SandboxID: box.ID(), CodingAgent: "claude-code"}
	again, _, err := m.Reconnect(t.Context(), Placement(run.Placement), run.SandboxID, run.CodingAgent)
	if err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if again.ID() != box.ID() {
		t.Fatalf("reconnected to %q, want the run's own box %q", again.ID(), box.ID())
	}
	// A row that named the wrong cell finds nothing, which is exactly the
	// abandoned-run failure the persisted field prevents.
	if _, _, err := m.Reconnect(t.Context(), Direct, box.ID(), "claude-code"); err == nil {
		t.Fatal("the local backend handed back a remote box")
	}
}

// A ROW WRITTEN BEFORE THE FIELD EXISTED DECODES EMPTY, and empty is the
// company default rather than a failure: a rolling upgrade has one build
// writing the placement and another not, and refusing the older rows would
// strand every run in flight across the deploy.
func TestARowWithNoPlacementTakesTheDefault(t *testing.T) {
	t.Parallel()
	local := NewFakeProvider()
	m, err := catalogue(t, ManagerOptions{Providers: map[Placement]Provider{Direct: local}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	box, _, err := m.Acquire(t.Context(), m.BuildSpec(SpecInput{}), nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	run := PendingRun{SandboxID: box.ID(), CodingAgent: "claude-code"}
	if _, _, err := m.Reconnect(t.Context(), Placement(run.Placement), run.SandboxID, run.CodingAgent); err != nil {
		t.Fatalf("a row with no placement could not reconnect: %v", err)
	}
}

// THE ROW REMEMBERS WHERE THE RUN IS, written with the row and before the box
// exists.
//
// It is not re-derived, and that is the point: the process that collects this
// run may not be the one that started it, may be on another node, and may be
// reading a company configuration applied since. A row that forgot its cell
// reconnects through whichever backend the default names — which, for a run in
// a remote box, reports a box that has vanished and abandons a job that is
// still going.
func TestALaunchRecordsWhereTheRunIs(t *testing.T) {
	rig := newWaiterRig(t)
	remote := NewFakeProvider()
	manager, err := catalogue(t, ManagerOptions{
		Providers:        map[Placement]Provider{Direct: rig.provider, E2B: remote},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.runners = map[string]Runner{"claude-code": rig.runner}
	rig.manager = manager

	req := launchReq("t1")
	req.Spec = manager.BuildSpec(SpecInput{Placement: E2B, CodingAgent: "claude-code"})
	res, err := Launch(t.Context(), manager, rig.pending, rig.queue, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("Get: %v (found %v)", err, found)
	}
	if run.Placement != string(E2B) {
		t.Fatalf("the row records placement %q, want %q — a run that forgets "+
			"its cell reconnects through the wrong backend and is abandoned "+
			"as gone", run.Placement, E2B)
	}
	if remote.Box(res.SandboxID) == nil {
		t.Fatalf("the box was created outside the cell the row names")
	}
	// And the row is enough on its own to get back to the box, which is all
	// a completion turn in another process ever has.
	box, _, err := manager.Reconnect(t.Context(), Placement(run.Placement),
		run.SandboxID, run.CodingAgent)
	if err != nil {
		t.Fatalf("reconnect from the row alone: %v", err)
	}
	if box.ID() != res.SandboxID {
		t.Fatalf("reconnected to %q, want %q", box.ID(), res.SandboxID)
	}
}
