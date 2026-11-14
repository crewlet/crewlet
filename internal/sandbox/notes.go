package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A person's notes to a turn whose executor is out, on the run's own record.
//
// # Who has to agree on them: the whole company, now
//
// From a run's launch to its turn's end, the notes a person sends the turn wait
// here, on the run's record in the coordination store — the same record that
// carries its bridged calls, and for the same reason. The node a person's
// request reaches, the node serving the run's tool bridge (the seat's holder),
// the node that resumes the turn and the node that finishes the run's ending
// can be four different processes across a restart and a seat moving, and each
// of them already reads this record. A note held in one node's memory would be
// lost with that node, or reachable from it alone.
//
// # What this package decides, and what it does not
//
// The record's shape, and the conditions every change to it is taken on: a
// note filed while the run's ending is undecided, taken only by the job the
// taker names and only while the run is in a status the taker names, every read
// recorded by the write that hands the note over — and a take whose answer was
// lost settled by its identity, its reads answered to a reader that can still
// hand them over or marked unanswered where its reader never had them
// ([PendingStore.SettleTake]).
//
// A HOOK'S SERVE IS NO READ. A harness answered its notes by a hook is SERVED
// them, in a write before its answer that keeps the call it answered
// ([PendingStore.ServeHook]), and a note so served is read only once the job's
// own session shows the hook took it: a verdict settled by the serve's take,
// beside the serves its harness cancelled or answered nothing
// ([PendingStore.SettleHook]). The first serve of a note fixes its place among
// the turn's reads ([RunNote.Place]), and every later read of it is recorded
// there. And an ITEM FOR ONE JOB ([RunNote.For]) is filed only while that job
// is the record's, handed over only to a reader inside it, and removed by the
// write of the launch that opens the next.
//
// What a note SAYS it never
// reads: a note is sealed by the engine under the fleet keyring before it is
// filed ([RunNote.Sealed]), and opened by the engine before anything of it
// reaches a model (internal/engine, runsteer.go, and ADR-0067). The store keeps
// the seal as the bytes it was handed, as it keeps an answer's
// ([RecordedAnswer.Sealed]).
//
// # Bounds are the caller's
//
// How many notes may wait, and how many a turn takes in all, are
// internal/agent/steer's ([NoteFiling]): this package must not import the
// agent layer, and a second copy of the two numbers here would be one that
// drifts.

