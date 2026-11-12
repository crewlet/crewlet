package gather_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// The engine's own code read is a Gitlet source as it stands.
var _ gather.GitletReads = estate.CodeSeam{}

var now = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// fakeGitlet answers a repository listing in pages, each repository's detail
// and its checks, and records every query it was asked.
type fakeGitlet struct {
	mu      sync.Mutex
	asked   []gitlet.ReadQuery
	repos   []gitlet.RepoSummary
	page    int
	fail    map[gitlet.ReadKind]error
	remotes map[string]gitlet.RemoteState
	// checksPerRepo is how many checks each default branch runs; 0 is one.
	checksPerRepo int
}

func (f *fakeGitlet) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, q)
	if err := f.fail[q.Kind]; err != nil {
		return gitlet.ReadAnswer{}, err
	}
	switch q.Kind {
	case gitlet.ReadRepos:
		start := 0
		if q.Cursor != "" {
			start = slices.IndexFunc(f.repos, func(r gitlet.RepoSummary) bool { return r.Name == q.Cursor })
		}
		end := min(len(f.repos), start+f.page)
		a := gitlet.ReadAnswer{Repos: f.repos[start:end]}
		if end < len(f.repos) {
			a.Next = f.repos[end].Name
		}
		return a, nil
	case gitlet.ReadRepo:
		return gitlet.ReadAnswer{Repo: &gitlet.RepoDetail{Remotes: f.remotes[q.Repo]}}, nil
	case gitlet.ReadChecks:
		a := gitlet.ReadAnswer{Checks: []gitlet.CheckRow{
			{CheckKey: gitlet.CheckKey{Name: "test"}, Repo: q.Repo, State: gitlet.CheckStatePassed, Required: true},
		}}
		for i := 1; i < f.checksPerRepo; i++ {
			a.Checks = append(a.Checks, gitlet.CheckRow{CheckKey: gitlet.CheckKey{Name: fmt.Sprintf("check-%d", i)},
				Repo: q.Repo, State: gitlet.CheckStateFailed})
		}
		return a, nil
	}
	return gitlet.ReadAnswer{}, errors.New("not asked of the atlas")
}

func repo(name string) gitlet.RepoSummary {
	return gitlet.RepoSummary{RepoRef: gitlet.RepoRef{Name: name, Status: gitlet.RepoStatusActive, DefaultBranch: "main",
		Visibility: gitlet.VisibilityCompany, Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "platform"}},
		Head: strings.Repeat("a", 40)}
}

func acme() *config.Company {
	c := &config.Company{Name: "acme"}
	c.Code.Backend = config.CodeGitlet
	c.Code.Repos = []config.CodeRepo{{Name: "api", MirrorFrom: &config.CodeMirrorSource{URL: "https://github.com/acme/api"}}}
	return c
}

var asker = gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1"}

// THE ATLAS READS GITLET AS THE READER: every question carries who reads, so
// Gitlet lists only what they may read; every page of the listing is read; and
// only a repository the settings do not declare is read in detail for its
// other hosts.
func TestTheAtlasReadsGitletAsTheReader(t *testing.T) {
	t.Parallel()
	f := &fakeGitlet{repos: []gitlet.RepoSummary{repo("api"), repo("tools"), repo("web")}, page: 2,
		remotes: map[string]gitlet.RemoteState{
			"tools": {Import: &gitlet.ImportState{Source: gitlet.ImportSource{URL: "https://gitlab.acme.example/p/tools.git",
				Credential: "${TOOLS_TOKEN}"}}},
		}}
	a := gather.Read(context.Background(), gather.Sources{Gitlet: f}, gather.Request{Company: acme(), Revision: "r1",
		Asker: asker, Now: now})
	for _, q := range f.asked {
		if q.Asker != asker {
			t.Errorf("%s was asked as %+v, not as the reader", q.Kind, q.Asker)
		}
	}
	var details []string
	for _, q := range f.asked {
		if q.Kind == gitlet.ReadRepo {
			details = append(details, q.Repo)
		}
	}
	if !slices.Equal(details, []string{"tools", "web"}) {
		t.Errorf("read in detail: %v, want the two the settings do not declare", details)
	}
	for _, key := range []string{"gitlet/api", "gitlet/tools", "gitlet/web", "gitlet/web#test"} {
		k := atlas.KindRepository
		if strings.Contains(key, "#") {
			k = atlas.KindPipeline
		}
		if _, ok := a.View.Find(atlas.MustRef(k, key).ID()); !ok {
			t.Errorf("%s is not in the atlas", key)
		}
	}
	if !slices.ContainsFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == gather.SourceGitlet && c.Complete }) {
		t.Errorf("a whole Gitlet read is not reported complete: %+v", a.Coverage)
	}
}

