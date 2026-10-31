package coord

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"
)

// The reader's own marks, kept in the same record as their read cursors: how a
// room reaches them, the rooms they starred, the messages they saved for later
// and the threads they followed or unfollowed by hand.
//
// # Why they share the read state's record
//
// Each is a fact about one reader's attention, exactly as a cursor and a mute
// are: nobody replays it, no member of the fleet derives anything from its
// history, and it passes with the seat to whoever holds it next. So it is
// kept where that kind of fact already lives, for the reasons the read state
// gives ([ChatReads]), and it rides the same merge, the same compare-and-set
// and the same watch: a mark made in one tab reaches every other tab of the
// seat through the push the record already raises, with no message of its
// own for a peer to forge.
//
// # Why every mark is named, never the list
//
// For the mute's reason ([ChatReadDelta]): a flush names the rooms, messages
// or threads whose mark changes, and everything it does not name keeps its
// state, so two tabs marking two things at once both land and a screen opened
// a moment ago needs nothing read before it marks.
//
// # Why the caps differ in what they do at the edge
//
// A star, a notify level and a save are each one deliberate gesture, so one
// past [MaxReaderMarks] is REFUSED, as a mute is: dropping an older one
// silently is a saved message gone with no way to find out. A thread mark is
// different, because every unfollow leaves one: a cap that refused would keep
// somebody in a thread they asked to leave, so past the cap the mark made
// longest ago is dropped instead, and that thread falls back to what the
// conversation says (followed when the reader spoke or was named in it).

// MaxReaderMarks bounds each of the reader's mark sets on its own: the rooms
// set to every message, the starred rooms, the saved messages and the thread
// marks.
//
// 256, the bound [MaxReadCursors] puts on the cursors and the mutes, for the
// same reason and so that the record stays one small value: at the cap every
// set together is about a hundred kilobytes, inside one coordination value
// with room to spare. The dashboard holds the same number
// (`contract/chat.ts`, MAX_READER_MARKS) to say so before a press is refused.
const MaxReaderMarks = 256

// NotifyLevel is how a room reaches one reader.
//
// A READER'S OWN CHOICE, and nothing else's: a person is never woken, so the
// level decides what the reader's own screens raise for the room (every
// message, mentions only, or nothing), never a delivery, and a seat's wake is
// not touched by anybody's level. It is not the membership's `follow_all`,
// which is the wake policy on the chat log and is the org chart's for a unit
// room.
type NotifyLevel string

const (
	// NotifyAll raises every message in the room.
	NotifyAll NotifyLevel = "all"
	// NotifyMentions raises only what names the reader: the default, and a
	// room in neither [ChatReadState.Every] nor [ChatReadState.Muted].
	NotifyMentions NotifyLevel = "mentions"
	// NotifyMuted raises nothing: the room is in [ChatReadState.Muted].
	NotifyMuted NotifyLevel = "muted"
)

// NotifyLevels are the three, in the order a menu offers them.
var NotifyLevels = []NotifyLevel{NotifyAll, NotifyMentions, NotifyMuted}

// Valid reports whether l is a level this build knows.
func (l NotifyLevel) Valid() bool { return slices.Contains(NotifyLevels, l) }

