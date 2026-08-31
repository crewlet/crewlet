package api_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
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
			ID: "ev" + string(rune('a'+i)), Type: "task_started", Source: "engine",
			Time: base.Add(time.Duration(i) * time.Second), Category: "task",
			Actor: "Lead", Summary: "did a thing", TraceID: "tr-1",
			Payload: json.RawMessage(`{"role":"Lead"}`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	state := livestate.New()
	state.Apply(&livestate.Envelope{
		ID: "e1", Type: "task_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Category: "task", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})

	opts := api.Options{
		State:   state,
		Sources: queries.Sources{State: state, Events: db.Events()},
		Now:     func() time.Time { return clock },
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
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
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
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream", nil)
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
		{"agent", url.Values{"role": {"Lead"}}, map[string]any{"role": "Lead"}},
		{"events", url.Values{"limit": {"3"}}, map[string]any{"limit": float64(3)}},
		{"events", url.Values{"actor": {"Lead"}}, map[string]any{"actor": "Lead"}},
		{"trace", url.Values{"trace_id": {"tr-1"}}, map[string]any{"trace_id": "tr-1"}},
		{"tokens", nil, nil},
		{"stream", nil, nil},
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
	status, _ := overREST(t, a, "events", url.Values{"before_id": {"ev1"}})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestAFailingQuestionReportsACodeAndNothingElse(t *testing.T) {
	t.Parallel()
	// The reason reaches the LOG, not the caller: it can carry a database
	// path or a driver's own message, and this route is reachable under
	// the anonymous read posture.
	a := seededApp(t, nil)
	a.Queries().Register("boom", func(context.Context, queries.Params) (any, error) {
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

func TestAQuestionWithNoSourceIsUnknownRatherThanEmpty(t *testing.T) {
	t.Parallel()
	// A dashboard drawing "no events" for "this node has no event log"
	// would report a quiet company during a misconfiguration.
	a := newApp(t, api.Options{})
	status, body := overREST(t, a, "events", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 with no event log wired", status)
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
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	a := seededApp(t, func(o *api.Options) { o.Bootstrap = &b })
	a.Queries().RegisterOperator("secrets", func(context.Context, queries.Params) (any, error) {
		return map[string]any{"ok": true}, nil
	})

	status, body := overREST(t, a, "secrets", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("REST status = %d, want 401", status)
	}
	if got := body.(map[string]any)["error"]; got != "unauthorized" {
		t.Errorf("REST error = %v", got)
	}
	if got := overSocket(t, a, "secrets", nil); got["error"] != "unauthorized" {
		t.Errorf("socket answer = %v", got)
	}
}

func TestAnOperatorTokenReachesTheQuestionOverREST(t *testing.T) {
	t.Parallel()
	// The counterfactual: the guard attaches the operator, and the route
	// reads it from the same place every other route does.
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	a := seededApp(t, func(o *api.Options) { o.Bootstrap = &b })

	seen := make(chan string, 1)
	a.Queries().RegisterOperator("secrets", func(context.Context, queries.Params) (any, error) {
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
