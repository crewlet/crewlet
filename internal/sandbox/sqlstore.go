package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// SQLStore is the durable pending-run store.
type SQLStore struct{ db *store.DB }

// NewSQLStore builds a store over a database.
func NewSQLStore(db *store.DB) *SQLStore { return &SQLStore{db: db} }

var _ PendingStore = (*SQLStore)(nil)

// columns is every field a read selects, in one place so the scan below cannot
// drift from it — a mis-ordered scan on a table this wide fails as a type
// error at best and silently swaps two strings at worst.
const columns = `turn_id, agent_handle, agent_id, role, sandbox_id, coding_agent,
	command_id, status, owner, owner_epoch, plan, task_description,
	success_criteria, conversation_key, notification_metadata, branch,
	session_id, question, audience, trace_id, span_id, budget_remaining,
	delegation_depth, delegation_chain, execute_state, pause_ttl_seconds,
	paused_at, created_at, updated_at`

const insertSQL = `
INSERT INTO pending_sandbox_run (` + columns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (turn_id) DO NOTHING`

// Create persists a new row, idempotently.
//
// DO NOTHING rather than an error: the duplicate is expected. A kick-off turn
// redelivered after its ack was lost presents the same turn id, and raising
// would turn a recoverable redelivery into a failed turn — while the row that
// is already there is the correct one, possibly with a box attached to it.
func (s *SQLStore) Create(ctx context.Context, run PendingRun) error {
	if run.TurnID == "" {
		return fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	if run.Status == "" {
		run.Status = StatusRunning
	}
	now := time.Now().UTC()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now

	_, err := s.db.SQL().ExecContext(ctx, insertSQL,
		run.TurnID, run.AgentHandle, run.AgentID, run.Role, run.SandboxID,
		run.CodingAgent, run.CommandID, run.Status, nullString(run.Owner),
		nullInt(run.OwnerEpoch),
		encodeMap(run.Plan), run.TaskDescription, encodeList(run.SuccessCriteria),
		run.ConversationKey, encodeMap(run.NotificationMetadata), run.Branch,
		run.SessionID, run.Question, run.Audience, run.TraceID, run.SpanID,
		run.BudgetRemaining, run.DelegationDepth, encodeList(run.DelegationChain),
		encodeMap(run.ExecuteState), run.PauseTTLSeconds,
		nullTime(run.PausedAt), store.EncodeTime(run.CreatedAt),
		store.EncodeTime(run.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sandbox: create pending run %s: %w", run.TurnID, err)
	}
	return nil
}

// Get reads one run.
func (s *SQLStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	row := s.db.SQL().QueryRowContext(ctx,
		`SELECT `+columns+` FROM pending_sandbox_run WHERE turn_id = ?`, turnID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingRun{}, false, nil
	}
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: get pending run %s: %w", turnID, err)
	}
	return run, true, nil
}

// claimSQL is THE at-most-once tail guard.
//
// One conditional UPDATE, so a duplicate completion signal — or a restart
// followed by a resume — runs the tail exactly once. Two nodes both splicing a
// result into the same suspended loop would produce two turns from one job,
// which the seat sees as its own work arriving twice.
//
// No RETURNING: outside the intersection of the two certified drivers
// (decisions/002). The outcome is RowsAffected and the row is read
// back inside the same transaction.
var claimSQL = `
UPDATE pending_sandbox_run
SET status = '` + StatusResumed + `', updated_at = ?
WHERE turn_id = ? AND status IN (` + placeholders(len(Claimable)) + `)`

// ClaimForResume flips a claimable run to resumed, reporting whether this call
// won.
func (s *SQLStore) ClaimForResume(ctx context.Context, turnID string) (PendingRun, bool, error) {
	var (
		out PendingRun
		won bool
	)
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		// The PRE-flip status, read inside the transaction so it is the one
		// the update is about to change. A failed dispatch reverts to
		// exactly this; inferring it afterwards is unsound, because a
		// reused run keeps its old question.
		before, err := statusIn(ctx, tx, turnID)
		if err != nil {
			return err
		}
		args := append([]any{store.EncodeTime(time.Now().UTC()), turnID}, asAny(Claimable)...)
		res, err := tx.ExecContext(ctx, claimSQL, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		row := tx.QueryRowContext(ctx,
			`SELECT `+columns+` FROM pending_sandbox_run WHERE turn_id = ?`, turnID)
		out, err = scanRun(row)
		if err != nil {
			return err
		}
		out.ClaimedFrom = before
		won = true
		return nil
	})
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: claim %s: %w", turnID, err)
	}
	return out, won, nil
}

func statusIn(ctx context.Context, tx *sql.Tx, turnID string) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM pending_sandbox_run WHERE turn_id = ?`, turnID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, err
}

