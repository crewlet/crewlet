package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE UNIT ROOMS, which are the one kind of channel the ENGINE fills.
//
// # What this is for
//
// A unit that DECLARES a `channel` in the org chart gets that room, with its
// subtree's seats and its lead in it, and nothing else keeps a room's members
// to the chart: `crewlet validate` says so to a Chatlet company that
// declares none ("no team here has a room of its own until somebody opens one
// by hand"), and it was the only claim about chat in this tree that nothing
// performed.
//
// # Why it runs on every node rather than under a duty lease
//
// Because the broker already arbitrates it, which is cheaper than a lease and
// strictly safer. A create takes the channel NAME as its subject, so two nodes
// reconciling the same unit contend at the broker and exactly one wins; the
// loser is told the name is claimed, which is this function's success case and
// not an error. A membership write arbitrates on the room. So the worst a
// second node costs is a refused record, and the worst a LEASE would cost is
// the whole company's team rooms waiting on whichever node held it.
//
// It is the shape [Engine.applyChart] already uses for the same job — the org
// chart projected into a domain's own objects, following every published
// company ([Engine.followCompany]), best effort, retried by the next one — and
// it runs beside it, before the mailboxes are let through, because a released
// inbox runs a turn that reads its team's room.
//
// # Why every create is a fresh operation
//
// On [Engine.applyChart]'s and [Engine.applyContainers]' reasoning: this is a
// reconcile decided from the rows, so a second call — on another node, at the
// next boot, after a lost acknowledgement — finds the room there and decides
// nothing, and N nodes creating one room in one apply are settled by the
// broker. Deriving the create from the activation instead, so the N creates
// were ONE operation the ledger collapses, would stamp it with the
// ACTIVATION's instant: once the operation ledger had swept past an activation
// — thirty days on, for a company that has not been reconfigured since — a
// room whose create never landed would be answered `unknown` without being
// published, on every node and at every apply, for the life of that
// activation. A fresh operation is minted now and is never that old.
//
// # Stamped with the ACTIVATION, on the chart's terms
//
// c.ActivatedAt is the instant the fleet activated c, and every unit room's
// create and membership is stamped with it exactly as [Engine.applyChart]
// stamps a project and [Engine.applyContainers] a knowledge container — for
// the same two reasons: a node that boots on a revision the fleet has since
// replaced must not write its own chart's membership back over the current
// one, and a reapply of one activation must write nothing. A company no
// activation has named yet is not applied here either; its first activation
// is. See [chatlet.Store.SetUnitMembers].
//
// # A room follows its channel
//
// A room records the unit it was made for, and the CHANNEL is what the chart
// gives a unit — so when the chart gives a channel to another unit (moved to
// the team that replaced its own in a reorg), the room goes with it: the next
// activation re-homes it to the unit that declares it now, staffed with that
// unit's people, in the same record that sets its membership
// ([chatlet.Store.SetUnitMembers]). A room never moves
// while the unit it is recorded under still declares it, so two units
// declaring one channel in an applied revision leave it where it is. The room's
// transcript moves with it and nothing is disclosed: a unit room is readable
// by every seat of the company. Without this a validated document — a
// channel moved to another unit — refused its new declarer the room on every
// apply of every data node for good, and emptied the room besides.
//
// # What it does NOT do
//
// It never removes a room. A unit that drops its `channel`, or is deleted
// outright, keeps what was said in it: a transcript is the only copy of what
// people said, and an org chart edit is not a request to destroy one. What
// the chart DOES own is the room's membership, and a room it no longer names
// is staffed by nobody — so its members are taken out (see
// [applyChartRooms]) while every word of it stays readable, and an operator
// archives it if they want it closed.
//
// # Behind the mode gate, and Chatlet's alone
//
// The writer is [Engine.ChatStore], which is [Engine.writeSide]'s: a node in a
// maintenance mode hands out none, so a boot or an apply there writes no room
// into a log whose usage the window is measuring. And the company has to be
// on Chatlet — see [applyCompanyRooms].
func (e *Engine) applyUnitRooms(ctx context.Context, c *Company) {
	rooms := e.ChatStore()
	if rooms == nil || c == nil {
		return
	}
	reportUnitRooms(ctx, applyCompanyRooms(ctx, rooms,
		unitRoomRows{db: e.domainEstate()}, c, c.ActivatedAt))
}

