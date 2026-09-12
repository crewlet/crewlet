package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The person tools: one human's inbox, queue and pins.
//
// # Why a SEAT has none of these
//
// A seat is not a human. It has a mailbox rather than an inbox — the durable
// subscription the engine attaches when it acquires the seat — and nothing on
// a person's record describes it. These are the OPERATOR's, which is the one
// surface where a person acts through their own credential.
//
// # The authority is the engine's, not this package's
//
// Every rule about who may write what lives in internal/tracker: the inbox and
// the pins only on behalf of the person whose they are, the priorities also by
// a LEAD for somebody in their line. What these tools add is the org lookup
// the tracker deliberately does not do — this package has a chart and that one
// does not.

// PersonWriter is the tracker write side these tools need.
type PersonWriter interface {
	WriteInbox(ctx context.Context, opID, handle string,
		read, unread, snoozed []tracker.InboxEntry, reasons []tracker.Reason,
		seenThrough tracker.Position) (tracker.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string,
		pinnedViews []string, favorites []tracker.Favorite) (tracker.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string,
		priorities []string, authority tracker.PersonAuthority) (tracker.WriteResult, error)
}

// PersonReader is the read side.
type PersonReader interface {
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
}

// Leads reports whether one handle leads another in the org chart.
//
// A SEAM RATHER THAN A CHART, because the answer is a fact about the company's
// configuration and this package holds none — and because a lead relation that
// this package derived would be a second opinion about the hierarchy.
//
// NIL RESOLVES NOTHING, which degrades to "your own only": a company whose
// surface did not wire this loses a lead's convenience rather than gaining a
// hole.
type Leads func(ctx context.Context, actor, handle string) bool

type getPerson struct{ deps WorkDeps }

var _ tools.Callable = (*getPerson)(nil)

func (t *getPerson) Name() string { return tracker.GetPersonTool }

func (t *getPerson) Description() string {
	return "One person's own state: what is in their inbox, what they have " +
		"read, what is snoozed and which of those snoozes are now due, the " +
		"order they mean to work in, and their pinned views."
}

func (t *getPerson) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{"type": "string", "description": "Whose state to read."},
		},
		"required": []string{"handle"},
	}
}

func (t *getPerson) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	if t.deps.Reader == nil {
		return unconfigured(tracker.GetPersonTool), nil
	}
	handle := strings.TrimSpace(argString(args, "handle"))
	if handle == "" {
		return failed("Name whose state to read with `handle`."), nil
	}
	state, err := t.deps.Reader.Person(ctx, tracker.PersonQuery{
		Handle: handle, Level: statelog.ReadSession,
	}, t.deps.now())
	if err != nil {
		return failed(readFailure(tracker.GetPersonTool, err)), nil
	}
	return jsonResult(state)
}

type setPriorities struct {
	deps  WorkDeps
	leads Leads
}

var _ tools.Callable = (*setPriorities)(nil)

func (t *setPriorities) Name() string { return tracker.SetPrioritiesTool }

func (t *setPriorities) Description() string {
	return "Set the order somebody means to work in — your own, or somebody " +
		"in your line. Setting another person's is recorded as yours on their " +
		"record, so they can see who chose it; their own next change clears " +
		"that. The list REPLACES the one it names."
}

func (t *setPriorities) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{
				"type":        "string",
				"description": "Whose queue. Omit for your own.",
			},
			"items": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Work item ids or keys, most important first.",
			},
		},
		"required": []string{"items"},
	}
}

