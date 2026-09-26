package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/statelog"
)

// healthFrames dials the socket anonymously and returns the snapshot's health
// and the first health tick after it, both as the tab decodes them.
func healthFrames(t *testing.T, a *api.App) (snapshot, tick map[string]any) {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/stream", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	for range 50 {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		_, raw, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		switch env.Kind {
		case "snapshot":
			snapshot, _ = env.Data["health"].(map[string]any)
		case "health":
			if snapshot != nil {
				return snapshot, env.Data
			}
		}
	}
	t.Fatal("no health tick followed the snapshot")
	return nil, nil
}

// withoutClients drops the one field that legitimately differs between two
// reads a moment apart: opening the socket to read the push is itself a client.
func withoutClients(body map[string]any) map[string]any {
	out := maps.Clone(body)
	delete(out, "clients")
	return out
}

// THE HEALTH PUSH IS THE WHOLE ENVELOPE — the snapshot's and every tick's.
//
// It was three fields (`status`, `in_flight`, `shutting_down`) while a `stream`
// query answered the rest, and five screens polled that query at 5 s and 15 s
// of their own: the rail could say a revision had applied while the panel in
// front of it said it had not. So the push, the snapshot and GET /health are
// held to ONE body here, field for field, including every field this change
// added — the fleet's size, the alarm count and the seed's coverage.
//
// Mutation: narrow streamHealth back to a subset of the body, and this fails.
func TestTheHealthPushIsTheWholeEnvelope(t *testing.T) {
	t.Parallel()
	nodes := 3
	state := livestate.New()
	state.Seed(livestate.History{Coverage: eventfan.Coverage{
		Nodes: []eventfan.NodeCoverage{
			{ID: "node-a", Answered: true},
			{ID: "node-b", Answered: false, Error: "no answer inside 2s"},
		},
	}})
	a := newApp(t, api.Options{
		State:          state,
		Sources:        queries.Sources{Company: active()},
		HealthInterval: 20 * time.Millisecond,
		Runtime: &fakeRuntime{state: api.RuntimeState{
			InFlight: 2, Posture: "serve", AppliedEpoch: 41,
			StartedAt: "2026-06-14T11:00:00Z", Seats: []string{"ceo"},
			StallLag: 7 * time.Second,
		}, fleet: api.FleetState{
			LiveNodes: &nodes,
			Alarms:    []string{string(statelog.KindWALLarge), string(statelog.KindApplyLag)},
		}},
	})
	a.Start(t.Context())

	_, body := get(t, a, "/health")
	snapshot, tick := healthFrames(t, a)

	for field, want := range map[string]any{
		"applied_epoch": float64(41), "posture": "serve", "nodes": float64(3),
		"stall_lag_seconds": float64(7), "in_flight": float64(2),
	} {
		if body[field] != want {
			t.Errorf("GET /health %s = %v, want %v", field, body[field], want)
		}
	}
	if _, seeded := body["seeded_from"].(map[string]any); !seeded {
		t.Errorf("seeded_from = %v, want the seed's coverage", body["seeded_from"])
	}
	for name, frame := range map[string]map[string]any{"snapshot": snapshot, "tick": tick} {
		if !reflect.DeepEqual(withoutClients(frame), withoutClients(body)) {
			t.Errorf("the %s's health is not the envelope GET /health answers:\n"+
				"  push    %v\n  /health %v", name, frame, body)
		}
	}
}

