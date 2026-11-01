package code_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// anApply is an apply_suggestions any party may act on: alice's suggestion of
// one finding onto a branch at the head she read.
func anApply() code.EngineWrite {
	w := aWrite()
	w.Kind, w.Base, w.Files, w.ExpectedHead = code.WriteApplySuggestions, "", nil, oidOf("H")
	w.Message = "Apply the review's suggestions"
	w.Suggestions = []code.SuggestionOp{{FindingID: uuid.Must(uuid.NewV7()).String(), Path: "retry.go",
		BlobOID: oidOf("B"), LineStart: 3, LineEnd: 4, Text: "retry(3)\n"}}
	return w
}

// APPLY_SUGGESTIONS EXTENDS EVERY ENGINE WRITE TABLE: the kind is in the
// closed set every party reads and makes a commit — a pack, a signature and a
// build; its two refusals are in the closed set of a change's refusals; and
// the one rule every party holds a write to takes its fields and no other —
// the head it lands on, a message and 1 to code.MaxSuggestionOps suggestions,
// each a finding's id once, a path one write may name, a whole blob id, lines
// from 1 to code.MaxAnchorLine and a text within its bound, the payload
// within one write's — and refuses suggestions on every other kind.
//
// Mutations: Commits leaving apply_suggestions out → no signature, red; a
// kind that takes suggestions beside files → red; a finding named twice
// admitted → red; a line past code.MaxAnchorLine admitted → red.
func TestApplySuggestionsExtendsEveryEngineWriteTable(t *testing.T) {
	t.Parallel()
	if !slices.Contains(code.EngineWriteKinds(), code.WriteApplySuggestions) || !code.WriteApplySuggestions.Valid() ||
		!code.WriteApplySuggestions.Commits() {
		t.Fatalf("apply_suggestions is not a kind every party knows to commit: %v", code.EngineWriteKinds())
	}
	for _, r := range []code.WriteRefusal{code.WriteSuggestionOutdated, code.WriteSuggestionOverlap} {
		if !slices.Contains(code.WriteRefusals(), r) || !r.Valid() {
			t.Errorf("%s is not a refusal of a change every party knows", r)
		}
	}
	if err := anApply().Validate(); err != nil {
		t.Fatalf("a well-formed apply: %v", err)
	}
	edit := func(f func(*code.EngineWrite)) code.EngineWrite {
		w := anApply()
		f(&w)
		return w
	}
	many := make([]code.SuggestionOp, code.MaxSuggestionOps+1)
	for i := range many {
		many[i] = code.SuggestionOp{FindingID: uuid.Must(uuid.NewV7()).String(), Path: "a.go", BlobOID: oidOf("B"),
			LineStart: 1, LineEnd: 1}
	}
	wide := strings.Repeat("x", code.MaxSuggestionBytes)
	for name, c := range map[string]struct {
		w     code.EngineWrite
		field string
	}{
		"no head":        {edit(func(w *code.EngineWrite) { w.ExpectedHead = "" }), "expected_head"},
		"no message":     {edit(func(w *code.EngineWrite) { w.Message = "" }), "message"},
		"no suggestions": {edit(func(w *code.EngineWrite) { w.Suggestions = nil }), "suggestions"},
		"files beside":   {edit(func(w *code.EngineWrite) { w.Files = aWrite().Files }), "files"},
		"a base":         {edit(func(w *code.EngineWrite) { w.Base = oidOf("C") }), "base"},
		"a commit":       {edit(func(w *code.EngineWrite) { w.Commit = oidOf("C") }), "commit"},
		"too many":       {edit(func(w *code.EngineWrite) { w.Suggestions = many }), "suggestions"},
		"a finding twice": {edit(func(w *code.EngineWrite) {
			// THE SAME FINDING'S, at other lines: what is refused is the
			// finding named twice, not a suggestion repeated.
			again := w.Suggestions[0]
			again.LineStart, again.LineEnd = 7, 7
			w.Suggestions = append(w.Suggestions, again)
		}), "finding_id"},
		"no finding's id": {edit(func(w *code.EngineWrite) { w.Suggestions[0].FindingID = "f-1" }), "finding_id"},
		"a short blob":    {edit(func(w *code.EngineWrite) { w.Suggestions[0].BlobOID = "abc1234" }), "blob_oid"},
		"lines backwards": {edit(func(w *code.EngineWrite) { w.Suggestions[0].LineEnd = 2 }), "suggestions[0]"},
		"a line past any file's": {edit(func(w *code.EngineWrite) {
			w.Suggestions[0].LineEnd = code.MaxAnchorLine + 1
		}), "suggestions[0]"},
		"a path git keeps": {edit(func(w *code.EngineWrite) { w.Suggestions[0].Path = ".git/config" }), "path"},
		"a text past its bound": {edit(func(w *code.EngineWrite) { w.Suggestions[0].Text = wide + "x" }),
			"text"},
		"suggestions on a commit_files": {func() code.EngineWrite {
			w := aWrite()
			w.Suggestions = anApply().Suggestions
			return w
		}(), "suggestions"},
	} {
		err := c.w.Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: Validate answered %v, want ErrInvalid naming %s", name, err, c.field)
		}
	}
	// THE PAYLOAD'S BOUND: suggestions within their own bounds whose whole
	// passes one write's.
	full := edit(func(w *code.EngineWrite) {
		w.Suggestions = nil
		// AS MANY AS ONE WRITE APPLIES, each at its text's bound: their texts
		// alone are the whole payload, so the encoding beside them is past it.
		for range code.MaxSuggestionOps {
			w.Suggestions = append(w.Suggestions, code.SuggestionOp{FindingID: uuid.Must(uuid.NewV7()).String(),
				Path: "a.go", BlobOID: oidOf("B"), LineStart: 1, LineEnd: 1, Text: wide})
		}
	})
	if err := full.Validate(); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "MaxCommitPayloadBytes") {
		t.Errorf("an apply past one write's payload: Validate answered %v", err)
	}
}