func (t *setPriorities) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setPriorities) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.person(ctx, turn, tracker.SetPrioritiesTool)
	if refusal != nil {
		return *refusal, nil
	}
	handle := strings.TrimSpace(argString(args, "handle"))
	if handle == "" {
		handle = actor.Handle
	}
	// THE HANDLE IS RESOLVED AGAINST THE CHART, because a typo here writes
	// a whole PERSON RECORD for somebody who does not exist — a queue
	// nobody will ever read, and a wake routed to a handle Route drops in
	// silence.
	whose, unknown := t.deps.resolveHandle(tracker.SetPrioritiesTool,
		"`handle`", handle)
	if unknown != "" {
		return failed(unknown), nil
	}
	handle = whose
	authority := tracker.PersonAuthority{
		// A HUMAN OR AN OPERATOR MAY WRITE ANYBODY'S, which is the
		// design's own rule and the one the gate was missing. It matters
		// here specifically: this tool is registered on the operator MCP
		// alone, and an operator's actor is a TOKEN's name rather than a
		// handle in the chart — so no ancestor walk can ever match it,
		// `Lead` is false for every operator by construction, and the
		// only shipped surface for the verb could not use it.
		Person: actor.Kind == tracker.AuthorHuman ||
			actor.Kind == tracker.AuthorOperator,
	}
	// AND THE LEAD RELATION IS RESOLVED HERE and passed as a value,
	// because the tracker has no chart — see the file head. A surface that
	// wired no lookup resolves false, which degrades to "your own only".
	if t.leads != nil && handle != actor.Handle {
		authority.Lead = t.leads(ctx, actor.Handle, handle)
	}
	// EVERY ENTRY IS RESOLVED TO AN ID, because the list is stored as ids
	// and read back by joining on them — and this tool's own description
	// invites a key. An unresolved `ENG-42` failed in three places at
	// once and reported nothing anywhere: the entry vanished from
	// `my_work`, it vanished from `preset=priorities`, and the wake that
	// tells the person their queue changed was silently suppressed —
	// while the call answered `outcome: applied`.
	items := argStrings(args, "items")
	resolved := make([]string, 0, len(items))
	for _, ref := range items {
		id, refusal := t.deps.resolveRef(ctx, tracker.SetPrioritiesTool,
			"`items`", ref)
		if refusal != "" {
			return failed(refusal), nil
		}
		resolved = append(resolved, id)
	}
	result, err := writer.WritePriorities(ctx,
		"prio-"+handle+"-"+turnKeyOr(turn), handle, resolved, authority)
	if err != nil {
		return failed(writeFailure(tracker.SetPrioritiesTool, err)), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"handle": handle, "outcome": string(result.Outcome),
		"version": result.Version,
	})
}

type setPins struct{ deps WorkDeps }

var _ tools.Callable = (*setPins)(nil)

func (t *setPins) Name() string { return tracker.SetPinsTool }

func (t *setPins) Description() string {
	return "Set your own pinned views and starred things. A pin puts a view " +
		"first in its container's strip, for you and nobody else. Both lists " +
		"REPLACE what is there."
}

func (t *setPins) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"views": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Saved view ids to pin.",
			},
			"favorites": map[string]any{
				"type":        "array",
				"description": "Things to star.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "description": "project, task, view or goal."},
						"id":   map[string]any{"type": "string"},
					},
					"required": []string{"kind", "id"},
				},
			},
		},
	}
}

func (t *setPins) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setPins) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, tracker.SetPinsTool)
	if refusal != nil {
		return *refusal, nil
	}
	favorites, bad := personFavorites(args)
	if bad != "" {
		return failed(bad), nil
	}
	result, err := writer.WritePins(ctx,
		"pins-"+actor.Handle+"-"+turnKeyOr(turn), actor.Handle,
		argStrings(args, "views"), favorites)
	if err != nil {
		return failed(writeFailure(tracker.SetPinsTool, err)), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"outcome": string(result.Outcome), "version": result.Version,
	})
}

type markInbox struct{ deps WorkDeps }

var _ tools.Callable = (*markInbox)(nil)

func (t *markInbox) Name() string { return tracker.MarkInboxTool }

func (t *markInbox) Description() string {
	return "Move your own inbox on: what you have read, what is still unread, " +
		"what is snoozed and how far you have read. Entries at or below the " +
		"seen-through position are dropped, because nothing will render them " +
		"again. Read it with get_person first — every list REPLACES the one " +
		"it names."
}

