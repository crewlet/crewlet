package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
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
// # A name is resolved to its RECORD before anything is decided about it
//
// Every person verb names whose record it is about, and every one resolves that
// name through [WorkDeps.personRecord] — [iam.OwnerOf], the one function the
// query surface reads a record under too — BEFORE it asks the table or touches
// a row. The caller's own names are their own record; somebody else's LOGIN
// is their holder's record, looked up in the identity directory; anything
// else is the chart's. They used to be read literally, or sent through the
// fuzzy colleague resolver: an administrator's `jane.doe`, for a person bound
// to the seat `jane`, read an empty inbox and wrote marks where nothing of hers
// reads them, and `set_priorities` set the queue of whichever seat the login
// resembled. And the verb is asked on the RESOLVED record, because the lead
// relation is a fact about a seat: decided on a login, a lead was refused the
// report they lead.

// PersonWriter is the tracker write side these tools need.
type PersonWriter interface {
	WriteInbox(ctx context.Context, opID, handle string,
		read, unread, snoozed []tracker.InboxEntry, reasons []tracker.Reason,
		seenThrough tracker.Position,
		authority tracker.PersonAuthority) (tracker.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string,
		pinnedViews []string, favorites []tracker.Favorite,
		authority tracker.PersonAuthority) (tracker.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string,
		priorities []string, authority tracker.PersonAuthority) (tracker.WriteResult, error)
}

// PersonReader is the read side.
type PersonReader interface {
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
}

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
	name := strings.TrimSpace(argString(args, "handle"))
	if name == "" {
		return failed("Name whose state to read with `handle`."), nil
	}
	handle, refusal := t.deps.personRecord(ctx, tracker.GetPersonTool,
		authz.ActionPersonRead, name, readAsNamed)
	if refusal != nil {
		return *refusal, nil
	}
	state, err := t.deps.Reader.Person(ctx, tracker.PersonQuery{
		Handle: handle, Level: seatReadLevel,
	}, t.deps.now())
	if err != nil {
		return readFailed(tracker.GetPersonTool, err), nil
	}
	return jsonResult(state)
}

type setPriorities struct{ deps WorkDeps }

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
	// YOUR OWN QUEUE IS NEVER LOOKED UP, and SOMEBODY ELSE'S LOGIN IS NEVER
	// A SEAT. Your own — by omission or by either of your names — is the
	// record your writes are made under; it was sent through the fuzzy
	// colleague resolver like anybody else's, so an unbound `jane.doe`
	// holding the admin grant set the priorities of the seat `jane`. And
	// the same happened to an administrator naming `jane.doe` for somebody
	// else: the route admitted the request on that name and the resolver
	// wrote into the seat it resembled. A login is the identity directory's
	// to resolve, so only a name that is neither — a handle, a role, a
	// display name, an address — reaches the chart.
	//
	// THE AUTHORITY IS ASKED ON WHAT THE NAME RESOLVES TO and not at the
	// gate, because the ARGUMENT IS NOT THE OBJECT: decided on what a model
	// typed, a lead who wrote their report's NAME was refused as leading
	// nobody called that. [subjectOf] marks this verb `inTool` for exactly
	// that reason, and [WorkDeps.personRecord] is the one relation decision
	// the call gets.
	handle, refused := t.deps.personRecord(ctx, tracker.SetPrioritiesTool,
		authz.ActionPrioritiesSet, argString(args, "handle"),
		func(typed string) (string, string) {
			// A TYPED NAME IS RESOLVED AGAINST THE CHART, because a
			// typo here writes a whole PERSON RECORD for somebody who
			// does not exist — a queue nobody will ever read, and a
			// wake routed to a handle Route drops in silence.
			return t.deps.resolveHandle(ctx, tracker.SetPrioritiesTool,
				"`handle`", typed)
		})
	if refused != nil {
		return *refused, nil
	}
	// AND THE GESTURE BAR IS THE TRACKER'S OWN, passed rather than
	// decided: a seat that LEADS somebody is admitted by the table above
	// and still may not re-order their queue, because that is a hand-off
	// in disguise. See [tracker.PersonAuthority].
	authority := admittedWrite(actor)
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
		"prio-"+handle+"-"+callKey(actor), handle, resolved, authority)
	if err != nil {
		return writeFailed(tracker.SetPrioritiesTool, err), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"handle": handle, "outcome": string(result.Outcome), "position": positionOf(result.Position),
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
						"kind": map[string]any{"type": "string", "description": "project, task or view."},
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
	return t.deps.writePins(ctx, actor, writer, actor.Handle, args,
		admittedWrite(actor)), nil
}

