package tracker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The comparisons, exercised as VALUES.
//
// IN-PACKAGE, and not only because the document comparisons are unexported:
// they are pure over two documents, which is the property `deltas.go` claims
// for them, and a rule only reachable through a database is a rule nobody
// re-measures. The applier's own wiring — that each kind reaches the right
// comparison against the right stored row — is `historydeltas_test.go`.

// AN EDGE SET IS A SET, so its delta is about membership and not about order.
//
// [RelationIntent.resolve] appends what a gesture adds, so two writers adding
// the same two edges in opposite orders leave the same set in two orders. A
// delta joined in document order would then record a change on the second
// write that nothing made — in a table that is part of the state log's
// identity claim and that nothing ever rewrites.
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
	// AND A REAL MEMBERSHIP CHANGE IS RECORDED, or the assertion above
	// would pass for a comparison that answers nothing at all.
	grew := Task{Relations: append([]Relation{{Kind: RelationWaitingOn, Other: "c"}},
		other.Relations...)}
	if got := TaskDeltas(other, grew, nil)["waiting_on"]; got != (Delta{From: "a, b", To: "a, b, c"}) {
		t.Errorf("adding an edge recorded %+v", got)
	}
}

// EVERY RELATION KIND IS ITS OWN FIELD, derived from [RelationKinds].
//
// The kinds are unrelated facts — `waiting_on` is work somebody has to clear
// and `page` is a link to a wiki page — so one field holding all of them would
// make adding a link read as a change to what the task is waiting for. The
// loop over the declared set is also what makes a fifth kind recorded with no
// second edit, which is the reason that slice exists.
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
		if got.To != "other-"+string(kind) {
			t.Errorf("the %s field recorded %+v", kind, got)
		}
	}
	// AND THE MIRROR IS ITS OWN FIELD, under the word this package uses
	// for that direction everywhere else. It is not a fifth relation kind
	// because it is not an authored edge: it is the copy a blocker carries
	// so a close can name who it unblocks.
	mirrored := TaskDeltas(Task{}, Task{Dependents: []string{"dep-2", "dep-1"}}, nil)
	if got := mirrored["blocking"]; got != (Delta{From: "", To: "dep-1, dep-2"}) {
		t.Errorf("the mirror recorded %+v", got)
	}
}

// A DELTA SIDE IS A LOG LINE, so a collection past the budget is cut at a
// WHOLE MEMBER and says how many it dropped.
//
// `tracker_history` is never swept: every byte written here is kept for the
// life of the company on every node. A task may wait on [MaxWaitingOn] tasks
// and carry [MaxOtherRelations] other edges, each named by a uuid, which is
// several kilobytes on each side of one field.
func TestADeltaValueIsBoundedToOneLogLine(t *testing.T) {
	t.Parallel()
	members := make([]string, 0, MaxWaitingOn)
	for i := range MaxWaitingOn {
		members = append(members, strings.Repeat("x", 36)+string(rune('a'+i%26)))
	}
	got := listText(members)
	if len(got) > MaxDeltaValue {
		t.Fatalf("a %d-member collection rendered %d bytes against a %d "+
			"budget", len(members), len(got), MaxDeltaValue)
	}
	if !strings.HasSuffix(got, " more") {
		t.Fatalf("a collection that did not fit was cut silently: %q — a list "+
			"cut without a count reads as the whole set", got)
	}
	// AND EVERY MEMBER IT KEPT IS WHOLE, which is the other half: a list
	// cut inside a member reads as a member.
	for _, part := range strings.Split(strings.TrimSuffix(got, " more"), ", ") {
		if strings.HasPrefix(part, "+") {
			continue
		}
		if len(part) != 37 {
			t.Fatalf("the cut left %q, which is not one of the members it was "+
				"given", part)
		}
	}
	// A COLLECTION THAT FITS IS WHOLE AND CARRIES NO TAIL, or the bound
	// would be rewriting every ordinary row it touches.
	if got := listText([]string{"a", "b", "c"}); got != "a, b, c" {
		t.Errorf("a collection well inside the budget rendered %q", got)
	}
	// AND ONE MEMBER TOO LARGE FOR ITS OWN SHARE IS CUT AND MARKED rather
	// than dropped: a delta side that was only a count would be a number
	// where every reader expects a list.
	huge := listText([]string{strings.Repeat("y", 4*MaxDeltaValue), "b", "c"})
	if len(huge) > MaxDeltaValue {
		t.Fatalf("an oversized member rendered %d bytes against a %d budget",
			len(huge), MaxDeltaValue)
	}
	if !strings.HasPrefix(huge, strings.Repeat("y", MaxDeltaElement-len("…"))+"…") {
		t.Errorf("an oversized member was not cut and marked: %q", huge)
	}
	if !strings.HasSuffix(huge, ", b, c") && !strings.HasSuffix(huge, " more") {
		t.Errorf("an oversized member left %q, which neither kept its "+
			"neighbours nor counted them", huge)
	}
}

