package caveat

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// THE REFUSAL CATALOG: the person's words for the refusals a person meets most
// often on the dashboard, answering a decision, filing or editing a work item,
// publishing an artifact an assistant drafted, writing its data past a limit
// on what the data may hold, and merging a change Gitlet refuses for a
// reason the change page's controls cannot say in words of their own. Every
// other refusal is answered with its code's own sentence until it has a
// phrasing here (ADR-0060 names the rest). Each names only what a person
// sees: a handle as `@handle`, a question, an option, a title.

// TitleMissing is a work item filed with no title.
func TitleMissing() Refusal {
	return Refusal{person: "A work item needs a title. Add one, then try again."}
}

// TitleTooLong is a work item's title over the most a title holds.
func TitleTooLong() Refusal {
	return Refusal{person: "The title is too long. Shorten it, and put the " +
		"detail in the description."}
}

// DescriptionTooLong is a work item's description over the most it holds.
func DescriptionTooLong() Refusal {
	return Refusal{person: "The description is too long. Shorten it, or put " +
		"the long form on a page and link to it."}
}

// CommentTooLong is a comment over the most a comment holds.
func CommentTooLong() Refusal {
	return Refusal{person: "The comment is too long. Shorten it, or put the " +
		"long form on a page and link to it."}
}

// NotAQuestion is an answer to a comment that asked nobody anything.
func NotAQuestion() Refusal {
	return Refusal{person: "That comment does not ask anything, so there is " +
		"nothing to answer."}
}

// QuestionRemoved is an answer to a question that was removed.
func QuestionRemoved() Refusal {
	return Refusal{person: "That question was removed, so it can no longer " +
		"be answered."}
}

// AskedSomebodyElse is an answer to a question that was asked of handle, by
// somebody else.
func AskedSomebodyElse(handle string) Refusal {
	return Refusal{person: fmt.Sprintf("This question was asked of @%s, so "+
		"only they can answer it.", handle)}
}

// AnsweredAlready is an answer to an executor's question that another answer
// was recorded against first, by handle. No time: the person's screen says
// when, on their own clock, off the run's record.
func AnsweredAlready(handle string) Refusal {
	if handle == "" {
		return Refusal{person: "This question was already answered, so your answer " +
			"was not used."}
	}
	return Refusal{person: fmt.Sprintf("This question was already answered by @%s, "+
		"so your answer was not used.", handle)}
}

// RefusedReplyFirst is an answer to an executor's question that a reply too
// large for the executor to take reached first, from handle: the reply was
// refused, the executor is resumed told it did not reach it, and it asks again
// if it still needs an answer (ADR-0114). Never told as an answer the executor
// carries on with, which it did not get, nor as a telling already made, which
// is owed until a resumed turn makes it. No time, for [AnsweredAlready]'s
// reason.
func RefusedReplyFirst(handle string) Refusal {
	if handle == "" {
		return Refusal{person: "A reply in the conversation reached this question first and was too " +
			"long for the executor to take, so it was refused, and the executor is resumed told it did " +
			"not reach it. Your answer was not used either; if the executor still needs an answer, it " +
			"asks again."}
	}
	return Refusal{person: fmt.Sprintf("A reply from @%s reached this question first and was too long "+
		"for the executor to take, so it was refused, and the executor is resumed told it did not reach "+
		"it. Your answer was not used either; if the executor still needs an answer, it asks again.",
		handle)}
}

// HeldCallAnswer is a reply or a choice given to an executor that is waiting
// for a call to be approved, which only the call's own answers settle.
func HeldCallAnswer() Refusal {
	return Refusal{person: "This request waits for approval and is answered with Allow once or " +
		"Deny, not with a reply, so nothing was answered."}
}

// QuestionChanged is an answer to an executor's question given against a
// question the run no longer asks: it moved on to another since the person
// read it, which is the one they need to see before they answer.
func QuestionChanged() Refusal {
	return Refusal{person: "The question changed since you read it, so your answer " +
		"was not sent. Reload to see the question it asks now."}
}

// NoQuestionForChoice is a choice made where no open question is addressed to
// the person making it.
func NoQuestionForChoice() Refusal {
	return Refusal{person: "There is no open question for you here, so a " +
		"choice cannot be recorded. Answer in a comment instead."}
}

// QuestionHasNoOptions is a choice made on a question that offers none.
func QuestionHasNoOptions() Refusal {
	return Refusal{person: "This question offers no options to choose from. " +
		"Answer it in words instead."}
}

