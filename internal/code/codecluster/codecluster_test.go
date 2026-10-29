package codecluster_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Not t.Parallel(): two three-member clusters at once on one runner is the
// contention internal/solo exists to remove.

// TestTheCodeHostPassesTheFleetSuiteOnTheBroker runs every multi-node case
// with the broker's own object store: one bucket, reached by each member
// through its own broker client, as a fleet's data nodes reach it.
func TestTheCodeHostPassesTheFleetSuiteOnTheBroker(t *testing.T) {
	codetest.RunFleet(t, func(t *testing.T) codetest.Cluster { return startCluster(t, "nats") })
}

// TestTheCodeHostPassesTheFleetSuiteOnS3 runs every multi-node case with S3,
// through an in-process bucket speaking its wire — the production store,
// whose puts go in parts and whose reads go in ranges.
func TestTheCodeHostPassesTheFleetSuiteOnS3(t *testing.T) {
	codetest.RunFleet(t, func(t *testing.T) codetest.Cluster { return startCluster(t, "s3") })
}

// EVERY PUSH OF A REPOSITORY IS APPENDED BY ITS COMMITTER, WHICHEVER MEMBER
// RECEIVED IT: for each of sixteen repositories a push through every member,
// and only the repository's committer — the first of its rendezvous order over
// the running members — appends anything. A router that asked its own member
// first would have each receiver commit its own pushes, and a burst to one
// repository would contend at the broker between three committers. And a
// COMPACTION of a repository, keyed by the same repository id, meets the same
// committer whichever member proposes it: asked through every member at once,
// it is appended once, by the committer alone (ADR-0060).
func TestEveryMemberRoutesARepositoryToOneCommitter(t *testing.T) {
	const repos = 16
	c := startCluster(t, "nats")
	declared := make([]code.Declared, repos)
	for i := range declared {
		declared[i] = code.Declared{Name: fmt.Sprintf("route-%02d", i), Settings: codetest.Settings()}
	}
	c.Declare(t, declared...)
	if err := c.Settle(t.Context()); err != nil {
		t.Fatalf("settle the fleet: %v", err)
	}
	p := codetest.NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	members := c.DataNodes()
	for _, d := range declared {
		rows, err := c.Rows(t.Context(), d.Name, codetest.Linearizable)
		if err != nil {
			t.Fatalf("the rows of %s: %v", d.Name, err)
		}
		committer := c.Committer(rows.Repo.ID)
		before := map[string]int{}
		for _, m := range members {
			before[m] = c.Appends(m)
		}
		results := make([]codetest.Pushed, len(members))
		var wg sync.WaitGroup
		for i, m := range members {
			url := urlOf(t, c, m, d.Name)
			wg.Go(func() { results[i] = p.Push(t.Context(), work, url, fmt.Sprintf("main:refs/heads/via-%s", m)) })
		}
		wg.Wait()
		for i, res := range results {
			if res.Err != nil {
				t.Errorf("%s: the push through %s: %s", d.Name, members[i], res)
			}
		}
		for _, m := range members {
			grew := c.Appends(m) - before[m]
			switch {
			case m == committer && grew == 0:
				t.Errorf("%s: its committer %s appended none of the pushes received by %v", d.Name, m, members)
			case m != committer && grew != 0:
				t.Errorf("%s: %s appended %d records, and the repository's committer is %s", d.Name, m, grew,
					committer)
			}
		}
	}
	compactionMeetsTheCommitter(t, c, p)
}

