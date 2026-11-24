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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// applyUnitRooms gives every unit that DECLARES a `channel` its room, staffed
// with its subtree's seats and lead; nothing else keeps a room to the chart.
// Every node runs it, under no duty lease: a create's subject is the channel
// name, so the broker settles a race ([applyUnitRoom]), where a lease would
// hold every team room on one node. Like [Engine.applyChart] it runs before
// the mailboxes open (a released inbox reads its team's room) and stamps every
// write with c.ActivatedAt, so a stale node cannot overwrite a newer chart and
// a reapply writes nothing. Each create is a fresh operation: one derived from
// the activation would carry its instant, and once the ledger swept past it a
// create that never landed would read `unknown` for good. A room is emptied,
// never removed: its transcript is the only copy ([applyChartRooms]). A node
// in a maintenance mode gets no [Engine.ChatStore], so it writes no room.
func (e *Engine) applyUnitRooms(ctx context.Context, c *Company) {
	rooms := e.ChatStore()
	if rooms == nil || c == nil {
		return
	}
	reportUnitRooms(ctx, applyCompanyRooms(ctx, rooms,
		unitRoomRows{db: e.domainEstate()}, c, c.ActivatedAt))
}

// applyCompanyRooms is the reconcile's two gates over [applyChartRooms]: a
// company on Chatlet, and an activation to stamp with. A function over its
// seams so both gates are tested with a writer that records every call.
//
// Only on Chatlet ([Engine.chatRetentionPolicy]): the chat store is built once
// per process, so without this gate a company moved to `chat.backend: vendor`
// or `none` would keep writing unit rooms onto a log nothing reads and no
// prune duty trims. Only with an activation, for [Engine.applyChart]'s reason:
// the store refuses a zero instant ([chatlet.ErrNoActivation]), so reaching it
// would log an ERROR per unit at every unactivated boot.
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
	// The engine narrating, not a seat or an operator ([chatlet.AuthorSystem]):
	// either would make it a participant in a conversation it only shaped.
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
// Every failure is an ERROR, and one unit's failure never stops the next
// unit's room: they are independent records on independent subjects. A room
// already right logs nothing, and a re-stamp (every activation moves every
// stamp, re-activating an unchanged revision included) is only DEBUG: nobody
// moved, but a stamp is worth finding when a room's activation is the question.
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
// line with the chart, empties every live unit room the chart no longer names,
// and reports what it did to each. A function over its seams, for
// [applyUnitRoom]'s reason.
//
// It walks [org.Unit.DeclaredChannel], never the effective [org.Unit.Channel]:
// an inheriting child's seats are already in its parent's room, and walking
// the effective field claims that room once per such child, each refused as a
// takeover ([liveChannels.UnitChannels] reads the same field). Two units
// declaring one channel are refused when the document is written (config's
// validateUnitRooms); an applied revision still carrying it has the second
// unit's refusal reported by [applyUnitRoom].
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

	// The chart is the only writer of a unit room's membership, so a room no
	// unit declares ([unitRoomOwner] answers nil) would keep its last members
	// for good, its FollowAll agents woken into a turn by every message. It is
	// emptied at this activation ([chatlet.Store.ReleaseUnitRoom]); the room
	// and its transcript stay, and declaring it again staffs it again.
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

// chatUnitActor is the name the engine writes unit-room changes under: a name
// no seat takes. [chatlet.AuthorSystem] is what renders it as a system line
// that wakes nobody.
const chatUnitActor = iam.EngineOrgChart

// unitRoomDid is what [applyUnitRoom] did to one declaring unit's room.
type unitRoomDid struct {
	created    bool
	members    int
	id         string
	membership chatlet.MembershipWrite

	// from is the unit the room was recorded under before a re-home.
	from string
}

