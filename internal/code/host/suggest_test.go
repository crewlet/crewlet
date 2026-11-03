package host_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
)

// THE APPLY_SUGGESTIONS' OWN CASES: a review's suggestions put in place of
// their findings' lines on a git node's copy, by the one rule a finding is
// shown by, all of them or none.

// suggest is an apply_suggestions by who onto ref, at head.
func suggest(repo string, who code.Principal, ref, head string, ss ...code.SuggestionOp) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteApplySuggestions, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: ref, ExpectedHead: head, Message: "Apply the review's suggestions", Suggestions: ss}
}

// suggestion is a suggestion of a finding of its own on lines start to end of
// path, read in blob.
func suggestion(path, blob string, start, end int, text string) code.SuggestionOp {
	return code.SuggestionOp{FindingID: uuid.Must(uuid.NewV7()).String(), Path: path, BlobOID: blob,
		LineStart: start, LineEnd: end, Text: text}
}

// numbered is a file of the lines named, each ended.
func numbered(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// blobAt is path's blob at commit, as the case's person reads it.
func (c *writeCase) blobAt(commit, path string) string {
	c.T.Helper()
	return strings.TrimSpace(c.p.Git().Run(c.T, c.work, "rev-parse", commit+":"+path))
}

// fileAt is path's text at commit, read from a fresh clone.
func (c *writeCase) fileAt(commit, path string) string {
	c.T.Helper()
	return c.p.Git().Run(c.T, c.clone(), "show", commit+":"+path)
}

// A SUGGESTION ON A MOVED LINE IS RE-ANCHORED: a finding read on line 4 of a
// file its branch has since moved on — two lines put above it, a line below it
// rewritten — is applied where its line went, line 6, by the diff between the
// blob it was read in and the head's; one read in the head's own blob where it
// was read; a suggestion of nothing deletes its lines; and the rest of the file
// is the head's, byte for byte, its unterminated last line kept so.
//
// Mutation: splice at the lines the finding was read at → the suggestion lands
// on line 4, red.
func TestASuggestionOnAMovedLineIsReanchored(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{
		"retry.go": numbered("a", "b", "c", "d", "e"),
		"notes.md": "one\ntwo\nthree",
	})
	read := c.blobAt(c.head, "retry.go")
	moved := c.p.Commit(c.work, map[string]string{"retry.go": numbered("x", "y", "a", "b", "c", "d", "E")}, "move")
	c.MustPush(c.p, c.work, "main")
	notes := c.blobAt(moved, "notes.md")
	res := c.land(suggest(c.repo, pat(), "refs/heads/main", moved,
		suggestion("retry.go", read, 4, 4, "D\n"),
		suggestion("notes.md", notes, 3, 3, "THREE\n"),
		suggestion("notes.md", notes, 1, 1, "")))
	if got, want := c.fileAt(res.Commit, "retry.go"), numbered("x", "y", "a", "b", "c", "D", "E"); got != want {
		t.Errorf("retry.go is %q, want %q: the finding's line where it went", got, want)
	}
	if got, want := c.fileAt(res.Commit, "notes.md"), "two\nTHREE"; got != want {
		t.Errorf("notes.md is %q, want %q: its first line deleted, its unterminated last replaced as it was",
			got, want)
	}
	if parent := c.show(c.clone(), "%P", res.Commit); parent != moved {
		t.Errorf("the commit sits on %s, want the head the write read, %s", parent, moved)
	}
}

