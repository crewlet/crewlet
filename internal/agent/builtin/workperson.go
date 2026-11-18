package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The person tools: one human's inbox, queue and pins. A seat has none: it has
// a mailbox (the durable subscription the engine attaches with the seat), and
// nothing on a person's record describes it. They are the OPERATOR surface's,
// where a person acts through their own credential.
//
// Who may write what is internal/worklet's: inbox and pins only for their
// owner, priorities also for a LEAD of that person; these tools add the org
// lookup the tracker lacks. Every verb resolves its name to a RECORD through
// [WorkDeps.personRecord] ([iam.OwnerOf], which the query surface reads under
// too) BEFORE deciding anything, and asks the verb of that record: the lead
// relation is about a seat, so decided on a login it refuses a lead their report.

// PersonWriter is the tracker write side these tools need.
//
// The inbox and pins take GESTURES, never lists: the tracker resolves each
// against the record its own decide reads, so a tool never writes back a list
// it read and erases marks made since ([inbox.Gesture]). Every verb carries the
// authority the table decided ([admittedWrite]): the tracker keeps one rule of
// its own (an agent never re-orders a colleague's queue) and refuses a write
// nobody admitted.
type PersonWriter interface {
	MarkInbox(ctx context.Context, opID, handle string, seen statelog.Position,
		gesture inbox.Gesture[worklet.Reason],
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string,
		gesture worklet.PinGesture,
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string,
		priorities []string, ifMatch *uint64,
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
}

// PersonReader is the read side.
type PersonReader interface {
	Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error)
}

type getPerson struct{ deps WorkDeps }

var _ tools.Callable = (*getPerson)(nil)

func (t *getPerson) Name() string { return worklet.GetPersonTool }

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
		return unconfigured(worklet.GetPersonTool), nil
	}
	name := strings.TrimSpace(argString(args, "handle"))
	if name == "" {
		return failed("Name whose state to read with `handle`."), nil
	}
	handle, refusal := t.deps.personRecord(ctx, worklet.GetPersonTool,
		authz.ActionPersonRead, name, readAsNamed)
	if refusal != nil {
		return *refusal, nil
	}
	state, err := t.deps.Reader.Person(ctx, worklet.PersonQuery{
		Handle: handle, Level: seatReadLevel,
	}, t.deps.now())
	if err != nil {
		return readFailure(ctx, worklet.GetPersonTool, err), nil
	}
	return jsonResult(state)
}

type setPriorities struct{ deps WorkDeps }

var _ tools.Callable = (*setPriorities)(nil)

func (t *setPriorities) Name() string { return worklet.SetPrioritiesTool }

func (t *setPriorities) Description() string {
	return "Set the order somebody means to work in — your own, or somebody " +
		"in your line. Setting another person's is recorded as yours on their " +
		"record, so they can see who chose it; their own next change clears " +
		"that. An order is a statement about every entry at once, so the " +
		"list you send is the whole list — pass `if_match` with the version " +
		worklet.GetPersonTool + " answered and it is refused if the list changed since."
}

