package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// seededApp is an app whose sources hold a known company's worth of history.
func seededApp(t *testing.T, mutate func(*api.Options)) *api.App {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "q.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Now().UTC().Add(-time.Hour)
	for i := range 6 {
		if err := db.Events().Append(t.Context(), store.EventRecord{
			ID: "ev" + string(rune('a'+i)), Type: "agent_phase_started", Source: "engine",
			Time: base.Add(time.Duration(i) * time.Second), Category: "task",
			Actor: "Lead", Summary: "did a thing", TraceID: "tr-1",
			Payload: json.RawMessage(`{"role":"Lead"}`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// A COMPANY, so a seat question has a seat to resolve: `agent` is asked
	// by handle and answered from the projection by the agent id it
	// resolves to.
	company, err := config.ParseCompany([]byte(rosterCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	organization, err := company.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	ceo, ok := organization.AgentIDFor(organization.AgentSeatByHandle("ceo"))
	if !ok {
		t.Fatal("the fixture's CEO is no agent seat")
	}

	state := livestate.New()
	state.Apply(&livestate.Envelope{
		ID: "e1", Type: "agent_phase_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Category: "task", Payload: map[string]any{
			"agent_id": ceo.String(), "role": "CEO", "task_id": "t-1",
		},
	})

	// AND A NODE HOLDING THE SEAT the live state shows at work: a seat no
	// node holds is `unplaced`, which the socket's placement tick writes
	// onto the shared projection the moment a socket opens — so with no
	// lease, the question asked over REST before any socket and again over
	// one after it read two different (and both honest) states.
	leases := coordmemory.New()
	if lease, err := leases.TryAcquire(t.Context(), coord.SeatResource(ceo),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Hour}); err != nil || lease == nil {
		t.Fatalf("hold the CEO's seat: %v (%v)", err, lease)
	}

	opts := api.Options{
		State:    state,
		EventLog: db.Events(),
		Sources: queries.Sources{
			State: state, Events: eventfan.Solo("node-a", db.Events()),
			Usage: db.Replicated(), Company: companySource(t, company),
			Coord: leases,
		},
		Now: func() time.Time { return clock },
	}
	if mutate != nil {
		mutate(&opts)
	}
	return newApp(t, opts)
}

// overREST asks a question over HTTP.
func overREST(t *testing.T, a *api.App, what string, params url.Values) (int, any) {
	t.Helper()
	target := "/query/" + what
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, target, nil)))
	res := rec.Result()
	var body any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	return res.StatusCode, body
}

// overSocket asks the same question over the live channel.
func overSocket(t *testing.T, a *api.App, what string, params map[string]any) map[string]any {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)

	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fixtureToken}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	read := func() map[string]any {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return got
	}
	read() // the snapshot

	frame, err := json.Marshal(map[string]any{
		"kind": "query", "id": 1, "what": what, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Skip any live push that raced the answer.
	for range 20 {
		got := read()
		if got["kind"] == "result" || got["kind"] == "error" {
			return got
		}
	}
	t.Fatalf("%s was never answered", what)
	return nil
}

// TestBothTransportsAnswerTheSameQuestionIdentically is the point of the whole
// query package.
//
// Two surfaces answering one question from two implementations is how they end
// up disagreeing with nobody noticing — a filter honoured on one path and
// ignored on the other, a limit clamped differently, a field present over HTTP
// and missing over the socket. This compares the two answers directly.
func TestBothTransportsAnswerTheSameQuestionIdentically(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)

	for _, tc := range []struct {
		what   string
		rest   url.Values
		socket map[string]any
	}{
		{"agent", url.Values{"id": {"ceo"}}, map[string]any{"id": "ceo"}},
		{"events", url.Values{"limit": {"3"}}, map[string]any{"limit": float64(3)}},
		{"events", url.Values{"actor": {"Lead"}}, map[string]any{"actor": "Lead"}},
		{"trace", url.Values{"trace_id": {"tr-1"}}, map[string]any{"trace_id": "tr-1"}},
		{"tokens", nil, nil},
	} {
		status, restBody := overREST(t, a, tc.what, tc.rest)
		if status != http.StatusOK {
			t.Errorf("%s over REST: status = %d (%v)", tc.what, status, restBody)
			continue
		}
		socketFrame := overSocket(t, a, tc.what, tc.socket)
		if socketFrame["kind"] != "result" {
			t.Errorf("%s over the socket: %v", tc.what, socketFrame)
			continue
		}
		// Compared after a JSON round trip on both sides, which is what
		// each transport actually delivers — minus the one field that
		// legitimately differs, since asking over the socket opens a
		// connection and the health body counts them.
		rest, socket := withoutClientCount(restBody), withoutClientCount(socketFrame["data"])
		if !reflect.DeepEqual(rest, socket) {
			t.Errorf("%s answered differently:\n  REST   %#v\n  socket %#v",
				tc.what, rest, socket)
		}
	}
}

