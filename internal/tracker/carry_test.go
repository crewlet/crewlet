package tracker_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// wireCases is every object the tracker stores as a document or carries on its
// log, filled so that every member it has is on the wire, and encoded and
// decoded the way the tracker itself does it.
func wireCases() map[string]wireCase {
	return map[string]wireCase{
		"task":            documentCase[tracker.Task](),
		"task patch":      documentCase[tracker.TaskPatch](),
		"counter":         documentCase[tracker.Counter](),
		"rank order":      documentCase[tracker.RankOrder](),
		"eviction":        documentCase[tracker.Eviction](),
		"generation":      documentCase[tracker.Generation](),
		"type catalogue":  documentCase[tracker.TypeCatalogue](),
		"field catalogue": documentCase[tracker.FieldCatalogue](),
		"project":         documentCase[tracker.Project](),
		"tag set":         documentCase[tracker.TagSet](),
		"view":            documentCase[tracker.View](),
		"goal":            documentCase[tracker.Goal](),
		"person":          documentCase[tracker.Person](),
		"comment":         documentCase[tracker.Comment](),
		"body revision":   documentCase[tracker.BodyRevision](),
		"key alias":       documentCase[tracker.KeyAlias](),
		"turn spend":      documentCase[tracker.TurnSpend](),
		"record": {
			typ: reflect.TypeFor[tracker.MutationRecord](),
			encode: func() ([]byte, error) {
				rec := jsoncarrytest.Filled[tracker.MutationRecord]()
				rec.V = tracker.RecordVersion
				rec.Subject = tracker.TaskSubject("t-1")
				rec.Op = tracker.OpPatch
				rec.Scope = tracker.ScopeSet{Subject: true, Container: "ENG"}
				return rec.Encode()
			},
			decode: func(b []byte) ([]byte, error) {
				rec, err := tracker.Decode(b)
				if err != nil {
					return nil, err
				}
				return rec.Encode()
			},
		},
	}
}

type wireCase struct {
	typ    reflect.Type
	encode func() ([]byte, error)
	decode func([]byte) ([]byte, error)
}

func documentCase[T any]() wireCase {
	return wireCase{
		typ:    reflect.TypeFor[T](),
		encode: func() ([]byte, error) { return json.Marshal(jsoncarrytest.Filled[T]()) },
		decode: func(b []byte) ([]byte, error) {
			var v T
			if err := json.Unmarshal(b, &v); err != nil {
				return nil, err
			}
			return json.Marshal(v)
		},
	}
}