// RunSteer is the notes a person sent the run's turn: on the record from the
// run's launch to its turn's end.
//
// A WIRE FORMAT like the rest of [PendingRun]: a node on another build reads
// it, so fields are added and never renamed. A build that does not know the
// section carries it whole through [PendingRun.Extra]; one that does carries
// every key it does not know in the section's own Extra, and in each note's
// and each read's — so a later build may add a field at any level of it.
type RunSteer struct {
	// Seat is the SEALED ANCHOR naming the seat the run serves — the seat a
	// person's authority to steer the turn is decided on. Written by the
	// launch that creates the record, kept by every reset of it, and opened
	// by the engine alone: a reader that could rewrite the record in the
	// clear could otherwise steer a person's authority onto another seat.
	Seat []byte `json:"seat,omitempty"`

	// Seq is the place of the last note filed, from 1: every note is
	// numbered as it is filed, so a reader that has seen up to one place
	// asks only for what came after it.
	Seq int `json:"seq,omitempty"`

	// Notes is every note the turn has taken since the run's launch, read
	// or waiting, in the order they were filed.
	Notes []RunNote `json:"notes,omitempty"`

	// Pulled is the job whose harness has asked for notes at least once
	// — what tells a run whose harness never read its notes from one that had
	// none to read.
	Pulled string `json:"pulled,omitempty"`

	// Reads is the place of the last write that recorded a note read, from
	// 1: every such write — a launch's for its brief, a take — numbers itself
	// one past it, in its own compare-and-swap, and stamps the number on
	// every read it records ([NoteRead.Order]). The order the turn read its
	// notes in is that number, and no clock's: the writes are made by
	// whichever nodes launch, serve the run's bridge and resume the turn,
	// each stamping its own time. A write that serves a note no hook was
	// served before takes one too, and stamps it on that note
	// ([RunNote.Place]), where every later read of the note is recorded.
	Reads int `json:"reads,omitempty"`

	// Extra is every key of the section this build does not know, kept
	// verbatim and written back by every write of the record (wirekeys.go).
	// Filled by the decode, never by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// RunNote is one note on a run's record.
type RunNote struct {
	// ID is the note's identity, the person's request id: a retry of one
	// request is one note. In the clear, because the store dedupes on it.
	ID string `json:"id"`

	// Seq is the note's place among the turn's notes ([RunSteer.Seq]).
	Seq int `json:"seq"`

	// Sealed is the note — its text and who sent it — sealed by the engine
	// under the fleet keyring, bound to the turn and to ID. The only copy of
	// either: no field beside it carries them, so no reader can use them
	// without opening the seal. SIGNED, NOT ENCRYPTED — the seal is what
	// proves a note is a person's, not what keeps it from a reader of the
	// coordination store, which already reads the run's task.
	Sealed []byte `json:"sealed"`

	// At is when the turn took it, on the clock of the node that filed it.
	At time.Time `json:"at"`

	// Read is where the turn read it, nil while it WAITS. Recorded by the
	// write that hands the note over, so a note is never handed over and
	// left waiting, nor recorded read and never handed over.
	Read *NoteRead `json:"read,omitempty"`

	// Served is the job the run's bridge last served the note to while it
	// waited: by a poll ([PendingStore.ServeNotes]), since a harness that
	// polls for its notes (OpenCode's plugin) is answered one only once this
	// records it, and the note is recorded read only once the harness
	// confirms delivering it; or by a hook's serve ([PendingStore.ServeHook]),
	// the note read only once the job's own session shows the hook took it
	// ([PendingStore.SettleHook]). So a note served and not read may have been
	// delivered all the same, its confirmation lost or its harness cut off
	// before it landed, and a launch that continues a session tells it as a
	// note the session may already show (internal/agent/runner). CLEARED where
	// a settle leaves the note no live hook serve to that job: every one lost,
	// or removed as answered nothing. Kept once the note is read; the latest
	// job served it, where more than one was.
	Served string `json:"served,omitempty"`

	// Place is the note's place among the turn's reads ([RunSteer.Reads]),
	// taken by the first write that served it to a job's hook
	// ([PendingStore.ServeHook]): every read of the note recorded after it is
	// recorded at this place, whoever reads it, so the note stays ahead of a
	// correction a person filed after it in every later telling, whatever its
	// harness happened to see first. Zero for a note no hook was served.
	Place int `json:"place,omitempty"`

	// Serves are the note's serves to ONE job's hooks, oldest first, at most
	// [MaxNoteServes] ([HookServed]): a serve to another job replaces them. A
	// serve is LIVE while no loss struck it, the note waits, and Served names
	// its job ([RunNote.LiveServe]): a note a later job's poll served has no
	// live hook serve. Kept once the note is read, as the history its read was
	// confirmed from.
	Serves []HookServed `json:"serves,omitempty"`

	// For is the one job the item is for, where its filer made it that job's
	// alone: filed only while that job is the record's
	// ([PendingStore.FileNote]), bounded apart from a person's notes
	// ([MaxJobItems]), handed over by every write only to a reader inside that
	// job ([RunNote.IsFor]), passed over by every reader of a person's notes
	// (the ending, the carry, the review, a launch's brief, the capture's
	// cursor), and removed by the write of the launch that opens the next job
	// ([openNotes]). Empty for a person's note, an item of the whole turn.
	For string `json:"for,omitempty"`

	// LostTake is TRANSIENT, and set on a note a launch's brief carries
	// ([PendingRun.BriefNotes]) alone: the take that recorded the note read
	// and whose answer never reached its reader, whose settle handed the note
	// to the brief instead ([PendingStore.SettleTake]). The record holds the
	// note read by that take, yet the brief is the first to tell it, so the
	// launch's write records it read by the job it opens, in the take's place
	// and at the take's own place among the reads ([openNotes]): it is that
	// job's as every note its brief carries is — the job's ending reports it
	// where the job never started, and the job's execute record lists it where
	// it did — and it stays where the record first numbered its read, ahead of
	// every read a later take made. Never persisted.
	LostTake string `json:"-"`

	// Extra is every key of the note this build does not know — see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// Waiting reports whether nothing has read the note yet.
func (n RunNote) Waiting() bool { return n.Read == nil }

// IsFor reports whether a reader inside job may be handed the note: a person's
// note, an item of the whole turn, by any reader; an item for one job
// ([RunNote.For]) by a reader inside that job alone. A reader inside no job
// (the review's, a launch's brief) names "" and is handed no job's item.
func (n RunNote) IsFor(job string) bool { return n.For == "" || n.For == job }

// LiveServe is the note's LIVE hook serve: the newest of its serves no loss
// struck, while the note waits and [RunNote.Served] names that serve's job.
// False where no hook holds a serve of it now: read, every serve lost, or
// served since by another job's poll.
func (n RunNote) LiveServe() (HookServed, bool) {
	if n.Read != nil || n.Served == "" {
		return HookServed{}, false
	}
	for i := len(n.Serves) - 1; i >= 0; i-- {
		if s := n.Serves[i]; s.Launch == n.Served && s.Lost == "" {
			return s, true
		}
	}
	return HookServed{}, false
}

// HookServed is one hook's serve of a note to a job, as the record keeps it
// ([PendingStore.ServeHook]): the take that served it, the job and the step of
// its harness's loop the hook answered, when, the call it answered, and why
// the harness is known not to hold the note from it, once that is known.
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), so a key a later build adds
// to a serve rides every write this build makes.
type HookServed struct {
	// Take names the serve, fresh for every take: the name its answer carries
	// to the job's session, and what a settle finds the serve by
	// ([PendingStore.SettleHook]).
	Take string `json:"take"`

	// Launch is the job whose hook was served.
	Launch string `json:"launch"`

	// Boundary and Iteration are the step of the harness's loop the hook
	// answered, on the agent layer's scale (its steer boundary as text:
	// tool_batch or finish) and the job's iteration.
	Boundary  string `json:"boundary"`
	Iteration int    `json:"iteration,omitempty"`

	// At is when, on the clock of the node whose write recorded the serve, as
	// every read's is ([NoteRead.At]); a read confirmed off the serve is at it.
	At time.Time `json:"at"`

	// Call is the call the serve answered, as the harness names it when it
	// gives the call up; nil where the bridge could not read it, or the store
	// keeps none of its shape ([keptCallOf]), so only the job's session can
	// settle the serve.
	Call *NoteCall `json:"call,omitempty"`

	// Lost is why the harness is known not to hold the note from this serve;
	// empty while that is not known.
	Lost ServeLoss `json:"lost,omitempty"`

	// Extra is every key of the serve this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// MayHold reports whether the session of the serve's job may hold the note from
// this serve: one no loss struck, or one its harness cancelled, whose answer
// may have outrun the cancel. False for a serve the job's whole session proves
// it never took. A loss this build does not know may hold it too, the reading
// that never tells a session holding a note that it lacks it.
func (s HookServed) MayHold() bool { return s.Lost != ServeAbsent }

// NoteCall is the call a hook's serve answered, as its harness names it when it
// gives the call up: the MCP session the call came on, and the call's JSON-RPC
// id as canonical JSON text (7, or "a"). A serve keeps one only in the shape
// every real call has, and records any other as no call, never cut
// ([keptCallOf]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), as a serve is.
type NoteCall struct {
	Session string `json:"session"`
	ID      string `json:"id"`

	// Extra is every key of the call this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// is reports whether c and other name one call: one session AND one id, since
// a harness numbers its calls per session.
func (c NoteCall) is(other NoteCall) bool { return c.Session == other.Session && c.ID == other.ID }

// keptCallOf is call as a serve keeps it: whole where its session is of the id
// charset (letters, digits, `-` and `_`, [ValidCallID]'s) and its id is a JSON
// integer's text or a JSON string of that charset, each at most
// [MaxNoteCallBytes], a string's quotes counted; and NONE otherwise, as where
// it names no session or no id.
//
// ONE RULE, THE STORE'S. The call is the box's word, which the bridge hands on
// as it read it, so the write that serves holds every writer to the rule,
// whatever bridge, or build, handed the call, as the log holds the id a
// bridged call keeps ([keptCallID]).
//
// THE CHARSET REFUSES NOTHING REAL, AND HOLDS THE ROW. Claude Code numbers its
// JSON-RPC calls with integers, and the session is the bridge's own, which
// go-sdk mints as rand.Text, in base32. A kept call is then written as itself,
// each half costing the row its length and a string id's two quotes a byte
// more each, where the store's encoder writes a byte it escapes as six
// (`\u003c`): so the row's worst serve carries a call at its length
// (TestTheWholeRowFitsItsBudget).
//
// NEVER CUT: a cut call could match another call cut alike, and lose a serve
// its harness kept. A serve kept with no call is settled by its job's session
// alone.
func keptCallOf(call *NoteCall) *NoteCall {
	if call == nil || !validNoteSession(call.Session) || !validNoteCallID(call.ID) {
		return nil
	}
	kept := *call
	return &kept
}

// validNoteSession reports whether session is a call's session a serve keeps:
// of the id charset, at most [MaxNoteCallBytes] of it.
func validNoteSession(session string) bool {
	return session != "" && len(session) <= MaxNoteCallBytes && !strings.ContainsFunc(session, outsideIDCharset)
}

// validNoteCallID reports whether id is a call's id a serve keeps: a JSON
// integer's text or a JSON string of the id charset, at most
// [MaxNoteCallBytes] with its quotes.
func validNoteCallID(id string) bool {
	return len(id) <= MaxNoteCallBytes && (jsonInteger(id) || idString(id))
}

// jsonInteger reports whether text is a JSON integer's: an optional minus,
// then digits with no leading zero but a lone one.
func jsonInteger(text string) bool {
	digits := strings.TrimPrefix(text, "-")
	return digits != "" && (digits == "0" || digits[0] != '0') &&
		!strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
}

// idString reports whether text is a JSON string of the id charset: two
// quotes around letters, digits, `-` and `_`, none of which JSON escapes.
func idString(text string) bool {
	inner, opened := strings.CutPrefix(text, `"`)
	inner, closed := strings.CutSuffix(inner, `"`)
	return opened && closed && !strings.ContainsFunc(inner, outsideIDCharset)
}

// ServeLoss is why a hook's serve is known not to have reached its job's
// session.
type ServeLoss string

const (
	// ServeCancelled is a serve whose call its harness cancelled: whatever the
	// hook was answered, the harness gave it up. Its session may hold the
	// note all the same, where the answer outran the cancel, which only the
	// session settles ([PendingStore.SettleHook]).
	ServeCancelled ServeLoss = "cancelled"

	// ServeAbsent is a serve the job's whole session proves it never took.
	ServeAbsent ServeLoss = "absent"
)

// ServeLosses are the losses this build knows.
var ServeLosses = []ServeLoss{ServeCancelled, ServeAbsent}

// Valid reports whether this build knows the loss. A loss it does not know is a
// loss all the same: every reader asks whether a serve is lost, and only
// whether its session may hold the note asks which ([HookServed.MayHold]).
func (l ServeLoss) Valid() bool { return slices.Contains(ServeLosses, l) }

// EndedNote is a note on a run's record whose end the run's ending reports
// ([PendingRun.EndedNotes]), what it reports it as, and where.
type EndedNote struct {
	Note RunNote

	// Outcome is what the record knows became of the note: delivered, for a
	// note a launch's brief told again ([NoteRead.Retold]); unconfirmed;
	// missed; or expired.
	Outcome types.SteerOutcome

	// Read is the read the outcome is at: for a note delivered, the brief
	// that told it again; for one missed, the take's whose answer was lost —
	// the note's own read, or, where a brief told the note again and its job
	// never started, the take's read that brief kept ([NoteRead.Retold]). Nil
	// for a note unconfirmed, which is at its hook's serve or at the job it was
	// served to ([RunNote.Served]), for one missed at a hook's serve, and for
	// one expired, at no step.
	Read *NoteRead

	// Serve is the hook's serve the outcome is at, where it is one: for a note
	// unconfirmed, the serve that was live before every read of it that handed
	// nothing over, a take marked unanswered or the brief of a job that never
	// started ([RunNote.LiveServe] of the note unread); for one missed that no
	// read marked, the last of its serves, every one of them lost. Nil
	// otherwise.
	Serve *HookServed
}

// EndedNotes is every note on the run's record that no step reported reading,
// each as what the record knows became of it: what the run's ending reports
// ([CoordinatorOptions.SteersEnded]), decided in this order of precedence.
//
//   - DELIVERED: read by a launch's brief that told it again ([NoteRead.Retold])
//     after a take that recorded it read was marked unanswered, where that
//     job started. Its harness was handed the note in its brief; the step that
//     took it first never handed it over, and nothing reported the telling.
//   - UNCONFIRMED: SERVED to a job's harness ([RunNote.Served]) and read by no
//     step that handed it over: still waiting, or read only by a take marked
//     unanswered or by the brief of a job that never started. The session it
//     was handed into may hold it, and nothing says it does; a note that may
//     have reached the run is never said to have reached nothing. At the hook's
//     serve that was live before those reads, where a hook was served it
//     ([EndedNote.Serve]), the step it was handed over at; else at the job a
//     poll served it to.
//   - MISSED: read by a take marked UNANSWERED ([NoteRead.Unanswered]), and
//     told again by no launch whose job started: the take's write landed, its
//     answer never reached its reader, and no later write records the note
//     handed to a harness. A launch that tells it again records the telling
//     and clears the mark ([PendingRun.RetoldNotes]), keeping the marked
//     take's read beside it ([NoteRead.Retold]): so a note still marked is one
//     no launch recorded telling again, and one whose telling's job never
//     started is missed all the same, at that take, since the brief handed
//     nothing over. And SERVED TO A JOB'S HOOKS AND EVERY SERVE LOST
//     ([HookServed.Lost]), read by nothing since: its harness cancelled the
//     call, or its session proves it never took the note. At the last of
//     those serves ([EndedNote.Serve]).
//   - EXPIRED: still WAITING, or read by the BRIEF OF THE RECORD'S CURRENT JOB
//     where that job never started and that brief was the first to tell it:
//     the job's record never took its command ([LaunchRecord.Started]),
//     because the launch failed, or was ended, between the write that opened
//     the job and the job's start.
//
// A note any other step read is none of them: that step published it delivered
// as it read it, and the ending reports nothing of it. NOR IS AN ITEM FOR ONE
// JOB ([RunNote.For]): the ending reports what became of a person's notes, and
// what becomes of a job's item is its filer's to say.
//
// NEVER THE ROW'S COMMAND ([PendingRun.CommandID]): that is the box's, and goes
// with the box — a park's teardown and an expired pause clear it from a job
// that ran — so a turn ending after either would report expired the notes its
// job's brief carried, which its runner had already recorded delivered.
//
// THE RECORD IS THE ONE WITNESS. A launch records its brief's notes read in the
// write that opens its job ([PendingStore.BeginLaunch]), before it knows whether
// the job will run, and every way it can then fail ends the record
// ([Coordinator.abandon]) — or meets an ending another party decided, which
// finishes in its place. So the ending is the one party that sees both what the
// brief took and that no job ran with it, and it reports those notes under the
// same ids as the waiting ones: a launch that reported them itself could not
// tell a note its own brief took from one another node's ending had already
// reported, and published one twice. And the same holds of a take whose
// answer was lost, which reports nothing as it is lost; of a note a brief told
// again, whose job is known to have started only once the launch has returned;
// and of a note a harness was handed and never confirmed, which nothing else
// sees unconfirmed.
func (r PendingRun) EndedNotes() []EndedNote {
	var out []EndedNote
	for _, n := range r.Steer.Notes {
		if end, ended := r.endOf(n); ended {
			out = append(out, end)
		}
	}
	return out
}

// endOf is what the run's ending reports n as, and where, and false for a note
// a step reported reading: see [PendingRun.EndedNotes].
//
// A BRIEF WHOSE JOB NEVER STARTED HANDED NOTHING OVER, so what the record knows
// of a note that brief read is what it knew before the brief: that nothing
// read it, where the brief was the first to tell it, and that a take whose
// answer was lost read it, where the brief told it again — the take's read
// the brief kept ([NoteRead.Retold]). NOR DID A TAKE MARKED UNANSWERED, whose
// answer never reached its reader ([NoteRead.Unanswered]). So a hook's serve
// that was live before every read of the note that handed nothing over is
// live still, and a note unconfirmed is reported at it: the step its hook
// answered, never a poll's.
func (r PendingRun) endOf(n RunNote) (EndedNote, bool) {
	if n.For != "" {
		return EndedNote{}, false
	}
	read := n.Read
	if r.briefNeverRan(n) {
		read = read.Retold
	}
	lost := lostServe(n)
	end := EndedNote{Note: n}
	switch {
	case read != nil && read.Retold != nil:
		end.Outcome, end.Read = types.SteerDelivered, read
	case read != nil && !read.Unanswered:
		return EndedNote{}, false
	case n.Served != "":
		// EVERY READ OF THE NOTE HANDED NOTHING OVER here: none read it, or a
		// take marked unanswered did, or the brief of a job that never
		// started, so its serve is asked of the note as it stood unread.
		end.Outcome = types.SteerUnconfirmed
		unread := n
		unread.Read = nil
		if live, ok := unread.LiveServe(); ok {
			end.Serve = &live
		}
	case read != nil:
		end.Outcome, end.Read = types.SteerMissed, read
	case lost != nil:
		end.Outcome, end.Serve = types.SteerMissed, lost
	default:
		end.Outcome = types.SteerExpired
	}
	return end, true
}

// lostServe is the last of n's serves a loss struck, nil where none did.
func lostServe(n RunNote) *HookServed {
	for i := len(n.Serves) - 1; i >= 0; i-- {
		if s := n.Serves[i]; s.Lost != "" {
			return &s
		}
	}
	return nil
}

// briefNeverRan reports whether n was read by the brief of the record's job
// while that job has not started.
func (r PendingRun) briefNeverRan(n RunNote) bool {
	return n.Read != nil && !r.Launch.Started && r.LaunchID != "" &&
		n.Read.Launch == r.LaunchID && n.Read.Boundary == string(types.SteerBoundaryBrief)
}

// endsNotes reports whether the run's record holds a note its ending reports.
func (r PendingRun) endsNotes() bool { return len(r.EndedNotes()) > 0 }

// NoteRead is where a note was read: the phase, iteration and round on the
// phase's own scale, the kind of step (the agent layer's vocabulary, carried as
// text: the record is this package's wire format and imports no agent
// package), and the job whose harness read it — empty for a read the engine's own
// loop made. At is when, on the clock of the node whose write recorded it, for
// a person to read; Order is the place of that write among the record's reads
// ([RunSteer.Reads]), which is what orders them — a note's Seq orders the reads
// one write recorded. A note a hook was served is read at its own place, its
// first serve's ([RunNote.Place]), whichever write reads it.
type NoteRead struct {
	At        time.Time `json:"at"`
	Order     int       `json:"order,omitempty"`
	Phase     string    `json:"phase"`
	Iteration int       `json:"iteration,omitempty"`
	Round     int       `json:"round,omitempty"`
	Boundary  string    `json:"boundary"`
	Launch    string    `json:"launch,omitempty"`

	// Take is the identity of the take whose write recorded the read, as its
	// taker named it ([NoteTake]'s Read): what [PendingStore.SettleTake] finds
	// the take's reads by once its answer was lost. Empty for a read no take
	// recorded — a launch's brief — and for one a take that named none recorded,
	// an older build's among them.
	Take string `json:"take,omitempty"`

	// Unanswered says the take that recorded the read never answered its
	// reader: its write landed, and its answer did not reach the harness that
	// asked ([PendingStore.SettleTake]). The note is still read by the record's
	// word — no later take hands it over again, and every reader that carries
	// the turn's read notes carries it, the review that judges the work among
	// them — but the session of the job named does not hold it from this read,
	// so no capture of that session counts it held (notesHeld), and every
	// later launch tells it again, whose write then records it read by that
	// launch's job ([NoteRead.Retold]). Marked still when the turn ends, it is
	// reported missed ([PendingRun.EndedNotes]).
	Unanswered bool `json:"unanswered,omitempty"`

	// Retold is the read a launch's brief told the note again in place of,
	// where this read is that brief's ([PendingRun.RetoldNotes]): a take's,
	// marked unanswered, as the take's settle left it — its job, its step and
	// its own place among the reads. Nil on every other read. The launch's
	// write recorded this read in that take's place and at that place among
	// the reads. Its job's harness was handed the note in its brief, where the
	// job started, so the session of that job holds it from here; but no step
	// published it delivered, as the step that reads a note does, so the
	// run's ending reports it ([PendingRun.EndedNotes]): delivered at this
	// brief where its job started, and missed at the take's read kept here
	// where it never did — the brief handed nothing over, and the take's
	// reader never had the note.
	Retold *NoteRead `json:"retold,omitempty"`

	// Restarted says the read is a hook's serve its job's session shows taken
	// BEFORE the session's last restart (a compaction, or a later root of its
	// thread) ([PendingStore.SettleHook]): the harness took the note, and a
	// resume of that session rebuilds its conversation from the restart on,
	// which is not known to hold it. Delivered all the same, and carried; but
	// no capture of the session counts it held ([notesHeld]), so a launch that
	// continues one tells it again.
	Restarted bool `json:"restarted,omitempty"`

	// Extra is every key of the read this build does not know — see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// NoteFiled is what became of a note filed on a run's record.
type NoteFiled string

const (
	// NoteAccepted — the note is on the record, waiting.
	NoteAccepted NoteFiled = "accepted"
	// NoteDuplicate — a note with the same id is already on the record:
	// the same request, retried. Nothing was added, and the first filing's
	// answer stands.
	NoteDuplicate NoteFiled = "duplicate"
	// NoteFull — as many notes as may wait are already waiting.
	NoteFull NoteFiled = "full"
	// NoteSpent — the turn has taken as many notes as it takes.
	// For an item for one job: that job holds [MaxJobItems] already.
	NoteSpent NoteFiled = "spent"
	// NoteClosed — the run's ending is decided, or the run has no record:
	// nothing will read the note. Or the note is an item for a job the
	// record no longer names ([RunNote.For]), which nothing will read either.
	NoteClosed NoteFiled = "closed"
)

// NoteFiling is one note to file on a run's record, and the turn's bounds.
type NoteFiling struct {
	Note RunNote

	// MaxWaiting is how many notes may wait unread at once, and MaxNotes how
	// many the turn takes in all — internal/agent/steer's MaxPendingNotes and
	// MaxTurnNotes, which the caller passes so the two numbers have one home.
	// Both required. A PERSON'S BOUNDS: they count a person's notes alone, and
	// an item for one job ([RunNote.For]) is held to [MaxJobItems] apart from
	// them.
	MaxWaiting, MaxNotes int
}

// NoteTake is a step taking notes off a run's record and recording them read.
//
// PINNED, every way a take can be stale: to the JOB the taker serves (Launch,
// the record's [PendingRun.LaunchID]), to the STATUSES the taker may act in, to
// an ending NOT DECIDED, and to the taker's LEASE (Fence; zero for a take that
// holds none, as the bridge's does). A take that fails any of them takes
// nothing — a run's previous job, a session left open on a node the seat moved
// away from, and a resumed segment whose claim another node outranked are each
// a step that is no longer the turn's.
type NoteTake struct {
	// IDs are the notes to take, by id; one no longer waiting is skipped.
	// Empty takes nothing — a taker names what it can deliver, and a note
	// it could not open is one it must not record read. An item for a job
	// the take's Read does not name is skipped too ([RunNote.IsFor]), so a
	// Read that names no job, as the review's does, takes no item at all.
	IDs []string

	Launch   string
	Statuses []string
	Fence    Fence

	// Read is where the notes are read; the store stamps its At and Order.
	// Its Take names this take, fresh for every call, so a taker whose call
	// answered an error can find what the call recorded
	// ([PendingStore.SettleTake]).
	Read NoteRead
}

// NoteServe is a poll of a run's notes serving them to the run's harness
// ([PendingStore.ServeNotes]), PINNED as a take is ([NoteTake]): to the job the
// poll serves, to the statuses it may serve in, and to an undecided ending.
// Unlike a take it holds no lease: a poll is the bridge's, as its takes are.
type NoteServe struct {
	// IDs are the notes to serve, by id; one no longer waiting is skipped,
	// and so is an item for another job ([RunNote.IsFor]).
	IDs []string

	Launch   string
	Statuses []string
}

// HookServe is a hook's take serving notes to its job's harness
// ([PendingStore.ServeHook]), PINNED as a poll's serve is ([NoteServe]): to the
// job the hook serves, to the statuses it may serve in, and to an undecided
// ending, with no lease, the hook's take being the bridge's.
type HookServe struct {
	// IDs are the notes to serve, by id.
	IDs []string

	Launch   string
	Statuses []string

	// Serve is the serve as the record keeps it ([HookServed]): its Take,
	// fresh for every take, and the step it answers; and the Call it answers,
	// held to [MaxNoteCallBytes] and the id charset and kept as none outside
	// them ([keptCallOf]). The store stamps its job (Launch) and its At, and
	// no loss.
	Serve HookServed

	// Cancelled are the calls the serving inbox heard its harness cancel:
	// the write loses each live serve to the job that answered one, before it
	// serves, so the note is served again in the same write.
	Cancelled []NoteCall

	// Unanswered are the serving inbox's takes whose hooks were answered
	// nothing: the write removes each one's serve to the job before it
	// serves, so the note is served again without spending its bound.
	Unanswered []string
}

// HookSettle is a settle of a job's hooks' serves ([PendingStore.SettleHook]),
// by their takes and the calls they answered: the facts a serving inbox learns
// once its hook was answered, and the verdict of the job's own session.
type HookSettle struct {
	// Launch is the job whose serves are settled; required.
	Launch string

	// Unanswered are takes whose hooks were answered nothing: their serves go,
	// and each note waits again, no session holding it from them.
	Unanswered []string

	// Cancelled are calls the job's harness cancelled.
	Cancelled []NoteCall

	// Held are takes the job's session shows taken, after its last restart.
	Held []string

	// Restarted are takes the job's session shows taken before its last
	// restart ([NoteRead.Restarted]).
	Restarted []string

	// Absent are takes the job's whole session proves it never took.
	Absent []string
}

// NoteSettle is a take settled once its answer was lost
// ([PendingStore.SettleTake]).
type NoteSettle struct {
	// Take is the take's identity, as its read named it ([NoteRead.Take]).
	Take string

	// Unanswered says the take's reader never had its notes — a harness's
	// hook answered nothing — so every read it recorded is marked so
	// ([NoteRead.Unanswered]). Unset, they are answered as they stand, to a
	// reader that hands them over itself.
	Unanswered bool
}

// NotesView is what a reader following a run's notes needs of its record, and
// nothing else: a view is decoded on every write the record takes — every
// bridged call the run makes — so it reads four fields and the notes, never the
// whole run.
type NotesView struct {
	// Found is false while the run has no record.
	Found bool

	Status   string
	LaunchID string

	// EndingDecided is whether the run's ending is decided: from then on
	// nothing takes a note off the record.
	EndingDecided bool

	Steer RunSteer
}

// notesWire is the slice of a record a [NotesView] decodes.
type notesWire struct {
	Status   string          `json:"status"`
	LaunchID string          `json:"launch_id"`
	Ending   json.RawMessage `json:"ending"`
	Steer    RunSteer        `json:"steer"`
}

// viewOf decodes the view of one state of a run's record.
func viewOf(update coord.SandboxRunUpdate) (NotesView, error) {
	if !update.Found {
		return NotesView{}, nil
	}
	var wire notesWire
	if err := json.Unmarshal(update.Record.Value, &wire); err != nil {
		return NotesView{}, fmt.Errorf("sandbox: decode the notes of run %s: %w", update.Record.Key, err)
	}
	ending := bytes.TrimSpace(wire.Ending)
	return NotesView{
		Found: true, Status: wire.Status, LaunchID: wire.LaunchID,
		EndingDecided: len(ending) > 0 && !bytes.Equal(ending, []byte("null")),
		Steer:         wire.Steer,
	}, nil
}

// errNoteBounds is a filing that names no bounds.
var errNoteBounds = errors.New("sandbox: a note is filed with the turn's bounds, and none were given")

// FileNote files a note on a run's record. See the contract on [PendingStore].
//
// NO FENCE, as [CoordStore.AppendBridgeCall] takes none: a person's note is an
// append to the turn, never an ownership decision, and refusing one because the
// seat's lease moved would lose a note whose turn goes on either way. The
// record's own version still guards the write.
func (s *CoordStore) FileNote(ctx context.Context, turnID string, filing NoteFiling) (NoteFiled, PendingRun, error) {
	note := filing.Note
	switch {
	case filing.MaxWaiting <= 0 || filing.MaxNotes <= 0:
		return "", PendingRun{}, errNoteBounds
	case note.ID == "" || len(note.Sealed) == 0:
		return "", PendingRun{}, fmt.Errorf("sandbox: a note to run %s needs its id and its seal", turnID)
	case len(note.Sealed) > MaxNoteSealedBytes:
		// HELD TO ITS SHARE OF THE ROW before anything is written: see
		// [MaxNoteSealedBytes].
		return "", PendingRun{}, fmt.Errorf("sandbox: a note to run %s is sealed in %d bytes, past the "+
			"%d a note may be: %w", turnID, len(note.Sealed), MaxNoteSealedBytes, ErrNoteTooLarge)
	}
	if note.At.IsZero() {
		note.At = s.clock()
	}
	// FILED WAITING, SERVED TO NOBODY: what a note's reads and serves say is
	// the store's to record, never a filer's to hand in.
	note.Read, note.Served, note.Place, note.Serves = nil, "", 0, nil
	var (
		filed NoteFiled
		read  PendingRun
	)
	run, won, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		read = *run
		notes := run.Steer.Notes
		if slices.ContainsFunc(notes, func(held RunNote) bool { return held.ID == note.ID }) {
			filed = NoteDuplicate
			return false
		}
		if note.For != "" {
			// AN ITEM FOR ONE JOB, filed only while that job is the record's,
			// in this decide: an item whose filer lost a race to the launch
			// that opens the next job is never written behind it, since
			// nothing in the job it names will read it.
			switch {
			case run.LaunchID != note.For:
				filed = NoteClosed
				return false
			case jobItems(notes, note.For) >= MaxJobItems:
				filed = NoteSpent
				return false
			}
		} else {
			// SPENT BEFORE FULL, as a turn's box answers: a turn that has
			// taken all it takes will never take this note.
			mine, waiting := personsNotes(notes, run.LaunchID)
			switch {
			case mine >= filing.MaxNotes:
				filed = NoteSpent
				return false
			case waiting >= filing.MaxWaiting:
				filed = NoteFull
				return false
			}
		}
		filed = NoteAccepted
		run.Steer.Seq++
		note.Seq = run.Steer.Seq
		run.Steer.Notes = append(slices.Clone(notes), note)
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return NoteClosed, PendingRun{}, nil
	case err != nil:
		return "", PendingRun{}, err
	case won:
		return NoteAccepted, run, nil
	case !found:
		// NOTHING WAS DECIDED ON: the run has no record — the last attempt
		// found none, whatever an earlier one decided before it lost.
		return NoteClosed, PendingRun{}, nil
	}
	return filed, read, nil
}

// jobItems is how many of notes are items for job ([RunNote.For]): what
// [MaxJobItems] bounds, whichever write files one.
func jobItems(notes []RunNote, job string) int {
	count := 0
	for _, held := range notes {
		if held.For != "" && held.For == job {
			count++
		}
	}
	return count
}

// personsNotes is how many of notes are a person's, the turn's own and no job's
// item, and how many of those wait to be read at the turn's next step: every
// one unread but a note a hook's live serve handed to job, the record's own
// ([RunNote.LiveServe]). A hook's answer hands its notes to the model at the
// step it answers, so such a note no longer waits for a step to read it,
// whatever its session's record later says; a note a poll served still does,
// until its harness confirms delivering it at its next step.
func personsNotes(notes []RunNote, job string) (mine, waiting int) {
	for _, held := range notes {
		if held.For != "" {
			continue
		}
		mine++
		if !held.Waiting() {
			continue
		}
		if live, ok := held.LiveServe(); ok && live.Launch == job {
			continue
		}
		waiting++
	}
	return mine, waiting
}

// TakeNotes records notes read and answers the ones this take recorded. See the
// contract on [PendingStore].
func (s *CoordStore) TakeNotes(ctx context.Context, turnID string, take NoteTake) ([]RunNote, error) {
	if len(take.IDs) == 0 {
		return nil, nil
	}
	if take.Launch == "" || len(take.Statuses) == 0 {
		return nil, fmt.Errorf("sandbox: a take of run %s's notes names no job or no status it "+
			"may take in", turnID)
	}
	read := take.Read
	var taken []RunNote
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		taken = nil
		if run.LaunchID != take.Launch || !slices.Contains(take.Statuses, run.Status) ||
			outranked(*run, take.Fence) {
			return false
		}
		// THIS WRITE'S PLACE among the record's reads, stamped on each read
		// it records — see [RunSteer.Reads].
		// But a note a hook was served is read at its own place
		// ([RunNote.Place]), and the record moves its count only where a
		// read took the write's new place.
		read.At = s.clock()
		order, placed := run.Steer.Reads+1, false
		notes := slices.Clone(run.Steer.Notes)
		for i, held := range notes {
			if !held.Waiting() || !held.IsFor(take.Read.Launch) || !slices.Contains(take.IDs, held.ID) {
				continue
			}
			marked := read
			if marked.Order = held.Place; marked.Order == 0 {
				marked.Order, placed = order, true
			}
			held.Read = &marked
			notes[i] = held
			taken = append(taken, held)
		}
		if len(taken) == 0 {
			return false
		}
		run.Steer.Notes = notes
		if placed {
			run.Steer.Reads = order
		}
		return true
	})
	if err != nil || !won {
		// NOT TAKEN unless the write landed: what a decision chose before it
		// lost a race, to a record that then went, was never recorded read.
		return nil, err
	}
	return taken, nil
}

// ServeNotes records notes served to a job's harness, and answers every one
// named that waits served to it. See the contract on [PendingStore].
//
// THROUGH THE WRITE A LIVE RUN TAKES, as a filing is ([CoordStore.FileNote]): a
// run whose ending is decided serves nothing more, and the refusal is an answer,
// never an error.
func (s *CoordStore) ServeNotes(ctx context.Context, turnID string, serve NoteServe) ([]RunNote, error) {
	if len(serve.IDs) == 0 {
		return nil, nil
	}
	if serve.Launch == "" || len(serve.Statuses) == 0 {
		return nil, fmt.Errorf("sandbox: a serve of run %s's notes names no job or no status it "+
			"may serve in", turnID)
	}
	var served []RunNote
	_, _, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		served = nil
		if run.LaunchID != serve.Launch || !slices.Contains(serve.Statuses, run.Status) {
			return false
		}
		notes := slices.Clone(run.Steer.Notes)
		marked := false
		for i, held := range notes {
			if !held.Waiting() || !held.IsFor(serve.Launch) || !slices.Contains(serve.IDs, held.ID) {
				continue
			}
			if held.Served != serve.Launch {
				held.Served = serve.Launch
				notes[i] = held
				marked = true
			}
			served = append(served, held)
		}
		run.Steer.Notes = notes
		// WRITTEN ONLY FOR A MARK NOT MADE YET: a poll answered again from the
		// same view serves what an earlier one already recorded.
		return marked
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return nil, nil
	case err != nil:
		return nil, err
	case !found:
		// NOTHING WAS DECIDED ON: the record is gone, whatever an earlier
		// attempt decided before it lost.
		return nil, nil
	}
	return served, nil
}

