package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/store"
)

// THE ROOM'S OWN CASES: create, patch, archive and membership.
//
// Everything here arbitrates. A room's state is decided against its own last
// record — on the NAME's subject for a create that claims an address, on the
// ROOM's for everything after — so two writers changing one room contend at
// the broker and exactly one wins. The messages in that room do not, which is
// the other half of this log and lives in apply_message.go.

// The two values `chat_members.source` holds, and who each belongs to.
//
// EXPORTED because the applier is the only WRITER of the column and a reader
// renders it ([ChannelMember.Source]): a screen offers to remove a member
// somebody invited, and must not offer it for one the org chart put there,
// whom the next apply would put straight back. Derived from the room's kind
// rather than carried per member — see [memberSource].
const (
	// MemberSourceOrg is a member the unit reconcile put in the room.
	MemberSourceOrg = "org"

	// MemberSourceExplicit is a member somebody put there.
	MemberSourceExplicit = "explicit"
)

// applyChannelName applies a record published on an ADDRESS.
//
// ONE OP LIVES HERE, and it is the create of a NAMED room: the name is what
// two writers contend for, so `#launch` is a create-only append on the name's
// own subject and exactly one of them wins. Everything a room's people
// afterwards change — its topic, its purpose, its membership, whether it is
// archived — arbitrates on the ROOM, because a name never moves again: see
// [ChannelPatch] for why this build has no rename at all.
func (a *Applier) applyChannelName(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(ChannelCreate)
	if !ok {
		return 0, fmt.Errorf("chatlet: the %s record at %s carries a %T, and an "+
			"address arbitrates a create and nothing else",
			at.record.Op, at.position, payload)
	}
	// THE SUBJECT IS THE ADDRESS, so the payload's name has to be the one
	// that was arbitrated. Without this a writer could take one name at the
	// broker — where exactly one writer can — and write a different one
	// into every node's row, leaving the arbitrated name held by nothing
	// and the written name held by two rooms.
	if token := ChannelToken(p.Name); token != at.subject().ID {
		return 0, fmt.Errorf("chatlet: the record at %s arbitrated the address %s "+
			"and its payload claims %q, whose address is %s — the subject IS "+
			"the address, so a record that took one and wrote another would "+
			"leave the arbitrated name held by nothing",
			at.position, at.subject().ID, p.Name, token)
	}
	return a.applyCreate(ctx, tx, at, p)
}

