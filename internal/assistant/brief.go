package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A BRIEF IS A TURN (wire.TurnBrief): the first answer of a conversation the
// person goes on asking in, so it is started, admitted, metered, recorded and
// streamed exactly as a question is, with three differences, all here.
//
//   - It only reads. The turn is offered the person's read tools and
//     [catalog.BriefTool], nothing that proposes, navigates or hands work on,
//     and its reads draw no cards: the run chip shows each read, and the
//     brief card is the whole answer. Its prose is never drawn.
//   - It answers in a fixed shape. The model calls answer_brief once; the
//     engine holds the call to its schema and bounds, and a call it cannot
//     accept is answered with EVERY reason and may be written again ONCE.
//     A second refusal ends the turn [wire.ErrorBriefInvalid]. The retry
//     always has a round to land in: reads stop [BriefAnswerAttempts]
//     rounds before the turn's last ([BriefReadRounds]), so an answer
//     written after the last read is never refused in the turn's last
//     round.
//   - The engine owns everything but the words. A row survives only when
//     the record it cites appears in what this turn's own reads answered AND
//     the record's own read, sent again as the person, answers it: a row
//     about a record nobody read, or one the person may not open, is dropped,
//     never drawn. Its address is the link table's, its danger tone is a fact
//     that read carried (a seat stopped, a task marked urgent, a failed last
//     turn), and its action opens that address or asks a question. Nothing a
//     brief offers writes.

// errBriefInvalid ends a brief turn whose model wrote no brief the engine
// could accept within its retry.
var errBriefInvalid = errors.New("assistant: the model wrote no brief the engine could accept")

// The bounds of a brief turn.
const (
	// BriefReadRounds is the most model requests whose reads a brief runs:
	// a page's brief reads what the page shows, what waits on the person
	// and the agents (three to five reads, often side by side), with room
	// to spare. The turn's round budget is these and [BriefAnswerAttempts]
	// more, so the answer and its one retry each have a round however the
	// reads were spent; a read asked for in those last rounds is refused,
	// and the answer to a read in the last read round says the reads are
	// spent.
	BriefReadRounds = 7
	// BriefAnswerAttempts is how many answer_brief calls a turn may make:
	// the first, and one more after the engine said what was wrong with it.
	BriefAnswerAttempts = 2
	// BriefMaxTokens is the most tokens one brief may spend, every round
	// together: about five rounds of a 20,000 token context (the prompt,
	// the tool list and the reads so far), which a brief of one page needs
	// and a brief that has set out to read the whole company does not get.
	// Past it the turn ends as a budget refusal of its own scope
	// ([BriefBudgetScope]), never as the company's.
	BriefMaxTokens = 100_000
	// BriefBudgetScope names the brief's own cap in a budget refusal.
	BriefBudgetScope = "brief"
)

// briefState is a brief turn's own state, under the turn's mutex.
type briefState struct {
	// on marks a brief turn.
	on bool
	// reads are what each read of the turn answered the model: what a row's
	// record must appear in.
	reads []string
	// answers counts the answer_brief calls made.
	answers int
	// readRounds is how many of the turn's rounds may read
	// ([BriefReadRounds], or fewer where the company allows fewer steps).
	readRounds int
}

// briefing reports whether t is a brief turn.
func (t *turn) briefing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.brief.on
}

// readForBrief keeps a read's answer as the model read it, for the citation
// check.
func (t *turn) readForBrief(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.brief.reads = append(t.brief.reads, model)
}

// cited reports whether id appears as a whole name in what this turn's reads
// answered: ENG-1 is not cited by an answer that names only ENG-12.
func (t *turn) cited(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, answer := range t.brief.reads {
		if containsName(answer, id) {
			return true
		}
	}
	return false
}

// containsName reports whether name appears in text with no name character
// either side of it.
func containsName(text, name string) bool {
	if name == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(text[from:], name)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(name)
		if (start == 0 || !nameByte(text[start-1])) && (end == len(text) || !nameByte(text[end])) {
			return true
		}
		from = start + 1
	}
}