// wireGolden is each case's encoding: the bytes every node running this format
// holds for it.
var wireGolden = map[string]string{
	"body revision":   `{"task":"Task","version":7,"body":"Body","author":"Author","author_kind":"AuthorKind","at":"2026-01-02T03:04:05Z"}`,
	"comment":         `{"id":"ID","task":"Task","author":"Author","author_kind":"AuthorKind","body":"Body","mentions":["Mentions"],"reply_to":"ReplyTo","ask":"Ask","answers":"Answers","resolved":true,"resolved_by":"ResolvedBy","resolved_at":"2026-01-02T03:04:05Z","removed":true,"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"counter":         `{"v":7,"version":7,"project":"Project","last":7}`,
	"eviction":        `{"v":7,"version":7,"node_id":"NodeID","evicted_by":"EvictedBy","evicted_at":"2026-01-02T03:04:05Z","readmitted":true}`,
	"field catalogue": `{"v":7,"version":7,"fields":[{"id":"ID","slug":"Slug","name":"Name","description":"Description","type":"Type","applies_to":["AppliesTo"],"required":true,"required_in_subtasks":true,"default":{"raw":true},"config":{"options":[{"id":"ID","slug":"Slug","name":"Name","color":"Color","order":7,"archived":true}],"unit":"Unit","precision":7,"min":1.5,"max":1.5,"time":true,"progress":"Progress","tracking":["Tracking"],"project":"Project","multi":true,"rollup":{"source":"Source","field":"Field","op":"Op"}},"archived":true,"pinned":true,"hide_from_agents":true,"created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z"}],"policy_version":7,"updated_at":"2026-01-02T03:04:05Z"}`,
	"generation":      `{"v":7,"gen":7,"prev_stream_created_at":"2026-01-02T03:04:05Z","new_stream_created_at":"2026-01-02T03:04:05Z","prev_last_seq_seen":7,"reanchored_by":"ReanchoredBy","reason":"Reason"}`,
	"goal":            `{"v":7,"version":7,"id":"ID","name":"Name","description":"Description","owners":["Owners"],"members":["Members"],"group":"Group","start_at":"2026-01-02T03:04:05Z","due_at":"2026-01-02T03:04:05Z","health":"Health","updates":[{"at":"2026-01-02T03:04:05Z","author":"Author","health":"Health","text":"Text"}],"targets":[{"id":"ID","name":"Name","type":"Type","start":1.5,"goal":1.5,"current":1.5,"unit":"Unit","tasks":["Tasks"],"projects":["Projects"],"done":true}],"archived":true,"created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"key alias":       `{"key":"Key","task_id":"TaskID","current":true}`,
	"person":          `{"v":7,"version":7,"handle":"Handle","generation":"Generation","seen_through":{"stream":"Stream","generation":7,"seq":7},"read":[{"record_id":"RecordID","position":7,"until":"2026-01-02T03:04:05Z"}],"unread":[{"record_id":"RecordID","position":7,"until":"2026-01-02T03:04:05Z"}],"snoozed":[{"record_id":"RecordID","position":7,"until":"2026-01-02T03:04:05Z"}],"primary_reasons":["PrimaryReasons"],"priorities":["Priorities"],"pinned_views":["PinnedViews"],"favorites":[{"kind":"Kind","id":"ID"}],"priorities_set_by":"PrioritiesSetBy","priorities_set_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"project":         `{"v":7,"version":7,"scoped_through":7,"key":"Key","name":"Name","purpose":"Purpose","unit":"Unit","chart_epoch":7,"fields":[{"id":"ID","slug":"Slug","name":"Name","description":"Description","type":"Type","applies_to":["AppliesTo"],"required":true,"required_in_subtasks":true,"default":{"raw":true},"config":{"options":[{"id":"ID","slug":"Slug","name":"Name","color":"Color","order":7,"archived":true}],"unit":"Unit","precision":7,"min":1.5,"max":1.5,"time":true,"progress":"Progress","tracking":["Tracking"],"project":"Project","multi":true,"rollup":{"source":"Source","field":"Field","op":"Op"}},"archived":true,"pinned":true,"hide_from_agents":true,"created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z"}],"default_assignee":"DefaultAssignee","policy_version":7,"archived":true,"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"rank order":      `{"v":7,"version":7,"scoped_through":7,"project":"Project","placements":[{"task":"Task","rank":"Rank"}]}`,
	"record":          `{"v":1,"op_id":"OpID","subject":{"kind":"task","id":"t-1"},"op":"patch","created_at":"2026-01-02T03:04:05Z","gen":7,"writer":"Writer","scope":"s/ENG","expect":7,"mutation":{"raw":true},"actor":"Actor","actor_kind":"ActorKind","operator_id":"OperatorID","turn_id":"TurnID","chain":["Chain"],"batch_id":"BatchID","kind":"Kind","notify":{"kind":"Kind","fields":{"key":{"from":"From","to":"To"}},"comment_id":"CommentID","excerpt":"Excerpt","mentions":["Mentions"],"late":true,"snapshot":{"key":"Key","project":"Project","title":"Title","status":"Status","status_group":"StatusGroup","assignee":"Assignee","reporter":"Reporter","watchers":["Watchers"],"collaborators":["Collaborators"],"project_lead":"ProjectLead","routing_unit_lead":"RoutingUnitLead","unblocked":[{"task":"Task","key":"Key","assignee":"Assignee"}],"dependents":[{"task":"Task","key":"Key","assignee":"Assignee"}],"prev_assignee":"PrevAssignee","prev_status_group":"PrevStatusGroup","comment_author_kind":"CommentAuthorKind","comment_ask":"CommentAsk","answered_author":"AnsweredAuthor","thread_participants":["ThreadParticipants"],"removed_watchers":["RemovedWatchers"],"parent_assignee":"ParentAssignee","checklist_assignees":["ChecklistAssignees"],"goal_owners":["GoalOwners"],"goal_members":["GoalMembers"],"routed_to":"RoutedTo","person":"Person","goal_name":"GoalName","prioritised_by":"PrioritisedBy","position":7,"task":"Task"}}}`,
	"tag set":         `{"v":7,"version":7,"project":"Project","tags":[{"slug":"Slug","label":"Label","color":"Color","description":"Description","archived":true,"created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z"}],"tags_version":7,"updated_at":"2026-01-02T03:04:05Z"}`,
	"task":            `{"v":7,"id":"ID","version":7,"scoped_through":7,"key":"Key","former_keys":["FormerKeys"],"project":"Project","filed_unit":"FiledUnit","routing_unit":"RoutingUnit","parent":"Parent","depth":7,"type":"Type","title":"Title","body":"Body","body_version":7,"body_author":"BodyAuthor","body_at":"2026-01-02T03:04:05Z","status":"Status","status_group":"StatusGroup","status_entered_at":"2026-01-02T03:04:05Z","priority":"Priority","rank":"Rank","reporter":"Reporter","assignee":"Assignee","collaborators":["Collaborators"],"watchers":["Watchers"],"muted":["Muted"],"tags":["Tags"],"fields":{"key":{"raw":true}},"relations":[{"kind":"Kind","other":"Other","note":"Note","created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z","one_sided_final":true}],"dependents":["Dependents"],"checklists":[{"id":"ID","name":"Name","items":[{"id":"ID","name":"Name","done":true,"assignee":"Assignee","parent":"Parent","order":7,"promoted_to":"PromotedTo"}]}],"start_at":"2026-01-02T03:04:05Z","due_at":"2026-01-02T03:04:05Z","due_all_day":true,"estimate_minutes":7,"points":1.5,"spend":{"turns":7,"rounds":7,"input":7,"output":7,"cache_read":7,"cache_write":7,"wall_ms":7,"tokens":7},"done_at":"2026-01-02T03:04:05Z","closed_at":"2026-01-02T03:04:05Z","archived":true,"merging":true,"merge_reparent":true,"merge_into":"MergeInto","archived_at":"2026-01-02T03:04:05Z","archived_by":"ArchivedBy","removed":{"by":"By","kind":"Kind","at":"2026-01-02T03:04:05Z","removed_with":"RemovedWith"},"change_seq":7,"reassignments":7,"policy_stamp":"PolicyStamp","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"task patch":      `{"title":"Title","body":"Body","body_revision":{"task":"Task","version":7,"body":"Body","author":"Author","author_kind":"AuthorKind","at":"2026-01-02T03:04:05Z"},"retires":[7],"status":"Status","priority":"Priority","type":"Type","assignee":"Assignee","routing_unit":"RoutingUnit","parent":"Parent","project":"Project","mint":{"n":7,"base":7,"length":7},"start_at":"2026-01-02T03:04:05Z","due_at":"2026-01-02T03:04:05Z","due_all_day":true,"estimate_minutes":7,"points":1.5,"archived":true,"removed":{"by":"By","kind":"Kind","at":"2026-01-02T03:04:05Z","removed_with":"RemovedWith"},"merging":true,"merge_reparent":true,"merge_into":"MergeInto","reassignments":7,"collaborators":["Collaborators"],"watchers":["Watchers"],"muted":["Muted"],"tags":["Tags"],"fields":{"key":{"raw":true}},"relations":[{"kind":"Kind","other":"Other","note":"Note","created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z","one_sided_final":true}],"dependents":["Dependents"],"checklists":[{"id":"ID","name":"Name","items":[{"id":"ID","name":"Name","done":true,"assignee":"Assignee","parent":"Parent","order":7,"promoted_to":"PromotedTo"}]}],"former_keys":["FormerKeys"],"comment":{"id":"ID","task":"Task","author":"Author","author_kind":"AuthorKind","body":"Body","mentions":["Mentions"],"reply_to":"ReplyTo","ask":"Ask","answers":"Answers","resolved":true,"resolved_by":"ResolvedBy","resolved_at":"2026-01-02T03:04:05Z","removed":true,"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}}`,
	"turn spend":      `{"turns":7,"rounds":7,"input":7,"output":7,"cache_read":7,"cache_write":7,"wall_ms":7}`,
	"type catalogue":  `{"v":7,"version":7,"types":[{"slug":"Slug","name":"Name","plural":"Plural","icon":"Icon","description":"Description","builtin":true,"archived":true}],"updated_at":"2026-01-02T03:04:05Z"}`,
	"view":            `{"v":7,"version":7,"id":"ID","container":{"kind":"Kind","id":"ID"},"name":"Name","type":"Type","params":{"key":"Params"},"owner":"Owner","protected":true,"default":true,"rank":"Rank","icon":"Icon","created_by":"CreatedBy","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
}

// EVERY OBJECT THIS BUILD WRITES IS THE BYTES IT ALWAYS WAS.
//
// A document and a record are shared by every node that holds the log, so the
// bytes are a contract between peers rather than a detail of this build: a
// record's size is the figure the read index's cost model rests on, and a
// document another build wrote is decoded, merged and encoded again by this
// one. An object carrying nothing this build does not know encodes exactly as
// its struct does, and decoding those bytes and encoding them again changes
// nothing — for a record whose kind was once filed as unknown and moved the
// whole record into key order, as much as for any other.
func TestEveryObjectEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	for name, tc := range wireCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, pinned := wireGolden[name]
			if !pinned {
				t.Fatalf("%s has no pinned encoding", name)
			}
			got, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != want {
				t.Fatalf("%s encodes as\n  %s\nand every node holds\n  %s", name, got, want)
			}
			again, err := tc.decode(got)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(again) != want {
				t.Fatalf("%s decoded and encoded again is\n  %s\nnot\n  %s", name, again, want)
			}
		})
	}
}

// EVERY OBJECT THE TRACKER SHARES CARRIES WHAT IT DOES NOT KNOW, at every
// depth — the terms of a scope written by a method of its own among them —
// bar its subject, an address extra.go names with the reason it does not.
func TestEveryObjectTheTrackerSharesCarries(t *testing.T) {
	t.Parallel()
	roots := []reflect.Type{}
	for _, tc := range wireCases() {
		roots = append(roots, tc.typ)
	}
	for _, missing := range jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[tracker.Subject](): "an address recovered from the log subject",
	}, roots...) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE A SCOPE'S TERM SURVIVES THIS BUILD'S
// DECODE AND ENCODE of the record. The scope is written by a method of its own
// as the list of its terms, so the member-by-member plant above never reaches
// inside one; this is that term, planted by hand.
//
// Mutation: drop ScopeTerm's UnmarshalJSON in extra.go and the member is gone.
func TestAMemberInsideAScopeTermSurvivesARecordsRoundTrip(t *testing.T) {
	t.Parallel()
	rec := jsoncarrytest.Filled[tracker.MutationRecord]()
	rec.V = tracker.RecordVersion
	rec.Subject = tracker.TaskSubject("t-1")
	rec.Op = tracker.OpPatch
	rec.Scope = tracker.ScopeSet{Terms: []tracker.ScopeTerm{
		{Kind: tracker.TermObject, Container: "ENG", ID: "t-1"},
		{Kind: tracker.TermObject, Container: "ENG", ID: "t-2"},
	}}
	body, err := rec.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const plain = `"scope":[{"k":"object","c":"ENG","i":"t-1"},{"k":"object","c":"ENG","i":"t-2"}]`
	if !strings.Contains(string(body), plain) {
		t.Fatalf("the enumerated scope is written as something other than %s: %s", plain, body)
	}
	newer := strings.Replace(string(body), `"i":"t-2"}`,
		`"i":"t-2","`+jsoncarrytest.PlantedName+`":`+jsoncarrytest.PlantedValue+`}`, 1)
	back, err := tracker.Decode([]byte(newer))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	again, err := back.Encode()
	if err != nil {
		t.Fatalf("encode again: %v", err)
	}
	if string(again) != newer {
		t.Errorf("the record came back as\n  %s\nnot\n  %s", again, newer)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE ANY OBJECT SURVIVES THIS BUILD'S DECODE
// AND ENCODE of every document and record, byte for byte.
//
// Mutation: drop any one type's UnmarshalJSON in extra.go and the objects of
// that type fail here, by path.
func TestAMemberANewerBuildAddedSurvivesInsideEveryObject(t *testing.T) {
	t.Parallel()
	for name, tc := range wireCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jsoncarrytest.Survives(t, tc.typ, []byte(wireGolden[name]), tc.decode)
		})
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE A STORED TASK SURVIVES A COMMIT ON IT.
//
// The commit decodes the stored document, merges the patch onto it and
// encodes the result — so a member inside one of the task's own objects that
// this build had no field for would be gone from this node's row, and kept on
// every peer that knows it.
func TestAMemberInsideAStoredTaskSurvivesACommitOnIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	task := newTask("c")
	task.Checklists = []tracker.Checklist{{ID: "cl-1", Name: "launch"}}
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("encode the task: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode the task: %v", err)
	}
	document["lane"] = "urgent"
	document["checklists"].([]any)[0].(map[string]any)["lane"] = "pinned"
	newer, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the newer task: %v", err)
	}
	if _, err := h.apply(taskRecord("c", tracker.OpCreate, json.RawMessage(newer), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	renamed := "renamed"
	patch := taskRecord("c", tracker.OpPatch, tracker.TaskPatch{Title: &renamed}, nil)
	patch.OpID = "c-rename"
	if _, err := h.apply(patch, at); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for path, want := range map[string]string{"$.lane": "urgent", "$.checklists[0].lane": "pinned", "$.title": renamed} {
		if got := h.text(`SELECT json_extract(CAST(document AS TEXT), '` + path + `')
			FROM tracker_tasks WHERE id = 'c'`); got != want {
			t.Errorf("after the commit the stored task holds %s = %q, want %q", path, got, want)
		}
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE A RECORD'S NOTIFICATION REACHES THE
// FEED'S BODY — the parser on a newer node reading a record an older node
// relayed sees what the writer wrote, inside the notification and its
// snapshot as much as at the top.
func TestAMemberInsideANotificationReachesTheFeedBody(t *testing.T) {
	t.Parallel()
	record := feedRecord(tracker.TaskSubject("t-1"), tracker.OpPatch, &tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Assignee: "ana",
			Unblocked: []tracker.TaskParty{{Task: "t-2", Key: "ENG-2"}},
		},
	}, time.Unix(1_700_000_000, 0).UTC())
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	notify := raw["notify"].(map[string]any)
	snapshot := notify["snapshot"].(map[string]any)
	notify["lane"] = "notify"
	snapshot["lane"] = "snapshot"
	snapshot["unblocked"].([]any)[0].(map[string]any)["lane"] = "party"
	if payload, err = json.Marshal(raw); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	delivery, wakes, err := tracker.NewTranslator().Translate(t.Context(),
		changefeed.Record{ID: record.OpID, Key: "k", Payload: payload})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	gotNotify, _ := delivery.Body["notify"].(map[string]any)
	gotSnapshot, _ := gotNotify["snapshot"].(map[string]any)
	gotUnblocked, _ := gotSnapshot["unblocked"].([]any)
	var gotParty map[string]any
	if len(gotUnblocked) == 1 {
		gotParty, _ = gotUnblocked[0].(map[string]any)
	}
	for where, got := range map[string]any{
		"notify": gotNotify["lane"], "snapshot": gotSnapshot["lane"], "party": gotParty["lane"],
	} {
		if got != where {
			t.Errorf("the body's %s holds lane %v, want the newer build's %q — body %v",
				where, got, where, delivery.Body)
		}
	}
}

// A GOAL'S TARGET ROW IS WRITTEN WITH ITS PROGRESS, AND WITH WHAT ITS TARGET
// CARRIES. It embeds the target, whose MarshalJSON would be promoted onto it
// and write the row as the bare target; the row's own methods (goalsread.go)
// write the target's members, what the target carries and the row's members,
// in the order encoding/json lays an embedding out — so a row whose target
// carries nothing is the bytes it always was.
//
// Mutation: delete GoalTargetRow's MarshalJSON and the progress is gone.
func TestAGoalsTargetRowIsWrittenWithItsProgress(t *testing.T) {
	t.Parallel()
	half := 0.5
	row := tracker.GoalTargetRow{
		GoalTarget: tracker.GoalTarget{ID: "g-1", Name: "ship", Type: "tasks"},
		Progress:   &half, Finished: 1, Total: 2,
	}
	out, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const golden = `{"id":"g-1","name":"ship","type":"tasks","progress":0.5,"finished_tasks":1,"total_tasks":2}`
	if string(out) != golden {
		t.Errorf("the row is written as\n  %s\nnot\n  %s", out, golden)
	}

	// What the target carries is written once, after the row's own
	// members; a carried member under a name the row decodes is the row's.
	row.Extra = map[string]json.RawMessage{
		"lane": json.RawMessage(`"urgent"`), "progress": json.RawMessage(`9`),
	}
	out, err = json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal with a carry: %v", err)
	}
	const carried = `{"id":"g-1","name":"ship","type":"tasks","progress":0.5,"finished_tasks":1,"total_tasks":2,"lane":"urgent"}`
	if string(out) != carried {
		t.Errorf("the row carrying a member is written as\n  %s\nnot\n  %s", out, carried)
	}
	var back tracker.GoalTargetRow
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Progress == nil || *back.Progress != half || back.Finished != 1 || back.Total != 2 ||
		back.ID != "g-1" || string(back.Extra["lane"]) != `"urgent"` || len(back.Extra) != 1 {
		t.Errorf("the row reads back as %+v (progress %v)", back, back.Progress)
	}
}

// A CHECKLIST ITEM IS NEWS TO ITS ASSIGNEE ON WHAT THE ASSIGNEE READS — its
// text, its done flag, its owner, the subtask it became — and on nothing else.
// A drag that renumbers the list and nests one item under another wakes
// nobody, and neither does a member a newer build wrote that this build
// carries and cannot read: the build that can wrote its own recipients.
//
// Mutation: compare the items with reflect.DeepEqual in wake.go and both
// assignees are woken.
func TestAChecklistItemIsNewsOnlyOnWhatItsAssigneeReads(t *testing.T) {
	t.Parallel()
	before := tracker.Task{ID: "t", Key: "ENG-1", Project: "ENG",
		Checklists: []tracker.Checklist{{ID: "l", Items: []tracker.ChecklistItem{
			{ID: "a", Name: "first", Assignee: "ana", Order: 0},
			{ID: "b", Name: "second", Assignee: "bo", Order: 1},
		}}}}
	nested := "b"
	after := before
	after.Checklists = []tracker.Checklist{{ID: "l", Items: []tracker.ChecklistItem{
		{ID: "b", Name: "second", Assignee: "bo", Order: 0},
		{ID: "a", Name: "first", Assignee: "ana", Order: 1, Parent: &nested,
			Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"urgent"`)}},
	}}}
	got := tracker.Wake{Kind: tracker.ChangeChecklist, Before: before, After: after}.Notify(fixedLeads{})
	if len(got.Snapshot.ChecklistAssignees) != 0 {
		t.Errorf("a drag and a carried member name %v as having had their item changed",
			got.Snapshot.ChecklistAssignees)
	}
	after.Checklists[0].Items[1].Name = "first, renamed"
	got = tracker.Wake{Kind: tracker.ChangeChecklist, Before: before, After: after}.Notify(fixedLeads{})
	if len(got.Snapshot.ChecklistAssignees) != 1 || got.Snapshot.ChecklistAssignees[0] != "ana" {
		t.Errorf("a renamed item names %v, want its assignee alone", got.Snapshot.ChecklistAssignees)
	}
}

