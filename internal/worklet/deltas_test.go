package worklet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
)

// The comparisons, exercised as values: in-package because they are pure over
// two documents, as `deltas.go` claims. The applier's wiring (each kind
// reaching the right comparison against the right stored row) is
// `historydeltas_test.go`.

// An edge set is a set, so its delta is about membership, not order:
// [RelationIntent.resolve] appends, so two writers adding the same edges in
// opposite orders leave one set in two orders, and a delta in document order
// would record a change nothing made, in a table nothing rewrites.
func TestAnEdgeDeltaIsAboutMembershipRatherThanOrder(t *testing.T) {
	t.Parallel()
	one := Task{Relations: []Relation{
		{Kind: RelationWaitingOn, Other: "b"},
		{Kind: RelationWaitingOn, Other: "a"},
	}}
	other := Task{Relations: []Relation{
		{Kind: RelationWaitingOn, Other: "a"},
		{Kind: RelationWaitingOn, Other: "b"},
	}}
	if moved := TaskDeltas(one, other, nil); moved != nil {
		t.Errorf("re-stating one edge set in another order recorded %v, and "+
			"nothing about the task changed", moved)
	}
	// And a real membership change is recorded, or the assertion above
	// would pass for a comparison that answers nothing.
	grew := Task{Relations: append([]Relation{{Kind: RelationWaitingOn, Other: "c"}},
		other.Relations...)}
	if got := TaskDeltas(other, grew, nil)["waiting_on"]; !sameDelta(got, moves([]string{"c"}, nil)) {
		t.Errorf("adding an edge recorded %+v, want only the edge it added", got)
	}
}

// A wait's reason is a delta of its own: restating a wait on the same date with
// another reason moves no date, so a row recording only the date says nothing.
//
// Mutation: drop `wait_reason` from TaskDeltas, and the restated reason
// records nil.
func TestARestatedWaitReasonIsRecorded(t *testing.T) {
	t.Parallel()
	start := time.Date(2031, 4, 20, 0, 0, 0, 0, time.UTC)
	before := Task{StartAt: &start, WaitReason: "until the vendor's window"}
	after := Task{StartAt: &start, WaitReason: "until the release is cut"}
	got := TaskDeltas(before, after, nil)
	if want := (Delta{From: before.WaitReason, To: after.WaitReason}); len(got) != 1 ||
		!sameDelta(got["wait_reason"], want) {
		t.Errorf("a reason restated on the same date recorded %+v, want only %+v", got, want)
	}
}

// Every relation kind is its own field, derived from [RelationKinds]: the kinds
// are unrelated facts (`waiting_on` is work to clear, `page` a wiki link), so
// one field would make adding a link read as a change to what the task waits
// on, and the derivation records a new kind with no second edit.
func TestEveryRelationKindGetsItsOwnDeltaField(t *testing.T) {
	t.Parallel()
	before := Task{}
	after := Task{}
	for _, kind := range RelationKinds {
		after.Relations = append(after.Relations,
			Relation{Kind: kind, Other: "other-" + string(kind)})
	}
	moved := TaskDeltas(before, after, nil)
	for _, kind := range RelationKinds {
		got, recorded := moved[string(kind)]
		if !recorded {
			t.Errorf("a %s edge moved and the row recorded nothing for it — "+
				"the field is named from RelationKinds so that a kind added "+
				"later needs no second edit", kind)
			continue
		}
		// AN ATLAS ENTRY IS COUNTED, never named ([deltaSet.count]).
		want := moves([]string{"other-" + string(kind)}, nil)
		if kind == RelationEntry {
			want = Delta{From: "0", To: "1"}
		}
		if !sameDelta(got, want) {
			t.Errorf("the %s field recorded %+v", kind, got)
		}
	}
	// And the mirror is its own field, `blocking`: not a relation kind,
	// because it is not an authored edge but the copy a blocker carries so a
	// close can name whom it unblocks.
	mirrored := TaskDeltas(Task{}, Task{Dependents: []string{"dep-2", "dep-1"}}, nil)
	if got := mirrored["blocking"]; !sameDelta(got, moves([]string{"dep-1", "dep-2"}, nil)) {
		t.Errorf("the mirror recorded %+v", got)
	}
}