func (t *setPriorities) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"handle": map[string]any{
				"type":        "string",
				"description": "Whose queue. Omit for your own.",
			},
			"items": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "The work items, most important first. " +
					itemRefs,
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The `version` " + worklet.GetPersonTool + " answered for this " +
					"person. Given, the write is REFUSED if their record " +
					"changed since you read it — a reorder made from an " +
					"old screen would otherwise put back an order somebody " +
					"has since replaced. Omitted, the list is written as sent.",
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

	actor, writer, refusal := t.person(ctx, turn, worklet.SetPrioritiesTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	// Your own queue is never looked up and somebody else's LOGIN is never a
	// seat: your own (by omission or either name) is the record your writes
	// are made under, and a login is the identity directory's to resolve. Only
	// a name that is neither (handle, role, display name, address) reaches the
	// chart, whose fuzzy resolver would turn a login into the seat it resembles.
	// The authority is asked on what the name RESOLVES TO, not at the gate,
	// because the argument is not the object: [subjectOf] marks this verb
	// `inTool` for that, and [WorkDeps.personRecord] is the one decision.
	handle, refusal := t.deps.personRecord(ctx, worklet.SetPrioritiesTool,
		authz.ActionPrioritiesSet, argString(args, "handle"),
		func(typed string) (string, string) {
			// Resolved against the chart: a typo would write a whole
			// person record nobody reads, with a wake Route drops.
			return t.deps.resolveHandle(ctx, worklet.SetPrioritiesTool,
				"`handle`", typed)
		})
	if refusal != nil {
		return *refusal, nil
	}
	// The gesture bar is the tracker's, passed rather than decided: a seat
	// that LEADS somebody is admitted above and still may not re-order their
	// queue, a hand-off in disguise ([worklet.PersonAuthority]).
	authority := admittedWrite(actor)
	// Every entry is resolved to an ID: the list is stored as ids and joined
	// on them, and the description invites a key. An unresolved key vanishes
	// from `my_work` and `preset=priorities` and suppresses the wake, while
	// the call answers `applied`.
	items := argStrings(args, "items")
	resolved := make([]string, 0, len(items))
	for _, ref := range items {
		id, unresolved := t.deps.resolveRef(ctx, worklet.SetPrioritiesTool,
			"`items`", ref)
		if unresolved != nil {
			return *unresolved, nil
		}
		resolved = append(resolved, id)
	}
	// An order is a statement about every entry at once, so a reorder from
	// an old screen would put back an order somebody has since replaced.
	var ifMatch *uint64
	if _, given := args["if_match"]; given {
		version := argInt(args, "if_match", -1)
		if version < 0 {
			return failed("`if_match` is the `version` " + worklet.GetPersonTool + " answered, " +
				"a whole number of zero or more."), nil
		}
		expected := uint64(version)
		ifMatch = &expected
	}
	opID := opIDFor(actor, t.Name(), "prio", handle, args)
	result, err := writer.WritePriorities(ctx, opID, handle, resolved, ifMatch,
		authority)
	if errors.Is(err, worklet.ErrStaleVersion) {
		// The remedy names the read that shows the list: the generic one
		// sends a caller to get_work_item, which knows no person's queue.
		return refusedBy(tools.RefusalStaleVersion, err, fmt.Sprintf("%s was "+
			"refused: %s. Nothing is wrong with your order — somebody "+
			"changed this person's record after you read it. Call "+
			worklet.GetPersonTool+" again and decide from the list it shows now.",
			worklet.SetPrioritiesTool, worklet.Sentence(err))), nil
	}
	if err != nil {
		return writeFailure(ctx, actor, worklet.SetPrioritiesTool, err), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, worklet.SetPrioritiesTool,
			fmt.Sprintf("%s's priorities were set", handle), opID,
			result.Unvouched, unknownNext(result.Unvouched,
				sameCall(actor, worklet.SetPrioritiesTool),
				"Read the list with "+worklet.GetPersonTool,
				"it sets the same list again, which changes nothing")), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"handle": handle, "outcome": string(result.Outcome), "position": positionOf(result.Position),
		"version": result.Version,
	}, actor))
}

type setPins struct{ deps WorkDeps }

var _ tools.Callable = (*setPins)(nil)

func (t *setPins) Name() string { return worklet.SetPinsTool }

func (t *setPins) Description() string {
	return "Change your own pinned views and starred things. A pin puts a " +
		"view first in its container's strip, for you and nobody else. Each " +
		"takes a change — `add` and `remove` against what is there now, or " +
		"`set` for the whole list — so a star from one screen never drops a " +
		"pin made from another."
}

func (t *setPins) Parameters() map[string]any {
	favorite := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "description": "project, task or view."},
			"id":   map[string]any{"type": "string"},
		},
		"required": []string{"kind", "id"},
	}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"views": setChangeSchema(map[string]any{"type": "string"},
				"Saved view ids: `add` pins, `remove` unpins, `set` is the "+
					"whole strip in order."),
			"favorites": setChangeSchema(favorite,
				"Things to star: `add` stars, `remove` unstars, `set` is "+
					"the whole list."),
		},
	})
}

// setChangeSchema is one [worklet.SetChange] as an argument: a delta, or the
// whole set, and never both.
func setChangeSchema(item map[string]any, description string) map[string]any {
	list := map[string]any{"type": "array", "items": item}
	return map[string]any{
		"type":        "object",
		"description": description + " State `set` or `add`/`remove`, not both.",
		"properties": map[string]any{
			"add": list, "remove": list, "set": list,
		},
	}
}

