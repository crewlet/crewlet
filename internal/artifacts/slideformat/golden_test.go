package slideformat

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/deck.golden.json from the parser's answer")

// THE DASHBOARD'S RENDERER IS TESTED ON THIS PARSER'S OWN ANSWER: the twelve-
// slide deck read whole, as the deck route sends it, is held in
// testdata/deck.golden.json, which dashboard/src/lib/slides.test.tsx draws. A
// parser change that moves the tree fails here until the golden is rewritten
// (`go test ./internal/artifacts/slideformat -run Golden -update`), so the
// renderer's tests never certify a tree the engine no longer sends.
func TestTheGoldenDeckIsTheParsersAnswer(t *testing.T) {
	deck, err := Read(testDeck(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(deck, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/deck.golden.json"
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: run with -update to write it", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not the parser's answer: run with -update and review the diff", path)
	}
}
