package canvasformat

import (
	"reflect"
	"strings"
	"testing"
)

// A CANVAS STORED BEFORE ADR-0059 READS AS IT DID ([Read]); only a publish
// writing it ([Parse], [Check]) holds it to the exact keys.
//
// Mutation: read a stored canvas with Parse and the legacy canvas is refused.
func TestACanvasStoredBeforeTheExactKeysStillReads(t *testing.T) {
	t.Parallel()
	want, err := Parse([]byte(landing), landingVersion())
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(landing, `"title": "Landing page"`, `"Title": "Landing page"`, 1)
	legacy = `{"cover": "boards/mobile.html",` + strings.TrimPrefix(legacy, "{")
	src := memSource{Path: []byte(legacy)}
	got, err := Read(src, landingVersion())
	if err != nil {
		t.Fatalf("the stored legacy canvas is refused: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the legacy canvas reads %+v, want %+v", got, want)
	}
	if _, err := Check(src, landingVersion()); err == nil {
		t.Error("a publish writing the legacy canvas admitted it")
	}
}

type memSource map[string][]byte

func (m memSource) Read(p string) ([]byte, error) { return m[p], nil }
