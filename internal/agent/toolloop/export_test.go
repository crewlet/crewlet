package toolloop

import (
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// JoinWait is [joinWait], for a test that holds a run to finishing without it.
const JoinWait = joinWait

// RunDispatcher is what the session of the latest run host served hands each
// call to, for a case that has to reach a call past the session's transport —
// one already on its way in when the run was released.
func RunDispatcher(host llm.ToolHost) toolsession.Dispatcher {
	d := host.(*dispatcher)
	d.mu.Lock()
	defer d.mu.Unlock()
	return &runCalls{d: d, r: d.runs[len(d.runs)-1]}
}

// RecordedCall is one call on the dispatcher's own record.
type RecordedCall struct {
	Seq    int
	Name   string
	Result ToolResult
}

// Recorded is the record the dispatcher serving host keeps of every call it
// has answered — what it settled each with, refusals and their class included
// — for the contract suite (toolsessiontest), which reads the bridge's off its
// ledger. The phase's record folds this into its conversation, where a model
// reads only the sentence.
func Recorded(host llm.ToolHost) []RecordedCall {
	d := host.(*dispatcher)
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []RecordedCall
	for _, c := range d.calls {
		if c.settled {
			out = append(out, RecordedCall{Seq: c.seq, Name: c.recorded(), Result: c.result})
		}
	}
	return out
}
