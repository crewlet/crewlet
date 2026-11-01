package assistant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tracing"
)

// A PERSON'S CONVERSATIONS, every read and write keyed on them: a
// conversation somebody else owns is one that does not exist (ADR-0053).

// Conversations is one page of p's conversations, the most recently used
// first.
func (s *Service) Conversations(ctx context.Context, p iam.Principal, cursor string,
	limit int) (wire.ConversationList, error) {

	if err := s.eligible(p); err != nil {
		return wire.ConversationList{}, err
	}
	list, err := s.transcripts.Conversations(ctx, p.ID, cursor, limit)
	if err != nil {
		return wire.ConversationList{}, s.readFailure(err)
	}
	if list.Conversations == nil {
		list.Conversations = []wire.ConversationSummary{}
	}
	return list, nil
}

// conversationNamespace is the UUIDv5 namespace a conversation's id is derived
// in from the operation that created it, so a retried create is one
// conversation.
var conversationNamespace = uuid.MustParse("b6e1d4a2-7c3f-5a98-8d2e-4f1a6c9b3e57")

// Create starts a conversation for p under the operation key, and answers it.
// A retry under the same key is the same conversation.
func (s *Service) Create(ctx context.Context, p iam.Principal, key string) (wire.ConversationSummary, error) {
	if err := s.eligible(p); err != nil {
		return wire.ConversationSummary{}, err
	}
	id := uuid.NewSHA1(conversationNamespace, []byte(key))
	w := Write{Owner: p.ID, By: iam.ActorFor(p), Key: key, Steps: []string{"create"}}
	if err := s.records.CreateConversation(ctx, w, id, ""); err != nil {
		return wire.ConversationSummary{}, s.writeFailure(err)
	}
	c, err := s.transcripts.Conversation(ctx, p.ID, id)
	if err != nil {
		return wire.ConversationSummary{}, s.readFailure(err)
	}
	return c.Summary(), nil
}

// Transcript is one of p's conversations as they read it after a reload: its
// messages, steps and cards, the turn still open, and every compliance read
// of it. A card whose app p no longer holds is replaced by a notice, and a
// proposal card carries its state now.
func (s *Service) Transcript(ctx context.Context, p iam.Principal, id uuid.UUID) (wire.Transcript, error) {
	if err := s.eligible(p); err != nil {
		return wire.Transcript{}, err
	}
	t, err := s.transcripts.Transcript(ctx, p.ID, id)
	if err != nil {
		return wire.Transcript{}, s.readFailure(err)
	}
	// A TURN PARKED ON PROPOSALS THAT ALL EXPIRED reads as expired, never
	// as still waiting: it is closed here and the transcript read again.
	current, err := s.lapseOpen(ctx, p.ID, t)
	if err != nil {
		return wire.Transcript{}, err
	}
	if !current {
		if t, err = s.transcripts.Transcript(ctx, p.ID, id); err != nil {
			return wire.Transcript{}, s.readFailure(err)
		}
	}
	return s.render(t, p), nil
}

// render is a conversation as reader is shown it: its messages, steps and
// cards, the turn still open and every compliance read of it, with every
// message and card built from an app the reader no longer holds replaced by a
// notice and every proposal card at its state now. The owner's reload and an administrator's
// compliance read are rendered by this one rule.
func (s *Service) render(t assistantdomain.Transcript, reader iam.Principal) wire.Transcript {
	id := t.Conversation.ID
	out := wire.Transcript{Conversation: t.Conversation.Summary(), Messages: []wire.Message{},
		Steps: []wire.StepEvent{}, Cards: []wire.CardEvent{}, AuditReads: []wire.AuditRead{}}
	for _, m := range t.Messages {
		out.Messages = append(out.Messages, shownMessage(m, reader).Wire())
	}
	out.Steps = append(out.Steps, t.Steps...)
	proposals := map[uuid.UUID]assistantdomain.Proposal{}
	for _, pr := range t.Proposals {
		proposals[pr.ID] = pr
	}
	for _, c := range t.Cards {
		out.Cards = append(out.Cards, shown(c, reader, proposals))
	}
	for _, a := range t.AuditReads {
		out.AuditReads = append(out.AuditReads, a.Wire())
	}
	out.Turns = make([]wire.TurnOutcome, 0, len(t.Turns))
	for _, turn := range t.Turns {
		outcome := wire.TurnOutcome{TurnID: turn.ID, State: turn.State}
		if code := wire.ErrorCode(turn.ErrorCode); code.Valid() {
			outcome.Error = &wire.ErrorEvent{Code: code, Message: code.Sentence()}
		}
		out.Turns = append(out.Turns, outcome)
	}
	if open := t.Conversation.OpenTurn; open != uuid.Nil {
		state := wire.TurnRunning
		for _, turn := range t.Turns {
			if turn.ID == open {
				state = turn.State
			}
		}
		var after uint64
		if l, ok := s.hub.Get(open); ok {
			after = l.Last()
		}
		out.OpenTurn = &wire.OpenTurn{TurnID: open, State: state,
			StreamURL: StreamPath(id, open), AfterSeq: after}
	}
	return out
}

