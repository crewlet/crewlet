package assistantdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// applyTurn applies a record on a turn's subject: its moves, and the messages,
// steps and cards the person saw during it.
func applyTurn(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	conversation, turn, ok := splitChild(c.record.Subject.ID)
	if !ok {
		return 0, nil
	}
	if _, found, err := readConversation(ctx, tx, conversation); err != nil || !found {
		return 0, err
	}
	var (
		written int
		err     error
	)
	switch p := payload.(type) {
	case TurnParked:
		written, err = applyParked(ctx, tx, c, conversation, turn, p)
	case TurnResumed:
		written, err = moveTurn(ctx, tx, c, conversation, turn,
			[]wire.TurnState{wire.TurnWaitingForApproval}, wire.TurnRunning,
			`node = ?`, p.Node)
	case TurnClosed:
		written, err = applyClosed(ctx, tx, c, conversation, turn, p)
	case MessageRecorded:
		written, err = applyMessage(ctx, tx, c, conversation, turn, p)
	case StepRecorded:
		written, err = applyStep(ctx, tx, c, conversation, turn, p)
	case CardRecorded:
		written, err = applyCard(ctx, tx, c, conversation, turn, p)
	default:
		return 0, fmt.Errorf("assistantdomain: the %s record at %s carries a %T",
			c.record.Op, c.position, payload)
	}
	if err != nil || written == 0 {
		return written, err
	}
	touched, err := touch(ctx, tx, c, conversation)
	return written + touched, err
}

// turnState is a turn's state in this conversation, and false where it has no
// row there.
func turnState(ctx context.Context, tx *sql.Tx, conversation, turn string) (wire.TurnState, bool, error) {
	var state string
	err := tx.QueryRowContext(ctx, `
		SELECT state FROM assistant_turns WHERE id = ? AND conversation_id = ?`,
		turn, conversation).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("assistantdomain: read turn %s: %w", turn, err)
	}
	return wire.TurnState(state), true, nil
}

// moveTurn moves a turn from one of from to to, setting the extra columns
// extra names (each with its own placeholder, bound to args), and writes
// nothing for a turn in any other state.
func moveTurn(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	from []wire.TurnState, to wire.TurnState, extra string, args ...any) (int, error) {

	state, found, err := turnState(ctx, tx, conversation, turn)
	if err != nil || !found || !slices.Contains(from, state) {
		return 0, err
	}
	set := `state = ?`
	if extra != "" {
		set += `, ` + extra
	}
	bound := append(append([]any{string(to)}, args...), c.packed, turn, c.packed)
	res, err := tx.ExecContext(ctx, `
		UPDATE assistant_turns SET `+set+`, version = ?
		WHERE id = ? AND version < ?`, bound...)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: move turn %s to %s at %s: %w",
			turn, to, c.position, err)
	}
	return affected(res), nil
}

// applyParked parks a running turn and keeps its loop beside it, replacing
// what an earlier park of the same turn kept.
func applyParked(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	p TurnParked) (int, error) {

	written, err := moveTurn(ctx, tx, c, conversation, turn,
		[]wire.TurnState{wire.TurnRunning}, wire.TurnWaitingForApproval, "steps = ?", p.Steps)
	if err != nil || written == 0 {
		return written, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_parked (turn_id, conversation_id, messages_sealed, version)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (turn_id) DO UPDATE SET messages_sealed = excluded.messages_sealed,
			version = excluded.version
		WHERE excluded.version > assistant_parked.version`,
		turn, conversation, p.MessagesSealed, c.packed)
	if err != nil {
		return written, fmt.Errorf("assistantdomain: park turn %s at %s: %w", turn, c.position, err)
	}
	return written + affected(res), nil
}

// applyClosed ends a turn, drops what it parked and frees its conversation for
// the next one. A turn already in a final state writes nothing: a turn closes
// once.
func applyClosed(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	p TurnClosed) (int, error) {

	if !p.State.Final() {
		return 0, nil
	}
	state, found, err := turnState(ctx, tx, conversation, turn)
	if err != nil || !found || state.Final() {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE assistant_turns SET state = ?, ended_at = ?, steps = ?, input_tokens = ?,
			output_tokens = ?, cache_read = ?, error_code = ?, version = ?
		WHERE id = ? AND version < ?`,
		string(p.State), c.at(), p.Steps, p.InputTokens, p.OutputTokens, p.CacheRead,
		p.ErrorCode, c.packed, turn, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: close turn %s at %s: %w", turn, c.position, err)
	}
	written := affected(res)
	for _, statement := range []string{
		`DELETE FROM assistant_parked WHERE turn_id = ?`,
		`UPDATE assistant_conversations SET open_turn_id = '' WHERE open_turn_id = ?`,
	} {
		res, err := tx.ExecContext(ctx, statement, turn)
		if err != nil {
			return written, fmt.Errorf("assistantdomain: close turn %s at %s: %w",
				turn, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applyMessage records one message, create-only by its id.
func applyMessage(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	p MessageRecorded) (int, error) {

	if ok := present(p.Message); !ok || !p.Role.Valid() {
		return 0, nil
	}
	apps, err := json.Marshal(canonicalApps(p.Apps))
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: encode message %s's apps: %w", p.Message, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_messages
			(id, conversation_id, turn_id, role, text_sealed, created_at,
			 page_title_sealed, page_screen, apps, attachments_sealed, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.Message, conversation, turn, string(p.Role), p.TextSealed, c.at(),
		p.PageTitleSealed, p.PageScreen, string(apps), p.AttachmentsSealed, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record message %s at %s: %w",
			p.Message, c.position, err)
	}
	return affected(res), nil
}

// applyStep records a step at its latest state: the later record wins, and
// only within the conversation that recorded the step first.
func applyStep(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	p StepRecorded) (int, error) {

	if ok := present(p.Step); !ok || !p.State.Valid() {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_steps
			(id, conversation_id, turn_id, idx, tool, state, refusal_code, body_sealed, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state,
			refusal_code = excluded.refusal_code, body_sealed = excluded.body_sealed,
			version = excluded.version
		WHERE excluded.version > assistant_steps.version
			AND excluded.conversation_id = assistant_steps.conversation_id
			AND excluded.turn_id = assistant_steps.turn_id`,
		p.Step, conversation, turn, p.Index, p.Tool, string(p.State), p.RefusalCode,
		p.BodySealed, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record step %s at %s: %w", p.Step, c.position, err)
	}
	return affected(res), nil
}

// applyCard records one card, create-only by its id.
func applyCard(ctx context.Context, tx *sql.Tx, c applyContext, conversation, turn string,
	p CardRecorded) (int, error) {

	if ok := present(p.Card); !ok || !p.Type.Valid() {
		return 0, nil
	}
	if ok := present(p.Message); !ok {
		return 0, nil
	}
	apps, err := json.Marshal(canonicalApps(p.Apps))
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: encode card %s's apps: %w", p.Card, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_cards
			(id, conversation_id, turn_id, message_id, type, apps, body_sealed, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.Card, conversation, turn, p.Message, string(p.Type), string(apps),
		p.BodySealed, c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record card %s at %s: %w", p.Card, c.position, err)
	}
	return affected(res), nil
}

// canonicalApps is a card's or a message's apps sorted and unique, never nil, so two nodes
// write one encoding.
func canonicalApps(apps []string) []string {
	out := slices.Clone(apps)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []string{}
	}
	return out
}
