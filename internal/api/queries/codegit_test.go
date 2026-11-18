package queries_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
	{"code_find", func(rev string) map[string]any { return map[string]any{"name": "web", "ref": rev, "q": "main"} }},
	{"code_blob", func(rev string) map[string]any {
		return map[string]any{"name": "web", "ref": rev, "path": "go.mod"}
	}},
	{"code_log", func(rev string) map[string]any { return map[string]any{"name": "web", "ref": rev} }},
	{"code_blame", func(rev string) map[string]any {
		return map[string]any{"name": "web", "ref": rev, "path": "go.mod"}
	}},
	{"code_tree_commits", func(rev string) map[string]any { return map[string]any{"name": "web", "ref": rev} }},
	{"code_diff", func(rev string) map[string]any {
		return map[string]any{"name": "web", "base": rev, "head": rev}
	}},
	{"code_tips", func(rev string) map[string]any {
		return map[string]any{"name": "web", "oids": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "base": rev}
	}},
	{"code_compare", func(rev string) map[string]any {
		return map[string]any{"name": "web", "base": rev, "head": rev}
	}},
}

// A DIFF'S EVERY PATCH IS ASKED BY ITS PARAMETER: `patches=true` asks the diff's Patches; a value
// that is no flag, or patches beside a file, is bad_params with nothing read by object id.
//
// Mutation: drop the patches parameter's read → the diff asks the stat, red.
func TestADiffsPatchesAreAskedByTheirParameter(t *testing.T) {
	t.Parallel()
	stub := &stubCode{floor: wrote}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_diff", map[string]any{"name": "web",
		"base": whole, "head": other, "patches": "true"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.read) != 1 || !stub.read[0].Diff.Patches {
		t.Fatalf("code_diff with patches read %+v, want one diff of every file's patch", stub.read)
	}
	for name, params := range map[string]map[string]any{
		"patches that is no flag": {"patches": "yes"},
		"patches beside a file":   {"patches": "true", "file": "go.mod"},
	} {
		all := map[string]any{"name": "web", "base": whole, "head": other}
		for k, v := range params {
			all[k] = v
		}
		stub := &stubCode{floor: wrote}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_diff", all); !errors.Is(err,
			queries.ErrBadParams) || len(stub.read) != 0 {
			t.Errorf("%s: answered %v having read %d times by object id, want bad_params before it", name, err,
				len(stub.read))
		}
	}
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