// errSettleUnnamed is a settle that names no take.
var errSettleUnnamed = errors.New("sandbox: a take of a run's notes is settled by its identity, and none was given")

// SettleTake settles a take whose [CoordStore.TakeNotes] answered an error. See
// the contract on [PendingStore].
//
// THROUGH THE WRITE EVERY RUN TAKES, its ending decided or not: what the take
// recorded is a fact about the take, true whoever holds the record, and the
// take's own write is in flight whatever the run's state. And ANSWERING NOTHING
// WHERE THE LAST ATTEMPT FOUND NO RECORD, as a filing does
// ([CoordStore.FileNote]): an attempt that decided and then lost its race to
// the record's removal recorded nothing, and what it decided is no answer.
func (s *CoordStore) SettleTake(ctx context.Context, turnID string, settle NoteSettle) ([]RunNote, error) {
	if settle.Take == "" {
		return nil, errSettleUnnamed
	}
	var recorded []RunNote
	_, _, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		recorded = nil
		notes := slices.Clone(run.Steer.Notes)
		marked := false
		for i, held := range notes {
			if held.Read == nil || held.Read.Take != settle.Take {
				continue
			}
			if settle.Unanswered && !held.Read.Unanswered {
				read := *held.Read
				read.Unanswered = true
				held.Read = &read
				notes[i] = held
				marked = true
			}
			recorded = append(recorded, held)
		}
		run.Steer.Notes = notes
		// WRITTEN WHERE THE TAKE RECORDED NOTHING, its record unchanged but
		// for the write itself: the take's own write may still be in flight —
		// sent before its caller gave up on the answer, and taken by the store
		// after this read — and it was conditioned on a version this write
		// moves past, so once this lands it never can. Where the take's reads
		// are on the record, its write landed and nothing of it is left in
		// flight: only a mark asked for and not yet made is written.
		return marked || len(recorded) == 0
	})
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, nil
	}
	return recorded, nil
}

