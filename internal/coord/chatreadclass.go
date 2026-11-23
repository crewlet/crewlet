package coord

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/google/uuid"
)

// The chat reads bucket's one key class (ADR-0175, "The chat reads bucket
// declares its class"): a seat's whole read record, which every flush reads
// and decides, so its read is given its reply's crossing at
// [MaxChatReadBytes]. Its counts are capped ([MaxReadCursors],
// [MaxReaderMarks]); what gives the record a widest form is that every id it
// stores is the one Chatlet minted, a canonical UUID ([CheckChatID]), refused
// in any other form where a flush names it.

// ChatReadClass keys one seat's read record, `chatread.<handle>`.
const ChatReadClass = "chatread"

// ChatReadKey is one seat's read record key.
func ChatReadKey(handle string) string { return DocumentKey(ChatReadClass, handle) }

// MaxChatReadBytes bounds one seat's read record as stored (json.Marshal,
// [EncodeChatRead]). Its widest holds 256 room and 256 thread cursors at an
// int64's 19 digits, 256 muted, 256 every-message and 256 starred rooms, 256
// saved messages and 256 thread marks unfollowed, each id a 36-byte UUID and
// each instant UTC to the nanosecond: 137,953 bytes
// (TestTheWidestChatReadRecordFitsItsBound), 9.5 KB under the bound. At the
// slowest link pace its read waits 5.7 s beyond the read term; nothing reads
// it on a boot or a probe.
const MaxChatReadBytes = 144 << 10

// chatIDLength is the length of a UUID as [uuid.UUID.String] writes it.
const chatIDLength = 36

// IsChatID reports whether id is in the one form Chatlet mints a room's, a
// message's or a thread root's id in: a UUID as [uuid.UUID.String] writes
// it, 36 bytes of lowercase hex and dashes, which JSON writes as itself.
func IsChatID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && len(id) == chatIDLength && parsed.String() == id
}

// CheckChatID refuses an id that is not [IsChatID] as [ErrInvalidReadState],
// naming field as its sender wrote it (`save[2].message_id`), so the record
// keeps a widest form ([MaxChatReadBytes]).
func CheckChatID(field, id string) error {
	if IsChatID(id) {
		return nil
	}
	// Quoted whole up to twice a UUID's length, so a near miss (upper case,
	// braces, a urn: prefix) shows; anything longer is said by its size.
	said := strconv.Quote(id)
	if len(id) > 2*chatIDLength {
		said = fmt.Sprintf("an id of %d bytes", len(id))
	}
	return fmt.Errorf("%w: %s names %s, which is not an id Chatlet gave a room or a message; "+
		"each is the UUID Chatlet minted (36 bytes, lower case), as its rooms and messages list it",
		ErrInvalidReadState, field, said)
}

// checkDeltaIDs holds every id a flush names to [CheckChatID], in the
// order the mark_chat_read tool lists its arguments and each map's keys
// sorted, so a flush wrong in two places is told the same one every time. An
// empty id is left to the merge, which refuses it in words of its own.
func checkDeltaIDs(d ChatReadDelta) error {
	for _, places := range []struct {
		field     string
		positions map[string]int64
	}{{"cursors", d.Cursors}, {"thread_cursors", d.ThreadCursors}, {"rewind", d.Rewind}} {
		for _, id := range slices.Sorted(maps.Keys(places.positions)) {
			if err := checkNamed(fmt.Sprintf("a key of `%s`", places.field), id); err != nil {
				return err
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(d.Notify)) {
		if err := checkNamed("a key of `notify`", id); err != nil {
			return err
		}
	}
	for _, rooms := range []struct {
		field string
		ids   []string
	}{{"mute", d.Mute}, {"unmute", d.Unmute}, {"star", d.Star}, {"unstar", d.Unstar}} {
		for i, id := range rooms.ids {
			if err := checkNamed(fmt.Sprintf("`%s[%d]`", rooms.field, i), id); err != nil {
				return err
			}
		}
	}
	for _, messages := range []struct {
		field string
		refs  []MessageRef
	}{{"save", d.Save}, {"unsave", d.Unsave}} {
		for i, ref := range messages.refs {
			if err := checkRef(messages.field, i, "message_id", ref.ChannelID, ref.MessageID); err != nil {
				return err
			}
		}
	}
	for _, threads := range []struct {
		field string
		refs  []ThreadRef
	}{{"follow", d.Follow}, {"unfollow", d.Unfollow}} {
		for i, ref := range threads.refs {
			if err := checkRef(threads.field, i, "root_id", ref.ChannelID, ref.RootID); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRef holds one entry of a list naming a room and a message or a thread
// root to [CheckChatID], each part named as its field (`save[2].message_id`).
func checkRef(field string, i int, idKey, channel, id string) error {
	if err := checkNamed(fmt.Sprintf("`%s[%d].channel_id`", field, i), channel); err != nil {
		return err
	}
	return checkNamed(fmt.Sprintf("`%s[%d].%s`", field, i, idKey), id)
}

// checkNamed is [CheckChatID] of an id the flush named, none for an empty one
// ([checkDeltaIDs]).
func checkNamed(field, id string) error {
	if id == "" {
		return nil
	}
	return CheckChatID(field, id)
}

// EncodeChatRead is a seat's read state as a backend stores it: without its
// [ChatReadState.Revision], which is never stored, refused past
// [MaxChatReadBytes] as [ErrValueTooLarge]. The one rule both backends write
// by, so the twin refuses what the KV backend does.
func EncodeChatRead(handle string, s ChatReadState) ([]byte, error) {
	s.Revision = 0
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("coord: encode the chat read state for %s: %w", handle, err)
	}
	if err := HoldValue(ChatReadKey(handle), len(raw), MaxChatReadBytes); err != nil {
		return nil, err
	}
	return raw, nil
}
