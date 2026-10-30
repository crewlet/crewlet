package assistantdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// The namespaces a turn's messages, steps and cards are derived in from the
// turn and their place in it. FIXED for the life of the format: a new one would
// give a retry that straddles an upgrade a second row.
var (
	MessageNamespace = uuid.MustParse("8e3b1d6c-2f4a-5b97-8c1d-3a5e7f9b0d24")
	StepNamespace    = uuid.MustParse("2c7f9a1e-6b3d-5e48-9f2a-7d1c4b8e6a35")
	CardNamespace    = uuid.MustParse("d4a8e2f6-1c9b-5d37-a6e4-9b2f5c8d1e46")
)

// MessageID is the id of the n-th message of a turn: the same turn and place
// always name the same message, so a duplicate publish writes one row.
func MessageID(turn uuid.UUID, n int) uuid.UUID { return childID(MessageNamespace, turn, n) }

// StepID is the id of the n-th tool step of a turn.
func StepID(turn uuid.UUID, n int) uuid.UUID { return childID(StepNamespace, turn, n) }

// CardID is the id of the n-th card of a turn.
func CardID(turn uuid.UUID, n int) uuid.UUID { return childID(CardNamespace, turn, n) }

// childID is a name-based id under ns for one place in one turn.
func childID(ns, turn uuid.UUID, n int) uuid.UUID {
	return uuid.NewSHA1(ns, fmt.Appendf(nil, "%s/%d", turn, n))
}

// NewTurn is one turn to open.
type NewTurn struct {
	Conversation uuid.UUID
	Turn         uuid.UUID

	// Node is the node whose runner takes the turn.
	Node string

	PromptVersion string
	Model         string
	ProviderKey   string
	TraceID       string

	// Page is the page chip the person's question carried, nil when they
	// sent it without the page.
	Page *wire.PageChip
}

// TurnEnd is how a turn ended.
type TurnEnd struct {
	// State is a final state: completed, stopped, failed, interrupted,
	// expired or budget exhausted.
	State        wire.TurnState
	Steps        int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	ErrorCode    string
}

// NewMessage is one message the person sent or was shown.
type NewMessage struct {
	// ID is the message's id, normally [MessageID] of its turn and place.
	ID   uuid.UUID
	Role wire.MessageRole
	Text string
	Page *wire.PageChip
	// Apps are the apps the turn had read through when it was written: a
	// reader who no longer holds one is shown a notice in its place.
	Apps []string
	// Attachments are the files a user message was sent with, at most
	// wire.AttachmentsPerTurn, each one of the conversation's own files
	// (the orchestrator checks them against the store before the turn).
	// Sealed with the message; each one's download route is the
	// conversation's own, whatever the caller handed in.
	Attachments []wire.FileRef
}

// sealPage seals a page chip's title for one record, or answers empties for
// none.
func (w *Writer) sealPage(page *wire.PageChip, aad string) (title, screen string, err error) {
	if page == nil {
		return "", "", nil
	}
	if err = checkText("the page chip's title", page.Title, TitleMaxBytes); err != nil {
		return "", "", err
	}
	if err = checkText("the page chip's screen", page.Screen, TitleMaxBytes); err != nil {
		return "", "", err
	}
	title, err = w.sealer.sealString(page.Title, aad)
	return title, page.Screen, err
}

// OpenTurn opens a turn in one of the owner's conversations, answering the
// turn's number in it. ONE AT A TIME: the record is arbitrated on the
// conversation's subject, so a second opening while a turn is running or
// parked, on this node or any other, is refused [ErrTurnRunning], and an
// opening of the turn already open (a retry whose first attempt landed) is
// refused [ErrTurnOpen], so it never starts a second runner. A retry the
// ledger collapses reports [statelog.Result.Collapsed], which the caller
// reads the same way.
func (w *Writer) OpenTurn(ctx context.Context, c Call, t NewTurn) (int, statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return 0, statelog.Result{}, err
	}
	if err := errors.Join(checkID("the conversation id", t.Conversation),
		checkID("the turn id", t.Turn)); err != nil {
		return 0, statelog.Result{}, err
	}
	for _, f := range []struct{ field, value string }{
		{"the node", t.Node}, {"the prompt version", t.PromptVersion},
		{"the model", t.Model}, {"the provider key", t.ProviderKey},
		{"the trace id", t.TraceID},
	} {
		if err := checkText(f.field, f.value, TitleMaxBytes); err != nil {
			return 0, statelog.Result{}, err
		}
	}
	pageTitle, pageScreen, err := w.sealPage(t.Page,
		AAD(t.Conversation, RecordTurn, t.Turn, ColumnPageTitle))
	if err != nil {
		return 0, statelog.Result{}, err
	}
	at := w.now()
	var seq int
	subject, scope := ConversationSubject(t.Conversation), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			row, ownedErr := owned(ctx, tx, c.Owner, t.Conversation.String())
			if ownedErr != nil {
				return statelog.Decision{}, ownedErr
			}
			switch row.openTurn {
			case t.Turn.String():
				// THIS TURN IS ALREADY OPEN: a retry whose first attempt
				// landed publishes nothing and answers the number it took.
				seq = row.turns
				return statelog.Decision{}, fmt.Errorf("%w: turn %s", ErrTurnOpen, t.Turn)
			case "":
			default:
				return statelog.Decision{}, fmt.Errorf("%w: turn %s", ErrTurnRunning, row.openTurn)
			}
			seq = row.turns + 1
			return decision(stamp, c.By, subject, OpTurnOpened, scope, c.OpID, TurnOpened{
				V: baseRecordVersion, Turn: t.Turn.String(), Owner: c.Owner.String(),
				Seq: seq, Node: t.Node, PromptVersion: t.PromptVersion, Model: t.Model,
				ProviderKey: t.ProviderKey, TraceID: t.TraceID,
				PageTitleSealed: pageTitle, PageScreen: pageScreen,
			}, at)
		}))
	return seq, res, err
}

