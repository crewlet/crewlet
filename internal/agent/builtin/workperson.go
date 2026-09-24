package builtin

import (
	"context"
	"fmt"
	"slices"
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
//
// # WHOSE record, which is not who wrote it
//
// These tools ACT AS A PERSON, and the person is not the credential in their
// hand. A write through the operator MCP is attributed to the TOKEN with
// author kind `operator`, deliberately and permanently, because a tracker
// whose author field is chosen by the writer is not an audit trail. The
// SUBJECT of the record is the other question, and its answer is
// [Actor.Record]: the seat the token is bound to, or the token itself where
// nothing is bound.
//
// They passed `actor.Handle` for both. So a founder whose assistant marked
// their inbox read wrote a second person record named after their credential,
// and their own screen — which asks under their seat — showed an inbox where
// nothing had ever been read.

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
	// BOTH OF THAT PERSON'S NAMES, resolved from the chart — never the
	// credential the caller is holding. An operator reading a report's own
	// state is handed THAT person's alias, and the ordinary case, where
	// they are reading their own, falls out of the same lookup rather than
	// being a second rule. See [WorkDeps.Party].
	state, err := t.deps.Reader.Person(ctx, tracker.PersonQuery{
		Who: t.deps.partyOf(handle), Level: seatReadLevel,
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
	return t.deps.operationParam(map[string]any{
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
	})
}

func (t *setPriorities) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setPriorities) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.person(ctx, turn, tracker.SetPrioritiesTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	handle := strings.TrimSpace(argString(args, "handle"))
	if handle != "" {
		// A HANDLE SOMEBODY TYPED IS RESOLVED AGAINST THE CHART, because
		// a typo here writes a whole PERSON RECORD for somebody who does
		// not exist — a queue nobody will ever read, and a wake routed to
		// a handle Route drops in silence.
		//
		// ONLY a handle somebody typed. The caller's own identity is not
		// a typo, and an unbound operator's is not in the chart at all —
		// so resolving it refused their own list by name, telling a
		// person holding the credential that there was nobody here called
		// that. Their record is the one `get_person` and `work_inbox`
		// already answer for them.
		whose, unknown := t.deps.resolveHandle(tracker.SetPrioritiesTool,
			"`handle`", handle)
		if unknown != "" {
			return failed(unknown), nil
		}
		handle = whose
	} else {
		// THE PERSON, NOT THE CREDENTIAL — see the file head.
		handle = actor.Record()
	}
	authority := tracker.PersonAuthority{
		// A HUMAN OR AN OPERATOR MAY WRITE ANYBODY'S, which is the
		// design's own rule and the one the gate was missing. It matters
		// here specifically: this tool is registered on the operator MCP
		// alone, and an operator's actor is a TOKEN's name rather than a
		// handle in the chart — so no ancestor walk can ever match it,
		// `Lead` is false for every operator by construction, and the
		// only shipped surface for the verb could not use it.
		Person: actor.Kind.Person(),
	}
	// AND THE LEAD RELATION IS RESOLVED HERE and passed as a value,
	// because the tracker has no chart — see the file head. A surface that
	// wired no lookup resolves false, which degrades to "your own only".
	//
	// ASKED ABOUT [Actor.Record]: a lead relation is between two PEOPLE in
	// the chart, and a credential is in no chart at all. A bound founder
	// asked about under their token matched nobody and fell through to
	// `Person` for an authority their seat actually holds.
	if t.leads != nil && handle != actor.Record() {
		authority.Lead = t.leads(ctx, actor.Record(), handle)
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
	opID := opIDFor(actor, t.Name(), "prio", handle, args)
	result, err := writer.WritePriorities(ctx, opID, handle, resolved, authority)
	if err != nil {
		return failed(writeFailure(actor, tracker.SetPrioritiesTool, err)), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return failed(unknownWrite(actor, tracker.SetPrioritiesTool,
			fmt.Sprintf("%s's priorities were set", handle), opID,
			result.Unvouched, unknownNext(result.Unvouched,
				sameCall(actor, tracker.SetPrioritiesTool),
				"Read the list with get_person",
				"it sets the same list again, which changes nothing"))), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"handle": handle, "outcome": string(result.Outcome), "position": positionOf(result.Position),
		"version": result.Version,
	}, actor))
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
	return t.deps.operationParam(map[string]any{
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
						"kind": map[string]any{"type": "string", "description": "project, task or view."},
						"id":   map[string]any{"type": "string"},
					},
					"required": []string{"kind", "id"},
				},
			},
		},
	})
}

