package code_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A READ BY OBJECT ID NAMES OBJECTS, NEVER REVISIONS, AND PATHS NO GRAMMAR
// READS. Every revision is a whole id or an abbreviation of git's default
// length or more — never a ref, `^`, `~` or `:`, which would hand git's
// revision grammar to the git node, nor fewer digits than name one object of a
// large repository — every path is clean and relative (git reads `./` and
// `../` as relative to a work tree, which dies over a bare repository's whole
// batch), and each refusal names its field.
func TestAGitQueryIsRefusedByField(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	asker := code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	tree := func(m func(q *code.TreeQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitTree, RepoID: repo, Asker: asker, Tree: &code.TreeQuery{Rev: id}}
		m(q.Tree)
		return q
	}
	for _, tc := range []struct {
		name  string
		q     code.GitQuery
		field string // "" is a valid query
		want  error
	}{
		{"a whole id", tree(func(*code.TreeQuery) {}), "", nil},
		{"an abbreviation", tree(func(q *code.TreeQuery) { q.Rev = id[:7] }), "", nil},
		{"a longer abbreviation", tree(func(q *code.TreeQuery) { q.Rev = id[:39] }), "", nil},
		{"a path", tree(func(q *code.TreeQuery) { q.Path = "src/a b/--output=x" }), "", nil},
		{"a path as bytes", tree(func(q *code.TreeQuery) {
			q.PathB64 = base64.StdEncoding.EncodeToString([]byte("caf\xe9"))
		}), "", nil},
		{"a short abbreviation", tree(func(q *code.TreeQuery) { q.Rev = id[:6] }), "tree.rev", code.ErrInvalid},
		{"a ref", tree(func(q *code.TreeQuery) { q.Rev = "main" }), "tree.rev", code.ErrInvalid},
		{"a revision expression", tree(func(q *code.TreeQuery) { q.Rev = id + "^" }), "tree.rev", code.ErrInvalid},
		{"upper case", tree(func(q *code.TreeQuery) { q.Rev = strings.ToUpper(id) }), "tree.rev", code.ErrInvalid},
		{"the zero id", tree(func(q *code.TreeQuery) { q.Rev = code.ZeroOID }), "tree.rev", code.ErrInvalid},
		{"a path from the work tree", tree(func(q *code.TreeQuery) { q.Path = "./src" }), "tree.path",
			code.ErrInvalid},
		{"a path up", tree(func(q *code.TreeQuery) { q.Path = "../src" }), "tree.path", code.ErrInvalid},
		{"a dot", tree(func(q *code.TreeQuery) { q.Path = "." }), "tree.path", code.ErrInvalid},
		{"an absolute path", tree(func(q *code.TreeQuery) { q.Path = "/etc" }), "tree.path", code.ErrInvalid},
		{"a trailing slash", tree(func(q *code.TreeQuery) { q.Path = "src/" }), "tree.path", code.ErrInvalid},
		{"a doubled slash", tree(func(q *code.TreeQuery) { q.Path = "src//a" }), "tree.path", code.ErrInvalid},
		{"a NUL", tree(func(q *code.TreeQuery) { q.Path = "a\x00b" }), "tree.path", code.ErrInvalid},
		{"a path too long", tree(func(q *code.TreeQuery) { q.Path = strings.Repeat("a", code.MaxGitPathBytes+1) }),
			"tree.path", code.ErrInvalid},
		{"both spellings of a path", tree(func(q *code.TreeQuery) { q.Path, q.PathB64 = "a", "YQ==" }), "tree.path",
			code.ErrInvalid},
		{"bytes that are no base64", tree(func(q *code.TreeQuery) { q.PathB64 = "!!" }), "tree.path_b64",
			code.ErrInvalid},
		{"no asker", func() code.GitQuery { q := tree(func(*code.TreeQuery) {}); q.Asker = code.Principal{}; return q }(),
			"asker", code.ErrInvalid},
		{"no repository", func() code.GitQuery { q := tree(func(*code.TreeQuery) {}); q.RepoID = "web"; return q }(),
			"repo_id", code.ErrInvalid},
		{"no kind", func() code.GitQuery { q := tree(func(*code.TreeQuery) {}); q.Kind = "grep"; return q }(),
			"kind", code.ErrInvalid},
		{"another kind's query", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Blob = &code.BlobQuery{Rev: id}
			return q
		}(), "blob", code.ErrInvalid},
		{"none of its own", code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: asker}, "blob", code.ErrInvalid},
		{"a backwards range", code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: asker,
			Blob: &code.BlobQuery{Rev: id, Path: "a", StartLine: 9, EndLine: 3}}, "blob.end_line", code.ErrInvalid},
		{"a negative line", code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: asker,
			Blob: &code.BlobQuery{Rev: id, Path: "a", StartLine: -1}}, "blob.start_line", code.ErrInvalid},
		{"a file and paths", code.GitQuery{Kind: code.GitDiff, RepoID: repo, Asker: asker,
			Diff: &code.DiffQuery{Base: id, Head: id, Paths: []string{"a"}, File: "a"}}, "diff.file", code.ErrInvalid},
		{"too many paths", code.GitQuery{Kind: code.GitDiff, RepoID: repo, Asker: asker,
			Diff: &code.DiffQuery{Base: id, Head: id, Paths: make([]string, code.MaxDiffPaths+1)}}, "diff.paths",
			code.ErrInvalid},
		{"an unclean filter", code.GitQuery{Kind: code.GitDiff, RepoID: repo, Asker: asker,
			Diff: &code.DiffQuery{Base: id, Head: id, Paths: []string{"a/../b"}}}, "diff.paths[0]", code.ErrInvalid},
		{"a comparison of a ref", code.GitQuery{Kind: code.GitCompare, RepoID: repo, Asker: asker,
			Compare: &code.CompareQuery{Base: id, Head: "HEAD"}}, "compare.head", code.ErrInvalid},
		{"a cursor longer than any", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Cursor = strings.Repeat("9", 65)
			return q
		}(), "cursor", code.ErrBadCursor},
		// ONLY A SEAT READS FOR ANOTHER SEAT, named by its handle: a person's
		// read and a token's are their own, which no ceiling bounds.
		{"a seat's read for another seat", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Asker, q.CeilingSeat = code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, "lead"
			return q
		}(), "", nil},
		{"a token's read for a seat", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.CeilingSeat = "lead"
			return q
		}(), "ceiling_seat", code.ErrInvalid},
		{"a seat's read for no handle", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Asker, q.CeilingSeat = code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, "Not A Handle"
			return q
		}(), "ceiling_seat", code.ErrInvalid},
		// THE ASKER'S FLOOR IS ON THE CODE LOG: a copy is brought along it
		// alone, and a floor on another log beside one there would be dropped.
		{"a floor on the code log", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Fresh = statelog.FloorAt(statelog.ReadSession,
				statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9})
			return q
		}(), "", nil},
		{"a floor on another log", func() code.GitQuery {
			q := tree(func(*code.TreeQuery) {})
			q.Fresh = statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{
				{Stream: topics.CodeLogStream, Generation: 1, Seq: 9},
				{Stream: topics.TrackerLogStream, Generation: 1, Seq: 4},
			}}
			return q
		}(), "fresh", code.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.q.Validate()
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("Validate = %v, want a valid query", err)
			case tc.want == nil:
			case !errors.Is(err, tc.want):
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			case !strings.Contains(err.Error(), tc.field+":"):
				t.Errorf("Validate = %v, want it to name %s", err, tc.field)
			}
		})
	}
}

// A PATH IS CARRIED WHOLE: as itself where it is UTF-8, and beside its exact
// bytes where it is not — JSON would otherwise answer every such path with
// U+FFFD, and two paths differing only there as one — and read back from
// those bytes exactly.
func TestAPathThatIsNoUTF8IsCarriedAsItsBytes(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"src/main.go", "caf\xe9/x", "nl\nname"} {
		text, b64 := code.WirePath(p)
		back, err := code.GitPath("path", "", b64, false)
		switch {
		case b64 == "" && text != p:
			t.Errorf("WirePath(%q) = %q with no bytes, want itself", p, text)
		case b64 == "":
		case err != nil || back != p:
			t.Errorf("WirePath(%q)'s bytes read back as %q (%v), want the path", p, back, err)
		}
	}
}
