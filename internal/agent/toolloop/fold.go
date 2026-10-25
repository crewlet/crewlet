package toolloop

import (
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The fold: what a CLI run did, turned into the loop's own record — the
// conversation, the executions, the narration and the rounds — in the shape
// the loop kept when it drove each round itself.
//
// THE SAME SHAPE ON PURPOSE. Everything downstream joins on it: the ledger
// reads the executions, the reviewer the text, a resume the conversation, the
// dashboard the rounds with their calls and their prose beside them. A run is
// folded as the rounds it was made of, each model request one round with the
// calls it asked for and their results after it, so a reader cannot tell a
// phase the CLI drove from one the loop drove, except by the [Invocation]
// entries that say so.

// folded is one fold's yield.
type folded struct {
	messages   []llm.Message
	executions []Execution
	narration  []Narration
	rounds     []Round

	// called reports a call to the phase's tools in the folded part — what
	// clears the stall allowances.
	called bool

	// lastText is the prose of the folded part's last response: the answer
	// a run that ended on its own ended with.
	lastText string
}

// group is one round of a fold: the response the CLI's record showed, where it
// showed one, and the calls placed on it.
type group struct {
	round int
	turn  *observedTurn
	calls []*callRec
	// text stands in for a response no record showed: the run's answer.
	text string
}

// fold turns everything since the last fold into the loop's record and
// advances past it.
func (d *dispatcher) fold(comp *llm.Completion, model string) folded {
	d.mu.Lock()
	defer d.mu.Unlock()
	if comp != nil && len(d.turns) == d.foldedTurns {
		// A RUN WHOSE RECORD SHOWED NO REQUEST — a CLI that records none, or
		// one that answered without calling anything — is one round, whose
		// prose is its answer. Allocated here, once the run is over, unless
		// a call already placed one.
		if r := d.lastRunLocked(); r == nil || r.synthetic == 0 || !d.hasCallsFromLocked(d.foldedCalls) {
			d.rounds++
			if r != nil {
				r.synthetic = d.rounds
			}
		}
	}
	out := d.foldLocked(comp, model, false)
	d.foldedTurns, d.foldedCalls = len(d.turns), len(d.calls)
	return out
}

// snapshot is what the run in flight has done so far, for a live frame:
// nothing is advanced, and a call still running is left to [dispatcher.running].
func (d *dispatcher) snapshot() folded {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.foldLocked(nil, "", true)
}

// Pending's fold — see [dispatcher.Pending].
func (d *dispatcher) foldLockedPending() []llm.Message {
	return d.foldLocked(nil, "", true).messages
}

func (d *dispatcher) hasCallsFromLocked(from int) bool { return len(d.calls) > from }

// foldLocked builds the record of turns and calls past the folded marks. live
// leaves out what has not settled.
func (d *dispatcher) foldLocked(comp *llm.Completion, model string, live bool) folded {
	groups := map[int]*group{}
	get := func(round int) *group {
		g := groups[round]
		if g == nil {
			g = &group{round: round}
			groups[round] = g
		}
		return g
	}
	for _, t := range d.turns[d.foldedTurns:] {
		get(t.round).turn = t
	}
	var out folded
	for _, c := range d.calls[d.foldedCalls:] {
		get(c.round).calls = append(get(c.round).calls, c)
		if c.admitted {
			out.called = true
		}
	}
	if r := d.lastRunLocked(); comp != nil && r != nil && r.synthetic > 0 {
		// The run's answer goes on its synthetic round, which no record
		// showed.
		if g := groups[r.synthetic]; g == nil || g.turn == nil {
			get(r.synthetic).text = comp.Content
		}
	}

	order := make([]int, 0, len(groups))
	for round := range groups {
		order = append(order, round)
	}
	slices.Sort(order)
	for _, round := range order {
		g := groups[round]
		d.foldGroup(g, comp, model, live, &out)
	}
	return out
}

// foldGroup folds one round.
func (d *dispatcher) foldGroup(g *group, comp *llm.Completion, model string, live bool, out *folded) {
	text := g.text
	if g.turn != nil {
		text = g.turn.text
	}

	// The calls of the round, in the order the model asked for them where
	// the record showed it, then any it did not.
	type entry struct {
		call   llm.ToolCall
		record *callRec
	}
	var entries []entry
	if g.turn != nil {
		for i, tc := range g.turn.calls {
			if !tc.Host {
				continue
			}
			var rec *callRec
			for _, c := range g.calls {
				if c.turn == g.turn && c.slot == i {
					rec = c
				}
			}
			id := tc.ID
			if rec != nil {
				id = rec.id
			}
			entries = append(entries, entry{
				call:   llm.ToolCall{ID: id, Name: tc.Name, Arguments: tc.Arguments},
				record: rec,
			})
		}
	}
	for _, c := range g.calls {
		if c.turn == g.turn && g.turn != nil {
			continue
		}
		entries = append(entries, entry{
			call:   llm.ToolCall{ID: c.id, Name: c.name, Arguments: c.args},
			record: c,
		})
	}

	trimmed := strings.TrimSpace(text)
	if trimmed != "" {
		out.narration = append(out.narration, Narration{Round: g.round, Content: trimmed})
		out.lastText = trimmed
	} else {
		out.lastText = ""
	}

	assistant := llm.Message{Role: llm.RoleAssistant, Content: text}
	var answers []llm.Message
	for _, e := range entries {
		rec := e.record
		if rec != nil && live && !rec.settled {
			continue
		}
		assistant.ToolCalls = append(assistant.ToolCalls, e.call)
		switch {
		case rec == nil:
			// A call the model asked for that never reached the phase:
			// the run was ended before the CLI made it.
			answers = append(answers, llm.Message{
				Role: llm.RoleTool, ToolCallID: e.call.ID, Name: e.call.Name, Failed: true,
				Content: e.call.Name + " was NOT run: the run ended before the call was made.",
			})
		case rec == d.suspended:
			// THE PARKED CALL STAYS UNANSWERED — exactly one dangling
			// call is the invariant a resume checks.
		default:
			answers = append(answers, llm.Message{
				Role: llm.RoleTool, Content: rec.result.Output, ToolCallID: rec.id,
				Name: rec.name, Failed: rec.result.Failed,
			})
		}
		if rec != nil && rec != d.suspended && rec.settled {
			out.executions = append(out.executions, Execution{
				Round: g.round, Name: rec.name, Args: rec.args, CallID: rec.id,
				Output: rec.result.Output, Failed: rec.result.Failed,
				StartedAt: zeroIfRefused(rec, rec.started), Duration: rec.took,
				Origin: rec.result.Origin, Server: rec.result.Server,
			})
		}
	}
	if assistant.Content != "" || len(assistant.ToolCalls) > 0 {
		out.messages = append(out.messages, assistant)
		out.messages = append(out.messages, answers...)
	}

	round := Round{Round: g.round, Model: model, ToolCalls: len(entries)}
	if g.turn != nil {
		round.StartedAt = g.turn.start.UTC()
		round.Duration = g.turn.at.Sub(g.turn.start)
		if g.turn.model != "" {
			round.Model = g.turn.model
		}
		if u := g.turn.usage; u != nil {
			round.InputTokens, round.OutputTokens = u.Input, u.Output
			round.CacheRead, round.CacheWrite = u.CacheRead, u.CacheWrite
		}
		round.StopReason = g.turn.stop
	} else if comp != nil && !live {
		// A round no record showed is the whole run: what it cost is the
		// run's, and when the model worked within it is not known — zero
		// means "not measured".
		round.InputTokens, round.OutputTokens = d.spent.Input, d.spent.Output
		round.CacheRead, round.CacheWrite = d.spent.CacheRead, d.spent.CacheWrite
	}
	if round.StopReason == "" {
		round.StopReason = llm.StopEnd
		if len(entries) > 0 {
			round.StopReason = llm.StopToolUse
		}
	}
	out.rounds = append(out.rounds, round)
}

// zeroIfRefused is a call's start, or zero for one that never ran — zero is
// "not measured", never "instant".
func zeroIfRefused(c *callRec, started time.Time) time.Time {
	if c.refused || started.IsZero() {
		return time.Time{}
	}
	return started.UTC()
}

// roundsUsed is how many model requests this loop invocation has made.
func (d *dispatcher) roundsUsed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rounds
}

