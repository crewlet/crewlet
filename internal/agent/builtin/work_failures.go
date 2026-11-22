package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// unknownWrite explains a tracker write whose outcome is unknown, as a failed
// result, never a receipt carrying `outcome: unknown`: a `comment_id` or
// `version` beside it reads as the change made. It names the caller's `op_id`
// where there is one ([sameCall]; a step's id handed back would be a new call's
// operation), else `opID`. Its cause is [UnknownOutcome] ([ErrOutcomeUnknown]).
func unknownWrite(actor Actor, tool, what, opID string, unvouched bool,
	next string) tools.Result {

	why := "the write's acknowledgement was lost"
	if unvouched {
		why = "this node's operation ledger may have lost the record of this " +
			"operation, so this node cannot tell"
	}
	if actor.Operation != "" {
		opID = actor.Operation
	}
	return failedUnknown(opID, unvouched, fmt.Sprintf("%s: whether %s is "+
		"unknown (%s; operation %s). It may have landed and it may not — do "+
		"not report it as done, and do not report it as failed. %s", tool,
		what, why, opID, strings.TrimSpace(next)))
}

// unknownNext says what a caller told `unknown` does next, from whether this
// node's ledger vouches for the operation and whether the caller has a repeat
// that IS the same operation (`again`, from [sameCall], or ""). That repeat
// finishes a lost acknowledgement; an unvouched one's is safe but answers alike
// until the write reaches this node, so looking says sooner. With no such repeat
// the caller looks first: `twice` is what a new operation does if the first
// landed. A reworded call is always the duplicate. `look` is an imperative
// clause naming the read that shows it.
func unknownNext(unvouched bool, again, look, twice string) string {
	switch {
	case unvouched && again != "":
		return look + " before doing anything else about it. To repeat it, " +
			again + ": that is safe — it writes nothing this node cannot vouch " +
			"for — but answers the same way until the write reaches this node. " +
			"Never make it again under different arguments: if the first call " +
			"landed, " + twice + "."
	case again != "":
		return capitalize(again) + ": the retry is the same operation, answers " +
			"with what landed, and lands once if it did not. Do not reword it: " +
			"a call with different arguments is a new operation, and if the " +
			"first one landed, " + twice + "."
	case unvouched:
		return look + " before making it again: this node cannot tell, and if " +
			"the first call landed, " + twice + "."
	}
	return look + " before making it again: a second call is a new operation, " +
		"and if the first one landed, " + twice + "."
}

// restateNext is [unknownNext] for a write that STATES a value (a catalogue
// list, a project's tags and policy): any repeat is harmless, so it is offered,
// never promised as "the same operation", which an operator's assistant's repeat
// is not.
func restateNext(look string) string {
	return look + " to see whether it did. Making the same call again is " +
		"harmless: it states the same thing again, so it changes nothing if " +
		"the first one landed."
}

// ---- shared rendering -------------------------------------------------- //

// jsonResult renders a value as the tool's output: JSON, since a model parsing a
// rendered table back gets fields wrong. It is [jsonAnswer] for answers bounded
// by their own shape (a receipt, a refusal); the ceiling still applies, since a
// new field is how one stops being small.
func jsonResult(v any) (tools.Result, error) {
	return jsonAnswer(v, "Narrow what you asked for.")
}

// ReadRefusalSentence is a read's refusal of what it was asked (an item or page
// that is not there, a query the tracker cannot run, a floor, cursor or `since`
// on another log) without the package names ("worklet: ", "doclet: ",
// "statelog: ") wherever in the message they sit. Exported for
// internal/api/workapi and internal/api/queries.
func ReadRefusalSentence(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"worklet: ", "doclet: ", "statelog: "} {
		msg = strings.ReplaceAll(msg, prefix, "")
	}
	return msg
}

// readFailure explains a read that could not be served, and never says "nothing
// found": a copy behind its log must not tell a seat there is no work, which it
// would duplicate or abandon.
//
// A read refused for what it asked ([worklet.ErrBadQuery],
// [statelog.ErrForeignPosition]) is [tools.RefusalInvalid]: refused alike every
// time. Everything else is this node's ([Condition]): a CONDITION is
// [tools.RefusalUnavailable]; a FAULT is [tools.RefusalInternalError], its error
// (a path, a SQL fragment) kept for the log ([faulted]). The error rides beneath
// the class ([tools.Classify]).
func readFailure(ctx context.Context, name string, err error) tools.Result {
	if errors.Is(err, worklet.ErrBadQuery) || errors.Is(err, statelog.ErrForeignPosition) {
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused "+
			"what it was asked: %s. Nothing is wrong with Worklet; change the "+
			"arguments, because the same ones are refused again.", name,
			ReadRefusalSentence(err)))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"read Worklet%s (%s). This is NOT an empty result: do not "+
			"conclude the item or the list does not exist. %s", name,
			rightNow(err), why,
			conditionNext(err, "Try again, or say you could not check.", stuckRead)))
	}
	return faulted(ctx, name, err, fmt.Sprintf("%s could not read Worklet: "+
		"%s. This is NOT an empty result: do not conclude the item or the list "+
		"does not exist. Say you could not check.", name, faultSaid))
}

