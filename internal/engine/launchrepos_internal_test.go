package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CODING RUN GIVEN REPOSITORIES ON THE COMPANY'S OWN CODE HOST (ADR-0063),
// through the engine's own launcher: what each repository resolves to on the
// run's row, what the seat may be given, what the box's environment carries,
// and what the turn holds — over a code host that answers as the estate's
// router does, judging every read by the seat's grant from the epoch's chart.

// launchReposDoc is a company hosting code whose seats run code in a fake
// remote cell: an engineering unit owning the repositories — its lead, a
// full_access engineer with an email, and a read_only one — and a design unit
// whose full_access seat sits outside it.
const launchReposDoc = `
name: Acme
providers:
  sandbox:
    fake: true
  llm:
    zulu:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "sk-ant-fake-launch-repos-key"}
roles:
  - name: Founder
    kind: human
    contact: {slack_user_id: U0FOUNDER}
units:
  - name: Engineering
    id: eng
    lead: lead
    roles:
      - name: Lead
        handle: lead
        llm: zulu
        code: {access: full_access}
        sandbox: {enabled: true}
      - name: SWE
        handle: swe
        email: swe@acme.example.com
        llm: zulu
        code: {access: full_access}
        sandbox: {enabled: true}
      - name: Reader
        handle: reader
        llm: zulu
        code: {access: read_only}
        sandbox: {enabled: true}
  - name: Design
    id: design
    lead: designer
    roles:
      - name: Designer
        handle: designer
        llm: zulu
        code: {access: full_access}
        sandbox: {enabled: true}
code:
  native: true
  repos:
    - name: web
      owner: {unit: Engineering}
      protect:
        - branch: main
`

// Object ids the fake code host's repositories hold.
var (
	mainHead  = strings.Repeat("1", 40)
	tagHead   = strings.Repeat("2", 40)
	seatHead  = strings.Repeat("3", 40)
	abbrevOID = "abc1234" + strings.Repeat("5", 33)
)

// launchHost is the code host as the launcher's resolution reads it through
// the estate's router: repositories by every name that names one, their refs,
// the commits a read by object id finds, every read judged by the asker's
// grant from the epoch's chart (code.JudgeRead, as the serving node judges
// it), and every lifecycle request — a hold record — applied.
type launchHost struct {
	mu     sync.Mutex
	roster func() code.Roster
	repos  map[string]code.RepoRef
	refs   map[string]map[string]string // repo id → whole ref → oid
	// commits are the ids a read by object id expands an abbreviation to.
	commits []string
	// read, resolved and git are where each kind of read answers, and
	// floor this node's own writes to every repository.
	read, resolved, git, floor statelog.Position
	lifecycles                 []code.LifecycleRequest
}

func newLaunchHost(e *Engine) *launchHost {
	return &launchHost{roster: e.codeRoster, repos: map[string]code.RepoRef{},
		refs: map[string]map[string]string{}, commits: []string{abbrevOID},
		read: codeLogAt(5), resolved: codeLogAt(7), git: codeLogAt(8), floor: codeLogAt(9)}
}

// add declares repo, its refs, and every other name it is known by.
func (h *launchHost) add(repo code.RepoRef, refs map[string]string, aliases ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.repos[repo.Name] = repo
	for _, alias := range aliases {
		h.repos[alias] = repo
	}
	h.refs[repo.ID] = refs
}

func (h *launchHost) judge(name string, asker code.Principal, ceiling string) (code.RepoRef, error) {
	repo, ok := h.repos[name]
	if !ok {
		return code.RepoRef{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, name)
	}
	if _, err := code.JudgeRead(asker, ceiling, repo, h.roster()); err != nil {
		return code.RepoRef{}, err
	}
	return repo, nil
}

func (h *launchHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q.Kind != code.ReadRepo {
		return code.ReadAnswer{}, fmt.Errorf("%w: a launch read kind %s", code.ErrInvalid, q.Kind)
	}
	repo, err := h.judge(q.Repo, q.Asker, q.CeilingSeat)
	if err != nil {
		return code.ReadAnswer{}, err
	}
	repo.Position = h.read
	return code.ReadAnswer{Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: repo}},
		Position: h.read, Level: q.Fresh.Level}, nil
}