// writePins is set_pins' body for ONE named record under an authority the
// caller has already decided — see [SetPinsFor].
func (d WorkDeps) writePins(ctx context.Context, actor Actor, writer PersonWriter,
	handle string, args map[string]any,
	authority tracker.PersonAuthority) tools.Result {

	favorites, bad := personFavorites(args)
	if bad != "" {
		return failed(bad)
	}
	result, err := writer.WritePins(ctx,
		"pins-"+handle+"-"+callKey(actor), handle,
		argStrings(args, "views"), favorites, authority)
	if err != nil {
		return writeFailed(tracker.SetPinsTool, err)
	}
	d.settle(ctx, result.Position)
	answer, _ := jsonResult(map[string]any{
		"handle": handle, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	})
	return answer
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
	}
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

	actor, writer, refusal := t.deps.personWriter(ctx, turn, tracker.MarkInboxTool)
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
	authority tracker.PersonAuthority) tools.Result {

	read, bad := inboxEntries(args, "read")
	if bad != "" {
		return failed(bad)
	}
	unread, bad := inboxEntries(args, "unread")
	if bad != "" {
		return failed(bad)
	}
	snoozed, bad := inboxEntries(args, "snoozed")
	if bad != "" {
		return failed(bad)
	}
	var reasons []tracker.Reason
	for _, raw := range argStrings(args, "primary_reasons") {
		reason := tracker.Reason(strings.TrimSpace(raw))
		if !slices.Contains(tracker.Reasons, reason) {
			return failed(fmt.Sprintf("%q is not a wake reason. The reasons "+
				"are: %s.", raw, reasonList()))
		}
		reasons = append(reasons, reason)
	}
	result, err := writer.WriteInbox(ctx,
		"inbox-"+handle+"-"+callKey(actor), handle,
		read, unread, snoozed, reasons, tracker.Position{
			Stream: strings.TrimSpace(argString(args, "seen_through_stream")),
			Seq:    uint64(argFloat(args, "seen_through")),
		}, authority)
	if err != nil {
		return writeFailed(tracker.MarkInboxTool, err)
	}
	d.settle(ctx, result.Position)
	answer, _ := jsonResult(map[string]any{
		"handle": handle, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	})
	return answer
}

// MarkInboxFor and SetPinsFor write the inbox or the pins of the person a NAME
// addresses — somebody else's, or the caller's own named by either of their
// names — deciding the verb on the record the name resolves to.
//
// # Why these are not arguments on the tools
//
// `mark_inbox` and `set_pins` take no handle, and that is deliberate: a model
// that could name whose inbox to mark could mark anybody's, and the only
// caller with a legitimate reason to reach another person's record is an
// administrator unsticking a departed person's queue — a person at a screen,
// never a seat in a turn. So the TOOLS stay narrow, and the one surface that
// serves that administrator (the HTTP write surface, `PUT
// /work/people/{handle}/inbox` and `…/pins`) reaches the same parsing, the
// same writer and the same receipt through here. Two copies of "read an inbox
// mark out of a body" would drift on exactly the parts nobody re-reads: which
// entries are dropped, what a bad reason is refused with.
//
// # Resolved, THEN decided, and never looked up in the chart
//
// The name goes through [WorkDeps.personRecord]: the caller's own names are
// their own record, somebody else's login is their holder's record in the
// identity directory, and anything else must be a seat the chart has EXACTLY
// ([chartRecord]). The verb is asked on the record that resolves to — the
// owner, or the admin grant — because it was decided on the path's name
// before, and that name was not the record: an administrator naming a bound
// person's login was admitted and the write landed under the login, where no
// screen of theirs reads it. It used to go the other way too: the fuzzy
// colleague resolver turned a login into the seat whose display name it
// resembled, and wrote there.
//
// The tracker's own rule — a seat never writes a colleague's record — still
// applies on top, so an authority marked Agent is refused there whatever the
// table said.
func MarkInboxFor(ctx context.Context, deps WorkDeps, name string,
	args map[string]any) tools.Result {

	actor, writer, refusal := deps.personWriter(ctx, nil, tracker.MarkInboxTool)
	if refusal != nil {
		return *refusal
	}
	whose, refused := deps.personRecord(ctx, tracker.MarkInboxTool,
		authz.ActionInboxMark, name, deps.chartRecord(tracker.MarkInboxTool))
	if refused != nil {
		return *refused
	}
	return deps.writeInbox(ctx, actor, writer, whose, args, admittedWrite(actor))
}