// storedDeclaration is a field as a newer build left it: a member this build
// does not know on the declaration, its configuration and one of its options,
// and the members a tool has no argument for.
func storedDeclaration() tracker.FieldDef {
	return tracker.FieldDef{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldDropdown,
		AppliesTo: []string{"bug"}, Default: json.RawMessage(`"o-high"`),
		Config: tracker.FieldConfig{
			Options: []tracker.Option{{ID: "o-high", Slug: "high", Name: "High",
				Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"option"`)}}},
			Unit:  "sev",
			Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"config"`)},
		},
		CreatedBy: "bo",
		Extra:     map[string]json.RawMessage{"lane": json.RawMessage(`"field"`)},
	}
}

// statedDeclaration is the same field as write_work_catalogue states it: the
// members its schema has, and the rest left to the stored declaration.
func statedDeclaration() tracker.FieldDef {
	return tracker.FieldDef{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldDropdown,
		Config: tracker.FieldConfig{
			Options: []tracker.Option{{ID: "o-high", Slug: "high", Name: "High"}},
		},
		Unstated: tracker.Unstated{AppliesTo: true, Default: true, Config: true},
	}
}

// keptDeclaration reads back, from the stored document at prefix, every member
// storedDeclaration holds that its caller did not state.
func keptDeclaration(t *testing.T, r *roundTrip, table, key, column, prefix string) {
	t.Helper()
	for path, want := range map[string]string{
		prefix + ".lane":                    "field",
		prefix + ".config.lane":             "config",
		prefix + ".config.options[0].lane":  "option",
		prefix + ".config.unit":             "sev",
		prefix + ".applies_to[0]":           "bug",
		prefix + ".default":                 "o-high",
		prefix + ".created_by":              "bo",
		prefix + ".config.options[0].name":  "High",
		prefix + ".config.options[0].slug":  "high",
		prefix + ".config.options[0].id":    "o-high",
		prefix + ".type":                    string(tracker.FieldDropdown),
		prefix + ".id":                      "f-sev",
		prefix + ".config.options[0].color": "",
	} {
		got := r.strings(`SELECT COALESCE(json_extract(CAST(document AS TEXT), '`+path+`'), '')
			FROM `+table+` WHERE `+column+` = ?`, key)
		if len(got) != 1 || got[0] != want {
			t.Errorf("the stored declaration holds %s = %q, want %q", path, got, want)
		}
	}
}

