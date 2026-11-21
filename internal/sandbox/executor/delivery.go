package executor

import (
	"unicode/utf8"
)

// UTF16Len is s's length in UTF-16 code units, as a JavaScript harness
// measures it and as the engine bounds a note's items. An invalid UTF-8 byte
// counts as one, as U+FFFD.
func UTF16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r >= 0x10000 {
			n += 2
			continue
		}
		n++
	}
	return n
}

// HookNote is one message a hook answer hands the harness: a person's note,
// whose sender By the transcript line names; a wake item, whose messages'
// writers Writers names, one a message; or the engine's account of a late call
// (Call), which the line names by its kind ([hookCallOne]).
type HookNote struct {
	By      string
	Message string
	Call    bool
	Writers []string
}

// HOW A PERSON'S NOTES REACH A HARNESS'S RUN, by the mechanism it takes them
// through: a hook the harness calls at its steps, answered with the
// notes ([hookDelivery]), or a poll its plugin makes, each note admitted under
// an id of the harness's own ([inboxDelivery]). The engine looks the mechanism
// up by the run's harness; a harness that takes notes some third way needs a
// bridge route of its own as well.

// hookDelivery is a harness whose hooks are answered with the run's notes.
type hookDelivery interface {
	// notesThatFit is how many of messages, from the first, one answer
	// carries: at least one, or an oversized note would wait for ever.
	notesThatFit(messages []string) int
	// roomBeside is how many UTF-16 units one answer has left for a second
	// message once it carries first: its ceiling, less first and the
	// separator between them.
	roomBeside(first string) int
	// hookAnswer is the hook's whole answer at event, handing notes under
	// take, in the harness's own event names and shape.
	hookAnswer(event, take string, notes []HookNote) string
}

// inboxDelivery is a harness whose plugin polls for the run's notes.
type inboxDelivery interface {
	// inboxID is the id the harness admits noteID under for launch.
	inboxID(launch, noteID string) string
	// pollCeiling is the most UTF-16 units one admitted message can carry.
	pollCeiling() int
}

// HookNotesThatFit is how many of messages, from the first, one answer to a
// hook of harness's run carries; all of them for a harness answered by no
// hook, which is never asked.
func HookNotesThatFit(harness string, messages []string) int {
	if h, ok := harnessNamed(harness).(hookDelivery); ok {
		return h.notesThatFit(messages)
	}
	return len(messages)
}

// HookRoomBeside is how many UTF-16 units one hook answer of harness's run has
// left beside first, false for a harness answered by no hook.
func HookRoomBeside(harness, first string) (int, bool) {
	if h, ok := harnessNamed(harness).(hookDelivery); ok {
		return h.roomBeside(first), true
	}
	return 0, false
}

// HookAnswer is the answer to a hook of harness's run at event, handing notes
// under take; "" for a harness answered by no hook, which answers nothing.
func HookAnswer(harness, event, take string, notes []HookNote) string {
	if h, ok := harnessNamed(harness).(hookDelivery); ok {
		return h.hookAnswer(event, take, notes)
	}
	return ""
}

// InboxID is the id harness's plugin admits noteID under for launch, "" for a
// harness that polls for no notes.
func InboxID(harness, launch, noteID string) string {
	if h, ok := harnessNamed(harness).(inboxDelivery); ok {
		return h.inboxID(launch, noteID)
	}
	return ""
}

// PollCeiling is the most UTF-16 units one message harness's plugin admits
// can carry, false for a harness that polls for no notes.
func PollCeiling(harness string) (int, bool) {
	if h, ok := harnessNamed(harness).(inboxDelivery); ok {
		return h.pollCeiling(), true
	}
	return 0, false
}

// confirms reports whether h, a harness that reads notes ([noteReader]), can
// confirm a take: by its hooks' receipts on its main thread, or by its
// plugin's own delivery event. A harness that reads notes but cannot confirm
// one would leave its wakes unclaimed, run again as turns of their own.
func confirms(h Harness) bool {
	if _, polls := h.(inboxDelivery); polls {
		return true
	}
	_, hooks := h.(hookDelivery)
	traits, reads := h.(threadHarness)
	return hooks && reads && traits.threadTraits().HookReceipts
}
