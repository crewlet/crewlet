package assistantdomain

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about a person's conversations from this node's own
// applied rows, opening every sealed value on this node's keyring.
//
// # Every read is the owner's
//
// Every method takes the owner and answers [ErrNotFound] for a conversation,
// turn, card or proposal that is not theirs, exactly as for one that does not
// exist: a caller with no relation is told it does not exist (ADR-0034's rule,
// ADR-0053). THE ONE EXCEPTION is [Reader.AuditTranscript], which opens
// somebody else's conversation only for the receipt of a compliance read
// already recorded beside it ([Writer.RecordAuditRead]), so no path reads
// another person's conversation without leaving the record they see.
//
// # Through the read authority
//
// Every read goes through [statelog.Reader], so the refusal ladder holds (an
// evicted node, one below the trim floor and one whose applier stopped serve
// nothing) and a record this node could not decode is reported rather than
// answered around. Reads are served at `stale`: a person reads their own
// conversation through the node their own writes waited on, and every write
// here waits for this node's applier before it answers.
type Reader struct {
	db      store.ReplicatedReader
	log     *statelog.Reader
	sealer  *Sealer
	logger  *slog.Logger
	metrics Metrics
}

// Metrics is what the reader counts into: the cards it reads back as a
// notice ([UnshownCardNotice]).
type Metrics interface {
	Add(name string, n uint64, attrs metrics.Attrs)
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee.
	Log *statelog.Reader

	// Sealer opens every stored value. REQUIRED.
	Sealer *Sealer

	// Logger says which stored card was read back as a notice, and why.
	// Nil is the assistant's own component logger.
	Logger *slog.Logger

	// Metrics counts the cards read back as a notice. Nil counts nothing.
	Metrics Metrics
}

// NewReader builds the assistant read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	switch {
	case opts.DB.IsZero():
		return nil, errors.New("assistantdomain: a reader needs the replicated estate")
	case opts.Log == nil:
		return nil, errors.New("assistantdomain: a reader needs its domain's read " +
			"authority: without it every read level is a label rather than a guarantee")
	case opts.Sealer == nil:
		return nil, errors.New("assistantdomain: a reader needs the keyring every " +
			"stored value is sealed under")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Get("assistant")
	}
	return &Reader{db: opts.DB, log: opts.Log, sealer: opts.Sealer, logger: logger,
		metrics: opts.Metrics}, nil
}

// readLevel is the level every read here is served at.
var readLevel = statelog.Freshness{Level: statelog.ReadStale}

// read runs fn in one transaction of the read authority, over scope.
func (r *Reader) read(ctx context.Context, scope statelog.ScopeSet, set bool,
	fn func(*sql.Tx) error) error {

	_, err := r.log.Read(ctx, readLevel.Query(scope, set), fn)
	return err
}

// readChild runs fn in one transaction of the read authority, over the scope
// of the conversation that the row of id in table belongs to: resolved inside
// the transaction, because only the row says which conversation it is in. A
// row this node does not hold reads over the whole domain, since a create this
// node could not decode is exactly a row it does not hold.
func (r *Reader) readChild(ctx context.Context, table string, id uuid.UUID,
	fn func(*sql.Tx) error) error {

	resolve := func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var conversation string
		err := tx.QueryRowContext(ctx,
			`SELECT conversation_id FROM `+table+` WHERE `+childKey[table]+` = ?`,
			id.String()).Scan(&conversation)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return DomainScope(), nil
		case err != nil:
			return statelog.ScopeSet{}, err
		}
		return ConversationScope(conversation), nil
	}
	q := readLevel.Query(statelog.ScopeSet{}, false)
	q.Resolve = resolve
	_, err := r.log.Read(ctx, q, fn)
	return err
}

// childKey is the key column of each table a point read resolves through.
var childKey = map[string]string{
	"assistant_turns": "id", "assistant_cards": "id", "assistant_proposals": "id",
	"assistant_parked": "turn_id",
}

// Conversation is one conversation as its owner reads it.
type Conversation struct {
	ID             uuid.UUID
	Owner          uuid.UUID
	Title          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastActivityAt time.Time

	// Turns is how many turns it has had, and OpenTurn the one running or
	// parked, uuid.Nil for none.
	Turns    int
	OpenTurn uuid.UUID

	// LastTurnState is the newest turn's state, empty before the first.
	LastTurnState wire.TurnState
}

// Summary is the conversation as a listing shows it.
func (c Conversation) Summary() wire.ConversationSummary {
	return wire.ConversationSummary{
		ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		LastActivityAt: c.LastActivityAt, LastTurnState: c.LastTurnState,
	}
}