// applyChannel applies a record published on a ROOM.
//
// SIX OPS, AND THE SWITCH IS EXHAUSTIVE. A create reaches here only for a
// DIRECT conversation, which has no name to contend for and therefore
// arbitrates on its own derived id; the other five are the room's own state —
// its settings, its retention, its membership — and the two destructive
// gestures over what was said in it.
func (a *Applier) applyChannel(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case ChannelCreate:
		// A DIRECT CONVERSATION'S ID IS ITS PARTICIPANT SET, derived
		// identically on every node, so the subject and the payload
		// must name one room. A record that derived one and claimed
		// another would make a second room with the same people in it,
		// each holding half the conversation.
		if p.ChannelID != at.subject().ID {
			return 0, fmt.Errorf("chatlet: the create at %s arbitrated room %s "+
				"and its payload claims %s — the subject IS the room",
				at.position, at.subject().ID, p.ChannelID)
		}
		return a.applyCreate(ctx, tx, at, p)
	case ChannelPatch:
		return a.applyChannelPatch(ctx, tx, at, p)
	case RetentionSet:
		return a.applyRetention(ctx, tx, at, p)
	case MemberSet:
		return a.applyMembers(ctx, tx, at, p)
	case MessageErase:
		return a.applyErase(ctx, tx, at, p)
	case Prune:
		return a.applyPrune(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("chatlet: the %s record at %s carries a %T, which is not "+
		"a payload a room's own subject arbitrates", at.record.Op, at.position, payload)
}

// applyCreate writes the name claim, the room and its founding membership — in
// ONE transaction, which is the whole reason this domain exists.
//
// On a coordination bucket a create was a two-key sequence, with an orphan
// claim as its crash state and a grace rule for stepping over the debris.
// Here there is no window in which a name is held by a room that was never
// written, so there is no orphan and no grace rule anywhere in this package.
func (a *Applier) applyCreate(ctx context.Context, tx *sql.Tx, at applyContext,
	p ChannelCreate) (int, error) {

	rows := 0
	if p.Kind.Named() {
		claimed, taken, err := claimName(ctx, tx, at, p)
		if err != nil {
			return 0, err
		}
		if taken {
			// THE ADDRESS IS HELD BY ANOTHER ROOM. The broker
			// arbitrates this name create-only, so the only way
			// here is a claim table restored beside newer rooms —
			// the worked example the schema's no-UNIQUE rule is
			// about. APPLIED COMPLETELY AS A NO-OP rather than
			// refused: a unique index would wedge every node at
			// once, where this leaves one odd row for the operator
			// surface to report.
			return 0, nil
		}
		rows += claimed
	}

	room := Channel{
		V: DocumentVersion, ID: p.ChannelID, Name: p.Name, Kind: p.Kind,
		Topic: p.Topic, Purpose: p.Purpose, Unit: p.Unit,
		CreatedBy: p.CreatedBy, CreatedByKind: p.CreatedByKind, CreatedAt: at.brokerAt,
		ChartEpoch: p.ChartEpoch,
	}
	document, err := EncodeChannel(room)
	if err != nil {
		return 0, err
	}
	// CREATE-ONLY ON THE ROOM'S ID. A second create for one room can only
	// be a redelivery or the loser of a race the broker already settled,
	// and letting either through would reset a live room's topic, its
	// retention and — worse — its MEMBERSHIP, which is what a private
	// room's readability is.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_channels
			(id, name, name_norm, kind, private, topic, purpose, unit,
			 retention_days, message_seq, created_at, created_by,
			 created_by_kind, archived_at, version, scoped_through, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, NULL, ?, 0, ?)
		ON CONFLICT (id) DO NOTHING`,
		room.ID, room.Name, NormalizeName(room.Name), string(room.Kind),
		boolValue(privateRoom(room.Kind)), room.Topic, room.Purpose, room.Unit,
		retentionValue(room.RetentionDays), store.EncodeTime(at.brokerAt),
		room.CreatedBy, string(room.CreatedByKind), at.packed, document)
	if err != nil {
		return 0, fmt.Errorf("chatlet: create room %s at %s: %w",
			room.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// THE ROOM ALREADY EXISTS, so this create wrote nothing and
		// its membership is not this record's to write. Writing them anyway is how a redelivered
		// create silently replaces a room's current membership with
		// the one it was founded with.
		return rows, nil
	}
	rows += int(n)

	members, err := replaceMembers(ctx, tx, at, room, p.Members)
	if err != nil {
		return 0, err
	}
	rows += members

	// A NAMED ROOM OPENS WITH WHO IS IN IT: its founding members joined,
	// authored by whoever made it (the engine, for a unit's room). A direct
	// conversation opens with no line, since its people are what it is.
	frame := at.live(room.ID)
	if p.Kind.Named() {
		written, err := a.writeEvents(ctx, tx, at, room.ID,
			membershipEvents(at.record.Actor, AuthorSystem, nil, p.Members))
		if err != nil {
			return 0, err
		}
		rows += written.rows
		noteEvents(&frame, written)
	}
	a.live.note(frame)
	return rows, nil
}

// claimName writes the company's hold on one address, reporting whether the
// address is already held by ANOTHER room.
//
// THE PROBE IS BEFORE THE INSERT rather than inferred from it, because the
// insert's conflict clause cannot tell "this room already claimed this name"
// — a redelivery, which must go on to be idempotent — from "another room holds
// it", which must write nothing at all. Both are one affected-row count of
// zero.
func claimName(ctx context.Context, tx *sql.Tx, at applyContext, p ChannelCreate) (
	rows int, taken bool, err error) {

	token := ChannelToken(p.Name)
	var holder string
	err = tx.QueryRowContext(ctx,
		`SELECT channel_id FROM chat_channel_names WHERE name_token = ?`, token).
		Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, false, fmt.Errorf("chatlet: read the claim on %q at %s: %w",
			p.Name, at.position, err)
	case holder != p.ChannelID:
		return 0, true, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_channel_names
			(name_token, name_norm, channel_id, claimed_at, version)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name_token) DO NOTHING`,
		token, NormalizeName(p.Name), p.ChannelID,
		store.EncodeTime(at.brokerAt), at.packed)
	if err != nil {
		return 0, false, fmt.Errorf("chatlet: claim %q at %s: %w",
			p.Name, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), false, nil
}

// applyChannelPatch changes a room's settings: its topic, its purpose, and
// whether it is archived.
//
// THE DOCUMENT MOVES WITH THE COLUMNS. Every reader here decodes `document` —
// a listing, a room header, the snapshot a write path decides against — so a
// topic moved in the column alone is a change NOBODY CAN SEE, and an archive
// moved in the column alone is a room that stays open after somebody closed
// it.
func (a *Applier) applyChannelPatch(ctx context.Context, tx *sql.Tx,
	at applyContext, p ChannelPatch) (int, error) {

	room, found, err := readChannel(ctx, tx, at.subject().ID)
	if err != nil {
		return 0, err
	}
	if !found {
		// A PATCH FOR A ROOM THIS NODE DOES NOT HAVE, under a STRICT
		// replay, is a malformed record rather than a race: the create
		// is below this position on the same ordered log — the room's
		// own subject for a direct conversation, and the name's for a
		// named one, which the record's scope makes intersect — so a
		// node that applied that position and has no row applied it
		// wrong.
		return 0, fmt.Errorf("chatlet: the patch at %s names room %s, which this "+
			"node has no row for — under a strict replay its create is below "+
			"this position, so a missing row is a record this build applied "+
			"incorrectly rather than one that has not arrived",
			at.position, at.subject().ID)
	}
	before := room
	if p.Topic != nil {
		room.Topic = *p.Topic
	}
	if p.Purpose != nil {
		room.Purpose = *p.Purpose
	}
	if p.Archived != nil {
		room.ArchivedAt = nil
		if *p.Archived {
			archived := at.brokerAt
			room.ArchivedAt = &archived
		}
	}
	if p.AutoJoin != nil {
		room.AutoJoin = *p.AutoJoin
	}
	rows, err := a.writeChannel(ctx, tx, at, room)
	if err != nil || rows == 0 {
		// THE VERSION GUARD SKIPPED IT, which is a redelivery or a
		// record the broker ordered below one already applied. A live
		// frame for it would announce a change that did not happen.
		return 0, err
	}
	if p.AutoJoin != nil && *p.AutoJoin != before.AutoJoin {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		accounted, err := resetAutoJoin(ctx, tx, at, room.ID, *p.AutoJoin, p.AutoJoinSeats)
		if err != nil {
			return 0, err
		}
		rows += accounted
	}
	frame := at.live(room.ID)
	written, err := a.writeEvents(ctx, tx, at, room.ID, patchEvents(before, room, p))
	if err != nil {
		return 0, err
	}
	rows += written.rows
	noteEvents(&frame, written)
	a.live.note(frame)
	return rows, nil
}

// resetAutoJoin starts or ends a room's auto-join account ([Channel.AutoJoin]):
// turned on, the seats the company had then, which the setting will not add;
// turned off, nothing, so turning it on again later starts from the company
// as it is then rather than from a list a year old.
func resetAutoJoin(ctx context.Context, tx *sql.Tx, at applyContext, roomID string,
	on bool, seats []string) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_autojoin WHERE channel_id = ?`, roomID); err != nil {
		return 0, fmt.Errorf("chatlet: clear room %s's auto-join account at %s: %w",
			roomID, at.position, err)
	}
	if !on {
		return 0, nil
	}
	return accountAutoJoin(ctx, tx, at, roomID, seats)
}

// accountAutoJoin records seats as accounted for by a room's auto-join, so the
// reconcile never puts them in again.
func accountAutoJoin(ctx context.Context, tx *sql.Tx, at applyContext, roomID string,
	seats []string) (int, error) {

	rows, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO chat_autojoin (channel_id, handle, at, version) VALUES`,
		`(?, ?, ?, ?)`,
		`ON CONFLICT (channel_id, handle) DO NOTHING`,
		len(seats), func(i int) []any {
			return []any{roomID, seats[i], store.EncodeTime(at.brokerAt), at.packed}
		})
	if err != nil {
		return 0, fmt.Errorf("chatlet: account room %s's auto-join seats at %s: %w",
			roomID, at.position, err)
	}
	return rows, nil
}

