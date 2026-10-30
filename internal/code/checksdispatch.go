package code

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
// DECIDED.
//
// A repository's home dispatches a change head's checks from its TARGET's
// checks file — the governing file is the target tip's, never the head's, so
// no change narrows the checks that judge it: it reads .crewlet/checks.yaml at
// the target's tip, queues each check the file defines at the head for that
// target (or records it skipped), and then RECORDS THAT IT DID — a
// check_dispatched record on the repository's checks subject naming the
// target, the tip it read, the blob it found there (none, where the tip holds
// no checks file) and the heads it dispatched under it. Two tables keep what
// those records say:
//
//   - code_check_targets: per repository and target branch, the checks file
//     the branch's tip held when the home last read it — its blob, or none —
//     with that tip, the position of the record that moved the branch there,
//     and the instant the reading was recorded;
//   - code_check_dispatches: per repository, head commit and target branch,
//     the checks file the head was dispatched under.
//
// The home's due listing ([ReadChecksDue], checksread.go) is then a function
// of the rows alone: a live change's head is due while no dispatch of it FOR
// ITS TARGET names the checks file the target's row holds — a head never
// dispatched, a change retargeted, a target whose checks file changed since —
// and a target is due a reading of its checks file again where its branch has
// moved past the tip its row was read at and the row is older than the home's
// refresh ([ChecksDueQuery.RefreshBefore]). A head whose target defines no
// checks leaves the listing after one record, as one whose target defines
// many does, and a check a target's file gains reaches the heads waiting on
// it.
//
// REJECTED: listing a head while its commit holds no attempt of any check.
// A target with no checks file is every repository that has not adopted
// checks, and none of its heads ever holds an attempt — no writer records an
// attempt without the definition it runs — so each stayed listed for ever,
// read on every pass of every home, its absent file read again each time; and
// a check a target's file gained never reached a head that held an attempt of
// the old file's. REJECTED too: a dispatch row naming the target's TIP, the
// listing comparing it with the branch's — every push to a busy target made
// every head waiting on it due again, a record each, for a file that seldom
// changes. The target's row is refreshed at the home's pace instead, one
// record a target, and the heads follow only a file that changed.
//
// # Additive, so the apply is a join
// # What a head owes, and the `checks_due` listing that says so
//
// A LIVE change of an ACTIVE repository, its target a live branch, owes its
// home ([CheckWork]) — internal/code/home's checks worker works it:
//
//   - its DISPATCH, while no dispatch of its head for its target names the
//     checks file its target's row holds. The worker first ends the change's
//     attempts still queued or running at a head it moved on from, or for a
//     target it was retargeted from — cancelled, superseded — but those a
//     live change into the attempt's target heads, which shares them; then it
//     queues every check the file at the target's tip defines that the head
//     holds no attempt of for its target, or records it skipped, a name the
//     target's merge gate requires marked required; a check whose latest
//     attempt there was errored at an earlier dispatch for a file that did
//     not run ([CheckReasonDefsInvalid]) is queued as its NEXT attempt, so a
//     target whose broken file is mended reaches the heads it errored; where
//     the file is none that runs, every name the gate requires the head
//     holds no attempt of is recorded errored defs_invalid
//     ([CheckQueued.Invalid]), a gate waiting on a check nobody can run
//     saying so on the change rather than in silence. The dispatch record is
//     the LAST of them.
//   - a REQUEUE, where the latest attempt of a check at its head for its
//     target ended errored for its infrastructure ([CheckReason.Requeued])
//     below [AutoRequeueAttempts]: the same check queued again as the next
//     attempt, under the definition it ran under.
//
// Each is taken off the list by the home's own records — the dispatch's row,
// the next attempt's row — and the listing asks both in SQL, over every live
// change: the dispatch by the two keys above, the requeue by
// code_checks_requeue_idx, which holds only the attempts their
// infrastructure ended. A name the target's gate requires that its file does
// not define leaves the gate waiting on it, and reaches the head once the
// file defines it, by the target's reading: REJECTED, a term listing a head
// while a required name lacks an attempt — a target whose rule requires a
// check its file has not defined yet kept every change into it its home's
// work at every pass, its file read again each time, for as long as the file
// stayed as it was.
//
//
// The subject carries no expectation, so a home's record and a failover's
// second one's land in either order. Each row keeps the record that read the
// LATER tip — the higher tip position — and of two that read one tip, the
// later record: a total order, so either order leaves one row byte for byte.
//
// # Their lifetime
//
// A dispatch row is one a head and a target — a few dozen bytes, beside the
// revision row its head already is, which the repository keeps as long — and
// a target row one a branch the repository's changes have targeted. Both go
// with the repository's purge.
//
// # Base format, as a stated condition
//
// The op and its payload are in the base format, under the condition the
// checks subject states (checks.go): once a build without them has run, the
// op takes {Name: "MutationRecord.Op check_dispatched", Since: 2, Path:
// []string{"op"}, Equals: "check_dispatched"} in versionedFields, with the
// companions the subject names.