func (t *setPins) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setPins) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, worklet.SetPinsTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	return t.deps.writePins(ctx, actor, writer, actor.Handle, args,
		admittedWrite(actor)), nil
}

// writePins is set_pins' body for ONE named record under an authority the
// caller has already decided — see [SetPinsFor].
func (d WorkDeps) writePins(ctx context.Context, actor Actor, writer PersonWriter,
	handle string, args map[string]any,
	authority worklet.PersonAuthority) tools.Result {

	views, bad := viewChange(args)
	if bad != "" {
		return failed(bad)
	}
	favorites, bad := favoriteChange(args)
	if bad != "" {
		return failed(bad)
	}
	opID := opIDFor(actor, worklet.SetPinsTool, "pins", handle, args)
	result, err := writer.WritePins(ctx, opID, handle,
		worklet.PinGesture{Views: views, Favorites: favorites}, authority)
	if err != nil {
		return writeFailure(ctx, actor, worklet.SetPinsTool, err)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, worklet.SetPinsTool,
			fmt.Sprintf("%s's pins were set", handle), opID, result.Unvouched,
			unknownNext(result.Unvouched, sameCall(actor, worklet.SetPinsTool),
				"Read them with "+worklet.GetPersonTool,
				"it sets the same lists again, which changes nothing"))
	}
	d.settle(ctx, result.Position)
	answer, _ := jsonResult(withOperation(map[string]any{
		"handle": handle, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	}, actor))
	return answer
}

type markInbox struct{ deps WorkDeps }

var _ tools.Callable = (*markInbox)(nil)

func (t *markInbox) Name() string { return worklet.MarkInboxTool }

func (t *markInbox) Description() string {
	return "Move your own inbox by one gesture. Name notices by the " +
		"`record_id` " + worklet.WorkInboxTool + " gives them: `read` marks them done, " +
		"`unread` brings them back, `snooze` puts them off until a time, " +
		"`unsnooze` brings a snoozed one back now. `read_through` reads " +
		"everything up to a log position — pass the newest notice you have " +
		"seen, as `<stream>@<generation>:<sequence>` — and never moves your " +
		"position backwards. Only what you name changes: every other mark, " +
		"snooze and your read position stay exactly as they are. Pass `seen`, " +
		"the newest tracker-log position you read (a notice's own `position`), " +
		"so the mark waits for the notices you were shown rather than being " +
		"refused by a node not yet at them. Your code notices are marked with " +
		CodeMarkInboxTool + ": each domain keeps its own marks."
}

func (t *markInbox) Parameters() map[string]any {
	ids := func(description string) map[string]any {
		return map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"description": description,
		}
	}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"read":     ids("Record ids to mark read — done."),
			"unread":   ids("Record ids to mark unread again."),
			"unsnooze": ids("Record ids whose snooze to lift now."),
			"snooze": map[string]any{
				"type":        "array",
				"description": "Notices to put off, each until an instant.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"record_id": map[string]any{"type": "string"},
						"until": map[string]any{
							"type": "string",
							"description": fmt.Sprintf("RFC3339; when it "+
								"comes back. In the future, and at most "+
								"%d days away.", int(inbox.MaxSnoozeAhead.Hours()/24)),
						},
					},
					"required": []string{"record_id", "until"},
				},
			},
			"read_through": map[string]any{
				"type": "string",
				"description": "Read everything at or before this log " +
					"position, `<stream>@<generation>:<sequence>` — a " +
					"notice's `log_stream`, `log_generation` and `log_seq`. " +
					"A position at or behind the one you have read to " +
					"changes nothing.",
			},
			"seen": map[string]any{
				"type": "string",
				"description": "How far you have read the tracker's log, " +
					"`<stream>@<generation>:<sequence>` — the `position` " +
					"of the newest work notice you were shown, or the " +
					"tracker source's `newest`. The mark is decided once " +
					"its node has applied it, so a notice you were shown " +
					"is never refused as unknown because the node asked " +
					"had not reached it yet. Omit it to wait for nothing.",
			},
			"primary_reasons": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Which wake reasons land in the PRIMARY half " +
					"of `" + worklet.WorkInboxTool + "`, the rest being context. One of " +
					words.Or(worklet.Reasons) + ". An empty list takes the shipped " +
					"default (" + words.And(worklet.DefaultPrimaryReasons) + ") rather than " +
					"making nothing primary; omit it to leave your choice " +
					"as it is.",
			},
		},
	})
}

