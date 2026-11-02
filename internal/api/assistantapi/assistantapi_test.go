package assistantapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/assistantapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tabular"
)

// grants derives a read grant of a fixed id per turn, valid for five minutes.
type grants struct{ now func() time.Time }

var _ approval.Grants = grants{}

func (g grants) Read(_ context.Context, _, turn uuid.UUID) (exec.Grant, error) {
	return evals.Grant{GrantID: evals.ID("grant/" + turn.String()),
		ExpiresAt: g.now().Add(5 * time.Minute)}, nil
}

func (grants) Exec(context.Context, approval.Exec) (exec.Grant, error) {
	return nil, errors.New("assistantapi_test: a read turn derives no exec grant")
}

func (grants) Revoke(uuid.UUID) {}

// site is the surface over a rig, mounted on a mux.
type site struct {
	rig     *assistanttest.Rig
	surface *assistantapi.Surface
	mux     *http.ServeMux
}

func newSite(t *testing.T, o evals.RigOptions) *site {
	t.Helper()
	rig := assistanttest.NewRig(t, o)
	surface, err := assistantapi.New(assistantapi.Options{Service: rig.Svc,
		Grants: grants{now: rig.Clock.Now}, Tables: tabular.Tables{}, Chart: authz.NoChart{},
		Now: rig.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := surface.Routes(mux); err != nil {
		t.Fatal(err)
	}
	return &site{rig: rig, surface: surface, mux: mux}
}

// opKey is an Idempotency-Key a dashboard would mint.
const opKey = "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a12"

// do serves one request as p.
func (s *site) do(p iam.Principal, method, path string, body any, header map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(context.Background(), p), method,
		path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// ask asks a question as the rig's person, over the route.
func (s *site) ask(t *testing.T, text string) wire.TurnAccepted {
	t.Helper()
	rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
		s.rig.Conversation.String()+"/turns", wire.TurnRequest{ProtocolVersion: wire.ProtocolVersion,
		Input: wire.TurnInput{Text: text}}, map[string]string{"Idempotency-Key": opKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	var accepted wire.TurnAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	return accepted
}

// stream reads a turn's stream over the route, resuming after last when set.
func (s *site) stream(t *testing.T, accepted wire.TurnAccepted, last string) []byte {
	t.Helper()
	header := map[string]string{}
	if last != "" {
		header["Last-Event-ID"] = last
	}
	rec := s.do(s.rig.Person, http.MethodGet, accepted.StreamURL, nil, header)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"),
		"text/event-stream") {
		t.Fatalf("stream = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	return rec.Body.Bytes()
}

// fixture is a stream as the golden file holds it: every frame decoded as the
// client decodes it, and re-encoded with the one value a run cannot fix, the
// turn's trace id, written as a constant.
func fixture(t *testing.T, body []byte) []byte {
	return assistanttest.Stream(t, frames(t, body)...)
}

// The constants a golden writes a fresh proposal's id and digest as.
var (
	fixedProposal = evals.ID("proposal")
	fixedDigest   = "sha256:" + strings.Repeat("0f", 32)
)

// frames is a stream as the client decodes it, normalised as [fixture] says.
func frames(t *testing.T, body []byte) []wire.Frame {
	t.Helper()
	dec := wire.NewDecoder(bytes.NewReader(body))
	var out []wire.Frame
	for {
		f, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the route wrote a stream its client refuses: %v", err)
		}
		if f.Done != nil && f.Done.TraceID != "" {
			f.Done.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		}
		// A PROPOSAL'S ID AND DIGEST ARE MINTED FRESH (proposal.New), so
		// the golden writes them as constants; every other byte is the
		// route's own.
		if f.Card != nil && f.Card.Card.Proposal != nil {
			card := *f.Card.Card.Proposal
			card.ProposalID, card.Digest = fixedProposal, fixedDigest
			f.Card.Card.Proposal = &card
		}
		if f.Action != nil && f.Action.ProposalID != nil {
			// A page action's id IS its proposal's (proposal.PageAction).
			if f.Action.ActionID == *f.Action.ProposalID {
				f.Action.ActionID = fixedProposal
			}
			id := fixedProposal
			f.Action.ProposalID = &id
		}
		if f.Action != nil && f.Action.Name == wire.ActionContextInvalidate {
			// Derived from the proposal's id, so it moves with it.
			f.Action.ActionID = evals.ID("invalidate")
		}
		out = append(out, f)
	}
	return out
}

// readTurn is a turn that reads ENG-7 through the catalogue's own get_task.
func readTurn(t *testing.T) evals.RigOptions {
	return evals.RigOptions{Tools: []catalog.Tool{assistanttest.Tool(t, "get_task")},
		Handler: assistanttest.Routes(map[string]exec.Response{
			"GET /worklet/ENG-7": assistanttest.Task("ENG-7", "Rotate keys",
				"Rotate the signing keys before Friday."),
		}),
		Rounds: []evals.Round{
			evals.Call("c1", "get_task", map[string]any{"item": "ENG-7"}),
			evals.Say("ENG-7 is to rotate the signing keys before Friday.")}}
}

// THE READ TURN FIXTURE IS THE ROUTES' OWN: a question asked over the route
// and its stream read over the route, as the dashboard's suite replays it.
func TestTheReadTurnFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn(t))
	accepted := s.ask(t, "Who works here?")
	assistanttest.Golden(t, "testdata/read_turn.sse", fixture(t, s.stream(t, accepted, "")))
}

// THE RESUME FIXTURE IS THE ROUTES' OWN: a client that drew three frames and
// reconnected with Last-Event-ID is replayed exactly what it missed.
func TestTheResumeFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn(t))
	accepted := s.ask(t, "Who works here?")
	s.stream(t, accepted, "")
	resumed := fixture(t, s.stream(t, accepted, wire.EventID(accepted.TurnID, 3)))
	if !bytes.HasPrefix(resumed, []byte("id: "+wire.EventID(accepted.TurnID, 4)+"\n")) {
		t.Fatalf("the resumed stream does not open at seq 4:\n%s", resumed)
	}
	assistanttest.Golden(t, "testdata/resume.sse", resumed)
}