// NotAnOption is a choice that is not one of the decision's options.
func NotAnOption() Refusal {
	return Refusal{person: "That choice is not one of this decision's " +
		"options. Pick one of the options shown, or answer in words."}
}

// ArtifactBreaksPolicy is a version of an artifact this company's artifact
// policy refuses.
func ArtifactBreaksPolicy() Refusal {
	return Refusal{person: "This version breaks the company's artifact rules, " +
		"so it was not published. Ask for a version that follows them."}
}

// ArtifactQuotaReached is a publish past the company's artifact storage limit.
func ArtifactQuotaReached() Refusal {
	return Refusal{person: "The company's artifact storage is full, so this " +
		"was not published. Remove artifacts nobody needs, or ask an admin to " +
		"raise the limit."}
}

// DatastoreQuotaReached is a write to an artifact's data, or a change to the
// collections it keeps, past a limit on what that data may hold: the
// company's storage settings, a collection's own row caps, or the engine's
// caps on one store's collections and documents. ONE PHRASING FOR EVERY ONE
// OF THEM, and true of each, because the refusal names which only in the
// datastore's reason, which is written for a model and never parsed for a
// person's words.
func DatastoreQuotaReached() Refusal {
	return Refusal{person: "This artifact's data has reached a limit on what " +
		"it can hold, so nothing was saved. Remove what is no longer needed, " +
		"or ask whoever sets the limit to raise it."}
}

// GateCondition is one condition of a merge gate that a change does not meet
// yet, as Gitlet judged it: its kind, by the host's own name for it
// (internal/code's PreconditionKind), and the handles of whoever it waits on.
//
// THE WORDS ARE THIS CATALOG'S: a kind is a value off the host's closed set,
// said here as what the change still waits for, and a kind this build does
// not know is said as another condition of its target's gate, so no text a
// caller hands over becomes a person's words. A handle is said as `@handle`,
// as every handle in this catalog is.
type GateCondition struct {
	Kind    string
	WaitsOn []string
}

// gateWaitsFor is what a change still waits for while each condition of its
// merge gate is unmet, keyed by Gitlet's name for the condition: the
// checklist the change's page draws, said as what is missing.
// builtin.TestEveryGateConditionHasAPersonsWords holds it to the host's set.
var gateWaitsFor = map[string]string{
	"draft":           "its owner to mark it ready for review",
	"facts":           "its measure against its target",
	"approval":        "an approval of its latest revision",
	"human_approval":  "a person's approval for the paths its target keeps for people",
	"owners_approval": "an approval for every path owner it touches",
	"blocker":         "its standing blockers to be cleared",
	"check":           "its required checks to pass on its latest commit",
	"mergeable":       "a clean merge into its target",
	"merge_authority": "somebody who may merge into its target",
	"queue":           "its target's queue to land it",
}

// gateWaitsForUnknown is a condition of a kind this build does not know, which
// Gitlet judges in a newer build.
const gateWaitsForUnknown = "another condition its target's gate sets"

// MergeGateUnmet is a merge, or a place in its target's queue, refused because
// the change does not meet its target's merge gate yet: what it still waits
// for, each condition once, and whoever can move it forward, each once. The
// change's page names every condition with where it stands; these words say
// what stopped this press, which a page read before the gate moved may not.
func MergeGateUnmet(unmet []GateCondition) Refusal {
	const lead = "It does not meet its target's merge gate yet, so nothing was merged or queued."
	var needs, who []words.Said
	for _, c := range unmet {
		need, known := gateWaitsFor[c.Kind]
		if !known {
			need = gateWaitsForUnknown
		}
		if !slices.Contains(needs, words.Said(need)) {
			needs = append(needs, words.Said(need))
		}
		for _, handle := range c.WaitsOn {
			if at := words.Said("@" + handle); handle != "" && !slices.Contains(who, at) {
				who = append(who, at)
			}
		}
	}
	if len(needs) == 0 {
		return Refusal{person: lead + " The change's page names what it still waits for."}
	}
	person := lead + " It still waits for " + words.And(needs) + "."
	if len(who) > 0 {
		person += " " + words.And(who) + " can move it forward."
	}
	return Refusal{person: person}
}

