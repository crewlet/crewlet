package chatlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// Waiting is what waits on the reader in one place: a room's own posts, or
// the replies in one of its threads (the package doc's "What waits on a
// reader").
type Waiting struct {
	ChannelID string `json:"channel_id"`

	// RootID is the thread's root message, or empty for the room's own posts.
	RootID string `json:"root_id"`

	Count int `json:"count"`

	// Capped says the room's count stopped at its window, so Count is a floor.
	Capped bool `json:"capped"`
}

// waitingRoomsLimit bounds the rooms one count reads: [MaxRailChannels], the
// most rooms a reader's places are kept for; past it the count is a floor.
const waitingRoomsLimit = MaxRailChannels

// tally is what waits on the reader: the one count the rail's places and
// Activity's unread counts both take ([waiting]).
type tally struct {
	places map[waitPlace]int
	// floors are the rooms whose count stopped at their window.
	floors map[string]bool
	// mentions and replies are the waiting rows Activity lists, by kind.
	mentions, replies int
	capped            bool
}

type waitPlace struct{ channel, root string }

// waitRoom is one room a count reads.
type waitRoom struct {
	id     string
	kind   Kind
	member bool
}

// list is the tally as the rail answers it, sorted.
func (t tally) list() []Waiting {
	out := make([]Waiting, 0, len(t.places))
	for p, n := range t.places {
		out = append(out, Waiting{ChannelID: p.channel, RootID: p.root, Count: n,
			Capped: t.floors[p.channel]})
	}
	slices.SortFunc(out, func(a, b Waiting) int {
		return cmp.Or(cmp.Compare(a.ChannelID, b.ChannelID), cmp.Compare(a.RootID, b.RootID))
	})
	return out
}

// waiting counts what waits on the reader ROOM BY ROOM: every room they are
// in, and every room they may read and are not in where they were named or
// follow a thread. Each is counted over its own newest [UnreadLimit] messages
// ([tally.room]), which hold everything above the reader's place there unless
// all of them are above it, and then the count says it is a floor: read rows
// elsewhere never push an unread one out. known holds rooms' newest messages
// a caller already read, which saves reading them again.
func waiting(ctx context.Context, tx *sql.Tx, who string, marks ReaderMarks,
	known map[string]*MessagePreview) (tally, error) {

	t := tally{places: map[waitPlace]int{}, floors: map[string]bool{}}
	if !marks.Known {
		return t, nil
	}
	rooms, cut, err := waitingRooms(ctx, tx, who, marks)
	if err != nil {
		return tally{}, err
	}
	t.capped = cut
	follows := waitFollows(marks)
	for _, room := range rooms {
		if marks.Muted[room.id] {
			continue
		}
		last, ok := known[room.id]
		if !ok {
			if last, err = lastMessage(ctx, tx, room.id); err != nil {
				return tally{}, err
			}
		}
		// A ROOM WHOSE NEWEST MESSAGE IS READ holds nothing waiting, with no
		// second statement: every caught-up room on every poll.
		if last == nil || last.Position.Packed() <= marks.Cursors[room.id] {
			continue
		}
		if err := t.room(ctx, tx, who, room, marks, follows); err != nil {
			return tally{}, err
		}
	}
	return t, nil
}

// waitFollows is the hand marks as sets: followed by room and root, and
// unfollowed by root, as Activity's reply source reads them.
func waitFollows(marks ReaderMarks) waitMarks {
	out := waitMarks{following: map[waitPlace]bool{}, unfollowed: marks.unfollowed()}
	for _, t := range marks.Following {
		out.following[waitPlace{t.ChannelID, t.RootID}] = true
	}
	return out
}

type waitMarks struct {
	following  map[waitPlace]bool
	unfollowed map[string]bool
}