// Turn is one turn.
type Turn struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	Owner        uuid.UUID
	Seq          int
	State        wire.TurnState
	Node         string
	StartedAt    time.Time
	// EndedAt is zero for a turn still running or parked.
	EndedAt       time.Time
	PromptVersion string
	Model         string
	ProviderKey   string
	Steps         int
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	ErrorCode     string
	TraceID       string
	Page          *wire.PageChip
}

// Message is one message, opened.
type Message struct {
	ID        uuid.UUID
	Turn      uuid.UUID
	Role      wire.MessageRole
	Text      string
	CreatedAt time.Time
	Page      *wire.PageChip
	// Apps are the apps a reader must hold to be shown the message rather
	// than a notice.
	Apps []string
	// Attachments are the files a user message was sent with.
	Attachments []wire.FileRef
}

// Wire is the message as a transcript carries it.
func (m Message) Wire() wire.Message {
	return wire.Message{ID: m.ID, TurnID: m.Turn, Role: m.Role, Text: m.Text,
		CreatedAt: m.CreatedAt, Page: m.Page, Attachments: m.Attachments}
}

// Card is one card, opened, with the apps a reader must hold for it to be
// shown and exported rather than replaced by a notice.
type Card struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Message      uuid.UUID
	Type         wire.CardKind
	Apps         []string
	Card         wire.Card
}

// Event is the card as a transcript carries it.
func (c Card) Event() wire.CardEvent {
	return wire.CardEvent{CardID: c.ID, MessageID: c.Message, Card: c.Card}
}

// Proposal is one proposal, opened, at its state now.
type Proposal struct {
	ID             uuid.UUID
	Conversation   uuid.UUID
	Turn           uuid.UUID
	Owner          uuid.UUID
	Tool           string
	Act            string
	Scope          wire.ProposalScope
	Method         string
	Path           string
	Query          url.Values
	Body           []byte
	RequestDigest  string
	Digest         string
	OpKey          string
	SourceRevision string
	StepUp         bool
	Card           wire.Card
	State          wire.ProposalState
	ExpiresAt      time.Time
	CreatedAt      time.Time
	// DecidedAt is the last move's instant, SettledAt the settlement's;
	// each zero until it happens.
	DecidedAt   time.Time
	SettledAt   time.Time
	Outcome     string
	Position    string
	Receipt     *wire.Receipt
	Failure     *wire.Failure
	RefusalCode string
}

// AuditRead is one compliance read of a conversation, as its owner is shown
// it.
type AuditRead struct {
	Reader iam.Actor
	Reason string
	At     time.Time
}

// Wire is the read as a transcript carries it.
func (a AuditRead) Wire() wire.AuditRead {
	return wire.AuditRead{Reader: a.Reader.Name, Reason: a.Reason, At: a.At}
}

// Transcript is a conversation and everything in it, in log order.
type Transcript struct {
	Conversation Conversation
	Turns        []Turn
	Messages     []Message
	Steps        []wire.StepEvent
	Cards        []Card
	Proposals    []Proposal
	AuditReads   []AuditRead
}

// ---- listings ----------------------------------------------------------- //

// Conversations is one page of the owner's conversations, the most recently
// used first. cursor is a page's NextCursor, empty for the first; limit is at
// most [MaxListLimit], zero for [DefaultListLimit].
func (r *Reader) Conversations(ctx context.Context, owner uuid.UUID, cursor string,
	limit int) (wire.ConversationList, error) {

	switch {
	case limit == 0:
		limit = DefaultListLimit
	case limit < 0 || limit > MaxListLimit:
		return wire.ConversationList{}, invalid("a listing takes 1 to %d conversations",
			MaxListLimit)
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return wire.ConversationList{}, err
	}
	list := wire.ConversationList{Conversations: []wire.ConversationSummary{}}
	err = r.read(ctx, DomainScope(), true, func(tx *sql.Tx) (err error) {
		args := []any{owner.String()}
		where := `c.owner_id = ?`
		if after != nil {
			where += ` AND (c.last_activity_at < ? OR (c.last_activity_at = ? AND c.id < ?))`
			args = append(args, after.at, after.at, after.id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+conversationColumns+`
			FROM assistant_conversations c WHERE `+where+`
			ORDER BY c.last_activity_at DESC, c.id DESC LIMIT ?`, append(args, limit+1)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var page []Conversation
		var activity []int64
		for rows.Next() {
			c, at, err := r.scanConversation(rows)
			if err != nil {
				return err
			}
			page = append(page, c)
			activity = append(activity, at)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page) > limit {
			page = page[:limit]
			list.NextCursor = encodeCursor(activity[limit-1], page[limit-1].ID)
		}
		for _, c := range page {
			list.Conversations = append(list.Conversations, c.Summary())
		}
		return nil
	})
	if err != nil {
		return wire.ConversationList{}, fmt.Errorf("assistantdomain: list %s's "+
			"conversations: %w", owner, err)
	}
	return list, nil
}

// cursorAt is where a listing page ended.
type cursorAt struct {
	at int64
	id string
}

// encodeCursor is a page's continuation: the last row's activity and id,
// opaque to the caller.
func encodeCursor(at int64, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(at, 10) + ":" + id.String()))
}