// MergeWouldDeleteAKeptBranch is a merge asked to delete its branch where that
// deletion is refused: a protection rule keeps the branch, or whoever merges
// may not delete it. ONE PHRASING FOR BOTH, and true of each, for
// [DatastoreQuotaReached]'s reason: which one it was Gitlet names only
// in its sentence for a model, which is never parsed for a person's words.
func MergeWouldDeleteAKeptBranch() Refusal {
	return Refusal{person: "Nothing was merged, because this merge would also delete its " +
		"branch, and a protection rule keeps that branch or you may not delete it. Merge it " +
		"keeping its branch."}
}

// RepositoryFull is a merge whose commits would take its repository past the
// most the company lets a repository hold. NO REMEDY beside it, as
// [ArtifactQuotaReached] has none: no dashboard screen edits the company's code
// limits, so the words name who raises it.
func RepositoryFull() Refusal {
	return Refusal{person: "Merging it would take its repository past the most a repository " +
		"may hold, so nothing was merged. Ask an admin to raise the limit."}
}

// MergeAskedForSomebodyWhoMayNot is a merge, or a place in its target's queue,
// asked on behalf of somebody who may not make it: a seat's work for whoever
// started its turn, who may not change the company's code, read the
// repository or merge into the target.
func MergeAskedForSomebodyWhoMayNot() Refusal {
	return Refusal{person: "This merge was asked for somebody who may not make it, so nothing " +
		"was merged or queued. Somebody who may merge into its target can."}
}

// QueueEntryNotYours is a change taken off its target's queue by somebody who
// may not: whoever queued it, the change's owner, somebody who maintains its
// repository or a person takes it out, and nobody else.
func QueueEntryNotYours() Refusal {
	return Refusal{person: "Only whoever queued it, the change's owner, somebody who " +
		"maintains its repository or a person may take it off the queue, so it is still queued."}
}

// ArtifactTrashed is a change to an artifact that is in the trash.
func ArtifactTrashed() Refusal {
	return Refusal{person: "This artifact is in the trash. Restore it first, " +
		"then try again."}
}

// MembersChanged is a room's membership stated against members that changed
// before the change landed: somebody added, removed or moved a member while
// the person was looking, so the whole set they would have written is not
// written over it.
func MembersChanged() Refusal {
	return Refusal{person: "The members of this room changed while you were " +
		"looking, so nothing was changed. Check the members again, then make " +
		"the change."}
}

// ValueInWrongForm is a change refused before it was made because the request
// sent a value in a form it does not take (ADR-0059's misread). WHO READS IT is
// mostly a person who approved an assistant's action, on the act route or the
// work API: the request was malformed, not their choice, so resending it gets
// the same answer, and what they can do is have it sent again correctly or
// make the change themselves.
func ValueInWrongForm() Refusal {
	return Refusal{person: "The request sent a value in a form this change " +
		"does not take, so nothing was changed. Ask Crewlet to send it " +
		"again, or make the change yourself."}
}

// TokenBudgetSpent is a call a token budget refused before anything was spent,
// carrying where the ceiling that refused it is changed. NEVER "WAIT" ALONE:
// the window turning over is one way back, and a raised ceiling is the other,
// which is the one the remedy beside these words opens.
func TokenBudgetSpent(where Remedy) Refusal {
	const back = " It comes back when the window resets, or as soon as the " +
		"ceiling is raised."
	if !where.valid() {
		where = CompanyTokenBudget()
	}
	var whose string
	switch where.Setting {
	case SettingSeatTokenBudget:
		whose = "@" + where.Seat + "'s token budget"
	case SettingAssistantTokenBudget:
		whose = "Your own Ask Crewlet budget"
	default:
		whose = "The company's token budget"
	}
	return Refusal{person: whose + " for this window is spent, so nothing was " +
		"sent." + back, remedy: where}
}

// ModelAtLimit is a question no model was asked, because the model that
// answers it has reached a usage limit it reported, carrying the company's
// models as where it is given a fallback or its plan changed (ADR-0064). NO
// TIME, for [AnsweredAlready]'s reason: an instant in these words is on no
// reader's clock, and when the model is tried again rides beside them as the
// answer's Retry-After, for a screen to say on its own. And NEVER "WAIT"
// ALONE, for [TokenBudgetSpent]'s.
func ModelAtLimit() Refusal {
	return Refusal{person: "The model that answers questions has reached its " +
		"usage limit, so this wasn't answered and nothing was spent. It answers " +
		"again once the limit lifts, or sooner if the company's models change.",
		remedy: ModelProviders()}
}
