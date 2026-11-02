package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ROOM EVENTS: what the applier writes into a room's transcript when a
// record changes the room itself ([RoomEvent]).
//
// Three records write them, each in its own transaction and only when it
// changed the room: a NAMED room's create (its founding members joined), a
// membership record (who came and went), and a patch (the topic, the purpose,
// or a template). A direct conversation writes none: its people are its
// identity, so nobody joins or leaves one, and it has no topic to set. An
// archive writes none either, because the room's header already says it is
// archived and the transcript is what the archive closed.
//
// WHAT AN EVENT SAYS IS DECIDED HERE, FROM THE ROWS, never carried by the
// record. The record says what the room becomes; the applier holds what it
// was, so "Ana added Ravi" is the difference between the two at this
// position, identical on every node, and a writer could not make a line
// claim a change the record did not make.

// eventID is a room event's id: a function of the record that wrote it and
// its place among that record's events.
//
// DERIVED, because every data node writes the same row from the same record,
// and an id minted from anything else would make the rows differ. THE
// OPERATION RATHER THAN THE POSITION, because a reanchor replays a record at
// another position and the row it wrote is the same row.
func eventID(opID string, index int) string {
	return uuid.NewSHA1(eventNamespace, []byte(opID+"/"+strconv.Itoa(index))).String()
}

// eventNamespace scopes [eventID]. FIXED for the life of the format: a room
// event's id is a row key every node agrees on, and moving it re-keys every
// event in every room.
var eventNamespace = uuid.MustParse("0b6e3c1a-7d24-4f5e-9a61-2c8f4e5d7b90")

// writtenEvents is what [Applier.writeEvents] wrote: how many rows, and the
// last event's id and number, which the record's live frame carries.
type writtenEvents struct {
	rows      int
	messageID string
	seq       int64
}

