package chatlet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The reader's own views of the company's chat, each answered for one seat and
// each reaching across rooms: Activity (what waits for them), the threads they
// follow, what they saved, what they sent, a room's shared links, and one
// message read in place.
//
// # The reader's record arrives as a value
//
// Where somebody has read to, which threads they marked and what they saved
// are their read state, kept in coordination rather than on this log
// (internal/coord's chat read record). It reaches these reads as a value
// ([ReaderMarks]) for the reason [ChannelsQuery.Cursors] does: a reader that
// dialled coordination itself would put a fleet round trip inside every read
// and give this package a second store to be wrong about. A record that could
// not be read is said ([ReaderMarks.Known]) rather than taken for an empty
// one, which would call everything unread.
//
// # Every row passes [Visible]
//
// Each read narrows in SQL with [visibleWhere] so that a LIMIT counts only rows
// the reader may see, and decides every row with [Visible] in Go, the one rule
// for who may read a room. A message the reader saved, a thread they once
// spoke in and a mention of them in a room they have since left are each
// answered exactly as a room that does not exist: nothing.
//
// # Room events are not things somebody said
//
// A room's own change (who joined, a new topic) is a row in `chat_messages`
// with `event` set, so the transcript draws it in place. None of these views
// lists one: an event names nobody, nobody replies to it, and it carries no
// link.

// ReaderMarks is what the reader's own record says that a read here needs.
type ReaderMarks struct {
	// Known is false when the record could not be read: no row is then
	// called unread, and a thread the reader marked by hand is unknown.
	Known bool

	// Cursors and ThreadCursors are the PACKED positions read through, by
	// room and by thread root. A row is unread above both of its own.
	Cursors       map[string]int64
	ThreadCursors map[string]int64

	// Following and Unfollowed are the threads marked by hand. A mark
	// decides; without one, a thread the reader started, replied in or was
	// named in is followed.
	Following  []ThreadRef
	Unfollowed []ThreadRef
}

// ThreadRef names one thread by its room and its root message.
type ThreadRef struct {
	ChannelID string
	RootID    string
}

// unreadAbove reports whether a row at position, in channel and thread, is
// above both of the reader's places for it.
func (m ReaderMarks) unreadAbove(channel, thread string, position int64) bool {
	if !m.Known {
		return false
	}
	read := m.Cursors[channel]
	if thread != "" {
		read = max(read, m.ThreadCursors[thread])
	}
	return position > read
}

// unfollowed is the set of thread roots marked not followed.
func (m ReaderMarks) unfollowed() map[string]bool {
	out := make(map[string]bool, len(m.Unfollowed))
	for _, t := range m.Unfollowed {
		out[t.RootID] = true
	}
	return out
}

// ---- Activity ------------------------------------------------------------ //

// ActivityKind is why an Activity row is there.
type ActivityKind string

const (
	// ActivityMention is a message that named the reader, or the whole
	// room they are in.
	ActivityMention ActivityKind = "mention"
	// ActivityReply is a reply in a thread the reader follows that did not
	// name them.
	ActivityReply ActivityKind = "reply"
	// ActivityReaction is somebody else's reaction to the reader's message.
	ActivityReaction ActivityKind = "reaction"
)

// ActivityKinds are the three, in the order a screen's tabs list them.
var ActivityKinds = []ActivityKind{ActivityMention, ActivityReply, ActivityReaction}

// Valid reports whether k is a kind this build knows.
func (k ActivityKind) Valid() bool { return slices.Contains(ActivityKinds, k) }

// ActivityQuery asks for a page of the reader's Activity.
type ActivityQuery struct {
	// Kind is one of the three, or empty for all of them.
	Kind ActivityKind

	// Unread asks for the unread mentions and replies alone: the newest
	// [UnreadLimit] of each, whole, with no cursor. Refused over a record
	// that could not be read, whose unread nobody can tell.
	Unread bool

	Marks ReaderMarks

	// Cursor is [Activity.NextCursor] from the page before, unchanged.
	Cursor string

	Limit int
}

