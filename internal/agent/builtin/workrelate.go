package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The set-valued arguments, and why a bare list is refused.
//
// A record carries a work item's collection WHOLE, so `waiting_on: ["ENG-7"]`
// read as the whole set silently replaces every other blocker, with a wake
// announcing removals nobody meant. So these take one of two shapes:
//
//	{"set": ["ENG-7", "ENG-9"]}          — the whole set, replacing
//	{"add": ["ENG-7"], "remove": ["ENG-2"]} — a delta against what is there
//
// and a bare list is refused naming both: its two readings are opposite, and
// a guess is right half the time.

// setArg is one set-valued argument as a model stated it.
type setArg struct {
	// Whole is true for `{set: …}`, and Values is that set. Add and
	// Remove are the delta shape.
	Whole  bool
	Values []string
	Add    []string
	Remove []string
}

// Empty reports an argument that asks for nothing.
func (s setArg) Empty() bool {
	return !s.Whole && len(s.Add) == 0 && len(s.Remove) == 0
}

// parseSetArg reads one set-valued argument, or returns the model-facing
// refusal.
//
// The third return distinguishes "absent" from "present and empty": `{set: []}`
// is a caller clearing a collection, which is a write, while an absent
// argument is a collection this call does not touch.
func parseSetArg(args map[string]any, name string) (setArg, bool, string) {
	raw, held := args[name]
	if !held || raw == nil {
		return setArg{}, false, ""
	}
	// Null is absent ([argval.Object]), as the check reads it: `{"set":
	// null}` states no set, never an empty one clearing every relation.
	object, ok := argval.Object(raw)
	if !ok {
		return setArg{}, false, fmt.Sprintf("`%s` takes an object saying which "+
			"shape you mean: {\"set\": [...]} replaces the whole set, and "+
			"{\"add\": [...], \"remove\": [...]} changes only what you name. "+
			"A bare list is refused because the two readings are opposite — "+
			"one of them silently drops every entry you did not repeat.", name)
	}
	out := setArg{}
	if values, held := object["set"]; held {
		if len(object) > 1 {
			return setArg{}, false, fmt.Sprintf("`%s` states both `set` and a "+
				"delta. Say one or the other: `set` replaces the collection, "+
				"`add`/`remove` change part of it.", name)
		}
		out.Whole = true
		list, ok := refList(values)
		if !ok {
			return setArg{}, false, fmt.Sprintf("`%s.set` is a list of references.", name)
		}
		out.Values = list
		return out, true, ""
	}
	for key := range object {
		if key != "add" && key != "remove" {
			return setArg{}, false, fmt.Sprintf("`%s` has no %q. It takes "+
				"{\"set\": [...]}, or {\"add\": [...], \"remove\": [...]}.",
				name, key)
		}
	}
	var addOK, removeOK bool
	out.Add, addOK = refList(object["add"])
	out.Remove, removeOK = refList(object["remove"])
	if !addOK || !removeOK {
		return setArg{}, false, fmt.Sprintf("`%s.add` and `%s.remove` are lists of references.",
			name, name)
	}
	if out.Empty() {
		return setArg{}, false, fmt.Sprintf("`%s` adds and removes nothing. "+
			"Leave it out to change nothing, or pass {\"set\": []} to clear "+
			"the collection.", name)
	}
	return out, true, ""
}

// refList is one list of references, trimmed and deduped, order preserved,
// read as every list of strings is ([argval.Strings]): a single reference is a
// list of one, and a number the reference it spells. ok is false for anything
// else, which the caller refuses by name: read as no list, a `set` would
// clear every relation and be answered applied.
func refList(raw any) ([]string, bool) {
	if raw == nil {
		return nil, true
	}
	return argval.Strings(raw)
}