// EVERY ROUTE ANSWERS NOT FOUND FOR ANOTHER PERSON'S CONVERSATION (ADR-0053).
//
// Ada asks every route about Grace's conversation, its turn and its card, and
// is told each does not exist, exactly as for one that never did: no route
// says "forbidden", which would confirm it is there.
//
// Mutation: key any read on the conversation alone rather than its owner and
// that route answers 200.
func TestEveryAssistantRouteAnswersNotFoundForAnotherPersonsConversation(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn(t))
	accepted := s.ask(t, "Who works here?")
	s.stream(t, accepted, "")
	transcript, err := s.rig.Svc.Transcript(t.Context(), s.rig.Person, s.rig.Conversation)
	if err != nil || len(transcript.Cards) == 0 {
		t.Fatalf("grace's transcript = %+v, %v", transcript, err)
	}
	card := transcript.Cards[0].CardID
	ada := evals.Person("ada", "ada", iam.GrantWorklet)
	conv := "/assistant/conversations/" + s.rig.Conversation.String()
	versioned := wire.Versioned{ProtocolVersion: wire.ProtocolVersion}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, conv, nil},
		{http.MethodDelete, conv, nil},
		{http.MethodPost, conv + "/turns", wire.TurnRequest{ProtocolVersion: wire.ProtocolVersion,
			Input: wire.TurnInput{Text: "Read hers"}}},
		{http.MethodGet, accepted.StreamURL, nil},
		{http.MethodPost, "/assistant/turns/" + accepted.TurnID.String() + "/stop", versioned},
		{http.MethodGet, "/assistant/cards/" + card.String() + "/export.csv", nil},
		{http.MethodGet, "/assistant/cards/" + card.String() + "/export.xlsx", nil},
	} {
		rec := s.do(ada, c.method, c.path, c.body, map[string]string{
			"Idempotency-Key": "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a13"})
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("%s %s as ada = %d %s, want 404 not_found", c.method, c.path, rec.Code,
				rec.Body)
		}
	}
	if rec := s.do(s.rig.Person, http.MethodGet, conv, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("grace reading her own conversation = %d", rec.Code)
	}
}

