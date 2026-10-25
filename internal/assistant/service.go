package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Deps is what a [Service] is built from. Every field but Proposer and Remote
// is REQUIRED, and a nil is refused by name: the assistant runs beside an
// engine that holds all of them, so a missing one is a wiring mistake a
// narrower assistant would hide.
type Deps struct {
	// Node is this node's id, recorded on every turn it runs.
	Node string

	Company Company
	// Acts is the authority table's verdict on which writes a person may
	// propose (internal/api/operator's Server.Acts): the act verbs it can
	// admit them to before any object is named.
	Acts        func(ctx context.Context, p iam.Principal) ([]string, error)
	Models      Models
	Budget      Budget
	Catalogue   Catalogue
	Cards       Cards
	Transcripts Transcripts
	Records     Records
	Dispatcher  exec.Dispatcher
	Revoker     Revoker
	Audit       AuditPublisher
	Metrics     Metrics

	// Proposer turns a write into a proposal; nil offers no write
	// (internal/assistant/proposal).
	Proposer Proposer
	// Remote follows and stops turns running on other nodes; nil serves
	// this node's own.
	Remote Remote

	// Now is injectable so a test pins every instant.
	Now func() time.Time
}

// Service runs a node's Dashboard Assistant turns: it starts them, streams
// them, stops them and answers a person's conversations.
type Service struct {
	node        string
	company     Company
	acts        func(ctx context.Context, p iam.Principal) ([]string, error)
	models      Models
	budget      Budget
	catalogue   Catalogue
	cards       Cards
	transcripts Transcripts
	records     Records
	dispatcher  exec.Dispatcher
	revoker     Revoker
	audit       AuditPublisher
	metrics     Metrics
	proposer    Proposer
	remote      Remote
	now         func() time.Time
	log         *slog.Logger

	hub     *live.Hub
	limiter *costlimit.Limiter

	// base is every turn's parent context, cancelled with errDraining when
	// the node stops.
	base   context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	decliner Decliner
	turns    map[uuid.UUID]*turn
	running  map[uuid.UUID]int
	streams  int
	closed   bool
	titlesWG sync.WaitGroup
}

// turnNamespace is the UUIDv5 namespace a turn's id is derived in from the
// operation it was asked under, so a retry of one question is one turn.
var turnNamespace = uuid.MustParse("3f9d6a1c-8e2b-5c47-a1d9-6b0e4f2c8a73")

// New builds the service.
func New(d Deps) (*Service, error) {
	var missing []string
	for name, absent := range map[string]bool{
		"Node": strings.TrimSpace(d.Node) == "", "Company": d.Company == nil,
		"Acts":   d.Acts == nil,
		"Models": d.Models == nil, "Budget": d.Budget == nil,
		"Catalogue": d.Catalogue == nil, "Cards": d.Cards == nil,
		"Transcripts": d.Transcripts == nil, "Records": d.Records == nil,
		"Dispatcher": d.Dispatcher == nil, "Revoker": d.Revoker == nil,
		"Audit": d.Audit == nil, "Metrics": d.Metrics == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("assistant: Deps.%s required: the assistant runs beside an "+
			"engine that holds every one of them", strings.Join(missing, ", Deps."))
	}
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	base, cancel := context.WithCancelCause(context.Background())
	return &Service{
		node: d.Node, company: d.Company, acts: d.Acts, models: d.Models, budget: d.Budget,
		catalogue: d.Catalogue, cards: d.Cards, transcripts: d.Transcripts,
		records: d.Records, dispatcher: d.Dispatcher, revoker: d.Revoker, audit: d.Audit,
		metrics: d.Metrics, proposer: d.Proposer, remote: d.Remote, now: now,
		log: logging.Get("assistant"),
		hub: live.NewHub(now, config.Assistant{ProposalTTLSeconds: maxProposalTTLSeconds}.ProposalTTL()),
		limiter: costlimit.New(costlimit.Rates{
			PrincipalRate: TurnsPerMinute / 60.0, PrincipalBurst: TurnBurst,
			ScopeRate: NodeTurnsPerSecond, ScopeBurst: NodeTurnBurst,
		}, now),
		base: base, cancel: cancel,
		turns: map[uuid.UUID]*turn{}, running: map[uuid.UUID]int{},
	}, nil
}

