// Package steer is how a person's note reaches a turn that is already running.
//
// A turn runs for minutes, and a person watching it sometimes sees it heading
// the wrong way — about to post to the wrong channel, re-reading the page it
// already has, missing the one constraint they forgot to write down. Stopping
// it throws away everything it has done; waiting for it to finish and filing a
// correction costs a whole second turn. A STEER is the third answer: a short
// note that enters the running work at its NEXT STEP, as something the model
// reads before it decides what to do next.
//
// # Where it lands, and why there
//
// Wherever the work's own loop pauses between steps with nothing half done.
// The turn's own conversation — the reviewer — is a run of the engine's loop,
// and a note lands at the OPENING of its next run, as a USER message after the
// seat fence: a note that arrives while a run is going ends that run as soon as
// the calls of the response it arrived during have their answers
// ([Box.Waiting] is how the tool loop asks), and the next run opens with it.
// The work itself is a coding CLI's own run in a box, and the engine has no
// round there to open: a note reaches it in the BRIEF the run starts with, and
// inside the run at a boundary of the CLI's own loop, through the run's tool
// bridge — where the CLI places what a hook hands it (Claude Code, after a
// batch of tool calls or as it is about to finish), or as a message of its own
// at the next step (OpenCode). The two vendors place it differently, and the
// engine sends it either way at a point where every call has its answer, so a
// note never sits between a call and its result — which a provider rejects
// outright, and which would make a model read the note as the tool's output.
// The ENGINE never folds a note into a tool's result either, which any tool's
// output could imitate; where a CLI then places a hook's context is the CLI's
// own — Claude Code, on some models, appends it to the end of the batch's last
// tool result as a reminder of its own — so the run's standing instruction
// tells the model a note by the heading the CLI puts on it and by the note's
// own, and never by where it sits (internal/sandbox/codingagent, steer.go).
// After the fence, because a turn this node no longer holds, or one a person
// stopped, must not be told anything more. And the ENGINE never edits a system
// prompt mid-turn: the system prompt is the frozen prefix a provider caches on,
// and moving it re-bills the whole prompt on every later request.
//
// # Two homes, one after the other
//
// Before a turn launches its coding run, its notes wait in the [Box] the turn's
// first segment holds, on the node running it, reached by the signed scatter
// on the steer wire (wire.go). From the run's launch to the turn's end they wait
// on the run's own record in the coordination store, sealed, where any node may
// file one and the run, its reviewer and whatever resumes the turn take them
// from (internal/engine, runsteer.go, and ADR-0066). Every read is recorded on
// that record by the write that hands the note over, so the record's read notes
// are the turn's carried notes ([Read]), which open every later phase.
//
// # What this package is
//
// The [Box] one segment holds, the wire a note crosses to reach it, and the
// vocabulary of what became of a note — where it was read ([Reading]), and the
// record that says so ([Record]). A leaf: it knows nothing of the loop that
// drains it, the engine that serves it or who publishes the record, because
// each of those is a different package's answer and the vocabulary is the one
// thing they share.
//
// # Four answers, and a note is one note
//
// An offer is answered with a [Status], and the four are four different facts
// a person acts on differently: taken ([StatusAccepted]), the turn holds too
// many unread notes already ([StatusFull]), the turn has taken as many notes as
// a turn takes ([StatusSpent]), and the turn has ended ([StatusClosed]). A retry
// of a note already taken is answered [StatusAccepted] and adds nothing: the
// note's id is the person's request id, so a retry after an answer that was lost
// in transit is the same note, and a turn that read the same instruction twice
// would act on it twice.
package steer

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// MaxPendingNotes bounds the notes a turn holds unread.
//
// FIVE. A note is read at the turn's next step, which is seconds to a minute
// away; a person who has written five corrections into that window is having a
// conversation, and a conversation belongs in chat or on the work item, where
// it is threaded, kept and answered. The bound is also what keeps one step's
// reading from being a wall of notes the model weighs against its own task.
// "Unread" on a run's record means not yet recorded read, so a note a CLI has
// been handed and has not yet confirmed putting in front of its model still
// counts.
const MaxPendingNotes = 5

// MaxTurnNotes bounds the notes one turn takes in all, read or not.
//
// TEN: two boxes' worth. A person who has sent one turn ten notes is having a
// conversation with it, which belongs on the work item for the reason five
// unread notes do. It also bounds the run's record, which every bridged call
// the run makes rewrites whole: a sealed note is at most about 11 KiB (2,000
// four-byte runes, its sender and the seal's frame, base64-encoded), so a turn's
// notes add at most about 110 KiB to it, and an ordinary note a few hundred
// bytes.
const MaxTurnNotes = 10