func (t *setPins) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setPins) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, tracker.SetPinsTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	favorites, bad := personFavorites(args)
	if bad != "" {
		return failed(bad), nil
	}
	// THE PERSON'S OWN RECORD, which is [Actor.Record] and not the author
	// — see the file head. A pin is an arrangement somebody made, so it
	// belongs to them and not to whichever credential they were holding.
	whose := actor.Record()
	opID := opIDFor(actor, t.Name(), "pins", whose, args)
	result, err := writer.WritePins(ctx, opID, whose,
		argStrings(args, "views"), favorites)
	if err != nil {
		return failed(writeFailure(actor, tracker.SetPinsTool, err)), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return failed(unknownWrite(actor, tracker.SetPinsTool,
			"your pins were set", opID, result.Unvouched,
			unknownNext(result.Unvouched, sameCall(actor, tracker.SetPinsTool),
				"Read them with get_person",
				"it sets the same lists again, which changes nothing"))), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
	}, actor))
}

type markInbox struct{ deps WorkDeps }

var _ tools.Callable = (*markInbox)(nil)

func (t *markInbox) Name() string { return tracker.MarkInboxTool }

func (t *markInbox) Description() string {
	return "Move your own inbox on: what you have read, what is still unread, " +
		"what is snoozed, how far you have read, and which wake reasons are " +
		"yours to act on. Entries at or below the seen-through position are " +
		"dropped, because nothing will render them again. Read it with " +
		"get_person first — every list REPLACES the one it names, so an " +
		"omitted one is CLEARED rather than left alone."
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
	return t.deps.operationParam(map[string]any{
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
			"primary_reasons": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Which wake reasons land in the PRIMARY half " +
					"of `work_inbox`, the rest being context. One of: " +
					reasonList() + ". An empty list takes the shipped " +
					"default (" + defaultPrimaryList() + ") rather than " +
					"making nothing primary.",
			},
		},
	})
}

// defaultPrimaryList is the shipped split as one sentence, derived for the
// reason [reasonList] is derived.
func defaultPrimaryList() string {
	names := make([]string, 0, len(tracker.DefaultPrimaryReasons))
	for _, reason := range tracker.DefaultPrimaryReasons {
		names = append(names, string(reason))
	}
	return strings.Join(names, ", ")
}

func (t *markInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markInbox) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, tracker.MarkInboxTool, args)
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
	var reasons []tracker.Reason
	for _, raw := range argStrings(args, "primary_reasons") {
		reason := tracker.Reason(strings.TrimSpace(raw))
		if !slices.Contains(tracker.Reasons, reason) {
			return failed(fmt.Sprintf("%q is not a wake reason. The reasons "+
				"are: %s.", raw, reasonList())), nil
		}
		reasons = append(reasons, reason)
	}
	// THE PERSON'S OWN RECORD — see [setPins.CallForTurn] and the file
	// head. An inbox is the one object in this tracker that must never be
	// written on somebody else's behalf, and a founder's inbox is the
	// founder's whichever credential their assistant holds.
	whose := actor.Record()
	opID := opIDFor(actor, t.Name(), "inbox", whose, args)
	result, err := writer.WriteInbox(ctx, opID, whose,
		read, unread, snoozed, reasons, tracker.Position{
			Stream: strings.TrimSpace(argString(args, "seen_through_stream")),
			Seq:    uint64(argFloat(args, "seen_through")),
		})
	if err != nil {
		return failed(writeFailure(actor, tracker.MarkInboxTool, err)), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return failed(unknownWrite(actor, tracker.MarkInboxTool,
			"the inbox was marked", opID, result.Unvouched,
			unknownNext(result.Unvouched, sameCall(actor, tracker.MarkInboxTool),
				"Read it with work_inbox",
				"it marks the same notices again, which changes nothing"))), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
	}, actor))
}

// person resolves the actor and the writer for a priority write.
func (t *setPriorities) person(ctx context.Context, turn *turnctx.Turn,
	name string, args map[string]any) (Actor, PersonWriter, *tools.Result) {

	return t.deps.personWriter(ctx, turn, name, args)
}