// maxProposalTTLSeconds is the longest proposal lifetime a company may set
// (`assistant.proposal_ttl_seconds`): a parked turn's numbering is kept that
// long.
const maxProposalTTLSeconds = 3600

// turnRateScope is the costlimit scope every turn start is taken from.
const turnRateScope = "assistant.turn"

// Eligible says whether p may use the assistant on this node now, and the
// first reason not, in the order `/viewer` reports them: the company has not
// turned it on, it has no usable model chain, or p is not a person signed in
// through their own browser. The authority table's own verdict is the HTTP
// surface's, which holds the chart.
func (s *Service) Eligible(p iam.Principal) (bool, wire.ViewerReason) {
	c, _ := s.company()
	if c == nil || !c.Assistant.On() {
		return false, wire.ViewerDisabled
	}
	if _, err := c.AssistantChain(); err != nil {
		return false, wire.ViewerNoModel
	}
	if iam.Assistable(p) != "" {
		return false, wire.ViewerNotAPerson
	}
	return true, ""
}

// eligible is [Service.Eligible] as an error.
func (s *Service) eligible(p iam.Principal) error {
	if ok, reason := s.Eligible(p); !ok {
		detail := iam.Assistable(p)
		if detail == "" {
			detail = string(reason)
		}
		return &NotAvailable{Reason: reason, Detail: detail}
	}
	return nil
}

// TurnStart is one question.
type TurnStart struct {
	// Person is the resolved person asking.
	Person       iam.Principal
	Conversation uuid.UUID
	// Key is the request's Idempotency-Key, scoped to the person: the same
	// key is the same turn.
	Key   string
	Input string
	// Page is the validated page context, nil when the person removed it.
	Page *wire.PageContext
	// Read is the read grant derived from THIS request.
	Read exec.Grant
}

// TurnID is the turn a question asked under the operation key starts: the
// same key is always the same turn, so a retry of one question is one turn,
// and the HTTP surface can derive the turn's grant before the turn exists.
func TurnID(key string) uuid.UUID { return uuid.NewSHA1(turnNamespace, []byte(key)) }

// StreamPath is where a client reads a turn of a conversation.
func StreamPath(conversation, turn uuid.UUID) string {
	return "/assistant/conversations/" + conversation.String() + "/stream?turn=" + turn.String()
}