// applyRetention sets a room's retention override, or clears it.
//
// CARRIED AS A POINTER ALL THE WAY TO THE COLUMN, because [RetentionForever]
// is zero and is a real setting: NULL is "take the company's default" and 0 is
// "keep this room for ever", and an int that collapsed the two would silently
// delete a year of somebody's decisions. Like a patch, it moves the document
// with the column, and a record the version guard skips announces nothing.
func (a *Applier) applyRetention(ctx context.Context, tx *sql.Tx,
	at applyContext, p RetentionSet) (int, error) {

	room, found, err := readChannel(ctx, tx, at.subject().ID)
	if err != nil {
		return 0, err
	}
	if !found {
		// A RETENTION FOR A ROOM THIS NODE DOES NOT HAVE is a malformed
		// record under a strict replay, for [Applier.applyChannelPatch]'s
		// reason.
		return 0, fmt.Errorf("chatlet: the retention at %s names room %s, which "+
			"this node has no row for — under a strict replay its create is "+
			"below this position, so a missing row is a record this build "+
			"applied incorrectly rather than one that has not arrived",
			at.position, at.subject().ID)
	}
	room.RetentionDays = nil
	if p.RetentionDays != nil {
		days := *p.RetentionDays
		room.RetentionDays = &days
	}
	rows, err := a.writeChannel(ctx, tx, at, room)
	if err != nil || rows == 0 {
		return 0, err
	}
	a.live.note(at.live(room.ID))
	return rows, nil
}