// AN APP'S CLOCK IS THE CLOCK ITS QUESTIONS ARE ANSWERED ON.
//
// The case above asks each question once per transport, at two instants, and
// compares the answers — so an answer that reads the wall clock can only agree
// with itself when both asks land in the same second. `tokens` labels its
// window with the instant it was asked, and the case failed under load when
// its REST ask and its socket ask straddled a second: the app's clock was
// pinned ([seededApp]), and never reached the questions, which kept a wall
// clock of their own.
func TestAPinnedAppClockPinsItsAnswers(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)
	status, body := overREST(t, a, "tokens", nil)
	if status != http.StatusOK {
		t.Fatalf("tokens: status = %d (%v)", status, body)
	}
	answer, _ := body.(map[string]any)
	if want := clock.Format(time.RFC3339); answer["until"] != want {
		t.Errorf("tokens ends at %v, want the app's clock %s — an answer on the "+
			"wall clock differs from itself one second later", answer["until"], want)
	}
}

func TestAFilterIsHonouredOnBothTransports(t *testing.T) {
	t.Parallel()
	// The specific divergence the shared accessors exist to prevent: a
	// query string spells a limit as text and a socket frame spells it as
	// a number, and a reader per transport is where one of them starts
	// being ignored.
	a := seededApp(t, nil)

	_, restBody := overREST(t, a, "events", url.Values{"limit": {"2"}})
	restRows := len(restBody.(map[string]any)["events"].([]any))

	socket := overSocket(t, a, "events", map[string]any{"limit": float64(2)})
	socketRows := len(socket["data"].(map[string]any)["events"].([]any))

	if restRows != 2 || socketRows != 2 {
		t.Errorf("limit honoured as REST=%d socket=%d, want 2 and 2", restRows, socketRows)
	}
}

func TestAnUnknownQuestionIsRefusedOnBothTransports(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)

	status, body := overREST(t, a, "nonsense", nil)
	if status != http.StatusNotFound {
		t.Errorf("REST status = %d, want 404", status)
	}
	if got := body.(map[string]any)["error"]; got != "unknown_query" {
		t.Errorf("REST error = %v", got)
	}
	// THE ENVELOPE, sentence and all. The query surface answered its
	// refusals as a bare `{"error": code}`, so the family of routes a
	// dashboard reads most was the one whose refusals had nothing to show.
	if got := body.(map[string]any)["message"]; got != httpjson.CodeUnknownQuery.Message() {
		t.Errorf("REST message = %v, want the code's own sentence", got)
	}

	socket := overSocket(t, a, "nonsense", nil)
	if socket["kind"] != "error" || socket["error"] != "unknown_query" {
		t.Errorf("socket answer = %v", socket)
	}
}