// AN OID QUESTION IS IMMUTABLE. A question every revision of which is a whole
// object id is answered queries.Immutable and asks its node for the page alone
// (gitlet.GitQuery.Unstamped), so it carries nothing a node stamped (position,
// push, coverage, repository id, resolved ref), even from a node that stamped
// them. Asked by a ref or an abbreviation it is answered bare with what the
// read stamped, and not unstamped; so is a comparison whose merge its home
// could not decide.
//
// Mutation: Immutable whatever the spelling → the ref's answer is cached for
// a year; leave Unstamped unset → the page is cut against the stamps; copy
// the stamps onto an immutable page; all red.
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
// freshness and asks its node at the furthest of the node's own write, the
// caller's floor and where each ref last moved, never the resolve's own
// checkpoint. A tree at `HEAD` is ONE read of the rows (gitlet.HeadRef), with
// no read of the repository ahead of it. A name no live ref holds is
// not_found (`no_ref`), and nothing is read by object id.
//
// Mutation: floor at the resolve's Position; resolve each ref in its own
// read; read the repository first to name its default branch; all red.
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
// into base would do of the repository's git home (estate.CodeSeam.MergePreview)
// once, by the WHOLE ids the comparison answered whatever the request
// spelled, as the caller at the comparison's floor, and answers its state and
// conflicts beside it, for two diverged commits, the one comparison git has to
// merge to answer. A home that cannot answer is unavailable, never an answer
// about the two commits.
//
// Mutation: preview the request's own spellings; answer the comparison
// without its preview; both red.
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
				Commit: &gitlet.CommitChanges{Commit: q.Commit, Revisions: []gitlet.CommitRevision{{
					Change: gitlet.ChangeLanding{ChangeName: change}, N: 2}}}}, nil
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
		len(got.Revisions) != 1 || got.Revisions[0].Change.ChangeName != change || got.Position != rowsAt {
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

// A TIPS QUESTION NAMES ITS TIPS by whole ids, listed or comma-separated, in
// order; a base by ref is resolved once and not immutable; bad oids read nothing.
// Mutations: take oids as one id → red; answer a base by ref immutable → red.
func TestATipsQuestionNamesItsTips(t *testing.T) {
	t.Parallel()
	for _, oids := range []any{whole + "," + other, []any{whole, other}, " " + whole + " , " + other + ","} {
		stub := &stubCode{git: stamping}
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_tips", map[string]any{"name": "web",
			"oids": oids})
		if err != nil {
			t.Fatalf("oids %q: %v", oids, err)
		}
		if _, immutable := answer.(queries.Immutable); !immutable || len(stub.resolved) != 0 {
			t.Errorf("oids %q with no base answered %T having resolved %v, want immutable and nothing resolved",
				oids, answer, stub.resolved)
		}
		if len(stub.read) != 1 || stub.read[0].Tips == nil || !slices.Equal(stub.read[0].Tips.OIDs,
			[]string{whole, other}) || stub.read[0].Tips.Base != "" {
			t.Errorf("oids %q were read as %+v, want the two ids in order and no base", oids, stub.read)
		}
	}

	stub := &stubCode{git: stamping}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_tips", map[string]any{"name": "web",
		"oids": whole, "base": "HEAD"})
	if _, immutable := answer.(queries.Immutable); err != nil || immutable {
		t.Errorf("a base by ref answered %T (%v), want it bare", answer, err)
	}
	if len(stub.resolved) != 1 || !slices.Equal(stub.resolved[0].Refs, []string{gitlet.HeadRef}) ||
		len(stub.read) != 1 || stub.read[0].Tips.Base != stubOID {
		t.Errorf("a base by ref resolved %+v and read %+v, want one resolve of HEAD and its id as the base",
			stub.resolved, stub.read)
	}

	for name, oids := range map[string]any{"none": nil, "an abbreviation": whole + "," + abbrev, "a name": "main"} {
		stub := &stubCode{git: stamping}
		args := map[string]any{"name": "web"}
		if oids != nil {
			args["oids"] = oids
		}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_tips", args); !errors.Is(err,
			queries.ErrBadParams) || len(stub.read) != 0 {
			t.Errorf("%s: answered %v having read %d, want bad_params and nothing read", name, err, len(stub.read))
		}
	}
}

