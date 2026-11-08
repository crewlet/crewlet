package builtin_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/tools"
)

// The findings of site!7 an apply names: two that suggest, one that suggests
// nothing, one on the old side and one dismissed.
const (
	suggestedA   = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d40"
	suggestedB   = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d41"
	suggestsNone = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d42"
	suggestsOld  = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d43"
	dismissedOne = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d44"
)

// suggestedChange is site!7 holding findings with suggestions.
func suggestedChange() *changeFixture {
	f := siteChange()
	retry, deleted := "retry(3)\n", ""
	finding := func(id, path, blob string, side code.Side, start, end int, state code.FindingState,
		s *string) code.FindingRow {
		return code.FindingRow{ID: id, ReviewID: reviewedID, Raiser: "agent-ceo", RaiserKind: code.PartyAgent,
			Revision: 3, Path: path, Side: side, LineStart: start, LineEnd: end, CommitOID: revHead(3),
			BlobOID: blob, Severity: code.SeverityMajor, Category: code.CategoryCorrectness, Title: "t",
			State: state, Suggestion: s}
	}
	f.findings = []code.FindingRow{
		finding(suggestedA, "a.txt", aNew, code.SideNew, 3, 3, code.FindingOpen, &retry),
		finding(suggestedB, "r3.txt", r3New, code.SideNew, 2, 4, code.FindingWontFix, &deleted),
		finding(suggestsNone, "a.txt", aNew, code.SideNew, 1, 1, code.FindingOpen, nil),
		finding(suggestsOld, "a.txt", aOld, code.SideOld, 1, 1, code.FindingOpen, &retry),
		finding(dismissedOne, "a.txt", aNew, code.SideNew, 5, 5, code.FindingDismissed, &retry),
	}
	return f
}

// AN APPLY READS ONLY THE FINDINGS IT NAMES: whatever the change's review
// history holds, the findings an apply names are ONE read of the change's
// findings narrowed to them — never a walk of the part, on whose last page a
// time-ordered history puts the newest review's findings, the ones an apply
// names — each id asked once, and one no finding could carry asked not at all
// and refused as one the change does not hold.
//
// Mutations: page the findings from no cursor, unnarrowed → three reads, red;
// ask a malformed id → the read names it, red; ask an id named twice twice →
// red.
func TestAnApplyReadsOnlyTheFindingsItNames(t *testing.T) {
	t.Parallel()
	f := suggestedChange()
	// A HISTORY OF EARLIER REVIEWS, two pages' worth before the findings named.
	var history []code.FindingRow
	for i := range 2 * code.MaxFindingsPerPage {
		history = append(history, code.FindingRow{ID: fmt.Sprintf("0192f6c1-6a7e-7c1b-9d4e-%012x", i),
			ReviewID: reviewedID, Raiser: "agent-ceo", RaiserKind: code.PartyAgent, Revision: 1, Path: "a.txt",
			Side: code.SideNew, LineStart: 1, LineEnd: 1, Title: "t", State: code.FindingSuperseded})
	}
	f.findings = append(history, f.findings...)
	host := newChangeHost(f)
	writer := &codeWrites{}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
		builtin.CodeApplySuggestionsTool, applyArgs(suggestedA, suggestedB))
	if writes, _ := writer.asked(); got.Failed || len(writes) != 1 || len(writes[0].Suggestions) != 2 {
		t.Fatalf("the apply answered %q, sending %d writes", got.Output, len(writes))
	}
	findingReads := func(host *gitletHost) []code.ReadQuery {
		reads, _, _ := host.asked()
		return slices.DeleteFunc(reads, func(q code.ReadQuery) bool { return q.Part != code.PartFindings })
	}
	if parts := findingReads(host); len(parts) != 1 ||
		!slices.Equal(parts[0].FindingIDs, []string{suggestedA, suggestedB}) || parts[0].Limit != 0 ||
		parts[0].Cursor != "" {
		t.Errorf("the apply read the findings %+v, want ONE read of the two it names", parts)
	}
	twice := newChangeHost(suggestedChange())
	callWrite(everyGrant(), t, codeRegistry(t, writeDeps(twice, &codeWrites{})), writerTurn(t),
		builtin.CodeApplySuggestionsTool, applyArgs(suggestedA, suggestedA))
	if parts := findingReads(twice); len(parts) != 1 || !slices.Equal(parts[0].FindingIDs, []string{suggestedA}) {
		t.Errorf("an apply naming a finding twice read %+v, want it asked once", parts)
	}

	malformed := &codeWrites{}
	quiet := newChangeHost(suggestedChange())
	got = callWrite(everyGrant(), t, codeRegistry(t, writeDeps(quiet, malformed)), writerTurn(t),
		builtin.CodeApplySuggestionsTool, applyArgs(suggestedA, "finding-1"))
	for _, q := range findingReads(quiet) {
		if slices.Contains(q.FindingIDs, "finding-1") {
			t.Errorf("the apply asked the read for %q, which no finding is", "finding-1")
		}
	}
	if writes, _ := malformed.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalNotFound ||
		len(writes) != 0 {
		t.Errorf("an apply naming no finding's id answered %q as %s, sending %d writes; want not_found and none",
			got.Output, tools.RefusalOf(got), len(writes))
	}
}