// applyCompanyRooms is the reconcile's two gates over [applyChartRooms]: a
// company whose chat is the engine's own, and an activation to stamp with.
//
// A FUNCTION OVER ITS SEAMS for [applyUnitRoom]'s reason, and so that both
// gates are exercised with a writer that records every call.
//
// # Only on Chatlet
//
// [Engine.chatRetentionPolicy] states the rule for this domain: a company
// that moved to a vendor surface publishes nothing, because its rooms are
// another product's archive now. The chat store is built once per process,
// from the first company that ran Chatlet, so without this gate a
// revision moving to `chat.backend: vendor` or `none` went on creating and
// re-stamping unit rooms on every boot and every apply of every data node —
// onto a log nothing reads any more and whose prune duty has stopped.
//
// # And only with an activation
//
// A company no activation has named is not applied: its first activation is,
// for [Engine.applyChart]'s reason. The store refuses a zero instant
// ([chatlet.ErrNoActivation]) rather than guessing one, so reaching it would log
// one ERROR per unit at every unactivated boot.
func applyCompanyRooms(ctx context.Context, rooms unitRoomWriter, rows unitRoomReader,
	c *Company, activatedAt time.Time) []unitRoomResult {

	if c == nil || c.Org == nil || c.Config == nil ||
		c.Config.ChatBackendFor() != config.ChatChatlet {
		return nil
	}
	if activatedAt.IsZero() {
		log.DebugContext(ctx, "chat_unit_rooms_not_activated",
			"detail", "this company was not built from an activation, so its "+
				"unit rooms wait for the reconcile that activates it")
		return nil
	}
	// THE ENGINE NARRATING, not a seat and not an operator token. See
	// [chatlet.AuthorSystem]: attributing this to either would make the
	// engine a participant in a conversation it only shaped.
	actor := chatlet.Actor{Handle: chatUnitActor, Kind: chatlet.AuthorSystem}
	return applyChartRooms(ctx, rooms, rows, actor, c.Org, activatedAt)
}

// reportUnitRooms logs what one pass did, one line per room that changed or
// could not be brought into line — see [unitRoomEvent] for which.
func reportUnitRooms(ctx context.Context, results []unitRoomResult) {
	for _, r := range results {
		level, event := unitRoomEvent(r)
		if event == "" {
			continue
		}
		attrs := []any{"unit", r.unit, "channel", r.channel}
		switch {
		case r.err != nil:
			attrs = append(attrs, "error", r.err.Error(),
				"detail", "the unit's room is missing or its membership is "+
					"stale; the next config apply or restart retries it")
		case r.created:
			attrs = append(attrs, "members", r.members, "id", r.id)
		case r.released:
			attrs = append(attrs, "detail", "no unit in the chart declares "+
				"this room any more, so nobody is in it; what was said in it "+
				"is kept")
		case r.membership == chatlet.MembershipRehomed:
			attrs = append(attrs, "from", r.from, "members", r.members,
				"detail", "the chart gives this channel to another unit now, "+
					"so the room and what was said in it moved there")
		case r.membership == chatlet.MembershipMoved:
			attrs = append(attrs, "members", r.members)
		}
		log.Log(ctx, level, event, attrs...)
	}
}

// unitRoomEvent is the line one room's result is logged as, or "" for none.
//
// EVERY FAILURE, AT ERROR, and BEST EFFORT PER UNIT: one unit's failure must
// not stop the next one's room — they are independent records on independent
// subjects, and a company whose first unit hit a conflict would otherwise
// have no rooms at all.
//
// NOTHING AT INFO FOR A ROOM THAT WAS ALREADY RIGHT, which is every room of
// every boot of a company nobody edited — and a RE-STAMP is that too: every
// activation moves every unit room's stamp on the node that wins it,
// re-activating an unchanged revision (the credential-rotation gesture)
// included, and nobody moved. It is a DEBUG line, because a stamp that moved
// is still worth finding when a room's activation is the question.
func unitRoomEvent(r unitRoomResult) (slog.Level, string) {
	switch {
	case r.err != nil:
		return slog.LevelError, "chat_unit_room_not_reconciled"
	case r.created:
		return slog.LevelInfo, "chat_unit_room_created"
	case r.membership == chatlet.MembershipRehomed:
		return slog.LevelInfo, "chat_unit_room_rehomed"
	case r.membership == chatlet.MembershipMoved && r.released:
		return slog.LevelInfo, "chat_unit_room_released"
	case r.membership == chatlet.MembershipMoved:
		return slog.LevelInfo, "chat_unit_room_membership_set"
	case r.membership == chatlet.MembershipRestamped:
		return slog.LevelDebug, "chat_unit_room_restamped"
	}
	return 0, ""
}

