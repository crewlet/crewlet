package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WAKE IS ABOUT WHAT A RUN WROTE BY EXACT IDENTITY: its work item by ref, a
// key or project apart, or its conversation as the record was written; never
// another item or thread, a per-event fallback, an empty set or an item with
// no id.
//
// Mutation: compare an item by key, or match a conversation by prefix, and a
// case goes red.
func TestAWakeIsAboutWhatARunWroteByExactIdentity(t *testing.T) {
	t.Parallel()
	task := types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1", Project: "ENG"}
	change := gitlet.ChangeConversation("c-1")
	wrote := []types.WrittenRecord{types.ItemWritten(task), types.ConversationWritten(change)}
	for _, tc := range []struct {
		name  string
		wrote []types.WrittenRecord
		o     wakeOrigin
		want  bool
	}{
		{"its item, moved since", wrote, wakeOrigin{item: &types.WorkItem{Backend: types.WorkWorklet,
			ID: "task-1", Key: "OPS-9", Project: "OPS"}}, true},
		{"its change", wrote, wakeOrigin{conversation: change}, true},
		{"another item", wrote, wakeOrigin{item: &types.WorkItem{Backend: types.WorkWorklet, ID: "task-2"}}, false},
		{"the same id in another tracker", wrote, wakeOrigin{item: &types.WorkItem{Backend: "jira",
			ID: "task-1"}}, false},
		{"another change", wrote, wakeOrigin{conversation: gitlet.ChangeConversation("c-10")}, false},
		{"a per-event fallback", []types.WrittenRecord{types.ConversationWritten(notify.Fallback("e-1"))},
			wakeOrigin{conversation: notify.Fallback("e-1")}, false},
		{"an item with no id", []types.WrittenRecord{{Kind: types.WrittenItem, ID: string(types.WorkWorklet) + ":"}},
			wakeOrigin{item: &types.WorkItem{Backend: types.WorkWorklet}}, false},
		{"nothing written", nil, wakeOrigin{conversation: change, item: &task}, false},
	} {
		if got := wroteAbout(tc.wrote, tc.o); got != tc.want {
			t.Errorf("%s: about what the run wrote %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE THIRD FIXED RULE: THE REQUESTER'S WAKE ABOUT A RECORD THE RUN WROTE JOINS,
// ANYBODY ELSE'S IS TRIAGED. Checked after the conversation and the item, so a
// wake both name keeps their rule; never for a type the run does not take, a
// worked wake or another writer, whose wake about the record is a candidate
// with the relation `wrote`; a wake about a record the run only read stays as
// it did.
//
// Mutation: join another writer's wake about a written record, or check the
// written set before the conversation, and a case goes red.
func TestTheRequestersWakeAboutWhatTheRunWroteJoinsAndAnyoneElsesIsTriaged(t *testing.T) {
	t.Parallel()
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-1"}
	run := sandbox.PendingRun{Requester: "lead", ConversationKey: "chat:C1", WorkItem: item}
	elsewhere := wakeOrigin{requester: "lead", conversation: gitlet.ChangeConversation("c-1")}
	wake := &events.Event{Type: types.ExternalNotification{}.EventType()}
	ceo := elsewhere
	ceo.requester = "ceo"
	for _, tc := range []struct {
		name          string
		ev            *events.Event
		o             wakeOrigin
		worked, wrote bool
		rule          routeRule
		why           string
		triaged       bool
	}{
		{"the requester about a record it wrote", wake, elsewhere, false, true, ruleWrote, "", false},
		{"the requester in its conversation, a record it wrote too", wake,
			wakeOrigin{requester: "lead", conversation: "chat:C1"}, false, true, ruleConversation, "", false},
		{"the requester about its item, a record it wrote too", wake,
			wakeOrigin{requester: "lead", conversation: "chat:C9", item: item}, false, true, ruleWorkItem, "", false},
		{"another writer about a record it wrote", wake, ceo, false, true, ruleNone, stayWriter, true},
		{"the requester about a record it only read", wake, elsewhere, false, false, ruleNone, stayOtherWork, false},
		{"another writer about a record it only read", wake, ceo, false, false, ruleNone, stayWriter, false},
		{"a worked wake about a record it wrote", wake, elsewhere, true, true, ruleNone, stayWorked, false},
		{"a type the ledger does not key", &events.Event{Type: "agent_phase_started"}, elsewhere, false, true,
			ruleNone, stayKind, false},
	} {
		rule, why := routeOf(run, tc.ev, tc.o, tc.worked, tc.wrote)
		if rule != tc.rule || why != tc.why {
			t.Errorf("%s: routed (%q, %q), want (%q, %q)", tc.name, rule, why, tc.rule, tc.why)
		}
		if rule != ruleNone {
			continue
		}
		if got := candidate(why, wakeRelations{wrote: tc.wrote}); got != tc.triaged {
			t.Errorf("%s: triaged %v, want %v", tc.name, got, tc.triaged)
		}
	}
	r := newRouterRig(t)
	if rel := r.e.relationsOf(t.Context(), "swe", run, ceo, true); !rel.wrote || rel.conversation || rel.item {
		t.Errorf("another writer's wake about a written record relates as %+v, want wrote alone", rel)
	}
}

// A WAKE ABOUT A RECORD THE RUN WROTE REACHES THE RUNNING RUN INSTEAD OF
// WAITING. Before the run opens the change, the requester's review of it waits
// as every wake about other work did; once the run's bridged call that opened
// it is recorded (the ledger handing its log the record), a fresh read files
// the review on the run's record for its job. Another writer's review of the
// change is triaged, told to the model as about something the running task
// wrote, outside its fence, and its input joins; a review of a change the run
// never wrote costs no triage.
//
// Mutation: hand the store no Wrote from the ledger, read no written set in
// routeSeat, or drop the relation from the triage's prompt, and this goes red.
func TestAWakeAboutARecordTheRunWroteReachesTheRunningRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newTriageRig(t)
	script := &llmtest.Script{Runs: []llmtest.Run{decisionRun("input", "the reviewer's finding")}}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "cheap", Provider: script}})
	if err != nil {
		t.Fatal(err)
	}
	r.e.epoch.current.Store(&Company{Org: r.e.Company().Org, Models: models})

	review := r.reviewOf(t, "lead", "c-1", "rename the flag before it merges", 7)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if got := r.filed(t); len(got) != 0 {
		t.Fatalf("before the run wrote the change, the record holds wakes %v, want the review waiting", got)
	}

	row := mustRow(t, r.store, "run-1")
	ledger := newBridgeLedger(r.store, nil)
	ledger.bind(sandbox.OpenedJob{LaunchID: r.job, Round: row.Round})
	opened := mcpbridge.Call{Call: tools.Call{Name: "code_open_change"}, Seq: 1,
		StartedAt: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
		Wrote:     []types.WrittenRecord{types.ConversationWritten(gitlet.ChangeConversation("c-1"))}}
	if err := ledger.Record(ctx, "run-1", opened); err != nil {
		t.Fatalf("record the call that opened the change: %v", err)
	}

	other := r.reviewOf(t, "ceo", "c-1", "the migration also needs a down step", 8)
	unrelated := r.reviewOf(t, "ceo", "c-2", "looks good", 9)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if got, want := r.filed(t), []string{review.ID.String(), other.ID.String()}; !slices.Equal(got, want) {
		t.Fatalf("after the run opened the change, the record holds wakes %v, want the requester's review and "+
			"the other writer's input %v", got, want)
	}
	if slices.Contains(r.filed(t), unrelated.ID.String()) {
		t.Error("a review of a change the run never wrote was filed")
	}
	seen := script.Seen()
	if len(seen) != 1 {
		t.Fatalf("the triage was asked %d times, want once, for the other writer's review alone", len(seen))
	}
	var said strings.Builder
	for _, m := range seen[0].Messages {
		said.WriteString(m.Content)
	}
	text := said.String()
	if why, fence := strings.Index(text, "about something the running task created or wrote"),
		strings.Index(text, "the migration also needs a down step"); why < 0 || why > fence {
		t.Errorf("the triage was not told, before the wake's words, that it is about what the task wrote:\n%s",
			text)
	}
}

// reviewOf publishes to swe's inbox seat's review of Gitlet change id as a
// person, asking for changes, from the code log's record at seq, stamped with
// the change's conversation as every code wake is.
func (r *routerRig) reviewOf(t *testing.T, seat, change, words string, seq uint64) *events.Event {
	t.Helper()
	ev := events.New(types.ExternalNotification{NotificationSource: gitlet.Source, SourceEventType: "review",
		Sender: seat, Body: words, Addressed: true,
		Metadata: map[string]string{gitlet.MetaChangeID: change, gitlet.MetaVia: string(gitlet.ReasonChangesRequested)},
		Writer:   &types.NotificationWriter{Kind: iam.ActorHuman, Seat: seat}}, events.TraceContext{})
	conversation := gitlet.ChangeConversation(change)
	ev.Payload = map[string]any{notify.ConversationField: conversation, notify.PartitionField: conversation,
		notify.TriggerField: statelog.Position{Stream: "code", Generation: 1, Seq: seq}.String()}
	if err := r.q.Publish(t.Context(), r.subject, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}