// errHookServeUnnamed is a hook's serve that names no job, no status or no take.
var errHookServeUnnamed = errors.New("sandbox: a hook's serve of a run's notes names its job, the statuses it " +
	"may serve in and its take, and one was not given")

// ServeHook records the notes a hook's serve names served to its job's hook, and
// answers the ones this call served. See the contract on [PendingStore].
//
// THROUGH THE WRITE A LIVE RUN TAKES, pinned as a take is
// ([CoordStore.TakeNotes]): a run whose ending is decided serves nothing more,
// and a record gone, or on another job or in another status, is nothing
// served, never an error.
func (s *CoordStore) ServeHook(ctx context.Context, turnID string, serve HookServe) ([]RunNote, error) {
	if serve.Launch == "" || len(serve.Statuses) == 0 || serve.Serve.Take == "" {
		return nil, fmt.Errorf("sandbox: run %s: %w", turnID, errHookServeUnnamed)
	}
	var served []RunNote
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		served = nil
		if run.LaunchID != serve.Launch || !slices.Contains(serve.Statuses, run.Status) {
			return false
		}
		notes := slices.Clone(run.Steer.Notes)
		changed := false
		// FIRST WHAT THE SERVING INBOX KNOWS ITS HARNESS NEVER TOOK: a live
		// serve whose call it heard cancelled is lost, and the serve of a
		// take it answered nothing is removed, in this write, so the note is
		// served again here whatever the inbox's view of the record, or a
		// settle made off the hook's path, has caught up with.
		for i, held := range notes {
			if settled, touched := unserved(held, serve.Launch, serve.Cancelled, serve.Unanswered); touched {
				notes[i], changed = settled, true
			}
		}
		at := s.clock()
		order, placed := run.Steer.Reads+1, false
		for i, held := range notes {
			if !slices.Contains(serve.IDs, held.ID) || !held.Waiting() || !held.IsFor(serve.Launch) {
				continue
			}
			if live, ok := held.LiveServe(); ok && live.Launch == serve.Launch {
				continue
			}
			// A LIST OF ANOTHER JOB'S SERVES COUNTS NONE, and is replaced:
			// the bound is the serves of one note to one job.
			serves := slices.DeleteFunc(slices.Clone(held.Serves), func(h HookServed) bool {
				return h.Launch != serve.Launch
			})
			if len(serves) >= MaxNoteServes {
				continue
			}
			// THE FIRST SERVE PLACES THE NOTE among the turn's reads, once a
			// write: every later read of it is recorded at this place.
			if held.Place == 0 {
				held.Place, placed = order, true
			}
			next := serve.Serve
			next.Launch, next.At, next.Lost, next.Call = serve.Launch, at, "", keptCallOf(serve.Serve.Call)
			serves = append(serves, next)
			held.Serves, held.Served = serves, serve.Launch
			notes[i], changed = held, true
			served = append(served, held)
		}
		if !changed {
			return false
		}
		run.Steer.Notes = notes
		if placed {
			run.Steer.Reads = order
		}
		return true
	})
	if err != nil || !won {
		// NOT SERVED unless the write landed: what a decision chose before it
		// lost a race, to a record that then went or to the run's ending
		// decided under it (which mutate answers as nothing won, never an
		// error), was never recorded served.
		return nil, err
	}
	return served, nil
}