// resolveRefs turns every reference in a set argument into a task id.
//
// AT THE TOOL, for the reason [WorkDeps.resolveRef] gives: a relation stores an
// ID, so a key stored in one resolves to nothing on every node for ever.
func (d WorkDeps) resolveRefs(ctx context.Context, tool, field string,
	in setArg) (setArg, *tools.Result) {

	out := setArg{Whole: in.Whole}
	for _, group := range []struct {
		from []string
		into *[]string
	}{
		{in.Values, &out.Values}, {in.Add, &out.Add}, {in.Remove, &out.Remove},
	} {
		for _, ref := range group.from {
			id, refusal := d.resolveBlocker(ctx, tool, field, ref)
			if refusal != nil {
				return setArg{}, refusal
			}
			*group.into = append(*group.into, id)
		}
	}
	return out, nil
}

// dependencyChange composes the two dependency arguments into the sequence's
// own request, resolving each reference to an id.
//
// A `set` becomes a delta against the item's edges here: a dependency's two
// ends are two commits, and the sequence must know which edges arrived and
// which left to write each mirror on the right counterparty.
func (d WorkDeps) dependencyChange(ctx context.Context, tool string,
	args map[string]any, task worklet.Task) (worklet.DependencyChange, *tools.Result) {

	change := worklet.DependencyChange{Task: task.ID, Project: task.Project}
	change.Note = strings.TrimSpace(argString(args, "dependency_note"))
	for _, side := range []struct {
		name        string
		have        []string
		add, remove *[]string
	}{
		{"waiting_on", waitingOn(task), &change.WaitingOnAdd, &change.WaitingOnRemove},
		{"blocking", task.Dependents, &change.BlockingAdd, &change.BlockingRemove},
	} {
		stated, held, bad := parseSetArg(args, side.name)
		if bad != "" {
			return change, refusalOf(failed(bad))
		}
		if !held {
			continue
		}
		resolved, refusal := d.resolveRefs(ctx, tool, side.name, stated)
		if refusal != nil {
			return change, refusal
		}
		if !resolved.Whole {
			*side.add, *side.remove = resolved.Add, resolved.Remove
			continue
		}
		*side.add, *side.remove = delta(side.have, resolved.Values)
	}
	return change, nil
}

// waitingOn is the ids a task's own dependency edges name.
func waitingOn(task worklet.Task) []string {
	out := make([]string, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == worklet.RelationWaitingOn {
			out = append(out, relation.Other)
		}
	}
	return out
}

// delta is what a whole-set gesture adds and removes against what is there.
func delta(have, want []string) (add, remove []string) {
	held := make(map[string]bool, len(have))
	for _, id := range have {
		held[id] = true
	}
	wanted := make(map[string]bool, len(want))
	for _, id := range want {
		wanted[id] = true
		if !held[id] {
			add = append(add, id)
		}
	}
	for _, id := range have {
		if !wanted[id] {
			remove = append(remove, id)
		}
	}
	return add, remove
}

