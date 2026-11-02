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

// A CANVAS STORED WITH A STRAY CLOSING BRACKET STILL READS: the canvas was read
// with encoding/json's More after its value, which is false before a `}` or
// `]`, so `{...} }` and `{...}]]]` were accepted and stored.
//
// Mutation: read a stored canvas with the io.EOF rule and these are refused.
func TestACanvasStoredWithAStrayBracketStillReads(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{" }", "]]]", "\n}\n"} {
		src := memSource{Path: []byte(landing + tail)}
		if _, err := Read(src, landingVersion()); err != nil {
			t.Errorf("a stored canvas ending %q is refused: %v", tail, err)
		}
		if _, err := Check(src, landingVersion()); err == nil {
			t.Errorf("a publish writing a canvas ending %q admitted it", tail)
		}
	}
	if _, err := Read(memSource{Path: []byte(landing + " {}")}, landingVersion()); err == nil {
		t.Error("a stored canvas followed by another value reads")
	}
}