// errHookSettleUnnamed is a settle of hooks' serves that names no job.
var errHookSettleUnnamed = errors.New("sandbox: a settle of a hook's serves names the job they served, and none " +
	"was given")

// SettleHook settles a job's hooks' serves by their takes and the calls they
// answered. See the contract on [PendingStore].
//
// THROUGH THE WRITE EVERY RUN TAKES, its ending decided or not, as a take's
// settle is ([CoordStore.SettleTake]): what a hook's serve handed over is a
// fact about the job, true whoever holds the record, and a serve's own write
// can be in flight whatever the run's state. And ANSWERING NOTHING WHERE THE
// LAST ATTEMPT FOUND NO RECORD: an attempt that decided and then lost its race
// to the record's removal recorded nothing, and what it decided is no answer.
func (s *CoordStore) SettleHook(ctx context.Context, turnID string, settle HookSettle) ([]RunNote, error) {
	if settle.Launch == "" {
		return nil, errHookSettleUnnamed
	}
	confirms := len(settle.Held) > 0 || len(settle.Restarted) > 0
	var answered []RunNote
	_, _, found, err := s.writeFound(ctx, turnID, false, Fence{}, func(run *PendingRun) bool {
		answered = nil
		// A TAKE NAMED UNANSWERED THAT NO SERVE ON THE RECORD NAMES may have
		// its own write in flight: sent before its caller gave up on it, and
		// taken by the store only after this read. It was conditioned on a
		// version this write moves past, so the record is written all the
		// same, and once this lands that serve never can. Where the take's
		// serve is on the record, its write landed and nothing of it is left
		// in flight.
		inFlight := slices.ContainsFunc(settle.Unanswered, func(take string) bool {
			return !slices.ContainsFunc(run.Steer.Notes, func(n RunNote) bool {
				return slices.ContainsFunc(n.Serves, func(h HookServed) bool { return h.Take == take })
			})
		})
		notes := slices.Clone(run.Steer.Notes)
		order, placed, changed := run.Steer.Reads+1, false, false
		for i, held := range notes {
			next, touched, tookPlace := settledServes(held, settle, order)
			if touched {
				notes[i], changed, placed = next, true, placed || tookPlace
			}
			if touched || (confirms && readByServeOf(next, settle.Launch)) {
				answered = append(answered, next)
			}
		}
		if !changed {
			return inFlight
		}
		run.Steer.Notes = notes
		if placed {
			run.Steer.Reads = order
		}
		return true
	})
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, nil
	}
	return answered, nil
}

