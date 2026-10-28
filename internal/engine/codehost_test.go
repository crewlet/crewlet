package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	codecompact "github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// gitCompany is a company hosting one repository, `web`, owned by its founder,
// whose human seat the operator token `ops` is bound to in the identity
// directory ([bindOps]).
const gitCompany = `
name: Nimbus
tracker:
  backend: none
knowledge:
  backend: none
code:
  native: true
  repos:
    - name: web
      owner: {seat: founder}
roles:
  - name: SWE
    handle: swe
  - name: Founder
    handle: founder
    kind: human
`

// noCodeCompany is the same company hosting no code.
const noCodeCompany = `
name: Nimbus
tracker:
  backend: native
knowledge:
  backend: none
roles:
  - name: SWE
    handle: swe
`

// opsToken is the founder's operator token: long enough for the guard.
const opsToken = "ops-token-0123456789abcdefghijklmnopqrstuv"

// bindOps binds the `ops` Tier A token to the founder's human seat through e's
// identity directory — the row `crewlet iam create -kind machine -login
// token:ops -seat founder` writes — and waits until e reports the binding:
// the guard resolves a token's seat from this node's own copy of the
// directory, so a push before the row applied would be a push by a token
// bound to nobody, which fetches only.
func bindOps(t *testing.T, e *engine.Engine) {
	t.Helper()
	key := uuid.Must(uuid.NewV7()).String()
	id, err := iamdomain.CreatedPersonID(key)
	if err != nil {
		t.Fatalf("derive the machine's id: %v", err)
	}
	login := iam.TokenLogin("ops")
	if _, err := e.IAMWriter().Create(t.Context(), iamdomain.Creation{
		PersonID: id, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: login, Seat: "founder",
		OpID:   statelog.NewOpID(time.Now(), "create"),
		Reason: "the founder's git credential",
	}); err != nil {
		t.Fatalf("bind %s to the founder: %v", login, err)
	}
	waitFor(t, "the ops token's binding to the founder", func() bool {
		row, err := e.BoundSeat(t.Context(), login)
		return err == nil && row.Found && row.Seat == "founder"
	})
}

// gitNodeBootstrap is a data node's Tier A with a persisted stream, its roles
// mutated by roles (nil leaves them undeclared: every role), and the founder's
// token. The token states its grants, as every Tier A token must, and lists
// the tracker, which decides nothing a case here reads: what a credential may
// do to a repository is the standing of the seat the directory binds it to
// (ADR-0058).
func gitNodeBootstrap(t *testing.T, roles []string) *config.Bootstrap {
	t.Helper()
	return bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(filepath.Dir(b.Store.Path), "stream")
		b.Node.Roles = roles
		b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: opsToken,
			Grants: []iam.Grant{iam.GrantTracker}}}
	})
}

// newGitNode boots an engine over b for the company doc, its hook this test
// binary, and stops it at the end of the test — on the git the suite runs,
// held to the node floor first where the boot builds a code host
// (engine.RequireNodeGit), so a machine below it reads the contributor's
// sentence rather than the node's refusal.
func newGitNode(t *testing.T, b *config.Bootstrap, doc string) (*engine.Engine, error) {
	t.Helper()
	engine.RequireNodeGit(t, b, parsedCompany(t, doc))
	return bootGitNode(t, b, doc)
}

// bootGitNode is newGitNode on whatever git PATH names, for a case that hands
// the boot a git of its own.
func bootGitNode(t *testing.T, b *config.Bootstrap, doc string) (*engine.Engine, error) {
	t.Helper()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, doc),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	})
	if err == nil {
		t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	}
	return e, err
}