// MarkAwaiting parks a run on a question.
//
// The BRANCH is written with it, and that ordering is the point: the WIP is
// pushed before the question is asked, so a snapshot reaped days later loses
// nothing a re-seed cannot recover. A question parked over unpushed work is a
// question whose answer arrives to an empty box.
func (s *SQLStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	_, err := s.db.SQL().ExecContext(ctx, `
UPDATE pending_sandbox_run
SET status = ?, question = ?, audience = ?, branch = ?, session_id = ?, updated_at = ?
WHERE turn_id = ?`,
		StatusAwaiting, q.Question, q.Audience, q.Branch, q.SessionID,
		store.EncodeTime(time.Now().UTC()), turnID)
	if err != nil {
		return fmt.Errorf("sandbox: park %s on a question: %w", turnID, err)
	}
	return nil
}

// ClaimOwnership takes a run for a node.
//
// A run whose epoch is already HIGHER is not stolen: that node's lease is
// newer, and taking the run from it would put two engines on one box. Equal
// epochs are allowed through, which is a node re-claiming its own run after a
// restart within one lease.
func (s *SQLStore) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	res, err := s.db.SQL().ExecContext(ctx, `
UPDATE pending_sandbox_run
SET owner = ?, owner_epoch = ?, updated_at = ?
WHERE turn_id = ? AND COALESCE(owner_epoch, 0) <= ?`,
		owner, epoch, store.EncodeTime(time.Now().UTC()), turnID, epoch)
	if err != nil {
		return false, fmt.Errorf("sandbox: claim ownership of %s: %w", turnID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sandbox: claim ownership of %s: %w", turnID, err)
	}
	return n > 0, nil
}

// SetStatus moves a run, fenced.
func (s *SQLStore) SetStatus(ctx context.Context, turnID, status string, fence Fence) error {
	if !slices.Contains(allStatuses, status) {
		return fmt.Errorf("sandbox: unknown status %q", status)
	}
	query := `UPDATE pending_sandbox_run SET status = ?, updated_at = ? WHERE turn_id = ?`
	args := []any{status, store.EncodeTime(time.Now().UTC()), turnID}
	if fence.Fenced() {
		query += ` AND COALESCE(owner_epoch, 0) <= ?`
		args = append(args, fence.Epoch)
	}
	if _, err := s.db.SQL().ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("sandbox: set %s to %s: %w", turnID, status, err)
	}
	return nil
}

// ExpirePause flips a parked run to reseed and clears its box, if it is still
// parked.
//
// Unfenced on purpose: the reaper runs on whichever node holds the waiter
// duty, which is rarely the node that owns the seat, so an epoch check here
// would refuse every legitimate reap. The status predicate IS the exclusion —
// exactly one caller can move a row out of StatusAwaiting.
func (s *SQLStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	result, err := s.db.SQL().ExecContext(ctx,
		`UPDATE pending_sandbox_run
		 SET status = ?, sandbox_id = '', command_id = '', paused_at = NULL, updated_at = ?
		 WHERE turn_id = ? AND status = ?`,
		StatusReseed, store.EncodeTime(time.Now().UTC()), turnID, StatusAwaiting)
	if err != nil {
		return false, fmt.Errorf("sandbox: expire pause on %s: %w", turnID, err)
	}
	// RowsAffected rather than RETURNING: RETURNING is outside the
	// intersection of the two certified drivers (d-002).
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sandbox: expire pause on %s: %w", turnID, err)
	}
	return affected == 1, nil
}