// ActivityItem is one row of the reader's Activity.
type ActivityItem struct {
	// Key is unique in the feed and stable across reads.
	Key  string       `json:"key"`
	Kind ActivityKind `json:"kind"`

	// MessageID is the message the row is about: the one that named the
	// reader, the reply, or the reader's own message somebody reacted to.
	MessageID   string `json:"message_id"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	ChannelKind Kind   `json:"channel_kind"`
	ThreadRoot  string `json:"thread_root,omitempty"`

	// Actor is who did it; ActorKind is absent on a reaction, which
	// records only who reacted.
	Actor     string     `json:"actor"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`

	// Collective marks a mention of the whole room rather than of the
	// reader.
	Collective bool `json:"collective,omitempty"`

	// Excerpt is what the message says now, cut at [MaxExcerpt] bytes.
	Excerpt string `json:"excerpt,omitempty" untrusted:"fence"`
	Emoji   string `json:"emoji,omitempty"`

	// At is when the reader was named, the reply was said or the reaction
	// was made: the broker's instant, which is the feed's order.
	At time.Time `json:"at"`

	// Unread marks a mention or a reply above the reader's places for it.
	Unread bool `json:"unread,omitempty"`

	// ReadThrough is the PACKED position that, sent as the room's cursor,
	// marks this row read. Zero (absent) on a reaction.
	ReadThrough int64 `json:"read_through,omitempty"`
}

// ActivityCounts is what waits in Activity, for the badges.
type ActivityCounts struct {
	Mention int  `json:"mention"`
	Reply   int  `json:"reply"`
	Capped  bool `json:"capped,omitempty"`
}

// Activity is a page of the reader's Activity, newest first.
type Activity struct {
	Served

	Items      []ActivityItem `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`

	// Unread counts the unread mentions and replies over the newest
	// [UnreadLimit] of each, whatever the query's kind and cursor.
	Unread ActivityCounts `json:"unread"`

	// ReadState is [ReaderMarks.Known].
	ReadState bool `json:"read_state"`
}

// activityKey orders the feed: the instant, newest first, and then the row's
// key, which is unique and carries its kind's prefix, so two sources whose
// rows share an instant interleave the same way on every read.
type activityKey struct {
	at  int64 // the broker's instant, as the store encodes it
	key string
}

// before reports whether a sorts ahead of b in the feed (newer first).
func (a activityKey) before(b activityKey) bool {
	if a.at != b.at {
		return a.at > b.at
	}
	return a.key > b.key
}

func (a activityKey) String() string {
	return strconv.FormatInt(a.at, 10) + ":" + a.key
}

// parseActivityCursor reads [Activity.NextCursor] back.
func parseActivityCursor(raw string) (*activityKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	at, key, found := strings.Cut(raw, ":")
	micros, err := strconv.ParseInt(at, 10, 64)
	if !found || err != nil || key == "" {
		return nil, invalid("cursor", "%q is not an Activity cursor; send back the "+
			"`next_cursor` the page before answered, unchanged", raw)
	}
	return &activityKey{at: micros, key: key}, nil
}

// activityRow is one row a source read, with what the feed orders and marks it
// by.
type activityRow struct {
	item     ActivityItem
	order    activityKey
	position int64 // the record the row is about, for the unread mark
}

// activityWindow is what one source read: its rows after the reader's own
// filters, and where its scan stopped when it filled its window.
type activityWindow struct {
	rows      []activityRow
	truncated bool
	last      activityKey
}

// Activity answers a page of the reader's Activity: mentions of them, replies
// in threads they follow, and reactions to their messages, newest first.
//
// # Three sources, merged by instant
//
// Each kind is a range over an index of its own (the mention feed by when the
// reader was named, the replies of the threads they spoke or were named in,
// the reactions to their messages by their author), read newest first into a
// window one longer than the page, and the three are merged. A source that
// filled its window may have more rows newer than another source's oldest,
// so the page ends at the newest of the full windows' last rows: nothing older
// than that is answered before that source has been read past it.
//
// A reply that also names the reader is a mention and is listed once, as the
// mention.
func (r *Reader) Activity(ctx context.Context, viewer string, q ActivityQuery,
	fresh statelog.Freshness) (Activity, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return Activity{}, err
	}
	if fresh.Level == "" {
		return Activity{}, errNoLevel("an Activity read")
	}
	if q.Kind != "" && !q.Kind.Valid() {
		return Activity{}, invalid("kind", "%q is not an Activity kind; send "+
			"mention, reply or reaction, or nothing for all three", q.Kind)
	}
	if q.Unread && !q.Marks.Known {
		return Activity{}, invalid("unread", "which rows are unread cannot be "+
			"told while the read state cannot be read; ask without `unread`")
	}
	after, err := parseActivityCursor(q.Cursor)
	if err != nil {
		return Activity{}, err
	}
	if q.Unread && after != nil {
		return Activity{}, invalid("cursor", "an unread read answers the newest "+
			"unread rows whole and takes no cursor")
	}
	out := Activity{Items: []ActivityItem{}, ReadState: q.Marks.Known}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			counts, err := activityCounts(ctx, tx, who, q.Marks)
			if err != nil {
				return err
			}
			out.Unread = counts
			window, after := limitOf(q.Limit), after
			if q.Unread {
				window, after = UnreadLimit, nil
			}
			windows, err := activitySources(ctx, tx, who, q.Kind, q.Marks, after, window)
			if err != nil {
				return err
			}
			rows, next := mergeActivity(windows, window)
			for _, row := range rows {
				if q.Unread && !row.item.Unread {
					continue
				}
				out.Items = append(out.Items, row.item)
			}
			if !q.Unread && next != nil {
				out.NextCursor = next.String()
			}
			return nil
		})
	if err != nil {
		return Activity{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// activityCounts counts the unread mentions and replies over the newest
// [UnreadLimit] of each.
func activityCounts(ctx context.Context, tx *sql.Tx, who string,
	marks ReaderMarks) (ActivityCounts, error) {

	var counts ActivityCounts
	if !marks.Known {
		return counts, nil
	}
	for _, kind := range []ActivityKind{ActivityMention, ActivityReply} {
		windows, err := activitySources(ctx, tx, who, kind, marks, nil, UnreadLimit)
		if err != nil {
			return ActivityCounts{}, err
		}
		rows, _ := mergeActivity(windows, UnreadLimit)
		n := 0
		for _, row := range rows {
			if row.item.Unread {
				n++
			}
		}
		if n >= UnreadLimit {
			counts.Capped = true
		}
		if kind == ActivityMention {
			counts.Mention = n
		} else {
			counts.Reply = n
		}
	}
	return counts, nil
}

// activitySources reads each source the kind asks for, newest first, each into
// a window of limit+1 rows strictly after the cursor.
func activitySources(ctx context.Context, tx *sql.Tx, who string, kind ActivityKind,
	marks ReaderMarks, after *activityKey, limit int) ([]activityWindow, error) {

	var windows []activityWindow
	read := func(source func(context.Context, *sql.Tx, string, ReaderMarks,
		*activityKey, int) (activityWindow, error)) error {

		w, err := source(ctx, tx, who, marks, after, limit)
		if err != nil {
			return err
		}
		windows = append(windows, w)
		return nil
	}
	if kind == "" || kind == ActivityMention {
		if err := read(mentionRows); err != nil {
			return nil, err
		}
		if err := read(collectiveRows); err != nil {
			return nil, err
		}
	}
	if kind == "" || kind == ActivityReply {
		if err := read(replyRows); err != nil {
			return nil, err
		}
	}
	if kind == "" || kind == ActivityReaction {
		if err := read(reactionRows); err != nil {
			return nil, err
		}
	}
	return windows, nil
}

// mergeActivity merges the sources' windows into one page of at most limit
// rows, and the cursor of the page after it, nil at the end.
func mergeActivity(windows []activityWindow, limit int) ([]activityRow, *activityKey) {
	var all []activityRow
	var floor *activityKey
	for _, w := range windows {
		all = append(all, w.rows...)
		if w.truncated && (floor == nil || w.last.before(*floor)) {
			last := w.last
			floor = &last
		}
	}
	slices.SortFunc(all, func(a, b activityRow) int {
		return compareKeys(a.order, b.order)
	})
	// A ROW OLDER THAN A FULL WINDOW'S LAST waits for that window to be
	// read past it, since the full source may hold rows between the two.
	if floor != nil {
		all = slices.DeleteFunc(all, func(r activityRow) bool {
			return floor.before(r.order)
		})
	}
	if len(all) > limit {
		next := all[limit-1].order
		return all[:limit], &next
	}
	return all, floor
}

// afterClause is the keyset predicate of one source: rows strictly after the
// cursor in the feed's order, over the source's instant column and its key
// expression. Nothing when there is no cursor.
func afterClause(after *activityKey, at, key string) (string, []any) {
	if after == nil {
		return "", nil
	}
	return ` AND (` + at + ` < ? OR (` + at + ` = ? AND ` + key + ` < ?))`,
		[]any{after.at, after.at, after.key}
}

// windowOf turns one source's scanned rows into its window.
func windowOf(scanned []activityRow, kept []activityRow, limit int) activityWindow {
	w := activityWindow{rows: kept}
	if len(scanned) > limit {
		w.truncated = true
		w.last = scanned[limit-1].order
		w.rows = slices.DeleteFunc(w.rows, func(r activityRow) bool {
			return w.last.before(r.order)
		})
	}
	return w
}

// mentionRows reads the moments the reader was named, by when.
func mentionRows(ctx context.Context, tx *sql.Tx, who string, marks ReaderMarks,
	after *activityKey, limit int) (activityWindow, error) {

	const key = `('m:' || n.message_id)`
	clause, cursorArgs := afterClause(after, "n.named_at", key)
	args := append([]any{who}, cursorArgs...)
	args = append(args, who, limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT n.message_id, n.named_at, n.version, n.channel_id, c.name, c.kind,
		       m.thread_root, m.author_handle, m.author_kind, m.body, `+key+`
		  FROM chat_mentions n
		  JOIN chat_messages m ON m.id = n.message_id
		  JOIN chat_channels c ON c.id = n.channel_id
		 WHERE n.handle = ?`+clause+` AND m.event = '' AND `+visibleWhere+`
		 ORDER BY n.named_at DESC, `+key+` DESC
		 LIMIT ?`, args...)
	if err != nil {
		return activityWindow{}, fmt.Errorf("chatlet: read %s's mentions for "+
			"Activity: %w", who, err)
	}
	return scanMessageRows(rows, who, ActivityMention, marks, limit, false)
}