// A set records what moved, whole, however large the set: both sides cut to fit
// would drop exactly the member a commit added.
func TestASetRecordsWhatMovedHoweverLargeTheSet(t *testing.T) {
	t.Parallel()
	before := Task{}
	for i := range MaxWaitingOn - 1 {
		before.Relations = append(before.Relations, Relation{
			Kind: RelationWaitingOn, Other: fmt.Sprintf("%036d", i)})
	}
	after := before
	after.Relations = append(slices.Clone(before.Relations), Relation{
		Kind: RelationWaitingOn, Other: "the-one-it-added"})
	if got := TaskDeltas(before, after, nil)["waiting_on"]; !sameDelta(got,
		moves([]string{"the-one-it-added"}, nil)) {

		t.Errorf("adding one edge to a set of %d recorded %+v, want only the "+
			"edge it added", len(before.Relations), got)
	}
	// And an ordered list is whole, both sides: its order is its content.
	if got := listText([]string{"a", "b", "c"}); got != "a, b, c" {
		t.Errorf("listText = %q", got)
	}
}

// Free text past the budget is described by its size, never cut: an opening
// reads as the value, a size cannot. Two long texts of one size still record
// that they moved, because the move is established from the values.
func TestALongFreeTextIsDescribedBySizeNotCut(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("p", MaxDeltaValue+1)
	same := strings.Repeat("q", MaxDeltaValue+1)
	got := projectDeltas(Project{Purpose: long}, Project{Purpose: same})["purpose"]
	want := fmt.Sprintf("%d bytes", MaxDeltaValue+1)
	if got.From != want || got.To != want {
		t.Errorf("a long purpose recorded %+v, want its size on both sides", got)
	}
	if got := projectDeltas(Project{Purpose: "short"}, Project{Purpose: "still short"})["purpose"]; !sameDelta(got,
		Delta{From: "short", To: "still short"}) {
		t.Errorf("a short purpose recorded %+v, want it whole", got)
	}
}

// A saved view's parameter delta names what moved and nothing else: an arrival
// shows on `to` alone, a departure on `from` alone, a re-point on both, and an
// unchanged parameter on neither, since the whole query (up to
// [MaxViewParamsBytes]) does not belong in a log line twice.
func TestAViewsParameterDeltaNamesOnlyWhatMoved(t *testing.T) {
	t.Parallel()
	before := View{Params: map[string]string{
		"assignee": "ana", "sort": "-updated", "status": "todo",
	}}
	after := View{Params: map[string]string{
		"assignee": "bo", "sort": "-updated", "tag": "api",
	}}
	got := viewDeltas(before, after)["params"]
	if got.From != "assignee=ana, status=todo" || got.To != "assignee=bo, tag=api" {
		t.Errorf("the parameter delta recorded %+v — `sort` did not move and "+
			"belongs on neither side", got)
	}
	// And a save that changed no parameter records no parameter field.
	if _, recorded := viewDeltas(before, before)["params"]; recorded {
		t.Error("a re-save with the same query recorded a parameter move")
	}
}