func TestABadParameterIsRefusedRatherThanGuessedAt(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)
	// A cursor missing half its key would skip or repeat whatever collided
	// with it, silently.
	status, body := overREST(t, a, "events", url.Values{"before_id": {"ev1"}})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if got := body.(map[string]any)["error"]; got != "bad_params" {
		t.Errorf("REST error = %v, want bad_params — `query_failed` names a "+
			"fault of this node for a request the caller has to change", got)
	}
	if got := body.(map[string]any)["message"]; got != httpjson.CodeBadParams.Message() {
		t.Errorf("REST message = %v, want the code's own sentence", got)
	}

	// AND THE SOCKET SAYS THE SAME THING. It said `query_failed` — the
	// code a client retries — so a screen polling a question it was
	// malforming retried for ever, while this node logged a warning per
	// tick about a request that was never its fault.
	socket := overSocket(t, a, "events", map[string]any{"before_id": "ev1"})
	if socket["kind"] != "error" || socket["error"] != "bad_params" {
		t.Errorf("socket answer = %v, want bad_params", socket)
	}
}

// THE REFUSAL'S SENTENCE REACHES THE CALLER, ON BOTH TRANSPORTS, WORD FOR WORD.
//
// A spend window past the engine's ninety days is refused naming `days` and
// the bound — the one thing a person who typed a window can act on. It used to
// reach the debug log only, so the Spend screen could say nothing but "the
// engine refused this request". And the two transports carry the SAME
// sentence, with the package's sentinel taken out of it: "queries: bad
// parameters" is a Go package's name for the class, not something to read.
func TestARefusalsSentenceReachesTheCallerOnBothTransports(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)

	status, body := overREST(t, a, "tokens", url.Values{"days": {"91"}})
	if status != http.StatusBadRequest {
		t.Fatalf("REST status = %d, want 400", status)
	}
	rest, _ := body.(map[string]any)["detail"].(string)
	socket := overSocket(t, a, "tokens", map[string]any{"days": 91})
	sock, _ := socket["detail"].(string)

	for name, detail := range map[string]string{"REST": rest, "socket": sock} {
		if !strings.Contains(detail, "days is 91") || !strings.Contains(detail, "at most 90") {
			t.Errorf("%s detail = %q, want the refusal naming days, 91 and the bound", name, detail)
		}
		// NO CLASS NAME, the engine's or the finer one: `tokens:` twice over
		// is what the Spend screen used to show in front of this sentence.
		for _, class := range []error{queries.ErrBadParams, tokens.ErrWindowLength} {
			if strings.Contains(detail, class.Error()) {
				t.Errorf("%s detail = %q still carries %q", name, detail, class)
			}
		}
		if strings.HasPrefix(detail, "tokens") {
			t.Errorf("%s detail = %q opens with the question's name", name, detail)
		}
	}
	if rest != sock {
		t.Errorf("the transports disagree about the sentence:\n REST   %q\n socket %q", rest, sock)
	}
}

func TestAFailingQuestionReportsACodeAndNothingElse(t *testing.T) {
	t.Parallel()
	// The reason reaches the LOG, not the caller: it can carry a database
	// path or a driver's own message, and holding the grant a question
	// needs does not make a caller somebody that path is meant for.
	a := seededApp(t, nil)
	a.Queries().Register("boom", iam.GrantStateRead, func(context.Context, queries.Params) (any, error) {
		return nil, errors.New("open /var/lib/crewlet/crewlet.db: permission denied")
	})

	status, body := overREST(t, a, "boom", nil)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "query_failed") {
		t.Errorf("body = %s, want the code", raw)
	}
	if strings.Contains(string(raw), "/var/lib") {
		t.Errorf("the failure leaked its detail to the caller: %s", raw)
	}
}

// A COORDINATION BLIP IS "ASK AGAIN" ON BOTH TRANSPORTS: a 503 with a
// Retry-After over REST and `unavailable` on the socket. It was a 500 and
// `query_failed`, the pair a client gives up on.
func TestAnUnreachableCoordinationStoreIsUnavailableOnBothTransports(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)
	a.Queries().Register("blip", iam.GrantStateRead, func(context.Context, queries.Params) (any, error) {
		return nil, fmt.Errorf("list leases: %w", coord.ErrUnavailable)
	})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/query/blip", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("REST status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("a 503 with no Retry-After tells a client nothing about when to come back")
	}
	if !strings.Contains(rec.Body.String(), `"unavailable"`) {
		t.Errorf("REST body = %s, want the unavailable code", rec.Body.String())
	}
	socket := overSocket(t, a, "blip", nil)
	if socket["error"] != "unavailable" {
		t.Errorf("socket answer = %v, want unavailable", socket)
	}
	// NO REFUSAL IS BEHIND IT, and the hint is the health tick's.
	if _, named := socket["refusal"]; named {
		t.Errorf("a coordination blip named a state-log refusal: %v", socket)
	}
	if got := socket["retry_after"]; got != float64(stream.HealthInterval/time.Second) {
		t.Errorf("socket retry_after = %v, want the health tick's %s", got,
			stream.HealthInterval)
	}
}

