package assistanttest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// Store is the assistant's own domain in memory, both sides, keyed on the
// owner as the real one is: a conversation, turn or card somebody else owns is
// assistantdomain.ErrNotFound, one turn runs or parks per conversation, and a
// closed turn cannot close twice. Every record it was handed is kept, raw, so
// a suite can assert what never reached one.
type Store struct {
	now func() time.Time

	mu            sync.Mutex
	conversations map[uuid.UUID]*storedConversation
	turns         map[uuid.UUID]*storedTurn
	parked        map[uuid.UUID][]byte
	proposals     map[uuid.UUID]assistantdomain.Proposal
	records       []string
	ops           map[string]bool
}

type storedConversation struct {
	assistantdomain.Conversation
	messages []assistantdomain.Message
	steps    []wire.StepEvent
	cards    []assistantdomain.Card
	turns    []uuid.UUID
}

type storedTurn struct{ assistantdomain.Turn }

var (
	_ assistant.Transcripts = (*Store)(nil)
	_ assistant.Records     = (*Store)(nil)
)

// NewStore is an empty store on clock now.
func NewStore(now func() time.Time) *Store {
	return &Store{now: now, conversations: map[uuid.UUID]*storedConversation{},
		turns: map[uuid.UUID]*storedTurn{}, parked: map[uuid.UUID][]byte{},
		proposals: map[uuid.UUID]assistantdomain.Proposal{},
		ops:       map[string]bool{}}
}

// Records is every record the store was handed, each as JSON.
func (s *Store) Records() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records)
}

// Seed makes a conversation for owner directly, the way a create would.
func (s *Store) Seed(owner, id uuid.UUID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.now().UTC()
	s.conversations[id] = &storedConversation{Conversation: assistantdomain.Conversation{
		ID: id, Owner: owner, Title: title, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}}
}

// keep records one write, refusing a write that names no operation.
func (s *Store) keep(w assistant.Write, what string, v any) error {
	if w.Key == "" && w.Derived == nil {
		return fmt.Errorf("assistanttest: the %s record names no operation", what)
	}
	raw, err := json.Marshal(map[string]any{"record": what, "write": w, "value": v})
	if err != nil {
		return err
	}
	s.records = append(s.records, string(raw))
	return nil
}

// owned is a conversation the owner holds, under s.mu.
func (s *Store) owned(owner, id uuid.UUID) (*storedConversation, error) {
	c, ok := s.conversations[id]
	if !ok || c.Owner != owner {
		return nil, fmt.Errorf("%w: conversation %s", assistantdomain.ErrNotFound, id)
	}
	return c, nil
}

// ownedTurn is a turn the owner holds, under s.mu.
func (s *Store) ownedTurn(owner, id uuid.UUID) (*storedTurn, error) {
	t, ok := s.turns[id]
	if !ok || t.Owner != owner {
		return nil, fmt.Errorf("%w: turn %s", assistantdomain.ErrNotFound, id)
	}
	return t, nil
}