// unserved is n with its serves to job settled by what its harness is known
// never to have taken: the serve of a take named in unanswered is removed, its
// hook having been answered nothing, and the LIVE serve whose call is one named
// in cancelled is lost to the cancel; [RunNote.Served] is cleared where no live
// serve to job is left. It reports whether anything changed. A note read is
// left as it is: its serves are its read's history.
func unserved(n RunNote, job string, cancelled []NoteCall, unanswered []string) (RunNote, bool) {
	if n.Read != nil || (len(cancelled) == 0 && len(unanswered) == 0) {
		return n, false
	}
	changed := false
	serves := make([]HookServed, 0, len(n.Serves))
	for _, h := range n.Serves {
		switch {
		case h.Launch != job:
		case slices.Contains(unanswered, h.Take):
			changed = true
			continue
		case h.Lost == "" && n.Served == job && h.Call != nil && slices.ContainsFunc(cancelled, h.Call.is):
			h.Lost, changed = ServeCancelled, true
		}
		serves = append(serves, h)
	}
	if !changed {
		return n, false
	}
	n.Serves = nil
	if len(serves) > 0 {
		n.Serves = serves
	}
	return withoutDeadServe(n, job), true
}

// withoutDeadServe is n with [RunNote.Served] cleared where it names job and no
// live serve to job is left ([RunNote.LiveServe]).
func withoutDeadServe(n RunNote, job string) RunNote {
	if _, live := n.LiveServe(); n.Served == job && !live {
		n.Served = ""
	}
	return n
}