// A SAVED VIEW'S PARAMETER DELTA NAMES WHAT MOVED AND NOTHING ELSE.
//
// Three edits are three different pictures — a key that arrived shows on the
// `to` side alone, one that left on the `from` side alone, and one re-pointed
// on both — and an unchanged parameter appears on neither, because a saved
// query is bounded at [MaxViewParamsBytes] and carrying it whole twice would
// put the whole predicate in a log line to show that one filter moved.
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
	// AND A SAVE THAT CHANGED NO PARAMETER RECORDS NO PARAMETER FIELD.
	if _, recorded := viewDeltas(before, before)["params"]; recorded {
		t.Error("a re-save with the same query recorded a parameter move")
	}
}

// A PERSON'S INBOX IS COUNTED RATHER THAN LISTED, and their queue is not.
//
// Each inbox list holds up to [MaxInboxEntries] entries, and every entry is a
// record id and a log position — neither of which anybody reads. What a reader
// of the row wants to know is that an item was marked read, which the counts
// say because every gesture that writes these lists MOVES an entry between
// them. The priority list is the opposite: the order IS the instruction, so it
// is recorded as it is stored.
func TestAPersonsInboxIsCountedAndTheirQueueIsOrdered(t *testing.T) {
	t.Parallel()
	before := Person{
		Unread:     []InboxEntry{{RecordID: "r-1"}, {RecordID: "r-2"}},
		Priorities: []string{"t-1", "t-2"},
	}
	after := Person{
		Unread:     []InboxEntry{{RecordID: "r-2"}},
		Read:       []InboxEntry{{RecordID: "r-1"}},
		Priorities: []string{"t-2", "t-1"},
	}
	moved := personDeltas(before, after)
	if got := moved["unread"]; got != (Delta{From: "2", To: "1"}) {
		t.Errorf("the unread count recorded %+v", got)
	}
	if got := moved["read"]; got != (Delta{From: "0", To: "1"}) {
		t.Errorf("the read count recorded %+v — a zero is a value here, not "+
			"an absence", got)
	}
	if got := moved["priorities"]; got != (Delta{From: "t-1, t-2", To: "t-2, t-1"}) {
		t.Errorf("the queue recorded %+v, and a re-ordering with the same "+
			"members is exactly the change somebody made", got)
	}
	// WHEN IS NOT RECORDED, ONLY WHO. The row's own instant already says
	// when, and a stamp that moves on every write would make a lead
	// re-stating an unchanged list look like a change.
	restated := after
	restated.PrioritiesSetAt = restated.PrioritiesSetAt.AddDate(0, 0, 1)
	if moved := personDeltas(after, restated); moved != nil {
		t.Errorf("re-stating an unchanged queue recorded %v", moved)
	}
}