func (t *markInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markInbox) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, writer, refusal := t.deps.personWriter(ctx, turn, worklet.MarkInboxTool, args)
	if refusal != nil {
		return *refusal, nil
	}
	return t.deps.writeInbox(ctx, actor, writer, actor.Handle, args,
		admittedWrite(actor)), nil
}

// writeInbox is mark_inbox's body for ONE named record under an authority the
// caller has already decided — see [MarkInboxFor].
func (d WorkDeps) writeInbox(ctx context.Context, actor Actor, writer PersonWriter,
	handle string, args map[string]any,
	authority worklet.PersonAuthority) tools.Result {

	gesture, bad := inboxGesture(args)
	if bad != "" {
		return failed(bad)
	}
	seen, bad := markFloor(args, topics.TrackerLogStream, "tracker's log", "pass the `position` of a work notice")
	if bad != "" {
		return failed(bad)
	}
	opID := opIDFor(actor, worklet.MarkInboxTool, "inbox", handle, args)
	result, err := writer.MarkInbox(ctx, opID, handle, seen, gesture, authority)
	if err != nil {
		return writeFailure(ctx, actor, worklet.MarkInboxTool, err)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, worklet.MarkInboxTool,
			fmt.Sprintf("%s's inbox was marked", handle), opID, result.Unvouched,
			unknownNext(result.Unvouched, sameCall(actor, worklet.MarkInboxTool),
				"Read it with "+worklet.WorkInboxTool,
				"it marks the same notices again, which changes nothing"))
	}
	d.settle(ctx, result.Position)
	answer, _ := jsonResult(withOperation(map[string]any{
		"handle": handle, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	}, actor))
	return answer
}

// MarkInboxFor and SetPinsFor write the inbox or the pins of the person a NAME
// addresses (somebody else's, or the caller's own by either name), deciding
// the verb on the record the name resolves to.
//
// The tools take no handle on purpose: a model that could name whose inbox to
// mark could mark anybody's, and only an administrator at a screen unsticking a
// departed person's queue needs another's record. That surface (`PUT
// /worklet/people/{handle}/inbox` and `…/pins`) comes through here so parsing,
// writer and receipt have no second copy to drift. The name goes through
// [WorkDeps.personRecord], a seat matched EXACTLY against the chart
// ([chartRecord]), never fuzzily; the tracker's own rule (a seat never writes
// a colleague's record) still applies on top.
func MarkInboxFor(ctx context.Context, deps WorkDeps, name string,
	args map[string]any) tools.Result {

	actor, writer, refusal := deps.personWriter(ctx, nil, worklet.MarkInboxTool, args)
	if refusal != nil {
		return *refusal
	}
	whose, refusal := deps.personRecord(ctx, worklet.MarkInboxTool,
		authz.ActionInboxMark, name, deps.chartRecord(worklet.MarkInboxTool))
	if refusal != nil {
		return *refusal
	}
	return deps.writeInbox(ctx, actor, writer, whose, args, admittedWrite(actor))
}

// SetPinsFor is [MarkInboxFor] for the pins.
func SetPinsFor(ctx context.Context, deps WorkDeps, name string,
	args map[string]any) tools.Result {

	actor, writer, refusal := deps.personWriter(ctx, nil, worklet.SetPinsTool, args)
	if refusal != nil {
		return *refusal
	}
	whose, refusal := deps.personRecord(ctx, worklet.SetPinsTool,
		authz.ActionPinsSet, name, deps.chartRecord(worklet.SetPinsTool))
	if refusal != nil {
		return *refusal
	}
	return deps.writePins(ctx, actor, writer, whose, args, admittedWrite(actor))
}

