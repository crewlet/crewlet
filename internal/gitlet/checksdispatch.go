package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A HEAD'S DISPATCH IS A ROW, SO THE HOME'S WORK LIST CONVERGES ON WHAT IT
// DECIDED. The home dispatches a change head's checks from its TARGET tip's
// .crewlet/checks.yaml, never the head's, so no change narrows the checks that
// judge it, and records it as a check_dispatched: the target, the tip read,
// the blob found there (none where the tip holds no file), the heads and each
// attempt queued or skipped. code_check_targets keeps, per target branch, the
// file its tip held when last read, with that tip, its position and instant;
// code_check_dispatches, per head commit and target, the file the head was
// dispatched under. [ReadChecksDue] is then a function of the rows: a head is
// due while no dispatch of it for its target names the file the target's row
// holds, and a target is due a re-read once its branch has moved past the tip
// read and the row is older than [ChecksDueQuery.RefreshBefore].

// REJECTED: listing a head while its commit holds no attempt. No writer
// records an attempt without its definition, so every head of a repository
// with no checks file stayed listed for ever, its absent file read again every
// pass, and a check a target's file gained never reached a head holding an
// attempt of the old file's. REJECTED too: a dispatch row naming the target's
// tip — every push to a busy target made every head waiting on it due again,
// for a file that seldom changes. The target's row is refreshed at the home's
// pace instead, and the heads follow only a file that changed.

// # Additive, so the apply is a join
//
// The subject carries no expectation, so a home's record and a failover's
// second one land in either order. Each row keeps the record that read the
// later tip (the higher tip position), and of two at one tip the later record:
// a total order, so either order leaves one row byte for byte.

// # What a head owes, and the `checks_due` listing that says so
//
// A live change of an active repository, its target a live branch, owes its
// home ([CheckWork], internal/gitlet/home's checks worker) its DISPATCH while
// no dispatch of its head for its target names the file its target's row
// holds. The worker first cancels superseded the change's live attempts at a
// head or target it moved on from, but those a live change into that target
// heads, which shares them. Once the head revision's facts are recorded
// ([CheckWork.Facts]; a head its target holds records none and is landed
// instead), it queues each check the file defines that the head holds no
// attempt of, or records it skipped by its paths, marked required where the
// target's merge gate requires the name.

// A check whose latest attempt errored for a file that did not run
// ([CheckReasonDefsInvalid]) is queued as its next attempt, so a mended file
// reaches the heads it errored. Where no file runs, each required name the head
// holds no attempt of is recorded errored defs_invalid ([CheckQueued.Invalid]):
// a gate waiting on a check nobody can run says so on the change. The attempts
// ride the dispatch record ([CheckDispatched.Attempts]), so they and the head's
// dispatch row land in one append. The head also owes a REQUEUE of each check
// whose latest attempt its infrastructure ended ([CheckReason.Requeued]) below
// [AutoRequeueAttempts]: the next attempt, under the definition it ran under.

// The home's own records take each off the list (the dispatch's row, the next
// attempt's row), and the listing asks both in SQL over every live change, the
// requeue by code_checks_requeue_idx, which holds only the attempts their
// infrastructure ended. REJECTED: listing a head while a required name lacks
// an attempt — a rule requiring a check its file did not define yet kept every
// change into the target its home's work at every pass. The gate waits on such
// a name, and the head gets it once the file defines it, by the target's
// re-read.

// # Their lifetime
//
// A dispatch row is one a head and a target, a few dozen bytes beside the
// head's revision row, and a target row one a branch the repository's changes
// have targeted; both go with the repository's purge.
//
// # Base format
//
// The op and its payload are in the base format, as the checks subject is
// (checks.go).

// MaxDispatchRows is the most rows one check_dispatched record names, heads
// and attempts together: 500, an eighth of MaxRecordRows (the share a check
// record's horizon takes), so a record stays far inside what one may touch. A
// home dispatching more of one target's tip records them in more records.
const MaxDispatchRows = MaxRecordRows / 8

// MaxDispatchBytes is the most one check_dispatched record's payload encodes
// to: HALF of MaxRecordBytes, 522 240 bytes, leaving room for the record's
// frame, chiefly its scope (at most MaxScopePaths paths, about 22 KB). ONE
// HEAD'S WHOLE DISPATCH ALWAYS FITS, so a head is never split between records:
// its definitions are at most MaxChecksFileBytes (64 KiB) of YAML, which JSON
// writes at most six-fold (`<` as \u003c), and that fits with each attempt's
// own fields (TestTheWidestHeadsDispatchFitsOneRecord). A home whose heads
// weigh more records them in more records.
const MaxDispatchBytes = MaxRecordBytes / 2

