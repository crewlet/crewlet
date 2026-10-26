package host_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
)

// THE HOST COUNTS EACH REQUEST'S USE INTO ITS REPOSITORY'S HEAT, as a person's
// own git makes the requests: a push is one push whatever its requests; a
// clone is one fetch — its advertisement — and one clone, since it offers the
// node nothing it holds; a fetch of what moved since is a fetch and no clone,
// since it offers its tips; a fetch that finds its client up to date is still
// a fetch; and every answer adds the bytes git wrote its client. The status
// reads it back with the node, its cache, and the repository's copy in
// service, among the hottest and asked about by id.
//
// Mutation: count no fetch at the advertisement, count a clone for any fetch
// that wants (ignoring its haves), or count no push at its record, and the
// counts below go wrong.
func TestTheHostCountsEachRequestsUseIntoItsRepositorysHeat(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	id := c.Rows().Repo.ID
	status := func(step string) host.RepoStatus {
		t.Helper()
		st := node.Host().Status(id)
		if st.Repo == nil || st.Repo.ID != id {
			t.Fatalf("%s: the status asked about %s answered %+v", step, id, st.Repo)
		}
		return *st.Repo
	}
	check := func(step string, got cache.Use, fetches, clones, pushes int64, bytesAbove int64) int64 {
		t.Helper()
		if got.Fetches != fetches || got.Clones != clones || got.Pushes != pushes || got.BytesOut <= bytesAbove {
			t.Fatalf("%s: the day's heat is %+v, want %d fetches, %d clones, %d pushes and more than %d "+
				"bytes out", step, got, fetches, clones, pushes, bytesAbove)
		}
		return got.BytesOut
	}

	bytes := check("the first push", status("the first push").Day, 0, 0, 1, 0)
	clone := p.Clone(c.URL(0))
	r := status("a clone")
	bytes = check("a clone", r.Day, 1, 1, 1, bytes)
	if r.Hour != r.Day || r.Name != codetest.RepoName {
		t.Errorf("within the hour the hour's heat %+v and the day's %+v differ, or the repository is named %q",
			r.Hour, r.Day, r.Name)
	}

	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	bytes = check("a second push", status("a second push").Day, 1, 1, 2, bytes)
	p.Git().Run(t, clone, "fetch", "--quiet", "origin")
	bytes = check("a fetch of what moved", status("a fetch of what moved").Day, 2, 1, 2, bytes)
	p.Git().Run(t, clone, "fetch", "--quiet", "origin")
	check("a fetch with nothing new", status("a fetch with nothing new").Day, 3, 1, 2, bytes)

	st := node.Host().Status("")
	if st.Node != node.ID() || st.Repo != nil || st.Cache.Warm != 1 || st.Cache.Bytes <= 0 {
		t.Fatalf("the node's status = node %q, repo %+v, cache %+v; want %s, no repository asked about, one "+
			"warm repository and its bytes", st.Node, st.Repo, st.Cache, node.ID())
	}
	if len(st.Hot) != 1 || st.Hot[0].ID != id || st.Hot[0].Copy == nil || st.Hot[0].Copy.Packs < 2 {
		t.Fatalf("the hot list = %+v, want the one repository with its copy in service holding both pushes",
			st.Hot)
	}
}