// A FIELDS WRITE KEEPS WHAT ITS CALLER CANNOT STATE, and a restatement of the
// declarations as they are publishes nothing.
//
// The caller's list is the whole set, and each element is laid onto the
// stored declaration with its id: a member a newer build wrote — on the
// declaration, its configuration or an option — and what the tool has no
// argument for are kept, so a restatement is measured on what the caller can
// say and moves no policy version, and a rename keeps them all.
//
// Mutation: replace the mergeFields call in WriteFields with the caller's list
// and the no-op publishes and every kept member is gone.
func TestAFieldsWriteKeepsWhatItsCallerCannotState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-newer",
		tracker.CatalogueSubject(tracker.CatalogueFields), "", tracker.FieldCatalogue{
			V: tracker.DocumentVersion, PolicyVersion: 3, UpdatedAt: wednesday,
			Fields: []tracker.FieldDef{storedDeclaration(),
				{ID: "f-note", Slug: "note", Name: "Note", Type: tracker.FieldText}},
			Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"catalogue"`)},
		}, tracker.ChangeCatalogue, nil); err != nil {
		t.Fatalf("store the newer catalogue: %v", err)
	}
	r.drain()

	// THE NOTE'S OPTIONS ARRIVE AS AN EMPTY LIST, as the tool builds them
	// for `options: []`, where the stored declaration has none at all: the
	// same declaration written, and so no change.
	note := tracker.FieldDef{ID: "f-note", Slug: "note", Name: "Note", Type: tracker.FieldText,
		Config: tracker.FieldConfig{Options: []tracker.Option{}}}
	result, err := r.writer.WriteFields(t.Context(), "op-same", []tracker.FieldDef{statedDeclaration(), note})
	if err != nil {
		t.Fatalf("restate the fields: %v", err)
	}
	if result.Position.Seq != 0 {
		t.Errorf("restating the fields as they are published a record at %+v", result.Position)
	}
	r.drain()
	if got := r.catalogue(tracker.CatalogueQuery{}).PolicyVersion; got != 3 {
		t.Errorf("restating the fields as they are moved the policy version to %d", got)
	}

	renamed := statedDeclaration()
	renamed.Name = "Sev"
	if result, err = r.writer.WriteFields(t.Context(), "op-rename", []tracker.FieldDef{renamed, note}); err != nil {
		t.Fatalf("rename the field: %v", err)
	}
	if result.Position.Seq == 0 {
		t.Fatal("a rename published nothing")
	}
	r.drain()
	key := tracker.CatalogueSubject(tracker.CatalogueFields).ID
	keptDeclaration(t, r, "tracker_catalogues", key, "name", "$.fields[0]")
	for path, want := range map[string]string{"$.lane": "catalogue", "$.fields[0].name": "Sev"} {
		if got := r.strings(`SELECT json_extract(CAST(document AS TEXT), '`+path+`')
			FROM tracker_catalogues WHERE name = ?`, key); len(got) != 1 || got[0] != want {
			t.Errorf("after the rename the catalogue holds %s = %q, want %q", path, got, want)
		}
	}
}

// A PROJECT'S FIELDS EDIT KEEPS WHAT ITS CALLER CANNOT STATE, on the
// workspace catalogue's rule — the same merge, the same measure of
// "unchanged", so a restatement publishes nothing and bumps no policy version.
func TestAProjectsFieldsEditKeepsWhatItsCallerCannotState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-newer",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering", PolicyVersion: 3,
			Fields:    []tracker.FieldDef{storedDeclaration()},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectUpdated, nil); err != nil {
		t.Fatalf("store the newer project: %v", err)
	}
	r.drain()

	same := []tracker.FieldDef{statedDeclaration()}
	result, err := r.writer.WriteProject(t.Context(), "op-same", "ENG",
		tracker.ProjectEdit{Fields: &same}, tracker.ProjectAuthority{Lead: true})
	if err != nil {
		t.Fatalf("restate the project's fields: %v", err)
	}
	if result.Position.Seq != 0 {
		t.Errorf("restating the project's fields as they are published a record at %+v", result.Position)
	}

	renamed := []tracker.FieldDef{statedDeclaration()}
	renamed[0].Name = "Sev"
	if result, err = r.writer.WriteProject(t.Context(), "op-rename", "ENG",
		tracker.ProjectEdit{Fields: &renamed}, tracker.ProjectAuthority{Lead: true}); err != nil {
		t.Fatalf("rename the project's field: %v", err)
	}
	if result.Position.Seq == 0 {
		t.Fatal("a rename published nothing")
	}
	r.drain()
	keptDeclaration(t, r, "tracker_projects", "ENG", "key", "$.fields[0]")
	if got := r.strings(`SELECT json_extract(CAST(document AS TEXT), '$.policy_version')
		FROM tracker_projects WHERE key = 'ENG'`); len(got) != 1 || got[0] != "4" {
		t.Errorf("after one real edit the policy version is %v, want 4", got)
	}
}

// A TYPES WRITE KEEPS WHAT A NEWER BUILD WROTE on a type it names and on the
// catalogue itself, and a restatement publishes nothing.
func TestATypesWriteKeepsWhatItsCallerCannotState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-newer",
		tracker.CatalogueSubject(tracker.CatalogueTypes), "", tracker.TypeCatalogue{
			V: tracker.DocumentVersion, UpdatedAt: wednesday,
			Types: []tracker.TaskType{{Slug: "incident", Name: "Incident",
				Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"type"`)}}},
			Extra: map[string]json.RawMessage{"lane": json.RawMessage(`"catalogue"`)},
		}, tracker.ChangeCatalogue, nil); err != nil {
		t.Fatalf("store the newer catalogue: %v", err)
	}
	r.drain()

	result, err := r.writer.WriteTypes(t.Context(), "op-same",
		[]tracker.TaskType{{Slug: "incident", Name: "Incident"}})
	if err != nil {
		t.Fatalf("restate the types: %v", err)
	}
	if result.Position.Seq != 0 {
		t.Errorf("restating the types as they are published a record at %+v", result.Position)
	}
	if result, err = r.writer.WriteTypes(t.Context(), "op-rename",
		[]tracker.TaskType{{Slug: "incident", Name: "Outage"}}); err != nil {
		t.Fatalf("rename the type: %v", err)
	}
	if result.Position.Seq == 0 {
		t.Fatal("a rename published nothing")
	}
	r.drain()
	key := tracker.CatalogueSubject(tracker.CatalogueTypes).ID
	for path, want := range map[string]string{
		"$.lane": "catalogue", "$.types[0].lane": "type", "$.types[0].name": "Outage",
	} {
		if got := r.strings(`SELECT json_extract(CAST(document AS TEXT), '`+path+`')
			FROM tracker_catalogues WHERE name = ?`, key); len(got) != 1 || got[0] != want {
			t.Errorf("after the rename the catalogue holds %s = %q, want %q", path, got, want)
		}
	}
}

