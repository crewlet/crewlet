// Package steer is how a person's note reaches a turn that is already running.
//
// A turn runs for minutes, and a person watching it sometimes sees it heading
// the wrong way — about to post to the wrong channel, re-reading the page it
// already has, missing the one constraint they forgot to write down. Stopping
// it throws away everything it has done; waiting for it to finish and filing a
// correction costs a whole second turn. A STEER is the third answer: a short
// note that enters the running conversation at its NEXT ROUND BOUNDARY, as a
// message the model reads before it decides what to do next.
//
// # Where it lands, and why there
//
// At the top of a round, immediately after the seat fence, and nowhere else.
// That is the one point in a round where the conversation is COMPLETE: every
// tool call the previous round made has its answer, so a note appended there
// can never sit between a call and its result — which a provider rejects
// outright, and which would make a model read the note as the tool's output.
// After the fence, because a turn this node no longer holds, or one a person
// stopped, must not be told anything more: the note would be read by a turn
// that is about to end without acting on it. And it is a USER message, never
// a system message: the system prompt is the frozen prefix a provider caches
// on, and moving it mid-turn re-bills the whole prompt on every later round.
//
// # What this package is
//
// The [Box] one turn holds — the notes offered to it and not yet read — and
// the wire a note crosses to reach it. A leaf: it knows nothing of the loop
// that drains it, the engine that serves it or the event that records it,
// because each of those is a different package's answer and the box is the
// one thing they share.
//
// # Four answers, and a note is one note
//
// An offer is answered with a [Status], and the four are four different facts
// a person acts on differently: taken ([StatusAccepted]), the turn has too
// many unread notes already ([StatusFull]), the turn has ended
// ([StatusClosed]), and the turn's runtime cannot read a note at all
// ([StatusUnsupported] — an executor running as a coding CLI's own loop,
// which has no round boundary the engine can reach). A retry of a note the box
// already took is answered [StatusAccepted] and adds nothing: the note's id is
// the person's request id, so a retry after an answer that was lost in transit
// is the same note, and a turn that read the same instruction twice would act
// on it twice.
package steer

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxPendingNotes bounds the notes a turn holds unread.
//
// FIVE. A note is read at the next round boundary, which is seconds to a
// minute away; a person who has written five corrections into that window is
// having a conversation, and a conversation belongs in chat or on the work
// item, where it is threaded, kept and answered. The bound is also what keeps
// one round's opening from being a wall of notes the model weighs against its
// own task.
const MaxPendingNotes = 5

// MaxNoteRunes bounds one note.
//
// Two thousand runes — a paragraph. A steer re-points work in flight; an
// instruction longer than that is a brief, and a brief belongs on the work
// item where the turn and every later one can read it. Counted in runes
// because a person writes characters, not bytes.
const MaxNoteRunes = 2000

// Status is a box's answer to one offer.
type Status string

const (
	// StatusAccepted — the turn holds the note and reads it at its next
	// round boundary. Also the answer to a retry of a note already taken.
	StatusAccepted Status = "accepted"

	// StatusFull — the turn already holds [MaxPendingNotes] unread notes.
	// Nothing was taken; the same note may be sent again once the turn has
	// read what it holds.
	StatusFull Status = "full"

	// StatusClosed — the turn has ended or parked, so no later round will
	// read anything.
	StatusClosed Status = "closed"

	// StatusUnsupported — the turn runs where no round boundary is the
	// engine's: an executor running as a coding CLI's own agentic loop.
	StatusUnsupported Status = "unsupported"
)

// Statuses is every answer this build gives, in the order a reader's table
// lists them.
var Statuses = []Status{StatusAccepted, StatusFull, StatusClosed, StatusUnsupported}

// Valid reports whether a status off the wire is one this build knows. A newer
// peer may answer one it does not, and that must arrive as a value a caller
// can refuse to act on rather than as a decode error.
func (s Status) Valid() bool { return slices.Contains(Statuses, s) }

