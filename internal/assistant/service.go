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
	"github.com/crewlet/crewlet/internal/assistant/cards"
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
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Deps is what a [Service] is built from. Every field but Remote, Rate and Now is
// REQUIRED, and a nil is refused by name: the assistant runs beside an
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
	Transcripts Transcripts
	Records     Records
	Dispatcher  exec.Dispatcher
	Revoker     Revoker
	Audit       AuditPublisher
	Metrics     Metrics

	// Remote follows and stops turns running on other nodes; nil serves
	// this node's own.
	Remote Remote
	// Rate counts a person's turn starts across the fleet; nil counts them
	// on this node alone.
	Rate TurnRate
	// Files keeps a conversation's files: the images a person attaches and
	// the files a deep-work run delivers, and answers whether a file a draft
	// change names is the person's. Nil keeps none: a question takes no
	// attachment, no deep work is offered, and every draft change naming a
	// file is refused.
	Files Files

	// --- deep work (im-deep) ---
	// Deep runs deep work; nil is a node that offers none (ADR-0054).
	Deep DeepWork

	// Sessions is where a turn serves its tools to its model's CLI, as the
	// one MCP server `crewlet` (ADR-0049): this process's tool-session host.
	Sessions *toolsession.Host

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
	transcripts Transcripts
	records     Records
	dispatcher  exec.Dispatcher
	revoker     Revoker
	audit       AuditPublisher
	metrics     Metrics
	remote      Remote
	rate        TurnRate
	files       Files
	deep        DeepWork
	sessions    *toolsession.Host
	now         func() time.Time
	log         *slog.Logger

	hub     *live.Hub
	limiter *costlimit.Limiter
	// opens is the stream-open rate ([StreamOpensPerMinute]).
	opens *costlimit.Limiter

	// base is every turn's parent context, cancelled with errDraining when
	// the node stops.
	base   context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	decliner Decliner
	// attempts are the turn starts in flight here ([Service.Start]).
	attempts map[uuid.UUID]*attempt
	turns    map[uuid.UUID]*turn
	running  map[uuid.UUID]int
	streams  int
	// streaming counts each person's open streams ([StreamsPerPerson]).
	streaming map[uuid.UUID]int
	closed    bool
	titlesWG  sync.WaitGroup
	reaperWG  sync.WaitGroup
	// admissions counts the starts and resumes admitted and not yet either
	// running or given up: what Close waits for before it gathers the
	// runners, so a start that passed its admission just before Close is
	// never missed.
	admissions sync.WaitGroup
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
		"Catalogue":   d.Catalogue == nil,
		"Transcripts": d.Transcripts == nil, "Records": d.Records == nil,
		"Dispatcher": d.Dispatcher == nil, "Revoker": d.Revoker == nil,
		"Audit": d.Audit == nil, "Metrics": d.Metrics == nil,
		"Sessions": d.Sessions == nil,
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
	var tap live.Tap
	if d.Remote != nil {
		tap = d.Remote.Publish
	}
	s := &Service{
		node: d.Node, company: d.Company, acts: d.Acts, models: d.Models, budget: d.Budget,
		catalogue: d.Catalogue, transcripts: d.Transcripts,
		records: d.Records, dispatcher: d.Dispatcher, revoker: d.Revoker, audit: d.Audit,
		metrics: d.Metrics, remote: d.Remote, rate: d.Rate, files: d.Files, deep: d.Deep,
		sessions: d.Sessions, now: now,
		log: logging.Get("assistant"),
		hub: live.NewHub(now, config.Assistant{ProposalTTLSeconds: maxProposalTTLSeconds}.ProposalTTL(),
			tap),
		limiter: costlimit.New(costlimit.Rates{
			PrincipalRate: TurnsPerMinute / 60.0, PrincipalBurst: TurnBurst,
			ScopeRate: NodeTurnsPerSecond, ScopeBurst: NodeTurnBurst,
		}, now),
		opens: costlimit.New(costlimit.Rates{
			PrincipalRate: StreamOpensPerMinute / 60.0, PrincipalBurst: StreamOpenBurst,
			ScopeRate: NodeStreamOpensPerSecond, ScopeBurst: NodeStreamOpenBurst,
		}, now),
		base: base, cancel: cancel,
		turns: map[uuid.UUID]*turn{}, running: map[uuid.UUID]int{},
		streaming: map[uuid.UUID]int{}, attempts: map[uuid.UUID]*attempt{},
	}
	s.reaperWG.Add(1)
	go s.reapLoop()
	return s, nil
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
	// Attachments are files the person uploaded to the conversation for
	// this question ([wire.TurnRequest.Attachments]).
	Attachments []uuid.UUID
	// Mode is what the question asks for ([wire.TurnRequest.Mode]); empty
	// is [wire.TurnAsk].
	Mode wire.TurnMode
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
//
// ONE ATTEMPT AT A TIME PER TURN: a start under a key whose first attempt is
// still in flight on this node (a client retrying on its own timeout) waits
// for that attempt and answers what it answered, rather than running a second
// loop for one turn. Its own read grant is ended unused.
func (s *Service) Start(ctx context.Context, ts TurnStart) (wire.TurnAccepted, error) {
	id := TurnID(ts.Key)
	s.mu.Lock()
	if first, ok := s.attempts[id]; ok {
		s.mu.Unlock()
		if ts.Read != nil {
			s.revoker.Revoke(ts.Read.ID())
		}
		select {
		case <-first.done:
			return first.accepted, first.err
		case <-ctx.Done():
			return wire.TurnAccepted{}, &Unavailable{RetryAfter: time.Second, Cause: ctx.Err()}
		}
	}
	a := &attempt{done: make(chan struct{})}
	s.attempts[id] = a
	s.mu.Unlock()
	a.accepted, a.err = s.start(ctx, ts)
	s.mu.Lock()
	delete(s.attempts, id)
	s.mu.Unlock()
	close(a.done)
	return a.accepted, a.err
}

