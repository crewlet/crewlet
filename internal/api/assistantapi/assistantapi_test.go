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
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// grants derives a read grant of a fixed id per turn, valid for five minutes.
type grants struct{ now func() time.Time }

var _ approval.Grants = grants{}

func (g grants) Read(_ context.Context, _, turn uuid.UUID) (exec.Grant, error) {
	return assistanttest.Grant{GrantID: assistanttest.ID("grant/" + turn.String()),
		ExpiresAt: g.now().Add(5 * time.Minute)}, nil
}

func (grants) Exec(context.Context, approval.Exec) (exec.Grant, error) {
	return nil, errors.New("assistantapi_test: a read turn derives no exec grant")
}

func (grants) Revoke(uuid.UUID) {}

// tables writes a table's titles, one per line.
type tables struct{}

func (tables) CSV(w http.ResponseWriter, t wire.TableCard) error {
	_, err := io.WriteString(w, t.Title+"\n")
	return err
}

func (tables) XLSX(w http.ResponseWriter, t wire.TableCard) error { return tables{}.CSV(w, t) }

// site is the surface over a rig, mounted on a mux.
type site struct {
	rig     *assistanttest.Rig
	surface *assistantapi.Surface
	mux     *http.ServeMux
}

func newSite(t *testing.T, o assistanttest.RigOptions) *site {
	t.Helper()
	rig := assistanttest.NewRig(t, o)
	surface, err := assistantapi.New(assistantapi.Options{Service: rig.Svc,
		Grants: grants{now: rig.Clock.Now}, Tables: tables{}, Chart: authz.NoChart{},
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
	t.Helper()
	dec := wire.NewDecoder(bytes.NewReader(body))
	var frames []wire.Frame
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
		frames = append(frames, f)
	}
	return assistanttest.Stream(t, frames...)
}

var peopleTool = assistanttest.ReadTool("list_people", "/people", true, iam.GrantTracker)

func people(exec.Request) exec.Response {
	return assistanttest.JSON([]map[string]any{{"name": "Ada"}, {"name": "Grace"}})
}

func readTurn() assistanttest.RigOptions {
	return assistanttest.RigOptions{Tools: []catalog.Tool{peopleTool}, Handler: people,
		Rounds: []assistanttest.Round{assistanttest.Call("c1", "list_people", nil),
			assistanttest.Say("There are two people: Ada and Grace.")}}
}

// THE READ TURN FIXTURE IS THE ROUTES' OWN: a question asked over the route
// and its stream read over the route, as the dashboard's suite replays it.
func TestTheReadTurnFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn())
	accepted := s.ask(t, "Who works here?")
	assistanttest.Golden(t, "testdata/read_turn.sse", fixture(t, s.stream(t, accepted, "")))
}

// THE RESUME FIXTURE IS THE ROUTES' OWN: a client that drew three frames and
// reconnected with Last-Event-ID is replayed exactly what it missed.
func TestTheResumeFixtureIsTheRoutesOwn(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn())
	accepted := s.ask(t, "Who works here?")
	s.stream(t, accepted, "")
	resumed := fixture(t, s.stream(t, accepted, wire.EventID(accepted.TurnID, 3)))
	if !bytes.HasPrefix(resumed, []byte("id: "+wire.EventID(accepted.TurnID, 4)+"\n")) {
		t.Fatalf("the resumed stream does not open at seq 4:\n%s", resumed)
	}
	assistanttest.Golden(t, "testdata/resume.sse", resumed)
}

// EVERY ROUTE ANSWERS NOT FOUND FOR ANOTHER PERSON'S CONVERSATION (ADR-0049).
//
// Ada asks every route about Grace's conversation, its turn and its card, and
// is told each does not exist, exactly as for one that never did: no route
// says "forbidden", which would confirm it is there.
//
// Mutation: key any read on the conversation alone rather than its owner and
// that route answers 200.
func TestEveryAssistantRouteAnswersNotFoundForAnotherPersonsConversation(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn())
	accepted := s.ask(t, "Who works here?")
	s.stream(t, accepted, "")
	transcript, err := s.rig.Svc.Transcript(t.Context(), s.rig.Person, s.rig.Conversation)
	if err != nil || len(transcript.Cards) == 0 {
		t.Fatalf("grace's transcript = %+v, %v", transcript, err)
	}
	card := transcript.Cards[0].CardID
	ada := assistanttest.Person("ada", "ada", iam.GrantTracker)
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
	s := newSite(t, readTurn())
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

// A QUESTION THE BUDGET REFUSES IS A 429 NAMING WHEN THE WINDOW RESETS.
func TestABudgetRefusalIsATooManyRequestsWithItsReset(t *testing.T) {
	t.Parallel()
	o := readTurn()
	o.Budget = assistanttest.Budget{M: assistanttest.FullMeter{
		ResetsAt: assistanttest.Epoch.Add(2 * time.Hour)}}
	s := newSite(t, o)
	rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
		s.rig.Conversation.String()+"/turns", wire.TurnRequest{ProtocolVersion: wire.ProtocolVersion,
		Input: wire.TurnInput{Text: "Hi"}}, map[string]string{"Idempotency-Key": opKey})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "7200" ||
		!strings.Contains(rec.Body.String(), `"budget_exhausted"`) {
		t.Fatalf("a refused budget = %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
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
	token := assistanttest.Person("ci", "grace", iam.GrantTracker)
	token.Via = "pat:" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("token")).String()
	for _, c := range []struct {
		name   string
		o      assistanttest.RigOptions
		who    iam.Principal
		reason wire.ViewerReason
	}{
		{"off", assistanttest.RigOptions{Off: true}, assistanttest.Person("grace", "grace"),
			wire.ViewerDisabled},
		{"no usable chain", assistanttest.RigOptions{Company: func() (*config.Company, *org.Organization) {
			return unusable, nil
		}}, assistanttest.Person("grace", "grace"), wire.ViewerNoModel},
		{"a machine token", assistanttest.RigOptions{}, token, wire.ViewerNotAPerson},
		{"available", assistanttest.RigOptions{}, assistanttest.Person("grace", "grace"), ""},
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

// A MACHINE TOKEN IS REFUSED EVERY ROUTE by the authority table, before any
// handler reads its request: using the assistant needs a person present.
func TestAMachineTokenIsRefusedEveryRoute(t *testing.T) {
	t.Parallel()
	s := newSite(t, readTurn())
	token := s.rig.Person
	token.Via = "pat:" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("token")).String()
	for _, path := range []string{"/assistant/conversations",
		"/assistant/conversations/" + s.rig.Conversation.String()} {
		if rec := s.do(token, http.MethodGet, path, nil, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a machine token = %d %s", path, rec.Code, rec.Body)
		}
	}
}
