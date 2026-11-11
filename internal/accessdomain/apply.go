package accessdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier is a PURE FUNCTION of the log: it reads no config, clock,
// keyring or other domain; every instant it writes is the broker's; a record
// the rows no longer admit writes nothing rather than failing; and every write
// is guarded so a redelivery writes nothing it has not written.

// Applier writes this node's copy of the access estate. It holds nothing.
type Applier struct{}

// NewApplier builds the access applier.
func NewApplier() *Applier { return &Applier{} }

// Committed is the post-commit half, and there is nothing to tell: a seat
// reads what it was told at its next dispatch.
func (*Applier) Committed(context.Context) {}

// Gated reports a record that must produce no rows: one its writer published
// inside that writer's eviction window, the one apply gate on this log.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer == "" {
		return "", false, nil
	}
	evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
	if err != nil {
		return "", false, err
	}
	if evicted {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}

// evictedAt reports whether a record at packed, written by writer, falls in
// writer's eviction window. THE WINDOW IS HALF-OPEN AT BOTH ENDS: a record at
// or below the eviction's own position was written while the node was counted,
// and one at or above a readmission by a node the fleet took back.
func evictedAt(ctx context.Context, tx *sql.Tx, writer string, packed int64) (bool, error) {
	var from, readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM access_evictions
		WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("accessdomain: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	evicted := from.Valid && packed > from.Int64
	back := readmitted.Valid && packed >= readmitted.Int64
	return evicted && !back, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position
	packed   int64

	// brokerAt is the broker's own instant for this record, the only instant
	// a row records of its own.
	brokerAt time.Time
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows, dispatched on the subject kind.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt}
	payload, err := DecodeMutation(record)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: the record at %s: %w", rec.Position, err)
	}
	switch record.Subject.Kind {
	case KindBarrier:
		return 0, nil
	case KindTrimmed:
		// The framework removes this log's anchors below the floor in this
		// same transaction (ADR-0080); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return applyEviction(ctx, tx, c, payload)
	case KindGeneration:
		return applyGeneration(ctx, tx, c, payload)
	case KindTold:
		return applyTold(ctx, tx, c, payload)
	case KindCertificate:
		return applyCertificate(ctx, tx, c, payload)
	case KindCommand:
		if record.Op == OpResealed {
			return applyResealed(ctx, tx, c, payload)
		}
		return applyCommand(ctx, tx, c, payload)
	case KindVerdict:
		if record.Op == OpResealed {
			return applyVerdictResealed(ctx, tx, c, payload)
		}
		return applyVerdict(ctx, tx, c, payload)
	case KindSeat:
		return applyForget(ctx, tx, c, payload)
	case KindExpiry:
		return applyExpire(ctx, tx, c, payload)
	}
	// A kind this build does not know at a version it reads is a writer
	// publishing a kind it never declared; failing makes that visible.
	return 0, fmt.Errorf("accessdomain: %s is not a kind this build applies, and the record "+
		"at %s claims version %d", record.Subject.Kind, rec.Position, record.V)
}

// ownedKey is a seat-owned record's key, read off its subject and held to the
// payload's own: the writer builds both from one value.
func ownedKey(c applyContext, handle, id string) error {
	h, object, ok := splitOwned(c.record.Subject.ID)
	if !ok || h != handle || object != id {
		return fmt.Errorf("accessdomain: the %s record at %s is on %s and names %s/%s",
			c.record.Op, c.position, c.record.Subject, handle, id)
	}
	return nil
}

// applyTold writes one turn's told record and makes it the seat's baseline,
// unless a told record landed after the read its turn was diffed from: that
// one is newer than what this turn read. One decided before a removal of its
// seat it did not see writes nothing.
func applyTold(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	t, ok := payload.(Told)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the told record at %s carries a %T", c.position, payload)
	}
	if err := ownedKey(c, t.Handle, t.Turn); err != nil {
		return 0, err
	}
	removed, err := removalOf(ctx, tx, t.Handle)
	if err != nil {
		return 0, err
	}
	if removed > t.SeenRemoval {
		return 0, nil
	}
	authors, err := json.Marshal(t.Authors)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: encode the authors at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO access_told (agent_handle, turn_id, digest, snapshot, changes, baseline,
			revision_from, revision_to, authors, latest, at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT (agent_handle, turn_id) DO NOTHING`,
		t.Handle, t.Turn, t.Digest, string(t.Snapshot), string(t.Changes), boolInt(t.Baseline),
		t.RevisionFrom, t.RevisionTo, string(authors), c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record what %s's turn %s was told at %s: %w",
			t.Handle, t.Turn, c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	// THE NEWEST READ IS THE BASELINE, decided in log order on every node.
	var current int64
	err = tx.QueryRowContext(ctx, `
		SELECT position FROM access_told INDEXED BY access_told_latest_idx
		WHERE agent_handle = ? AND latest = 1`, t.Handle).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, fmt.Errorf("accessdomain: read %s's baseline at %s: %w", t.Handle, c.position, err)
	case current > t.ReadThrough:
		return written, nil
	}
	moved, err := tx.ExecContext(ctx, `
		UPDATE access_told SET latest = CASE WHEN turn_id = ? THEN 1 ELSE 0 END
		WHERE agent_handle = ? AND (latest = 1 OR turn_id = ?)`, t.Turn, t.Handle, t.Turn)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: move %s's baseline at %s: %w", t.Handle, c.position, err)
	}
	return written + affected(moved), nil
}

// removalOf is the position a seat's told records were last forgotten at, zero
// for none.
func removalOf(ctx context.Context, tx *sql.Tx, handle string) (int64, error) {
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT at_seq FROM access_seat_removals WHERE agent_handle = ?`,
		handle).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("accessdomain: read %s's removal: %w", handle, err)
	}
	return at, nil
}

