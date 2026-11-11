package queries

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/org"
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

// THE INBOX READS WHAT A HELD CALL SENDS. A parked run's row carries the call
// it holds for approval (`held_call`), which the Inbox, Home and a seat's
// work answer through `POST /mcp-approvals/{id}` rather than the run's own
// answer; the dashboard reads it as `McpHeldCall` (`contract/mcp.ts`). Held
// both ways: every member the dashboard declares is sent, and every key sent
// is declared.
//
// Mutation: drop `digest` from McpHeldCall, or add a key to the row's
// `held_call`, and this is red.
func TestTheInboxReadsWhatAHeldCallSends(t *testing.T) {
	t.Parallel()
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000002", LaunchID: "launch-2",
		Server: "github", Tool: "gh_merge_pull_request", Raw: "merge_pull_request",
		Digest: "sha256:abc", HeldAt: time.Now().UTC()}
	run := sandbox.PendingRun{TurnID: "t-gate", LaunchID: "launch-2", AgentHandle: "backend-dev",
		Status: sandbox.StatusAwaiting, HeldCall: &held}
	row := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{Body: true}, Sources{}.opened(run))
	call, ok := row["held_call"].(map[string]any)
	if !ok {
		t.Fatalf("a parked run holding a call carries held_call %v", row["held_call"])
	}
	members, err := clientsource.Interface(clientsource.Tree(t), "McpHeldCall")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	declared := map[string]bool{}
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if _, sent := call[name]; !sent && declared[name] {
			t.Errorf("McpHeldCall declares %s and a held call's row does not send it", name)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(call)) {
		if _, ok := declared[key]; !ok {
			t.Errorf("a held call's row sends %s and McpHeldCall does not declare it", key)
		}
	}
}

// A HELD OPERATE CALL IS IN ITS APPROVERS' INBOX AND ANSWERED THERE ALONE: the
// decisions read puts it to the approvers the park wrote and to nobody else,
// and its row says it cannot be answered in chat, whatever conversation it
// came from.
//
// Mutation: answer answerable_in_chat by the conversation alone, and a held
// call reads as one a chat reply answers.
func TestAHeldOperateCallIsInItsApproversInbox(t *testing.T) {
	t.Parallel()
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000003", LaunchID: "launch-3",
		Server: "crewlet", Tool: "ssh_operate", Raw: "ssh_operate", Digest: "sha256:abc", Fixed: true,
		Approvers: sandbox.HeldApproversSSH, HeldAt: time.Now().UTC()}
	run := sandbox.PendingRun{TurnID: "t-op", LaunchID: "launch-3", AgentHandle: "sre",
		Status: sandbox.StatusAwaiting, HeldCall: &held, ConversationKey: "chatlet:room-1:thread-9",
		AudienceHandles: []string{"ops-lead", "founder"}}
	for handle, want := range map[string]bool{"ops-lead": true, "founder": true, "pm": false, "": false} {
		if got := putTo(run, handle, Sources{}.delegating); got != want {
			t.Errorf("the decisions read puts the held call to %q: %v, want %v", handle, got, want)
		}
	}
	row := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{Body: true}, Sources{}.opened(run))
	if row["answerable_in_chat"] != false {
		t.Fatalf("a held call reads answerable in chat: %v", row["answerable_in_chat"])
	}
	unheld := run
	unheld.HeldCall = nil
	if row := serialiseRun(unheld, sandbox.BridgeLog{}, livestate.Reach{Body: true}, Sources{}.opened(unheld)); row["answerable_in_chat"] != true {
		t.Fatalf("the premise: a question on a conversation reads answerable in chat, got %v", row["answerable_in_chat"])
	}
}

// A delegated call is put to nobody until escalated (ADR-0125): while it waits
// on its delegates its approvers' decisions do not list it, once escalated
// they do, and turning delegated decisions off lists it on the next read,
// before any sweep has written why.
//
// Mutation: drop the delegating check from putTo, or read the row alone
// without the live rules, and the owner's Inbox fills with routine commands,
// or a switched-off rule hides them until the sweep.
func TestADelegatedCallIsPutToNobodyUntilEscalated(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nroles:\n  - {name: Ops Lead, handle: ops-lead, kind: human}\n" +
		"  - {name: Security, handle: security-a}\n" +
		"delegated_decisions:\n  enabled: true\n  rules:\n" +
		"    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n"
	company, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	s := Sources{Company: func() (*config.Company, *org.Organization) { return company, nil }}
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000f1", LaunchID: "launch-5",
		Server: "crewlet", Tool: "ssh_operate", Raw: "ssh_operate", Fixed: true,
		Approvers: sandbox.HeldApproversSSH, Act: string(delegated.KindSSHOperate), HeldAt: time.Now().UTC(),
		Delegation: &sandbox.HeldDelegation{Rule: 1, Delegates: []string{"security-a"}}}
	run := sandbox.PendingRun{TurnID: "t-del", LaunchID: "launch-5", AgentHandle: "sre",
		Status: sandbox.StatusAwaiting, HeldCall: &held, AudienceHandles: []string{"ops-lead"}}
	if putTo(run, "ops-lead", s.delegating) {
		t.Fatal("a call waiting on its delegates is put to ops-lead")
	}
	escalated := held
	delegation := *held.Delegation
	delegation.Escalated = &sandbox.HeldEscalation{Overruled: []string{delegated.OverruledDeadline},
		At: time.Now().UTC()}
	escalated.Delegation = &delegation
	run.HeldCall = &escalated
	if !putTo(run, "ops-lead", s.delegating) {
		t.Fatal("an escalated call is not put to ops-lead")
	}
	run.HeldCall = &held
	company.DelegatedDecisions.Enabled = false
	if !putTo(run, "ops-lead", s.delegating) {
		t.Fatal("a call whose delegation was turned off is not put to ops-lead")
	}
}
