package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/named"
)

// Every event whose line names something a person can open says so beside the
// line, and the line itself reads exactly as it did: the references are found
// in it by their words, so a line and its refs that drifted apart would leave
// a link on nothing.
func TestEveryLineSaysWhatItNames(t *testing.T) {
	t.Parallel()
	oid := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name    string
		payload events.Payload
		want    string
		refs    []named.Ref
	}{{
		name:    "a pickup names the item it hands over, by its id, spelled as its key",
		payload: WorkPickup{RoleName: "Site Reliability Engineer", Item: "0198-task", Key: "ENG-287"},
		want:    "Site Reliability Engineer was handed its next ready work item ENG-287",
		refs:    []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"}},
	}, {
		name:    "a dead wait's hand-back names the item too",
		payload: WorkPickup{RoleName: "SRE", Item: "0198-task", Key: "ENG-287", Why: PickupDeadWait},
		want:    "SRE was handed back ENG-287, whose wait on a pull request nothing moves",
		refs:    []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"}},
	}, {
		name: "a scheduled fire names its schedule by scope and name",
		payload: TaskAssigned{RoleName: "Dev", Schedule: "weekly-report", ScopeType: ScheduleScopeRole,
			ScopeID: "dev"},
		want: "Dev was assigned scheduled work weekly-report",
		refs: []named.Ref{{Kind: named.Schedule, ID: "role/dev/weekly-report", Text: "weekly-report"}},
	}, {
		name: "a turn on a Worklet task names it",
		payload: AgentTurnStarted{RoleName: "Dev", WorkItem: &WorkItem{Backend: WorkWorklet,
			ID: "0198-task", Key: "ENG-12"}},
		want: "Dev started a turn on ENG-12",
		refs: []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-12"}},
	}, {
		name: "a turn on another tracker's item says its key and names nothing",
		payload: AgentTurnStarted{RoleName: "Dev", WorkItem: &WorkItem{Backend: WorkJira,
			ID: "10001", Key: "ENG-1"}},
		want: "Dev started a turn on ENG-1",
	}, {
		name:    "an opened change names its change",
		payload: ChangeOpened{RepoName: "web", Number: 42, Target: "refs/heads/main", Title: "Fix login"},
		want:    "web opened web!42 into main: Fix login",
		refs:    []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}},
	}, {
		name:    "a merge names its change",
		payload: ChangeMerged{RepoName: "web", Number: 42, Target: "refs/heads/main", Method: "squash"},
		want:    "web merged web!42 into main (squash)",
		refs:    []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}},
	}, {
		name: "a review names its change",
		payload: ReviewSubmitted{RepoName: "web", Number: 42, Revision: 2, Verdict: "changes_requested",
			Findings: 1},
		want: "web reviewed web!42 (revision 2): changes requested, 1 finding",
		refs: []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}},
	}, {
		name: "an ejection names its change",
		payload: CodeQueueEjected{RepoName: "web", Number: 7, Target: "refs/heads/main", Reason: "checks",
			Checks: []string{"lint"}},
		want: "web ejected web!7 from main's merge queue (checks): lint failed",
		refs: []named.Ref{{Kind: named.Change, ID: "web!7", Text: "web!7"}},
	}, {
		name:    "a check's ending names the repository and the commit",
		payload: CodeCheckFinished{RepoName: "web", OID: oid, Name: "test", Attempt: 1, State: "passed"},
		want:    "web check test (attempt 1) of web@0123456789ab passed",
		refs: []named.Ref{{Kind: named.Repository, ID: "web", Text: "web"},
			{Kind: named.Commit, ID: "web@" + oid, Text: "0123456789ab"}},
	}, {
		name: "a broken branch names the change that landed",
		payload: CodeTargetBroken{RepoName: "web", Target: "refs/heads/main", ChangeID: "c-1", Number: 9,
			Commit: oid, Checks: []string{"test"}},
		want: "web found main broken by web!9: test failed",
		refs: []named.Ref{{Kind: named.Change, ID: "web!9", Text: "web!9"}},
	}, {
		name: "a broken branch with no change names the commit",
		payload: CodeTargetBroken{RepoName: "web", Target: "refs/heads/main", Commit: oid,
			Checks: []string{"test"}},
		want: "web found main broken by 0123456789ab: test failed",
		refs: []named.Ref{{Kind: named.Commit, ID: "web@" + oid, Text: "0123456789ab"}},
	}, {
		name:    "a fetch names the repository",
		payload: CodeFetched{Seat: "dev", RepoName: "web", Via: FetchedViaGit, Bytes: 10},
		want:    "dev fetched web (10 bytes)",
		refs:    []named.Ref{{Kind: named.Repository, ID: "web", Text: "web"}},
	}, {
		name:    "an expunge names the repository",
		payload: CodeExpunged{RepoName: "web", DroppedObjects: 3, DroppedBytes: 9},
		want:    "web expunged 3 unreachable object(s), 9 bytes, from web on its weekly schedule",
		refs:    []named.Ref{{Kind: named.Repository, ID: "web", Text: "web"}},
	}, {
		name:    "an operator's push names the repository",
		payload: OperatorActed{ActorName: "ana", Tool: "git_push", Repo: "web", Outcome: "ok"},
		want:    "ana ran git_push on web: " + outcomeWords("ok", ""),
		refs:    []named.Ref{{Kind: named.Repository, ID: "web", Text: "web"}},
	}, {
		name: "a read of one Doclet page names it by its title",
		payload: KnowledgeRead{RoleName: "Dev", Via: ReadViaSearch, Backend: KnowledgeNative, Query: "deploy",
			Pages: []KnowledgeReadPage{{ID: "page-1", Title: "Runbook"}}},
		want: `Dev searched knowledge for "deploy" ('Runbook')`,
		refs: []named.Ref{{Kind: named.Page, ID: "page-1", Text: "Runbook"}},
	}, {
		name: "a read of another backend's page names nothing",
		payload: KnowledgeRead{RoleName: "Dev", Backend: "confluence",
			Pages: []KnowledgeReadPage{{ID: "9", Title: "Runbook"}}},
		want: "Dev read 'Runbook'",
	}, {
		name:    "a colleague's ask names its channel",
		payload: A2ARequest{Requester: "ana", ChannelID: "ch-1"},
		want:    "ana asked a colleague on ch-1",
		refs:    []named.Ref{{Kind: named.Channel, ID: "ch-1", Text: "ch-1"}},
	}, {
		name:    "a closed channel names it",
		payload: A2AChannelClosed{ChannelID: "ch-1", ClosedBy: "ana", MessageCount: 3},
		want:    "ana closed A2A channel ch-1 (3 messages)",
		refs:    []named.Ref{{Kind: named.Channel, ID: "ch-1", Text: "ch-1"}},
	}, {
		name:    "a reflection names the seat and the turn",
		payload: ReflectionDue{Turn: TurnCompleted{AgentHandle: "dev", TurnID: "turn-1"}},
		want:    "Reflection due on dev's turn turn-1",
		refs: []named.Ref{{Kind: named.Seat, ID: "dev", Text: "dev"},
			{Kind: named.Turn, ID: "turn-1", Text: "turn-1"}},
	}, {
		name: "a schedule's fire names the schedule and the seat",
		payload: ScheduledTaskFired{ScopeType: ScheduleScopeRole, ScopeID: "dev", ScheduleName: "standup",
			TargetHandle: "dev"},
		want: "Scheduled 'standup' fired for dev",
		refs: []named.Ref{{Kind: named.Schedule, ID: "role/dev/standup", Text: "standup"},
			{Kind: named.Seat, ID: "dev", Text: "dev"}},
	}, {
		name: "a notification names what its subject names",
		payload: ExternalNotification{Sender: "ana", Subject: "web!42: Fix login",
			SubjectRefs: []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}}},
		want: "Message from ana: web!42: Fix login",
		refs: []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}},
	}, {
		name:    "a vendor's notification names nothing",
		payload: ExternalNotification{Sender: "ana", Subject: "ENG-1 looks broken"},
		want:    "Message from ana: ENG-1 looks broken",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ev := events.NewFrom(c.payload, events.TraceContext{})
			ev.Source = "web"
			if got := ev.Summary(); got != c.want {
				t.Fatalf("summary = %q, want %q", got, c.want)
			}
			if got := ev.References(); !reflect.DeepEqual(got, c.refs) {
				t.Fatalf("refs = %+v, want %+v", got, c.refs)
			}
			at := 0
			for _, r := range c.refs {
				i := strings.Index(c.want[at:], r.Text)
				if i < 0 {
					t.Fatalf("%+v's words are not in %q after byte %d", r, c.want, at)
				}
				at += i + len(r.Text)
			}
		})
	}
}