// settledServes is n with its serves to the settle's job settled
// ([PendingStore.SettleHook]), whether anything changed, and whether the read
// it recorded took next, the write's own place among the reads, which only a
// note no serve placed can ([RunNote.Place]). A note read, or holding no serve
// to the job, is left as it is.
func settledServes(n RunNote, settle HookSettle, next int) (RunNote, bool, bool) {
	if n.Read != nil || !slices.ContainsFunc(n.Serves, func(h HookServed) bool { return h.Launch == settle.Launch }) {
		return n, false, false
	}
	n, changed := unserved(n, settle.Launch, nil, settle.Unanswered)
	// THE SESSION OUTRANKS A CANCEL, and an absence the same settle names: a
	// note one of whose serves its session shows taken is read, from the
	// EARLIEST such serve, whatever any of them lost. One shown taken only
	// before the session's last restart is read so, marked, where none is
	// shown taken after it.
	for _, restarted := range []bool{false, true} {
		takes := settle.Held
		if restarted {
			takes = settle.Restarted
		}
		i := slices.IndexFunc(n.Serves, func(h HookServed) bool {
			return h.Launch == settle.Launch && slices.Contains(takes, h.Take)
		})
		if i < 0 {
			continue
		}
		h := n.Serves[i]
		read := NoteRead{At: h.At, Order: n.Place, Phase: string(types.PhaseExecute), Iteration: h.Iteration,
			Round: 1, Boundary: h.Boundary, Launch: settle.Launch, Take: h.Take, Restarted: restarted}
		tookPlace := read.Order == 0
		if tookPlace {
			read.Order = next
		}
		n.Read, n.Served = &read, settle.Launch
		return n, true, tookPlace
	}
	n, cancelled := unserved(n, settle.Launch, settle.Cancelled, nil)
	changed = changed || cancelled
	serves, absent := slices.Clone(n.Serves), false
	for i, h := range serves {
		if h.Launch == settle.Launch && (h.Lost == "" || h.Lost == ServeCancelled) &&
			slices.Contains(settle.Absent, h.Take) {
			serves[i].Lost, absent = ServeAbsent, true
		}
	}
	if absent {
		n.Serves, changed = serves, true
		n = withoutDeadServe(n, settle.Launch)
	}
	return n, changed, false
}