// compactionMeetsTheCommitter is TestEveryMemberRoutesARepositoryToOneCommitter's
// compaction: a repository of two pushes' packs, merged into one bundle stored
// as a compaction's output, proposed through every member at once under one
// compaction id — appended once, by the repository's committer alone.
func compactionMeetsTheCommitter(t *testing.T, c *cluster, p *codetest.Person) {
	t.Helper()
	const name = "route-compaction"
	c.Declare(t, code.Declared{Name: name, Settings: codetest.Settings()})
	members := c.DataNodes()
	work := p.Init()
	var head string
	for i := range 2 {
		head = p.Commit(work, map[string]string{"README.md": fmt.Sprintf("compaction %d\n", i)}, "compaction")
		if res := p.Push(t.Context(), work, urlOf(t, c, members[0], name), "HEAD:refs/heads/main"); res.Err != nil {
			t.Fatalf("the push to %s: %s", name, res)
		}
	}
	rows, err := c.Rows(t.Context(), name, codetest.Linearizable)
	if err != nil {
		t.Fatalf("the rows of %s: %v", name, err)
	}
	if len(rows.Packs) != 2 {
		t.Fatalf("%s holds %d packs after two pushes, want two", name, len(rows.Packs))
	}
	retire := []string{rows.Packs[0].PackID, rows.Packs[1].PackID}
	slices.Sort(retire)
	dir := gittest.Empty(t)
	checksum := gittest.Pack(t, p.Git(), dir, work, head)
	path := filepath.Join(dir, "objects", "pack", "pack-"+checksum+".pack")
	id := statelog.NewOpID(time.Now(), "")
	out, err := code.LayBundle(code.DeriveOutputPackID(id, 0), path, checksum)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewStore(c.Objects())
	if err != nil {
		t.Fatal(err)
	}
	if err := code.StoreBundle(t.Context(), objects, path, out, nil); err != nil {
		t.Fatal(err)
	}
	proposal := code.CompactionProposal{Compaction: code.Compaction{RepoID: rows.Repo.ID, CompactionID: id,
		Node: members[0], Seen: rows.Position, Retire: retire, Add: []code.PackRef{*out}}}
	committer := c.Committer(rows.Repo.ID)
	before := map[string]int{}
	for _, m := range members {
		before[m] = c.Appends(m)
	}
	results := make([]code.CompactResult, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Go(func() { results[i], errs[i] = c.member(t, m).router.Load().Code().Compact(t.Context(), proposal) })
	}
	wg.Wait()
	for i, res := range results {
		if errs[i] != nil || res.Outcome != statelog.OutcomeApplied {
			t.Errorf("the compaction asked through %s: %+v, %v; want applied, every ask collapsed onto one record",
				members[i], res, errs[i])
		}
	}
	for _, m := range members {
		grew := c.Appends(m) - before[m]
		switch {
		case m == committer && grew != 1:
			t.Errorf("the committer %s appended %d records for one compaction asked through every member, want one",
				m, grew)
		case m != committer && grew != 0:
			t.Errorf("%s appended %d records of a compaction, and the repository's committer is %s", m, grew,
				committer)
		}
	}
}