// A person's inbox is counted rather than listed, and their queue is not
// ([personDeltas]): an inbox gesture moves an entry between lists, which the
// counts show, while the queue's order is the instruction.
func TestAPersonsInboxIsCountedAndTheirQueueIsOrdered(t *testing.T) {
	t.Parallel()
	before := Person{
		Marks: inbox.Marks[Reason]{
			Unread: []inbox.Entry{{RecordID: "r-1"}, {RecordID: "r-2"}},
		},
		Priorities: []string{"t-1", "t-2"},
	}
	after := Person{
		Marks: inbox.Marks[Reason]{
			Unread: []inbox.Entry{{RecordID: "r-2"}},
			Read:   []inbox.Entry{{RecordID: "r-1"}},
		},
		Priorities: []string{"t-2", "t-1"},
	}
	moved := personDeltas(before, after)
	if got := moved["unread"]; !sameDelta(got, Delta{From: "2", To: "1"}) {
		t.Errorf("the unread count recorded %+v", got)
	}
	if got := moved["read"]; !sameDelta(got, Delta{From: "0", To: "1"}) {
		t.Errorf("the read count recorded %+v — a zero is a value here, not "+
			"an absence", got)
	}
	if got := moved["priorities"]; !sameDelta(got, Delta{From: "t-1, t-2", To: "t-2, t-1"}) {
		t.Errorf("the queue recorded %+v, and a re-ordering with the same "+
			"members is exactly the change somebody made", got)
	}
	// When is not recorded, only who: the row's own instant says when, and a
	// moving stamp would make re-stating an unchanged list a change.
	restated := after
	restated.PrioritiesSetAt = restated.PrioritiesSetAt.AddDate(0, 0, 1)
	if moved := personDeltas(after, restated); moved != nil {
		t.Errorf("re-stating an unchanged queue recorded %v", moved)
	}
}

// Only the delta fields whose members are task ids are resolved.
//
// [counterpartyDeltaFields] builds an activity answer's key map and is derived
// from [RelationKinds], so a new edge kind resolves with no second edit;
// [RelationPage] and [RelationEntry] are excluded because their edges name a
// knowledge-base page and an atlas entry, never a task. A declaration held
// against the rule (the `internal/solo` roster tradition), because the runtime
// cannot show it: a page id resolves to nothing either way. It fails in both
// directions: a kind no longer derived, and `page` creeping in.
func TestOnlyTheDeltaFieldsThatNameTasksAreResolved(t *testing.T) {
	t.Parallel()
	named := make(map[string]bool, len(counterpartyDeltaFields))
	for _, field := range counterpartyDeltaFields {
		named[field] = true
	}
	for _, kind := range RelationKinds {
		want := kind != RelationPage && kind != RelationEntry
		if named[string(kind)] != want {
			t.Errorf("the key map %s the %s edges; a relation names a task "+
				"unless it is a %s or an %s, and the field list is derived from "+
				"RelationKinds so that a kind added later needs no second edit",
				map[bool]string{true: "resolves", false: "does not resolve"}[named[string(kind)]],
				kind, RelationPage, RelationEntry)
		}
	}
	// And the four beyond the edges: the blocker's mirror, a person's queue
	// (task ids on the same page), and the scalars `parent` and
	// `removed_with`, which [TaskDeltas] records by ID like every relation;
	// left out, a reparent and a cascade removal would render a uuid.
	for _, field := range []string{
		"blocking", "priorities", "parent", "removed_with",
	} {
		if !named[field] {
			t.Errorf("the key map does not resolve %q, so that column renders "+
				"uuids", field)
		}
	}
	if len(counterpartyDeltaFields) != len(named) {
		t.Errorf("the field list carries a duplicate: %v", counterpartyDeltaFields)
	}
}

// A document that did not move records nothing, as nil: the one spelling of
// "nothing moved" the column takes ([deltaSet.done]).
func TestAnUnmovedDocumentRecordsNothing(t *testing.T) {
	t.Parallel()
	project := Project{Key: "ENG", Name: "Engineering", Purpose: "builds it"}
	view := View{ID: "v-1", Name: "My work", Type: ViewList}
	person := Person{Handle: "ana", Priorities: []string{"t-1"}}
	tags := TagSet{Project: "ENG", Tags: []Tag{{Slug: "api"}}, TagsVersion: 3}
	types := TypeCatalogue{Types: []TaskType{{Slug: "incident"}}}
	fields := FieldCatalogue{Fields: []FieldDef{{Slug: "severity"}}, PolicyVersion: 2}

	for name, got := range map[string]map[string]Delta{
		"project":         projectDeltas(project, project),
		"view":            viewDeltas(view, view),
		"person":          personDeltas(person, person),
		"tags":            tagSetDeltas(tags, tags),
		"type catalogue":  typeCatalogueDeltas(types, types),
		"field catalogue": fieldCatalogueDeltas(fields, fields),
	} {
		if got != nil {
			t.Errorf("an unchanged %s recorded %v, and a non-nil empty map "+
				"is the second spelling of empty this column must not hold",
				name, got)
		}
	}
	// And a real move is recorded, so the loop above cannot pass because
	// these functions answer nothing at all.
	moved := project
	moved.Purpose = "ships it"
	if got := projectDeltas(project, moved)["purpose"]; !sameDelta(got,
		Delta{From: "builds it", To: "ships it"}) {

		t.Errorf("a purpose edit recorded %+v", got)
	}
}