// Start starts a turn and answers where to read it. Nothing is spent and
// nothing is recorded until the person may use the assistant, their load
// allows another turn, the company's windows have room and a model chain
// resolves.
func (s *Service) Start(ctx context.Context, ts TurnStart) (wire.TurnAccepted, error) {
	// A GRANT NO RUNNER TAKES IS ENDED HERE: every return below that starts
	// nothing (a retry answered with the turn it already started, a refusal,
	// a failure) leaves the grant derived for this request revoked rather
	// than live for its five minutes.
	started := false
	defer func() {
		if !started {
			if ts.Read != nil {
				s.revoker.Revoke(ts.Read.ID())
			}
		}
	}()
	if err := s.eligible(ts.Person); err != nil {
		return wire.TurnAccepted{}, err
	}
	input := strings.TrimSpace(ts.Input)
	switch {
	case input == "":
		return wire.TurnAccepted{}, invalid("input.text is empty: ask a question")
	case utf8.RuneCountInString(input) > wire.InputMaxRunes:
		return wire.TurnAccepted{}, invalid("input.text is %d characters and the most a "+
			"question may be is %d", utf8.RuneCountInString(input), wire.InputMaxRunes)
	case ts.Read == nil:
		return wire.TurnAccepted{}, errors.New("assistant: a turn needs the read grant " +
			"derived from the request that asked it")
	}
	if ts.Page != nil {
		if err := ts.Page.Validate(); err != nil {
			return wire.TurnAccepted{}, invalid("page_context: %v", err)
		}
	}
	input = redact.Secrets(input)
	id := TurnID(ts.Key)
	accepted := wire.TurnAccepted{TurnID: id, StreamURL: StreamPath(ts.Conversation, id)}

	// THE SAME KEY IS THE SAME TURN: a retry whose first attempt landed
	// answers the turn it started, wherever it is now.
	if s.runningHere(id) {
		return accepted, nil
	}
	switch _, err := s.transcripts.Turn(ctx, ts.Person.ID, id); {
	case err == nil:
		return accepted, nil
	case !errors.Is(err, assistantdomain.ErrNotFound):
		return wire.TurnAccepted{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
	}

	history, err := s.transcripts.Transcript(ctx, ts.Person.ID, ts.Conversation)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	if history.Conversation.OpenTurn != uuid.Nil {
		return wire.TurnAccepted{}, ErrTurnRunning
	}
	acts, err := s.acts(ctx, ts.Person)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
	}
	if err := s.admit(ts.Person); err != nil {
		return wire.TurnAccepted{}, err
	}
	admitted := true
	defer func() {
		if admitted {
			s.unadmit(ts.Person.ID)
		}
	}()

	meter, err := s.budget.Meter(ctx, ts.Person)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
	}
	if refusal := budgetRefusal(ctx, meter); refusal != nil {
		return wire.TurnAccepted{}, refusal
	}
	use := auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAssistantTurn,
		TurnID: id.String()}
	provider, key, err := s.models.Chain(ctx, ts.Person, use)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{Cause: err}
	}

	t := s.newTurn(ts.Person, ts.Conversation, id, ts.Key, ts.Read, ts.Page, acts)
	var chip *wire.PageChip
	if ts.Page != nil {
		chip = &wire.PageChip{Title: ts.Page.Title, Screen: ts.Page.Screen}
	}
	if _, err := s.records.OpenTurn(ctx, t.write().Step("open"), assistantdomain.NewTurn{
		Conversation: ts.Conversation, Turn: id, Node: s.node, PromptVersion: PromptVersion,
		Model: provider.Model(), ProviderKey: key, TraceID: traceOf(ctx), Page: chip,
	}); err != nil {
		t.cancel(nil)
		return wire.TurnAccepted{}, s.writeFailure(err)
	}
	t.messages = 1
	if err := s.records.RecordMessage(ctx, t.write().Step("message", "question"), id,
		assistantdomain.NewMessage{ID: assistantdomain.MessageID(id, 1), Role: wire.RoleUser,
			Text: input, Page: chip}); err != nil {
		s.log.WarnContext(ctx, "assistant_record_failed", "turn", id, "record", "question",
			"error", redact.Secrets(err.Error()))
	}
	t.messages = 2
	t.answer = assistantdomain.MessageID(id, 2)

	c, organization := s.company()
	tools := s.catalogue.Offered(ts.Person, acts)
	in := promptInput{Person: ts.Person, Apps: iam.Expand(ts.Person.Grants), Now: s.now(),
		History: historyEntries(history), Page: ts.Page, Boundary: t.boundary, Question: input,
		Proposable: proposable(tools, s.proposer != nil)}
	if c != nil {
		in.Company, in.Zone = c.Name, c.Location()
	}
	if organization != nil {
		if seat := organization.SeatByHandle(ts.Person.Seat); seat != nil {
			in.Name = seat.Name
		}
	}
	s.openStream(t, history.Conversation, false)

	assistantCfg := config.Assistant{}
	if c != nil {
		assistantCfg = c.Assistant
	}
	admitted, started = false, true
	s.track(t)
	go t.run(loopRun{messages: buildMessages(in), provider: provider, key: key, meter: meter,
		tools: tools, steps: assistantCfg.Steps(), timeout: assistantCfg.TurnTimeout(),
		fresh: history.Conversation.Title == "", question: input})
	return accepted, nil
}

