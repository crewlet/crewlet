package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CODING RUN GIVEN REPOSITORIES ON THE COMPANY'S OWN CODE HOST (ADR-0086),
// as the engine resolves each checkout ([Engine.resolveRunRepos]): what
// each repository resolves to, what the seat may be given and the steps its
// box is given them by — over a code host that answers as the estate's
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
      - name: SWE
        handle: swe
        email: swe@acme.example.com
        llm: zulu
        code: {access: full_access}
      - name: Reader
        handle: reader
        llm: zulu
        code: {access: read_only}
  - name: Design
    id: design
    lead: designer
    roles:
      - name: Designer
        handle: designer
        llm: zulu
        code: {access: full_access}
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

// reposRig is an engine whose coding runs' boxes are in a fake cell, serving a
// code host its checkouts resolve repositories through.
type reposRig struct {
	e        *Engine
	c        *Company
	host     *launchHost
	provider *sandbox.FakeProvider
	jobs     map[string]*sandbox.FakeRunner
	tokens   *host.BoxTokens
	pending  sandbox.PendingStore
	// requesters composes whoever woke a turn; nil composes nobody.
	requesters builtin.Requesters
	// placement is the cell every box of the rig runs in.
	placement sandbox.Placement
}

// newReposRig is a rig over doc, its code host at base (CREWLET_BOX_GIT_URL
// as the boot read it, "" for unset), its boxes in the remote cell.
func newReposRig(t *testing.T, doc, base string) *reposRig {
	t.Helper()
	return newReposRigIn(t, doc, base, sandbox.E2B)
}

// newReposRigIn is newReposRig with its catalogue's one cell, where every
// seat's box runs, at placement.
func newReposRigIn(t *testing.T, doc, base string, placement sandbox.Placement) *reposRig {
	t.Helper()
	c := specCompany(t, doc)
	c.roster = rosterOf(c)
	jobs := map[string]*sandbox.FakeRunner{codingagent.ClaudeCodeName: sandbox.NewFakeRunner(codingagent.ClaudeCodeName)}
	provider := sandbox.NewFakeProvider()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{placement: provider},
		DefaultPlacement: placement,
		Runners:          map[string]sandbox.Runner{codingagent.ClaudeCodeName: jobs[codingagent.ClaudeCodeName]},
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
		Pushes: noPushes{}, Answers: testAnswers(),
		Audience: noAudience{}, Queue: q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	e.useSandbox(pending, coordinator)
	e.epoch.current.Store(c)
	return &reposRig{e: e, c: c, host: newLaunchHost(e), provider: provider, jobs: jobs, tokens: tokens,
		pending: pending, placement: placement}
}

// deps are the rig's code tools' seams over its code host, as the epoch's
// equip hands them, composing whoever woke a turn by the rig's requesters
// (nobody, unless a case says).
func (r *reposRig) deps() builtin.CodeDeps {
	requesters := r.requesters
	if requesters == nil {
		requesters = noRequester{}
	}
	return builtin.CodeDeps{Code: r.host, Holder: r.host, Requesters: requesters, Chart: authz.NoChart{}}
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

// launch resolves the repositories a run of turn is asked to be given, in
// the rig's cell, as a launch does before it opens its row.
func (r *reposRig) launch(t *testing.T, turn *turnctx.Turn, repos ...builtin.RepoRequest) (launchRepos, error) {
	t.Helper()
	return r.e.resolveRunRepos(t.Context(), r.deps(), turn, repos, r.placement)
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
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
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
			if len(row.repos) != 1 {
				t.Fatalf("the run is granted %+v, want one repository", row.repos)
			}
			got := row.repos[0]
			if got.Repo != "web" || got.RepoID != web.ID || got.Branch != want || got.Base != tc.wantBase {
				t.Errorf("the run is given %+v, want web (%s) on %s from %q", got, web.ID, want, tc.wantBase)
			}
			if row.floor != codeLogAt(9) {
				t.Errorf("the run's floor is %s, want the furthest of every read and this node's own writes, %s",
					row.floor, codeLogAt(9))
			}
			if tc.item == nil {
				// THE SECOND CALL OF THE TURN works on the same branch.
				again, err := rig.launch(t, turn, tc.req)
				if err != nil || len(again.repos) != 1 || again.repos[0].Branch != want {
					t.Errorf("the turn's second call works on %+v (%v), want %s again", again.repos, err, want)
				}
			}
		})
	}
}

