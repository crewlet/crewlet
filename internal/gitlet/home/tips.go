package home

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// tip is a protected target's TIP WORK (ADR-0098; internal/gitlet's tipwork.go), in order:
//   - no check required: the green row moved onto the newest landing;
//   - a required check the newest landing holds no attempt of: DISPATCHED there (purpose
//     tip) under the checks file the BASE holds, one round at a time: never while another
//     tip round of the target is in flight (gitlet.TipDue.InFlight), so a target landed on
//     faster than its checks run keeps one round in flight, not one a landing;
//   - an attempt owed again (landingStates' requeue): queued again;
//   - all passed (or skipped): the green row moved onto it; one failed: BISECTED, not
//     waiting on a check the base's file (a commit's, never changing) cannot define.
//
// Anything else waits for the record that ends it. Each writer (code.checks, code.queue)
// refuses what the one snapshot t was read in no longer proves; the next turn reads again.
func (k *checks) tip(ctx context.Context, repoID string, t gitlet.TipDue, memo *defsMemo,
	at statelog.Position) error {

	// THE CULPRIT PAST THE WINDOW is warned of once a tip (culpritsPastWindow):
	// forgotten on any turn that finds the target out of that state.
	pastWindow := false
	defer func() {
		if !pastWindow {
			k.culprits.forget(repoID, t.TargetRef)
		}
	}()
	newest := t.Newest()
	about := fmt.Sprintf("target %s at %s", t.TargetRef, short(newest.OID))
	if len(t.Required) == 0 {
		return k.green(ctx, repoID, t, newest, at)
	}
	states := tipStates(newest, t.Required)
	if len(states.missing) > 0 && t.InFlight != "" {
		k.log.Debug("code_tip_waits_on_its_round", "repo", repoID, "about", about, "in_flight",
			short(t.InFlight))
		return nil
	}
	if len(states.missing) > 0 || len(states.invalid) > 0 || len(states.requeue) > 0 {
		defs, err := k.baseDefs(ctx, repoID, t, memo, at)
		if err != nil {
			return err
		}
		// WHAT IT QUEUED IS WHAT THE TIP WAITS ON; an attempt the base's
		// file could not define, which nothing queues again, does not hold
		// back the measurements beside it.
		if queued, err := k.probe(ctx, repoID, t, newest, gitlet.PurposeTip, states, defs, at); err != nil ||
			queued > 0 {
			return err
		}
	}
	switch {
	case len(states.pending) > 0:
		return nil
	case len(states.failed) > 0:
		var err error
		pastWindow, err = k.bisect(ctx, repoID, t, states.failed, memo, at)
		return err
	case states.passed():
		return k.green(ctx, repoID, t, newest, at)
	}
	k.log.Debug("code_tip_waits_on_a_person", "repo", repoID, "about", about, "invalid", states.invalid,
		"checks", states.stuck)
	return nil
}

// green asks the repository's committer to move t's green row onto l, the
// newest landing above it and so the tip of its record.
func (k *checks) green(ctx context.Context, repoID string, t gitlet.TipDue, l gitlet.TipLanding,
	at statelog.Position) error {

	return k.queueRecord(ctx, fmt.Sprintf("target %s at %s", t.TargetRef, short(l.OID)), gitlet.QueueProposal{
		OpID: statelog.NewOpID(k.now(), ""), Seen: at,
		Green: &gitlet.Green{RepoID: repoID, TargetRef: t.TargetRef, OID: l.OID, Through: l.Position}},
		"move its green row")
}