// A CODE HOST IS BUILT EXACTLY WHERE THE COMPANY HOSTS CODE AND THE NODE HAS
// THE GIT ROLE: a company without code.native builds none, a node whose roles
// name no git builds none — and neither makes a cache directory — and a node
// with every role for a company that hosts code builds one, its directory
// tagged as the cache's.
//
// Mutation: build the host regardless of the git role, and the second case
// goes red.
func TestACompanyWithoutNativeCodeBuildsNoHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		roles []string
		doc   string
		hosts bool
	}{
		{"no code hosted, every role", nil, noCodeCompany, false},
		{"code hosted, roles without git", []string{"data", "ingress", "seats", "workers"}, gitCompany, false},
		{"code hosted, every role", nil, gitCompany, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := gitNodeBootstrap(t, tc.roles)
			e, err := newGitNode(t, b, tc.doc)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, hosts := e.CodeHost()
			dir := filepath.Join(filepath.Dir(b.Store.Path), "git")
			_, statErr := os.Stat(filepath.Join(dir, "CACHEDIR.TAG"))
			switch {
			case hosts != tc.hosts:
				t.Fatalf("CodeHost() = %t, want %t", hosts, tc.hosts)
			case tc.hosts && statErr != nil:
				t.Fatalf("the node serves git and its cache directory holds no tag: %v", statErr)
			case !tc.hosts:
				if _, err := os.Stat(dir); err == nil {
					t.Fatalf("a node that serves no git made a cache directory at %s", dir)
				}
			}
		})
	}
}

// A VOLUME WITH NO ROOM FOR A CACHE REFUSES THE GIT NODE BY NAME. Here the
// broker beside the cache may still write every byte the volume has free, so
// the request spools leave the cache nothing to take a share of: the node is
// refused with the engine's own account of the volume — the figures and every
// setting that changes one — never with the cache's refusal of a budget of
// nothing, which names none of them. An explicit git.cache_max_bytes on the
// same volume is the operator's answer, and the node boots on it.
//
// Mutation: derive a budget of nothing without refusing it, and the refusal is
// the cache's, naming no setting.
func TestAVolumeWithNoRoomRefusesTheCodeHostByName(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const gib = int64(1) << 30
	boot := func(b *config.Bootstrap) (*engine.Engine, error) {
		e, err := engine.New(t.Context(), engine.WithCacheDisk(engine.Options{
			Bootstrap: b, Company: parsedCompany(t, gitCompany),
			ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			GitHook:     codetest.HookCommand(t),
		}, 16*gib, 16*gib))
		if err == nil {
			t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
		}
		return e, err
	}

	_, err := boot(gitNodeBootstrap(t, nil))
	if err == nil {
		t.Fatal("a git node booted on a volume whose broker may fill all of it")
	}
	for _, want := range []string{"no room for the code cache", "git.cache_dir",
		"git.cache_max_bytes", "stream.store_max_bytes", "git.max_transfers",
		fmt.Sprintf("its %d transfers'", engine.TestGitTransfers)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	b := gitNodeBootstrap(t, nil)
	b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
	e, err := boot(b)
	if err != nil {
		t.Fatalf("an explicit budget on the same volume was refused: %v", err)
	}
	if _, ok := e.CodeHost(); !ok {
		t.Fatal("a node given an explicit budget built no code host")
	}
}

// A GIT NODE BOOTS ONLY AT ITS FLOOR — git 2.55 or newer, gitproc.NodeMinVersion
// — and the refusal names the role, code.native, the version it needs and the
// git it found: a boot, not a first push, is where an operator learns the
// host's git is too old.
//
// Not parallel: it sets PATH, which every git the process starts reads.
//
// Mutation: check gitproc.ClientMinVersion in gitproc.New, and the 2.54.9 node
// boots.
func TestAGitNodeBootsOnlyAtItsFloor(t *testing.T) {
	old := gittest.Fake(t, "git version 2.54.9", "exit 1")
	t.Setenv("PATH", filepath.Dir(old))
	_, err := bootGitNode(t, gitNodeBootstrap(t, nil), gitCompany)
	if err == nil {
		t.Fatal("a git node booted on git 2.54.9")
	}
	for _, want := range []string{`"git"`, "code.native", gitproc.NodeMinVersion.String(), "2.54.9", old} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	var tooOld *gitproc.ErrTooOld
	if !errors.As(err, &tooOld) || tooOld.Floor != gitproc.FloorNode {
		t.Errorf("the refusal is %v, want a *gitproc.ErrTooOld at the node floor", err)
	}

	fresh := gittest.Fake(t, "git version "+gitproc.NodeMinVersion.String(), "exit 1")
	t.Setenv("PATH", filepath.Dir(fresh))
	e, err := bootGitNode(t, gitNodeBootstrap(t, nil), gitCompany)
	if err != nil {
		t.Fatalf("a git node on git %s did not boot: %v", gitproc.NodeMinVersion, err)
	}
	if _, ok := e.CodeHost(); !ok {
		t.Fatalf("a git node on git %s built no code host", gitproc.NodeMinVersion)
	}
}

// A PERSON PUSHES THROUGH THE ENGINE'S OWN HOST: the founder clones the empty
// repository the company declares, commits and pushes with their own operator
// token as the Basic password, the branch lands, and this node's event store
// holds the push audited as the founder, by repository, through the token it
// came through — which is the host built over the engine's own guard, Audit,
// Estate and Objects.
func TestAPersonPushesThroughTheEnginesHost(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	e, err := newGitNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := serveWeb(t, b, e)

	founder := codetest.PersonWithToken(t, opsToken)
	dir := founder.Clone(url)
	commit := founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	pushed := founder.Push(t.Context(), dir, url, "HEAD:refs/heads/main")
	if !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}
	if got := founder.LsRemote(url, 2)["refs/heads/main"]; got != commit {
		t.Fatalf("main advertises %q, want the pushed %s", got, commit)
	}

	var acted types.OperatorActed
	waitFor(t, "the push's audit record", func() bool {
		rows, err := e.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Type: types.OperatorActed{}.EventType(), Limit: 10})
		if err != nil || len(rows) == 0 {
			return false
		}
		if len(rows) != 1 {
			t.Fatalf("%d audit records for one push", len(rows))
		}
		row, err := e.Backends().Store.Events().ByID(t.Context(), rows[0].ID, time.Now())
		if err != nil {
			return false
		}
		return json.Unmarshal(row.Payload, &acted) == nil
	})
	if acted.Transport != types.TransportGit || acted.Tool != "git_push" ||
		acted.ActorName != "founder" || acted.ActorKind != string(iam.ActorHuman) ||
		acted.Outcome != types.AuditApplied || acted.Repo != "web" ||
		acted.OperatorID != iam.TokenLogin("ops") {
		t.Fatalf("the push was audited as %+v, want the founder's applied git_push to web", acted)
	}
}

