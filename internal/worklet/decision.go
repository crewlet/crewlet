package worklet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/caveat"
)

// A DECISION IS A STRUCTURED ASK — ADR-0023.
//
// A question that needs somebody to choose — ship or hold, which region, which
// of three designs — used to travel as prose on an `ask` comment, and every
// part a person needs to answer it well was optional and unplaced: the
// options were somewhere in the body, the recommendation was a sentence among
// others, the evidence was a list of links a reader had to go and find, and
// the answer was prose the asker then had to interpret back into one of the
// options it had in mind. So the ask carries a [Decision] — the question, two
// to six options, what the asker recommends and why, what it looked at, and
// the role the person is asked in — and the answer carries a [Comment.Choice]
// naming one of those options by id.
//
// # What the engine enforces, and what it deliberately does not
//
// Exactly two things: that a decision is well formed (this file, over values)
// and that an answer's choice names an option OF THE ASK IT ANSWERS (the
// writer's decide, against the ask's own row). Nothing else. There is no
// approval chain, no quorum, no state machine and no escalation timer: a
// decision is still ONE ask to ONE person, answered once, and what the asker
// does with the choice is the asker's turn — the DACI roles stay behavioural
// guidance, because a company's way of deciding is not a workflow the engine
// should own. The [DecisionRole] is a label on the ask telling the person
// whether their answer IS the decision (approver) or an input to one
// (contributor); it gates nothing.
//
// # Why pure over values
//
// The same reason [internal/textindex]'s arithmetic and this package's own
// coercion table are: a rule exercised only through a database is a rule
// nobody re-reads. [Decision.Validate] is the whole structural contract and
// runs wherever a decision is written; [ValidateDecision] adds the two checks
// that need the world (a task reference resolves, a page exists) through a
// seam its caller declares, and normalises what it accepts, so the record
// carries a task's id rather than a key a move will retire.
//
// # Immutable once asked
//
// A decision is set only on the comment that ASKS, and never changes after:
// the answer names an option by id, so an ask whose options could be edited
// under it would turn an answer given to one set of options into an answer
// to another. The writer refuses an edit that changes it.

// The decision caps. Each is refused at WRITE naming the field, never cut —
// the rule every other tracker cap follows — and each is sized to where the
// value is RENDERED, because a decision is read by a person deciding at a
// glance and by a model composing the answering call.
const (
	// MaxDecisionQuestion is one sentence on a card. The long form — the
	// context, the history, the tradeoffs — is the comment's own body,
	// which already takes 32 KiB; a question longer than this is a body
	// that was put in the wrong field.
	MaxDecisionQuestion = 300

	// MinDecisionOptions and MaxDecisionOptions bound the choice. One
	// option is not a choice — it is an approval, which is an ask with
	// the two options "yes" and "no" — and past six a person can no
	// longer weigh them side by side on one card, which is the one thing
	// structuring the ask was for. The asking guidance recommends two to
	// four; six is the ceiling a well-meant long list still fits under.
	MinDecisionOptions = 2
	MaxDecisionOptions = 6

	// MaxOptionLabel is a button's text. MaxOptionDetail is the line under
	// it, and MaxEvidenceLabel is a link's text, sized like a label for
	// the same reason.
	MaxOptionLabel   = 80
	MaxOptionDetail  = 500
	MaxEvidenceLabel = 80

	// MaxDecisionRationale is why the asker recommends what it does — a
	// paragraph or three. Longer is an argument, and an argument is a page
	// the evidence can link to.
	MaxDecisionRationale = 1500

	// MaxDecisionEvidence is how many things the asker may cite. Eight is
	// what a card lists without scrolling; a decision resting on more is
	// one whose evidence belongs on a page, cited once.
	MaxDecisionEvidence = 8

	// MaxEvidenceRef bounds a reference. An id is far shorter; a URL is
	// the long case, and 2048 is the length every browser and proxy in
	// common use still carries whole.
	MaxEvidenceRef = 2048

	// MaxInformChannel is Slack's own channel-name ceiling, the longest of
	// the three chat surfaces' (Mattermost's and Chatlet's are both 64).
	MaxInformChannel = 80
)