// personRecord is the record a person verb's NAME addresses with the verb
// decided on it, or the refusal. [iam.OwnerOf] resolves the name (the query
// surface reads under it too); one it leaves unsettled goes to seat: as typed
// for a read ([readAsNamed]), exactly against the chart for a route's path
// ([chartRecord]), the colleague lookup for a model's ([WorkDeps.resolveHandle]).
//
// Somebody else's login is looked up only once [WorkDeps.mayLook] admits it,
// and the verb is decided again on the resolved SEAT, never the spelling. A
// login naming no record is decided on the name as typed (only admin passes)
// before it is refused as not found, and an undecidable node answers 503
// ([ErrUndecidable]) with the detail only in the log: any other order lets a caller with no authority read
// which logins exist, and whose seat each holds, off the difference.
func (d WorkDeps) personRecord(ctx context.Context, tool string,
	action authz.Action, name string,
	seat func(string) (string, string)) (string, *tools.Result) {

	name = strings.TrimSpace(name)
	principal, _ := iam.From(ctx)
	owner, settled, err := iam.OwnerOf(ctx, principal, name, d.Holders,
		d.mayLook(action))
	var looked *iam.LookRefused
	switch {
	case errors.As(err, &looked):
		// The table's own refusal: the bytes this caller gets on a seat
		// they do not lead.
		return "", refusalOf(authorityRefusal(string(action), looked.Err))
	case errors.Is(err, iam.ErrNoHolder), errors.Is(err, iam.ErrHolderUnseated):
		if denied := d.mayWrite(ctx, action,
			authz.Object{Kind: authz.KindPerson, Owner: name}); denied != nil {

			return "", denied
		}
		// No such record: the not-found class, not an argument to change.
		return "", refusalOf(refusedBy(tools.RefusalNotFound, err, fmt.Sprintf(
			"%s names %q, and there is no record of anybody's under it: %v.",
			tool, clip(name), err)))
	case err != nil:
		log.WarnContext(ctx, "builtin_person_record_undecidable",
			"tool", tool, "name", name, "error", err.Error(),
			"detail", "this node could not say whose record the name is; the "+
				"caller was told to try again and not why")
		return "", refusalOf(refusedBy(tools.RefusalUnavailable,
			fmt.Errorf("%w whose record %q is", ErrUndecidable, name),
			fmt.Sprintf("%s could not be decided: this node cannot say whose "+
				"record %q is right now. Try again in a moment.", tool,
				clip(name))))
	case !settled:
		resolved, refusal := seat(owner)
		if refusal != "" {
			return "", refusalOf(failed(refusal))
		}
		owner = resolved
	case owner == "":
		return "", refusalOf(failed(fmt.Sprintf("%s has nobody's record to "+
			"act on: this credential has no record of its own. Name whose it "+
			"is.", tool)))
	}
	if denied := d.mayWrite(ctx, action,
		authz.Object{Kind: authz.KindPerson, Owner: owner}); denied != nil {

		return "", denied
	}
	return owner, nil
}

// mayLook is the authority a person verb decides BEFORE the identity directory
// is asked whose record somebody else's login is ([iam.MayLook],
// [authz.Object.Unresolved]). [authz.ErrUnresolved], the answer for a caller
// who could be admitted only as the login holder's lead, lets the lookup
// happen and nothing more: [WorkDeps.personRecord] decides the verb again on
// the resolved record. Every other answer comes back as it is.
func (d WorkDeps) mayLook(action authz.Action) iam.MayLook {
	return func(ctx context.Context, login string) error {
		if d.Authorize == nil {
			return ErrNoAuthorizer
		}
		err := d.Authorize(ctx, action, authz.Object{
			Kind: authz.KindPerson, Owner: login, Unresolved: true,
		})
		if errors.Is(err, authz.ErrUnresolved) {
			return nil
		}
		return err
	}
}

// readAsNamed is how a READ resolves a name [iam.OwnerOf] left unsettled: as
// typed, so a name with no record reads the empty one a person starts with,
// as the query surface answers, and the tool and the screen agree.
func readAsNamed(name string) (string, string) { return name, "" }