func (t *markInbox) Parameters() map[string]any {
	entry := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"record_id": map[string]any{"type": "string"},
			"position":  map[string]any{"type": "number", "description": "The log position the entry was at."},
			"until":     map[string]any{"type": "string", "description": "RFC3339; on a snooze, when it comes back."},
		},
		"required": []string{"record_id", "position"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"unread":  map[string]any{"type": "array", "items": entry},
			"read":    map[string]any{"type": "array", "items": entry},
			"snoozed": map[string]any{"type": "array", "items": entry},
			"seen_through": map[string]any{
				"type":        "number",
				"description": "How far you have read, as a log position.",
			},
			"seen_through_stream": map[string]any{
				"type": "string",
				"description": "The stream that position is in. A position " +
					"from a recreated stream compares as current, which is " +
					"why this travels with it.",
			},
		},
	}
}

func (t *markInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markInbox) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, tracker.MarkInboxTool)
	if refusal != nil {
		return *refusal, nil
	}
	read, bad := inboxEntries(args, "read")
	if bad != "" {
		return failed(bad), nil
	}
	unread, bad := inboxEntries(args, "unread")
	if bad != "" {
		return failed(bad), nil
	}
	snoozed, bad := inboxEntries(args, "snoozed")
	if bad != "" {
		return failed(bad), nil
	}
	result, err := writer.WriteInbox(ctx,
		"inbox-"+actor.Handle+"-"+turnKeyOr(turn), actor.Handle,
		read, unread, snoozed, nil, tracker.Position{
			Stream: strings.TrimSpace(argString(args, "seen_through_stream")),
			Seq:    uint64(argFloat(args, "seen_through")),
		})
	if err != nil {
		return failed(writeFailure(tracker.MarkInboxTool, err)), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"outcome": string(result.Outcome), "version": result.Version,
	})
}

// person resolves the actor and the writer for a priority write.
func (t *setPriorities) person(ctx context.Context, turn *turnctx.Turn,
	name string) (Actor, PersonWriter, *tools.Result) {

	return t.deps.personWriter(ctx, turn, name)
}

// personWriter resolves the actor and the person write side, or the refusal.
func (d WorkDeps) personWriter(ctx context.Context, turn *turnctx.Turn,
	name string) (Actor, PersonWriter, *tools.Result) {

	actor, err := d.actor(ctx, turn)
	if err != nil {
		refusal := notInATurn(name)
		return Actor{}, nil, &refusal
	}
	if d.PersonWriter == nil {
		refusal := unconfigured(name)
		return Actor{}, nil, &refusal
	}
	return actor, d.PersonWriter(actor), nil
}

// turnKeyOr is the turn's own idempotency key, or a stable empty one.
//
// AN OPERATOR HAS NO TURN and no redelivery — their client made one call — so
// there is nothing to deduplicate against and an invented key would be a lie
// about what produced the write. What the empty string buys is that two calls
// in one session are two writes, which is what the caller meant.
func turnKeyOr(turn *turnctx.Turn) string {
	if key := turnKey(turn); key != "" {
		return key
	}
	return "operator"
}

// inboxEntries reads one of the three lists.
func inboxEntries(args map[string]any, key string) ([]tracker.InboxEntry, string) {
	raw, held := args[key].([]any)
	if !held {
		return nil, ""
	}
	out := make([]tracker.InboxEntry, 0, len(raw))
	for i, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Entry %d of `%s` is not an object.", i+1, key)
		}
		entry := tracker.InboxEntry{
			RecordID: strings.TrimSpace(argString(fields, "record_id")),
			Position: uint64(argFloat(fields, "position")),
		}
		if raw := strings.TrimSpace(argString(fields, "until")); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Sprintf("`until` on entry %d of `%s` is a date "+
					"and time in RFC3339, like 2026-06-30T09:00:00Z — %q is "+
					"not one.", i+1, key, clip(raw))
			}
			utc := at.UTC()
			entry.Until = &utc
		}
		out = append(out, entry)
	}
	return out, ""
}

// personFavorites reads the starred things.
func personFavorites(args map[string]any) ([]tracker.Favorite, string) {
	raw, held := args["favorites"].([]any)
	if !held {
		return nil, ""
	}
	out := make([]tracker.Favorite, 0, len(raw))
	for i, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Favourite %d is not an object.", i+1)
		}
		out = append(out, tracker.Favorite{
			Kind: strings.TrimSpace(argString(fields, "kind")),
			ID:   strings.TrimSpace(argString(fields, "id")),
		})
	}
	return out, ""
}