func (h *launchHost) ResolveRef(_ context.Context, q code.RefQuery) (code.RefAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	repo, err := h.judge(q.Repo, q.Asker, q.CeilingSeat)
	if err != nil {
		return code.RefAnswer{}, err
	}
	out := code.RefAnswer{Repo: repo, Position: h.resolved}
	for _, asked := range q.Refs {
		names := []string{asked}
		if !strings.HasPrefix(asked, "refs/") {
			names = []string{"refs/heads/" + asked, "refs/tags/" + asked}
		}
		resolved := code.ResolvedRef{Name: asked, Missing: "repository " + repo.Name + " holds no live " +
			strings.Join(names, " or ")}
		for _, name := range names {
			if oid, ok := h.refs[repo.ID][name]; ok {
				resolved = code.ResolvedRef{Name: asked, Ref: name, OID: oid, Moved: codeLogAt(4)}
				break
			}
		}
		out.Refs = append(out.Refs, resolved)
	}
	return out, nil
}

func (h *launchHost) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q.Kind != code.GitTree || q.Tree == nil {
		return code.GitAnswer{}, fmt.Errorf("%w: a launch git read kind %s", code.ErrInvalid, q.Kind)
	}
	for _, oid := range h.commits {
		if strings.HasPrefix(oid, q.Tree.Rev) {
			return code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Rev: oid, Position: h.git,
				Tree: &code.TreeAnswer{Tree: oid, Entries: []code.TreeEntry{}}}, nil
		}
	}
	return code.GitAnswer{}, fmt.Errorf("%w: %s", code.ErrObjectNotFound, q.Tree.Rev)
}

func (h *launchHost) FloorOf(string) statelog.Position { return h.floor }

func (h *launchHost) Lifecycle(_ context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lifecycles = append(h.lifecycles, r)
	return code.LifecycleResult{Outcome: statelog.OutcomeApplied, Position: codeLogAt(30)}, nil
}

// reposRig is an engine whose run_sandbox launches reach a fake remote cell,
// serving a code host its launcher resolves repositories through.
type reposRig struct {
	e        *Engine
	c        *Company
	host     *launchHost
	provider *sandbox.FakeProvider
	jobs     map[string]*sandbox.FakeRunner
	tokens   *host.BoxTokens
	pending  sandbox.PendingStore
}

// newReposRig is a rig over doc, its code host at base (CREWLET_BOX_GIT_URL
// as the boot read it, "" for unset).
func newReposRig(t *testing.T, doc, base string) *reposRig {
	t.Helper()
	c := specCompany(t, doc)
	c.roster = rosterOf(c)
	jobs := map[string]*sandbox.FakeRunner{codingagent.ClaudeCodeName: sandbox.NewFakeRunner(codingagent.ClaudeCodeName)}
	provider := sandbox.NewFakeProvider()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          map[sandbox.Placement]sandbox.Provider{sandbox.E2B: provider},
		Runners:            map[string]sandbox.Runner{codingagent.ClaudeCodeName: jobs[codingagent.ClaudeCodeName]},
		DefaultCodingAgent: codingagent.ClaudeCodeName,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	q := memory.New()
	pending := sandbox.NewCoordStore(coordmemory.NewFleet())
	tokens, err := host.NewBoxTokens(runtoken.Material{ActiveID: "k1",
		Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the launch rig's run-token key"}}}, nil)
	if err != nil {
		t.Fatalf("NewBoxTokens: %v", err)
	}
	e := &Engine{backends: &Backends{Queue: q}, boxTokens: tokens, boxGitURL: base}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes:   noPushes{},
		Audience: noAudience{}, Queue: q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.useSandbox(pending, coordinator)
	e.epoch.current.Store(c)
	return &reposRig{e: e, c: c, host: newLaunchHost(e), provider: provider, jobs: jobs, tokens: tokens,
		pending: pending}
}

// launcher is the rig's run_sandbox launcher, its code tools' seams over the
// rig's code host — as the epoch's equip hands them.
func (r *reposRig) launcher() *launcher {
	return &launcher{engine: r.e, code: builtin.CodeDeps{Code: r.host, Holder: r.host,
		Requesters: noRequester{}, Chart: authz.NoChart{}}}
}

// turn is a fresh turn of the seat handle, charged to item (nil for none).
func (r *reposRig) turn(t *testing.T, handle string, item *types.WorkItem) *turnctx.Turn {
	t.Helper()
	seat := r.c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		t.Fatalf("no seat %s", handle)
	}
	return &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat, Org: r.c.Org,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil), WorkItem: item}
}

