package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A DECISION IS AN ARGUMENT OF TWO TOOLS, stated once.
//
// `comment_on_work_item` asks a question on an item that exists, and
// `create_work_item` files an item AS a question — the "ask an agent"
// gesture, one record rather than a create and a comment. Both take the same
// `decision` object with the same schema, the same parse and the same checks
// against the world, because two copies of a structure a model composes from a
// schema are two chances to describe it differently, and the model reads
// whichever it met last.

// decisionSchema is the `decision` argument's JSON Schema.
//
// EVERY CAP IS IN A DESCRIPTION, read off the decision's own constants, so the
// number a model is told is the number it is refused at.
func decisionSchema() map[string]any {
	properties := choiceProperties()
	properties["question"] = map[string]any{
		"type": "string",
		"description": fmt.Sprintf("What is being decided, in one "+
			"sentence of at most %d bytes. The context goes in `body`.",
			decision.MaxQuestion),
	}
	properties["inform"] = map[string]any{
		"type": "object",
		"description": "The chat channel you will report the outcome in " +
			"once it is decided: a surface you can post on — `chat` is " +
			"the company's own — and a channel a team in the org chart " +
			"declares. The person answering is told you will post it " +
			"there, and your turn woken by their answer is not finished " +
			"until you have.",
		"properties": map[string]any{
			"surface": map[string]any{
				"type": "string",
				"enum": informSurfaces(),
			},
			"channel": map[string]any{"type": "string"},
		},
		"required": []any{"surface", "channel"},
	}
	return map[string]any{
		"type": "object",
		"description": "The question as a choice: the options, the one you " +
			"recommend and why, and what you looked at. Only with `ask`, and " +
			"REQUIRED of an agent whose `ask` names a person: every question an " +
			"agent puts to a person offers options they answer with one press, " +
			"and they can still answer in their own words. The person answers " +
			"by naming an option in `choice`, and you are woken with their choice.",
		"properties": properties,
		// THE RECOMMENDATION IS NOT LISTED: a person asking through these
		// tools may have no view, and an agent's is required by its validator.
		"required": []any{"question", "options", "role"},
	}
}

// choiceProperties are the schema of a decision's CHOICE (its options, the one
// recommended and why, what was looked at, and the role the person is asked
// in), which every tool that asks a person to choose takes alike: a work
// item's ask under `decision` ([decisionSchema]), and an executor's ask at the
// top of its arguments ([AskAndWait]). One builder, so the shape a model learns
// on one is the shape it meets on the other. A fresh map on every call: each
// caller adds the properties of its own.
func choiceProperties() map[string]any {
	option := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "A short slug the answer names in `choice`: 1 " +
					"to 32 of a-z, 0-9, `_` and `-`.",
			},
			"label": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The option as a button would "+
					"say it, at most %d bytes.", decision.MaxOptionLabel),
			},
			"detail": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What choosing it means — its "+
					"cost, its risk, what happens next. At most %d bytes.",
					decision.MaxOptionDetail),
			},
		},
		"required": []any{"id", "label"},
	}
	evidence := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []any{
					string(decision.EvidenceTask), string(decision.EvidencePage),
					string(decision.EvidenceTurn), string(decision.EvidenceRun),
					string(decision.EvidenceURL),
				},
			},
			"ref": map[string]any{
				"type": "string",
				"description": "A work item, a page's id or CONTAINER/Title, " +
					"a turn or run id, or an https:// URL. For a work item: " +
					itemRef,
			},
			"label": map[string]any{
				"type": "string",
				"description": "What it is, as a link would say it. A work " +
					"item's link says its key first, so its label is what " +
					"follows the key (\"sitemap fix\", not \"GP-20 sitemap fix\").",
			},
		},
		"required": []any{"kind", "ref"},
	}
	return map[string]any{
		"options": map[string]any{
			"type": "array",
			"description": fmt.Sprintf("%d to %d concrete answers the person "+
				"can give with one press. For a question you would ask in "+
				"words, offer the answers you expect; \"Something else\" is "+
				"always theirs to answer in words.", decision.MinOptions,
				decision.MaxOptions),
			"items": option,
		},
		"recommended": map[string]any{
			"type": "string",
			"description": "The id of the option you would choose. Required of an " +
				"agent: the person sees it marked Recommended and accepts it with " +
				"one press.",
		},
		"rationale": map[string]any{
			"type": "string",
			"description": fmt.Sprintf("Why you recommend it, in a sentence "+
				"or two, at most %d bytes. Required of an agent.", decision.MaxRationale),
		},
		"evidence": map[string]any{
			"type": "array",
			"description": fmt.Sprintf("What you looked at, at most %d. A "+
				"work item or page must exist.", decision.MaxEvidence),
			"items": evidence,
		},
		"role": map[string]any{
			"type": "string",
			"enum": []any{string(decision.RoleApprover), string(decision.RoleContributor)},
			"description": "`approver` when their answer IS the decision; " +
				"`contributor` when it is an input to one somebody else makes.",
		},
	}
}

