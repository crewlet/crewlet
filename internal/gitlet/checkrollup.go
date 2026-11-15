package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// A CHECK ROLLUP is each check's latest attempt of one commit for one branch,
// judged as the merge gate judges a check (gatejudge.go), so no screen says
// "passing" where the gate waits. Every screen reads it through rollUp.

// CheckRollup is a rollup.
type CheckRollup struct {
	// State is the rollup's one word, the first that holds of: failing,
	// running, pending, cancelled, passed.
	State CheckRollupState `json:"state"`
	// Counts is how many checks' latest attempts are in each state.
	Counts CheckCounts `json:"counts"`
	// Required is how many of the checks are required, and RequiredPassed
	// how many of those passed or were skipped: "Passed 2 of 2".
	Required       int `json:"required"`
	RequiredPassed int `json:"required_passed"`
	// LastEnded is the newest ending among the checks, and RunningSince the
	// oldest start among those running; each absent where none.
	LastEnded    time.Time `json:"last_ended,omitzero"`
	RunningSince time.Time `json:"running_since,omitzero"`
	// Failing is the checks that failed or errored past the home's requeue,
	// required first and then by name, at most MaxRollupFailing;
	// FailingMore how many more.
	Failing     []FailingCheck `json:"failing,omitempty"`
	FailingMore int            `json:"failing_more,omitempty"`
	// More is set where the commit holds more checks for the branch than a
	// rollup reads (maxRollupChecks): the counts are of those it read.
	More bool `json:"more,omitempty"`
}

// CheckRollupState is a rollup's one word. A closed set
// ([CheckRollupStates]), with Valid.
type CheckRollupState string

const (
	// RollupFailing is a set with a check that failed, or errored past
	// the home's requeue: the gate waits on the change's owner.
	RollupFailing CheckRollupState = "failing"
	// RollupRunning is a set none of whose checks failed, one of them
	// running.
	RollupRunning CheckRollupState = "running"
	// RollupPending is a set with a check queued, errored and queued again
	// by the home, or in a state a later build records, and none running.
	RollupPending CheckRollupState = "pending"
	// RollupCancelled is a set whose checks all ended, one of them
	// cancelled and none failing: a head its next push moved on from, which
	// a history row does not draw red. The gate's own reading stays in
	// Required and RequiredPassed, where a cancelled required check has
	// not passed.
	RollupCancelled CheckRollupState = "cancelled"
	// RollupPassed is a set whose every check passed or was skipped.
	RollupPassed CheckRollupState = "passed"
)

// CheckRollupStates is the closed set, in the order State is judged.
func CheckRollupStates() []CheckRollupState {
	return []CheckRollupState{RollupFailing, RollupRunning, RollupPending, RollupCancelled, RollupPassed}
}

// Valid reports a state this build knows.
func (s CheckRollupState) Valid() bool { return slices.Contains(CheckRollupStates(), s) }

// CheckCounts is how many checks' latest attempts are in each state, by the
// state's own name ([CheckStates]); Other counts a state this build does not
// know (a later build's row).
type CheckCounts struct {
	Queued    int `json:"queued,omitempty"`
	Running   int `json:"running,omitempty"`
	Passed    int `json:"passed,omitempty"`
	Failed    int `json:"failed,omitempty"`
	Errored   int `json:"errored,omitempty"`
	Cancelled int `json:"cancelled,omitempty"`
	Skipped   int `json:"skipped,omitempty"`
	Other     int `json:"other,omitempty"`
}

// count adds one attempt in state s.
func (c *CheckCounts) count(s CheckState) {
	switch s {
	case CheckStateQueued:
		c.Queued++
	case CheckStateRunning:
		c.Running++
	case CheckStatePassed:
		c.Passed++
	case CheckStateFailed:
		c.Failed++
	case CheckStateErrored:
		c.Errored++
	case CheckStateCancelled:
		c.Cancelled++
	case CheckStateSkipped:
		c.Skipped++
	default:
		c.Other++
	}
}

// FailingCheck is one check that did not pass.
type FailingCheck struct {
	Name     string     `json:"name"`
	State    CheckState `json:"state"`
	Required bool       `json:"required,omitempty"`
}

// MaxRollupFailing is the most failing checks a rollup names: a row's title
// names a few, and the count says the rest.
const MaxRollupFailing = 8

// maxRollupChecks is the most checks one rollup reads for one commit and
// branch: twice MaxChecksPerFile, a target whose checks file changed between
// two dispatches of one head.
const maxRollupChecks = 2 * MaxChecksPerFile

// rollupRow is one check's latest attempt as a rollup reads it.
type rollupRow struct {
	name                  string
	attempt               int
	state                 CheckState
	reason                CheckReason
	required              bool
	startedAt, finishedAt time.Time
}

// failing reports an attempt that did not pass and that nobody but the
// change's owner moves on: failed, or errored where the home does not queue
// it again (its infrastructure's ending past AutoRequeueAttempts, or one the
// commit or its definition caused), as the gate's check precondition reads it.
func (r rollupRow) failing() bool {
	switch r.state {
	case CheckStateFailed:
		return true
	case CheckStateErrored:
		return !r.requeued()
	}
	return false
}

// requeued reports an errored attempt the home queues again on its own.
func (r rollupRow) requeued() bool {
	return r.state == CheckStateErrored && r.reason.Requeued() && r.attempt < AutoRequeueAttempts
}