// writeFailure explains a write that did not land, in terms the model can act
// on. Each sentinel has its own class, which a person's surface acts on: a stale
// version is re-read, a lost race retried, a spent hand-off budget never retried.
//
// A content refusal is MARKED and the unmarked is the node's: the writer wraps
// [worklet.ErrInvalid] (what was asked) or [worklet.ErrState] (what the object is
// now) on every refusal, and types [worklet.TextCapError]. The node's is a
// CONDITION ([tools.RefusalUnavailable]) or a FAULT ([tools.RefusalInternalError],
// logged by [faulted]), as in [readFailure]. The actor is for [sameCall].
func writeFailure(ctx context.Context, actor Actor, name string, err error) tools.Result {
	// The error rides beneath the class, and a gesture stopped at an unknown
	// step carries that unknown ([writeCause]).
	out := func(class tools.Refusal, msg string) tools.Result {
		return refusedBy(class, writeCause(actor, err), msg)
	}
	if reused := reusedOperation(actor, name, err); reused != "" {
		return out(tools.RefusalInvalid, reused)
	}
	var clash *worklet.TagClash
	var full *worklet.TagsFull
	switch {
	case errors.As(err, &clash) && name == worklet.WriteProjectTool:
		// The declaration itself: a collision ([worklet.ErrTaken]), never a
		// malformed label.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused, and nothing was written: %s already "+
			"has a tag %s labelled %q, and %s (%q) would be a second tag nobody "+
			"could tell apart from it. Use %s if it means the same thing, or "+
			"give %s a label of its own.", name, clash.Project,
			clash.Other.Slug, clash.Other.Label, clash.Slug, clash.Label,
			clash.Other.Slug, clash.Slug))
	case errors.As(err, &clash):
		// A label two projects use for different tags, met declaring tags
		// before the first append; named with the two ways past it.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused, and nothing was written: it has to "+
			"declare the tag %s (%q) in %s, where the tag %s is already labelled "+
			"%q — two tags nobody could tell apart. If they mean the same thing, "+
			"put the items under %s instead ("+UpdateWorkItemTool+"'s `labels`); if "+
			"not, declare %s in %s yourself under a label of its own "+
			"("+worklet.WriteProjectTool+" on %s with tags_add: [{\"slug\": %q, \"label\": "+
			"\"<a label %s does not use>\"}]). Then make this call again.",
			name, clash.Slug, clash.Label, clash.Project, clash.Other.Slug,
			clash.Other.Label, clash.Other.Slug, clash.Slug, clash.Project,
			clash.Project, clash.Slug, clash.Project))
	case errors.As(err, &full):
		// The project's state ([worklet.ErrState]): the add lands once a tag is
		// archived.
		return out(tools.RefusalConflict, fmt.Sprintf("%s was refused, and nothing was written: %s already "+
			"keeps %d tags, the most a project may, and %s is not among them. "+
			"Archive tags %s no longer files under ("+worklet.WriteProjectTool+"'s "+
			"tags_archive — its lead's decision), or take %s off the items with "+
			UpdateWorkItemTool+", then make this call again.", name, full.Project,
			worklet.MaxTagsPerProject, full.Slug, full.Project, full.Slug))
	case errors.Is(err, worklet.ErrNoTask):
		return out(tools.RefusalNotFound, fmt.Sprintf("%s: %s.", name, worklet.Sentence(err)))
	case errors.Is(err, worklet.ErrForbidden):
		// Not a race: re-reading changes nothing; the sentence names who may.
		return out(tools.RefusalForbidden, fmt.Sprintf("%s was refused: "+
			"%s. Re-reading will not change this — ask whoever the refusal "+
			"names.", name, worklet.Sentence(err)))
	case errors.Is(err, statelog.ErrConflict):
		// A lost race, in words, not the broker path; other domains use
		// [statelog.LostRace] ([WriterSentence]), a person's surface its code's
		// sentence (ADR-0060).
		return out(tools.RefusalConflict, fmt.Sprintf("%s could not land: somebody "+
			"else is editing this item, so nothing was written. Read it again with "+
			GetWorkItemTool+" and decide from what it says now.", name))
	case errors.Is(err, statelog.ErrExists):
		// Most often an earlier attempt's: the same create sent again once its
		// operation was forgotten.
		return out(tools.RefusalExists, fmt.Sprintf("%s was refused: what it creates "+
			"is already there, so nothing was written. Look for it with "+
			ListWorkItemsTool+", or read it with "+GetWorkItemTool+", rather than "+
			"making it again.", name))
	case errors.Is(err, worklet.ErrStaleVersion):
		// The one refusal answered "read it again": a model told only "it
		// failed" would re-send the same patch against the moved item forever.
		return out(tools.RefusalStaleVersion, fmt.Sprintf("%s was "+
			"refused: %s. Nothing is wrong with your edit — somebody "+
			"changed the item after you read it. Call "+GetWorkItemTool+" again "+
			"and decide from what it says now.", name, worklet.Sentence(err)))
	case errors.Is(err, worklet.ErrAlreadyAnswered):
		// Read that answer, never resend this one.
		return out(tools.RefusalAlreadyAnswered, fmt.Sprintf("%s was "+
			"refused: %s. Nothing was posted.", name, worklet.Sentence(err)))
	case errors.Is(err, inbox.ErrFull):
		// Names `read_through`, the one move that empties these lists.
		return out(tools.RefusalInboxFull, fmt.Sprintf("%s was refused: "+
			"%s.", name, inbox.Sentence(err)))
	case errors.Is(err, estate.ErrOutcomeUnknown):
		// No data node answered: a silent one may have run it, so "NOT made" is
		// false; the same OPERATION again settles it ([sameCall]). Fixed words
		// ([Unanswered]), not the router's node list. Ahead of the condition
		// below, which would call a cancelled caller's case "NOT made".
		return tools.Result{Output: fmt.Sprintf("%s: %s. It may have landed and "+
			"it may not — do not report it as done, and do not report it as "+
			"failed. %s", name, Unanswered, unknownNext(false, sameCall(actor, name),
			"Read what it writes with "+GetWorkItemTool+" or "+ListWorkItemsTool,
			"that is a second write")), Failed: true, Cause: writeCause(actor, err)}
	case errors.Is(err, worklet.ErrStepUnvouched):
		// An ErrStepUnresolved, so first: a retry here stops at the same step,
		// the ledger row it needs being the one the loss took.
		return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some of "+
			"it may already have landed, and this node cannot tell which — "+
			"calling %s again here stops at the same step. Do not report it as "+
			"done or as failed, and do not redo it by other means: read the "+
			"items it touches with "+GetWorkItemTool+", and say which part is "+
			"unconfirmed.", name, worklet.Sentence(err), name))
	case errors.Is(err, worklet.ErrStepUnresolved):
		// A walking gesture (move, merge, dependency) stopped at an unknown step:
		// neither "NOT made" nor "done". The same OPERATION again finishes it
		// ([sameCall]).
		again := sameCall(actor, name)
		if again == "" {
			return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some "+
				"of it may already have landed. Calling %s again is a new "+
				"operation here and does not finish this one: read the items "+
				"it touches with "+GetWorkItemTool+" and finish what is missing with "+
				"the tool for each. Do not report it as done.", name, worklet.Sentence(err), name))
		}
		return out(tools.RefusalUnavailable, fmt.Sprintf("%s stopped part of the way through: %s. Some of "+
			"it may already have landed. %s — the retry answers what landed "+
			"and finishes the rest. Do not report it as done until a call "+
			"answers without this error.", name, worklet.Sentence(err), capitalize(again)))
	case errors.Is(err, worklet.ErrReassignmentBudget):
		// Must not invite another attempt: the item is circulating between
		// agents, and a transient-sounding message keeps it circulating.
		return out(tools.RefusalReassignmentBudget, fmt.Sprintf("%s was "+
			"refused: %s. Do not reassign it again. Comment on the item "+
			"saying what is blocking it and who you think should own it, "+
			"and leave it where it is.", name, worklet.Sentence(err)))
	}
	var capped *worklet.TextCapError
	if errors.As(err, &capped) {
		return out(tools.RefusalInvalid, textCapRefusal(capped))
	}
	if errors.Is(err, worklet.ErrState) {
		// What the object is now (archived project, trashed task, set at its
		// cap), the writer's sentence naming who changes it: re-reading will not
		// help.
		return out(tools.RefusalConflict, fmt.Sprintf("%s refused that: %s",
			name, worklet.Sentence(err)))
	}
	if errors.Is(err, worklet.ErrInvalid) {
		// What was asked, in the writer's sentence, which names what to change
		// (ADR-0060 for a person's surface).
		return out(tools.RefusalInvalid, fmt.Sprintf("%s refused that: %s",
			name, worklet.Sentence(err)))
	}
	if class, ok := WriterClass(err); ok {
		// Any other domain refusal ([WriterClass]), never told as the node
		// breaking.
		return out(class, domainRefusal(name, err))
	}
	if why, ok := Condition(err); ok {
		return out(tools.RefusalUnavailable, sentence(fmt.Sprintf("%s did not land (%s). "+
			"The change was NOT made — do not report it as done.", name, why),
			conditionNext(err, "", stuckWrite)))
	}
	// Not "NOT made": a fault after the append was acknowledged (ledger or gates
	// unreadable) leaves a durable record nobody here can account for, so claim
	// neither way and forbid the move that could make a second copy.
	return faulted(ctx, name, err, fmt.Sprintf("%s failed: %s. Do not report "+
		"it as done, and do not make it again by other means — say it could "+
		"not be made.", name, faultSaid))
}