var allStatuses = []string{
	StatusRunning, StatusAwaiting, StatusResumed, StatusDone, StatusFailed, StatusReseed,
}

// AttachSandbox records the box and command a run is using.
//
// Written as soon as the box exists, BEFORE the job starts. A crash in between
// then leaves a row naming a box nobody is using, which recovery can kill — the
// reverse ordering leaves a box nothing names, which nothing can ever reclaim.
func (s *SQLStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error {
	query := `
UPDATE pending_sandbox_run
SET sandbox_id = ?, command_id = ?, coding_agent = ?, session_id = ?,
    pause_ttl_seconds = ?, updated_at = ?
WHERE turn_id = ?`
	args := []any{
		box.SandboxID, box.CommandID, box.CodingAgent, box.SessionID,
		box.PauseTTLSec, store.EncodeTime(time.Now().UTC()), turnID,
	}
	if fence.Fenced() {
		query += ` AND COALESCE(owner_epoch, 0) <= ?`
		args = append(args, fence.Epoch)
	}
	if _, err := s.db.SQL().ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("sandbox: attach box to %s: %w", turnID, err)
	}
	return nil
}

// MarkBoxPaused records that this run's box is snapshotted.
func (s *SQLStore) MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE pending_sandbox_run SET paused_at = ?, updated_at = ? WHERE turn_id = ?`,
		nullTime(at), store.EncodeTime(time.Now().UTC()), turnID)
	if err != nil {
		return fmt.Errorf("sandbox: mark %s paused: %w", turnID, err)
	}
	return nil
}

// ReleaseBox clears the box record: no box, not paused.
//
// BOTH FIELDS TOGETHER. Clearing only the id would leave a paused_at pointing
// at nothing, which the reaper reads as a snapshot to reclaim and then cannot
// find — a warning every tick, for ever.
func (s *SQLStore) ReleaseBox(ctx context.Context, turnID string) error {
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE pending_sandbox_run SET sandbox_id = '', command_id = '', paused_at = NULL,
		 updated_at = ? WHERE turn_id = ?`,
		store.EncodeTime(time.Now().UTC()), turnID)
	if err != nil {
		return fmt.Errorf("sandbox: release %s's box: %w", turnID, err)
	}
	return nil
}

// SaveExecuteState persists the suspended conversation.
func (s *SQLStore) SaveExecuteState(ctx context.Context, turnID string, state map[string]any) error {
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE pending_sandbox_run SET execute_state = ?, updated_at = ? WHERE turn_id = ?`,
		encodeMap(state), store.EncodeTime(time.Now().UTC()), turnID)
	if err != nil {
		return fmt.Errorf("sandbox: save %s's execute state: %w", turnID, err)
	}
	return nil
}

// ListActive returns every run that still owns engine-side state.
func (s *SQLStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	return s.query(ctx,
		`SELECT `+columns+` FROM pending_sandbox_run
		 WHERE status IN (`+placeholders(len(Active))+`) ORDER BY created_at, turn_id`,
		asAny(Active)...)
}

// ListActiveForSeat is the "is this seat busy?" read.
func (s *SQLStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	args := append([]any{handle}, asAny(Active)...)
	return s.query(ctx,
		`SELECT `+columns+` FROM pending_sandbox_run
		 WHERE agent_handle = ? AND status IN (`+placeholders(len(Active))+`)
		 ORDER BY created_at, turn_id`, args...)
}

// FindAwaitingByConversation matches a person's answer back to the run that
// asked it.
//
// NEWEST FIRST: a seat can have parked more than one question on one thread,
// and the answer belongs to the most recent — the person is replying to what
// they were just asked.
func (s *SQLStore) FindAwaitingByConversation(ctx context.Context, handle, conversation string) (PendingRun, bool, error) {
	if conversation == "" {
		// No conversation means nothing to match on, and matching by seat
		// alone would hand an unrelated message to whichever run happened
		// to be waiting.
		return PendingRun{}, false, nil
	}
	args := append([]any{handle, conversation}, asAny(Awaiting)...)
	runs, err := s.query(ctx,
		`SELECT `+columns+` FROM pending_sandbox_run
		 WHERE agent_handle = ? AND conversation_key = ?
		   AND status IN (`+placeholders(len(Awaiting))+`)
		 ORDER BY created_at DESC, turn_id DESC LIMIT 1`, args...)
	if err != nil || len(runs) == 0 {
		return PendingRun{}, false, err
	}
	return runs[0], true, nil
}

// ListPausedBefore returns snapshots older than the cutoff, for the reaper.
func (s *SQLStore) ListPausedBefore(ctx context.Context, cutoff time.Time) ([]PendingRun, error) {
	return s.query(ctx,
		`SELECT `+columns+` FROM pending_sandbox_run
		 WHERE paused_at IS NOT NULL AND paused_at < ? AND sandbox_id <> ''
		 ORDER BY paused_at`, store.EncodeTime(cutoff))
}

// Delete removes a run.
func (s *SQLStore) Delete(ctx context.Context, turnID string) error {
	if _, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM pending_sandbox_run WHERE turn_id = ?`, turnID); err != nil {
		return fmt.Errorf("sandbox: delete %s: %w", turnID, err)
	}
	return nil
}