// launch launches turn's run with repos and answers its row.
func (r *reposRig) launch(t *testing.T, turn *turnctx.Turn, repos ...builtin.RepoRequest) (sandbox.PendingRun, error) {
	t.Helper()
	_, err := r.launcher().Launch(t.Context(), turn, builtin.SandboxRequest{Brief: "fix the failing test",
		Repos: repos})
	if err != nil {
		return sandbox.PendingRun{}, err
	}
	row, found, getErr := r.pending.Get(t.Context(), turn.RunID)
	if getErr != nil || !found {
		t.Fatalf("the launched run's row: found %v, %v", found, getErr)
	}
	return row, nil
}

// project is a repository of a unit, visibility company unless said, its
// default branch main.
func project(name, unit string, visibility code.Visibility) code.RepoRef {
	return code.RepoRef{ID: uuid.NewString(), Name: name, Status: code.RepoStatusActive,
		ManagedBy: code.ManagedConfig, Kind: code.RepoProject, Owner: code.Owner{Kind: code.OwnerUnit, Ref: unit},
		Visibility: visibility, DefaultBranch: "main", Position: codeLogAt(5)}
}

// A LAUNCH RESOLVES EACH REPOSITORY into the run's grant on its row: the
// branch is the seat's own for the turn's work item — seats/<handle>/<key>,
// never the turn's dedupe key — or for the run where the turn is on none, the
// same branch for a second call of the turn; its base is that branch's head
// where it exists (a second run on one item continues the first's work), else
// the default branch's, none for an empty repository, a tag as resolved, and
// an abbreviated id expanded by a read by object id; a renamed repository's
// run names its current name; and the run's floor is the furthest position
// any read answered at and this node's own writes.
//
// Mutations: the base from the default branch always → the existing-branch
// row, red; the branch from turnctx.Turn.WorkKey → the ENG-42 row's branch is
// a digest, red.
func TestALaunchResolvesEachRepository(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	item := &types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42"}
	for _, tc := range []struct {
		name     string
		refs     map[string]string
		item     *types.WorkItem
		req      builtin.RepoRequest
		aliases  []string
		branch   string // "" is the run form, checked against the turn
		wantBase string
	}{
		{"the item's branch exists, and is the base", map[string]string{"refs/heads/main": mainHead,
			"refs/heads/seats/swe/ENG-42": seatHead}, item, builtin.RepoRequest{Repo: "web"},
			nil, "seats/swe/ENG-42", seatHead},
		{"the item's branch is absent: the default branch's head", map[string]string{"refs/heads/main": mainHead},
			item, builtin.RepoRequest{Repo: "web"}, nil, "seats/swe/ENG-42", mainHead},
		{"an empty repository has no base", map[string]string{}, item, builtin.RepoRequest{Repo: "web"}, nil,
			"seats/swe/ENG-42", ""},
		{"a tag named as the base", map[string]string{"refs/heads/main": mainHead, "refs/tags/v1": tagHead},
			item, builtin.RepoRequest{Repo: "web", Base: "v1"}, nil, "seats/swe/ENG-42", tagHead},
		{"an abbreviated id expanded", map[string]string{"refs/heads/main": mainHead}, item,
			builtin.RepoRequest{Repo: "web", Base: "abc1234"}, nil, "seats/swe/ENG-42", abbrevOID},
		{"a renamed repository's run names its current name", map[string]string{"refs/heads/main": mainHead},
			item, builtin.RepoRequest{Repo: "site"}, []string{"site"}, "seats/swe/ENG-42", mainHead},
		{"a turn on no work item works on the run's branch", map[string]string{"refs/heads/main": mainHead},
			nil, builtin.RepoRequest{Repo: "web"}, nil, "", mainHead},
		{"a branch named in the call", map[string]string{"refs/heads/main": mainHead}, item,
			builtin.RepoRequest{Repo: "web", Branch: "seats/swe/spike"}, nil, "seats/swe/spike", mainHead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newReposRig(t, launchReposDoc, base)
			web := project("web", "Engineering", code.VisibilityCompany)
			rig.host.add(web, tc.refs, tc.aliases...)
			turn := rig.turn(t, "swe", tc.item)
			row, err := rig.launch(t, turn, tc.req)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			want := tc.branch
			if want == "" {
				id := uuid.MustParse(turn.RunID).String()
				want = "seats/swe/run-" + id[len(id)-12:]
			}
			if len(row.Repos) != 1 {
				t.Fatalf("the row grants %+v, want one repository", row.Repos)
			}
			got := row.Repos[0]
			if got.Repo != "web" || got.RepoID != web.ID || got.Branch != want || got.Base != tc.wantBase {
				t.Errorf("the run is given %+v, want web (%s) on %s from %q", got, web.ID, want, tc.wantBase)
			}
			if row.CodeFloor != codeLogAt(9).String() {
				t.Errorf("the run's floor is %s, want the furthest of every read and this node's own writes, %s",
					row.CodeFloor, codeLogAt(9))
			}
			if tc.item == nil {
				// THE SECOND CALL OF THE TURN works on the same branch.
				again, err := rig.launch(t, turn, tc.req)
				if err != nil || len(again.Repos) != 1 || again.Repos[0].Branch != want {
					t.Errorf("the turn's second call works on %+v (%v), want %s again", again.Repos, err, want)
				}
			}
		})
	}
}

