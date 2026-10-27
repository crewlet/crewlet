package tools

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
)

// PolicyRefusalLimit is how many policy refusals one write tool answers on one
// surface before its answer tells the model, in plain words, to stop retrying.
//
// FOUR, because a write refused on its policy is fixed in one or two attempts
// when the refusal names what to change (every refusal of a closed format
// does, and the format is one read away), so a fourth refusal of the same tool
// says the model is not converging: it is guessing, or probing the policy for
// a way round it. The GoPhrase brand turn (GP-38) made fourteen such attempts
// and spent eight minutes on them before anybody saw it.
const PolicyRefusalLimit = 4

var loopLog = logging.Get("tools.refusals")

// refusalLoop is a write tool's answer once the tool has refused on its policy
// [PolicyRefusalLimit] times or more on this surface: the refusal, unchanged,
// then a plain instruction to stop, raise the blocker on the work item and end
// the branch; and a log line, so an operator sees a loop the turn's own record
// shows only as failed calls.
//
// NEVER A SILENT CAP. The call ran and its refusal is answered whole; the
// guard adds the sentence a model needs to stop, and refuses nothing itself,
// so a model that does fix the version on its fifth attempt still publishes.
//
// A POLICY REFUSAL is [RefusalInvalid] (the arguments are what is wrong) on a
// tool not annotated read-only. A read refused for a bad argument is not a
// loop worth stopping (it changes nothing), and a refusal of any other class
// (stale, conflict, forbidden) has its own answer that says what to do.
func (s *Surface) refusalLoop(tool string, ann Annotations, turn *turnctx.Turn, res Result) Result {
	if !res.Failed || ann.ReadOnly == mcp.Yes || RefusalOf(res) != RefusalInvalid {
		return res
	}
	n := 1 + s.policyRefusals(tool)
	if n < PolicyRefusalLimit {
		return res
	}
	attrs := []any{"tool", tool, "refusals", n, "phase", s.Phase()}
	if turn != nil {
		attrs = append(attrs, "turn", turn.RunID, "work_key", turn.WorkKey)
	}
	loopLog.Warn("tool_policy_refusal_loop", attrs...)
	res.Output += fmt.Sprintf("\n\nSTOP RETRYING %s. It has refused %d calls in this turn on its "+
		"policy, so another guess will not land. Do not probe it with other values or look "+
		"for a way round it. Raise a blocker on the work item instead: say what you were "+
		"making, what you tried and the last refusal, so a person can unblock it. Then "+
		"finish what does not depend on it and end the turn.", tool, n)
	return res
}

// policyRefusals is how many calls of tool this surface has answered with a
// policy refusal so far.
func (s *Surface) policyRefusals(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.called {
		if c.Name == tool && c.Refusal() == RefusalInvalid {
			n++
		}
	}
	return n
}