// applyUnitRoom brings one unit's room into line with the chart. A function
// over its two seams rather than an engine method, so what it decides (a race
// lost to another node, an unknown create, a name somebody else holds) is
// tested without a broker or an engine.
//
// Not named `reconcile*`: [TestEveryVendorReconcilerRunsOnApply] derives the
// vendor reconcilers by that prefix and asserts [Engine.Apply] calls each, and
// this is neither a vendor wiring nor called by Apply.
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
			// A UNIT WITH NOBODY IN IT GETS NO ROOM: this kind refuses a
			// join, so an empty room is unreachable and would only claim
			// the name. A room that already exists is emptied below.
			return unitRoomDid{}, nil
		}
		written, createErr := rooms.CreateChannel(ctx, actor, chatlet.NewChannel{
			Name: unit.DeclaredChannel, Kind: chatlet.KindUnit, Unit: unit.Key(),
			Members: want, ActivatedAt: activatedAt,
		})
		switch {
		case createErr == nil && written.Outcome.Outcome == statelog.OutcomeUnknown:
			// NOT A CREATED ROOM: the create may or may not have landed,
			// so the next pass, which reads the row first, decides again.
			return unitRoomDid{}, fmt.Errorf("whether the room was created is "+
				"unknown (operation %s); the next config apply or restart "+
				"decides it again", written.Outcome.OpID)
		case createErr == nil:
			return unitRoomDid{created: true, members: len(want),
				id: written.Channel.ID}, nil
		case errors.Is(createErr, chatlet.ErrNameTaken):
			// ANOTHER NODE, or an earlier pass this node had not applied,
			// holds the name: the broker lets one create win, and the
			// refusal comes once this node has caught up with the winner.
			// So re-read the room and judge it like any existing one:
			// this unit's is success, anybody else's the takeover
			// refused below.
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

	// The room exists and may not be this unit's: [unitRoomOwner] decides,
	// the one rule the sweep and the answerer ask too. A unit room it gives
	// this unit is this unit's to staff, whatever unit it is recorded
	// under; the write below re-homes it.
	if held.kind != string(chatlet.KindUnit) {
		return unitRoomDid{}, heldElsewhere(held, unit, nil, name)
	}
	if owner := unitRoomOwner(chart, held.unit, name); owner != unit {
		return unitRoomDid{}, heldElsewhere(held, unit, owner, name)
	}

	// THE STORE DECIDES, inside its own snapshot, so nothing is compared
	// here: a room at an OLDER activation over the same set must still be
	// re-stamped, which a comparison here would skip. An unchanged room at
	// this activation publishes nothing; an EMPTY set is written like any
	// other. The unit is stated by its KEY, so the store re-homes a room
	// recorded under another spelling (a unit that gained an id, or renamed
	// only in case) to the key that survives its next rename.
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
// A room's name never changes (no rename in [chatlet.ChannelPatch], and an
// archived room keeps its name), so "rename the room" is never a remedy. A
// declared name is somebody else's in two ways: a room a person opened by
// hand, never taken over since that hands a public room's transcript to a
// membership the chart decides; or the room of another unit that declares it
// too, which config's validateUnitRooms refuses when a document is next
// written and a stored one still carries.
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

// unitRoomOwner is the unit this chart says a unit room belongs to, or nil
// when no unit declares it. First the unit its stored key names, if that unit
// still declares the room, so a room never moves while its unit keeps it;
// otherwise the FIRST unit declaring it, where a create would have put it, so
// a moved channel takes its room along ([applyUnitRoom] re-homes it), which
// discloses nothing: every seat of the company may read a unit room.
//
// The per-unit pass, the sweep and the answerer ([unitRoomLead]) all ask this
// one rule, so an emptied room wakes no lead and a post reaches a new owner's
// lead before the re-home lands. stored, a durable unit reference, resolves
// through [org.Organization.UnitByRef] (id or name, case-folded, id wins), so
// a case-only rename keeps its room. room is the room's NORMALISED name.
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
// [unitRoomOwner] gives it to, or empty where none does. It is what
// [chatlet.Roster.Lead] answers, put on a person's post as the room's
// answerer ([chatlet.ReasonAnswerer]).
func unitRoomLead(chart *org.Organization, stored, room string) string {
	return leadOfUnit(chart, unitRoomOwner(chart, stored, chatlet.NormalizeName(room)))
}

// unitRoomMembers is who belongs in a unit's room: every seat in its subtree,
// plus the lead that answers for it. The subtree, because a division's room is
// where its teams are reachable.
//
// FollowAll (every message wakes them, [chatlet.Member.FollowAll]) is set for
// this unit's OWN agent seats only. A descendant's agents, an inheriting
// child's included, are members reached by a mention, a thread, `@channel` or
// the lead, or one message in a division's room would be a turn, and a box,
// for every agent beneath it. A human is never woken: a person runs no turn.
//
// Deduplicated (the lead is nearly always in the subtree) and SORTED, so every
// node writes the same bytes; whether the set moved is the store's comparison.
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

// unitRoomRows reads the replicated rows this reconcile compares against, with
// its own SQL for [chatRoomRows]'s reason: the chat reader filters by what a
// viewer may see, and this is the engine asking what is there.
type unitRoomRows struct{ db store.ReplicatedReader }

// heldRoom is a room as the reconcile needs it: which room holds the name, and
// whose it is. Membership is compared inside the store
// ([chatlet.Store.SetUnitMembers]).
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

// unitRooms is every unit room that is not archived: an archived room takes
// no message and so wakes nobody, and emptying one would publish a record no
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