// turnOf is the conversation one of the owner's turns belongs to, read before
// a record about it is formed: a turn's conversation never changes, and its
// subject is named by both.
func (w *Writer) turnOf(ctx context.Context, owner, turn uuid.UUID) (uuid.UUID, error) {
	var conversation string
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT conversation_id FROM assistant_turns WHERE id = ? AND owner_id = ?`,
			turn.String(), owner.String()).Scan(&conversation)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, fmt.Errorf("%w: turn %s", ErrNotFound, turn)
	case err != nil:
		return uuid.Nil, fmt.Errorf("assistantdomain: read turn %s: %w", turn, err)
	}
	return uuid.Parse(conversation)
}

// onTurn publishes one record about one of the owner's turns, on the turn's
// subject, decided in a snapshot that finds the turn the owner's and, where
// states names any, in one of them.
func (w *Writer) onTurn(ctx context.Context, c Call, turn uuid.UUID, op OpKind,
	pattern statelog.Pattern, states []wire.TurnState,
	payload func(conversation uuid.UUID) (any, error)) (statelog.Result, error) {

	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	conversation, err := w.turnOf(ctx, c.Owner, turn)
	if err != nil {
		return statelog.Result{}, err
	}
	body, err := payload(conversation)
	if err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := TurnSubject(conversation, turn), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, pattern,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, conversation.String()); err != nil {
				return statelog.Decision{}, err
			}
			state, found, err := turnState(ctx, tx, conversation.String(), turn.String())
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !found:
				return statelog.Decision{}, fmt.Errorf("%w: turn %s", ErrNotFound, turn)
			case len(states) > 0 && !slices.Contains(states, state):
				return statelog.Decision{}, &TurnMoved{State: state}
			}
			return decision(stamp, c.By, subject, op, scope, c.OpID, body, at)
		}))
}

// RecordMessage records one message of a turn. Additive: its id is derived from
// the turn ([MessageID]), so a duplicate publish writes one row.
func (w *Writer) RecordMessage(ctx context.Context, c Call, turn uuid.UUID,
	m NewMessage) (statelog.Result, error) {

	if err := checkID("the message id", m.ID); err != nil {
		return statelog.Result{}, err
	}
	if !m.Role.Valid() {
		return statelog.Result{}, invalid("role %q is not one of %s", m.Role,
			words.Or(wire.MessageRoles))
	}
	if err := checkText("the message", m.Text, MessageMaxBytes); err != nil {
		return statelog.Result{}, err
	}
	if err := checkApps("the message", m.Apps); err != nil {
		return statelog.Result{}, err
	}
	if err := checkAttachments(m.Role, m.Attachments); err != nil {
		return statelog.Result{}, err
	}
	return w.onTurn(ctx, c, turn, OpMessageRecorded, statelog.PatternAdditive, nil,
		func(conversation uuid.UUID) (any, error) {
			text, err := w.sealer.sealString(m.Text,
				AAD(conversation, RecordMessage, m.ID, ColumnText))
			if err != nil {
				return nil, err
			}
			attachments, err := w.sealAttachments(conversation, m.ID, m.Attachments)
			if err != nil {
				return nil, err
			}
			pageTitle, pageScreen, err := w.sealPage(m.Page,
				AAD(conversation, RecordMessage, m.ID, ColumnPageTitle))
			if err != nil {
				return nil, err
			}
			return MessageRecorded{V: baseRecordVersion, Message: m.ID.String(),
				Role: m.Role, TextSealed: text, PageTitleSealed: pageTitle,
				PageScreen: pageScreen, Apps: canonicalApps(m.Apps),
				AttachmentsSealed: attachments}, nil
		})
}

// RecordStep records one tool step at its latest state: the wire StepEvent the
// person was shown, sealed whole, with its place, tool, state and refusal code
// in the clear. Recorded again under the same step id as it moves, the later
// record wins.
func (w *Writer) RecordStep(ctx context.Context, c Call, turn uuid.UUID,
	step wire.StepEvent) (statelog.Result, error) {

	if err := step.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the step: %w", ErrInvalid, err)
	}
	body, err := json.Marshal(step)
	if err != nil {
		return statelog.Result{}, fmt.Errorf("assistantdomain: encode step %s: %w", step.StepID, err)
	}
	if len(body) > StepMaxBytes {
		return statelog.Result{}, invalid("the step is %d bytes encoded and the "+
			"maximum is %d", len(body), StepMaxBytes)
	}
	var code string
	if step.Refusal != nil {
		code = step.Refusal.Code
	}
	return w.onTurn(ctx, c, turn, OpStepRecorded, statelog.PatternAdditive, nil,
		func(conversation uuid.UUID) (any, error) {
			sealed, err := w.sealer.Seal(body, AAD(conversation, RecordStep, step.StepID, ColumnBody))
			if err != nil {
				return nil, err
			}
			return StepRecorded{V: baseRecordVersion, Step: step.StepID.String(),
				Index: step.Index, Tool: step.Tool, State: step.State, RefusalCode: code,
				BodySealed: sealed}, nil
		})
}

// RecordCard records one card the person was shown, with the apps a reader
// must hold for it to be shown again rather than a notice.
func (w *Writer) RecordCard(ctx context.Context, c Call, turn uuid.UUID, card wire.CardEvent,
	apps []string) (statelog.Result, error) {

	if err := card.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the card: %w", ErrInvalid, err)
	}
	if err := checkApps("the card", apps); err != nil {
		return statelog.Result{}, err
	}
	body, err := json.Marshal(card.Card)
	if err != nil {
		return statelog.Result{}, fmt.Errorf("assistantdomain: encode card %s: %w", card.CardID, err)
	}
	if len(body) > CardMaxBytes {
		return statelog.Result{}, invalid("the card is %d bytes encoded and the "+
			"maximum is %d", len(body), CardMaxBytes)
	}
	return w.onTurn(ctx, c, turn, OpCardRecorded, statelog.PatternAdditive, nil,
		func(conversation uuid.UUID) (any, error) {
			sealed, err := w.sealer.Seal(body, AAD(conversation, RecordCard, card.CardID, ColumnBody))
			if err != nil {
				return nil, err
			}
			return CardRecorded{V: baseRecordVersion, Card: card.CardID.String(),
				Message: card.MessageID.String(), Type: card.Card.Type,
				Apps: canonicalApps(apps), BodySealed: sealed}, nil
		})
}

// checkAttachments refuses a message's attachments past
// wire.AttachmentsPerTurn, on a message that is not the person's own, or naming
// a file no attachment could be: no id, the same file twice, a type nobody may
// attach, a size past wire.FileMaxBytes, or a name a download cannot carry.
func checkAttachments(role wire.MessageRole, files []wire.FileRef) error {
	if len(files) == 0 {
		return nil
	}
	if role != wire.RoleUser {
		return invalid("a %s message carries %d attachments: only the person's own "+
			"question is sent with files", role, len(files))
	}
	if len(files) > wire.AttachmentsPerTurn {
		return invalid("the message carries %d attachments and the maximum is %d",
			len(files), wire.AttachmentsPerTurn)
	}
	seen := make(map[uuid.UUID]bool, len(files))
	for _, f := range files {
		switch {
		case f.FileID == uuid.Nil:
			return invalid("an attachment names no file")
		case seen[f.FileID]:
			return invalid("file %s is attached twice", f.FileID)
		case !slices.Contains(wire.AttachmentMediaTypes, f.MediaType):
			return invalid("attachment %s is %q, and only %s are attached", f.FileID,
				f.MediaType, words.And(wire.AttachmentMediaTypes))
		case f.Size <= 0 || f.Size > wire.FileMaxBytes:
			return invalid("attachment %s is %d bytes, want 1..%d", f.FileID, f.Size,
				wire.FileMaxBytes)
		}
		if err := checkFileName(f.Name); err != nil {
			return err
		}
		seen[f.FileID] = true
	}
	return nil
}

// sealAttachments seals a message's attachments, each one's download route
// the conversation's own, or answers empty for none.
func (w *Writer) sealAttachments(conversation, message uuid.UUID, files []wire.FileRef) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	refs := make([]wire.FileRef, len(files))
	for i, f := range files {
		f.Download = wire.FilePath(conversation, f.FileID)
		refs[i] = f
	}
	body, err := json.Marshal(refs)
	if err != nil {
		return "", fmt.Errorf("assistantdomain: encode message %s's attachments: %w", message, err)
	}
	return w.sealer.Seal(body, AAD(conversation, RecordMessage, message, ColumnAttachments))
}

// checkApps refuses a record's apps past [MaxApps] or one that is no app name.
func checkApps(what string, apps []string) error {
	if len(apps) > MaxApps {
		return invalid("%s names %d apps and the maximum is %d", what, len(apps), MaxApps)
	}
	for _, app := range apps {
		if strings.TrimSpace(app) == "" || len(app) > TitleMaxBytes {
			return invalid("app %q is not an app name", app)
		}
	}
	return nil
}

// ParkTurn parks a running turn on a proposal, with the loop's conversation
// (at most [ParkedMaxBytes], opaque here) sealed beside it until the turn
// resumes and closes, and the steps it has taken so far counted on the turn.
func (w *Writer) ParkTurn(ctx context.Context, c Call, turn uuid.UUID, parked []byte,
	steps int) (statelog.Result, error) {
	switch {
	case len(parked) == 0:
		return statelog.Result{}, invalid("a parked turn keeps its loop's conversation")
	case len(parked) > ParkedMaxBytes:
		return statelog.Result{}, invalid("the parked loop is %d bytes and the maximum "+
			"is %d", len(parked), ParkedMaxBytes)
	case steps < 0:
		return statelog.Result{}, invalid("a parked turn has taken %d steps", steps)
	}
	return w.onTurn(ctx, c, turn, OpTurnParked, statelog.PatternArbitrated,
		[]wire.TurnState{wire.TurnRunning},
		func(conversation uuid.UUID) (any, error) {
			sealed, err := w.sealer.Seal(parked, AAD(conversation, RecordParked, turn, ColumnMessages))
			if err != nil {
				return nil, err
			}
			return TurnParked{V: baseRecordVersion, MessagesSealed: sealed, Steps: steps}, nil
		})
}

// ResumeTurn hands a parked turn to the runner on node.
func (w *Writer) ResumeTurn(ctx context.Context, c Call, turn uuid.UUID, node string) (statelog.Result, error) {
	if err := checkText("the node", node, TitleMaxBytes); err != nil {
		return statelog.Result{}, err
	}
	return w.onTurn(ctx, c, turn, OpTurnResumed, statelog.PatternArbitrated,
		[]wire.TurnState{wire.TurnWaitingForApproval},
		func(uuid.UUID) (any, error) {
			return TurnResumed{V: baseRecordVersion, Node: node}, nil
		})
}

// CloseTurn ends a running or parked turn in a final state, drops what it
// parked and frees its conversation for the next turn. ONCE: a second close is
// refused [TurnMoved] naming the state the first left it in.
//
// An end only a parked turn can come to ([ClosableFrom]) is refused
// [TurnMoved] for a turn that is running: one an approval resumed a moment
// before is the runner's to end, never expired under it.
func (w *Writer) CloseTurn(ctx context.Context, c Call, turn uuid.UUID, end TurnEnd) (statelog.Result, error) {
	if !end.State.Final() {
		return statelog.Result{}, invalid("a turn closes in a final state, and %q is not one",
			end.State)
	}
	if err := checkText("the error code", end.ErrorCode, TitleMaxBytes); err != nil {
		return statelog.Result{}, err
	}
	return w.onTurn(ctx, c, turn, OpTurnClosed, statelog.PatternArbitrated,
		ClosableFrom(end.State),
		func(uuid.UUID) (any, error) {
			return TurnClosed{V: baseRecordVersion, State: end.State, Steps: end.Steps,
				InputTokens: end.InputTokens, OutputTokens: end.OutputTokens,
				CacheRead: end.CacheRead, ErrorCode: end.ErrorCode}, nil
		})
}

// ClosableFrom is the states a turn may close into end from: expired (a
// parked turn whose proposals nobody decided in time) and superseded (a
// parked turn the person asked past) from waiting for approval alone, every
// other end from running or waiting.
func ClosableFrom(end wire.TurnState) []wire.TurnState {
	if end == wire.TurnExpired || end == wire.TurnSuperseded {
		return []wire.TurnState{wire.TurnWaitingForApproval}
	}
	return []wire.TurnState{wire.TurnRunning, wire.TurnWaitingForApproval}
}
