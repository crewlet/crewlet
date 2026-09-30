package seatnames_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seatnames"
)

// identity is a chart over one rename: `chief` was created as `cto`.
func identity(handle string) string {
	if handle == "chief" {
		return "cto"
	}
	return handle
}

type note struct {
	Author  string   `person:"seat"`
	Mention []string `person:"seat"`
	Body    string
}

type record struct {
	Owner    string    `person:"seat"`
	Reviewer *string   `person:"seat"`
	Watchers []string  `person:"seat"`
	Muted    *[]string `person:"seat"`
	Title    string
	Notes    []note
	ByKey    map[string]note
	Pinned   *note
	Any      []record
	private  string
}

// A TAGGED NAME IS REWRITTEN, AND ONLY IN A COPY.
//
// A writer rewrites the value it was handed, and a decide runs again on a retry
// against that same value — so a walk that wrote through a shared slice, map
// or pointer would rewrite its caller's value under it.
//
// Mutation: set a rewritten list back into the slice it came in, or skip the
// copy of a pointer, and the original comes back rewritten.
func TestATaggedNameIsRewrittenOnlyInACopy(t *testing.T) {
	t.Parallel()
	reviewer, muted := "chief", []string{"chief"}
	in := record{
		Owner: "chief", Reviewer: &reviewer, Watchers: []string{"bob", "chief"},
		Muted: &muted, Title: "chief's plan",
		Notes:   []note{{Author: "chief", Mention: []string{"chief"}, Body: "chief"}},
		ByKey:   map[string]note{"a": {Author: "chief"}},
		Pinned:  &note{Author: "chief"},
		Any:     []record{{Owner: "chief"}},
		private: "chief",
	}
	walker := seatnames.NewWalker()
	got := seatnames.Rewrite(walker, in, identity)

	switch {
	case got.Owner != "cto", *got.Reviewer != "cto", !slices.Equal(got.Watchers, []string{"bob", "cto"}),
		!slices.Equal(*got.Muted, []string{"cto"}), got.Notes[0].Author != "cto",
		!slices.Equal(got.Notes[0].Mention, []string{"cto"}), got.ByKey["a"].Author != "cto",
		got.Pinned.Author != "cto", got.Any[0].Owner != "cto":
		t.Errorf("a tagged name was not rewritten: %+v", got)
	}
	if got.Title != "chief's plan" || got.Notes[0].Body != "chief" || got.private != "chief" {
		t.Errorf("a field that is not a name moved: %+v", got)
	}
	if in.Owner != "chief" || reviewer != "chief" || in.Watchers[1] != "chief" ||
		muted[0] != "chief" || in.Notes[0].Author != "chief" ||
		in.Notes[0].Mention[0] != "chief" || in.ByKey["a"].Author != "chief" ||
		in.Pinned.Author != "chief" || in.Any[0].Owner != "chief" {
		t.Errorf("the caller's own value was rewritten under it: %+v", in)
	}
}

// ONE SEAT UNDER TWO SPELLINGS IS ONE MEMBER, in a tagged list and through
// [seatnames.Names] alike — and nobody stays nobody.
//
// Mutation: drop the contains check and the list holds cto twice; map an
// empty name through the chart and a chart answering "" for "" is trusted.
func TestASetNamingOneSeatTwiceIsOneMember(t *testing.T) {
	t.Parallel()
	got := seatnames.Rewrite(seatnames.NewWalker(),
		record{Watchers: []string{"chief", "bob", "cto"}}, identity)
	if !slices.Equal(got.Watchers, []string{"cto", "bob"}) {
		t.Errorf("the watchers are %v, want cto once and bob", got.Watchers)
	}
	loud := func(string) string { return "somebody" }
	if got := seatnames.Rewrite(seatnames.NewWalker(), record{Owner: ""}, loud); got.Owner != "" {
		t.Errorf("an empty name was mapped to %q, want it left as nobody", got.Owner)
	}
}

