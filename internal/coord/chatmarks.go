package coord

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"
)

// The reader's own marks, kept in the read state's record: how a room reaches
// them, the rooms they starred, the messages they saved and the threads they
// followed or unfollowed by hand. Each is one reader's attention, as a cursor
// is, so it passes with the seat and rides [ChatReads]' merge, compare-and-set
// and watch: a mark made in one tab reaches every other tab through the push
// the record already raises, with no message of its own for a peer to forge.
// Every mark is NAMED, never the list, for the mute's reason ([ChatReadDelta]).

// # Why the caps differ in what they do at the edge
//
// A star, a notify level and a save are each one deliberate gesture, so one
// past [MaxReaderMarks] is REFUSED, as a mute is: a silent drop is a saved
// message gone with no way to find out. Every unfollow leaves a thread mark,
// and a refusal would keep somebody in a thread they asked to leave, so past
// the cap the oldest mark is dropped and that thread falls back to what the
// conversation says (followed when the reader spoke or was named in it).

// MaxReaderMarks bounds each of the reader's mark sets on its own: the rooms
// set to every message, the starred rooms, the saved messages and the thread
// marks. 256, [MaxReadCursors]'s bound, so every set at the cap is about a
// hundred kilobytes, well inside one coordination value. The dashboard holds
// the same number (`contract/chat.ts`, MAX_READER_MARKS) to say so before a
// press is refused.
const MaxReaderMarks = 256

// NotifyLevel is how a room reaches one reader: what their own screens raise
// for it (every message, mentions only, or nothing), never a delivery. No
// seat's wake reads it: that is the membership's `follow_all`, the chat log's
// wake policy (the org chart's for a unit room).
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

// mergeRoomSet is a set of rooms after one flush: stored, plus add, less
// remove, sorted so two backends hold one value.
//
// The cap (capAt) is checked against the set the flush leaves, and only when
// the flush GROWS it, so a removal or an unrelated flush always lands, even on
// a record kept under a larger cap.
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
		// One shape for an empty set: the suite compares records, and an
		// empty list and a nil one encode differently.
		return nil, nil
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// mergeLevels is the muted and every-message sets after one flush's mutes,
// unmutes and levels. ONE ROOM, ONE LEVEL: a room is in at most one set, so a
// mute also takes it out of every message. A room given a level and a mute or
// unmute in one flush is refused: two facts with no order between them.
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
	// Sorted, so two nodes merging one flush refuse it, if they do, in the
	// same words.
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

// mergeSaved is the saved messages after one flush, the newest save first. A
// save of a message already saved KEEPS ITS FIRST INSTANT, so the merge is
// idempotent: a resent flush does not lift the message to the top again.
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

// mergeThreadMarks is the thread marks after one flush, the newest first. A
// mark already saying what the flush says KEEPS ITS INSTANT, for
// [mergeSaved]'s reason; the opposite mark is replaced at the flush's instant.
// Past [MaxReaderMarks] the oldest are dropped (see "Why the caps differ").
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
	// The root id breaks a tie, so two nodes holding the same marks drop
	// the same ones.
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

// advanceCursors moves each named cursor forward, never back: the rule room
// and thread cursors share. what is the refusal of an empty key ("a read
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
		// Forward only: a cursor moving back would make an unread badge
		// flicker as two tabs disagree, decided by delivery order.
		if at > out[key] {
			out[key] = at
		}
	}
	return out, nil
}