// applyArgs is an apply of findings to site!7 at its head.
func applyArgs(findings ...string) map[string]any {
	ids := make([]any, len(findings))
	for i, f := range findings {
		ids[i] = f
	}
	return map[string]any{"change": "site!7", "findings": ids, "expected_head": revHead(3)}
}

// AN APPLIED SUGGESTION RESOLVES ITS FINDING AS A STEP: the call's findings'
// suggestions are ONE engine write onto the change's source branch at the head
// the caller read, each named by its finding, read in the blob and lines its
// finding was raised on; once it landed, each finding is answered resolved
// with the commit it made, as a second write — a step of the apply's operation
// decided at or past the push's record, where the applier is an author. A
// finding no apply takes — gone, standing no longer, suggesting nothing, on
// its revision's old side — refuses the call with nothing written; an apply
// the Gitlet node refuses as outdated resolves nothing.
//
// Mutations: resolve the findings before the push lands, or under an
// operation of their own → red; take a finding that suggests nothing → red.
func TestAnAppliedSuggestionResolvesItsFindingAsAStep(t *testing.T) {
	t.Parallel()
	host := newChangeHost(suggestedChange())
	writer := &codeWrites{}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
		builtin.CodeApplySuggestionsTool, applyArgs(suggestedA, suggestedB))
	writes, _ := writer.asked()
	_, converses := writer.changesAsked()
	if got.Failed || len(writes) != 1 || len(converses) != 1 {
		t.Fatalf("the apply answered %q, sending %d writes and %d answers", got.Output, len(writes), len(converses))
	}
	w := writes[0]
	want := []code.SuggestionOp{
		{FindingID: suggestedA, Path: "a.txt", BlobOID: aNew, LineStart: 3, LineEnd: 3, Text: "retry(3)\n"},
		{FindingID: suggestedB, Path: "r3.txt", BlobOID: r3New, LineStart: 2, LineEnd: 4, Text: ""},
	}
	if w.Kind != code.WriteApplySuggestions || w.Ref != "refs/heads/feature" || w.ExpectedHead != revHead(3) ||
		!slices.Equal(w.Suggestions, want) || !strings.Contains(w.Message, "site!7") ||
		w.Principal.Kind != code.PrincipalAgent || w.Principal.Run.TurnID == "" {
		t.Errorf("the apply is %+v, want site!7's two suggestions onto feature at revision 3's head", w)
	}
	answer := converses[0]
	if answer.FindingResolve == nil || !strings.HasPrefix(answer.OpID, w.OpID+".") || answer.Seen != codeAt(13) {
		t.Fatalf("the findings' answer is %+v under %s at %s, want a step of the apply's %s at its push (%s)",
			answer.FindingResolve, answer.OpID, answer.Seen, w.OpID, codeAt(13))
	}
	for i, id := range []string{suggestedA, suggestedB} {
		if f := answer.FindingResolve.Findings[i]; f != (code.FindingResolution{FindingID: id,
			Resolution: code.FindingResolved, Commit: writtenOID}) {
			t.Errorf("finding %s is answered %+v, want resolved with the commit %s", id, f, writtenOID)
		}
	}
	if out := decoded(t, got); out["commit"] != writtenOID || len(out["findings_resolved"].([]any)) != 2 {
		t.Errorf("answered %q, want the commit and both findings resolved", got.Output)
	}

	for name, c := range map[string]struct {
		finding string
		class   tools.Refusal
	}{
		"one the change does not hold": {"0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d4f", tools.RefusalNotFound},
		"one dismissed":                {dismissedOne, tools.RefusalNotFound},
		"one suggesting nothing":       {suggestsNone, tools.RefusalInvalid},
		"one on the old side":          {suggestsOld, tools.RefusalInvalid},
	} {
		writer := &codeWrites{}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(suggestedChange()), writer)),
			writerTurn(t), builtin.CodeApplySuggestionsTool, applyArgs(suggestedA, c.finding))
		writes, _ := writer.asked()
		_, converses := writer.changesAsked()
		if !got.Failed || tools.RefusalOf(got) != c.class || len(writes)+len(converses) != 0 {
			t.Errorf("%s answered %q as %s, sending %d writes; want a %s refusal and none", name, got.Output,
				tools.RefusalOf(got), len(writes)+len(converses), c.class)
		}
	}

	outdated := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Refusal: code.WriteSuggestionOutdated,
			Detail: "finding " + suggestedA + "'s lines 3 to 3 of a.txt changed since they were read"}, nil
	}}
	got = callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(suggestedChange()), outdated)),
		writerTurn(t), builtin.CodeApplySuggestionsTool, applyArgs(suggestedA))
	if _, answers := outdated.changesAsked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalStaleVersion ||
		!strings.Contains(got.Output, suggestedA) || len(answers) != 0 {
		t.Errorf("an outdated apply answered %q as %s, resolving %d; want stale and nothing resolved", got.Output,
			tools.RefusalOf(got), len(answers))
	}
}
