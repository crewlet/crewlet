package sandbox

import (
	"testing"
	"time"
)

func specManager(t *testing.T, opts ManagerOptions) *Manager {
	t.Helper()
	if opts.Providers == nil {
		opts.Providers = map[Placement]Provider{Direct: NewFakeProvider()}
	}
	opts.Runners = map[string]Runner{"claude-code": NewFakeRunner("claude-code")}
	if opts.DefaultCodingAgent == "" {
		opts.DefaultCodingAgent = "claude-code"
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func ptr[T any](v T) *T { return &v }

// THE ONLY ENGINE-SIDE BOUND ON A RUNAWAY CODING AGENT, so the three states
// have to survive the overlay: a job is deliberately never stopped on a clock,
// and the box's TTL is refreshed on every waiter tick precisely so the clock
// cannot stop it either.
//
// The pointer is what carries the third state. Without it a seat could not
// escape a company-wide cap, because its "no cap" and its "say nothing" would
// both be zero — the same mistake pause_ttl_seconds was written to avoid.
func TestTheRoundCapOverlaysTheProviderDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		company int
		seat    *int
		want    int
	}{
		{"nothing anywhere is uncapped", 0, nil, 0},
		{"the company default reaches a silent seat", 40, nil, 40},
		{"a seat's own cap wins", 40, ptr(120), 120},
		{"a seat opts out of the company cap", 40, ptr(0), 0},
		{"a seat can cap where the company did not", 0, ptr(25), 25},
		// Refused by config validation, so this is the belt to that
		// braces: a negative that reached here would render as
		// `--max-turns -3`, which the CLI reads as its own flag.
		{"a negative is clamped, never passed on", 40, ptr(-3), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := specManager(t, ManagerOptions{DefaultMaxTurns: tc.company})
			got := m.BuildSpec(SpecInput{MaxTurns: tc.seat})
			if got.MaxTurns != tc.want {
				t.Fatalf("MaxTurns = %d, want %d", got.MaxTurns, tc.want)
			}
		})
	}
}

// The cap has to reach the runner, which is the only place it does anything.
// It was carried on LaunchRequest and set by nobody, so every coding run went
// out uncapped while the field looked wired.
func TestTheResolvedCapReachesTheCodingAgent(t *testing.T) {
	rig := newWaiterRig(t)
	rig.manager = specManager(t, ManagerOptions{DefaultMaxTurns: 40})
	rig.manager.providers = map[Placement]Provider{Direct: rig.provider}
	rig.manager.runners = map[string]Runner{"claude-code": rig.runner}

	req := launchReq("t1")
	req.Spec = rig.manager.BuildSpec(SpecInput{CodingAgent: "claude-code"})
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs, want 1", len(started))
	}
	if started[0].Limits.MaxTurns != 40 {
		t.Fatalf("the coding agent was given MaxTurns = %d, want the resolved 40",
			started[0].Limits.MaxTurns)
	}
}

// The box TTL is not a run cap and must not become one by accident.
func TestTheBoxTtlAndTheRoundCapAreSeparateKnobs(t *testing.T) {
	m := specManager(t, ManagerOptions{
		DefaultTimeout: 90 * time.Second, DefaultMaxTurns: 7,
	})
	spec := m.BuildSpec(SpecInput{})
	if spec.TimeoutSec != 90 {
		t.Fatalf("TimeoutSec = %v, want 90", spec.TimeoutSec)
	}
	if spec.MaxTurns != 7 {
		t.Fatalf("MaxTurns = %d, want 7", spec.MaxTurns)
	}
}