// A CHECKOUT IS HELD TO THE SEAT'S GRANT (ADR-0086): a read_only seat asking
// to write is refused with its grant's sentence and given `read` where it asks
// nothing; a full_access seat outside the owning unit writes its own namespace
// and is refused writing main; a `visibility: unit` repository outside the
// seat's unit is refused with the grant's read refusal; an archived repository
// is given to read and refused to write. Nothing refused leaves a row.
//
// Mutation: skip the access precheck → the read_only seat is given write, red.
func TestACheckoutIsHeldToTheSeatsGrant(t *testing.T) {
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
				if got := row.repos[0].Access; got != tc.access {
					t.Errorf("the run is given %s, want %s", got, tc.access)
				}
				return
			}
			var refused *builtin.RefusedCheckout
			if !errors.As(err, &refused) || !strings.Contains(refused.Result.Output, tc.says) ||
				tools.RefusalOf(refused.Result) != tc.class {
				t.Fatalf("Launch answered %v, want a %s refusal saying %q", err, tc.class, tc.says)
			}
		})
	}
}

// colleagues composes whoever woke a rig's turn: the founder's seat as the
// person holding it, and any other handle as that agent seat itself.
type colleagues struct{}

func (colleagues) Requester(_ context.Context, handle string) (iam.Principal, error) {
	if handle == "founder" {
		return iam.Principal{Kind: iam.KindPerson, Login: "founder@example.com", Seat: handle,
			Stage: iam.StageActive, Grants: iam.Apps}, nil
	}
	return iam.Principal{Kind: iam.KindSeat, Seat: handle, Stage: iam.StageActive, Grants: iam.Apps}, nil
}

// A RUN CHECKS OUT FOR WHOEVER WOKE ITS TURN (M3-decisions U40 (a), the write
// half for a coding run): a checkout in a turn a colleague seat woke names that seat
// on the run's row, which the code host's box admission reads back
// (boxRuns.Run) to hold the box to the colleague's grant beside the seat's;
// `access: write` on a branch the colleague could not push is refused at
// launch, leaving no row, while the seat's own branch is written; a turn a
// person woke names nobody, and pushes what the seat may.
//
// Mutations: the launch request's ceiling dropped — the row names nobody, red;
// boxRuns.Run not copying the row's ceiling — the admission reads none, red.
func TestARunChecksOutForWhoeverWokeItsTurn(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	rig.requesters = colleagues{}
	rig.host.add(project("web", "Engineering", code.VisibilityCompany),
		map[string]string{"refs/heads/main": mainHead})

	// designer, full_access in Design, reads web and pushes only its own
	// namespace there.
	turn := rig.turn(t, "swe", nil)
	turn.Requester = "designer"
	_, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web", Branch: "main", Access: "write"})
	var refused *builtin.RefusedCheckout
	if !errors.As(err, &refused) || tools.RefusalOf(refused.Result) != tools.RefusalForbidden ||
		!strings.Contains(refused.Result.Output, "this work is for the seat whose ask started its turn") {
		t.Fatalf("a write of main for designer answered %v, want a forbidden refusal naming whom the run "+
			"works for", err)
	}
	row, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if row.ceiling != "designer" || len(row.repos) != 1 || row.repos[0].Access != sandbox.RepoWrite {
		t.Errorf("the run is given %+v under ceiling %q, want web to write under designer", row.repos,
			row.ceiling)
	}

	person := rig.turn(t, "swe", nil)
	person.Requester = "founder"
	row, err = rig.launch(t, person, builtin.RepoRequest{Repo: "web", Branch: "main", Access: "write"})
	if err != nil {
		t.Fatalf("Launch for the founder: %v", err)
	}
	if row.ceiling != "" || row.repos[0].Access != sandbox.RepoWrite {
		t.Errorf("a run the founder asked for is given %+v under ceiling %q, want main to write under none",
			row.repos, row.ceiling)
	}
}