// newTurn builds a segment's runner state.
func (s *Service) newTurn(p iam.Principal, conversation, id uuid.UUID, key string,
	read exec.Grant, page *wire.PageContext, acts []string) *turn {

	ctx, cancel := context.WithCancelCause(s.base)
	return &turn{svc: s, person: p, by: iam.ActorFor(p), conversation: conversation, id: id,
		key: key, read: read, page: page, acts: acts, boundary: provenance.NewBoundary(),
		started: s.now(), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// openStream opens the segment's frame log and writes its opening frames,
// answering the seq a client that drew everything before it reads after.
func (s *Service) openStream(t *turn, c assistantdomain.Conversation, resumed bool) uint64 {
	t.log = s.hub.Open(t.conversation, t.id)
	after := t.log.Last()
	t.emit(wire.Frame{Kind: wire.EventConversation, Conversation: &wire.ConversationEvent{
		Title: c.Title, CreatedAt: c.CreatedAt, Resumed: resumed}})
	if !resumed {
		used := &wire.PageContextEvent{Used: t.page != nil}
		if t.page != nil {
			used.Page = &wire.PageChip{Title: t.page.Title, Screen: t.page.Screen}
		}
		t.emit(wire.Frame{Kind: wire.EventPageContext, PageContext: used})
	}
	t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
		State: wire.StatusThinking, StepsDone: t.steps}})
	return after
}

// historyEntries is a conversation's messages as the next turn re-reads them,
// each answer with its cards' one-line summaries.
func historyEntries(t assistantdomain.Transcript) []historyEntry {
	cards := map[uuid.UUID][]string{}
	for _, c := range t.Cards {
		cards[c.Message] = append(cards[c.Message], cardSummary(c.Card))
	}
	out := make([]historyEntry, 0, len(t.Messages))
	for _, m := range t.Messages {
		out = append(out, historyEntry{Role: m.Role, Text: m.Text, Cards: cards[m.ID]})
	}
	return out
}

// proposable is every write the person may propose: the offered tools that
// write, where the node has an approval path.
func proposable(tools []catalog.Tool, approvals bool) []catalog.Tool {
	if !approvals {
		return nil
	}
	var out []catalog.Tool
	for _, t := range tools {
		if t.Kind == catalog.KindWrite || t.Kind == catalog.KindPageAction {
			out = append(out, t)
		}
	}
	return out
}

// budgetRefusal is the refusal a meter holds before any round, or nil.
func budgetRefusal(ctx context.Context, meter toolloop.BudgetMeter) error {
	err := toolloop.Refusal(ctx, meter)
	if err == nil {
		return nil
	}
	var b *toolloop.BudgetError
	if errors.As(err, &b) {
		return &BudgetRefused{Scope: b.Scope, Window: b.Window, Used: b.Used, Limit: b.Limit,
			ResetsAt: b.ResetsAt}
	}
	return &BudgetRefused{}
}

// admit takes one turn for p from their running allowance and the turn rate,
// or answers why not.
func (s *Service) admit(p iam.Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &Unavailable{Cause: errDraining}
	}
	if s.running[p.ID] >= RunningTurnsPerPerson {
		return &Busy{RetryAfter: 5 * time.Second, Reason: fmt.Sprintf(
			"%d turns are running for this person; wait for one to finish",
			RunningTurnsPerPerson)}
	}
	if refused := s.limiter.Take(turnRateScope, p.ID.String(), 1); refused != nil {
		return &Busy{RetryAfter: refused.RetryAfter, Reason: "too many questions in a " +
			"short time; wait a moment"}
	}
	s.running[p.ID]++
	return nil
}