// optionIDPattern is what an option id may be.
//
// A SLUG, because it is typed back: the answering call names it in `choice`,
// a model composes that call from the ask it read, and the dashboard sends it
// from a button. Lower case with digits, `_` and `-` is what every one of
// those spells the same way, and 32 keeps it an identifier rather than a
// sentence somebody would paraphrase.
var optionIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Decision is the structure an ask carries when it needs somebody to choose.
type Decision struct {
	// Question is what is being decided, in one sentence.
	Question string `json:"question" untrusted:"name"`

	// Options are the choices, in the order the asker put them.
	Options []DecisionOption `json:"options"`

	// Recommended is the option the asker would choose, by id, or empty
	// when it has no view.
	Recommended string `json:"recommended,omitempty"`

	// Rationale is why it recommends that one.
	Rationale string `json:"rationale,omitempty" untrusted:"name"`

	// Evidence is what the asker looked at.
	Evidence []Evidence `json:"evidence,omitempty"`

	// Role is what the person asked is being asked AS.
	Role DecisionRole `json:"role"`

	// Inform is where the asker will say what was decided, once it is.
	// Nil when the answer stays on the item.
	Inform *Inform `json:"inform,omitempty"`
}

// DecisionOption is one choice.
type DecisionOption struct {
	ID     string `json:"id"`
	Label  string `json:"label" untrusted:"name"`
	Detail string `json:"detail,omitempty" untrusted:"name"`
}

// DecisionRole is what the person asked is being asked as.
//
// A NAMED STRING WITH [DecisionRole.Valid], so an unknown value off the wire
// is a value rather than a panic — and it has NO ZERO: an ask that did not say
// whether the answer is the decision or an input to it leaves the person
// answering it guessing at the one thing the structure was for.
type DecisionRole string

const (
	// RoleApprover — the answer IS the decision.
	RoleApprover DecisionRole = "approver"
	// RoleContributor — the answer is an input to a decision somebody
	// else makes.
	RoleContributor DecisionRole = "contributor"
)

// Valid reports whether r is a role this build knows.
func (r DecisionRole) Valid() bool {
	return r == RoleApprover || r == RoleContributor
}

// EvidenceKind is what a piece of evidence points at.
type EvidenceKind string

const (
	// EvidenceTask is a work item. Its reference is resolved to the task's
	// ID at write, because a key moves with the task and an id does not.
	EvidenceTask EvidenceKind = "task"
	// EvidencePage is a knowledge-base page, by id, which must exist.
	EvidencePage EvidenceKind = "page"
	// EvidenceTurn is an agent turn, by id.
	EvidenceTurn EvidenceKind = "turn"
	// EvidenceRun is a coding run, by id.
	EvidenceRun EvidenceKind = "run"
	// EvidenceURL is anything else, as an https URL.
	EvidenceURL EvidenceKind = "url"
)

// Valid reports whether k is a kind this build knows.
func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceTask, EvidencePage, EvidenceTurn, EvidenceRun, EvidenceURL:
		return true
	}
	return false
}

// Evidence is one thing a decision cites.
type Evidence struct {
	Kind  EvidenceKind `json:"kind"`
	Ref   string       `json:"ref"`
	Label string       `json:"label,omitempty" untrusted:"name"`
}

// Inform is the chat channel the asker will report the decision in.
//
// A PROMISE THE ENGINE KEEPS, in three places and no fourth. Here, over values:
// a surface is one of the chat surfaces — the two vendor integrations or the
// engine's own chat — a channel is a name, and only
// an agent seat's ask may carry one ([checkCommentShape]) — the promise is kept
// by holding the asker's turn, and a person has none. At the tool, against the
// chart: the surface is one the asking seat can post on and the channel is one
// a unit declares. And at the answer: the asker's `answered` wake OWES that
// surface ([Owes]), so the turn it wakes is sent back until a tool on it has
// run — a comment on the item does not discharge it.
type Inform struct {
	Surface InformSurface `json:"surface"`
	Channel string        `json:"channel"`
}

// InformSurface is a chat surface an asker may report a decision in.
type InformSurface string

const (
	// InformMattermost is Mattermost.
	InformMattermost InformSurface = "mattermost"
	// InformSlack is Slack.
	InformSlack InformSurface = "slack"
	// InformChat is the engine's own chat (`chat.backend: chatlet`).
	//
	// ITS VALUE IS CHATLET'S FEED SOURCE, `chat`, and has to be: an
	// inform is kept by OWING the surface ([Owes]) — the asker's answered
	// turn is held until a tool that DELIVERS there has run — and Chatlet's posting tools
	// declare their delivery as that source. Any other spelling
	// is a promise no tool could ever discharge. This package cannot import
	// the chat domain, so a test where both are visible holds the two equal.
	InformChat InformSurface = "chat"
)