// MaxNoteRunes bounds one note.
//
// Two thousand runes — a paragraph. A steer re-points work in flight; an
// instruction longer than that is a brief, and a brief belongs on the work
// item where the turn and every later one can read it. Counted in runes
// because a person writes characters, not bytes.
const MaxNoteRunes = 2000

// Status is a box's answer to one offer, and a run record's to a note filed
// on it.
type Status string

const (
	// StatusAccepted — the turn holds the note and reads it at its next
	// step. Also the answer to a retry of a note already taken.
	StatusAccepted Status = "accepted"

	// StatusFull — the turn already holds [MaxPendingNotes] unread notes.
	// Nothing was taken; the same note may be sent again once the turn has
	// read what it holds.
	StatusFull Status = "full"

	// StatusSpent — the turn has already taken [MaxTurnNotes] notes, read or
	// not. Nothing was taken, and sending it again changes nothing: what is
	// left to say belongs on the work item.
	StatusSpent Status = "spent"

	// StatusClosed — the turn has ended, so nothing will read anything.
	StatusClosed Status = "closed"
)

// Statuses is every answer this build gives, in the order a reader's table
// lists them. An older peer reads one it does not know — [StatusSpent], to a
// build before it — as an answer it cannot act on, which its asker reports as
// `unknown`.
var Statuses = []Status{StatusAccepted, StatusFull, StatusSpent, StatusClosed}

// Valid reports whether a status off the wire is one this build knows. A newer
// peer may answer one it does not, and that must arrive as a value a caller
// can refuse to act on rather than as a decode error.
func (s Status) Valid() bool { return slices.Contains(Statuses, s) }

// Reads is when an accepted note will be read, as the person who sent it is
// told: what the turn is doing decides it.
type Reads string

const (
	// ReadsNextStep — the turn is working: its coding run, or the engine's
	// own loop, reads the note at its next step.
	ReadsNextStep Reads = "next_step"

	// ReadsAfterAnswer — the turn's coding run is parked on a question it
	// asked a person: the note waits, and whatever resumes the turn reads it
	// — the run carried on with the answer, or the reviewer after it.
	ReadsAfterAnswer Reads = "after_answer"
)

// ReadsAll is every value of [Reads] this build answers.
var ReadsAll = []Reads{ReadsNextStep, ReadsAfterAnswer}

// Valid reports whether a value off the wire is one this build knows.
func (r Reads) Valid() bool { return slices.Contains(ReadsAll, r) }

// Note is one person's note to a running turn.
type Note struct {
	// ID is the note's identity, and it is the person's REQUEST id: a
	// retry of one request is one note. See the package doc.
	ID string

	// Text is the note, as the person wrote it.
	Text string

	// By, ByKind and OperatorID are who sent it, in the three halves
	// iam.ActorFor makes of a principal: the author (the seat for a person
	// the identity directory binds to one, the login for anybody else),
	// what sort of author that is, and the credential the note came
	// through. By is also the name the note is shown to the turn under —
	// the person where they are bound to a seat — so there is no second
	// "sender" name beside it to disagree with.
	By         string
	ByKind     iam.ActorKind
	OperatorID string

	// At is when the box took it, in UTC.
	At time.Time
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

// Reading is where a turn read a note: the phase, iteration and round, on the
// phase's own one-based scale, the kind of step ([types.SteerBoundary]) and, for
// a coding run's read, the job that read it; At is when.
type Reading struct {
	Phase     types.Phase
	Iteration int
	Round     int
	Boundary  types.SteerBoundary
	Launch    string
	At        time.Time
}

// Read is a note the turn has read, and where: what a turn carries into every
// later phase.
type Read struct {
	Note
	Reading
}

// Turn is the turn a note was sent to, as its record names it.
type Turn struct {
	Agent, AgentHandle, Role, TurnID, WorkKey string
}

// Record is the agent_turn_steered record of note: delivered where read says,
// or expired where read is nil.
//
// THE ONE CONSTRUCTOR of the record, which every publisher calls — the runner,
// the bridge's run inbox and an ending — so no two of them build it differently:
// a delivery always says where, an expiry never names a step, and the send time
// is always UTC.
func Record(turn Turn, note Note, read *Reading) types.AgentTurnSteered {
	ev := types.AgentTurnSteered{
		Agent: turn.Agent, AgentHandle: turn.AgentHandle, RoleName: turn.Role,
		TurnID: turn.TurnID, WorkKey: turn.WorkKey,
		NoteID: note.ID, Outcome: types.SteerExpired, Note: note.Text,
		SteeredBy: note.By, SteeredByKind: string(note.ByKind), OperatorID: note.OperatorID,
		SentAt: note.At,
	}
	if !ev.SentAt.IsZero() {
		ev.SentAt = ev.SentAt.UTC()
	}
	if read != nil {
		ev.Outcome = types.SteerDelivered
		ev.Phase, ev.Iteration, ev.Round = read.Phase, read.Iteration, read.Round
		ev.Boundary, ev.LaunchID = read.Boundary, read.Launch
	}
	return ev
}

// Box holds the notes offered to ONE turn segment and not yet read by it: the
// turn's first, until it launches its coding run and hands what it still holds
// to the run's record ([Box.HandOver]).
//
// Safe for concurrent use: an offer arrives on a broker goroutine while the
// turn's loop drains on its own.
type Box struct {
	mu      sync.Mutex
	pending []Note
	seen    map[string]struct{}
	// taken is how many notes the box has ever taken — what [MaxTurnNotes]
	// bounds. Counted apart from seen, which a note with no id never joins.
	taken  int
	closed bool
	// handed is a box whose notes went to its turn's run record
	// ([Box.HandOver]): it takes no new note until [Box.Reopen].
	handed bool
	now    func() time.Time
}

// New opens a box.
func New() *Box { return &Box{seen: map[string]struct{}{}, now: time.Now} }

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
	if status := b.statusLocked(); status != StatusAccepted {
		return status
	}
	if n.At.IsZero() {
		n.At = b.now().UTC()
	}
	b.pending = append(b.pending, n)
	b.taken++
	if n.ID != "" {
		b.seen[n.ID] = struct{}{}
	}
	return StatusAccepted
}

