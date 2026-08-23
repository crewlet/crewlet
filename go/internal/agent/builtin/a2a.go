package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// A2AAskTool is the tool's wire name.
const A2AAskTool = "a2a_ask"

// Asker is what this tool needs from the A2A service.
//
// An interface so the tool can be built and tested without a broker, and so
// the builtin package does not become a second place that knows how a channel
// is opened.
type Asker interface {
	Open(ctx context.Context, ask a2a.Ask) (string, error)
}

// a2aAsk asks one colleague one question.
//
// ONE ASK, ONE ANSWER, THEN CLOSED — which is what stops a volley. Two agents
// that can keep replying to each other will, and a company where every turn
// can spawn an unbounded conversation has no bound on what a single trigger
// costs. The reply does not charge delegation depth; the ask does.
//
// It does NOT wait for the answer. The target is woken on its own inbox and
// answers in its own turn, on whichever node owns its seat — so blocking here
// would hold a seat open across a network hop for work that may take minutes.
// The answer arrives as a wake on this seat's inbox, which is a later turn.
type a2aAsk struct{ svc Asker }

var _ tools.SeatCallable = (*a2aAsk)(nil)

func (t *a2aAsk) Name() string { return A2AAskTool }

func (t *a2aAsk) Description() string {
	return "Ask one AI colleague one question, in their own turn. Use " +
		"lookup_colleague first to get an exact handle. The colleague is " +
		"woken with your brief and answers asynchronously — this call " +
		"returns as soon as they have been asked, NOT with their answer, " +
		"so finish your turn rather than waiting. AGENTS ONLY: a human " +
		"teammate is reached by mentioning them on a shared surface."
}

func (t *a2aAsk) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "The colleague's exact handle, from lookup_colleague",
			},
			"brief": map[string]any{
				"type": "string",
				"description": "What you need from them, in full. They see " +
					"only this — not your plan, not your conversation — so " +
					"a question that needs context must carry it.",
			},
		},
		"required": []any{"target", "brief"},
	}
}

func (t *a2aAsk) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *a2aAsk) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return failed("a2a_ask can only be called during a turn, on behalf of a seat."), nil
	}
	if t.svc == nil {
		return failed("Agent-to-agent messaging is not configured on this deployment."), nil
	}

	target := strings.TrimSpace(argString(args, "target"))
	brief := strings.TrimSpace(argString(args, "brief"))
	switch {
	case target == "":
		return failed("a2a_ask needs a `target` handle. Use lookup_colleague to find one."), nil
	case brief == "":
		return failed("a2a_ask needs a `brief`. The colleague sees only this — " +
			"not your plan and not your conversation — so say what you need in full."), nil
	}

	// RESOLVED, not trusted. A model writes the handle it remembers, and an
	// ask sent to a handle that does not exist is a turn spent waiting for
	// an answer that can never come — with nothing failing.
	resolved, why := t.resolve(turn, target)
	if why != "" {
		return failed(why), nil
	}
	if resolved.Kind == string(org.KindHuman) {
		// A human seat is addressable and never spawned, so no turn will
		// ever answer this channel. Saying so, and saying what to do
		// instead, is the difference between a redirected agent and one
		// that waits forever.
		return failed(fmt.Sprintf(
			"%s (%s) is a human teammate, so a2a_ask cannot reach them — "+
				"no turn answers on their behalf. Mention them on a shared "+
				"surface instead and continue without waiting.",
			resolved.Name, resolved.Handle)), nil
	}
	if resolved.Handle == seat.Handle() {
		// A channel to yourself has no responder: the answering side
		// decides who replies by comparing the woken seat against the
		// requester, so a self-ask wakes this seat, is read as an incoming
		// ANSWER, and is never replied to — a turn spent on nothing.
		return failed("a2a_ask cannot address your own seat."), nil
	}

	id, err := t.svc.Open(ctx, a2a.Ask{
		Requester: seat.Handle(), Target: resolved.Handle, Brief: brief,
		SenderRole: seat.Name,
		// The chain travels so the answering seat can refuse past the cap
		// rather than discovering the loop at runtime — an A2A ask is a
		// delegation, and an unbounded one is how two agents ask each
		// other the same question until a budget runs out.
		DelegationDepth: turn.Depth + 1,
		DelegationChain: append(append([]string(nil), turn.Chain...), seat.Handle()),
		ParentTurnID:    turn.ID,
	})
	if err != nil {
		return failed(fmt.Sprintf("Could not reach %s: %v", resolved.Handle, err)), nil
	}
	return tools.Result{Output: fmt.Sprintf(
		"Asked %s (%s). They answer in their own turn — their reply arrives "+
			"as a new message to you, so finish this turn rather than waiting. "+
			"Channel: %s",
		resolved.Handle, resolved.Name, id)}, nil
}

// resolve turns a model's spelling into a real seat, or explains why not.
func (t *a2aAsk) resolve(turn *turnctx.Turn, target string) (colleague.Seat, string) {
	if turn.Org == nil {
		return colleague.Seat{}, "No organization is in scope, so there is nobody to ask."
	}
	found := colleague.Resolve(target, Corpus(turn.Org))
	switch len(found) {
	case 0:
		return colleague.Seat{}, fmt.Sprintf(
			"No colleague matches %q. Call lookup_colleague to find the right handle.",
			clip(target))
	case 1:
		return found[0].Seat, ""
	}
	var names []string
	for _, c := range found {
		names = append(names, c.Seat.Handle)
		if len(names) == displayLimit {
			break
		}
	}
	return colleague.Seat{}, fmt.Sprintf(
		"%q is ambiguous — it could be %s. Ask again with one exact handle.",
		clip(target), strings.Join(names, ", "))
}