// COUNTS, NEVER ROWS — the envelope is public.
//
// GET /health is a probe and unguarded, and the push reaches an anonymous
// tab, so what it says about the fleet and the alarm table is a number and
// one name. Which nodes hold what, and what each alarm measured, are the
// operator-only `fleet` and `work_retention` answers.
//
// Mutation: carry the alarms' details or the node rows on the envelope, and
// this fails.
func TestAnAnonymousHealthReadCarriesCountsOnly(t *testing.T) {
	t.Parallel()
	nodes := 2
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Runtime: &fakeRuntime{state: api.RuntimeState{Posture: "serve"}, fleet: api.FleetState{
			LiveNodes: &nodes,
			Alarms:    []string{string(statelog.KindBackupAge)},
		}},
	})
	_, body := get(t, a, "/health")
	if _, isCount := body["nodes"].(float64); !isCount {
		t.Errorf("nodes = %#v, want a count", body["nodes"])
	}
	alarms, _ := body["alarms"].(map[string]any)
	if got := slices.Sorted(maps.Keys(alarms)); !slices.Equal(got, []string{"count", "worst"}) {
		t.Errorf("an anonymous read's alarms carry %v, want only the count and the "+
			"worst's name", got)
	}
}

// THE NODE COUNT IS ABSENT WHEN PRESENCE CANNOT BE READ — never 0.
//
// The node answering is itself a node, so a zero could only ever be a failed
// read wearing a number, and a health card told "0 nodes" by the node serving
// it has been told something false. Absent, a screen says "node count
// unavailable".
//
// Mutation: default LiveNodes to zero on a failed read, and this fails.
func TestTheNodeCountIsAbsentWhenPresenceCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what  string
		nodes *int
		want  any
	}{
		{"a failed presence read", nil, nil},
		{"one node", new(1), float64(1)},
		{"a three-node fleet", new(3), float64(3)},
	} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: "serve"},
			fleet: api.FleetState{LiveNodes: tc.nodes},
		}})
		_, body := get(t, a, "/health")
		got, present := body["nodes"]
		if tc.want == nil && present {
			t.Errorf("%s: nodes = %v, want the field absent", tc.what, got)
		}
		if tc.want != nil && got != tc.want {
			t.Errorf("%s: nodes = %v, want %v", tc.what, got, tc.want)
		}
	}
}

// THE ALARM COUNT IS THE STANDING ALARMS', and "not evaluated" is not zero.
//
// The runtime hands the envelope the evaluation the gauge and the log were
// fed, longest-standing first (the engine half of this is
// `engine.TestTheAlarmCountEqualsTheRetentionAlarms`). Here: the count is its
// length, the worst is its head, and a node that has not evaluated its table
// says nothing rather than `{count: 0}`, which a health card reads as healthy.
//
// Mutation: report the last kind as the worst, or send `{count: 0}` for a nil
// reading, and this fails.
func TestTheAlarmCountIsTheStandingAlarms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what   string
		alarms []string
		want   map[string]any
	}{
		{"no evaluation yet", nil, nil},
		{"evaluated, nothing firing", []string{}, map[string]any{"count": float64(0)}},
		{"two firing", []string{"wal_large", "apply_lag"},
			map[string]any{"count": float64(2), "worst": "wal_large"}},
	} {
		a := newApp(t, api.Options{Runtime: &fakeRuntime{
			state: api.RuntimeState{Posture: "serve"},
			fleet: api.FleetState{Alarms: tc.alarms},
		}})
		_, body := get(t, a, "/health")
		got, present := body["alarms"]
		switch {
		case tc.want == nil && present:
			t.Errorf("%s: alarms = %v, want the field absent", tc.what, got)
		case tc.want != nil && !reflect.DeepEqual(got, tc.want):
			t.Errorf("%s: alarms = %v, want %v", tc.what, got, tc.want)
		}
	}
}