// collectiveRows reads the messages that named the whole of a room the reader
// is in: `@channel`, which names nobody in `chat_mentions`.
func collectiveRows(ctx context.Context, tx *sql.Tx, who string, marks ReaderMarks,
	after *activityKey, limit int) (activityWindow, error) {

	const key = `('m:' || m.id)`
	clause, cursorArgs := afterClause(after, "m.created_at", key)
	args := append([]any{who, who}, cursorArgs...)
	args = append(args, who, limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.created_at, m.version, m.channel_id, c.name, c.kind,
		       m.thread_root, m.author_handle, m.author_kind, m.body, `+key+`
		  FROM chat_members mb
		  JOIN chat_messages m ON m.channel_id = mb.channel_id AND m.collective = 1
		  JOIN chat_channels c ON c.id = m.channel_id
		 WHERE mb.handle = ? AND m.author_handle <> ? AND m.deleted_at IS NULL
		   AND m.event = ''`+clause+` AND `+visibleWhere+`
		 ORDER BY m.created_at DESC, `+key+` DESC
		 LIMIT ?`, args...)
	if err != nil {
		return activityWindow{}, fmt.Errorf("chatlet: read the messages that "+
			"named %s's rooms for Activity: %w", who, err)
	}
	return scanMessageRows(rows, who, ActivityMention, marks, limit, true)
}

// replyRows reads the replies in the threads the reader follows: the threads
// they spoke or were named in, and those they followed by hand, less those
// they unfollowed. A reply that names them is the mention's, and one they
// wrote is not news to them.
func replyRows(ctx context.Context, tx *sql.Tx, who string, marks ReaderMarks,
	after *activityKey, limit int) (activityWindow, error) {

	const key = `('t:' || m.id)`
	clause, cursorArgs := afterClause(after, "m.created_at", key)
	scanned := make([]activityRow, 0, limit+1)
	kept := make([]activityRow, 0, limit+1)
	unfollowed := marks.unfollowed()
	collect := func(w activityWindow, all []activityRow) {
		scanned = append(scanned, all...)
		for _, row := range w.rows {
			if !unfollowed[row.item.ThreadRoot] {
				kept = append(kept, row)
			}
		}
	}

	// THE THREADS THE CONVERSATION SAYS THEY ARE IN, in one statement over
	// the two handle indexes.
	args := append([]any{who, who, who, who}, cursorArgs...)
	args = append(args, who, limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.created_at, m.version, m.channel_id, c.name, c.kind,
		       m.thread_root, m.author_handle, m.author_kind, m.body, `+key+`
		  FROM (SELECT channel_id, thread_root FROM chat_thread_participants
		         WHERE handle = ?
		        UNION
		        SELECT channel_id, thread_root FROM chat_follows WHERE handle = ?) t
		  JOIN chat_messages m
		    ON m.channel_id = t.channel_id AND m.thread_root = t.thread_root
		  JOIN chat_channels c ON c.id = m.channel_id
		 WHERE m.author_handle <> ? AND m.deleted_at IS NULL AND m.event = ''
		   AND NOT EXISTS (SELECT 1 FROM chat_mentions n
		                    WHERE n.message_id = m.id AND n.handle = ?)`+clause+`
		   AND `+visibleWhere+`
		 ORDER BY m.created_at DESC, `+key+` DESC
		 LIMIT ?`, args...)
	if err != nil {
		return activityWindow{}, fmt.Errorf("chatlet: read the replies in %s's "+
			"threads for Activity: %w", who, err)
	}
	w, all, err := scanReplyRows(rows, who, marks, limit)
	if err != nil {
		return activityWindow{}, err
	}
	collect(w, all)

	// AND EACH THREAD FOLLOWED BY HAND, one range on the thread index each:
	// a mark is a deliberate gesture, there are at most [MaxReaderMarks] of
	// them and few are of a thread nobody named the reader in.
	for _, thread := range marks.Following {
		args := append([]any{thread.ChannelID, thread.RootID, who, who}, cursorArgs...)
		args = append(args, who, limit+1)
		rows, err := tx.QueryContext(ctx, `
			SELECT m.id, m.created_at, m.version, m.channel_id, c.name, c.kind,
			       m.thread_root, m.author_handle, m.author_kind, m.body, `+key+`
			  FROM chat_messages m
			  JOIN chat_channels c ON c.id = m.channel_id
			 WHERE m.channel_id = ? AND m.thread_root = ?
			   AND m.author_handle <> ? AND m.deleted_at IS NULL AND m.event = ''
			   AND NOT EXISTS (SELECT 1 FROM chat_mentions n
			                    WHERE n.message_id = m.id AND n.handle = ?)`+clause+`
			   AND `+visibleWhere+`
			 ORDER BY m.created_at DESC, `+key+` DESC
			 LIMIT ?`, args...)
		if err != nil {
			return activityWindow{}, fmt.Errorf("chatlet: read the replies in "+
				"thread %s for Activity: %w", thread.RootID, err)
		}
		w, all, err := scanReplyRows(rows, who, marks, limit)
		if err != nil {
			return activityWindow{}, err
		}
		collect(w, all)
	}

	// ONE WINDOW OF THEM ALL: a reply in a thread both spoken in and marked
	// is one row, and the window is the newest limit+1 of the union.
	scanned = dedupeRows(scanned)
	kept = dedupeRows(kept)
	if len(scanned) > limit+1 {
		scanned = scanned[:limit+1]
	}
	return windowOf(scanned, kept, limit), nil
}

