package dashspec

import (
	"reflect"
	"strings"
	"testing"
)

// A BOARD STORED BEFORE ADR-0059 READS AS IT DID: its data is answered from
// the stored spec on every view, so a stored spec with a key in another case
// or written twice must read as encoding/json read it ([Read]); only a publish
// writing the spec ([Check]) holds it to the exact keys.
//
// Mutation: read a stored board with Parse and the legacy board is refused.
func TestABoardStoredBeforeTheExactKeysStillReads(t *testing.T) {
	t.Parallel()
	v := boardVersion(t)
	want := readBoard(t, v)
	raw, err := v.Read(EntryPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `"title":`, `"Title":`, 1)
	legacy = `{"widgets": [],` + strings.TrimPrefix(strings.TrimSpace(legacy), "{")
	v.over[EntryPath] = []byte(legacy)
	got, err := Read(v)
	if err != nil {
		t.Fatalf("the stored legacy board is refused: %v", err)
	}
	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Error("the legacy board reads differently from the board it spells")
	}
	if _, err := Check(v); err == nil {
		t.Error("a publish writing the legacy spec admitted it")
	}
}