func (s *Store) Conversations(_ context.Context, owner uuid.UUID, _ string, _ int) (wire.ConversationList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := wire.ConversationList{Conversations: []wire.ConversationSummary{}}
	for _, c := range s.conversations {
		if c.Owner == owner {
			out.Conversations = append(out.Conversations, c.Summary())
		}
	}
	slices.SortFunc(out.Conversations, func(a, b wire.ConversationSummary) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return out, nil
}

func (s *Store) Conversation(_ context.Context, owner, id uuid.UUID) (assistantdomain.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.owned(owner, id)
	if err != nil {
		return assistantdomain.Conversation{}, err
	}
	return c.Conversation, nil
}

func (s *Store) Transcript(_ context.Context, owner, id uuid.UUID) (assistantdomain.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.owned(owner, id)
	if err != nil {
		return assistantdomain.Transcript{}, err
	}
	t := assistantdomain.Transcript{Conversation: c.Conversation,
		Messages: slices.Clone(c.messages), Steps: slices.Clone(c.steps),
		Cards: slices.Clone(c.cards)}
	for _, turn := range c.turns {
		t.Turns = append(t.Turns, s.turns[turn].Turn)
	}
	for _, p := range s.proposals {
		if p.Conversation == id {
			t.Proposals = append(t.Proposals, p)
		}
	}
	return t, nil
}

func (s *Store) Turn(_ context.Context, owner, id uuid.UUID) (assistantdomain.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedTurn(owner, id)
	if err != nil {
		return assistantdomain.Turn{}, err
	}
	return t.Turn, nil
}

func (s *Store) Parked(_ context.Context, owner, turn uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ownedTurn(owner, turn); err != nil {
		return nil, err
	}
	raw, ok := s.parked[turn]
	if !ok {
		return nil, fmt.Errorf("%w: parked turn %s", assistantdomain.ErrNotFound, turn)
	}
	return slices.Clone(raw), nil
}

func (s *Store) Card(_ context.Context, owner, id uuid.UUID) (assistantdomain.Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conversations {
		if c.Owner != owner {
			continue
		}
		for _, card := range c.cards {
			if card.ID == id {
				return card, nil
			}
		}
	}
	return assistantdomain.Card{}, fmt.Errorf("%w: card %s", assistantdomain.ErrNotFound, id)
}

func (s *Store) CreateConversation(_ context.Context, w assistant.Write, id uuid.UUID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "conversation_created", title); err != nil {
		return err
	}
	if c, ok := s.conversations[id]; ok {
		if c.Owner != w.Owner {
			return fmt.Errorf("%w: conversation %s", assistantdomain.ErrNotFound, id)
		}
		return nil
	}
	at := s.now().UTC()
	s.conversations[id] = &storedConversation{Conversation: assistantdomain.Conversation{
		ID: id, Owner: w.Owner, Title: title, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}}
	return nil
}

func (s *Store) TitleConversation(_ context.Context, w assistant.Write, id uuid.UUID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "conversation_titled", title); err != nil {
		return err
	}
	c, err := s.owned(w.Owner, id)
	if err != nil {
		return err
	}
	c.Title = title
	return nil
}

func (s *Store) ClearConversation(_ context.Context, w assistant.Write, id uuid.UUID) (assistant.Written, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "conversation_purged", id); err != nil {
		return assistant.Written{}, err
	}
	c, err := s.owned(w.Owner, id)
	if err != nil {
		return assistant.Written{}, err
	}
	for _, turn := range c.turns {
		delete(s.turns, turn)
		delete(s.parked, turn)
	}
	delete(s.conversations, id)
	return assistant.Written{Outcome: "applied", Position: "CREWLET_ASSISTANT_LOG@1:1"}, nil
}

func (s *Store) OpenTurn(_ context.Context, w assistant.Write, t assistantdomain.NewTurn) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "turn_opened", t); err != nil {
		return 0, err
	}
	c, err := s.owned(w.Owner, t.Conversation)
	if err != nil {
		return 0, err
	}
	switch c.OpenTurn {
	case t.Turn:
		return c.Turns, nil
	case uuid.Nil:
	default:
		return 0, fmt.Errorf("%w: turn %s", assistantdomain.ErrTurnRunning, c.OpenTurn)
	}
	c.Turns++
	c.OpenTurn = t.Turn
	c.turns = append(c.turns, t.Turn)
	c.LastTurnState = wire.TurnRunning
	s.turns[t.Turn] = &storedTurn{Turn: assistantdomain.Turn{ID: t.Turn,
		Conversation: t.Conversation, Owner: w.Owner, Seq: c.Turns, State: wire.TurnRunning,
		Node: t.Node, StartedAt: s.now().UTC(), PromptVersion: t.PromptVersion, Model: t.Model,
		ProviderKey: t.ProviderKey, TraceID: t.TraceID, Page: t.Page}}
	return c.Turns, nil
}

// child is the conversation a turn of the owner's belongs to, under s.mu.
func (s *Store) child(owner, turn uuid.UUID) (*storedConversation, *storedTurn, error) {
	t, err := s.ownedTurn(owner, turn)
	if err != nil {
		return nil, nil, err
	}
	c, err := s.owned(owner, t.Conversation)
	return c, t, err
}

func (s *Store) RecordMessage(_ context.Context, w assistant.Write, turn uuid.UUID, m assistantdomain.NewMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "message_recorded", m); err != nil {
		return err
	}
	c, _, err := s.child(w.Owner, turn)
	if err != nil {
		return err
	}
	c.messages = slices.DeleteFunc(c.messages, func(x assistantdomain.Message) bool { return x.ID == m.ID })
	c.messages = append(c.messages, assistantdomain.Message{ID: m.ID, Turn: turn, Role: m.Role,
		Text: m.Text, CreatedAt: s.now().UTC(), Page: m.Page})
	return nil
}

