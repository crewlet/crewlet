package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/tracker"
)

// The set-valued arguments, and why a bare list is refused.
//
// A collection on a work item is carried WHOLE by the record that touches it,
// so `waiting_on: ["ENG-7"]` read as the whole set replaces every other
// blocker the item had — silently, and with a wake announcing the removal of
// edges nobody meant to touch. That exact shape is what `watchers: [me]` did
// to the watcher set before the watch gesture existed.
//
// So these arguments take ONE of two explicit shapes and never a bare list:
//
//	{"set": ["ENG-7", "ENG-9"]}          — the whole set, replacing
//	{"add": ["ENG-7"], "remove": ["ENG-2"]} — a delta against what is there
//
// and a bare list is refused NAMING BOTH, because the two readings of it are
// opposite and a tool that guessed would be right half the time.

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
	object, ok := raw.(map[string]any)
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
		out.Values = refList(values)
		return out, true, ""
	}
	for key := range object {
		if key != "add" && key != "remove" {
			return setArg{}, false, fmt.Sprintf("`%s` has no %q. It takes "+
				"{\"set\": [...]}, or {\"add\": [...], \"remove\": [...]}.",
				name, key)
		}
	}
	out.Add, out.Remove = refList(object["add"]), refList(object["remove"])
	if out.Empty() {
		return setArg{}, false, fmt.Sprintf("`%s` adds and removes nothing. "+
			"Leave it out to change nothing, or pass {\"set\": []} to clear "+
			"the collection.", name)
	}
	return out, true, ""
}