// A BOX IS GIVEN ONLY ITS GRANTS AT LAUNCH (ADR-0063): a read_only seat asking
// to write is refused with its grant's sentence and given `read` where it asks
// nothing; a full_access seat outside the owning unit writes its own namespace
// and is refused writing main; a `visibility: unit` repository outside the
// seat's unit is refused with the grant's read refusal; an archived repository
// is given to read and refused to write. Nothing refused leaves a row.
//
// Mutation: skip the access precheck → the read_only seat is given write, red.
func TestABoxIsGivenOnlyItsGrantsAtLaunch(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	archived := project("old", "Engineering", code.VisibilityCompany)
	archived.Status = code.RepoStatusArchived
	for _, tc := range []struct {
		name, seat string
		req        builtin.RepoRequest
		access     sandbox.RepoAccess // "" is refused
		says       string
		class      tools.Refusal
	}{
		{"a read_only seat asking write", "reader", builtin.RepoRequest{Repo: "web", Access: "write"}, "",
			"pushing under its own namespace needs", tools.RefusalForbidden},
		{"a read_only seat asking nothing", "reader", builtin.RepoRequest{Repo: "web"}, sandbox.RepoRead, "", ""},
		{"a seat outside the unit, its own namespace", "designer", builtin.RepoRequest{Repo: "web",
			Access: "write"}, sandbox.RepoWrite, "", ""},
		{"a seat outside the unit, main", "designer", builtin.RepoRequest{Repo: "web", Access: "write",
			Branch: "main"}, "", "needs write", tools.RefusalForbidden},
		{"a unit repository outside the seat's unit", "designer", builtin.RepoRequest{Repo: "secret"}, "",
			"holds no access to repository", tools.RefusalForbidden},
		{"an archived repository, to read", "swe", builtin.RepoRequest{Repo: "old"}, sandbox.RepoRead, "", ""},
		{"an archived repository, to write", "swe", builtin.RepoRequest{Repo: "old", Access: "write"}, "",
			"is archived", tools.RefusalForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newReposRig(t, launchReposDoc, base)
			rig.host.add(project("web", "Engineering", code.VisibilityCompany),
				map[string]string{"refs/heads/main": mainHead})
			rig.host.add(project("secret", "Engineering", code.VisibilityUnit),
				map[string]string{"refs/heads/main": mainHead})
			rig.host.add(archived, map[string]string{"refs/heads/main": mainHead})
			turn := rig.turn(t, tc.seat, nil)
			row, err := rig.launch(t, turn, tc.req)
			if tc.access != "" {
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got := row.Repos[0].Access; got != tc.access {
					t.Errorf("the run is given %s, want %s", got, tc.access)
				}
				return
			}
			var refused *builtin.RefusedLaunch
			if !errors.As(err, &refused) || !strings.Contains(refused.Result.Output, tc.says) ||
				tools.RefusalOf(refused.Result) != tc.class {
				t.Fatalf("Launch answered %v, want a %s refusal saying %q", err, tc.class, tc.says)
			}
			if _, found, _ := rig.pending.Get(t.Context(), turn.RunID); found {
				t.Error("a refused launch left a run row")
			}
		})
	}
}