// unitRoomResult is what one pass did to one unit room, or why it could not.
type unitRoomResult struct {
	unit, channel string

	// created is a room this pass opened, with members in it and the
	// id the store gave it.
	created bool
	members int
	id      string

	// membership is what a membership write did to a room that was
	// already there ([chatlet.MembershipWrite]); unchanged on a create.
	membership chatlet.MembershipWrite

	// released is a room no unit in the chart declares any more, whose
	// membership the pass took to nobody.
	released bool

	// from is the unit a re-homed room was recorded under before this pass
	// ([chatlet.MembershipRehomed]), as this node read it.
	from string

	err error
}

// applyChartRooms brings the room of every unit that DECLARES a channel into
// line with the chart, takes everybody out of a unit room the chart no longer
// names, and reports what it did to each.
//
// A FUNCTION OVER ITS TWO SEAMS, for [applyUnitRoom]'s reason: which units
// get a room is decided here, and it is exercised without a broker.
//
// # The DECLARED channel, never the effective one
//
// [org.Unit.Channel] is what a unit talks in AFTER the cascade, and a child
// that names no channel inherits its parent's ([org.Organization.Normalize]).
// So iterating the effective field hands the reconcile the parent's room once
// for every descendant that wrote nothing: whichever unit the walk reached
// first created it under its OWN key and subtree, and every other one then
// found a room belonging to somebody else and was refused as a takeover —
// an ERROR line per inheriting unit on every boot and every apply, for a
// chart that is exactly what its author meant. A room belongs to the unit
// that names it, and [org.Unit.DeclaredChannel] is that name; an inheriting
// child's seats are in the room already, as the parent's subtree.
// [liveChannels.UnitChannels] reads the same field for the same reason. Two
// units DECLARING one channel are refused when the document is written
// (config's validateUnitRooms); an applied revision carrying that shape — an
// admission rule's violation still runs — has its second unit reported here,
// by [applyUnitRoom].
//
// # Whose a room is, is [org.Organization.UnitByRef]'s answer
//
// A room records its unit's KEY at the create — the id where the unit has
// one, the name where it does not — and that string is read back here at
// every later activation, so it is a DURABLE unit reference, and every one of
// those goes through the chart's one resolver: it matches an id or a name,
// folds case the way the admission rule claims a key, and lets an id win an
// ambiguity. A private exact match here used to disagree with it — and with
// the room's own lead fallback, which already resolved through it — so a
// unit whose name changed only in case (the same team, to the chart) was
// refused its own room and had it emptied on every pass, and a stored chart
// where one unit's id is another's name had the per-unit pass staff a room
// the sweep then emptied, both at one activation.
//
// # A room the chart no longer names is staffed by nobody
//
// The chart is the only writer of a unit room's membership — every other
// membership gesture is refused for this kind — so a room whose unit stopped
// declaring it, or left the chart, kept its last members for good: agents
// with FollowAll among them, woken into a coding run by every message posted
// there, in a room no prompt names as theirs and nothing else
// could ever change. So every unit room NO unit in the chart declares —
// [unitRoomOwner] answers nil, and a room another unit declares has followed
// its channel there above — is emptied at this activation, under whatever
// unit it is recorded under ([chatlet.Store.ReleaseUnitRoom]). The room and its
// transcript stay; its people are what the chart says, which is nobody.
// Declaring it again, on any unit, staffs it again.
func applyChartRooms(ctx context.Context, rooms unitRoomWriter, rows unitRoomReader,
	actor chatlet.Actor, chart *org.Organization, activatedAt time.Time) []unitRoomResult {

	var out []unitRoomResult
	for unit := range chart.AllUnits() {
		name := chatlet.NormalizeName(unit.DeclaredChannel)
		if name == "" {
			continue
		}
		did, err := applyUnitRoom(ctx, rooms, rows, actor, chart, unit, name, activatedAt)
		out = append(out, unitRoomResult{unit: unit.Name, channel: name,
			created: did.created, members: did.members, id: did.id,
			membership: did.membership, from: did.from, err: err})
	}

	held, err := rows.unitRooms(ctx)
	if err != nil {
		return append(out, unitRoomResult{err: fmt.Errorf(
			"list the unit rooms to find any the chart no longer names: %w", err)})
	}
	for _, room := range held {
		if unitRoomOwner(chart, room.unit, room.name) != nil {
			continue
		}
		written, wrote, err := rooms.ReleaseUnitRoom(ctx, actor, room.id, activatedAt)
		result := unitRoomResult{unit: room.unit, channel: room.name, released: true,
			membership: wrote}
		switch {
		case err != nil:
			result.err = fmt.Errorf("take everybody out of a room no unit "+
				"declares any more: %w", err)
		case written.Outcome.Outcome == statelog.OutcomeUnknown:
			result.err = fmt.Errorf("whether a room no unit declares any more "+
				"was emptied is unknown (operation %s); the next config apply or "+
				"restart decides it again", written.Outcome.OpID)
		}
		out = append(out, result)
	}
	return out
}