// personWriter resolves the actor — with the operation the call is, where the
// surface has one ([WorkDeps.bindOperation]) — and the person write side, or
// the refusal.
func (d WorkDeps) personWriter(ctx context.Context, turn *turnctx.Turn,
	name string, args map[string]any) (Actor, PersonWriter, *tools.Result) {

	actor, err := d.actor(ctx, turn)
	if err != nil {
		refusal := notInATurn(name)
		return Actor{}, nil, &refusal
	}
	if d.PersonWriter == nil {
		refusal := unconfigured(name)
		return Actor{}, nil, &refusal
	}
	actor, bad := d.bindOperation(actor, name, args)
	if bad != "" {
		refusal := failed(bad)
		return Actor{}, nil, &refusal
	}
	return actor, d.PersonWriter(actor), nil
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

// InboxReader is the read side the inbox tool needs.
//
// DECLARED HERE, by the consumer, like every other seam in this tree — and
// separately from [PersonReader] because they are different questions: one is
// the person's own marks, the other is what the company asked of them.
type InboxReader interface {
	Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (
		tracker.InboxAnswer, error)
}

type workInbox struct{ deps WorkDeps }

var _ tools.Callable = (*workInbox)(nil)

func (t *workInbox) Name() string { return tracker.WorkInboxTool }

func (t *workInbox) Description() string {
	return "What the company has asked of somebody, newest first: every " +
		"change routed to them, the one reason each reached them under, and " +
		"whether they have read it. The PRIMARY half is what they have said " +
		"is theirs to act on — mentions, questions, their own work — and the " +
		"rest is context. This is the durable record the engine wrote when " +
		"the change landed, so it is there whether or not anybody was online."
}

func (t *workInbox) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{
				"type":        "string",
				"description": "Whose inbox to read.",
			},
			"reasons": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Narrow to these wake reasons. One of: " +
					reasonList() + ". Omit for every reason, which is not " +
					"the same as the primary half — that CLASSIFIES and this " +
					"FILTERS.",
			},
			"primary_only": map[string]any{
				"type": "boolean",
				"description": "Drop the context half rather than labelling " +
					"it, for a caller with room for one list.",
			},
			"unread": map[string]any{
				"type": "boolean",
				"description": "Drop what they have already read. Applied to " +
					"the page, so use `since` for the cheap form.",
			},
			"include_snoozed": map[string]any{
				"type": "boolean",
				"description": "Keep what they snoozed. Off by default, " +
					"because a snooze means `not now`. One whose time has " +
					"come comes back either way.",
			},
			"since": map[string]any{
				"type": "string",
				"description": "A log position — `stream@generation:seq`. " +
					"Pass the `seen_through` from a previous read to get " +
					"only what has arrived since.",
			},
			"cursor": map[string]any{
				"type":        "string",
				"description": "The `next_cursor` of the previous page.",
			},
			"limit": map[string]any{
				"type": "number",
				"description": fmt.Sprintf("How many notices, at most %d.",
					tracker.MaxInboxRows),
			},
		},
		"required": []string{"handle"},
	}
}

func (t *workInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	if t.deps.Inbox == nil {
		return unconfigured(tracker.WorkInboxTool), nil
	}
	handle := strings.TrimSpace(argString(args, "handle"))
	if handle == "" {
		return failed("Name whose inbox to read with `handle`."), nil
	}
	q := tracker.InboxQuery{
		// BOTH OF THAT PERSON'S NAMES — see [getPerson.Call]. The
		// applier writes one notification row per RECIPIENT the record
		// named, so a change that concerned a founder under the
		// credential they filed something with is a row the seat alone
		// never sees.
		Who:            t.deps.partyOf(handle),
		PrimaryOnly:    argBool(args, "primary_only"),
		Unread:         argBool(args, "unread"),
		IncludeSnoozed: argBool(args, "include_snoozed"),
		Cursor:         strings.TrimSpace(argString(args, "cursor")),
		Limit:          int(argFloat(args, "limit")),
		Level:          seatReadLevel,
	}
	for _, raw := range argStrings(args, "reasons") {
		reason := tracker.Reason(strings.TrimSpace(raw))
		if !slices.Contains(tracker.Reasons, reason) {
			return failed(fmt.Sprintf("%q is not a wake reason. The reasons "+
				"are: %s.", raw, reasonList())), nil
		}
		q.Reasons = append(q.Reasons, reason)
	}
	if since := strings.TrimSpace(argString(args, "since")); since != "" {
		at, err := tracker.ParseLogPosition(since)
		if err != nil {
			return failed(err.Error()), nil
		}
		q.Since = at
	}
	answer, err := t.deps.Inbox.Inbox(ctx, q, t.deps.now())
	if err != nil {
		return failed(readFailure(tracker.WorkInboxTool, err)), nil
	}
	return jsonResult(answer)
}

// reasonList is the wake reasons as one sentence, DERIVED rather than typed
// out: a reason added to the set has to reach the description, and a literal
// list is the reader it would not reach.
func reasonList() string {
	names := make([]string, 0, len(tracker.Reasons))
	for _, reason := range tracker.Reasons {
		names = append(names, string(reason))
	}
	return strings.Join(names, ", ")
}