// nameByte is a byte a key, a handle or an id is spelled with.
func nameByte(b byte) bool {
	return b == '-' || b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// briefTools are what a brief turn is offered: the person's reads, and the
// brief's answer. Never a write, a page action, a navigation or deep work.
func briefTools(offered []catalog.Tool) []catalog.Tool {
	out := make([]catalog.Tool, 0, len(offered)+1)
	for _, t := range offered {
		if t.Kind == catalog.KindRead {
			out = append(out, t)
		}
	}
	return append(out, catalog.BriefTool())
}

// The words a brief's model reads when its reads are spent.
const (
	// briefReadsSpent answers a read asked for in a round kept for the
	// answer.
	briefReadsSpent = "Not sent: this brief has made every read it may, and the rounds left " +
		"are for the answer. Call answer_brief now with what you have read."
	// briefLastReads follows the answer to a read in the last round that
	// may read.
	briefLastReads = "\n\nThat was the last round of reads this brief may make: call " +
		"answer_brief next with what you have read."
)

// briefRead carries out a read in a brief turn, held to the brief's read
// rounds: a read in a round kept for the answer is refused unsent, and a
// read in the last round that may read is answered with a note that the
// reads are spent. A call whose round the loop did not stamp is read as
// any read is.
func (s *surface) briefRead(ctx context.Context, tool catalog.Tool,
	args map[string]any) (toolloop.ToolResult, error) {

	s.t.mu.Lock()
	limit := s.t.brief.readRounds
	s.t.mu.Unlock()
	round, stamped := toolloop.CallRound(ctx)
	if stamped && limit > 0 && round > limit {
		s.recordRefusal(ctx, tool, args, "Not run", "the brief's reads are spent")
		return failed(mcp.RefusalNotRunning, briefReadsSpent), nil
	}
	res, err := s.read(ctx, tool, args)
	if err == nil && stamped && round == limit {
		res.Output += briefLastReads
	}
	return res, err
}

// answerBrief carries out an answer_brief call: the draft checked, every row
// resolved for the person, and the card shown. A draft the engine refuses is
// answered with its reason for the model to write again, once.
func (s *surface) answerBrief(ctx context.Context, tool catalog.Tool,
	args map[string]any) (toolloop.ToolResult, error) {

	s.t.mu.Lock()
	s.t.brief.answers++
	attempt, reads := s.t.brief.answers, s.t.brief.readRounds
	s.t.mu.Unlock()
	draft, err := catalog.ParseBrief(args)
	if err != nil {
		reason := s.recordRefusal(ctx, tool, args, "Not accepted", err.Error())
		s.t.svc.metrics.Add(metrics.AssistantBriefs, 1, metrics.Attrs{"outcome": "refused"})
		// FINAL ON THE LAST ATTEMPT, or in the turn's last round, where no
		// round is left for a retry: the turn ends brief_invalid with this
		// reason rather than as a step limit nobody reached for.
		round, stamped := toolloop.CallRound(ctx)
		final := attempt >= BriefAnswerAttempts ||
			(stamped && reads > 0 && round >= reads+BriefAnswerAttempts)
		// WHY, IN THE ENGINE'S OWN LOG: the reason names fields, counts and
		// caps, never the brief's lines, which are the person's records in
		// the model's words.
		s.t.svc.log.WarnContext(ctx, "assistant_brief_refused", "turn", s.t.id,
			"attempt", attempt, "attempts", BriefAnswerAttempts, "final", final,
			"reason", reason)
		if final {
			// THE TURN ENDS HERE, so the call is never answered: a harness
			// reports its relay closed under it ("Connection closed"),
			// which is the turn's end and not a fault of the relay.
			return toolloop.ToolResult{}, fmt.Errorf("%w: %w", errBriefInvalid, err)
		}
		return failed(mcp.Classify(mcp.RefusalInvalid, err), "The brief was not accepted: "+
			reason+". Call answer_brief again with every one of these fixed; this is the "+
			"last attempt."), nil
	}
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), nil, "")
	card, err := s.resolveBrief(ctx, draft)
	if err != nil {
		if ctx.Err() != nil {
			s.t.stepEnd(ctx, step, wire.StepStopped, "Stopped", nil, 0, 0)
			return toolloop.ToolResult{}, context.Cause(ctx)
		}
		s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be written", nil, 0, 0)
		return toolloop.ToolResult{}, err
	}
	s.t.stepEnd(ctx, step, wire.StepDone, "Written", nil, 0, 0)
	s.t.emitCard(ctx, wire.Card{Type: wire.CardBrief, Brief: card}, s.t.appsNow())
	s.t.svc.metrics.Add(metrics.AssistantBriefs, 1, metrics.Attrs{"outcome": "written"})
	return toolloop.ToolResult{Output: "The brief is shown to the person. Your turn is over: " +
		"write nothing more."}, nil
}