// A target-only edit records what moved, or a lead's edit setting nothing else
// would write a `project_updated` row with no delta ([projectDeltas]).
func TestATargetDateEditRecordsItsDelta(t *testing.T) {
	t.Parallel()
	before := Project{Key: "ENG", Name: "Engineering", TargetDate: "2026-12-18"}
	after := before
	after.TargetDate = "2027-01-29"
	if got := projectDeltas(before, after); len(got) != 1 ||
		!sameDelta(got["target_date"], Delta{From: "2026-12-18", To: "2027-01-29"}) {

		t.Errorf("a target edit recorded %+v, want exactly the target_date move", got)
	}
}

// A people set is a set, so its delta is about membership, not order: the
// three handle collections are assembled ([settleWatch] removes and appends; a
// caller may state a set in any order), so recording document order would
// write changes nobody made, as for the edge sets.
func TestThePeopleSetsRecordMembershipRatherThanOrder(t *testing.T) {
	t.Parallel()
	one := Task{
		Watchers:      []string{"bo", "ana"},
		Muted:         []string{"cy", "bo"},
		Collaborators: []string{"di", "ana"},
	}
	other := Task{
		Watchers:      []string{"ana", "bo"},
		Muted:         []string{"bo", "cy"},
		Collaborators: []string{"ana", "di"},
	}
	if moved := TaskDeltas(one, other, nil); moved != nil {
		t.Errorf("re-stating three sets in another order recorded %v, and "+
			"nothing about the task changed", moved)
	}
	// And a real membership change is recorded, or the assertion above
	// would pass for a comparison that answers nothing.
	grew := other
	grew.Watchers = []string{"ana", "bo", "zed"}
	grew.Collaborators = nil
	moved := TaskDeltas(other, grew, nil)
	if got := moved["watchers"]; !sameDelta(got, moves([]string{"zed"}, nil)) {
		t.Errorf("adding a watcher recorded %+v", got)
	}
	if got := moved["collaborators"]; !sameDelta(got, moves(nil, []string{"ana", "di"})) {
		t.Errorf("clearing the collaborators recorded %+v, want both removed", got)
	}
}

// A mute is its own field rather than a subtraction from the watchers:
// [Wake.snapshot] subtracts for a routing list, but a delta records what the
// document holds, and folding them would make "was never watching" and "chose
// to stop" the same row.
func TestAMuteIsRecordedApartFromTheWatcherSet(t *testing.T) {
	t.Parallel()
	// What [settleWatch] leaves behind for an unwatch: out of the set AND
	// into the muted list, in one patch.
	before := Task{Watchers: []string{"ana", "bo"}}
	after := Task{Watchers: []string{"bo"}, Muted: []string{"ana"}}
	moved := TaskDeltas(before, after, nil)
	if got := moved["watchers"]; !sameDelta(got, moves(nil, []string{"ana"})) {
		t.Errorf("the watcher set recorded %+v", got)
	}
	if got := moved["muted"]; !sameDelta(got, moves([]string{"ana"}, nil)) {
		t.Errorf("the mute recorded %+v — a watcher row that did not say who "+
			"opted out is the row this field exists to complete", got)
	}
	// And a mute with the watch intact: only `muted` moves, so a delta that
	// folded the two would record nothing.
	muted := Task{Watchers: []string{"ana", "bo"}, Muted: []string{"ana"}}
	if moved := TaskDeltas(before, muted, nil); len(moved) != 1 {
		t.Errorf("muting a watcher who stays on the list recorded %v, and it "+
			"is exactly one change", moved)
	}
}