// waitingRooms is every room [waiting] reads, the reader's own first, and
// whether some were left out past [waitingRoomsLimit]. A room the reader may
// not read is not one ([Visible]).
func waitingRooms(ctx context.Context, tx *sql.Tx, who string,
	marks ReaderMarks) ([]waitRoom, bool, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT channel_id FROM chat_members WHERE handle = ?
		UNION SELECT channel_id FROM chat_mentions WHERE handle = ?
		UNION SELECT channel_id FROM chat_thread_participants WHERE handle = ?
		UNION SELECT channel_id FROM chat_follows WHERE handle = ?`, who, who, who, who)
	if err != nil {
		return nil, false, fmt.Errorf("chatlet: read the rooms that may hold what "+
			"waits on %s: %w", who, err)
	}
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("chatlet: scan a room of %s's: %w", who, err)
		}
		ids[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, false, fmt.Errorf("chatlet: read the rooms of %s's: %w", who, err)
	}
	_ = rows.Close()
	for _, t := range marks.Following {
		ids[t.ChannelID] = true
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	args := []any{who}
	for id := range ids {
		args = append(args, id)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT c.id, c.kind, mb.handle IS NOT NULL
		  FROM chat_channels c
		  LEFT JOIN chat_members mb ON mb.channel_id = c.id AND mb.handle = ?
		 WHERE c.id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("chatlet: read the rooms of %s's: %w", who, err)
	}
	defer func() { _ = rows.Close() }()
	var out []waitRoom
	for rows.Next() {
		var (
			room waitRoom
			kind string
		)
		if err := rows.Scan(&room.id, &kind, &room.member); err != nil {
			return nil, false, fmt.Errorf("chatlet: scan a room of %s's: %w", who, err)
		}
		room.kind = Kind(kind)
		if Visible(who, Channel{Kind: room.kind}, room.member) {
			out = append(out, room)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("chatlet: read the rooms of %s's: %w", who, err)
	}
	slices.SortFunc(out, func(a, b waitRoom) int {
		if a.member != b.member {
			if a.member {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.id, b.id)
	})
	if len(out) > waitingRoomsLimit {
		return out[:waitingRoomsLimit], true, nil
	}
	return out, false, nil
}

// roomWaitingStatement is one room's newest [UnreadLimit] messages, each with
// whether it names the reader and whether they spoke in or were named in its
// thread. LEFT JOINs over the window, never a correlated EXISTS the driver may
// decorrelate over the whole table ([storetest.Decorrelated]).
const roomWaitingStatement = `
	SELECT t.id, t.version, t.thread_root, t.author_handle, t.author_kind,
	       t.deleted_at IS NOT NULL, t.collective,
	       n.handle IS NOT NULL, tp.handle IS NOT NULL, f.handle IS NOT NULL
	  FROM (SELECT id, version, thread_root, author_handle, author_kind,
	               deleted_at, collective, channel_seq
	          FROM chat_messages
	         WHERE channel_id = ? AND event = ''
	         ORDER BY channel_seq DESC
	         LIMIT ?) t
	  LEFT JOIN chat_mentions n ON n.message_id = t.id AND n.handle = ?
	  LEFT JOIN chat_thread_participants tp
	    ON tp.channel_id = ? AND tp.thread_root = t.thread_root AND tp.handle = ?
	  LEFT JOIN chat_follows f
	    ON f.channel_id = ? AND f.thread_root = t.thread_root AND f.handle = ?
	 ORDER BY t.channel_seq DESC`

// room counts what waits on the reader in one room, by the rule Activity
// lists: a message above both its places, not theirs, not taken back, and
// naming them (or the room they are in), replying in a thread they follow,
// or posted in their direct conversation. Each message counts once.
func (t *tally) room(ctx context.Context, tx *sql.Tx, who string, room waitRoom,
	marks ReaderMarks, follows waitMarks) error {

	rows, err := tx.QueryContext(ctx, roomWaitingStatement,
		room.id, UnreadLimit, who, room.id, who, room.id, who)
	if err != nil {
		return fmt.Errorf("chatlet: count what waits on %s in room %s: %w", who, room.id, err)
	}
	defer func() { _ = rows.Close() }()
	cursor := marks.Cursors[room.id]
	seen, above := 0, 0
	for rows.Next() {
		var (
			id, root, author, authorKind              string
			version                                   int64
			deleted, collective, named, spoke, follow bool
		)
		if err := rows.Scan(&id, &version, &root, &author, &authorKind, &deleted,
			&collective, &named, &spoke, &follow); err != nil {
			return fmt.Errorf("chatlet: scan what waits on %s in room %s: %w", who, room.id, err)
		}
		seen++
		if version <= cursor {
			continue
		}
		above++
		own := author == who && AuthorKind(authorKind) != AuthorSystem
		if own || deleted || (root != "" && version <= marks.ThreadCursors[root]) {
			continue
		}
		mention := named || (collective && room.member)
		reply := root != "" && !follows.unfollowed[root] &&
			(spoke || follow || follows.following[waitPlace{room.id, root}])
		switch {
		case mention:
			t.mentions++
		case reply:
			t.replies++
		case root == "" && room.kind.Direct():
		default:
			continue
		}
		t.places[waitPlace{room.id, root}]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chatlet: count what waits on %s in room %s: %w", who, room.id, err)
	}
	// EVERY MESSAGE THE WINDOW HOLDS IS ABOVE THE READER'S PLACE, so older
	// ones may be too: the count is a floor.
	if seen == UnreadLimit && above == seen {
		t.floors[room.id] = true
		t.capped = true
	}
	return nil
}