// A WEDGED PRESENCE READ COSTS THE LIVENESS PROBE ONE BUDGET, NOT A HANG.
//
// The presence count is a key-iterating coordination scan the NATS client's
// per-API timeout does not cover, and GET /health is the liveness probe: a
// probe that waits on it for as long as the broker is wedged is one an
// orchestrator times out and answers by killing a healthy node. The envelope
// bounds the read to engine.ProbeReadBudget and answers without the count —
// absent, which is what "cannot say" means — keeping the alarms, which are in
// memory. /ready decides nothing on either and never asks.
//
// Mutation: drop the budget in App.health (hand Fleet the request context),
// and the /health half hangs until the test's own deadline; have readiness
// build the envelope, and the /ready half counts a fleet read.
func TestAWedgedPresenceReadDoesNotHangTheProbes(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{
		state:         api.RuntimeState{Posture: "serve"},
		fleet:         api.FleetState{Alarms: []string{string(statelog.KindBackupAge)}},
		presenceHangs: true,
	}
	a := newApp(t, api.Options{Sources: queries.Sources{Company: active()}, Runtime: runtime})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)

	// The request's own deadline is well past the budget, so only the
	// envelope's bound can make this answer in time.
	ctx, cancel := context.WithTimeout(t.Context(), engine.ProbeReadBudget+10*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/health", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /health with a wedged presence read: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	took := time.Since(start)
	if limit := engine.ProbeReadBudget + time.Second; took > limit {
		t.Errorf("GET /health took %v with a wedged presence read, want inside %v", took, limit)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200: liveness does not depend on the fleet", resp.StatusCode)
	}
	if got, present := body["nodes"]; present {
		t.Errorf("nodes = %v after a presence read that never finished, want the field absent", got)
	}
	if alarms, _ := body["alarms"].(map[string]any); alarms["count"] != float64(1) {
		t.Errorf("alarms = %v, want the in-memory count kept when presence ran out", body["alarms"])
	}

	before := runtime.fleetReads.Load()
	get(t, a, "/ready")
	if after := runtime.fleetReads.Load(); after != before {
		t.Errorf("GET /ready made %d fleet reads, want none: readiness decides nothing on them",
			after-before)
	}
}

// THE SOCKET'S `unavailable` FRAME CARRIES THE SAME RETRY HINT AS THE REST 503.
//
// Both come from [stream.RetryAfterSeconds]: the refusal's own derived hint
// where it has one — how far behind this node is over how fast it drains —
// and the shared tick's cadence otherwise. The socket used to send the bare
// code, so a screen waited whatever it had hard-coded while the same question
// over REST was told what the node actually needed.
//
// Mutation: drop the frame's field, or compute it separately from the header,
// and this fails.
func TestTheSocketUnavailableFrameCarriesTheSameRetryHintAsREST(t *testing.T) {
	t.Parallel()
	a := seededApp(t, nil)
	a.Queries().Register("behind", func(context.Context, queries.Params) (any, error) {
		return nil, fmt.Errorf("%w: %w", queries.ErrUnavailable,
			&statelog.Refused{Code: statelog.RefuseBehind, RetryAfter: 12400 * time.Millisecond})
	})
	a.Queries().Register("blip", func(context.Context, queries.Params) (any, error) {
		return nil, fmt.Errorf("%w: store unreachable", queries.ErrUnavailable)
	})

	for what, want := range map[string]int{
		"behind": 12, // the refusal's own hint, rounded
		"blip":   int(stream.HealthInterval / time.Second),
	} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/query/"+what, nil))
		header, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if rec.Code != http.StatusServiceUnavailable || err != nil {
			t.Fatalf("%s over REST: %d, Retry-After %q", what, rec.Code,
				rec.Header().Get("Retry-After"))
		}
		frame := overSocket(t, a, what, nil)
		if frame["error"] != stream.CodeUnavailable {
			t.Fatalf("%s over the socket: %v", what, frame)
		}
		got, _ := frame["retry_after_seconds"].(float64)
		if int(got) != header || header != want {
			t.Errorf("%s: the socket frame says wait %v s and REST says %d s, want "+
				"both %d", what, frame["retry_after_seconds"], header, want)
		}
	}
}

// The typed refusal is what the socket reads its hint from, so it must still
// be an ErrUnavailable to every other reader.
func TestAnUnavailableErrorIsStillTheSentinel(t *testing.T) {
	t.Parallel()
	err := error(&stream.UnavailableError{What: "turns", Hint: time.Second})
	if !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("%v is not stream.ErrUnavailable to errors.Is", err)
	}
}
