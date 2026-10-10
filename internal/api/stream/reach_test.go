package stream_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
)

// A SOCKET IS PUSHED WHAT ITS REACH IS SERVED (ADR-0031). These cases hold the
// delivery table total, the fan-out to one build per reach, and the member
// variants to what a member may read.

// EVERY FRAME KIND HAS A DELIVERY, AND EVERY PUSH A REACH.
//
// The constants are read from this package's source (engineKinds), so a kind
// declared tomorrow fails here until somebody decides who is pushed it — a
// broadcast to everybody is not a default. The four kinds that are not pushes
// (the handshake, the two answers and the pong) are named, so moving one onto
// the broadcast path is a decision in a reviewed diff too.
//
// Mutation: delete `KindSchedules` from the table, and the first half fails.
func TestEveryPushKindHasAReach(t *testing.T) {
	t.Parallel()
	notPushed := []stream.Kind{stream.KindSnapshot, stream.KindResult, stream.KindError, stream.KindPong}
	for _, name := range engineKinds(t) {
		kind := stream.Kind(name)
		if !stream.Routed(kind) {
			t.Errorf("stream.Kind %q has no delivery: decide who is pushed it in the "+
				"kinds table, beside the reason", kind)
			continue
		}
		reach, pushed := stream.KindReach(kind)
		switch {
		case slices.Contains(notPushed, kind) && pushed:
			t.Errorf("%q is pushed at %q; it is sent to the one client it is for", kind, reach)
		case !slices.Contains(notPushed, kind) && !pushed:
			t.Errorf("%q is not pushed; a kind the engine broadcasts needs a reach", kind)
		case pushed && !reach.Valid():
			t.Errorf("%q is pushed at %q, which is not a reach", kind, reach)
		}
	}
	if _, pushed := stream.KindReach("not_a_kind"); pushed {
		t.Error("a kind this build does not know has a reach")
	}
}

// reaches is one connected client per reach a socket can hold, plus one whose
// reach is not a reach at all.
func reachClients(h *stream.Hub) map[auth.Reach][]*stream.Client {
	out := map[auth.Reach][]*stream.Client{}
	for _, reach := range append(slices.Clone(auth.Reaches), "") {
		for range 3 {
			c := stream.NewClient(reach)
			h.Register(c)
			out[reach] = append(out[reach], c)
		}
	}
	return out
}

// A PUSH IS BUILT ONCE PER REACH, NEVER PER CLIENT, AND EACH CLIENT IS SENT ITS
// OWN REACH'S VARIANT.
//
// Three clients of each reach, for every push kind: the variant builder is
// called exactly once for each reach that covers the kind's, never for one
// that does not, and every client covered receives the variant built for its
// own reach and nothing else. A client whose reach is not a reach is pushed
// nothing.
//
// Mutation: build per client, and the call count fails; send every client the
// first variant built, and the variant check fails.
func TestAPushIsBuiltOncePerReachAndSentAsTheClientsOwn(t *testing.T) {
	t.Parallel()
	for _, name := range engineKinds(t) {
		kind := stream.Kind(name)
		need, pushed := stream.KindReach(kind)
		if !pushed {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := stream.NewHub()
			clients := reachClients(h)
			var mu sync.Mutex
			built := map[auth.Reach]int{}
			h.Broadcast(kind, func(reach auth.Reach) (any, bool) {
				mu.Lock()
				defer mu.Unlock()
				built[reach]++
				return "built for " + string(reach), true
			}, clock)

			for reach, cs := range clients {
				covered := reach.Covers(need)
				if n := built[reach]; covered && n != 1 || !covered && n != 0 {
					t.Errorf("the %q variant was built %d times; want once if %q covers %q, else never",
						reach, n, reach, need)
				}
				for _, c := range cs {
					got := drain(c)
					switch {
					case !covered && len(got) != 0:
						t.Errorf("a %q client was pushed %q, which needs %q", reach, kind, need)
					case covered && (len(got) != 1 || got[0].Data != "built for "+string(reach)):
						t.Errorf("a %q client received %+v, want its own reach's variant", reach, got)
					}
				}
			}
		})
	}
}

