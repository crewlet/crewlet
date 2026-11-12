package gitlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// anApply is an apply_suggestions any party may act on: alice's suggestion of
// one finding onto a branch at the head she read.
func anApply() gitlet.EngineWrite {
	w := aWrite()
	w.Kind, w.Base, w.Files, w.ExpectedHead = gitlet.WriteApplySuggestions, "", nil, oidOf("H")
	w.Message = "Apply the review's suggestions"
	w.Suggestions = []gitlet.SuggestionOp{{FindingID: uuid.Must(uuid.NewV7()).String(), Path: "retry.go",
		BlobOID: oidOf("B"), LineStart: 3, LineEnd: 4, Text: "retry(3)\n"}}
	return w
}

// APPLY_SUGGESTIONS EXTENDS EVERY ENGINE WRITE TABLE: the kind is in the
// closed set every party reads and makes a commit — a pack, a signature and a
// build; its two refusals are in the closed set of a change's refusals; and
// the one rule every party holds a write to takes its fields and no other —
// the head it lands on, a message and 1 to gitlet.MaxSuggestionOps suggestions,
// each a finding's id once, a path one write may name, a whole blob id, lines
// from 1 to gitlet.MaxAnchorLine and a text within its bound, the payload
// within one write's — and refuses suggestions on every other kind.
//
// Mutations: Commits leaving apply_suggestions out → no signature, red; a
// kind that takes suggestions beside files → red; a finding named twice
// admitted → red; a line past gitlet.MaxAnchorLine admitted → red.
func TestApplySuggestionsExtendsEveryEngineWriteTable(t *testing.T) {
	t.Parallel()
	if !slices.Contains(gitlet.EngineWriteKinds(), gitlet.WriteApplySuggestions) || !gitlet.WriteApplySuggestions.Valid() ||
		!gitlet.WriteApplySuggestions.Commits() {
		t.Fatalf("apply_suggestions is not a kind every party knows to commit: %v", gitlet.EngineWriteKinds())
	}
	for _, r := range []gitlet.WriteRefusal{gitlet.WriteSuggestionOutdated, gitlet.WriteSuggestionOverlap} {
		if !slices.Contains(gitlet.WriteRefusals(), r) || !r.Valid() {
			t.Errorf("%s is not a refusal of a change every party knows", r)
		}
	}
	if err := anApply().Validate(); err != nil {
		t.Fatalf("a well-formed apply: %v", err)
	}
	edit := func(f func(*gitlet.EngineWrite)) gitlet.EngineWrite {
		w := anApply()
		f(&w)
		return w
	}
	many := make([]gitlet.SuggestionOp, gitlet.MaxSuggestionOps+1)
	for i := range many {
		many[i] = gitlet.SuggestionOp{FindingID: uuid.Must(uuid.NewV7()).String(), Path: "a.go", BlobOID: oidOf("B"),
			LineStart: 1, LineEnd: 1}
	}
	wide := strings.Repeat("x", gitlet.MaxSuggestionBytes)
	for name, c := range map[string]struct {
		w     gitlet.EngineWrite
		field string
	}{
		"no head":        {edit(func(w *gitlet.EngineWrite) { w.ExpectedHead = "" }), "expected_head"},
		"no message":     {edit(func(w *gitlet.EngineWrite) { w.Message = "" }), "message"},
		"no suggestions": {edit(func(w *gitlet.EngineWrite) { w.Suggestions = nil }), "suggestions"},
		"files beside":   {edit(func(w *gitlet.EngineWrite) { w.Files = aWrite().Files }), "files"},
		"a base":         {edit(func(w *gitlet.EngineWrite) { w.Base = oidOf("C") }), "base"},
		"a commit":       {edit(func(w *gitlet.EngineWrite) { w.Commit = oidOf("C") }), "commit"},
		"too many":       {edit(func(w *gitlet.EngineWrite) { w.Suggestions = many }), "suggestions"},
		"a finding twice": {edit(func(w *gitlet.EngineWrite) {
			// THE SAME FINDING'S, at other lines: what is refused is the
			// finding named twice, not a suggestion repeated.
			again := w.Suggestions[0]
			again.LineStart, again.LineEnd = 7, 7
			w.Suggestions = append(w.Suggestions, again)
		}), "finding_id"},
		"no finding's id": {edit(func(w *gitlet.EngineWrite) { w.Suggestions[0].FindingID = "f-1" }), "finding_id"},
		"a short blob":    {edit(func(w *gitlet.EngineWrite) { w.Suggestions[0].BlobOID = "abc1234" }), "blob_oid"},
		"lines backwards": {edit(func(w *gitlet.EngineWrite) { w.Suggestions[0].LineEnd = 2 }), "suggestions[0]"},
		"a line past any file's": {edit(func(w *gitlet.EngineWrite) {
			w.Suggestions[0].LineEnd = gitlet.MaxAnchorLine + 1
		}), "suggestions[0]"},
		"a path git keeps": {edit(func(w *gitlet.EngineWrite) { w.Suggestions[0].Path = ".git/config" }), "path"},
		"a text past its bound": {edit(func(w *gitlet.EngineWrite) { w.Suggestions[0].Text = wide + "x" }),
			"text"},
		"suggestions on a commit_files": {func() gitlet.EngineWrite {
			w := aWrite()
			w.Suggestions = anApply().Suggestions
			return w
		}(), "suggestions"},
	} {
		err := c.w.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: Validate answered %v, want ErrInvalid naming %s", name, err, c.field)
		}
	}
	// THE PAYLOAD'S BOUND: suggestions within their own bounds whose whole
	// passes one write's.
	full := edit(func(w *gitlet.EngineWrite) {
		w.Suggestions = nil
		// AS MANY AS ONE WRITE APPLIES, each at its text's bound: their texts
		// alone are the whole payload, so the encoding beside them is past it.
		for range gitlet.MaxSuggestionOps {
			w.Suggestions = append(w.Suggestions, gitlet.SuggestionOp{FindingID: uuid.Must(uuid.NewV7()).String(),
				Path: "a.go", BlobOID: oidOf("B"), LineStart: 1, LineEnd: 1, Text: wide})
		}
	})
	if err := full.Validate(); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "MaxCommitPayloadBytes") {
		t.Errorf("an apply past one write's payload: Validate answered %v", err)
	}
}

