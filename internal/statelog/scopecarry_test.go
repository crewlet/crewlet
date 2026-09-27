package statelog_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SCOPE ON THE WIRE IS THE BYTES IT ALWAYS WAS, AND KEEPS WHAT A NEWER
// BUILD WROTE ON IT.
//
// A domain whose record holds the framework's own scope — the vector domain's
// does — puts it on every node's log, so its bytes are a contract between
// peers: one carrying nothing this build does not know encodes exactly as its
// struct does, and a member a newer build added comes back from this build's
// decode and encode byte for byte.
//
// Mutation: drop ScopeSet's UnmarshalJSON in domain.go and the member is gone.
func TestAScopeOnTheWireKeepsWhatANewerBuildWrote(t *testing.T) {
	t.Parallel()
	const golden = `{"Paths":["t/c/ENG","t/f/person"]}`
	out, err := json.Marshal(statelog.ScopeSet{Paths: []string{"t/c/ENG", "t/f/person"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != golden {
		t.Fatalf("the scope encodes as\n  %s\nand every node holds\n  %s", out, golden)
	}
	jsoncarrytest.Survives(t, reflect.TypeFor[statelog.ScopeSet](), []byte(golden),
		func(raw []byte) ([]byte, error) {
			var scope statelog.ScopeSet
			if err := json.Unmarshal(raw, &scope); err != nil {
				return nil, err
			}
			return json.Marshal(scope)
		})
	if missing := jsoncarrytest.Uncarried(nil, reflect.TypeFor[statelog.ScopeSet]()); len(missing) != 0 {
		t.Errorf("the scope does not carry: %q", missing)
	}
}