// A body delta is a size, never the prose: [MaxBody] is 32 KiB,
// `tracker_history` is never swept, and the row's mutation holds the text.
func TestABodyDeltaCarriesASizeAndNotTheText(t *testing.T) {
	t.Parallel()
	prose := "the quick brown fox"
	written := TaskDeltas(Task{}, Task{Body: prose}, nil)["body"]
	if strings.Contains(written.To, "fox") {
		t.Fatalf("the body delta carried the prose: %+v", written)
	}
	if !sameDelta(written, Delta{From: "", To: "19 bytes"}) {
		t.Errorf("writing a body recorded %+v — the empty `from` is what a "+
			"renderer draws as an em dash for a task that had none", written)
	}
	cleared := TaskDeltas(Task{Body: prose}, Task{}, nil)["body"]
	if !sameDelta(cleared, Delta{From: "19 bytes", To: ""}) {
		t.Errorf("clearing a body recorded %+v", cleared)
	}
	// And a rewrite of the same length is still a change ([deltaSet.mark]):
	// the two sizes read the same and the key's presence says it moved.
	// Compared on the text, as `add` does, a typo fix would record nothing.
	same := TaskDeltas(Task{Body: prose}, Task{Body: "the quick brown cat"}, nil)
	if got, held := same["body"]; !held || !sameDelta(got, Delta{From: "19 bytes", To: "19 bytes"}) {
		t.Errorf("an edit that kept the length recorded %+v (held=%v)", got, held)
	}
	// An unchanged body is not a change, or every commit would carry one.
	if moved := TaskDeltas(Task{Body: prose}, Task{Body: prose}, nil); moved != nil {
		t.Errorf("an untouched body recorded %v", moved)
	}
}

// A checklist delta is counts per named list: [MaxChecklists] lists holding
// [MaxChecklistItemsTotal] items cannot ride a log line.
func TestAChecklistDeltaCountsItemsPerNamedList(t *testing.T) {
	t.Parallel()
	before := Task{Checklists: []Checklist{{
		ID: "c-1", Name: "Setup", Items: []ChecklistItem{
			{ID: "i-1", Name: "clone"}, {ID: "i-2", Name: "build"},
			{ID: "i-3", Name: "ship"},
		},
	}}}
	done := before
	done.Checklists = []Checklist{{
		ID: "c-1", Name: "Setup", Items: []ChecklistItem{
			{ID: "i-1", Name: "clone", Done: true}, {ID: "i-2", Name: "build"},
			{ID: "i-3", Name: "ship"},
		},
	}}
	got := TaskDeltas(before, done, nil)["checklists"]
	if !sameDelta(got, Delta{From: "Setup: 0 of 3 done", To: "Setup: 1 of 3 done"}) {
		t.Errorf("checking an item off recorded %+v", got)
	}
	if strings.Contains(got.To, "clone") {
		t.Errorf("the checklist delta carried an item's text: %+v", got)
	}

	// A second list appears on one side only, so an addition and a removal
	// read differently.
	added := done
	added.Checklists = []Checklist{
		done.Checklists[0],
		{ID: "c-2", Name: "Rollout", Items: []ChecklistItem{{ID: "i-4"}}},
	}
	if got := TaskDeltas(done, added, nil)["checklists"]; !sameDelta(got,
		Delta{From: "Setup: 1 of 3 done", To: "Setup: 1 of 3 done, Rollout: 0 of 1 done"}) {

		t.Errorf("adding a list recorded %+v", got)
	}

	// A list nobody named is still attachable to something.
	nameless := Task{Checklists: []Checklist{{ID: "c-7"}}}
	if got := TaskDeltas(Task{}, nameless, nil)["checklists"]; got.To != "c-7: 0 of 0 done" {
		t.Errorf("a nameless list recorded %+v", got)
	}
}