// applyCertificate writes one certificate's row, create-only.
func applyCertificate(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	cert, ok := payload.(Certificate)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the certificate record at %s carries a %T", c.position, payload)
	}
	serial := SerialString(cert.Serial)
	if err := ownedKey(c, cert.Handle, serial); err != nil {
		return 0, err
	}
	hosts, err := json.Marshal(nonNil(cert.HostIDs))
	if err != nil {
		return 0, fmt.Errorf("accessdomain: encode the hosts at %s: %w", c.position, err)
	}
	principals, err := json.Marshal(nonNil(cert.Principals))
	if err != nil {
		return 0, fmt.Errorf("accessdomain: encode the principals at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO access_certificates (agent_handle, serial, key_id, turn_id, held_call,
			purpose, host_ids, principals, valid_after, valid_before, public_key_sha256,
			ca_sha256, node, at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, serial) DO NOTHING`,
		cert.Handle, serial, cert.KeyID, cert.Turn, cert.HeldCall, string(cert.Purpose),
		string(hosts), string(principals), store.EncodeTime(cert.ValidAfter),
		store.EncodeTime(cert.ValidBefore), cert.PublicKeySHA256, cert.CASHA256, cert.Node,
		c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record certificate %s at %s: %w", serial, c.position, err)
	}
	return affected(res), nil
}

// applyCommand writes one command's row, create-only, its text as sealed.
func applyCommand(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	cmd, ok := payload.(Command)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the command record at %s carries a %T", c.position, payload)
	}
	if err := ownedKey(c, cmd.Handle, cmd.ID); err != nil {
		return 0, err
	}
	serial := ""
	if cmd.CertificateSerial != 0 {
		serial = SerialString(cmd.CertificateSerial)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO access_commands (agent_handle, command_id, turn_id, held_call, host,
			login_user, level, command_sealed, content_key, stdin_bytes, stdin_sha256, ran,
			exit_code, duration_ms, stdout_bytes, stderr_bytes, output_sha256, truncated,
			approver, decision, certificate_serial, failure, at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, command_id) DO NOTHING`,
		cmd.Handle, cmd.ID, cmd.Turn, cmd.HeldCall, cmd.Host, cmd.User, string(cmd.Level),
		cmd.CommandSealed, cmd.ContentKey, cmd.StdinBytes, cmd.StdinSHA256, boolInt(cmd.Ran),
		cmd.ExitCode, cmd.DurationMS, cmd.StdoutBytes, cmd.StderrBytes, cmd.OutputSHA256,
		boolInt(cmd.Truncated), cmd.Approver, string(cmd.Decision), serial, string(cmd.Failure),
		c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record command %s at %s: %w", cmd.ID, c.position, err)
	}
	return affected(res), nil
}

