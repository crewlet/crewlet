package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE CHAT READ STATE OF A SEAT THAT LEFT.
//
// Read state is one record per HUMAN SEAT (its next holder inherits it) in a
// bucket with NO AGE ([coord.ChatReads]: an expired cursor marks a finished
// room unread), so this job forgets the records of seats that left. It is the
// mailbox retirement's protocol (mailboxes.go) with its two rules:
//
//   - ABSENCE IS OBSERVED, NOT INFERRED: the first sweep lacking a seat stamps
//     its record ([coord.ChatReads.SetChatReadAbsence]), a sweep finding it
//     back clears the stamp, and it is forgotten once the stamp is older than
//     [ChatReadForgetGrace]. The stamp is in the record: the duty moves.
//   - UNKNOWN IS NEVER ABSENT: an unreadable roster stamps and forgets nothing.

// ChatReadForgetGrace is how long a seat must have been absent from the
// company the fleet is pointed at before its chat read state is forgotten.
//
// [MailboxRetirementGrace], deliberately the same value: both are what a seat
// REMOVED from the chart leaves behind, and the undo window for a removal has
// to be one answer, else a seat restored by accident would get back its
// mailbox but not its read state. Like the mailbox's, the clock starts when a
// sweep FIRST OBSERVES the absence, so the delay is this plus up to one
// [Interval]: a forget is never early, only late.
const ChatReadForgetGrace = MailboxRetirementGrace

// chatReadsJobName is what the sweep log and [Worker.Jobs] call the job.
const chatReadsJobName = "chat_read_state"

// ChatReadRecords is the slice of the coordination store's chat read state
// this job calls. Declared here, by the consumer; [coord.ChatReads]
// satisfies it.
type ChatReadRecords interface {
	ChatReaders(ctx context.Context) ([]coord.ChatReader, error)
	SetChatReadAbsence(ctx context.Context, handle string, since time.Time) (bool, error)
	ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error)
}

// HumanSeatRoster reads the handle of every HUMAN seat in the company the
// fleet is pointed at.
//
// An error means UNKNOWN, never "nobody": it is returned when the pointer
// cannot be read, when this node has not applied the revision it names, and
// [ErrNoActiveRevision] when nothing has ever been activated.
type HumanSeatRoster func(ctx context.Context) ([]string, error)

// ChatReadJobs is the reconcile, for [Options.Jobs]. Its horizon is the
// grace, so the cutoff a tick hands it is the instant an absence must have
// begun by to be forgotten.
//
// [Fleet]: the records are the company's, one copy every node agrees on, so
// one node judging them per tick is the whole job.
//
// A store or a roster that is missing contributes nothing, rather than a job
// that fails every tick: a node with no fleet store has no read state to
// judge.
func ChatReadJobs(records ChatReadRecords, roster HumanSeatRoster) []Job {
	if records == nil || roster == nil {
		return nil
	}
	r := chatReadReconcile{records: records, roster: roster}
	return []Job{{Name: chatReadsJobName, Scope: Fleet, Horizon: Fixed(ChatReadForgetGrace), Run: r.sweep}}
}

type chatReadReconcile struct {
	records ChatReadRecords
	roster  HumanSeatRoster
}

// sweep is the job: clear the stamp of whoever is back, stamp whoever is
// newly missing, and forget whoever has been missing since before the cutoff.
// It reports how many records it forgot. It needs no lease or retirement mark:
// the store checks the stamp in the same compare-and-set that removes the
// record ([coord.ChatReads.ForgetChatRead]). Only a human seat owns a record,
// so one under any other handle is forgotten a grace after first seen.
//
// The roster is read BEFORE the listing, so a record that appeared after the
// roster was taken — a seat added to the chart and read as at once — is judged
// against a chart that is no newer than its arrival and may be stamped absent
// for one tick. That costs nothing: the next sweep finds the seat in the chart
// and clears the stamp a grace before it could matter.
func (r chatReadReconcile) sweep(ctx context.Context, now, cutoff time.Time) (int64, error) {
	roster, err := r.roster(ctx)
	if errors.Is(err, ErrNoActiveRevision) {
		log.DebugContext(ctx, "chat_read_state_not_judged", "reason", err.Error())
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("the human seats of the active revision could not be read, so "+
			"no chat reader is judged absent this tick: %w", err)
	}
	present := make(map[string]bool, len(roster))
	for _, handle := range roster {
		present[handle] = true
	}
	readers, err := r.records.ChatReaders(ctx)
	if err != nil {
		return 0, fmt.Errorf("the chat read state could not be listed: %w", err)
	}

	var forgot int64
	var errs []error
	for _, reader := range readers {
		switch {
		case present[reader.Handle]:
			if reader.AbsentSince.IsZero() {
				continue
			}
			// BACK IN THE COMPANY before the grace ran out: the clock
			// stops, and a later removal starts a new one.
			if _, err := r.records.SetChatReadAbsence(ctx, reader.Handle, time.Time{}); err != nil {
				errs = append(errs, fmt.Errorf("clear the absence of %q: %w", reader.Handle, err))
				continue
			}
			log.InfoContext(ctx, "chat_read_state_returned", "handle", reader.Handle,
				"absent_since", reader.AbsentSince)
		case reader.AbsentSince.IsZero():
			// THE TICK'S OWN INSTANT, not a later reading: the roster
			// that showed the seat missing was read as the tick began,
			// and an absence is never observed later than the read that
			// saw it.
			exists, err := r.records.SetChatReadAbsence(ctx, reader.Handle, now)
			if err != nil {
				errs = append(errs, fmt.Errorf("record the absence of %q: %w", reader.Handle, err))
				continue
			}
			if exists {
				log.InfoContext(ctx, "chat_read_state_absent", "handle", reader.Handle,
					"forget_after", now.Add(now.Sub(cutoff)),
					"detail", "this human seat is not in the company the fleet is running; its "+
						"read cursors, mutes and do-not-disturb are kept until the grace period "+
						"ends, and a seat restored under the same handle before then finds every "+
						"room as read")
			}
		case reader.AbsentSince.After(cutoff):
			// Missing, and inside the grace.
		default:
			gone, err := r.records.ForgetChatRead(ctx, reader.Handle, cutoff)
			if err != nil {
				errs = append(errs, fmt.Errorf("forget the chat read state of %q: %w", reader.Handle, err))
				continue
			}
			if !gone {
				// Found back by a peer's sweep, or forgotten by one,
				// since this sweep listed it. Either is that sweep's.
				continue
			}
			forgot++
			log.InfoContext(ctx, "chat_read_state_forgotten", "handle", reader.Handle,
				"absent_since", reader.AbsentSince,
				"detail", "this human seat has been absent from the company the fleet is running "+
					"for longer than the grace period; its read cursors, mutes and do-not-disturb "+
					"are forgotten, and a seat added again under the same handle starts with "+
					"every room unread")
		}
	}
	return forgot, errors.Join(errs...)
}