// A NATIVE QUESTION ON A NODE WITH NO COMPANY IS ONE ANSWER ON BOTH TRANSPORTS.
//
// The company's own tracker and knowledge base come up with its first
// revision, and until then every question over them is answered for what it
// is: no company yet, come back at the reconcile poll, and why. REST answered
// that — `503 no_active_revision`, the poll's fifteen seconds and the halves'
// sentence — while the socket read the same error through the reading it
// shares with REST, found no state-log refusal behind it and sent a bare
// `unavailable` at the health tick's five: one question, two hints, and the
// reason on one channel only. And a half the company keeps with a vendor is
// the question this node does not have, on both.
//
// Mutation: drop the no-company arm from [stream.UnavailableOf] and the socket
// answers five seconds and no refusal.
func TestANativeQuestionOnANodeWithNoCompanyIsOneAnswerOnBothTransports(t *testing.T) {
	t.Parallel()
	poll := httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry)

	t.Run("no company yet", func(t *testing.T) {
		t.Parallel()
		a := seededApp(t, nil)
		a.Queries().Register("board", iam.GrantStateRead,
			func(context.Context, queries.Params) (any, error) {
				return nil, api.NativeAbsent(false, "tracker")
			})

		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/query/board", nil)))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("REST body: %v", err)
		}
		if rec.Code != http.StatusServiceUnavailable ||
			body["error"] != string(httpjson.CodeNoActiveRevision) ||
			body["detail"] != httpjson.NativeHalvesNotUp {
			t.Errorf("REST = %d %v, want 503 %s with the halves' sentence",
				rec.Code, body, httpjson.CodeNoActiveRevision)
		}
		if got := rec.Header().Get("Retry-After"); got != fmt.Sprint(poll) {
			t.Errorf("REST Retry-After = %q, want the reconcile poll's %d", got, poll)
		}

		socket := overSocket(t, a, "board", nil)
		if socket["error"] != "unavailable" ||
			socket["refusal"] != string(httpjson.CodeNoActiveRevision) ||
			socket["detail"] != httpjson.NativeHalvesNotUp {
			t.Errorf("socket answer = %v, want unavailable naming %s in the "+
				"halves' words", socket, httpjson.CodeNoActiveRevision)
		}
		if got := socket["retry_after"]; got != float64(poll) {
			t.Errorf("socket retry_after = %v, want the reconcile poll's %d — "+
				"what REST told the same caller", got, poll)
		}
	})

	t.Run("a half the company keeps elsewhere", func(t *testing.T) {
		t.Parallel()
		a := seededApp(t, nil)
		a.Queries().Register("board", iam.GrantStateRead,
			func(context.Context, queries.Params) (any, error) {
				return nil, api.NativeAbsent(true, "tracker")
			})
		status, answered := overREST(t, a, "board", nil)
		body, _ := answered.(map[string]any)
		if status != http.StatusNotFound || body["error"] != "unknown_query" {
			t.Errorf("REST = %d %v, want 404 unknown_query", status, body)
		}
		if socket := overSocket(t, a, "board", nil); socket["error"] != "unknown_query" {
			t.Errorf("socket answer = %v, want unknown_query", socket)
		}
	})
}

