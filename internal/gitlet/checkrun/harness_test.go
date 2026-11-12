package checkrun_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RUNNER OVER A REAL CHECKS WRITER: every claim, box name and ending a
// runner or a home's sweep asks is decided by the shipped checks writer on a
// data node over an embedded broker, as a record on the repository's checks
// subject applied by the shipped applier — so what a case says of an attempt
// is what its row holds. The boxes are the sandbox catalogue's own manager
// over fake backends, whose boxes run the check's command as long as a case
// says.

// gitBase is Gitlet's base every case's boxes are given.
const gitBase = "https://git.example.com/box/git"

// codeBase is where every case's boxes read their commit without a clone: on
// another host than gitBase's, so a runner that spelled its own sibling of
// gitBase rather than taking the base it is given reads as one.
const codeBase = "https://read.example.com/box/gitlet"

// fast is the timing every case runs its runners at: a poll of twenty
// milliseconds, a stagger long enough to order two runners' claims, and an
// ending read for as long as a loaded runner takes to read one from memory.
var fast = checkrun.Timing{Poll: 20 * time.Millisecond, Stagger: 300 * time.Millisecond,
	Walk: 5 * time.Millisecond, Backstop: 50 * time.Millisecond, Renew: 20 * time.Millisecond, End: 10 * time.Second}

// estate is a data node's code domain as a runner reaches it: its reads and
// its checks writer, in process.
type estate struct{ d *gitlettest.DataNode }

func (e estate) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return e.d.Served().Read(ctx, q)
}

func (e estate) Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	return e.d.Served().PublishChecks(ctx, r)
}

// order is the git order a case fixes when it builds its runner: every
// repository's the same, and never moved under the runner, so it needs no
// lock.
type order struct{ nodes []string }

func (o *order) GitOrder(string) ([]string, error) { return slices.Clone(o.nodes), nil }

// tokens mints a token naming the attempt it is for.
type tokens struct{}

func (tokens) Mint(k gitlet.CheckKey, repo string, timeout time.Duration) string {
	return fmt.Sprintf("chk-%s-%s-%d-%s", repo, k.Name, k.Attempt, timeout)
}

// cell is one fake backend of the catalogue whose boxes run a check's command
// until a case ends it: every spec it was asked for, and each box's job.
type cell struct {
	*sandbox.FakeProvider
	mu    sync.Mutex
	specs []sandbox.Spec
	boxes []*sandbox.FakeSandbox
	ended map[string]bool
	made  chan *sandbox.FakeSandbox
}

func newCell() *cell {
	return &cell{FakeProvider: sandbox.NewFakeProvider(), ended: map[string]bool{},
		made: make(chan *sandbox.FakeSandbox, 64)}
}

func (c *cell) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	made, err := c.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	box := made.(*sandbox.FakeSandbox)
	id := box.ID()
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if strings.HasPrefix(cmd, "kill -0 ") {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.ended[id] {
				return sandbox.ExecResult{ExitCode: 1}, nil
			}
		}
		return sandbox.ExecResult{}, nil
	}
	c.mu.Lock()
	c.specs = append(c.specs, spec)
	c.boxes = append(c.boxes, box)
	c.mu.Unlock()
	c.made <- box
	return box, nil
}

// end ends box's command with status, its output written as said.
func (c *cell) end(box *sandbox.FakeSandbox, status int, output string) {
	dir := box.Home() + "/.crewlet-check"
	box.Put(dir+"/output", output)
	box.Put(dir+"/exit", strconv.Itoa(status)+"\n")
	c.mu.Lock()
	c.ended[box.ID()] = true
	c.mu.Unlock()
}

// box is the next box the cell makes, within a few seconds.
func (c *cell) box(t *testing.T) *sandbox.FakeSandbox {
	t.Helper()
	select {
	case b := <-c.made:
		return b
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("no box was made")
		return nil
	}
}

// made is how many boxes the cell has made.
func (c *cell) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.boxes)
}

// spec is the i-th spec the cell was asked for.
func (c *cell) spec(i int) sandbox.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.specs[i]
}

