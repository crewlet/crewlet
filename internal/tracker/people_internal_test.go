package tracker

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fixedChart is [Identities] over one rename: `chief` was created as `cto`.
type fixedChart struct{}

func (fixedChart) Identity(handle string) string {
	if handle == "chief" {
		return "cto"
	}
	return handle
}

func (fixedChart) Current(identity string) string {
	if identity == "cto" {
		return "chief"
	}
	return identity
}

// A PERSON IS REWRITTEN IN A COPY, NEVER IN THE VALUE IT CAME IN.
//
// The writer rewrites the patch it was handed, and a decide runs again on a
// retry against that same patch — so a walk that wrote through a shared slice
// or pointer would rewrite the caller's value under it, and a caller that went
// on using its patch would find somebody else's spelling in it.
//
// Mutation: set the rewritten list into the caller's slice rather than a new
// one, and the original comes back rewritten.
func TestAPersonIsRewrittenInACopyNeverInTheValueItCameIn(t *testing.T) {
	t.Parallel()
	watchers := []string{"chief", "bob", "cto"}
	assignee := "chief"
	patch := TaskPatch{
		Assignee: &assignee,
		Watchers: &watchers,
		Comment:  &Comment{Author: "chief", Mentions: []string{"chief"}, Body: "hi"},
		Checklists: &[]Checklist{{ID: "c", Items: []ChecklistItem{
			{ID: "i", Assignee: "chief"}}}},
	}
	got := identified[TaskPatch](fixedChart{}, patch)

	if *got.Assignee != "cto" || got.Comment.Author != "cto" ||
		(*got.Checklists)[0].Items[0].Assignee != "cto" {
		t.Errorf("the patch was not rewritten to the identity: assignee %q, "+
			"author %q, checklist %q", *got.Assignee, got.Comment.Author,
			(*got.Checklists)[0].Items[0].Assignee)
	}
	// ONE SEAT UNDER TWO SPELLINGS IS ONE MEMBER.
	if !slices.Equal(*got.Watchers, []string{"cto", "bob"}) {
		t.Errorf("the watchers are %v, want cto once and bob", *got.Watchers)
	}
	if assignee != "chief" || !slices.Equal(watchers, []string{"chief", "bob", "cto"}) ||
		patch.Comment.Author != "chief" || patch.Comment.Mentions[0] != "chief" ||
		(*patch.Checklists)[0].Items[0].Assignee != "chief" {
		t.Errorf("the caller's own patch was rewritten under it: %+v", patch)
	}
	if got.Comment.Body != "hi" {
		t.Errorf("a field that names nobody moved: %q", got.Comment.Body)
	}
}

// THE THREE SHAPES A TAG CANNOT STATE ARE THE WALKER'S OWN: a delta's text,
// a people field's value, and the column a board is grouped by.
//
// Mutation: drop the delta arm, the field arm (or only its list half), or the
// person-axis test, and its case shows `cto`.
func TestTheShapesATagCannotStateAreRewrittenByTheirOwnRule(t *testing.T) {
	t.Parallel()
	shownBy := fixedChart{}

	deltas := shown[map[string]Delta](shownBy, map[string]Delta{
		"assignee": {From: "cto", To: "bob"},
		"watchers": {From: "bob", To: "bob, cto, +2 more"},
		"title":    {From: "cto", To: "cto plan"},
	})
	if deltas["assignee"].From != "chief" ||
		deltas["watchers"].To != "bob, chief, +2 more" {
		t.Errorf("the person deltas read %+v, want chief in both", deltas)
	}
	if deltas["title"].From != "cto" {
		t.Errorf("a title that happens to read `cto` was rewritten: %+v",
			deltas["title"])
	}

	fields := shown[[]FieldValue](shownBy, []FieldValue{
		{ID: "f1", Type: FieldPeople, Value: json.RawMessage(`"cto"`)},
		{ID: "f2", Type: FieldText, Value: json.RawMessage(`"cto"`)},
		// A PEOPLE FIELD IS MULTI-VALUED, so two colleagues in one field
		// are stored as a list — and read as one, a renamed seat in it was
		// shown by its identity.
		{ID: "f3", Type: FieldPeople, Value: json.RawMessage(`["bob","cto"]`)},
	})
	if string(fields[0].Value) != `"chief"` || string(fields[1].Value) != `"cto"` {
		t.Errorf("the field values read %s and %s, want the people field "+
			"rewritten and the text one as it was", fields[0].Value, fields[1].Value)
	}
	if string(fields[2].Value) != `["bob","chief"]` {
		t.Errorf("a people field naming two colleagues reads %s, want the "+
			"renamed one shown as chief", fields[2].Value)
	}
	// AND THE OTHER WAY: a list naming one seat by both of its handles is
	// one member, as every other person set here is.
	written := identified[[]FieldValue](fixedChart{}, []FieldValue{
		{ID: "f3", Type: FieldPeople, Value: json.RawMessage(`["chief","bob","cto"]`)},
	})
	if string(written[0].Value) != `["cto","bob"]` {
		t.Errorf("a people list written as %s, want each seat once by its "+
			"identity", written[0].Value)
	}

	declared := map[string]resolvedField{"owner": {Slug: "owner", Type: FieldPeople}}
	group := "chief"
	asked := identifiedByType(fixedChart{}, Query{
		GroupBy: "f.owner", Group: &group,
		Fields: []FieldFilter{{Ref: "owner", Op: FieldOpAny, Value: "chief, bob"}},
	}, declared)
	if *asked.Group != "cto" || asked.Fields[0].Value != "cto,bob" {
		t.Errorf("a people field's column and filter read %q and %q, want the "+
			"identity", *asked.Group, asked.Fields[0].Value)
	}
	if group != "chief" {
		t.Error("the caller's own column value was rewritten under it")
	}
	// A DOCUMENT HANDED IN AS `any` is walked as what it holds.
	var document any = View{ID: "v", Owner: "chief"}
	if got, ok := identified(fixedChart{}, document).(View); !ok || got.Owner != "cto" {
		t.Errorf("a view handed in as a document came back %#v, want its "+
			"owner's identity", got)
	}
	columns := shownGroups(shownBy, []Group{{Key: "cto"}, {Key: ""}}, true, false)
	if columns[0].Key != "chief" || columns[1].Key != "" {
		t.Errorf("an assignee board's columns are %q and %q, want chief and "+
			"the unassigned column", columns[0].Key, columns[1].Key)
	}
}