// applyResealed writes a command's text sealed afresh over its row. A command
// its retention already took writes nothing, and a redelivery nothing new.
func applyResealed(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	r, ok := payload.(Resealed)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the reseal record at %s carries a %T", c.position, payload)
	}
	if err := ownedKey(c, r.Handle, r.ID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE access_commands SET command_sealed = ?, content_key = ?
		WHERE agent_handle = ? AND command_id = ? AND (command_sealed != ? OR content_key != ?)`,
		r.CommandSealed, r.ContentKey, r.Handle, r.ID, r.CommandSealed, r.ContentKey)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: reseal command %s at %s: %w", r.ID, c.position, err)
	}
	return affected(res), nil
}

// applyVerdict writes one giver's verdict's row, create-only, its texts as
// sealed. The record's id is its request's and giver's, held here as the
// subject is, so one giver never holds two rows for one request.
func applyVerdict(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	v, ok := payload.(Verdict)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the verdict record at %s carries a %T", c.position, payload)
	}
	if err := ownedKey(c, v.Handle, v.ID); err != nil {
		return 0, err
	}
	if v.ID != VerdictID(v.Request, v.By) {
		return 0, fmt.Errorf("accessdomain: the verdict record at %s is %s and names request %s by %q",
			c.position, v.ID, v.Request, v.By)
	}
	overruled, err := json.Marshal(nonNil(v.Overruled))
	if err != nil {
		return 0, fmt.Errorf("accessdomain: encode the overrulings at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO access_verdicts (agent_handle, verdict_id, request_id, request_kind, rule_number,
			turn_id, by_handle, by_kind, through, asked, confidence, verdict, final, overruled,
			sampled, digest, host, login_user, stdin_bytes, stdin_sha256, decision_id, item,
			subject_sealed, words_sealed, content_key, at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, verdict_id) DO NOTHING`,
		v.Handle, v.ID, v.Request, string(v.Kind), v.Rule, v.Turn, v.By, string(v.ByKind),
		string(v.Through), string(v.Asked), string(v.Confidence), string(v.Verdict), boolInt(v.Final),
		string(overruled), boolInt(v.Sampled), v.Digest, v.Host, v.User, v.StdinBytes, v.StdinSHA256,
		v.Decision, v.Item, v.SubjectSealed, v.WordsSealed, v.ContentKey, c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record the verdict on %s by %q at %s: %w",
			v.Request, v.By, c.position, err)
	}
	return affected(res), nil
}

// applyVerdictResealed writes a verdict's two texts sealed afresh over its
// row, as [applyResealed] does a command's.
func applyVerdictResealed(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	r, ok := payload.(VerdictResealed)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the verdict reseal record at %s carries a %T", c.position, payload)
	}
	if err := ownedKey(c, r.Handle, r.ID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE access_verdicts SET subject_sealed = ?, words_sealed = ?, content_key = ?
		WHERE agent_handle = ? AND verdict_id = ?
		  AND (subject_sealed != ? OR words_sealed != ? OR content_key != ?)`,
		r.SubjectSealed, r.WordsSealed, r.ContentKey, r.Handle, r.ID,
		r.SubjectSealed, r.WordsSealed, r.ContentKey)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: reseal verdict %s at %s: %w", r.ID, c.position, err)
	}
	return affected(res), nil
}

// applyForget removes what a removed seat was told and records the removal,
// which a told record decided before it is held to. Its certificates,
// commands and verdicts are the audit trail and stay to their retention.
func applyForget(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	f, ok := payload.(Forget)
	if !ok || f.Handle == "" || f.Handle != c.record.Subject.ID {
		return 0, fmt.Errorf("accessdomain: the removal at %s does not name its own seat", c.position)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM access_told WHERE agent_handle = ?`, f.Handle)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: forget what %s was told at %s: %w", f.Handle, c.position, err)
	}
	marked, err := tx.ExecContext(ctx, `
		INSERT INTO access_seat_removals (agent_handle, at_seq, at) VALUES (?, ?, ?)
		ON CONFLICT (agent_handle) DO UPDATE SET at_seq = excluded.at_seq, at = excluded.at
		WHERE excluded.at_seq > access_seat_removals.at_seq`, f.Handle, c.packed, c.at())
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record %s's removal at %s: %w", f.Handle, c.position, err)
	}
	return affected(res) + affected(marked), nil
}

// expiryStatements are what an expiry deletes, oldest first, each by its broker
// instant: every told record but a seat's baseline, every removal, certificate,
// command and verdict.
var expiryStatements = []string{
	`DELETE FROM access_told WHERE latest = 0 AND at < ?`,
	`DELETE FROM access_seat_removals WHERE at < ?`,
	`DELETE FROM access_certificates WHERE at < ?`,
	`DELETE FROM access_commands WHERE at < ?`,
	`DELETE FROM access_verdicts WHERE at < ?`,
}

// applyExpire removes every record past the retention through the hour the
// record names.
func applyExpire(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Expire)
	if !ok || e.Through.IsZero() {
		return 0, fmt.Errorf("accessdomain: the expiry at %s names no instant", c.position)
	}
	through := store.EncodeTime(e.Through)
	written := 0
	for _, statement := range expiryStatements {
		res, err := tx.ExecContext(ctx, statement, through)
		if err != nil {
			return 0, fmt.Errorf("accessdomain: expire through %s at %s: %w", e.Through, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func applyEviction(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("accessdomain: the eviction at %s names no node", c.position)
	}
	var (
		res sql.Result
		err error
	)
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE access_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO access_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > access_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("accessdomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	return affected(res), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("accessdomain: the generation record at %s carries "+
			"a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO access_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("accessdomain: record generation %d at %s: %w",
			g.Generation, c.position, err)
	}
	return affected(res), nil
}

// affected is how many rows a statement wrote, the row budget's input.
func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

// boolInt is a flag as a row stores it.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nonNil is a list that encodes as an array, never as null.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
