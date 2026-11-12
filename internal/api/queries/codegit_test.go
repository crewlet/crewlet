package queries_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// whole, other, forked, abbrev and the two positions are what these cases
// name: two commits' whole ids, the one both forked from, an abbreviation of
// the first, and where a ref last moved and the node's own write landed on
// the code log.
const (
	whole  = "1111111111111111111111111111111111111111"
	other  = "2222222222222222222222222222222222222222"
	forked = "3333333333333333333333333333333333333333"
	abbrev = "1111111"
)

var (
	moved = statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 40}
	wrote = statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 12}
)

// gitQuestions is every question read by object id, with the arguments that
// name its revisions by whatever ref names: a function of the spelling, so a
// case asks each by whole ids, by refs and by an abbreviation alike.
var gitQuestions = []struct {
	what string
	args func(rev string) map[string]any
}{
	{"code_tree", func(rev string) map[string]any { return map[string]any{"name": "web", "ref": rev} }},
	{"code_blob", func(rev string) map[string]any {
		return map[string]any{"name": "web", "ref": rev, "path": "go.mod"}
	}},
	{"code_log", func(rev string) map[string]any { return map[string]any{"name": "web", "ref": rev} }},
	{"code_blame", func(rev string) map[string]any {
		return map[string]any{"name": "web", "ref": rev, "path": "go.mod"}
	}},
	{"code_diff", func(rev string) map[string]any {
		return map[string]any{"name": "web", "base": rev, "head": rev}
	}},
	{"code_compare", func(rev string) map[string]any {
		return map[string]any{"name": "web", "base": rev, "head": rev}
	}},
}

// stamping is a Gitlet node that stamps every answer whatever it was asked —
// the copy's position, the push behind its rev, what its index covers — so a
// case sees that a question never carries a stamp into an answer it calls
// immutable, whoever answered it.
func stamping(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	a := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Rev: whole, Position: moved,
		PushedBy: map[string]gitlet.PushedBy{whole: {PushID: "p1", Position: moved}},
		Coverage: &gitlet.Coverage{Complete: true}}
	switch q.Kind {
	case gitlet.GitTree:
		a.Tree = &gitlet.TreeAnswer{Tree: whole}
	case gitlet.GitCompare:
		a.Compare = &gitlet.CompareAnswer{Base: whole, Head: whole}
	}
	return a, nil
}

