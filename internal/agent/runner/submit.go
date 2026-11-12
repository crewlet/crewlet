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
//
// `called` is the TURN's record, the closed rounds included ([turn.Record]),
// never the phase's own: a delivery an earlier round made is citable, because
// the prior-work ledger forbids the round to make it again. Read per phase, a
// round sent back for a re-read had eleven citations of its own turn's comment
// refused, and ran out of rounds saying so.
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

// typedPickup is decode held to a work pickup's typed answer (ADR-0122): a
// submission reporting the work delivered or blocked while the turn's record
// shows nothing typed on the handed item, and the item still the seat's, is
// refused, naming the five answers, while the run can still make one.
// [turn.Check] and [turn.OverrideDone] say the same after the phase, so what
// is refused here is refused there too.
//
// FIRST, before the decode's own checks: a pickup's run that wrote nothing is
// told what would answer the pickup rather than that any delivery would do.
// answer reads the record at the submission, under the call's own context.
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
// log keeps whole.
//
// THE COLLECTION REPLAYS A RUN'S SUBMISSION FROM ITS LOG ([replaySubmission]),
// the bridged call log kept beside the run's row, so a submission is the run's
// outcome only as the log holds it, and the log is bounded in bytes: it keeps
// the newest successful submission whole up to sandbox.MaxSubmissionArgsBytes,
// and every other call's arguments with their largest values left out. A
// submission past what it keeps would be collected as a round that submitted
// nothing. So the tool refuses it here, while the run can still shorten it,
// naming its size: past sandbox.MaxSubmissionBytes, measured as the log would
// hold it (its arguments as the engine records them and the log escapes them),
// which is half what the log keeps — the run's own spelling of what the tool
// reads in canonical form can only add to it, and never past the other half.
// The size is stated rounded up (internal/kept's SizeUp), so a submission a
// byte past the bound never reads as the bound itself.
//
// LIVE ONLY. The replay decodes without it: the submission is made, and one a
// build before this bound recorded whole is replayed as it stands.
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