// InformSurfaces is every chat surface this build knows, in the order a
// refusal lists them.
func InformSurfaces() []InformSurface {
	return []InformSurface{InformMattermost, InformSlack, InformChat}
}

// Valid reports whether s is a chat surface this build knows.
func (s InformSurface) Valid() bool { return slices.Contains(InformSurfaces(), s) }

// Option is the option with this id, or false.
func (d Decision) Option(id string) (DecisionOption, bool) {
	for _, option := range d.Options {
		if option.ID == id {
			return option, true
		}
	}
	return DecisionOption{}, false
}

// OptionIDs is every option's id, in order, for a refusal that has to list
// them.
func (d Decision) OptionIDs() []string {
	ids := make([]string, 0, len(d.Options))
	for _, option := range d.Options {
		ids = append(ids, option.ID)
	}
	return ids
}

// Validate is the whole structural contract of a decision.
//
// Every refusal is marked [ErrInvalid] and names the field, because each is
// something the caller changes. Nothing is trimmed, defaulted or cut: a
// decision is stored exactly as it was accepted.
func (d Decision) Validate() error {
	// EVERY FAULT AT ONCE (ADR-0059): a decision is written again once it
	// is refused, and a refusal naming one fault of several spends that
	// attempt on the next.
	var found []error
	note := func(err error) {
		if err != nil {
			found = append(found, err)
		}
	}
	note(checkText("decision.question", d.Question, MaxDecisionQuestion, true))
	switch n := len(d.Options); {
	case n < MinDecisionOptions:
		note(invalid("a decision has %d option(s) and needs at "+
			"least %d — a single option is an approval, which is the "+
			"options yes and no", n, MinDecisionOptions))
	case n > MaxDecisionOptions:
		note(invalid("a decision has %d options and the maximum is "+
			"%d — a person cannot weigh more side by side; narrow them, or "+
			"ask about the shortlist first", n, MaxDecisionOptions))
	}
	seen := make(map[string]bool, len(d.Options))
	for i, option := range d.Options {
		field := fmt.Sprintf("decision.options[%d]", i)
		switch {
		case !optionIDPattern.MatchString(option.ID):
			note(invalid("%s.id is %q and must be 1 to 32 of "+
				"a-z, 0-9, `_` and `-` — it is typed back in `choice`",
				field, option.ID))
		case seen[option.ID]:
			note(invalid("%s.id %q names two options: an answer "+
				"naming it would choose both", field, option.ID))
		}
		seen[option.ID] = true
		note(checkText(field+".label", option.Label, MaxOptionLabel, true))
		note(checkText(field+".detail", option.Detail, MaxOptionDetail, false))
	}
	if d.Recommended != "" && !seen[d.Recommended] {
		note(invalid("decision.recommended is %q, which is not one "+
			"of its options (%s)", d.Recommended,
			strings.Join(d.OptionIDs(), ", ")))
	}
	note(checkText("decision.rationale", d.Rationale, MaxDecisionRationale, false))
	if len(d.Evidence) > MaxDecisionEvidence {
		note(invalid("a decision cites %d pieces of evidence and the "+
			"maximum is %d — put the rest on a page and cite the page",
			len(d.Evidence), MaxDecisionEvidence))
	}
	for i, evidence := range d.Evidence {
		note(evidence.validate(fmt.Sprintf("decision.evidence[%d]", i)))
	}
	if !d.Role.Valid() {
		note(invalid("decision.role is %q and must be %q (the "+
			"answer is the decision) or %q (the answer is an input to it)",
			d.Role, RoleApprover, RoleContributor))
	}
	if d.Inform != nil {
		if !d.Inform.Surface.Valid() {
			note(invalid("decision.inform.surface is %q and must be one of "+
				"%v", d.Inform.Surface, InformSurfaces()))
		}
		note(checkText("decision.inform.channel", d.Inform.Channel,
			MaxInformChannel, true))
	}
	return joinInvalid(found)
}