// Note is one person's note to a running turn.
type Note struct {
	// ID is the note's identity, and it is the person's REQUEST id: a
	// retry of one request is one note. See the package doc.
	ID string

	// Text is the note, as the person wrote it.
	Text string

	// By is who sent it — the credential's own name, the author an audit
	// names — and BySeat the person that credential is bound to, empty for
	// a token nobody bound.
	By     string
	BySeat string

	// At is when the box took it, in UTC.
	At time.Time
}

// Sender is the name a note is shown under: the person where the credential is
// bound to one, else the credential.
func (n Note) Sender() string {
	if n.BySeat != "" {
		return n.BySeat
	}
	return n.By
}

// Validate reports what is wrong with a note's text, or nil. The ONE rule,
// read by the tool that takes a note from a person and by the node that
// offers it to a box, so the two cannot disagree about what a note may be.
func Validate(text string) error {
	switch n := utf8.RuneCountInString(strings.TrimSpace(text)); {
	case n == 0:
		return errEmpty
	case n > MaxNoteRunes:
		return &TooLongError{Runes: n}
	}
	return nil
}

var errEmpty = validationError("a note has no text")

type validationError string

func (e validationError) Error() string { return string(e) }

// TooLongError is a note past [MaxNoteRunes].
type TooLongError struct{ Runes int }

func (e *TooLongError) Error() string {
	return "a note is " + strconv.Itoa(e.Runes) + " characters and may be at most " +
		strconv.Itoa(MaxNoteRunes)
}

// Box holds the notes offered to ONE turn and not yet read by it.
//
// One per turn, opened when the turn starts and closed when it ends or parks.
// Safe for concurrent use: an offer arrives on a broker goroutine while the
// turn's loop drains on its own.
type Box struct {
	mu          sync.Mutex
	pending     []Note
	seen        map[string]struct{}
	closed      bool
	unsupported bool
	now         func() time.Time
}

// New opens a box for a turn whose rounds the engine drives.
func New() *Box { return &Box{seen: map[string]struct{}{}, now: time.Now} }

// Unsupported opens a box for a turn whose rounds the engine does NOT drive —
// one whose executor runs as a coding CLI's own loop. Every new note is
// answered [StatusUnsupported], so the person is told the truth rather than
// promised a delivery no round boundary will ever make.
func Unsupported() *Box {
	b := New()
	b.unsupported = true
	return b
}

// Offer hands the box one note, and answers what became of it.
//
// A note whose id the box has taken before is answered [StatusAccepted] in
// every state — including after the box closed — and changes nothing. The
// first answer is the true one about that note: it was taken, and whether it
// was read or expired is recorded where the turn records it.
func (b *Box) Offer(n Note) Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.seen[n.ID]; dup && n.ID != "" {
		return StatusAccepted
	}
	switch {
	case b.closed:
		return StatusClosed
	case b.unsupported:
		return StatusUnsupported
	case len(b.pending) >= MaxPendingNotes:
		return StatusFull
	}
	if n.At.IsZero() {
		n.At = b.now().UTC()
	}
	b.pending = append(b.pending, n)
	if n.ID != "" {
		b.seen[n.ID] = struct{}{}
	}
	return StatusAccepted
}

// Drain takes every note waiting, oldest first, and leaves the box empty.
//
// Nil on a closed box: a round that begins after the turn ended is not one
// the turn runs.
func (b *Box) Drain() []Note {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.pending) == 0 {
		return nil
	}
	out := b.pending
	b.pending = nil
	return out
}

// Close ends the box and returns the notes it took and nobody read — the ones
// that EXPIRED, because the turn ended or parked before its next round.
//
// Idempotent: a second close returns nothing, so a caller that closes on two
// paths reports each expired note once.
func (b *Box) Close() []Note {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	out := b.pending
	b.pending = nil
	return out
}

// Supported reports whether this box's turn can read a note at all.
func (b *Box) Supported() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.unsupported
}