// chartRecord is how a write a route names by its path resolves a name
// [iam.OwnerOf] left unsettled: EXACTLY a seat the chart has, or refused.
// Never the fuzzy resolver, which is for a model's words and would write into
// whichever seat a path resembled; but a name not shaped like a handle, or a
// handle the chart lacks, is refused so no record lands under a name nobody
// has. A nil or empty roster admits a well-formed handle, for
// [WorkDeps.resolveHandle]'s reason.
func (d WorkDeps) chartRecord(tool string) func(string) (string, string) {
	return func(handle string) (string, string) {
		refusal := fmt.Sprintf("%s names %q, and nobody's own record is kept "+
			"under it: a record is under a seat's exact handle, or a person's or "+
			"a machine's login.", tool, clip(handle))
		if !org.ValidHandle(handle) {
			return "", refusal
		}
		if d.Seats == nil {
			return handle, ""
		}
		seats := d.Seats()
		if len(seats) == 0 {
			return handle, ""
		}
		for _, seat := range seats {
			if seat.Handle == handle {
				return handle, ""
			}
		}
		return "", refusal
	}
}

// admittedWrite is the authority a person verb passes the tracker once the
// table has admitted it ([WorkDeps.personRecord] for a named record, the
// registration gate for the caller's own). Authorized is always true because
// nothing reaches here unadmitted, and [worklet.ownRecord] compares actor to
// handle first; what matters is Agent, the tracker's own GESTURE bar.
func admittedWrite(actor Actor) worklet.PersonAuthority {
	return worklet.PersonAuthority{Authorized: true, Agent: !actor.Kind.Person()}
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
		refusal := unattributed(name, err)
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

// inboxGesture reads mark_inbox's arguments, or the sentence refusing them —
// [gestureOf] over the tracker's reasons.
func inboxGesture(args map[string]any) (inbox.Gesture[worklet.Reason], string) {
	return gestureOf(args, worklet.Reasons, words.And(worklet.Reasons))
}

// gestureOf reads an inbox mark's arguments as one gesture over a domain's
// reasons, or the sentence refusing them: ONE PARSER for every domain's mark
// tool (mark_inbox, code_mark_inbox), so a shared schema is read and refused
// the same way. PRESENCE decides `primary_reasons`, which a nil slice cannot:
// an empty list takes the shipped default, an absent one leaves the person's
// choice alone. reasons is the domain's set a declared reason must be in, and
// list names it as one sentence.
func gestureOf[R ~string](args map[string]any, reasons []R, list string) (inbox.Gesture[R], string) {
	var g inbox.Gesture[R]
	g.Read = argStrings(args, "read")
	g.Unread = argStrings(args, "unread")
	g.Unsnooze = argStrings(args, "unsnooze")
	if raw, held := args["snooze"]; held {
		items, ok := argval.List(raw)
		if !ok {
			return g, "`snooze` is a list of `{record_id, until}`."
		}
		for i, item := range items {
			fields, ok := item.(map[string]any)
			if !ok {
				return g, fmt.Sprintf("Entry %d of `snooze` is not an object "+
					"with `record_id` and `until`.", i+1)
			}
			raw := strings.TrimSpace(argString(fields, "until"))
			until, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return g, fmt.Sprintf("`until` on entry %d of `snooze` is a "+
					"date and time in RFC3339, like 2026-06-30T09:00:00Z — %q "+
					"is not one.", i+1, clip(raw))
			}
			g.Snooze = append(g.Snooze, inbox.Snooze{
				RecordID: strings.TrimSpace(argString(fields, "record_id")),
				Until:    until.UTC(),
			})
		}
	}
	if raw := strings.TrimSpace(argString(args, "read_through")); raw != "" {
		at, err := statelog.ParsePosition(raw)
		if err != nil {
			return g, err.Error()
		}
		g.ReadThrough = &at
	}
	if _, held := args["primary_reasons"]; held {
		declared := []R{}
		for _, raw := range argStrings(args, "primary_reasons") {
			reason := R(strings.TrimSpace(raw))
			if !slices.Contains(reasons, reason) {
				return g, fmt.Sprintf("%q is not a wake reason. The reasons "+
					"are: %s.", raw, list)
			}
			declared = append(declared, reason)
		}
		g.PrimaryReasons = &declared
	}
	if g.Empty() {
		return g, "Name what to mark: `read`, `unread`, `snooze` or " +
			"`unsnooze` with the notices' `record_id`s, `read_through` with " +
			"a position, or `primary_reasons`."
	}
	return g, ""
}