// resolveBrief turns a checked draft into the card: the scope from the page
// context, each row kept only for a record this turn read that the person may
// open, its tone held to the facts that read carried, and every action built
// by the engine. The verdict is the rows that survive: a row dropped here is
// one the card does not count, and so is a second row about a record a kept
// row already cites (two spellings of one seat, its handle and its name,
// resolve to one record here, after the draft's own check could see them).
func (s *surface) resolveBrief(ctx context.Context, d catalog.BriefDraft) (*wire.BriefCard, error) {
	card := &wire.BriefCard{About: d.About, Needs: []wire.BriefNeed{}, Context: d.Context,
		FollowUps: append([]string{}, d.FollowUps...), AsOf: s.t.svc.now().UTC()}
	if page := s.t.page; page != nil {
		card.Scope = wire.BriefScope{Screen: page.Screen, Title: page.Title, Record: page.Selection}
	}
	kept := map[wire.ResourceRef]bool{}
	for _, n := range d.Needs {
		record, err := s.resolved(ctx, n.Ref)
		if err != nil {
			return nil, err
		}
		if record == nil {
			continue
		}
		if kept[record.ref] {
			s.t.svc.log.InfoContext(ctx, "assistant_brief_row_dropped", "turn", s.t.id,
				"kind", string(record.ref.Kind), "reason", "duplicate")
			s.t.svc.metrics.Add(metrics.AssistantBriefRowsDropped, 1, nil)
			continue
		}
		kept[record.ref] = true
		need := wire.BriefNeed{Ref: record.ref, Text: n.Text, Detail: n.Detail,
			Tone: toneOf(n.Tone, record.urgent)}
		switch n.Action {
		case catalog.BriefActionOpen:
			need.Action = record.open()
		case catalog.BriefActionAsk:
			need.Action = askAction(n.Question)
		}
		card.Needs = append(card.Needs, need)
	}
	points, err := s.resolvePoints(ctx, d.Points)
	if err != nil {
		return nil, err
	}
	card.Points = points
	if err := card.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", errBriefInvalid, err)
	}
	return card, nil
}

// resolvePoints keeps each point of a chat room's brief whose message this
// turn's reads named and whose own read, sent again as the person, answers it,
// and builds its source from that read: the room, the message and the thread
// it is in. A brief of anything but a room draws no point, and a point the
// engine cannot vouch for is dropped, counted and logged, as a row is.
func (s *surface) resolvePoints(ctx context.Context,
	drafts []catalog.BriefDraftPoint) ([]wire.BriefPoint, error) {

	if len(drafts) == 0 {
		return nil, nil
	}
	page := s.t.page
	if page == nil || page.Selection == nil || page.Selection.Kind != wire.ResourceRoom {
		for range drafts {
			s.pointDropped(ctx, &rowDropped{reason: "not_a_room"})
		}
		return nil, nil
	}
	room := page.Selection.ID
	out := make([]wire.BriefPoint, 0, len(drafts))
	for _, p := range drafts {
		source, err := s.resolvePoint(ctx, room, p.MessageID)
		var dropped *rowDropped
		switch {
		case errors.As(err, &dropped):
			s.pointDropped(ctx, dropped)
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, wire.BriefPoint{Text: p.Text, Source: source})
	}
	return out, nil
}

// resolvePoint reads one cited message again as the person: a [rowDropped] for
// one no read of this turn named or one the person may not read now; any other
// error is the turn's own.
func (s *surface) resolvePoint(ctx context.Context, room, messageID string) (
	wire.BriefSource, error) {

	if !s.t.cited(messageID) {
		return wire.BriefSource{}, drop("not_read", nil)
	}
	probe := exec.Request{Method: http.MethodGet, Path: "/chatlet/channels/" +
		url.PathEscape(room) + "/messages/" + url.PathEscape(messageID)}
	resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, probe)
	switch {
	case err != nil && ctx.Err() != nil:
		return wire.BriefSource{}, context.Cause(ctx)
	case err != nil:
		return wire.BriefSource{}, drop("not_sent", err)
	case resp.Status == http.StatusUnauthorized:
		return wire.BriefSource{}, errGrantEnded
	case !cards.Permitted(resp.Status):
		return wire.BriefSource{}, drop("not_permitted", nil)
	}
	var read struct {
		View struct {
			Message struct {
				ID         string `json:"id"`
				ChannelID  string `json:"channel_id"`
				ThreadRoot string `json:"thread_root"`
			} `json:"message"`
		} `json:"view"`
	}
	if err := json.Unmarshal(resp.Body, &read); err != nil || read.View.Message.ID == "" {
		return wire.BriefSource{}, drop("unreadable", err)
	}
	m := read.View.Message
	return wire.BriefSource{ChannelID: m.ChannelID, MessageID: m.ID,
		ThreadRoot: m.ThreadRoot}, nil
}