func (s *SQLStore) query(ctx context.Context, q string, args ...any) ([]PendingRun, error) {
	rows, err := s.db.SQL().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list pending runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("sandbox: list pending runs: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sandbox: list pending runs: %w", err)
	}
	return out, nil
}

// scanner is what a row and a *sql.Row have in common.
type scanner interface{ Scan(dest ...any) error }

func scanRun(row scanner) (PendingRun, error) {
	var (
		r                                      PendingRun
		plan, criteria, meta, chain, execState string
		createdAt, updatedAt                   int64
		// NULLABLE, and every one of them means the same thing: the fact
		// is not yet established. Unclaimed, unfenced, not paused. A
		// zero would be a claim by node "" under epoch 0, and a paused_at
		// of 0 lands in 1970 — a snapshot the reaper is permanently
		// overdue on.
		owner      sql.NullString
		ownerEpoch sql.NullInt64
		pausedAt   sql.NullInt64
	)
	err := row.Scan(
		&r.TurnID, &r.AgentHandle, &r.AgentID, &r.Role, &r.SandboxID,
		&r.CodingAgent, &r.CommandID, &r.Status, &owner, &ownerEpoch,
		&plan, &r.TaskDescription, &criteria, &r.ConversationKey, &meta,
		&r.Branch, &r.SessionID, &r.Question, &r.Audience, &r.TraceID,
		&r.SpanID, &r.BudgetRemaining, &r.DelegationDepth, &chain,
		&execState, &r.PauseTTLSeconds, &pausedAt, &createdAt, &updatedAt)
	if err != nil {
		return PendingRun{}, err
	}
	r.Owner, r.OwnerEpoch = owner.String, ownerEpoch.Int64
	if pausedAt.Valid && pausedAt.Int64 > 0 {
		r.PausedAt = store.DecodeTime(pausedAt.Int64)
	}
	r.Plan = decodeMap(plan)
	r.SuccessCriteria = decodeList(criteria)
	r.NotificationMetadata = decodeMap(meta)
	r.DelegationChain = decodeList(chain)
	r.ExecuteState = decodeMap(execState)
	r.CreatedAt = store.DecodeTime(createdAt)
	r.UpdatedAt = store.DecodeTime(updatedAt)
	return r, nil
}

// The JSON columns.
//
// Encoded to "{}" and "[]" rather than to null, so a reader never has to tell
// an absent value from a null one — the two mean the same thing here and one
// spelling is enough.

func encodeMap(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func encodeList(l []string) string {
	if len(l) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func decodeMap(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

func decodeList(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// The three nullable columns, written as NULL when the fact is not yet
// established rather than as a zero — a zero owner is a claim by nobody, and a
// zero paused_at is a snapshot from 1970 that the reaper reads as overdue.

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return store.EncodeTime(t)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func asAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
