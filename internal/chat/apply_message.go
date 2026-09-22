package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// THE TRANSCRIPT'S CASES: post, edit, delete, react — and the two gestures
// that destroy what was said, erase and prune.
//
// Nothing here arbitrates. A post carries no expectation and decides against
// nothing, because the records COMMUTE: two people talking in one room are not
// racing for anything, and paying for arbitration would serialise the single
// hottest subject in the company behind itself — and turn a lost race into a
// message somebody typed and lost, which is the one failure a chat system may
// not have.
//
// What replaces arbitration is the INSERT'S OWN ID GATE. Every side effect a
// post has — the room's sequence, the mentions, the thread rows — is issued
// only when the message row was NEW, exactly as the tracker's turn records
// spend only when its own insert landed. A redelivery therefore consumes
// nothing and repairs nothing, which is what makes this idempotent at a
// position without a version to compare.

// chatShardSource is the namespace a message's search bucket is hashed under.
//
// IT MUST EQUAL `internal/search`'s own `chatShardSource`, which is a local
// constant there: the chat indexer buckets the same message into `chat_docs`
// with it, and a fan-out that assigned a node the range [16,32) would scan two
// corpora that disagreed about which sixty-fourth a message is in — a search
// returning one fewer hit, which looks exactly like a corpus with one fewer
// document. The right shape is one exported constant; until that export
// exists, this is the copy and this comment is what holds them together.
const chatShardSource = "message"