// AN OID QUESTION IS IMMUTABLE. A question every revision of which is a
// whole object id is answered queries.Immutable — sent with a strong entity
// tag and `Cache-Control: immutable` — and asks its Gitlet node for the page
// alone (gitlet.GitQuery.Unstamped), so the page is a function of what it names;
// and what it carries is the page and nothing a node stamped — no position,
// no push, no coverage, no repository id, no resolved ref — even from a node
// that stamped them. Asked by a ref it is answered bare, carrying the ref it
// resolved and what the read stamped, and so is one asked by an abbreviation,
// which whichever object it finds now answers; neither is asked unstamped. A
// comparison whose merge its home could not decide is answered bare too: a
// later read may decide it.
//
// Mutation: answer Immutable whatever the spelling (read's whole check) — the
// ref's answer is cached for a year, red; leave Unstamped unset — the oid's
// page is cut against the stamps, red; copy the stamps onto an immutable page
// — red.
func TestAnOidQuestionIsImmutable(t *testing.T) {
	t.Parallel()
	for _, q := range gitQuestions {
		t.Run(q.what, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				spelled   string
				immutable bool
			}{{whole, true}, {"main", false}, {abbrev, false}, {"HEAD", false}} {
				stub := &stubCode{git: stamping}
				answer, err := askNative(t, queries.Sources{Code: stub}, q.what, q.args(tc.spelled))
				if err != nil {
					t.Fatalf("%s by %s: %v", q.what, tc.spelled, err)
				}
				fixed, immutable := answer.(queries.Immutable)
				if immutable != tc.immutable {
					t.Errorf("%s by %s answered immutable %t, want %t", q.what, tc.spelled, immutable,
						tc.immutable)
				}
				if len(stub.read) != 1 || stub.read[0].Unstamped != tc.immutable {
					t.Fatalf("%s by %s asked %d reads by object id, unstamped %v; want one, unstamped %t",
						q.what, tc.spelled, len(stub.read), stub.read, tc.immutable)
				}
				encoded, err := json.Marshal(answer)
				if err != nil {
					t.Fatal(err)
				}
				stamped := false
				for _, key := range []string{`"position"`, `"pushed_by"`, `"coverage"`, `"repo_id"`, `"resolved"`} {
					stamped = stamped || strings.Contains(string(encoded), key)
				}
				if immutable && stamped {
					t.Errorf("%s by %s is immutable and carries a stamp: %s", q.what, tc.spelled, encoded)
				}
				if !immutable && (!strings.Contains(string(encoded), `"position"`) ||
					!strings.Contains(string(encoded), `"pushed_by"`)) {
					t.Errorf("%s by %s is answered bare without what its read stamped: %s", q.what,
						tc.spelled, encoded)
				}
				if immutable {
					page, err := json.Marshal(fixed.Answer)
					if err != nil || string(page) != string(encoded) {
						t.Errorf("an immutable answer encodes as %s, want what it wraps, %s", encoded, page)
					}
				}
				if refs := len(stub.resolved) > 0; refs != (tc.spelled == "main" || tc.spelled == "HEAD") {
					t.Errorf("%s by %s resolved refs %v", q.what, tc.spelled, stub.resolved)
				}
			}
		})
	}
	stub := &stubCode{git: stamping, preview: func(q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
		return gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: gitlet.MergeUnknown}, nil
	}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_compare",
		map[string]any{"name": "web", "base": whole, "head": whole})
	if _, immutable := answer.(queries.Immutable); err != nil || immutable {
		t.Errorf("a comparison by whole ids whose merge is unknown answered %T (%v), want it bare", answer, err)
	}
}

// A REF IS RESOLVED ONCE A QUESTION, and the read by object id is floored at
// its move. A diff naming two refs reads both in ONE resolve at the caller's
// freshness, and asks its Gitlet node at the furthest of the node's own write to
// the repository, the caller's floor on the code log and where each ref last
// moved — never at the resolve's own checkpoint. `HEAD` is the default branch,
// which the resolve answers from the repository's row in its own transaction
// (gitlet.HeadRef): a tree at HEAD — the Repository page's commonest question —
// is ONE read of the rows, a resolve naming HEAD at the caller's freshness,
// with no read of the repository ahead of it. A name no live ref holds is
// not_found (`no_ref`), naming it, and nothing is read by object id.
//
// Mutation: floor the git read at the resolve's Position — red; resolve each
// ref in a read of its own — red; read the repository first to name its
// default branch — red.
func TestARefQuestionResolvesItsRefsOnceAndReadsAtTheirMove(t *testing.T) {
	t.Parallel()
	caller := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 30}
	stub := &stubCode{floor: wrote, refs: func(q gitlet.RefQuery) (gitlet.RefAnswer, error) {
		a := gitlet.RefAnswer{Repo: stubRepo("web").RepoRef,
			Position: statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 99}}
		for i, name := range q.Refs {
			a.Refs = append(a.Refs, gitlet.ResolvedRef{Name: name, Ref: "refs/heads/" + name,
				OID: []string{whole, other}[i], Moved: statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1,
					Seq: moved.Seq - uint64(i)}})
		}
		return a, nil
	}}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_diff", map[string]any{"name": "web",
		"base": "main", "head": "feature", "read_level": "session", "min_position": caller.String()}); err != nil {
		t.Fatal(err)
	}
	if len(stub.resolved) != 1 || !slices.Equal(stub.resolved[0].Refs, []string{"main", "feature"}) ||
		stub.resolved[0].Fresh.Level != statelog.ReadSession || len(stub.asked) != 0 {
		t.Fatalf("a diff of two refs resolved %+v and read rows %+v, want one resolve of both at the caller's "+
			"session", stub.resolved, stub.asked)
	}
	q := stub.read[0]
	if q.Diff.Base != whole || q.Diff.Head != other || q.RepoID != stubRepoID ||
		q.Fresh.Level != statelog.ReadSession || q.Fresh.Floor("CREWLET_CODE_LOG") != moved {
		t.Errorf("the diff was read as %+v at %+v, want %s..%s of %s at session floored at %v", q.Diff, q.Fresh,
			whole, other, stubRepoID, moved)
	}

	headed := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: headed}, "code_tree", map[string]any{"name": "web",
		"read_level": "session", "min_position": caller.String()}); err != nil {
		t.Fatal(err)
	}
	if len(headed.asked) != 0 || len(headed.resolved) != 1 ||
		!slices.Equal(headed.resolved[0].Refs, []string{gitlet.HeadRef}) ||
		headed.resolved[0].Fresh.Level != statelog.ReadSession ||
		headed.resolved[0].Fresh.Floor("CREWLET_CODE_LOG") != caller {
		t.Errorf("a tree at HEAD read %+v and resolved %+v, want one resolve naming HEAD at the caller's "+
			"session floored at %v and no read of the repository", headed.asked, headed.resolved, caller)
	}

	missing := &stubCode{refs: func(q gitlet.RefQuery) (gitlet.RefAnswer, error) {
		return gitlet.RefAnswer{Repo: stubRepo("web").RepoRef, Refs: []gitlet.ResolvedRef{
			{Name: q.Refs[0], Missing: fmt.Sprintf("repository web has no branch or tag %q", q.Refs[0])}}}, nil
	}}
	_, err := askNative(t, queries.Sources{Code: missing}, "code_log", map[string]any{"name": "web", "ref": "gone"})
	var absent *queries.NotFound
	if !errors.As(err, &absent) || absent.Reason != queries.ReasonNoRef || !strings.Contains(absent.Detail, "gone") ||
		len(missing.read) != 0 {
		t.Errorf("a log of a ref nobody holds answered %v having read %d by object id, want not_found (no_ref) "+
			"naming it, before any", err, len(missing.read))
	}
}