// A STATE-LOG REFUSAL IS UNAVAILABLE ON BOTH TRANSPORTS, CARRYING ITS CODE,
// ITS WORDS AND ITS OWN HINT.
//
// A linearizable read whose barrier the broker refused — a full log, a sealed
// stream — reached the caller as a 500 `query_failed` with the refusal's words
// sent only to the log, and a refusal waiting cannot clear, a record this node
// cannot decode, always had. Neither is a fault. Both transports now answer
// `unavailable` with the refusal's code and detail, and the hint is the state
// log's: none at all for a refusal no wait changes, so a client is told what
// to do rather than to poll, and the derived hint for a node that is behind.
func TestAStateLogRefusalIsUnavailableOnBothTransports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refused *statelog.Refused
		retry   string
	}{
		{"a log at its byte ceiling", &statelog.Refused{Code: statelog.RefuseLogFull,
			Level: statelog.ReadLinearizable, Detail: "raise the stream's byte ceiling"}, ""},
		{"a barrier the broker refused", &statelog.Refused{Code: statelog.RefuseBrokerRefused,
			Level: statelog.ReadLinearizable, Detail: "code 10109: sealed"}, ""},
		// THE CONTROL: a refusal that clears keeps its derived hint.
		{"a node that is behind", &statelog.Refused{Code: statelog.RefuseBehind,
			Level: statelog.ReadLinearizable, Detail: "40 000 records behind",
			RetryAfter: 12 * time.Second}, "12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := seededApp(t, nil)
			a.Queries().Register("refused", iam.GrantStateRead,
				func(context.Context, queries.Params) (any, error) {
					return nil, fmt.Errorf("read the board: %w", tc.refused)
				})

			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/query/refused", nil)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("REST status = %d, want 503 — a refusal is not a fault", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.retry {
				t.Errorf("REST Retry-After = %q, want %q", got, tc.retry)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("REST body: %v", err)
			}
			if body["error"] != "unavailable" || body["refusal"] != string(tc.refused.Code) ||
				body["detail"] != tc.refused.Detail {
				t.Errorf("REST body = %v, want unavailable carrying %s and its words",
					body, tc.refused.Code)
			}

			socket := overSocket(t, a, "refused", nil)
			if socket["error"] != "unavailable" || socket["refusal"] != string(tc.refused.Code) ||
				socket["detail"] != tc.refused.Detail {
				t.Errorf("socket answer = %v, want unavailable carrying %s and its words",
					socket, tc.refused.Code)
			}
			wantSeconds := 0.0
			if tc.retry != "" {
				wantSeconds = 12
			}
			if got, ok := socket["retry_after"].(float64); !ok || got != wantSeconds {
				t.Errorf("socket retry_after = %v, want %v — its zero is the answer, "+
					"so it is never omitted", socket["retry_after"], wantSeconds)
			}
		})
	}
}

// A FLOOR ON ANOTHER DOMAIN'S LOG IS THE CALLER'S MISTAKE ON BOTH TRANSPORTS,
// AND A NODE ON A REBUILT STREAM IS STILL THIS NODE'S REFUSAL.
//
// `min_position=CREWLET_PAGES_LOG@1:5` on a tracker question is refused
// identically by every node however long anybody waits, so it is `400
// bad_params` over REST and `bad_params` on the socket — the request is what
// has to change. It was answered as the reader's `wrong_stream` refusal: a 503
// with no Retry-After, a frame telling the dashboard to send the person to
// another node or an operator. The control is that refusal's other meaning,
// which is a state of the node: a checkpoint past its log's end, or a stream
// rebuilt under it, stays `unavailable` on both.
func TestAFloorOnAnotherLogIsABadRequestOnBothTransports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a min_position on another domain's log",
			fmt.Errorf("read the board: %w: this read floors at CREWLET_PAGES_LOG@1:5",
				statelog.ErrForeignPosition),
			http.StatusBadRequest, "bad_params"},
		{"a node whose stream was rebuilt under it, the control",
			fmt.Errorf("read the board: %w", &statelog.Refused{
				Code: statelog.RefuseWrongStream, Level: statelog.ReadStale,
				Detail: "the stream was recreated under this node"}),
			http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := seededApp(t, nil)
			a.Queries().Register("floored", iam.GrantStateRead,
				func(context.Context, queries.Params) (any, error) { return nil, tc.err })

			status, answered := overREST(t, a, "floored", nil)
			body, _ := answered.(map[string]any)
			if status != tc.status || body["error"] != tc.code {
				t.Errorf("REST = %d %v, want %d %s", status, body, tc.status, tc.code)
			}
			if socket := overSocket(t, a, "floored", nil); socket["error"] != tc.code {
				t.Errorf("socket answer = %v, want %s", socket, tc.code)
			}
		})
	}
}