// rollUp is rows' rollup (every row one check's latest attempt for one commit
// and branch, by name), or nil for none: a rollup of nothing is no "passed".
// more says the statement read maxRollupChecks and stopped.
func rollUp(rows []rollupRow, more bool) *CheckRollup {
	if len(rows) == 0 {
		return nil
	}
	out := &CheckRollup{More: more}
	var failing, running, pending, cancelled bool
	var failed []FailingCheck
	for _, r := range rows {
		out.Counts.count(r.state)
		if r.required {
			out.Required++
			if r.state == CheckStatePassed || r.state == CheckStateSkipped {
				out.RequiredPassed++
			}
		}
		if !r.finishedAt.IsZero() && r.finishedAt.After(out.LastEnded) {
			out.LastEnded = r.finishedAt
		}
		switch {
		case r.failing():
			failing = true
			failed = append(failed, FailingCheck{Name: r.name, State: r.state, Required: r.required})
		case r.state == CheckStateRunning:
			running = true
			if !r.startedAt.IsZero() && (out.RunningSince.IsZero() || r.startedAt.Before(out.RunningSince)) {
				out.RunningSince = r.startedAt
			}
		case r.state == CheckStateQueued, r.requeued(), !r.state.Valid():
			// A STATE A LATER BUILD RECORDS is pending, as the gate reads it:
			// no ending this build can read as a pass, and no failure it can
			// name.
			pending = true
		case r.state == CheckStateCancelled:
			cancelled = true
		}
	}
	switch {
	case failing:
		out.State = RollupFailing
	case running:
		out.State = RollupRunning
	case pending:
		out.State = RollupPending
	case cancelled:
		out.State = RollupCancelled
	default:
		out.State = RollupPassed
	}
	// REQUIRED FIRST, then by name: a row's title names what blocks the
	// merge before what only reports.
	slices.SortFunc(failed, func(a, b FailingCheck) int {
		if a.Required != b.Required {
			if a.Required {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	if len(failed) > MaxRollupFailing {
		out.FailingMore = len(failed) - MaxRollupFailing
		failed = failed[:MaxRollupFailing]
	}
	out.Failing = failed
	return out
}

// rollupColumns is a latest attempt as a rollup reads it, in [scanRollupRow]'s
// order.
const rollupColumns = `k.name, k.attempt, k.state, k.reason, k.required, k.started_at, k.finished_at`

// A ROLLUP'S READS, each a walk of code_checks' key (checksKey) kept to each
// check's latest attempt by latestOnly's one seek above it, in the key's own
// order: the PRIMARY KEY holds every attempt, where code_checks_requeue_idx
// holds only the errored ones the home queues again. Each read one row past
// its bound to say More. Registered in plan_test.go.
const (
	// latestChecksOfTargetSQL is each check's latest attempt of one commit
	// for one branch, by name: the rollup of a merge's gate, of a branch's
	// tip, of a change's head.
	latestChecksOfTargetSQL = `SELECT ` + rollupColumns + ` FROM code_checks AS k INDEXED BY ` + checksKey + `
		WHERE k.repo_id = ?1 AND k.commit_oid = ?2 AND k.target_ref = ?3 AND ` + latestOnly + `
		ORDER BY k.name, k.attempt LIMIT ?4`
	// latestChecksOfCommitSQL is the same for every branch the commit was
	// checked for, by branch and name: a history page's marks.
	latestChecksOfCommitSQL = `SELECT k.target_ref, ` + rollupColumns + ` FROM code_checks AS k INDEXED BY ` +
		checksKey + `
		WHERE k.repo_id = ?1 AND k.commit_oid = ?2 AND ` + latestOnly + `
		ORDER BY k.target_ref, k.name, k.attempt LIMIT ?3`
)

// scanRollupRow reads one row of [rollupColumns], after lead where given.
func scanRollupRow(s scanner, lead ...any) (rollupRow, error) {
	var (
		r                     rollupRow
		state, reason         string
		startedAt, finishedAt int64
	)
	if err := s.Scan(append(lead, &r.name, &r.attempt, &state, &reason, &r.required, &startedAt,
		&finishedAt)...); err != nil {
		return rollupRow{}, err
	}
	r.state, r.reason = CheckState(state), CheckReason(reason)
	r.startedAt, r.finishedAt = activityAt(startedAt), activityAt(finishedAt)
	return r, nil
}

// rollupOfTarget is where commit's checks for target stand in tx, or nil
// where the rows keep no attempt of them: one walk of latestChecksOfTargetSQL,
// read maxRollupChecks + 1 deep.
func rollupOfTarget(ctx context.Context, tx *sql.Tx, repoID, commit, target string) (*CheckRollup, error) {
	rows, err := tx.QueryContext(ctx, latestChecksOfTargetSQL, repoID, commit, target, maxRollupChecks+1)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the checks of %s for %s: %w", commit, target, err)
	}
	var read []rollupRow
	err = eachRow(rows, func(s scanner) error {
		r, err := scanRollupRow(s)
		if err == nil {
			read = append(read, r)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the checks of %s for %s: %w", commit, target, err)
	}
	more := len(read) > maxRollupChecks
	return rollUp(read[:min(len(read), maxRollupChecks)], more), nil
}