// A PERSON'S INBOX AND STARS KEEP WHAT A NEWER BUILD WROTE ON AN ENTRY the
// person restates — an entry moved from unread to read included, since it is
// the same record's entry in another list.
func TestAPersonsEntriesKeepWhatANewerBuildWrote(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	newer := map[string]json.RawMessage{"lane": json.RawMessage(`"newer"`)}
	if _, err := r.writer.WriteDocument(t.Context(), "op-newer",
		tracker.PersonSubject("ana"), "", tracker.Person{
			V: tracker.DocumentVersion, Handle: "ana", UpdatedAt: wednesday,
			Unread:    []tracker.InboxEntry{{RecordID: "rec-1", Position: 5, Extra: newer}},
			Favorites: []tracker.Favorite{{Kind: "task", ID: "t-1", Extra: newer}},
		}, tracker.ChangePersonUpdated, nil); err != nil {
		t.Fatalf("store the newer person: %v", err)
	}
	r.drain()

	if _, err := r.writer.WriteInbox(t.Context(), "op-read", "ana",
		[]tracker.InboxEntry{{RecordID: "rec-1", Position: 5}}, nil, nil, nil,
		tracker.Position{}); err != nil {
		t.Fatalf("mark the entry read: %v", err)
	}
	r.drain()
	if _, err := r.writer.WritePins(t.Context(), "op-star", "ana", nil,
		[]tracker.Favorite{{Kind: "task", ID: "t-1"}, {Kind: "goal", ID: "g-1"}}); err != nil {
		t.Fatalf("star another thing: %v", err)
	}
	r.drain()
	key := tracker.PersonSubject("ana").ID
	for _, column := range []string{"read_json", "favorites_json"} {
		got := r.strings(`SELECT COALESCE(json_extract(CAST(`+column+` AS TEXT), '$[0].lane'), '')
			FROM tracker_persons WHERE handle = ?`, key)
		if len(got) != 1 || got[0] != "newer" {
			t.Errorf("after the write the person's %s holds lane %q, want %q", column, got, "newer")
		}
	}
}

