package sandbox

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/redact"
)

// A RUN ASKS A PERSON IN THE SHAPE A DECISION IS ASKED: ADR-0068.
//
// A coding run that cannot go on without a person's answer asks through one
// tool over its bridge (`ask_and_wait`, internal/agent/builtin), and what it
// asked is an [Ask]: the question in one sentence, its context in Markdown,
// and, where the person is to choose, a [decision.Decision] whose options the
// answer names by id. The tool validates it whole before anything is written,
// so a malformed ask is refused to the run while it can still fix it; the
// store records it on the job's row ([PendingStore.RecordAsk]); and the
// collection parks the run on what the row holds, never on anything read out
// of the box.
//
// THE ROW IS THE ONE COPY. The ask is written onto the fields the parked
// question is read from ([PendingRun.Question], [PendingRun.QuestionContext],
// [PendingRun.Decision], [PendingRun.Audience], [PendingRun.Branch]) by the
// write that records it, under the job that asked, and every reader (the park,
// the answer, the dashboard, the resume) reads them there. A job's question is
// cleared when the next job opens, exactly as a parked question always was.

// The bounds of an ask beside its decision's own ([decision.MaxQuestion] and
// the rest). Each is refused at the tool naming the field, never cut.
const (
	// MaxAskContextBytes bounds an ask's context.
	//
	// SIZED TO WHERE IT TRAVELS, and it travels further than anything else a
	// run writes: it is a field of the run's coordination row, read and
	// written whole on every status flip and every listing of active runs
	// for as long as it waits; the park's announcement; the pane a person
	// answers it from; and, once answered, the prompt of the run that
	// carries the work on, which that run's CLI re-sends with every request
	// it makes from then on. 16 KiB is about four thousand tokens: a page of
	// prose, the bound a run's whole free-text question was held to before
	// asks were structured, so no context that fitted then is refused now.
	// Longer is the report put in the wrong place, or a page the context
	// should link to.
	MaxAskContextBytes = 16 << 10

	// MaxAskToBytes bounds who an ask is put to: `requester`, `team`,
	// `manager`, or a colleague's handle or name. A handle is a short slug
	// and a person's name a few words; a label past this names nobody the
	// chart has, and would only reach the seat's lead chain by fallback.
	MaxAskToBytes = 128
)

// The audiences an ask names in words, beside a colleague's handle or name.
// Resolved against the chart when the run parks (internal/engine's audience
// resolver), which is where each is defined.
const (
	AskToRequester = "requester"
	AskToTeam      = "team"
	AskToManager   = "manager"
)

// Ask is what a coding run asked a person.
type Ask struct {
	// Question is what the run needs answered, in one sentence: the title
	// a person reads first. Held to [decision.MaxQuestion], the bound a
	// decision's question has, because it is drawn in the same place.
	Question string

	// Context is what the person needs to answer well, in Markdown: what
	// the run found, what is at stake, where its work stands. Empty when
	// the question says it all.
	Context string

	// Decision is the choice the person is asked to make, or nil for an
	// open question answered in words. Its Question is the ask's own, and
	// it never carries an `inform`: a run's answer resumes the run itself,
	// and there is no answered turn of the seat's to hold to a promise.
	Decision *decision.Decision

	// To is who the run puts the question to: [AskToRequester],
	// [AskToTeam], [AskToManager], or a colleague's handle or name.
	// Required.
	To string

	// Branch is the branch the run pushed its work in progress to before
	// asking: what a re-seeded run is told to check out once the answer
	// arrives ([PendingRun.Branch]). Empty where it pushed nothing.
	Branch string
}

// Validate is the whole contract of an ask, every fault at once (ADR-0059),
// each marked [decision.ErrInvalid] and naming its field. Nothing is trimmed,
// defaulted or cut.
func (a Ask) Validate() error {
	var found []error
	note := func(err error) {
		if err != nil {
			found = append(found, err)
		}
	}
	note(decision.CheckText("question", a.Question, decision.MaxQuestion, true))
	note(decision.CheckText("context", a.Context, MaxAskContextBytes, false))
	note(decision.CheckText("to", a.To, MaxAskToBytes, true))
	if a.Branch != "" && (len(a.Branch) > MaxBranchBytes || !ValidBranch(a.Branch)) {
		note(fieldRefusal("branch", fmt.Sprintf("is %q, which is not a branch name git "+
			"accepts (or is past %d bytes): name the branch exactly as you pushed it, "+
			"or leave it out", a.Branch, MaxBranchBytes)))
	}
	if d := a.Decision; d != nil {
		if d.Question != a.Question {
			// ONE QUESTION, never two that can disagree: the tool builds
			// the decision from the ask's own question, so a mismatch is
			// a caller assembling the value by hand.
			note(fieldRefusal("decision.question", "differs from the ask's question; "+
				"a run's decision is asked in the ask's own words"))
		}
		if d.Inform != nil {
			note(fieldRefusal("decision.inform", "is set, and a run's ask carries "+
				"none: its answer resumes the run itself, which says what was decided "+
				"where its work is"))
		}
		note(d.Validate())
	}
	return decision.Join(found)
}

