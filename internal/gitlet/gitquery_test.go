package gitlet_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A READ BY OBJECT ID NAMES OBJECTS, NEVER REVISIONS, AND PATHS NO GRAMMAR
// READS. Every revision is a whole id or an abbreviation of git's default
// length or more — never a ref, `^`, `~` or `:`, which would hand git's
// revision grammar to the Gitlet node, nor fewer digits than name one object of a
// large repository — every path is clean and relative (git reads `./` and
// `../` as relative to a work tree, which dies over a bare repository's whole
// batch), and each refusal names its field.
func TestAGitQueryIsRefusedByField(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	tree := func(m func(q *gitlet.TreeQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo, Asker: asker, Tree: &gitlet.TreeQuery{Rev: id}}
		m(q.Tree)
		return q
	}
	for _, tc := range []struct {
		name  string
		q     gitlet.GitQuery
		field string // "" is a valid query
		want  error
	}{
		{"a whole id", tree(func(*gitlet.TreeQuery) {}), "", nil},
		{"an abbreviation", tree(func(q *gitlet.TreeQuery) { q.Rev = id[:7] }), "", nil},
		{"a longer abbreviation", tree(func(q *gitlet.TreeQuery) { q.Rev = id[:39] }), "", nil},
		{"a path", tree(func(q *gitlet.TreeQuery) { q.Path = "src/a b/--output=x" }), "", nil},
		{"a path as bytes", tree(func(q *gitlet.TreeQuery) {
			q.PathB64 = base64.StdEncoding.EncodeToString([]byte("caf\xe9"))
		}), "", nil},
		{"a short abbreviation", tree(func(q *gitlet.TreeQuery) { q.Rev = id[:6] }), "tree.rev", gitlet.ErrInvalid},
		{"a ref", tree(func(q *gitlet.TreeQuery) { q.Rev = "main" }), "tree.rev", gitlet.ErrInvalid},
		{"a revision expression", tree(func(q *gitlet.TreeQuery) { q.Rev = id + "^" }), "tree.rev", gitlet.ErrInvalid},
		{"upper case", tree(func(q *gitlet.TreeQuery) { q.Rev = strings.ToUpper(id) }), "tree.rev", gitlet.ErrInvalid},
		{"the zero id", tree(func(q *gitlet.TreeQuery) { q.Rev = gitlet.ZeroOID }), "tree.rev", gitlet.ErrInvalid},
		{"a path from the work tree", tree(func(q *gitlet.TreeQuery) { q.Path = "./src" }), "tree.path",
			gitlet.ErrInvalid},
		{"a path up", tree(func(q *gitlet.TreeQuery) { q.Path = "../src" }), "tree.path", gitlet.ErrInvalid},
		{"a dot", tree(func(q *gitlet.TreeQuery) { q.Path = "." }), "tree.path", gitlet.ErrInvalid},
		{"an absolute path", tree(func(q *gitlet.TreeQuery) { q.Path = "/etc" }), "tree.path", gitlet.ErrInvalid},
		{"a trailing slash", tree(func(q *gitlet.TreeQuery) { q.Path = "src/" }), "tree.path", gitlet.ErrInvalid},
		{"a doubled slash", tree(func(q *gitlet.TreeQuery) { q.Path = "src//a" }), "tree.path", gitlet.ErrInvalid},
		{"a NUL", tree(func(q *gitlet.TreeQuery) { q.Path = "a\x00b" }), "tree.path", gitlet.ErrInvalid},
		{"a path too long", tree(func(q *gitlet.TreeQuery) { q.Path = strings.Repeat("a", gitlet.MaxGitPathBytes+1) }),
			"tree.path", gitlet.ErrInvalid},
		{"both spellings of a path", tree(func(q *gitlet.TreeQuery) { q.Path, q.PathB64 = "a", "YQ==" }), "tree.path",
			gitlet.ErrInvalid},
		{"bytes that are no base64", tree(func(q *gitlet.TreeQuery) { q.PathB64 = "!!" }), "tree.path_b64",
			gitlet.ErrInvalid},
		{"no asker", func() gitlet.GitQuery { q := tree(func(*gitlet.TreeQuery) {}); q.Asker = gitlet.Principal{}; return q }(),
			"asker", gitlet.ErrInvalid},
		{"no repository", func() gitlet.GitQuery { q := tree(func(*gitlet.TreeQuery) {}); q.RepoID = "web"; return q }(),
			"repo_id", gitlet.ErrInvalid},
		{"no kind", func() gitlet.GitQuery { q := tree(func(*gitlet.TreeQuery) {}); q.Kind = "grep"; return q }(),
			"kind", gitlet.ErrInvalid},
		{"another kind's query", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Blob = &gitlet.BlobQuery{Rev: id}
			return q
		}(), "blob", gitlet.ErrInvalid},
		{"none of its own", gitlet.GitQuery{Kind: gitlet.GitBlob, RepoID: repo, Asker: asker}, "blob", gitlet.ErrInvalid},
		{"a backwards range", gitlet.GitQuery{Kind: gitlet.GitBlob, RepoID: repo, Asker: asker,
			Blob: &gitlet.BlobQuery{Rev: id, Path: "a", StartLine: 9, EndLine: 3}}, "blob.end_line", gitlet.ErrInvalid},
		{"a negative line", gitlet.GitQuery{Kind: gitlet.GitBlob, RepoID: repo, Asker: asker,
			Blob: &gitlet.BlobQuery{Rev: id, Path: "a", StartLine: -1}}, "blob.start_line", gitlet.ErrInvalid},
		{"a file and paths", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, Paths: []string{"a"}, File: "a"}}, "diff.file", gitlet.ErrInvalid},
		{"every patch under paths", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, Paths: []string{"a"}, Patches: true}}, "", nil},
		{"every patch and a file", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, File: "a", Patches: true}}, "diff.patches", gitlet.ErrInvalid},
		{"every patch and a file's bytes", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, FileB64: "YQ==", Patches: true}}, "diff.patches",
			gitlet.ErrInvalid},
		{"too many paths", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, Paths: make([]string, gitlet.MaxDiffPaths+1)}}, "diff.paths",
			gitlet.ErrInvalid},
		{"an unclean filter", gitlet.GitQuery{Kind: gitlet.GitDiff, RepoID: repo, Asker: asker,
			Diff: &gitlet.DiffQuery{Base: id, Head: id, Paths: []string{"a/../b"}}}, "diff.paths[0]", gitlet.ErrInvalid},
		{"a comparison of a ref", gitlet.GitQuery{Kind: gitlet.GitCompare, RepoID: repo, Asker: asker,
			Compare: &gitlet.CompareQuery{Base: id, Head: "HEAD"}}, "compare.head", gitlet.ErrInvalid},
		{"a cursor longer than any", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Cursor = strings.Repeat("9", 385)
			return q
		}(), "cursor", gitlet.ErrBadCursor},
		// ONLY A SEAT READS FOR ANOTHER SEAT, named by its handle: a person's
		// read and a token's are their own, which no ceiling bounds.
		{"a seat's read for another seat", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Asker, q.CeilingSeat = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}, "lead"
			return q
		}(), "", nil},
		{"a token's read for a seat", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.CeilingSeat = "lead"
			return q
		}(), "ceiling_seat", gitlet.ErrInvalid},
		{"a seat's read for no handle", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Asker, q.CeilingSeat = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}, "Not A Handle"
			return q
		}(), "ceiling_seat", gitlet.ErrInvalid},
		// THE ASKER'S FLOOR IS ON THE CODE LOG: a copy is brought along it
		// alone, and a floor on another log beside one there would be dropped.
		{"a floor on the code log", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Fresh = statelog.FloorAt(statelog.ReadSession,
				statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9})
			return q
		}(), "", nil},
		{"a floor on another log", func() gitlet.GitQuery {
			q := tree(func(*gitlet.TreeQuery) {})
			q.Fresh = statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{
				{Stream: topics.CodeLogStream, Generation: 1, Seq: 9},
				{Stream: topics.TrackerLogStream, Generation: 1, Seq: 4},
			}}
			return q
		}(), "fresh", gitlet.ErrInvalid},
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
// name and no later than a day past the node's clock (a year typed wrong is
// told so rather than answered an empty history). An Until is after the epoch,
// after a Since and no later than a day ahead, and a PushedBy is a seat's
// handle, whether a Since is set or not. A blame names a commit, a file (never
// the root), and a range of at most MaxBlameLines lines counted from 1, its end
// at or after its start; a start with no end is a range all the same, however
// far out. Each refusal names its field.
//
// Mutation: the Until and PushedBy arms inside the Since switch → accepted with no since, red.
func TestAHistoryAndABlameAreRefusedByField(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	history := func(m func(q *gitlet.LogQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitLog, RepoID: repo, Asker: asker, Log: &gitlet.LogQuery{Rev: id}}
		m(q.Log)
		return q
	}
	blame := func(m func(q *gitlet.BlameQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitBlame, RepoID: repo, Asker: asker,
			Blame: &gitlet.BlameQuery{Rev: id, Path: "src/a.go"}}
		m(q.Blame)
		return q
	}
	for _, tc := range []struct {
		name  string
		q     gitlet.GitQuery
		field string // "" is a valid query
	}{
		{"every commit", history(func(*gitlet.LogQuery) {}), ""},
		{"a path's", history(func(q *gitlet.LogQuery) { q.Path = "src/--output=x" }), ""},
		{"an abbreviation", history(func(q *gitlet.LogQuery) { q.Rev = id[:7] }), ""},
		{"a literal at its bound", history(func(q *gitlet.LogQuery) {
			q.Grep = strings.Repeat("é", gitlet.MaxLogGrepBytes/2)
		}), ""},
		{"the most commits", history(func(q *gitlet.LogQuery) { q.Limit = gitlet.MaxLogLimit }), ""},
		{"since the epoch", history(func(q *gitlet.LogQuery) { q.Since = time.Unix(0, 0) }), ""},
		{"since later today", history(func(q *gitlet.LogQuery) { q.Since = time.Now().Add(12 * time.Hour) }), ""},
		{"until a second after the epoch", history(func(q *gitlet.LogQuery) { q.Until = time.Unix(1, 0) }), ""},
		{"until later today", history(func(q *gitlet.LogQuery) { q.Until = time.Now().Add(12 * time.Hour) }), ""},
		{"a window", history(func(q *gitlet.LogQuery) {
			q.Since, q.Until = time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 1)
		}), ""},
		{"a seat's handle", history(func(q *gitlet.LogQuery) { q.PushedBy = "ceo" }), ""},
		{"a ref", history(func(q *gitlet.LogQuery) { q.Rev = "main" }), "log.rev"},
		{"a path up", history(func(q *gitlet.LogQuery) { q.Path = "../a" }), "log.path"},
		{"a literal past its bound", history(func(q *gitlet.LogQuery) {
			q.Grep = strings.Repeat("a", gitlet.MaxLogGrepBytes+1)
		}), "log.grep"},
		{"a literal that is no UTF-8", history(func(q *gitlet.LogQuery) { q.Grep = "caf\xe9" }), "log.grep"},
		{"a literal holding a NUL", history(func(q *gitlet.LogQuery) { q.Grep = "a\x00b" }), "log.grep"},
		{"a negative limit", history(func(q *gitlet.LogQuery) { q.Limit = -1 }), "log.limit"},
		{"a limit past the most", history(func(q *gitlet.LogQuery) { q.Limit = gitlet.MaxLogLimit + 1 }), "log.limit"},
		{"since before 1970", history(func(q *gitlet.LogQuery) { q.Since = time.Unix(-1, 0) }), "log.since"},
		{"since a year not yet", history(func(q *gitlet.LogQuery) { q.Since = time.Now().AddDate(1, 0, 0) }),
			"log.since"},
		{"until at the epoch", history(func(q *gitlet.LogQuery) { q.Until = time.Unix(0, 0) }), "log.until"},
		{"until before 1970", history(func(q *gitlet.LogQuery) { q.Until = time.Unix(-1, 0) }), "log.until"},
		{"until a year not yet", history(func(q *gitlet.LogQuery) { q.Until = time.Now().AddDate(1, 0, 0) }),
			"log.until"},
		{"until at since", history(func(q *gitlet.LogQuery) {
			q.Since, q.Until = time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 0)
		}), "log.until"},
		{"until before since", history(func(q *gitlet.LogQuery) {
			q.Since, q.Until = time.Unix(1_700_000_000, 0), time.Unix(1_600_000_000, 0)
		}), "log.until"},
		{"a pushed_by that is no handle", history(func(q *gitlet.LogQuery) { q.PushedBy = "Not A Seat" }),
			"log.pushed_by"},
		{"a pushed_by that is no handle, beside a since", history(func(q *gitlet.LogQuery) {
			q.Since, q.PushedBy = time.Unix(1_700_000_000, 0), "Not A Seat"
		}), "log.pushed_by"},
		{"a blame beside it", func() gitlet.GitQuery {
			q := history(func(*gitlet.LogQuery) {})
			q.Blame = &gitlet.BlameQuery{Rev: id, Path: "a"}
			return q
		}(), "blame"},
		{"a file", blame(func(*gitlet.BlameQuery) {}), ""},
		{"the widest range", blame(func(q *gitlet.BlameQuery) { q.StartLine, q.EndLine = 10, 9+gitlet.MaxBlameLines }),
			""},
		{"a start with no end", blame(func(q *gitlet.BlameQuery) { q.StartLine = 1 << 40 }), ""},
		{"the last line an int counts", blame(func(q *gitlet.BlameQuery) { q.StartLine = math.MaxInt }), ""},
		{"no file", blame(func(q *gitlet.BlameQuery) { q.Path = "" }), "blame.path"},
		{"a ref", blame(func(q *gitlet.BlameQuery) { q.Rev = "HEAD" }), "blame.rev"},
		{"a negative start", blame(func(q *gitlet.BlameQuery) { q.StartLine = -1 }), "blame.start_line"},
		{"a negative end", blame(func(q *gitlet.BlameQuery) { q.EndLine = -1 }), "blame.end_line"},
		{"a backwards range", blame(func(q *gitlet.BlameQuery) { q.StartLine, q.EndLine = 9, 3 }), "blame.end_line"},
		{"a line too many", blame(func(q *gitlet.BlameQuery) { q.StartLine, q.EndLine = 10, 10+gitlet.MaxBlameLines }),
			"blame.end_line"},
		{"none of its own", gitlet.GitQuery{Kind: gitlet.GitBlame, RepoID: repo, Asker: asker}, "blame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.q.Validate()
			switch {
			case tc.field == "" && err != nil:
				t.Fatalf("Validate = %v, want a valid query", err)
			case tc.field == "":
			case !errors.Is(err, gitlet.ErrInvalid):
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
// of ranges, each named once and within the lines a file can hold
// (gitlet.MaxAnchorLine), and an interdiff's touched lists are the ones
// facts give: within their bounds, beside no word that they touch all, and
// clean paths. Each refusal names its field.
//
// Mutation: drop the bound on an anchor's last line — the line past any
// file's is admitted, red.
func TestAReadOfAChangesRevisionsIsRefusedByField(t *testing.T) {
	t.Parallel()
	const a, b = "0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	// A REVISION'S FACTS ARE THE ENGINE'S OWN READ, its repository's home's,
	// which names no asker.
	facts := func(m func(q *gitlet.RevisionFactsQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitRevisionFacts, RepoID: repo,
			RevisionFacts: &gitlet.RevisionFactsQuery{Head: a, Target: b}}
		m(q.RevisionFacts)
		return q
	}
	asked := facts(func(*gitlet.RevisionFactsQuery) {})
	asked.Asker = asker
	ceiling := facts(func(*gitlet.RevisionFactsQuery) {})
	ceiling.CeilingSeat = "dev"
	anchor := gitlet.LineAnchor{ID: "f-1", Path: "src/a.go", Side: gitlet.SideNew, BlobOID: b, Start: 3, End: 4}
	reanchor := func(m func(q *gitlet.ReanchorQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitReanchor, RepoID: repo, Asker: asker,
			Reanchor: &gitlet.ReanchorQuery{Head: a, Anchors: []gitlet.LineAnchor{anchor}}}
		m(q.Reanchor)
		return q
	}
	interdiff := func(m func(q *gitlet.InterdiffQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitInterdiff, RepoID: repo, Asker: asker,
			Interdiff: &gitlet.InterdiffQuery{FromBase: a, FromHead: b, ToBase: b, ToHead: a,
				FromTouched: []string{"a*"}, ToTouched: []string{"src/a.go"}}}
		m(q.Interdiff)
		return q
	}
	excerpts := func(m func(q *gitlet.ExcerptsQuery)) gitlet.GitQuery {
		q := gitlet.GitQuery{Kind: gitlet.GitExcerpts, RepoID: repo, Asker: asker,
			Excerpts: &gitlet.ExcerptsQuery{Anchors: []gitlet.ExcerptAnchor{{LineAnchor: anchor, Base: a, Head: b}}}}
		m(q.Excerpts)
		return q
	}
	excerpted := func(n int) []gitlet.ExcerptAnchor {
		out := make([]gitlet.ExcerptAnchor, n)
		for i := range out {
			out[i] = gitlet.ExcerptAnchor{LineAnchor: anchor, Head: b}
			out[i].ID = fmt.Sprintf("f-%d", i)
		}
		return out
	}
	many := func(n int) []gitlet.LineAnchor {
		out := make([]gitlet.LineAnchor, n)
		for i := range out {
			out[i] = anchor
			out[i].ID = strings.Repeat("x", i+1)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		q     gitlet.GitQuery
		field string // "" is a valid query
	}{
		{"facts", facts(func(*gitlet.RevisionFactsQuery) {}), ""},
		{"facts a principal asks", asked, "asker"},
		{"facts read for a seat", ceiling, "asker"},
		{"facts of an abbreviation", facts(func(q *gitlet.RevisionFactsQuery) { q.Head = a[:7] }),
			"revision_facts.head"},
		{"facts against the zero id", facts(func(q *gitlet.RevisionFactsQuery) { q.Target = gitlet.ZeroOID }),
			"revision_facts.target"},
		{"an anchor", reanchor(func(*gitlet.ReanchorQuery) {}), ""},
		{"an anchor on the old side", reanchor(func(q *gitlet.ReanchorQuery) {
			q.Base, q.Anchors[0].Side = b, gitlet.SideOld
		}), ""},
		{"the most anchors", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors = many(gitlet.MaxReanchors) }), ""},
		{"no anchor", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors = nil }), "reanchor.anchors"},
		{"an anchor too many", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors = many(gitlet.MaxReanchors + 1) }),
			"reanchor.anchors"},
		{"an old side with no base", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].Side = gitlet.SideOld }),
			"reanchor.anchors[0].side"},
		{"no side", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].Side = "" }), "reanchor.anchors[0].side"},
		{"one id twice", reanchor(func(q *gitlet.ReanchorQuery) {
			q.Anchors = []gitlet.LineAnchor{anchor, anchor}
		}), "reanchor.anchors[1].id"},
		{"no id", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].ID = "" }), "reanchor.anchors[0].id"},
		{"a backwards range", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].Start = 5 }),
			"reanchor.anchors[0]"},
		{"line zero", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].Start = 0 }), "reanchor.anchors[0]"},
		{"the last line a file holds", reanchor(func(q *gitlet.ReanchorQuery) {
			q.Anchors[0].Start, q.Anchors[0].End = gitlet.MaxAnchorLine, gitlet.MaxAnchorLine
		}), ""},
		{"a line past any file's", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].End = gitlet.MaxAnchorLine + 1 }),
			"reanchor.anchors[0]"},
		{"an unclean path", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].Path = "src/../a.go" }),
			"reanchor.anchors[0].path"},
		{"an abbreviated blob", reanchor(func(q *gitlet.ReanchorQuery) { q.Anchors[0].BlobOID = b[:9] }),
			"reanchor.anchors[0].blob_oid"},
		{"an abbreviated base", reanchor(func(q *gitlet.ReanchorQuery) { q.Base = b[:8] }), "reanchor.base"},
		{"an interdiff", interdiff(func(*gitlet.InterdiffQuery) {}), ""},
		{"an interdiff of no merge base", interdiff(func(q *gitlet.InterdiffQuery) { q.FromBase = "" }), ""},
		{"an interdiff touching all", interdiff(func(q *gitlet.InterdiffQuery) {
			q.ToTouched, q.ToTouchesAll = nil, true
		}), ""},
		{"a list beside touching all", interdiff(func(q *gitlet.InterdiffQuery) { q.ToTouchesAll = true }),
			"interdiff.to_touched"},
		{"a list past its paths", interdiff(func(q *gitlet.InterdiffQuery) {
			q.FromTouched = make([]string, gitlet.MaxTouchedPaths+1)
			for i := range q.FromTouched {
				q.FromTouched[i] = "p"
			}
		}), "interdiff.from_touched"},
		// Nine paths of a thousand `<` are some nine kilobytes as bytes and
		// past the bound as JSON, which writes each `<` in six.
		{"a list past its bytes", interdiff(func(q *gitlet.InterdiffQuery) {
			for i := range 9 {
				q.FromTouched = append(q.FromTouched, fmt.Sprintf("%d%s", i, strings.Repeat("<", 999)))
			}
		}), "interdiff.from_touched"},
		{"an unclean touched path", interdiff(func(q *gitlet.InterdiffQuery) { q.ToTouched = []string{"./a"} }),
			"interdiff.to_touched[0]"},
		{"an abbreviated head", interdiff(func(q *gitlet.InterdiffQuery) { q.ToHead = a[:12] }), "interdiff.to_head"},
		{"a file", interdiff(func(q *gitlet.InterdiffQuery) { q.File = "a*" }), ""},
		{"a file from the work tree", interdiff(func(q *gitlet.InterdiffQuery) { q.File = "./a" }), "interdiff.file"},
		{"every patch", interdiff(func(q *gitlet.InterdiffQuery) { q.Patches = true }), ""},
		{"every patch and a file", interdiff(func(q *gitlet.InterdiffQuery) { q.Patches, q.File = true, "a*" }),
			"interdiff.patches"},
		{"an excerpt", excerpts(func(*gitlet.ExcerptsQuery) {}), ""},
		{"an excerpt of a revision with no base", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Base = "" }),
			""},
		{"the most excerpts at the widest context", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Anchors, q.Context = excerpted(gitlet.MaxExcerptAnchors), new(gitlet.MaxExcerptContext)
		}), ""},
		{"no excerpt", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors = nil }), "excerpts.anchors"},
		{"an excerpt too many", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Anchors = excerpted(gitlet.MaxExcerptAnchors + 1)
		}), "excerpts.anchors"},
		{"one excerpt id twice", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors = append(q.Anchors, q.Anchors[0]) }),
			"excerpts.anchors[1].id"},
		{"an excerpt id past its bytes", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Anchors[0].ID = strings.Repeat("x", 65)
		}), "excerpts.anchors[0].id"},
		{"an old side excerpt with no base", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Anchors[0].Side, q.Anchors[0].Base = gitlet.SideOld, ""
		}), "excerpts.anchors[0].side"},
		{"an excerpt of an abbreviated head", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Head = b[:7] }),
			"excerpts.anchors[0].head"},
		{"an excerpt of an abbreviated base", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Base = a[:7] }),
			"excerpts.anchors[0].base"},
		{"an excerpt of the zero blob", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Anchors[0].BlobOID = gitlet.ZeroOID
		}), "excerpts.anchors[0].blob_oid"},
		{"an excerpt from line zero", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Start = 0 }),
			"excerpts.anchors[0]"},
		{"a backwards excerpt", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].End = 2 }),
			"excerpts.anchors[0]"},
		{"an excerpt of an unclean path", excerpts(func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Path = "./a" }),
			"excerpts.anchors[0].path"},
		{"no context", excerpts(func(q *gitlet.ExcerptsQuery) { q.Context = new(0) }), ""},
		{"a context below none", excerpts(func(q *gitlet.ExcerptsQuery) { q.Context = new(-1) }), "excerpts.context"},
		{"a context past the widest", excerpts(func(q *gitlet.ExcerptsQuery) {
			q.Context = new(gitlet.MaxExcerptContext + 1)
		}), "excerpts.context"},
		{"an excerpts read carrying a re-anchoring", func() gitlet.GitQuery {
			q := excerpts(func(*gitlet.ExcerptsQuery) {})
			q.Reanchor = &gitlet.ReanchorQuery{Head: a, Anchors: []gitlet.LineAnchor{anchor}}
			return q
		}(), "reanchor"},
		{"an unstamped excerpts read", func() gitlet.GitQuery {
			q := excerpts(func(*gitlet.ExcerptsQuery) {})
			q.Unstamped = true
			return q
		}(), "unstamped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.q.Validate()
			switch {
			case tc.field == "" && err != nil:
				t.Fatalf("Validate = %v, want a valid query", err)
			case tc.field == "":
			case !errors.Is(err, gitlet.ErrInvalid):
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
		text, b64 := gitlet.WirePath(p)
		back, err := gitlet.GitPath("path", "", b64, false)
		switch {
		case b64 == "" && text != p:
			t.Errorf("WirePath(%q) = %q with no bytes, want itself", p, text)
		case b64 == "":
		case err != nil || back != p:
			t.Errorf("WirePath(%q)'s bytes read back as %q (%v), want the path", p, back, err)
		}
	}
}