// A LAUNCH WITH REPOS NEEDS THE BOX URL: a node with no CREWLET_BOX_GIT_URL
// refuses a launch naming repositories, naming the variable, before anything
// is read or written — a box would be given a repository and no address to
// clone it from — and launches one naming none.
func TestALaunchWithReposNeedsTheBoxURL(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "")
	rig.host.add(project("web", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	turn := rig.turn(t, "swe", nil)
	_, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"})
	if err == nil || !strings.Contains(err.Error(), sandbox.BoxGitURLVar+" is not set") {
		t.Fatalf("Launch answered %v, want a refusal naming %s", err, sandbox.BoxGitURLVar)
	}
	if _, found, _ := rig.pending.Get(t.Context(), turn.RunID); found {
		t.Error("a refused launch left a run row")
	}
	if _, err := rig.launch(t, rig.turn(t, "lead", nil)); err != nil {
		t.Errorf("a launch naming no repository was refused: %v", err)
	}
}

// A LAUNCH WITH REPOS NEEDS CODE.NATIVE: a company that hosts no code is told
// so and pointed at the brief; one that does, on a node whose runtime does not
// serve it yet, is told why — a restart owed — rather than that it hosts none.
func TestALaunchWithReposNeedsCodeNative(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	noCode := strings.Split(launchReposDoc, "code:\n  native: true")[0]
	for _, tc := range []struct{ name, doc, says string }{
		{"a company hosting no code", noCode, "this company hosts no repositories (code.native is not set)"},
		{"a node not serving it yet", launchReposDoc, "native_backend_needs_restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newReposRig(t, tc.doc, base)
			l := rig.launcher()
			l.code = builtin.CodeDeps{}
			_, err := l.Launch(t.Context(), rig.turn(t, "swe", nil), builtin.SandboxRequest{Brief: "fix it",
				Repos: []builtin.RepoRequest{{Repo: "web"}}})
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Launch answered %v, want a refusal saying %q", err, tc.says)
			}
		})
	}
}

// startedEnv is the environment rig's n-th started job ran with.
func (r *reposRig) startedEnv(t *testing.T, n int) map[string]string {
	t.Helper()
	started := r.jobs[codingagent.ClaudeCodeName].Started()
	if len(started) <= n {
		t.Fatalf("%d jobs started, want at least %d", len(started), n+1)
	}
	return started[n].Env
}