// fieldRefusal is a refusal of one field, marked [decision.ErrInvalid]: the
// field's name and what is wrong with it.
func fieldRefusal(field, what string) error {
	return decision.Refuse("%s %s", field, what)
}

// Redacted is the ask with a secret it carries replaced by its marker, in
// every text a person or a model reads: the question, the context, and each
// part of the decision. Redacted where it is recorded, as everything read
// out of a run is, because the row, the announcement and the continuing
// run's prompt keep it.
func (a Ask) Redacted() Ask {
	a.Question = redact.Secrets(a.Question)
	a.Context = redact.Secrets(a.Context)
	// A BRANCH IS AN IDENTIFIER, so one a redaction would change is
	// dropped rather than recorded as its marker, which names no branch a
	// fresh machine could check out: its answer then tells a re-seeded run
	// in general terms to check out the branch it pushed.
	if redact.Secrets(a.Branch) != a.Branch {
		a.Branch = ""
	}
	if a.Decision != nil {
		d := *a.Decision
		d.Question = a.Question
		d.Options = make([]decision.Option, len(a.Decision.Options))
		for i, option := range a.Decision.Options {
			option.Label = redact.Secrets(option.Label)
			option.Detail = redact.Secrets(option.Detail)
			d.Options[i] = option
		}
		d.Rationale = redact.Secrets(d.Rationale)
		d.Evidence = make([]decision.Evidence, len(a.Decision.Evidence))
		for i, evidence := range a.Decision.Evidence {
			// THE REFERENCE TOO: a url the run cites can carry a token in
			// its query, and a marker in its place leaves a link that
			// names where it went without what opened it.
			evidence.Ref = redact.Secrets(evidence.Ref)
			evidence.Label = redact.Secrets(evidence.Label)
			d.Evidence[i] = evidence
		}
		a.Decision = &d
	}
	return a
}

// AskOutcome is what recording an ask did ([PendingStore.RecordAsk]).
type AskOutcome string

const (
	// AskRecorded: the ask is on the job's row, and the run parks on it
	// once its CLI ends.
	AskRecorded AskOutcome = "recorded"
	// AskAlreadyAsked: the job asked a question already, which stands. A
	// run asks one question at a time, because one answer resumes it.
	AskAlreadyAsked AskOutcome = "already_asked"
	// AskNotRunning: the row does not hold this job running, so nothing
	// can park on the ask: the job ended, was stopped, or was replaced.
	AskNotRunning AskOutcome = "not_running"
)

// Valid reports whether o is an outcome this build knows.
func (o AskOutcome) Valid() bool {
	return o == AskRecorded || o == AskAlreadyAsked || o == AskNotRunning
}

// asked reports whether the job the row holds asked a question: the one fact
// a collection parks on.
func (r PendingRun) asked() bool { return r.Question != "" }

// withoutQuestion is the row as a collection that parks nothing reads it: the
// job's question, its context, its decision, its audience and its branch
// gone, whatever the job asked on its way out.
func withoutQuestion(r PendingRun) PendingRun {
	r.Question, r.QuestionContext, r.Decision, r.Audience, r.Branch = "", "", nil, "", ""
	return r
}

// AnswerText is the answer a parked run reads, from what a person gave: the
// option they chose, as the run's own decision names it, and their words. A
// choice the decision does not offer is an error, because the run would be
// told it chose something nobody offered.
//
// THE RUN READS THE CHOICE IN WORDS AND BY ID, because it is what it asked:
// the label is what it wrote for the option, and the id is what it named it
// by in its own reasoning.
func AnswerText(d *decision.Decision, choice, words string) (string, error) {
	words = strings.TrimSpace(words)
	if choice == "" {
		return words, nil
	}
	if d == nil {
		return "", decision.Refuse("choice is %q, and the question offered no options "+
			"to choose from; answer it in words", choice)
	}
	if err := d.CheckChoice(choice); err != nil {
		return "", err
	}
	option, _ := d.Option(choice)
	text := fmt.Sprintf("Chosen option: %s (`%s`).", option.Label, option.ID)
	if words != "" {
		text += "\n\n" + words
	}
	return text, nil
}