// readDecision parses and checks a `decision` argument, or returns the refusal.
//
// STRICT ABOUT KEYS, because every one a model misspells is a part of the
// question the person deciding never sees: `recommendation` for `recommended`
// would store a decision with no recommendation and report it applied. So an
// unknown key is refused by name rather than dropped.
//
// THE WORLD IS CHECKED HERE, before anything is published: a task the
// evidence names is resolved to its id and a page must exist
// ([worklet.ValidateDecision]). A lookup that FAILS is the node's condition,
// and is answered as one.
func (d WorkDeps) readDecision(ctx context.Context, tool string, actor Actor,
	raw any, pageReader PageReader) (*decision.Decision, *tools.Result) {

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, refusalOf(failed(fmt.Sprintf("%s: `decision` is not a JSON "+
			"object (%v).", tool, err)))
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var asked decision.Decision
	if err = decoder.Decode(&asked); err != nil {
		return nil, refusalOf(failed(fmt.Sprintf("%s: `decision` does not have "+
			"the shape its schema describes (%v). It takes question, options "+
			"[{id, label, detail}], recommended, rationale, evidence [{kind, ref, "+
			"label}], role and inform {surface, channel}.", tool, err)))
	}
	checked, err := worklet.ValidateDecision(ctx, asked, actor.Kind, evidenceLookup{
		work: d, pages: pageReader,
	})
	switch {
	case errors.Is(err, worklet.ErrInvalid):
		return nil, refusalOf(refusedBy(tools.RefusalInvalid, err, fmt.Sprintf(
			"%s was refused: %s. Nothing was posted.", tool, worklet.Sentence(err))))
	case err != nil:
		return nil, refusalOf(readFailure(ctx, tool, err))
	}
	if checked.Inform != nil {
		inform, refusal := d.checkInform(tool, actor, *checked.Inform)
		if refusal != nil {
			return nil, refusal
		}
		checked.Inform = &inform
	}
	return &checked, nil
}

// checkAskOfPerson refuses an agent seat's question to a person that carries
// no `decision` (ADR-0100), or returns nil. `asked` is the handle as
// [WorkDeps.resolveHandle] returned it.
//
// THE ASKER IS AN AGENT SEAT, because the rule is what a person is shown when
// an agent asks them something: a person asking through the dashboard or an
// assistant asks in their own words, and the decision stays theirs to add.
// AND THE ASKED IS ANYBODY THE ROSTER DOES NOT NAME AS AN AGENT SEAT: a
// question between two agents may be answered in words, but a person who
// cannot be told apart from one is asked the way a person is.
func (d WorkDeps) checkAskOfPerson(tool string, actor Actor, asked string, decided bool) *tools.Result {
	if decided || asked == "" || actor.Kind != worklet.AuthorAgent || d.asksAgentSeat(asked) {
		return nil
	}
	return refusalOf(failed(fmt.Sprintf("%s: `ask` puts a question to %s, who is "+
		"not an agent seat, and every question an agent puts to a person carries a "+
		"`decision`: %d to %d `options` they answer with one press, the one you "+
		"`recommended`, your `rationale` and the `role` they answer in. A question "+
		"you would ask in words still has answers you expect: offer them, and the "+
		"person can always answer in their own words instead. Nothing was posted.",
		tool, clip(asked), decision.MinOptions, decision.MaxOptions)))
}