// A GIT NODE ANSWERS THE FLEET'S CODE STATUS, on the broker, as a peer asks it:
// a question on topics.CodeStatus naming a repository is answered by the node
// serving the code host with its host's own status — the node, its cache, and
// the repository's heat and copy in service, as the person's clone and push
// through the engine's host left them. The wire is the question's and the
// answer's own (version, repo, status). A question at a version this build
// does not speak is answered by nobody, which its asker reads as a node that
// did not answer, never as a guess at its fields.
//
// Mutation: register no answerer beside the host (buildCodeHost), and the
// question is answered by nobody.
func TestAGitNodeAnswersTheFleetsCodeStatus(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	e, err := newGitNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := serveWeb(t, b, e)
	founder := codetest.PersonWithToken(t, opsToken)
	dir := founder.Clone(url)
	founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := founder.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}
	h, _ := e.CodeHost()
	seam, _ := engine.OperatorCode(e)
	repo, err := seam.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepo, Repo: "web",
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("ops"), Seat: "founder"},
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil || repo.Repo == nil {
		t.Fatalf("the repository's detail: %+v, %v", repo.Repo, err)
	}
	id := repo.Repo.ID

	ask := func(question string, within time.Duration) [][]byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), within)
		defer cancel()
		replies, err := e.Backends().Queue.Ask(ctx, topics.CodeStatus, []byte(question), 1)
		if err != nil {
			t.Fatalf("ask %s: %v", question, err)
		}
		return replies
	}
	replies := ask(`{"version":1,"repo":"`+id+`"}`, 10*time.Second)
	if len(replies) != 1 {
		t.Fatalf("a code status question was answered %d times, want once, by the git node", len(replies))
	}
	var answer struct {
		Version int             `json:"version"`
		Status  host.CodeStatus `json:"status"`
	}
	if err := json.Unmarshal(replies[0], &answer); err != nil {
		t.Fatalf("the answer %s: %v", replies[0], err)
	}
	st := answer.Status
	if answer.Version != 1 || st.Node != h.Status("").Node || st.Cache.Warm != 1 || st.Cache.Budget <= 0 {
		t.Fatalf("the answer is version %d from node %q with cache %+v, want version 1 from %q, its one "+
			"warm repository and its budget", answer.Version, st.Node, st.Cache, h.Status("").Node)
	}
	if st.Repo == nil || st.Repo.ID != id || st.Repo.Day.Pushes != 1 || st.Repo.Day.Fetches != 1 ||
		st.Repo.Copy == nil {
		t.Fatalf("the repository asked about is answered %+v, want web's heat — the founder's one clone of it "+
			"empty and one push — and its copy in service", st.Repo)
	}

	if replies := ask(`{"version":2}`, time.Second); len(replies) != 0 {
		t.Fatalf("a question at a version this build does not speak was answered: %s", replies)
	}
}