// refList is one list of references, trimmed and deduped, order preserved.
func refList(raw any) []string {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

// resolveRefs turns every reference in a set argument into a task id.
//
// AT THE TOOL, for the reason [WorkDeps.resolveRef] gives: a relation stores an
// ID, so a key stored in one resolves to nothing on every node for ever.
func (d WorkDeps) resolveRefs(ctx context.Context, tool, field string,
	in setArg) (setArg, string) {

	out := setArg{Whole: in.Whole}
	for _, group := range []struct {
		from []string
		into *[]string
	}{
		{in.Values, &out.Values}, {in.Add, &out.Add}, {in.Remove, &out.Remove},
	} {
		for _, ref := range group.from {
			id, refusal := d.resolveRef(ctx, tool, "`"+field+"`", ref)
			if refusal != "" {
				return setArg{}, refusal
			}
			*group.into = append(*group.into, id)
		}
	}
	return out, ""
}

// dependencyChange composes the two dependency arguments into the sequence's
// own request, resolving each reference to an id.
//
// THE `set` SHAPE IS A DELTA HERE, computed against what the item already has,
// because the two ends of a dependency are two commits: the sequence has to
// know which edges ARRIVED and which LEFT to write the right mirror on the
// right counterparty. A whole-set gesture that reached the writer as a set
// would give it no way to tell one from the other.
func (d WorkDeps) dependencyChange(ctx context.Context, tool string,
	args map[string]any, task tracker.Task) (tracker.DependencyChange, string) {

	change := tracker.DependencyChange{Task: task.ID, Project: task.Project}
	change.Note = strings.TrimSpace(argString(args, "dependency_note"))
	for _, side := range []struct {
		name        string
		have        []string
		add, remove *[]string
	}{
		{"waiting_on", waitingOn(task), &change.WaitingOnAdd, &change.WaitingOnRemove},
		{"blocking", task.Dependents, &change.BlockingAdd, &change.BlockingRemove},
	} {
		stated, held, refusal := parseSetArg(args, side.name)
		if refusal != "" {
			return change, refusal
		}
		if !held {
			continue
		}
		resolved, refusal := d.resolveRefs(ctx, tool, side.name, stated)
		if refusal != "" {
			return change, refusal
		}
		if !resolved.Whole {
			*side.add, *side.remove = resolved.Add, resolved.Remove
			continue
		}
		*side.add, *side.remove = delta(side.have, resolved.Values)
	}
	return change, ""
}

// waitingOn is the ids a task's own dependency edges name.
func waitingOn(task tracker.Task) []string {
	out := make([]string, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == tracker.RelationWaitingOn {
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
// bytes and nothing else, so they ride the item's own patch rather than a
// sequence.
//
// A PAGE REFERENCE IS NOT RESOLVED AS A TASK. Its other end is a knowledge-base
// page id, and there is no write to the pages family at all — which is why it
// is a separate argument rather than a `kind` on one list.
func (d WorkDeps) inertRelations(ctx context.Context, tool string,
	args map[string]any, task tracker.Task, actor Actor) (*tracker.RelationIntent, string) {

	intent := &tracker.RelationIntent{}
	touched := false
	for _, side := range []struct {
		name    string
		kind    tracker.RelationKind
		resolve bool
	}{
		{"linked", tracker.RelationLinked, true},
		{"linked_pages", tracker.RelationPage, false},
	} {
		stated, held, refusal := parseSetArg(args, side.name)
		if refusal != "" {
			return nil, refusal
		}
		if !held {
			continue
		}
		if side.resolve {
			var refusal string
			if stated, refusal = d.resolveRefs(ctx, tool, side.name, stated); refusal != "" {
				return nil, refusal
			}
		}
		touched = true
		if stated.Whole {
			// A WHOLE SET OF ONE KIND IS A DELTA OVER THAT KIND ALONE,
			// never over the collection: the relation set holds every
			// kind together, and a `set` that reached the writer whole
			// would drop the item's duplicates, pages and dependency
			// edges along with the links it replaced.
			stated.Add, stated.Remove = delta(othersOfKind(task, side.kind), stated.Values)
		}
		for _, id := range stated.Add {
			intent.Add = append(intent.Add, tracker.Relation{
				Kind: side.kind, Other: id, CreatedBy: actor.Handle,
			})
		}
		for _, id := range stated.Remove {
			intent.Remove = append(intent.Remove, tracker.Relation{
				Kind: side.kind, Other: id,
			})
		}
	}
	if !touched || (len(intent.Add) == 0 && len(intent.Remove) == 0) {
		return nil, ""
	}
	return intent, ""
}

// othersOfKind is the ids this task's relations of one kind name.
func othersOfKind(task tracker.Task, kind tracker.RelationKind) []string {
	out := make([]string, 0, len(task.Relations))
	for _, relation := range task.Relations {
		if relation.Kind == kind {
			out = append(out, relation.Other)
		}
	}
	return out
}

// parentParty is the item's parent as the wake needs it, or nil.
//
// READ ONLY ON THE ONE TRANSITION THAT USES IT — a status move across the
// finished edge — so an ordinary edit costs no read at all. The gate is
// computed from the same two values the router's own gate reads, which is what
// stops this tool paying for a lookup whose answer would be dropped.
func (d WorkDeps) parentParty(ctx context.Context, task tracker.Task,
	patch tracker.TaskPatch) *tracker.TaskParty {

	if patch.Status == nil || task.Parent == nil || *task.Parent == "" || d.Reader == nil {
		return nil
	}
	if task.StatusGroup.Finished() == patch.Status.Group().Finished() {
		return nil
	}
	got, err := d.Reader.Task(ctx, *task.Parent, tracker.DetailWants{}, seatReadLevel)
	if err != nil {
		// BEST EFFORT, like every other routing lookup: a parent this
		// node cannot read is a parent's assignee who is not told, and
		// failing somebody's close because of it would be worse than
		// the omission. The reporter, the watchers and the assignee are
		// all still routed from the snapshot.
		return nil
	}
	return &tracker.TaskParty{
		Task: got.Task.ID, Key: got.Task.Key, Assignee: got.Task.Assignee,
	}
}

// resolveThread reads the conversation a comment is joining, or returns the
// model-facing refusal.
//
// THE PARTICIPANTS ARE BEST EFFORT AND THE ANSWER IS NOT, which is the reader's
// own split: a participant list that came up short wakes somebody as a watcher
// instead of under `thread`, while an `answers` resolved wrongly closes
// somebody else's question. So a read failure here empties the first and
// refuses the second — and the refusal is the reader's own text, which already
// names the candidates when the ask is ambiguous.
func (d WorkDeps) resolveThread(ctx context.Context,
	q tracker.ThreadQuery) (tracker.ResolvedThread, string) {

	if d.Reader == nil {
		return tracker.ResolvedThread{}, unconfiguredText(CommentOnWorkTool)
	}
	if q.ReplyTo == "" && q.Answers == "" && q.Ask == "" {
		// NOTHING TO RESOLVE. A top-level remark that asks nobody and
		// answers nothing still has to be checked for an INFERRED
		// answer, though — which is why this returns early only when
		// the caller named no thread at all and is not the kind of
		// comment an inference would apply to.
		return d.inferOnly(ctx, q)
	}
	resolved, err := d.Reader.Thread(ctx, q, seatReadLevel)
	if err != nil {
		var ambiguous *tracker.ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return tracker.ResolvedThread{}, ambiguousText(ambiguous)
		}
		return tracker.ResolvedThread{}, readFailure(CommentOnWorkTool, err)
	}
	return resolved, ""
}

// inferOnly is the top-level case: no thread to read, but an open ask
// addressed to this author still closes.
func (d WorkDeps) inferOnly(ctx context.Context,
	q tracker.ThreadQuery) (tracker.ResolvedThread, string) {

	resolved, err := d.Reader.Thread(ctx, q, seatReadLevel)
	if err != nil {
		var ambiguous *tracker.ErrAmbiguousAnswer
		if errors.As(err, &ambiguous) {
			return tracker.ResolvedThread{}, ambiguousText(ambiguous)
		}
		// BEST EFFORT ON THIS PATH ALONE. The caller asked for no
		// answer and named no thread, so a read failure costs an
		// inference nobody requested — failing their comment for it
		// would refuse a write for a reason unrelated to what they
		// asked.
		return tracker.ResolvedThread{}, ""
	}
	return resolved, ""
}

// ambiguousText is the refusal that lists the open questions, because "which
// one" is the whole of what the caller has to decide.
func ambiguousText(e *tracker.ErrAmbiguousAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d open questions on this item are addressed to you, so "+
		"which one this answers cannot be inferred. Pass `answers` with one "+
		"of these comment ids:", len(e.Asks))
	for i, ask := range e.Asks {
		if i == tracker.MaxOpenAsksNamed {
			fmt.Fprintf(&b, "\n  … and more — read the item with "+
				"get_work_item to see the rest.")
			break
		}
		fmt.Fprintf(&b, "\n  %s — %s: %s", ask.Comment, ask.Author,
			clip(ask.Excerpt))
	}
	return b.String()
}
