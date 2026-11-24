package sandbox

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// A RUN ASKS A PERSON IN THE SHAPE A DECISION IS ASKED (ADR-0068): through
// `ask_and_wait` (internal/agent/builtin), as an [Ask] whose [decision.Decision]
// offers options the answer names by id and recommends one (ADR-0100). The
// tool validates it whole before anything is written, so the run can still fix
// a malformed ask, and [PendingStore.RecordAsk] records it on the job's row.
//
// THE ROW IS THE ONE COPY: written under the job that asked onto
// [PendingRun.Question], [PendingRun.QuestionContext], [PendingRun.Decision],
// [PendingRun.Audience] and [PendingRun.Branch], where the park, the answer,
// the dashboard and the resume read it, never out of the box. A job's
// question is cleared when the next job opens.

// The bounds of an ask beside its decision's own ([decision.MaxQuestion] and
// the rest), each refused at the tool naming the field, never cut.
const (
	// MaxAskContextBytes bounds an ask's context, SIZED TO WHERE IT TRAVELS:
	// the run's coordination row (read and written whole on every status flip
	// and active-run listing while it waits), the park's announcement, the
	// answer pane, and the continuing run's prompt, which its harness re-sends
	// with every request. 16 KiB, about 4k tokens, is a page of prose; longer
	// is a report in the wrong place, or a page the context should link to.
	MaxAskContextBytes = 16 << 10

	// MaxAskToBytes bounds who an ask is put to. A handle is a short slug and
	// a name a few words; a label past this names nobody the chart has, and
	// would only reach the seat's lead chain by fallback.
	MaxAskToBytes = 128
)

// The audiences an ask names in words, beside a colleague's handle or name,
// each defined by internal/engine's audience resolver, which resolves it
// against the chart when the run parks.
const (
	AskToRequester = "requester"
	AskToTeam      = "team"
	AskToManager   = "manager"
)

// Ask is what an executor asked a person.
type Ask struct {
	// Question is what the run needs answered, in one sentence: the title
	// a person reads first. Held to [decision.MaxQuestion] because it is
	// drawn where a decision's question is.
	Question string

	// Context is what the person needs to answer well, in Markdown: what
	// the run found, what is at stake, where its work stands. Empty when
	// the question says it all.
	Context string

	// Decision is the choice the person is asked to make. Required: every
	// run's question offers options and recommends one (ADR-0100), and words
	// come through "Something else". Nil only on a row recorded before that.
	// Its Question is the ask's own, and it carries no `inform`: the answer
	// resumes the run itself, so no answered turn holds a promise.
	Decision *decision.Decision

	// To is who the run puts the question to: [AskToRequester],
	// [AskToTeam], [AskToManager], or a colleague's handle or name.
	// Required.
	To string

	// Branch is the branch the run pushed its work in progress to before
	// asking: what a re-seeded run is told to check out once the answer
	// arrives ([PendingRun.Branch]). Empty where it pushed nothing.
	Branch string

	// Held is the call to a tool at Needs approval the question asks
	// about, nil for a question the run asked itself (ADR-0077). Its
	// Decision is [HeldDecision]; it is recorded with the question
	// ([PendingRun.HeldCall]), its LaunchID the job's.
	Held *HeldCall

	// Wait is the run `wait_for_run` parks the job until, nil for a question
	// put to a person (ADR-0176): its Question is the engine's words, it is put
	// to nobody (To and Decision empty), and the engine answers it once that run
	// leaves [Holding]. Recorded on the row as [PendingRun.WaitsFor].
	Wait *RunWait

	// Call is the job's own bridged call it parks on, answered still running
	// before its harness's ceiling (callwait.go), nil for every other ask: the
	// ask's one field. Added to [PendingRun.ParkedCalls] WHATEVER THE JOB ASKED
	// BEFORE it, so its resume tells it; where the job asked nothing, it is
	// the engine's ask ([CallWait.Ask]), [CallWaitQuestion] put to nobody and
	// carried on by the engine once its run is collected.
	Call *CallWait
}