// A CHECKOUT NEEDS THE BOX URL: a node with no CREWLET_BOX_GIT_URL refuses a
// checkout, naming the variable, before anything
// is read or written — a box would be given a repository and no address to
// clone it from — and launches one naming none.
func TestACheckoutNeedsTheBoxURL(t *testing.T) {
	t.Parallel()
	rig := newReposRig(t, launchReposDoc, "")
	rig.host.add(project("web", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	turn := rig.turn(t, "swe", nil)
	_, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"})
	if err == nil || !strings.Contains(err.Error(), sandbox.BoxGitURLVar+" is not set") {
		t.Fatalf("Launch answered %v, want a refusal naming %s", err, sandbox.BoxGitURLVar)
	}
	if got, err := rig.launch(t, rig.turn(t, "lead", nil)); err != nil || len(got.repos) != 0 || len(got.clone) != 0 {
		t.Errorf("a launch naming no repository answered %+v, %v; want nothing given and no refusal", got, err)
	}
}

// A CHECKOUT NEEDS CODE.NATIVE: a company that hosts no code is told so and
// pointed at the other host's credentials; one that does, on a node whose runtime does not
// serve it yet, is told why — a restart owed — rather than that it hosts none.
func TestACheckoutNeedsCodeNative(t *testing.T) {
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
			_, err := rig.e.resolveRunRepos(t.Context(), builtin.CodeDeps{}, rig.turn(t, "swe", nil),
				[]builtin.RepoRequest{{Repo: "web"}}, rig.placement)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Launch answered %v, want a refusal saying %q", err, tc.says)
			}
		})
	}
}

// THE CELL DECIDES GIT'S MONITOR: a launch given two repositories runs, after
// their clones, the step that turns git's file-system monitor on across both
// (sandbox.MonitorStep) in a remote box, which owns its kernel's per-user
// inotify budget — and never in a direct or container box on the engine
// host, whose every box spends the engine user's, beside the other seats'
// coding CLIs.
//
// Mutation: CodingSteps appending the step in every cell → the direct and
// container rows, red; appending it in none → the remote row, red.
func TestOnlyABoxWithAKernelOfItsOwnIsGivenGitsMonitor(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	for _, tc := range []struct {
		placement sandbox.Placement
		monitor   bool
	}{
		{sandbox.E2B, true},
		{sandbox.Direct, false},
		{sandbox.Container, false},
	} {
		t.Run(string(tc.placement), func(t *testing.T) {
			t.Parallel()
			rig := newReposRigIn(t, launchReposDoc, base, tc.placement)
			rig.host.add(project("web", "Engineering", code.VisibilityCompany),
				map[string]string{"refs/heads/main": mainHead})
			rig.host.add(project("api", "Engineering", code.VisibilityCompany),
				map[string]string{"refs/heads/main": mainHead})
			turn := rig.turn(t, "swe", nil)
			row, err := rig.launch(t, turn, builtin.RepoRequest{Repo: "web"}, builtin.RepoRequest{Repo: "api"})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			monitor := sandbox.MonitorStep(row.repos).Commands[0]
			var ran []string
			for _, step := range row.clone {
				ran = append(ran, step.Commands...)
			}
			at := slices.Index(ran, monitor)
			if (at >= 0) != tc.monitor {
				t.Fatalf("the %s box is given the monitor's step: %v, want %v; it is given\n%q", tc.placement, at >= 0,
					tc.monitor, ran)
			}
			if !tc.monitor {
				return
			}
			for _, r := range row.repos {
				clone, err := sandbox.CloneRecipe(r, sandbox.CloneCoding)
				if err != nil {
					t.Fatal(err)
				}
				if i := slices.Index(ran, clone.Commands[0]); i < 0 || i > at {
					t.Errorf("the clone of %s ran at %d, want it before the monitor's step at %d", r.Repo, i, at)
				}
			}
		})
	}
}

