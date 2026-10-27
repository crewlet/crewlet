package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A person's notes to a turn whose coding run is out, on the run's own record.
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
// recorded by the write that hands the note over. What a note SAYS it never
// reads: a note is sealed by the engine under the fleet keyring before it is
// filed ([RunNote.Sealed]), and opened by the engine before anything of it
// reaches a model (internal/engine, runsteer.go, and ADR-0059). The store keeps
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

	// Pulled is the job whose coding CLI has asked for notes at least once
	// — what tells a run whose CLI never read its notes from one that had
	// none to read.
	Pulled string `json:"pulled,omitempty"`

	// Reads is the place of the last write that recorded a note read, from
	// 1: every such write — a launch's for its brief, a take — numbers itself
	// one past it, in its own compare-and-swap, and stamps the number on
	// every read it records ([NoteRead.Order]). The order the turn read its
	// notes in is that number, and no clock's: the writes are made by
	// whichever nodes launch, serve the run's bridge and resume the turn,
	// each stamping its own time.
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

	// Extra is every key of the note this build does not know — see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// Waiting reports whether nothing has read the note yet.
func (n RunNote) Waiting() bool { return n.Read == nil }

// UnreadNotes is every note on the run's record that no job ran with: the ones
// still WAITING, and the ones the BRIEF OF THE RECORD'S CURRENT JOB recorded
// read where that job never started — its command was never attached
// ([PendingRun.CommandID]), because the launch failed, or was ended, between
// the write that opened the job and the job's start. What the run's ending
// reports expired ([CoordinatorOptions.SteersExpired]).
//
// THE RECORD IS THE ONE WITNESS. A launch records its brief's notes read in the
// write that opens its job ([PendingStore.BeginLaunch]), before it knows whether
// the job will run, and every way it can then fail ends the record
// ([Coordinator.abandon]) — or meets an ending another party decided, which
// finishes in its place. So the ending is the one party that sees both what the
// brief took and that no job ran with it, and it reports those notes under the
// same ids as the waiting ones: a launch that reported them itself could not
// tell a note its own brief took from one another node's ending had already
// reported, and published one twice.
func (r PendingRun) UnreadNotes() []RunNote {
	var out []RunNote
	for _, n := range r.Steer.Notes {
		if n.Waiting() || r.briefNeverRan(n) {
			out = append(out, n)
		}
	}
	return out
}

// briefNeverRan reports whether n was read by the brief of the record's job
// while that job has not started.
func (r PendingRun) briefNeverRan(n RunNote) bool {
	return n.Read != nil && r.CommandID == "" && r.LaunchID != "" &&
		n.Read.Launch == r.LaunchID && n.Read.Boundary == string(types.SteerBoundaryBrief)
}

// unreadNotes reports whether the run's record holds a note no job ran with.
func (r PendingRun) unreadNotes() bool { return len(r.UnreadNotes()) > 0 }

// NoteRead is where a note was read: the phase, iteration and round on the
// phase's own scale, the kind of step (the agent layer's vocabulary, carried as
// text: the record is this package's wire format and imports no agent
// package), and the job whose CLI read it — empty for a read the engine's own
// loop made. At is when, on the clock of the node whose write recorded it, for
// a person to read; Order is the place of that write among the record's reads
// ([RunSteer.Reads]), which is what orders them — a note's Seq orders the reads
// one write recorded.
type NoteRead struct {
	At        time.Time `json:"at"`
	Order     int       `json:"order,omitempty"`
	Phase     string    `json:"phase"`
	Iteration int       `json:"iteration,omitempty"`
	Round     int       `json:"round,omitempty"`
	Boundary  string    `json:"boundary"`
	Launch    string    `json:"launch,omitempty"`

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
	NoteSpent NoteFiled = "spent"
	// NoteClosed — the run's ending is decided, or the run has no record:
	// nothing will read the note.
	NoteClosed NoteFiled = "closed"
)

// NoteFiling is one note to file on a run's record, and the turn's bounds.
type NoteFiling struct {
	Note RunNote

	// MaxWaiting is how many notes may wait unread at once, and MaxNotes how
	// many the turn takes in all — internal/agent/steer's MaxPendingNotes and
	// MaxTurnNotes, which the caller passes so the two numbers have one home.
	// Both required.
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
	// it could not open is one it must not record read.
	IDs []string

	Launch   string
	Statuses []string
	Fence    Fence

	// Read is where the notes are read; the store stamps its At.
	Read NoteRead
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
	}
	if note.At.IsZero() {
		note.At = s.clock()
	}
	note.Read = nil
	var (
		filed NoteFiled
		read  PendingRun
	)
	run, won, found, err := s.writeFound(ctx, turnID, true, func(run *PendingRun) bool {
		read = *run
		notes := run.Steer.Notes
		waiting := 0
		for _, held := range notes {
			if held.ID == note.ID {
				filed = NoteDuplicate
				return false
			}
			if held.Waiting() {
				waiting++
			}
		}
		// SPENT BEFORE FULL, as a turn's box answers: a turn that has taken
		// all it takes will never take this note.
		switch {
		case len(notes) >= filing.MaxNotes:
			filed = NoteSpent
			return false
		case waiting >= filing.MaxWaiting:
			filed = NoteFull
			return false
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
		read.At, read.Order = s.clock(), run.Steer.Reads+1
		notes := slices.Clone(run.Steer.Notes)
		for i, held := range notes {
			if !held.Waiting() || !slices.Contains(take.IDs, held.ID) {
				continue
			}
			marked := read
			held.Read = &marked
			notes[i] = held
			taken = append(taken, held)
		}
		if len(taken) == 0 {
			return false
		}
		run.Steer.Notes = notes
		run.Steer.Reads = read.Order
		return true
	})
	if err != nil || !won {
		// NOT TAKEN unless the write landed: what a decision chose before it
		// lost a race, to a record that then went, was never recorded read.
		return nil, err
	}
	return taken, nil
}

// MarkPulled records that a job's coding CLI has asked for notes. See the
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
//   - the brief's notes ([PendingRun.BriefNotes]) are recorded READ by the job:
//     a note the record already holds, still waiting, is recorded read; one it
//     holds already read is left as it was read; and any other — a turn's first
//     segment's own, which the record has never held — is added already read,
//     numbered as if filed now. Every read this write records takes its place
//     among the record's reads ([RunSteer.Reads]);
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
			if steer.Notes[i].Waiting() {
				steer.Notes[i].Read = &read
				steer.Reads = order
			}
			continue
		}
		steer.Seq++
		steer.Notes = append(steer.Notes, RunNote{
			ID: note.ID, Seq: steer.Seq, Sealed: bytes.Clone(note.Sealed), At: note.At, Read: &read,
		})
		steer.Reads = order
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
