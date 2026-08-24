package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// The knob has three states and the manager's input carries a pointer for
// exactly that reason. Getting this wrong is not visible as a config error: a
// seat silently loses its checkout the moment a coding agent asks a question,
// which is the case a paused box exists for.
func TestTheSeatsPauseOverrideDistinguishesInheritFromNever(t *testing.T) {
	never := 0.0
	held := 600.0
	legacy := -1.0
	longhand := -30.0

	cases := []struct {
		name string
		gate config.RoleSandbox
		want *time.Duration
	}{
		{"unset inherits", config.RoleSandbox{}, nil},
		{"an explicit zero never pauses", config.RoleSandbox{PauseTTLSeconds: &never}, dur(0)},
		{"a set value is used", config.RoleSandbox{PauseTTLSeconds: &held}, dur(600 * time.Second)},
		// -1 is the field's earlier spelling of "inherit"; any negative
		// value reads the same way, because none of them can mean a
		// duration and "no expiry" is the leak the knob exists to prevent.
		{"the legacy -1 inherits", config.RoleSandbox{PauseTTLSeconds: &legacy}, nil},
		{"any negative inherits", config.RoleSandbox{PauseTTLSeconds: &longhand}, nil},
	}
	for _, c := range cases {
		got := pauseTTL(&c.gate)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: got %v, want inherit", c.name, *got)
		case c.want != nil && got == nil:
			t.Errorf("%s: got inherit, want %v", c.name, *c.want)
		case c.want != nil && got != nil && *got != *c.want:
			t.Errorf("%s: got %v, want %v", c.name, *got, *c.want)
		}
	}
}

func dur(d time.Duration) *time.Duration { return &d }
