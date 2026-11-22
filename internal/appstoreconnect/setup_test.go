package appstoreconnect_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE KEYS ARE ONE LIST THE FORM ADDS TO, never sixteen optional boxes: every
// key field is a member of one set of at least one key, leads the form, and
// the recommended role is offered first. The .p8 is a key that spans lines.
//
// Mutation: drop the Set or Connect from a key field, and the form folds the
// keys under More settings as optional while config refuses the block.
func TestTheKeysAreOneListOfAtLeastOneKey(t *testing.T) {
	t.Parallel()
	reqs := appstoreconnect.Requirements(nil, nil)
	var members []setup.Requirement
	for _, r := range reqs {
		if strings.Contains(r.ConfigPath, ".keys.") {
			members = append(members, r)
		}
	}
	if len(members) != 16 {
		t.Fatalf("%d key fields, want a key ID and a .p8 for each of eight roles", len(members))
	}
	for _, r := range members {
		if r.Set == nil || r.Set.ID != "main_keys" || r.Set.Min != 1 || !r.Connect {
			t.Errorf("%s is not a leading field of the one set of at least one key: %+v", r.Field, r.Set)
		}
	}
	if first := members[0].Set.Entry; first.Value != "marketing" || !strings.Contains(members[0].Set.Hint, first.Label) {
		t.Errorf("the first role offered is %s and the hint says %q: the recommended role leads",
			first.Value, members[0].Set.Hint)
	}
	if p8 := members[1]; p8.Kind != setup.KindSecret || !p8.Shape.Multiline() || p8.Shape.Accept() != ".p8" {
		t.Errorf("the .p8 field %s is not a secret that spans lines, offered as a .p8 file", p8.Field)
	}
	if out := setup.Outstanding(reqs); len(out) == 0 {
		t.Error("a team with no key has nothing outstanding")
	}
}
