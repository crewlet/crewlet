package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
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

// AgainID is the second id harness's plugin may admit noteID under for launch,
// once, where a compaction folded its first admission ([InboxID]'s, which it
// never equals): the same derivation over the note's id and a suffix no note
// id carries, so it is the job's and the note's, and stable. "" for a harness
// that polls for no notes.
func AgainID(harness, launch, noteID string) string {
	if h, ok := harnessNamed(harness).(inboxDelivery); ok {
		return h.inboxID(launch, noteID+"\x00again")
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

// ConfirmsByRecord reports whether harness's session record confirms a take
// by the item its note was admitted as ([recordConfirmer]): the poll's serves
// of its run are then recorded under the items' inbox ids, beside their
// digests (sandbox.NoteServe.Inbox), so its receipts can settle them.
func ConfirmsByRecord(harness string) bool {
	_, ok := harnessNamed(harness).(recordConfirmer)
	return ok
}

// confirms reports whether h, a harness that reads notes ([noteReader]), can
// confirm a take, by one of three: its hooks' serves its thread shows
// ([sandbox.ThreadTraits.Receipts]); the notes its session record holds, read
// for a poll's admitted ids ([recordConfirmer]); or its plugin's own delivery
// event ([pluginConfirmer]). A harness that reads notes and confirms none
// would leave its wakes unclaimed, run again as turns of their own.
func confirms(h Harness) bool {
	_, hooks := h.(hookDelivery)
	_, polls := h.(inboxDelivery)
	traits, reads := h.(threadHarness)
	_, records := h.(recordConfirmer)
	plugin, posts := h.(pluginConfirmer)
	switch {
	case hooks && reads && traits.threadTraits().Receipts:
		return true
	case polls && records:
		return true
	case polls && posts && plugin.pluginConfirms():
		return true
	}
	return false
}

// pluginConfirmer is an [inboxDelivery] whose plugin confirms each note itself,
// off the harness's own view of the model requests that carry it plain.
type pluginConfirmer interface{ pluginConfirms() bool }

// ItemDigest is the digest a poll's serve records of an item's text as the
// poll rendered it (sandbox.InboxItem.Digest): SHA-256, hex. A record that
// holds the text it admitted rather than an id matches by it
// ([recordConfirmer]), hashing each entry with this same function.
func ItemDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ServedTake is one poll serve a session record is asked for: the inbox id
// its note was admitted under ([inboxDelivery.inboxID]), and the digest of the
// item's rendered text ([ItemDigest]).
type ServedTake struct{ InboxID, Digest string }

// recordConfirmer is an [inboxDelivery] whose session record shows the notes
// its plugin admitted, each by its inbox id or by its rendered text:
// notesHeld is the inbox ids of takes, in their order, whose item the record
// holds whole, reading the newest summary into summary in the same walk where
// it is not nil, and recordSummary whether a read asking both takes and
// summary has that walk read it (its summary read off this record, no other
// walk of the read reading it). A record of ids ignores Digest; one of text
// hashes each entry with [ItemDigest]. The record's shape is the harness's
// own. An error is a record that could not be read, which decides nothing.
type recordConfirmer interface {
	notesHeld(ctx context.Context, box sandbox.Sandbox, session Session, takes []ServedTake,
		summary *summaryRead) ([]string, error)
	recordSummary(ask sandbox.MainThreadAsk) bool
}

// recordReceipts is what h's session record shows of the takes ask asks, each
// keyed by the inbox id it was admitted under: held where the record holds it
// whole, undecided with a reason where the record could not be read, never
// absent, since a record that does not show an item has not proved it absent.
// The record is matched against every serve in order, read notes' included
// ([sandbox.MainThreadAsk.Served]), and answers the takes asked alone.
func recordReceipts(ctx context.Context, h recordConfirmer, box sandbox.Sandbox,
	ask sandbox.MainThreadAsk, summary *summaryRead) sandbox.TakeReceipts {

	served := ask.Served
	if served == nil {
		served = ask.Takes
	}
	takes := make([]ServedTake, 0, len(served))
	for _, take := range served {
		takes = append(takes, ServedTake{InboxID: take.Take, Digest: take.Digest})
	}
	ids := make([]string, 0, len(ask.Takes))
	for _, take := range ask.Takes {
		ids = append(ids, take.Take)
	}
	held, err := h.notesHeld(ctx, box, Session{ID: ask.Session}, takes, summary)
	if err != nil {
		return sandbox.TakeReceipts{Why: "the session record could not be read: " + err.Error()}
	}
	var out sandbox.TakeReceipts
	for _, id := range ids {
		if slices.Contains(held, id) {
			out.Held = append(out.Held, id)
		}
	}
	if len(out.Held) == 0 {
		out.Why = "the session record holds none of the notes asked"
	}
	return out
}