// attempt is one start of a turn in flight on this node, and what it
// answered once done is closed.
type attempt struct {
	done     chan struct{}
	accepted wire.TurnAccepted
	err      error
}

// start is one attempt at [Service.Start].
func (s *Service) start(ctx context.Context, ts TurnStart) (wire.TurnAccepted, error) {
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
	page, err := admitPage(ts.Page)
	if err != nil {
		return wire.TurnAccepted{}, invalid("page_context: %v", err)
	}
	if ts.Mode == "" {
		ts.Mode = wire.TurnAsk
	}
	switch {
	case !ts.Mode.Valid():
		return wire.TurnAccepted{}, invalid("mode %q is not one of %v", ts.Mode, wire.TurnModes)
	case ts.Mode == wire.TurnBrief && page == nil:
		return wire.TurnAccepted{}, invalid("mode brief needs the page_context it briefs")
	case ts.Mode == wire.TurnBrief && len(ts.Attachments) > 0:
		return wire.TurnAccepted{}, invalid("mode brief takes no attachments")
	}
	briefing := ts.Mode == wire.TurnBrief
	input = redact.Secrets(input)
	ts.Page = page
	attached, attachErr := s.attachmentsOf(ctx, ts)
	if attachErr != nil {
		return wire.TurnAccepted{}, attachErr
	}
	id := TurnID(ts.Key)
	accepted := wire.TurnAccepted{TurnID: id, StreamURL: StreamPath(ts.Conversation, id)}

	// THE SAME KEY IS THE SAME TURN: a retry whose first attempt landed
	// answers the turn it started, wherever it is now.
	if s.runningHere(id) {
		return accepted, nil
	}
	switch _, turnErr := s.transcripts.Turn(ctx, ts.Person.ID, id); {
	case turnErr == nil:
		return accepted, nil
	case !errors.Is(turnErr, assistantdomain.ErrNotFound):
		return wire.TurnAccepted{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: turnErr}
	}
	// A MODEL KNOWN UNUSABLE REFUSES THE QUESTION HERE, before anything is
	// recorded or counted, in the words a failed round would have used
	// ([Service.ModelNotice] is the same reading for `/viewer`). After the
	// retry check above, so a retry of a question that already started is
	// answered with its turn whatever the model's state now.
	if refusal := s.modelRefusal(ctx, ts.Person); refusal != nil {
		return wire.TurnAccepted{}, refusal
	}

	history, err := s.transcripts.Transcript(ctx, ts.Person.ID, ts.Conversation)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	// A PARKED TURN WAITS ON ITS CARDS, NOT ON THE CONVERSATION: a new
	// question is taken, and the parked turn is closed superseded just
	// before this one opens (its proposals stay approvable until they
	// expire). A turn that is running holds the conversation.
	var past *assistantdomain.Turn
	if current, lapseErr := s.lapseOpen(ctx, ts.Person.ID, history); lapseErr != nil {
		return wire.TurnAccepted{}, lapseErr
	} else if open := history.Conversation.OpenTurn; current && open != uuid.Nil {
		row, parked := parkedIn(history, open)
		if !parked {
			return wire.TurnAccepted{}, ErrTurnRunning
		}
		past = &row
	}
	acts, err := s.acts(ctx, ts.Person)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
	}
	if err = s.admit(ctx, ts.Person); err != nil {
		return wire.TurnAccepted{}, err
	}
	defer s.admissions.Done()
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
	purpose := types.AuxAssistantTurn
	if briefing {
		purpose = types.AuxAssistantBrief
	}
	use := auxspend.Use{Stage: types.AuxStageOperator, Purpose: purpose, TurnID: id.String()}
	provider, key, err := s.models.Chain(ctx, ts.Person, use)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{Cause: err}
	}

	if past != nil {
		if err := s.supersede(ctx, ts.Person, ts.Key, *past); err != nil {
			return wire.TurnAccepted{}, err
		}
	}
	t := s.newTurn(ts.Person, ts.Conversation, id, ts.Key, ts.Read, ts.Page, acts)
	t.brief.on = briefing
	var chip *wire.PageChip
	if ts.Page != nil {
		chip = &wire.PageChip{Title: ts.Page.Title, Screen: ts.Page.Screen}
	}
	// THE FRAME LOG OPENS BEFORE THE ROW SAYS THE TURN RUNS HERE: a stream
	// that reads the row running on this node then always finds its log,
	// and is never answered from the transcript as a turn whose runner died
	// ([Service.answerFromTranscript]), which would close a live turn
	// interrupted.
	s.openStream(t, history.Conversation, false, 0)
	abandon := func() {
		t.cancel(nil)
		s.hub.Forget(id)
	}
	if _, err := s.records.OpenTurn(ctx, t.write().Step("open"), assistantdomain.NewTurn{
		Conversation: ts.Conversation, Turn: id, Node: s.node, PromptVersion: t.promptVersion(),
		Model: provider.Model(), ProviderKey: key, TraceID: traceOf(ctx), Page: chip,
	}); errors.Is(err, assistantdomain.ErrTurnOpen) {
		// THE SAME KEY IS THE SAME TURN, decided by the record: an attempt
		// that raced its first one (on another node) starts nothing.
		abandon()
		return accepted, nil
	} else if err != nil {
		abandon()
		return wire.TurnAccepted{}, s.writeFailure(err)
	}
	t.messages = 1
	if err := s.records.RecordMessage(ctx, t.write().Step("message", "question"), id,
		assistantdomain.NewMessage{ID: assistantdomain.MessageID(id, 1), Role: wire.RoleUser,
			Text: input, Page: chip, Attachments: refsOf(attached)}); err != nil {
		s.log.WarnContext(ctx, "assistant_record_failed", "turn", id, "record", "question",
			"error", redact.Secrets(err.Error()))
	}
	// A CONVERSATION IS NAMED BY ITS FIRST QUESTION AT ONCE, whatever its
	// first turn then does: stopped, failed or parked on a change, it is
	// still the conversation that question started, and the history lists
	// it by that question rather than as one more untitled entry. A title
	// the model writes once a first answer completes replaces it
	// ([Service.title]).
	if history.Conversation.Title == "" {
		title := conversationTitle(input)
		if briefing {
			title = briefTitle(ts.Page)
		}
		t.record(ctx, "title", func(ctx context.Context, w Write) error {
			return s.records.TitleConversation(ctx, w.Step("title", "question"), ts.Conversation,
				title)
		})
	}

	c, organization := s.company()
	var tools []catalog.Tool
	if briefing {
		tools = briefTools(s.catalogue.Offered(ts.Person, acts))
	} else {
		tools = s.withDeep(ctx, s.catalogue.Offered(ts.Person, acts))
	}
	earlier := historyEntries(history, ts.Person)
	t.used(historyApps(earlier))
	t.deep.attachments = attached
	in := promptInput{Person: ts.Person, Apps: iam.Expand(ts.Person.Grants), Now: s.now(),
		History: earlier, Page: ts.Page, Boundary: t.boundary, Question: input,
		Proposable: proposable(tools), Deep: offersDeep(tools), Attachments: refsOf(attached)}
	if c != nil {
		in.Company, in.Zone = c.Name, c.Location()
	}
	if organization != nil {
		if seat := organization.SeatByHandle(ts.Person.Seat); seat != nil {
			in.Name = seat.Name
		}
	}

	assistantCfg := config.Assistant{}
	if c != nil {
		assistantCfg = c.Assistant
	}
	run := loopRun{messages: buildMessages(in), provider: provider, key: key, meter: meter,
		tools: tools, steps: assistantCfg.Steps(), timeout: assistantCfg.TurnTimeout(),
		fresh: history.Conversation.Title == "", question: input}
	if briefing {
		run.messages = buildBriefMessages(in)
		run.meter = &briefMeter{inner: meter}
		// THE ANSWER'S ROUNDS ARE KEPT: reads run in the first rounds, and
		// the answer and its one retry each have one of their own after
		// them ([BriefReadRounds]).
		reads := min(run.steps, BriefReadRounds)
		t.mu.Lock()
		t.brief.readRounds = reads
		t.mu.Unlock()
		run.rounds = reads + BriefAnswerAttempts
		run.terminate = []string{catalog.AnswerBriefTool}
	}
	admitted, started = false, true
	s.track(t)
	go t.run(run) //nolint:contextcheck // the turn's own lifetime (t.ctx, under the service's base), never the request's: a turn outlives the POST that started it
	return accepted, nil
}