// A MERGE PREVIEW IS ASKED OF THE HOME. A comparison asks what merging head
// into base would do of the repository's home Gitlet node — the estate's
// code.merge_preview, through the code reads (estate.CodeSeam.MergePreview,
// keyed on the repository's git home as a read by object id is) — once, by
// the WHOLE ids the comparison answered, so the preview merges exactly the
// two commits compared whatever the request spelled; as the caller, at the
// floor the comparison was read at; and answers its state and conflicts
// beside the comparison — of two commits that have diverged, neither
// containing the other, which is the one comparison git has to merge to
// answer. A home that cannot answer now is unavailable, never an answer about
// the two commits.
//
// Mutation: preview the request's own spellings — an abbreviation reaches the
// preview, red; answer the comparison without its preview — red.
func TestAMergePreviewIsAskedOfTheHome(t *testing.T) {
	t.Parallel()
	stub := &stubCode{floor: wrote,
		git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
			return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Compare: &gitlet.CompareAnswer{Base: whole,
				Head: other, MergeBase: forked, Ahead: 1, Behind: 2}}, nil
		},
		preview: func(q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
			return gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head, State: gitlet.MergeConflict,
				Conflicts: []string{"go.mod"}, ConflictsOmitted: 2}, nil
		}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_compare", map[string]any{"name": "web",
		"base": abbrev, "head": "2222222"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.previewed) != 1 {
		t.Fatalf("a comparison asked %d merge previews, want one", len(stub.previewed))
	}
	p := stub.previewed[0]
	if p.RepoID != stubRepoID || p.Base != whole || p.Head != other || p.Asker != stub.read[0].Asker ||
		p.Fresh.Floor("CREWLET_CODE_LOG") != wrote {
		t.Errorf("the preview was asked %+v, want %s into %s of %s as the comparison's asker, at its floor %v", p,
			other, whole, stubRepoID, wrote)
	}
	got, ok := answer.(queries.CodeComparison)
	if !ok || got.Compare == nil || got.Compare.MergeBase != forked || got.Preview.State != gitlet.MergeConflict ||
		!slices.Equal(got.Preview.Conflicts, []string{"go.mod"}) || got.Preview.ConflictsOmitted != 2 {
		t.Errorf("the comparison answered %+v, want the comparison and its preview", answer)
	}

	down := &stubCode{preview: func(gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
		return gitlet.MergePreviewAnswer{}, &gitlet.ErrGitUnready{Reason: gitlet.UnreadyShare, Detail: "busy"}
	}}
	if _, err := askNative(t, queries.Sources{Code: down}, "code_compare", map[string]any{"name": "web",
		"base": whole, "head": other}); !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a comparison whose home could not preview answered %v, want unavailable", err)
	}
}

