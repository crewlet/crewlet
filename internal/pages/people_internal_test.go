package pages

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seatnames"
)

// EVERY FIELD THAT NAMES SOMEBODY SAYS SO, and nothing else does.
//
// The tag is the whole classification — people.go rewrites exactly what it
// marks — so a person field added without one is written and read under
// whatever handle the caller had, and is the rename bug again in one column.
// This walks every value the store is handed and every value it and the
// reader answer with, and every record and document the applier writes rows
// from, and fails on a field whose name says it names somebody and which
// carries no tag, and on a tag on a field that cannot hold a name.
//
// Mutation: take the tag off Comment.Author, or put one on Page.Title.
func TestEveryPageFieldThatNamesSomebodyIsTagged(t *testing.T) {
	t.Parallel()
	roots := []any{
		NewPage{}, Save{}, NewComment{}, Actor{}, Written{},
		Filter{}, PageActivityQuery{},
		Listing{}, Detail{}, PageActivity{}, ContainerListing{},
		Page{}, Revision{}, Comment{}, Container{}, TitleClaim{}, Change{},
		MutationRecord{}, CreatePayload{}, RenamePayload{}, RetitlePayload{},
		PagePatch{}, CommentPatch{}, ContainerPayload{}, StatusPayload{},
		Eviction{}, Generation{},
	}
	seen := map[reflect.Type]bool{}
	excuses := map[string]bool{}
	var missing, misplaced []string
	var visit func(t reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice ||
			typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] ||
			typ.PkgPath() != reflect.TypeOf(Page{}).PkgPath() {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			where := typ.Name() + "." + field.Name
			_, tagged := field.Tag.Lookup(seatnames.Tag)
			switch {
			case tagged && !seatnames.HoldsNames(field.Type):
				misplaced = append(misplaced, where)
			case !tagged && seatnames.HoldsNames(field.Type) && namesSomebody(field.Name):
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

// personNames are the field names that name somebody, and namedBy is the
// suffixes that do. A field under one of them carries `person:"seat"`, or is
// in [notAPerson] with the reason it is not.
var (
	personNames = []string{
		"Watchers", "Watcher", "Muted", "Author", "Actor", "Owner", "Handle",
		"Person", "Mentions", "Recipients", "Lead",
	}
	namedBy = []string{"By", "Author", "Watchers", "Lead"}

	// notAPerson is every field whose NAME says somebody and whose value
	// is not one of this domain's person columns.
	notAPerson = map[string]string{
		"Eviction.EvictedBy": "a node eviction's author, recorded by every " +
			"domain's gate identically — not a person column of this one",
		"Generation.By": "a node id",
	}
)

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

// A PERSON IS REWRITTEN IN A COPY, NEVER IN THE VALUE IT CAME IN.
//
// A write rewrites what it was handed before its decide runs, and a decide the
// framework retries reads it again — so a rewrite in place would hand the
// caller's own slice back to them spelled differently, and a retried decide
// would rewrite an identity a second time.
//
// Mutation: rewrite the watchers in place.
func TestAPagePersonIsRewrittenInACopy(t *testing.T) {
	t.Parallel()
	watchers := []string{"chief", "jane"}
	in := NewPage{Title: "Runbook", Watchers: watchers}
	got := identified(renamedChart{}, in)
	if !slices.Equal(got.Watchers, []string{"cto", "jane"}) {
		t.Errorf("the watchers came out %v, want the seat's identity", got.Watchers)
	}
	if !slices.Equal(watchers, []string{"chief", "jane"}) {
		t.Errorf("the caller's own watchers were rewritten under it: %v", watchers)
	}
	shownPage := shown(renamedChart{}, Page{Author: "cto", Watchers: []string{"cto"},
		LastChange: &Change{Actor: "cto", Snapshot: Snapshot{Author: "cto"}}})
	if shownPage.Author != "chief" || shownPage.LastChange.Actor != "chief" ||
		shownPage.LastChange.Snapshot.Author != "chief" {
		t.Errorf("a page is shown as %+v, want chief throughout", shownPage)
	}
	// A NIL CHART is a build holding none, which records every value as given.
	if got := identified(nil, in); !slices.Equal(got.Watchers, watchers) {
		t.Errorf("no chart rewrote the watchers to %v", got.Watchers)
	}
}

// renamedChart is one rename: `chief` was created as `cto`.
type renamedChart struct{}

func (renamedChart) Identity(handle string) string {
	if handle == "chief" {
		return "cto"
	}
	return handle
}

func (renamedChart) Current(identity string) string {
	if identity == "cto" {
		return "chief"
	}
	return identity
}