// ONLY THE DELTA FIELDS WHOSE MEMBERS ARE TASK IDS ARE RESOLVED.
//
// [counterpartyDeltaFields] is what an activity answer's key map is built
// from, and it is DERIVED from [RelationKinds] so a fifth kind of edge is
// resolved with no second edit. [RelationPage] is the one exclusion, and it is
// a fact about the type rather than about any row: a `page` edge names a
// knowledge-base page, so asking `tracker_tasks` about one is asking whether a
// page is a task.
//
// A DECLARATION HELD AGAINST THE RULE, in the `internal/solo` roster
// tradition, because the runtime cannot show it: a page id resolves to nothing
// whether or not it is excluded, so a round-trip case built on one passes with
// the exclusion gone. This fails in BOTH directions — a kind that stopped
// being derived, and `page` creeping in.
func TestOnlyTheDeltaFieldsThatNameTasksAreResolved(t *testing.T) {
	t.Parallel()
	named := make(map[string]bool, len(counterpartyDeltaFields))
	for _, field := range counterpartyDeltaFields {
		named[field] = true
	}
	for _, kind := range RelationKinds {
		want := kind != RelationPage
		if named[string(kind)] != want {
			t.Errorf("the key map %s the %s edges; a relation names a task "+
				"unless it is a %s, and the field list is derived from "+
				"RelationKinds so that a kind added later needs no second edit",
				map[bool]string{true: "resolves", false: "does not resolve"}[named[string(kind)]],
				kind, RelationPage)
		}
	}
	// AND THE FOUR BEYOND THE EDGES: the mirror a blocker carries, a
	// person's own queue — an ordered list of task ids that renders on the
	// same page — the parent a reparent moves a task between, and the root a
	// cascade removed it with. The last two are scalars and are resolved all
	// the same: [TaskDeltas] records them by ID like every relation, so
	// leaving them out is what makes a reparent and a cascade removal the
	// two rows on an item's History tab still rendering a uuid.
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

// A DOCUMENT THAT DID NOT MOVE RECORDS NOTHING, AS NIL.
//
// [Applier.writeHistory] reads the length of what it is handed and falls
// through to the notification when it is empty; a NIL map is what keeps the
// column to one spelling of "nothing moved", because `jsonOf` is
// `json.Marshal` and a nil map marshals to the literal `null` where every
// other row stores `{}`.
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
	// AND A REAL MOVE IS RECORDED, so the loop above cannot be passing
	// because these functions answer nothing at all.
	moved := project
	moved.Purpose = "ships it"
	if got := projectDeltas(project, moved)["purpose"]; got !=
		(Delta{From: "builds it", To: "ships it"}) {

		t.Errorf("a purpose edit recorded %+v", got)
	}
}

// A TARGET-ONLY EDIT RECORDS WHAT MOVED.
//
// The target date is a lead-owned policy facet like the default assignee, and
// a lead's edit that set nothing else would otherwise write a
// `project_updated` history row with no delta at all — the empty row
// [projectDeltas] exists to end.
func TestATargetDateEditRecordsItsDelta(t *testing.T) {
	t.Parallel()
	before := Project{Key: "ENG", Name: "Engineering", TargetDate: "2026-12-18"}
	after := before
	after.TargetDate = "2027-01-29"
	if got := projectDeltas(before, after); len(got) != 1 ||
		got["target_date"] != (Delta{From: "2026-12-18", To: "2027-01-29"}) {

		t.Errorf("a target edit recorded %+v, want exactly the target_date move", got)
	}
}

// A PEOPLE SET IS A SET, so its delta is about membership and not about order.
//
// The three handle collections on a task are assembled rather than arranged:
// [settleWatch] rebuilds the watcher list by removing a handle and appending
// it, and a caller may state a whole set it read in any order. Recorded in
// document order, every re-statement that moved nobody would write a change
// into a table nothing rewrites — which is the same failure the edge sets are
// sorted to avoid, one collection along.
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
	// AND A REAL MEMBERSHIP CHANGE IS RECORDED, or the assertion above
	// would pass for a comparison that answers nothing at all.
	grew := other
	grew.Watchers = []string{"ana", "bo", "zed"}
	grew.Collaborators = nil
	moved := TaskDeltas(other, grew, nil)
	if got := moved["watchers"]; got != (Delta{From: "ana, bo", To: "ana, bo, zed"}) {
		t.Errorf("adding a watcher recorded %+v", got)
	}
	if got := moved["collaborators"]; got != (Delta{From: "ana, di", To: ""}) {
		t.Errorf("clearing the collaborators recorded %+v — an emptied set is "+
			"the empty string, which every renderer draws as an em dash", got)
	}
}