// unadmit gives back a turn [Service.admit] took for a start that failed
// before its runner began.
func (s *Service) unadmit(person uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[person]--; s.running[person] <= 0 {
		delete(s.running, person)
	}
}

// track registers a runner about to start.
func (s *Service) track(t *turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[t.id] = t
}

// release forgets a runner that ended and gives its person their turn back.
func (s *Service) release(t *turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turns[t.id] == t {
		delete(s.turns, t.id)
	}
	if s.running[t.person.ID]--; s.running[t.person.ID] <= 0 {
		delete(s.running, t.person.ID)
	}
	t.cancel(nil)
}

// runningHere reports whether this node runs turn id now.
func (s *Service) runningHere(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.turns[id]
	return ok
}

// local is the runner of turn id on this node, if it is the owner's.
func (s *Service) local(owner, id uuid.UUID) (*turn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.turns[id]
	if !ok || t.person.ID != owner {
		return nil, false
	}
	return t, true
}

// Stop stops one of the owner's turns: a running one is cancelled where it
// runs, and a parked one is closed stopped (its proposal can then no longer
// resume it). A turn already over answers the state it ended in.
//
// key is the operation the Stop was asked under (its Idempotency-Key, scoped
// to the person), which a parked turn's close and the declines of what it
// waited on are published under, so a retried Stop records nothing twice.
func (s *Service) Stop(ctx context.Context, owner, turnID uuid.UUID, key string) (wire.StopAnswer, error) {
	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return wire.StopAnswer{}, s.readFailure(err)
	}
	if t, ok := s.local(owner, turnID); ok {
		t.stop()
		select {
		case <-t.done:
		case <-ctx.Done():
		case <-time.After(stopWait):
		}
		return wire.StopAnswer{State: wire.TurnStopped}, nil
	}
	switch {
	case row.State.Final():
		return wire.StopAnswer{State: row.State}, nil
	case row.State == wire.TurnWaitingForApproval:
		p, how := iam.From(ctx)
		if how != iam.Resolved || p.ID != owner {
			return wire.StopAnswer{}, errors.New("assistant: a parked turn is stopped by " +
				"its owner's own request")
		}
		w := Write{Owner: owner, By: iam.ActorFor(p), Key: key, Steps: []string{"stop"}}
		err := s.records.CloseTurn(ctx, w, turnID, assistantdomain.TurnEnd{
			State: wire.TurnStopped, Steps: row.Steps})
		var moved *assistantdomain.TurnMoved
		switch {
		case errors.As(err, &moved):
			return wire.StopAnswer{State: moved.State}, nil
		case err != nil:
			return wire.StopAnswer{}, s.writeFailure(err)
		}
		s.revoker.RevokeTurn(turnID)
		s.declineParked(ctx, p, row, key)
		return wire.StopAnswer{State: wire.TurnStopped}, nil
	}
	if s.remote != nil {
		answer, answered, err := s.remote.Stop(ctx, owner, turnID)
		if err != nil {
			return wire.StopAnswer{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
		}
		if answered {
			return answer, nil
		}
	}
	return wire.StopAnswer{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: fmt.Errorf(
		"turn %s runs on node %s, which this node cannot reach", turnID, row.Node)}
}

// declineParked declines every proposal a stopped parked turn waited on, so
// none of them can be approved into a turn that has ended. A decline that does
// not land is logged: the turn is closed either way, and an approval of a
// proposal whose turn ended resumes nothing.
func (s *Service) declineParked(ctx context.Context, p iam.Principal, row assistantdomain.Turn,
	key string) {
	s.mu.Lock()
	d := s.decliner
	s.mu.Unlock()
	if d == nil {
		return
	}
	if err := d.DeclineTurn(ctx, approval.Stop{Owner: p.ID, Conversation: row.Conversation,
		Turn: row.ID, By: iam.ActorFor(p), Key: key}); err != nil {
		s.log.WarnContext(ctx, "assistant_stop_decline_failed", "turn", row.ID,
			"error", redact.Secrets(err.Error()))
	}
}