// A payload writes ONE line: a type with a phrase and a summary besides would
// put two answers to "what does this row say" on two surfaces.
func TestEveryRegisteredTypeWritesOneLine(t *testing.T) {
	t.Parallel()
	for _, name := range events.RegisteredTypes() {
		payload, ok := events.PayloadFor(name)
		if !ok {
			t.Fatalf("%s is registered and has no payload", name)
		}
		_, phrase := payload.(events.Phraser)
		_, actor := payload.(events.ActorSummarizer)
		_, plain := payload.(events.Summarizer)
		if n := btoi(phrase) + btoi(actor) + btoi(plain); n > 1 {
			t.Errorf("%s writes its line %d ways (PhraseFor %v, SummaryFor %v, Summary %v); keep one",
				name, n, phrase, actor, plain)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The trigger a turn is woken by carries its line's refs on the wire, so a
// turn's page and its live row can draw them, and a descriptor read back keeps
// them.
func TestATriggerCarriesWhatItsSummaryNames(t *testing.T) {
	t.Parallel()
	ev := events.NewFrom(WorkPickup{RoleName: "SRE", Item: "0198-task", Key: "ENG-287"},
		events.TraceContext{})
	trigger := DescribeTrigger(ev)
	want := []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"}}
	if !reflect.DeepEqual(trigger.Refs, want) {
		t.Fatalf("trigger refs = %+v, want %+v", trigger.Refs, want)
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"refs":[{"kind":"work_item","id":"0198-task","text":"ENG-287"}]`) {
		t.Fatalf("wire form %s does not carry the refs", raw)
	}
	var back Trigger
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Refs, want) {
		t.Fatalf("read back refs = %+v, want %+v", back.Refs, want)
	}
	if plain := DescribeTrigger(events.NewFrom(AgentSpawned{}, events.TraceContext{})); plain.Refs != nil ||
		strings.Contains(string(mustJSON(t, plain)), `"refs"`) {
		t.Fatalf("a trigger whose line names nothing carries refs: %+v", plain.Refs)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
