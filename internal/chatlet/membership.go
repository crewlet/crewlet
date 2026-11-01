package chatlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// ADDING PEOPLE TO A ROOM, and the room that takes in everybody the company
// takes on. Both are membership writes on [Store.setMembership]'s terms: read
// the room and its members inside one snapshot, decide the new set there,
// publish only if it moved. What each adds is decided against the room AS
// THIS SNAPSHOT HOLDS IT, never a set the caller read earlier, so two people
// adding at once both land: the second's snapshot holds the first's people.

// AddMembers puts people in a room, and answers who it put there.
//
// ADDITIVE, where [Store.SetMembers] states the whole set: a person adding a
// colleague names the colleague, not everybody already in the room, so a
// membership somebody else changed a moment before is never written back over.
//
// THE RULES ARE THE MEMBERSHIP'S OWN ([Store.setMembership]): a private room
// refuses whoever is outside it as though it were not there, a direct
// conversation refuses because its people are its identity, and a unit's room
// refuses because the org chart decides who is in it, in the words
// [Store.SetMembers] uses. Every handle must be a seat of the company, since a
// member nothing employs is a name in a list that nobody can be.
//
// The second value is the seats this call put in the room, sorted; the people
// named who were already members are left out, and nobody named being new is
// the empty list beside a write that published nothing.
func (s *Store) AddMembers(ctx context.Context, actor Actor, channelID string,
	handles []string) (Written, []string, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, nil, err
	}
	named := cleanHandles(handles)
	slices.Sort(named)
	if len(named) == 0 {
		return Written{}, nil, invalid("members", "name the people and agents to "+
			"add to the room")
	}
	if len(named) > MaxMembers {
		return Written{}, nil, invalid("members", "%d people and agents against "+
			"a room's %d cap", len(named), MaxMembers)
	}
	opID, opErr := s.operation(actor, "add", channelID, named)
	if opErr != nil {
		return Written{}, nil, opErr
	}
	var added []string
	written, _, err := s.setMembership(ctx, actor, channelID, opID, chartWrite{},
		plainMembership(func(room Channel, held []Member) ([]Member, error) {
			added = nil
			if room.Kind == KindUnit {
				return nil, fmt.Errorf("%w: %s is a unit's own room, and the "+
					"org chart decides who's in it. Adding people here would be "+
					"undone the next time the company's configuration is applied. "+
					"Move the seats into the unit instead", ErrForbidden,
					room.called())
			}
			for _, handle := range named {
				if exists, _ := s.roster.Seat(handle); !exists {
					return nil, invalid("members", "%q is not a seat of this "+
						"company, so there is nobody to add", handle)
				}
			}
			set := slices.Clone(held)
			for _, handle := range named {
				if !slices.ContainsFunc(held, func(m Member) bool { return m.Handle == handle }) {
					set = append(set, Member{Handle: handle})
					added = append(added, handle)
				}
			}
			return set, nil
		}))
	if err != nil {
		return Written{}, nil, err
	}
	// NOTHING THIS NODE CAN VOUCH FOR was added on an unvouched answer: the
	// outcome beside it says the write may have landed, and the room is
	// what a caller reads to learn who is in it.
	if written.Outcome.Unvouched {
		added = nil
	}
	if added == nil {
		added = []string{}
	}
	return written, added, nil
}

// AutoJoin puts in an auto-join room ([Channel.AutoJoin]) every seat of the
// company it has not accounted for yet, and accounts for them, answering who
// it put there.
//
// THE ENGINE'S ALONE ([AuthorSystem]), called by its reconcile at every
// activation with the org chart's whole seat list: a seat the room has not
// accounted for is one that joined the company after the setting was turned
// on, since turning it on accounted for every seat the company had then.
// Accounting is what makes it once: a newcomer who leaves the room afterwards
// is accounted for, and is not put back by the next activation. A newcomer
// already in the room (somebody added them first) is accounted for and
// otherwise left alone.
//
// A ROOM THAT IS NOT AUTO-JOIN, OR IS ARCHIVED, DECIDES NOTHING, read in the
// snapshot rather than from the caller's listing: the setting may have been
// turned off since, and a closed room takes nobody in. An archived room's
// newcomers are accounted for when it is opened again, at the next
// activation, so nobody who joined meanwhile is missed.
//
// A FRESH OPERATION PER CALL, for [Store.SetUnitMembers]' reason: this is a
// reconcile decided from the rows, so a second call on another node finds the
// newcomers accounted for and decides nothing, and N nodes racing one
// activation are settled by the broker's arbitration on the room.
func (s *Store) AutoJoin(ctx context.Context, actor Actor, channelID string,
	seats []string) (Written, []string, error) {

	if err := actor.seatedOrSystem(); err != nil {
		return Written{}, nil, err
	}
	if actor.Kind != AuthorSystem {
		return Written{}, nil, fmt.Errorf("%w: who an auto-join room takes in is "+
			"the engine's to decide, from the org chart", ErrForbidden)
	}
	company := cleanHandles(seats)
	slices.Sort(company)
	var added []string
	written, _, err := s.setMembership(ctx, actor, channelID, s.newOpID(), chartWrite{},
		func(tx *sql.Tx, room Channel, held []Member) (memberChange, error) {
			added = nil
			if !room.AutoJoin || room.ArchivedAt != nil || room.Kind == KindUnit {
				return memberChange{set: held}, nil
			}
			accounted, err := autoJoinAccountTx(ctx, tx, room.ID)
			if err != nil {
				return memberChange{}, err
			}
			var newcomers []string
			for _, handle := range company {
				if !accounted[handle] {
					newcomers = append(newcomers, handle)
				}
			}
			if len(newcomers) > MaxMembers {
				newcomers = newcomers[:MaxMembers]
			}
			set := slices.Clone(held)
			for _, handle := range newcomers {
				if !slices.ContainsFunc(held, func(m Member) bool { return m.Handle == handle }) {
					set = append(set, Member{Handle: handle})
					added = append(added, handle)
				}
			}
			return memberChange{set: set, autoJoined: newcomers}, nil
		})
	if err != nil {
		return Written{}, nil, err
	}
	if written.Outcome.Unvouched {
		added = nil
	}
	return written, added, nil
}

// autoJoinAccountTx is the seats a room's auto-join has accounted for, read
// inside a decision.
func autoJoinAccountTx(ctx context.Context, tx *sql.Tx, channelID string) (
	map[string]bool, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT handle FROM chat_autojoin WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read room %s's auto-join account: %w",
			channelID, err)
	}
	out := map[string]bool{}
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("chatlet: scan room %s's auto-join account: %w",
				channelID, err)
		}
		out[handle] = true
	}
	return out, closeRows(rows, "room "+channelID+"'s auto-join account")
}

// AutoJoinRooms is every live room whose auto-join is on, in id order: the
// rooms the engine's reconcile asks [Store.AutoJoin] about at an activation.
//
// FROM THIS NODE'S OWN ROWS, on the partial index the migration keeps for
// exactly this, and it is only a list of candidates: each room's setting is
// read again inside [Store.AutoJoin]'s own snapshot, so a room turned off
// after this read takes nobody in.
func (s *Store) AutoJoinRooms(ctx context.Context) ([]string, error) {
	var out []string
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		ids, err := handlesTx(ctx, tx, `
			SELECT id FROM chat_channels
			 WHERE auto_join = 1 AND archived_at IS NULL
			 ORDER BY id`)
		out = ids
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chatlet: read the auto-join rooms: %w", err)
	}
	return out, nil
}
