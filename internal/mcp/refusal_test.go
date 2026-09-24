package mcp

import (
	"encoding/json"
	"testing"
)

// TestARefusalCodeOffTheWireIsAValue is the enum rule for a class that
// travels: a newer build may classify a refusal this one has no constant for,
// and it has to arrive as a value a reader can fall back from — never a decode
// error that loses the refusal's sentence along with its class.
func TestARefusalCodeOffTheWireIsAValue(t *testing.T) {
	t.Parallel()
	var got struct {
		Refusal Refusal `json:"refusal"`
	}
	if err := json.Unmarshal([]byte(`{"refusal":"quota_exhausted"}`), &got); err != nil {
		t.Fatalf("an unknown class did not decode: %v", err)
	}
	if got.Refusal != "quota_exhausted" {
		t.Fatalf("an unknown class decoded as %q, want it kept verbatim", got.Refusal)
	}
	if got.Refusal.Valid() {
		t.Fatal("a class this build has no constant for reports Valid")
	}
	// THE EMPTY CLASS IS NOT A CLASS: it is what an MCP server's failure
	// carries, and a reader treating it as valid would read "unclassified"
	// as a known answer.
	if Refusal("").Valid() {
		t.Fatal("the empty class reports Valid")
	}
}

// TestEveryRefusalClassIsKnownOnce holds the closed set a person's surface
// maps to statuses: every class is Valid, spelled as a lowercase wire token,
// and listed once — a duplicate is a table row a reader maps twice.
func TestEveryRefusalClassIsKnownOnce(t *testing.T) {
	t.Parallel()
	seen := map[Refusal]bool{}
	for _, r := range Refusals {
		if !r.Valid() {
			t.Errorf("%q is listed and not Valid", r)
		}
		if seen[r] {
			t.Errorf("%q is listed twice", r)
		}
		seen[r] = true
		for _, c := range r {
			if (c < 'a' || c > 'z') && c != '_' {
				t.Errorf("%q is not a lowercase wire token", r)
				break
			}
		}
	}
	if len(Refusals) != 13 {
		t.Errorf("%d classes, want the 13 the write surface's status table maps", len(Refusals))
	}
}
