package accessdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MOVING THE COMMANDS ONTO THE KEYRING'S ACTIVE KEY. A keyring rotation adds a
// key, makes it active, re-seals what the old key sealed, and then drops it.
// These rows are derived from the log, so a node never rewrites its own: each
// command still sealed under another key is moved by a RECORD on its own
// subject ([OpResealed]), carrying the same text sealed afresh, which every
// data node applies.

// ResealReport is what one pass over the commands moved.
type ResealReport struct {
	// Commands is how many commands a record re-sealed.
	Commands int
	// Unknown is how many records nobody could confirm: run the pass
	// again, which re-seals what did not land and finds nothing to do for
	// what did.
	Unknown int
}

// errResealed is a command already under the active key, or one its retention
// took since it was listed: nothing to publish.
var errResealed = errors.New("accessdomain: the command is already under the active key")

// Reseal moves every command's text onto the keyring's active key, one record
// per command sealed under another, AS by.
//
// IT IS ITS OWN RETRY: each record is decided in its own snapshot against what
// the row holds now, so a pass run again re-seals only what is still under an
// old key. A command that fails does not stop the rest; every failure is
// returned beside the report of what moved.
func (w *Writer) Reseal(ctx context.Context, by iam.Actor, reader *Reader) (ResealReport, error) {
	var report ResealReport
	if reader == nil {
		return report, errors.New("accessdomain: a reseal needs the read side to find the commands")
	}
	commands, err := reader.sealedOutside(ctx, w.sealer.active)
	if err != nil {
		return report, err
	}
	var errs []error
	for _, cmd := range commands {
		if ctx.Err() != nil {
			return report, errors.Join(append(errs, ctx.Err())...)
		}
		res, perr := w.reseal(ctx, by, cmd.handle, cmd.id)
		switch {
		case errors.Is(perr, errResealed):
		case perr != nil:
			errs = append(errs, fmt.Errorf("accessdomain: reseal command %s: %w", cmd.id, perr))
		case res.Outcome == statelog.OutcomeUnknown:
			report.Unknown++
		default:
			report.Commands++
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

// sealedCommand names one command row.
type sealedCommand struct{ handle, id string }

// sealedOutside is every command whose text is sealed under a key other than
// active, in row order.
func (r *Reader) sealedOutside(ctx context.Context, active string) ([]sealedCommand, error) {
	var out []sealedCommand
	_, err := r.log.Read(ctx, stale.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT agent_handle, command_id FROM access_commands INDEXED BY access_commands_key_idx
			WHERE content_key != ? ORDER BY agent_handle, command_id`, active)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c sealedCommand
			if scanErr := rows.Scan(&c.handle, &c.id); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			out = append(out, c)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("accessdomain: find the commands under another key: %w", err)
	}
	return out, nil
}
