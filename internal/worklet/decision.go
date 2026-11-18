package worklet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/words"
)

// A decision is a structured ask (package doc, "A decision is a structured
// ask, and nothing more"). Its shape is [decision.Decision], shared with the
// ask an executor parks on (ADR-0068); this file adds only what the tracker can
// check: cited tasks and pages exist ([ValidateDecision]), an agent's decision
// recommends an option and says why (ADR-0100), a decision rides only an asking
// comment and an `inform` only an agent seat's ([checkCommentShape]), and a
// choice names an option of the ask it answers (the writer's decide).
//
// Immutable once asked, as answers name options by id, and no write is refused
// for trying: a post naming an existing remark is answered from its row
// ([postedComment]); [Writer.EditComment] edits from the stored row, decision
// whole.

// checkText is [decision.CheckText] for an optional value, marked [ErrInvalid]
// with the sentence unchanged: an answered decision keeps its ask's caps.
func checkText(field, value string, limit int) error {
	if err := decision.CheckText(field, value, limit, false); err != nil {
		return invalid("%w", err)
	}
	return nil
}

// validDecision is the contract an authored decision is held to, faults named
// under prefix: an agent's must recommend an option and say why (ADR-0100); a
// person's only the shape, since a person asking may have no view.
func validDecision(d decision.Decision, author AuthorKind, prefix string) error {
	validate := d.ValidateAt
	if author == AuthorAgent {
		validate = d.ValidateRecommendedAt
	}
	if err := validate(prefix); err != nil {
		return invalid("%w", err)
	}
	return nil
}

// EvidenceLookup is the world a decision's evidence is checked against,
// implemented by the surface that writes the decision, which holds both readers.
type EvidenceLookup interface {
	// Task resolves a key or an id to the task's id and every key it has
	// been known by, or [ErrNoTask].
	Task(ctx context.Context, ref string) (CitedTask, error)

	// PageID resolves a page id or "CONTAINER/Title" address to the id of a
	// LIVE page, false for none. An error is the lookup failing, never "no".
	PageID(ctx context.Context, ref string) (id string, found bool, err error)

	// OwnArtifact reads ref as the address this engine hands out for one of
	// its artifacts and answers that artifact's id, or false for any other.
	OwnArtifact(ref string) (id string, ok bool)
}

// CitedTask is a cited task as [EvidenceLookup] resolves it.
type CitedTask struct {
	ID string
	// Keys are its key and its former keys: what a label may already say
	// of it ([labelAfterKey]).
	Keys []string
}

// labelAfterKey is a task evidence's label without a leading copy of one of
// the task's keys, and the separator after it: the link is drawn as the
// current key then the label, which would otherwise read "GP-20 GP-20 sitemap
// fix". A former key is stripped too, or the link would name the task under a
// key no longer its own.
func labelAfterKey(label string, keys []string) string {
	for _, key := range keys {
		rest, ok := strings.CutPrefix(label, key)
		if !ok || key == "" {
			continue
		}
		// A whole key only: GP-2 is not the start of GP-20, nor GP-20 of
		// GP-20-b (the boundary of `dashboard/src/lib/inlineTokens.ts`, rule 5).
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

// joinInvalid is several [ErrInvalid] refusals of one value as one, every
// sentence in order: nil for none, the refusal itself for one. An error that is
// not [ErrInvalid] is returned alone, as itself.
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
func ValidateDecision(ctx context.Context, d decision.Decision, author AuthorKind,
	lookup EvidenceLookup) (decision.Decision, error) {
	return ValidateDecisionAt(ctx, d, author, lookup, decision.FieldPrefix)
}

// ValidateDecisionAt is [validDecision] plus the checks that need the world, in
// ONE PASS (ADR-0059): every fault comes back in one refusal, each named under
// prefix. It returns the decision as it is to be stored.
//
// A task and a page are stored by id, since a move retires a key and a rename
// a page's title-address, and a page must exist, or the person deciding follows
// a dead link; a task's label loses its leading key ([labelAfterKey]). Turns,
// runs and URLs are not looked up: the asker's tooling names the first two, and
// a URL is outside the company. A reference the shape already refused is not
// looked up, so it is named once. A lookup that FAILS is returned unmarked: the
// caller cannot fix it by changing the decision.
func ValidateDecisionAt(ctx context.Context, d decision.Decision, author AuthorKind,
	lookup EvidenceLookup, prefix string) (decision.Decision, error) {

	d = ownLinksCited(d, lookup)
	var found []error
	if err := validDecision(d, author, prefix); err != nil {
		found = append(found, err)
	}
	if !decision.EngineOptions(ctx) {
		if err := d.ReservedFaultAt(prefix); err != nil {
			found = append(found, invalid("%w", err))
		}
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
				// Nothing to check it against (a company on another
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

// ownLinksCited is d with every url this engine handed out for one of its
// artifacts cited as that artifact (ADR-0160): its address is on the
// deployment's own URL, which may be http, and the kind is a link that is not.
func ownLinksCited(d decision.Decision, lookup EvidenceLookup) decision.Decision {
	cited := slices.Clone(d.Evidence)
	for i, evidence := range cited {
		if evidence.Kind != decision.EvidenceURL {
			continue
		}
		if id, ok := lookup.OwnArtifact(evidence.Ref); ok {
			cited[i].Kind, cited[i].Ref = decision.EvidenceArtifact, id
		}
	}
	d.Evidence = cited
	return d
}

// lookupSentence is a lookup's refusal without the "worklet: invalid: " prefix,
// which says where the error came from rather than what to change.
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
		if err := validDecision(*c.Decision, c.AuthorKind, decision.FieldPrefix); err != nil {
			return err
		}
		// An inform is a promise a turn keeps: the engine holds the asker's
		// answered turn open until a tool on the named surface has run
		// ([Owes]). A person has no turn to hold, so nothing would keep it.
		if c.Decision.Inform != nil && c.AuthorKind != AuthorAgent {
			return invalid("comment %s on task %s asks the engine to "+
				"report the outcome in %s, and only an agent seat's ask may — "+
				"a %s author has no turn the engine can hold to it; post it "+
				"yourself once it is decided", c.ID, task,
				c.Decision.Inform.Channel, cmp.Or(string(c.AuthorKind), "nameless"))
		}
	}
	if a := c.Act; a != nil {
		if err := a.Validate(); err != nil {
			return invalid("comment %s on task %s: %w", c.ID, task, err)
		}
		if c.Decision == nil {
			return invalid("comment %s on task %s carries an act and no decision to approve it by",
				c.ID, task)
		}
		if _, ok := c.Decision.Option(a.Approve); !ok {
			return invalid("comment %s on task %s carries an act whose approving option %q is not an "+
				"option of its decision", c.ID, task, a.Approve)
		}
	}
	if c.Choice != "" && (c.Answers == nil || *c.Answers == "") {
		return invalid("comment %s on task %s makes the choice %q and "+
			"answers nothing — a choice is an answer to a decision, so it "+
			"needs `answers`", c.ID, task, c.Choice)
	}
	return nil
}

// checkChoice checks that an answer's choice names an option of the decision
// it answers. No choice is always valid: "none of these", in prose, is an answer.
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