// bisect finds the first landing above t's green on which one of failed (the tip's
// failed checks, so a second break behind a named one is found by its own) failed,
// from each landing's own attempts in the rows, never git's history:
//   - red is the first landing one of failed FAILED on (the tip at the latest), good
//     the last before it every one of failed passed on (else the green itself);
//   - adjacent, red is the culprit, recorded broken once;
//   - otherwise the landing midway is PROBED (purpose bisect) under the base's file.
//
// A probe still running, or one only a person's rerun moves, waits; so does a culprit
// the window cannot name (good its last landing, the tip read past a gap) until a green
// tip beyond clears it. bisect answers true for that, WARNED ONCE A TIP, not once a turn
// as every check record asks one (code_tip_culprit_past_the_window).
func (k *checks) bisect(ctx context.Context, repoID string, t gitlet.TipDue, failed []string, memo *defsMemo,
	at statelog.Position) (bool, error) {

	red := slices.IndexFunc(t.Landings, func(l gitlet.TipLanding) bool {
		return slices.ContainsFunc(failed, func(name string) bool {
			c, ok := l.Check(name)
			return ok && c.State == gitlet.CheckStateFailed
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
			return false, nil
		case t.More && red == len(t.Landings)-1:
			// THE GAP: the tip is read past the window, so the landing
			// before it in the list is not the one before it on the branch.
			if k.culprits.first(repoID, t.TargetRef, culprit.OID) {
				k.log.Warn("code_tip_culprit_past_the_window", "repo", repoID, "target", t.TargetRef,
					"tip", culprit.OID, "landings", gitlet.MaxTipLandings,
					"detail", "more landings lie above the green than one bisect reads; a green tip past "+
						"them clears them, or a person reverts to the last green")
			}
			return true, nil
		}
		var names []string
		for _, name := range failed {
			if c, ok := culprit.Check(name); ok && c.State == gitlet.CheckStateFailed {
				names = append(names, name)
			}
		}
		return false, k.queueRecord(ctx, fmt.Sprintf("target %s at %s", t.TargetRef, short(culprit.OID)),
			gitlet.QueueProposal{OpID: statelog.NewOpID(k.now(), ""), Seen: at,
				Broken: &gitlet.Broken{RepoID: repoID, TargetRef: t.TargetRef, Position: culprit.Position,
					Ordinal: culprit.Ordinal, OID: culprit.OID, Checks: names}},
			"record its first red landing broken")
	}
	// MIDWAY, strictly between good and red: never the tip, which red is at
	// the latest, so never across the window's gap.
	mid := t.Landings[(good+red+1)/2]
	states := tipStates(mid, failed)
	if len(states.missing) == 0 && len(states.invalid) == 0 && len(states.requeue) == 0 {
		return false, nil
	}
	defs, err := k.baseDefs(ctx, repoID, t, memo, at)
	if err != nil {
		return false, err
	}
	_, err = k.probe(ctx, repoID, t, mid, gitlet.PurposeBisect, states, defs, at)
	return false, err
}

// culpritsPastWindow is, by repository and target, the tip last warned of on this
// node as a culprit past its bisect window. An entry goes on the first turn that finds
// its target out of that state, so it holds a few at most; one whose target never
// turns here again stays, a key and a commit id, until restart. In memory, not the
// rows, because it decides only a log line: a restarted node, or a new home, warns of
// a standing gap once more.
type culpritsPastWindow struct {
	mu   sync.Mutex
	tips map[[2]string]string
}

// first records tip as target's on repoID and reports whether it is new: not
// the tip last warned of.
func (c *culpritsPastWindow) first(repoID, target, tip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]string{repoID, target}
	if c.tips[key] == tip {
		return false
	}
	if c.tips == nil {
		c.tips = map[[2]string]string{}
	}
	c.tips[key] = tip
	return true
}

// forget drops target's entry on repoID: it is out of that state.
func (c *culpritsPastWindow) forget(repoID, target string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tips, [2]string{repoID, target})
}