// chatUnitActor is the name the engine writes unit-room changes under.
//
// A NAME RATHER THAN A HANDLE, and it is not a seat: [chatlet.AuthorSystem] is
// what makes it render as a system line and wake nobody, and the string is
// what a reader sees beside it.
const chatUnitActor = "org-chart"

// unitRoomDid is what [applyUnitRoom] did to one declaring unit's room.
type unitRoomDid struct {
	created    bool
	members    int
	id         string
	membership chatlet.MembershipWrite

	// from is the unit the room was recorded under before a re-home.
	from string
}

// applyUnitRoom brings one unit's room into line with the chart.
//
// A FUNCTION OVER ITS TWO SEAMS rather than a method on the engine, so what it
// decides — a race lost to another node, an unknown create, a name somebody
// else holds — is exercised without a broker or an engine.
//
// NOT `reconcileUnitRoom`, and the prefix is the reason: `reconcile*` is a
// RESERVED VOCABULARY here — [TestEveryVendorReconcilerRunsOnApply] derives
// the vendor reconcilers from the source by that prefix and asserts
// [Engine.Apply] calls each one, because a third-party wiring that is not
// rebuilt keeps its boot-time org chart and a credential the revision may have
// rotated. This is neither a vendor wiring nor called by Apply directly, so
// naming it `reconcile*` would have diluted what that list means.
func applyUnitRoom(ctx context.Context, rooms unitRoomWriter,
	rows unitRoomReader, actor chatlet.Actor, chart *org.Organization,
	unit *org.Unit, name string, activatedAt time.Time) (unitRoomDid, error) {

	want := unitRoomMembers(chart, unit)
	held, found, err := rows.room(ctx, name)
	if err != nil {
		return unitRoomDid{}, fmt.Errorf("read the room named %q: %w", name, err)
	}
	if !found {
		if len(want) == 0 {
			// A UNIT WITH NOBODY IN IT GETS NO ROOM CREATED. An empty
			// room is not the same as a room somebody may join later
			// — this kind refuses a join — so it would be a room
			// nothing could reach, and creating it would claim the
			// name against the day the unit is staffed. A room that
			// already EXISTS is another matter: it is emptied below,
			// because the chart says nobody is in it.
			return unitRoomDid{}, nil
		}
		written, createErr := rooms.CreateChannel(ctx, actor, chatlet.NewChannel{
			Name: unit.DeclaredChannel, Kind: chatlet.KindUnit, Unit: unit.Key(),
			Members: want, ActivatedAt: activatedAt,
		})
		switch {
		case createErr == nil && written.Outcome.Outcome == statelog.OutcomeUnknown:
			// NOT A CREATED ROOM. The create may have landed and it may
			// not, so the next pass — which reads the row first — is
			// what decides it again.
			return unitRoomDid{}, fmt.Errorf("whether the room was created is "+
				"unknown (operation %s); the next config apply or restart "+
				"decides it again", written.Outcome.OpID)
		case createErr == nil:
			return unitRoomDid{created: true, members: len(want),
				id: written.Channel.ID}, nil
		case errors.Is(createErr, chatlet.ErrNameTaken):
			// ANOTHER NODE GOT THERE — every node runs this on every
			// apply, and the broker lets exactly one create take the
			// name — or an earlier pass did and this node had not
			// applied it when it read. The refusal is decided against
			// this node's own rows once it has caught up with the
			// winner, so the room that holds the name is read again
			// here and judged like any room that was already there:
			// this unit's is this function working, and anybody
			// else's is the takeover the chart refuses below.
			held, found, err = rows.room(ctx, name)
			switch {
			case err != nil:
				return unitRoomDid{}, fmt.Errorf("read the room that took the "+
					"name %q: %w", name, err)
			case !found:
				return unitRoomDid{}, fmt.Errorf("the name %q is claimed and "+
					"this node holds no room by it — it was taken and then "+
					"renamed or removed since; the next config apply or restart "+
					"reads it again", name)
			}
		default:
			return unitRoomDid{}, fmt.Errorf("create the room: %w", createErr)
		}
	}

	// THE ROOM EXISTS AND MAY NOT BE THIS UNIT'S — by [unitRoomOwner],
	// the one rule the sweep and the room's lead fallback ask too. A unit
	// room it names THIS unit for is this unit's to staff, whoever it is
	// recorded under: a room its recorded unit no longer declares follows
	// its channel here, and the write below re-homes it.
	if held.kind != string(chatlet.KindUnit) {
		return unitRoomDid{}, heldElsewhere(held, unit, nil, name)
	}
	if owner := unitRoomOwner(chart, held.unit, name); owner != unit {
		return unitRoomDid{}, heldElsewhere(held, unit, owner, name)
	}

	// THE STORE DECIDES, INSIDE ITS OWN SNAPSHOT, and this does not
	// compare first: whether the set moved is only half the question, the
	// other half being whether the room already carries this activation —
	// and a room at an OLDER one over the same set is re-stamped, which a
	// comparison here would skip. An unchanged room at this activation
	// publishes nothing, which is every boot of every node on a company
	// nobody edited. An EMPTY set is written like any other: a unit whose
	// last seat left keeps its room and loses its members.
	//
	// AND THE UNIT IS STATED BY ITS KEY, so the store re-homes a room
	// recorded under any other spelling — a unit that gained an id, or
	// one whose name changed only in case, is recorded under the key that
	// survives its next rename.
	written, wrote, err := rooms.SetUnitMembers(ctx, actor, held.id, unit.Key(),
		want, activatedAt)
	switch {
	case err != nil:
		return unitRoomDid{}, fmt.Errorf("set the membership: %w", err)
	case written.Outcome.Outcome == statelog.OutcomeUnknown:
		return unitRoomDid{}, fmt.Errorf("whether the membership was set is "+
			"unknown (operation %s); the next config apply or restart decides "+
			"it again", written.Outcome.OpID)
	}
	did := unitRoomDid{members: len(want), membership: wrote}
	if wrote == chatlet.MembershipRehomed {
		did.from = held.unit
	}
	return did, nil
}

