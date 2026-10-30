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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/config"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
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

// gitBase is the code host's base every case's boxes are given.
const gitBase = "https://git.example.com/box/git"

// fast is the timing every case runs its runners at: a poll of twenty
// milliseconds, a stagger long enough to order two runners' claims.
var fast = checkrun.Timing{Poll: 20 * time.Millisecond, Stagger: 300 * time.Millisecond,
	Walk: 5 * time.Millisecond, Backstop: 50 * time.Millisecond}

// estate is a data node's code domain as a runner reaches it: its reads and
// its checks writer, in process.
type estate struct{ d *codetest.DataNode }

func (e estate) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return e.d.Served().Read(ctx, q)
}

func (e estate) Checks(ctx context.Context, r code.ChecksRequest) (code.ChecksResult, error) {
	return e.d.Served().PublishChecks(ctx, r)
}

// order is a git order a case sets: every repository's the same.
type order struct {
	mu    sync.Mutex
	nodes []string
}

func (o *order) GitOrder(string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.nodes), nil
}

func (o *order) set(nodes ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nodes = nodes
}

// tokens mints a token naming the attempt it is for.
type tokens struct{}

func (tokens) Mint(k code.CheckKey, repo string, timeout time.Duration) string {
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
	case <-time.After(10 * time.Second):
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
	d         *codetest.DataNode
	repo      string
	container *cell
	e2b       *cell
	manager   *sandbox.Manager
	company   *config.Company
	faults    *codefault.Script
}

// newFleet is a data node holding the repository web, and a catalogue of a
// container and an e2b cell.
func newFleet(t *testing.T) *fleet {
	t.Helper()
	dir := t.TempDir()
	q, err := js.Open(t.Context(), js.Config{StoreDir: filepath.Join(dir, "stream")})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	f := &fleet{t: t, faults: &codefault.Script{}}
	f.d = codetest.StartDataNode(t, codetest.DataOptions{ID: "data-a", Queue: q, Dir: dir, LogBytes: 64 << 20,
		Faults: f.faults})
	f.d.Declare(t, time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		code.Declared{Name: "web", Settings: codetest.Settings()})
	ref, err := f.d.Served().Resolve(t.Context(), code.ResolveQuery{Name: "web",
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
	f.company = &config.Company{Code: config.Code{Native: true, Checks: config.CodeChecks{
		RunIn: config.PlacementContainer, AllowRunIn: []config.Placement{config.PlacementContainer, config.PlacementE2B},
	}}}
	return f
}

// options are a runner's on node over the fleet, with slots container cells,
// ranked by o.
func (f *fleet) options(node string, slots int, o checkrun.Order) checkrun.Options {
	return checkrun.Options{NodeID: node, Estate: estate{f.d}, Order: o,
		Boxes:   func() (checkrun.Boxes, bool) { return f.manager, true },
		Company: f.companyOf, Tokens: tokens{}, GitBase: gitBase, Slots: slots, Timing: fast, Faults: f.faults}
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
func (f *fleet) key(oid, name string, attempt int) code.CheckKey {
	return code.CheckKey{RepoID: f.repo, OID: oid, Target: "refs/heads/main", Name: name, Attempt: attempt}
}

// oidOf is a commit id spelled from s.
func oidOf(s string) string { return strings.Repeat(s, 40)[:40] }

// queue queues def's first attempt at oid, as the home's dispatch does.
func (f *fleet) queue(oid string, def code.CheckDef) code.CheckKey {
	f.t.Helper()
	key := f.key(oid, def.Name, 1)
	f.queueAs(key, def, code.QueueDispatch)
	return key
}

// queueAs queues key under def for reason.
func (f *fleet) queueAs(key code.CheckKey, def code.CheckDef, reason code.QueueReason) (code.ChecksResult,
	error) {
	f.t.Helper()
	res, err := f.d.Served().PublishChecks(f.t.Context(), code.ChecksRequest{
		OpID: statelog.NewOpID(time.Now(), "code_check_queue"), Queued: &code.CheckQueued{CheckKey: key,
			Purpose: code.PurposeTip, Def: &def, Reason: reason, QueuedAt: time.Now().UTC()}})
	if err == nil && reason == code.QueueDispatch && res.Refusal != "" {
		f.t.Fatalf("queue %s: refused %s: %s", key.Name, res.Refusal, res.Detail)
	}
	if err != nil && reason == code.QueueDispatch {
		f.t.Fatalf("queue %s: %v", key.Name, err)
	}
	return res, err
}

// row is key's attempt as its row stands, false where there is none.
func (f *fleet) row(key code.CheckKey) (code.CheckRow, bool) {
	f.t.Helper()
	a, err := f.d.Served().Read(f.t.Context(), code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &key,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		f.t.Fatalf("read %s attempt %d: %v", key.Name, key.Attempt, err)
	}
	if len(a.Checks) != 1 {
		return code.CheckRow{}, false
	}
	return a.Checks[0], true
}

// await waits until key's row says what ok accepts, failing t past ten
// seconds with what it last said.
func (f *fleet) await(key code.CheckKey, what string, ok func(code.CheckRow) bool) code.CheckRow {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, held := f.row(key)
		if held && ok(row) {
			return row
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("attempt %d of %s never %s: %+v", key.Attempt, key.Name, what, row)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ended waits until key's attempt has ended.
func (f *fleet) ended(key code.CheckKey) code.CheckRow {
	f.t.Helper()
	return f.await(key, "ended", func(r code.CheckRow) bool { return r.State.Ended() })
}

// detail is key's attempt read whole, as a person's `check` read reads it.
func (f *fleet) detail(key code.CheckKey) code.CheckDetail {
	f.t.Helper()
	a, err := f.d.Served().Read(f.t.Context(), code.ReadQuery{Kind: code.ReadCheck, Repo: "web",
		Asker:  code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-pat", Seat: codetest.FixturePat},
		Checks: &code.CheckQuery{OID: key.OID, Target: key.Target, Name: key.Name, Attempt: key.Attempt},
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
func def(name string, timeout time.Duration) code.CheckDef {
	return code.CheckDef{Name: name, Run: "make " + name, TimeoutSeconds: int(timeout / time.Second)}
}
