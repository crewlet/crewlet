// Package decision is the shape of a structured ask: the question somebody is
// asked to choose on, two to six options, what the asker recommends and why,
// what it looked at, and the role the person is asked in (ADR-0023).
//
// # One shape, wherever an agent asks a person
//
// An agent asks a person from two places. A seat's turn puts a decision on a
// work item, as the `ask` comment that carries it (internal/worklet). A coding
// run that cannot go on without a person's answer parks on it, and its ask
// rides the run's coordination row until somebody answers (internal/sandbox,
// ADR-0068). The person deciding reads both as one thing, the dashboard draws
// both with one set of components, and an agent learns one shape for both. So
// the shape, its caps and its structural contract live here, below both
// packages, rather than in either: internal/worklet already depends on
// internal/sandbox, so a decision declared in the tracker could never reach a
// run's row.
//
// # What lives here, and what does not
//
// The VALUES alone: the types, the caps and [Decision.Validate], which is the
// whole structural contract and needs nothing but the value. What needs the
// world stays with the writer that has it: a task reference resolving and a
// page existing are internal/worklet's ([worklet.ValidateDecision]); that an
// ask carries its decision only on the comment that asks, that an `inform` is
// an agent seat's, and that a choice names an option of the ask it answers
// against that ask's own row are the tracker's and the run record's, each
// checked inside the write that commits it.
//
// # Refusals
//
// Every refusal is marked [ErrInvalid] and names the field, because each is
// something the caller changes. Nothing is trimmed, defaulted or cut: a
// decision is stored exactly as it was accepted. A refusal's text is the
// sentence a model or a person reads, with no package prefix, so a writer
// that wraps it into its own refusal keeps the words unchanged.
package decision

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/words"
)

// The decision caps. Each is refused at write naming the field, never cut,
// the rule every tracker cap follows, and each is sized to where the value is
// RENDERED, because a decision is read by a person deciding at a glance and by
// a model composing the answering call.
const (
	// MaxQuestion is one sentence on a card. The long form (the context,
	// the history, the tradeoffs) is the ask's own body, which takes far
	// more; a question longer than this is a body that was put in the wrong
	// field.
	MaxQuestion = 300

	// MinOptions and MaxOptions bound the choice. One option is not a
	// choice, it is an approval, which is an ask with the two options
	// "yes" and "no"; past six a person can no longer weigh them side by
	// side on one card, which is the one thing structuring the ask was for.
	// The asking guidance recommends two to four; six is the ceiling a
	// well-meant long list still fits under.
	MinOptions = 2
	MaxOptions = 6

	// MaxOptionLabel is a button's text. MaxOptionDetail is the line under
	// it, and MaxEvidenceLabel is a link's text, sized like a label for the
	// same reason.
	MaxOptionLabel   = 80
	MaxOptionDetail  = 500
	MaxEvidenceLabel = 80

	// MaxRationale is why the asker recommends what it does: a paragraph or
	// three. Longer is an argument, and an argument is a page the evidence
	// can link to.
	MaxRationale = 1500

	// MaxEvidence is how many things the asker may cite. Eight is what a
	// card lists without scrolling; a decision resting on more is one whose
	// evidence belongs on a page, cited once.
	MaxEvidence = 8

	// MaxEvidenceRef bounds a reference. An id is far shorter; a URL is the
	// long case, and 2048 is the length every browser and proxy in common
	// use still carries whole.
	MaxEvidenceRef = 2048

	// MaxInformChannel is Slack's own channel-name ceiling, the longest of
	// the three chat surfaces' (Mattermost's and Chatlet's are both 64).
	MaxInformChannel = 80
)

// optionIDPattern is what an option id may be.
//
// A SLUG, because it is typed back: the answering call names it in `choice`, a
// model composes that call from the ask it read, and the dashboard sends it
// from a button. Lower case with digits, `_` and `-` is what every one of
// those spells the same way, and 32 keeps it an identifier rather than a
// sentence somebody would paraphrase.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var optionIDPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
})

