package query_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// fixture is a solo fleet holding codetest.RepoName, a person who pushes to it
// and their work tree.
type fixture struct {
	t    *testing.T
	f    *codetest.SoloFleet
	p    *codetest.Person
	work string
}

// newFixture is a fleet of o with codetest.RepoName declared under settings.
func newFixture(t *testing.T, o codetest.SoloOptions, settings code.RepoSettings) *fixture {
	t.Helper()
	f := codetest.SoloNode(t, o)
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings})
	x := &fixture{t: t, f: f, p: codetest.NewPerson(t, f, "ceo")}
	x.work = x.p.Init()
	return x
}

// url is name's URL through the i-th git node.
func (x *fixture) url(i int, name string) string { return x.f.Nodes()[i].URL() + "/git/" + name }

// commit commits files in the work tree and pushes main through the first git
// node, answering the commit.
func (x *fixture) commit(files map[string]string, message string) string {
	x.t.Helper()
	id := x.p.Commit(x.work, files, message)
	x.push(codetest.RepoName, "main")
	return id
}

// push pushes refspecs of the work tree to name through the first git node.
func (x *fixture) push(name string, refspecs ...string) {
	x.t.Helper()
	if res := x.p.PushArgs(x.t.Context(), x.work, []string{"--force"}, x.url(0, name), refspecs...); res.Err != nil {
		x.t.Fatalf("git push %v: %s", refspecs, res)
	}
}

// git runs the person's git in the work tree.
func (x *fixture) git(args ...string) string {
	x.t.Helper()
	return x.p.Git().Run(x.t, x.work, args...)
}

// repo is name's id, read at the log's head.
func (x *fixture) repo(name string) string {
	x.t.Helper()
	rows, err := x.f.Rows(x.t.Context(), name, codetest.Linearizable)
	if err != nil {
		x.t.Fatalf("the rows of %s: %v", name, err)
	}
	return rows.Repo.ID
}

// server is a query server over the i-th git node's cache and pool, o's
// unset fields filled: the node's own id, cache and pool, a share of its read
// lane, and no chart.
func (x *fixture) server(i int, o query.Options) *query.Server {
	x.t.Helper()
	n := x.f.Nodes()[i]
	if o.NodeID == "" {
		o.NodeID = n.ID()
	}
	if o.Cache == nil {
		o.Cache = n.Cache()
	}
	if o.Git == nil {
		o.Git = n.Pool()
	}
	if o.Share == nil {
		o.Share = code.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil)
	}
	if o.Roster == nil {
		o.Roster = func() code.Roster { return nil }
	}
	if o.Fetched == nil {
		o.Fetched = noFetched
	}
	s, err := query.New(o)
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

// noFetched keeps no read a server records.
func noFetched(context.Context, types.CodeFetched) {}

// fetches is a server's recorder of the reads it records (query.Options.Fetched).
type fetches struct {
	mu   sync.Mutex
	seen []types.CodeFetched
}

func (f *fetches) record(_ context.Context, e types.CodeFetched) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, e)
}

// all is every read recorded so far.
func (f *fetches) all() []types.CodeFetched {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// synced brings the i-th git node's copy of repo to the log's head, folding
// every push its rows hold into the node's index.
func (x *fixture) synced(i int, repo string) {
	x.t.Helper()
	s, err := x.f.Nodes()[i].Cache().Sync(x.t.Context(), repo, codetest.Linearizable)
	if err != nil {
		x.t.Fatalf("sync node %d's copy of %s: %v", i, repo, err)
	}
	s.Release()
}

// decoration is what q's provenance adds to every page of its answer on the
// first git node — the push that introduced each commit it names, and what
// the node's index holds (code.GitAnswer.PushedBy, Coverage) — read whole once
// the copy is level with the log, so every page read after it carries the
// same: the room a case scaling the page bound leaves for it, beside the
// kind's own.
func (x *fixture) decoration(q code.GitQuery) int {
	x.t.Helper()
	x.synced(0, q.RepoID)
	a := mustRead(x.t, x.server(0, query.Options{}), q)
	bare := a
	bare.PushedBy, bare.Coverage = nil, nil
	return encoded(x.t, a) - encoded(x.t, bare)
}

// person is the person every fixture pushes as, as a read names them.
var person = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}

// seat is the seat handle as a read names it.
func seat(handle string) code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: handle}
}

// treeQ, blobQ, diffQ and compareQ are a person's reads of repo, each asked of
// the node it is handed as its walk's effective home (code.GitQuery.Home), the
// one node a walk lets fill a copy: a case here asks one node directly, as the
// walk asks its first, and a read not stamped so is the case of
// TestANodeThatHoldsNoCopyFillsOnlyAsHome alone.
func treeQ(repo string, t code.TreeQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitTree, RepoID: repo, Asker: person, Home: true, Tree: &t}
}

func blobQ(repo string, b code.BlobQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: person, Home: true, Blob: &b}
}

func diffQ(repo string, d code.DiffQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitDiff, RepoID: repo, Asker: person, Home: true, Diff: &d}
}

func compareQ(repo, base, head string) code.GitQuery {
	return code.GitQuery{Kind: code.GitCompare, RepoID: repo, Asker: person, Home: true,
		Compare: &code.CompareQuery{Base: base, Head: head}}
}

// factsQ is the home's read of a revision's facts — the engine's own, which
// names no asker — and reanchorQ and interdiffQ are a person's reads of a
// change's revisions, asked as the other reads here are.
func factsQ(repo, head, target string) code.GitQuery {
	return code.GitQuery{Kind: code.GitRevisionFacts, RepoID: repo, Home: true,
		RevisionFacts: &code.RevisionFactsQuery{Head: head, Target: target}}
}