// MaxDispatchHeads is the most heads one check_dispatched record names: 500,
// an eighth of MaxRecordRows — the share a check record's horizon takes — so
// a record's rows, one a head beside its target's, stay far inside what one
// record may touch; a home dispatching more heads of one target records them
// in more records.
const MaxDispatchHeads = MaxRecordRows / 8

// CheckDispatched records a repository's home's reading of a target branch's
// checks file at its tip and the change heads it dispatched under it
// ([OpCheckDispatched]): every check the file defines queued at each head for
// the target, or recorded skipped, BEFORE this record — so a home stopped
// between the two is listed the head again, and its queues are answered from
// their rows. With no heads it is a refresh of the target's reading alone.
// Every field is in the base format, under the condition the subject states.
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
	// Heads are the change heads dispatched under it, at most
	// [MaxDispatchHeads], none for a refresh.
	Heads []DispatchedHead `json:"heads,omitempty"`
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
	case len(d.Heads) > MaxDispatchHeads:
		return checkField("heads", "%d heads, past the %d one record names (code.MaxDispatchHeads)", len(d.Heads),
			MaxDispatchHeads)
	case d.DispatchedAt.IsZero():
		return checkField("dispatched_at", "a dispatch names the instant it was made")
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
	return nil
}

// decideDispatched is the checks writer's decide of a dispatch: answered from
// the rows, unchanged, where they hold it already — the target's reading at
// this tip or a later one naming this file, and every head's dispatch for the
// target at this tip or a later one naming it — and appended otherwise.
func decideDispatched(ctx context.Context, tx *sql.Tx, d CheckDispatched, out checksDecision) (checksDecision,
	error) {

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
		return checksDecision{}, fmt.Errorf("code: read target %s's checks file: %w", d.Target, err)
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
			return checksDecision{}, fmt.Errorf("code: read head %s's dispatch: %w", h.OID, err)
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
// (the file's head says how each is owed and what takes it off the list): the
// change named by the [ChangeDue] it rides. AN ESTATE ANSWER'S FIELD, NOT A
// RECORD'S, so it takes no versionedFields row; as a stated condition, as
// [ChangeDue.RefreshDue]'s: no build that carries the code domain has run
// without it.
type CheckWork struct {
	// Dispatch is a head no dispatch of which for its target names the
	// checks file its target's row holds.
	Dispatch bool `json:"dispatch,omitempty"`
	// Held is, for a head owing its dispatch, every check name an attempt of
	// which the head commit holds for its target, sorted: the checks its
	// dispatch queues nothing of — but a name whose latest attempt was
	// errored at its dispatch for a checks file that did not run, which is in
	// Invalid.
	Held []string `json:"held,omitempty"`
	// Invalid is, for a head owing its dispatch, each check whose latest
	// attempt at the head for its target was errored at its dispatch
	// ([CheckReasonDefsInvalid]), as that attempt's key: a dispatch under a
	// file defining the check in a form that runs queues the next attempt.
	Invalid []CheckKey `json:"invalid,omitempty"`
	// Required is, for a head owing its dispatch, every check name its
	// target's merge gate requires ([GateRules.Checks]), sorted.
	Required []string `json:"required,omitempty"`
	// Stale is, for a head owing its dispatch, the change's attempts still
	// queued or running at a head it moved on from, or for a target it was
	// retargeted from: each one its home cancels superseded before it
	// dispatches the head.
	Stale []CheckKey `json:"stale,omitempty"`
	// Requeue is each check of the head for its target whose latest attempt
	// its infrastructure ended below AutoRequeueAttempts, as the next attempt
	// its home queues: that attempt's queue, under the definition it ran
	// under, its reason requeue — all but the instant, which the home's is.
	Requeue []CheckQueued `json:"requeue,omitempty"`
}

// requiredChecks is a repository's rules as a `checks_due` page judges its
// changes' targets by: each target's required checks as ITS MERGE GATE reads
// them ([GateRulesOf]'s Checks) — one derivation, so a head's attempts are
// marked required by exactly the names its gate judges — a target judged
// once a page.
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
// gate refuses it: a requirement read as absent is a check the gate waits for
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

// dueCheckHeadsIn is a page of repoID's live changes owing their checks work,
// by source branch after cursor — read whole before any change's work is, so
// no other statement runs beside the listing's.
func dueCheckHeadsIn(ctx context.Context, tx *sql.Tx, repoID, cursor string, limit int) ([]checkHead, error) {
	rows, err := tx.QueryContext(ctx, dueCheckChangesSQL, repoID, cursor, limit, AutoRequeueAttempts)
	if err != nil {
		return nil, fmt.Errorf("code: list repository %s's heads due their checks: %w", repoID, err)
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
			return nil, fmt.Errorf("code: list repository %s's heads due their checks: %w", repoID, err)
		}
		h.due.TipPosition = unpacked(at)
		out = append(out, h)
	}
	return out, rows.Err()
}