// dedupeRows sorts rows in the feed's order and keeps one of each key.
func dedupeRows(rows []activityRow) []activityRow {
	slices.SortFunc(rows, func(a, b activityRow) int {
		return compareKeys(a.order, b.order)
	})
	return slices.CompactFunc(rows, func(a, b activityRow) bool {
		return a.order == b.order
	})
}

// reactionRows reads the reactions others made to the reader's messages, by
// when they were made.
func reactionRows(ctx context.Context, tx *sql.Tx, who string, _ ReaderMarks,
	after *activityKey, limit int) (activityWindow, error) {

	const key = `('r:' || r.message_id || ':' || r.handle || ':' || r.emoji)`
	clause, cursorArgs := afterClause(after, "r.at", key)
	args := append([]any{who, who}, cursorArgs...)
	args = append(args, who, limit+1)
	rows, err := tx.QueryContext(ctx, `
		SELECT r.message_id, r.emoji, r.handle, r.at, m.channel_id, c.name, c.kind,
		       m.thread_root, m.body, `+key+`
		  FROM chat_reactions r
		  JOIN chat_messages m ON m.id = r.message_id
		  JOIN chat_channels c ON c.id = m.channel_id
		 WHERE r.message_author = ? AND r.handle <> ? AND m.deleted_at IS NULL`+clause+`
		   AND `+visibleWhere+`
		 ORDER BY r.at DESC, `+key+` DESC
		 LIMIT ?`, args...)
	if err != nil {
		return activityWindow{}, fmt.Errorf("chatlet: read the reactions to %s's "+
			"messages for Activity: %w", who, err)
	}
	defer func() { _ = rows.Close() }()
	var scanned []activityRow
	for rows.Next() {
		var (
			row        activityRow
			at         int64
			name, kind string
			body       string
		)
		item := &row.item
		if err := rows.Scan(&item.MessageID, &item.Emoji, &item.Actor, &at,
			&item.ChannelID, &name, &kind, &item.ThreadRoot, &body,
			&row.order.key); err != nil {

			return activityWindow{}, fmt.Errorf("chatlet: scan a reaction to %s's "+
				"messages: %w", who, err)
		}
		item.Kind, item.Key = ActivityReaction, row.order.key
		item.ChannelName, item.ChannelKind = name, Kind(kind)
		item.Excerpt, item.At = Excerpt(body), store.DecodeTime(at)
		row.order.at = at
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		return activityWindow{}, fmt.Errorf("chatlet: read the reactions to %s's "+
			"messages: %w", who, err)
	}
	return windowOf(scanned, visibleRows(who, scanned, limit), limit), nil
}

// scanMessageRows scans a mention source's rows: a message, its room and the
// position the unread mark is measured by.
func scanMessageRows(rows *sql.Rows, who string, kind ActivityKind,
	marks ReaderMarks, limit int, collective bool) (activityWindow, error) {

	all, err := scanActivityMessages(rows, who, kind, marks, collective)
	if err != nil {
		return activityWindow{}, err
	}
	return windowOf(all, visibleRows(who, all, limit), limit), nil
}

// scanReplyRows scans a reply source's rows, answering both its window and
// every row it scanned, which the union of the reply sources is cut from.
func scanReplyRows(rows *sql.Rows, who string, marks ReaderMarks,
	limit int) (activityWindow, []activityRow, error) {

	all, err := scanActivityMessages(rows, who, ActivityReply, marks, false)
	if err != nil {
		return activityWindow{}, nil, err
	}
	return activityWindow{rows: visibleRows(who, all, limit)}, all, nil
}

// scanActivityMessages reads the rows of a message source, closing them.
func scanActivityMessages(rows *sql.Rows, who string, kind ActivityKind,
	marks ReaderMarks, collective bool) ([]activityRow, error) {

	defer func() { _ = rows.Close() }()
	var out []activityRow
	for rows.Next() {
		var (
			row                    activityRow
			at                     int64
			name, roomKind, author string
			body                   string
		)
		item := &row.item
		if err := rows.Scan(&item.MessageID, &at, &row.position, &item.ChannelID,
			&name, &roomKind, &item.ThreadRoot, &item.Actor, &author, &body,
			&row.order.key); err != nil {

			return nil, fmt.Errorf("chatlet: scan an Activity row for %s: %w", who, err)
		}
		item.Kind, item.Key, item.Collective = kind, row.order.key, collective
		item.ChannelName, item.ChannelKind = name, Kind(roomKind)
		item.ActorKind, item.Excerpt = AuthorKind(author), Excerpt(body)
		item.At = store.DecodeTime(at)
		item.Unread = marks.unreadAbove(item.ChannelID, item.ThreadRoot, row.position)
		item.ReadThrough = row.position
		row.order.at = at
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read Activity for %s: %w", who, err)
	}
	return out, nil
}