func reanchorQ(repo string, r code.ReanchorQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitReanchor, RepoID: repo, Asker: person, Home: true, Reanchor: &r}
}

func interdiffQ(repo string, i code.InterdiffQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitInterdiff, RepoID: repo, Asker: person, Home: true, Interdiff: &i}
}

// branch is the person's work tree on a new branch from at, the branch
// checked out — what a change's source branch is begun as.
func (x *fixture) branch(name, at string) {
	x.t.Helper()
	x.git("checkout", "-q", "-b", name, at)
}

// facts is the first git node's facts of head against target.
func (x *fixture) facts(s *query.Server, repo, head, target string) *code.RevisionFactsAnswer {
	x.t.Helper()
	a := mustRead(x.t, s, factsQ(repo, head, target))
	if a.RevisionFacts == nil {
		x.t.Fatalf("the facts of %s against %s answered no facts: %+v", head, target, a)
	}
	return a.RevisionFacts
}

// mustRead is s's answer to q, failing the case on an error.
func mustRead(t *testing.T, s *query.Server, q code.GitQuery) code.GitAnswer {
	t.Helper()
	a, err := s.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("Read(%s %+v): %v", q.Kind, q, err)
	}
	return a
}

// pages reads q page by page, each answer held to limit bytes of JSON,
// answering every page.
func pages(t *testing.T, s *query.Server, q code.GitQuery, limit int) []code.GitAnswer {
	t.Helper()
	var out []code.GitAnswer
	for range 10_000 {
		a := mustRead(t, s, q)
		if n := encoded(t, a); n > limit {
			t.Fatalf("page %d of the %s read encodes to %d bytes, past its %d", len(out), q.Kind, n, limit)
		}
		out = append(out, a)
		if a.Next == "" {
			return out
		}
		q.Cursor = a.Next
	}
	t.Fatalf("the %s read never ended", q.Kind)
	return nil
}

// encoded is a's JSON length.
func encoded(t *testing.T, a code.GitAnswer) int {
	t.Helper()
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return len(out)
}

// unready reports err as ErrGitUnready for why.
func unready(err error, why code.UnreadyReason) bool {
	var u *code.ErrGitUnready
	return errors.As(err, &u) && u.Reason == why
}

// recording is a query.Pool that records every Cmd it runs, and what each
// was fed on its standard input, before running it on the pool beneath.
type recording struct {
	query.Pool
	mu   sync.Mutex
	runs []run
}

// run is one Cmd a recording ran, and its whole standard input.
type run struct {
	cmd   gitproc.Cmd
	stdin string
}

func (r *recording) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	c.Args = slices.Clone(c.Args)
	var in []byte
	if c.Stdin != nil {
		var err error
		if in, err = io.ReadAll(c.Stdin); err != nil {
			return gitproc.Result{}, err
		}
		c.Stdin = bytes.NewReader(in)
	}
	r.mu.Lock()
	r.runs = append(r.runs, run{cmd: c, stdin: string(in)})
	r.mu.Unlock()
	return r.Pool.Run(ctx, c)
}

// ran is every Cmd recorded so far.
func (r *recording) ran() []gitproc.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmds := make([]gitproc.Cmd, len(r.runs))
	for i, x := range r.runs {
		cmds[i] = x.cmd
	}
	return cmds
}

// fed is the input of every process recorded so far whose subcommand is sub.
func (r *recording) fed(sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, x := range r.runs {
		if x.cmd.Args[0] == sub {
			out = append(out, x.stdin)
		}
	}
	return out
}

// since is every run recorded from the n-th on.
func (r *recording) since(n int) []run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs[min(n, len(r.runs)):])
}

// count is how many runs are recorded.
func (r *recording) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// counter is name's total over the series whose attributes include attrs.
func counter(rec *metrics.Recorder, name string, attrs map[string]string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range attrs {
			match = match && s.Attrs[k] == v
		}
		if match {
			n += s.Total
		}
	}
	return n
}

// chart is a roster: each seat's unit and tier, and each unit's lead and the
// unit above it.
type chart struct {
	seats  map[string]code.RosterSeat
	leads  map[string]string
	parent map[string]string
}

func (c chart) Seat(handle string) (code.RosterSeat, bool) {
	s, ok := c.seats[handle]
	return s, ok
}

func (c chart) Lead(unit string) string {
	for u := unit; u != ""; u = c.parent[u] {
		if l := c.leads[u]; l != "" {
			return l
		}
	}
	return ""
}

func (c chart) Leads(unit string) []string {
	var out []string
	for u := unit; u != ""; u = c.parent[u] {
		if l := c.leads[u]; l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func (c chart) Within(unit, ancestor string) bool {
	for u := unit; u != ""; u = c.parent[u] {
		if u == ancestor {
			return true
		}
	}
	return false
}

// company is the chart the grant tests read: ENG (led by eng-lead, holding
// eng-dev) and SALES (led by sales-lead, holding sales-rep).
func company() code.Roster {
	return chart{
		seats: map[string]code.RosterSeat{
			"eng-lead":   {Handle: "eng-lead", Tier: code.TierFullAccess, Unit: "ENG"},
			"eng-dev":    {Handle: "eng-dev", Tier: code.TierReadOnly, Unit: "ENG"},
			"sales-lead": {Handle: "sales-lead", Tier: code.TierReadOnly, Unit: "SALES"},
			"sales-rep":  {Handle: "sales-rep", Tier: code.TierFullAccess, Unit: "SALES"},
		},
		leads: map[string]string{"ENG": "eng-lead", "SALES": "sales-lead"},
	}
}