// A GIT READ IS SERVED ONCE AN APPLY BUILDS THE CODE HOST, with no restart
// (M3-O17). A git node booted with no company has built no code host, so a
// read of a repository by object id through its own router is unserved, the
// node saying it runs none; the first apply that sets code.native builds the
// code host and the git reads beside it, and the next read — of the commit the
// founder pushed through that host — is answered by this node's own git half.
// The router and the estate server ask for that half per request: either one
// built before the runtime existed, and holding what it found then, would
// answer "no code host" until the node restarted.
//
// Mutation: resolve the git half once, when the router is built — the read
// after the apply is still unserved, red.
func TestAGitReadIsServedOnceAnApplyBuildsTheCodeHost(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	b := gitNodeBootstrap(t, nil)
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: b, GitHook: codetest.HookCommand(t)})
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	founder := code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("ops"), Seat: "founder"}
	read := code.GitQuery{Kind: code.GitTree, RepoID: "019a0c2e-0000-7000-8000-0000000000a1", Asker: founder,
		Tree: &code.TreeQuery{Rev: "1111111111111111111111111111111111111111"}}
	_, err = engine.GitReadForTest(t.Context(), e, read)
	var unserved *estate.ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "runs no code host") {
		t.Fatalf("a git read on a git node with no company = %v, want it unserved, the node running no "+
			"code host", err)
	}

	if status, _, err := e.Apply(t.Context(), parsedCompany(t, gitCompany), time.Now()); err != nil ||
		status != configplane.StatusOK {
		t.Fatalf("the first apply, which hosts code = (%s, %v)", status, err)
	}
	url := serveWeb(t, b, e)
	person := codetest.PersonWithToken(t, opsToken)
	dir := person.Clone(url)
	commit := person.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := person.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations after the apply")
	}
	repo, err := seam.Read(t.Context(), code.ReadQuery{Asker: founder, Kind: code.ReadRepo, Repo: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil || repo.Repo == nil {
		t.Fatalf("the repository's detail: %+v, %v", repo.Repo, err)
	}
	read.RepoID, read.Tree.Rev = repo.Repo.ID, commit
	a, err := engine.GitReadForTest(t.Context(), e, read)
	if err != nil || a.Tree == nil || !slices.ContainsFunc(a.Tree.Entries, func(entry code.TreeEntry) bool {
		return entry.Path == "README.md"
	}) {
		t.Fatalf("a git read once the apply built the code host = (%+v, %v), want web's root listing "+
			"README.md, answered by this node", a.Tree, err)
	}
}

// A GIT NODE'S OWN ALARMS NAME A BITMAP ITS CACHE REFUSED FOR MEMORY. The
// cache judges every repository it holds against what the node's memory can
// give its background work, and a bitmap past that is refused — logged,
// counted, gauged — and stands as code_maintenance_refused on the node whose
// cache refused it, since an alarm describes one node. Nothing filled the
// reading's code half from the cache, so the alarm could never fire on a
// running node.
//
// The node here is one whose transfer lane takes all its memory, by
// arithmetic rather than by the machine running the case: its git pool is
// told the node has 8 GiB (engine.WithGitMemory, gitproc.Options.Memory) and
// its operator set git.max_transfers to one transfer more than 8 GiB holds at
// gitproc.TransferMemory each — 37 at 224 MiB, 8 288 MiB kept — so whatever
// the machine's cores add for serving, the background work's budget is
// nothing and the bitmap of every repository the node holds a pack of is
// refused. Its cache budget is explicit so the case is not decided by the
// machine's free disk either: those 37 transfers' request spools take
// 37 × 64 MiB = 2 368 MiB (host.SpoolAllowance) out of the room a derived
// budget is a share of. Before the node holds a pack its alarms are silent —
// the alarm follows the refusal, never the configuration — and once the
// founder's push lands the node's own evaluation, the one its gauge, its log
// and its health envelope are fed, names it within a maintainer pass and an
// evaluation.
//
// Mutation: drop r.code from the reading's assembly (retentionreport.go), or
// fill nothing from the cache in codeReading, and the alarm never fires.
func TestTheGitCachesMaintenanceAlarmFiresOnItsNode(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const memory = int64(8) << 30
	b := gitNodeBootstrap(t, nil)
	b.Git.MaxTransfers = int(memory/gitproc.TransferMemory) + 1
	b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
	if kept := int64(b.Git.MaxTransfers) * gitproc.TransferMemory; kept <= memory {
		t.Fatalf("%d transfers keep %d bytes of a %d-byte node, which leaves its background work a "+
			"budget", b.Git.MaxTransfers, kept, memory)
	}
	e, err := engine.New(t.Context(), engine.WithGitMemory(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	}, memory))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	url := serveWeb(t, b, e)
	waitFor(t, "the node's first alarm evaluation", func() bool {
		_, evaluated := e.Alarms()
		return evaluated
	})
	if kinds, _ := e.Alarms(); slices.Contains(kinds, statelog.KindCodeMaintenanceRefused) {
		t.Fatalf("%s fires on a node holding no pack: %v", statelog.KindCodeMaintenanceRefused, kinds)
	}

	founder := codetest.PersonWithToken(t, opsToken)
	dir := founder.Clone(url)
	founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := founder.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}

	// A MAINTAINER PASS AND AN EVALUATION: the cache judges what it holds on
	// its maintainer's tick, and the node evaluates its table on its own.
	// The minute is room for a loaded machine, never the expected wait.
	deadline := time.Now().Add(cache.MidxDebounce + statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeMaintenanceRefused) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with the bitmap of the repository it holds refused for "+
				"memory, want %s", kinds, statelog.KindCodeMaintenanceRefused)
		}
		time.Sleep(50 * time.Millisecond)
	}
	report, ok := e.RetentionReport(t.Context())
	if !ok {
		t.Fatal("a node evaluating alarms has no retention report")
	}
	var detail string
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindCodeMaintenanceRefused {
			detail = a.Detail
		}
	}
	if !strings.HasPrefix(detail, "1 repository(ies)") {
		t.Errorf("the report's %s says %q, want the one repository the node holds", statelog.KindCodeMaintenanceRefused,
			detail)
	}
}