// heldElsewhere is why the room a unit declares is not its to maintain, with
// the remedy that works for THAT reason. owner is the unit [unitRoomOwner]
// gives a unit room to, and nil for a room of another kind.
//
// A ROOM'S NAME NEVER CHANGES — this build has no rename ([chatlet.ChannelPatch])
// — and an archived room keeps its name, so "rename the room" is never a
// remedy. A unit room that the unit it is recorded under no longer declares
// is not here at all: it follows its channel to this unit ([applyUnitRoom]).
// What is left are the two ways a declared name is somebody else's:
//
//   - a room a person opened by hand: taking it over would hand a public
//     room's transcript to a membership the chart decides, which is a
//     disclosure rather than a reconcile;
//   - the room of another unit that declares it too — the unit it is
//     recorded under, or the first in the chart that declares it — which a
//     document is refused for when it is next written (config's
//     validateUnitRooms) and a stored one still carries.
func heldElsewhere(held heldRoom, unit, owner *org.Unit, name string) error {
	if held.kind != string(chatlet.KindUnit) || owner == nil {
		return fmt.Errorf("#%s is a %s room somebody opened, so the chart will "+
			"not take it over, and a room's name never changes — give unit %q "+
			"a channel nobody holds", name, held.kind, unit.Name)
	}
	return fmt.Errorf("#%s is the room of unit %q, which declares it too — "+
		"on Chatlet a room belongs to one unit, and a document naming "+
		"one channel on two units is refused when it is next written; give "+
		"%q a channel of its own, or leave it empty to inherit its parent's",
		name, owner.Name, unit.Name)
}

