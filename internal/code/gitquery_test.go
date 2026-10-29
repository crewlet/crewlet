package code_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

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

// A HISTORY'S AND A BLAME'S READ IS REFUSED BY FIELD. A log names a commit, a
// clean path or none (every commit), a literal of at most MaxLogGrepBytes that
// is UTF-8 holding no NUL, a page of up to MaxLogLimit commits (none the
// default), and a Since no earlier than the first instant a commit's date can
// name and no later than a day past the node's clock — a year typed wrong is
// told so rather than answered an empty history. A blame names a commit, a
// file (never the root), and a range of at most MaxBlameLines lines counted
// from 1, its end at or after its start; a start with no end is a range all
// the same, however far out. Each refusal names its field.
func TestAHistoryAndABlameAreRefusedByField(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	asker := code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	history := func(m func(q *code.LogQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitLog, RepoID: repo, Asker: asker, Log: &code.LogQuery{Rev: id}}
		m(q.Log)
		return q
	}
	blame := func(m func(q *code.BlameQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitBlame, RepoID: repo, Asker: asker,
			Blame: &code.BlameQuery{Rev: id, Path: "src/a.go"}}
		m(q.Blame)
		return q
	}
	for _, tc := range []struct {
		name  string
		q     code.GitQuery
		field string // "" is a valid query
	}{
		{"every commit", history(func(*code.LogQuery) {}), ""},
		{"a path's", history(func(q *code.LogQuery) { q.Path = "src/--output=x" }), ""},
		{"an abbreviation", history(func(q *code.LogQuery) { q.Rev = id[:7] }), ""},
		{"a literal at its bound", history(func(q *code.LogQuery) {
			q.Grep = strings.Repeat("é", code.MaxLogGrepBytes/2)
		}), ""},
		{"the most commits", history(func(q *code.LogQuery) { q.Limit = code.MaxLogLimit }), ""},
		{"since the epoch", history(func(q *code.LogQuery) { q.Since = time.Unix(0, 0) }), ""},
		{"since later today", history(func(q *code.LogQuery) { q.Since = time.Now().Add(12 * time.Hour) }), ""},
		{"a ref", history(func(q *code.LogQuery) { q.Rev = "main" }), "log.rev"},
		{"a path up", history(func(q *code.LogQuery) { q.Path = "../a" }), "log.path"},
		{"a literal past its bound", history(func(q *code.LogQuery) {
			q.Grep = strings.Repeat("a", code.MaxLogGrepBytes+1)
		}), "log.grep"},
		{"a literal that is no UTF-8", history(func(q *code.LogQuery) { q.Grep = "caf\xe9" }), "log.grep"},
		{"a literal holding a NUL", history(func(q *code.LogQuery) { q.Grep = "a\x00b" }), "log.grep"},
		{"a negative limit", history(func(q *code.LogQuery) { q.Limit = -1 }), "log.limit"},
		{"a limit past the most", history(func(q *code.LogQuery) { q.Limit = code.MaxLogLimit + 1 }), "log.limit"},
		{"since before 1970", history(func(q *code.LogQuery) { q.Since = time.Unix(-1, 0) }), "log.since"},
		{"since a year not yet", history(func(q *code.LogQuery) { q.Since = time.Now().AddDate(1, 0, 0) }),
			"log.since"},
		{"a blame beside it", func() code.GitQuery {
			q := history(func(*code.LogQuery) {})
			q.Blame = &code.BlameQuery{Rev: id, Path: "a"}
			return q
		}(), "blame"},
		{"a file", blame(func(*code.BlameQuery) {}), ""},
		{"the widest range", blame(func(q *code.BlameQuery) { q.StartLine, q.EndLine = 10, 9+code.MaxBlameLines }),
			""},
		{"a start with no end", blame(func(q *code.BlameQuery) { q.StartLine = 1 << 40 }), ""},
		{"the last line an int counts", blame(func(q *code.BlameQuery) { q.StartLine = math.MaxInt }), ""},
		{"no file", blame(func(q *code.BlameQuery) { q.Path = "" }), "blame.path"},
		{"a ref", blame(func(q *code.BlameQuery) { q.Rev = "HEAD" }), "blame.rev"},
		{"a negative start", blame(func(q *code.BlameQuery) { q.StartLine = -1 }), "blame.start_line"},
		{"a negative end", blame(func(q *code.BlameQuery) { q.EndLine = -1 }), "blame.end_line"},
		{"a backwards range", blame(func(q *code.BlameQuery) { q.StartLine, q.EndLine = 9, 3 }), "blame.end_line"},
		{"a line too many", blame(func(q *code.BlameQuery) { q.StartLine, q.EndLine = 10, 10+code.MaxBlameLines }),
			"blame.end_line"},
		{"none of its own", code.GitQuery{Kind: code.GitBlame, RepoID: repo, Asker: asker}, "blame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.q.Validate()
			switch {
			case tc.field == "" && err != nil:
				t.Fatalf("Validate = %v, want a valid query", err)
			case tc.field == "":
			case !errors.Is(err, code.ErrInvalid):
				t.Fatalf("Validate = %v, want ErrInvalid", err)
			case !strings.Contains(err.Error(), tc.field+":"):
				t.Errorf("Validate = %v, want it to name %s", err, tc.field)
			}
		})
	}
}