// inertRelations is the `linked` and `linked_pages` half: edges that cost
// bytes and nothing else, so they ride the item's own patch, not a sequence.
//
// A page reference is not resolved as a task: its other end is a knowledge
// base page id, and the edge rides the task's own record with no write to the
// pages log, hence a separate argument rather than a `kind` on one list.
func (d WorkDeps) inertRelations(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any, task worklet.Task) (*worklet.RelationIntent, *tools.Result) {

	intent := &worklet.RelationIntent{}
	touched, refusal := d.entryRelations(ctx, turn, tool, args, task, intent)
	if refusal != nil {
		return nil, refusal
	}
	for _, side := range []struct {
		name    string
		kind    worklet.RelationKind
		resolve bool
	}{
		{"linked", worklet.RelationLinked, true},
		{"linked_pages", worklet.RelationPage, false},
	} {
		stated, held, bad := parseSetArg(args, side.name)
		if bad != "" {
			return nil, refusalOf(failed(bad))
		}
		if !held {
			continue
		}
		if side.resolve {
			var refusal *tools.Result
			if stated, refusal = d.resolveRefs(ctx, tool, side.name, stated); refusal != nil {
				return nil, refusal
			}
		}
		touched = true
		if stated.Whole {
			// A whole set of one kind is a delta over that kind alone: the
			// relation set holds every kind, and a whole `set` would drop
			// duplicates, pages and dependency edges with the links.
			stated.Add, stated.Remove = delta(othersOfKind(task, side.kind), stated.Values)
		}
		for _, id := range stated.Add {
			// Unsigned: the writer signs with the actor it acts as, and a
			// second rule stating the handle is how the two come to differ.
			intent.Add = append(intent.Add, worklet.Relation{
				Kind: side.kind, Other: id,
			})
		}
		for _, id := range stated.Remove {
			intent.Remove = append(intent.Remove, worklet.Relation{
				Kind: side.kind, Other: id,
			})
		}
	}
	// The duplicate link: one edge on one item with no second end, as a
	// link is. In the same intent, because a patch carries ONE relation
	// gesture and separate ones drop each other's edges. A scalar, not the
	// `set` grammar: an item duplicates ONE other. Null or "" clears the
	// item's own duplicate edges, as for `assignee`, so a mistaken link can
	// be taken off.
	if raw, held := args["duplicate_of"]; held {
		// Read as every string argument is ([argval.String]): null and "" clear;
		// an object or a list is neither, left to the schema check that names it.
		text, readable := argval.String(raw)
		switch ref := strings.TrimSpace(text); {
		case ref != "":
			id, refusal := d.resolveRef(ctx, tool, "`duplicate_of`", ref)
			if refusal != nil {
				return nil, refusal
			}
			touched = true
			intent.Add = append(intent.Add, worklet.Relation{
				Kind: worklet.RelationDuplicates, Other: id,
			})
		case readable:
			touched = true
			for _, id := range othersOfKind(task, worklet.RelationDuplicates) {
				intent.Remove = append(intent.Remove, worklet.Relation{
					Kind: worklet.RelationDuplicates, Other: id,
				})
			}
		}
	}
	if !touched || (len(intent.Add) == 0 && len(intent.Remove) == 0) {
		return nil, nil
	}
	return intent, nil
}

// readsAtlas reports whether the call's caller may read the company's atlas:
// a holder of its app, and a seat only within whom its turn works for
// (ADR-0109). An answer that could not be decided reads as no.
func (d WorkDeps) readsAtlas(ctx context.Context) bool {
	return d.decide(ctx, authz.Action(AtlasGetTool), authz.Object{Kind: authz.KindAtlasEntry}) == nil
}

// entryRelations is the `linked_entries` half: the atlas entries the item
// touches, each stored by its id (ADR-0140). An entry is added only through
// the caller's own read of the atlas, so nobody links what they may not read
// and no id that is not an entry enters the record. One is removed by its id
// with no read, so a link to an entry the atlas no longer holds can be taken
// off.
func (d WorkDeps) entryRelations(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any, task worklet.Task, intent *worklet.RelationIntent) (bool, *tools.Result) {

	const name = "linked_entries"
	stated, held, bad := parseSetArg(args, name)
	if bad != "" {
		return false, refusalOf(failed(bad))
	}
	if !held {
		return false, nil
	}
	var read *atlasRead
	resolve := func(ref string, mustRead bool) (string, *tools.Result) {
		if id, err := uuid.Parse(ref); err == nil && !mustRead {
			return id.String(), nil
		}
		if read == nil {
			if refusal := askAuthority(ctx, d.Authorize, authz.Action(AtlasGetTool),
				authz.Object{Kind: authz.KindAtlasEntry}); refusal != nil {
				return "", refusal
			}
			got, refusal := d.entries.read(ctx, turn, tool)
			if refusal != nil {
				return "", refusal
			}
			read = &got
		}
		by := "name"
		if _, err := uuid.Parse(ref); err == nil {
			by = "id"
		}
		entry, refusal := read.entryArg(tool, map[string]any{by: ref})
		if refusal != nil {
			return "", refusal
		}
		return entry.ID.String(), nil
	}
	var add, remove []string
	for _, group := range []struct {
		from     []string
		into     *[]string
		mustRead bool
	}{
		{stated.Values, &add, true}, {stated.Add, &add, true}, {stated.Remove, &remove, false},
	} {
		for _, ref := range group.from {
			id, refusal := resolve(ref, group.mustRead)
			if refusal != nil {
				return false, refusal
			}
			*group.into = append(*group.into, id)
		}
	}
	if stated.Whole {
		// A WHOLE SET IS A DELTA OVER THIS KIND ALONE, as for the links above.
		add, remove = delta(task.Entries(), add)
	}
	for _, id := range add {
		intent.Add = append(intent.Add, worklet.Relation{Kind: worklet.RelationEntry, Other: id})
	}
	for _, id := range remove {
		intent.Remove = append(intent.Remove, worklet.Relation{Kind: worklet.RelationEntry, Other: id})
	}
	return true, nil
}

