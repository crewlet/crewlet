package docindex_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/docindex"
)

// A DOC STORED BEFORE ADR-0059 READS AS IT DID ([docindex.Read]); only a
// publish writing its index ([docindex.Check]) holds it to the exact keys.
//
// Mutation: read a stored doc with ParseIndex and the legacy index is refused.
func TestADocStoredBeforeTheExactKeysStillReads(t *testing.T) {
	t.Parallel()
	exact := files{"doc.json": `{"v":1,"tabs":[{"path":"a.md","title":"A"}]}`, "a.md": "# A\n"}
	legacy := files{"doc.json": `{"tabs":[],"V":1,"Tabs":[{"Path":"a.md","TITLE":"A"}]}`, "a.md": "# A\n"}
	want, err := docindex.Read(exact, docindex.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := docindex.Read(legacy, docindex.IndexPath)
	if err != nil {
		t.Fatalf("the stored legacy doc is refused: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the legacy doc reads %+v, want %+v", got, want)
	}
	if _, err := docindex.Check(legacy, docindex.IndexPath); err == nil {
		t.Error("a publish writing the legacy index admitted it")
	}
}