// parkedIn is the conversation's turn id when it is parked on a proposal.
func parkedIn(t assistantdomain.Transcript, id uuid.UUID) (assistantdomain.Turn, bool) {
	for _, row := range t.Turns {
		if row.ID == id {
			return row, row.State == wire.TurnWaitingForApproval
		}
	}
	return assistantdomain.Turn{}, false
}

// supersede closes a parked turn the person asked past, under the asking
// request's operation (a retried question records it once), and tells its
// stream it ended. It withdraws nothing: the proposals it waited on stay
// approvable until they expire, and one approved later is applied and shown
// on its card without resuming the loop ([Service.Resume]). A turn an
// approval resumed a moment before is running again, and holds the
// conversation.
func (s *Service) supersede(ctx context.Context, p iam.Principal, key string,
	row assistantdomain.Turn) error {

	// READ BEFORE THE CLOSE, which drops the parked record: the last seq its
	// segment told, which the closing frames number after.
	var floor uint64
	keep, keepErr := s.parkedLoop(ctx, p.ID, row.ID)
	if keepErr == nil {
		floor = keep.Seq
	}
	err := s.records.CloseTurn(ctx, Write{Owner: p.ID, By: iam.ActorFor(p), Key: key,
		Steps: []string{"supersede"}}, row.ID, assistantdomain.TurnEnd{
		State: wire.TurnSuperseded, Steps: row.Steps})
	if err == nil && keepErr == nil {
		// No approval resumes a turn asked past, so a deep-work run it
		// parked on is never continued.
		s.dropParkedDeep(ctx, keep)
	}
	var moved *assistantdomain.TurnMoved
	switch {
	case errors.As(err, &moved) && moved.State.Final():
		// Another end landed first (a second question, its expiry): the
		// conversation is free either way.
		return nil
	case err != nil:
		return s.writeFailure(err)
	}
	row.State = wire.TurnSuperseded
	if _, err := s.tell(p, row, s.continuing(ctx, p.ID, row.ID, floor), nil); err != nil {
		s.log.WarnContext(ctx, "assistant_superseded_untold", "turn", row.ID,
			"error", redact.Secrets(err.Error()),
			"detail", "the turn is closed; a stream of it reads the transcript")
	}
	return nil
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
func (s *Service) openStream(t *turn, c assistantdomain.Conversation, resumed bool,
	floor uint64) uint64 {
	t.log = s.hub.Open(t.person.ID, t.conversation, t.id, floor)
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

// historyEntries is a conversation's messages as the next turn re-reads them
// for reader, each answer with its cards' one-line summaries: a message or a
// card built from an app the reader no longer holds is its notice, so the
// model is never handed what the reader may no longer read.
func historyEntries(t assistantdomain.Transcript, reader iam.Principal) []historyEntry {
	summaries := map[uuid.UUID][]string{}
	apps := map[uuid.UUID][]string{}
	for _, c := range t.Cards {
		card, visible := cards.Visible(c.Apps, c.Card, reader)
		summaries[c.Message] = append(summaries[c.Message], cardSummary(card))
		if visible {
			apps[c.Message] = append(apps[c.Message], c.Apps...)
		}
	}
	out := make([]historyEntry, 0, len(t.Messages))
	for _, m := range t.Messages {
		m = shownMessage(m, reader)
		out = append(out, historyEntry{Role: m.Role, Text: m.Text, Cards: summaries[m.ID],
			Apps: append(slices.Clone(m.Apps), apps[m.ID]...)})
	}
	return out
}

// historyApps are the apps of the history a turn's prompt carries (its last
// [HistoryMessages] entries): what the turn knows before it reads anything.
func historyApps(entries []historyEntry) []string {
	var out []string
	for _, e := range entries[max(0, len(entries)-HistoryMessages):] {
		out = append(out, e.Apps...)
	}
	return out
}

// proposable is every write the person may propose: the offered tools that
// write.
func proposable(tools []catalog.Tool) []catalog.Tool {
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
// or answers why not. A caller admitted calls s.admissions.Done once its
// runner is tracked or it gave up.
func (s *Service) admit(ctx context.Context, p iam.Principal) error {
	if err := s.admitHere(p); err != nil {
		return err
	}
	// THE FLEET'S COUNT, outside the lock: it is a round trip to the
	// coordination store, and no other person's start waits on it.
	if err := s.admitAcrossTheFleet(ctx, turnRateScope, p.ID, TurnsPerMinute,
		turnRateReason); err != nil {
		s.unadmit(p.ID)
		s.admissions.Done()
		return err
	}
	return nil
}

// admitHere takes one turn for p from this node's running allowance and
// turn rate.
func (s *Service) admitHere(p iam.Principal) error {
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
		return &Busy{RetryAfter: refused.RetryAfter, Reason: turnRateReason}
	}
	s.running[p.ID]++
	s.admissions.Add(1)
	return nil
}

// turnRateReason is what a person past their turn rate is told, on this node
// or across the fleet alike.
const turnRateReason = "too many questions in a short time; wait a moment"

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
	if s.turns[t.id] == t {
		delete(s.turns, t.id)
	}
	s.mu.Unlock()
	t.freeSlot()
	t.cancel(nil)
}

// runningHere reports whether this node runs turn id now.
func (s *Service) runningHere(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.turns[id]
	return ok
}

// liveHere reports whether this node runs turn id, or has a start of it in
// flight.
func (s *Service) liveHere(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, running := s.turns[id]
	_, starting := s.attempts[id]
	return running || starting
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
	// THE ROW DECIDES FIRST: a runner that has just parked or ended its turn
	// is still tearing down on this node for a moment after its record
	// landed, and cancelling it then stops nothing while the turn stays
	// parked. Only a turn the record still says runs is a runner's to stop.
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
		if t, err := s.transcripts.Transcript(ctx, owner, row.Conversation); err == nil {
			s.settleWaiting(ctx, w, waitingSteps(t, turnID), wire.ProposalDeclined, nil, nil)
		} else {
			s.log.WarnContext(ctx, "assistant_stop_steps_unsettled", "turn", turnID,
				"error", redact.Secrets(err.Error()),
				"detail", "the turn is stopped; its waiting step reads as waiting on a reload")
		}
		return wire.StopAnswer{State: wire.TurnStopped}, nil
	}
	if t, ok := s.local(owner, turnID); ok {
		return wire.StopAnswer{State: s.stopRunner(ctx, t)}, nil
	}
	if s.remote != nil {
		state, reach, err := s.remote.Stop(ctx, owner, turnID, row.Node)
		switch {
		case errors.Is(err, ErrNotFound):
			return wire.StopAnswer{}, err
		case err != nil:
			return wire.StopAnswer{}, &Unavailable{RetryAfter: 2 * time.Second, Cause: err}
		case reach == live.ReachServed:
			return wire.StopAnswer{State: state}, nil
		case reach == live.ReachGone:
			// THE RUNNER DIED WITH THE TURN: there is nothing to stop,
			// and the turn is closed interrupted, once, as a stream of
			// it would close it.
			state, _, err := s.closeInterrupted(ctx, owner, row)
			if err != nil {
				return wire.StopAnswer{}, err
			}
			return wire.StopAnswer{State: state}, nil
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
	if keep, err := s.parkedLoop(ctx, p.ID, row.ID); err == nil {
		s.dropParkedDeep(ctx, keep)
	}
	if err := d.DeclineTurn(ctx, approval.Stop{Owner: p.ID, Conversation: row.Conversation,
		Turn: row.ID, By: iam.ActorFor(p), Key: key}); err != nil {
		s.log.WarnContext(ctx, "assistant_stop_decline_failed", "turn", row.ID,
			"error", redact.Secrets(err.Error()))
	}
}

// stopRunner stops a turn this node runs and waits, at most [stopWait], for
// its runner to record its end, answering the state it truly ended in: stopped,
// or what it reached first (completed, waiting for approval, failed). A runner
// that has not ended in the wait is still running, the stop asked of it, and
// that is the answer; the client reads the end on the turn's stream.
func (s *Service) stopRunner(ctx context.Context, t *turn) wire.TurnState {
	t.stop()
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.ended
	case <-ctx.Done():
	case <-time.After(stopWait):
	}
	return wire.TurnRunning
}

// stopWait is how long a Stop waits for the runner to record its end before
// answering: long enough for the loop to notice its cancelled context, which
// ends a model call in progress (a CLI is signalled at once).
const stopWait = 5 * time.Second

// Stream writes one of the owner's turns of conversation to w from after seq
// after, until the segment ends, the subscriber falls behind or ctx ends. A
// segment that has ended with nothing after seq after answers [ErrCaughtUp],
// having written nothing: the client holds all of it.
//
// A turn this node runs (or ran in the last [live.EndedRetention]) is served
// from its frame log; frames the log no longer holds are answered with a
// snapshot of the transcript. A turn running elsewhere is followed through
// [Remote]; with none, or when no node answers for it, the transcript is the
// answer, and a turn whose runner this node was is closed interrupted.
func (s *Service) Stream(ctx context.Context, owner, conversation, turnID uuid.UUID, after uint64,
	w FrameWriter) error {

	// THE PERSON'S STREAMS ARE BOUNDED BEFORE ANYTHING IS READ: each one
	// holds a goroutine and a frame buffer for as long as its turn runs, and
	// one of an ended turn unseals the transcript to answer it.
	if err := s.streamOpened(ctx, owner); err != nil {
		return err
	}
	defer s.streamClosed(owner)
	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return s.readFailure(err)
	}
	if row.Conversation != conversation {
		return fmt.Errorf("%w: turn %s is not in conversation %s", ErrNotFound, turnID, conversation)
	}
	l, node := s.streamSource(ctx, owner, row, after)
	if l != nil {
		return s.follow(ctx, owner, row, l, after, w)
	}
	gone := false
	if node != "" {
		reach, err := s.remote.Follow(ctx, owner, turnID, node, after, w)
		if err != nil || reach == live.ReachServed {
			return err
		}
		// Only the record's own runner gone ends the turn with it.
		gone = reach == live.ReachGone && node == row.Node
	}
	return s.answerFromTranscript(ctx, owner, row, after, w, gone)
}