func TestAQuestionWithNoSourceIsUnknownRatherThanEmpty(t *testing.T) {
	t.Parallel()
	// A dashboard drawing "no pages" for "this company keeps its knowledge
	// base somewhere else" would report an empty wiki that is not empty.
	a := newApp(t, api.Options{})
	status, body := overREST(t, a, "pages", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 with no native knowledge base wired", status)
	}
	if got := body.(map[string]any)["error"]; got != "unknown_query" {
		t.Errorf("error = %v", got)
	}
}

func TestAnOperatorQuestionIsGuardedOnBothTransports(t *testing.T) {
	t.Parallel()
	// The REST route and the socket make the same decision, so a route
	// that read its own params and forgot the operator check is not a
	// shape this can take.
	a := seededApp(t, nil)
	ran := make(chan struct{}, 1)
	a.Queries().Register("secrets", iam.GrantSecretRead, func(context.Context, queries.Params) (any, error) {
		ran <- struct{}{}
		return map[string]any{"ok": true}, nil
	})

	// WITH NO CREDENTIAL, BOTH TRANSPORTS REFUSE BEFORE THE QUESTION IS
	// REACHED — which is where the refusal moved when the anonymous read
	// posture went. It used to arrive from the query layer as
	// `unauthorized`, because an anonymous caller got as far as asking;
	// now the guard answers `invalid_token` on the REST route and closes
	// the handshake on the socket. What this pins is that they still make
	// the SAME decision, which is the whole point of the case: a route
	// that read its own params and forgot the check is not a shape this
	// can take.
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/query/secrets", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("REST status = %d, want 401", rec.Code)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	if conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream", nil); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("the socket opened with no credential, so the question was reachable")
	}
	select {
	case <-ran:
		t.Error("the operator-only question ran for a caller with no credential")
	default:
	}

	// AND THE REGISTRY'S OWN CHECK STILL REFUSES, asked directly with no
	// operator. It is what the guard's exemption list is checked against:
	// a question registered as operator-only must not answer merely
	// because the route it arrived on was not guarded.
	if _, err := a.Queries().Answer(t.Context(), "secrets", nil); err == nil {
		t.Error("the registry answered an operator-only question to nobody")
	}
}

func TestAnOperatorTokenReachesTheQuestionOverREST(t *testing.T) {
	t.Parallel()
	// The counterfactual: the guard attaches the operator, and the route
	// reads it from the same place every other route does.
	b := closedPosture()
	a := seededApp(t, func(o *api.Options) { o.Bootstrap = &b })

	seen := make(chan string, 1)
	a.Queries().Register("secrets", iam.GrantSecretRead, func(context.Context, queries.Params) (any, error) {
		seen <- "ran"
		return map[string]any{"ok": true}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/query/secrets", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an operator", rec.Code)
	}
	select {
	case <-seen:
	default:
		t.Error("the question never ran")
	}
}

func TestTheQuerySurfaceIsGuardedLikeAnyOtherRead(t *testing.T) {
	t.Parallel()
	// It carries the same LLM transcripts /events does, so a closed read
	// posture has to close it too.
	b := closedPosture()
	a := seededApp(t, func(o *api.Options) { o.Bootstrap = &b })

	if status, _ := overREST(t, a, "events", nil); status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 under a closed posture", status)
	}
}

// withoutClientCount drops the one health field that changes because the
// comparison itself opened a socket.
func withoutClientCount(v any) any {
	body, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(body))
	for k, val := range body {
		if k == "clients" {
			continue
		}
		out[k] = val
	}
	return out
}
