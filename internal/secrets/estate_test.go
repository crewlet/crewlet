package secrets_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE ENGINE'S NAMESPACE AND THE OPERATOR'S NEVER OVERLAP, by the shape of the
// value: every engine name carries a '/', which the reference grammar has no
// room for, so no name is both a `${VAR}` and an engine key — and a name
// reserved by its owner stays reserved however malformed the rest of it is,
// because a surface that refused only well-formed engine names would admit
// exactly the ones nobody meant to write.
func TestTheEnginesNamespaceAndTheOperatorsNeverOverlap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		reserved bool
		estate   bool
	}{
		{"iam/blind-index-key", true, true},
		{"iam/nested/018f3a9c-0000-7000-8000-000000000001/key", true, true},
		// RESERVED AND STILL NOT WRITABLE: an owner with a malformed tail.
		{"iam/nested//key", true, false},
		{"iam/Nested/x/key", true, false},
		{"iam/", true, false},
		// NEITHER: an operator's name, and a slash under nobody's owner.
		{"GITLAB_TOKEN", false, false},
		{"IAM_PERSON_X_DEK", false, false},
		{"other/thing", false, false},
		{"seat/018f3a9c-0000-7000-8000-000000000002/dek", false, false},
		{"iamx/thing", false, false},
	} {
		if got := secrets.Reserved(tc.name); got != tc.reserved {
			t.Errorf("Reserved(%q) = %t, want %t", tc.name, got, tc.reserved)
		}
		err := secrets.CheckEstateName(tc.name)
		if (err == nil) != tc.estate {
			t.Errorf("CheckEstateName(%q) = %v, want accepted=%t", tc.name, err, tc.estate)
		}
		if err != nil && !errors.Is(err, secrets.ErrInvalidName) {
			t.Errorf("CheckEstateName(%q) refused with %v, want ErrInvalidName",
				tc.name, err)
		}
		if tc.reserved && secrets.CheckName(tc.name) == nil {
			t.Errorf("%q is reserved AND a name a ${VAR} reaches", tc.name)
		}
	}
}

// A ROTATION COUNTS WHAT IT STILL HAS TO MOVE, per key, without a name.
func TestEngineKeysCountWhatARotationStillHasToMove(t *testing.T) {
	t.Parallel()
	keys := secrets.EngineKeys{Total: 5, ByKey: map[string]int{"k1": 3, "k2": 2}}
	if got := keys.StaleUnder("k2"); got != 3 {
		t.Errorf("StaleUnder(k2) = %d, want the 3 under k1", got)
	}
	if got := (secrets.EngineKeys{}).StaleUnder("k1"); got != 0 {
		t.Errorf("an empty count reports %d stale", got)
	}
}
