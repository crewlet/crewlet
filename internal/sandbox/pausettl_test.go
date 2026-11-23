package sandbox_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE PROVIDER-WIDE PAUSE TTL HAS THREE STATES, and a plain Duration has two.
// An explicit 0 means never pause (tear the box down the moment it blocks and
// re-seed from the pushed branch), a real choice since a paused box holds its
// directory and RAM on its host; read as unset, it would silently take the
// 1800s default.
func TestTheEngineWidePauseTTLTellsUnsetFromZero(t *testing.T) {
	t.Parallel()
	never := time.Duration(0)
	short := 60 * time.Second

	for _, tc := range []struct {
		name string
		opt  *time.Duration
		want float64
	}{
		{"unset takes the engine default", nil, sandbox.DefaultPauseTTL.Seconds()},
		{"an explicit zero means never pause", &never, 0},
		{"an explicit value is carried through", &short, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newPauseManager(t, tc.opt)
			if got := m.BuildSpec(sandbox.SpecInput{}).PauseTTLSec; got != tc.want {
				t.Fatalf("PauseTTLSec = %v, want %v", got, tc.want)
			}
		})
	}
}

// A SEAT STILL OVERRIDES THE PROVIDER, in both directions: the per-role knob
// is the reason the provider-level one is a default rather than a setting.
func TestASeatsPauseTTLOverridesTheEngineWideOne(t *testing.T) {
	t.Parallel()
	never := time.Duration(0)
	m := newPauseManager(t, nil) // provider unset, so the 1800s default

	seatNever := time.Duration(0)
	if got := m.BuildSpec(sandbox.SpecInput{PauseTTL: &seatNever}).PauseTTLSec; got != 0 {
		t.Errorf("a seat asking never-pause got %v", got)
	}
	// And the other way: a seat that says nothing inherits whatever the
	// provider set, including an explicit zero.
	off := newPauseManager(t, &never)
	if got := off.BuildSpec(sandbox.SpecInput{}).PauseTTLSec; got != 0 {
		t.Errorf("a seat inheriting a never-pause provider got %v", got)
	}
}

func newPauseManager(t *testing.T, pause *time.Duration) *sandbox.Manager {
	t.Helper()
	m, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:       map[sandbox.Placement]sandbox.Provider{sandbox.Container: sandbox.NewFakeProvider()},
		Runners:         map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
		DefaultPauseTTL: pause,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}
