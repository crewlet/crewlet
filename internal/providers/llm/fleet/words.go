package fleet

import (
	"errors"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// What a move did to the work in flight, in a person's words.
const (
	// WorkBetweenTurns is a pick away from the home: no turn was running.
	WorkBetweenTurns = "Between turns: its next turn started on the new member"
	// WorkWaits is a call no member could take: the seat parks.
	WorkWaits = "Its turn finished on the work done so far; the next one waits for room"
	// WorkMidTurn follows the task's name: a call handed on mid-turn.
	WorkMidTurn = " was mid-turn: it continued on the new member from its last step, nothing redone"
)

// WhyFailed is why a member stopped taking a call, in a person's words.
func WhyFailed(err error) string {
	var failure *llm.Error
	switch {
	case llm.KindOf(err) == llm.KindAuth:
		return "Its sign-in was refused"
	case llm.KindOf(err) != llm.KindRateLimit:
		return "Its call failed: " + llm.KindOf(err).String()
	case errors.As(err, &failure) && failure.Window == llm.LimitWeekly:
		return "Reached its weekly limit"
	case errors.As(err, &failure) && failure.Window == llm.LimitSession:
		return "Reached its session limit"
	}
	return "Reached its usage limit"
}
