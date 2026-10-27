package slideformat

import (
	"reflect"
	"strings"
	"testing"
)

// A DECK STORED BEFORE ADR-0059 READS AS IT DID: encoding/json matched its keys
// without regard to case and kept the last of a key written twice, so a stored
// index may carry either. Every read of it (Read) decodes it as before; only a
// publish that writes the index (Check) holds it to the exact keys.
//
// Mutation: read a stored deck with ParseIndex and the legacy index is refused.
func TestADeckStoredBeforeTheExactKeysStillReads(t *testing.T) {
	t.Parallel()
	src := testDeck(t)
	want, err := Read(src)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(src.files[IndexPath]), `"title":`, `"Title":`, 1)
	legacy = `{"order": ["gone"],` + strings.TrimPrefix(strings.TrimSpace(legacy), "{")
	src.files[IndexPath] = []byte(legacy)
	got, err := Read(src)
	if err != nil {
		t.Fatalf("the stored legacy deck is refused: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("the legacy deck reads differently from the deck it spells")
	}
	if _, err := Check(src); err == nil {
		t.Error("a publish writing the legacy index admitted it")
	}
}