// joinInvalid is several refusals of one value as one, every sentence in
// order: nil for none, the refusal itself for one. Every error here is an
// [ErrInvalid] refusal; one that is not is answered alone, as itself.
func joinInvalid(found []error) error {
	switch len(found) {
	case 0:
		return nil
	case 1:
		return found[0]
	}
	sentences := make([]string, len(found))
	for i, err := range found {
		if !errors.Is(err, ErrInvalid) {
			return err
		}
		sentences[i] = Sentence(err)
	}
	return invalid("%s", strings.Join(sentences, "; "))
}

func (e Evidence) validate(field string) error {
	var found []error
	note := func(err error) {
		if err != nil {
			found = append(found, err)
		}
	}
	if !e.Kind.Valid() {
		note(invalid("%s.kind is %q and must be one of task, page, "+
			"turn, run or url", field, e.Kind))
	}
	note(checkText(field+".ref", e.Ref, MaxEvidenceRef, true))
	note(checkText(field+".label", e.Label, MaxEvidenceLabel, false))
	if e.Kind == EvidenceURL {
		// HTTPS AND ABSOLUTE, because the ref is rendered as a link a
		// person clicks: a relative ref resolves against whichever page
		// shows it, and a `javascript:` or `http:` one is a link nobody
		// should be handed from a record an agent wrote.
		parsed, err := url.Parse(e.Ref)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			note(invalid("%s.ref is %q and a url must be an absolute "+
				"https:// address", field, e.Ref))
		}
	}
	return joinInvalid(found)
}

// checkText refuses an empty required value and one over its cap, in bytes —
// the unit every tracker cap counts in.
func checkText(field, value string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(value) == "":
		return invalid("%s is empty", field)
	case len(value) > limit:
		return invalid("%s is %d bytes and the maximum is %d — it is "+
			"refused rather than cut", field, len(value), limit)
	}
	return nil
}

// EvidenceLookup is the world a decision's evidence is checked against,
// declared here — by the one caller that needs it — and implemented by the
// surface that writes the decision, which holds both readers.
type EvidenceLookup interface {
	// Task resolves a key or an id to the task's id and every key it has
	// been known by, or [ErrNoTask].
	Task(ctx context.Context, ref string) (CitedTask, error)

	// PageID resolves a page reference — an id, or the
	// "CONTAINER/Title" address a person and a model name a page by — to
	// the id of a LIVE page, reporting false for none. An error is the
	// lookup failing, never "no".
	PageID(ctx context.Context, ref string) (id string, found bool, err error)
}

// CitedTask is a cited task as [EvidenceLookup] resolves it.
type CitedTask struct {
	ID string
	// Keys are its key and its former keys: what a label may already say
	// of it ([labelAfterKey]).
	Keys []string
}

// labelAfterKey is a task evidence's label without a leading copy of one of
// the task's keys, and the separator after it.
//
// A TASK'S LINK IS DRAWN AS ITS CURRENT KEY, THEN ITS LABEL, so a label that
// starts with the key reads it twice ("GP-20 GP-20 sitemap fix"). An asker
// writes a label as a link would say it, and a link to a task says its key,
// so the label is stored as what follows the key. A former key is stripped
// too: the label was written while the task carried it, and drawn after the
// current key it names the task under a key that is not its own.
func labelAfterKey(label string, keys []string) string {
	for _, key := range keys {
		rest, ok := strings.CutPrefix(label, key)
		if !ok || key == "" {
			continue
		}
		// A WHOLE KEY ONLY: GP-2 is not the start of GP-20, nor GP-20 of
		// GP-20-b. The boundary is the one a key is recognised at everywhere
		// else (`dashboard/src/lib/inlineTokens.ts`, rule 5).
		if r, _ := utf8.DecodeRuneInString(rest); rest != "" &&
			(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			continue
		}
		return strings.TrimLeftFunc(rest, keySeparator)
	}
	return label
}

// keySeparator is what an asker writes between a key and what it is: a space,
// a colon, a comma, a stop, a dash or a middle dot.
func keySeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(":,.-\u2013\u2014\u00b7|", r)
}

