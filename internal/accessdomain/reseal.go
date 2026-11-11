package accessdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MOVING THE SEALED TEXTS ONTO THE KEYRING'S ACTIVE KEY. A keyring rotation
// adds a key, makes it active, re-seals what the old key sealed, and then drops
// it. These rows are derived from the log, so a node never rewrites its own:
// each command or verdict still sealed under another key is moved by a RECORD
// on its own subject ([OpResealed]), carrying the same texts sealed afresh,
// which every data node applies.

// ResealReport is what one pass over the commands and the verdicts moved.
type ResealReport struct {
	// Commands and Verdicts are how many of each a record re-sealed.
	Commands int
	Verdicts int
	// Unknown is how many records nobody could confirm: run the pass
	// again, which re-seals what did not land and finds nothing to do for
	// what did.
	Unknown int
}

// errResealed is a record already under the active key, or one its retention
// took since it was listed: nothing to publish.
var errResealed = errors.New("accessdomain: the record is already under the active key")

// Reseal moves every command's text and every verdict's texts onto the
// keyring's active key, one record per command or verdict sealed under
// another, AS by.
//
// IT IS ITS OWN RETRY: each record is decided in its own snapshot against what
// the row holds now, so a pass run again re-seals only what is still under an
// old key. A record that fails does not stop the rest; every failure is
// returned beside the report of what moved.
func (w *Writer) Reseal(ctx context.Context, by iam.Actor, reader *Reader) (ResealReport, error) {
	var report ResealReport
	if reader == nil {
		return report, errors.New("accessdomain: a reseal needs the read side to find the sealed records")
	}
	var errs []error
	for _, table := range []struct {
		kind   ObjectKind
		moved  *int
		reseal func(ctx context.Context, by iam.Actor, handle, id string) (statelog.Result, error)
	}{
		{KindCommand, &report.Commands, w.reseal},
		{KindVerdict, &report.Verdicts, w.resealVerdict},
	} {
		sealed, err := reader.sealedOutside(ctx, table.kind, w.sealer.active)
		if err != nil {
			return report, errors.Join(append(errs, err)...)
		}
		for _, rec := range sealed {
			if ctx.Err() != nil {
				return report, errors.Join(append(errs, ctx.Err())...)
			}
			res, perr := table.reseal(ctx, by, rec.handle, rec.id)
			switch {
			case errors.Is(perr, errResealed):
			case perr != nil:
				errs = append(errs, fmt.Errorf("accessdomain: reseal %s %s: %w", table.kind, rec.id, perr))
			case res.Outcome == statelog.OutcomeUnknown:
				report.Unknown++
			default:
				*table.moved++
			}
		}
	}
	return report, errors.Join(errs...)
}

// reseal publishes one command's text sealed afresh, decided against its row.
func (w *Writer) reseal(ctx context.Context, by iam.Actor, handle, id string) (statelog.Result, error) {
	subject, scope := CommandSubject(handle, id), ScopeSet{Subject: true}
	opID := statelog.NewOpID(w.now(), "access-reseal")
	return publish(ctx, w.publisher, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			var sealed, key string
			err := tx.QueryRowContext(ctx, `
				SELECT command_sealed, content_key FROM access_commands
				WHERE agent_handle = ? AND command_id = ?`, handle, id).Scan(&sealed, &key)
			switch {
			case errors.Is(err, sql.ErrNoRows), err == nil && key == w.sealer.active:
				return statelog.Decision{}, errResealed
			case err != nil:
				return statelog.Decision{}, err
			}
			text, err := w.sealer.Open(sealed, AAD(handle, id))
			if err != nil {
				return statelog.Decision{}, err
			}
			next, err := w.sealer.SealCommand(handle, id, text)
			if err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, by, subject, OpResealed, scope, opID, Resealed{
				V: baseRecordVersion, ID: id, Handle: handle,
				CommandSealed: next.Sealed, ContentKey: next.Key,
			}, w.now())
		}))
}

// resealVerdict publishes one verdict's two texts sealed afresh, decided
// against its row, in one record.
func (w *Writer) resealVerdict(ctx context.Context, by iam.Actor, handle, id string) (statelog.Result, error) {
	subject, scope := verdictSubjectOf(handle, id), ScopeSet{Subject: true}
	opID := statelog.NewOpID(w.now(), "access-reseal")
	return publish(ctx, w.publisher, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			var subjectSealed, wordsSealed, key string
			err := tx.QueryRowContext(ctx, `
				SELECT subject_sealed, words_sealed, content_key FROM access_verdicts
				WHERE agent_handle = ? AND verdict_id = ?`, handle, id).Scan(&subjectSealed, &wordsSealed, &key)
			switch {
			case errors.Is(err, sql.ErrNoRows), err == nil && key == w.sealer.active:
				return statelog.Decision{}, errResealed
			case err != nil:
				return statelog.Decision{}, err
			}
			text, reason, findings, err := w.sealer.openVerdict(handle, id, subjectSealed, wordsSealed)
			if err != nil {
				return statelog.Decision{}, err
			}
			next, err := w.sealer.SealVerdict(handle, id, text, reason, findings)
			if err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, by, subject, OpResealed, scope, opID, VerdictResealed{
				V: baseRecordVersion, ID: id, Handle: handle,
				SubjectSealed: next.Subject, WordsSealed: next.Words, ContentKey: next.Key,
			}, w.now())
		}))
}

// sealedRecord names one sealed row.
type sealedRecord struct{ handle, id string }

// sealedOutsideStatements find a sealed table's rows under a key other than
// the one asked, in row order.
var sealedOutsideStatements = map[ObjectKind]string{
	KindCommand: `SELECT agent_handle, command_id FROM access_commands INDEXED BY access_commands_key_idx
		WHERE content_key != ? ORDER BY agent_handle, command_id`,
	KindVerdict: `SELECT agent_handle, verdict_id FROM access_verdicts INDEXED BY access_verdicts_key_idx
		WHERE content_key != ? ORDER BY agent_handle, verdict_id`,
}

// sealedOutside is every command or verdict, as kind says, whose texts are
// sealed under a key other than active, in row order.
func (r *Reader) sealedOutside(ctx context.Context, kind ObjectKind, active string) ([]sealedRecord, error) {
	statement, ok := sealedOutsideStatements[kind]
	if !ok {
		return nil, fmt.Errorf("accessdomain: a %s record seals nothing", kind)
	}
	var out []sealedRecord
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, statement, active)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c sealedRecord
			if scanErr := rows.Scan(&c.handle, &c.id); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out = append(out, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("accessdomain: find the %ss under another key: %w", kind, err)
	}
	return out, nil
}