// fleet is a case's data node, its repository and the catalogue its runners
// make boxes from.
type fleet struct {
	t         *testing.T
	d         *gitlettest.DataNode
	repo      string
	container *cell
	e2b       *cell
	manager   *sandbox.Manager
	company   *config.Company
	faults    *gitletfault.Script
	// leases and counter are the company's coordination store: the remote
	// cells' slots and the fleet's counter their box-minutes are charged to.
	// The leases keep A CLOCK THE CASE HOLDS, frozen where the fleet began and
	// moved only by its Advance: a slot lapses only where a case lapses it, so
	// a slot nobody gave back stays live however long a case waits, and a
	// wait for every slot given back is met by the giving alone — never by a
	// lease's TTL running out on the wall clock a minute later.
	leases  *memory.Backend
	counter *memory.Fleet
}

// newFleet is a data node holding the repository web, and a catalogue of a
// container and an e2b cell.
func newFleet(t *testing.T) *fleet {
	t.Helper()
	dir := t.TempDir()
	q, err := js.Open(t.Context(), js.Config{StoreDir: filepath.Join(dir, "stream"),
		StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	began := time.Now().UTC()
	f := &fleet{t: t, faults: &gitletfault.Script{}, leases: &memory.Backend{Clock: func() time.Time { return began }},
		counter: memory.NewFleet()}
	f.d = gitlettest.StartDataNode(t, gitlettest.DataOptions{ID: "data-a", Queue: q, Dir: dir, LogBytes: 64 << 20,
		Faults: f.faults})
	f.d.Declare(t, time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		gitlet.Declared{Name: "web", Settings: gitlettest.Settings()})
	ref, err := f.d.Served().Resolve(t.Context(), gitlet.ResolveQuery{Name: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		t.Fatalf("resolve web: %v", err)
	}
	f.repo = ref.ID
	f.container, f.e2b = newCell(), newCell()
	f.manager, err = sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Container: f.container, sandbox.E2B: f.e2b},
		DefaultPlacement: sandbox.Container,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	f.company = &config.Company{Code: config.Code{Backend: config.CodeGitlet, Checks: config.CodeChecks{
		RunIn: config.PlacementContainer, AllowRunIn: []config.Placement{config.PlacementContainer, config.PlacementE2B},
	}}}
	return f
}

// options are a runner's on node over the fleet, with slots container cells,
// ranked by o.
func (f *fleet) options(node string, slots int, o checkrun.Order) checkrun.Options {
	return checkrun.Options{NodeID: node, Estate: estate{f.d}, Order: o,
		Boxes:   func() (checkrun.Boxes, bool) { return f.manager, true },
		Company: f.companyOf, Tokens: tokens{}, GitBase: gitBase, CodeBase: codeBase, Slots: slots, Timing: fast,
		Faults: f.faults, Leases: f.leases, Minutes: f.counter}
}

// companyOf is the case's company, its ${VAR} references resolved from env.
func (f *fleet) companyOf() (checkrun.Company, bool) {
	return checkrun.Company{Config: f.company, Resolve: func(v string) (string, []string) {
		return expand(v)
	}}, true
}

// secrets are the variables a case's ${VAR} references resolve from.
var secrets = map[string]string{"NPM_SECRET": "npm-from-the-environment"}

// expand resolves value's ${VAR} references from secrets.
func expand(value string) (string, []string) {
	var missing []string
	out := value
	for name, v := range secrets {
		out = strings.ReplaceAll(out, "${"+name+"}", v)
	}
	if i := strings.Index(out, "${"); i >= 0 {
		missing = append(missing, strings.TrimSuffix(out[i+2:], "}"))
	}
	return out, missing
}

// runner starts a runner on node, stopped with the case.
func (f *fleet) runner(o checkrun.Options) *checkrun.Runner {
	f.t.Helper()
	r, err := checkrun.New(o)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	r.Start()
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.Stop(ctx)
	})
	return r
}

// key is attempt of check name of the commit oid on the case's repository,
// checked for main.
func (f *fleet) key(oid, name string, attempt int) gitlet.CheckKey {
	return gitlet.CheckKey{RepoID: f.repo, OID: oid, Target: "refs/heads/main", Name: name, Attempt: attempt}
}

