package learning

import (
	"testing"
	"time"
)

// A ROW'S TEXT IS EVERY TEXT IT CARRIES. A listing bounded by bytes counts its
// rows with textBytes, so a text it skipped is one that grows a page past its
// bound unseen: every string, list element and map key and value, through
// structs, pointers and interfaces, and nothing for an instant or a number.
//
// Mutations, each red: skip lists; skip map keys; skip map values; skip
// pointers or interfaces; stop at a nested struct; count an instant's zone.
func TestEveryTextARowCarriesIsCounted(t *testing.T) {
	t.Parallel()
	type inner struct{ Note string }
	type row struct {
		Name   string
		Tools  []string
		Traits map[string]any
		Nested inner
		Label  *inner
		Absent *inner
		At     time.Time
		Count  int
		Raw    []byte
	}
	zone := time.FixedZone("A-ZONE-WITH-A-LONG-NAME", 3600)
	r := row{
		Name: "ab", Tools: []string{"cde", "f"},
		Traits: map[string]any{"gh": "ijk", "l": []any{"mn"}, "n": 3.0},
		Nested: inner{Note: "opq"}, Label: &inner{Note: "rs"},
		At: time.Date(2031, 4, 1, 9, 0, 0, 0, zone), Count: 7, Raw: []byte("tuv"),
	}
	// 2 + (3+1) + (2+3 + 1+2 + 1) + 3 + 2 + 3
	if got, want := textBytes(r), 23; got != want {
		t.Errorf("textBytes = %d, want %d: every string, list element, map key and value", got, want)
	}
	if got := textBytes(row{}); got != 0 {
		t.Errorf("an empty row counts %d bytes, want 0", got)
	}
}
