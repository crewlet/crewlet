package queries

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A HELD CALL'S ARGUMENTS ARE THE SEAT'S TRAIL on the executors board
// (ADR-0077): the call it holds (tool and server) is read with the question by
// whom it is put to, and what the call would send only by a reader of the
// seat's trail, its lead or an administrator, who are who may approve it. Both
// keys are absent on a run that holds no call.
//
// Mutation: classify held_arguments as the question, and the audience reads
// what the call would send.
func TestAHeldCallsArgumentsAreReadOnlyByWhoMayApproveIt(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	src := Sources{HeldCalls: sandbox.NewCallSeal(cipher)}
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: "launch-1",
		Server: "github", Tool: "gh_merge_pull_request", Raw: "merge_pull_request", HeldAt: time.Now().UTC()}
	if held.Arguments, err = src.HeldCalls.SealArguments("t-held", held, map[string]any{"pull": float64(7)}); err != nil {
		t.Fatal(err)
	}
	run := sandbox.PendingRun{TurnID: "t-held", LaunchID: "launch-1", AgentHandle: "backend-dev",
		Status: sandbox.StatusAwaiting, AudienceHandles: []string{"cto"}, HeldCall: &held}

	whole := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{Body: true}, src.opened(run))
	if call, ok := whole["held_call"].(map[string]any); !ok || call["tool"] != "gh_merge_pull_request" {
		t.Fatalf("a reader of the trail reads held_call %v", whole["held_call"])
	}
	if args, ok := whole["held_arguments"].(map[string]any); !ok || args["pull"] != float64(7) {
		t.Fatalf("a reader of the trail reads held_arguments %v, want them opened", whole["held_arguments"])
	}
	asked := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{Record: "cto"}, src.opened(run))
	if _, ok := asked["held_call"]; !ok {
		t.Error("the person the question is put to does not read which call it is about")
	}
	if _, ok := asked["held_arguments"]; ok {
		t.Error("the question's audience reads what the call would send without reading the seat's trail")
	}
	plain := sandbox.PendingRun{TurnID: "t-plain", Status: sandbox.StatusRunning}
	bare := serialiseRun(plain, sandbox.BridgeLog{}, livestate.Reach{Body: true}, src.opened(plain))
	if slices.ContainsFunc([]string{"held_call", "held_arguments"}, func(k string) bool { _, ok := bare[k]; return ok }) {
		t.Errorf("a run holding no call carries %v", bare)
	}
}