// makingWork is decode for a submission a run is MAKING: every rule
// [decodeWork] holds a submission to, and then a no_action refused once the
// turn's record shows it acted outside the engine ([turn.Acted]), saying what
// it did ([turn.ActedAccount]: a call the engine only presumes wrote named as
// one) — one bounced call inside the round, where [turn.Check]'s correction
// of the same claim relaunches the round in a new run and spends a review
// after it. On a turn somebody is waiting on, decodeWork's own refusal of any
// no_action speaks first.
//
// LIVE ONLY, as [keptWhole] is, and for a reason of its own: the collection
// replays a submission against EVERY call the round made
// ([Runner.resumeAgentRun]), those the run made after submitting included. A
// no_action the run made before it acted would be refused there, and the round
// collected as one that submitted nothing — rescued as incomplete, its account
// lost, while the lost path's [submitted] reads the same calls as a
// submission. Accepted on replay, the claim reaches [turn.Check], which
// corrects it by the same rule on the same record.
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
// ONE SPELLING, EXPORTED, because a test that scripts a model has to recognise
// the refusal to answer it, and a copy of the sentence drifts silently: the
// end-to-end chat case once matched the surface without the backticks this
// puts round it, never saw the refusal, and answered submit_work until the
// rounds ran out.
func NotDeliveredOn(surface string) string {
	return fmt.Sprintf("nothing has been delivered on `%s` yet, which is where this was asked",
		surface)
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
//
// ELIGIBILITY IS SURFACE-SCOPED, by the same rule [turn.DeliveredTo] applies
// and for the same reason the two must agree: what the engine refuses here it
// must also refuse after the review, or a model is told one thing at
// submission and judged by another. A flat list is how this told a seat to
// "cite one of create_work_item" while a founder waited in a chat thread —
// which would have produced a clean `delivered`, a clean `done`, and the same
// silence.
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
		// THE REMEDY NAMES THE TOOLS ([turn.Remedy]). Pointing every seat at
		// discovery sent one woken on the tracker — whose tools it already
		// held — to the only MCP server it had, to post where nothing could
		// count it.
		remedy := turn.Remedy(s, reply)
		on := reply.Surface
		named := on != "" && s.Reaches(on)
		acted := turn.ActedWith(calls, s)
		switch {
		case named && turn.Delivered(calls, s):
			// NAMED WHEN IT IS KNOWN. "No tool that acts outside the engine
			// was called" reads as plainly false to a model looking at its
			// own successful write to another surface, and a refusal a model
			// does not believe is one it argues with rather than acts on.
			return fmt.Errorf("%s — a call that reached somewhere else does not "+
				"answer the person waiting. %s Do that before reporting the work "+
				"delivered", NotDeliveredOn(on), remedy)
		case named && acted != "":
			// ACTED, AND REACHED NOBODY WAITING THERE: an ask, a reaction, a
			// publish. "No call in this turn has reached anybody", said to
			// it, is false, so it is told what it did call
			// ([turn.ActedWith]), as every correction that finds the turn
			// acted but delivered nothing is.
			return fmt.Errorf("%s: this turn called %s, but none of that reached the person "+
				"waiting there. %s Do that before reporting the work delivered",
				NotDeliveredOn(on), acted, remedy)
		case named:
			// And only when it is TRUE. "A call that reached somewhere else"
			// said to a turn that reached nowhere sends the model looking for
			// a write it never made.
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
	// AGAINST PHASE'S OWN LIST, never a spelling of it. This switch listed
	// "done", "self_iterate" and "failed" as literals, so renaming one of
	// them left this accepting a value nothing else produces; then it
	// listed the constants, which catches a rename but still leaves an
	// ADDITION refused here while the schema offers it.
	// [phase.ReviewDecisions] is the list, and the schema's enum is built
	// from the same call — see [reviewDecisionEnum].
	//
	// phase.Skipped is deliberately absent from it: a skip is the ENGINE's
	// own reading of a round nobody was waiting on (see [turn.Check]), and
	// a reviewer reaching it would mean the turn ran after deciding not to.
	switch {
	case r.Decision == "":
		// An absent decision is `done`. The alternative — defaulting to
		// self_iterate — spends another whole round on a review that
		// simply forgot a field, and the engine still overturns a `done`
		// that delivered nothing.
		r.Decision = phase.Done
	case !slices.Contains(phase.ReviewDecisions(), r.Decision):
		return r, fmt.Errorf("decision must be one of %s, %s or %s, got %q",
			phase.Done, phase.SelfIterate, phase.Failed, r.Decision)
	}
	if r.Decision == phase.SelfIterate && strings.TrimSpace(r.Notes) == "" {
		// A loop-back with no correction sends the next round to do
		// exactly what the last one did. The stall guard would eventually
		// catch it, but only after spending the rounds.
		return r, fmt.Errorf("self_iterate needs notes saying what the next round should do differently")
	}
	if err := reviewWithinBounds(r); err != nil {
		return r, err
	}
	return r, nil
}

// The most a review's words may weigh, in bytes. See [reviewWithinBounds].
const (
	// finalArtifactBytes is what a run's own report may weigh
	// (sandbox.MaxRunTextBytes): the final artifact is what the turn hands
	// back in place of its executor's output, and is never longer than that
	// output may be.
	finalArtifactBytes = sandbox.MaxRunTextBytes

	// reviewProseBytes is what each of a review's notes and its account of
	// what landed may weigh: what an executor's whole submission may
	// (sandbox.MaxSubmissionBytes). A correction the next round can act on
	// is a few paragraphs; one longer is a document, which belongs where the
	// work was delivered.
	reviewProseBytes = sandbox.MaxSubmissionBytes
)

// reviewWithinBounds refuses a review whose words are past their bounds,
// naming the field, its size and its bound, while the reviewer can still
// shorten them.
//
// REFUSED WHERE THEY ARE MADE, never cut where they are read. The final
// artifact is the turn's answer: its record's response and its
// `plan_summary`, the reply a colleague who asked is handed, and the episode
// the seat's memory keeps. The notes are the next round's brief and every
// later round's prior-work ledger, and both ride the review's own record. Cut
// at each of those, every reader would hold a different opening of one
// answer, and none could ask for the rest.
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