// shown is a stored card as reader is shown it now: a notice in its place
// when they no longer hold every app it was built from ([cards.Visible]), and a
// proposal card at its proposal's state now.
func shown(c assistantdomain.Card, reader iam.Principal,
	proposals map[uuid.UUID]assistantdomain.Proposal) wire.CardEvent {

	ev := c.Event()
	visible, ok := cards.Visible(c.Apps, ev.Card, reader)
	ev.Card = visible
	if !ok {
		return ev
	}
	if ev.Card.Type == wire.CardProposal && ev.Card.Proposal != nil {
		if pr, ok := proposals[ev.Card.Proposal.ProposalID]; ok {
			proposal := *ev.Card.Proposal
			proposal.State, proposal.Receipt, proposal.Failure = pr.State, pr.Receipt, pr.Failure
			ev.Card.Proposal = &proposal
		}
	}
	return ev
}

// Table is one of p's table cards, for its export: from the snapshot the
// conversation stored, never a fresh read. A card p no longer holds every app
// of, and a card that is not a table, are not found.
func (s *Service) Table(ctx context.Context, p iam.Principal, card uuid.UUID) (wire.TableCard, error) {
	if err := s.eligible(p); err != nil {
		return wire.TableCard{}, err
	}
	c, err := s.transcripts.Card(ctx, p.ID, card)
	if err != nil {
		return wire.TableCard{}, s.readFailure(err)
	}
	visible, ok := cards.Visible(c.Apps, c.Card, p)
	if !ok || visible.Type != wire.CardTable || visible.Table == nil {
		return wire.TableCard{}, ErrNotFound
	}
	return *visible.Table, nil
}

// Clear purges one of p's conversations at once, the Clear gesture: a turn of
// it running here is stopped, every grant of its open turn is ended, and the
// purge record keeps any later record about it from writing a row.
func (s *Service) Clear(ctx context.Context, p iam.Principal, id uuid.UUID, key string) (wire.PurgeAnswer, error) {
	if err := s.eligible(p); err != nil {
		return wire.PurgeAnswer{}, err
	}
	c, err := s.transcripts.Conversation(ctx, p.ID, id)
	if err != nil {
		return wire.PurgeAnswer{}, s.readFailure(err)
	}
	if open := c.OpenTurn; open != uuid.Nil {
		if t, ok := s.local(p.ID, open); ok {
			t.stop()
		}
		s.revoker.RevokeTurn(open)
	}
	written, err := s.records.ClearConversation(ctx, Write{Owner: p.ID, By: iam.ActorFor(p),
		Key: key, Steps: []string{"clear"}}, id)
	if err != nil {
		return wire.PurgeAnswer{}, s.writeFailure(err)
	}
	if c.OpenTurn != uuid.Nil {
		s.hub.Forget(c.OpenTurn)
	}
	answer := wire.PurgeAnswer{Outcome: written.Outcome}
	if written.Position != "" {
		position := written.Position
		answer.Position = &position
	}
	return answer, nil
}