// A READ OF A CHANGE'S REVISIONS NAMES THEM AS THEIR ROWS DO. A revision's
// facts, a re-anchoring and an interdiff name every commit by the whole id
// its rows recorded — never an abbreviation, which would make an interdiff's
// two bases unequal ids of one commit — a re-anchoring places a bounded list
// of ranges, each named once, and an interdiff's touched lists are the ones
// facts give: within their bounds, beside no word that they touch all, and
// clean paths. Each refusal names its field.
func TestAReadOfAChangesRevisionsIsRefusedByField(t *testing.T) {
	t.Parallel()
	const a, b = "0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"
	asker := code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	// A REVISION'S FACTS ARE THE ENGINE'S OWN READ, its repository's home's,
	// which names no asker.
	facts := func(m func(q *code.RevisionFactsQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitRevisionFacts, RepoID: repo,
			RevisionFacts: &code.RevisionFactsQuery{Head: a, Target: b}}
		m(q.RevisionFacts)
		return q
	}
	asked := facts(func(*code.RevisionFactsQuery) {})
	asked.Asker = asker
	ceiling := facts(func(*code.RevisionFactsQuery) {})
	ceiling.CeilingSeat = "dev"
	anchor := code.LineAnchor{ID: "f-1", Path: "src/a.go", Side: code.SideNew, BlobOID: b, Start: 3, End: 4}
	reanchor := func(m func(q *code.ReanchorQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitReanchor, RepoID: repo, Asker: asker,
			Reanchor: &code.ReanchorQuery{Head: a, Anchors: []code.LineAnchor{anchor}}}
		m(q.Reanchor)
		return q
	}
	interdiff := func(m func(q *code.InterdiffQuery)) code.GitQuery {
		q := code.GitQuery{Kind: code.GitInterdiff, RepoID: repo, Asker: asker,
			Interdiff: &code.InterdiffQuery{FromBase: a, FromHead: b, ToBase: b, ToHead: a,
				FromTouched: []string{"a*"}, ToTouched: []string{"src/a.go"}}}
		m(q.Interdiff)
		return q
	}
	many := func(n int) []code.LineAnchor {
		out := make([]code.LineAnchor, n)
		for i := range out {
			out[i] = anchor
			out[i].ID = strings.Repeat("x", i+1)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		q     code.GitQuery
		field string // "" is a valid query
	}{
		{"facts", facts(func(*code.RevisionFactsQuery) {}), ""},
		{"facts a principal asks", asked, "asker"},
		{"facts read for a seat", ceiling, "asker"},
		{"facts of an abbreviation", facts(func(q *code.RevisionFactsQuery) { q.Head = a[:7] }),
			"revision_facts.head"},
		{"facts against the zero id", facts(func(q *code.RevisionFactsQuery) { q.Target = code.ZeroOID }),
			"revision_facts.target"},
		{"an anchor", reanchor(func(*code.ReanchorQuery) {}), ""},
		{"an anchor on the old side", reanchor(func(q *code.ReanchorQuery) {
			q.Base, q.Anchors[0].Side = b, code.SideOld
		}), ""},
		{"the most anchors", reanchor(func(q *code.ReanchorQuery) { q.Anchors = many(code.MaxReanchors) }), ""},
		{"no anchor", reanchor(func(q *code.ReanchorQuery) { q.Anchors = nil }), "reanchor.anchors"},
		{"an anchor too many", reanchor(func(q *code.ReanchorQuery) { q.Anchors = many(code.MaxReanchors + 1) }),
			"reanchor.anchors"},
		{"an old side with no base", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].Side = code.SideOld }),
			"reanchor.anchors[0].side"},
		{"no side", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].Side = "" }), "reanchor.anchors[0].side"},
		{"one id twice", reanchor(func(q *code.ReanchorQuery) {
			q.Anchors = []code.LineAnchor{anchor, anchor}
		}), "reanchor.anchors[1].id"},
		{"no id", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].ID = "" }), "reanchor.anchors[0].id"},
		{"a backwards range", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].Start = 5 }),
			"reanchor.anchors[0]"},
		{"line zero", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].Start = 0 }), "reanchor.anchors[0]"},
		{"an unclean path", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].Path = "src/../a.go" }),
			"reanchor.anchors[0].path"},
		{"an abbreviated blob", reanchor(func(q *code.ReanchorQuery) { q.Anchors[0].BlobOID = b[:9] }),
			"reanchor.anchors[0].blob_oid"},
		{"an abbreviated base", reanchor(func(q *code.ReanchorQuery) { q.Base = b[:8] }), "reanchor.base"},
		{"an interdiff", interdiff(func(*code.InterdiffQuery) {}), ""},
		{"an interdiff of no merge base", interdiff(func(q *code.InterdiffQuery) { q.FromBase = "" }), ""},
		{"an interdiff touching all", interdiff(func(q *code.InterdiffQuery) {
			q.ToTouched, q.ToTouchesAll = nil, true
		}), ""},
		{"a list beside touching all", interdiff(func(q *code.InterdiffQuery) { q.ToTouchesAll = true }),
			"interdiff.to_touched"},
		{"a list past its paths", interdiff(func(q *code.InterdiffQuery) {
			q.FromTouched = make([]string, code.MaxTouchedPaths+1)
			for i := range q.FromTouched {
				q.FromTouched[i] = "p"
			}
		}), "interdiff.from_touched"},
		// Nine paths of a thousand `<` are some nine kilobytes as bytes and
		// past the bound as JSON, which writes each `<` in six.
		{"a list past its bytes", interdiff(func(q *code.InterdiffQuery) {
			for i := range 9 {
				q.FromTouched = append(q.FromTouched, fmt.Sprintf("%d%s", i, strings.Repeat("<", 999)))
			}
		}), "interdiff.from_touched"},
		{"an unclean touched path", interdiff(func(q *code.InterdiffQuery) { q.ToTouched = []string{"./a"} }),
			"interdiff.to_touched[0]"},
		{"an abbreviated head", interdiff(func(q *code.InterdiffQuery) { q.ToHead = a[:12] }), "interdiff.to_head"},
		{"a file", interdiff(func(q *code.InterdiffQuery) { q.File = "a*" }), ""},
		{"a file from the work tree", interdiff(func(q *code.InterdiffQuery) { q.File = "./a" }), "interdiff.file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.q.Validate()
			switch {
			case tc.field == "" && err != nil:
				t.Fatalf("Validate = %v, want a valid query", err)
			case tc.field == "":
			case !errors.Is(err, code.ErrInvalid):
				t.Fatalf("Validate = %v, want ErrInvalid", err)
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
