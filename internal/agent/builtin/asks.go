package builtin

import (
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// The `asks` argument every tool posting an agent's words requires of it
// (ADR-0113), worded per surface by what its words wake there. Not in the
// schema's required list, because a person's words ask whatever they say.
type asksSurface struct{ asked, quiet string }

var (
	// chatAsks is a chat message's: a direct conversation's other side
	// answers an ask, and a stake in the thread hears every reply.
	chatAsks = asksSurface{
		asked: "The colleagues you @-mention, or the other side of a direct " +
			"conversation, are woken and must answer.",
		quiet: "whoever's question it answers and anyone with an open question in " +
			"the thread are woken.",
	}
	// workAsks is a work item comment's: an answer wakes its asker, and a
	// stake in the thread hears every reply.
	workAsks = asksSurface{
		asked: "The colleagues you @-mention are woken and must answer; a person " +
			"you ask is asked with `ask` and a `decision`, never in words alone.",
		quiet: "whoever's question it answers (`answers`) and anyone with an open " +
			"question in the thread are woken.",
	}
	// pageAsks is a page comment's: an ask stays open, and every reply in
	// its thread wakes its author, until somebody else answers it.
	pageAsks = asksSurface{
		asked: "The colleagues you @-mention are woken, and every later reply in " +
			"this comment's thread wakes you until one of them answers it with a " +
			"comment that asks nothing (anybody, if you named nobody).",
		quiet: "the agents with an open ask in this comment's thread (or, at the " +
			"top level, the ones you name whose open ask named you) are woken, and " +
			"the asks it answers close.",
	}
)

// asksParam is the `asks` argument as surface s words it.
func asksParam(s asksSurface) map[string]any {
	return map[string]any{
		"type": "boolean",
		"description": "Required of an agent. true when this asks someone for an " +
			"answer or an action: a question, a request, a hand-off. " + s.asked +
			" false for news, status notes, approvals, acknowledgements and " +
			"thanks: an @-mention is then a reference that wakes no agent, and only " +
			s.quiet,
	}
}

// asksOf is the call's `asks`, or, for an agent, the refusal of a call that
// left it out: whether its words ask is the author's to say, never the
// engine's guess. A person's call may leave it out.
func asksOf(tool string, args map[string]any, agent bool) (bool, *tools.Result) {
	asks, typed := argval.Bool(args["asks"])
	if !typed && agent {
		refused := failed(tool + " needs `asks`: true when this asks someone " +
			"for an answer or an action, false otherwise.")
		return false, &refused
	}
	return asks, nil
}
