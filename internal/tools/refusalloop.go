package tools

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// PolicyRefusalLimit is how many policy refusals IN A ROW one write tool may
// answer for one target before its answer tells the model, in plain words, to
// stop retrying.
//
// FOUR, because a write refused on its policy is fixed in one or two attempts
// when the refusal names what to change (every refusal of a closed format
// does, and the format is one read away), so a fourth refusal in a row says
// the model is not converging: it is guessing, or probing the policy for a
// way round it. The GoPhrase brand turn (GP-38) made fourteen such attempts
// and spent eight minutes on them before anybody saw it (ADR-0059).
const PolicyRefusalLimit = 4

var loopLog = logging.Get("tools.refusals")

// Counter is where a surface counts what it decided, consumer-defined and one
// method wide: the node's metrics recorder answers it.
type Counter interface {
	Add(name string, n uint64, attrs metrics.Attrs)
}

// WithCounter installs the counter the refusal-loop guard records into
// ([metrics.ToolPolicyRefusalLoops]) and returns the surface. Nil counts
// nothing, which is a test's and a validate command's case.
func (s *Surface) WithCounter(c Counter) *Surface {
	s.counter = c
	return s
}

// Targeted is a write tool whose every call acts on one target (an artifact,
// a work item), named from its arguments, so the refusal-loop guard counts a
// run of refusals per target: refusals across four artifacts are four first
// attempts, not a loop, and a success on one does not end a loop on another.
// A tool that is not Targeted, or a call naming no target (""), is counted per
// tool.
type Targeted interface {
	RefusalTarget(args map[string]any) string
}

// Wrapper is a tool that wraps another (an authority gate), which a surface
// looks through for what the wrapped tool declares ([TargetOf]).
type Wrapper interface {
	Unwrap() Callable
}

// TargetOf is the target a call of tool acts on, through every [Wrapper]
// around it, or "" (the whole tool) for a tool that is not [Targeted].
func TargetOf(tool Callable, args map[string]any) string {
	for tool != nil {
		if t, ok := tool.(Targeted); ok {
			return t.RefusalTarget(args)
		}
		w, ok := tool.(Wrapper)
		if !ok {
			return ""
		}
		tool = w.Unwrap()
	}
	return ""
}

// streakKey is one run of refusals: a tool, and the target it acted on.
type streakKey struct{ tool, target string }

// settle records one call a tool answered, deciding the refusal-loop guard in
// the same hold of the surface's lock as the record, so two calls answered at
// once (a response's calls run side by side) cannot read the same count. It
// answers the call as recorded, which is what the model reads, and the run of
// policy refusals it ends or extends (0 when it is not one).
//
// THE RUN IS CONSECUTIVE: a policy refusal ([RefusalInvalid]) of a tool not
// annotated read-only extends its target's run, a success of the tool on that
// target ends it, and any other answer (stale, conflict, forbidden, each with
// its own instruction) leaves it as it was. From the [PolicyRefusalLimit]th
// refusal in a row, the answer carries the instruction to stop.
//
// NEVER A SILENT CAP. The call ran and its refusal is answered whole; the
// guard adds the sentence a model needs to stop and refuses nothing itself,
// so a model that does fix the version on its fifth attempt still publishes.
func (s *Surface) settle(c Call, ann Annotations, target string) (Call, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := 0
	if ann.ReadOnly != mcp.Yes {
		key := streakKey{tool: c.Name, target: target}
		switch {
		case !c.Failed:
			delete(s.streaks, key)
		case c.Refusal() == RefusalInvalid:
			if s.streaks == nil {
				s.streaks = map[streakKey]int{}
			}
			s.streaks[key]++
			run = s.streaks[key]
		}
	}
	if run >= PolicyRefusalLimit {
		c.Output += stopRetrying(c.Name, target, run)
	}
	s.called = append(s.called, c)
	return c, run
}

// stopRetrying is the instruction a run of refusals at the limit ends with.
//
// IT NAMES NEITHER THE TURN NOR A WORK ITEM AS GIVEN: the run is this phase's
// (a resumed phase counts again), and a turn may have no work item, so the
// blocker is raised where there is one and said to whoever asked where there
// is not.
func stopRetrying(tool, target string, run int) string {
	on := ""
	if target != "" {
		on = " on " + target
	}
	return fmt.Sprintf("\n\nSTOP RETRYING %s%s. It has refused your last %d calls to it on its "+
		"policy, so another guess will not land. Do not probe it with other values or look "+
		"for a way round it. If you are working on a work item, raise a blocker on it; "+
		"otherwise tell whoever asked. Say what you were making, what you tried and the last "+
		"refusal, so a person can unblock it. Then finish what does not depend on it and end "+
		"the turn.", tool, on, run)
}

// reportLoop logs and counts a run of refusals the moment it reaches the
// limit, ONCE per run: an answer past the limit repeats the instruction to
// the model, but the run is one loop to an operator, however long it goes.
func (s *Surface) reportLoop(tool, target string, run int, turn *turnctx.Turn) {
	if run != PolicyRefusalLimit {
		return
	}
	attrs := []any{"tool", tool, "target", target, "refusals", run, "phase", s.Phase()}
	if turn != nil {
		attrs = append(attrs, "turn", turn.RunID, "work_key", turn.WorkKey)
	}
	loopLog.Warn("tool_policy_refusal_loop", attrs...)
	if s.counter != nil {
		s.counter.Add(metrics.ToolPolicyRefusalLoops, 1, metrics.Attrs{"tool": tool})
	}
}