// pointDropped counts and logs a point the brief may not draw, with its
// reason, as [surface.resolved] does a row.
func (s *surface) pointDropped(ctx context.Context, dropped *rowDropped) {
	attrs := []any{"turn", s.t.id, "kind", "point", "reason", dropped.reason}
	if dropped.cause != nil {
		attrs = append(attrs, "error", redact.Secrets(dropped.cause.Error()))
	}
	s.t.svc.log.InfoContext(ctx, "assistant_brief_row_dropped", attrs...)
	s.t.svc.metrics.Add(metrics.AssistantBriefRowsDropped, 1, nil)
}

// briefRecord is a row's record as the engine resolved it for the person.
type briefRecord struct {
	ref  wire.ResourceRef
	link wire.Link
	// urgent says the record's own read carried a fact a danger row needs.
	urgent bool
}

// open is the action that opens the record, labelled by the engine.
func (r briefRecord) open() *wire.BriefAction {
	link := r.link
	return &wire.BriefAction{Kind: wire.BriefOpen, Label: openLabel(r.ref), Link: &link}
}

// askAction is a question to ask in the conversation.
func askAction(question string) *wire.BriefAction {
	return &wire.BriefAction{Kind: wire.BriefAsk, Label: "Ask", Question: question}
}

// openLabel is an open action's words: a task or a project by its key, which
// the person knows it by, and any other record by what it is.
func openLabel(ref wire.ResourceRef) string {
	switch ref.Kind {
	case wire.ResourceTask, wire.ResourceProject:
		if len(ref.ID) <= 24 {
			return "Open " + ref.ID
		}
		return "Open the task"
	case wire.ResourceSeat:
		return "Open the agent"
	case wire.ResourcePage:
		return "Open the page"
	case wire.ResourceArtifact:
		return "Open the artifact"
	case wire.ResourceRoom:
		return "Open the room"
	}
	return "Open"
}

// toneOf is a row's tone: the model's, except that danger needs the engine's
// fact and is drawn warning without one.
func toneOf(asked wire.BriefTone, urgent bool) wire.BriefTone {
	if asked == wire.BriefDanger && !urgent {
		return wire.BriefWarning
	}
	return asked
}

// briefNames resolves a cited record of kind given by its name to its key, the
// way open_in_app does, through the turn's own reads: a seat by its name to
// its handle, a project by its name to its key, and a chat room by its name
// with its "#" to its id, which is the one value the room's link takes. A
// kind's parameter is its alone here, since a task's, a page's and a room's
// are all `id`.
func briefNames(kind wire.ResourceKind) catalog.Tool {
	names := map[string]catalog.Directory{}
	switch kind {
	case wire.ResourceSeat:
		names["handle"] = catalog.DirSeat
	case wire.ResourceProject:
		names["key"] = catalog.DirProject
	case wire.ResourceRoom:
		names["id"] = catalog.DirRoom
	}
	return catalog.Tool{Kind: catalog.KindNavigate, Names: names}
}

// rowDropped is a cited record a brief may not draw a row for, and why: a
// reason the drop is counted and logged by, and the error that decided it
// where one did.
type rowDropped struct {
	reason string
	cause  error
}

func (d *rowDropped) Error() string {
	if d.cause == nil {
		return "brief row dropped: " + d.reason
	}
	return "brief row dropped: " + d.reason + ": " + d.cause.Error()
}

func (d *rowDropped) Unwrap() error { return d.cause }

// drop is a row left out for reason, decided by cause where one decided it.
func drop(reason string, cause error) error { return &rowDropped{reason: reason, cause: cause} }

// resolveRecord resolves one cited record for the person. A record that may
// not be drawn is a [rowDropped]: one no read of this turn named, one the link
// table cannot address or whose name resolves to nothing, or one whose own
// read, sent as the person, does not answer it. Any other error is the turn's
// own (its grant ended, it was stopped).
func (s *surface) resolveRecord(ctx context.Context, ref wire.ResourceRef) (*briefRecord, error) {
	if !s.t.cited(ref.ID) {
		return nil, drop("not_read", nil)
	}
	args, ok := catalog.BriefOpensWith(ref)
	if !ok {
		return nil, drop("no_link", nil)
	}
	resolved, err := s.names.Resolve(ctx, briefNames(ref.Kind), args)
	if err != nil {
		return nil, drop("unresolved", err)
	}
	link, probe, err := catalog.Open(catalog.Args(resolved))
	switch {
	case err != nil:
		return nil, drop("no_link", err)
	case probe == nil:
		return nil, drop("no_link", nil)
	}
	resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, *probe)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, context.Cause(ctx)
	case err != nil:
		return nil, drop("not_sent", err)
	case resp.Status == http.StatusUnauthorized:
		return nil, errGrantEnded
	case !cards.Permitted(resp.Status):
		return nil, drop("not_permitted", nil)
	}
	_, param, _ := catalog.BriefScreenOf(ref.Kind)
	id := catalog.Args(resolved).String(param)
	return &briefRecord{ref: wire.ResourceRef{Kind: ref.Kind, ID: id}, link: link,
		urgent: urgentIn(ref.Kind, resp.Body)}, nil
}