// textCapRefusal says which of a task's texts is too long and by how much, for a
// model and a person alike (the invalid class is shown verbatim). Composed from
// the typed fields: the writer's Go error carries a prefix and the id of a task
// never created.
func textCapRefusal(e *worklet.TextCapError) string {
	argument, holds := "`title`", "a task's title"
	switch e.Field {
	case worklet.TextBody:
		argument, holds = "`body`", "a task's description"
	case worklet.TextComment:
		argument, holds = "`body`", "a comment"
	}
	return fmt.Sprintf("%s is %d bytes and %s holds at most %d, so nothing was "+
		"changed. Shorten it, or put the long form on a page and link it here "+
		"— a value is refused rather than cut.", argument, e.Size, holds, e.Limit)
}

// reusedOperation explains a write refused because the caller's operation
// already names a write to another object, or is "" for any other error.
//
// A brought-back `op_id` carries its call's arguments ([WorkDeps.bindOperation]),
// but the world can move under them (a move aliases the item's CURRENT key), and
// the state log then refuses the step ([statelog.ReasonOpReused]). The remedy is
// an operation of its own: leave `op_id` out, or for a request a new key
// ([sameCall]'s split). A seat has none to leave out, so it gets "".
func reusedOperation(actor Actor, tool string, err error) string {
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonOpReused {
		return ""
	}
	where := string(refusal.Reason)
	if refusal.Detail != "" {
		where += ": " + refusal.Detail
	}
	if actor.keyedRequest() {
		return fmt.Sprintf("%s was refused, and this request's change was NOT "+
			"made: the operation its Idempotency-Key names already wrote to "+
			"something else (%s) — what it names has changed since the request "+
			"was first sent — so it cannot finish that request here. Read the "+
			"item again, and send this as a new request, under a new key.",
			tool, where)
	}
	if actor.Operation == "" {
		return ""
	}
	return fmt.Sprintf("%s was refused, and this call's change was NOT made: "+
		"the operation `op_id` %q names already wrote to something else (%s) — "+
		"what this call names has changed since the call that op_id was "+
		"answered for — so it cannot finish that call here. Read the item "+
		"again, and leave `op_id` out to make this as a new write.",
		tool, actor.Operation, where)
}