// baseDefs is the checks file t's tip checks are defined by: the one t's base
// holds — and, where the first landing above the green CREATED the branch,
// none that runs, since the branch held no file before a landing could write
// one, and a landing's own file never defines its measurement.
func (k *checks) baseDefs(ctx context.Context, repoID string, t gitlet.TipDue, memo *defsMemo,
	at statelog.Position) (*checksDefs, error) {

	if t.Base() == gitlet.ZeroOID {
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
func passedAll(l gitlet.TipLanding, names []string) bool {
	for _, name := range names {
		c, ok := l.Check(name)
		if !ok || (c.State != gitlet.CheckStatePassed && c.State != gitlet.CheckStateSkipped) {
			return false
		}
	}
	return true
}

// landingStates is where each of a set of checks stands on one landing by its latest
// attempt: none (missing); errored at a dispatch for a checks file that did not run
// (invalid); still to run (pending); ended by its infrastructure below
// gitlet.AutoRequeueAttempts or cancelled by a merge queue giving its train up below
// gitlet.MaxCheckAttempts (requeue), since neither measured the commit and a rebuilt
// chain is the same commits; ended otherwise unmeasured (stuck); failed; or passed.
type landingStates struct {
	missing, invalid, pending, requeue, stuck, failed []string
	latest                                            map[string]int
}

// passed reports every check of the set passed (or skipped) on the landing.
func (s landingStates) passed() bool {
	return len(s.missing)+len(s.invalid)+len(s.pending)+len(s.requeue)+len(s.stuck)+len(s.failed) == 0
}

// tipStates is where each of names stands on l.
func tipStates(l gitlet.TipLanding, names []string) landingStates { return statesOf(l.Check, names) }

// statesOf is where each of names stands on one commit, check its latest
// attempt of a name — a landing's (tipStates) or a candidate's commit's
// (commitStates).
func statesOf(check func(name string) (gitlet.TipCheck, bool), names []string) landingStates {
	s := landingStates{latest: map[string]int{}}
	for _, name := range names {
		c, ok := check(name)
		if !ok {
			s.missing = append(s.missing, name)
			continue
		}
		s.latest[name] = c.Attempt
		switch {
		case c.State == gitlet.CheckStatePassed || c.State == gitlet.CheckStateSkipped:
		case c.State == gitlet.CheckStateFailed:
			s.failed = append(s.failed, name)
		case !c.State.Ended():
			s.pending = append(s.pending, name)
		case c.State == gitlet.CheckStateErrored && c.Reason == gitlet.CheckReasonDefsInvalid:
			s.invalid = append(s.invalid, name)
		case c.State == gitlet.CheckStateErrored && c.Reason.Requeued() && c.Attempt < gitlet.AutoRequeueAttempts,
			c.State == gitlet.CheckStateCancelled && c.Reason.GivenUp() && c.Attempt < gitlet.MaxCheckAttempts:
			s.requeue = append(s.requeue, name)
		default:
			s.stuck = append(s.stuck, name)
		}
	}
	return s
}

// probe queues on l what s says it owes for t's target under defs, the checks file
// t's base holds: the first attempt of each missing check, the next of each invalid
// one once the file runs, and the next of each requeue. Where the file is none that
// runs or defines no such check, a tip's first dispatch is queued ERRORED saying so
// (a bisect probes only checks its tip ran, under the same file). It answers how many
// attempts it asked: none where all it owes is already errored on the rows for that.
func (k *checks) probe(ctx context.Context, repoID string, t gitlet.TipDue, l gitlet.TipLanding,
	purpose gitlet.CheckPurpose, s landingStates, defs *checksDefs, at statelog.Position) (int, error) {

	about := fmt.Sprintf("target %s at %s", t.TargetRef, short(l.OID))
	base := gitlet.CheckQueued{CheckKey: gitlet.CheckKey{RepoID: repoID, OID: l.OID, Target: t.TargetRef},
		Purpose: purpose, DefsOID: defs.oid, Required: true, Reason: gitlet.QueueDispatch}
	if t.Base() != gitlet.ZeroOID {
		base.Base = t.Base()
	}
	queued := 0
	owe := func(name string, attempt int, reason gitlet.QueueReason) error {
		q := base
		q.Name, q.Attempt, q.Reason = name, attempt, reason
		i := slices.IndexFunc(defs.file.Checks, func(d gitlet.CheckDef) bool { return d.Name == name })
		switch {
		case defs.invalid != "" || i < 0:
			if purpose != gitlet.PurposeTip || reason != gitlet.QueueDispatch || attempt > 1 {
				// NOTHING TO RUN IT UNDER, and nothing to say it again: an
				// attempt already errored for the file says so on the rows.
				return nil
			}
			q.Invalid = defs.invalid
			if q.Invalid == "" {
				q.Invalid = summarize(fmt.Sprintf("%s at %s defines no check %s, which %s's gate requires",
					gitlet.ChecksFilePath, short(t.Base()), name, t.TargetRef))
			}
		default:
			def := defs.file.Checks[i]
			q.Def = &def
		}
		queued++
		return k.queue(ctx, about, q, at)
	}
	for _, name := range s.missing {
		if err := owe(name, 1, gitlet.QueueDispatch); err != nil {
			return queued, err
		}
	}
	if defs.invalid == "" {
		for _, name := range s.invalid {
			if err := owe(name, s.latest[name]+1, gitlet.QueueDispatch); err != nil {
				return queued, err
			}
		}
	}
	for _, name := range s.requeue {
		if err := owe(name, s.latest[name]+1, gitlet.QueueRequeue); err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// queueRecord asks one of the queue's own records of the repository's
// committer (code.queue), about what it is for: a refusal is the rows
// answering — a green or a broken its snapshot no longer proves — read again
// next turn, and only an error of the write itself fails the turn.
func (k *checks) queueRecord(ctx context.Context, about string, p gitlet.QueueProposal, what string) error {
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