// A BOX REACHES ONLY WHAT ITS RUN CHECKED OUT (ADR-0086): a job opens with no
// grant, so the code host's box admission, which reads the row on every
// request (boxRuns.Run), lets its box reach nothing; each code_checkout adds
// exactly the repository it named, with its access and its ask to open a
// change at the collection, and raises the floor; the same checkout again is
// the grant the row holds, cloned again, while another branch of it is
// refused; a round carried on keeps the job's checkouts and a fresh launch
// keeps none; and a job that is over is given nothing.
//
// Mutations: GrantRepo appending without the launch check → the ended job's
// checkout is granted, red; carriedGrants answering the row's grants to every
// launch → the fresh launch carries web, red.
func TestABoxReachesOnlyWhatItCheckedOut(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	rig.host.add(project("web", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	rig.host.add(project("api", "Engineering", code.VisibilityCompany), map[string]string{"refs/heads/main": mainHead})
	turn := rig.turn(t, "swe", nil)
	row, err := rig.pending.BeginLaunch(ctx, sandbox.PendingRun{TurnID: turn.RunID, AgentHandle: "swe",
		AgentID: "a-1", Role: "SWE", CodingAgent: codingagent.ClaudeCodeName, Placement: string(rig.placement),
		Reply: "tool", CreatedAt: time.Now().UTC()}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("begin the launch: %v", err)
	}
	box := boxRuns{pending: rig.pending, log: slog.Default()}
	granted := func() []string {
		t.Helper()
		run, found, err := box.Run(ctx, turn.RunID)
		if err != nil || !found {
			t.Fatalf("the box's run reads %v, %v", found, err)
		}
		var names []string
		for _, g := range run.Repos {
			access := "read"
			if g.Access >= code.AccessWrite {
				access = "write"
			}
			names = append(names, g.Name+":"+access)
		}
		return names
	}
	if got := granted(); len(got) != 0 {
		t.Fatalf("a job no checkout was made in reaches %v, want nothing", got)
	}

	desk := runCheckouts{engine: rig.e, deps: rig.deps}
	got, err := desk.Checkout(ctx, turn, row.LaunchID, builtin.RepoRequest{Repo: "web",
		OpenChange: &builtin.OpenChangeRequest{}})
	if err != nil || got.Outcome != sandbox.GrantAdded || got.Repo.Open == nil {
		t.Fatalf("the checkout of web answered %+v, %v; want it added with its change to open", got, err)
	}
	clone, err := sandbox.CloneRecipe(got.Repo, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got.Steps, func(s sandbox.SetupStep) bool {
		return slices.Equal(s.Commands, clone.Commands)
	}) {
		t.Errorf("the checkout's steps %+v do not clone web", got.Steps)
	}
	if _, err := desk.Checkout(ctx, turn, row.LaunchID, builtin.RepoRequest{Repo: "api", Access: "read"}); err != nil {
		t.Fatalf("the checkout of api: %v", err)
	}
	if names := granted(); !slices.Equal(names, []string{"web:write", "api:read"}) {
		t.Errorf("the box reaches %v, want web to write and api to read", names)
	}
	stored, _, err := rig.pending.Get(ctx, turn.RunID)
	if err != nil || stored.CodeFloor == "" || stored.CodeLaunch == "" || stored.Repos[0].Open == nil {
		t.Errorf("the row holds floor %q, launch floor %q and %+v (%v), want both floors and web's change to "+
			"open at the collection", stored.CodeFloor, stored.CodeLaunch, stored.Repos, err)
	}

	again, err := desk.Checkout(ctx, turn, row.LaunchID, builtin.RepoRequest{Repo: "web",
		OpenChange: &builtin.OpenChangeRequest{}})
	if err != nil || again.Outcome != sandbox.GrantHeld || len(again.Steps) == 0 {
		t.Errorf("web checked out again answered %+v, %v; want the grant it holds, cloned again", again, err)
	}
	other, err := desk.Checkout(ctx, turn, row.LaunchID, builtin.RepoRequest{Repo: "web", Branch: "seats/swe/other"})
	if err != nil || other.Outcome != sandbox.GrantConflict {
		t.Errorf("web on another branch answered %+v, %v; want a conflict", other, err)
	}

	if carried, err := carriedGrants(ctx, rig.pending, turn.RunID, true); err != nil || len(carried.Repos) != 2 ||
		carried.CodeFloor != stored.CodeFloor {
		t.Errorf("a round carried on is launched with %+v (%v), want the job's two checkouts at its floor",
			carried, err)
	}
	if fresh, err := carriedGrants(ctx, rig.pending, turn.RunID, false); err != nil || len(fresh.Repos) != 0 {
		t.Errorf("a fresh launch is given %+v (%v), want none", fresh, err)
	}

	ended, err := desk.Checkout(ctx, turn, "another-launch", builtin.RepoRequest{Repo: "web"})
	if err != nil || ended.Outcome != sandbox.GrantNotRunning {
		t.Errorf("a checkout of a job that is over answered %+v, %v; want nothing given", ended, err)
	}
}

// A CHECKOUT HOLDS WHAT IT RESOLVES ON ITS JOB'S ROW (M3-decisions M3-O61,
// M3-O64): a coding run on ENG-42 checks out a scratch repository of its seat
// it never wrote, and web, whose seats/swe/ENG-42 an earlier turn pushed. The
// row the launch attached the turn's holds to holds the scratch repository
// and web's branch, and neither a project repository nor a default branch,
// which no seat's branch is: the code host's expiry and its branch sweep read
// both held for as long as the row lives, with no turn running. A checkout of
// a scratch repository the seat may not read is refused with the grant's
// sentence and holds nothing of it.
//
// Mutations: resolve without HoldRepo → the scratch repository is absent from
// the row, red; resolveRunRepos skipping HoldRef → web's branch key absent,
// red; the hold taken before the grant → the refused repository on the row,
// red.
func TestACheckoutHoldsWhatItResolvesOnItsJobsRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newReposRig(t, launchReposDoc, "https://git.example.com/box/git")
	swe := rig.c.Org.AgentSeatByHandle("swe")
	lab := scratchOf(swe, "lab")
	web := project("web", "Engineering", code.VisibilityCompany)
	const branch = "refs/heads/seats/swe/ENG-42"
	rig.host.add(lab, map[string]string{})
	rig.host.add(web, map[string]string{"refs/heads/main": mainHead, branch: seatHead})
	secret := scratchOf(rig.c.Org.AgentSeatByHandle("designer"), "secret")
	secret.Visibility = code.VisibilityUnit
	rig.host.add(secret, map[string]string{})

	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
	turn := rig.turn(t, "swe", item)
	row, err := rig.pending.BeginLaunch(ctx, sandbox.PendingRun{TurnID: turn.RunID, AgentHandle: "swe",
		AgentID: "a-1", Role: "SWE", CodingAgent: codingagent.ClaudeCodeName, Placement: string(rig.placement),
		Reply: "tool", CreatedAt: time.Now().UTC()}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("begin the launch: %v", err)
	}
	// THE LAUNCH'S ATTACHMENT: from the row's opening on, a hold the turn
	// takes is written to the row before it is held.
	turn.Holds.Attach(holdWriter(rig.pending, turn.RunID))

	desk := runCheckouts{engine: rig.e, deps: rig.deps}
	for _, req := range []builtin.RepoRequest{{Repo: lab.Name, Access: "read"}, {Repo: web.Name}} {
		if got, err := desk.Checkout(ctx, turn, row.LaunchID, req); err != nil || got.Outcome != sandbox.GrantAdded {
			t.Fatalf("the checkout of %s answered %+v, %v; want it added", req.Repo, got, err)
		}
	}
	stored, _, err := rig.pending.Get(ctx, turn.RunID)
	if err != nil {
		t.Fatal(err)
	}
	webKey := code.RefHoldKey(web.ID, branch)
	switch {
	case !slices.Contains(stored.CodeHolds, lab.ID):
		t.Errorf("the row holds %v, want the scratch repository %s", stored.CodeHolds, lab.ID)
	case !slices.Contains(stored.CodeHolds, webKey):
		t.Errorf("the row holds %v, want web's %s", stored.CodeHolds, branch)
	case slices.Contains(stored.CodeHolds, web.ID):
		t.Errorf("the row holds the project repository %s itself, which nothing expires", web.ID)
	case slices.Contains(stored.CodeHolds, code.RefHoldKey(web.ID, "refs/heads/main")):
		t.Errorf("the row holds web's main, which no seat's branch is")
	}

	if _, err := desk.Checkout(ctx, turn, row.LaunchID, builtin.RepoRequest{Repo: secret.Name}); err == nil ||
		!strings.Contains(err.Error(), "holds no access to repository") {
		t.Errorf("a scratch repository the seat may not read answered %v, want the grant's refusal", err)
	}
	if again, _, _ := rig.pending.Get(ctx, turn.RunID); slices.Contains(again.CodeHolds, secret.ID) {
		t.Errorf("a refused checkout holds %s, a scratch repository the seat may not read", secret.ID)
	}
}

// A LAUNCH IS GIVEN ITS BOX'S GIT EXACTLY WHERE A CHECKOUT CAN GRANT IT A
// REPOSITORY (ADR-0086): on a node that serves the company's code host and has
// a box base, every launch of an executor gets its run's git environment —
// its credential, the base its clones dial and git's configuration — and on a
// node serving no code host, or one with no base, none, since nothing there
// could check a repository out to it. Asked of booted nodes, because "serves"
// is the running runtime's answer ([OperatorCode]) and no field a rig sets.
//
// Mutations: gate the launch on Engine.CodeUnserved() == nil, which answers
// no node that serves code → "serving code" given no git, red (what left the
// coding run of internal/e2e's A1 case cloning from an empty base); drop the
// served check → "hosting none" given git, red.
func TestALaunchIsGivenGitWhereTheCodeHostIsServed(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	withBase := config.MapSource{sandbox.BoxGitURLVar: base}
	for _, tc := range []struct {
		name    string
		company string
		env     config.Source
		given   bool
	}{
		{"serving code", codeHostCompany, withBase, true},
		{"hosting none", nativeCleanupCompany, withBase, false},
		{"no box base", codeHostCompany, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newSandboxNodeIn(t, parseCompany(t, tc.company), tc.env)
			seat := e.Company().Org.Role("ceo")
			if seat == nil {
				t.Fatal("the premise: the company has the seat ceo")
			}
			mint := e.launchBoxGit(seat, uuid.NewString(), nil, nil)
			if !tc.given {
				if mint != nil {
					t.Fatal("a launch on a node that can check nothing out was given the box's git")
				}
				return
			}
			if mint == nil {
				t.Fatal("a launch on a node serving the code host was given no git: every checkout of " +
					"its run would clone from an empty base")
			}
			env, err := mint(uuid.NewString())
			if err != nil {
				t.Fatalf("the launch's git environment: %v", err)
			}
			if env[sandbox.GitURLVar] != base || env[sandbox.GitTokenVar] == "" || env["GIT_CONFIG_COUNT"] == "" {
				t.Errorf("the launch's git environment names base %q, a token of %d bytes and %q "+
					"configuration entries; want %q, a token and the engine's entries",
					env[sandbox.GitURLVar], len(env[sandbox.GitTokenVar]), env["GIT_CONFIG_COUNT"], base)
			}
		})
	}
}