// AN ANCESTRAL COMPARISON ASKS NO PREVIEW. Where one commit contains the
// other — base inside head, a fast-forward, or head inside base, a merge of
// nothing — the merge a preview runs answers `clean` with no path conflicted,
// so the comparison answers that from its own merge base and asks its home
// nothing: no scratch, no git and no unit of the node's seat git-work share
// for the commonest comparison there is. Asked by whole ids it is immutable,
// the merge decided.
//
// Mutation: ask the home whatever the comparison answered — a preview is
// asked, red; answer an ancestral comparison anything but clean — red.
func TestAnAncestralComparisonAsksNoPreview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		relation gitlet.CompareAnswer
	}{
		{"base inside head", gitlet.CompareAnswer{MergeBase: whole, BaseIsAncestor: true, Ahead: 3}},
		{"head inside base", gitlet.CompareAnswer{MergeBase: other, HeadIsAncestor: true, Behind: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubCode{
				git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
					c := tc.relation
					c.Base, c.Head = whole, other
					return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Compare: &c}, nil
				},
				preview: func(q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
					return gitlet.MergePreviewAnswer{RepoID: q.RepoID, Base: q.Base, Head: q.Head,
						State: gitlet.MergeConflict, Conflicts: []string{"go.mod"}}, nil
				}}
			answer, err := askNative(t, queries.Sources{Code: stub}, "code_compare", map[string]any{"name": "web",
				"base": whole, "head": other})
			if err != nil {
				t.Fatal(err)
			}
			if len(stub.previewed) != 0 {
				t.Errorf("an ancestral comparison asked its home %d merge previews, want none: %+v",
					len(stub.previewed), stub.previewed)
			}
			fixed, immutable := answer.(queries.Immutable)
			if !immutable {
				t.Fatalf("an ancestral comparison by whole ids answered %T, want it immutable", answer)
			}
			got, ok := fixed.Answer.(queries.CodeComparison)
			if !ok || got.Preview.State != gitlet.MergeClean || len(got.Preview.Conflicts) != 0 ||
				got.Preview.ConflictsOmitted != 0 {
				t.Errorf("an ancestral comparison answered %+v, want its preview clean, no path conflicted",
					fixed.Answer)
			}
		})
	}
}