// A VARIANT A REACH IS NOT SENT IS SKIPPED FOR THAT REACH ALONE.
func TestAVariantThatDeclinesSendsThatReachNothing(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	member, admin := stream.NewClient(auth.ReachMember), stream.NewClient(auth.ReachAdmin)
	h.Register(member)
	h.Register(admin)
	h.Broadcast(stream.KindAgents, func(reach auth.Reach) (any, bool) {
		return "rows", reach == auth.ReachAdmin
	}, clock)
	if got := drain(member); len(got) != 0 {
		t.Errorf("the member was sent a variant that declined: %+v", got)
	}
	if got := drain(admin); len(got) != 1 {
		t.Errorf("the admin received %+v, want its variant", got)
	}
}

// A FRAME FOR ONE CLIENT IS NEVER FANNED OUT: an answer broadcast would hand
// every socket another socket's question's answer.
//
// Mutation: drop the route check in Broadcast, and the answer reaches all.
func TestAFrameForOneClientIsNeverBroadcast(t *testing.T) {
	t.Parallel()
	for _, kind := range []stream.Kind{
		stream.KindSnapshot, stream.KindResult, stream.KindError, stream.KindPong, "not_a_kind",
	} {
		h := stream.NewHub()
		clients := reachClients(h)
		called := false
		h.Broadcast(kind, func(auth.Reach) (any, bool) { called = true; return "x", true }, clock)
		if called {
			t.Errorf("broadcasting %q built a variant", kind)
		}
		for reach, cs := range clients {
			for _, c := range cs {
				if got := drain(c); len(got) != 0 {
					t.Errorf("broadcasting %q reached a %q client: %+v", kind, reach, got)
				}
			}
		}
	}
}

// memberAndAdmin is a service with one member client and one admin client.
func memberAndAdmin(t *testing.T, opts stream.Options) (*stream.Service, *stream.Client, *stream.Client) {
	t.Helper()
	s := buildService(t, opts)
	member, admin := stream.NewClient(auth.ReachMember), stream.NewClient(auth.ReachAdmin)
	s.Hub().Register(member)
	s.Hub().Register(admin)
	return s, member, admin
}

// machineKeys is every wire key a member socket must never receive, anywhere in
// any frame: what the machine processed to reach a seat's state.
var machineKeys = []string{
	"live_call", "prompt", "prompt_messages", "response", "round_narration",
	"tool_executions", "rounds", "versions", "node", "model", "used", "limit",
	"total_tokens", "payload",
}

// A MEMBER'S SOCKET IS PUSHED THE SEAT'S STATE AND NEVER THE CALL.
//
// One seat runs a call with a distinctive prompt, response, tool call and
// model, spends, and is metered. The admin socket is pushed the event, the
// overlay with its live call, the budget's figures; the member socket is pushed
// the seat's member status and the budget's state — and not one machine key,
// nor one of those strings, in any frame it receives.
//
// Mutation: hand the member the admin variant of `agents`, and every half of
// this fails.
func TestAMemberSocketIsPushedTheStateAndNeverTheCall(t *testing.T) {
	t.Parallel()
	s, member, admin := memberAndAdmin(t, stream.Options{})
	s.Ingest(envelope("agent_phase_started", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "phase": "execute",
	}))
	s.Ingest(livestate.Envelope{
		ID: "p1", Type: "agent_turn_progress", Timestamp: "2026-06-14T12:00:01Z",
		Payload: map[string]any{
			"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "phase": "execute",
			"round_num": 0, "prompt": "prompt-secret-7", "response": "response-secret-7",
			"model":           "model-secret-7",
			"tool_executions": []any{map[string]any{"name": "tool-secret-7"}},
		},
	})
	s.Ingest(livestate.Envelope{
		ID: "b1", Type: "budget_meters", Timestamp: "2026-06-14T12:00:02Z",
		Payload: map[string]any{
			"meter_id": "m-1", "seq": 1, "timezone": "UTC",
			"org": map[string]any{"windows": []any{map[string]any{
				"period": "day", "window": "2026-06-14", "resets_at": "2099-01-01T00:00:00Z",
				"used": 7770, "limit": 7770, "state": "refusing", "refused_at": "2026-06-14T12:00:00Z",
			}}},
			"seats": []any{},
		},
	})

	adminKinds := map[stream.Kind]bool{}
	for _, env := range drain(admin) {
		adminKinds[env.Kind] = true
	}
	for _, kind := range []stream.Kind{stream.KindEvent, stream.KindAgents, stream.KindBudget} {
		if !adminKinds[kind] {
			t.Errorf("the admin socket was not pushed %q: %v", kind, adminKinds)
		}
	}

	frames := drain(member)
	var kinds []stream.Kind
	for _, env := range frames {
		kinds = append(kinds, env.Kind)
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-7", "7770"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("the member socket was pushed %q in a %s frame: %s", secret, env.Kind, raw)
			}
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		for key := range everyKey(decoded) {
			if slices.Contains(machineKeys, key) {
				t.Errorf("the member socket was pushed the key %q in a %s frame: %s", key, env.Kind, raw)
			}
		}
	}
	slices.Sort(kinds)
	kinds = slices.Compact(kinds)
	if !slices.Equal(kinds, []stream.Kind{stream.KindAgents, stream.KindBudget}) {
		t.Errorf("the member socket was pushed %v, want the seat's state and the budget's", kinds)
	}
	var last any
	for _, env := range frames {
		if env.Kind == stream.KindAgents {
			last = env.Data
		}
	}
	if statuses, ok := last.([]livestate.AgentStatus); !ok || len(statuses) != 1 ||
		statuses[0].Activity != livestate.ActivityWorking {
		t.Errorf("the member's last agents push = %#v, want the seat working", last)
	}
}

