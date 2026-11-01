package configapi

import (
	"strings"
	"testing"
)

// EVERY COLLECTION STATES HOW A MEMBER OF IT IS LISTED, READ, REPLACED AND
// ADDED, in the stored tree as well as the struct.
//
// The route table mounts a read and a write for every kind it lists, so a
// member left nil is not a narrower collection: it is a nil function called
// inside a request. The ONE exception is the create-only write, and only for
// the collections whose members have a place in the company the path cannot
// name — a seat in a unit, a unit under its parent: they state neither create
// nor place, both together, and are answered [ErrNotCreatable]. A kind that
// states one and not the other is a half-built create.
//
// Mutation: drop any one of the four members every kind needs, or one of the
// create pair from llm-providers, and this fails.
func TestEveryEntityKindStatesEveryAccess(t *testing.T) {
	t.Parallel()
	if len(entityKinds) == 0 {
		t.Fatal("the entity table is empty, so this case certifies nothing")
	}
	notCreatable := map[string]bool{EntityRoles: true, EntityUnits: true}
	for kind, access := range entityKinds {
		for name, missing := range map[string]bool{
			"ids":     access.ids == nil,
			"find":    access.find == nil,
			"replace": access.replace == nil,
			"stored":  access.stored == nil,
		} {
			if missing {
				t.Errorf("%s states no %s, and both its routes are mounted", kind, name)
			}
		}
		creates, places := access.create != nil, access.place != nil
		switch {
		case creates != places:
			t.Errorf("%s states create %v and place %v: a create-only write needs "+
				"both or neither", kind, creates, places)
		case notCreatable[kind] && creates:
			t.Errorf("%s states a create, but its members have a place the path "+
				"cannot name", kind)
		case !notCreatable[kind] && !creates:
			t.Errorf("%s states no create, and its create-only write is mounted", kind)
		}
	}
}

// EVERY COLLECTION SAYS WHAT ONE OF ITS MEMBERS IS CALLED, because a refusal
// naming a miss in it is read by a person ([NotFound]): named by its URL
// segment, it read "nothing in the running company's mcp-servers". A kind
// added without a noun fails here rather than answering "no entry".
// Mutation: drop a row's noun and this fails naming the kind.
func TestEveryEntityKindNamesItsMember(t *testing.T) {
	t.Parallel()
	if len(entityKinds) < 4 {
		t.Fatalf("the table holds %d kinds; a walk that read nothing passes", len(entityKinds))
	}
	for kind, access := range entityKinds {
		if access.noun == "" || strings.Contains(access.noun, "-") {
			t.Errorf("%s names its member %q, which a sentence cannot say", kind, access.noun)
		}
	}
}
