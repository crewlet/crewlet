package queries_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// repoReply is code_repo's answer as a client decodes it.
type repoReply struct {
	Repo     *gitlet.RepoDetail `json:"repo"`
	Position statelog.Position  `json:"position"`
	Level    string             `json:"level"`
	CloneURL *string            `json:"clone_url"`
}

// askRepo asks code_repo of the repository name over a stub answering every repository as web,
// and decodes its answer.
func askRepo(t *testing.T, s queries.Sources, name string) (repoReply, error) {
	t.Helper()
	answered, err := askNative(t, s, "code_repo", map[string]any{"name": name})
	if err != nil {
		return repoReply{}, err
	}
	body, err := json.Marshal(answered)
	if err != nil {
		t.Fatalf("encode code_repo's answer: %v", err)
	}
	var reply repoReply
	if err = json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("decode code_repo's answer %s: %v", body, err)
	}
	return reply, nil
}

// A REPOSITORY READ ANSWERS THE ADDRESS IT IS CLONED FROM: this deployment's own address and
// the repository's current name, beside the read's own answer whole; none where the deployment
// names no address; and a refused read is refused as before.
// Mutation: build the address from the asked name, or drop the read's own fields.
func TestARepositoryReadAnswersTheAddressItIsClonedFrom(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CODE", Generation: 1, Seq: 42}
	stub := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		if q.Repo == "nope" {
			return gitlet.ReadAnswer{}, gitlet.ErrRepoNotFound
		}
		repo := &gitlet.RepoDetail{RepoSummary: gitlet.RepoSummary{RepoRef: gitlet.RepoRef{ID: "id-web",
			Name: "web", DefaultBranch: "main", Position: at}}}
		return gitlet.ReadAnswer{Repo: repo, Position: at, Level: statelog.ReadStale}, nil
	}}
	base := "https://crewlet.example.com"
	s := queries.Sources{Code: stub, DashboardBase: func() string { return base }}
	for _, asked := range []string{"web", "old-web"} {
		reply, err := askRepo(t, s, asked)
		if err != nil {
			t.Fatalf("code_repo of %s: %v", asked, err)
		}
		if reply.CloneURL == nil || *reply.CloneURL != base+"/git/web.git" {
			t.Errorf("code_repo of %s answers the clone address %v, want the current name's on %s", asked,
				reply.CloneURL, base)
		}
		if reply.Repo == nil || reply.Repo.Name != "web" || reply.Position != at ||
			reply.Level != string(statelog.ReadStale) {
			t.Errorf("code_repo of %s answers %+v, want the read's repository, position and level", asked, reply)
		}
	}
	for name, unnamed := range map[string]queries.Sources{
		"no address":    {Code: stub},
		"empty address": {Code: stub, DashboardBase: func() string { return "" }},
	} {
		if reply, err := askRepo(t, unnamed, "web"); err != nil || reply.CloneURL != nil || reply.Repo == nil {
			t.Errorf("code_repo on a deployment of %s answers %+v, %v; want the repository and no clone address",
				name, reply, err)
		}
	}
	if _, err := askRepo(t, s, "nope"); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("code_repo of a repository nobody declared answered %v, want not found", err)
	}
}

// A REPOSITORY LISTING ASKS ITS KIND AND ORDER: `kind` and `sort` reach the reader as the
// listing's own fields, neither set asks the zeros, and code_counts asks both its parts.
// Mutation: read `sort` into another read's field, or drop `kind`.
func TestARepositoryListingAsksItsKindAndOrder(t *testing.T) {
	t.Parallel()
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_repos",
		map[string]any{"kind": "scratch", "sort": "pushed"}); err != nil {
		t.Fatalf("code_repos of one kind by last push: %v", err)
	}
	if asked := stub.last(); asked.Kind != gitlet.ReadRepos || asked.RepoKind != gitlet.RepoScratch ||
		asked.RepoSort != gitlet.RepoSortPushed {
		t.Errorf("code_repos of one kind by last push asked %+v", asked)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_repos", map[string]any{}); err != nil {
		t.Fatalf("code_repos: %v", err)
	}
	if asked := stub.last(); asked.RepoKind != "" || asked.RepoSort != "" {
		t.Errorf("code_repos naming neither asked %+v, want the zeros", asked)
	}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_counts",
		map[string]any{"parts": "changes,repos"}); err != nil {
		t.Fatalf("code_counts of both parts: %v", err)
	}
	if asked := stub.last(); asked.Counts == nil ||
		*asked.Counts != (gitlet.CountsQuery{Changes: true, Repos: true}) {
		t.Errorf("code_counts of both parts asked %+v", asked.Counts)
	}
}
