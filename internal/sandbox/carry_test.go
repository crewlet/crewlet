package sandbox_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// wireGolden is the bytes each row object encodes to, filled so that every
// member it has is on the wire.
var wireGolden = map[string]string{
	"bridge call": `{"seq":7,"name":"Name","args":"Args","output":"Output","failed":true,"at":"2026-01-02T03:04:05Z","whole_bytes":7,"whole_parts":7}`,
	"run":         `{"turn_id":"TurnID","work_key":"WorkKey","agent_handle":"AgentHandle","agent_id":"AgentID","role":"Role","sandbox_id":"SandboxID","coding_agent":"CodingAgent","placement":"Placement","command_id":"CommandID","status":"Status","launch_id":"LaunchID","launches":7,"owner":"Owner","owner_epoch":7,"task_description":"TaskDescription","reply":"Reply","conversation_key":"ConversationKey","branch":"Branch","session_id":"SessionID","question":"Question","audience":"Audience","asked_spend":{"input_tokens":7,"output_tokens":7,"cost_usd":1.5,"models":[{"model":"Model","input_tokens":7,"output_tokens":7,"cost_usd":1.5}],"whole":true},"asked_launch":"AskedLaunch","trace_id":"TraceID","span_id":"SpanID","delegation_depth":7,"delegation_chain":["DelegationChain"],"execute_state":{"key":null},"bridge_calls":[{"seq":7,"name":"Name","args":"Args","output":"Output","failed":true,"at":"2026-01-02T03:04:05Z","whole_bytes":7,"whole_parts":7}],"bridge_calls_elided":7,"charged":true,"carried_counted":true,"held_answer":{"launch":"Launch","text":"Text","in_box":true,"success":true,"cost_usd":1.5,"delivered_refs":["DeliveredRefs"],"spend":{"input_tokens":7,"output_tokens":7,"cost_usd":1.5,"models":[{"model":"Model","input_tokens":7,"output_tokens":7,"cost_usd":1.5}],"whole":true},"trigger":{"delegation_chain":["DelegationChain"],"delegation_depth":7,"id":"07070707-0707-0707-0707-070707070707","parent_span_id":"ParentSpanID","parent_turn_id":"ParentTurnID","payload":{"key":null},"source":"Source","span_id":"SpanID","timestamp":"2026-01-02T03:04:05Z","trace_id":"TraceID","type":"Type"},"parts":{"id":"ID","parts":7,"bytes":7},"at":"2026-01-02T03:04:05Z"},"triggered_at":"2026-01-02T03:04:05Z","pause_ttl_seconds":1.5,"paused_at":"2026-01-02T03:04:05Z","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
}

// A RUN'S ROW THIS BUILD WRITES IS THE BYTES IT ALWAYS WAS.
//
// The row is one value in the fleet's coordination store, read and written
// back by every build in the fleet, so its bytes are a contract between peers
// rather than a detail of this build. An object carrying nothing this build
// does not know encodes exactly as its struct does, and decoding those bytes
// and encoding them again changes nothing.
func TestEveryRowObjectEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	for name, roundTrip := range map[string]func() ([]byte, []byte, error){
		"run":         pinned[sandbox.PendingRun],
		"bridge call": pinned[sandbox.BridgeCall],
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, ok := wireGolden[name]
			if !ok {
				t.Fatalf("%s has no pinned encoding", name)
			}
			got, again, err := roundTrip()
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if string(got) != want {
				t.Fatalf("%s encodes as\n  %s\nand every node holds\n  %s", name, got, want)
			}
			if string(again) != want {
				t.Fatalf("%s decoded and encoded again is\n  %s\nnot\n  %s", name, again, want)
			}
		})
	}
}

// EVERY OBJECT ON A RUN'S ROW CARRIES WHAT IT DOES NOT KNOW.
//
// The carry is per object, so an object added to the row without it is one a
// newer build's member inside is erased from by this build's first write — and
// nothing else would say so. Every object reachable from the row's type is
// held to it, another package's among them where the row holds one.
func TestEveryObjectOnARunsRowCarriesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	for _, missing := range jsoncarrytest.Uncarried(nil,
		reflect.TypeFor[sandbox.PendingRun](), reflect.TypeFor[sandbox.BridgeCall]()) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE ANY OBJECT ON THE ROW SURVIVES THIS
// BUILD'S DECODE AND ENCODE of it, byte for byte — the held answer, its spend
// and each model's part of it, the reference to its parts, and every bridged
// call. The writes the store makes are walked one by one in
// TestAMemberANewerBuildAddedInsideAnObjectSurvivesEveryWrite.
func TestAMemberANewerBuildAddedSurvivesInsideEveryRowObject(t *testing.T) {
	t.Parallel()
	for name, typ := range map[string]reflect.Type{
		"run":         reflect.TypeFor[sandbox.PendingRun](),
		"bridge call": reflect.TypeFor[sandbox.BridgeCall](),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jsoncarrytest.Survives(t, typ, []byte(wireGolden[name]), func(raw []byte) ([]byte, error) {
				if typ == reflect.TypeFor[sandbox.BridgeCall]() {
					return roundTripAs[sandbox.BridgeCall](raw)
				}
				return roundTripAs[sandbox.PendingRun](raw)
			})
		})
	}
}

func roundTripAs[T any](raw []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// pinned is a filled T's encoding, and that encoding decoded and encoded
// again.
func pinned[T any]() ([]byte, []byte, error) {
	got, err := json.Marshal(jsoncarrytest.Filled[T]())
	if err != nil {
		return nil, nil, err
	}
	var back T
	if err := json.Unmarshal(got, &back); err != nil {
		return nil, nil, err
	}
	again, err := json.Marshal(back)
	return got, again, err
}