// title names a conversation after its first answer, in the background: one
// model call, recorded on the ledger and charged through the turn's meter,
// best effort. A title that cannot be had leaves the conversation named by its
// first question, which its first turn recorded ([Service.Start]). ctx is the turn's, detached
// so the title outlives the segment that asked for it and keeps its trace.
func (s *Service) title(ctx context.Context, t *turn, question, answer string,
	meter toolloop.BudgetMeter) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.titlesWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.titlesWG.Done()
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), TitleTimeout)
		defer cancel()
		if err := s.nameConversation(bounded, t, question, answer, meter); err != nil {
			s.log.InfoContext(bounded, "assistant_title_skipped", "conversation", t.conversation,
				"reason", redact.Secrets(err.Error()))
		}
	}()
}

// nameConversation makes the title call and records the title.
func (s *Service) nameConversation(ctx context.Context, t *turn, question, answer string,
	meter toolloop.BudgetMeter) error {

	if err := toolloop.Refusal(ctx, meter); err != nil {
		return err
	}
	use := auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAssistantTitle,
		TurnID: t.id.String()}
	provider, _, err := s.models.Chain(ctx, t.person, use)
	if err != nil {
		return err
	}
	completion, err := provider.Complete(ctx, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: titleInstruction},
		{Role: llm.RoleUser, Content: "Question: " + textcut.Within(question, 2000) +
			"\n\nAnswer: " + textcut.Within(answer, 2000)},
	}})
	if completion != nil && meter != nil {
		if _, spendErr := meter.Spend(context.WithoutCancel(ctx),
			completion.InputTokens+completion.OutputTokens); spendErr != nil {
			s.log.WarnContext(ctx, "assistant_title_uncounted", "error", spendErr)
		}
	}
	if err != nil {
		return err
	}
	title := conversationTitle(strings.Trim(strings.TrimSpace(completion.Content), "\"'"))
	if title == "" {
		return errors.New("the model answered no title")
	}
	return s.records.TitleConversation(ctx, Write{Owner: t.person.ID, By: t.by, Key: t.key,
		Steps: []string{"title"}}, t.conversation, title)
}

// conversationTitle is text as a conversation's title, the ONE rule for both
// titles a conversation gets (its first question at once, the model's once an
// answer completes): redacted, on one line, and at most [TitleMaxRunes], cut
// at a word where one is near the end and marked as cut.
func conversationTitle(text string) string {
	title := oneLine(redact.Secrets(text))
	runes := []rune(title)
	if len(runes) <= TitleMaxRunes {
		return title
	}
	cut := string(runes[:TitleMaxRunes-1])
	if space := strings.LastIndexByte(cut, ' '); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// titleInstruction is the title call's whole instruction.
const titleInstruction = "Write a title of at most eight words for this conversation " +
	"between a person and Ask Crewlet. Answer with the title alone: no quotes, no " +
	"full stop, no explanation."

// traceOf is the trace a turn runs under, for its row and its done frame.
func traceOf(ctx context.Context) string { return tracing.TraceOf(ctx).TraceID }

// Close stops every turn this node runs, each ending interrupted, and waits
// for their ends to be recorded and for any title in flight, within ctx.
//
// A START ADMITTED BEFORE CLOSE IS WAITED FOR: once closed, nothing more is
// admitted, and every start or resume already admitted either tracks its
// runner (which meets the cancelled base at once and ends interrupted) or
// gives up, before the runners are gathered.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel(errDraining)
	s.reaperWG.Wait()
	if err := waitWithin(ctx, s.admissions.Wait); err != nil {
		return err
	}
	s.mu.Lock()
	running := make([]*turn, 0, len(s.turns))
	for _, t := range s.turns {
		running = append(running, t)
	}
	s.mu.Unlock()
	for _, t := range running {
		select {
		case <-t.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	titled := make(chan struct{})
	go func() { s.titlesWG.Wait(); close(titled) }()
	select {
	case <-titled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(TitleTimeout):
		return nil
	}
}

// waitWithin runs wait (a WaitGroup's Wait) and answers once it returns, or
// ctx's error when ctx ends first.
func waitWithin(ctx context.Context, wait func()) error {
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