// decodeCursor reads a continuation back, nil for the first page.
func decodeCursor(cursor string) (*cursorAt, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, invalid("the cursor is not one a listing handed out")
	}
	at, id, ok := strings.Cut(string(raw), ":")
	n, perr := strconv.ParseInt(at, 10, 64)
	if _, uerr := uuid.Parse(id); !ok || perr != nil || uerr != nil {
		return nil, invalid("the cursor is not one a listing handed out")
	}
	return &cursorAt{at: n, id: id}, nil
}

// conversationColumns is every column a Conversation is read from, in
// [Reader.scanConversation]'s order, the newest turn's state included.
const conversationColumns = `c.id, c.owner_id, c.title_sealed, c.created_at, c.updated_at,
	c.last_activity_at, c.turns, c.open_turn_id,
	COALESCE((SELECT t.state FROM assistant_turns t WHERE t.conversation_id = c.id
		ORDER BY t.seq DESC LIMIT 1), '')`

// rowScanner is a row or a cursor over rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanConversation reads one conversation and its raw activity instant.
func (r *Reader) scanConversation(row rowScanner) (Conversation, int64, error) {
	var (
		c                          Conversation
		id, owner, title, open     string
		created, updated, activity int64
		last                       string
	)
	if err := row.Scan(&id, &owner, &title, &created, &updated, &activity, &c.Turns,
		&open, &last); err != nil {
		return Conversation{}, 0, err
	}
	var err error
	if c.ID, err = uuid.Parse(id); err != nil {
		return Conversation{}, 0, fmt.Errorf("assistantdomain: conversation id %q: %w", id, err)
	}
	if c.Owner, err = uuid.Parse(owner); err != nil {
		return Conversation{}, 0, fmt.Errorf("assistantdomain: conversation %s's owner: %w", id, err)
	}
	if open != "" {
		if c.OpenTurn, err = uuid.Parse(open); err != nil {
			return Conversation{}, 0, fmt.Errorf("assistantdomain: conversation %s's open turn: %w", id, err)
		}
	}
	if c.Title, err = r.sealer.openString(title, AAD(c.ID, RecordConversation, c.ID, ColumnTitle)); err != nil {
		return Conversation{}, 0, fmt.Errorf("conversation %s's title: %w", id, err)
	}
	c.CreatedAt, c.UpdatedAt = store.DecodeTime(created), store.DecodeTime(updated)
	c.LastActivityAt = store.DecodeTime(activity)
	c.LastTurnState = wire.TurnState(last)
	return c, activity, nil
}

// ---- point reads -------------------------------------------------------- //

// Conversation is one of the owner's conversations.
func (r *Reader) Conversation(ctx context.Context, owner, id uuid.UUID) (Conversation, error) {
	var c Conversation
	err := r.read(ctx, ConversationScope(id.String()), false, func(tx *sql.Tx) error {
		var err error
		c, err = r.conversationIn(ctx, tx, id, &owner)
		return err
	})
	return c, err
}

