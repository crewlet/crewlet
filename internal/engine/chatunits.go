package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// THE UNIT ROOMS, which are the one kind of channel the ENGINE fills.
//
// # What this is for
//
// A unit that names a `channel` in the org chart gets that room, with its
// subtree's seats and its lead in it. Nothing else opens one: `crewlet
// validate` says so to a native-chat company that declares none ("no team here
// has anywhere to talk to itself"), and it was the only claim about chat in
// this tree that nothing performed.
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
// chart projected into a domain's own objects, at boot and on every apply,
// best effort, retried by the next one.
//
// # What it does NOT do
//
// It never removes a room. A unit that drops its `channel`, or is deleted
// outright, keeps what was said in it: a transcript is the only copy of what
// people said, and an org chart edit is not a request to destroy one. The room
// simply stops being maintained, and an operator archives it if they want it
// gone. That asymmetry is deliberate and is why this is not a full
// reconciliation in both directions.
func (e *Engine) applyUnitRooms(ctx context.Context, c *Company) {
	rooms := e.ChatStore()
	if rooms == nil || c == nil || c.Org == nil || e.backends == nil {
		return
	}
	// THE ENGINE NARRATING, not a seat and not an operator token. See
	// [chat.AuthorSystem]: attributing this to either would make the
	// engine a participant in a conversation it only shaped.
	actor := chat.Actor{Handle: chatUnitActor, Kind: chat.AuthorSystem}
	rows := unitRoomRows{db: e.backends.Store}

	for unit := range c.Org.AllUnits() {
		name := chat.NormalizeName(unit.Channel)
		if name == "" {
			continue
		}
		if err := e.applyUnitRoom(ctx, rooms, rows, actor, c.Org, unit, name); err != nil {
			// BEST EFFORT, PER UNIT. One unit's failure must not stop
			// the next one's room: they are independent records on
			// independent subjects, and a company whose first unit
			// hit a conflict would otherwise have no rooms at all.
			log.ErrorContext(ctx, "chat_unit_room_not_reconciled",
				"unit", unit.Name, "channel", name, "error", err.Error(),
				"detail", "the unit's room is missing or its membership is "+
					"stale; the next config apply or restart retries it")
		}
	}
}

// chatUnitActor is the name the engine writes unit-room changes under.
//
// A NAME RATHER THAN A HANDLE, and it is not a seat: [chat.AuthorSystem] is
// what makes it render as a system line and wake nobody, and the string is
// what a reader sees beside it.
const chatUnitActor = "org-chart"

// applyUnitRoom brings one unit's room into line with the chart.
//
// NOT `reconcileUnitRoom`, and the prefix is the reason: `reconcile*` on
// *Engine is a RESERVED VOCABULARY here — [TestEveryVendorReconcilerRunsOnApply]
// derives the vendor reconcilers from the source by that prefix and asserts
// [Engine.Apply] calls each one, because a third-party wiring that is not
// rebuilt keeps its boot-time org chart and a credential the revision may have
// rotated. This is neither a vendor wiring nor called by Apply directly, so
// naming it `reconcile*` would have put it in that list and diluted what the
// list means — the gate said so, which is the gate working.
func (e *Engine) applyUnitRoom(ctx context.Context, rooms *chat.Store,
	rows unitRoomRows, actor chat.Actor, chart *org.Organization,
	unit *org.Unit, name string) error {

	want := unitRoomMembers(chart, unit)
	if len(want) == 0 {
		// A UNIT WITH NOBODY IN IT GETS NO ROOM. An empty room is not
		// the same as a room somebody may join later — this kind
		// refuses a join — so it would be a room nothing could ever
		// reach, and creating it would claim the name against the day
		// the unit is staffed.
		return nil
	}

	held, found, err := rows.room(ctx, name)
	if err != nil {
		return fmt.Errorf("read the room named %q: %w", name, err)
	}
	if !found {
		written, err := rooms.CreateChannel(ctx, actor, chat.NewChannel{
			Name: unit.Channel, Kind: chat.KindUnit, Unit: unit.Key(),
			Members: want,
		})
		switch {
		case err == nil:
			log.InfoContext(ctx, "chat_unit_room_created",
				"unit", unit.Name, "channel", name,
				"members", len(want), "id", written.Channel.ID)
			return nil
		case errors.Is(err, chat.ErrConflict):
			// ANOTHER NODE GOT THERE, or an earlier tick did and this
			// node has not applied it yet. Both are this function
			// working: the name is claimed by the room it was
			// supposed to claim it for. The membership is the
			// winner's to maintain and the next pass sees the row.
			return nil
		default:
			return fmt.Errorf("create the room: %w", err)
		}
	}

	// THE ROOM EXISTS AND MAY NOT BE THIS UNIT'S. A name is the company's
	// one address space, so a unit can name a channel somebody already made
	// by hand. Taking it over would hand a public room's transcript to a
	// membership the chart decides, which is a disclosure rather than a
	// reconcile — so it is REPORTED and left alone.
	if held.kind != string(chat.KindUnit) || !sameUnit(held.unit, unit) {
		return fmt.Errorf("#%s is a %q room belonging to %q, so the chart "+
			"will not take it over — rename the unit's channel or the room",
			name, held.kind, held.unit)
	}

	if slices.EqualFunc(want, held.members, sameMember) {
		return nil
	}
	if _, err := rooms.SetUnitMembers(ctx, actor, held.id, want); err != nil {
		return fmt.Errorf("set the membership: %w", err)
	}
	log.InfoContext(ctx, "chat_unit_room_membership_set",
		"unit", unit.Name, "channel", name,
		"members", len(want), "was", len(held.members))
	return nil
}