// A MUTE IS ITS OWN FIELD RATHER THAN A SUBTRACTION FROM THE WATCHERS.
//
// [Wake.snapshot] subtracts the muted from the watchers because it is building
// a ROUTING list and the feed must never have to remember to. A DELTA records
// what the document holds, and folding the two here would make "was never
// watching" and "chose to stop" the same row — the one distinction those two
// fields exist to keep, and the one an unwatch is entirely about.
func TestAMuteIsRecordedApartFromTheWatcherSet(t *testing.T) {
	t.Parallel()
	// What [settleWatch] leaves behind for an unwatch: out of the set AND
	// into the muted list, in one patch.
	before := Task{Watchers: []string{"ana", "bo"}}
	after := Task{Watchers: []string{"bo"}, Muted: []string{"ana"}}
	moved := TaskDeltas(before, after, nil)
	if got := moved["watchers"]; got != (Delta{From: "ana, bo", To: "bo"}) {
		t.Errorf("the watcher set recorded %+v", got)
	}
	if got := moved["muted"]; got != (Delta{From: "", To: "ana"}) {
		t.Errorf("the mute recorded %+v — a watcher row that did not say who "+
			"opted out is the row this field exists to complete", got)
	}
	// AND A MUTE WITH THE WATCH INTACT IS THE OTHER HALF: somebody still
	// on the list who has asked not to hear. Only `muted` moves, so a
	// delta that had folded the two would record nothing at all.
	muted := Task{Watchers: []string{"ana", "bo"}, Muted: []string{"ana"}}
	if moved := TaskDeltas(before, muted, nil); len(moved) != 1 {
		t.Errorf("muting a watcher who stays on the list recorded %v, and it "+
			"is exactly one change", moved)
	}
}

// A BODY DELTA IS A MARKER AND NEVER THE PROSE.
//
// [MaxBody] is 32 KiB and `tracker_history` is never swept, so carrying both
// sides would put 64 KiB on one log line on every node for the life of the
// company — while the mutation the row already stores holds the text for
// anybody who needs it.
func TestABodyDeltaCarriesASizeAndNotTheText(t *testing.T) {
	t.Parallel()
	prose := "the quick brown fox"
	written := TaskDeltas(Task{}, Task{Body: prose}, nil)["body"]
	if strings.Contains(written.To, "fox") {
		t.Fatalf("the body delta carried the prose: %+v", written)
	}
	if written != (Delta{From: "", To: "19 bytes"}) {
		t.Errorf("writing a body recorded %+v — the empty `from` is what a "+
			"renderer draws as an em dash for a task that had none", written)
	}
	cleared := TaskDeltas(Task{Body: prose}, Task{}, nil)["body"]
	if cleared != (Delta{From: "19 bytes", To: ""}) {
		t.Errorf("clearing a body recorded %+v", cleared)
	}
	// AND A REWRITE OF THE SAME LENGTH IS STILL A CHANGE, which is the
	// whole reason [deltaSet.mark] exists: the two markers read the same,
	// and the KEY's presence is what says the field moved. Compared on the
	// text, as `add` does, a typo fix would have recorded nothing.
	same := TaskDeltas(Task{Body: prose}, Task{Body: "the quick brown cat"}, nil)
	if got, held := same["body"]; !held || got != (Delta{From: "19 bytes", To: "19 bytes"}) {
		t.Errorf("an edit that kept the length recorded %+v (held=%v)", got, held)
	}
	// An unchanged body is not a change, or every commit would carry one.
	if moved := TaskDeltas(Task{Body: prose}, Task{Body: prose}, nil); moved != nil {
		t.Errorf("an untouched body recorded %v", moved)
	}
}

