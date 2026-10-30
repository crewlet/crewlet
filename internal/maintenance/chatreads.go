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
// How far each person has read in each room, what they have muted and whether
// they are in do-not-disturb is one record per HUMAN SEAT — kept under the seat
// the person acts as, so its next holder inherits it — in a coordination bucket
// with NO AGE ([coord.ChatReads] says why: an expired cursor marks a room its
// reader had finished as unread). So a record leaves only when somebody
// decides it should, and until this job nothing did: every seat anybody ever
// read a room as kept up to [coord.MaxReadCursors] cursors for the life of the
// deployment, after leaving the company as much as before.
//
// The reconcile is the mailbox retirement's protocol at the size of what it
// protects (mailboxes.go), and keeps the two rules that make that one safe:
//
//   - ABSENCE IS OBSERVED, NOT INFERRED. The first sweep whose roster lacks a
//     record's seat stamps the record ([coord.ChatReads.SetChatReadAbsence]),
//     a sweep that finds the seat back clears the stamp, and a record is
//     forgotten only once the stamp is older than [ChatReadForgetGrace]. The
//     stamp is in the record rather than in this job, because the duty moves
//     and a grace remembered in memory would restart at every handoff.
//   - UNKNOWN IS NEVER ABSENT. The roster is the human seats of the company
//     the fleet is pointed at, and a roster that cannot be read — the pointer
//     unreachable, this node not yet on that revision — stamps nothing and
//     forgets nothing. A store blip or a lagging duty holder must not be able
//     to cost a seat its place in every room.
//
// What it does NOT need from the mailbox protocol is the lease and the
// retirement mark: a forget deletes one record and nothing outside it, and
// the store checks the stamp against the bound in the same compare-and-set
// that removes it ([coord.ChatReads.ForgetChatRead]), so a seat a peer sweep
// has just found back keeps its cursors whatever this sweep listed.
//
// # The roster is the HUMAN seats, and a seat is what leaves
//
// A record is kept under the seat its person acts as, so it is the SEAT that
// has to be gone, never its holder: a person removed or moved off a seat that
// the company still has leaves the record to the seat's next holder, exactly
// as the seat's tracker inbox stays. And only a human seat can own one — every
// binding a person or a machine acts through names a human seat, and a turn's
// agent seat is never handed the flush — so a record under any other handle
// is one nobody can reach, and the roster leaving it out forgets it a grace
// after it was first seen, rather than keeping it for as long as an agent seat
// of that name exists. The forgotten record's purge marker is removed in its
// turn by [MarkerJobs].

// ChatReadForgetGrace is how long a seat must have been absent from the
// company the fleet is pointed at before its chat read state is forgotten.
//
// [MailboxRetirementGrace], deliberately the same value: both are what a seat
// REMOVED from the chart leaves behind, and the undo window for a removal has
// to be one answer. A seat deleted by accident and restored within a working
// day comes back to its mailbox AND to every room exactly as read; a window
// that differed would restore one and not the other, which is a stranger
// outcome than losing both. The cost of the day is a record per removed seat
// kept a day longer, which is nothing against a bucket that kept them for
// ever. Like the mailbox's, the clock starts when a sweep FIRST OBSERVES
// the absence, so the effective delay is this plus up to one [Interval]: a
// forget is never early, only late.
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
// It reports how many records it forgot.
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