// changeOf reads one set_pins argument as its three lists, or the sentence
// refusing it.
//
// read is how each list is read: [argval.StringItems] for a list of ids, which
// takes the single string a model sends for one, and [argval.List] for a list
// of objects.
func changeOf(args map[string]any, key string, read func(any) ([]any, bool)) (set, add, remove []any,
	hasSet bool, bad string) {

	raw, held := args[key]
	if !held || raw == nil {
		return nil, nil, nil, false, ""
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, nil, false, fmt.Sprintf("`%s` is a change, not a "+
			"list: `{add: [...]}` or `{remove: [...]}` against what is there, "+
			"or `{set: [...]}` for the whole list.", key)
	}
	list := func(name string) ([]any, bool, string) {
		value, present := fields[name]
		if !present || value == nil {
			return nil, false, ""
		}
		items, ok := read(value)
		if !ok {
			return nil, false, fmt.Sprintf("`%s.%s` is a list.", key, name)
		}
		return items, true, ""
	}
	set, hasSet, bad = list("set")
	if bad != "" {
		return nil, nil, nil, false, bad
	}
	if add, _, bad = list("add"); bad != "" {
		return nil, nil, nil, false, bad
	}
	if remove, _, bad = list("remove"); bad != "" {
		return nil, nil, nil, false, bad
	}
	return set, add, remove, hasSet, ""
}

// viewChange reads the pinned views' change.
func viewChange(args map[string]any) (worklet.SetChange[string], string) {
	set, add, remove, hasSet, bad := changeOf(args, "views", argval.StringItems)
	if bad != "" {
		return worklet.SetChange[string]{}, bad
	}
	strs := func(items []any) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			if v, ok := argval.String(item); ok && item != nil {
				out = append(out, strings.TrimSpace(v))
			} else {
				out = append(out, "")
			}
		}
		return out
	}
	change := worklet.SetChange[string]{Add: strs(add), Remove: strs(remove)}
	if hasSet {
		whole := strs(set)
		change.Set = &whole
	}
	return change, ""
}

// favoriteChange reads the starred things' change.
func favoriteChange(args map[string]any) (worklet.SetChange[worklet.Favorite], string) {
	set, add, remove, hasSet, bad := changeOf(args, "favorites", argval.List)
	if bad != "" {
		return worklet.SetChange[worklet.Favorite]{}, bad
	}
	favs := func(name string, items []any) ([]worklet.Favorite, string) {
		out := make([]worklet.Favorite, 0, len(items))
		for i, item := range items {
			fields, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Sprintf("Favourite %d of `favorites.%s` is not "+
					"an object with `kind` and `id`.", i+1, name)
			}
			out = append(out, worklet.Favorite{
				Kind: strings.TrimSpace(argString(fields, "kind")),
				ID:   strings.TrimSpace(argString(fields, "id")),
			})
		}
		return out, ""
	}
	var change worklet.SetChange[worklet.Favorite]
	if change.Add, bad = favs("add", add); bad != "" {
		return change, bad
	}
	if change.Remove, bad = favs("remove", remove); bad != "" {
		return change, bad
	}
	if hasSet {
		whole, bad := favs("set", set)
		if bad != "" {
			return change, bad
		}
		change.Set = &whole
	}
	return change, ""
}

// InboxReader is the read side the inbox tool needs, apart from
// [PersonReader]: that is the person's own marks, this what the company asked
// of them.
type InboxReader interface {
	Inbox(ctx context.Context, q worklet.InboxQuery, now time.Time) (
		worklet.InboxAnswer, error)
}

type workInbox struct{ deps WorkDeps }

var _ tools.Callable = (*workInbox)(nil)

func (t *workInbox) Name() string { return worklet.WorkInboxTool }