// A PLACING MOVES LINES BY EVERY HUNK ABOVE THEM AND SETTLES AT THE FIRST AT
// OR BELOW THEM: fed a diff's zero-context hunks one at a time, in the diff's
// order, it places a range where the new file holds it — moved by what each
// hunk wholly above it added less what it removed, an insertion right above
// it included — and settles at the first hunk that touches or follows it: the
// same lines where that hunk is below them or an insertion right after their
// last line, OUTDATED where it replaces, deletes or inserts between any of
// them, placed then where only the hunks above moved it. Each placing is held
// to the figures, written out, it must reach after each prefix of the diff,
// and Reanchor — a placing fed a slice — to the figures after the whole of it.
//
// Mutations: a hunk above moving the range by one more than it added less it
// removed → red; an insertion right after the range's last line read as
// inside it → red; an insertion above it moving nothing → red; a hunk below
// it reported unsettled → red.
func TestAPlacingSettlesAtTheFirstHunkAtOrBelowItsLines(t *testing.T) {
	t.Parallel()
	hunks := []gitlet.Hunk{
		{OldStart: 2, OldLines: 1, NewStart: 2, NewLines: 3},   // old line 2 → three lines: +2 below it
		{OldStart: 4, OldLines: 0, NewStart: 7, NewLines: 1},   // one line inserted after old line 4
		{OldStart: 8, OldLines: 2, NewStart: 12, NewLines: 0},  // old lines 8..9 deleted: -2 below them
		{OldStart: 12, OldLines: 0, NewStart: 14, NewLines: 5}, // five lines inserted after old line 12
	}
	type placed struct {
		start, end int
		kept       bool
	}
	for _, c := range []struct {
		start, end int
		// after is where the lines are after the first n hunks, n = 0..4.
		after [5]placed
		// settles is the hunk that settles the placing, -1 for none.
		settles int
	}{
		{5, 6, [5]placed{{5, 6, true}, {7, 8, true}, {8, 9, true}, {8, 9, true}, {8, 9, true}}, 2},
		{1, 1, [5]placed{{1, 1, true}, {1, 1, true}, {1, 1, true}, {1, 1, true}, {1, 1, true}}, 0},
		// An insertion right after the range's last line is below it.
		{4, 4, [5]placed{{4, 4, true}, {6, 6, true}, {6, 6, true}, {6, 6, true}, {6, 6, true}}, 1},
		{3, 3, [5]placed{{3, 3, true}, {5, 5, true}, {5, 5, true}, {5, 5, true}, {5, 5, true}}, 1},
		// An insertion between two of its lines outdates it where it stood.
		{4, 5, [5]placed{{4, 5, true}, {6, 7, true}, {6, 7, false}, {6, 7, false}, {6, 7, false}}, 1},
		// A deletion of its lines outdates it.
		{8, 9, [5]placed{{8, 9, true}, {10, 11, true}, {11, 12, true}, {11, 12, false}, {11, 12, false}}, 2},
		{10, 20, [5]placed{{10, 20, true}, {12, 22, true}, {13, 23, true}, {11, 21, true}, {11, 21, false}}, 3},
		// Every hunk above it, the last an insertion: never settled.
		{13, 14, [5]placed{{13, 14, true}, {15, 16, true}, {16, 17, true}, {14, 15, true}, {19, 20, true}}, -1},
	} {
		p := gitlet.PlaceLines(c.start, c.end)
		for n := 0; n <= len(hunks); n++ {
			if n > 0 {
				want := c.settles >= 0 && n-1 >= c.settles
				if settled := p.Hunk(hunks[n-1]); settled != want {
					t.Errorf("lines %d..%d: hunk %d reports settled %v, want %v", c.start, c.end, n-1, settled, want)
				}
			}
			start, end, kept := p.Placed()
			if got := (placed{start, end, kept}); got != c.after[n] {
				t.Errorf("lines %d..%d after %d hunks: placed %+v, want %+v", c.start, c.end, n, got, c.after[n])
			}
		}
		start, end, kept := gitlet.Reanchor(hunks, c.start, c.end)
		if got := (placed{start, end, kept}); got != c.after[len(hunks)] {
			t.Errorf("Reanchor places lines %d..%d at %+v, want %+v", c.start, c.end, got, c.after[len(hunks)])
		}
	}
}
