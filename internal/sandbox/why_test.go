package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// What these cases protect: every failure a launch can meet is CLASSED where it
// is met, by what clears it ([sandbox.Why]). A failure that lost its class
// reads as a fault of the node; one with the wrong class sends its reader to
// wait on a configuration nobody fixed, or to give up on an outage that clears
// in a minute.

// A BOX THAT IS GONE OR PAUSED IS NOT A CLASS: each is its own sentinel, a gone
// box settled at once and a paused one read later, whatever mark a caller
// wraps around it.
//
// Mutation: drop WhyOf's sentinel guard, and the marked cases are classed.
func TestAGoneBoxIsNoLaunchClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := sandbox.NewFakeProvider()
	_, gone := provider.Connect(ctx, "never-made")
	if !errors.Is(gone, sandbox.ErrBoxGone) {
		t.Fatalf("connecting to a box never made = %v; want ErrBoxGone", gone)
	}
	box, err := provider.Create(ctx, sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	_, paused := provider.Attach(ctx, box.ID())
	if !errors.Is(paused, sandbox.ErrBoxPaused) {
		t.Fatalf("attaching to a paused box = %v; want ErrBoxPaused", paused)
	}
	for name, err := range map[string]error{
		"a gone box":                   gone,
		"a gone box a caller marked":   sandbox.Because(sandbox.WhyProviderUnavailable, fmt.Errorf("collecting: %w", gone)),
		"a paused box":                 paused,
		"a paused box a caller marked": sandbox.Because(sandbox.WhyConfiguration, paused),
	} {
		if why, ok := sandbox.WhyOf(err); ok {
			t.Errorf("%s is classed %q", name, why)
		}
	}
}

// A SETUP STEP IS THE COMPANY'S RECIPE WHERE A COMMAND OF IT EXITED, and the
// box's where the box never ran it to an exit: only the recipe is a person's to
// change. Exit 75 is the recipe saying a download fails for now, which a wait
// clears; its own timeout's 124 is still the recipe's. The command's output is
// never read: every step prints a 503.
//
// Mutation: class every [sandbox.SetupError] as configuration, or drop its
// Unwrap, or class 75 as configuration or 124 as temporary, and a case goes
// red.
func TestASetupFailureIsClassedByWhoseItIs(t *testing.T) {
	t.Parallel()
	steps := []sandbox.SetupStep{{Name: "deps", Commands: []string{"npm ci"}}}

	for status, want := range map[int]sandbox.Why{
		3:   sandbox.WhyConfiguration,
		124: sandbox.WhyConfiguration,
		75:  sandbox.WhySetupUnavailable,
	} {
		exited := sandbox.NewFakeSandbox("b1")
		exited.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: status, Stderr: "npm error code E503"}, nil
		}
		err := sandbox.ApplySetup(context.Background(), exited, steps, nil)
		if why, ok := sandbox.WhyOf(err); !ok || why != want {
			t.Errorf("a step that exited %d is classed %q (%v), want %q", status, why, ok, want)
		}
	}

	gone := sandbox.NewFakeSandbox("b2")
	gone.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, sandbox.Because(sandbox.WhyProviderUnavailable,
			errors.New("the container runtime could not start the exec: connection refused"))
	}
	err := sandbox.ApplySetup(context.Background(), gone, steps, nil)
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyProviderUnavailable {
		t.Errorf("a step the box never ran is classed %q (%v), want the box's own %q", why, ok,
			sandbox.WhyProviderUnavailable)
	}
	var setup *sandbox.SetupError
	if !errors.As(err, &setup) || setup.Exited {
		t.Errorf("the failure lost its step: %v", err)
	}

	broken := sandbox.NewFakeSandbox("b3")
	broken.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New("fork: resource temporarily unavailable")
	}
	err = sandbox.ApplySetup(context.Background(), broken, steps, nil)
	if why, ok := sandbox.WhyOf(err); ok {
		t.Errorf("a step the host could not run is classed %q, a failure nothing classed", why)
	}
}

// A CELL OR A HARNESS THE CATALOGUE DOES NOT HAVE is configuration: a
// person's providers.sandbox, which no wait changes.
func TestACatalogueGapIsConfiguration(t *testing.T) {
	t.Parallel()
	m, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Container: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.Container,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]sandbox.Spec{
		"a cell nobody configured": {Placement: sandbox.Direct, Harness: "claude-code"},
		"a runner nobody has":      {Placement: sandbox.Container, Harness: "unheard-of"},
	} {
		_, _, err := m.Acquire(context.Background(), spec, nil)
		if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
			t.Errorf("%s is classed %q (%v), want %q: %v", name, why, ok, sandbox.WhyConfiguration, err)
		}
	}
}

// THE CLASS TABLE IS WHOLE: every class is valid, says whether a wait clears
// it, and an unknown one is not a class.
func TestEveryClassIsKnown(t *testing.T) {
	t.Parallel()
	waits := map[sandbox.Why]bool{
		sandbox.WhyConfiguration: false, sandbox.WhySetupUnavailable: true,
		sandbox.WhyProviderUnavailable: true,
	}
	if len(sandbox.Whys) != len(waits) {
		t.Fatalf("%d classes declared, %d judged here", len(sandbox.Whys), len(waits))
	}
	for _, why := range sandbox.Whys {
		if !why.Valid() {
			t.Errorf("%q is declared and not valid", why)
		}
		if why.Waits() != waits[why] {
			t.Errorf("%q waits %t, want %t", why, why.Waits(), waits[why])
		}
	}
	if sandbox.Why("from_a_newer_build").Valid() {
		t.Error("an unknown class is valid")
	}
	if why, ok := sandbox.WhyOf(errors.New("plain")); ok {
		t.Errorf("an unmarked error is classed %q", why)
	}
	if sandbox.Because(sandbox.WhyConfiguration, nil) != nil {
		t.Error("marking no error made one")
	}
}