// A BOX IS CONFIGURED BY ENVIRONMENT: a launch with one repository hands the
// box its run's credential — a token the code host verifies for exactly this
// launch — the code host's base, and git's configuration through the
// environment: exactly the engine's seven entries, in order, the helper reset
// first, the seat's identity as an engine-built commit names it (a role with
// no email on the identity domain), protocol v2, and the index settings a wide
// checkout's status was measured faster with (index version 4, the untracked
// cache); and the commit hook's three values for THIS launch — a second
// launch in the turn carries the second launch's id. The variables beyond
// git's own are exactly the ones config reserves, but the check's own, which
// only a check's box is told.
//
// Mutations: drop entry 1 (the helper reset) → the exact-entries assertion,
// red; drop entry 7 (the untracked cache) → the same, red; take the launch id
// from the row's first launch → the relaunch row, red.
func TestABoxIsConfiguredByEnvironment(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	item := &types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42"}
	for _, tc := range []struct{ seat, name, email string }{
		{"swe", "SWE", "swe@acme.example.com"},
		{"lead", "Lead", "lead@seats.invalid"},
	} {
		t.Run(tc.seat, func(t *testing.T) {
			t.Parallel()
			rig := newReposRig(t, launchReposDoc, base)
			rig.host.add(project("web", "Engineering", code.VisibilityCompany),
				map[string]string{"refs/heads/main": mainHead})
			turn := rig.turn(t, tc.seat, item)
			row, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			env := rig.startedEnv(t, 0)
			ref, ok := rig.tokens.Verify(env[sandbox.GitTokenVar])
			if !ok || ref != (host.BoxRunRef{TurnID: turn.RunID, LaunchID: row.LaunchID}) {
				t.Errorf("the box's token verifies %v (%v), want this launch's %s~%s", ref, ok, row.LaunchID,
					turn.RunID)
			}
			if env[sandbox.GitURLVar] != base {
				t.Errorf("%s=%q, want %q", sandbox.GitURLVar, env[sandbox.GitURLVar], base)
			}
			helper := `!f() { test "$1" = get || return 0; printf 'username=run\npassword=%s\n' "$CREWLET_GIT_TOKEN"; }; f`
			want := [][2]string{
				{"credential." + base + "/.helper", ""},
				{"credential." + base + "/.helper", helper},
				{"user.name", tc.name},
				{"user.email", tc.email},
				{"protocol.version", "2"},
				{"index.version", "4"},
				{"core.untrackedCache", "true"},
			}
			if env["GIT_CONFIG_COUNT"] != strconv.Itoa(len(want)) {
				t.Errorf("GIT_CONFIG_COUNT=%q, want %d", env["GIT_CONFIG_COUNT"], len(want))
			}
			for i, kv := range want {
				key, value := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
				if key != kv[0] || value != kv[1] {
					t.Errorf("entry %d is %q=%q, want %q=%q", i, key, value, kv[0], kv[1])
				}
			}
			trailers, err := sandbox.TrailerEnv(tc.seat, row.LaunchID, item)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range trailers {
				if env[name] != value {
					t.Errorf("%s=%q, want this launch's %q", name, env[name], value)
				}
			}

			// A SECOND LAUNCH IN THE TURN names itself.
			again, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"})
			if err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			second := rig.startedEnv(t, 1)
			if again.LaunchID == row.LaunchID || second[sandbox.TrailerRunVar] != again.LaunchID {
				t.Errorf("the relaunch's run trailer is %q, want its own launch %s (the first was %s)",
					second[sandbox.TrailerRunVar], again.LaunchID, row.LaunchID)
			}
			if ref, ok := rig.tokens.Verify(second[sandbox.GitTokenVar]); !ok || ref.LaunchID != again.LaunchID {
				t.Errorf("the relaunch's token names %v, want launch %s", ref, again.LaunchID)
			}
		})
	}

	// ONE LIST: what the engine sets beyond git's own variables is exactly
	// what configuration reserves — but the check's own, which a check's box
	// is told and a coding run's is not — so no operator's value can stand
	// beside it, and configuration reserves no name nothing sets.
	rig := newReposRig(t, launchReposDoc, base)
	env, err := rig.e.boxGitEnv(rig.c.Org.AgentSeatByHandle("swe"), base,
		host.BoxRunRef{TurnID: statelog.NewOpID(time.Now(), ""), LaunchID: uuid.NewString()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var set []string
	for name := range env {
		if !strings.HasPrefix(name, "GIT_CONFIG_") {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	coding := slices.DeleteFunc(slices.Sorted(slices.Values(config.ReservedSandboxEnv())), func(name string) bool {
		return slices.Contains(config.ReservedCheckEnv(), name)
	})
	if !slices.Equal(set, coding) {
		t.Errorf("a launch sets %v beyond git's own variables, and configuration reserves %v for a coding run",
			set, coding)
	}
}

// THE OPERATOR'S GIT CONFIGURATION IS KEPT: a seat whose sandbox env hands git
// a proxy through GIT_CONFIG_COUNT keeps it at entry 0, the engine's seven
// appended after it and the count eight; a count naming an entry the env does
// not set is refused, naming it, since git refuses a configuration with a hole
// in it and every clone would fail.
//
// Mutation: overwrite the operator's entries rather than append → the proxy
// gone, red.
func TestTheOperatorsGitConfigIsKept(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	withEnv := func(env string) string {
		return strings.Replace(launchReposDoc, "        email: swe@acme.example.com\n        llm: zulu\n"+
			"        code: {access: full_access}\n        sandbox: {enabled: true}\n",
			"        email: swe@acme.example.com\n        llm: zulu\n        code: {access: full_access}\n"+
				"        sandbox:\n          enabled: true\n          env: "+env+"\n", 1)
	}
	rig := newReposRig(t, withEnv(`{GIT_CONFIG_COUNT: "1", GIT_CONFIG_KEY_0: http.proxy, `+
		`GIT_CONFIG_VALUE_0: "http://proxy.example.com:3128"}`), base)
	rig.host.add(project("web", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	if _, err := rig.launch(t, rig.turn(t, "swe", nil), builtin.RepoRequest{Repo: "web"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	env := rig.startedEnv(t, 0)
	if env["GIT_CONFIG_COUNT"] != "8" || env["GIT_CONFIG_KEY_0"] != "http.proxy" ||
		env["GIT_CONFIG_VALUE_0"] != "http://proxy.example.com:3128" ||
		env["GIT_CONFIG_KEY_1"] != "credential."+base+"/.helper" || env["GIT_CONFIG_KEY_5"] != "protocol.version" ||
		env["GIT_CONFIG_KEY_7"] != "core.untrackedCache" {
		t.Errorf("the box's git configuration is %v, want the proxy at 0 and the engine's seven at 1..7",
			gitConfigOf(env))
	}

	holed := newReposRig(t, withEnv(`{GIT_CONFIG_COUNT: "2", GIT_CONFIG_KEY_0: http.proxy, `+
		`GIT_CONFIG_VALUE_0: "http://proxy.example.com:3128"}`), base)
	holed.host.add(project("web", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	if _, err := holed.launch(t, holed.turn(t, "swe", nil), builtin.RepoRequest{Repo: "web"}); err == nil ||
		!strings.Contains(err.Error(), "GIT_CONFIG_COUNT=2 but no GIT_CONFIG_KEY_1") ||
		!strings.Contains(err.Error(), "role.sandbox.env") {
		t.Errorf("a count past the entries the env sets answered %v, want it refused naming the hole", err)
	}
	if n := len(holed.jobs[codingagent.ClaudeCodeName].Started()); n != 0 {
		t.Errorf("%d jobs started on a refused configuration", n)
	}
}

// gitConfigOf is env's git configuration entries, for a failure's message.
func gitConfigOf(env map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range env {
		if strings.HasPrefix(name, "GIT_CONFIG_") {
			out[name] = value
		}
	}
	return out
}

// A RUN WITHOUT REPOS GETS NO GIT CONFIG: its environment holds none of the
// engine's git variables, and its box runs none of the engine's clone steps —
// the M2 environment exactly.
func TestARunWithoutReposGetsNoGitConfig(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	res, err := rig.launcher().Launch(t.Context(), rig.turn(t, "swe", nil),
		builtin.SandboxRequest{Brief: "fix the failing test"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	env := rig.startedEnv(t, 0)
	for name := range env {
		if strings.HasPrefix(name, "GIT_CONFIG_") || slices.Contains(config.ReservedSandboxEnv(), name) {
			t.Errorf("a run given no repository carries %s", name)
		}
	}
	for _, exec := range rig.provider.Box(res.SandboxID).Execs() {
		if strings.Contains(exec.Command, "git clone") || strings.Contains(exec.Command, "git version") {
			t.Errorf("a run given no repository ran %q", exec.Command)
		}
	}
}

// A LAUNCH HOLDS EVERY SCRATCH REPOSITORY IT GRANTS (M3-decisions M3-O61): a
// native executor's run_sandbox call names a scratch repository it never
// wrote and a project repository, and suspends: the row's holds name the
// scratch repository and not the project's, and the code host's expiry reads
// it held. The resumed executor's second call names the project repository
// only — the row's grants are replaced, its holds are not — and the scratch
// repository is still held. The run ends and its turn returns: nothing holds
// it any more. And a call naming a scratch repository the seat may not read
// is refused with the grant's sentence, holding nothing of it.
//
// Mutations: resolve without HoldRepo → the scratch repository unheld after
// the first suspension, red; BeginLaunch replacing CodeHolds like Repos →
// unheld after the second, red; the hold taken before the grant → the refused
// repository in the turn's set, red.
func TestALaunchHoldsEveryScratchRepositoryItGrants(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	swe := rig.c.Org.AgentSeatByHandle("swe")
	lab := scratchOf(swe, "lab")
	web := project("web", "Engineering", code.VisibilityCompany)
	rig.host.add(lab, map[string]string{})
	rig.host.add(web, map[string]string{"refs/heads/main": mainHead})
	secret := scratchOf(rig.c.Org.AgentSeatByHandle("designer"), "secret")
	secret.Visibility = code.VisibilityUnit
	rig.host.add(secret, map[string]string{})

	turns := &runningTurns{}
	turn := rig.turn(t, "swe", nil)
	frame := turns.running(turn.Holds)
	if _, err := rig.launch(t, turn, builtin.RepoRequest{Repo: lab.Name, Access: "read"},
		builtin.RepoRequest{Repo: web.Name}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	frame() // the turn suspends, and its frame returns
	row, _, err := rig.pending.Get(t.Context(), turn.RunID)
	if err != nil || !slices.Contains(row.CodeHolds, lab.ID) || slices.Contains(row.CodeHolds, web.ID) {
		t.Fatalf("the run's row holds %v (%v), want the scratch repository %s and not the project %s",
			row.CodeHolds, err, lab.ID, web.ID)
	}
	if held := liveHolds(t, rig.pending, turns, lab.ID); !held[lab.ID] {
		t.Fatalf("the parked run's scratch repository %s reads unheld", lab.ID)
	}

	resumed := &turnctx.Turn{RunID: turn.RunID, Seat: swe, Org: rig.c.Org, Lookups: turnctx.NewLookups(),
		Holds: rig.e.rowHolds(row)}
	done := turns.running(resumed.Holds)
	again, err := rig.launch(t, resumed, builtin.RepoRequest{Repo: web.Name})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	done()
	if len(again.Repos) != 1 || again.Repos[0].RepoID != web.ID || !slices.Contains(again.CodeHolds, lab.ID) {
		t.Fatalf("the relaunch's row grants %+v and holds %v, want web alone and the scratch repository "+
			"still held", again.Repos, again.CodeHolds)
	}
	if held := liveHolds(t, rig.pending, turns, lab.ID); !held[lab.ID] {
		t.Errorf("after the relaunch the scratch repository %s reads unheld", lab.ID)
	}

	ending, decided, err := rig.pending.DecideEnding(t.Context(), turn.RunID, sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Active, Launch: sandbox.EveryLaunch}})
	if err != nil || !decided || ending.Ending == nil {
		t.Fatalf("decide the run's ending: %v, %v", decided, err)
	}
	if _, finished, err := rig.pending.Finish(t.Context(), turn.RunID, ending.Ending.ID); err != nil || !finished {
		t.Fatalf("finish the run: %v, %v", finished, err)
	}
	if held := liveHolds(t, rig.pending, turns, lab.ID); held[lab.ID] {
		t.Errorf("after the run ended the scratch repository %s reads held", lab.ID)
	}

	refusedTurn := rig.turn(t, "swe", nil)
	if _, err := rig.launch(t, refusedTurn, builtin.RepoRequest{Repo: secret.Name}); err == nil ||
		!strings.Contains(err.Error(), "holds no access to repository") {
		t.Errorf("a scratch repository the seat may not read answered %v, want the grant's refusal", err)
	}
	if slices.Contains(refusedTurn.Holds.IDs(), secret.ID) {
		t.Errorf("a refused launch holds %s, a scratch repository the seat may not read", secret.ID)
	}
}

// EVERY REF A LAUNCH RESOLVES IS HELD ON ITS ROW (M3-decisions M3-O61,
// M3-O64): a native executor on ENG-42 launches on web, whose
// seats/swe/ENG-42 an earlier turn pushed, and parks; answered, the resumed
// executor relaunches on api alone — the row's grants are replaced and no
// longer name web — and the row still holds web's branch, beside api's, which
// does not exist yet and is held with no read; the default branch each launch
// resolved, main, is no seat's and holds nothing. The code host's reader
// answers both branches held from the row, with no turn running.
//
// Mutation: resolveRunRepos skipping HoldRef → web's key is absent after the
// relaunch, red.
func TestEveryRefALaunchResolvesIsHeldOnItsRow(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	web := project("web", "Engineering", code.VisibilityCompany)
	api := project("api", "Engineering", code.VisibilityCompany)
	const branch = "refs/heads/seats/swe/ENG-42"
	rig.host.add(web, map[string]string{"refs/heads/main": mainHead, branch: seatHead})
	rig.host.add(api, map[string]string{"refs/heads/main": mainHead})
	item := &types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42"}

	turn := rig.turn(t, "swe", item)
	first, err := rig.launch(t, turn, builtin.RepoRequest{Repo: web.Name})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	resumed := &turnctx.Turn{RunID: turn.RunID, Seat: turn.Seat, Org: rig.c.Org, Lookups: turnctx.NewLookups(),
		Holds: rig.e.rowHolds(first), WorkItem: item}
	again, err := rig.launch(t, resumed, builtin.RepoRequest{Repo: api.Name})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	webKey, apiKey := code.RefHoldKey(web.ID, branch), code.RefHoldKey(api.ID, branch)
	if len(again.Repos) != 1 || again.Repos[0].RepoID != api.ID {
		t.Fatalf("the relaunch's row grants %+v, want api alone", again.Repos)
	}
	if !slices.Contains(again.CodeHolds, webKey) || !slices.Contains(again.CodeHolds, apiKey) {
		t.Errorf("the relaunch's row holds %v, want web's %s (the first launch's) and api's (the relaunch's)",
			again.CodeHolds, branch)
	}
	for _, repo := range []code.RepoRef{web, api} {
		if slices.Contains(again.CodeHolds, code.RefHoldKey(repo.ID, "refs/heads/main")) {
			t.Errorf("the row holds %s's main, which no seat's branch is", repo.Name)
		}
	}
	if held := liveHolds(t, rig.pending, &runningTurns{}, webKey, apiKey); !held[webKey] || !held[apiKey] {
		t.Errorf("the parked run's branches read %v, want both held from its row", held)
	}
}