// A COMMIT IS ANSWERED WITH THE PUSH THAT INTRODUCED IT AND THE CHANGES IT
// BELONGS TO. Its rows are read first at the caller's freshness — the
// repository and the commit's changes in one snapshot (the reader's
// `commit`) — then the commit by its whole id of the repository's home, and
// the push the node's index names read whole, at session floored at the later
// of the rows' read and the push's own record, so a push the rows' copy had
// not reached is read where it is. An abbreviation is refused before anything
// is read, and so is an id that names no commit.
//
// Mutation: read the push at the rows' position alone — red.
func TestACommitIsAnsweredWithItsPushAndItsChanges(t *testing.T) {
	t.Parallel()
	rowsAt := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 20}
	change := gitlet.ChangeName{Repo: "web", Ref: "web!4", Title: "Retry", State: gitlet.ChangeStateOpen}
	stub := &stubCode{
		answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
			if q.Kind == gitlet.ReadPushes {
				return gitlet.ReadAnswer{Pushes: []gitlet.PushRow{{PushID: q.Push}}}, nil
			}
			return gitlet.ReadAnswer{Repo: stubRepo(q.Repo), Position: rowsAt, Level: q.Fresh.Level,
				Commit: &gitlet.CommitChanges{Commit: q.Commit, Revisions: []gitlet.CommitRevision{{Change: change,
					N: 2}}}}, nil
		},
		git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
			return gitlet.GitAnswer{Kind: q.Kind, Log: &gitlet.LogAnswer{Commits: []gitlet.LogCommit{{OID: q.Log.Rev,
				Subject: "Retry the upload"}}}, PushedBy: map[string]gitlet.PushedBy{whole: {PushID: "p7",
				Position: moved}}}, nil
		}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_commit", map[string]any{"name": "web",
		"sha": whole})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := answer.(queries.CodeCommit)
	if !ok || got.Commit.OID != whole || got.PushedBy == nil || got.Push == nil || got.Push.PushID != "p7" ||
		len(got.Revisions) != 1 || got.Revisions[0].Change != change || got.Position != rowsAt {
		t.Fatalf("the commit answered %+v, want it, its push and its revision", answer)
	}
	if len(stub.asked) != 2 || stub.asked[0].Kind != gitlet.ReadCommit || stub.asked[0].Commit != whole ||
		stub.asked[1].Push != "p7" || stub.asked[1].Fresh.Level != statelog.ReadSession ||
		stub.asked[1].Fresh.Floor("CREWLET_CODE_LOG") != moved {
		t.Errorf("the commit read %+v, want its rows and then its push at session floored at %v", stub.asked,
			moved)
	}
	if stub.read[0].Unstamped || stub.read[0].Log.Rev != whole || stub.read[0].Log.Limit != 1 {
		t.Errorf("the commit was read by object id as %+v, want its one commit, stamped", stub.read[0])
	}
	for _, sha := range []string{abbrev, "main"} {
		none := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: none}, "code_commit", map[string]any{"name": "web",
			"sha": sha}); !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 {
			t.Errorf("a commit named %q answered %v having read %d, want bad_params before anything", sha, err,
				len(none.asked))
		}
	}
	tree := &stubCode{git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
		return gitlet.GitAnswer{Kind: q.Kind, Log: &gitlet.LogAnswer{Commits: []gitlet.LogCommit{{OID: other}}}}, nil
	}}
	if _, err := askNative(t, queries.Sources{Code: tree}, "code_commit", map[string]any{"name": "web",
		"sha": whole}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an id whose history begins at another commit answered %v, want bad_params", err)
	}
}

// A READ BY OBJECT ID REFUSES IN THIS SURFACE'S VOCABULARY: an object the
// repository does not hold, or a path its revision holds no such directory
// or file at, is not_found (`no_object`) in the Gitlet node's own sentence; a
// read too large to answer whole is the request's to narrow; and no Gitlet node
// able to answer now is unavailable — never a 500, never an answer about the
// repository.
//
// Mutation: drop codeRefusal's object arm — the absence is an unclassified
// failure, red.
func TestAReadByObjectIDRefusesInThisSurfacesVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"no object", fmt.Errorf("%w: web holds no object %s", gitlet.ErrObjectNotFound, whole), func(err error) bool {
			var absent *queries.NotFound
			return errors.As(err, &absent) && absent.Reason == queries.ReasonNoObject &&
				strings.Contains(absent.Detail, "holds no object")
		}},
		{"no directory", fmt.Errorf("%w: go.mod is a file", gitlet.ErrNotATree), func(err error) bool {
			var absent *queries.NotFound
			return errors.As(err, &absent) && absent.Reason == queries.ReasonNoObject
		}},
		{"too large", fmt.Errorf("%w: past its budget", gitlet.ErrSliceTooLarge), func(err error) bool {
			return errors.Is(err, queries.ErrBadParams)
		}},
		{"no Gitlet node", &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld, Detail: "filling"}, func(err error) bool {
			return errors.Is(err, queries.ErrUnavailable)
		}},
	} {
		stub := &stubCode{git: func(gitlet.GitQuery) (gitlet.GitAnswer, error) { return gitlet.GitAnswer{}, tc.err }}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_tree", map[string]any{"name": "web",
			"ref": whole}); !tc.check(err) {
			t.Errorf("%s: answered %v", tc.name, err)
		}
	}
}
