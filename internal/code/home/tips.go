package home

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// tip is one protected target's TIP WORK (ADR-0068; internal/code's
// tipwork.go), decided from the one snapshot t was read in — the landings
// above the target's green, oldest first, each with the latest attempt of
// each required check on its commit — in this order:
//
//   - a target whose gate requires no check has its green row moved onto its
//     newest landing as it stands;
//   - a required check the newest landing holds no attempt of is DISPATCHED
//     there (purpose tip), under the checks file the window's BASE holds —
//     coalesced: only the newest landing is ever dispatched, so a burst of
//     landings costs one round of checks, not one a landing;
//   - an attempt there its infrastructure ended is queued again below
//     code.AutoRequeueAttempts;
//   - every required check passed (or skipped) there: the green row moved onto
//     it;
//   - one failed there: the failed checks BISECTED over the landing rows —
//     beside a check the base's file cannot define, which errored on the tip
//     and stays errored until the green moves: the file at the base is a
//     commit's and never changes, so waiting on that check would leave the
//     failed ones' first red landing unnamed for as long as the gate asks for
//     it.
//
// Anything else — an attempt still queued or running, one ended in a way
// nobody but a person's rerun moves — waits for the record that ends it, which
// nudges this work again.
//
// Every write is a record decided where it lands: a check's by the
// repository's checks writer, a green or a broken by its committer
// (code.queue), each refusing what its own snapshot does not prove, so a step
// decided from a snapshot the log has moved past is refused and read again
// next turn rather than recorded wrong.
func (k *checks) tip(ctx context.Context, repoID string, t code.TipDue, memo *defsMemo, at statelog.Position) error {
	newest := t.Newest()
	about := fmt.Sprintf("target %s at %s", t.TargetRef, short(newest.OID))
	if len(t.Required) == 0 {
		return k.green(ctx, repoID, t, newest, at)
	}
	states := tipStates(newest, t.Required)
	if len(states.missing) > 0 || len(states.invalid) > 0 || len(states.requeue) > 0 {
		defs, err := k.baseDefs(ctx, repoID, t, memo, at)
		if err != nil {
			return err
		}
		// WHAT IT QUEUED IS WHAT THE TIP WAITS ON; an attempt the base's
		// file could not define, which nothing queues again, does not hold
		// back the measurements beside it.
		if queued, err := k.probe(ctx, repoID, t, newest, code.PurposeTip, states, defs, at); err != nil ||
			queued > 0 {
			return err
		}
	}
	switch {
	case len(states.pending) > 0:
		return nil
	case len(states.failed) > 0:
		return k.bisect(ctx, repoID, t, states.failed, memo, at)
	case states.passed():
		return k.green(ctx, repoID, t, newest, at)
	}
	k.log.Debug("code_tip_waits_on_a_person", "repo", repoID, "about", about, "invalid", states.invalid,
		"checks", states.stuck)
	return nil
}

// green asks the repository's committer to move t's green row onto l, the
// newest landing above it and so the tip of its record.
func (k *checks) green(ctx context.Context, repoID string, t code.TipDue, l code.TipLanding,
	at statelog.Position) error {

	return k.queueRecord(ctx, fmt.Sprintf("target %s at %s", t.TargetRef, short(l.OID)), code.QueueProposal{
		OpID: statelog.NewOpID(k.now(), ""), Seen: at,
		Green: &code.Green{RepoID: repoID, TargetRef: t.TargetRef, OID: l.OID, Through: l.Position}},
		"move its green row")
}

