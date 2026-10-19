package edits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload/edits"
)

func unlimited(string) int64 { return 1 << 30 }

func apply(t *testing.T, texts map[string]string, list []edits.Edit) (map[string]string, error) {
	t.Helper()
	in := map[string][]byte{}
	for p, s := range texts {
		in[p] = []byte(s)
	}
	out, err := edits.Apply(in, list, unlimited)
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	for p, b := range out {
		got[p] = string(b)
	}
	return got, nil
}

func refusal(t *testing.T, err error) *edits.Refused {
	t.Helper()
	var r *edits.Refused
	if !errors.As(err, &r) || !errors.Is(err, edits.ErrRefused) {
		t.Fatalf("answered %v, want a *Refused that is ErrRefused", err)
	}
	return r
}

// EDITS APPLY IN ORDER, each to the text the one before it left: the second
// edit may match text the first one wrote.
func TestEditsApplyInOrderToTheTextBeforeThem(t *testing.T) {
	t.Parallel()
	got, err := apply(t, map[string]string{"index.html": "<h1>Hello</h1>"}, []edits.Edit{
		{Path: "index.html", Old: "Hello", New: "Hello, world"},
		{Path: "index.html", Old: ", world", New: ", team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["index.html"] != "<h1>Hello, team</h1>" {
		t.Fatalf("the text reads %q", got["index.html"])
	}
}

// AN EDIT MATCHING NOTHING OR SEVERAL PLACES REFUSES THE WHOLE SET, naming its
// index and the count, and nothing an earlier edit did is answered.
func TestAnEditThatDoesNotMatchExactlyOnceRefusesEverything(t *testing.T) {
	t.Parallel()
	texts := map[string]string{"a.css": "p { color: red } p { color: red }"}
	_, err := apply(t, texts, []edits.Edit{
		{Path: "a.css", Old: "p {", New: "div {"},
	})
	r := refusal(t, err)
	if r.Index != 0 || r.Matches != 2 {
		t.Fatalf("refused edit %d with %d matches, want edit 0 with 2", r.Index, r.Matches)
	}
	_, err = apply(t, texts, []edits.Edit{
		{Path: "a.css", Old: "color: red } p", New: "color: blue } p"},
		{Path: "a.css", Old: "green", New: "blue"},
	})
	r = refusal(t, err)
	if r.Index != 1 || r.Matches != 0 {
		t.Fatalf("refused edit %d with %d matches, want edit 1 with 0", r.Index, r.Matches)
	}
}

// OVERLAPPING OCCURRENCES ARE SEVERAL: `aa` sits at two places in `aaa`, and
// replacing either would be a guess about which one the author meant.
func TestOverlappingOccurrencesAreSeveralMatches(t *testing.T) {
	t.Parallel()
	_, err := apply(t, map[string]string{"x.txt": "aaa"}, []edits.Edit{
		{Path: "x.txt", Old: "aa", New: "b"},
	})
	if r := refusal(t, err); r.Matches != 2 {
		t.Fatalf("an overlapping old_str counted %d matches, want 2", r.Matches)
	}
}

// AN EDIT NAMES THE BASE VERSION: a file it does not hold, or one that is not
// UTF-8, is refused rather than created or mangled.
func TestAnEditNeedsTextTheBaseVersionHolds(t *testing.T) {
	t.Parallel()
	_, err := apply(t, map[string]string{"a.txt": "x"}, []edits.Edit{
		{Path: "b.txt", Old: "x", New: "y"},
	})
	if r := refusal(t, err); r.Matches != -1 || !strings.Contains(r.Error(), "no such file") {
		t.Fatalf("an edit of a missing file answered %v", r)
	}
	_, err = apply(t, map[string]string{"logo.png": "\xff\xfe\x00x"}, []edits.Edit{
		{Path: "logo.png", Old: "x", New: "y"},
	})
	if r := refusal(t, err); !strings.Contains(r.Error(), "UTF-8") {
		t.Fatalf("an edit of binary bytes answered %v", r)
	}
}

// THE RESULT STAYS INSIDE ITS LIMIT, and the refusal names the edit that
// crossed it.
func TestAnEditMayNotGrowAFilePastItsLimit(t *testing.T) {
	t.Parallel()
	in := map[string][]byte{"a.txt": []byte("ab")}
	_, err := edits.Apply(in, []edits.Edit{
		{Path: "a.txt", Old: "a", New: "aa"},
		{Path: "a.txt", Old: "b", New: "bbbb"},
	}, func(string) int64 { return 4 })
	if r := refusal(t, err); r.Index != 1 {
		t.Fatalf("refused edit %d, want the edit that crossed the limit", r.Index)
	}
	if string(in["a.txt"]) != "ab" {
		t.Fatalf("the base text was written: %q", in["a.txt"])
	}
}

// AN EMPTY PATH IS THE ENTRY, and an empty old_str is refused before anything
// is read: empty text occurs everywhere.
func TestNormalizeDefaultsToTheEntryAndRefusesEmptyText(t *testing.T) {
	t.Parallel()
	got, err := edits.Normalize([]edits.Edit{{Old: "a", New: "b"}}, "index.html")
	if err != nil || got[0].Path != "index.html" {
		t.Fatalf("Normalize = %+v, %v", got, err)
	}
	if _, err := edits.Normalize([]edits.Edit{{Path: "a", New: "b"}}, "index.html"); err == nil {
		t.Fatal("an empty old_str was accepted")
	}
	if _, err := edits.Normalize([]edits.Edit{{Old: "a"}}, ""); err == nil {
		t.Fatal("an edit with no path and no entry was accepted")
	}
	if _, err := edits.Normalize([]edits.Edit{{Path: "a", Old: "\xff", New: "b"}}, ""); err == nil {
		t.Fatal("an old_str that is not UTF-8 was accepted")
	}
	if got := edits.Paths([]edits.Edit{{Path: "b"}, {Path: "a"}, {Path: "b"}}); strings.Join(got, ",") != "b,a" {
		t.Fatalf("Paths = %v, want first-named order", got)
	}
}