// oidOf is a commit id spelled from s.
func oidOf(s string) string { return strings.Repeat(s, 40)[:40] }

// queue queues def's first attempt at oid, as the home's dispatch does.
func (f *fleet) queue(oid string, def gitlet.CheckDef) gitlet.CheckKey {
	f.t.Helper()
	key := f.key(oid, def.Name, 1)
	f.queueAs(key, def, gitlet.QueueDispatch)
	return key
}

// queueAs queues key under def for reason.
func (f *fleet) queueAs(key gitlet.CheckKey, def gitlet.CheckDef, reason gitlet.QueueReason) (gitlet.ChecksResult,
	error) {
	f.t.Helper()
	res, err := f.d.Served().PublishChecks(f.t.Context(), gitlet.ChecksRequest{
		OpID: statelog.NewOpID(time.Now(), "code_check_queue"), Queued: &gitlet.CheckQueued{CheckKey: key,
			Purpose: gitlet.PurposeTip, Def: &def, Reason: reason, QueuedAt: time.Now().UTC()}})
	if err == nil && reason == gitlet.QueueDispatch && res.Refusal != "" {
		f.t.Fatalf("queue %s: refused %s: %s", key.Name, res.Refusal, res.Detail)
	}
	if err != nil && reason == gitlet.QueueDispatch {
		f.t.Fatalf("queue %s: %v", key.Name, err)
	}
	return res, err
}

// row is key's attempt as its row stands, false where there is none.
func (f *fleet) row(key gitlet.CheckKey) (gitlet.CheckRow, bool) {
	f.t.Helper()
	a, err := f.d.Served().Read(f.t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCheckBox, CheckBox: &key,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		f.t.Fatalf("read %s attempt %d: %v", key.Name, key.Attempt, err)
	}
	if len(a.Checks) != 1 {
		return gitlet.CheckRow{}, false
	}
	return a.Checks[0], true
}

// queuedAt is key's attempt's queuing instant as its row holds it: the
// instant every rank's stagger is counted from.
func (f *fleet) queuedAt(key gitlet.CheckKey) time.Time {
	f.t.Helper()
	return f.await(key, "queued", func(r gitlet.CheckRow) bool { return !r.QueuedAt.IsZero() }).QueuedAt
}

// await waits until key's row says what ok accepts, failing t past ten
// seconds with what it last said.
func (f *fleet) await(key gitlet.CheckKey, what string, ok func(gitlet.CheckRow) bool) gitlet.CheckRow {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, held := f.row(key)
		if held && ok(row) {
			return row
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			f.t.Fatalf("attempt %d of %s never %s: %+v", key.Attempt, key.Name, what, row)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// ended waits until key's attempt has ended.
func (f *fleet) ended(key gitlet.CheckKey) gitlet.CheckRow {
	f.t.Helper()
	return f.await(key, "ended", func(r gitlet.CheckRow) bool { return r.State.Ended() })
}

// detail is key's attempt read whole, as a person's `check` read reads it.
func (f *fleet) detail(key gitlet.CheckKey) gitlet.CheckDetail {
	f.t.Helper()
	a, err := f.d.Served().Read(f.t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCheck, Repo: "web",
		Asker:  gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-pat", Seat: gitlettest.FixturePat},
		Checks: &gitlet.CheckQuery{OID: key.OID, Target: key.Target, Name: key.Name, Attempt: key.Attempt},
		Fresh:  statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil || a.Check == nil {
		f.t.Fatalf("read %s attempt %d whole: (%+v, %v)", key.Name, key.Attempt, a.Check, err)
	}
	return *a.Check
}

// statelogZero is no floor: a read stale, as a home's backstop pass reads.
var statelogZero statelog.Position

// newOpID is a fresh operation id named name.
func newOpID(name string) string { return statelog.NewOpID(time.Now(), name) }

// def is a check of name running make name for timeout.
func def(name string, timeout time.Duration) gitlet.CheckDef {
	return gitlet.CheckDef{Name: name, Run: "make " + name, TimeoutSeconds: int(timeout / time.Second)}
}