// A CONFIG APPLY RE-SENDS EVERY CONFIGURED SURFACE IN EACH REACH'S VARIANT: the
// roster as each reach reads it, the chart as each reach is served it, and the
// catalogue to the admin alone. The schedules go to both keyed reaches, and an
// anonymous socket is sent the chart and nothing else.
func TestAnApplyResendsEachReachItsOwnSurfaces(t *testing.T) {
	t.Parallel()
	s, member, admin := memberAndAdmin(t, stream.Options{
		Roster: func() []map[string]any {
			return []map[string]any{{"id": "lead", "role": "Lead", "handle": "lead", "agent_id": "a-1"}}
		},
		Org: func(reach auth.Reach) any { return "chart for " + string(reach) },
	})
	anonymous := stream.NewClient(auth.ReachPublic)
	s.Hub().Register(anonymous)

	s.ResendConfigured()

	byKind := func(c *stream.Client) map[stream.Kind]any {
		out := map[stream.Kind]any{}
		for _, env := range drain(c) {
			out[env.Kind] = env.Data
		}
		return out
	}
	got := map[auth.Reach]map[stream.Kind]any{
		auth.ReachPublic: byKind(anonymous), auth.ReachMember: byKind(member), auth.ReachAdmin: byKind(admin),
	}
	for reach, want := range map[auth.Reach][]stream.Kind{
		auth.ReachPublic: {stream.KindOrg},
		auth.ReachMember: {stream.KindOrg, stream.KindSchedules, stream.KindSeats},
		auth.ReachAdmin:  {stream.KindOrg, stream.KindSchedules, stream.KindSeats, stream.KindTools},
	} {
		var kinds []stream.Kind
		for kind := range got[reach] {
			kinds = append(kinds, kind)
		}
		slices.Sort(kinds)
		if !slices.Equal(kinds, want) {
			t.Errorf("a %s socket was re-sent %v, want %v", reach, kinds, want)
		}
		if chart := got[reach][stream.KindOrg]; chart != "chart for "+string(reach) {
			t.Errorf("a %s socket was re-sent the chart %v, want its own reach's", reach, chart)
		}
	}
	if _, ok := got[auth.ReachMember][stream.KindSeats].([]livestate.SeatStatus); !ok {
		t.Errorf("the member's roster is %T, want the member rows", got[auth.ReachMember][stream.KindSeats])
	}
	if _, ok := got[auth.ReachAdmin][stream.KindSeats].([]map[string]any); !ok {
		t.Errorf("the admin's roster is %T, want the overlay rows", got[auth.ReachAdmin][stream.KindSeats])
	}
}

// everyKey yields every object key anywhere in a decoded JSON value.
func everyKey(v any) func(func(string) bool) {
	return func(yield func(string) bool) {
		var walk func(any) bool
		walk = func(v any) bool {
			switch v := v.(type) {
			case map[string]any:
				for key, child := range v {
					if !yield(key) || !walk(child) {
						return false
					}
				}
			case []any:
				for _, child := range v {
					if !walk(child) {
						return false
					}
				}
			}
			return true
		}
		walk(v)
	}
}