// CheckDispatched records a home's reading of a target branch's checks file
// at its tip and the change heads it dispatched under it ([OpCheckDispatched]),
// WITH THE ATTEMPTS THE DISPATCH QUEUES at those heads (queued, skipped by
// paths, or errored for a file that does not run), decided in one snapshot
// and applied in one transaction, each attempt's row a join as a
// check_queued's is (M5-D18). Every attempt is at a head the record names, so
// a head's attempts and the row taking it off its home's list land together,
// in one append. With no heads it only refreshes the target's reading. Base
// format, as the checks subject is (checks.go).
type CheckDispatched struct {
	RepoID string `json:"repo_id"`
	// Target is the branch, as a ref.
	Target string `json:"target"`
	// Tip is the target's tip the checks file was read at, and TipPosition
	// the position of the record that moved the branch there.
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
	// DefsOID is the blob of .crewlet/checks.yaml at Tip, "" where the tip
	// holds none.
	DefsOID string `json:"defs_oid,omitempty"`
	// Heads are the change heads dispatched under it, none for a refresh.
	Heads []DispatchedHead `json:"heads,omitempty"`
	// Attempts are the dispatch's attempts, each one of Heads' for Target under
	// DefsOID, one a check of a commit; with Heads at most [MaxDispatchRows].
	// Base format (checks.go).
	Attempts []CheckQueued `json:"attempts,omitempty"`
	// DispatchedAt is the home's instant.
	DispatchedAt time.Time `json:"dispatched_at"`
}

// DispatchedHead is one change head a dispatch names: its commit, and the
// change and revision the home read it as.
type DispatchedHead struct {
	OID      string `json:"oid"`
	ChangeID string `json:"change_id"`
	Revision int    `json:"revision"`
}

// Validate refuses a dispatch no writer forms ([ErrInvalid], naming the
// field).
func (d CheckDispatched) Validate() error {
	switch p := d.TipPosition; {
	case !canonicalUUID(d.RepoID):
		return checkField("repo_id", "%q is not a UUID in its canonical lowercase spelling", d.RepoID)
	case !strings.HasPrefix(d.Target, "refs/heads/") || !ValidRef(d.Target):
		return checkField("target", "%q is not a branch's ref", d.Target)
	case !gitref.ValidOID(d.Tip) || d.Tip == ZeroOID:
		return checkField("tip", "%q is not a commit's whole id", d.Tip)
	case p.IsZero() || p.Stream != topics.CodeLogStream || p.Valid() != nil:
		return checkField("tip_position", "%s is not the position on the code log (%s) of the record that moved "+
			"the target to its tip", p, topics.CodeLogStream)
	case d.DefsOID != "" && (!gitref.ValidOID(d.DefsOID) || d.DefsOID == ZeroOID):
		return checkField("defs_oid", "%q is not a blob's whole id", d.DefsOID)
	case len(d.Heads)+len(d.Attempts) > MaxDispatchRows:
		return checkField("heads", "%d heads and %d attempts, past the %d rows one record names "+
			"(gitlet.MaxDispatchRows)", len(d.Heads), len(d.Attempts), MaxDispatchRows)
	case d.DispatchedAt.IsZero():
		return checkField("dispatched_at", "a dispatch names the instant it was made")
	}
	if encoded, err := json.Marshal(d); err != nil || len(encoded) > MaxDispatchBytes {
		return checkField("attempts", "%d bytes encoded (%v), past the %d one record carries "+
			"(gitlet.MaxDispatchBytes)", len(encoded), err, MaxDispatchBytes)
	}
	seen := make(map[string]bool, len(d.Heads))
	for i, h := range d.Heads {
		field := fmt.Sprintf("heads[%d]", i)
		switch {
		case !gitref.ValidOID(h.OID) || h.OID == ZeroOID:
			return checkField(field+".oid", "%q is not a commit's whole id", h.OID)
		case seen[h.OID]:
			return checkField(field+".oid", "%s is named twice: a dispatch names each head once", h.OID)
		case !canonicalUUID(h.ChangeID) || h.Revision < 1:
			return checkField(field+".change_id", "%q, revision %d: a head is named by its change's id and one "+
				"of its revisions", h.ChangeID, h.Revision)
		}
		seen[h.OID] = true
	}
	attempts := make(map[CheckKey]bool, len(d.Attempts))
	for i, a := range d.Attempts {
		field := fmt.Sprintf("attempts[%d]", i)
		// AN ATTEMPT PAST THE LAST is the decide's to refuse
		// attempts_exhausted, against the rows, as a check_queued's is
		// ([ChecksRequest.Validate]).
		held := a
		held.Attempt = min(held.Attempt, MaxCheckAttempts)
		if err := held.Validate(); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		check := a.CheckKey
		check.Attempt = 0
		switch {
		case a.RepoID != d.RepoID || a.Target != d.Target:
			return checkField(field, "an attempt of %s for %s, in a dispatch of repository %s's %s", a.RepoID,
				a.Target, d.RepoID, d.Target)
		case a.Purpose != PurposeHead || a.Reason != QueueDispatch:
			return checkField(field+".purpose", "%s by %s: a dispatch queues a change head's checks, by its "+
				"dispatch", a.Purpose, a.Reason)
		case a.DefsOID != d.DefsOID:
			return checkField(field+".defs_oid", "%q, in a dispatch under %q: a dispatch queues the checks the "+
				"file it read defines", a.DefsOID, d.DefsOID)
		case !seen[a.OID]:
			return checkField(field+".oid", "%s is no head the dispatch names: a head's attempts ride the record "+
				"that dispatches it", a.OID)
		case attempts[check]:
			return checkField(field, "check %s of %s is queued twice: a dispatch queues a check of a commit once",
				a.Name, a.OID)
		}
		attempts[check] = true
	}
	return nil
}