// resolved is a cited record the brief may draw, or nil for one it may not,
// counted and logged with its reason; an error is the turn's own.
func (s *surface) resolved(ctx context.Context, ref wire.ResourceRef) (*briefRecord, error) {
	record, err := s.resolveRecord(ctx, ref)
	var dropped *rowDropped
	if errors.As(err, &dropped) {
		attrs := []any{"turn", s.t.id, "kind", string(ref.Kind), "reason", dropped.reason}
		if dropped.cause != nil {
			attrs = append(attrs, "error", redact.Secrets(dropped.cause.Error()))
		}
		s.t.svc.log.InfoContext(ctx, "assistant_brief_row_dropped", attrs...)
		s.t.svc.metrics.Add(metrics.AssistantBriefRowsDropped, 1, nil)
		return nil, nil
	}
	return record, err
}

// urgentIn reports whether a record's own read carries a fact a danger row
// needs: a seat stopped or whose last turn failed, or a task marked urgent.
// Every other kind carries none.
func urgentIn(kind wire.ResourceKind, body []byte) bool {
	switch kind {
	case wire.ResourceSeat:
		var seat struct {
			Live struct {
				Activity      string          `json:"activity"`
				StoppedReason json.RawMessage `json:"stopped_reason"`
				LastTurn      *struct {
					Outcome string `json:"outcome"`
				} `json:"last_turn"`
			} `json:"live"`
		}
		if json.Unmarshal(body, &seat) != nil {
			return false
		}
		stopped := seat.Live.Activity == "stopped" ||
			(len(seat.Live.StoppedReason) > 0 && string(seat.Live.StoppedReason) != "null")
		failed := seat.Live.LastTurn != nil && seat.Live.LastTurn.Outcome == "failed"
		return stopped || failed
	case wire.ResourceTask:
		var task struct {
			Task struct {
				Priority string `json:"priority"`
			} `json:"task"`
		}
		return json.Unmarshal(body, &task) == nil && task.Task.Priority == "urgent"
	}
	return false
}

// briefMeter is a brief turn's meter: every charge goes to the person's and
// the company's windows as any turn's does, and the brief's own cap,
// [BriefMaxTokens], refuses past it. The inner meter is told first, so a
// round the cap refuses is still recorded where the company counts it.
type briefMeter struct {
	inner toolloop.BudgetMeter

	mu    sync.Mutex
	spent int
}

var _ toolloop.BudgetMeter = (*briefMeter)(nil)

// Spend charges the windows, then the brief's cap.
func (m *briefMeter) Spend(ctx context.Context, tokens int) (toolloop.SpendOutcome, error) {
	if m.inner != nil {
		outcome, err := m.inner.Spend(ctx, tokens)
		if err != nil || !outcome.OK {
			m.add(tokens)
			return outcome, err
		}
	}
	if spent := m.add(tokens); spent > BriefMaxTokens {
		return m.refusal(spent), nil
	}
	return toolloop.SpendOutcome{OK: true}, nil
}

// Refused is the windows' standing refusal, or the brief's own once its cap
// is spent.
func (m *briefMeter) Refused(ctx context.Context) (toolloop.SpendOutcome, bool) {
	if m.inner != nil {
		if outcome, refused := m.inner.Refused(ctx); refused {
			return outcome, true
		}
	}
	m.mu.Lock()
	spent := m.spent
	m.mu.Unlock()
	if spent >= BriefMaxTokens {
		return m.refusal(spent), true
	}
	return toolloop.SpendOutcome{}, false
}

func (m *briefMeter) add(tokens int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spent += tokens
	return m.spent
}

func (m *briefMeter) refusal(spent int) toolloop.SpendOutcome {
	return toolloop.SpendOutcome{Scope: BriefBudgetScope, Used: spent, Limit: BriefMaxTokens}
}