// checkAsksOfNamed refuses an agent seat's comment that asks (`asks`) and names a
// person it does not put the question to as an ask (ADR-0100): what an agent asks
// a person reaches their decisions only as an ask carrying a decision.
func (d WorkDeps) checkAsksOfNamed(tool string, actor Actor, asks bool, ask string, named []string) *tools.Result {
	persons := askedInWords(actor.Kind == worklet.AuthorAgent, asks, ask, named, d.Seats)
	if len(persons) == 0 {
		return nil
	}
	return refusalOf(failed(fmt.Sprintf("%s: `asks` is true and this names a person (%s) "+
		"outside `ask`. An agent's question to a person reaches their decisions only as an "+
		"ask: set `ask` to their handle and give it a `decision` with %d to %d `options` they "+
		"answer with one press, the one you `recommended`, your `rationale` and the `role` "+
		"they answer in. If you only mention them, set `asks` to false. Nothing was posted.",
		tool, words.And(persons), decision.MinOptions, decision.MaxOptions)))
}

// askedInWords is the persons' seats an agent's words that ask (`asks`) name
// other than ask, the one they put the question to (ADR-0100).
func askedInWords(agent, asks bool, ask string, named []string, roster func() []colleague.Seat) []string {
	if !asks || !agent || roster == nil {
		return nil
	}
	humans := map[string]bool{}
	for _, seat := range roster() {
		humans[seat.Handle] = seat.Kind == string(org.KindHuman)
	}
	var persons []string
	for _, handle := range named {
		if handle != ask && humans[handle] && !slices.Contains(persons, handle) {
			persons = append(persons, handle)
		}
	}
	return persons
}

// statedAsks is an agent's statement of whether its comment asks, which every
// agent comment carries (ADR-0113), and nil for a person's.
func (d WorkDeps) statedAsks(agent, asks bool) *bool {
	if !agent {
		return nil
	}
	return &asks
}

// agentsAmong is the handles an agent's comment names that are agent seats,
// and nil for a person's comment.
func (d WorkDeps) agentsAmong(agent bool, handles []string) []string {
	if !agent {
		return nil
	}
	var out []string
	for _, handle := range handles {
		if d.asksAgentSeat(handle) {
			out = append(out, handle)
		}
	}
	return out
}

// asksAgentSeat reports whether handle names an agent seat on the roster. No
// roster names nobody, so a question it cannot place is asked as of a person.
func (d WorkDeps) asksAgentSeat(handle string) bool {
	if d.Seats == nil {
		return false
	}
	for _, seat := range d.Seats() {
		if seat.Handle == handle {
			return seat.Kind == string(org.KindAgent)
		}
	}
	return false
}

// informSurfaces is the schema's enum: every surface this build knows, so the
// list a model is offered is the list the value layer accepts.
func informSurfaces() []any {
	out := make([]any, 0, len(decision.InformSurfaces()))
	for _, surface := range decision.InformSurfaces() {
		out = append(out, string(surface))
	}
	return out
}

// ChannelDirectory is the chart's answer to WHERE an asker may promise to
// report a decision — declared here, by the one caller that asks, and
// implemented by the engine over the epoch current when the tool runs.
type ChannelDirectory interface {
	// ChatSurfaces is the chat surfaces the seat `handle` can post on:
	// the ones this company runs AND that seat has an identity on — a bot
	// on a vendor surface, and on the engine's own chat
	// ([decision.InformChat]) the seat itself, which every agent seat
	// posts as. Both, because a promise to post where the seat has no
	// identity is one it can never keep, and the engine would hold its
	// turn open for a post no tool of its can make.
	ChatSurfaces(handle string) []decision.InformSurface

	// UnitChannels is every channel a unit of the chart declares, as the
	// chart spells it.
	UnitChannels() []string
}