// applyMembers replaces a room's membership, WHOLE.
//
// A DELTA CANNOT REBUILD A ROW ON A REPLAY FROM ZERO, and this is the
// collection where that matters most: membership is what a private room's
// readability IS, so a node that replayed a room's adds and missed one of its
// removals would serve a conversation to somebody who was taken out of it.
func (a *Applier) applyMembers(ctx context.Context, tx *sql.Tx, at applyContext,
	p MemberSet) (int, error) {

	room, found, err := readChannel(ctx, tx, at.subject().ID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("chatlet: the membership record at %s names room %s, "+
			"which this node has no row for — under a strict replay its create "+
			"is below this position", at.position, at.subject().ID)
	}
	// THE CHART'S ACTIVATION TRAVELS WITH ITS MEMBERSHIP. Only the org
	// chart's own writer sets one ([Store.SetUnitMembers]), and every other
	// membership write leaves the room's stamp where it was.
	if p.ChartEpoch != 0 {
		room.ChartEpoch = p.ChartEpoch
	}
	// AND SO DOES THE UNIT IT GIVES THE ROOM TO, when it moves one
	// ([MemberSet.Unit]). Only a unit room belongs to a unit, and the write
	// path refuses the chart any other kind inside its own snapshot — so a
	// record re-homing another room is one this build did not write, and
	// applying it would hand a person's room a lead to fall back to.
	rehomed := p.Unit != "" && p.Unit != room.Unit
	if p.Unit != "" {
		if room.Kind != KindUnit {
			return 0, fmt.Errorf("chatlet: the membership record at %s re-homes "+
				"room %s, a %s room, to unit %q — only a %s room belongs to a "+
				"unit", at.position, room.ID, room.Kind, p.Unit, KindUnit)
		}
		room.Unit = p.Unit
	}
	// READ BEFORE THE ROW IS STAMPED, because the stamp is what makes a
	// redelivery skip — and whether this record moves anybody decides what
	// else it writes.
	held, err := readMembersTx(ctx, tx, room.ID)
	if err != nil {
		return 0, err
	}
	// THE ROOM'S OWN ROW IS STAMPED FIRST, AND IT IS WHAT GATES THE
	// REPLACEMENT. A membership is a DELETE plus an INSERT, which has no
	// version guard of its own to skip on — so a redelivered record would
	// delete the room's current members and re-insert the ones it carried,
	// silently reinstating somebody a later record removed. The room's
	// version is the guard, and this record arbitrated on the room's own
	// subject, so moving it is what the record did.
	rows, err := a.writeChannel(ctx, tx, at, room)
	if err != nil || rows == 0 {
		return 0, err
	}
	moved := !slices.Equal(sortedMembers(p.Members), held)
	if !moved && !rehomed {
		// A RE-STAMP: the chart's set at a later activation, which moved
		// nobody. The room's row carries the new stamp and that is the
		// whole of what this record did — a live frame would tell a
		// screen to redraw a room that has not changed.
		return rows, nil
	}
	// A RE-HOME OVER THE SAME SEATS — a unit renamed without an `id` — is
	// still a change somebody can see: the room answers to another unit,
	// and its answerer with it. Its members' rows are already right.
	members := 0
	frame := at.live(room.ID)
	if moved {
		members, err = replaceMembers(ctx, tx, at, room, p.Members)
		if err != nil {
			return 0, err
		}
		frame.Removed = removedFrom(held, p.Members)
		written, err := a.writeEvents(ctx, tx, at, room.ID,
			membershipEvents(at.record.Actor, at.record.ActorKind, held, p.Members))
		if err != nil {
			return 0, err
		}
		members += written.rows
		noteEvents(&frame, written)
	}
	if len(p.AutoJoined) > 0 {
		accounted, err := accountAutoJoin(ctx, tx, at, room.ID, p.AutoJoined)
		if err != nil {
			return 0, err
		}
		members += accounted
	}
	a.live.note(frame)
	return rows + members, nil
}