// TWO NODES QUEUE ONE CHECK: two members asking at once for the same attempt
// queued — two homes each believing a head is theirs to dispatch, each under
// an operation id of its own — meet at the repository's committing data node,
// whose checks writer decides the one after the other: the first appends, the
// second is answered from the attempt's row, and the fleet holds ONE row,
// queued, appended once by the committer alone.
//
// Mutation: answer the attempt queued already by appending again, and the
// committer appends twice for one attempt — red.
func TestTwoNodesQueueOneCheck(t *testing.T) {
	const name = "checks-one-row"
	c := startCluster(t, "nats")
	c.Declare(t, code.Declared{Name: name, Settings: codetest.Settings()})
	if err := c.Settle(t.Context()); err != nil {
		t.Fatalf("settle the fleet: %v", err)
	}
	rows, err := c.Rows(t.Context(), name, codetest.Linearizable)
	if err != nil {
		t.Fatalf("the rows of %s: %v", name, err)
	}
	repo := rows.Repo.ID
	members := c.DataNodes()
	committer := c.Committer(repo)
	before := map[string]int{}
	for _, m := range members {
		before[m] = c.Appends(m)
	}
	key := code.CheckKey{RepoID: repo, OID: "1111111111111111111111111111111111111111", Name: "unit", Attempt: 1}
	askers := members[:2]
	results := make([]code.ChecksResult, len(askers))
	errs := make([]error, len(askers))
	var wg sync.WaitGroup
	for i, m := range askers {
		req := code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_dispatch"),
			Queued: &code.CheckQueued{CheckKey: key, Purpose: code.PurposeTip, Target: "refs/heads/main",
				Def:    &code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600},
				Reason: code.QueueDispatch, QueuedAt: time.Now().UTC()}}
		wg.Go(func() { results[i], errs[i] = c.member(t, m).router.Load().Code().Checks(t.Context(), req) })
	}
	wg.Wait()
	unchanged := 0
	for i, res := range results {
		if errs[i] != nil || res.Outcome != statelog.OutcomeApplied || res.Row == nil ||
			res.Row.State != code.CheckStateQueued {
			t.Fatalf("the dispatch asked through %s answered (%+v, %v), want applied with the queued row",
				askers[i], res, errs[i])
		}
		if res.Unchanged {
			unchanged++
		}
	}
	if unchanged != 1 {
		t.Errorf("%d of the two dispatches were answered from the row, want the second of them", unchanged)
	}
	for _, m := range members {
		grew := c.Appends(m) - before[m]
		switch {
		case m == committer && grew != 1:
			t.Errorf("the committer %s appended %d records for one attempt queued twice, want one", m, grew)
		case m != committer && grew != 0:
			t.Errorf("%s appended %d records of a check, and the repository's committer is %s", m, grew,
				committer)
		}
	}
	read, err := c.member(t, members[len(members)-1]).router.Load().Code().Read(t.Context(), code.ReadQuery{
		Kind: code.ReadChecks, Repo: name, Checks: &code.CheckQuery{OID: key.OID}, Fresh: codetest.Linearizable,
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:ceo", Seat: "ceo"}})
	if err != nil {
		t.Fatalf("read the commit's checks: %v", err)
	}
	if len(read.Checks) != 1 || read.Checks[0].CheckKey != key || read.Checks[0].State != code.CheckStateQueued {
		t.Fatalf("the fleet holds %+v for the attempt, want one queued row", read.Checks)
	}
}

// EVERY MEMBER ROUTES A GIT READ TO ONE HOME (ADR-0059): a read of a
// repository by object id through any member's router is answered by the
// repository's home — the first of its rendezvous order over the running
// members, which every member computes alike — and by no other member, the
// home's own router answering in process; and with the home KILLED, it is
// answered by the home's runner-up, whichever survivor asked. A router that
// asked its own member first would warm a copy of every repository on every
// member its seats read through.
func TestEveryMemberRoutesAGitReadToOneHome(t *testing.T) {
	c := startCluster(t, "nats")
	c.Declare(t, code.Declared{Name: "read-home", Settings: codetest.Settings()})
	if err := c.Settle(t.Context()); err != nil {
		t.Fatalf("settle the fleet: %v", err)
	}
	rows, err := c.Rows(t.Context(), "read-home", codetest.Linearizable)
	if err != nil {
		t.Fatalf("the rows of read-home: %v", err)
	}
	members := c.DataNodes()
	order := rendezvous.Order(rows.Repo.ID, members)
	home, runnerUp := order[0], order[1]
	p := codetest.NewPerson(t, c, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "home\n"}, "home")
	if res := p.Push(t.Context(), work, urlOf(t, c, home, "read-home"), "HEAD:refs/heads/main"); res.Err != nil {
		t.Fatalf("the push through the home %s: %s", home, res)
	}
	q := code.GitQuery{Kind: code.GitTree, RepoID: rows.Repo.ID, Asker: codetest.Reader,
		Tree: &code.TreeQuery{Rev: head}}
	answered := func() map[string]int64 {
		out := map[string]int64{}
		for _, m := range members {
			out[m] = c.gitAnswered(t, m)
		}
		return out
	}
	for _, via := range members {
		before := answered()
		if a, err := c.GitReadThrough(t.Context(), via, q); err != nil || a.Tree == nil {
			t.Fatalf("the read through %s = (%+v, %v), want the repository's root listing", via, a.Tree, err)
		}
		after := answered()
		for _, m := range members {
			want := before[m]
			if m == home {
				want++
			}
			if after[m] != want {
				t.Errorf("the read through %s was answered %d times by %s, want it answered by the home %s "+
					"alone", via, after[m]-before[m], m, home)
			}
		}
	}

	c.Kill(t, home)
	defer c.Restart(t, home)
	before := answered()
	deadline := time.Now().Add(time.Minute)
	for served := false; !served; {
		for _, via := range order[1:] {
			if _, err := c.GitReadThrough(t.Context(), via, q); err == nil {
				served = true
				break
			}
		}
		if !served && time.Now().After(deadline) {
			t.Fatalf("no read was served within a minute of the home %s's death", home)
		}
		if !served {
			time.Sleep(100 * time.Millisecond)
		}
	}
	after := answered()
	if after[runnerUp] <= before[runnerUp] || after[order[2]] != before[order[2]] {
		t.Errorf("with the home %s dead, the reads were answered %d times by its runner-up %s and %d times "+
			"by %s, want the runner-up alone", home, after[runnerUp]-before[runnerUp], runnerUp,
			after[order[2]]-before[order[2]], order[2])
	}
}

// urlOf is the repository called name's URL through member node.
func urlOf(t *testing.T, c *cluster, node, name string) string {
	t.Helper()
	for _, n := range c.Nodes() {
		if n.ID() == node {
			return n.URL() + "/git/" + name
		}
	}
	t.Fatalf("member %s serves no git", node)
	return ""
}
