package queries_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TAGS QUESTION READS ITS REPOSITORY, floors at its version, never immutable.
//
// Mutations: answer it Immutable, or floor without the version → red.
func TestATagsQuestionReadsItsRepositoryAndIsNeverImmutable(t *testing.T) {
	t.Parallel()
	version := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 33}
	detail := func(v statelog.Position) func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
			d := stubRepo(q.Repo)
			d.Version = v
			return gitlet.ReadAnswer{Repo: d, Level: q.Fresh.Level}, nil
		}
	}
	stub := &stubCode{answer: detail(version), git: stamping}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_tags", map[string]any{"name": "web",
		"limit": "3", "cursor": "d:5:dg"})
	if err != nil {
		t.Fatal(err)
	}
	page, bare := answer.(queries.CodePage)
	if !bare || page.Position != moved {
		t.Errorf("the tags question answered %T %+v, want a bare page carrying the read's position", answer, answer)
	}
	if len(stub.asked) != 1 || stub.asked[0].Kind != gitlet.ReadRepo || len(stub.read) != 1 {
		t.Fatalf("the question asked %+v and read %+v, want one read of the repository and one tags read",
			stub.asked, stub.read)
	}
	q := stub.read[0]
	if q.Kind != gitlet.GitTags || q.Tags == nil || q.Tags.Limit != 3 || q.Cursor != "d:5:dg" || q.Unstamped {
		t.Errorf("the tags read is %+v, want a stamped tags page of 3 from the cursor", q)
	}
	if q.Fresh.Level != statelog.ReadSession || q.Fresh.Floor("CREWLET_CODE_LOG") != version {
		t.Errorf("the tags read is floored at %+v, want session at the repository's version %v", q.Fresh, version)
	}

	none := &stubCode{answer: detail(statelog.Position{}), git: stamping}
	if _, err := askNative(t, queries.Sources{Code: none}, "code_tags", map[string]any{"name": "web"}); err != nil {
		t.Fatal(err)
	}
	if got := none.read[0].Fresh; !got.Floor("CREWLET_CODE_LOG").IsZero() {
		t.Errorf("a detail naming no version floored the tags read at %+v, want the caller's own floor only", got)
	}

	for _, limit := range []string{"x", "-1"} {
		refused := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: refused}, "code_tags", map[string]any{"name": "web",
			"limit": limit}); !errors.Is(err, queries.ErrBadParams) || len(refused.asked)+len(refused.read) != 0 {
			t.Errorf("limit %q answered %v having read %d, want bad_params and nothing read", limit, err,
				len(refused.asked)+len(refused.read))
		}
	}
	past := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: past}, "code_tags", map[string]any{"name": "web",
		"limit": "101"}); !errors.Is(err, queries.ErrBadParams) || len(past.read) != 0 {
		t.Errorf("limit 101 answered %v, want bad_params from the node's validation", err)
	}
}

// A COMMIT NAMES ITS TAGS where the node's tag index holds them, else none,
// read at the repository's version.
//
// Mutations: drop Tagged from code_commit's answer, or floor the commit's
// read without the version → red.
func TestACommitNamesItsTags(t *testing.T) {
	t.Parallel()
	version := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 41}
	marks := &gitlet.TagMarks{Commits: map[string]gitlet.CommitTags{whole: {Refs: []string{"refs/tags/v1"}}}}
	for _, tagged := range []*gitlet.TagMarks{marks, nil} {
		stub := &stubCode{
			answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
				d := stubRepo(q.Repo)
				d.Version = version
				return gitlet.ReadAnswer{Repo: d, Level: q.Fresh.Level,
					Commit: &gitlet.CommitChanges{Commit: q.Commit}}, nil
			},
			git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
				return gitlet.GitAnswer{Kind: q.Kind, Tagged: tagged,
					Log: &gitlet.LogAnswer{Commits: []gitlet.LogCommit{{OID: q.Log.Rev}}}}, nil
			}}
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_commit", map[string]any{"name": "web",
			"sha": whole})
		if err != nil {
			t.Fatal(err)
		}
		if got := answer.(queries.CodeCommit).Tagged; got != tagged {
			t.Errorf("the commit carries tagged %+v, want %+v", got, tagged)
		}
		if got := stub.read[0].Fresh; got.Level != statelog.ReadSession || got.Floor("CREWLET_CODE_LOG") != version {
			t.Errorf("the commit's read is floored at %+v, want session at the repository's version %v", got,
				version)
		}
	}
}