// Decision is the structure an ask carries when it needs somebody to choose.
type Decision struct {
	// Question is what is being decided, in one sentence.
	Question string `json:"question" untrusted:"name"`

	// Options are the choices, in the order the asker put them.
	Options []Option `json:"options"`

	// Recommended is the option the asker would choose, by id, or empty
	// when it has no view.
	Recommended string `json:"recommended,omitempty"`

	// Rationale is why it recommends that one.
	Rationale string `json:"rationale,omitempty" untrusted:"name"`

	// Evidence is what the asker looked at.
	Evidence []Evidence `json:"evidence,omitempty"`

	// Role is what the person asked is being asked AS.
	Role Role `json:"role"`

	// Inform is where the asker will say what was decided, once it is.
	// Nil when the answer stays where it was given.
	Inform *Inform `json:"inform,omitempty"`
}

// Option is one choice.
type Option struct {
	ID     string `json:"id"`
	Label  string `json:"label" untrusted:"name"`
	Detail string `json:"detail,omitempty" untrusted:"name"`
}

// Role is what the person asked is being asked as.
//
// A NAMED STRING WITH [Role.Valid], so an unknown value off the wire is a value
// rather than a panic, and it has NO ZERO: an ask that did not say whether the
// answer is the decision or an input to it leaves the person answering it
// guessing at the one thing the structure was for.
type Role string

const (
	// RoleApprover is a person whose answer IS the decision.
	RoleApprover Role = "approver"
	// RoleContributor is a person whose answer is an input to a decision
	// somebody else makes.
	RoleContributor Role = "contributor"
)

// Valid reports whether r is a role this build knows.
func (r Role) Valid() bool {
	return r == RoleApprover || r == RoleContributor
}

// EvidenceKind is what a piece of evidence points at.
type EvidenceKind string

