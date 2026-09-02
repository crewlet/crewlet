// Package runner drives the three phases against real models and real tools.
//
// It is the wiring between four things that each know nothing about the
// others: the prompt builder, the tool registry, the provider chain, and the
// turn loop's [turn.Phases] contract. Everything here is plumbing plus the one
// rule plumbing cannot express: what happens when the model never gives a
// structured answer at all.
//
// What a structured answer IS, and the three rules for capturing one, are
// [github.com/crewlet/crewlet/internal/agent/structured]'s. What counts as a
// VALID one stays here, in the decoders below, because that belongs to the
// phase doing the asking.
package runner

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// The names of the two structured-output tools. A phase ENDS by calling its
// own; that is what turns free prose into a decision the engine can act on.
const (
	SubmitWorkTool   = "submit_work"
	SubmitReviewTool = "submit_review"
)

// workPayload is the wire shape of a submitted piece of work.
type workPayload struct {
	Outcome       string   `json:"outcome"`
	Summary       string   `json:"summary"`
	Deliveries    []string `json:"deliveries"`
	Evidence      string   `json:"evidence"`
	OpenQuestions string   `json:"open_questions"`
}

// decodeWork validates a submission against the engine's own record of the
// turn.
//
// A CLOSURE over that record, not a plain function, because the two questions
// worth asking cannot be answered from the arguments alone: whether a cited
// tool was really called, and whether anybody is waiting for this turn. The
// first is the engine's log; the second is the trigger's own type. Both are
// facts the model does not control, which is the whole point — the account a
// phase gives of itself is exactly what a phase can get wrong.
//
// Checking HERE rather than after the phase means a wrong claim costs one
// bounced tool call inside the loop, which the model can fix, instead of a
// whole review round or a silently accepted no-op.
func decodeWork(reply turn.Reply, called func() []ledger.Call, surface func() turn.Surface,
) func(map[string]any) (workPayload, error) {
	return func(args map[string]any) (workPayload, error) {
		var w workPayload
		if err := structured.Remarshal(args, &w); err != nil {
			return w, err
		}
		switch turn.Outcome(w.Outcome) {
		case turn.OutcomeDelivered, turn.OutcomeNoAction, turn.OutcomeBlocked:
		case "":
			// An absent outcome is `delivered`, the common case and the
			// one every other field is written for. It is also the safe
			// default: the engine checks a delivery claim against the
			// record either way, so a forgotten field costs a correction
			// at worst, while refusing the submission outright throws
			// away a round of real work over the most predictable
			// omission.
			w.Outcome = string(turn.OutcomeDelivered)
		default:
			return w, fmt.Errorf(
				"outcome must be one of delivered, no_action or blocked, got %q", w.Outcome)
		}
		if strings.TrimSpace(w.Summary) == "" {
			return w, fmt.Errorf("summary is required: say what you did")
		}
		switch turn.Outcome(w.Outcome) {
		case turn.OutcomeNoAction:
			if reply.Awaited() {
				// SILENCE IS NOT A DECLINE. The requester cannot tell it
				// from a message that was lost, and the engine knows one
				// arrived — so this is refused where the model can still
				// act on it rather than corrected a phase later.
				return w, fmt.Errorf("this turn was asked for directly, so no_action is not " +
					"available: reply where you were asked — even to decline — and report " +
					"that as delivered")
			}
		case turn.OutcomeBlocked:
			if strings.TrimSpace(w.Evidence) == "" {
				return w, fmt.Errorf("blocked needs evidence: what did you try, and what " +
					"stopped you")
			}
		case turn.OutcomeDelivered:
			if err := citations(w.Deliveries, reply, called(), surface()); err != nil {
				return w, err
			}
		}
		return w, nil
	}
}

// citations checks that a delivery claim names calls that actually happened.
//
// Only where a TOOL was the way to deliver. A colleague's ask is answered by
// the engine on the channel it opened, and an unaddressed turn owes nobody a
// posted answer, so demanding a citation in either case would loop a turn that
// did exactly the right thing.
//
// The refusal lists what IS citable, because the failure this catches is
// usually a model naming the tool it meant to call rather than one it did, and
// a bare "no" sends it round the same loop.
func citations(cited []string, reply turn.Reply, calls []ledger.Call, s turn.Surface) error {
	if reply != turn.ReplyTool {
		return nil
	}
	var eligible []string
	for _, c := range calls {
		if !c.Failed && turn.Deliverable(c.Name, s) && !slices.Contains(eligible, c.Name) {
			eligible = append(eligible, c.Name)
		}
	}
	for _, name := range cited {
		if slices.Contains(eligible, name) {
			return nil
		}
	}
	slices.Sort(eligible)
	if len(eligible) == 0 {
		return fmt.Errorf("nothing has been delivered yet: no tool that acts outside the " +
			"engine has been called successfully in this turn. Call the one that delivers " +
			"on the surface this arrived from — `list_mcp_server_tools` and " +
			"`activate_tool` will find it — before reporting the work delivered")
	}
	return fmt.Errorf("deliveries names no call that delivered: cite one of %s, "+
		"or report the outcome honestly if none of them is the delivery",
		strings.Join(eligible, ", "))
}

// reviewPayload is the wire shape of a submitted review.
type reviewPayload struct {
	Decision      string `json:"decision"`
	Notes         string `json:"notes"`
	CompletedWork string `json:"completed_work"`
	FinalArtifact string `json:"final_artifact"`
}

func decodeReview(args map[string]any) (reviewPayload, error) {
	var r reviewPayload
	if err := structured.Remarshal(args, &r); err != nil {
		return r, err
	}
	switch r.Decision {
	case "done", "self_iterate", "failed":
	case "":
		// An absent decision is `done`. The alternative — defaulting to
		// self_iterate — spends another whole round on a review that
		// simply forgot a field, and the engine still overturns a `done`
		// that delivered nothing.
		r.Decision = "done"
	default:
		return r, fmt.Errorf("decision must be one of done, self_iterate or failed, got %q", r.Decision)
	}
	if r.Decision == "self_iterate" && strings.TrimSpace(r.Notes) == "" {
		// A loop-back with no correction sends the next round to do
		// exactly what the last one did. The stall guard would eventually
		// catch it, but only after spending the rounds.
		return r, fmt.Errorf("self_iterate needs notes saying what the next round should do differently")
	}
	return r, nil
}
