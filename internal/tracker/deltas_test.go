package tracker

import (
	"strings"
	"testing"
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
	if moved := TaskDeltas(one, other); moved != nil {
		t.Errorf("re-stating one edge set in another order recorded %v, and "+
			"nothing about the task changed", moved)
	}
	// AND A REAL MEMBERSHIP CHANGE IS RECORDED, or the assertion above
	// would pass for a comparison that answers nothing at all.
	grew := Task{Relations: append([]Relation{{Kind: RelationWaitingOn, Other: "c"}},
		other.Relations...)}
	if got := TaskDeltas(other, grew)["waiting_on"]; got != (Delta{From: "a, b", To: "a, b, c"}) {
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
	moved := TaskDeltas(before, after)
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
	mirrored := TaskDeltas(Task{}, Task{Dependents: []string{"dep-2", "dep-1"}})
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