// decideDispatched is the checks writer's decide of a dispatch. Each attempt
// is decided as a check_queued of it would be ([decideQueued]) in the same
// snapshot; one the rows already hold or refuse (moved past, exhausted, a
// genuine ending a dispatch does not follow) is DROPPED, its reason kept for
// the writer's log, and the dispatch goes on. It is answered unchanged where
// no attempt is left and the target's reading and every head's dispatch
// already name this file at this tip or a later one, and appended otherwise.
func decideDispatched(ctx context.Context, tx *sql.Tx, d CheckDispatched, out checksDecision,
	named func(CheckKey) string) (checksDecision, error) {

	kept := make([]CheckQueued, 0, len(d.Attempts))
	for _, a := range d.Attempts {
		row, held, err := checkRowIn(ctx, tx, a.CheckKey)
		if err != nil {
			return checksDecision{}, err
		}
		var one checksDecision
		if held {
			one.row = &row
		}
		if one, err = decideQueued(ctx, tx, a, one, held, named); err != nil {
			return checksDecision{}, err
		}
		switch {
		case one.payload != nil:
			kept = append(kept, a)
		case one.refusal != "":
			out.dropped = append(out.dropped, fmt.Sprintf("%s: %s", one.refusal, one.detail))
		}
	}
	d.Attempts = kept
	if len(kept) > 0 {
		out.payload = d
		return out, nil
	}
	d.Attempts = nil
	tip := d.TipPosition.Packed()
	var (
		heldTip int64
		defs    string
	)
	switch err := tx.QueryRowContext(ctx, checkTargetDefsSQL, d.RepoID, d.Target).Scan(&heldTip, &defs); {
	case errors.Is(err, sql.ErrNoRows):
		out.payload = d
		return out, nil
	case err != nil:
		return checksDecision{}, fmt.Errorf("gitlet: read target %s's checks file: %w", d.Target, err)
	case heldTip < tip || defs != d.DefsOID:
		out.payload = d
		return out, nil
	}
	for _, h := range d.Heads {
		switch err := tx.QueryRowContext(ctx, checkDispatchDefsSQL, d.RepoID, h.OID, d.Target).Scan(&heldTip,
			&defs); {
		case errors.Is(err, sql.ErrNoRows):
			out.payload = d
			return out, nil
		case err != nil:
			return checksDecision{}, fmt.Errorf("gitlet: read head %s's dispatch: %w", h.OID, err)
		case heldTip < tip || defs != d.DefsOID:
			out.payload = d
			return out, nil
		}
	}
	out.unchanged = true
	return out, nil
}

