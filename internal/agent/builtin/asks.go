package builtin

import (
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// asksParam is the `asks` argument every tool that posts an agent's words
// requires of it, a chat message or a work item comment alike: its own
// statement that the words ask something (ADR-0113). Not in the schema's
// required list, because a person's words ask whatever they say.
func asksParam() map[string]any {
	return map[string]any{
		"type": "boolean",
		"description": "Required of an agent. true when this asks someone for an " +
			"answer or an action: a question, a request, a hand-off. The colleagues " +
			"you @-mention, or the other side of a direct conversation, must then " +
			"answer. false for news, status notes, approvals, acknowledgements and " +
			"thanks: an @-mention is then a reference that wakes no agent, and only " +
			"whoever's question it answers and anyone with an open question in the " +
			"thread are woken.",
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