// Validate is the whole contract of an ask, every fault at once (ADR-0059),
// each marked [decision.ErrInvalid] and named once, as the run's arguments
// carry it (`question`, `options[0].label`, `recommended`): the ask's own
// fields ([Ask.ValidateOwn]) and its decision's. Nothing is trimmed,
// defaulted or cut. The run's own question is held to the recommendation
// ([decision.Decision.ValidateRecommendedAt]); a held call's is the engine's
// approval, with no view to recommend, held to the shape alone.
func (a Ask) Validate() error {
	found := a.ownFaults()
	if a.Decision != nil {
		validate := a.Decision.ValidateRecommendedAt
		if a.Held != nil {
			validate = a.Decision.ValidateAt
		}
		if err := validate(""); err != nil {
			found = append(found, err)
		}
	}
	return decision.Join(found)
}

// ValidateOwn is the ask's own contract, without its decision's shape, for a
// caller that checks the decision itself (`ask_and_wait`). Where the ask has a
// decision, the question is checked there, once.
func (a Ask) ValidateOwn() error { return decision.Join(a.ownFaults()) }

func (a Ask) ownFaults() []error {
	if a.Call != nil {
		found := a.Call.validate()
		if a.Question != "" || a.Context != "" || a.To != "" || a.Branch != "" || a.Decision != nil ||
			a.Held != nil || a.Wait != nil {
			found = append(found, fieldRefusal("call", "is a park on the job's own call, which asks "+
				"nothing else"))
		}
		if a.Call.Ask {
			found = append(found, fieldRefusal("call", "marks itself the job's ask, which only the "+
				"park's write decides"))
		}
		return found
	}
	var found []error
	note := func(err error) {
		if err != nil {
			found = append(found, err)
		}
	}
	if a.Decision == nil || a.Decision.Question != a.Question {
		note(decision.CheckText("question", a.Question, decision.MaxQuestion, true))
	}
	note(decision.CheckText("context", a.Context, MaxAskContextBytes, false))
	note(decision.CheckText("to", a.To, MaxAskToBytes, a.Wait == nil))
	// THE ENGINE'S ONE GRAMMAR OF A BRANCH (ADR-0008): it refuses what git
	// does, non-UTF-8 (the JSON row would turn it into another branch's
	// name), and past [gitref.MaxBranchBytes], a ref no push to Gitlet makes.
	if a.Branch != "" && !gitref.ValidBranch(a.Branch) {
		note(fieldRefusal("branch", fmt.Sprintf("is %q, which is not a branch name git "+
			"accepts (or is not UTF-8, or is past %d bytes): name the branch exactly as you "+
			"pushed it, or leave it out", a.Branch, gitref.MaxBranchBytes)))
	}
	switch {
	case a.Wait != nil:
		// A WAIT IS PUT TO NOBODY: the engine answers it with how the run it
		// waits for went, so it offers no choice and names no audience.
		found = append(found, a.Wait.validate()...)
		if a.Held != nil || a.Decision != nil || a.To != "" {
			note(fieldRefusal("turn_id", "names a run to wait for on an ask that is also a question to a person"))
		}
	case a.Held != nil:
		found = append(found, a.Held.validate(a.Decision)...)
	case a.Decision == nil:
		// THE RUN'S OWN QUESTION IS A CHOICE, always (ADR-0100): the person
		// answers it with one press, or in words through "Something else".
		note(fieldRefusal("options", "is empty, and every question a run puts to a "+
			"person offers 2 to 4 options with the one it `recommended` and its "+
			"`rationale`: give the answers you expect, and the person can still "+
			"answer in their own words"))
	}
	if d := a.Decision; d != nil {
		if d.Question != a.Question {
			// ONE QUESTION, never two that can disagree: the tool builds
			// the decision from the ask's question, so a mismatch is a
			// caller assembling the value by hand.
			note(fieldRefusal("question", "differs from its decision's; a run's "+
				"decision is asked in the ask's own words"))
		}
		if d.Inform != nil {
			note(fieldRefusal("inform", "is set, and a run's ask carries "+
				"none: its answer resumes the run itself, which says what was decided "+
				"where its work is"))
		}
	}
	return found
}

