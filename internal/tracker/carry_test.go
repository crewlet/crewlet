package tracker_test

import (
	"encoding/json"
	"reflect"
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
// depth, bar the ones extra.go names with the reason each does not.
func TestEveryObjectTheTrackerSharesCarries(t *testing.T) {
	t.Parallel()
	roots := []reflect.Type{}
	for _, tc := range wireCases() {
		roots = append(roots, tc.typ)
	}
	for _, missing := range jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[tracker.Subject]():       "an address recovered from the log subject",
		reflect.TypeFor[tracker.Relation]():      "compared by value",
		reflect.TypeFor[tracker.ChecklistItem](): "compared by value",
		reflect.TypeFor[tracker.Delta]():         "compared by value",
		reflect.TypeFor[tracker.GoalTarget]():    "embedded in GoalTargetRow, which its methods would take over",
	}, roots...) {
		t.Error(missing)
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

// A GOAL'S TARGET ROW IS WRITTEN WITH ITS PROGRESS. It embeds the target, so a
// MarshalJSON on the target would be promoted onto it and write the row as the
// bare target — which is why the target does not carry (extra.go).
func TestAGoalsTargetRowIsWrittenWithItsProgress(t *testing.T) {
	t.Parallel()
	half := 0.5
	out, err := json.Marshal(tracker.GoalTargetRow{
		GoalTarget: tracker.GoalTarget{ID: "g-1", Name: "ship"}, Progress: &half, Finished: 1, Total: 2,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(out, &members); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for member, want := range map[string]string{
		"id": `"g-1"`, "progress": "0.5", "finished_tasks": "1", "total_tasks": "2",
	} {
		if string(members[member]) != want {
			t.Errorf("the row writes %s as %s, want %s: %s", member, members[member], want, out)
		}
	}
}
