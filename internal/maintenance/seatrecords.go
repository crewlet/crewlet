package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A PERSONAL RECORD OF A SEAT THAT LEFT.
//
// A person's chat read state and Home board are each one record per HUMAN SEAT
// (its next holder inherits it) that no clock ages, so one reconcile forgets
// the records of seats that left, a job per kind. It is the mailbox
// retirement's protocol (mailboxes.go) with its two rules:
//
//   - ABSENCE IS OBSERVED, NOT INFERRED: the first sweep lacking a seat stamps
//     its record, a sweep finding it back clears the stamp, and it is forgotten
//     once the stamp is older than the kind's grace. The stamp is in the
//     record: the duty moves.
//   - UNKNOWN IS NEVER ABSENT: an unreadable roster stamps and forgets nothing.

// HumanSeatRoster reads the handle of every HUMAN seat in the company the
// fleet is pointed at.
//
// An error means UNKNOWN, never "nobody": it is returned when the pointer
// cannot be read, when this node has not applied the revision it names, and
// [ErrNoActiveRevision] when nothing has ever been activated.
type HumanSeatRoster func(ctx context.Context) ([]string, error)

// seatRecords is one kind of personal record as the reconcile judges it: who
// holds one, and the two writes a judgement makes. Each kind adapts its own
// slice of the coordination store to it.
type seatRecords interface {
	holders(ctx context.Context) ([]seatHolder, error)
	setAbsence(ctx context.Context, handle string, since time.Time) (bool, error)
	forget(ctx context.Context, handle string, absentBy time.Time) (bool, error)
}

// seatHolder is one seat holding a record, and when a sweep first found it
// missing from the company (zero while the company has it).
type seatHolder struct {
	handle      string
	absentSince time.Time
}

// seatRecordKind is how one kind's job is named and said: job names the job
// and prefixes its log events, record is what an error names, and kept and
// lost are what an absent seat keeps through the grace and loses after it.
type seatRecordKind struct {
	job, record, kept, lost string
}

// seatRecordJobs is one kind's reconcile, for [Options.Jobs]: a [Fleet] job,
// since the records are the company's, at the grace as its horizon, so the
// cutoff a tick hands it is the instant an absence must have begun by. A
// missing roster contributes nothing, as each kind's missing store does.
func seatRecordJobs(kind seatRecordKind, records seatRecords, roster HumanSeatRoster,
	grace time.Duration) []Job {

	if roster == nil {
		return nil
	}
	r := seatRecordReconcile{kind: kind, records: records, roster: roster}
	return []Job{{Name: kind.job, Scope: Fleet, Horizon: Fixed(grace), Run: r.sweep}}
}

type seatRecordReconcile struct {
	kind    seatRecordKind
	records seatRecords
	roster  HumanSeatRoster
}

// sweep is the job: clear the stamp of whoever is back, stamp whoever is
// newly missing, and forget whoever has been missing since before the cutoff.
// It reports how many records it forgot. It needs no lease or retirement mark:
// the store checks the stamp in the same compare-and-set that removes the
// record. Only a human seat owns a record, so one under any other handle is
// forgotten a grace after first seen.
//
// The roster is read BEFORE the listing, so a record that appeared after the
// roster was taken (a seat added to the chart and used at once) may be stamped
// absent for one tick; the next sweep clears it a grace before it could matter.
func (r seatRecordReconcile) sweep(ctx context.Context, now, cutoff time.Time) (int64, error) {
	roster, err := r.roster(ctx)
	if errors.Is(err, ErrNoActiveRevision) {
		log.DebugContext(ctx, r.kind.job+"_not_judged", "reason", err.Error())
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("the human seats of the active revision could not be read, so "+
			"no %s is judged absent this tick: %w", r.kind.record, err)
	}
	present := make(map[string]bool, len(roster))
	for _, handle := range roster {
		present[handle] = true
	}
	holders, err := r.records.holders(ctx)
	if err != nil {
		return 0, fmt.Errorf("the %s records could not be listed: %w", r.kind.record, err)
	}

	var forgot int64
	var errs []error
	for _, holder := range holders {
		switch {
		case present[holder.handle]:
			if holder.absentSince.IsZero() {
				continue
			}
			// BACK IN THE COMPANY before the grace ran out: the clock
			// stops, and a later removal starts a new one.
			if _, err := r.records.setAbsence(ctx, holder.handle, time.Time{}); err != nil {
				errs = append(errs, fmt.Errorf("clear the absence of %q: %w", holder.handle, err))
				continue
			}
			log.InfoContext(ctx, r.kind.job+"_returned", "handle", holder.handle,
				"absent_since", holder.absentSince)
		case holder.absentSince.IsZero():
			// THE TICK'S OWN INSTANT, not a later reading: the roster that
			// showed the seat missing was read as the tick began.
			exists, err := r.records.setAbsence(ctx, holder.handle, now)
			if err != nil {
				errs = append(errs, fmt.Errorf("record the absence of %q: %w", holder.handle, err))
				continue
			}
			if exists {
				log.InfoContext(ctx, r.kind.job+"_absent", "handle", holder.handle,
					"forget_after", now.Add(now.Sub(cutoff)),
					"detail", "this human seat is not in the company the fleet is running; its "+
						r.kind.kept+" are kept until the grace period ends, and a seat restored "+
						"under the same handle before then gets them back")
			}
		case holder.absentSince.After(cutoff):
			// Missing, and inside the grace.
		default:
			gone, err := r.records.forget(ctx, holder.handle, cutoff)
			if err != nil {
				errs = append(errs, fmt.Errorf("forget the %s of %q: %w", r.kind.record, holder.handle, err))
				continue
			}
			if !gone {
				// Found back by a peer's sweep, or forgotten by one, since
				// this sweep listed it. Either is that sweep's.
				continue
			}
			forgot++
			log.InfoContext(ctx, r.kind.job+"_forgotten", "handle", holder.handle,
				"absent_since", holder.absentSince,
				"detail", "this human seat has been absent from the company the fleet is running "+
					"for longer than the grace period; its "+r.kind.kept+" are forgotten, and a "+
					"seat added again under the same handle "+r.kind.lost)
		}
	}
	return forgot, errors.Join(errs...)
}