// reopen readies the dispatcher for the next CLI run, after a run that was
// ended for a reason the next one answers.
func (d *dispatcher) reopen() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.ended() || d.state == stateExhausted {
		return
	}
	d.state = stateOpen
	select {
	case <-d.done:
		d.done = make(chan struct{})
	default:
	}
	d.parked = nil
	d.invCalls = len(d.calls)
}

// running is the calls in flight, for a live frame.
func (d *dispatcher) running() []RunningCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []RunningCall
	for _, c := range d.calls {
		if c.admitted && !c.settled && !c.started.IsZero() {
			out = append(out, RunningCall{
				ID: c.id, Round: c.round, Name: c.name, Args: c.args, StartedAt: c.started.UTC(),
			})
		}
	}
	return out
}

// awaitSettled waits for every call the dispatcher admitted to settle.
func (d *dispatcher) awaitSettled() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		pending := false
		for _, c := range d.calls {
			if !c.settled {
				pending = true
				break
			}
		}
		if !pending {
			return
		}
		d.cond.Wait()
	}
}

// chargeUnreported charges a run's cost from its answer when the backend
// reported none of it while it ran.
func (d *dispatcher) chargeUnreported(u llm.Usage) {
	d.mu.Lock()
	reported := d.spent != (llm.Usage{})
	d.mu.Unlock()
	if reported || u.Total() <= 0 {
		return
	}
	d.Spent(u)
}

// takeSpent is what the run reported spending, and resets it for the next.
func (d *dispatcher) takeSpent() llm.Usage {
	d.mu.Lock()
	defer d.mu.Unlock()
	used := d.spent
	d.spent = llm.Usage{}
	return used
}

// invocation is the record of the run that just ended.
func (d *dispatcher) invocation(began time.Time, took time.Duration, comp *llm.Completion,
	err error, used llm.Usage) Invocation {
	d.mu.Lock()
	defer d.mu.Unlock()
	inv := Invocation{
		StartedAt: began.UTC(), Duration: took,
		InputTokens: used.Input, OutputTokens: used.Output,
		CacheRead: used.CacheRead, CacheWrite: used.CacheWrite,
	}
	if r := d.lastRunLocked(); r != nil {
		inv.RequestsObserved = r.opts.ObservesRequests
	}
	for _, c := range d.calls[d.invCalls:] {
		if c.admitted {
			inv.Calls++
		}
	}
	if comp != nil {
		inv.ProviderKey, inv.Model = comp.ProviderKey, comp.Model
	}
	switch {
	case d.state != stateOpen:
		inv.Outcome = OutcomeStopped
	case err != nil:
		inv.Outcome = OutcomeFailed
	case comp != nil && comp.StopReason == llm.StopCap:
		inv.Outcome = OutcomeCap
	default:
		inv.Outcome = OutcomeEnd
	}
	return inv
}

// current is the phase's state and, for a cut, why.
func (d *dispatcher) current() (dstate, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.cutErr
}

// pending is the parked call's id and name.
func (d *dispatcher) pending() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.suspended == nil {
		return "", ""
	}
	return d.suspended.id, d.suspended.name
}
