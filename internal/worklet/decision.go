package worklet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/words"
)

// A DECISION IS A STRUCTURED ASK: ADR-0023.
//
// A question that needs somebody to choose (ship or hold, which region, which
// of three designs) used to travel as prose on an `ask` comment, and every
// part a person needs to answer it well was optional and unplaced: the
// options were somewhere in the body, the recommendation was a sentence among
// others, the evidence was a list of links a reader had to go and find, and
// the answer was prose the asker then had to interpret back into one of the
// options it had in mind. So the ask carries a [decision.Decision] (the
// question, two to six options, what the asker recommends and why, what it
// looked at, and the role the person is asked in) and the answer carries a
// [Comment.Choice] naming one of those options by id.
//
// # The shape is internal/decision's, the record is this package's
//
// The value and its structural contract are [decision.Decision], shared with
// the ask a coding run parks on (ADR-0068), so an agent asks a person in one
// shape wherever it asks from. What this file adds is what only the tracker
// can check: that a cited task and page exist ([ValidateDecision]), that a
// decision rides only the comment that asks and an `inform` only an agent
// seat's ([checkCommentShape]), and that an answer's choice names an option
// OF THE ASK IT ANSWERS (the writer's decide, against the ask's own row).
// Nothing else. There is no approval chain, no quorum, no state machine and
// no escalation timer: a decision is still ONE ask to ONE person, answered
// once, and what the asker does with the choice is the asker's turn.
//
// # Immutable once asked
//
// A decision is set only on the comment that ASKS, and never changes after:
// the answer names an option by id, so an ask whose options could be edited
// under it would turn an answer given to one set of options into an answer
// to another. No write can change it, so none is refused for trying: a post
// naming a remark already on the thread is answered from its row
// ([postedComment]), and the one door to an edit is [Writer.EditComment],
// which forms it from the stored row and so carries the decision whole.

// checkText is [decision.CheckText] for an optional value, as this package's
// refusal, marked [ErrInvalid] with the sentence unchanged: what an answered
// decision carries is bounded by the caps its ask was written under, and may
// be empty.
func checkText(field, value string, limit int) error {
	if err := decision.CheckText(field, value, limit, false); err != nil {
		return invalid("%w", err)
	}
	return nil
}

// validDecision is [decision.Decision.Validate] as this package's refusal.
func validDecision(d decision.Decision) error {
	if err := d.Validate(); err != nil {
		return invalid("%w", err)
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

// ValidateDecision is [ValidateDecisionAt] for a work item's ask, whose
// decision is its `decision` argument.
func ValidateDecision(ctx context.Context, d decision.Decision, lookup EvidenceLookup,
) (decision.Decision, error) {
	return ValidateDecisionAt(ctx, d, lookup, decision.FieldPrefix)
}

// ValidateDecisionAt is [decision.Decision.ValidateAt] plus the checks that
// need the world, in ONE PASS (ADR-0059): every fault of the shape and every
// reference that resolves to nothing come back in one refusal, each named
// under prefix as the caller's arguments carry the decision. It returns the
// decision as it is to be stored.
//
// A TASK IS RESOLVED TO ITS ID, because a key is retired by a move and the
// record is kept for ever, and its label loses a leading copy of its key,
// which the link already says ([labelAfterKey]); a PAGE MUST EXIST, because a
// decision citing a page that is not there sends the person deciding to a dead
// link at the moment they are weighing it, and it is stored by ITS id too, for
// the task's reason: a page's address is its title, and a rename retires the
// title. Turns, runs and URLs are not looked up: a turn or a run is named by
// the asker's own tooling, and a URL is outside the company. A reference the
// shape already refused (no kind, no ref) is not looked up, so it is named
// once.
//
// A lookup that FAILS is returned unmarked: the node could not check, which
// is not something the caller can fix by changing the decision.
func ValidateDecisionAt(ctx context.Context, d decision.Decision, lookup EvidenceLookup,
	prefix string) (decision.Decision, error) {

	var found []error
	if err := d.ValidateAt(prefix); err != nil {
		found = append(found, invalid("%w", err))
	}
	out := d
	out.Evidence = make([]decision.Evidence, len(d.Evidence))
	copy(out.Evidence, d.Evidence)
	for i, evidence := range out.Evidence {
		field := fmt.Sprintf("%sevidence[%d]", prefix, i)
		if strings.TrimSpace(evidence.Ref) == "" {
			continue
		}
		switch evidence.Kind {
		case decision.EvidenceTask:
			task, err := lookup.Task(ctx, evidence.Ref)
			switch {
			case errors.Is(err, ErrNoTask):
				found = append(found, invalid("%s.ref names the work "+
					"item %q, which does not exist", field, evidence.Ref))
				continue
			case errors.Is(err, ErrInvalid):
				// NOTHING TO CHECK IT AGAINST (a company on another
				// tracker), which the caller changes: a fault like any.
				found = append(found, invalid("%s.ref: %s", field, lookupSentence(err)))
				continue
			case err != nil:
				return decision.Decision{}, fmt.Errorf("worklet: resolve %s.ref %q: %w",
					field, evidence.Ref, err)
			}
			out.Evidence[i].Ref = task.ID
			out.Evidence[i].Label = labelAfterKey(evidence.Label, task.Keys)
		case decision.EvidencePage:
			id, exists, err := lookup.PageID(ctx, evidence.Ref)
			switch {
			case errors.Is(err, ErrInvalid):
				found = append(found, invalid("%s.ref: %s", field, lookupSentence(err)))
				continue
			case err != nil:
				return decision.Decision{}, fmt.Errorf("worklet: check %s.ref %q: %w",
					field, evidence.Ref, err)
			case !exists:
				found = append(found, invalid("%s.ref names the page "+
					"%q, which does not exist", field, evidence.Ref))
				continue
			}
			out.Evidence[i].Ref = id
		}
	}
	if err := joinInvalid(found); err != nil {
		return decision.Decision{}, err
	}
	if len(d.Evidence) == 0 {
		out.Evidence = d.Evidence
	}
	return out, nil
}

// lookupSentence is a lookup's refusal as the sentence a caller reads, without
// the class's own words in front ("worklet: invalid: "), which say where the
// error came from rather than what to change.
func lookupSentence(err error) string {
	return strings.TrimPrefix(Sentence(err), ErrInvalid.Error()+": ")
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
		if err := validDecision(*c.Decision); err != nil {
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
func checkChoice(task, ask string, d *decision.Decision, choice string) error {
	if choice == "" {
		return nil
	}
	if d == nil {
		return invalidSaying(caveat.QuestionHasNoOptions(), "the ask %s on task %s carries no decision, "+
			"so there is nothing for the choice %q to name — answer it in the "+
			"body", ask, task, choice)
	}
	if _, ok := d.Option(choice); !ok {
		return invalidSaying(caveat.NotAnOption(), "the choice %q is not an option of the "+
			"decision in %s on task %s — it offers %s", choice, ask, task,
			words.Or(d.OptionIDs()))
	}
	return nil
}