// A CHECKLIST DELTA IS COUNTS PER NAMED LIST, and a promotion is its own
// number.
//
// A task carries [MaxChecklists] lists holding [MaxChecklistItemsTotal] items
// between them, so the items themselves cannot ride a log line. What makes the
// counts sufficient rather than merely cheap is the promotion column:
// promoting an item to a subtask is a `checklist` commit that moves neither
// the done count nor the total, so counts alone would have left the one
// checklist gesture this build has recording nothing at all.
func TestAChecklistDeltaCountsItemsPerNamedList(t *testing.T) {
	t.Parallel()
	subtask := "t-9"
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
	if got != (Delta{From: "Setup: 0 of 3 done", To: "Setup: 1 of 3 done"}) {
		t.Errorf("checking an item off recorded %+v", got)
	}
	if strings.Contains(got.To, "clone") {
		t.Errorf("the checklist delta carried an item's text: %+v", got)
	}

	// A SECOND LIST IS NAMED BY BEING THERE on one side only, which is how
	// an addition and a removal read as two different pictures.
	added := done
	added.Checklists = []Checklist{
		done.Checklists[0],
		{ID: "c-2", Name: "Rollout", Items: []ChecklistItem{{ID: "i-4"}}},
	}
	if got := TaskDeltas(done, added, nil)["checklists"]; got !=
		(Delta{From: "Setup: 1 of 3 done", To: "Setup: 1 of 3 done, Rollout: 0 of 1 done"}) {

		t.Errorf("adding a list recorded %+v", got)
	}

	// AND A PROMOTION, which moves neither count.
	promoted := done
	promoted.Checklists = []Checklist{{
		ID: "c-1", Name: "Setup", Items: []ChecklistItem{
			{ID: "i-1", Name: "clone", Done: true},
			{ID: "i-2", Name: "build", PromotedTo: &subtask},
			{ID: "i-3", Name: "ship"},
		},
	}}
	if got := TaskDeltas(done, promoted, nil)["checklists"]; got !=
		(Delta{From: "Setup: 1 of 3 done", To: "Setup: 1 of 3 done (1 promoted)"}) {

		t.Errorf("promoting an item recorded %+v, and the one checklist "+
			"gesture this build has must not write an empty row", got)
	}
	// A list nobody named is still attachable to something.
	nameless := Task{Checklists: []Checklist{{ID: "c-7"}}}
	if got := TaskDeltas(Task{}, nameless, nil)["checklists"]; got.To != "c-7: 0 of 0 done" {
		t.Errorf("a nameless list recorded %+v", got)
	}
}

// A CUSTOM FIELD IS NAMED BY ITS SLUG, WHICH TAKES THE CATALOGUE.
//
// Values are keyed by field ID precisely so a rename never re-points one, and
// an id is a uuid — so the declarations in hand are what turn the key back
// into the word somebody typed, and a choice's stored option id back into the
// option they picked. Without them the comparison is SKIPPED rather than
// keyed by uuid, because `0f3c…=3 → 0f3c…=5` is worse on every surface than
// no delta at all.
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
	// ORDERED BY SLUG, both sides, and `eta_days` did not move so it is on
	// neither — [paramsText]'s rule, for [paramsText]'s reason.
	if got != (Delta{
		From: "environment=staging, severity=low",
		To:   "environment=production, severity=high",
	}) {
		t.Errorf("two moved fields recorded %+v", got)
	}

	// THE WRITER'S FRAME HOLDS NO CATALOGUE, so it records no `fields` at
	// all rather than a pair of uuids.
	if _, held := TaskDeltas(before, after, nil)["fields"]; held {
		t.Error("a comparison with no catalogue named a field by its id")
	}

	// A FIELD NOBODY DECLARES IS COUNTED. There is no slug to name it
	// with, and dropping it in silence would make a commit that moved only
	// such a value read as a commit that moved nothing.
	foreign := Task{Fields: map[string]json.RawMessage{
		"f-sev": json.RawMessage(`"low"`),
		"f-env": json.RawMessage(`"o-1"`),
		"f-eta": json.RawMessage(`3`),
		"f-???": json.RawMessage(`"anything"`),
	}}
	if got := TaskDeltas(before, foreign, declared)["fields"]; got !=
		(Delta{From: "", To: "1 undeclared"}) {

		t.Errorf("a value for an undeclared field recorded %+v", got)
	}
	// AND NOTHING MOVING RECORDS NOTHING, or every commit on a task with
	// custom values would carry an empty pair.
	if moved := TaskDeltas(before, before, declared); moved != nil {
		t.Errorf("an untouched field map recorded %v", moved)
	}
}

// A MULTI-VALUED FIELD KEEPS ITS MEMBERS APART FROM ITS NEIGHBOURS.
//
// ", " separates one FIELD from the next on a delta side, so the members of
// one field cannot use it: a labels field holding two options would otherwise
// read as two fields, one of them nameless.
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
	if got := TaskDeltas(Task{}, after, declared)["fields"]; got !=
		(Delta{From: "", To: "labels=api/ui"}) {

		t.Errorf("a two-option labels field recorded %+v", got)
	}
}