// personNames are the field names that name somebody, and namedBy is the
// suffixes that do. A field under one of them carries `person:"seat"`, or is
// in [notAPerson] with the reason it is not.
var (
	personNames = []string{
		"Assignee", "Reporter", "Watchers", "Watcher", "Collaborators",
		"Collaborator", "Muted", "Author", "Actor", "Owner", "Handle", "Person",
		"Mentions", "Ask", "Asked", "Viewer", "Participants", "RoutedTo",
		"CommentAsk", "PriorityListOf", "AskedOf",
	}
	namedBy = []string{"By", "Lead", "Assignee", "Assignees", "Author", "Watchers"}

	// notAPerson is every field whose NAME says somebody and whose value
	// is not one of this domain's person columns.
	notAPerson = map[string]string{
		"Eviction.EvictedBy": "a node eviction's author, recorded by every " +
			"domain's gate identically — not a person column of this one",
		"Generation.ReanchoredBy": "a node id",
		"Query.GroupBy":           "the name of a grouping axis",
	}
)

// EVERY FIELD THAT NAMES SOMEBODY SAYS SO, and nothing else does.
//
// The tag is the whole classification — people.go rewrites exactly what it
// marks — so a person field added without one is written and read under
// whatever handle the caller had, and is the rename bug again in one column.
// This walks every value a writer is handed and a reader answers with, and
// fails on a field whose name says it names somebody and which carries no tag,
// and on a tag on a field that cannot hold a name.
//
// Mutation: take the tag off Task.Assignee, or put one on Task.Title.
func TestEveryFieldThatNamesSomebodyIsTagged(t *testing.T) {
	t.Parallel()
	roots := []any{
		Task{}, TaskPatch{}, Comment{}, BodyRevision{}, Notify{},
		MutationRecord{}, View{}, ViewPrior{}, Person{}, Project{},
		ProjectEdit{}, Unblock{}, InboxMovement{},
		Query{}, MyWorkQuery{}, InboxQuery{}, PersonQuery{}, ViewQuery{},
		ActivityQuery{}, ThreadQuery{}, Viewer{}, WorkloadQuery{},
		Answer{}, TaskDetail{}, MyWork{}, InboxAnswer{}, PersonState{},
		ProjectListing{}, ProjectDetail{}, RoutingAnswer{}, ViewListing{},
		WorkloadAnswer{}, ActivityAnswer{}, CatalogueAnswer{},
		ResolvedThread{}, ErrAmbiguousAnswer{}, Ranked{}, Eviction{},
		Generation{},
	}
	seen := map[reflect.Type]bool{}
	excuses := map[string]bool{}
	var visit func(t reflect.Type)
	var missing, misplaced []string
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice ||
			typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] ||
			typ.PkgPath() != reflect.TypeOf(Task{}).PkgPath() {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			where := typ.Name() + "." + field.Name
			_, tagged := field.Tag.Lookup(personTag)
			switch {
			case tagged && !holdsNames(field.Type):
				misplaced = append(misplaced, where)
			case !tagged && holdsNames(field.Type) && namesSomebody(field.Name):
				if _, excused := notAPerson[where]; excused {
					excuses[where] = true
				} else {
					missing = append(missing, where)
				}
			}
			visit(field.Type)
		}
	}
	for _, root := range roots {
		visit(reflect.TypeOf(root))
	}
	if len(missing) > 0 {
		t.Errorf("these fields name somebody and carry no `person:\"seat\"` "+
			"tag, so a rename strands whatever they hold: %s",
			strings.Join(missing, ", "))
	}
	if len(misplaced) > 0 {
		t.Errorf("these fields carry `person:\"seat\"` and cannot hold a "+
			"handle: %s", strings.Join(misplaced, ", "))
	}
	// AN EXCUSE NOTHING NEEDS IS STALE, and a stale one would excuse the
	// next field that happens to take its name.
	for where := range notAPerson {
		if !excuses[where] {
			t.Errorf("%s is excused and no walked field needed it — drop the "+
				"excuse", where)
		}
	}
}

// namesSomebody reports a field name that says it holds a person.
func namesSomebody(name string) bool {
	if slices.Contains(personNames, name) {
		return true
	}
	for _, suffix := range namedBy {
		if strings.HasSuffix(name, suffix) && name != suffix+"s" {
			return true
		}
	}
	return false
}

// holdsNames reports a type the walker can rewrite under a tag.
func holdsNames(typ reflect.Type) bool {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	return typ.Kind() == reflect.String
}
