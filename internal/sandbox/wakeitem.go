package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A wake handed to a running run: somebody's message about the work its job is
// doing, filed by the engine alone ([PendingStore.FileWake]) as an item of that
// job ([RunNote.Wake]) and read between the job's steps as a note is. Every
// reader sorts on [RunNote.For] before it opens a seal, so none takes a wake
// for a person's note. See the package doc, "A wake joins a running run".

// WakeItem is the wakes a wake item hands its job ([RunNote.Wake]), oldest
// first: every wake filed while the item waited served to nobody
// ([PendingStore.FileWake]), so several between two steps reach the run as one
// item. Each is sealed by the engine; the item holds in the clear only what the
// store files by.
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), as a late call is.
type WakeItem struct {
	Wakes []SealedWake `json:"wakes"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// SealedWake is one wake of an item ([WakeItem.Wakes]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), as the item is.
type SealedWake struct {
	// ID is the wake's identity as its filer named it, so a retry of one wake
	// is one wake: in the clear, because the store dedupes on it, and at most
	// [MaxWakeIDBytes] of the id charset ([validWakeID]).
	ID string `json:"id"`

	// Sealed is the wake, sealed by the engine under the fleet keyring in a
	// domain of its own and bound to the turn, the job and ID: who wrote it,
	// where, whether it owes an answer, its words and its deliveries'
	// completion-ledger keys. SIGNED, NOT ENCRYPTED, as a note is
	// ([RunNote.Sealed]), so no store writer can say a wake is anybody's.
	Sealed []byte `json:"sealed"`

	// At is when the store filed it, on the clock of the node that did.
	At time.Time `json:"at"`

	// From is the turn id of the run that sent it with `message_run`, "" for a
	// wake from the seat's inbox: IN THE CLEAR, so the store holds the job to
	// [MaxJobRunMessages] and a reader opens it in the run messages' own seal
	// domain, whose body names the same run, so no writer of the store can
	// say a wake is a run's message nor a run's message a wake (ADR-0176).
	From string `json:"from,omitempty"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// IsWake reports whether n is a wake item ([RunNote.Wake]).
func (n RunNote) IsWake() bool { return n.Wake != nil }

// MaxServes is how often hooks may serve n to one job ([RunNote.Serves]): a
// wake item once ([MaxWakeServes]), a note or a late call's item
// [MaxNoteServes] times.
func (n RunNote) MaxServes() int {
	if n.IsWake() {
		return MaxWakeServes
	}
	return MaxNoteServes
}

// WakeEnd is what became of wake item n as its record knows it: delivered,
// where a step read it; unconfirmed, where it was served to its job's harness
// and no read of the job's session showed it taken, a serve its session may
// hold ([HookServed.MayHold]); unread, where nothing served it. "" for a note
// or a late call's item.
func (n RunNote) WakeEnd() types.JoinOutcome {
	switch {
	case !n.IsWake():
		return ""
	case n.Read != nil && !n.Read.Unanswered:
		return types.JoinDelivered
	case n.Served != "" || slices.ContainsFunc(n.Serves, HookServed.MayHold):
		return types.JoinUnconfirmed
	}
	return types.JoinUnread
}

// wakeItemID is the id of the wake item first filed for job with the wake
// first: one per job and first wake, never a person's request id (a UUID).
func wakeItemID(job, first string) string { return "wake:" + job + ":" + first }

// validWakeID reports whether id is a wake's id a filing may name: 1 to
// [MaxWakeIDBytes] bytes of the id charset, so it is written as itself and the
// row's worst case holds it (TestTheWholeRowFitsItsBudget).
func validWakeID(id string) bool {
	return id != "" && len(id) <= MaxWakeIDBytes && !strings.ContainsFunc(id, outsideIDCharset)
}

// WakeFiling is one wake to file for a run's job ([PendingStore.FileWake]).
type WakeFiling struct {
	// Launch is the job the wake is sealed for, which the record must name.
	Launch string

	// Wake is the wake as the engine sealed it; the store stamps its At.
	Wake SealedWake
}

// ErrWakeClosed is a wake a run's record takes no more of for the job it names:
// the record is gone, its ending is decided, or it names another job or a
// status whose harness takes no notes ([ItemStatuses]). The wake is left to a
// turn of its own.
var ErrWakeClosed = errors.New("sandbox: the run takes no wake for that job now")

// ErrWakesSpent is a wake for a job holding [MaxJobWakes] wake items, or a turn
// holding [MaxTurnWakes], that it cannot join. The wake is left to a turn of
// its own.
var ErrWakesSpent = errors.New("sandbox: the run holds every wake item it may")

// ErrWakeTooLarge is a wake sealed past [MaxWakeItemBytes], refused before
// anything is written.
var ErrWakeTooLarge = errors.New("sandbox: a wake is sealed past what one wake item holds")

// FileWake files a wake for a run's job. See the contract on [PendingStore].
//
// ONE LIVE WRITE, pinned to the job and [ItemStatuses]: the wake joins the
// job's newest wake item while that waits served to nobody ([joinable]), else
// it opens one, within [MaxJobWakes] for the job and [MaxTurnWakes] for the
// turn. NO FENCE, as a note's filing takes none ([CoordStore.FileNote]).
func (s *CoordStore) FileWake(ctx context.Context, turnID string, filing WakeFiling) (RunNote, error) {
	wake := filing.Wake
	switch {
	case filing.Launch == "" || !validWakeID(wake.ID) || len(wake.Sealed) == 0:
		return RunNote{}, fmt.Errorf("sandbox: a wake for run %s names its job, its seal, and an id of 1 to %d "+
			"bytes of the id charset", turnID, MaxWakeIDBytes)
	case wake.From != "" && !validWakeID(wake.From):
		return RunNote{}, fmt.Errorf("sandbox: wake %s for run %s names a sending run outside the id charset",
			wake.ID, turnID)
	case len(wake.Sealed) > MaxWakeItemBytes:
		return RunNote{}, fmt.Errorf("sandbox: wake %s for run %s is sealed in %d bytes, past the %d a wake item "+
			"holds: %w", wake.ID, turnID, len(wake.Sealed), MaxWakeItemBytes, ErrWakeTooLarge)
	}
	wake.At = s.clock()
	var (
		item                  RunNote
		closed, spent, others bool
	)
	_, won, found, err := s.writeFound(ctx, turnID, true, Fence{}, func(run *PendingRun) bool {
		item, closed, spent, others = RunNote{}, false, false, false
		if run.LaunchID != filing.Launch || !slices.Contains(ItemStatuses, run.Status) {
			closed = true
			return false
		}
		if wake.From != "" && runMessagesIn(run.Steer.Notes, filing.Launch) >= MaxJobRunMessages &&
			!slices.ContainsFunc(run.Steer.Notes, func(n RunNote) bool { return n.holdsWake(wake.ID) }) {
			others = true
			return false
		}
		notes := slices.Clone(run.Steer.Notes)
		newest, job, turn := -1, 0, 0
		for i, n := range notes {
			if !n.IsWake() {
				continue
			}
			if slices.ContainsFunc(n.Wake.Wakes, func(w SealedWake) bool { return w.ID == wake.ID }) {
				item = n
				return false
			}
			turn++
			if n.For == filing.Launch {
				newest, job = i, job+1
			}
		}
		if newest >= 0 && joinable(notes[newest], wake) {
			joined := notes[newest]
			section := *joined.Wake
			section.Wakes = append(slices.Clone(section.Wakes), wake)
			joined.Wake = &section
			notes[newest], item = joined, joined
			run.Steer.Notes = notes
			return true
		}
		if job >= MaxJobWakes || turn >= MaxTurnWakes {
			spent = true
			return false
		}
		run.Steer.Seq++
		item = RunNote{ID: wakeItemID(filing.Launch, wake.ID), Seq: run.Steer.Seq, At: wake.At, For: filing.Launch,
			Wake: &WakeItem{Wakes: []SealedWake{wake}}}
		notes = append(notes, item)
		run.Steer.Notes = notes
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return RunNote{}, fmt.Errorf("sandbox: wake %s for run %s: %w", wake.ID, turnID, ErrWakeClosed)
	case err != nil:
		return RunNote{}, err
	case won:
		return item, nil
	case !found || closed:
		// NOTHING WAS DECIDED ON where the last attempt found no record,
		// whatever an earlier one decided before it lost.
		return RunNote{}, fmt.Errorf("sandbox: wake %s for run %s's job %s: %w", wake.ID, turnID,
			filing.Launch, ErrWakeClosed)
	case spent:
		return RunNote{}, fmt.Errorf("sandbox: wake %s for run %s's job %s: %w", wake.ID, turnID,
			filing.Launch, ErrWakesSpent)
	case others:
		return RunNote{}, fmt.Errorf("sandbox: message %s for run %s's job %s, which holds %d from other runs: %w",
			wake.ID, turnID, filing.Launch, MaxJobRunMessages, ErrRunMessagesSpent)
	}
	return item, nil
}

// holdsWake reports whether n is a wake item holding the wake named id.
func (n RunNote) holdsWake(id string) bool {
	return n.IsWake() && slices.ContainsFunc(n.Wake.Wakes, func(w SealedWake) bool { return w.ID == id })
}

// joinable reports whether wake may join item, its job's newest wake item: one
// waiting that nothing served, no hook and no poll, with room under
// [MaxItemWakes] and [MaxWakeItemBytes], neither of them another run's message. A wake joining an item once served
// would reach no harness: a hook serves an item once, and a poll's harness has
// admitted it as it was.
func joinable(item RunNote, wake SealedWake) bool {
	if !item.Waiting() || item.Served != "" || len(item.Serves) > 0 || len(item.Wake.Wakes) >= MaxItemWakes {
		return false
	}
	// ANOTHER RUN'S MESSAGE IS AN ITEM OF ITS OWN, so it is handed over whole,
	// never left out of an item's answer for length (ADR-0176).
	if wake.From != "" || slices.ContainsFunc(item.Wake.Wakes, func(w SealedWake) bool { return w.From != "" }) {
		return false
	}
	size := len(wake.Sealed)
	for _, w := range item.Wake.Wakes {
		size += len(w.Sealed)
	}
	return size <= MaxWakeItemBytes
}

// handsWhole reports whether a serve handing over wakes, how many of each wake
// item by id, hands over every wake n holds: a note or a late call's item it
// always does. A wake that joined the item after its server read it would
// otherwise be served and claimed unseen, so the item waits for a serve that
// read it whole ([NoteServe.Wakes], [HookServe.Wakes]).
func handsWhole(n RunNote, wakes map[string]int) bool {
	return !n.IsWake() || wakes[n.ID] == len(n.Wake.Wakes)
}

// unreadWakes is every wake item of notes no step read: what the write that
// opens the next job removes ([openNotes]), whose ending reports them
// ([PriorBox.Wakes]).
func unreadWakes(notes []RunNote) []RunNote {
	var out []RunNote
	for _, n := range notes {
		if n.IsWake() && n.Read == nil {
			out = append(out, n)
		}
	}
	return out
}