// ValidateDecision is [Decision.Validate] plus the checks that need the
// world, and returns the decision as it is to be stored.
//
// A TASK IS RESOLVED TO ITS ID, because a key is retired by a move and the
// record is kept for ever, and its label loses a leading copy of its key,
// which the link already says ([labelAfterKey]); a PAGE MUST EXIST, because a decision citing a page
// that is not there sends the person deciding to a dead link at the moment
// they are weighing it — and it is stored by ITS id too, for the task's
// reason: a page's address is its title, and a rename retires the title. Turns, runs and URLs are not looked up: a turn or a
// run is named by the asker's own tooling, and a URL is outside the company.
//
// A lookup that FAILS is returned unmarked — the node could not check, which
// is not something the caller can fix by changing the decision.
func ValidateDecision(ctx context.Context, d Decision, lookup EvidenceLookup) (Decision, error) {
	if err := d.Validate(); err != nil {
		return Decision{}, err
	}
	if len(d.Evidence) == 0 {
		return d, nil
	}
	out := d
	out.Evidence = make([]Evidence, len(d.Evidence))
	copy(out.Evidence, d.Evidence)
	// EVERY MISSING REFERENCE AT ONCE; a lookup that fails is the node's,
	// and answered at once as itself.
	var missing []error
	for i, evidence := range out.Evidence {
		field := fmt.Sprintf("decision.evidence[%d]", i)
		switch evidence.Kind {
		case EvidenceTask:
			task, err := lookup.Task(ctx, evidence.Ref)
			switch {
			case errors.Is(err, ErrNoTask):
				missing = append(missing, invalid("%s.ref names the work "+
					"item %q, which does not exist", field, evidence.Ref))
				continue
			case err != nil:
				return Decision{}, fmt.Errorf("worklet: resolve %s.ref %q: %w",
					field, evidence.Ref, err)
			}
			out.Evidence[i].Ref = task.ID
			out.Evidence[i].Label = labelAfterKey(evidence.Label, task.Keys)
		case EvidencePage:
			id, exists, err := lookup.PageID(ctx, evidence.Ref)
			switch {
			case err != nil:
				return Decision{}, fmt.Errorf("worklet: check %s.ref %q: %w",
					field, evidence.Ref, err)
			case !exists:
				missing = append(missing, invalid("%s.ref names the page "+
					"%q, which does not exist", field, evidence.Ref))
				continue
			}
			out.Evidence[i].Ref = id
		}
	}
	if err := joinInvalid(missing); err != nil {
		return Decision{}, err
	}
	return out, nil
}

// checkCommentShape is what a comment's decision and choice must be on their
// own, before any row is read: a decision rides only the comment that asks,
// and a choice only a comment that answers.
func checkCommentShape(task string, c *Comment) error {
	if c.Decision != nil {
		if c.Ask == "" {
			return invalid("comment %s on task %s carries a decision "+
				"and asks nobody — a decision is a question put to somebody, "+
				"so it needs `ask`", c.ID, task)
		}
		if err := c.Decision.Validate(); err != nil {
			return err
		}
		// AN INFORM IS A PROMISE A TURN KEEPS, so only an asker that HAS
		// turns may make one: the engine enforces it by holding the
		// asker's answered turn open until a tool on the named surface has
		// run ([Owes]). A person — at the dashboard, through an assistant,
		// or as a human seat — has no turn to hold, so an inform they
		// asked for is a line on a card that nothing would ever keep.
		if c.Decision.Inform != nil && c.AuthorKind != AuthorAgent {
			return invalid("comment %s on task %s asks the engine to "+
				"report the outcome in %s, and only an agent seat's ask may — "+
				"a %s author has no turn the engine can hold to it; post it "+
				"yourself once it is decided", c.ID, task,
				c.Decision.Inform.Channel, cmp.Or(string(c.AuthorKind), "nameless"))
		}
	}
	if c.Choice != "" && (c.Answers == nil || *c.Answers == "") {
		return invalid("comment %s on task %s makes the choice %q and "+
			"answers nothing — a choice is an answer to a decision, so it "+
			"needs `answers`", c.ID, task, c.Choice)
	}
	return nil
}

// checkChoice is the check that an answer's choice names an option of the
// decision it answers. No choice is always an answer: a person may answer a
// decision in prose — "none of these" is an answer too.
func checkChoice(task, ask string, decision *Decision, choice string) error {
	if choice == "" {
		return nil
	}
	if decision == nil {
		return invalidSaying(caveat.QuestionHasNoOptions(), "the ask %s on task %s carries no decision, "+
			"so there is nothing for the choice %q to name — answer it in the "+
			"body", ask, task, choice)
	}
	if _, ok := decision.Option(choice); !ok {
		return invalidSaying(caveat.NotAnOption(), "the choice %q is not an option of the "+
			"decision in %s on task %s — it offers %s", choice, ask, task,
			strings.Join(decision.OptionIDs(), ", "))
	}
	return nil
}