// The decide's reads of the rows a dispatch's apply writes (apply_dispatch.go),
// each by its table's whole key and registered in plan_test.go.
const (
	checkTargetDefsSQL = `SELECT tip_position, defs_oid FROM code_check_targets INDEXED BY ` + checkTargetsKey + `
		WHERE repo_id = ? AND target_ref = ?`
	checkDispatchDefsSQL = `SELECT tip_position, defs_oid FROM code_check_dispatches INDEXED BY ` +
		checkDispatchesKey + ` WHERE repo_id = ? AND commit_oid = ? AND target_ref = ?`
)

// CheckWork is what a `checks_due` read says a change's head owes its checks
// (this file's head comment says how each is owed and what clears it), for the
// change the [ChangeDue] it rides names. An estate answer's field, not a
// record's, so it takes no versionedFields row.
type CheckWork struct {
	// Dispatch is a head no dispatch of which for its target names the
	// checks file its target's row holds.
	Dispatch bool `json:"dispatch,omitempty"`
	// Held is, for a head owing its dispatch, each check name the head holds
	// an attempt of for its target, sorted, but those in Invalid: its
	// dispatch queues none of them.
	Held []string `json:"held,omitempty"`
	// Invalid is, for a head owing its dispatch, the latest attempt's key of
	// each check errored at its dispatch ([CheckReasonDefsInvalid]): a
	// dispatch under a file defining it in a form that runs queues the next.
	Invalid []CheckKey `json:"invalid,omitempty"`
	// Required is, for a head owing its dispatch, every check name its
	// target's merge gate requires ([GateRules.Checks]), sorted.
	Required []string `json:"required,omitempty"`
	// Stale is, for a head owing its dispatch, the change's attempts still
	// queued or running at a head or target it moved on from, which its home
	// cancels superseded before dispatching the head.
	Stale []CheckKey `json:"stale,omitempty"`
	// Requeue is the next attempt of each check of the head for its target
	// whose latest its infrastructure ended below AutoRequeueAttempts: that
	// attempt's queue, under the definition it ran under, its reason requeue,
	// all but the instant, which is the home's.
	Requeue []CheckQueued `json:"requeue,omitempty"`
	// Facts is, for a head owing its dispatch, what its head revision touches
	// against its target as code_revision_facts records it; nil until
	// recorded, and the dispatch waits for them whatever the file defines.
	Facts *TouchedPaths `json:"facts,omitempty"`
}

// TouchedPaths is what a change's head revision touches against its target, as
// its recorded facts say ([Revise]'s Touched and TouchesAll): what a `paths`
// check's globs are matched against, every check running where the revision
// touches more than a list holds.
type TouchedPaths struct {
	Touched    []string `json:"touched,omitempty"`
	TouchesAll bool     `json:"touches_all,omitempty"`
}

// requiredChecks is a repository's rules as a `checks_due` page judges its
// changes' targets: each target's required checks as ITS MERGE GATE reads
// them ([GateRulesOf]'s Checks), one derivation so a head's attempts are
// marked required by exactly the names its gate judges, once a page.
type requiredChecks struct {
	rules    []Protection
	byTarget map[string][]string
}

// requiredIn reads repoID's rules whole.
func requiredIn(ctx context.Context, tx *sql.Tx, repoID string) (*requiredChecks, error) {
	rules, err := protectionsIn(ctx, tx, repoID)
	if err != nil {
		return nil, err
	}
	return &requiredChecks{rules: rules, byTarget: map[string][]string{}}, nil
}

// of is every check name target's gate requires, sorted and each once; nil
// for none. A rule whose pattern this build does not read is refused, as the
// gate refuses it: read as absent, its check would be one the gate waits for
// that nobody marks required.
func (r *requiredChecks) of(target string) ([]string, error) {
	if out, ok := r.byTarget[target]; ok {
		return out, nil
	}
	gate, err := GateRulesOf(r.rules, target, "")
	if err != nil {
		return nil, err
	}
	r.byTarget[target] = gate.Checks
	return gate.Checks, nil
}

// checkHead is a live change as one repository's `checks_due` page reads it:
// its due change, and whether its dispatch and a requeue are owed.
type checkHead struct {
	due               ChangeDue
	dispatch, requeue bool
}