// writeEvents writes a record's room events as rows of the room's transcript,
// each taking the room's next number, in the order given.
//
// THE ROOM'S NUMBER MOVES TO AN ABSOLUTE VALUE, as a post's does, and it
// stamps neither `version` nor `scoped_through`: the record writing these is
// on the room's own subject and has already stamped `version` through
// [Applier.writeChannel], which is the guard that keeps a redelivery from
// writing them twice. Each insert is also gated on its own id, so an event a
// replay already wrote takes no second number.
func (a *Applier) writeEvents(ctx context.Context, tx *sql.Tx, at applyContext,
	roomID string, events []RoomEvent) (writtenEvents, error) {

	var out writtenEvents
	if len(events) == 0 {
		return out, nil
	}
	var seq int64
	seqErr := tx.QueryRowContext(ctx,
		`SELECT message_seq FROM chat_channels WHERE id = ?`, roomID).Scan(&seq)
	switch {
	case errors.Is(seqErr, sql.ErrNoRows):
		return out, fmt.Errorf("chatlet: the record at %s wrote events into room "+
			"%s, which this node has no row for", at.position, roomID)
	case seqErr != nil:
		return out, fmt.Errorf("chatlet: read room %s's sequence at %s: %w",
			roomID, at.position, seqErr)
	}
	for i, event := range events {
		id := eventID(at.record.OpID, i)
		message := Message{
			V: DocumentVersion, ID: id, ChannelID: roomID,
			Author: at.record.Actor, AuthorKind: at.record.ActorKind,
			CreatedAt: at.brokerAt, Event: &events[i],
		}
		document, err := EncodeMessage(message)
		if err != nil {
			return out, err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO chat_messages
				(id, channel_id, thread_root, author_handle, author_kind, body,
				 links, channel_seq, created_at, edited_at, deleted_at,
				 log_stream, log_generation, log_seq, version, shard, document,
				 event, collective)
			VALUES (?, ?, '', ?, ?, '', '', ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT (id) DO NOTHING`,
			id, roomID, message.Author, string(message.AuthorKind), seq+1,
			store.EncodeTime(at.brokerAt),
			at.position.Stream, at.position.Generation, int64(at.position.Seq),
			at.packed, search.ChatShardOf(id), document, eventColumn(message))
		if err != nil {
			return out, fmt.Errorf("chatlet: write room %s's %s event at %s: %w",
				roomID, event.Kind, at.position, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		seq++
		out.rows++
		out.messageID, out.seq = id, seq
	}
	if out.rows == 0 {
		return out, nil
	}
	bump, err := tx.ExecContext(ctx, `
		UPDATE chat_channels SET message_seq = ? WHERE id = ? AND message_seq < ?`,
		seq, roomID, seq)
	if err != nil {
		return out, fmt.Errorf("chatlet: move room %s's sequence at %s: %w",
			roomID, at.position, err)
	}
	moved, _ := bump.RowsAffected()
	out.rows += int(moved)
	return out, nil
}

// noteEvents puts the last event a record wrote on its live frame, so a
// screen holding the room's previous number applies it rather than reading
// the room again. A record that wrote two events skips a number on the frame,
// which a screen treats as the gap it is and reads.
func noteEvents(frame *Applied, written writtenEvents) {
	if written.rows == 0 || written.messageID == "" {
		return
	}
	frame.MessageID, frame.ChannelSeq = written.messageID, written.seq
}

// membershipEvents is what a membership record says happened, from the set
// the room held to the set it holds now.
//
// WHO MADE THE CHANGE DECIDES THE WORDS. The engine (the org chart, or an
// auto-join room taking in a newcomer) puts seats in and takes them out with
// nobody's hand on it, so its lines say they joined and left. A person's own
// place is the same: they joined, or they left. Anybody else's is what the
// person did to them: added, or removed.
func membershipEvents(actor string, kind AuthorKind, held, members []Member) []RoomEvent {
	before := make(map[string]bool, len(held))
	for _, m := range held {
		before[m.Handle] = true
	}
	after := make(map[string]bool, len(members))
	var came, went []string
	for _, m := range members {
		after[m.Handle] = true
		if !before[m.Handle] {
			came = append(came, m.Handle)
		}
	}
	for _, m := range held {
		if !after[m.Handle] {
			went = append(went, m.Handle)
		}
	}
	slices.Sort(came)
	slices.Sort(went)
	if kind == AuthorSystem {
		return eventsOf(EventJoined, came, EventLeft, went)
	}
	self := []string{actor}
	var out []RoomEvent
	if slices.Contains(came, actor) {
		out = append(out, RoomEvent{Kind: EventJoined, Handles: self})
		came = slices.DeleteFunc(came, func(h string) bool { return h == actor })
	}
	out = append(out, eventsOf(EventAdded, came, "", nil)...)
	out = append(out, eventsOf(EventRemoved, slices.DeleteFunc(slices.Clone(went),
		func(h string) bool { return h == actor }), "", nil)...)
	if slices.Contains(went, actor) {
		out = append(out, RoomEvent{Kind: EventLeft, Handles: self})
	}
	return out
}

// eventsOf is one event of each kind whose list is not empty.
func eventsOf(first EventKind, firstHandles []string, second EventKind,
	secondHandles []string) []RoomEvent {

	var out []RoomEvent
	if len(firstHandles) > 0 {
		out = append(out, RoomEvent{Kind: first, Handles: firstHandles})
	}
	if second != "" && len(secondHandles) > 0 {
		out = append(out, RoomEvent{Kind: second, Handles: secondHandles})
	}
	return out
}

// patchEvents is what a patch says happened to the room's words, from the
// room as it was to the room as the patch leaves it: a line for the topic and
// a line for the purpose that moved, or ONE template line where the patch
// names the template it set them from. An archive and an auto-join setting
// write no line.
func patchEvents(before, after Channel, patch ChannelPatch) []RoomEvent {
	topic := before.Topic != after.Topic
	purpose := before.Purpose != after.Purpose
	if !topic && !purpose {
		return nil
	}
	if patch.Template != nil {
		return []RoomEvent{{Kind: EventTemplate, Text: *patch.Template}}
	}
	var out []RoomEvent
	if purpose {
		out = append(out, RoomEvent{Kind: EventDescription, Text: after.Purpose})
	}
	if topic {
		out = append(out, RoomEvent{Kind: EventTopic, Text: after.Topic})
	}
	return out
}

// isEvent refuses a gesture on a room event: it is the room's own record of a
// change, and nobody's words to answer, react to, rewrite or take back.
func isEvent(m Message, gesture string) error {
	if m.Event == nil {
		return nil
	}
	return invalid("message_id", "that line records a change to the room, not "+
		"something somebody said, so it takes no %s", gesture)
}