// A custom field is named by its slug, which takes the catalogue: values are
// keyed by field ID, a uuid, so the declarations turn the key back into its
// slug and a choice's option id into the option. Without them the comparison
// is skipped, since `0f3c…=3 → 0f3c…=5` is worse than no delta.
func TestACustomFieldDeltaIsNamedBySlugOrNotAtAll(t *testing.T) {
	t.Parallel()
	declared := map[string]FieldDef{
		"f-sev": {ID: "f-sev", Slug: "severity", Type: FieldText},
		"f-env": {ID: "f-env", Slug: "environment", Type: FieldDropdown, Config: FieldConfig{
			Options: []Option{
				{ID: "o-1", Slug: "staging"}, {ID: "o-2", Slug: "production"},
			},
		}},
		"f-eta": {ID: "f-eta", Slug: "eta_days", Type: FieldNumber},
	}
	before := Task{Fields: map[string]json.RawMessage{
		"f-sev": json.RawMessage(`"low"`),
		"f-env": json.RawMessage(`"o-1"`),
		"f-eta": json.RawMessage(`3`),
	}}
	after := Task{Fields: map[string]json.RawMessage{
		"f-sev": json.RawMessage(`"high"`),
		"f-env": json.RawMessage(`"o-2"`),
		"f-eta": json.RawMessage(`3`),
	}}
	got := TaskDeltas(before, after, declared)["fields"]
	// Ordered by slug, both sides; `eta_days` did not move so it is on
	// neither ([paramsText]'s rule).
	if !sameDelta(got, Delta{
		From: "environment=staging, severity=low",
		To:   "environment=production, severity=high",
	}) {
		t.Errorf("two moved fields recorded %+v", got)
	}

	// The writer's frame holds no catalogue, so it records no `fields`
	// rather than a pair of uuids.
	if _, held := TaskDeltas(before, after, nil)["fields"]; held {
		t.Error("a comparison with no catalogue named a field by its id")
	}

	// A field nobody declares is counted: it has no slug, and dropping it
	// would make a commit that moved only it read as moving nothing.
	foreign := Task{Fields: map[string]json.RawMessage{
		"f-sev": json.RawMessage(`"low"`),
		"f-env": json.RawMessage(`"o-1"`),
		"f-eta": json.RawMessage(`3`),
		"f-???": json.RawMessage(`"anything"`),
	}}
	if got := TaskDeltas(before, foreign, declared)["fields"]; !sameDelta(got,
		Delta{From: "", To: "1 undeclared"}) {

		t.Errorf("a value for an undeclared field recorded %+v", got)
	}
	// And nothing moving records nothing, or every commit on a task with
	// custom values would carry an empty pair.
	if moved := TaskDeltas(before, before, declared); moved != nil {
		t.Errorf("an untouched field map recorded %v", moved)
	}
}

// A multi-valued field keeps its members apart from its neighbours: ", "
// separates one field from the next, so a labels field's two options joined by
// it would read as two fields, one nameless.
func TestAMultiValuedFieldSeparatesItsOwnMembers(t *testing.T) {
	t.Parallel()
	declared := map[string]FieldDef{
		"f-lab": {ID: "f-lab", Slug: "labels", Type: FieldLabels, Config: FieldConfig{
			Options: []Option{{ID: "o-1", Slug: "api"}, {ID: "o-2", Slug: "ui"}},
		}},
	}
	after := Task{Fields: map[string]json.RawMessage{
		"f-lab": json.RawMessage(`["o-1","o-2"]`),
	}}
	if got := TaskDeltas(Task{}, after, declared)["fields"]; !sameDelta(got,
		Delta{From: "", To: "labels=api/ui"}) {

		t.Errorf("a two-option labels field recorded %+v", got)
	}
}