// removedFrom is who was in held and is not in members: the people a
// membership record took out of a room, sorted by handle — the order held is
// read in.
func removedFrom(held, members []Member) []string {
	kept := make(map[string]bool, len(members))
	for _, m := range members {
		kept[m.Handle] = true
	}
	var out []string
	for _, m := range held {
		if !kept[m.Handle] {
			out = append(out, m.Handle)
		}
	}
	return out
}

// writeChannel writes a room's changed state, guarded by its own version.
//
// SIX COLUMNS AND THE DOCUMENT, and the ones NOT here are the point: a
// room's id, its name, its kind and who made it are IMMUTABLE — the name is
// the address the create arbitrated, and the rest is what the room is — so
// an update that could move them would be a rename with no claim behind it.
// The UNIT is here because the org chart may give a unit room to another unit
// ([MemberSet.Unit]), on the record it arbitrates on the room's own subject;
// every other write carries the room's unit back unchanged.
//
// `message_seq` is not here either, and that is the sharpest of them: it is
// written by MESSAGE records, from another subject, and stamping it from here
// would overwrite the room's high-water mark with a number this record never
// read.
func (a *Applier) writeChannel(ctx context.Context, tx *sql.Tx, at applyContext,
	room Channel) (int, error) {

	document, err := EncodeChannel(room)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE chat_channels
		SET topic = ?, purpose = ?, unit = ?, retention_days = ?, archived_at = ?,
		    auto_join = ?, version = ?, document = ?
		WHERE id = ? AND version < ?`,
		room.Topic, room.Purpose, room.Unit, retentionValue(room.RetentionDays),
		nullInstant(room.ArchivedAt), boolValue(room.AutoJoin), at.packed, document,
		room.ID, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chatlet: apply room %s at %s: %w",
			room.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// replaceMembers rewrites one room's membership.
//
// A DELETE AND ONE INSERT PER CHUNK, not one per member: the set's size is the
// founder's rather than the engine's, and it is bounded at [MaxMembers]
// precisely so the widest membership write fits in one apply transaction. The
// DELETE stays — this converges a collection, and a member who left has to
// lose their row.
//
// THE ROWS ARE SORTED BY HANDLE, and not because the record's order could
// differ between nodes — it is one record's bytes, so it cannot. It is so the
// row order is CANONICAL whatever order a writer happened to list a membership
// in, which is what keeps a file-level identity claim comparing equal the day
// a write path starts building the set from a map.
func replaceMembers(ctx context.Context, tx *sql.Tx, at applyContext,
	room Channel, members []Member) (int, error) {

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_members WHERE channel_id = ?`, room.ID); err != nil {
		return 0, fmt.Errorf("chatlet: clear room %s's members at %s: %w",
			room.ID, at.position, err)
	}
	set := slices.Clone(members)
	slices.SortFunc(set, func(x, y Member) int {
		switch {
		case x.Handle < y.Handle:
			return -1
		case x.Handle > y.Handle:
			return 1
		}
		return 0
	})
	source := memberSource(room.Kind)
	joined := store.EncodeTime(at.brokerAt)
	// THE CONFLICT CLAUSE OUTLIVES THE DELETE ABOVE, which looks redundant
	// and is not: the delete clears what a PREVIOUS record wrote, and a
	// record naming one handle twice collides with itself INSIDE one
	// statement, where a per-row loop could only ever collide across two.
	rows, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO chat_members
			(channel_id, handle, role, source, follow_all, joined_at, version)
		VALUES`,
		`(?, ?, '', ?, ?, ?, ?)`,
		`ON CONFLICT (channel_id, handle) DO UPDATE SET
			source = excluded.source, follow_all = excluded.follow_all,
			version = excluded.version`,
		len(set), func(i int) []any {
			return []any{room.ID, set[i].Handle, source,
				boolValue(set[i].FollowAll), joined, at.packed}
		})
	if err != nil {
		// The chunk names no single handle, so neither does this: a
		// statement carrying hundreds of them failed, and naming one
		// would point a reader at a row that is probably fine.
		return 0, fmt.Errorf("chatlet: write room %s's members at %s: %w",
			room.ID, at.position, err)
	}
	return rows, nil
}

// readChannel reads one room's current state out of this transaction.
//
// THROUGH THE DOCUMENT rather than the columns, although every field it holds
// is also a column: the document is what carries a NEWER BUILD'S fields
// through this node untouched, and a patch rebuilt from columns would drop
// them on the first edit a mid-upgrade fleet applied.
func readChannel(ctx context.Context, tx *sql.Tx, id string) (Channel, bool, error) {
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM chat_channels WHERE id = ?`, id).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Channel{}, false, nil
	case err != nil:
		return Channel{}, false, fmt.Errorf("chatlet: read room %s: %w", id, err)
	}
	room, err := DecodeChannel(document)
	if err != nil {
		return Channel{}, false, fmt.Errorf("chatlet: decode room %s: %w", id, err)
	}
	return room, true, nil
}