// fieldRefusal is a refusal of one field, marked [decision.ErrInvalid]: the
// field's name and what is wrong with it.
func fieldRefusal(field, what string) error {
	return decision.Refuse("%s %s", field, what)
}

// Redacted is the ask with a secret it carries replaced by its marker, in
// every text a person or a model reads. Redacted where it is recorded,
// because the row, the announcement and the continuing run's prompt keep it.
func (a Ask) Redacted() Ask {
	a.Question = redact.Secrets(a.Question)
	a.Context = redact.Secrets(a.Context)
	// A BRANCH IS AN IDENTIFIER: one a redaction would change is dropped,
	// since a marker names no branch to check out, and the answer then
	// tells a re-seeded run in general terms to check out what it pushed.
	if redact.Secrets(a.Branch) != a.Branch {
		a.Branch = ""
	}
	if a.Wait != nil {
		w := *a.Wait
		w.Reason = redact.Secrets(w.Reason)
		a.Wait = &w
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
			// THE REFERENCE TOO: a cited url can carry a token in its query.
			evidence.Ref = redact.Secrets(evidence.Ref)
			evidence.Label = redact.Secrets(evidence.Label)
			d.Evidence[i] = evidence
		}
		a.Decision = &d
	}
	if a.Held != nil && a.Held.Acting != nil {
		// THE DECISION A HELD CALL CARRIES OUT, as its seat asked it and a
		// person chose: words, beside the closed ids and act a call is
		// matched to, which stay.
		held, act := *a.Held, *a.Held.Acting
		act.Question, act.Choice = redact.Secrets(act.Question), redact.Secrets(act.Choice)
		held.Acting = &act
		a.Held = &held
	}
	return a
}

// AskOutcome is what recording an ask did ([PendingStore.RecordAsk]).
type AskOutcome string

const (
	// AskRecorded is an ask on the job's row, which the run parks on once
	// its harness ends.
	AskRecorded AskOutcome = "recorded"
	// AskAlreadyAsked is a job that asked a question already, which stands:
	// one answer resumes a run, so it asks one question at a time.
	AskAlreadyAsked AskOutcome = "already_asked"
	// AskNotRunning is a row that does not hold this job running, so
	// nothing can park on the ask: the job ended, was stopped, or was
	// replaced.
	AskNotRunning AskOutcome = "not_running"
)

// Valid reports whether o is an outcome this build knows.
func (o AskOutcome) Valid() bool {
	return o == AskRecorded || o == AskAlreadyAsked || o == AskNotRunning
}

// asked reports whether the job the row holds asked a question: the one fact
// a collection parks on.
func (r PendingRun) asked() bool { return r.Question != "" }

// AskedBy reports whether job `launch` is the job the row holds and has asked
// a person a question ([PendingStore.RecordAsk]): the job is then parking, and
// its run's only remaining move is to end. A job the row no longer holds has
// asked nothing, whatever it called: its question went with it.
func (r PendingRun) AskedBy(launch string) bool {
	return launch != "" && r.LaunchID == launch && r.asked()
}

// withoutQuestion is the row as a collection that parks nothing reads it:
// everything the job asked on its way out gone.
func withoutQuestion(r PendingRun) PendingRun {
	r.Question, r.QuestionContext, r.Decision, r.Audience, r.Branch = "", "", nil, "", ""
	r.HeldCall, r.WaitsFor, r.ParkedCalls = nil, nil, nil
	return r
}

// AnswerText is the answer a parked run reads, from what a person gave: the
// option they chose and their words. A choice the decision does not offer is
// an error, because the run would be told it chose something nobody offered.
// The choice reads as the label the run wrote and the id it reasoned with.
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