// stopWait is how long a Stop waits for the runner to record its end before
// answering: long enough for the loop to notice its cancelled context, which
// ends a model call in progress (a CLI is signalled at once).
const stopWait = 5 * time.Second

// Stream writes one of the owner's turns of conversation to w from after seq
// after, until the
// segment ends, the subscriber falls behind or ctx ends.
//
// A turn this node runs (or ran in the last [live.EndedRetention]) is served
// from its frame log; frames the log no longer holds are answered with a
// snapshot of the transcript. A turn running elsewhere is followed through
// [Remote]; with none, or when no node answers for it, the transcript is the
// answer, and a turn whose runner this node was is closed interrupted.
func (s *Service) Stream(ctx context.Context, owner, conversation, turnID uuid.UUID, after uint64,
	w FrameWriter) error {

	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return s.readFailure(err)
	}
	if row.Conversation != conversation {
		return fmt.Errorf("%w: turn %s is not in conversation %s", ErrNotFound, turnID, conversation)
	}
	s.streamOpened()
	defer s.streamClosed()
	if l, ok := s.hub.Get(turnID); ok {
		return s.follow(ctx, owner, row, l, after, w)
	}
	if !row.State.Ended() && row.Node != s.node && s.remote != nil {
		answered, err := s.remote.Follow(ctx, owner, turnID, after, w)
		if err != nil || answered {
			return err
		}
	}
	return s.answerFromTranscript(ctx, owner, row, after, w)
}

// follow serves a stream from this node's frame log.
func (s *Service) follow(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	l *live.Log, after uint64, w FrameWriter) error {

	replay, sub, gap := l.Subscribe(after)
	defer sub.Cancel()
	if gap {
		snap, err := s.snapshot(ctx, owner, row, after)
		if err != nil {
			return err
		}
		if err := w.Frame(snap); err != nil {
			return err
		}
		replay = slices.DeleteFunc(replay, func(f wire.Frame) bool { return f.Seq <= snap.Seq })
	}
	for _, f := range replay {
		if err := w.Frame(f); err != nil {
			return err
		}
	}
	keepalive := time.NewTicker(wire.KeepaliveSeconds * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-keepalive.C:
			if err := w.Keepalive(); err != nil {
				return err
			}
		case f, open := <-sub.C():
			if !open {
				return nil
			}
			if err := w.Frame(f); err != nil {
				return err
			}
		}
	}
}

// answerFromTranscript answers a stream no live log serves: a snapshot of what
// the transcript holds of the turn, then how it ended. A turn still open whose
// runner was this node died with it, and is closed interrupted once (the close
// is arbitrated, so of two nodes noticing, one records it).
func (s *Service) answerFromTranscript(ctx context.Context, owner uuid.UUID,
	row assistantdomain.Turn, after uint64, w FrameWriter) error {

	snap, err := s.snapshot(ctx, owner, row, after)
	if err != nil {
		return err
	}
	if err := w.Frame(snap); err != nil {
		return err
	}
	state := row.State
	var failure *wire.ErrorEvent
	switch {
	case state == wire.TurnRunning && row.Node == s.node:
		err := s.records.CloseTurn(ctx, Write{Owner: owner,
			By: iam.Actor{Name: "engine", Kind: iam.ActorSystem},
			Derived: &Derivation{At: row.StartedAt, Name: "assistant_interrupt",
				Identity: []string{row.ID.String()}}}, row.ID, assistantdomain.TurnEnd{
			State: wire.TurnInterrupted, Steps: row.Steps, ErrorCode: string(wire.ErrorTurnInterrupted)})
		var moved *assistantdomain.TurnMoved
		switch {
		case errors.As(err, &moved):
			state = moved.State
		case err != nil:
			s.log.WarnContext(ctx, "assistant_interrupt_unrecorded", "turn", row.ID,
				"error", redact.Secrets(err.Error()))
			state = wire.TurnInterrupted
		default:
			state = wire.TurnInterrupted
		}
		if state == wire.TurnInterrupted {
			failure = &wire.ErrorEvent{Code: wire.ErrorTurnInterrupted, Retryable: true,
				Message: "The node answering you restarted before the turn finished."}
		}
	case state == wire.TurnRunning:
		// Running on another node this one cannot follow: the snapshot is
		// what it can say, and the client asks again.
		return nil
	}
	seq := snap.Seq
	if failure != nil {
		seq++
		if err := w.Frame(frameAt(row, seq, s.now(), wire.Frame{Kind: wire.EventError,
			Error: failure})); err != nil {
			return err
		}
	}
	seq++
	return w.Frame(frameAt(row, seq, s.now(), wire.Frame{Kind: wire.EventDone,
		Done: &wire.DoneEvent{State: state, Steps: row.Steps, TraceID: row.TraceID}}))
}