// visibleRows is the first limit+1 rows the reader may read, decided by
// [Visible]: the SQL narrowed, and this decides.
func visibleRows(who string, rows []activityRow, limit int) []activityRow {
	kept := make([]activityRow, 0, min(len(rows), limit+1))
	for _, row := range rows {
		if Visible(who, Channel{Kind: row.item.ChannelKind}, true) {
			kept = append(kept, row)
		}
	}
	return kept
}

// ---- the threads the reader follows -------------------------------------- //

// FollowReason is why the reader follows a thread, the strongest first.
type FollowReason string

const (
	// FollowMarked is a thread the reader followed by hand.
	FollowMarked FollowReason = "marked"
	// FollowStarted is a thread whose root the reader wrote.
	FollowStarted FollowReason = "started"
	// FollowReplied is a thread the reader replied in.
	FollowReplied FollowReason = "replied"
	// FollowNamed is a thread a message named the reader in.
	FollowNamed FollowReason = "named"
)

// FollowReasons are the four, the strongest first.
var FollowReasons = []FollowReason{FollowMarked, FollowStarted, FollowReplied, FollowNamed}

// Valid reports whether f is a reason this build knows.
func (f FollowReason) Valid() bool { return slices.Contains(FollowReasons, f) }

// ThreadsQuery asks for a page of the threads the reader follows.
type ThreadsQuery struct {
	Marks ReaderMarks

	// Cursor is [FollowedThreads.NextCursor] from the page before.
	Cursor string

	// Limit is how many threads a page holds: [DefaultThreadsLimit] when
	// unset, [MaxThreadsLimit] at the top.
	Limit int
}

// The bounds of a page of followed threads: each carries its root and its
// newest replies, so a page is three messages a thread, and twenty is a
// screen.
const (
	DefaultThreadsLimit = 20
	MaxThreadsLimit     = 50

	// threadLatest is how many of a thread's newest replies it carries.
	threadLatest = 2
)

// FollowedThread is one thread the reader follows.
type FollowedThread struct {
	ChannelID   string       `json:"channel_id"`
	ChannelName string       `json:"channel_name,omitempty"`
	ChannelKind Kind         `json:"channel_kind"`
	Reason      FollowReason `json:"reason"`

	// Root carries the thread line ([MessageView.Thread]).
	Root MessageView `json:"root"`

	// Latest are the newest replies, oldest first.
	Latest []MessageView `json:"latest"`
}

// FollowedThreads is a page of the threads the reader follows, newest reply
// first.
type FollowedThreads struct {
	Served

	Threads    []FollowedThread `json:"threads"`
	NextCursor string           `json:"next_cursor,omitempty"`

	// ReadState is [ReaderMarks.Known]: without it the hand marks are
	// unknown.
	ReadState bool `json:"read_state"`
}

// followedThread is one candidate thread before its messages are read.
type followedThread struct {
	channelID, root string
	name            string
	kind            Kind
	reason          FollowReason
	order           activityKey // the newest reply's instant, then the root
}