// The four scalars record their moves, and the two ids stay ids: `parent` and
// `removed_with` name another task's row, and a key there would differ on a
// node that had not applied that task, in a table nothing repairs. The
// activity read resolves them.
func TestTheScalarTaskFieldsRecordWhatMoved(t *testing.T) {
	t.Parallel()
	oldParent, newParent, root := "t-1", "t-2", "t-root"
	before := Task{
		Reporter: "ana", RoutingUnit: "Engineering", Parent: &oldParent,
	}
	after := Task{
		Reporter: "bo", RoutingUnit: "Platform", Parent: &newParent,
		Archived: true,
		Removed:  &Tombstone{By: "bo", RemovedWith: &root},
	}
	moved := TaskDeltas(before, after, nil)
	for field, want := range map[string]Delta{
		"reporter":     {From: "ana", To: "bo"},
		"routing_unit": {From: "Engineering", To: "Platform"},
		"parent":       {From: "t-1", To: "t-2"},
		// Both states present for a flag ([boolText]).
		"archived": {From: "false", To: "true"},
		// The one thing a tombstone holds that the row's own actor,
		// actor_kind and created_at do not.
		"removed_with": {From: "", To: "t-root"},
	} {
		if got := moved[field]; !sameDelta(got, want) {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
	// Losing a parent is a move like any other.
	orphaned := TaskDeltas(Task{Parent: &oldParent}, Task{}, nil)["parent"]
	if !sameDelta(orphaned, Delta{From: "t-1", To: ""}) {
		t.Errorf("clearing a parent recorded %+v", orphaned)
	}
	// And an ordinary removal records no `removed_with`, which answers
	// "was this a cascade".
	alone := TaskDeltas(Task{}, Task{Removed: &Tombstone{By: "bo"}}, nil)
	if _, held := alone["removed_with"]; held {
		t.Errorf("a removal that took nothing with it recorded %v", alone)
	}
}

// What a card carries and what a history row carries differ in exactly one
// field: only the applier holds the project's catalogue, so a custom-field move
// is history-only. [TaskDeltas] answers both frames, so holding any other field
// back from the card would reintroduce the disagreement it prevents.
func TestACardCarriesEveryDeltaButTheCatalogueOne(t *testing.T) {
	t.Parallel()
	declared := map[string]FieldDef{
		"f-sev": {ID: "f-sev", Slug: "severity", Type: FieldText},
	}
	before := Task{
		Key: "ENG-1", Watchers: []string{"ana"},
		Fields: map[string]json.RawMessage{"f-sev": json.RawMessage(`"low"`)},
	}
	after := Task{
		Key: "ENG-1", Watchers: []string{"ana", "bo"}, Archived: true,
		Fields: map[string]json.RawMessage{"f-sev": json.RawMessage(`"high"`)},
	}

	card := Wake{Kind: ChangeWatchers, Before: before, After: after}.Notify(nil)
	if got := card.Fields["watchers"]; !sameDelta(got, moves([]string{"bo"}, nil)) {
		t.Errorf("a watcher change reached the card as %+v — a card that "+
			"named no watcher is the row this field exists to complete", got)
	}
	if got := card.Fields["archived"]; !sameDelta(got, Delta{From: "false", To: "true"}) {
		t.Errorf("the archive flag reached the card as %+v", got)
	}
	if got, held := card.Fields["fields"]; held {
		t.Errorf("the card carried %+v for a custom field — the writer holds "+
			"no catalogue, so it could only have named it by uuid", got)
	}

	// And the applier's answer is the same set plus that one field: a
	// superset of the card, not a second opinion.
	row := TaskDeltas(before, after, declared)
	for name, got := range card.Fields {
		if !sameDelta(row[name], got) {
			t.Errorf("the row records %+v for %s and the card %+v — one "+
				"function answers both, so they cannot differ", row[name], name, got)
		}
	}
	if got := row["fields"]; !sameDelta(got, Delta{From: "severity=low", To: "severity=high"}) {
		t.Errorf("the history row recorded %+v for the custom field", got)
	}
	if len(row) != len(card.Fields)+1 {
		t.Errorf("the row records %v and the card %v — the catalogue field is "+
			"the only difference there is meant to be", row, card.Fields)
	}
}

// Making a due date all-day is a change the instant cannot show: an all-day
// date is stored as the company's midnight, so turning a midnight due date
// all-day moves only the flag, and comparing `due` alone records nothing.
func TestMakingADueDateAllDayIsRecorded(t *testing.T) {
	t.Parallel()
	midnight := time.Date(2031, 4, 16, 0, 0, 0, 0, time.UTC)
	timed := Task{DueAt: &midnight}
	allDay := Task{DueAt: &midnight, DueAllDay: true}

	moved := TaskDeltas(timed, allDay, nil)
	if got := moved["due_all_day"]; !sameDelta(got, Delta{From: "false", To: "true"}) {
		t.Errorf("making a due date all-day recorded %+v", got)
	}
	if _, held := moved["due"]; held {
		t.Error("the instant did not move and is in the deltas")
	}
	// And no all-day date on either side records nothing, or every commit
	// would carry the flag.
	if moved := TaskDeltas(timed, timed, nil); moved != nil {
		t.Errorf("an untouched schedule recorded %v", moved)
	}
}

// sameDelta compares two deltas, a set's moves included.
func sameDelta(a, b Delta) bool {
	return a.From == b.From && a.To == b.To &&
		slices.Equal(a.Added, b.Added) && slices.Equal(a.Removed, b.Removed)
}

// moves is a set's delta: what it gained and what it lost.
func moves(added, removed []string) Delta { return Delta{Added: added, Removed: removed} }

// A card shows at most MaxDeltas fields and says how many it left off, and the
// history row is never trimmed.
func TestACardTrimsToTheDisplayLimitAndCountsTheRest(t *testing.T) {
	t.Parallel()
	all := deltaSet{}
	for i := range MaxDeltas + 3 {
		all.add(fmt.Sprintf("f%02d", i), "", "x")
	}
	moved := all.done()
	if len(moved) != MaxDeltas+3 {
		t.Fatalf("the row's delta set holds %d fields, want all %d", len(moved), MaxDeltas+3)
	}
	shown, omitted := cardFields(moved)
	if len(shown) != MaxDeltas || omitted != 3 {
		t.Errorf("the card shows %d and omits %d, want %d and 3", len(shown), omitted, MaxDeltas)
	}
	if _, held := shown["f00"]; !held {
		t.Error("the card did not keep the first fields by name")
	}
	if _, held := shown[fmt.Sprintf("f%02d", MaxDeltas+2)]; held {
		t.Error("the card kept a field past the limit")
	}
	if err := (&Notify{Kind: ChangeFields, Fields: shown, FieldsOmitted: omitted}).Validate(); err != nil {
		t.Errorf("a trimmed card was refused: %v", err)
	}
	if err := (&Notify{Kind: ChangeFields, FieldsOmitted: -1}).Validate(); err == nil {
		t.Error("a negative omitted count was accepted")
	}
	if got := changedText(shown, omitted); !strings.HasSuffix(got, "- and 3 more field(s) this card had no room for") {
		t.Errorf("the wake prompt does not count what the card left off:\n%s", got)
	}
}

// THE WAKE PROMPT NAMES A SET'S MOVES, so a seat reads what joined and what
// left rather than diffing two whole lists itself.
func TestTheWakePromptNamesWhatASetGainedAndLost(t *testing.T) {
	t.Parallel()
	got := changedText(map[string]Delta{
		"watchers": moves([]string{"ana", "bo"}, []string{"cy"}),
		"status":   {From: "todo", To: "done"},
	}, 0)
	want := "- status: todo → done\n- watchers: added ana, bo; removed cy"
	if got != want {
		t.Errorf("changedText =\n%s\nwant\n%s", got, want)
	}
}