// SetPinsFor is [MarkInboxFor] for the pins.
func SetPinsFor(ctx context.Context, deps WorkDeps, name string,
	args map[string]any) tools.Result {

	actor, writer, refusal := deps.personWriter(ctx, nil, tracker.SetPinsTool)
	if refusal != nil {
		return *refusal
	}
	whose, refused := deps.personRecord(ctx, tracker.SetPinsTool,
		authz.ActionPinsSet, name, deps.chartRecord(tracker.SetPinsTool))
	if refused != nil {
		return *refused
	}
	return deps.writePins(ctx, actor, writer, whose, args, admittedWrite(actor))
}

// personRecord is the record a person verb's NAME addresses, with the verb
// decided on it — or the refusal the tool answers.
//
// # Resolved first, by the one owner function
//
// [iam.OwnerOf] answers the name: the caller's own record for no name or
// either of theirs, a login's holder's record through [WorkDeps.Holders], and
// anything else handed back UNSETTLED for seat to resolve the way this verb
// resolves a name — as typed for a read, exactly against the chart for a
// write a route names by its path ([chartRecord]), through the colleague
// lookup for one a model typed ([WorkDeps.resolveHandle]). The query surface reads a
// person's record through the same function, so a record this writes is the
// record a screen reads.
//
// # But the authority comes before the directory
//
// Somebody else's login is looked up only once [WorkDeps.mayLook] has let it
// be: the verb decided on a record nobody has resolved yet. The admin grant
// passes, a caller who could be admitted only as the lead of whoever holds the
// login passes, and everybody else — a caller who leads nobody, and any
// non-admin on a verb only a record's owner may take — is refused with the
// very refusal they would get on somebody's seat they do not lead, before the
// directory is asked anything. Asked after, what the directory said differed
// by login: a login nobody holds was refused on the name as typed, and a held
// one this node could not place answered 503 naming the seat it was bound to —
// so a caller with no authority over anybody read which logins exist, and
// whose seat each holds, off the difference.
//
// # Then decided, on what it resolved to
//
// The relation the table asks is about a SEAT, so it is asked of the record
// and never of the spelling: a lead naming their report's login is admitted,
// where decided on the login they led nobody.
//
// # And a login that names nobody discloses nothing
//
// A login nobody holds, and a holder whose seat the chart no longer has, are
// decided on the name AS TYPED — which nobody leads, so only the admin grant
// passes — and only then refused as naming no record. A node that cannot say
// is 503-shaped ([ErrUndecidable]) and says nothing either way: the
// directory's own words go to the log, because they name the seat a login is
// bound to and the answer goes to whoever asked.
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
		// THE TABLE'S OWN REFUSAL, worded as every other one is — the
		// same bytes this caller gets on a seat they do not lead.
		refusal := refused(string(action), looked.Err)
		return "", &refusal
	case errors.Is(err, iam.ErrNoHolder), errors.Is(err, iam.ErrHolderUnseated):
		if refused := d.mayWrite(ctx, action,
			authz.Object{Kind: authz.KindPerson, Owner: name}); refused != nil {

			return "", refused
		}
		refusal := failedBy(err, fmt.Sprintf("%s names %q, and there is no "+
			"record of anybody's under it: %v.", tool, clip(name), err))
		return "", &refusal
	case err != nil:
		log.WarnContext(ctx, "builtin_person_record_undecidable",
			"tool", tool, "name", name, "error", err.Error(),
			"detail", "this node could not say whose record the name is; the "+
				"caller was told to try again and not why")
		refusal := failedBy(fmt.Errorf("%w whose record %q is", ErrUndecidable,
			name), fmt.Sprintf("%s could not be decided: this node cannot say "+
			"whose record %q is right now. Try again in a moment.", tool,
			clip(name)))
		return "", &refusal
	case !settled:
		resolved, refusal := seat(owner)
		if refusal != "" {
			failure := failed(refusal)
			return "", &failure
		}
		owner = resolved
	case owner == "":
		refusal := failed(fmt.Sprintf("%s has nobody's record to act on: "+
			"this credential has no record of its own. Name whose it is.", tool))
		return "", &refusal
	}
	if refused := d.mayWrite(ctx, action,
		authz.Object{Kind: authz.KindPerson, Owner: owner}); refused != nil {

		return "", refused
	}
	return owner, nil
}