// follow serves a stream from this node's frame log.
func (s *Service) follow(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	l *live.Log, after uint64, w FrameWriter) error {

	replay, sub, gap := l.Subscribe(after)
	defer sub.Cancel()
	replay, err := s.filled(ctx, owner, row, after, replay, gap)
	if err != nil {
		return err
	}
	for _, f := range replay {
		if err := w.Frame(f); err != nil {
			return err
		}
	}
	// WHETHER THIS STREAM HAS SAID ANYTHING: a subscription the ended log
	// closed before a single write is a client that already holds the whole
	// segment (it resumed after the `done`), told so rather than answered an
	// empty stream it would read as a drop ([live.ErrCaughtUp]). One closed
	// for falling behind is not: its client resumes from where it is.
	wrote := len(replay) > 0
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
			wrote = true
		case f, open := <-sub.C():
			if !open {
				if !wrote && !sub.Overflowed() {
					return live.ErrCaughtUp
				}
				return nil
			}
			if err := w.Frame(f); err != nil {
				return err
			}
			wrote = true
		}
	}
}

// filled is a log's frames after seq after as a stream is handed them: where
// the log no longer holds every frame after it (gap), a snapshot of the
// transcript first, then the frames numbered past the snapshot.
func (s *Service) filled(ctx context.Context, owner uuid.UUID, row assistantdomain.Turn,
	after uint64, frames []wire.Frame, gap bool) ([]wire.Frame, error) {

	if !gap {
		return frames, nil
	}
	snap, err := s.snapshot(ctx, owner, row, after)
	if err != nil {
		return nil, err
	}
	frames = slices.DeleteFunc(frames, func(f wire.Frame) bool { return f.Seq <= snap.Seq })
	return append([]wire.Frame{snap}, frames...), nil
}

