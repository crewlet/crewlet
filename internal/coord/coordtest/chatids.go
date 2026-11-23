package coordtest

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ChatID is a test's room, message or thread root id named name: the UUID
// whose bytes are name's, zero-padded, so it is an id Chatlet could mint
// (coord.IsChatID) and a test's ids sort and break ties as their names do.
// A name is at most 16 bytes.
func ChatID(name string) string {
	var id uuid.UUID
	if len(name) > len(id) {
		panic(fmt.Sprintf("coordtest: the chat id name %q is past %d bytes", name, len(id)))
	}
	copy(id[:], name)
	return id.String()
}

// WidestChatReadDelta is the flush that leaves a fresh record at the widest a
// writer can make it (coord.MaxChatReadBytes): every set at its cap, every
// position at an int64's 19 digits, every thread mark an unfollow ("false"
// is a byte longer than "true"), the muted and every-message rooms distinct
// (one room, one level), and do-not-disturb and every mark at at, which is
// widest UTC to the nanosecond. Each id is a fresh UUID, so no link
// compresses it. An absence stamped at at beside it is the rest.
func WidestChatReadDelta(at time.Time) coord.ChatReadDelta {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = uuid.NewString()
		}
		return out
	}
	positions := func() map[string]int64 {
		out := map[string]int64{}
		for _, id := range ids(coord.MaxReadCursors) {
			out[id] = math.MaxInt64
		}
		return out
	}
	notify := map[string]coord.NotifyLevel{}
	for _, id := range ids(coord.MaxReaderMarks) {
		notify[id] = coord.NotifyAll
	}
	dnd := at
	delta := coord.ChatReadDelta{
		Cursors: positions(), ThreadCursors: positions(), Notify: notify,
		Mute: ids(coord.MaxReadCursors), Star: ids(coord.MaxReaderMarks), DNDUntil: &dnd, At: at,
	}
	for _, id := range ids(coord.MaxReaderMarks) {
		delta.Save = append(delta.Save, coord.MessageRef{ChannelID: uuid.NewString(), MessageID: id})
		delta.Unfollow = append(delta.Unfollow, coord.ThreadRef{ChannelID: uuid.NewString(), RootID: id})
	}
	return delta
}
