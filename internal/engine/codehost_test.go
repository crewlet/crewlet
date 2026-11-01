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
	"sync"
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
	"github.com/crewlet/crewlet/internal/runtoken"
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

// A GIT NODE VERIFIES A BOX'S TOKEN, ITS CODE HOST BUILT OR NOT: the box
// tokens — a coding run's and a check's — are built at New over the fleet's
// keyring, whatever the company runs, so a node whose box routes find no code
// host to serve them — which tells a box why only once its token verifies
// (api.CodeSurface.VerifyBox) — verifies a token another node of the fleet
// minted exactly as a node serving the code host does, and refuses one another
// keyring signed.
//
// Mutation: build the box tokens, or the check tokens, with the code host
// rather than at New — the node serving none holds none, red.
func TestAGitNodeVerifiesABoxsTokenWithOrWithoutItsHost(t *testing.T) {
	t.Parallel()
	ref := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	stranger, err := host.NewBoxTokens(runtoken.OneKey("k", "another fleet's key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	check := host.CheckRunRef{Repo: "web", Attempt: code.CheckKey{RepoID: uuid.NewString(),
		OID: strings.Repeat("ab", 20), Target: "refs/heads/main", Name: "unit", Attempt: 1}}
	strangerChecks, err := host.NewCheckTokens(runtoken.OneKey("k", "another fleet's key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		doc   string
		hosts bool
	}{
		{"no code hosted", noCodeCompany, false},
		{"code hosted", gitCompany, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := gitNodeBootstrap(t, nil)
			e, err := newGitNode(t, b, tc.doc)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, hosts := e.CodeHost(); hosts != tc.hosts {
				t.Fatalf("CodeHost() = %t, want %t", hosts, tc.hosts)
			}
			peer, err := host.NewBoxTokens(b.Secrets.TokenMaterial(), nil)
			if err != nil {
				t.Fatal(err)
			}
			tokens := e.BoxTokens()
			if tokens == nil {
				t.Fatal("the node holds no box tokens")
			}
			if got, signed := tokens.Verify(peer.Mint(ref)); !signed || got != ref {
				t.Errorf("a token the fleet's keyring signed verifies as %+v, %t; want %+v", got, signed, ref)
			}
			if _, signed := tokens.Verify(stranger.Mint(ref)); signed {
				t.Error("a token another keyring signed verifies")
			}
			checks := e.CheckTokens()
			if checks == nil {
				t.Fatal("the node holds no check tokens")
			}
			peerChecks, err := host.NewCheckTokens(b.Secrets.TokenMaterial(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, signed := checks.Verify(peerChecks.Mint(check, time.Minute)); !signed || got != check {
				t.Errorf("a check token the fleet's keyring signed verifies as %+v, %t; want %+v", got, signed,
					check)
			}
			if _, signed := checks.Verify(strangerChecks.Mint(check, time.Minute)); signed {
				t.Error("a check token another keyring signed verifies")
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

// A PUSH'S CHANGE IS LINKED ON THE DEPLOYMENT'S ADDRESS: the founder pushes a
// branch with `-o change` through the engine's own host, and git prints the
// change it opened with the link to its page on `api.external_url` — the
// address the engine took from its Tier A at construction, which the host is
// built with, so a node serving a push never names a change and no page.
//
// Mutation: build the host with no ExternalBase (buildCodeHost) — git prints
// the change's name alone, red.
func TestAPushsChangeIsLinkedOnTheDeploymentsAddress(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	b.API.ExternalURL = "https://crewlet.example.com/"
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
	founder.Commit(dir, map[string]string{"README.md": "# web, retried\n"}, "Retry the upload")
	pushed := founder.PushArgs(t.Context(), dir, []string{"-o", "change"}, url, "HEAD:refs/heads/feature")
	if !pushed.OK("refs/heads/feature") {
		t.Fatalf("the founder's push with -o change did not land:\n%s", pushed)
	}
	const want = "remote: crewlet: change web!1 opened: https://crewlet.example.com/dashboard#/code/repos/web/changes/1"
	if !strings.Contains(pushed.Stderr, want) {
		t.Fatalf("git printed no %q:\n%s", want, pushed)
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

// THE GIT HALF IS SERVED ONCE AN APPLY BUILDS THE CODE HOST, with no restart
// (M3-O17). A git node booted with no company has built no code host, so a
// read of a repository by object id and an engine write through its own router
// are each unserved, the node saying it runs none; the first apply that sets
// code.native builds the code host and the git reads beside it, and the next
// read — of the commit the founder pushed through that host — is answered by
// this node's own git half, and so is the founder's next write, built and
// landed by the code host and read back at the commit it made. The router and
// the estate server ask for that half per request: either one built before
// the runtime existed, and holding what it found then, would answer "no code
// host" until the node restarted.
//
// Mutations: resolve the git half once, when the router is built — the read
// after the apply is still unserved, red; answer the half's writes with
// nothing built (a stub where the code host's Write is) — the write after the
// apply is not applied, red.
func TestTheGitHalfIsServedOnceAnApplyBuildsTheCodeHost(t *testing.T) {
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
	guide := "# the guide\n"
	write := func(repo, head string) code.EngineWrite {
		return code.EngineWrite{Kind: code.WriteCommitFiles, RepoID: repo, Principal: founder,
			OpID: statelog.NewOpID(time.Now(), ""), Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
			Ref: "refs/heads/main", ExpectedHead: head, Message: "a guide, written through a tool",
			Files: []code.FileOp{{Op: code.FileOpPut, Path: "docs/guide.md", Content: &guide}}}
	}
	_, err = engine.GitWriteForTest(t.Context(), e, write(read.RepoID, read.Tree.Rev))
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "runs no code host") {
		t.Fatalf("an engine write on a git node with no company = %v, want it unserved, the node running no "+
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

	wrote, err := engine.GitWriteForTest(t.Context(), e, write(repo.Repo.ID, commit))
	if err != nil || wrote.Outcome != statelog.OutcomeApplied || wrote.Refusal != "" || wrote.Commit == "" {
		t.Fatalf("the founder's engine write once the apply built the code host = (%+v, %v), want it applied, "+
			"built by this node's code host", wrote, err)
	}
	read.Tree.Rev = wrote.Commit
	if a, err = engine.GitReadForTest(t.Context(), e, read); err != nil || a.Tree == nil ||
		!slices.ContainsFunc(a.Tree.Entries, func(entry code.TreeEntry) bool { return entry.Path == "docs" }) {
		t.Fatalf("a git read of the write's commit = (%+v, %v), want web's root listing docs", a.Tree, err)
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
// node has: a STARTED git node — listed among the git nodes once its host
// beats, and so, alone, the maintainer of every repository — whose compactor's
// pass finds a repository at the pack bound fills its alarm reading's
// compaction half, which the retention loop's own reading carries into the
// alarm table — so code_compaction_behind stands on that node, naming the
// repository, for as long as its pushes would be refused. And it is the
// compactor's FIRST pass that names it: started only once the runtime it reads
// through is published, and passing only once this node is listed, that pass
// finds its listing served and this node the repository's maintainer.
//
// STARTED, because a node is a repository's maintainer only once a listing of
// the git nodes carries it, and only a started node's host beats its presence:
// a node built and never started maintains nothing, its compactor waiting to
// be listed.
//
// Mutations: leave the compactor's half out of the reading (codeReading), and
// the alarm never fires; pass at Start without waiting for this node to be
// listed (compact's awaitListed), and the first pass, before the host beats,
// finds no listing that carries this node — the repository is named a listing
// later.
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
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// THE COMPACTOR'S READING, read until it names the repository: its first
	// pass's, once the node's first beat is listed — which the presence view
	// hears within about a second, the pass following within a
	// codecompact.ListedRecheck — the next comes a Poll later, and the
	// deadline leaves room for it only so that a pass too early fails here by
	// its count rather than by the clock.
	deadline := time.Now().Add(codecompact.Poll + time.Minute)
	for engine.CodeReadingForTest(t.Context(), e).CodeCompactionBehind == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the compactor's reading named no repository at the pack bound after %d listings",
				listings.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := listings.Load(); n != 1 {
		t.Errorf("the compactor's reading first named the repository at the bound after %d listings, want its "+
			"first pass's: a pass before this node is listed finds no maintainer of the repository here", n)
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

// heldPublisher holds every publish of a data node's committer until the case
// releases it — or the batch's own budget ends — and then publishes through
// the code log's own write authority.
type heldPublisher struct {
	inner   code.Publisher
	entered chan struct{}
	release chan struct{}
}

func (h *heldPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return statelog.Result{}, ctx.Err()
	}
	return h.inner.Publish(ctx, req)
}

// busyCompany is gitCompany hosting a second repository, `api`, beside `web`:
// one repository's queue filled while the other's proposal waits out its
// asker's attempt.
const busyCompany = `
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
    - name: api
      owner: {seat: founder}
roles:
  - name: SWE
    handle: swe
  - name: Founder
    handle: founder
    kind: human
`

// THE BUSY ALARM FIRES ON ITS DATA NODE, through the wiring a running node
// has: the committer of a data node WITHOUT the git role — a node that holds
// no git cache, and decides the pushes of every repository rendezvous ranks it
// first for — refuses busy the proposals its repository's full queue cannot
// hold, and one that waited in another repository's queue until its asker
// could wait no longer, and the node's own reading carries them into the
// alarm table, so code_commit_busy stands on that node naming the repository
// with the most by its name. Each queue waits behind its repository's first
// batch, held at its publish.
//
// Mutations: read the committer only where a git cache stands (codeReading's
// early return on a node without one), and the reading counts nothing and the
// alarm never fires here; leave the name out of the reading, and the detail
// names the repository's id; carry the node's share as the proposals that
// waited, and the reading says none waited.
func TestTheCommitBusyAlarmFiresOnItsDataNode(t *testing.T) {
	t.Parallel()
	const past = 3
	b := gitNodeBootstrap(t, []string{"data", "ingress", "seats", "workers"})
	held := &heldPublisher{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e, err := engine.New(t.Context(), engine.WithCommitterPublisher(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, busyCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	}, func(inner code.Publisher) code.Publisher {
		held.inner = inner
		return held
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	// RELEASED BEFORE THE NODE STOPS (cleanups run last first), so the stop
	// does not wait out the held batch's budget.
	var released sync.Once
	release := func() { released.Do(func() { close(held.release) }) }
	t.Cleanup(release)
	if _, hosts := e.CodeHost(); hosts {
		t.Fatal("the premise: a node without the git role builds no code host, and holds no git cache")
	}
	c := engine.CommitterOf(e)
	if c == nil {
		t.Fatal("the premise: a data node of a company hosting code decides its pushes")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	var web, api string
	waitFor(t, "the declared repositories to exist", func() bool {
		answer, err := seam.Read(t.Context(), code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator,
			OperatorID: "op-1", Seat: "founder"}, Kind: code.ReadRepos,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			return false
		}
		for _, r := range answer.Repos {
			switch r.Name {
			case "web":
				web = r.ID
			case "api":
				api = r.ID
			}
		}
		return web != "" && api != ""
	})

	// STALE, every one, so the batches append nothing once released.
	stale := func(repo string, i int) code.Proposal {
		return code.Proposal{
			PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
			Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "founder"},
			Via:       code.ViaGit, Node: "node-a", ReceivedAt: time.Now().UTC(),
			Commands: []code.Command{{Ref: fmt.Sprintf("refs/heads/q%04d", i),
				Old: "1111111111111111111111111111111111111111", New: "2222222222222222222222222222222222222222"}},
		}
	}
	if err := stale(web, 0).Validate(); err != nil {
		t.Fatalf("the premise: the proposal is one a committer would decide: %v", err)
	}
	type answer struct {
		res code.ProposeResult
		err error
	}
	answers := make(chan answer, code.CommitterInbox+past+2)
	propose := func(repo string, i int) {
		res, err := c.Propose(t.Context(), stale(repo, i))
		answers <- answer{res, err}
	}
	entered := func(repo string) {
		t.Helper()
		select {
		case <-held.entered:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s's first batch never reached its publish", repo)
		}
	}
	go propose(web, 0)
	entered("web")
	go propose(api, 0)
	entered("api")

	// A PROPOSAL THAT WAITED OUT ITS ASKER'S ATTEMPT behind api's held
	// batch: queued with two seconds more than a batch and its answer —
	// room for a loaded machine between the deadline and its arrival — and
	// answered busy while it waits.
	short, cancel := context.WithTimeout(t.Context(), code.CommitBudget+code.ReplyMargin+2*time.Second)
	defer cancel()
	if res, err := c.Propose(short, stale(api, 1)); err != nil || res.Answer.Refused != code.RefusalBusy ||
		!strings.Contains(res.Answer.Detail, "stops waiting") {
		t.Fatalf("a proposal that waited out its asker's attempt answered %+v, %v; want busy", res.Answer, err)
	}

	// THE QUEUE FILLED BEHIND THE HELD BATCH, and every proposal past it
	// answered busy at once.
	for i := 1; i <= code.CommitterInbox+past; i++ {
		go propose(web, i)
	}
	for range past {
		select {
		case a := <-answers:
			if a.err != nil || a.res.Answer.Refused != code.RefusalBusy ||
				!strings.Contains(a.res.Answer.Detail, "queued for this repository") {
				t.Fatalf("a proposal past the full queue answered %+v, %v; want busy, the queue full", a.res.Answer,
					a.err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no proposal past the full queue was answered busy")
		}
	}

	reading := engine.CodeReadingForTest(t.Context(), e)
	if reading.CodeCommitBusy != past+1 || reading.CodeCommitBusyOfRepo != past || reading.CodeCommitBusyNode != 0 ||
		reading.CodeCommitBusyWaited != 1 || reading.CodeCommitBusyRepo != "web" {
		t.Fatalf("the node's reading is %d refused, %d of %q, %d at the node's bound, %d waited; want %d "+
			"refused, %d of web by its name, none at the node's bound and 1 waited",
			reading.CodeCommitBusy, reading.CodeCommitBusyOfRepo, reading.CodeCommitBusyRepo,
			reading.CodeCommitBusyNode, reading.CodeCommitBusyWaited, past+1, past)
	}

	// AND THE ALARM, at the node's next evaluation of its reading. The rest
	// is room for a loaded machine.
	deadline := time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeCommitBusy) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with its committer refusing a full queue's pushes, want %s",
				kinds, statelog.KindCodeCommitBusy)
		}
		time.Sleep(50 * time.Millisecond)
	}
	report, ok := e.RetentionReport(t.Context())
	if !ok {
		t.Fatal("a node evaluating alarms has no retention report")
	}
	var detail string
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindCodeCommitBusy {
			detail = a.Detail
		}
	}
	if !strings.Contains(detail, fmt.Sprintf("%d for web alone", past)) ||
		!strings.Contains(detail, "1 after waiting in the queue") {
		t.Errorf("the report's %s says %q, want the repository's refusals named by its name, and the one "+
			"that waited out its asker's attempt", statelog.KindCodeCommitBusy, detail)
	}

	// EVERY CALLER ANSWERED once the batches are let through: web's lead and
	// its full queue, and api's lead.
	release()
	for range code.CommitterInbox + 2 {
		select {
		case <-answers:
		case <-time.After(time.Minute):
			t.Fatal("a queued proposal was never answered after the batch ahead of it was let through")
		}
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

// THE BACKLOG ALARM BORROWS EACH CHECK'S OWN TIMEOUT, through the wiring a
// running node has: two attempts queued on a data node at the same instant,
// two minutes ago — one of a check given a minute to run, one given ten — and
// the node's reading counts the first alone, naming it, so
// code_checks_backlog stands on the node at its next evaluation. The node runs
// no git, so no runner ever claims either: the backlog is measured from the
// rows by the data node, which is how a fleet with no live runner still
// raises it.
//
// Mutation: judge every attempt at one threshold of the rule's own — any one
// — and the two attempts, queued together, are counted together or not at
// all — red.
func TestTheBacklogAlarmBorrowsEachChecksTimeout(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, []string{"data", "ingress", "seats", "workers"})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	if _, hosts := e.CodeHost(); hosts {
		t.Fatal("the premise: a node without the git role runs no check, so nothing here claims one")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	var web string
	waitFor(t, "the declared repository to exist", func() bool {
		answer, err := seam.Read(t.Context(), code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator,
			OperatorID: "op-1", Seat: "founder"}, Kind: code.ReadRepos,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			return false
		}
		for _, r := range answer.Repos {
			if r.Name == "web" {
				web = r.ID
			}
		}
		return web != ""
	})
	queued := time.Now().UTC().Add(-2 * time.Minute)
	for _, c := range []struct {
		name    string
		timeout int
	}{{"lint", 60}, {"unit", 600}} {
		res, err := seam.Checks(t.Context(), code.ChecksRequest{
			OpID: statelog.NewOpID(time.Now(), "code_check_dispatch"),
			Queued: &code.CheckQueued{CheckKey: code.CheckKey{RepoID: web,
				OID: "1111111111111111111111111111111111111111", Target: "refs/heads/main", Name: c.name, Attempt: 1},
				Purpose: code.PurposeTip,
				Def:     &code.CheckDef{Name: c.name, Run: "make " + c.name, TimeoutSeconds: c.timeout},
				Reason:  code.QueueDispatch, QueuedAt: queued}})
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("queue %s: (%+v, %v), want applied", c.name, res, err)
		}
	}
	const lint = "web@111111111111 for main: lint (attempt 1)"
	reading := engine.CodeReadingForTest(t.Context(), e)
	if reading.CodeChecksBacklog != 1 || reading.CodeChecksBacklogCheck != lint ||
		reading.CodeChecksBacklogOldest < 2*time.Minute {
		t.Fatalf("the node's reading is %d behind, the longest %q for %s; want lint's one attempt, past its "+
			"minute, and not unit's, inside its ten", reading.CodeChecksBacklog, reading.CodeChecksBacklogCheck,
			reading.CodeChecksBacklogOldest)
	}

	// AND THE ALARM, at the node's next evaluation of its reading. The rest
	// is room for a loaded machine.
	deadline := time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeChecksBacklog) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with a check queued past its own timeout, want %s", kinds,
				statelog.KindCodeChecksBacklog)
		}
		time.Sleep(50 * time.Millisecond)
	}
	report, ok := e.RetentionReport(t.Context())
	if !ok {
		t.Fatal("a node evaluating alarms has no retention report")
	}
	var detail string
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindCodeChecksBacklog {
			detail = a.Detail
		}
	}
	if !strings.Contains(detail, "1 queued check attempt(s)") || !strings.Contains(detail, lint) {
		t.Errorf("the report's %s says %q, want lint's attempt alone named", statelog.KindCodeChecksBacklog,
			detail)
	}
}