// A SOURCE THAT FAILS IS REPORTED, NOT HIDDEN: the settings still answer, and
// the read says Gitlet is unread and why. A failed detail still lists the
// repository and says the read is partial.
func TestASourceThatFailsIsReportedNotHidden(t *testing.T) {
	t.Parallel()
	f := &fakeGitlet{repos: []gitlet.RepoSummary{repo("api")}, page: 10,
		fail: map[gitlet.ReadKind]error{gitlet.ReadRepos: errors.New("estate unavailable")}}
	a := gather.Read(context.Background(), gather.Sources{Gitlet: f}, gather.Request{Company: acme(), Revision: "r1",
		Asker: asker, Now: now})
	if _, ok := a.View.Find(atlas.MustRef(atlas.KindAccount, "gitlet").ID()); !ok {
		t.Error("the settings no longer answer once Gitlet fails")
	}
	got := a.Coverage[slices.IndexFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == gather.SourceGitlet })]
	if got.Complete || !strings.Contains(got.Reason, "estate unavailable") {
		t.Errorf("a failed Gitlet read: %+v", got)
	}

	f = &fakeGitlet{repos: []gitlet.RepoSummary{repo("tools")}, page: 10,
		fail: map[gitlet.ReadKind]error{gitlet.ReadRepo: errors.New("node draining")}}
	a = gather.Read(context.Background(), gather.Sources{Gitlet: f}, gather.Request{Company: acme(), Revision: "r1",
		Asker: asker, Now: now})
	if _, ok := a.View.Find(atlas.MustRef(atlas.KindRepository, "gitlet/tools").ID()); !ok {
		t.Error("a repository whose detail failed is not listed")
	}
	got = a.Coverage[slices.IndexFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == gather.SourceGitlet })]
	if got.Complete || !strings.Contains(got.Reason, "tools") {
		t.Errorf("a partial Gitlet read: %+v", got)
	}

	a = gather.Read(context.Background(), gather.Sources{}, gather.Request{Company: acme(), Revision: "r1", Now: now})
	got = a.Coverage[slices.IndexFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == gather.SourceGitlet })]
	if got.Complete || got.Reason != gather.ErrNoSource.Error() {
		t.Errorf("a node serving no Gitlet: %+v", got)
	}
}

// A CREDENTIAL BESIDE A REPOSITORY'S OTHER HOST IS NEVER READ: the domain keeps
// a reference there, and the atlas takes the address alone.
func TestACredentialBesideAnotherHostIsNeverRead(t *testing.T) {
	t.Parallel()
	f := &fakeGitlet{repos: []gitlet.RepoSummary{repo("tools")}, page: 10, remotes: map[string]gitlet.RemoteState{
		"tools": {MirrorFrom: &gitlet.MirrorSource{URL: "https://github.com/acme/tools", Credential: "${MIRROR_TOKEN}"}},
	}}
	a := gather.Read(context.Background(), gather.Sources{Gitlet: f}, gather.Request{Company: acme(), Revision: "r1",
		Asker: asker, Now: now})
	for _, e := range a.View.Entries {
		for _, fv := range e.Facts {
			if strings.Contains(fv.Value.Text, "MIRROR_TOKEN") {
				t.Fatalf("%s carries the mirror's credential", e.Ref)
			}
		}
	}
	if len(a.Withheld) != 0 {
		t.Errorf("withheld: %+v", a.Withheld)
	}
}