// snapshot is the turn as the transcript holds it, numbered after seq after.
func (s *Service) snapshot(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	after uint64) (wire.Frame, error) {

	transcript, err := s.transcripts.Transcript(ctx, owner, row.Conversation)
	if err != nil {
		return wire.Frame{}, s.readFailure(err)
	}
	snap := &wire.SnapshotEvent{Messages: []wire.Message{}, Steps: []wire.StepEvent{},
		Cards: []wire.CardEvent{}}
	for _, m := range transcript.Messages {
		if m.Turn == row.ID {
			snap.Messages = append(snap.Messages, m.Wire())
		}
	}
	for _, st := range transcript.Steps {
		if assistantdomain.StepID(row.ID, st.Index) == st.StepID {
			st.TurnID = row.ID
			snap.Steps = append(snap.Steps, st)
		}
	}
	for _, c := range transcript.Cards {
		if c.Turn == row.ID {
			snap.Cards = append(snap.Cards, c.Event())
		}
	}
	return frameAt(row, after+1, s.now(), wire.Frame{Kind: wire.EventSnapshot, Snapshot: snap}), nil
}

// frameAt stamps a frame's head for a stream no frame log numbers.
func frameAt(row assistantdomain.Turn, seq uint64, at time.Time, f wire.Frame) wire.Frame {
	f.V, f.ConversationID, f.TurnID, f.Seq, f.At = wire.ProtocolVersion, row.Conversation,
		row.ID, seq, at.UTC()
	return f
}

// streamOpened and streamClosed keep the active streams gauge.
func (s *Service) streamOpened() {
	s.mu.Lock()
	s.streams++
	n := s.streams
	s.mu.Unlock()
	s.metrics.Set(metrics.AssistantStreamsActive, float64(n), nil)
}

func (s *Service) streamClosed() {
	s.mu.Lock()
	s.streams--
	n := s.streams
	s.mu.Unlock()
	s.metrics.Set(metrics.AssistantStreamsActive, float64(n), nil)
}

// readFailure maps a read of the assistant's domain to what a caller acts on.
func (s *Service) readFailure(err error) error {
	switch {
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged),
		errors.Is(err, assistantdomain.ErrErased):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, assistantdomain.ErrInvalid):
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
}

// writeFailure maps a write of the assistant's domain to what a caller acts on.
func (s *Service) writeFailure(err error) error {
	switch {
	case errors.Is(err, assistantdomain.ErrTurnRunning), errors.Is(err, assistantdomain.ErrTurnMoved):
		return fmt.Errorf("%w: %w", ErrTurnRunning, err)
	case errors.Is(err, ErrOutcomeUnknown):
		return &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
	}
	return s.readFailure(err)
}