// AN INTEGER PAST 2^53 LEAVES THE FEED AS THE WRITER WROTE IT — the version a
// record was decided against, and one a newer build carried inside the
// notification. The body is a map, and a number decoded into `any` is a
// float64; the body holds each as the digits it was read from instead.
//
// Mutation: decode recordBody without UseNumber and both come out rounded.
func TestAnIntegerPast2To53LeavesTheFeedExactly(t *testing.T) {
	t.Parallel()
	record := feedRecord(tracker.TaskSubject("t-1"), tracker.OpPatch, &tracker.Notify{
		Kind:     tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{Key: "ENG-1", Project: "ENG", Assignee: "ana"},
		Extra:    map[string]json.RawMessage{"later": json.RawMessage(`9007199254740993`)},
	}, time.Unix(1_700_000_000, 0).UTC())
	record.Expect = 1<<60 + 1
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	delivery, wakes, err := tracker.NewTranslator().Translate(t.Context(),
		changefeed.Record{ID: record.OpID, Key: "k", Payload: payload})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	out, err := json.Marshal(delivery.Body)
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	for _, exact := range []string{`"expect":1152921504606846977`, `"later":9007199254740993`} {
		if !strings.Contains(string(out), exact) {
			t.Errorf("the body lost %s: %s", exact, out)
		}
	}
}