// A REQUEST IN ANOTHER PROTOCOL VERSION IS REFUSED, asking the person to
// reload, before anything is started.
func TestAnotherProtocolVersionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn(t))
	rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
		s.rig.Conversation.String()+"/turns", wire.TurnRequest{ProtocolVersion: 2,
		Input: wire.TurnInput{Text: "Hi"}}, map[string]string{"Idempotency-Key": opKey})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(),
		`"assistant_protocol_mismatch"`) {
		t.Fatalf("protocol 2 = %d %s", rec.Code, rec.Body)
	}
	if n := len(s.rig.Model.Requests()); n != 0 {
		t.Errorf("a refused version reached the model %d times", n)
	}
}

// A QUESTION THE BUDGET REFUSES IS A 429 NAMING WHEN THE WINDOW RESETS, AND
// WHICH CEILING TO RAISE: the company's, or the one every person's assistant
// spend is held to, as its `remedy` (ADR-0060). The drawer opens that ceiling,
// so a remedy naming the wrong one sends the person to change a setting that
// does not refuse them.
//
// Mutation: drop the remedy from the route's detail and both cases fail.
func TestABudgetRefusalIsATooManyRequestsWithItsReset(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		scope   string
		setting caveat.Setting
	}{
		{"", caveat.SettingCompanyTokenBudget},
		{assistant.PersonBudgetScope, caveat.SettingAssistantTokenBudget},
	} {
		t.Run(string(c.setting), func(t *testing.T) {
			t.Parallel()
			o := readTurn(t)
			o.Budget = evals.Budget{M: assistanttest.FullMeter{
				ResetsAt: evals.Epoch.Add(2 * time.Hour), Scope: c.scope}}
			s := newSite(t, o)
			rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
				s.rig.Conversation.String()+"/turns", wire.TurnRequest{
				ProtocolVersion: wire.ProtocolVersion, Input: wire.TurnInput{Text: "Hi"}},
				map[string]string{"Idempotency-Key": opKey})
			if rec.Code != http.StatusTooManyRequests ||
				rec.Header().Get("Retry-After") != "7200" ||
				!strings.Contains(rec.Body.String(), `"budget_exhausted"`) {
				t.Fatalf("a refused budget = %d %q %s", rec.Code,
					rec.Header().Get("Retry-After"), rec.Body)
			}
			var answer struct {
				Remedy caveat.Remedy `json:"remedy"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
			if answer.Remedy != (caveat.Remedy{Setting: c.setting}) {
				t.Errorf("a refused budget named the remedy %+v, want %s: %s",
					answer.Remedy, c.setting, rec.Body)
			}
		})
	}
}

// THE VIEWER SAYS WHY THE ASSISTANT IS UNAVAILABLE, naming the first reason in
// the order the pill reads them, and lit for a person when nothing stops it.
//
// Mutation: answer every refusal with one reason and the cases disagree.
func TestTheViewerSaysWhyTheAssistantIsUnavailable(t *testing.T) {
	t.Parallel()
	on := true
	unusable := &config.Company{Name: "Nimbus", Assistant: config.Assistant{Enabled: &on,
		LLM: config.ProviderKeys{"missing"}}}
	token := evals.Person("ci", "grace", iam.GrantWorklet)
	token.Via = "pat:" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("token")).String()
	for _, c := range []struct {
		name   string
		o      evals.RigOptions
		who    iam.Principal
		reason wire.ViewerReason
	}{
		{"off", evals.RigOptions{Off: true}, evals.Person("grace", "grace"),
			wire.ViewerDisabled},
		{"no usable chain", evals.RigOptions{Company: func() (*config.Company, *org.Organization) {
			return unusable, nil
		}}, evals.Person("grace", "grace"), wire.ViewerNoModel},
		{"a machine token", evals.RigOptions{}, token, wire.ViewerNotAPerson},
		{"available", evals.RigOptions{}, evals.Person("grace", "grace"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := newSite(t, c.o).surface.Viewer(t.Context(), c.who)
			if got.Reason != c.reason || got.Available != (c.reason == "") {
				t.Errorf("viewer = %+v, want reason %q", got, c.reason)
			}
		})
	}
}

// THE VIEWER SAYS A MODEL IS NOT READY BEFORE THE PERSON ASKS, with what a
// failed round would have said, and only to a person the assistant answers: a
// caller refused on an earlier reason learns nothing of its model. Asking
// anyway is refused before any round, 503 with the same words and, for a spent
// plan, the wait until its reset.
//
// Mutation: check the model before the earlier reasons, and the machine token
// is told about it; drop the model check from Viewer, and the person is told
// the assistant is available.
func TestTheViewerSaysAModelIsNotReadyBeforeAnyoneAsks(t *testing.T) {
	t.Parallel()
	s := newSite(t, evals.RigOptions{})
	reset := s.rig.Clock.Now().Add(90 * time.Second)
	s.rig.Model.SetUnready(&llm.Error{Kind: llm.KindRateLimit, Key: "assistant", RetryAt: reset,
		Err: errors.New("Usage limit reached")})

	got := s.surface.Viewer(t.Context(), s.rig.Person)
	if got.Available || got.Reason != wire.ViewerModelNotReady || got.Model == nil ||
		got.Model.Code != wire.ErrorModelBusy || got.Model.RetryAt == nil || !got.Model.RetryAt.Equal(reset) {
		t.Fatalf("viewer = %+v (model %+v), want model_not_ready naming model_busy until %v",
			got, got.Model, reset)
	}
	token := s.rig.Person
	token.Via = "pat:" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("token")).String()
	if other := s.surface.Viewer(t.Context(), token); other.Reason != wire.ViewerNotAPerson ||
		other.Model != nil {
		t.Errorf("a machine token's viewer = %+v, want not_a_person and nothing of the model", other)
	}

	rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
		s.rig.Conversation.String()+"/turns", wire.TurnRequest{ProtocolVersion: wire.ProtocolVersion,
		Input: wire.TurnInput{Text: "Hi"}}, map[string]string{"Idempotency-Key": opKey})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "90" ||
		body["error"] != string(httpjson.CodeAssistantModelNotReady) ||
		body["reason"] != string(wire.ErrorModelBusy) || body["detail"] != got.Model.Message {
		t.Fatalf("asking anyway = %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if n := len(s.rig.Model.Requests()); n != 0 {
		t.Errorf("the model was asked %d rounds", n)
	}
}

// A MACHINE TOKEN IS REFUSED EVERY ROUTE by the authority table, before any
// handler reads its request: using the assistant needs a person present.
func TestAMachineTokenIsRefusedEveryRoute(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn(t))
	token := s.rig.Person
	token.Via = "pat:" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("token")).String()
	for _, path := range []string{"/assistant/conversations",
		"/assistant/conversations/" + s.rig.Conversation.String()} {
		if rec := s.do(token, http.MethodGet, path, nil, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a machine token = %d %s", path, rec.Code, rec.Body)
		}
	}
}

// THE WRITE TURN FIXTURE IS THE ROUTES' OWN: a question that proposes a task
// through the catalogue's own create_task, parks on the proposal, and, once
// the approval path hands the turn its receipt, the card and the
// context.invalidate the approval landed, resumes and answers.
func TestTheWriteTurnFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "create_task")},
		Acts:  []string{"create_work_item"},
		Rounds: []evals.Round{
			evals.Call("c1", "create_task", map[string]any{"title": "Rotate keys"}),
			evals.Say("I filed ENG-8 to rotate the keys.")}})
	accepted := s.ask(t, "File a task to rotate the keys")
	parked := s.stream(t, accepted, "")
	proposals := s.rig.Store.Proposals()
	if len(proposals) != 1 {
		t.Fatalf("the turn recorded %d proposals, want 1", len(proposals))
	}
	p := proposals[0]
	applied := p.Card
	card := *applied.Proposal
	card.State = wire.ProposalApplied
	card.Receipt = &wire.Receipt{Summary: "Created ENG-8.",
		Position: "CREWLET_TRACKER_LOG@1:42"}
	applied.Proposal = &card
	resumed, err := s.rig.Svc.Resume(iam.WithPrincipal(context.Background(), s.rig.Person),
		approval.Resumption{Owner: s.rig.Person.ID, Conversation: s.rig.Conversation,
			Turn: accepted.TurnID, Proposal: p.ID, Read: s.rig.Grant("approve"),
			State: wire.ProposalApplied, Receipt: card.Receipt, Card: applied,
			Invalidate: &wire.Invalidate{Domain: "tracker", Position: "CREWLET_TRACKER_LOG@1:42"}})
	if err != nil {
		t.Fatal(err)
	}
	rest := s.stream(t, wire.TurnAccepted{TurnID: accepted.TurnID, StreamURL: resumed.StreamURL},
		wire.EventID(accepted.TurnID, resumed.AfterSeq))
	all := append(frames(t, parked), frames(t, rest)...)
	if last := all[len(all)-1]; last.Done == nil || last.Done.State != wire.TurnCompleted {
		t.Fatalf("the write turn ended %+v", last)
	}
	assistanttest.Golden(t, "testdata/write_turn.sse", assistanttest.Stream(t, all...))
}

// THE PAGE ACTION TURN FIXTURE IS THE ROUTES' OWN: a question asked on the
// open screen changes its view at once (a `confirm: none` action, D11),
// prepared against the screen's own revision, handed to the page as an action
// frame with no card and no wait, and the turn answers on.
func TestThePageActionTurnFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "propose_page_change")},
		Rounds: []evals.Round{
			evals.Call("c1", "propose_page_change", map[string]any{
				"action": "work.filter.apply", "args": map[string]any{"status": "todo"}}),
			evals.Say("Showing only the tasks still to do.")}})
	page := &wire.PageContext{Screen: "worklet-tasks", Title: "Worklet \u203a Tasks",
		Params: map[string]string{}, Query: map[string]string{}, Resources: []wire.Resource{},
		SourceRevision: "work@3", Accepts: []string{"work.filter.apply"},
		ActionArgs: map[string]*wire.ActionArgs{"work.filter.apply": assistanttest.StatusFilterArgs()}}
	rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
		s.rig.Conversation.String()+"/turns", wire.TurnRequest{ProtocolVersion: wire.ProtocolVersion,
		Input: wire.TurnInput{Text: "Show only what is still to do"}, PageContext: page},
		map[string]string{"Idempotency-Key": opKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	var accepted wire.TurnAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	body := s.stream(t, accepted, "")
	drawn := frames(t, body)
	action := evals.Last(drawn, wire.EventAction)
	if action == nil || action.Action.Confirm != wire.ConfirmNone ||
		action.Action.SourceRevision != "work@3" || action.Action.ProposalID == nil {
		t.Fatalf("the page action = %+v, want one applied at once against work@3", action)
	}
	if card := evals.Last(drawn, wire.EventCard); card != nil {
		t.Errorf("a view change drew a card %+v; its receipt is the page's", card)
	}
	if done := evals.Last(drawn, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("the turn ended %+v, want completed without parking", done)
	}
	assistanttest.Golden(t, "testdata/page_action_turn.sse", fixture(t, body))
}
