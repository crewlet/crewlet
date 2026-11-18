// Package runner drives a turn's phases (onboarding, the executor and the
// reviewer) against real models and real tools.
//
// The executor is never a phase of the engine's tool loop: it is the seat's
// harness's own run in a box, launched here with the turn parking behind
// it, and collected back into the same turn for the reviewer (ADR-0066;
// agentrun.go).
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
//
// # A continued session's words and measure
//
// A relaunch that continues a kept capture taken while its job still worked
// ([SessionBasis.WentOn]) tells the session it was saved then, when
// ([SessionBasis.CapturedAt]), and that its run went on past it
// (prompts.SavedSession), in place of the words that say only that it was cut
// off: told those, the run takes the session's end for where its work stopped.
// And each session a relaunch may continue is held to its own measure: a kept
// session to what its own capture measured ([SessionBasis.ContextTokens]),
// never the copy's, and the job's copy in its own box to its newest capture's
// measure, kept or not ([SessionBasis.CopyContext]), or its record's where
// larger. One figure for both would hold one session to what the other sends.
package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// The names of the two structured-output tools. A phase ENDS by calling its
// own; that is what turns free prose into a decision the engine can act on.
const (
	SubmitWorkTool   = toolname.SubmitWork
	SubmitReviewTool = toolname.SubmitReview
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
// turn: a closure, because whether a cited tool was really called and whether
// anybody is waiting are facts the model does not control. Checked HERE, a
// wrong claim costs one bounced call the model can fix, not a review round or
// an accepted no-op.
//
// `called` is the TURN's record, closed rounds included ([turn.Record]), never
// the phase's: a delivery an earlier round made is citable, since the
// prior-work ledger forbids making it again.
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
			// Absent is `delivered`: the claim is checked against the
			// record either way, so a forgotten field costs a correction
			// at worst, where refusing it throws away a round of work.
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
				// SILENCE IS NOT A DECLINE: the requester cannot tell it
				// from a lost message, so it is refused while the model
				// can still act.
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

// typedPickup is decode held to a work pickup's typed answer (ADR-0122): a
// submission reporting the work delivered or blocked while the turn's record
// shows nothing typed on the handed item, and the item still the seat's, is
// refused, naming the five answers, while the run can still make one.
// [turn.Check] and [turn.OverrideDone] say the same after the phase, so what
// is refused here is refused there too.
//
// FIRST, before decode's checks, so a run that wrote nothing is told what
// answers the pickup rather than that any delivery would do. answer reads the
// record at the submission, under the call's context.
func typedPickup(reply turn.Reply, answer func(context.Context) types.PickupAnswer,
	decode func(map[string]any) (workPayload, error),
) func(context.Context, map[string]any) (workPayload, error) {
	return func(ctx context.Context, args map[string]any) (workPayload, error) {
		if reply.Item == "" {
			return decode(args)
		}
		outcome, _ := args["outcome"].(string)
		switch turn.Outcome(outcome) {
		case "", turn.OutcomeDelivered, turn.OutcomeBlocked:
			if !answer(ctx).Accepted() {
				return workPayload{}, errors.New(turn.PickupUntyped)
			}
		}
		return decode(args)
	}
}

// keptWhole is decode for a submission a run is MAKING, held to what the run's
// log keeps whole. The collection replays a submission from that log
// ([replaySubmission]), which keeps the newest one whole only up to
// sandbox.MaxSubmissionArgsBytes, so a longer one would collect as no
// submission: it is refused here, while the run can shorten it, past
// sandbox.MaxSubmissionBytes as the log would hold it. That is half what the
// log keeps, since the run's own spelling of the canonical form can only add
// to it, and never past the other half. The size is stated rounded up
// (kept.SizeUp), so a byte past never reads as the bound itself.
//
// LIVE ONLY: the replay decodes without it, as the submission is already made.
func keptWhole(decode func(map[string]any) (workPayload, error)) func(map[string]any) (workPayload, error) {
	return func(args map[string]any) (workPayload, error) {
		text, err := tools.RecordArgs(args)
		if err == nil {
			if n := kept.Quoted(text); n > sandbox.MaxSubmissionBytes {
				return workPayload{}, fmt.Errorf("this submission is %s as the run's record holds it, and "+
					"the record keeps a submission whole up to %s: shorten the summary, the evidence or "+
					"the open questions (the long form belongs where you delivered the work) and submit "+
					"again", kept.SizeUp(n), kept.Size(sandbox.MaxSubmissionBytes))
			}
		}
		return decode(args)
	}
}

// makingWork is decode for a submission a run is MAKING: [decodeWork]'s rules,
// then a no_action refused once the turn's record shows it acted outside the
// engine ([turn.Acted]), saying what it did ([turn.ActedAccount]). One bounced
// call here saves [turn.Check]'s correction a relaunched run and a review. On
// an awaited turn decodeWork's refusal of any no_action speaks first.
//
// LIVE ONLY: the replay checks against EVERY call the round made
// ([Runner.resumeAgentRun]), those after the submission included, so a
// no_action made before acting would collect as no submission, its account
// lost. Accepted on replay, [turn.Check] corrects it by the same rule.
func makingWork(reply turn.Reply, called func() []ledger.Call, surface func() turn.Surface,
) func(map[string]any) (workPayload, error) {
	decode := decodeWork(reply, called, surface)
	return func(args map[string]any) (workPayload, error) {
		w, err := decode(args)
		if err != nil || turn.Outcome(w.Outcome) != turn.OutcomeNoAction {
			return w, err
		}
		if account := turn.ActedAccount(called(), surface()); account != "" {
			return w, fmt.Errorf("no_action is not available: %s. Report what this turn did, as "+
				"delivered, or as blocked with what stopped you", account)
		}
		return w, nil
	}
}

// NotDeliveredOn is the clause each of [citations]' surface-named refusals
// opens with: a turn that claims delivery while nothing reached the surface it
// was asked on.
//
// EXPORTED, because a test that scripts a model must recognise the refusal,
// and a copied sentence drifts silently.
func NotDeliveredOn(surface string) string {
	return fmt.Sprintf("nothing has been delivered on `%s` yet, which is where this was asked",
		surface)
}

// citations checks that a delivery claim names calls that actually happened,
// only where a TOOL was the way to deliver: a colleague's ask is answered by
// the engine, and an unaddressed turn owes nobody a post, so a citation there
// would loop a turn that did right.
//
// The refusal lists what IS citable: the usual failure is a model naming the
// tool it meant to call, and a bare "no" sends it round the same loop.
//
// ELIGIBILITY IS SURFACE-SCOPED by [turn.DeliveredTo]'s rule, so what is
// refused at submission is refused after the review too.
func citations(cited []string, reply turn.Reply, calls []ledger.Call, s turn.Surface) error {
	if reply.Kind != turn.ReplyTool {
		return nil
	}
	countable := turn.DeliverersFor(s, reply)
	var eligible []string
	for _, c := range calls {
		if !c.Failed && slices.Contains(countable, c.Name) && !slices.Contains(eligible, c.Name) {
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
		// THE REMEDY NAMES THE TOOLS ([turn.Remedy]), not discovery, which
		// can lead to a server where nothing could count the post.
		remedy := turn.Remedy(s, reply)
		on := reply.Surface
		named := on != "" && s.Reaches(on)
		acted := turn.ActedWith(calls, s)
		switch {
		case named && turn.KnownDelivered(calls, s):
			// A write elsewhere is NAMED, or "no tool was called" reads as
			// false and the model argues rather than acts. Known, not
			// presumed ([turn.KnownDelivered]): an unhinted call falls to
			// the next case.
			return fmt.Errorf("%s — a call that reached somewhere else does not "+
				"answer the person waiting. %s Do that before reporting the work "+
				"delivered", NotDeliveredOn(on), remedy)
		case named && acted != "":
			// ACTED, AND REACHED NOBODY WAITING (an ask, a reaction, a
			// publish): told what it called ([turn.ActedWith]), since "no
			// call reached anybody" would be false.
			return fmt.Errorf("%s: this turn called %s, but none of that reached the person "+
				"waiting there. %s Do that before reporting the work delivered",
				NotDeliveredOn(on), acted, remedy)
		case named:
			// Only when TRUE, or the model looks for a write it never made.
			return fmt.Errorf("%s: no call in this turn has reached anybody. %s Do "+
				"that before reporting the work delivered", NotDeliveredOn(on), remedy)
		case acted != "":
			return fmt.Errorf("nothing has been delivered yet: this turn called %s, but none of "+
				"that delivers anything to the person waiting. %s Do that before reporting the "+
				"work delivered", acted, remedy)
		}
		return fmt.Errorf("nothing has been delivered yet: no tool that acts outside the "+
			"engine has been called successfully in this turn. %s Do that before "+
			"reporting the work delivered", remedy)
	}
	return fmt.Errorf("deliveries names no call that delivered: cite one of %s, "+
		"or report the outcome honestly if none of them is the delivery",
		words.Or(eligible))
}

// reviewPayload is the wire shape of a submitted review.
type reviewPayload struct {
	Decision      phase.Decision `json:"decision"`
	Notes         string         `json:"notes"`
	CompletedWork string         `json:"completed_work"`
	FinalArtifact string         `json:"final_artifact"`
}

func decodeReview(args map[string]any) (reviewPayload, error) {
	var r reviewPayload
	if err := structured.Remarshal(args, &r); err != nil {
		return r, err
	}
	// AGAINST [phase.ReviewDecisions], never a spelling of it, so a rename or
	// an addition reaches this and the schema's enum ([reviewDecisionEnum])
	// together. phase.Skipped is absent on purpose: a skip is the ENGINE's
	// reading of a round nobody waited on ([turn.Check]).
	switch {
	case r.Decision == "":
		// Absent is `done`: self_iterate would spend a round on a forgotten
		// field, and the engine still overturns a `done` that delivered
		// nothing.
		r.Decision = phase.Done
	case !slices.Contains(phase.ReviewDecisions(), r.Decision):
		return r, fmt.Errorf("decision must be one of %s, %s or %s, got %q",
			phase.Done, phase.SelfIterate, phase.Failed, r.Decision)
	}
	if r.Decision == phase.SelfIterate && strings.TrimSpace(r.Notes) == "" {
		// A loop-back with no correction repeats the last round; the stall
		// guard would catch it only after spending the rounds.
		return r, fmt.Errorf("self_iterate needs notes saying what the next round should do differently")
	}
	if err := reviewWithinBounds(r); err != nil {
		return r, err
	}
	return r, nil
}

// The most a review's words may weigh, in bytes. See [reviewWithinBounds].
const (
	// finalArtifactBytes is a run's own report's bound
	// (sandbox.MaxRunTextBytes): the artifact stands in for that output.
	finalArtifactBytes = sandbox.MaxRunTextBytes

	// reviewProseBytes bounds the notes and completed_work each, as an
	// executor's whole submission is (sandbox.MaxSubmissionBytes): a
	// correction is a few paragraphs, and a document belongs where the work
	// was delivered.
	reviewProseBytes = sandbox.MaxSubmissionBytes
)

// reviewWithinBounds refuses a review whose words are past their bounds,
// naming the field, its size and its bound, while the reviewer can still
// shorten them.
//
// REFUSED WHERE THEY ARE MADE, never cut where they are read: the final
// artifact and the notes each reach several readers (the record's response
// and `plan_summary`, a colleague's reply, memory; the next round's brief and
// prior-work ledger), and cut at each, every reader would hold a different
// opening of one answer.
func reviewWithinBounds(r reviewPayload) error {
	for _, field := range []struct {
		name  string
		text  string
		bound int
		fix   string
	}{
		{"final_artifact", r.FinalArtifact, finalArtifactBytes,
			"put the long form where you delivered the work and hand back what it says"},
		{"notes", r.Notes, reviewProseBytes, "say what the next round should do differently in fewer words"},
		{"completed_work", r.CompletedWork, reviewProseBytes, "say in a sentence what already landed"},
	} {
		if n := len(field.text); n > field.bound {
			return fmt.Errorf("%s is %s, and a review's %s is kept up to %s: %s, and submit again", field.name,
				kept.SizeUp(n), field.name, kept.Size(field.bound), field.fix)
		}
	}
	return nil
}