// atTheBound is a git node's compactor's code seam with every due listing
// answered as naming one more repository, at the most live packs a repository
// may hold — the reading a running node's compactor raises the alarm from,
// shown it without two thousand packs pushed into a repository — and every
// listing counted.
type atTheBound struct {
	codecompact.Estate
	listings *atomic.Int64
}

// boundRepo is the repository atTheBound names.
const boundRepo = "at-the-bound"

func (e atTheBound) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind != code.ReadCompactionDue {
		return e.Estate.Read(ctx, q)
	}
	e.listings.Add(1)
	return code.ReadAnswer{Due: []code.CompactionCandidate{{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(boundRepo)).String(), Name: boundRepo,
		LivePacks: code.MaxPacksPerRepo,
	}}}, nil
}

// THE COMPACTION-BEHIND ALARM FIRES ON ITS NODE, through the wiring a running
// node has: a git node whose compactor's pass finds a repository it is home
// for at the pack bound fills its alarm reading's compaction half, which the
// retention loop's own reading carries into the alarm table — so
// code_compaction_behind stands on that node, naming the repository, for as
// long as its pushes would be refused. And it is the compactor's FIRST pass
// that names it: started only once the runtime it reads through is
// published, that pass finds this node the repository's home.
//
// Mutations: leave the compactor's half out of the reading (codeReading), and
// the alarm never fires; start the compactor where the code host is built,
// before the runtime is published, and its first pass finds this node home to
// nothing — the repository is named a listing later.
func TestTheCompactionBehindAlarmFiresOnItsNode(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, gitCompany))
	var listings atomic.Int64
	e, err := engine.New(t.Context(), engine.WithCompactorEstate(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	}, func(inner codecompact.Estate) codecompact.Estate {
		return atTheBound{Estate: inner, listings: &listings}
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })

	// THE COMPACTOR'S READING, read until it names the repository: its first
	// pass's, at once — the next comes a Poll later, and the deadline leaves
	// room for it only so that a pass too early fails here by its count rather
	// than by the clock.
	deadline := time.Now().Add(codecompact.Poll + time.Minute)
	for engine.CodeReadingForTest(e).CodeCompactionBehind == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the compactor's reading named no repository at the pack bound after %d listings",
				listings.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := listings.Load(); n != 1 {
		t.Errorf("the compactor's reading first named the repository at the bound after %d listings, want its "+
			"first pass's: a pass started before the runtime it reads through was published finds this node "+
			"home to nothing", n)
	}

	// AND THE ALARM, at the retention loop's next evaluation of its reading.
	// The rest is room for a loaded machine.
	deadline = time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeCompactionBehind) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with its compactor finding a repository at the pack bound, want %s",
				kinds, statelog.KindCodeCompactionBehind)
		}
		time.Sleep(50 * time.Millisecond)
	}
	report, ok := e.RetentionReport(t.Context())
	if !ok {
		t.Fatal("a node evaluating alarms has no retention report")
	}
	var detail string
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindCodeCompactionBehind {
			detail = a.Detail
		}
	}
	if !strings.Contains(detail, boundRepo) {
		t.Errorf("the report's %s says %q, want the repository at the bound named", statelog.KindCodeCompactionBehind,
			detail)
	}
}