// answerFromTranscript answers a stream no live log serves: a snapshot of what
// the transcript holds of the turn, then how it ended. A turn still open whose
// runner was this node, or whose runner the fleet found gone, died with it,
// and is closed interrupted once ([Service.closeInterrupted]).
func (s *Service) answerFromTranscript(ctx context.Context, owner uuid.UUID,
	row assistantdomain.Turn, after uint64, w FrameWriter, gone bool) error {

	snap, err := s.snapshot(ctx, owner, row, after)
	if err != nil {
		return err
	}
	if frameErr := w.Frame(snap); frameErr != nil {
		return frameErr
	}
	state := row.State
	var failure *wire.ErrorEvent
	switch {
	case state == wire.TurnRunning && row.Node == s.node && s.liveHere(row.ID):
		// A RUNNER OF THIS NODE HOLDS IT (starting, or between its log
		// and its track): the snapshot is what it can say now, and the
		// client asks again.
		return nil
	case state == wire.TurnRunning && (row.Node == s.node || gone):
		state, failure, err = s.closeInterrupted(ctx, owner, row)
		if err != nil {
			return err
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

// closeInterrupted closes a running turn whose runner died with it, and answers
// the state it is in now and the error a stream shows for it. A close that
// did not land is the caller's error to surface ([Service.writeFailure]),
// never an "interrupted" the record does not say: the client asks again, and
// the next attempt closes it.
//
// EXACTLY ONCE across the fleet: the close is published under an operation
// every node derives alike from the turn ([Derivation]), and the state log
// arbitrates the turn's end, so of two nodes noticing at once one records it
// and the other reads the state it landed in ([assistantdomain.TurnMoved]).
func (s *Service) closeInterrupted(ctx context.Context, owner uuid.UUID,
	row assistantdomain.Turn) (wire.TurnState, *wire.ErrorEvent, error) {

	state, err := s.closeDerived(ctx, derivedWrite(owner, row, "assistant_interrupt"), row.ID, assistantdomain.TurnEnd{
		State: wire.TurnInterrupted, Steps: row.Steps, ErrorCode: string(wire.ErrorTurnInterrupted)})
	if err != nil {
		return "", nil, err
	}
	if state != wire.TurnInterrupted {
		return state, nil, nil
	}
	return state, &wire.ErrorEvent{Code: wire.ErrorTurnInterrupted, Retryable: true,
		Message: wire.ErrorTurnInterrupted.Sentence()}, nil
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

// streamOpenScope is the costlimit scope every stream open is taken from.
const streamOpenScope = "assistant.stream"

// streamOpened takes one of owner's open streams ([StreamsPerPerson]) and one
// stream open from their rate, on this node and on the fleet's counter, or
// answers why not; it keeps the active streams gauge.
func (s *Service) streamOpened(ctx context.Context, owner uuid.UUID) error {
	if err := s.takeStream(owner); err != nil {
		return err
	}
	if err := s.admitAcrossTheFleet(ctx, streamOpenScope, owner, StreamOpensPerMinute,
		streamRateReason); err != nil {
		s.streamClosed(owner)
		return err
	}
	return nil
}

// takeStream takes one of owner's open streams on this node.
func (s *Service) takeStream(owner uuid.UUID) error {
	s.mu.Lock()
	if s.streaming[owner] >= StreamsPerPerson {
		s.mu.Unlock()
		return &Busy{RetryAfter: 5 * time.Second, Reason: fmt.Sprintf(
			"%d streams are open for this person; close one", StreamsPerPerson)}
	}
	if refused := s.opens.Take(streamOpenScope, owner.String(), 1); refused != nil {
		s.mu.Unlock()
		return &Busy{RetryAfter: refused.RetryAfter, Reason: streamRateReason}
	}
	s.streaming[owner]++
	s.streams++
	n := s.streams
	s.mu.Unlock()
	s.metrics.Set(metrics.AssistantStreamsActive, float64(n), nil)
	return nil
}

// streamRateReason is what a person past their stream-open rate is told.
const streamRateReason = "too many streams opened in a short time; wait a moment"

// streamClosed gives back one of owner's open streams.
func (s *Service) streamClosed(owner uuid.UUID) {
	s.mu.Lock()
	if s.streaming[owner]--; s.streaming[owner] <= 0 {
		delete(s.streaming, owner)
	}
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