// A PLACING IS REANCHOR FED ONE HUNK AT A TIME: over every hunk sequence of
// the re-anchoring cases, feeding a placing hunk by hunk in the diff's order
// places the lines exactly where Reanchor over the slice does, and settles at
// the first hunk at or below them — so a reader streaming a diff keeps a
// placing an anchor, never the diff's hunks.
//
// Mutation: a placing that keeps moving after a hunk below it → the shift
// past it differs, red.
func TestAPlacingIsReanchorFedOneHunkAtATime(t *testing.T) {
	t.Parallel()
	hunks := []code.Hunk{
		{OldStart: 2, OldLines: 1, NewStart: 2, NewLines: 3},  // above lines 5..6: +2
		{OldStart: 4, OldLines: 0, NewStart: 7, NewLines: 1},  // an insertion after 4: +1
		{OldStart: 8, OldLines: 2, NewStart: 12, NewLines: 0}, // below
		{OldStart: 12, OldLines: 0, NewStart: 14, NewLines: 5},
	}
	for _, r := range []struct{ start, end int }{{5, 6}, {1, 1}, {4, 4}, {4, 5}, {8, 9}, {10, 20}, {3, 3}} {
		for n := 0; n <= len(hunks); n++ {
			wantStart, wantEnd, wantKept := code.Reanchor(hunks[:n], r.start, r.end)
			p := code.PlaceLines(r.start, r.end)
			for _, h := range hunks[:n] {
				p.Hunk(h)
			}
			if start, end, kept := p.Placed(); start != wantStart || end != wantEnd || kept != wantKept {
				t.Errorf("lines %d..%d over %d hunks: placed %d..%d (%v), Reanchor %d..%d (%v)", r.start, r.end, n,
					start, end, kept, wantStart, wantEnd, wantKept)
			}
		}
	}
	settled := code.PlaceLines(5, 6)
	if settled.Hunk(hunks[0]) || !settled.Hunk(hunks[2]) || !settled.Hunk(hunks[3]) {
		t.Error("a placing settled at the first hunk below it, and stayed so, is not what Hunk reports")
	}
}