// AN ARM REWRITES ITS TYPE BY ITS OWN RULE, wherever the type is found.
//
// Mutation: drop the arm lookup and the text is left as it was.
func TestAnArmRewritesItsTypeByItsOwnRule(t *testing.T) {
	t.Parallel()
	type deltas map[string]string
	type change struct {
		Moved deltas
		By    string `person:"seat"`
	}
	walker := seatnames.NewWalker(seatnames.Arm{
		Type: reflect.TypeFor[deltas](),
		Rewrite: func(v reflect.Value, f func(string) string) reflect.Value {
			out := deltas{}
			for key, text := range v.Interface().(deltas) {
				if key == "assignee" {
					text = f(text)
				}
				out[key] = text
			}
			return reflect.ValueOf(out)
		},
	})
	in := change{Moved: deltas{"assignee": "chief", "title": "chief"}, By: "chief"}
	got := seatnames.Rewrite(walker, in, identity)
	if got.Moved["assignee"] != "cto" || got.Moved["title"] != "chief" || got.By != "cto" {
		t.Errorf("the change reads %+v, want the arm's field and the tag rewritten "+
			"and the title as it was", got)
	}
	if in.Moved["assignee"] != "chief" {
		t.Error("the arm's input was rewritten under its caller")
	}
	// A WALKER WITHOUT THE ARM does not reach the map at all.
	if plain := seatnames.Rewrite(seatnames.NewWalker(), in, identity); plain.Moved["assignee"] != "chief" {
		t.Error("a walker with no arm rewrote a map of plain strings")
	}
}

// A VALUE HANDED IN AS AN INTERFACE is walked as what it holds, and handed
// back as the same interface — a document a writer takes as `any`.
func TestAValueHandedInAsAnInterfaceComesBackAsOne(t *testing.T) {
	t.Parallel()
	var document any = note{Author: "chief"}
	got, ok := seatnames.Rewrite(seatnames.NewWalker(), document, identity).(note)
	if !ok || got.Author != "cto" {
		t.Errorf("a note handed in as `any` came back %#v, want its author's identity", got)
	}
	var none any
	if seatnames.Rewrite(seatnames.NewWalker(), none, identity) != nil {
		t.Error("a nil interface came back as something")
	}
}

// A VALUE THAT CANNOT HOLD A NAME IS HANDED BACK AS IT IS, with no copy — and
// the tag is what makes a type able to hold one.
func TestAValueThatHoldsNoNameIsHandedBack(t *testing.T) {
	t.Parallel()
	type plain struct{ Title []string }
	in := plain{Title: []string{"chief"}}
	got := seatnames.Rewrite(seatnames.NewWalker(), in, identity)
	if &got.Title[0] != &in.Title[0] {
		t.Error("a value with nothing to rewrite was copied")
	}
	if seatnames.NewWalker().Reaches(reflect.TypeFor[plain]()) {
		t.Error("a type with no tag and no arm reports that it holds a name")
	}
	if !seatnames.NewWalker().Reaches(reflect.TypeFor[[]record]()) {
		t.Error("a list of a tagged type reports that it holds no name")
	}
}

// WHAT A TAG MAY STAND ON is what the walker rewrites under one, and nothing
// else — the rule every domain's tag test holds its fields to.
func TestATagStandsOnANameOrAListOfThem(t *testing.T) {
	t.Parallel()
	for typ, want := range map[reflect.Type]bool{
		reflect.TypeFor[string]():            true,
		reflect.TypeFor[*string]():           true,
		reflect.TypeFor[[]string]():          true,
		reflect.TypeFor[*[]string]():         true,
		reflect.TypeFor[int]():               false,
		reflect.TypeFor[note]():              false,
		reflect.TypeFor[map[string]string](): false,
	} {
		if got := seatnames.HoldsNames(typ); got != want {
			t.Errorf("HoldsNames(%s) = %v, want %v", typ, got, want)
		}
	}
	if got := seatnames.Names([]string{"a", "chief", "cto", ""}, identity); strings.Join(got, ",") != "a,cto," {
		t.Errorf("Names = %q, want a, cto once and the empty member kept", got)
	}
}
