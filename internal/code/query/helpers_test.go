package query_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
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
	s, err := query.New(o)
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

// person is the person every fixture pushes as, as a read names them.
var person = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}

// seat is the seat handle as a read names it.
func seat(handle string) code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: handle}
}

// treeQ, blobQ, diffQ and compareQ are a person's reads of repo.
func treeQ(repo string, t code.TreeQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitTree, RepoID: repo, Asker: person, Tree: &t}
}

func blobQ(repo string, b code.BlobQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: person, Blob: &b}
}

func diffQ(repo string, d code.DiffQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitDiff, RepoID: repo, Asker: person, Diff: &d}
}

func compareQ(repo, base, head string) code.GitQuery {
	return code.GitQuery{Kind: code.GitCompare, RepoID: repo, Asker: person,
		Compare: &code.CompareQuery{Base: base, Head: head}}
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

// recording is a query.Pool that records every Cmd it runs before running it
// on the pool beneath.
type recording struct {
	query.Pool
	mu   sync.Mutex
	cmds []gitproc.Cmd
	// stdin is what each cat-file batch was fed, in order.
	stdin []string
}

func (r *recording) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	r.mu.Lock()
	c.Args = slices.Clone(c.Args)
	r.cmds = append(r.cmds, c)
	if c.Stdin != nil && len(c.Args) > 1 && c.Args[0] == "cat-file" {
		b := new(strings.Builder)
		if src, ok := c.Stdin.(interface{ String() string }); ok {
			b.WriteString(src.String())
		}
		r.stdin = append(r.stdin, b.String())
	}
	r.mu.Unlock()
	return r.Pool.Run(ctx, c)
}

// ran is every Cmd recorded so far.
func (r *recording) ran() []gitproc.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cmds)
}

// fed is every cat-file batch's input so far.
func (r *recording) fed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.stdin)
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