// dueCheckHeadsIn is a page of repoID's live changes owing checks work, by
// source branch after cursor, read whole before any change's work so no other
// statement runs beside the listing's.
func dueCheckHeadsIn(ctx context.Context, tx *sql.Tx, repoID, cursor string, limit int) ([]checkHead, error) {
	rows, err := tx.QueryContext(ctx, dueCheckChangesSQL, repoID, cursor, limit, AutoRequeueAttempts)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list repository %s's heads due their checks: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []checkHead
	for rows.Next() {
		var (
			h  checkHead
			at int64
		)
		if err := rows.Scan(&h.due.ID, &h.due.Number, &h.due.Revision, &h.due.Head, &h.due.SourceRef,
			&h.due.TargetRef, &h.due.Tip, &at, &h.dispatch, &h.requeue); err != nil {
			return nil, fmt.Errorf("gitlet: list repository %s's heads due their checks: %w", repoID, err)
		}
		h.due.TipPosition = unpacked(at)
		out = append(out, h)
	}
	return out, rows.Err()
}

// checkWorkOf is what h owes its checks, each [CheckWork] field read only
// where its dispatch or a requeue is owed.
func checkWorkOf(ctx context.Context, tx *sql.Tx, repoID string, h checkHead, required *requiredChecks) (
	CheckWork, error) {

	w := CheckWork{Dispatch: h.dispatch}
	var err error
	if h.dispatch {
		if w.Held, w.Invalid, err = headAttemptsIn(ctx, tx, repoID, h.due.Head, h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
		if w.Required, err = required.of(h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
		if w.Stale, err = staleChecksIn(ctx, tx, repoID, h.due.ID, h.due.Head, h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
		if w.Facts, err = touchedPathsIn(ctx, tx, h.due.ID, h.due.Revision, h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
	}
	if h.requeue {
		if w.Requeue, err = requeuesIn(ctx, tx, repoID, h.due.Head, h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
	}
	return w, nil
}

// headAttemptsIn is what commit oid holds of each check for target, as
// [CheckWork]'s Held and Invalid: a range of code_checks' key, at most
// MaxChecksPerFile checks of MaxCheckAttempts attempts each.
func headAttemptsIn(ctx context.Context, tx *sql.Tx, repoID, oid, target string) ([]string, []CheckKey, error) {
	rows, err := tx.QueryContext(ctx, headAttemptsSQL, repoID, oid, target)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlet: read the checks %s holds for %s: %w", oid, target, err)
	}
	defer func() { _ = rows.Close() }()
	type latest struct {
		attempt       int
		state, reason string
	}
	var (
		names []string
		last  = map[string]latest{}
	)
	for rows.Next() {
		var (
			name string
			l    latest
		)
		if err := rows.Scan(&name, &l.attempt, &l.state, &l.reason); err != nil {
			return nil, nil, fmt.Errorf("gitlet: read the checks %s holds for %s: %w", oid, target, err)
		}
		if _, seen := last[name]; !seen {
			names = append(names, name)
		}
		last[name] = l
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("gitlet: read the checks %s holds for %s: %w", oid, target, err)
	}
	var (
		held    []string
		invalid []CheckKey
	)
	for _, name := range names {
		l := last[name]
		if CheckState(l.state) == CheckStateErrored && CheckReason(l.reason) == CheckReasonDefsInvalid {
			invalid = append(invalid, CheckKey{RepoID: repoID, OID: oid, Target: target, Name: name,
				Attempt: l.attempt})
			continue
		}
		held = append(held, name)
	}
	return held, invalid, nil
}

// touchedPathsIn is what revision n of changeID touches against target, one
// seek of code_revision_facts' key; nil where the revision holds no facts for
// target: none read yet, or read against a target it was retargeted from.
func touchedPathsIn(ctx context.Context, tx *sql.Tx, changeID string, n int64, target string) (*TouchedPaths, error) {
	var (
		f       TouchedPaths
		touched string
	)
	switch err := tx.QueryRowContext(ctx, touchedPathsSQL, changeID, n, target).Scan(&f.TouchesAll, &touched); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("gitlet: read the facts of revision %d of %s: %w", n, changeID, err)
	}
	if touched != "" {
		f.Touched = strings.Split(touched, "\x00")
	}
	return &f, nil
}

// staleChecksIn is [CheckWork.Stale]: changeID's attempts not yet ended at a
// commit or target other than its head's, by code_checks_live_idx, each kept
// where no live change into the attempt's target heads its commit.
func staleChecksIn(ctx context.Context, tx *sql.Tx, repoID, changeID, head, target string) ([]CheckKey, error) {
	rows, err := tx.QueryContext(ctx, staleChecksSQL, changeID, head, target, repoID)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the checks change %s moved on from: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []CheckKey
	for rows.Next() {
		k := CheckKey{RepoID: repoID}
		if err := rows.Scan(&k.OID, &k.Target, &k.Name, &k.Attempt); err != nil {
			return nil, fmt.Errorf("gitlet: read the checks change %s moved on from: %w", changeID, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// requeuesIn is [CheckWork.Requeue] for commit oid and target.
func requeuesIn(ctx context.Context, tx *sql.Tx, repoID, oid, target string) ([]CheckQueued, error) {
	rows, err := tx.QueryContext(ctx, requeuesSQL, repoID, oid, target, AutoRequeueAttempts)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the checks of %s for %s due a requeue: %w", oid, target, err)
	}
	defer func() { _ = rows.Close() }()
	var out []CheckQueued
	for rows.Next() {
		var (
			q        CheckQueued
			purpose  string
			revision int64
			def      string
		)
		q.RepoID, q.OID, q.Target = repoID, oid, target
		if err := rows.Scan(&q.Name, &q.Attempt, &purpose, &q.ChangeID, &revision, &q.CandidateID, &q.EntryID,
			&q.Base, &q.DefsOID, &q.Required, &def); err != nil {
			return nil, fmt.Errorf("gitlet: read the checks of %s for %s due a requeue: %w", oid, target, err)
		}
		q.Attempt++
		q.Purpose, q.Revision, q.Reason = CheckPurpose(purpose), int(revision), QueueRequeue
		if def != "" {
			q.Def = &CheckDef{}
			if err := json.Unmarshal([]byte(def), q.Def); err != nil {
				return nil, fmt.Errorf("gitlet: read check %s of %s's definition: %w", q.Name, oid, err)
			}
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// The work's statements, each registered in plan_test.go. Facts are one seek
// of code_revision_facts' key (change_id, n), the target tested on the row;
// stale attempts read code_checks_live_idx (replicated 0116) and the sharing
// change code_changes_target_idx (liveChanges); a requeue reads
// code_checks_requeue_idx (replicated 0116), then the next attempt's absence
// by code_checks' key. Each partial index's predicate is spelled as the index
// declares it.
const (
	touchedPathsSQL = `SELECT touches_all, touched FROM code_revision_facts
		WHERE change_id = ? AND n = ? AND target_ref = ?`
	headAttemptsSQL = `SELECT name, attempt, state, reason FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? ORDER BY name, attempt`
	staleChecksSQL = `SELECT k.commit_oid, k.target_ref, k.name, k.attempt
		FROM code_checks AS k INDEXED BY code_checks_live_idx
		WHERE k.change_id = ?1 AND k.state IN ('queued', 'running') AND k.purpose = 'head'
			AND (k.commit_oid != ?2 OR k.target_ref != ?3)
			AND NOT EXISTS (SELECT 1 FROM code_changes AS o INDEXED BY code_changes_target_idx
				WHERE o.repo_id = ?4 AND o.target_ref = k.target_ref AND o.head_oid = k.commit_oid
					AND ` + liveChanges + `)
		ORDER BY k.commit_oid, k.target_ref, k.name, k.attempt`
	requeuesSQL = `SELECT k.name, k.attempt, k.purpose, k.change_id, k.revision, k.candidate_id, k.entry_id,
			k.base_oid, k.defs_oid, k.required, k.def
		FROM code_checks AS k INDEXED BY code_checks_requeue_idx
		WHERE k.repo_id = ?1 AND k.commit_oid = ?2 AND k.target_ref = ?3 AND ` + requeuedRows + `
			AND k.attempt < ?4
			AND NOT EXISTS (SELECT 1 FROM code_checks AS n INDEXED BY ` + checksKey + `
				WHERE n.repo_id = k.repo_id AND n.commit_oid = k.commit_oid AND n.target_ref = k.target_ref
					AND n.name = k.name AND n.attempt = k.attempt + 1)
		ORDER BY k.name`
	// requeuedRows is code_checks_requeue_idx's predicate on the alias k: an
	// attempt its infrastructure ended ([CheckReason.Requeued]).
	requeuedRows = `k.state = 'errored' AND k.reason IN ('runner_lost', 'box_failed', 'setup_failed')`
)