func (t *workInbox) Description() string {
	return "What the company has asked of somebody, newest first: every " +
		"change routed to them, the one reason each reached them under, and " +
		"whether they have read it. The PRIMARY half is what they have said " +
		"is theirs to act on — mentions, questions, their own work — and the " +
		"rest is context. This is the durable record the engine wrote when " +
		"the change landed, so it is there whether or not anybody was online. " +
		"Each notice's text arrives fenced as text its change's maker wrote; " +
		"the two fence lines are a label, not part of the notice."
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
				"description": "Narrow to these wake reasons. One of " +
					words.Or(worklet.Reasons) + ". Omit for every reason, which is not " +
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
				"description": "Only what they have not read. The page is " +
					"still full: this narrows the scan, not the page.",
			},
			"snoozed": map[string]any{
				"type": "string",
				"enum": snoozeScopeNames(),
				"description": "What to do with what they snoozed: `exclude` " +
					"(the default — a snooze means `not now`), `include` to " +
					"keep them marked, or `only` for just what they put off. " +
					"One whose time has come is back in the inbox and is not " +
					"`only`.",
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
				"type": "integer",
				"description": fmt.Sprintf("How many notices, at most %d.",
					inbox.MaxRows),
			},
		},
		"required": []string{"handle"},
	}
}

func (t *workInbox) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	if t.deps.Inbox == nil {
		return unconfigured(worklet.WorkInboxTool), nil
	}
	name := strings.TrimSpace(argString(args, "handle"))
	if name == "" {
		return failed("Name whose inbox to read with `handle`."), nil
	}
	handle, refusal := t.deps.personRecord(ctx, worklet.WorkInboxTool,
		authz.ActionInboxRead, name, readAsNamed)
	if refusal != nil {
		return *refusal, nil
	}
	q := worklet.InboxQuery{
		Handle:      handle,
		PrimaryOnly: argBool(args, "primary_only"),
		Unread:      argBool(args, "unread"),
		Snoozed:     inbox.SnoozeExclude,
		Cursor:      strings.TrimSpace(argString(args, "cursor")),
		Limit:       argInt(args, "limit", 0),
		Level:       seatReadLevel,
	}
	if raw := strings.TrimSpace(argString(args, "snoozed")); raw != "" {
		q.Snoozed = inbox.SnoozeScope(raw)
		if !q.Snoozed.Valid() {
			return failed(fmt.Sprintf("%q is not a `snoozed` scope. The "+
				"scopes are: %s.", raw, strings.Join(snoozeScopeNames(), ", "))), nil
		}
	}
	for _, raw := range argStrings(args, "reasons") {
		reason := worklet.Reason(strings.TrimSpace(raw))
		if !slices.Contains(worklet.Reasons, reason) {
			return failed(fmt.Sprintf("%q is not a wake reason. The reasons "+
				"are: %s.", raw, words.And(worklet.Reasons))), nil
		}
		q.Reasons = append(q.Reasons, reason)
	}
	if since := strings.TrimSpace(argString(args, "since")); since != "" {
		at, err := statelog.ParsePosition(since)
		if err != nil {
			return failed(err.Error()), nil
		}
		q.Since = at
	}
	answer, err := t.deps.Inbox.Inbox(ctx, q, t.deps.now())
	if err != nil {
		return readFailure(ctx, worklet.WorkInboxTool, err), nil
	}
	// Each notice's text is its change's maker's words, fenced as the activity
	// feed fences that change (ADR-0040); an ask's decision is a name with no
	// recorded writer. Read as the caller, so their own change is their words.
	self := ""
	if actor, err := t.deps.actor(ctx, nil); err == nil {
		self = actor.Handle
	}
	read := newForeignRead(nil, self)
	for i, n := range answer.Notices {
		fenceExcerpt(read, fieldPath{}.key("notices").index(i).key("excerpt"),
			n.Kind, n.Actor, n.ActorKind, n.Excerpt)
		if n.Ask != nil {
			namesBy(read)
			fenceWithdrawn(read, fieldPath{}.key("notices").index(i).key("ask"), n.Ask)
		}
	}
	return read.answer(ctx, worklet.WorkInboxTool, answer,
		"Ask for fewer with `limit`, or narrow with `reasons` or `since`.")
}

// snoozeScopeNames is the scopes, derived from [inbox.SnoozeScopes] so a new
// one needs no copy here.
func snoozeScopeNames() []string {
	out := make([]string, 0, len(inbox.SnoozeScopes))
	for _, scope := range inbox.SnoozeScopes {
		out = append(out, string(scope))
	}
	return out
}