func (s *Store) RecordStep(_ context.Context, w assistant.Write, turn uuid.UUID, step wire.StepEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "step_recorded", step); err != nil {
		return err
	}
	c, _, err := s.child(w.Owner, turn)
	if err != nil {
		return err
	}
	c.steps = slices.DeleteFunc(c.steps, func(x wire.StepEvent) bool { return x.StepID == step.StepID })
	c.steps = append(c.steps, step)
	return nil
}

func (s *Store) RecordCard(_ context.Context, w assistant.Write, turn uuid.UUID, card wire.CardEvent, apps []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "card_recorded", card); err != nil {
		return err
	}
	c, _, err := s.child(w.Owner, turn)
	if err != nil {
		return err
	}
	c.cards = append(c.cards, assistantdomain.Card{ID: card.CardID, Conversation: c.ID,
		Turn: turn, Message: card.MessageID, Type: card.Card.Type, Apps: slices.Clone(apps),
		Card: card.Card})
	return nil
}

// move changes a turn's state from one of from, under s.mu.
func (s *Store) move(owner, turn uuid.UUID, to wire.TurnState, from ...wire.TurnState) (*storedConversation, *storedTurn, error) {
	c, t, err := s.child(owner, turn)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(from, t.State) {
		return nil, nil, &assistantdomain.TurnMoved{State: t.State}
	}
	t.State = to
	c.LastTurnState = to
	return c, t, nil
}

func (s *Store) ParkTurn(_ context.Context, w assistant.Write, turn uuid.UUID, parked []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "turn_parked", string(parked)); err != nil {
		return err
	}
	if _, _, err := s.move(w.Owner, turn, wire.TurnWaitingForApproval, wire.TurnRunning); err != nil {
		return err
	}
	s.parked[turn] = slices.Clone(parked)
	return nil
}

func (s *Store) ResumeTurn(_ context.Context, w assistant.Write, turn uuid.UUID, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "turn_resumed", node); err != nil {
		return err
	}
	_, t, err := s.move(w.Owner, turn, wire.TurnRunning, wire.TurnWaitingForApproval)
	if err == nil {
		t.Node = node
	}
	return err
}

func (s *Store) CloseTurn(_ context.Context, w assistant.Write, turn uuid.UUID, end assistantdomain.TurnEnd) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "turn_closed", end); err != nil {
		return err
	}
	c, t, err := s.move(w.Owner, turn, end.State, wire.TurnRunning, wire.TurnWaitingForApproval)
	if err != nil {
		return err
	}
	t.Steps, t.ErrorCode, t.EndedAt = end.Steps, end.ErrorCode, s.now().UTC()
	c.OpenTurn = uuid.Nil
	delete(s.parked, turn)
	return nil
}

// CreateProposal keeps a proposal a turn of the owner's made.
func (s *Store) CreateProposal(_ context.Context, w assistant.Write, p assistantdomain.NewProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keep(w, "proposal_created", p); err != nil {
		return err
	}
	c, _, err := s.child(w.Owner, p.Turn)
	if err != nil {
		return err
	}
	s.proposals[p.Proposal] = assistantdomain.Proposal{ID: p.Proposal, Conversation: c.ID,
		Turn: p.Turn, Owner: w.Owner, Tool: p.Tool, Act: p.Act, Scope: p.Scope,
		Method: p.Method, Path: p.Path, Query: p.Query, Body: p.Body,
		RequestDigest: p.RequestDigest, Digest: p.Digest, OpKey: p.OpKey,
		SourceRevision: p.SourceRevision, StepUp: p.StepUp, Card: p.Card,
		State: wire.ProposalProposed, ExpiresAt: p.ExpiresAt, CreatedAt: s.now().UTC()}
	return nil
}

// Proposals is every proposal the store keeps.
func (s *Store) Proposals() []assistantdomain.Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]assistantdomain.Proposal, 0, len(s.proposals))
	for _, p := range s.proposals {
		out = append(out, p)
	}
	return out
}
