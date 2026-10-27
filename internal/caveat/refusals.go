package caveat

import "fmt"

// THE REFUSAL CATALOG: the person's words for the refusals a person meets most
// often on the dashboard, answering a decision, filing or editing a work item,
// and publishing an artifact an assistant drafted. Every other refusal is
// answered with its code's own sentence until it has a phrasing here
// (ADR-0060 names the rest). Each names only what a person sees: a handle as
// `@handle`, a question, an option, a title.

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

// ArtifactTrashed is a change to an artifact that is in the trash.
func ArtifactTrashed() Refusal {
	return Refusal{person: "This artifact is in the trash. Restore it first, " +
		"then try again."}
}

// ValueInWrongForm is a change refused before it was made because the request
// sent a value in a form it does not take (ADR-0059's misread). WHO READS IT is
// mostly a person who approved an assistant's action, on the act route or the
// work API: the request was malformed, not their choice, so resending it gets
// the same answer, and what they can do is have it sent again correctly or
// make the change themselves.
func ValueInWrongForm() Refusal {
	return Refusal{person: "The request sent a value in a form this change " +
		"does not take, so nothing was changed. Ask the assistant to send it " +
		"again, or make the change yourself."}
}

// TokenBudgetSpent is a call a token budget refused before anything was spent,
// carrying where the ceiling that refused it is changed. NEVER "WAIT" ALONE:
// the window turning over is one way back, and a raised ceiling is the other,
// which is the one the remedy beside these words opens.
func TokenBudgetSpent(where Remedy) Refusal {
	const back = " It comes back when the window resets, or as soon as the " +
		"ceiling is raised."
	var whose string
	switch where.Setting {
	case SettingSeatTokenBudget:
		whose = "@" + where.Seat + "'s token budget"
	case SettingAssistantTokenBudget:
		whose = "Your own assistant token budget"
	default:
		whose = "The company's token budget"
		where = CompanyTokenBudget()
	}
	return Refusal{person: whose + " for this window is spent, so nothing was " +
		"sent." + back, remedy: where}
}