// bisect finds the first landing above t's green on which the checks named
// failed — the tip's failed checks, so a second break behind one already
// named is found by its own checks — reading each landing's own attempts from
// the rows, never git's history:
//
//   - red is the first landing one of failed FAILED on (the tip, at the
//     latest), good the last landing before it every one of failed passed
//     on — the green itself, before the window, where none did;
//   - adjacent, red is the culprit, recorded broken (once: a landing a
//     bisect already named is left as it is);
//   - otherwise the landing midway between them is PROBED (purpose bisect):
//     each of failed it holds no attempt of queued there, under the base's
//     checks file, and an attempt its infrastructure ended queued again.
//
// A probe still running, or one ended in a way only a person's rerun moves,
// waits; so does a culprit the window cannot name — the window's last landing
// good and the tip, read past a gap, red — which a green tip beyond it clears.
func (k *checks) bisect(ctx context.Context, repoID string, t code.TipDue, failed []string, memo *defsMemo,
	at statelog.Position) error {

	red := slices.IndexFunc(t.Landings, func(l code.TipLanding) bool {
		return slices.ContainsFunc(failed, func(name string) bool {
			c, ok := l.Check(name)
			return ok && c.State == code.CheckStateFailed
		})
	})
	good := -1
	for i := red - 1; i >= 0; i-- {
		if passedAll(t.Landings[i], failed) {
			good = i
			break
		}
	}
	culprit := t.Landings[red]
	if red == good+1 {
		switch {
		case culprit.Broken:
			return nil
		case t.More && red == len(t.Landings)-1:
			// THE GAP: the tip is read past the window, so the landing
			// before it in the list is not the one before it on the branch.
			k.log.Warn("code_tip_culprit_past_the_window", "repo", repoID, "target", t.TargetRef,
				"tip", culprit.OID, "landings", code.MaxTipLandings,
				"detail", "more landings lie above the green than one bisect reads; a green tip past them "+
					"clears them, or a person reverts to the last green")
			return nil
		}
		var names []string
		for _, name := range failed {
			if c, ok := culprit.Check(name); ok && c.State == code.CheckStateFailed {
				names = append(names, name)
			}
		}
		return k.queueRecord(ctx, fmt.Sprintf("target %s at %s", t.TargetRef, short(culprit.OID)),
			code.QueueProposal{OpID: statelog.NewOpID(k.now(), ""), Seen: at,
				Broken: &code.Broken{RepoID: repoID, TargetRef: t.TargetRef, Position: culprit.Position,
					Ordinal: culprit.Ordinal, OID: culprit.OID, Checks: names}},
			"record its first red landing broken")
	}
	// MIDWAY, strictly between good and red: never the tip, which red is at
	// the latest, so never across the window's gap.
	mid := t.Landings[(good+red+1)/2]
	states := tipStates(mid, failed)
	if len(states.missing) == 0 && len(states.invalid) == 0 && len(states.requeue) == 0 {
		return nil
	}
	defs, err := k.baseDefs(ctx, repoID, t, memo, at)
	if err != nil {
		return err
	}
	_, err = k.probe(ctx, repoID, t, mid, code.PurposeBisect, states, defs, at)
	return err
}

// baseDefs is the checks file t's tip checks are defined by: the one t's base
// holds — and, where the first landing above the green CREATED the branch,
// none that runs, since the branch held no file before a landing could write
// one, and a landing's own file never defines its measurement.
func (k *checks) baseDefs(ctx context.Context, repoID string, t code.TipDue, memo *defsMemo,
	at statelog.Position) (*checksDefs, error) {

	if t.Base() == code.ZeroOID {
		return &checksDefs{invalid: summarize(fmt.Sprintf("%s was created by its first landing above its "+
			"green, so no checks file defined its checks before a landing could; a green landing through its "+
			"queue, or a person changing its gate, moves it on", t.TargetRef))}, nil
	}
	defs, err := memo.at(ctx, k.estate, repoID, t.Base(), at)
	if err != nil {
		return nil, fmt.Errorf("home: read the checks file of %s's base %s: %w", t.TargetRef, short(t.Base()), err)
	}
	return defs, nil
}

// passedAll reports whether every one of names passed — or was skipped, its
// paths untouched — on l by its latest attempt.
func passedAll(l code.TipLanding, names []string) bool {
	for _, name := range names {
		c, ok := l.Check(name)
		if !ok || (c.State != code.CheckStatePassed && c.State != code.CheckStateSkipped) {
			return false
		}
	}
	return true
}

// landingStates is where each of a set of checks stands on one landing by its
// latest attempt: none (missing), errored at a dispatch for a checks file
// that did not run (invalid), still to run (pending), ended by its
// infrastructure below code.AutoRequeueAttempts (requeue), ended otherwise
// without a measurement (stuck), failed — and, beside them, passed.
type landingStates struct {
	missing, invalid, pending, requeue, stuck, failed []string
	latest                                            map[string]int
}

// passed reports every check of the set passed (or skipped) on the landing.
func (s landingStates) passed() bool {
	return len(s.missing)+len(s.invalid)+len(s.pending)+len(s.requeue)+len(s.stuck)+len(s.failed) == 0
}