// Threads answers a page of the threads the reader follows, newest reply
// first.
//
// THE CANDIDATES ARE READ WHOLE AND PAGED HERE: the order is each thread's
// newest reply, which no index holds, so the threads the reader spoke or was
// named in are aggregated in one statement over the handle indexes (bounded by
// what one seat says within the room's horizon), joined by the threads marked
// by hand, less the ones unmarked, and sorted. Only the page's threads have
// their messages read.
func (r *Reader) Threads(ctx context.Context, viewer string, q ThreadsQuery,
	fresh statelog.Freshness) (FollowedThreads, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return FollowedThreads{}, err
	}
	if fresh.Level == "" {
		return FollowedThreads{}, errNoLevel("a followed threads read")
	}
	after, err := parseActivityCursor(q.Cursor)
	if err != nil {
		return FollowedThreads{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultThreadsLimit
	}
	limit = min(limit, MaxThreadsLimit)
	out := FollowedThreads{Threads: []FollowedThread{}, ReadState: q.Marks.Known}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			candidates, err := followedCandidates(ctx, tx, who, q.Marks)
			if err != nil {
				return err
			}
			if after != nil {
				candidates = slices.DeleteFunc(candidates, func(c followedThread) bool {
					return !after.before(c.order)
				})
			}
			held := 0
			for i, c := range candidates {
				if len(out.Threads) == limit || (len(out.Threads) > 0 && held > pageBytes) {
					out.NextCursor = candidates[i-1].order.String()
					break
				}
				thread, size, found, err := readFollowedThread(ctx, tx, who, c)
				if err != nil {
					return err
				}
				if found {
					out.Threads = append(out.Threads, thread)
					held += size
				}
			}
			return nil
		})
	if err != nil {
		return FollowedThreads{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// followedCandidates is every thread the reader follows, newest reply first.
func followedCandidates(ctx context.Context, tx *sql.Tx, who string,
	marks ReaderMarks) ([]followedThread, error) {

	byRoot := map[string]*followedThread{}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.channel_id, t.thread_root, MIN(t.reason), MAX(m.created_at),
		       c.name, c.kind
		  FROM (SELECT channel_id, thread_root, 'p' AS reason
		          FROM chat_thread_participants WHERE handle = ?
		        UNION
		        SELECT channel_id, thread_root, 'q' AS reason
		          FROM chat_follows WHERE handle = ?) t
		  JOIN chat_messages m
		    ON m.channel_id = t.channel_id AND m.thread_root = t.thread_root
		  JOIN chat_channels c ON c.id = t.channel_id
		 WHERE m.event = '' AND `+visibleWhere+`
		 GROUP BY t.channel_id, t.thread_root, c.name, c.kind`, who, who, who)
	if err != nil {
		return nil, fmt.Errorf("chatlet: read the threads %s follows: %w", who, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c          followedThread
			reason     string
			last       int64
			name, kind string
		)
		if err := rows.Scan(&c.channelID, &c.root, &reason, &last, &name, &kind); err != nil {
			return nil, fmt.Errorf("chatlet: scan a thread %s follows: %w", who, err)
		}
		c.name, c.kind = name, Kind(kind)
		// 'p' SORTS BEFORE 'q', so MIN is "spoke in it" whenever they did;
		// whether they started it is the root's author, read with the root.
		c.reason = FollowReplied
		if reason == "q" {
			c.reason = FollowNamed
		}
		c.order = activityKey{at: last, key: c.root}
		if Visible(who, Channel{Kind: c.kind}, true) {
			byRoot[c.root] = &c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatlet: read the threads %s follows: %w", who, err)
	}
	_ = rows.Close()

	for _, marked := range marks.Following {
		c, err := markedThread(ctx, tx, who, marked)
		if err != nil {
			return nil, err
		}
		if c != nil {
			byRoot[c.root] = c
		}
	}
	for root := range marks.unfollowed() {
		delete(byRoot, root)
	}
	out := make([]followedThread, 0, len(byRoot))
	for _, c := range byRoot {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b followedThread) int {
		return compareKeys(a.order, b.order)
	})
	return out, nil
}

// markedThread is a thread followed by hand, ordered by its newest reply or,
// with none yet, by its root: nil when the root is not there or the reader may
// not read its room.
func markedThread(ctx context.Context, tx *sql.Tx, who string,
	marked ThreadRef) (*followedThread, error) {

	var (
		at         int64
		name, kind string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT m.created_at, c.name, c.kind
		  FROM chat_messages m
		  JOIN chat_channels c ON c.id = m.channel_id
		 WHERE m.channel_id = ? AND (m.id = ? OR m.thread_root = ?) AND m.event = ''
		   AND `+visibleWhere+`
		 ORDER BY m.channel_seq DESC
		 LIMIT 1`, marked.ChannelID, marked.RootID, marked.RootID, who).
		Scan(&at, &name, &kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("chatlet: read thread %s %s marked: %w",
			marked.RootID, who, err)
	}
	if !Visible(who, Channel{Kind: Kind(kind)}, true) {
		return nil, nil
	}
	return &followedThread{channelID: marked.ChannelID, root: marked.RootID,
		name: name, kind: Kind(kind), reason: FollowMarked,
		order: activityKey{at: at, key: marked.RootID}}, nil
}

// readFollowedThread reads one thread's root and its newest replies, and how
// many bytes of words they carry, which the page is bounded by.
func readFollowedThread(ctx context.Context, tx *sql.Tx, who string,
	c followedThread) (FollowedThread, int, bool, error) {

	root, err := readMessagePage(ctx, tx, who,
		[]string{"channel_id = ?", "id = ?"}, []any{c.channelID, c.root},
		"channel_seq DESC", 1)
	if err != nil {
		return FollowedThread{}, 0, false, err
	}
	if len(root.views) == 0 {
		// A ROOT PRUNED, ERASED OR UNREADABLE BY THIS BUILD names no thread
		// this read can draw.
		return FollowedThread{}, 0, false, nil
	}
	latest, err := readMessagePage(ctx, tx, who,
		[]string{"channel_id = ?", "thread_root = ?", "event = ''"},
		[]any{c.channelID, c.root}, "channel_seq DESC", threadLatest)
	if err != nil {
		return FollowedThread{}, 0, false, err
	}
	slices.Reverse(latest.views)
	reason := c.reason
	if reason != FollowMarked && root.views[0].Message.Author == who {
		reason = FollowStarted
	}
	thread := FollowedThread{ChannelID: c.channelID, ChannelName: c.name,
		ChannelKind: c.kind, Reason: reason, Root: root.views[0],
		Latest: latest.views}
	size := len(thread.Root.Message.Body)
	for _, v := range thread.Latest {
		size += len(v.Message.Body)
	}
	return thread, size, true, nil
}

// ---- saved messages ------------------------------------------------------ //

// SavedMark is one message the reader saved, as their record holds it.
type SavedMark struct {
	ChannelID string    `json:"channel_id"`
	MessageID string    `json:"message_id"`
	At        time.Time `json:"at"`
}

// SavedQuery asks for a page of the reader's saved messages.
type SavedQuery struct {
	// Saved are the reader's marks, the newest save first, as their record
	// orders them.
	Saved []SavedMark

	Cursor string
	Limit  int
}

// SavedEntry is one saved message as the Saved view lists it.
type SavedEntry struct {
	Mark        SavedMark    `json:"mark"`
	ChannelName string       `json:"channel_name,omitempty"`
	ChannelKind Kind         `json:"channel_kind,omitempty"`
	View        *MessageView `json:"view,omitempty"`

	// Gone marks a message that is not there to show: taken back, erased,
	// pruned, or in a room the reader may not read, answered alike.
	Gone bool `json:"gone,omitempty"`
}

// SavedMessages is a page of the reader's saved messages.
type SavedMessages struct {
	Served

	Saved      []SavedEntry `json:"saved"`
	NextCursor string       `json:"next_cursor,omitempty"`
	ReadState  bool         `json:"read_state"`
}

// savedKey orders the marks as the record does: the newest save first, the
// message id breaking a tie.
func savedKey(m SavedMark) activityKey {
	// THE ID NEGATED IN ORDER: the record sorts a tie by the id ascending,
	// and the feed's key sorts descending, so the key is the id's own
	// complement under the feed's comparison, spelled by inverting it.
	return activityKey{at: store.EncodeTime(m.At), key: invertKey(m.MessageID)}
}

// invertKey maps a string to one whose descending order is the original's
// ascending order, for ids of the engine's own alphabet.
func invertKey(id string) string {
	b := []byte(id)
	for i, c := range b {
		b[i] = 0xff - c
	}
	return string(b)
}

// Saved answers a page of the reader's saved messages, each with its message
// unless it is gone.
//
// THE MARK'S ROOM IS CHECKED, NEVER TRUSTED: a mark names its room, and a
// message is shown only when it is in that room and the reader may read it,
// so a mark naming a message under a room it is not in shows nothing.
func (r *Reader) Saved(ctx context.Context, viewer string, q SavedQuery,
	fresh statelog.Freshness) (SavedMessages, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return SavedMessages{}, err
	}
	if fresh.Level == "" {
		return SavedMessages{}, errNoLevel("a saved messages read")
	}
	after, err := parseActivityCursor(q.Cursor)
	if err != nil {
		return SavedMessages{}, err
	}
	marks := q.Saved
	if after != nil {
		marks = slices.DeleteFunc(slices.Clone(marks), func(m SavedMark) bool {
			return !after.before(savedKey(m))
		})
	}
	limit := limitOf(q.Limit)
	if len(marks) > limit {
		marks = marks[:limit]
	}
	out := SavedMessages{Saved: make([]SavedEntry, 0, len(marks)), ReadState: true}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			held := 0
			for i, mark := range marks {
				if i > 0 && held > pageBytes {
					out.NextCursor = savedKey(marks[i-1]).String()
					return nil
				}
				entry, err := readSaved(ctx, tx, who, mark)
				if err != nil {
					return err
				}
				if entry.View != nil {
					held += len(entry.View.Message.Body)
				}
				out.Saved = append(out.Saved, entry)
			}
			if len(marks) == limit && len(q.Saved) > len(marks) && len(marks) > 0 {
				last := savedKey(marks[len(marks)-1])
				for _, m := range q.Saved {
					if last.before(savedKey(m)) {
						out.NextCursor = last.String()
						break
					}
				}
			}
			return nil
		})
	if err != nil {
		return SavedMessages{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// readSaved reads one saved message for the reader.
func readSaved(ctx context.Context, tx *sql.Tx, who string, mark SavedMark) (SavedEntry, error) {
	entry := SavedEntry{Mark: mark, Gone: true}
	room, _, _, err := readRoom(ctx, tx, mark.ChannelID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
		return entry, nil
	case err != nil:
		return SavedEntry{}, err
	}
	member := false
	if privateRoom(room.Kind) {
		if member, err = memberOf(ctx, tx, mark.ChannelID, who); err != nil {
			return SavedEntry{}, err
		}
	}
	if !Visible(who, room, member) {
		return entry, nil
	}
	entry.ChannelName, entry.ChannelKind = room.Name, room.Kind
	page, err := readMessagePage(ctx, tx, who,
		[]string{"channel_id = ?", "id = ?", "event = ''"},
		[]any{mark.ChannelID, mark.MessageID}, "channel_seq DESC", 1)
	if err != nil {
		return SavedEntry{}, err
	}
	if len(page.views) == 0 || page.views[0].Message.DeletedAt != nil {
		return entry, nil
	}
	view := page.views[0]
	entry.View, entry.Gone = &view, false
	return entry, nil
}

// ---- what the reader sent ------------------------------------------------ //

// SentQuery asks for a page of what the reader said, in every room.
type SentQuery struct {
	Cursor string
	Limit  int
}

// SentMessage is one of the reader's own messages, where it was said.
type SentMessage struct {
	ChannelName string      `json:"channel_name,omitempty"`
	ChannelKind Kind        `json:"channel_kind"`
	View        MessageView `json:"view"`
}

// SentMessages is a page of what the reader said, newest first.
type SentMessages struct {
	Served

	Messages   []SentMessage `json:"messages"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// Sent answers a page of what the reader said, newest first, over the rooms
// they may still read. A message they took back is left out.
func (r *Reader) Sent(ctx context.Context, viewer string, q SentQuery,
	fresh statelog.Freshness) (SentMessages, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return SentMessages{}, err
	}
	if fresh.Level == "" {
		return SentMessages{}, errNoLevel("a sent messages read")
	}
	after, err := parseActivityCursor(q.Cursor)
	if err != nil {
		return SentMessages{}, err
	}
	limit := limitOf(q.Limit)
	out := SentMessages{Messages: []SentMessage{}}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true),
		func(tx *sql.Tx) error {
			where := []string{"author_handle = ?", "deleted_at IS NULL", "event = ''"}
			args := []any{who}
			if after != nil {
				where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
				args = append(args, after.at, after.at, after.key)
			}
			// THE VISIBILITY NARROWING, over the room each message is in:
			// a private room the reader has left keeps their words, and
			// shows them to nobody outside it, them included.
			where = append(where, `channel_id IN (SELECT c.id FROM chat_channels c
			     WHERE `+visibleWhere+`)`)
			args = append(args, who)
			page, err := readMessagePage(ctx, tx, who, where, args,
				"created_at DESC, id DESC", limit)
			if err != nil {
				return err
			}
			rooms := map[string]Channel{}
			for _, view := range page.views {
				room, ok := rooms[view.Message.ChannelID]
				if !ok {
					room, _, _, err = readRoom(ctx, tx, view.Message.ChannelID)
					if err != nil {
						return err
					}
					rooms[view.Message.ChannelID] = room
				}
				// THE SQL NARROWED (membership included) AND THIS DECIDES.
				if !Visible(who, Channel{Kind: room.Kind}, true) {
					continue
				}
				out.Messages = append(out.Messages, SentMessage{
					ChannelName: room.Name, ChannelKind: room.Kind, View: view})
			}
			if page.next != "" && len(page.views) > 0 {
				last := page.views[len(page.views)-1].Message
				out.NextCursor = activityKey{at: store.EncodeTime(last.CreatedAt),
					key: last.ID}.String()
			}
			return nil
		})
	if err != nil {
		return SentMessages{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// ---- a room's shared links ----------------------------------------------- //

// LinksQuery asks for a page of the links one room shared.
type LinksQuery struct {
	ChannelID string

	// Cursor is the `channel_seq` of the last message of the page before,
	// as [SharedLinks.NextCursor] handed it back.
	Cursor string

	// Limit is how many MESSAGES a page reads; each carries up to
	// [MaxLinks] links.
	Limit int
}

// SharedLink is one link a room shared.
type SharedLink struct {
	URL        string     `json:"url" untrusted:"name"`
	MessageID  string     `json:"message_id"`
	ThreadRoot string     `json:"thread_root,omitempty"`
	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`
	At         time.Time  `json:"at"`
	ChannelSeq int64      `json:"channel_seq"`
}

// SharedLinks is a page of the links one room shared, newest first.
type SharedLinks struct {
	Served

	ChannelID  string       `json:"channel_id"`
	Links      []SharedLink `json:"links"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// Links answers a page of the links one room shared, newest first: each link
// of each message that carried one, a taken-back message's left out.
//
// A POINT READ on the room, as its transcript is ([Reader.Messages]): a room
// the reader may not read is not found.
func (r *Reader) Links(ctx context.Context, viewer string, q LinksQuery,
	fresh statelog.Freshness) (SharedLinks, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return SharedLinks{}, err
	}
	if fresh.Level == "" {
		return SharedLinks{}, errNoLevel("a shared links read")
	}
	before, err := parseSeqCursor(q.Cursor)
	if err != nil {
		return SharedLinks{}, err
	}
	limit := limitOf(q.Limit)
	out := SharedLinks{ChannelID: q.ChannelID, Links: []SharedLink{}}
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(q.ChannelID), false),
		func(tx *sql.Tx) error {
			if err := r.reachable(ctx, tx, who, q.ChannelID); err != nil {
				return err
			}
			args := []any{q.ChannelID}
			clause := ""
			if before > 0 {
				clause = " AND channel_seq < ?"
				args = append(args, before)
			}
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, `
				SELECT document, channel_seq FROM chat_messages
				 WHERE channel_id = ? AND links <> '' AND deleted_at IS NULL
				   AND event = ''`+clause+`
				 ORDER BY channel_seq DESC
				 LIMIT ?`, args...)
			if err != nil {
				return fmt.Errorf("chatlet: read room %s's shared links: %w",
					q.ChannelID, err)
			}
			defer func() { _ = rows.Close() }()
			read, held := 0, 0
			var lastSeq int64
			for rows.Next() {
				var document []byte
				var seq int64
				if err := rows.Scan(&document, &seq); err != nil {
					return fmt.Errorf("chatlet: scan a shared link of room %s: %w",
						q.ChannelID, err)
				}
				if read == limit || (read > 0 && held+len(document) > pageBytes) {
					out.NextCursor = strconv.FormatInt(lastSeq, 10)
					break
				}
				read, held, lastSeq = read+1, held+len(document), seq
				message, err := DecodeMessage(document)
				if err != nil {
					// A MESSAGE A NEWER PEER WROTE still holds its
					// place, which the cursor passes.
					continue
				}
				for _, link := range message.Links {
					out.Links = append(out.Links, SharedLink{URL: link,
						MessageID: message.ID, ThreadRoot: message.ThreadRoot,
						Author: message.Author, AuthorKind: message.AuthorKind,
						At: message.CreatedAt, ChannelSeq: seq})
				}
			}
			return rows.Err()
		})
	if err != nil {
		return SharedLinks{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// ---- one message, read in place ------------------------------------------ //

// MessageQuery names one message by its room and its id.
type MessageQuery struct {
	ChannelID string
	MessageID string
}

// OneMessage is one message with what reading it in place needs.
type OneMessage struct {
	Served

	ChannelName string      `json:"channel_name,omitempty"`
	ChannelKind Kind        `json:"channel_kind"`
	View        MessageView `json:"view"`

	// Root is the thread's root when the message is a reply.
	Root *MessageView `json:"root,omitempty"`

	// Before are up to two replies said just before it in its thread,
	// oldest first.
	Before []MessageView `json:"before,omitempty"`
}

// Message answers one message by its id, with its thread's root and the two
// replies before it when it is a reply. A room the reader may not read, and a
// message that is not in it, are not found.
func (r *Reader) Message(ctx context.Context, viewer string, q MessageQuery,
	fresh statelog.Freshness) (OneMessage, error) {

	who, err := viewerOf(viewer)
	if err != nil {
		return OneMessage{}, err
	}
	if fresh.Level == "" {
		return OneMessage{}, errNoLevel("a message read")
	}
	if strings.TrimSpace(q.MessageID) == "" {
		return OneMessage{}, invalid("message_id", "a message read names no message")
	}
	var out OneMessage
	answer, err := r.log.Read(ctx, fresh.Query(ReadScope(q.ChannelID), false),
		func(tx *sql.Tx) error {
			if err := r.reachable(ctx, tx, who, q.ChannelID); err != nil {
				return err
			}
			room, _, _, err := readRoom(ctx, tx, q.ChannelID)
			if err != nil {
				return err
			}
			out.ChannelName, out.ChannelKind = room.Name, room.Kind
			page, err := readMessagePage(ctx, tx, who,
				[]string{"channel_id = ?", "id = ?"},
				[]any{q.ChannelID, q.MessageID}, "channel_seq DESC", 1)
			if err != nil {
				return err
			}
			if len(page.views) == 0 {
				return fmt.Errorf("%w: there's no such message", ErrNotFound)
			}
			out.View = page.views[0]
			root := out.View.Message.ThreadRoot
			if root == "" {
				return nil
			}
			rootPage, err := readMessagePage(ctx, tx, who,
				[]string{"channel_id = ?", "id = ?"}, []any{q.ChannelID, root},
				"channel_seq DESC", 1)
			if err != nil {
				return err
			}
			if len(rootPage.views) > 0 {
				out.Root = &rootPage.views[0]
			}
			before, err := readMessagePage(ctx, tx, who,
				[]string{"channel_id = ?", "thread_root = ?", "channel_seq < ?",
					"event = ''"},
				[]any{q.ChannelID, root, out.View.ChannelSeq},
				"channel_seq DESC", threadLatest)
			if err != nil {
				return err
			}
			slices.Reverse(before.views)
			out.Before = before.views
			return nil
		})
	if err != nil {
		return OneMessage{}, err
	}
	out.Served = servedFrom(answer)
	return out, nil
}

// compareKeys orders two feed keys, newest first, for a sort.
func compareKeys(a, b activityKey) int {
	return cmp.Or(cmp.Compare(b.at, a.at), cmp.Compare(b.key, a.key))
}