// memberSource is who a room's members are there by: the org chart, or
// somebody's decision.
//
// DERIVED FROM THE ROOM'S KIND, because a [Member] carries no source of its
// own: a unit's room is filled and maintained by the chart's reconcile, which
// restates the WHOLE set at every activation ([Store.SetUnitMembers]), and
// every other room's membership is somebody's decision. Nothing decides from
// the column — the chart's write replaces the set rather than withdrawing what
// it added — so it exists for the screens that render it ([ChannelMember]):
// one that could not tell the two apart would offer to remove a seat the next
// apply puts straight back.
//
// IT STOPS BEING TRUE THE DAY A PERSON CAN INVITE A GUEST INTO A UNIT ROOM,
// and what keeps that day away is a rule rather than an accident:
// [Store.Join], [Store.Leave] and [Store.SetMembers] all REFUSE a unit room,
// so the chart's own writer ([Store.SetUnitMembers]) is the only thing that
// ever sets one's membership and every member of it is therefore `org`.
//
// Should a guest ever be admitted, this derivation is what has to go first —
// the seam to widen is a `Source` on [Member], rather than a rule to add here,
// because only the writer knows which half of a set it is restating.
func memberSource(kind Kind) string {
	if kind == KindUnit {
		return MemberSourceOrg
	}
	return MemberSourceExplicit
}

// privateRoom reports whether a room's transcript is reachable only through
// its membership.
//
// A COLUMN RATHER THAN A RE-DERIVATION AT EVERY READ, because it is a
// PREDICATE: the visibility filter on every listing and every transcript read
// is `private = 0 OR <this reader is a member>`, and a filter that had to
// decode a kind per row could not use an index at all.
//
// A UNIT ROOM IS NOT PRIVATE. It is where work addressed to nobody in
// particular lands, and a company that could not see what its own units were
// deciding would be one where the org chart hid the work rather than
// organised it.
func privateRoom(kind Kind) bool {
	return kind == KindPrivate || kind.Direct()
}

// retentionValue is a room's override as the nullable column holds it.
//
// NULL IS "INHERIT" AND 0 IS "FOR EVER". Three settings in one column is
// exactly why it is nullable and why every field carrying it is a pointer;
// see [RetentionForever].
func retentionValue(days *int) any {
	if days == nil {
		return nil
	}
	return *days
}

// boolValue is a flag as the INTEGER column holds it.
func boolValue(b bool) int {
	if b {
		return 1
	}
	return 0
}