// serveWeb waits for gitCompany's declared repository to exist on e, binds the
// `ops` token to the founder ([bindOps]), and serves e's own code host behind
// b's guard over e's identity directory, as the API mounts it: the URL a
// person's git clones and pushes `web` at.
func serveWeb(t *testing.T, b *config.Bootstrap, e *engine.Engine) string {
	t.Helper()
	h, ok := e.CodeHost()
	if !ok {
		t.Fatal("the node built no code host")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	waitFor(t, "the declared repository to exist", func() bool {
		answer, err := seam.Read(t.Context(), code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"}, Kind: code.ReadRepos,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			return false
		}
		for _, r := range answer.Repos {
			if r.Name == "web" {
				return true
			}
		}
		return false
	})

	bindOps(t, e)

	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, h.InfoRefs())
	mux.Handle(host.PatternUploadPack, h.UploadPack())
	mux.Handle(host.PatternReceivePack, h.ReceivePack())
	// THE GUARD `crewlet run` BUILDS, over the engine's own directory and
	// seat view, so a token acts as the seat the directory binds it to.
	guard := auth.New(b).BindSeats(auth.SeatBindings{Directory: e, Chart: engine.SeatViewOf(e)})
	srv := httptest.NewServer(guard.Middleware(mux))
	t.Cleanup(srv.Close)
	return srv.URL + "/git/web.git"
}