// unitRoomOwner is the unit this chart says a unit room belongs to, or nil for
// a room it staffs with nobody.
//
// TWO STEPS, IN THIS ORDER:
//
//   - the unit its stored key names, if that unit still declares the room —
//     so a room never moves while the unit it is recorded under keeps it,
//     and two units declaring one channel in an applied revision leave it
//     where it is;
//   - otherwise the FIRST unit in the chart that declares it, which is the
//     unit a create would have given the room to had it not existed — so a
//     channel moved to another unit takes its room with it ([applyUnitRoom]
//     re-homes it), and the room ends up where a fresh reconcile would have
//     put it.
//
// nil only when no unit in the chart declares the room at all.
//
// ONE RULE, ASKED BY EVERYTHING THAT DECIDES ON A ROOM'S OWNER: the per-unit
// pass staffing a room, the sweep emptying one, and the room's lead fallback
// ([unitRoomLead]). The sweep used to decide "nobody's" while the fallback
// still resolved the stored unit's lead regardless, so a unit that dropped
// its channel had its room emptied and its lead woken — under a reason that
// obliges an answer — by every message a person posted there, from a room no
// prompt names as theirs: the very cost the sweep exists to remove. And the
// fallback asking the second step too is what has a person's post reach the
// new owner's lead before this node's re-home has landed.
//
// The stored key goes through [org.Organization.UnitByRef], the chart's one
// resolver of a durable unit reference — see [applyChartRooms]. room is the
// room's NORMALISED name, which is how a declared channel is compared.
func unitRoomOwner(chart *org.Organization, stored, room string) *org.Unit {
	if chart == nil {
		return nil
	}
	if owner := chart.UnitByRef(stored); owner != nil &&
		chatlet.NormalizeName(owner.DeclaredChannel) == room {
		return owner
	}
	for unit := range chart.AllUnits() {
		if chatlet.NormalizeName(unit.DeclaredChannel) == room {
			return unit
		}
	}
	return nil
}

// unitRoomLead is who answers for a unit's room: the lead of the unit
// [unitRoomOwner] says it belongs to, or empty where none does.
//
// IT IS WHAT [chatlet.Roster.Lead] ANSWERS, and the chat write path puts it on
// the record a person's post publishes, where the lead fallback reads it.
func unitRoomLead(chart *org.Organization, stored, room string) string {
	return leadOfUnit(chart, unitRoomOwner(chart, stored, chatlet.NormalizeName(room)))
}