// SavedMessage is one message a reader saved for later.
type SavedMessage struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`

	// At is when it was saved, which is the Saved view's order.
	At time.Time `json:"at"`
}

// ThreadMark is one thread a reader followed or unfollowed by hand.
//
// A MARK DECIDES. Without one, a thread is followed when the reader started
// it, replied in it or was named in it, which the conversation itself says;
// with one, the mark is the answer either way, so an unfollow keeps a thread
// they spoke in quiet and a follow takes in one they never spoke in.
type ThreadMark struct {
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	Following bool   `json:"following"`

	// At is when the mark was made, which is what the cap drops by.
	At time.Time `json:"at"`
}

// MessageRef names one message, as a save or an unsave does.
type MessageRef struct {
	ChannelID string
	MessageID string
}

// ThreadRef names one thread by its root, as a follow or an unfollow does.
type ThreadRef struct {
	ChannelID string
	RootID    string
}

// setWords are how one room set's refusals say what was wrong, so the one
// merge below answers a mute and a star each in its own words.
type setWords struct {
	add, remove string // "a mute", "an unmute"
	both        string // "both muted and unmuted"
	growing     string // "muting"
	remedy      string // "unmute something first"
}

var (
	muteWords = setWords{add: "a mute", remove: "an unmute",
		both: "both muted and unmuted", growing: "muting",
		remedy: "unmute something first"}
	everyWords = setWords{add: "a level of every message", remove: "a level",
		both: "both set to every message and not", growing: "setting every message",
		remedy: "set a room back to mentions first"}
	starWords = setWords{add: "a star", remove: "an unstar",
		both: "both starred and unstarred", growing: "starring",
		remedy: "unstar something first"}
)

// mergeRoomSet is a set of rooms after one flush: stored, plus every room add
// names, less every room remove names, sorted so a set two backends hold is
// one value.
//
// THE CAP IS CHECKED AGAINST THE SET THE FLUSH LEAVES, and only when the flush
// grows it, at capAt. A flush that does not grow the set is never refused for
// its size, so a removal, or a flush about something else entirely, always
// lands on whatever the record holds, one kept from a build with a larger
// cap included.
func mergeRoomSet(stored, add, remove []string, capAt int, w setWords) ([]string, error) {
	if len(add) == 0 && len(remove) == 0 {
		return slices.Clone(stored), nil
	}
	set := make(map[string]bool, len(stored)+len(add))
	for _, channel := range stored {
		set[channel] = true
	}
	lifted := make(map[string]bool, len(remove))
	for _, channel := range remove {
		if channel == "" {
			return nil, fmt.Errorf("%w: %s needs a channel; the flush named one "+
				"with no id, which is a client that lost the value on the way "+
				"here", ErrInvalidReadState, w.remove)
		}
		lifted[channel] = true
	}
	for _, channel := range add {
		switch {
		case channel == "":
			return nil, fmt.Errorf("%w: %s needs a channel; the flush named one "+
				"with no id, which is a client that lost the value on the way "+
				"here", ErrInvalidReadState, w.add)
		case lifted[channel]:
			return nil, fmt.Errorf("%w: %s is %s in one flush; send the one "+
				"that is meant", ErrInvalidReadState, channel, w.both)
		}
		set[channel] = true
	}
	for channel := range lifted {
		delete(set, channel)
	}
	if len(set) > capAt && len(set) > len(stored) {
		return nil, fmt.Errorf("%w: %s that would hold %d channels and the cap "+
			"is %d; %s", ErrInvalidReadState, w.growing, len(set), capAt, w.remedy)
	}
	if len(set) == 0 {
		// ONE SHAPE FOR AN EMPTY SET: the record is compared by the suite,
		// and an empty list and a nil one encode differently.
		return nil, nil
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// mergeLevels is the muted set and the every-message set after one flush:
// its mutes and unmutes, and the levels it sets.
//
// ONE ROOM, ONE LEVEL. A room is in at most one of the two sets, so a mute
// takes a room out of every message as well, and a level moves a room between
// them. A room a flush both gives a level and mutes or unmutes is refused,
// because the flush would be stating two facts about it and no order between
// them is the caller's.
func mergeLevels(muted, every, mute, unmute []string,
	levels map[string]NotifyLevel) ([]string, []string, error) {

	named := make(map[string]bool, len(mute)+len(unmute))
	for _, channel := range mute {
		named[channel] = true
	}
	for _, channel := range unmute {
		named[channel] = true
	}
	muteAdd, muteLift := slices.Clone(mute), slices.Clone(unmute)
	everyAdd, everyLift := []string(nil), slices.Clone(mute)
	// SORTED, so two nodes merging one flush build the same lists in the
	// same order and refuse it, if they do, in the same words.
	for _, channel := range slices.Sorted(maps.Keys(levels)) {
		level := levels[channel]
		switch {
		case channel == "":
			return nil, nil, fmt.Errorf("%w: a level needs a channel; the "+
				"flush named one with no id, which is a client that lost the "+
				"value on the way here", ErrInvalidReadState)
		case !level.Valid():
			return nil, nil, fmt.Errorf("%w: %q is not a level for %s; send "+
				"one of all, mentions or muted", ErrInvalidReadState, level, channel)
		case named[channel]:
			return nil, nil, fmt.Errorf("%w: %s is given a level and a mute or "+
				"an unmute in one flush; send the one that is meant",
				ErrInvalidReadState, channel)
		}
		switch level {
		case NotifyAll:
			everyAdd = append(everyAdd, channel)
			muteLift = append(muteLift, channel)
		case NotifyMentions:
			everyLift = append(everyLift, channel)
			muteLift = append(muteLift, channel)
		case NotifyMuted:
			muteAdd = append(muteAdd, channel)
			everyLift = append(everyLift, channel)
		}
	}
	outMuted, err := mergeRoomSet(muted, muteAdd, muteLift, MaxReadCursors, muteWords)
	if err != nil {
		return nil, nil, err
	}
	outEvery, err := mergeRoomSet(every, everyAdd, everyLift, MaxReaderMarks, everyWords)
	if err != nil {
		return nil, nil, err
	}
	return outMuted, outEvery, nil
}

// mergeSaved is the saved messages after one flush, the newest save first.
//
// A SAVE OF A MESSAGE ALREADY SAVED KEEPS ITS FIRST INSTANT, which is what
// makes the merge idempotent: a flush whose answer was lost and is sent again
// leaves the record exactly as the first one did, rather than lifting the
// message to the top of the list a second time.
func mergeSaved(stored []SavedMessage, save, unsave []MessageRef,
	at time.Time) ([]SavedMessage, error) {

	if len(save) == 0 && len(unsave) == 0 {
		return slices.Clone(stored), nil
	}
	lifted := make(map[string]bool, len(unsave))
	for _, ref := range unsave {
		if ref.MessageID == "" {
			return nil, fmt.Errorf("%w: an unsave needs a message; the flush "+
				"named one with no id, which is a client that lost the value on "+
				"the way here", ErrInvalidReadState)
		}
		lifted[ref.MessageID] = true
	}
	kept := make(map[string]SavedMessage, len(stored)+len(save))
	for _, s := range stored {
		kept[s.MessageID] = s
	}
	for _, ref := range save {
		switch {
		case ref.MessageID == "" || ref.ChannelID == "":
			return nil, fmt.Errorf("%w: a save needs a channel and a message; "+
				"the flush named one without, which is a client that lost the "+
				"value on the way here", ErrInvalidReadState)
		case lifted[ref.MessageID]:
			return nil, fmt.Errorf("%w: %s is both saved and unsaved in one "+
				"flush; send the one that is meant", ErrInvalidReadState,
				ref.MessageID)
		}
		if _, ok := kept[ref.MessageID]; !ok {
			kept[ref.MessageID] = SavedMessage{ChannelID: ref.ChannelID,
				MessageID: ref.MessageID, At: at.UTC()}
		}
	}
	for id := range lifted {
		delete(kept, id)
	}
	if len(kept) > MaxReaderMarks && len(kept) > len(stored) {
		return nil, fmt.Errorf("%w: saving that would hold %d messages and the "+
			"cap is %d; remove a saved message first", ErrInvalidReadState,
			len(kept), MaxReaderMarks)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	out := slices.Collect(maps.Values(kept))
	slices.SortFunc(out, func(a, b SavedMessage) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.MessageID, b.MessageID))
	})
	return out, nil
}

// mergeThreadMarks is the thread marks after one flush, the newest first.
//
// A MARK THAT ALREADY SAYS WHAT THE FLUSH SAYS KEEPS ITS INSTANT, for
// [mergeSaved]'s reason; one that says the opposite is replaced, at the
// flush's instant. Past [MaxReaderMarks] the marks made longest ago are
// dropped, never refused: see "Why the caps differ".
func mergeThreadMarks(stored []ThreadMark, follow, unfollow []ThreadRef,
	at time.Time) ([]ThreadMark, error) {

	if len(follow) == 0 && len(unfollow) == 0 {
		return slices.Clone(stored), nil
	}
	marks := make(map[string]ThreadMark, len(stored)+len(follow)+len(unfollow))
	for _, m := range stored {
		marks[m.RootID] = m
	}
	stated := make(map[string]bool, len(follow)+len(unfollow))
	mark := func(ref ThreadRef, following bool) error {
		if ref.RootID == "" || ref.ChannelID == "" {
			return fmt.Errorf("%w: a follow or an unfollow needs a channel and "+
				"a thread; the flush named one without, which is a client that "+
				"lost the value on the way here", ErrInvalidReadState)
		}
		if said, ok := stated[ref.RootID]; ok && said != following {
			return fmt.Errorf("%w: thread %s is both followed and unfollowed "+
				"in one flush; send the one that is meant", ErrInvalidReadState,
				ref.RootID)
		}
		stated[ref.RootID] = following
		if held, ok := marks[ref.RootID]; ok && held.Following == following {
			return nil
		}
		marks[ref.RootID] = ThreadMark{ChannelID: ref.ChannelID, RootID: ref.RootID,
			Following: following, At: at.UTC()}
		return nil
	}
	for _, ref := range follow {
		if err := mark(ref, true); err != nil {
			return nil, err
		}
	}
	for _, ref := range unfollow {
		if err := mark(ref, false); err != nil {
			return nil, err
		}
	}
	out := slices.Collect(maps.Values(marks))
	// NEWEST FIRST, the root id breaking a tie, so two nodes holding the
	// same marks hold them in the same order and drop the same ones.
	slices.SortFunc(out, func(a, b ThreadMark) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.RootID, b.RootID))
	})
	if len(out) > MaxReaderMarks {
		out = out[:MaxReaderMarks]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// advanceCursors moves each named cursor forward, never back, and bounds the
// map at [MaxReadCursors] by dropping the stalest: the rule the room cursors
// and the thread cursors share. what names the key in a refusal ("a read
// cursor needs a channel").
func advanceCursors(stored, delta map[string]int64, what string) (map[string]int64, error) {
	out := make(map[string]int64, len(stored)+len(delta))
	maps.Copy(out, stored)
	for key, at := range delta {
		if key == "" {
			return nil, fmt.Errorf("%w: %s; the flush named one with no id, "+
				"which is a client that lost the value on the way here",
				ErrInvalidReadState, what)
		}
		// FORWARD ONLY. A cursor that could move backwards would make an
		// unread badge flicker as two tabs disagree about which is
		// further along, and the loser would be whichever flush the
		// network happened to deliver second.
		if at > out[key] {
			out[key] = at
		}
	}
	return out, nil
}