// THE FOUR SCALARS RECORD THEIR MOVES, and the two ids stay ids.
//
// `parent` and `removed_with` name ANOTHER TASK'S ROW, so they carry its id
// and never its key: `tracker_history` is inside this domain's identity claim
// and is repaired by nothing, so a node that had not applied that task would
// store a different string there for ever. The activity read resolves them.
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
		// BOTH STATES PRESENT for a flag, which is [boolText]'s rule:
		// "— → true" would read as a field that had no value before.
		"archived": {From: "false", To: "true"},
		// THE ONE THING A TOMBSTONE HOLDS that the history row's own
		// actor, actor_kind and created_at do not already carry.
		"removed_with": {From: "", To: "t-root"},
	} {
		if got := moved[field]; got != want {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
	// A ROOT HAS NO PARENT, and losing one is a move like any other.
	orphaned := TaskDeltas(Task{Parent: &oldParent}, Task{}, nil)["parent"]
	if orphaned != (Delta{From: "t-1", To: ""}) {
		t.Errorf("clearing a parent recorded %+v", orphaned)
	}
	// AND AN ORDINARY REMOVAL WENT WITH NOTHING, so it records nothing
	// here: `removed_with` answers "was this a cascade", and a task
	// somebody removed on purpose is not one.
	alone := TaskDeltas(Task{}, Task{Removed: &Tombstone{By: "bo"}}, nil)
	if _, held := alone["removed_with"]; held {
		t.Errorf("a removal that took nothing with it recorded %v", alone)
	}
}

// WHAT A CARD CARRIES AND WHAT A HISTORY ROW CARRIES DIFFER IN EXACTLY ONE
// FIELD.
//
// [TaskDeltas] is exported so that the two frames cannot disagree about what
// moved, and this is the whole of the one split that remains: the APPLIER
// holds the project's catalogue and the WRITER does not, so a custom-field
// move is history-only. Everything else a task can move reaches both, and a
// field held back from the notification "because a card is one line" would be
// that disagreement re-introduced by hand.
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
	if got := card.Fields["watchers"]; got != (Delta{From: "ana", To: "ana, bo"}) {
		t.Errorf("a watcher change reached the card as %+v — a card that "+
			"named no watcher is the row this field exists to complete", got)
	}
	if got := card.Fields["archived"]; got != (Delta{From: "false", To: "true"}) {
		t.Errorf("the archive flag reached the card as %+v", got)
	}
	if got, held := card.Fields["fields"]; held {
		t.Errorf("the card carried %+v for a custom field — the writer holds "+
			"no catalogue, so it could only have named it by uuid", got)
	}

	// AND THE APPLIER'S OWN ANSWER IS THE SAME SET PLUS THAT ONE FIELD, so
	// the history row is a superset of the card rather than a second
	// opinion about it.
	row := TaskDeltas(before, after, declared)
	for name, got := range card.Fields {
		if row[name] != got {
			t.Errorf("the row records %+v for %s and the card %+v — one "+
				"function answers both, so they cannot differ", row[name], name, got)
		}
	}
	if got := row["fields"]; got != (Delta{From: "severity=low", To: "severity=high"}) {
		t.Errorf("the history row recorded %+v for the custom field", got)
	}
	if len(row) != len(card.Fields)+1 {
		t.Errorf("the row records %v and the card %v — the catalogue field is "+
			"the only difference there is meant to be", row, card.Fields)
	}
}

// MAKING A DUE DATE ALL-DAY IS A CHANGE THE INSTANT CANNOT SHOW.
//
// An all-day date is stored as the company's own midnight, so the gesture that
// turns a midnight due date into an all-day one moves the flag and leaves the
// instant exactly where it was. Compared on `due` alone that is a row saying
// the schedule changed and naming nothing — the same failure the schedule
// fields were added to these deltas to end, one field along.
func TestMakingADueDateAllDayIsRecorded(t *testing.T) {
	t.Parallel()
	midnight := time.Date(2031, 4, 16, 0, 0, 0, 0, time.UTC)
	timed := Task{DueAt: &midnight}
	allDay := Task{DueAt: &midnight, DueAllDay: true}

	moved := TaskDeltas(timed, allDay, nil)
	if got := moved["due_all_day"]; got != (Delta{From: "false", To: "true"}) {
		t.Errorf("making a due date all-day recorded %+v", got)
	}
	if _, held := moved["due"]; held {
		t.Error("the instant did not move and is in the deltas")
	}
	// AND A TASK WITH NO ALL-DAY DATE ON EITHER SIDE RECORDS NOTHING, or
	// every commit in the company would carry the flag.
	if moved := TaskDeltas(timed, timed, nil); moved != nil {
		t.Errorf("an untouched schedule recorded %v", moved)
	}
}
