package coord_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// widestChatRead is the widest read record a writer can make
// (coordtest.WidestChatReadDelta), merged as a flush is and stamped absent.
func widestChatRead(t *testing.T) coord.ChatReadState {
	t.Helper()
	delta := coordtest.WidestChatReadDelta(widestInstant)
	state, err := coord.MergeChatRead(coord.ChatReadState{}, delta)
	if err != nil {
		t.Fatalf("merge the widest flush: %v", err)
	}
	state, _ = state.WithAbsence(widestInstant)
	for _, c := range []struct {
		what    string
		n, most int
	}{
		{"cursors", len(state.Cursors), coord.MaxReadCursors},
		{"thread cursors", len(state.ThreadCursors), coord.MaxReadCursors},
		{"muted rooms", len(state.Muted), coord.MaxReadCursors},
		{"every-message rooms", len(state.Every), coord.MaxReaderMarks},
		{"starred rooms", len(state.Starred), coord.MaxReaderMarks},
		{"saved messages", len(state.Saved), coord.MaxReaderMarks},
		{"thread marks", len(state.Threads), coord.MaxReaderMarks},
	} {
		if c.n != c.most {
			t.Fatalf("premise: the widest record holds %d %s, want the cap's %d", c.n, c.what, c.most)
		}
	}
	return state
}

// THE WIDEST CHAT READ RECORD FITS ITS BOUND, as both backends store it
// (coord.EncodeChatRead), and is the 137,953 bytes MaxChatReadBytes states:
// every id held to the UUID Chatlet mints, nothing in the record is unbounded.
//
// Mutation: lower MaxChatReadBytes to 128 << 10, or raise MaxReaderMarks to
// 257, and this is red.
func TestTheWidestChatReadRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	state := widestChatRead(t)
	stored, err := coord.EncodeChatRead("founder", state)
	if err != nil {
		t.Fatalf("store the widest record: %v, want it within %d bytes", err, coord.MaxChatReadBytes)
	}
	const stated = 137953
	if len(stored) != stated {
		t.Errorf("the widest record is stored as %d bytes; MaxChatReadBytes's arithmetic states %d",
			len(stored), stated)
	}
}

// A CHAT READ RECORD PAST ITS BOUND IS REFUSED BY THE ENCODING BOTH BACKENDS
// WRITE BY, as coord.ErrValueTooLarge naming the record's key and the bound,
// and one at it is written: a twin that encoded past it would certify a
// record the KV backend refuses.
//
// Mutation: drop the HoldValue from EncodeChatRead, and the record past it
// is written.
func TestAChatReadRecordPastItsBoundIsRefusedWhereItIsEncoded(t *testing.T) {
	t.Parallel()
	frame := len(`{"muted":[""]}`)
	at := coord.ChatReadState{Muted: []string{strings.Repeat("r", coord.MaxChatReadBytes-frame)}}
	if raw, err := coord.EncodeChatRead("founder", at); err != nil || len(raw) != coord.MaxChatReadBytes {
		t.Fatalf("a record of exactly its bound = %d bytes, %v; want it written", len(raw), err)
	}
	past := coord.ChatReadState{Muted: []string{strings.Repeat("r", coord.MaxChatReadBytes-frame+1)}}
	_, err := coord.EncodeChatRead("founder", past)
	if !errors.Is(err, coord.ErrValueTooLarge) || !strings.Contains(err.Error(), "147456") ||
		!strings.Contains(err.Error(), coord.ChatReadKey("founder")) {
		t.Errorf("a record a byte past its bound = %v, want coord.ErrValueTooLarge naming %s and 147456",
			err, coord.ChatReadKey("founder"))
	}
}

// EVERY ID A FLUSH NAMES IS ONE CHATLET MINTED, refused in any other form
// with the field that named it, in the order the tool lists its arguments:
// the record stores each, and only a UUID's 36 bytes give it a widest form.
//
// Mutation: drop checkDeltaIDs from MergeChatRead, or any field from it, and
// its row is merged.
func TestAFlushNamingAnIDChatletDidNotMintIsRefusedNamingItsField(t *testing.T) {
	t.Parallel()
	room, message := uuid.NewString(), uuid.NewString()
	long := strings.Repeat("a", 1<<20)
	for _, c := range []struct {
		name, field string
		delta       coord.ChatReadDelta
	}{
		{"a room's cursor", "a key of `cursors`", coord.ChatReadDelta{Cursors: map[string]int64{"eng": 1}}},
		{"a thread's cursor", "a key of `thread_cursors`",
			coord.ChatReadDelta{ThreadCursors: map[string]int64{long: 1}}},
		{"a rewind", "a key of `rewind`", coord.ChatReadDelta{Rewind: map[string]int64{strings.ToUpper(room): 1}}},
		{"a level", "a key of `notify`", coord.ChatReadDelta{Notify: map[string]coord.NotifyLevel{"{" + room + "}": coord.NotifyAll}}},
		{"a mute", "`mute[1]`", coord.ChatReadDelta{Mute: []string{room, "urn:uuid:" + room}}},
		{"an unmute", "`unmute[0]`", coord.ChatReadDelta{Unmute: []string{long}}},
		{"a star", "`star[0]`", coord.ChatReadDelta{Star: []string{"ops"}}},
		{"an unstar", "`unstar[0]`", coord.ChatReadDelta{Unstar: []string{"ops"}}},
		// THE TOOL'S ORDER (mark_chat_read's readers): a mute before a star.
		{"a star and a mute", "`mute[0]`", coord.ChatReadDelta{Star: []string{"eng"}, Mute: []string{"ops"}}},
		{"a saved message's room", "`save[0].channel_id`",
			coord.ChatReadDelta{Save: []coord.MessageRef{{ChannelID: "eng", MessageID: message}}}},
		{"a saved message", "`save[0].message_id`",
			coord.ChatReadDelta{Save: []coord.MessageRef{{ChannelID: room, MessageID: long}}}},
		{"an unsaved message", "`unsave[0].message_id`",
			coord.ChatReadDelta{Unsave: []coord.MessageRef{{ChannelID: room, MessageID: "m-1"}}}},
		{"a followed thread", "`follow[0].root_id`",
			coord.ChatReadDelta{Follow: []coord.ThreadRef{{ChannelID: room, RootID: "root"}}}},
		{"an unfollowed thread's room", "`unfollow[0].channel_id`",
			coord.ChatReadDelta{Unfollow: []coord.ThreadRef{{ChannelID: long, RootID: message}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := coord.MergeChatRead(coord.ChatReadState{}, c.delta)
			if !errors.Is(err, coord.ErrInvalidReadState) || !strings.Contains(err.Error(), c.field) {
				t.Errorf("merge = %v, want coord.ErrInvalidReadState naming %s", err, c.field)
			}
			if err != nil && len(err.Error()) > 1024 {
				t.Errorf("the refusal is %d bytes: an id past twice a UUID's length is said by its size",
					len(err.Error()))
			}
		})
	}
	if _, err := coord.MergeChatRead(coord.ChatReadState{}, coord.ChatReadDelta{
		Cursors: map[string]int64{room: 1}, Save: []coord.MessageRef{{ChannelID: room, MessageID: message}},
	}); err != nil {
		t.Errorf("a flush naming only ids Chatlet mints = %v, want it merged", err)
	}
}
