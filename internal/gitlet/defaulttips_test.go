package gitlet_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DEFAULT TIPS LISTING NAMES EVERY ACTIVE PROJECT REPOSITORY, by name,
// with who may read it and its default branch's tip: an empty repository with
// no tip, a unit-only one marked as one, and one repository alone where the
// read names it. It is the engine's own read, so it names no asker.
//
// Mutation: drop the tip's join from defaultTipsSQL, and a repository that
// has a default branch is listed with no tip.
func TestTheDefaultTipsListingNamesEveryActiveProjectRepositoryAndItsTip(t *testing.T) {
	t.Parallel()
	n, private := unitRepos(t)
	web := repoOn(t, n, "web")
	n.ensure("empty", suiteSettings(), t1)
	n.drain()
	stale := statelog.Freshness{Level: statelog.ReadStale}
	all, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadDefaultTips, Fresh: stale})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		visibility gitlet.Visibility
		head       string
	}
	got := map[string]row{}
	var order []string
	for _, tip := range all.DefaultTips {
		got[tip.Name] = row{tip.Visibility, tip.Head}
		order = append(order, tip.Name)
	}
	want := map[string]row{
		"empty":         {gitlet.VisibilityCompany, ""},
		"plat-private":  {gitlet.VisibilityUnit, oidOf("secret")},
		"platform":      {gitlet.VisibilityCompany, ""},
		"sales-private": {gitlet.VisibilityUnit, ""},
		"web":           {gitlet.VisibilityCompany, oidOf("A")},
	}
	if len(got) != len(want) || all.Next != "" {
		t.Fatalf("default_tips listed %v (next %q), want %d repositories on one page", order, all.Next, len(want))
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s is listed as %+v, want %+v", name, got[name], w)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("the listing is not by name: %v", order)
		}
	}

	one := func(id string) []gitlet.DefaultTip {
		t.Helper()
		a, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadDefaultTips, Fresh: stale,
			DefaultTips: &gitlet.DefaultTipsQuery{RepoID: id}})
		if err != nil {
			t.Fatal(err)
		}
		return a.DefaultTips
	}
	if tips := one(web); len(tips) != 1 || tips[0].ID != web || tips[0].Head != oidOf("A") {
		t.Errorf("default_tips of web answered %+v, want its one row", tips)
	}
	if tips := one(private); len(tips) != 1 || tips[0].Visibility != gitlet.VisibilityUnit {
		t.Errorf("default_tips of the unit-only repository answered %+v, want it marked unit-only", tips)
	}
	if tips := one("0192f3a0-7c1e-7b3a-9d2e-5f6a7b8c9d0e"); len(tips) != 0 {
		t.Errorf("default_tips of no repository answered %+v, want no row", tips)
	}

	for name, q := range map[string]gitlet.ReadQuery{
		"an asker":       {Kind: gitlet.ReadDefaultTips, Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1"}},
		"a repository":   {Kind: gitlet.ReadDefaultTips, Repo: "web"},
		"no uuid":        {Kind: gitlet.ReadDefaultTips, DefaultTips: &gitlet.DefaultTipsQuery{RepoID: "web"}},
		"one and a page": {Kind: gitlet.ReadDefaultTips, Cursor: "a", DefaultTips: &gitlet.DefaultTipsQuery{RepoID: web}},
		"another kind":   {Kind: gitlet.ReadIndexed, DefaultTips: &gitlet.DefaultTipsQuery{RepoID: web}},
	} {
		q.Fresh = stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}