// unitRoomMembers is who belongs in a unit's room: every seat in its subtree,
// plus the lead that answers for it.
//
// THE SUBTREE RATHER THAN THE DIRECT MEMBERS, because a division's room is
// where its teams are reachable — the alternative leaves a lead talking to an
// empty room while the people doing the work sit one level down.
//
// FollowAll IS SET FOR THIS UNIT'S OWN AGENT SEATS AND NOBODY ELSE'S, which is
// the rule [chatlet.Member.FollowAll] states: every message in the room wakes
// them. A DESCENDANT'S agents are members and not followers — and that holds
// for a child that INHERITS this channel too, whose prompt names this room as
// its team's: its agents are reached here by a mention, a thread they are in,
// `@channel` or the lead, and the room's own seats route the rest down the
// chart as they would any other ask. A HUMAN is never woken by anything,
// because a person is addressable and runs no turn. Without that split, one
// message in a division's room would be a turn for every agent beneath it —
// every inheriting team's included, since a channel cascades down a whole
// branch — each its own coding run in a box.
//
// DEDUPLICATED, because the lead is nearly always also a member of the subtree
// and the record names each member once; SORTED so the record is the same
// bytes on every node that writes it. Whether the set moved is the store's
// comparison, inside its own snapshot ([chatlet.Store.SetUnitMembers]).
func unitRoomMembers(chart *org.Organization, unit *org.Unit) []chatlet.Member {
	direct := map[string]bool{}
	for _, r := range unit.Roles {
		if r != nil {
			direct[r.Handle()] = r.Kind != org.KindHuman
		}
	}
	seen := map[string]chatlet.Member{}
	add := func(handle string, followAll bool) {
		if handle == "" {
			return
		}
		m, held := seen[handle]
		if !held {
			m = chatlet.Member{Handle: handle}
		}
		// ONCE TRUE, STAYS TRUE: a seat reached by two paths keeps the
		// stronger arrangement rather than whichever path came last.
		m.FollowAll = m.FollowAll || followAll
		seen[handle] = m
	}
	for r := range unit.AllRoles() {
		if r == nil {
			continue
		}
		handle := r.Handle()
		add(handle, direct[handle])
	}
	if lead := chart.EffectiveLead(unit); lead != nil {
		handle := lead.Handle()
		add(handle, direct[handle])
	}
	out := make([]chatlet.Member, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b chatlet.Member) int {
		return strings.Compare(a.Handle, b.Handle)
	})
	return out
}

// unitRoomWriter is what the unit-room reconcile writes through: the chat
// store's three gestures it makes, declared by their consumer.
type unitRoomWriter interface {
	CreateChannel(ctx context.Context, actor chatlet.Actor, in chatlet.NewChannel) (chatlet.Written, error)
	SetUnitMembers(ctx context.Context, actor chatlet.Actor, channelID, unit string,
		members []chatlet.Member, activatedAt time.Time) (chatlet.Written, chatlet.MembershipWrite, error)
	ReleaseUnitRoom(ctx context.Context, actor chatlet.Actor, channelID string,
		activatedAt time.Time) (chatlet.Written, chatlet.MembershipWrite, error)
}

// unitRoomReader is the two reads the reconcile decides from: the room that
// holds a name, and every live unit room, so one the chart no longer names is
// found.
type unitRoomReader interface {
	room(ctx context.Context, name string) (heldRoom, bool, error)
	unitRooms(ctx context.Context) ([]heldRoom, error)
}

// unitRoomRows reads the replicated rows this reconcile compares against.
//
// ITS OWN SQL rather than the chat reader's, for [chatRoomRows]'s reason: a
// reader answers a VIEWER's question and filters by what that viewer may see,
// and this is the engine asking what is there.
type unitRoomRows struct{ db store.ReplicatedReader }

// heldRoom is a room as the reconcile needs it: which room holds the name, and
// whose it is. Its membership is not here — the store compares that inside
// its own snapshot ([chatlet.Store.SetUnitMembers]).
type heldRoom struct {
	id   string
	name string
	kind string
	unit string
}

// room finds a channel by its normalised name.
func (r unitRoomRows) room(ctx context.Context, name string) (heldRoom, bool, error) {
	var out heldRoom
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT id, name_norm, kind, unit FROM chat_channels WHERE name_norm = ?`,
			name).Scan(&out.id, &out.name, &out.kind, &out.unit)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return heldRoom{}, false, nil
	case err != nil:
		return heldRoom{}, false, err
	}
	return out, true, nil
}

// unitRooms is every unit room that is not archived.
//
// NOT ARCHIVED, because an archived room takes no message and so wakes
// nobody: emptying one would be a record on the log that changes what no
// reader can notice, once per activation.
func (r unitRoomRows) unitRooms(ctx context.Context) ([]heldRoom, error) {
	var out []heldRoom
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, name_norm, kind, unit FROM chat_channels
			  WHERE kind = ? AND archived_at IS NULL
			  ORDER BY name_norm`, string(chatlet.KindUnit))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var room heldRoom
			if err := rows.Scan(&room.id, &room.name, &room.kind, &room.unit); err != nil {
				return err
			}
			out = append(out, room)
		}
		return rows.Err()
	})
	return out, err
}