// A HISTORY NARROWED BY WHO PUSHED IT IS NEVER IMMUTABLE: read stamped and answered
// bare, `until` reaches the read, and a bad `until` or handle is bad_params unread.
// Mutation: judge immutability by the revisions alone.
func TestAHistoryNarrowedByWhoPushedIsNeverImmutable(t *testing.T) {
	t.Parallel()
	stub := &stubCode{git: stamping}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_log", map[string]any{"name": "web",
		"ref": whole, "pushed_by": " ceo ", "until": "2026-03-01T00:00:00.5Z"})
	if err != nil {
		t.Fatalf("code_log narrowed to a seat: %v", err)
	}
	if _, immutable := answer.(queries.Immutable); immutable {
		t.Error("a history narrowed by who pushed it is answered immutable")
	}
	until := time.Date(2026, 3, 1, 0, 0, 0, 5e8, time.UTC)
	if len(stub.read) != 1 || stub.read[0].Unstamped || stub.read[0].Log.PushedBy != "ceo" ||
		!stub.read[0].Log.Until.Equal(until) {
		t.Fatalf("the narrowed history was read as %+v, want one read, stamped, pushed by ceo until %s",
			stub.read, until)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"position"`) || !strings.Contains(string(encoded), `"pushed_by"`) {
		t.Errorf("the narrowed history is answered without what its read stamped: %s", encoded)
	}
	plain := &stubCode{git: stamping}
	if answer, err := askNative(t, queries.Sources{Code: plain}, "code_log", map[string]any{"name": "web",
		"ref": whole, "until": "2026-03-01T00:00:00Z"}); err != nil || len(plain.read) != 1 ||
		!plain.read[0].Unstamped {
		t.Errorf("a history by a whole id with until alone answered %T (%v), read %+v; want it unstamped",
			answer, err, plain.read)
	} else if _, immutable := answer.(queries.Immutable); !immutable {
		t.Error("a history by a whole id with until alone is not immutable")
	}
	for _, args := range []map[string]any{
		{"name": "web", "ref": whole, "until": "yesterday"},
		{"name": "web", "ref": whole, "since": "2026-03-01T00:00:00Z", "until": "2026-03-01T00:00:00Z"},
		{"name": "web", "ref": whole, "pushed_by": "Not A Seat"},
	} {
		bad := &stubCode{git: stamping}
		if _, err := askNative(t, queries.Sources{Code: bad}, "code_log", args); !errors.Is(err,
			queries.ErrBadParams) || len(bad.read) != 0 {
			t.Errorf("code_log with %v answered %v after %d reads, want bad_params before any", args, err,
				len(bad.read))
		}
	}
}

// A FIND NAMES ITS QUERY: `q` and `limit` reach the read as the find's query and
// limit; no `q`, or a limit that is no whole number, is bad_params before any read.
// Mutation: drop codeFind's check of q, or of its limit being whole.
func TestAFindNamesItsQuery(t *testing.T) {
	t.Parallel()
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_find", map[string]any{"name": "web",
		"ref": whole, "q": "cmd/ma", "limit": 7}); err != nil {
		t.Fatal(err)
	}
	if len(stub.read) != 1 || stub.read[0].Kind != gitlet.GitFind ||
		*stub.read[0].Find != (gitlet.FindQuery{Rev: whole, Query: "cmd/ma", Limit: 7}) {
		t.Fatalf("the find was read as %+v, want one find of cmd/ma at %s, limit 7", stub.read, whole)
	}
	for _, args := range []map[string]any{
		{"name": "web", "ref": whole},
		{"name": "web", "ref": whole, "q": ""},
		{"name": "web", "ref": "main", "q": "main", "limit": 2.5},
		{"name": "web", "ref": "main", "q": "main", "limit": -1},
		{"name": "web", "ref": "main", "q": "main", "limit": "many"},
	} {
		none := &stubCode{}
		_, err := askNative(t, queries.Sources{Code: none}, "code_find", args)
		if !errors.Is(err, queries.ErrBadParams) || len(none.asked)+len(none.resolved)+len(none.read) != 0 {
			t.Errorf("a find of %v answered %v having read %d rows, %d resolves and %d reads, want bad_params "+
				"before anything", args, err, len(none.asked), len(none.resolved), len(none.read))
		}
	}
	past := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: past}, "code_find", map[string]any{"name": "web",
		"ref": whole, "q": "main", "limit": gitlet.MaxFindLimit + 1}); !errors.Is(err, queries.ErrBadParams) ||
		len(past.read) != 0 {
		t.Errorf("a find of %d files answered %v having read %d, want bad_params", gitlet.MaxFindLimit+1, err,
			len(past.read))
	}
}

// A CUT TREE-COMMITS PAGE IS NEVER IMMUTABLE: a page of a directory's last
// commits its read's budget ended, asked by a whole id, is answered bare and
// unstamped in both its shapes (a prefix of answered entries with a next, and
// a first entry out of time), and the same page unmarked is immutable.
// Mutation: answer the page immutable whatever its mark.
func TestACutTreeCommitsPageIsNeverImmutable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		page gitlet.TreeCommitsAnswer
		next string
		cut  bool
	}{
		{"a prefix", gitlet.TreeCommitsAnswer{Tree: whole, Entries: []gitlet.TreeCommit{{Path: "a", Commit: whole}},
			Total: 2, OutOfTime: true}, "e:1", true},
		{"a first entry out of time", gitlet.TreeCommitsAnswer{Tree: whole,
			Entries: []gitlet.TreeCommit{{Path: "a", OutOfTime: true}}, Total: 2, OutOfTime: true}, "e:1", true},
		{"unmarked", gitlet.TreeCommitsAnswer{Tree: whole, Entries: []gitlet.TreeCommit{{Path: "a", Commit: whole}},
			Total: 1}, "", false},
	} {
		stub := &stubCode{git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
			page := tc.page
			return gitlet.GitAnswer{Kind: q.Kind, Rev: whole, Next: tc.next, TreeCommits: &page}, nil
		}}
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_tree_commits",
			map[string]any{"name": "web", "ref": whole})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, immutable := answer.(queries.Immutable); immutable == tc.cut {
			t.Errorf("%s: answered immutable %t, want %t", tc.name, immutable, !tc.cut)
		}
		if len(stub.read) != 1 || !stub.read[0].Unstamped || stub.read[0].TreeCommits == nil {
			t.Fatalf("%s: read as %+v, want one unstamped read of the directory's last commits", tc.name, stub.read)
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"position"`) || strings.Contains(string(encoded), `"pushed_by"`) {
			t.Errorf("%s: answered with a stamp: %s", tc.name, encoded)
		}
	}
}