const (
	// EvidenceTask is a work item. Its reference is resolved to the task's
	// id at write, because a key moves with the task and an id does not.
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
// A PROMISE THE ENGINE KEEPS, and only where a turn can be held to it: the
// tracker accepts one only on an agent seat's ask, and the asker's `answered`
// wake then owes that surface (internal/worklet). Here it is only a value: a
// surface is one of the chat surfaces (the two vendor integrations, or the
// engine's own chat) and a channel is a name.
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
	// ITS VALUE IS CHATLET'S FEED SOURCE, `chat`, and has to be: an inform
	// is kept by OWING the surface ([worklet.Owes]), the asker's answered
	// turn held until a tool that DELIVERS there has run, and Chatlet's
	// posting tools declare their delivery as that source. Any other
	// spelling is a promise no tool could ever discharge. This package
	// cannot import the chat domain, so a test where both are visible holds
	// the two equal.
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
func (d Decision) Option(id string) (Option, bool) {
	for _, option := range d.Options {
		if option.ID == id {
			return option, true
		}
	}
	return Option{}, false
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

// ErrInvalid marks every refusal this package writes: the value is wrong, and
// the sentence says which field to change.
var ErrInvalid = errors.New("decision: invalid")

// refusal is a sentence marked [ErrInvalid]. Its text is the sentence alone,
// with no package prefix, because it is what a model or a person reads.
type refusal struct{ sentence string }

func (e *refusal) Error() string        { return e.sentence }
func (e *refusal) Is(target error) bool { return target == ErrInvalid }

// invalid is fmt.Sprintf for a refusal, marked [ErrInvalid].
func invalid(format string, args ...any) error {
	return &refusal{sentence: fmt.Sprintf(format, args...)}
}

// Refuse is a refusal of a field a writer holds beside a decision (a run's
// ask holds its context and its branch), marked [ErrInvalid] like every
// refusal here, so [Join] carries it with the decision's own in one sentence.
func Refuse(format string, args ...any) error { return invalid(format, args...) }

// Validate is the whole structural contract of a decision: every fault at
// once, joined into one refusal (ADR-0059), because a decision is written
// again once it is refused and a refusal naming one fault of several spends
// that attempt on the next. Each fault names its field under `decision.`, as
// a work item's ask carries it ([Decision.ValidateAt]).
func (d Decision) Validate() error { return d.ValidateAt(FieldPrefix) }

// FieldPrefix is where a work item's ask carries its decision: the argument
// `decision`, whose fields a refusal names as `decision.options[0].id`.
const FieldPrefix = "decision."

// ValidateAt is [Decision.Validate] naming each field under prefix, the path
// its caller's arguments carry the decision at: `decision.` on a work item's
// ask, and nothing on a coding run's, whose arguments carry the decision's
// fields at their top (`options[0].label`, `recommended`). A refusal names a
// field the way the caller wrote it, or the caller is told to fix an argument
// it never sent.
func (d Decision) ValidateAt(prefix string) error {
	var found []error
	note := func(err error) {
		if err != nil {
			found = append(found, err)
		}
	}
	note(CheckText(prefix+"question", d.Question, MaxQuestion, true))
	switch n := len(d.Options); {
	case n < MinOptions:
		note(invalid("a decision has %d option(s) and needs at "+
			"least %d: a single option is an approval, which is the "+
			"options yes and no", n, MinOptions))
	case n > MaxOptions:
		note(invalid("a decision has %d options and the maximum is "+
			"%d: a person cannot weigh more side by side; narrow them, or "+
			"ask about the shortlist first", n, MaxOptions))
	}
	seen := make(map[string]bool, len(d.Options))
	for i, option := range d.Options {
		field := fmt.Sprintf("%soptions[%d]", prefix, i)
		switch {
		case !optionIDPattern().MatchString(option.ID):
			note(invalid("%s.id is %q and must be 1 to 32 of "+
				"a-z, 0-9, `_` and `-`: it is typed back in `choice`",
				field, option.ID))
		case seen[option.ID]:
			note(invalid("%s.id %q names two options: an answer "+
				"naming it would choose both", field, option.ID))
		}
		seen[option.ID] = true
		note(CheckText(field+".label", option.Label, MaxOptionLabel, true))
		note(CheckText(field+".detail", option.Detail, MaxOptionDetail, false))
	}
	if d.Recommended != "" && !seen[d.Recommended] {
		note(invalid("%srecommended is %q, which is not one "+
			"of its options (%s)", prefix, d.Recommended, words.Or(d.OptionIDs())))
	}
	note(CheckText(prefix+"rationale", d.Rationale, MaxRationale, false))
	if len(d.Evidence) > MaxEvidence {
		note(invalid("a decision cites %d pieces of evidence and the "+
			"maximum is %d: put the rest on a page and cite the page",
			len(d.Evidence), MaxEvidence))
	}
	for i, evidence := range d.Evidence {
		note(evidence.validate(fmt.Sprintf("%sevidence[%d]", prefix, i)))
	}
	if !d.Role.Valid() {
		note(invalid("%srole is %q and must be %q (the "+
			"answer is the decision) or %q (the answer is an input to it)",
			prefix, d.Role, RoleApprover, RoleContributor))
	}
	if d.Inform != nil {
		if !d.Inform.Surface.Valid() {
			note(invalid("%sinform.surface is %q and must be one of "+
				"%s", prefix, d.Inform.Surface, words.Or(InformSurfaces())))
		}
		note(CheckText(prefix+"inform.channel", d.Inform.Channel,
			MaxInformChannel, true))
	}
	return Join(found)
}

// CheckChoice is the check that an answer's choice names one of the
// decision's options. An empty choice is always an answer: a person may
// answer a decision in words, and "none of these" is an answer too. The
// refusal lists the options, which is what the caller needs to choose again.
func (d Decision) CheckChoice(choice string) error {
	if choice == "" {
		return nil
	}
	if _, ok := d.Option(choice); !ok {
		return invalid("the choice %q is not an option of this decision: it offers %s",
			choice, words.Or(d.OptionIDs()))
	}
	return nil
}

// Join is several refusals of one value as one, every sentence in order: nil
// for none, the refusal itself for one. Every error in found must be marked
// [ErrInvalid]; one that is not is answered alone, as itself, because it is a
// failure rather than something the caller can change.
func Join(found []error) error {
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
		sentences[i] = err.Error()
	}
	return &refusal{sentence: strings.Join(sentences, "; ")}
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
	note(CheckText(field+".ref", e.Ref, MaxEvidenceRef, true))
	note(CheckText(field+".label", e.Label, MaxEvidenceLabel, false))
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
	return Join(found)
}

// CheckText refuses an empty required value and one over its cap, in bytes,
// the unit every cap here counts in. Exported for the writers that hold a
// field of their own beside a decision (a run's ask holds its context), so
// every field of one ask is refused in the same words.
func CheckText(field, value string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(value) == "":
		return invalid("%s is empty", field)
	case len(value) > limit:
		return invalid("%s is %d bytes and the maximum is %d: it is "+
			"refused rather than cut", field, len(value), limit)
	}
	return nil
}