// mayLook is the authority a person verb decides BEFORE the identity directory
// is asked whose record somebody else's login is — see [iam.MayLook] and
// [authz.Object.Unresolved]: the same verb, through the same authorizer, asked
// of a record nobody has resolved yet.
//
// [authz.ErrUnresolved] LETS THE LOOKUP HAPPEN and nothing more. It is the
// answer for a caller who could be admitted only as the lead of whoever holds
// the login, which is a question about the record — so the login is looked
// up, and [WorkDeps.personRecord] decides the verb again on what it resolved
// to. Every other answer is the caller's, and comes back as it is.
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

// readAsNamed is how a READ resolves a name [iam.OwnerOf] handed back
// unsettled: as typed. A seat's handle reads that seat's record, and a name no
// record is under reads the empty one a person starts with — the answer the
// query surface gives the same question, so the tool and the screen cannot
// disagree about somebody who has no record yet.
func readAsNamed(name string) (string, string) { return name, "" }

// chartRecord is how a write a route names by its path resolves a name
// [iam.OwnerOf] handed back unsettled: EXACTLY a seat the chart has, or
// refused.
//
// # Never looked up
//
// The fuzzy colleague resolver is for words a model typed. A route's path is
// a record's name, and resolving it turned it into whichever seat it
// resembled — a write into a record nobody named.
//
// # But refused when nobody could own it
//
// What the resolver did guard against is a record under a name nobody has, so
// a name that is not a seat handle's shape is refused, and so is a handle the
// chart lacks. A nil or empty roster admits a well-formed handle, for
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
// table has admitted it — [WorkDeps.personRecord]'s decision for a named
// record, or the registration gate's for the caller's own.
//
// AUTHORIZED IS ALWAYS TRUE HERE and that is not a rubber stamp: nothing
// reaches this without the table having answered for the record it names, and
// [tracker.ownRecord] compares the actor to the handle before it ever reads
// this. What the value carries that matters is the GESTURE bar, which is the
// tracker's own and not the table's.
func admittedWrite(actor Actor) tracker.PersonAuthority {
	return tracker.PersonAuthority{Authorized: true, Agent: !actor.Kind.Person()}
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

// callKey is the idempotency scope of ONE tool call — the actor's own
// operation seed where it has one, and a fresh value where it has none.
//
// THE ACTOR'S SEED AND NOT THE TURN'S KEY, because they are the same value
// for a seat ([actorFor] copies the turn's key onto the actor) and different
// for the one caller that has a seed and no turn: an HTTP write retried under
// its Idempotency-Key, whose actor carries that key. Read off the turn, the
// retry minted a fresh id and wrote the inbox, the pins or the priorities a
// second time.
//
// AN OPERATOR HAS NO TURN and no redelivery: their client made one call, so
// there is nothing to deduplicate against and two calls in one session are two
// writes, which is what the caller meant.
//
// THAT IS WHAT THIS ALWAYS CLAIMED AND NEVER DID. It returned the literal
// string `operator`, so the operation id it is half of — `prio-<handle>-operator`,
// `pins-…`, `inbox-…` — was stable for the
// life of the deployment, and the ledger collapsed every write after the first
// as a redelivery. `set_priorities` through `/operator/mcp` wrote one list per
// person, ever; the second call answered `applied` with the FIRST call's
// position and changed nothing. (The empty string the old comment named would
// have done exactly the same: what makes a key unique is that it is fresh, not
// that it is blank.) See [opIDFor], which had the same defect on the same day.
func callKey(actor Actor) string {
	if key := actor.OperationSeed(); key != "" {
		return key
	}
	return "operator-" + uuid.NewString()
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
	name := strings.TrimSpace(argString(args, "handle"))
	if name == "" {
		return failed("Name whose inbox to read with `handle`."), nil
	}
	handle, refusal := t.deps.personRecord(ctx, tracker.WorkInboxTool,
		authz.ActionInboxRead, name, readAsNamed)
	if refusal != nil {
		return *refusal, nil
	}
	q := tracker.InboxQuery{
		Handle:         handle,
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
		return readFailed(tracker.WorkInboxTool, err), nil
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