// sameCall says how this caller makes THIS call again as the same operation, as
// a clause, or "" where no repeat is. A SEAT repeats the same arguments before
// any different call ([Actor.callKey]); a REQUEST resends under its key
// ([Actor.keyedRequest]); an OPERATOR brings back its `op_id`; any other caller
// mints afresh, so a repeat is a second write.
func sameCall(actor Actor, tool string) string {
	switch {
	case actor.Operation != "":
		return fmt.Sprintf("call %s again with exactly the same arguments and "+
			"`op_id` %q", tool, actor.Operation)
	case actor.keyedRequest():
		return fmt.Sprintf("send the same %s request again — under the same "+
			"Idempotency-Key, with exactly the same arguments", tool)
	case actor.OperationSeed() != "":
		return fmt.Sprintf("call %s again with exactly the same arguments, "+
			"before calling it with any others", tool)
	}
	return ""
}

// writeTold is how a write's error reads inside a sentence naming what it
// stopped, classed as every write's error is ([writeFailure]): a fault in
// [faultSaid], logged there; anything else in its own words ([toldOf]).
func writeTold(ctx context.Context, actor Actor, tool string, err error) string {
	if err == nil {
		return "it named no reason"
	}
	return toldOf(writeFailure(ctx, actor, tool, err), err)
}

// capitalize upper-cases a clause's first letter, ASCII only ([sameCall]'s).
func capitalize(clause string) string {
	if clause == "" || clause[0] < 'a' || clause[0] > 'z' {
		return clause
	}
	return string(clause[0]-'a'+'A') + clause[1:]
}
