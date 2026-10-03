package configapi

import "testing"

// EVERY COLLECTION STATES HOW A MEMBER OF IT IS LISTED, READ, REPLACED AND
// ADDED, in the stored tree as well as the struct.
//
// The route table mounts a read and a write for every kind it lists, so a
// member left nil is not a narrower collection: it is a nil function called
// inside a request. There is no read-only collection here any more — the one
// kind of collection that could not be written at its address, the org
// chart's, is not addressed at all — so the table states every member or the
// kind does not belong in it. Mutation: drop any one member of either entry
// and this fails.
func TestEveryEntityKindStatesEveryAccess(t *testing.T) {
	t.Parallel()
	if len(entityKinds) == 0 {
		t.Fatal("the entity table is empty, so this case certifies nothing")
	}
	for kind, access := range entityKinds {
		for name, missing := range map[string]bool{
			"ids":     access.ids == nil,
			"find":    access.find == nil,
			"replace": access.replace == nil,
			"stored":  access.stored == nil,
			"create":  access.create == nil,
			"place":   access.place == nil,
		} {
			if missing {
				t.Errorf("%s states no %s, and both its routes are mounted", kind, name)
			}
		}
	}
}