// othersOfKind is the ids this task's relations of one kind name.
func othersOfKind(task worklet.Task, kind worklet.RelationKind) []string {
	out := make([]string, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == kind {
			out = append(out, relation.Other)
		}
	}
	return out
}

// resolveThread reads the conversation a comment is joining, or returns the
// refusal, carrying a failed read's error for the reason [WorkDeps.resolveRef]
// gives.
//
// Participants are best effort and the answer is not (the reader's own split):
// a short participant list wakes somebody as a watcher rather than under
// `thread`, while a wrong `answers` closes somebody else's question. So a read
// failure empties the first and refuses the second, in the reader's own text,
// which names the candidates when the ask is ambiguous.
func (d WorkDeps) resolveThread(ctx context.Context,
	q worklet.ThreadQuery) (worklet.ResolvedThread, *tools.Result) {

	if d.Reader == nil {
		return worklet.ResolvedThread{}, refusalOf(unconfigured(CommentOnWorkTool))
	}
	if q.ReplyTo == "" && q.Answers == "" && q.Ask == "" {
		// No thread named, but a top-level remark may still close an
		// INFERRED answer, which inferOnly checks.
		return d.inferOnly(ctx, q)
	}
	return d.readThread(ctx, q)
}

// readThread is [WorkDeps.resolveThread] without its best-effort arm: every
// read failure is the refusal, inference included. The answer-only arm reads
// this way, because there an inference nobody could make is not a remark that
// answers nothing — it is the whole of what would have admitted the caller.
func (d WorkDeps) readThread(ctx context.Context,
	q worklet.ThreadQuery) (worklet.ResolvedThread, *tools.Result) {

	resolved, err := d.Reader.Thread(ctx, q, seatRead)
	if err != nil {
		var ambiguous *worklet.ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return worklet.ResolvedThread{}, refusalOf(failed(ambiguousText(ambiguous)))
		}
		if refusal, ok := answerRefusal(err); ok {
			return worklet.ResolvedThread{}, refusalOf(refusal)
		}
		return worklet.ResolvedThread{}, refusalOf(readFailure(ctx, CommentOnWorkTool, err))
	}
	return resolved, nil
}

// answerRefusal is the reader's refusal of an `answers`, in its own words, or
// false when the read itself failed.
//
// Not a read failure, whose "try again" invites a retry that refuses
// identically and drops the reader's sentence saying which comment was wrong.
// The reader's error rides beneath the class ([refusedBy]) for a caller
// branching on the tracker's sentinel. Each class asks something different: a
// mistyped id is not_found, somebody else's question forbidden, an answered
// one already_answered, and a remark or a removed comment an argument to
// change.
func answerRefusal(err error) (tools.Result, bool) {
	var class tools.Refusal
	switch {
	case errors.Is(err, worklet.ErrNoComment):
		class = tools.RefusalNotFound
	case errors.Is(err, worklet.ErrForbidden):
		class = tools.RefusalForbidden
	case errors.Is(err, worklet.ErrAlreadyAnswered):
		class = tools.RefusalAlreadyAnswered
	case errors.Is(err, worklet.ErrInvalid):
		class = tools.RefusalInvalid
	default:
		return tools.Result{}, false
	}
	if class == tools.RefusalAlreadyAnswered {
		// Not "change `answers`": the call is right and the question has its
		// answer; the reader's sentence names who gave it and when.
		return refusedBy(class, err, fmt.Sprintf("%s was refused: %s. Nothing "+
			"was posted.", CommentOnWorkTool, worklet.Sentence(err))), true
	}
	// The argument the sentence names, not always `answers`: a choice
	// outside the ask's options is refused through this same read.
	return refusedBy(class, err, fmt.Sprintf("%s was refused: %s. Nothing was "+
		"posted — change the argument this names (`answers`, or `choice`) "+
		"and comment again.", CommentOnWorkTool, worklet.Sentence(err))), true
}