// applyMessage writes one record on a room's message stream.
//
// FOUR OPS, AND THE SWITCH IS EXHAUSTIVE. The erase and the prune are NOT here
// — both destroy rows across a whole room, so both are published on the ROOM's
// subject where they serialise against its own state, and both are reached
// through [Applier.applyChannel].
func (a *Applier) applyMessage(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	switch p := payload.(type) {
	case MessagePost:
		return a.applyPost(ctx, tx, at, p)
	case MessageEdit:
		return a.applyEdit(ctx, tx, at, p)
	case MessageDelete:
		return a.applyDelete(ctx, tx, at, p)
	case Reaction:
		return a.applyReact(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("chat: the %s record at %s carries a %T, which is not "+
		"a payload a room's message stream carries", at.record.Op, at.position, payload)
}

// applyPost writes one message, the number it landed at, and everything
// derived from it.
//
// # The per-channel sequence, and why the applier mints it
//
// `channel_seq` comes from the ROOM'S OWN high-water mark, read in this
// transaction and moved only when the insert was new. A writer cannot carry it
// — additive records form no expectation, so two writers in one room would
// compute the same number — and every node can, because every node applies the
// same records in the same order.
//
// The number is CONTIGUOUS per room, which is what lets a browser tell a live
// frame it can apply from a hole it must refetch: holding 41 and handed 43, it
// asks for 42 rather than for the room. That contiguity is also why a post's
// declared scope is its CHANNEL — see the package doc.
func (a *Applier) applyPost(ctx context.Context, tx *sql.Tx, at applyContext,
	p MessagePost) (int, error) {

	// A POST IS THE ONE PATH THAT CAN CREATE A MESSAGE ROW FROM NOTHING,
	// and therefore the one that could RESURRECT what a compliance erase
	// destroyed: every other case needs a row the erase already removed.
	// So the marker is consulted here, and here only.
	erased, err := messageErased(ctx, tx, p.MessageID, at.record.OpID)
	if err != nil {
		return 0, err
	}
	if erased {
		return 0, nil
	}

	var seq int64
	err = tx.QueryRowContext(ctx,
		`SELECT message_seq FROM chat_channels WHERE id = ?`, at.subject().ID).
		Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A POST INTO A ROOM THIS NODE DOES NOT HAVE, under a STRICT
		// replay, is a malformed record rather than a race: the room's
		// create is below this position on the same ordered log, and a
		// post's scope is that room, so a node that applied this
		// position without the room applied something wrong. Minting a
		// sequence against a room nobody has is the one mistake this
		// domain cannot repair afterwards.
		return 0, fmt.Errorf("chat: the post at %s names room %s, which this "+
			"node has no row for — under a strict replay the room's create is "+
			"below this position, so a missing row is a record this build "+
			"applied incorrectly rather than one that has not arrived",
			at.position, at.subject().ID)
	case err != nil:
		return 0, fmt.Errorf("chat: read room %s's sequence at %s: %w",
			at.subject().ID, at.position, err)
	}
	seq++

	message := Message{
		V: DocumentVersion, ID: p.MessageID, ChannelID: at.subject().ID,
		ThreadRoot: p.ThreadRoot, Author: p.Author, AuthorKind: p.AuthorKind,
		Body: p.Body, Links: p.Links, Mentions: p.Mentions,
		Collective: p.Collective, CreatedAt: at.brokerAt,
	}
	document, err := EncodeMessage(message)
	if err != nil {
		return 0, err
	}
	links, err := encodeLinks(p.Links)
	if err != nil {
		return 0, fmt.Errorf("chat: encode the links on message %s at %s: %w",
			p.MessageID, at.position, err)
	}
	// THE INSERT IS THE GATE. Additive records have no version to compare,
	// so what tells a new post from a redelivery is whether this statement
	// wrote a row — and every side effect below hangs off that answer, the
	// room's sequence first among them.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages
			(id, channel_id, thread_root, author_handle, author_kind, body,
			 links, channel_seq, created_at, edited_at, deleted_at,
			 log_stream, log_generation, log_seq, version, shard, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		message.ID, message.ChannelID, message.ThreadRoot, message.Author,
		string(message.AuthorKind), message.Body, links, seq,
		store.EncodeTime(at.brokerAt),
		at.position.Stream, at.position.Generation, int64(at.position.Seq),
		at.packed, search.ShardOf(chatShardSource, message.ID), document)
	if err != nil {
		return 0, fmt.Errorf("chat: write message %s at %s: %w",
			message.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// A REDELIVERY, and the whole point of gating on the insert: it
		// must NOT consume a second number. Under any other arrangement
		// the room's sequence has a hole in it, and a hole is
		// indistinguishable from a frame a browser missed — so every
		// viewer refetches a message that does not exist, for ever.
		return 0, nil
	}
	rows := int(n)

	// THE HIGH-WATER MARK MOVES TO AN ABSOLUTE VALUE, not by one: a
	// statement that added would be correct only if it ran exactly as
	// often as the insert did, which is a second idempotence argument
	// where this needs none.
	//
	// IT STAMPS `scoped_through` AND NEVER `version`. This record
	// arbitrated on the room's MESSAGE subject, so the room's own
	// expectation is its last channel record — and a post that moved
	// `version` would poison it, leaving a room whose topic nobody can
	// change until somebody posts again.
	bump, err := tx.ExecContext(ctx, `
		UPDATE chat_channels
		SET message_seq = ?, scoped_through = ?
		WHERE id = ? AND message_seq < ?`,
		seq, at.packed, message.ChannelID, seq)
	if err != nil {
		return 0, fmt.Errorf("chat: move room %s's sequence at %s: %w",
			message.ChannelID, at.position, err)
	}
	moved, _ := bump.RowsAffected()
	rows += int(moved)

	mentions, err := writeMentions(ctx, tx, at, message)
	if err != nil {
		return 0, err
	}
	rows += mentions

	thread, err := a.writeThreadRows(ctx, tx, at, message)
	if err != nil {
		return 0, err
	}
	rows += thread

	// NO HISTORY ROW FOR A POST. The message row IS the post's own
	// record, and a second row per message would double the highest-volume
	// table in the engine to say a thing the first one already says.
	frame := at.live(message.ChannelID)
	frame.MessageID = message.ID
	frame.ChannelSeq = seq
	a.live.note(frame)
	return rows, nil
}

// applyEdit rewrites a message's body in place.
//
// THE DERIVED COLLECTIONS ARE REWRITTEN WITH IT, because the body is what they
// were derived from: an edit that added a mention and left the old set would
// leave the row naming whoever the first draft named, in somebody's mention
// feed, for ever.
func (a *Applier) applyEdit(ctx context.Context, tx *sql.Tx, at applyContext,
	p MessageEdit) (int, error) {

	message, found, err := readMessage(ctx, tx, p.MessageID)
	if err != nil {
		return 0, err
	}
	if !found {
		// A MISSING ROW IS LEGITIMATE HERE, which is the opposite of
		// what a missing room means to a post — and the difference is
		// that this domain DELETES CONTENT. An erase destroyed the row
		// or a retention prune took it with everything else older than
		// a cutoff, and an edit that arrived behind either is ordinary
		// traffic. Faulting on it would stall every node's log over a
		// record whose subject the company deliberately removed.
		return 0, nil
	}
	if message.DeletedAt != nil {
		// AN EDIT OF A TOMBSTONE WRITES NOTHING. The body was removed
		// on purpose and the row survives only so the thread hung off
		// it still has its root; restoring text into it would undo a
		// deletion nobody asked to undo.
		return 0, nil
	}
	message.Body = p.Body
	message.Links = p.Links
	message.Mentions = p.Mentions
	edited := at.brokerAt
	message.EditedAt = &edited
	message.EditedBy, message.EditedByKind = p.EditedBy, p.EditedByKind

	rows, err := a.writeMessageBody(ctx, tx, at, message.Message)
	if err != nil || rows == 0 {
		// THE VERSION GUARD SKIPPED IT, which is a redelivery or a
		// record ordered below one already applied. Rewriting the
		// mentions anyway would replace a newer edit's set with an
		// older one's.
		return 0, err
	}
	// THE WHOLE SET IS REPLACED rather than merged, for the reason the
	// record carries it whole: a delta cannot rebuild the row on a replay
	// from zero.
	cleared, err := tx.ExecContext(ctx,
		`DELETE FROM chat_mentions WHERE message_id = ?`, message.ID)
	if err != nil {
		return 0, fmt.Errorf("chat: clear message %s's mentions at %s: %w",
			message.ID, at.position, err)
	}
	dropped, _ := cleared.RowsAffected()
	rows += int(dropped)

	mentions, err := writeMentions(ctx, tx, at, message.Message)
	if err != nil {
		return 0, err
	}
	entry, err := a.writeHistory(ctx, tx, at, message.ChannelID, message.ID)
	if err != nil {
		return 0, err
	}
	a.noteMessage(at, message)
	return rows + mentions + entry, nil
}

// applyDelete tombstones a message: the body goes, the row stays.
//
// THE ROW STAYS so a thread hung off this message still has its root and its
// replies stay reachable. Removing the row is [MessageErase], which is a
// different gesture with a different author and a marker that makes it
// permanent.
func (a *Applier) applyDelete(ctx context.Context, tx *sql.Tx, at applyContext,
	p MessageDelete) (int, error) {

	message, found, err := readMessage(ctx, tx, p.MessageID)
	if err != nil {
		return 0, err
	}
	if !found {
		// Erased or pruned — see [Applier.applyEdit] for why that is
		// ordinary traffic rather than a malformed record.
		return 0, nil
	}
	if message.DeletedAt != nil {
		// ALREADY A TOMBSTONE. Stamping a second instant would move the
		// moment somebody's words were removed to whenever the second
		// record arrived.
		return 0, nil
	}
	message.Body = ""
	message.Links = nil
	message.Mentions = nil
	deleted := at.brokerAt
	message.DeletedAt = &deleted
	message.DeletedBy, message.DeletedByKind = p.DeletedBy, p.DeletedByKind

	rows, err := a.writeMessageBody(ctx, tx, at, message.Message)
	if err != nil || rows == 0 {
		return 0, err
	}
	// THE MENTIONS GO WITH THE BODY. A mention row IS the mention feed, so
	// a tombstone that kept them leaves an empty message sitting in
	// somebody's list of things that named them.
	//
	// THE REACTIONS DO NOT. They are other people's marks ON the message
	// rather than anything derived from its text, and they go when the row
	// goes — with the erase, or with the prune.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM chat_mentions WHERE message_id = ?`, message.ID)
	if err != nil {
		return 0, fmt.Errorf("chat: clear message %s's mentions at %s: %w",
			message.ID, at.position, err)
	}
	gone, _ := res.RowsAffected()
	entry, err := a.writeHistory(ctx, tx, at, message.ChannelID, message.ID)
	if err != nil {
		return 0, err
	}
	a.noteMessage(at, message)
	return rows + int(gone) + entry, nil
}

// applyReact adds or removes one reaction.
//
// NO HISTORY ROW, and it is the one message mutation without one. A reaction
// carries no text an activity feed could render and wakes nobody, so an entry
// for it would be a row per thumb in the table a digest scans — the same
// argument the schema makes for a plain post, at a smaller scale.
func (a *Applier) applyReact(ctx context.Context, tx *sql.Tx, at applyContext,
	r Reaction) (int, error) {

	message, found, err := readMessage(ctx, tx, r.MessageID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	if r.Removed {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx, `
			DELETE FROM chat_reactions
			WHERE message_id = ? AND emoji = ? AND handle = ?`,
			r.MessageID, r.Emoji, r.By)
		if err != nil {
			return 0, fmt.Errorf("chat: remove %s's %q from message %s at "+
				"%s: %w", r.By, r.Emoji, r.MessageID, at.position, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			a.noteMessage(at, message)
		}
		return int(n), nil
	}
	// [MaxReactionEmoji] IS ENFORCED HERE AND NOWHERE ELSE, and the
	// record's shape is what forces it: a reaction is a single TOGGLE so
	// that two people reacting never overwrite each other, and a toggle
	// cannot see the set it is joining. The applier holds the message's
	// rows in its own transaction, so it declines the thirty-third
	// distinct emoji — deterministically, reaching the same answer on
	// every node, which a write-side cap read from one node's rows could
	// not promise.
	full, err := reactionsFull(ctx, tx, r)
	if err != nil {
		return 0, err
	}
	if full {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_reactions
			(message_id, emoji, handle, at, channel_id, message_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id, emoji, handle) DO NOTHING`,
		r.MessageID, r.Emoji, r.By, store.EncodeTime(at.brokerAt),
		// THE ROOM AND THE MESSAGE'S OWN INSTANT ARE COPIED ONTO THE
		// ROW, which looks redundant beside `message_id` and is what
		// makes the retention prune a range delete on an index rather
		// than a join the planner has to walk.
		message.ChannelID, store.EncodeTime(message.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("chat: add %s's %q to message %s at %s: %w",
			r.By, r.Emoji, r.MessageID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		a.noteMessage(at, message)
	}
	return int(n), nil
}

// applyErase destroys message rows and writes the markers that make it
// permanent.
//
// THE MARKER OUTLIVES EVERY OTHER ROW, which is what lets a node that was away
// tell a message that never existed from one the company destroyed — and what
// stops a redelivered post from writing it back. An erase is the only gesture
// in this domain that removes a message row somebody may still be looking at,
// which is why only an operator may publish one ([MessageErase]) and why it
// installs a gate by its OP rather than by its kind.
func (a *Applier) applyErase(ctx context.Context, tx *sql.Tx, at applyContext,
	p MessageErase) (int, error) {

	// THE ECHOED ROOM MUST BE THE SUBJECT'S. The history row and the wake
	// this produces are read without decoding the id list, so a record
	// whose summary named another room would be an audit trail pointing at
	// a conversation it never touched.
	if p.ChannelID != at.subject().ID {
		return 0, fmt.Errorf("chat: the erase at %s arbitrated room %s and its "+
			"payload echoes %s — the two are read by different readers, and a "+
			"record whose summary disagrees with its effect is one nobody can "+
			"audit", at.position, at.subject().ID, p.ChannelID)
	}
	rows := 0
	for _, id := range p.MessageIDs {
		// A MESSAGE ANOTHER RECORD ALREADY ERASED IS LEFT ALONE, and
		// what that buys is the gesture's COST: without it a repeated
		// 250-id erase issues four statements per id to remove rows
		// that are already gone. The attribution is made permanent by
		// the marker's own conflict clause below rather than by this —
		// the first erasure is the FACT an operator audit reads, and
		// the two guards are deliberately both here because one of
		// them is structural (a marker insert with no conflict clause
		// would abort the transaction on every node) and the other is
		// the rule in one sentence.
		//
		// THE RECORD'S OWN REDELIVERY IS EXCEPTED, by operation id, so
		// it re-asserts its own effect rather than reading it as
		// somebody else's.
		erased, err := messageErased(ctx, tx, id, at.record.OpID)
		if err != nil {
			return 0, err
		}
		if erased {
			continue
		}
		n, err := a.eraseMessage(ctx, tx, at, p, id)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	entry, err := a.writeHistory(ctx, tx, at, p.ChannelID, "")
	if err != nil {
		return 0, err
	}
	a.live.note(at.live(p.ChannelID))
	return rows + entry, nil
}

// eraseMessage removes one message and everything keyed on it.
//
// EVERY CHILD ROW IS NAMED, because a cascade is a delete nobody committed:
// these statements are part of this record's own effect and are therefore
// identical on every node. The schema carries no foreign key at all for
// exactly that reason.
func (a *Applier) eraseMessage(ctx context.Context, tx *sql.Tx, at applyContext,
	p MessageErase, id string) (int, error) {

	// READ BEFORE THE DELETE, because the thread this message belonged to
	// and the person who wrote it are what the participant row is keyed
	// on, and both are gone a statement later.
	var root, author string
	err := tx.QueryRowContext(ctx,
		`SELECT thread_root, author_handle FROM chat_messages WHERE id = ?`, id).
		Scan(&root, &author)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return 0, fmt.Errorf("chat: read message %s before erasing it at %s: %w",
			id, at.position, err)
	}

	rows := 0
	if !missing {
		for _, stmt := range []struct{ table, column string }{
			{"chat_messages", "id"},
			{"chat_mentions", "message_id"},
			{"chat_reactions", "message_id"},
		} {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			res, err := tx.ExecContext(ctx,
				`DELETE FROM `+stmt.table+` WHERE `+stmt.column+` = ?`, id)
			if err != nil {
				return 0, fmt.Errorf("chat: erase message %s from %s at %s: %w",
					id, stmt.table, at.position, err)
			}
			n, _ := res.RowsAffected()
			rows += int(n)
		}
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := dropThreadParticipation(ctx, tx, at, p.ChannelID,
			threadOf(id, root), author)
		if err != nil {
			return 0, err
		}
		rows += n
	}

	// THE MARKER IS WRITTEN EVEN WHEN NO ROW WAS THERE. A message a
	// retention prune already took has no marker of its own, and an
	// operator erasing it is saying it may never come back — which a
	// replay from zero, where the record IS below the prune, is exactly
	// the case that needs saying.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_deletions (message_id, channel_id, erased_at, op_id, by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (message_id) DO NOTHING`,
		id, p.ChannelID, store.EncodeTime(at.brokerAt), at.record.OpID, p.By)
	if err != nil {
		return 0, fmt.Errorf("chat: mark message %s erased at %s: %w",
			id, at.position, err)
	}
	n, _ := res.RowsAffected()
	return rows + int(n), nil
}

// dropThreadParticipation removes a speaker from a thread they no longer have
// a message in.
//
// PARTICIPATION IS DERIVED FROM THE MESSAGES, so it is recomputed from them
// rather than decremented: a counter would have to be right about every
// deletion for the life of the room, and this is right by looking. The two
// queries are the thread's replies and its root — the root's own row carries
// an empty `thread_root`, so one predicate cannot see both without an OR the
// planner would drive on neither index.
func dropThreadParticipation(ctx context.Context, tx *sql.Tx, at applyContext,
	channelID, root, handle string) (int, error) {

	if root == "" || handle == "" {
		return 0, nil
	}
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM chat_messages
		WHERE channel_id = ? AND thread_root = ? AND author_handle = ?
		LIMIT 1`, channelID, root, handle).Scan(&one)
	switch {
	case err == nil:
		return 0, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("chat: count %s's messages in thread %s at %s: %w",
			handle, root, at.position, err)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM chat_messages WHERE id = ? AND author_handle = ?`,
		root, handle).Scan(&one)
	switch {
	case err == nil:
		return 0, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("chat: read thread %s's root at %s: %w",
			root, at.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM chat_thread_participants
		WHERE channel_id = ? AND thread_root = ? AND handle = ?`,
		channelID, root, handle)
	if err != nil {
		return 0, fmt.Errorf("chat: remove %s from thread %s at %s: %w",
			handle, root, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyPrune removes everything in a room stored before a cutoff instant.
//
// # Why a record rather than a sweep
//
// "Older than a year" evaluated against each node's own clock deletes a
// different set on every node, for ever, and nothing would ever report it. The
// cutoff is one number on the log, compared against `created_at` — which is
// the BROKER'S own stored instant rather than anything a writer chose — so
// every node deletes exactly the same rows at exactly the same position.
//
// STRICTLY BEFORE, which is what [Prune] states: a cutoff is a boundary rather
// than a row, and a record that deleted its own boundary instant would remove
// a message a second prune at the same cutoff would not.
//
// EVERY TABLE IS RANGED ON ITS OWN COLUMN, and each of those columns is a copy
// of the parent message's — `message_at`, `root_at` — carried precisely so
// this delete is an index seek rather than a join the planner walks inside the
// transaction holding this store's only writer.
func (a *Applier) applyPrune(ctx context.Context, tx *sql.Tx, at applyContext,
	p Prune) (int, error) {

	channelID := at.subject().ID
	cutoff := store.EncodeTime(p.Cutoff)
	rows := 0
	for _, over := range []struct{ table, column, index string }{
		{"chat_messages", "created_at", "chat_messages_prune_idx"},
		{"chat_mentions", "message_at", "chat_mentions_prune_idx"},
		{"chat_reactions", "message_at", "chat_reactions_prune_idx"},
		{"chat_thread_participants", "root_at", "chat_thread_participants_prune_idx"},
		{"chat_follows", "root_at", "chat_follows_prune_idx"},
		{"chat_history", "broker_at", "chat_history_channel_idx"},
	} {
		// THE INDEX IS NAMED IN THE TABLE ABOVE rather than in a
		// comment beside each statement, so a column that loses its
		// index is a visible pair rather than a delete that quietly
		// became a scan of the corpus.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+over.table+` WHERE channel_id = ? AND `+
				over.column+` < ?`, channelID, cutoff)
		if err != nil {
			return 0, fmt.Errorf("chat: prune %s in room %s at %s (on %s): %w",
				over.table, channelID, at.position, over.index, err)
		}
		n, _ := res.RowsAffected()
		rows += int(n)
	}
	// `chat_deletions` IS NOT PRUNED. A marker is what makes a destruction
	// permanent, and a horizon on it would be the day a redelivered post
	// resurrects what a company erased.
	entry, err := a.writeHistory(ctx, tx, at, channelID, "")
	if err != nil {
		return 0, err
	}
	a.live.note(at.live(channelID))
	return rows + entry, nil
}

// writeMessageBody writes a message's changed text and stamps its version.
//
// ONE STATEMENT FOR THE EDIT AND THE TOMBSTONE, because they are one change to
// one row and two copies of an upsert are two places for the version guard to
// differ. THE DOCUMENT MOVES WITH THE COLUMNS: every reader here decodes it,
// so a body cleared in the column alone is a deletion nobody can see.
//
// `version` AND THE POSITION TRIPLE MOVE TOGETHER, which is what the keyword
// index's forward walk depends on: it reads `version` as a watermark and
// re-reads whatever moved above it, so an edit that changed the body without
// moving the watermark would leave the old text searchable for ever.
func (a *Applier) writeMessageBody(ctx context.Context, tx *sql.Tx,
	at applyContext, message Message) (int, error) {

	document, err := EncodeMessage(message)
	if err != nil {
		return 0, err
	}
	links, err := encodeLinks(message.Links)
	if err != nil {
		return 0, fmt.Errorf("chat: encode the links on message %s at %s: %w",
			message.ID, at.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE chat_messages
		SET body = ?, links = ?, edited_at = ?, deleted_at = ?,
		    log_stream = ?, log_generation = ?, log_seq = ?, version = ?,
		    document = ?
		WHERE id = ? AND version < ?`,
		message.Body, links, nullInstant(message.EditedAt),
		nullInstant(message.DeletedAt), at.position.Stream,
		at.position.Generation, int64(at.position.Seq), at.packed, document,
		message.ID, at.packed)
	if err != nil {
		return 0, fmt.Errorf("chat: write message %s at %s: %w",
			message.ID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// writeMentions writes one row per handle a message named.
//
// THIS IS THE @-MENTION FEED. There is no second mailbox table and no
// per-viewer notification row: a mention is a fact about the message, the feed
// is a range over `chat_mentions_handle_idx`, and an unread count is a counted
// range above a cursor that lives in coordination rather than here.
//
// A MENTION OF SOMEBODY WHO IS NOT IN THE ROOM IS STILL A ROW. Whether it
// woke them is the routing's answer and it rides the record; what the message
// SAID is this row, and a feed built from the woken set alone would render a
// message as though it had never named them.
func writeMentions(ctx context.Context, tx *sql.Tx, at applyContext,
	message Message) (int, error) {

	handles := sortedHandles(message.Mentions)
	messageAt := store.EncodeTime(message.CreatedAt)
	rows, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO chat_mentions
			(message_id, handle, version, channel_id, message_at) VALUES`,
		`(?, ?, ?, ?, ?)`,
		`ON CONFLICT (message_id, handle) DO NOTHING`,
		len(handles), func(i int) []any {
			return []any{message.ID, handles[i], at.packed,
				message.ChannelID, messageAt}
		})
	if err != nil {
		return 0, fmt.Errorf("chat: write message %s's mentions at %s: %w",
			message.ID, at.position, err)
	}
	return rows, nil
}

// writeThreadRows derives who has spoken in a thread and who follows it.
//
// # Two tables, because they answer two questions
//
// A PARTICIPANT has spoken in the thread, and a reply reaches them as
// [ReasonReply], which obliges nothing. A FOLLOWER was NAMED in it, and a
// reply reaches them as [ReasonFollow] — also unaddressed, because a standing
// arrangement is not somebody speaking to you. Collapsing the two would make
// every remark in a thread a standing subscription to it.
//
// BOTH ARE DERIVED BY THE APPLIER rather than authored by the writer, for the
// reason the schema states: participation is a fact about ROWS rather than a
// claim a record can make, and a writer stating it would be stating what it
// read in another transaction.
//
// PARTICIPATION IS WRITTEN ONLY FOR A REPLY, and the root's author is written
// with the first one. A room's every post is not a thread, and a participant
// row per message would be a second row per message in the highest-volume
// table in the engine — for a thread that mostly never happens.
func (a *Applier) writeThreadRows(ctx context.Context, tx *sql.Tx,
	at applyContext, message Message) (int, error) {

	root := threadOf(message.ID, message.ThreadRoot)
	rootAt := message.CreatedAt
	speakers := []string(nil)
	if message.ThreadRoot != "" {
		// THE ROOT'S OWN INSTANT IS WHAT THE PRUNE RANGES ON, so it is
		// read from the root's row rather than taken from this reply:
		// a thread is pruned with the message it hangs off, and a
		// reply that carried its own instant would keep a thread's
		// membership alive a year after its root went.
		//
		// A ROOT THIS NODE NO LONGER HAS is ordinary: an erase or a
		// prune removed it, and the reply is then a thread of its own
		// as far as retention is concerned.
		var created int64
		var author string
		err := tx.QueryRowContext(ctx,
			`SELECT created_at, author_handle FROM chat_messages WHERE id = ?`,
			root).Scan(&created, &author)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return 0, fmt.Errorf("chat: read thread %s's root at %s: %w",
				root, at.position, err)
		default:
			rootAt = store.DecodeTime(created)
			speakers = append(speakers, author)
		}
		speakers = append(speakers, message.Author)
	}

	rows := 0
	if len(speakers) > 0 {
		n, err := store.InsertRows(ctx, tx, at.maxVariables,
			`INSERT INTO chat_thread_participants
				(channel_id, thread_root, handle, first_spoken_at, root_at)
			VALUES`,
			`(?, ?, ?, ?, ?)`,
			// FIRST SPOKEN STAYS FIRST. The row records when
			// somebody joined the conversation, so a later reply
			// must not move it forward.
			`ON CONFLICT (channel_id, thread_root, handle) DO NOTHING`,
			len(speakers), func(i int) []any {
				return []any{message.ChannelID, root, speakers[i],
					store.EncodeTime(message.CreatedAt),
					store.EncodeTime(rootAt)}
			})
		if err != nil {
			return 0, fmt.Errorf("chat: write thread %s's participants at "+
				"%s: %w", root, at.position, err)
		}
		rows += n
	}

	// BEING NAMED SUBSCRIBES YOU TO THE THREAD, and that is the whole
	// producer of this table: there is no follow op in this build's
	// vocabulary, because a subscription nobody can express is one nobody
	// would ever have. A mention on a message that starts no thread still
	// writes the row, keyed on that message as the root it would be — so
	// somebody named in a question hears the answer.
	handles := sortedHandles(message.Mentions)
	followed, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO chat_follows
			(channel_id, thread_root, handle, reason, at, root_at, version)
		VALUES`,
		`(?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (channel_id, thread_root, handle) DO NOTHING`,
		len(handles), func(i int) []any {
			return []any{message.ChannelID, root, handles[i],
				string(ReasonMention), store.EncodeTime(message.CreatedAt),
				store.EncodeTime(rootAt), at.packed}
		})
	if err != nil {
		return 0, fmt.Errorf("chat: write thread %s's followers at %s: %w",
			root, at.position, err)
	}
	return rows + followed, nil
}

// storedMessage is a message row as the applier reads it back: the document,
// plus the ONE value the document deliberately does not carry.
//
// `channel_seq` is minted by the applier from the room's own high-water mark
// and lives in the column alone, so a message's document is a pure function of
// the record that wrote it. Anything here that needs the number — a live
// frame, so a browser can place what it is handed — reads it beside the blob
// rather than inside it.
type storedMessage struct {
	Message

	// Seq is the message's contiguous per-room number.
	Seq int64
}

// readMessage reads one message's current state out of this transaction.
//
// THROUGH THE DOCUMENT, on [readChannel]'s reasoning: it is what carries a
// newer build's fields through this node untouched, and an edit rebuilt from
// columns would drop them.
func readMessage(ctx context.Context, tx *sql.Tx, id string) (storedMessage, bool, error) {
	var document []byte
	var seq int64
	err := tx.QueryRowContext(ctx,
		`SELECT document, channel_seq FROM chat_messages WHERE id = ?`, id).
		Scan(&document, &seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedMessage{}, false, nil
	case err != nil:
		return storedMessage{}, false, fmt.Errorf("chat: read message %s: %w", id, err)
	}
	message, err := DecodeMessage(document)
	if err != nil {
		return storedMessage{}, false, fmt.Errorf("chat: decode message %s: %w",
			id, err)
	}
	return storedMessage{Message: message, Seq: seq}, true, nil
}

// reactionsFull reports whether this message already carries
// [MaxReactionEmoji] DISTINCT emoji and this one is not among them.
//
// THE COUNT IS OF DISTINCT EMOJI, not of rows: the cap is on how many marks a
// message carries, and a hundred people agreeing with one thumb is one mark.
func reactionsFull(ctx context.Context, tx *sql.Tx, r Reaction) (bool, error) {
	var distinct, mine int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT emoji),
		       COUNT(DISTINCT CASE WHEN emoji = ? THEN emoji END)
		FROM chat_reactions WHERE message_id = ?`, r.Emoji, r.MessageID).
		Scan(&distinct, &mine)
	if err != nil {
		return false, fmt.Errorf("chat: count message %s's reactions: %w",
			r.MessageID, err)
	}
	return mine == 0 && distinct >= MaxReactionEmoji, nil
}

// noteMessage records a live frame for a record that changed an existing
// message.
func (a *Applier) noteMessage(at applyContext, message storedMessage) {
	frame := at.live(message.ChannelID)
	frame.MessageID = message.ID
	frame.ChannelSeq = message.Seq
	a.live.note(frame)
}

// threadOf is the thread a message belongs to: its root's id, or its own when
// it IS the root.
//
// ONE SPELLING, because four callers derive it — the participant rows, the
// follow rows, the erase's participation repair and every read that pages a
// thread — and a root computed differently in one of them is a thread that
// exists twice with half the conversation in each.
func threadOf(messageID, threadRoot string) string {
	if threadRoot == "" {
		return messageID
	}
	return threadRoot
}

// sortedHandles is a copy in a canonical order, deduplicated.
//
// THE RECORD'S OWN LIST IS ALREADY IDENTICAL ON EVERY NODE — it is bytes off
// one log — so this is not what makes the rows deterministic, and a comment
// claiming it would be a reason the next reader could disprove. What it buys
// is the two things that are true: the DEDUPLICATION, without which a record
// naming one handle twice collides with itself inside a single multi-row
// statement, and a CANONICAL row order that holds even if a writer one day
// builds this set from a map — which is how every collection in this engine
// has diverged before.
func sortedHandles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// encodeLinks renders what a message points at, as the column holds it.
//
// AN EMPTY STRING RATHER THAN `[]` for a message with no links, which is the
// column's own DEFAULT: the overwhelming majority of messages carry none, and
// a reader tests for empty rather than parsing two bytes per row on the
// highest-volume table in the engine.
func encodeLinks(links []string) (string, error) {
	if len(links) == 0 {
		return "", nil
	}
	data, err := json.Marshal(links)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