// A SEARCH'S BOUNDS ARE REFUSED BY FIELD before anything is read: a context
// past gitlet.MaxSearchContext or below none, a per-file bound past
// gitlet.MaxSearchHits or below none, and a limit past
// gitlet.MaxSearchContextHits beside context, which a limit alone may pass.
//
// Mutation: drop the context bound (ValidateBounds), red.
func TestASearchsBoundsAreRefused(t *testing.T) {
	t.Parallel()
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	search := func(s gitlet.SearchQuery) gitlet.GitQuery {
		s.Text, s.Mode = "needle", "literal"
		return gitlet.GitQuery{Kind: gitlet.GitSearch, RepoID: "01920000-0000-7000-8000-000000000001",
			Asker: asker, Search: &s}
	}
	for _, tc := range []struct {
		q     gitlet.SearchQuery
		field string // "" is a valid search
	}{
		{gitlet.SearchQuery{Context: -1}, "search.context"},
		{gitlet.SearchQuery{Context: gitlet.MaxSearchContext + 1}, "search.context"},
		{gitlet.SearchQuery{PerFile: -1}, "search.per_file"},
		{gitlet.SearchQuery{PerFile: gitlet.MaxSearchHits + 1}, "search.per_file"},
		{gitlet.SearchQuery{Limit: gitlet.MaxSearchContextHits + 1, Context: 1}, "search.limit"},
		{gitlet.SearchQuery{Limit: gitlet.MaxSearchContextHits + 1}, ""},
		{gitlet.SearchQuery{Limit: gitlet.MaxSearchContextHits, Context: gitlet.MaxSearchContext,
			PerFile: gitlet.MaxSearchHits}, ""},
	} {
		err := search(tc.q).Validate()
		switch {
		case tc.field == "" && err != nil:
			t.Errorf("%+v refused: %v", tc.q, err)
		case tc.field != "" && (!errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(fmt.Sprint(err), tc.field+":")):
			t.Errorf("%+v answered %v, want invalid naming %s", tc.q, err, tc.field)
		}
	}
	if got := (gitlet.SearchQuery{Context: 1}).Hits(); got != gitlet.MaxSearchContextHits {
		t.Errorf("a search asking context with no limit answers %d hits, want %d", got, gitlet.MaxSearchContextHits)
	}
}