// inferOnly is the top-level case: no thread to read, but an open ask
// addressed to this author still closes.
func (d WorkDeps) inferOnly(ctx context.Context,
	q worklet.ThreadQuery) (worklet.ResolvedThread, *tools.Result) {

	resolved, err := d.Reader.Thread(ctx, q, seatRead)
	if err != nil {
		var ambiguous *worklet.ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return worklet.ResolvedThread{}, refusalOf(failed(ambiguousText(ambiguous)))
		}
		// Best effort on this path alone: the caller asked for no answer,
		// so a read failure costs only an unrequested inference, never
		// their comment.
		return worklet.ResolvedThread{}, nil
	}
	return resolved, nil
}

// ambiguousQuestionBytes is how much prose question text the refusal carries
// whole before it names the rest by id.
//
// 8 KiB: a structured ask's question ([decision.MaxQuestion]) is always
// carried, prose ones whole while they fit. The refusal is a tool result, and
// five questions at their 32 KiB cap would spend most of it asking which one.
const ambiguousQuestionBytes = 8 << 10

// ambiguousText is the refusal that lists the open questions, because "which
// one" is the whole of what the caller has to decide.
//
// Each question whole or by reference, never cut: two cut questions that open
// alike read as one asked twice.
func ambiguousText(e *worklet.ErrAmbiguousAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d open questions on this item are addressed to you, so "+
		"which one this answers cannot be inferred. Pass `answers` with one "+
		"of these comment ids:", len(e.Asks))
	room := ambiguousQuestionBytes
	for i, ask := range e.Asks {
		if i == worklet.MaxOpenAsksNamed {
			fmt.Fprintf(&b, "\n  … and more — read the item with "+
				GetWorkItemTool+" to see the rest.")
			break
		}
		question := strings.TrimSpace(ask.Question)
		if question == "" {
			question = strings.TrimSpace(ask.Body)
			if len(question) > room && i > 0 {
				question = fmt.Sprintf("(a question of %d bytes — read it with "+
					GetWorkItemTool+"(item=%q, comment=%q))", len(question), e.Task, ask.Comment)
			} else {
				room -= len(question)
			}
		}
		fmt.Fprintf(&b, "\n  %s — %s: %s", ask.Comment, ask.Author, clip(question))
	}
	return b.String()
}

// fieldMap turns a model's `fields` object into the raw values a record
// carries, or returns the model-facing refusal.
//
// Keys (slug, name or id) and values are left as they came. The writer
// resolves keys inside its own decide, in the snapshot of the declarations the
// write lands beside; resolved here, a slug could file under whichever field
// held that spelling a moment ago. Coercion is the writer's too: it is where a
// value is refused, and pre-coercing would be a second copy of those rules.
func fieldMap(raw any) (map[string]json.RawMessage, string) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, "`fields` takes an object keyed by field slug, like " +
			"{\"severity\": \"high\", \"effort\": 3}. Read the slugs with " +
			worklet.GetWorkCatalogueTool + "."
	}
	out := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, "`fields` carries an entry with no field name."
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Sprintf("`fields` carries a value for %q that "+
				"cannot be sent: %v", clip(key), err)
		}
		out[key] = encoded
	}
	return out, ""
}

// resolveBlocker is resolveRef, and for `waiting_on` also a pull request on
// the company's GitHub by its URL, kept under its tracker name.
func (d WorkDeps) resolveBlocker(ctx context.Context, tool, field, ref string) (string, *tools.Result) {
	if field == "waiting_on" && d.ChangeRef != nil {
		switch change, refused := d.ChangeRef(ref); {
		case refused != "":
			return "", refusalOf(failed(tool + ": `waiting_on` names " + refused + " Nothing was written."))
		case change != "":
			return change, nil
		}
	}
	return d.resolveRef(ctx, tool, "`"+field+"`", ref)
}
