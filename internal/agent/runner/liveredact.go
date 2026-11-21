package runner

import (
	"reflect"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
)

// A live frame's words are REDACTED BEFORE THEIR END IS TAKEN, as a record's
// are before it is cut (recordredact.go): an end taken first can open inside a
// credential, and the piece of it left is a head or tail no rule knows. A
// text's end is read [redact.LookBack] past what is shown, and only what
// that context settles is shown ([redact.SettledEnd]), as the executor's live
// tail shows a stream; the model's copy is never changed.

// shownEnd is the redaction of text's end, read bound bytes and
// [redact.LookBack] more from its end, whether any of text before it is left
// out, and whether any after it is held back. open says text is still being
// written, or ended where it was cut (a round in flight, an attempt
// abandoned): only its settled lines are shown ([redact.Settled]), since half
// a token is no shape a rule knows, and what is not settled yet is held back.
func shownEnd(text string, bound int, open bool) (end string, elided, held bool) {
	from := max(len(text)-bound-redact.LookBack, 0)
	reading, lead := text[from:], 0
	if from > 0 {
		lead = redact.LookBack
	}
	if open {
		settled := redact.Settled(reading)
		reading, held = reading[:settled], settled < len(reading)
	}
	end, before := redact.SettledEnd(reading, lead)
	return end, from+before > 0, held
}

// heldEnd is end within bound bytes as [textcut.Tail] keeps it, never cut
// inside a marker the redaction wrote ([redact.MarkersAround]), which would
// read as the text's own words: such a cut moves on to where the markers end.
// Marked where it was cut, or where elided says text before end is left out.
func heldEnd(end string, bound int, elided bool) string {
	if len(end) > bound {
		if _, after, inside := redact.MarkersAround(end, len(end)-bound); inside {
			return tailMarker + end[after:]
		}
		return textcut.Tail(end, bound)
	}
	if elided {
		return tailMarker + end
	}
	return end
}

// tail is the end of a text whole as it stands (a phase's answer, a call's),
// redacted, within [partialTail] bytes.
func tail(text string) string {
	end, elided, _ := shownEnd(text, partialTail, false)
	return heldEnd(end, partialTail, elided)
}

// writingTail is the end of the round in flight, redacted, within
// [partialTail] bytes: its settled lines alone, the line being written held
// until it ends, and whole on the round's prose once the round completes.
func writingTail(text string) string {
	end, elided, _ := shownEnd(text, partialTail, true)
	return heldEnd(end, partialTail, elided)
}

// liveWords is what a phase's live frames have redacted, kept by what it is
// of: every frame, five times a second, shows the phase's answer, its calls,
// its rounds' prose, its calls in flight and its abandoned attempts again,
// and most of them have not changed since the frame before. Each is redacted
// once, and again only where what it was redacted from is not what the frame
// holds now: a text compared whole (the same string compares at once), a
// call's arguments by the map the loop hands on and never changes.
type liveWords struct {
	mu       sync.Mutex
	answer   liveText
	writing  liveText
	calls    []liveCall
	prose    []liveText
	attempts []liveAttempt
	running  map[string]liveRunning
}

// liveText is a text a frame showed and what it showed of it.
type liveText struct {
	set        bool
	was, shown string
}

// of is what a frame shows of text by show, redacted again only where text is
// not what it was.
func (m *liveText) of(text string, show func(string) string) string {
	if !m.set || m.was != text {
		*m = liveText{set: true, was: text, shown: show(text)}
	}
	return m.shown
}

// liveArgs is a call's arguments a frame showed and their redaction.
type liveArgs struct {
	set        bool
	was, shown map[string]any
}

// of is args redacted as values ([tools.RedactedArgs]), again only where args
// is not the map it was.
func (m *liveArgs) of(args map[string]any) map[string]any {
	if !m.set || !sameArgs(m.was, args) {
		*m = liveArgs{set: true, was: args, shown: tools.RedactedArgs(args)}
	}
	return m.shown
}