// checkInform is where an asker's promise to report the outcome is checked
// against the world, and returns the inform as it is to be stored.
//
// # An agent seat's alone
//
// THE ENGINE ENFORCES AN INFORM: the asker's answered turn is held open until
// a tool on the named surface has run ([worklet.Owes]). A person (a bound
// operator at the dashboard, an assistant on their credential) has no turn
// to hold, so an inform they asked for would put "posts it to #channel" on the
// answering person's card as a promise nothing keeps. That is refused as
// FORBIDDEN rather than invalid: no spelling of the argument makes it right
// for this caller.
//
// # A surface the seat can post on, a channel the chart declares
//
// The channel is one a UNIT declares because that is the set the chart owns
// and every seat's prompt already names — an arbitrary name would be a
// channel nobody checked the seat's bot is in. A leading `#` is accepted and
// the chart's own spelling is stored, so the card and the prompt render the
// name the company uses.
func (d WorkDeps) checkInform(tool string, actor Actor, inform decision.Inform) (decision.Inform, *tools.Result) {
	if actor.Kind != worklet.AuthorAgent {
		return decision.Inform{}, refusalOf(refused(tools.RefusalForbidden,
			fmt.Sprintf("%s: `decision.inform` asks the engine to hold the "+
				"asker's turn until the outcome is posted in %s, and only an "+
				"agent seat has a turn to hold. Ask without `inform`, and post "+
				"the outcome yourself once it is decided. Nothing was posted.",
				tool, clip(inform.Channel))))
	}
	if d.Channels == nil {
		return decision.Inform{}, refusalOf(failed(fmt.Sprintf("%s: this "+
			"surface has no chart to check `decision.inform` against, so it "+
			"cannot promise a post. Ask without `inform`. Nothing was posted.",
			tool)))
	}
	surfaces := d.Channels.ChatSurfaces(actor.Handle)
	if !slices.Contains(surfaces, inform.Surface) {
		held := "none"
		if len(surfaces) > 0 {
			names := make([]string, len(surfaces))
			for i, surface := range surfaces {
				names[i] = string(surface)
			}
			held = strings.Join(names, ", ")
		}
		return decision.Inform{}, refusalOf(failed(fmt.Sprintf("%s: "+
			"`decision.inform.surface` is %q, which you cannot post on — the "+
			"chat surfaces you can post on here are: %s. Name one of them, "+
			"or ask without `inform`. Nothing was posted.", tool,
			string(inform.Surface), held)))
	}
	declared := d.Channels.UnitChannels()
	want := strings.TrimPrefix(strings.TrimSpace(inform.Channel), "#")
	for _, channel := range declared {
		if strings.TrimPrefix(channel, "#") == want {
			inform.Channel = channel
			return inform, nil
		}
	}
	listed := "none"
	if len(declared) > 0 {
		listed = strings.Join(declared, ", ")
	}
	return decision.Inform{}, refusalOf(failed(fmt.Sprintf("%s: "+
		"`decision.inform.channel` is %q, which no team in the org chart "+
		"declares — the channels it does are: %s. Name one of them, or ask "+
		"without `inform`. Nothing was posted.", tool, clip(inform.Channel),
		listed)))
}

// evidenceLookup is the world a decision's evidence is checked against: this
// surface's own Worklet reader and Doclet reader.
type evidenceLookup struct {
	work  WorkDeps
	pages PageReader
}

var _ worklet.EvidenceLookup = evidenceLookup{}

// Task resolves a key or an id through the same point read every other
// reference in these tools takes.
func (l evidenceLookup) Task(ctx context.Context, ref string) (worklet.CitedTask, error) {
	if l.work.Reader == nil {
		// A COMPANY WITHOUT WORKLET CANNOT CITE A WORK ITEM by
		// kind, for the reason [evidenceLookup.PageID] gives a page: an
		// executor of a company on another tracker asks with the same
		// evidence a work item's decision takes.
		return worklet.CitedTask{}, fmt.Errorf("%w: this company does not run "+
			"Worklet, so a `task` cannot be checked; cite it as a `url`",
			worklet.ErrInvalid)
	}
	got, err := l.work.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	if err != nil {
		return worklet.CitedTask{}, err
	}
	return worklet.CitedTask{
		ID:   got.Task.ID,
		Keys: append([]string{got.Task.Key}, got.Task.FormerKeys...),
	}, nil
}

// PageID resolves a page by id or by address to a live page's id.
//
// A COMPANY WITHOUT DOCLET CANNOT CITE A PAGE by kind: there is
// nothing to check it against, and a reference nothing checked is the dead
// link the check exists to prevent. That is the caller's to change — cite it
// as a url — so it is refused as invalid rather than reported as a failure.
// A trashed page is not live: it is where nobody deciding can read it.
func (l evidenceLookup) PageID(ctx context.Context, ref string) (string, bool, error) {
	if l.pages == nil {
		return "", false, fmt.Errorf("%w: this company does not run Doclet, "+
			"so a `page` cannot be checked; cite it as a `url`",
			worklet.ErrInvalid)
	}
	detail, err := l.pages.Get(ctx, strings.TrimSpace(ref), seatRead)
	switch {
	case errors.Is(err, doclet.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	case detail.Page.Status == doclet.StatusTrashed:
		return "", false, nil
	}
	return detail.Page.ID, true, nil
}
