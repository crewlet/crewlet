package harnessprofile

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// PlanRead is the harness's own read of where its plan's windows stand, made
// with no model call: Claude Code's usage screen (`claude -p /usage`). The
// engine runs it in the entry's login home, as it runs status_args, so the
// harness reaches its vendor with its own client and refreshes its own login.
//
// It reads text the harness writes about the plan, never a model's: Text is a
// field of the harness's own JSON, and Turns proves no model answered. A read
// that counts a turn is refused, and Fence keeps one from leaving the host.
type PlanRead struct {
	// Args runs the read.
	Args []string `yaml:"args,omitempty"`

	// Fence is set in the read's environment so that any model request the
	// command makes fails on this host: Claude Code sends a slash command it
	// does not know to the model as a prompt.
	Fence map[string]string `yaml:"fence,omitempty"`

	// Text is where the command's JSON carries the screen's text.
	Text Path `yaml:"text,omitempty"`

	// Turns is where the command's JSON counts the model turns it ran; any
	// but zero refuses the read.
	Turns Path `yaml:"turns,omitempty"`

	// Windows are the lines that report a window, each named by how it opens.
	Windows []PlanLine `yaml:"windows,omitempty"`

	// UsedSuffix follows the whole percent of the window used ("% used").
	UsedSuffix string `yaml:"used_suffix,omitempty"`

	// Reset reads the instant the window turns over out of its line.
	Reset ResetReading `yaml:",inline"`
}

// PlanLine is one line of a [PlanRead]: the text it opens with and the
// window it reports.
type PlanLine struct {
	Opens      string `yaml:"opens,omitempty"`
	WindowName `yaml:",inline"`
}

// validatePlanRead holds [Profile.PlanRead] to a read the engine can run and
// trust: in a login home, with a turn count to refuse a model's answer and a
// fence that names no credential.
func (p *Profile) validatePlanRead(add func(string, ...any)) {
	r := p.PlanRead
	if r == nil {
		return
	}
	if len(r.Args) == 0 {
		add("plan_read.args is empty: name the command that prints the plan's windows")
	}
	if !p.KeepsLogin() {
		add("plan_read is set on a harness that keeps no login: the read runs in the " +
			"entry's login home, and a token or a key has no plan to read")
	}
	if len(r.Text) == 0 || len(r.Turns) == 0 {
		add("plan_read needs text and turns: where the command's JSON carries the screen, " +
			"and the turn count that proves no model answered it")
	}
	for name := range r.Fence {
		if name == p.TokenEnv || name == p.APIKeyEnv || IsCredentialName(name) {
			add("plan_read.fence sets %s, a credential: the fence only keeps model requests "+
				"on this host", name)
		}
	}
	if len(r.Windows) == 0 || strings.TrimSpace(r.UsedSuffix) == "" {
		add("plan_read needs windows and used_suffix: the lines that report a window, " +
			"and what follows the percent used")
	}
	for i, w := range r.Windows {
		if strings.TrimSpace(w.Opens) == "" {
			add("plan_read.windows[%d].opens is empty", i)
		}
		switch w.Window {
		case llm.LimitSession, llm.LimitWeekly:
		case llm.LimitModel:
			if w.Model == "" {
				add("plan_read.windows[%d] is a model's window and names no model", i)
			}
		default:
			add("plan_read.windows[%d].window %q (want %s, %s or %s)", i, w.Window,
				llm.LimitSession, llm.LimitWeekly, llm.LimitModel)
		}
	}
	r.Reset.validate("plan_read", add)
}

// clone is r sharing nothing a caller can write through, nil for nil.
func (r *PlanRead) clone() *PlanRead {
	if r == nil {
		return nil
	}
	out := *r
	out.Args = slices.Clone(r.Args)
	out.Fence = maps.Clone(r.Fence)
	out.Text = slices.Clone(r.Text)
	out.Turns = slices.Clone(r.Turns)
	out.Windows = slices.Clone(r.Windows)
	return &out
}

// PlanLineOf is the window line opens, and false where none of the read's
// lines opens it.
func (r *PlanRead) PlanLineOf(line string) (PlanLine, bool) {
	for _, w := range r.Windows {
		if strings.HasPrefix(line, w.Opens) {
			return w, true
		}
	}
	return PlanLine{}, false
}

// String names the read for a log line.
func (r *PlanRead) String() string { return fmt.Sprintf("%q", strings.Join(r.Args, " ")) }