// checkWorkOf is what h owes its checks: where its dispatch is owed, the
// names its head holds for its target and those an earlier dispatch errored,
// the names its target's gate requires and the attempts it moved on from;
// and where a requeue is, the next attempt of each check it is owed of.
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
	}
	if h.requeue {
		if w.Requeue, err = requeuesIn(ctx, tx, repoID, h.due.Head, h.due.TargetRef); err != nil {
			return CheckWork{}, err
		}
	}
	return w, nil
}

// headAttemptsIn is what commit oid holds of each check for target — a range
// of code_checks' key on the commit and the target, at most MaxChecksPerFile
// checks of MaxCheckAttempts attempts each — as a dispatch reads it: every
// name held, sorted, but those whose latest attempt was errored at its
// dispatch, which are answered as that attempt's key.
func headAttemptsIn(ctx context.Context, tx *sql.Tx, repoID, oid, target string) ([]string, []CheckKey, error) {
	rows, err := tx.QueryContext(ctx, headAttemptsSQL, repoID, oid, target)
	if err != nil {
		return nil, nil, fmt.Errorf("code: read the checks %s holds for %s: %w", oid, target, err)
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
			return nil, nil, fmt.Errorf("code: read the checks %s holds for %s: %w", oid, target, err)
		}
		if _, seen := last[name]; !seen {
			names = append(names, name)
		}
		last[name] = l
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("code: read the checks %s holds for %s: %w", oid, target, err)
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

// staleChecksIn is changeID's attempts still queued or running at a commit or
// for a target other than its head's — the head it moved on from, the target
// it was retargeted from — by code_checks_live_idx, which holds only a change
// head's attempts not yet ended; each kept where no live change into the
// attempt's target heads its commit, which shares it.
func staleChecksIn(ctx context.Context, tx *sql.Tx, repoID, changeID, head, target string) ([]CheckKey, error) {
	rows, err := tx.QueryContext(ctx, staleChecksSQL, changeID, head, target, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read the checks change %s moved on from: %w", changeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []CheckKey
	for rows.Next() {
		k := CheckKey{RepoID: repoID}
		if err := rows.Scan(&k.OID, &k.Target, &k.Name, &k.Attempt); err != nil {
			return nil, fmt.Errorf("code: read the checks change %s moved on from: %w", changeID, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// requeuesIn is the next attempt of each check of commit oid for target whose
// latest attempt its infrastructure ended below AutoRequeueAttempts: that
// attempt's queue, its definition among it, as a requeue.
func requeuesIn(ctx context.Context, tx *sql.Tx, repoID, oid, target string) ([]CheckQueued, error) {
	rows, err := tx.QueryContext(ctx, requeuesSQL, repoID, oid, target, AutoRequeueAttempts)
	if err != nil {
		return nil, fmt.Errorf("code: read the checks of %s for %s due a requeue: %w", oid, target, err)
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
			return nil, fmt.Errorf("code: read the checks of %s for %s due a requeue: %w", oid, target, err)
		}
		q.Attempt++
		q.Purpose, q.Revision, q.Reason = CheckPurpose(purpose), int(revision), QueueRequeue
		if def != "" {
			q.Def = &CheckDef{}
			if err := json.Unmarshal([]byte(def), q.Def); err != nil {
				return nil, fmt.Errorf("code: read check %s of %s's definition: %w", q.Name, oid, err)
			}
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// The work's statements, each registered in plan_test.go. A head's attempts
// for its target are a range of code_checks' key; the change's attempts not
// yet ended are code_checks_live_idx's (replicated 0086), its predicate spelled
// as the index declares it, each kept where no live change into its target
// heads its commit — code_changes_target_idx, whose predicate liveChanges
// spells; and a requeue reads code_checks_requeue_idx (replicated 0086), its
// predicate spelled as the index declares it (requeuedRows), then the next
// attempt's absence by code_checks' key.
const (
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
	// requeuedRows is code_checks_requeue_idx's predicate, spelled as the
	// index declares it, on the alias k: an attempt its infrastructure ended
	// ([CheckReason.Requeued]).
	requeuedRows = `k.state = 'errored' AND k.reason IN ('runner_lost', 'box_failed', 'setup_failed')`
)