// conversationIn reads conversation id in tx, keyed on owner unless owner is
// nil (the audited read alone passes nil).
func (r *Reader) conversationIn(ctx context.Context, tx *sql.Tx, id uuid.UUID,
	owner *uuid.UUID) (Conversation, error) {

	args := []any{id.String()}
	where := `c.id = ?`
	if owner != nil {
		where += ` AND c.owner_id = ?`
		args = append(args, owner.String())
	}
	c, _, err := r.scanConversation(tx.QueryRowContext(ctx,
		`SELECT `+conversationColumns+` FROM assistant_conversations c WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, fmt.Errorf("%w: conversation %s", ErrNotFound, id)
	}
	return c, err
}

// Transcript is one of the owner's conversations and everything in it.
func (r *Reader) Transcript(ctx context.Context, owner, id uuid.UUID) (Transcript, error) {
	var t Transcript
	err := r.read(ctx, ConversationScope(id.String()), false, func(tx *sql.Tx) error {
		var err error
		t, err = r.transcriptIn(ctx, tx, id, &owner)
		return err
	})
	return t, err
}

// AuditTranscript is a conversation read for compliance (decision D06), for
// the receipt of the compliance read [Writer.RecordAuditRead] recorded beside
// it: a receipt whose record has not applied on this node opens nothing, so
// the conversation is never read here before its owner can see who read it.
func (r *Reader) AuditTranscript(ctx context.Context, receipt AuditReceipt) (Transcript, error) {
	if receipt.conversation == uuid.Nil || receipt.opID == "" {
		return Transcript{}, invalid("a compliance read opens a conversation only for " +
			"the receipt its recorded read answered")
	}
	var t Transcript
	err := r.read(ctx, ConversationScope(receipt.conversation.String()), false,
		func(tx *sql.Tx) error {
			if err := auditRecorded(ctx, tx, receipt); err != nil {
				return err
			}
			var err error
			t, err = r.transcriptIn(ctx, tx, receipt.conversation, nil)
			return err
		})
	return t, err
}

// auditRecorded refuses a receipt whose compliance read has not applied on
// this node as [ErrAuditPending]: what every audited read checks, in the same
// transaction it then reads in.
func auditRecorded(ctx context.Context, tx *sql.Tx, receipt AuditReceipt) error {
	var recorded int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM assistant_audit_reads
		WHERE conversation_id = ? AND record_id = ?`,
		receipt.conversation.String(), receipt.opID).Scan(&recorded); err != nil {
		return err
	}
	if recorded == 0 {
		return fmt.Errorf("%w: %w: the compliance read of %s has not applied on "+
			"this node, so its owner could not yet see it", ErrAuditPending, ErrNotFound,
			receipt.conversation)
	}
	return nil
}

// transcriptIn reads a whole conversation in tx.
func (r *Reader) transcriptIn(ctx context.Context, tx *sql.Tx, id uuid.UUID,
	owner *uuid.UUID) (Transcript, error) {

	c, err := r.conversationIn(ctx, tx, id, owner)
	if err != nil {
		return Transcript{}, err
	}
	t := Transcript{Conversation: c, Turns: []Turn{}, Messages: []Message{},
		Steps: []wire.StepEvent{}, Cards: []Card{}, Proposals: []Proposal{},
		AuditReads: []AuditRead{}}
	conversation := id.String()
	if err := eachRow(ctx, tx, `SELECT `+turnColumns+` FROM assistant_turns
		WHERE conversation_id = ? ORDER BY seq`, []any{conversation}, func(row rowScanner) error {
		turn, err := r.scanTurn(row)
		t.Turns = append(t.Turns, turn)
		return err
	}); err != nil {
		return Transcript{}, err
	}
	if err := eachRow(ctx, tx, `SELECT id, turn_id, role, text_sealed, created_at,
		page_title_sealed, page_screen, apps, attachments_sealed FROM assistant_messages
		WHERE conversation_id = ? ORDER BY version, id`, []any{conversation},
		func(row rowScanner) error {
			m, err := r.scanMessage(row, id)
			t.Messages = append(t.Messages, m)
			return err
		}); err != nil {
		return Transcript{}, err
	}
	if err := eachRow(ctx, tx, `SELECT id, body_sealed FROM assistant_steps
		WHERE conversation_id = ? ORDER BY turn_id, idx, id`, []any{conversation},
		func(row rowScanner) error {
			s, err := r.scanStep(row, id)
			t.Steps = append(t.Steps, s)
			return err
		}); err != nil {
		return Transcript{}, err
	}
	if err := eachRow(ctx, tx, `SELECT `+cardColumns+` FROM assistant_cards k
		WHERE k.conversation_id = ? ORDER BY k.version, k.id`, []any{conversation},
		func(row rowScanner) error {
			card, err := r.scanCard(row)
			t.Cards = append(t.Cards, card)
			return err
		}); err != nil {
		return Transcript{}, err
	}
	if err := eachRow(ctx, tx, `SELECT `+proposalColumns+` FROM assistant_proposals
		WHERE conversation_id = ? ORDER BY version, id`, []any{conversation},
		func(row rowScanner) error {
			p, err := r.scanProposal(row)
			t.Proposals = append(t.Proposals, p)
			return err
		}); err != nil {
		return Transcript{}, err
	}
	if err := eachRow(ctx, tx, `SELECT reader, reader_kind, operator_id, reason_sealed,
		record_id, at FROM assistant_audit_reads WHERE conversation_id = ?
		ORDER BY at, reader`, []any{conversation}, func(row rowScanner) error {
		a, err := r.scanAuditRead(row, id)
		t.AuditReads = append(t.AuditReads, a)
		return err
	}); err != nil {
		return Transcript{}, err
	}
	return t, nil
}

// Turn is one of the owner's turns.
func (r *Reader) Turn(ctx context.Context, owner, id uuid.UUID) (Turn, error) {
	var t Turn
	err := r.readChild(ctx, "assistant_turns", id, func(tx *sql.Tx) error {
		var err error
		t, err = r.scanTurn(tx.QueryRowContext(ctx, `SELECT `+turnColumns+`
			FROM assistant_turns WHERE id = ? AND owner_id = ?`, id.String(), owner.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: turn %s", ErrNotFound, id)
		}
		return err
	})
	return t, err
}

// Parked is what one of the owner's parked turns kept of its loop, opened, and
// [ErrNotFound] for a turn that keeps nothing.
func (r *Reader) Parked(ctx context.Context, owner, turn uuid.UUID) ([]byte, error) {
	var out []byte
	err := r.readChild(ctx, "assistant_parked", turn, func(tx *sql.Tx) error {
		var conversation, sealed string
		err := tx.QueryRowContext(ctx, `
			SELECT p.conversation_id, p.messages_sealed FROM assistant_parked p
			JOIN assistant_turns t ON t.id = p.turn_id
			WHERE p.turn_id = ? AND t.owner_id = ?`, turn.String(), owner.String()).Scan(
			&conversation, &sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: parked turn %s", ErrNotFound, turn)
		}
		if err != nil {
			return err
		}
		id, err := uuid.Parse(conversation)
		if err != nil {
			return err
		}
		out, err = r.sealer.Open(sealed, AAD(id, RecordParked, turn, ColumnMessages))
		return err
	})
	return out, err
}

// Card is one of the owner's cards.
func (r *Reader) Card(ctx context.Context, owner, id uuid.UUID) (Card, error) {
	var c Card
	err := r.readChild(ctx, "assistant_cards", id, func(tx *sql.Tx) error {
		var err error
		c, err = r.scanCard(tx.QueryRowContext(ctx, `SELECT `+cardColumns+`
			FROM assistant_cards k JOIN assistant_conversations c ON c.id = k.conversation_id
			WHERE k.id = ? AND c.owner_id = ?`, id.String(), owner.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: card %s", ErrNotFound, id)
		}
		return err
	})
	return c, err
}

// Proposal is one of the owner's proposals, at its state now.
func (r *Reader) Proposal(ctx context.Context, owner, id uuid.UUID) (Proposal, error) {
	var p Proposal
	err := r.readChild(ctx, "assistant_proposals", id, func(tx *sql.Tx) error {
		var err error
		p, err = r.scanProposal(tx.QueryRowContext(ctx, `SELECT `+proposalColumns+`
			FROM assistant_proposals WHERE id = ? AND owner_id = ?`, id.String(), owner.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: proposal %s", ErrNotFound, id)
		}
		return err
	})
	return p, err
}

// ---- the retention reaper's and the erasure's questions ---------------- //

// InactiveBefore nominates conversations untouched since before cutoff, the
// longest untouched first, at most limit of them. It only nominates: each
// purge is decided again in its own snapshot ([Writer.PurgeInactive]).
func (r *Reader) InactiveBefore(ctx context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		return nil, invalid("a retention listing names how many it wants")
	}
	return r.ids(ctx, `SELECT id FROM assistant_conversations WHERE last_activity_at < ?
		ORDER BY last_activity_at, id LIMIT ?`, store.EncodeTime(cutoff), limit)
}

// TurnRef names one turn and whose it is.
type TurnRef struct {
	Owner, Conversation, Turn uuid.UUID
}

// ParkedPast nominates parked turns none of whose proposals expires after
// now, the longest parked first, at most limit of them: the expiry
// backstop's question. It only nominates: whether a turn has lapsed is
// decided again against its proposals' states by the one rule that ends a
// parked turn as expired (internal/assistant/proposal.Lapsed), and the close
// is arbitrated.
func (r *Reader) ParkedPast(ctx context.Context, now time.Time, limit int) ([]TurnRef, error) {
	if limit <= 0 {
		return nil, invalid("a parked listing names how many it wants")
	}
	out := []TurnRef{}
	err := r.read(ctx, DomainScope(), true, func(tx *sql.Tx) error {
		out = out[:0]
		return eachRow(ctx, tx, `
			SELECT t.owner_id, t.conversation_id, t.id FROM assistant_parked k
			JOIN assistant_turns t ON t.id = k.turn_id
			WHERE t.state = ? AND NOT EXISTS (
				SELECT 1 FROM assistant_proposals p WHERE p.turn_id = t.id AND p.expires_at > ?)
			ORDER BY t.started_at, t.id LIMIT ?`,
			[]any{string(wire.TurnWaitingForApproval), store.EncodeTime(now), limit},
			func(row rowScanner) error {
				var owner, conversation, turn string
				if err := row.Scan(&owner, &conversation, &turn); err != nil {
					return err
				}
				o, c, t, err := parseThree(owner, conversation, turn)
				if err != nil {
					return err
				}
				out = append(out, TurnRef{Owner: o, Conversation: c, Turn: t})
				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: list the parked turns: %w", err)
	}
	return out, nil
}

// Owners is every person who owns a conversation here, in id order: what the
// erasure's backstop asks the identity estate about.
func (r *Reader) Owners(ctx context.Context) ([]uuid.UUID, error) {
	return r.ids(ctx, `SELECT DISTINCT owner_id FROM assistant_conversations ORDER BY owner_id`)
}

// ids reads one column of ids over the whole domain.
func (r *Reader) ids(ctx context.Context, query string, args ...any) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	err := r.read(ctx, DomainScope(), true, func(tx *sql.Tx) error {
		out = out[:0]
		return eachRow(ctx, tx, query, args, func(row rowScanner) error {
			var raw string
			if err := row.Scan(&raw); err != nil {
				return err
			}
			id, err := uuid.Parse(raw)
			if err != nil {
				return fmt.Errorf("assistantdomain: a stored id %q: %w", raw, err)
			}
			out = append(out, id)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: list the conversations: %w", err)
	}
	return out, nil
}

// ---- scanning ----------------------------------------------------------- //

// eachRow runs query in tx and hands each row to fn.
func eachRow(ctx context.Context, tx *sql.Tx, query string, args []any,
	fn func(rowScanner) error) error {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := fn(rows); err != nil {
			_ = rows.Close()
			return err
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// turnColumns is every column a Turn is read from, in [Reader.scanTurn]'s
// order.
const turnColumns = `id, conversation_id, owner_id, seq, state, node, started_at,
	ended_at, prompt_version, model, provider_key, steps, input_tokens, output_tokens,
	cache_read, error_code, trace_id, page_title_sealed, page_screen`

// scanTurn reads one turn.
func (r *Reader) scanTurn(row rowScanner) (Turn, error) {
	var (
		t                              Turn
		id, conversation, owner, state string
		started, ended                 int64
		pageTitle, pageScreen          string
	)
	if err := row.Scan(&id, &conversation, &owner, &t.Seq, &state, &t.Node, &started,
		&ended, &t.PromptVersion, &t.Model, &t.ProviderKey, &t.Steps, &t.InputTokens,
		&t.OutputTokens, &t.CacheRead, &t.ErrorCode, &t.TraceID, &pageTitle,
		&pageScreen); err != nil {
		return Turn{}, err
	}
	var err error
	if t.ID, t.Conversation, t.Owner, err = parseThree(id, conversation, owner); err != nil {
		return Turn{}, err
	}
	t.State, t.StartedAt = wire.TurnState(state), store.DecodeTime(started)
	if ended > 0 {
		t.EndedAt = store.DecodeTime(ended)
	}
	t.Page, err = r.openPage(pageTitle, pageScreen, AAD(t.Conversation, RecordTurn, t.ID, ColumnPageTitle))
	return t, err
}

// scanMessage reads one message of conversation.
func (r *Reader) scanMessage(row rowScanner, conversation uuid.UUID) (Message, error) {
	var (
		m                     Message
		id, turn, role, text  string
		created               int64
		pageTitle, pageScreen string
		apps, attachments     string
	)
	if err := row.Scan(&id, &turn, &role, &text, &created, &pageTitle, &pageScreen,
		&apps, &attachments); err != nil {
		return Message{}, err
	}
	if err := json.Unmarshal([]byte(apps), &m.Apps); err != nil {
		return Message{}, fmt.Errorf("assistantdomain: message %s's apps: %w", id, err)
	}
	var err error
	if m.ID, err = uuid.Parse(id); err != nil {
		return Message{}, err
	}
	if m.Turn, err = uuid.Parse(turn); err != nil {
		return Message{}, err
	}
	m.Role, m.CreatedAt = wire.MessageRole(role), store.DecodeTime(created)
	if m.Text, err = r.sealer.openString(text, AAD(conversation, RecordMessage, m.ID, ColumnText)); err != nil {
		return Message{}, fmt.Errorf("message %s: %w", id, err)
	}
	if m.Page, err = r.openPage(pageTitle, pageScreen,
		AAD(conversation, RecordMessage, m.ID, ColumnPageTitle)); err != nil {
		return Message{}, err
	}
	refs, err := r.sealer.Open(attachments, AAD(conversation, RecordMessage, m.ID, ColumnAttachments))
	if err != nil {
		return Message{}, fmt.Errorf("message %s's attachments: %w", id, err)
	}
	if len(refs) > 0 {
		if err := json.Unmarshal(refs, &m.Attachments); err != nil {
			return Message{}, fmt.Errorf("assistantdomain: message %s's attachments: %w", id, err)
		}
	}
	return m, nil
}

// scanStep reads one step of conversation, its whole wire event opened.
func (r *Reader) scanStep(row rowScanner, conversation uuid.UUID) (wire.StepEvent, error) {
	var id, sealed string
	if err := row.Scan(&id, &sealed); err != nil {
		return wire.StepEvent{}, err
	}
	step, err := uuid.Parse(id)
	if err != nil {
		return wire.StepEvent{}, err
	}
	body, err := r.sealer.Open(sealed, AAD(conversation, RecordStep, step, ColumnBody))
	if err != nil {
		return wire.StepEvent{}, fmt.Errorf("step %s: %w", id, err)
	}
	var out wire.StepEvent
	if err := json.Unmarshal(body, &out); err != nil {
		return wire.StepEvent{}, fmt.Errorf("assistantdomain: step %s: %w", id, err)
	}
	return out, nil
}

// cardColumns is every column a Card is read from, in [Reader.scanCard]'s
// order.
const cardColumns = `k.id, k.conversation_id, k.turn_id, k.message_id, k.type, k.apps,
	k.body_sealed`

// scanCard reads one card, its body opened.
func (r *Reader) scanCard(row rowScanner) (Card, error) {
	var (
		c                                     Card
		id, conversation, turn, message, kind string
		apps, sealed                          string
	)
	if err := row.Scan(&id, &conversation, &turn, &message, &kind, &apps, &sealed); err != nil {
		return Card{}, err
	}
	var err error
	if c.ID, c.Conversation, c.Turn, err = parseThree(id, conversation, turn); err != nil {
		return Card{}, err
	}
	if c.Message, err = uuid.Parse(message); err != nil {
		return Card{}, err
	}
	c.Type = wire.CardKind(kind)
	if err = json.Unmarshal([]byte(apps), &c.Apps); err != nil {
		return Card{}, fmt.Errorf("assistantdomain: card %s's apps: %w", id, err)
	}
	body, err := r.sealer.Open(sealed, AAD(c.Conversation, RecordCard, c.ID, ColumnBody))
	if err != nil {
		return Card{}, fmt.Errorf("card %s: %w", id, err)
	}
	if err := json.Unmarshal(body, &c.Card); err != nil {
		return Card{}, fmt.Errorf("assistantdomain: card %s: %w", id, err)
	}
	if c.Card.Type.Valid() {
		if err := c.Card.Validate(); err != nil {
			// WHICH CARD AND WHY, NEVER ITS BODY: the body is what a person
			// was shown, sealed for them alone.
			r.logger.Warn("assistant_card_shown_as_notice", "card", c.ID.String(),
				"type", string(c.Card.Type), "error", err.Error())
			if r.metrics != nil {
				r.metrics.Add(metrics.AssistantCardsShownAsNotice, 1,
					metrics.Attrs{"type": string(c.Card.Type)})
			}
			c.Type, c.Card = wire.CardNotice, UnshownCardNotice()
		}
	}
	return c, nil
}

// UnshownCardNotice is what a stored card of a kind this build draws, but in
// a shape its contract refuses, is read as: a plain notice in its place,
// never the body.
//
// A CARD IS VALIDATED BEFORE IT IS RECORDED and never rewritten after, so a
// stored card this build's [wire.Card.Validate] refuses was written to
// another contract of an unreleased surface: an earlier one (a brief before
// ADR-0062), or, during a rolling upgrade, a newer node's. Either way it is
// not read, migrated or tolerated field by field: the transcript, the
// dashboard and the conversation the model re-reads all see this notice, and
// asking again writes the card in the shape this node reads. A kind this
// build does not know at all passes through untouched, as the dashboard
// already draws one it cannot. Each one is logged, by id, kind and the
// contract's reason, and counted.
func UnshownCardNotice() wire.Card {
	return wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{Tone: wire.NoticeInfo,
		Text: "This answer can't be shown here. Ask again to see it now."}}
}

// proposalColumns is every column a Proposal is read from, in
// [Reader.scanProposal]'s order.
const proposalColumns = `id, conversation_id, turn_id, owner_id, tool, act, scope, method,
	path, query_sealed, body_sealed, request_digest, digest, op_key, source_revision,
	step_up, card_sealed, state, expires_at, created_at, decided_at, settled_at,
	outcome, position, receipt_sealed, refusal_code`

// scanProposal reads one proposal, every sealed value opened.
func (r *Reader) scanProposal(row rowScanner) (Proposal, error) {
	var (
		p                                           Proposal
		id, conversation, turn, owner, scope, state string
		query, body, card, receipt                  string
		stepUp                                      int
		expires, created, decided, settled          int64
	)
	if err := row.Scan(&id, &conversation, &turn, &owner, &p.Tool, &p.Act, &scope,
		&p.Method, &p.Path, &query, &body, &p.RequestDigest, &p.Digest, &p.OpKey,
		&p.SourceRevision, &stepUp, &card, &state, &expires, &created, &decided, &settled,
		&p.Outcome, &p.Position, &receipt, &p.RefusalCode); err != nil {
		return Proposal{}, err
	}
	var err error
	if p.ID, p.Conversation, p.Turn, err = parseThree(id, conversation, turn); err != nil {
		return Proposal{}, err
	}
	if p.Owner, err = uuid.Parse(owner); err != nil {
		return Proposal{}, err
	}
	open := func(sealed, column string) ([]byte, error) {
		plain, openErr := r.sealer.Open(sealed, AAD(p.Conversation, RecordProposal, p.ID, column))
		if openErr != nil {
			return nil, fmt.Errorf("proposal %s's %s: %w", id, column, openErr)
		}
		return plain, nil
	}
	rawQuery, err := open(query, ColumnQuery)
	if err != nil {
		return Proposal{}, err
	}
	if p.Query, err = url.ParseQuery(string(rawQuery)); err != nil {
		return Proposal{}, fmt.Errorf("assistantdomain: proposal %s's query: %w", id, err)
	}
	if p.Body, err = open(body, ColumnBody); err != nil {
		return Proposal{}, err
	}
	rawCard, err := open(card, ColumnCard)
	if err != nil {
		return Proposal{}, err
	}
	if err = json.Unmarshal(rawCard, &p.Card); err != nil {
		return Proposal{}, fmt.Errorf("assistantdomain: proposal %s's card: %w", id, err)
	}
	rawReceipt, err := open(receipt, ColumnReceipt)
	if err != nil {
		return Proposal{}, err
	}
	if len(rawReceipt) > 0 {
		var s settlement
		if err := json.Unmarshal(rawReceipt, &s); err != nil {
			return Proposal{}, fmt.Errorf("assistantdomain: proposal %s's receipt: %w", id, err)
		}
		p.Receipt, p.Failure = s.Receipt, s.Failure
	}
	p.Scope, p.State, p.StepUp = wire.ProposalScope(scope), wire.ProposalState(state), stepUp != 0
	p.ExpiresAt, p.CreatedAt = store.DecodeTime(expires), store.DecodeTime(created)
	if decided > 0 {
		p.DecidedAt = store.DecodeTime(decided)
	}
	if settled > 0 {
		p.SettledAt = store.DecodeTime(settled)
	}
	return p, nil
}

// scanAuditRead reads one compliance read of conversation.
func (r *Reader) scanAuditRead(row rowScanner, conversation uuid.UUID) (AuditRead, error) {
	var (
		a                    AuditRead
		kind, sealed, record string
		at                   int64
	)
	if err := row.Scan(&a.Reader.Name, &kind, &a.Reader.OperatorID, &sealed, &record, &at); err != nil {
		return AuditRead{}, err
	}
	a.Reader.Kind, a.At = iam.ActorKind(kind), store.DecodeTime(at)
	var err error
	a.Reason, err = r.sealer.openString(sealed,
		AAD(conversation, RecordAuditRead, auditRecordID(record), ColumnReason))
	if err != nil {
		return AuditRead{}, fmt.Errorf("a compliance read of %s: %w", conversation, err)
	}
	return a, nil
}

// openPage opens a page chip, nil for none.
func (r *Reader) openPage(sealedTitle, screen, aad string) (*wire.PageChip, error) {
	if sealedTitle == "" && screen == "" {
		return nil, nil
	}
	title, err := r.sealer.openString(sealedTitle, aad)
	if err != nil {
		return nil, fmt.Errorf("a page chip: %w", err)
	}
	return &wire.PageChip{Title: title, Screen: screen}, nil
}

// parseThree parses three stored ids.
func parseThree(a, b, c string) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	x, err := uuid.Parse(a)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("assistantdomain: a stored id %q: %w", a, err)
	}
	y, err := uuid.Parse(b)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("assistantdomain: a stored id %q: %w", b, err)
	}
	z, err := uuid.Parse(c)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("assistantdomain: a stored id %q: %w", c, err)
	}
	return x, y, z, nil
}