// tipStates is where each of names stands on l.
func tipStates(l code.TipLanding, names []string) landingStates {
	s := landingStates{latest: map[string]int{}}
	for _, name := range names {
		c, ok := l.Check(name)
		if !ok {
			s.missing = append(s.missing, name)
			continue
		}
		s.latest[name] = c.Attempt
		switch {
		case c.State == code.CheckStatePassed || c.State == code.CheckStateSkipped:
		case c.State == code.CheckStateFailed:
			s.failed = append(s.failed, name)
		case !c.State.Ended():
			s.pending = append(s.pending, name)
		case c.State == code.CheckStateErrored && c.Reason == code.CheckReasonDefsInvalid:
			s.invalid = append(s.invalid, name)
		case c.State == code.CheckStateErrored && c.Reason.Requeued() && c.Attempt < code.AutoRequeueAttempts:
			s.requeue = append(s.requeue, name)
		default:
			s.stuck = append(s.stuck, name)
		}
	}
	return s
}

// probe queues on l what s says it owes of its checks for t's target, under
// defs, the checks file t's base holds: the first attempt of each it holds
// none of, the next of each a dispatch errored for a file that did not run
// once the file runs, and the next of each its infrastructure ended — each
// under the base's definition, or, where the base's file is none that runs or
// defines no such check, ERRORED there saying so (a tip's dispatch alone: a
// bisect probes only checks its tip ran, under the same file). It answers how
// many attempts it asked: none where everything owed is an attempt the base's
// file cannot define and the rows already say so.
func (k *checks) probe(ctx context.Context, repoID string, t code.TipDue, l code.TipLanding,
	purpose code.CheckPurpose, s landingStates, defs *checksDefs, at statelog.Position) (int, error) {

	about := fmt.Sprintf("target %s at %s", t.TargetRef, short(l.OID))
	base := code.CheckQueued{CheckKey: code.CheckKey{RepoID: repoID, OID: l.OID, Target: t.TargetRef},
		Purpose: purpose, DefsOID: defs.oid, Required: true, Reason: code.QueueDispatch}
	if t.Base() != code.ZeroOID {
		base.Base = t.Base()
	}
	queued := 0
	owe := func(name string, attempt int, reason code.QueueReason) error {
		q := base
		q.Name, q.Attempt, q.Reason = name, attempt, reason
		i := slices.IndexFunc(defs.file.Checks, func(d code.CheckDef) bool { return d.Name == name })
		switch {
		case defs.invalid != "" || i < 0:
			if purpose != code.PurposeTip || reason != code.QueueDispatch || attempt > 1 {
				// NOTHING TO RUN IT UNDER, and nothing to say it again: an
				// attempt already errored for the file says so on the rows.
				return nil
			}
			q.Invalid = defs.invalid
			if q.Invalid == "" {
				q.Invalid = summarize(fmt.Sprintf("%s at %s defines no check %s, which %s's gate requires",
					code.ChecksFilePath, short(t.Base()), name, t.TargetRef))
			}
		default:
			def := defs.file.Checks[i]
			q.Def = &def
		}
		queued++
		return k.queue(ctx, about, q, at)
	}
	for _, name := range s.missing {
		if err := owe(name, 1, code.QueueDispatch); err != nil {
			return queued, err
		}
	}
	if defs.invalid == "" {
		for _, name := range s.invalid {
			if err := owe(name, s.latest[name]+1, code.QueueDispatch); err != nil {
				return queued, err
			}
		}
	}
	for _, name := range s.requeue {
		if err := owe(name, s.latest[name]+1, code.QueueRequeue); err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// queueRecord asks one of the queue's own records of the repository's
// committer (code.queue), about what it is for: a refusal is the rows
// answering — a green or a broken its snapshot no longer proves — read again
// next turn, and only an error of the write itself fails the turn.
func (k *checks) queueRecord(ctx context.Context, about string, p code.QueueProposal, what string) error {
	res, err := k.estate.Queue(ctx, p)
	switch {
	case err != nil:
		return fmt.Errorf("home: %s for %s: %w", what, about, err)
	case res.Refusal != "":
		k.log.Debug("code_tip_record_refused", "about", about, "write", what, "refusal", res.Refusal,
			"detail", res.Detail)
	}
	return nil
}