// readByServeOf reports whether n is read off one of its serves to job: a read
// a settle confirmed off the job's own session.
func readByServeOf(n RunNote, job string) bool {
	return n.Read != nil && n.Read.Launch == job && n.Read.Take != "" &&
		slices.ContainsFunc(n.Serves, func(h HookServed) bool { return h.Launch == job && h.Take == n.Read.Take })
}

// MarkPulled records that a job's harness has asked for notes. See the
// contract on [PendingStore].
func (s *CoordStore) MarkPulled(ctx context.Context, turnID, launch string) (bool, error) {
	if launch == "" {
		return false, nil
	}
	_, marked, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || run.Steer.Pulled == launch {
			return false
		}
		run.Steer.Pulled = launch
		return true
	})
	return marked, err
}

// WatchNotes follows one run's notes. See the contract on [PendingStore].
func (s *CoordStore) WatchNotes(ctx context.Context, turnID string) (<-chan NotesView, error) {
	updates, err := s.runs.WatchSandboxRun(ctx, turnID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: watch run %s: %w", turnID, err)
	}
	out := make(chan NotesView, 1)
	go func() {
		defer close(out)
		for update := range updates {
			view, err := viewOf(update)
			if err != nil {
				// A STATE THIS BUILD CANNOT READ is passed over, never
				// delivered as an empty record: the watcher keeps what it
				// last knew, and the next state that decodes replaces it.
				log.WarnContext(ctx, "sandbox_notes_view_unread", "turn_id", turnID,
					"error", err.Error())
				continue
			}
			// THE NEWEST STATE WINS: one the reader has not taken yet is
			// replaced rather than queued, so a slow reader is never
			// handed a history it has to walk.
			select {
			case <-out:
			default:
			}
			select {
			case out <- view:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// openNotes is the write a launch makes of the turn's notes on the record it
// opens, under the job it opens ([PendingStore.BeginLaunch]), in this order:
//
//   - every ITEM FOR ONE JOB ([RunNote.For]) is removed first: the job this
//     write opens is new, so every item on the record was the job before it's
//     alone, and nothing in this one will read it. An item for the job this
//     write opens may be filed after the removal, held to [MaxJobItems] as a
//     filing's is ([jobItems]); the steps below are a person's notes';
//   - the brief's notes ([PendingRun.BriefNotes]) are recorded READ by the job:
//     a note the record already holds, still waiting, is recorded read; one it
//     holds read by the take the brief names as lost ([RunNote.LostTake]) is
//     recorded read by the job in that take's place, the brief being the first
//     to tell it, and at that take's own place among the reads, which this
//     write leaves as it was; one it holds read otherwise is left as it was
//     read; and any other — a turn's first segment's own, which the record has
//     never held — is added already read, numbered as if filed now. Every
//     other read this write records takes its place among the record's reads
//     ([RunSteer.Reads]), but a waiting note a hook's serve placed, which is
//     read at its own place ([RunNote.Place]), ahead of every note a person
//     filed after it;
//   - the notes the brief tells again ([PendingRun.RetoldNotes]) are recorded
//     read by the job, told again: each one the record holds read by a take
//     marked unanswered, in that take's place and at that take's own place
//     among the reads, which this write leaves as it was, the take's read kept
//     beside the telling ([NoteRead.Retold]). A note the record holds
//     otherwise is left as it is, and one it does not hold is never added:
//     there is nothing to tell again;
//   - then the notes the first segment's box still held ([PendingRun.HandedNotes])
//     are filed WAITING, numbered after them — so they are on the record from
//     the write that creates it, ahead of every note a person files there once
//     it exists. A note the record already holds is left as it is.
//
// The handed notes are filed WHATEVER THE TURN'S BOUNDS SAY: the box answered
// each one accepted under the same two bounds ([NoteFiling]), so each is a note
// the person was told the turn holds, and a hand-over that refused one would
// turn an accepted note into an expiry. They count against the bounds for every
// filing after them.
func openNotes(steer *RunSteer, run PendingRun, launch string, at time.Time) {
	steer.Notes = slices.DeleteFunc(steer.Notes, func(held RunNote) bool { return held.For != "" })
	order := steer.Reads + 1
	for _, note := range run.BriefNotes {
		if note.ID == "" {
			continue
		}
		read := NoteRead{At: at, Launch: launch}
		if note.Read != nil {
			read = *note.Read
			read.At, read.Launch = at, launch
		}
		read.Order = order
		if i := slices.IndexFunc(steer.Notes, func(held RunNote) bool { return held.ID == note.ID }); i >= 0 {
			switch held := steer.Notes[i]; {
			case held.Waiting() && held.Place > 0:
				// AT THE NOTE'S OWN PLACE, its first serve's: a note a person
				// filed after it, correcting it, stays after it, whatever the
				// harness it was served to happened to see first.
				placed := read
				placed.Order = held.Place
				steer.Notes[i].Read = &placed
			case held.Waiting():
				steer.Notes[i].Read = &read
				steer.Reads = order
			case note.LostTake != "" && held.Read.Take == note.LostTake:
				// THE JOB IN THE LOST TAKE'S PLACE, AT THAT TAKE'S OWN PLACE
				// AMONG THE READS: the record numbered the note's read there,
				// before every read a later take made — a note a person sent
				// after it, correcting it, among them — and every reader
				// orders the turn's notes by that number. Read at this
				// write's place, it came after them, and every later brief
				// told the correction ahead of the note it corrects.
				took := read
				took.Order = held.Read.Order
				steer.Notes[i].Read = &took
			}
			continue
		}
		steer.Seq++
		steer.Notes = append(steer.Notes, RunNote{
			ID: note.ID, Seq: steer.Seq, Sealed: bytes.Clone(note.Sealed), At: note.At, Read: &read,
		})
		steer.Reads = order
	}
	for _, note := range run.RetoldNotes {
		i := slices.IndexFunc(steer.Notes, func(held RunNote) bool { return held.ID == note.ID })
		if note.ID == "" || i < 0 || steer.Notes[i].Read == nil || !steer.Notes[i].Read.Unanswered {
			continue
		}
		// TOLD AGAIN BY THIS JOB'S BRIEF, AT THE MARKED TAKE'S OWN PLACE: who
		// read the note changes, and never where it sits among the reads, as
		// for a lost take's note above. The take's reader never had it, and
		// this brief hands it to the job's harness where the job starts — so
		// THE TAKE'S READ IS KEPT beside the telling: where the job never
		// starts, the brief handed nothing over, and the ending reports the
		// note missed at that take, where the record knew it was missed before
		// this write.
		marked := *steer.Notes[i].Read
		told := NoteRead{Phase: string(types.PhaseExecute), Round: 1,
			Boundary: string(types.SteerBoundaryBrief)}
		if note.Read != nil {
			told = *note.Read
		}
		told.At, told.Launch, told.Order = at, launch, marked.Order
		told.Take, told.Unanswered, told.Retold = "", false, &marked
		steer.Notes[i].Read = &told
	}
	for _, note := range run.HandedNotes {
		if note.ID == "" || slices.ContainsFunc(steer.Notes, func(held RunNote) bool { return held.ID == note.ID }) {
			continue
		}
		steer.Seq++
		filed := RunNote{ID: note.ID, Seq: steer.Seq, Sealed: bytes.Clone(note.Sealed), At: note.At}
		if filed.At.IsZero() {
			filed.At = at
		}
		steer.Notes = append(steer.Notes, filed)
	}
}