// statusLocked is what a new note is answered now. SPENT BEFORE FULL: a turn
// that has taken all it takes will never take this note, and a person told it
// is full would wait for a read that changes nothing.
func (b *Box) statusLocked() Status {
	switch {
	case b.closed || b.handed:
		return StatusClosed
	case b.taken >= MaxTurnNotes:
		return StatusSpent
	case len(b.pending) >= MaxPendingNotes:
		return StatusFull
	}
	return StatusAccepted
}

// Drain takes every note waiting, oldest first, and leaves the box empty.
//
// Nil on a closed box: a step that begins after the turn ended is not one the
// turn runs.
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

// Take is [Box.Drain] as a round of the engine's loop reads it — where it is
// read is the caller's to record, and a box has nothing to write it on.
func (b *Box) Take(_ context.Context, _ Reading) []Note { return b.Drain() }

// Brief is [Box.Drain] as a coding run's brief reads it: the notes the launch
// takes, which are the box's no longer — see [Box.Return] for a launch that
// fails.
func (b *Box) Brief(context.Context) []Note { return b.Drain() }

// Return puts back notes a step took and could not deliver — a launch that
// failed before it wrote its run's record — AT THE FRONT, in their order, so
// they are read first by whatever reads next, or reported expired when the box
// closes. Dedupe is kept: a note taken once is still one note, and a retry of
// it is still answered accepted. A closed box keeps nothing.
func (b *Box) Return(_ context.Context, notes []Note) {
	if b == nil || len(notes) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.pending = append(slices.Clone(notes), b.pending...)
}

// HandOver ends the box's intake and answers every note it holds unread, for the
// caller to file where the turn's notes wait from here — its coding run's
// record, in the write the launch makes to create it. A new note is answered
// closed from here, which tells its caller to look where the turn's notes went.
//
// Undone by [Box.Reopen] where that write never landed. Idempotent, and nothing
// on a closed box.
func (b *Box) HandOver() []Note {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.handed {
		return nil
	}
	b.handed = true
	out := b.pending
	b.pending = nil
	return out
}

// Reopen undoes a [Box.HandOver] whose notes reached no record — the launch
// failed before the write that would have created it: the box takes notes
// again, with notes, what the hand-over answered, back at the FRONT and in
// their order, so whatever reads the box next reads them first. A closed box
// keeps nothing and answers them, for the caller to report as the close would
// have.
func (b *Box) Reopen(notes []Note) []Note {
	if b == nil {
		return notes
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return notes
	}
	b.handed = false
	b.pending = append(slices.Clone(notes), b.pending...)
	return nil
}

// Waiting reports a note waiting to be read, without taking it — what ends a
// run of the engine's loop once its calls in flight are answered, so the next
// run reads the note as a user message rather than the CLI reading it inside a
// tool's result.
func (b *Box) Waiting() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closed && len(b.pending) > 0
}

// Close ends the box and returns the notes it took and nobody read — the ones
// that EXPIRED, because the segment ended before anything read them.
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

// Status is what [Box.Offer] would answer a NEW note now, without offering
// one — the answer a probe gives ([Request.Probe]). A note whose id the box
// has already taken is still answered accepted by an offer; a probe carries
// no id, so it is answered for a note the box has never seen.
func (b *Box) Status() Status {
	if b == nil {
		return StatusClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statusLocked()
}