// A BOX IS CONFIGURED BY ENVIRONMENT (ADR-0086), the one every executor launch
// on a node serving the code host is given ([Engine.boxGitEnv], its
// [sandbox.LaunchRequest.RunEnv]): its run's credential — a token the code host
// verifies for exactly this launch — the code host's base, and git's
// configuration through the environment: exactly the engine's seven entries,
// in order, the helper reset first, the seat's identity as an engine-built
// commit names it (a role with no email on the identity domain), protocol v2,
// and the index settings a wide checkout's status was measured faster with;
// and the commit hook's three values for THIS launch — a second launch carries
// its own id. The variables beyond git's own are exactly the ones config
// reserves, but the check's own, which only a check's box is told.
//
// Mutations: drop entry 1 (the helper reset) → the exact-entries assertion,
// red; mint the token for the turn's first launch → the second launch, red.
func TestABoxIsConfiguredByEnvironment(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	item := &types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
	for _, tc := range []struct{ seat, name, email string }{
		{"swe", "SWE", "swe@acme.example.com"},
		{"lead", "Lead", "lead@seats.invalid"},
	} {
		t.Run(tc.seat, func(t *testing.T) {
			t.Parallel()
			rig := newReposRig(t, launchReposDoc, base)
			turn := rig.turn(t, tc.seat, item)
			for _, launch := range []string{uuid.NewString(), uuid.NewString()} {
				ref := host.BoxRunRef{TurnID: turn.RunID, LaunchID: launch}
				env, err := rig.e.boxGitEnv(turn.Seat, base, ref, item, nil)
				if err != nil {
					t.Fatalf("boxGitEnv: %v", err)
				}
				if got, ok := rig.tokens.Verify(env[sandbox.GitTokenVar]); !ok || got != ref {
					t.Errorf("the box's token verifies %v (%v), want this launch's %v", got, ok, ref)
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
				trailers, err := sandbox.TrailerEnv(tc.seat, launch, item)
				if err != nil {
					t.Fatal(err)
				}
				for name, value := range trailers {
					if env[name] != value {
						t.Errorf("%s=%q, want this launch's %q", name, env[name], value)
					}
				}
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

// THE OPERATOR'S GIT CONFIGURATION IS KEPT: a run environment handing git a
// proxy through GIT_CONFIG_COUNT keeps it at entry 0, the engine's seven
// appended after it and the count eight; a count naming an entry the env does
// not set is refused, naming it, since git refuses a configuration with a hole
// in it and every clone would fail.
//
// Mutation: overwrite the operator's entries rather than append → the proxy
// gone, red.
func TestTheOperatorsGitConfigIsKept(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	rig := newReposRig(t, launchReposDoc, base)
	seat := rig.c.Org.AgentSeatByHandle("swe")
	ref := host.BoxRunRef{TurnID: statelog.NewOpID(time.Now(), ""), LaunchID: uuid.NewString()}
	env, err := rig.e.boxGitEnv(seat, base, ref, nil, map[string]string{"GIT_CONFIG_COUNT": "1",
		"GIT_CONFIG_KEY_0": "http.proxy", "GIT_CONFIG_VALUE_0": "http://proxy.example.com:3128"})
	if err != nil {
		t.Fatalf("boxGitEnv: %v", err)
	}
	// THE ENGINE'S ENTRIES ARE ADDED OVER THE RUN ENVIRONMENT, which keeps the
	// operator's: the additions name no entry 0, so the proxy stays at 0 and
	// the count covers it.
	_, overwrote := env["GIT_CONFIG_KEY_0"]
	_, overwroteValue := env["GIT_CONFIG_VALUE_0"]
	if env["GIT_CONFIG_COUNT"] != "8" || overwrote || overwroteValue ||
		env["GIT_CONFIG_KEY_1"] != "credential."+base+"/.helper" || env["GIT_CONFIG_KEY_5"] != "protocol.version" ||
		env["GIT_CONFIG_KEY_7"] != "core.untrackedCache" {
		t.Errorf("the box's git configuration adds %v, want the proxy kept at 0 and the engine's seven at 1..7",
			env)
	}
	_, err = rig.e.boxGitEnv(seat, base, ref, nil, map[string]string{"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "http.proxy", "GIT_CONFIG_VALUE_0": "http://proxy.example.com:3128"})
	if err == nil || !strings.Contains(err.Error(), "GIT_CONFIG_COUNT=2 but no GIT_CONFIG_KEY_1") {
		t.Errorf("a count past the entries the env sets answered %v, want it refused naming the hole", err)
	}
}