// sameArgs reports whether a and b are one map: a call's arguments are handed
// on by the loop from frame to frame, never copied or changed (the model's
// copy), and the map compared is held here, so no other is given its place.
func sameArgs(a, b map[string]any) bool {
	return len(a) == len(b) && reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// liveCall is one call's answer and arguments as frames show them.
type liveCall struct {
	outputBytes int
	output      liveText
	args        liveArgs
}

// liveRunning is a call in flight's arguments as frames show them, redacted
// and written as JSON text.
type liveRunning struct {
	was       map[string]any
	arguments string
}

// liveAttempt is one abandoned attempt's end as frames show it, within budget.
type liveAttempt struct {
	budget int
	end    liveText
}

// response is the phase's answer as its frame shows it ([tail]).
func (w *liveWords) response(text string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.answer.of(text, tail)
}

// executions is the phase's calls as its frame shows them: each answer's end
// ([liveOutput]) and its arguments redacted as values, a copy.
func (w *liveWords) executions(execs []toolloop.Execution) []types.ToolExecution {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = resized(w.calls, len(execs))
	shown := make([]toolloop.Execution, len(execs))
	for i, ex := range execs {
		c := &w.calls[i]
		if c.outputBytes != ex.OutputBytes {
			*c = liveCall{outputBytes: ex.OutputBytes}
		}
		shown[i] = ex
		shown[i].Output = c.output.of(ex.Output, func(out string) string { return liveOutput(out, ex.OutputBytes) })
		shown[i].Args = c.args.of(ex.Args)
	}
	return toolExecutions(shown)
}

// resized is s holding n entries, those past its length zero.
func resized[T any](s []T, n int) []T {
	if n <= len(s) {
		clear(s[n:])
		return s[:n]
	}
	return append(s, make([]T, n-len(s))...)
}

// liveOutput is a call's answer as a frame shows it: its end, redacted
// ([tail]), or, for one its run's log kept only the opening of (outputBytes
// its whole's size), that opening's record ([recordedCut]) held to the same
// bound.
func liveOutput(output string, outputBytes int) string {
	if outputBytes > 0 {
		return heldEnd(recordedCut(output, outputBytes), partialTail, false)
	}
	return tail(output)
}

// narration is the phase's committed rounds' prose as its frame shows it,
// each round's redacted whole, as its record holds it ([recordedNarration]).
func (w *liveWords) narration(narr []toolloop.Narration) []types.RoundNarration {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prose = resized(w.prose, len(narr))
	shown := slices.Clone(narr)
	for i := range shown {
		shown[i].Content = w.prose[i].of(narr[i].Content, redact.Secrets)
	}
	return roundNarration(shown)
}

// runningCalls renders the calls in flight, their arguments redacted as values
// ([tools.RedactedArgs]) and kept by call, or nil when none is, so the key is
// absent on every frame but those published while a call runs.
func (w *liveWords) runningCalls(calls []toolloop.RunningCall) []types.RunningCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(calls) == 0 {
		w.running = nil
		return nil
	}
	kept := make(map[string]liveRunning, len(calls))
	out := make([]types.RunningCall, len(calls))
	for i, c := range calls {
		r, ok := w.running[c.ID]
		if !ok || !sameArgs(r.was, c.Args) {
			r = liveRunning{was: c.Args, arguments: encodeArgs(tools.RedactedArgs(c.Args))}
		}
		out[i] = types.RunningCall{
			ID: c.ID, Round: c.Round, Name: c.Name, Arguments: r.arguments, StartedAt: c.StartedAt.UTC(),
		}
		kept[c.ID] = r
	}
	w.running = kept
	return out
}

// partialRound renders the round currently being written ([writingTail]), or
// nil rather than an empty object, so `omitempty` drops the key: absent reads
// as "no round is open", empty as "open and silent".
func (w *liveWords) partialRound(p *toolloop.Partial) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p == nil {
		w.writing, w.attempts = liveText{}, nil
		return nil
	}
	out := map[string]any{
		"round":   p.Round,
		"content": w.writing.of(p.Content, writingTail),
	}
	if len(p.Abandoned) > 0 {
		// THE NEWEST ATTEMPTS, EACH ITS END within its even part of their
		// share (liveTail): a reader saw each one arrive and die, and the
		// round's own text replaces them all once it completes.
		attempts := slices.Clone(p.Abandoned[max(len(p.Abandoned)-liveAbandonedAttempts, 0):])
		each := liveAbandonedBytes/len(attempts) - narrationRowRoom
		w.attempts = resized(w.attempts, len(attempts))
		for i := range attempts {
			a := &w.attempts[i]
			if a.budget != each {
				*a = liveAttempt{budget: each}
			}
			attempts[i].Content = a.end.of(attempts[i].Content, func(s string) string { return liveTail(s, each) })
		}
		out["abandoned"] = roundNarration(attempts)
	}
	return out
}