// ONE OUTDATED SUGGESTION REFUSES THE WRITE: of two suggestions, one whose own
// line the head has since rewritten is outdated, and the write applies
// NOTHING — refused suggestion_outdated naming that finding alone, no push
// made, the branch where it was; so is one naming lines past the file's end;
// and suggestions whose lines, placed, overlap are refused suggestion_overlap
// naming every one of them — the third of a range that holds two apart among
// them, which overlaps the range and neither of the two.
//
// Mutation: drop the placing's verdict and splice the outdated suggestion
// where it moved → the write lands, red; compare each suggestion with its
// neighbour alone → the range's third goes unnamed, red.
func TestOneOutdatedSuggestionRefusesTheWrite(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"retry.go": numbered("a", "b", "c", "d", "e")})
	read := c.blobAt(c.head, "retry.go")
	moved := c.p.Commit(c.work, map[string]string{"retry.go": numbered("a", "B", "c", "d", "e")}, "rewrite b")
	c.MustPush(c.p, c.work, "main")
	stale := suggestion("retry.go", read, 2, 2, "bee\n")
	fine := suggestion("retry.go", read, 4, 4, "dee\n")
	at := c.blobAt(moved, "retry.go")
	past := suggestion("retry.go", at, 6, 6, "f\n")
	left, right := suggestion("retry.go", at, 3, 4, "x\n"), suggestion("retry.go", at, 4, 5, "y\n")
	wide, inner, third := suggestion("retry.go", at, 1, 5, "x\n"), suggestion("retry.go", at, 2, 2, "y\n"),
		suggestion("retry.go", at, 4, 4, "z\n")
	for _, tc := range []struct {
		name    string
		ss      []code.SuggestionOp
		refusal code.WriteRefusal
		named   []string
		not     []string
	}{
		{"a line the head rewrote", []code.SuggestionOp{fine, stale}, code.WriteSuggestionOutdated,
			[]string{stale.FindingID}, []string{fine.FindingID}},
		{"lines past the file's end", []code.SuggestionOp{past}, code.WriteSuggestionOutdated,
			[]string{past.FindingID}, nil},
		{"two that overlap", []code.SuggestionOp{right, left}, code.WriteSuggestionOverlap,
			[]string{left.FindingID, right.FindingID}, nil},
		{"a range that holds two apart", []code.SuggestionOp{third, wide, inner}, code.WriteSuggestionOverlap,
			[]string{wide.FindingID, inner.FindingID, third.FindingID}, nil},
	} {
		res, err := c.write(suggest(c.repo, pat(), "refs/heads/main", moved, tc.ss...))
		if err != nil || res.Refusal != tc.refusal || res.Outcome != "" {
			t.Errorf("%s: the write answered %+v %v, want refused %s with nothing landed", tc.name, res, err,
				tc.refusal)
			continue
		}
		for _, id := range tc.named {
			if !strings.Contains(res.Detail, id) {
				t.Errorf("%s: the refusal %q does not name finding %s", tc.name, res.Detail, id)
			}
		}
		for _, id := range tc.not {
			if strings.Contains(res.Detail, id) {
				t.Errorf("%s: the refusal %q names finding %s, whose lines are in place", tc.name, res.Detail, id)
			}
		}
	}
	if head := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main")); head != moved {
		t.Errorf("main is at %s after refused applies, want %s", head, moved)
	}
}

// APPLY_SUGGESTIONS IS THE ACTOR'S COMMIT (W-M1): a seat's apply is authored
// by the seat at the address the signer names and committed by the engine, on
// the seat's own branch, as every engine write's commit is.
//
// Mutation: sign the commit as its finding's raiser → red.
func TestApplySuggestionsIsTheActorsCommit(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": numbered("one", "two")})
	ref := "refs/heads/seats/" + codetest.FixtureAlice + "/fix"
	c.MustPush(c.p, c.work, "main:"+ref)
	res := c.land(suggest(c.repo, alice(), ref, c.head, suggestion("README.md", c.blobAt(c.head, "README.md"), 2, 2,
		"TWO\n")))
	got := c.show(c.clone(), "%an <%ae>|%cn <%ce>", res.Commit)
	want := fmt.Sprintf("%s <%s>|%s <%s>", codetest.FixtureAlice, codetest.FixtureAuthorEmail(codetest.FixtureAlice),
		codetest.FixtureCommitterName, codetest.FixtureCommitterEmail)
	if got != want {
		t.Errorf("the apply's commit is %q, want %q: the acting seat's, committed by the engine", got, want)
	}
}