// unitRoomMembers is who belongs in a unit's room: every seat in its subtree,
// plus the lead that answers for it.
//
// THE SUBTREE RATHER THAN THE DIRECT MEMBERS, because a division's room is
// where its teams are reachable — the alternative leaves a lead talking to an
// empty room while the people doing the work sit one level down.
//
// FollowAll IS SET FOR THIS UNIT'S OWN AGENT SEATS AND NOBODY ELSE'S, which is
// the rule [chat.Member.FollowAll] states: every message in the room wakes
// them. A descendant's agents are not woken by a parent unit's traffic — they
// have their own room — and a HUMAN is never woken by anything, because a
// person is addressable and runs no turn. Without that split, one message in a
// division's room would be a turn for every agent beneath it, against the
// node's whole concurrency budget.
//
// SORTED AND DEDUPLICATED, because the comparison against the held set is an
// equality and the lead is nearly always also a member of the subtree.
func unitRoomMembers(chart *org.Organization, unit *org.Unit) []chat.Member {
	direct := map[string]bool{}
	for _, r := range unit.Roles {
		if r != nil {
			direct[r.Handle()] = r.Kind != org.KindHuman
		}
	}
	seen := map[string]chat.Member{}
	add := func(handle string, followAll bool) {
		if handle == "" {
			return
		}
		m, held := seen[handle]
		if !held {
			m = chat.Member{Handle: handle}
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
	out := make([]chat.Member, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b chat.Member) int {
		return strings.Compare(a.Handle, b.Handle)
	})
	return out
}

// sameMember compares what the chart decides and nothing else.
func sameMember(a, b chat.Member) bool {
	return a.Handle == b.Handle && a.FollowAll == b.FollowAll
}

// sameUnit reports whether a room's stored unit names this one.
//
// BOTH SPELLINGS, because [org.Unit.Key] is the id when a unit has one and the
// name otherwise — and a company that ADDS ids to units it already had would
// otherwise have every room read as somebody else's on the next apply.
func sameUnit(stored string, unit *org.Unit) bool {
	return stored == unit.Key() || stored == unit.Name
}

// unitRoomRows reads the replicated rows this reconcile compares against.
//
// ITS OWN SQL rather than the chat reader's, for [chatRoomRows]'s reason: a
// reader answers a VIEWER's question and filters by what that viewer may see,
// and this is the engine asking what is there.
type unitRoomRows struct{ db *store.DB }

// heldRoom is a room as the reconcile needs it.
type heldRoom struct {
	id      string
	kind    string
	unit    string
	members []chat.Member
}

// room finds a channel by its normalised name.
func (r unitRoomRows) room(ctx context.Context, name string) (heldRoom, bool, error) {
	var out heldRoom
	found := false
	// ONE SNAPSHOT over both reads, so the membership compared against the
	// chart is the membership of the room that was found — not of a room a
	// concurrent apply replaced between the two statements.
	err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		scan := tx.QueryRowContext(ctx,
			`SELECT id, kind, unit FROM chat_channels WHERE name_norm = ?`,
			name).Scan(&out.id, &out.kind, &out.unit)
		if errors.Is(scan, sql.ErrNoRows) {
			return nil
		}
		if scan != nil {
			return scan
		}
		found = true
		rows, err := tx.QueryContext(ctx,
			`SELECT handle, follow_all FROM chat_members WHERE channel_id = ? `+
				`ORDER BY handle`, out.id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var m chat.Member
			if err := rows.Scan(&m.Handle, &m.FollowAll); err != nil {
				return err
			}
			out.members = append(out.members, m)
		}
		return rows.Err()
	})
	if err != nil {
		return heldRoom{}, false, err
	}
	return out, found, nil
}
