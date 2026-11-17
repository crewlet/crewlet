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
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	gitletcompact "github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
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
  backend: gitlet
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
  backend: worklet
knowledge:
  backend: none
roles:
  - name: SWE
    handle: swe
`

// opsToken is the founder's operator token: long enough for the guard.
const opsToken = "ops-token-0123456789abcdefghijklmnopqrstuv"

// bindOps binds the `ops` Tier A token to the founder's human seat in e's
// identity directory (the row `crewlet iam create -kind machine -login
// token:ops -seat founder` writes) and waits until e reports it: the guard
// reads this node's own copy, and a token bound to nobody only fetches.
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

// gitletNodeBootstrap is a data node's Tier A with a persisted stream, its roles
// set to roles (nil: every role), and the founder's token, whose grants list
// Gitlet, without which it reaches no repository (ADR-0110); what it may do
// there is its bound seat's standing (ADR-0088).
func gitletNodeBootstrap(t *testing.T, roles []string) *config.Bootstrap {
	t.Helper()
	return bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		brokertest.GitletCacheIn(&b.Gitlet)
		b.Node.Roles = roles
		b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: opsToken,
			Grants: []iam.Grant{iam.GrantGitlet}}}
	})
}

// newGitletNode boots an engine over b for doc, its hook this test binary,
// stopped at the test's end. Where the boot builds Gitlet it first holds the
// suite's git to the node floor (engine.RequireNodeGit), so a machine below it
// reads the contributor's sentence rather than the node's refusal.
func newGitletNode(t *testing.T, b *config.Bootstrap, doc string) (*engine.Engine, error) {
	t.Helper()
	engine.RequireNodeGit(t, b, parsedCompany(t, doc))
	return bootGitletNode(t, b, doc)
}

// bootGitletNode is newGitletNode on whatever git PATH names, for a case that hands
// the boot a git of its own.
func bootGitletNode(t *testing.T, b *config.Bootstrap, doc string) (*engine.Engine, error) {
	t.Helper()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, doc),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     gitlettest.HookCommand(t),
	})
	if err == nil {
		t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	}
	return e, err
}

// GITLET'S GIT SURFACE IS BUILT EXACTLY WHERE THE COMPANY HOSTS CODE AND THE
// NODE HAS THE GITLET ROLE: otherwise no host and no cache directory; with
// both, a host whose directory is tagged as the cache's.
//
// Mutation: build the host regardless of the gitlet role, and the second case
// goes red.
func TestACompanyWithoutGitletBuildsNoHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		roles []string
		doc   string
		hosts bool
	}{
		{"no code hosted, every role", nil, noCodeCompany, false},
		{"code hosted, roles without gitlet", []string{"data", "ingress", "seats", "workers"}, gitCompany, false},
		{"code hosted, every role", nil, gitCompany, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := gitletNodeBootstrap(t, tc.roles)
			e, err := newGitletNode(t, b, tc.doc)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, hosts := e.GitletHost()
			dir := filepath.Join(filepath.Dir(b.Store.Path), "git")
			_, statErr := os.Stat(filepath.Join(dir, "CACHEDIR.TAG"))
			switch {
			case hosts != tc.hosts:
				t.Fatalf("GitletHost() = %t, want %t", hosts, tc.hosts)
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

// A GITLET NODE VERIFIES A BOX'S TOKEN, WITH GITLET BUILT OR NOT: box and
// check tokens are built at New over the fleet's keyring, since a box route
// finding Gitlet unbuilt says so only once the token verifies
// (api.CodeSurface.VerifyBox). A peer's token verifies; another keyring's not.
//
// Mutation: build the box or check tokens with Gitlet rather than at New, and
// the node serving none holds none, red.
func TestAGitletNodeVerifiesABoxsTokenWithOrWithoutItsHost(t *testing.T) {
	t.Parallel()
	ref := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	stranger, err := host.NewBoxTokens(runtoken.OneKey("k", "another fleet's key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	check := host.CheckRunRef{Repo: "web", Attempt: gitlet.CheckKey{RepoID: uuid.NewString(),
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
			b := gitletNodeBootstrap(t, nil)
			e, err := newGitletNode(t, b, tc.doc)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, hosts := e.GitletHost(); hosts != tc.hosts {
				t.Fatalf("GitletHost() = %t, want %t", hosts, tc.hosts)
			}
			peer, err := host.NewBoxTokens(b.Secrets.TokenMaterial(), nil)
			if err != nil {
				t.Fatal(err)
			}
			tokens := e.BoxTokens()
			if tokens == nil {
				t.Fatal("the node holds no box tokens")
			}
			peerToken, err := peer.Mint(ref)
			if err != nil {
				t.Fatal(err)
			}
			strangerToken, err := stranger.Mint(ref)
			if err != nil {
				t.Fatal(err)
			}
			if got, signed := tokens.Verify(peerToken); !signed || got != ref {
				t.Errorf("a token the fleet's keyring signed verifies as %+v, %t; want %+v", got, signed, ref)
			}
			if _, signed := tokens.Verify(strangerToken); signed {
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
			peerCheck, err := peerChecks.Mint(check, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			strangerCheck, err := strangerChecks.Mint(check, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got, signed := checks.Verify(peerCheck); !signed || got != check {
				t.Errorf("a check token the fleet's keyring signed verifies as %+v, %t; want %+v", got, signed,
					check)
			}
			if _, signed := checks.Verify(strangerCheck); signed {
				t.Error("a check token another keyring signed verifies")
			}
		})
	}
}

// A VOLUME WITH NO ROOM FOR A CACHE REFUSES THE GITLET NODE BY NAME: with the
// broker free to fill the volume, the request spools leave the cache no share,
// and the refusal is the engine's account of the volume (the figures and every
// setting that changes one), never the cache's refusal of a zero budget, which
// names none. An explicit gitlet.cache_max_bytes on the same volume boots.
//
// Mutation: derive a zero budget without refusing it, and the refusal is the
// cache's, naming no setting.
func TestAVolumeWithNoRoomRefusesGitletByName(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const gib = int64(1) << 30
	boot := func(b *config.Bootstrap) (*engine.Engine, error) {
		e, err := engine.New(t.Context(), engine.WithCacheDisk(engine.Options{
			Bootstrap: b, Company: parsedCompany(t, gitCompany),
			ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			GitHook:     gitlettest.HookCommand(t),
		}, 16*gib, 16*gib))
		if err == nil {
			t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
		}
		return e, err
	}

	// THE DERIVED BUDGET: cleared, as the fixture declares one
	// (brokertest.GitletCacheIn); the room is read off WithCacheDisk's volume,
	// never the machine's.
	derived := gitletNodeBootstrap(t, nil)
	derived.Gitlet.CacheMaxBytes = 0
	_, err := boot(derived)
	if err == nil {
		t.Fatal("a Gitlet node booted on a volume whose broker may fill all of it")
	}
	for _, want := range []string{"no room for the code cache", "gitlet.cache_dir",
		"gitlet.cache_max_bytes", "stream.store_max_bytes", "gitlet.max_transfers",
		fmt.Sprintf("its %d transfers'", engine.TestGitTransfers)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	b := gitletNodeBootstrap(t, nil)
	b.Gitlet.CacheMaxBytes = config.GitletCacheMaxBytesFloor
	e, err := boot(b)
	if err != nil {
		t.Fatalf("an explicit budget on the same volume was refused: %v", err)
	}
	if _, ok := e.GitletHost(); !ok {
		t.Fatal("a node given an explicit budget did not build Gitlet")
	}
}

// A GITLET NODE BOOTS ONLY AT ITS FLOOR (gitproc.NodeMinVersion), refused at
// boot rather than at a first push, naming the role, code.backend: gitlet, the
// version it needs and the git it found.
//
// Not parallel: it sets PATH, which every git the process starts reads.
//
// Mutation: check gitproc.ClientMinVersion in gitproc.New, and the 2.54.9 node
// boots.
func TestAGitletNodeBootsOnlyAtItsFloor(t *testing.T) {
	old := gittest.Fake(t, "git version 2.54.9", "exit 1")
	t.Setenv("PATH", filepath.Dir(old))
	_, err := bootGitletNode(t, gitletNodeBootstrap(t, nil), gitCompany)
	if err == nil {
		t.Fatal("a Gitlet node booted on git 2.54.9")
	}
	for _, want := range []string{`"gitlet"`, "code.backend: gitlet", gitproc.NodeMinVersion.String(), "2.54.9", old} {
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
	e, err := bootGitletNode(t, gitletNodeBootstrap(t, nil), gitCompany)
	if err != nil {
		t.Fatalf("a Gitlet node on git %s did not boot: %v", gitproc.NodeMinVersion, err)
	}
	if _, ok := e.GitletHost(); !ok {
		t.Fatalf("a Gitlet node on git %s did not build Gitlet", gitproc.NodeMinVersion)
	}
}

// A PERSON PUSHES THROUGH GITLET with their operator token as the Basic
// password: the branch lands, and this node's event store audits the push as
// the founder, by repository and token, so the host is built over the
// engine's own guard, Audit, Estate and Objects.
func TestAPersonPushesThroughTheEnginesHost(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, nil)
	e, err := newGitletNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := serveWeb(t, b, e)

	founder := gitlettest.PersonWithToken(t, opsToken)
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

// A PUSH'S CHANGE IS LINKED ON THE DEPLOYMENT'S ADDRESS: a push with `-o
// change` prints the change it opened with its page on `api.external_url`,
// which the host is built with from Tier A.
//
// Mutation: build the host with no ExternalBase (buildGitletHost), and git
// prints the change's name alone, red.
func TestAPushsChangeIsLinkedOnTheDeploymentsAddress(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, nil)
	b.API.ExternalURL = "https://crewlet.example.com/"
	e, err := newGitletNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := serveWeb(t, b, e)

	founder := gitlettest.PersonWithToken(t, opsToken)
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
	const want = "remote: crewlet: change web!1 opened: https://crewlet.example.com/dashboard#/gitlet/repos/web/changes/1"
	if !strings.Contains(pushed.Stderr, want) {
		t.Fatalf("git printed no %q:\n%s", want, pushed)
	}
}

// A GITLET NODE ANSWERS THE FLEET'S CODE STATUS on topics.CodeStatus with its
// host's own status: the node, its cache, and the repository's heat and copy
// as the founder's clone and push left them, on the wire's own fields
// (version, repo, nodes, without_hot, status). A question naming its nodes is
// answered by those alone, under the host's own id; one at a version this
// build does not speak is answered by nobody, which the asker reads as a node
// that did not answer, never a guess at its fields.
//
// Mutation: register no answerer beside the host (buildGitletHost), and nobody
// answers; register it under another id than the host's, and the question
// naming the node goes unanswered, red.
func TestAGitletNodeAnswersTheFleetsCodeStatus(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, nil)
	e, err := newGitletNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := serveWeb(t, b, e)
	founder := gitlettest.PersonWithToken(t, opsToken)
	dir := founder.Clone(url)
	founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := founder.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}
	h, _ := e.GitletHost()
	seam, _ := engine.OperatorCode(e)
	repo, err := seam.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web",
		Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: iam.TokenLogin("ops"), Seat: "founder"},
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
		t.Fatalf("a code status question was answered %d times, want once, by the Gitlet node", len(replies))
	}
	var answer struct {
		Version int             `json:"version"`
		Status  host.CodeStatus `json:"status"`
	}
	if err := json.Unmarshal(replies[0], &answer); err != nil {
		t.Fatalf("the answer %s: %v", replies[0], err)
	}
	st := answer.Status
	self := h.Status(host.StatusAsk{}).Node
	if answer.Version != 1 || st.Node != self || st.Cache.Warm != 1 || st.Cache.Budget <= 0 {
		t.Fatalf("the answer is version %d from node %q with cache %+v, want version 1 from %q, its one "+
			"warm repository and its budget", answer.Version, st.Node, st.Cache, self)
	}
	if st.Repo == nil || st.Repo.ID != id || st.Repo.Day.Pushes != 1 || st.Repo.Day.Fetches != 1 ||
		st.Repo.Copy == nil {
		t.Fatalf("the repository asked about is answered %+v, want web's heat — the founder's one clone of it "+
			"empty and one push — and its copy in service", st.Repo)
	}
	if len(st.Hot) != 1 || st.Hot[0].ID != id {
		t.Fatalf("a question that names no node and leaves its hot list asked is answered hot %+v, want web",
			st.Hot)
	}

	// A QUESTION THAT NAMES ITS NODES is answered by those alone, and one
	// asking for no hot list gets none. The answer is cleared first: decoding
	// leaves a field the JSON lacks as it was.
	answer.Status = host.CodeStatus{}
	if replies := ask(`{"version":1,"nodes":["`+self+`"],"without_hot":true}`, 10*time.Second); len(replies) != 1 {
		t.Fatalf("a question naming the Gitlet node was answered %d times, want once", len(replies))
	} else if err := json.Unmarshal(replies[0], &answer); err != nil || answer.Status.Node != self ||
		answer.Status.Hot != nil {
		t.Fatalf("a question naming the Gitlet node and no hot list is answered %s (%v), want its status "+
			"with no hot list", replies[0], err)
	}
	if replies := ask(`{"version":1,"nodes":["another-git-node"]}`, time.Second); len(replies) != 0 {
		t.Fatalf("a question naming another Gitlet node was answered by this one: %s", replies)
	}

	if replies := ask(`{"version":2}`, time.Second); len(replies) != 0 {
		t.Fatalf("a question at a version this build does not speak was answered: %s", replies)
	}
}

// THE GIT HALF IS SERVED ONCE AN APPLY BUILDS GITLET, with no restart. With no
// company, a git read by object id and an engine write through the node's
// router are each unserved, saying so; after the first apply setting
// code.backend: gitlet, this node's git half answers the read of the founder's
// pushed commit and builds and lands the founder's next write. The router and
// the estate server resolve that half per request: one resolved at build
// would answer unserved until a restart.
//
// Mutations: resolve the git half once, when the router is built, and the read
// after the apply is unserved, red; stub Gitlet's Write so writes answer with
// nothing built, and the write after the apply is not applied, red.
func TestTheGitHalfIsServedOnceAnApplyBuildsGitlet(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	b := gitletNodeBootstrap(t, nil)
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: b, GitHook: gitlettest.HookCommand(t)})
	if err != nil {
		t.Fatalf("an engine with no company was refused: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	founder := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: iam.TokenLogin("ops"), Seat: "founder"}
	read := gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: "019a0c2e-0000-7000-8000-0000000000a1", Asker: founder,
		Tree: &gitlet.TreeQuery{Rev: "1111111111111111111111111111111111111111"}}
	_, err = engine.GitReadForTest(t.Context(), e, read)
	var unserved *estate.ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "does not run Gitlet") {
		t.Fatalf("a git read on a Gitlet node with no company = %v, want it unserved, the node not running "+
			"Gitlet", err)
	}
	guide := "# the guide\n"
	write := func(repo, head string) gitlet.EngineWrite {
		return gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: repo, Principal: founder,
			OpID: statelog.NewOpID(time.Now(), ""), Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
			Ref: "refs/heads/main", ExpectedHead: head, Message: "a guide, written through a tool",
			Files: []gitlet.FileOp{{Op: gitlet.FileOpPut, Path: "docs/guide.md", Content: &guide}}}
	}
	_, err = engine.GitWriteForTest(t.Context(), e, write(read.RepoID, read.Tree.Rev))
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "does not run Gitlet") {
		t.Fatalf("an engine write on a Gitlet node with no company = %v, want it unserved, the node not running "+
			"Gitlet", err)
	}

	if status, _, err := e.Apply(t.Context(), parsedCompany(t, gitCompany), time.Now()); err != nil ||
		status != configplane.StatusOK {
		t.Fatalf("the first apply, which hosts code = (%s, %v)", status, err)
	}
	url := serveWeb(t, b, e)
	person := gitlettest.PersonWithToken(t, opsToken)
	dir := person.Clone(url)
	commit := person.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := person.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations after the apply")
	}
	repo, err := seam.Read(t.Context(), gitlet.ReadQuery{Asker: founder, Kind: gitlet.ReadRepo, Repo: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil || repo.Repo == nil {
		t.Fatalf("the repository's detail: %+v, %v", repo.Repo, err)
	}
	read.RepoID, read.Tree.Rev = repo.Repo.ID, commit
	a, err := engine.GitReadForTest(t.Context(), e, read)
	if err != nil || a.Tree == nil || !slices.ContainsFunc(a.Tree.Entries, func(entry gitlet.TreeEntry) bool {
		return entry.Path == "README.md"
	}) {
		t.Fatalf("a git read once the apply built Gitlet = (%+v, %v), want web's root listing "+
			"README.md, answered by this node", a.Tree, err)
	}

	wrote, err := engine.GitWriteForTest(t.Context(), e, write(repo.Repo.ID, commit))
	if err != nil || wrote.Outcome != statelog.OutcomeApplied || wrote.Refusal != "" || wrote.Commit == "" {
		t.Fatalf("the founder's engine write once the apply built Gitlet = (%+v, %v), want it applied, "+
			"built by Gitlet on this node", wrote, err)
	}
	read.Tree.Rev = wrote.Commit
	if a, err = engine.GitReadForTest(t.Context(), e, read); err != nil || a.Tree == nil ||
		!slices.ContainsFunc(a.Tree.Entries, func(entry gitlet.TreeEntry) bool { return entry.Path == "docs" }) {
		t.Fatalf("a git read of the write's commit = (%+v, %v), want web's root listing docs", a.Tree, err)
	}
}

// A GITLET NODE'S OWN ALARMS NAME A BITMAP ITS CACHE REFUSED FOR MEMORY:
// code_maintenance_refused stands on that node once it holds a pack, never
// from its configuration alone. The memory is arithmetic, not the machine's:
// the pool is told 8 GiB (engine.WithGitMemory) and gitlet.max_transfers is one
// more than that holds at gitproc.TransferMemory each, leaving background work
// no budget. The cache budget is explicit so free disk decides nothing either
// (37 transfers' spools take 2 368 MiB of a derived budget's room,
// host.SpoolAllowance).
//
// Mutation: drop r.code from the reading's assembly (retentionreport.go), or
// fill nothing from the cache in codeReading, and the alarm never fires.
func TestTheGitCachesMaintenanceAlarmFiresOnItsNode(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	const memory = int64(8) << 30
	b := gitletNodeBootstrap(t, nil)
	b.Gitlet.MaxTransfers = int(memory/gitproc.TransferMemory) + 1
	b.Gitlet.CacheMaxBytes = config.GitletCacheMaxBytesFloor
	if kept := int64(b.Gitlet.MaxTransfers) * gitproc.TransferMemory; kept <= memory {
		t.Fatalf("%d transfers keep %d bytes of a %d-byte node, which leaves its background work a "+
			"budget", b.Gitlet.MaxTransfers, kept, memory)
	}
	e, err := engine.New(t.Context(), engine.WithGitMemory(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     gitlettest.HookCommand(t),
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

	founder := gitlettest.PersonWithToken(t, opsToken)
	dir := founder.Clone(url)
	founder.Commit(dir, map[string]string{"README.md": "# web\n"}, "first")
	if pushed := founder.Push(t.Context(), dir, url, "HEAD:refs/heads/main"); !pushed.OK("refs/heads/main") {
		t.Fatalf("the founder's push did not land:\n%s", pushed)
	}

	// A MAINTAINER PASS AND AN EVALUATION; the minute is room for a loaded
	// machine, never the expected wait.
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

// atTheBound is a Gitlet node's compactor's code seam answering every due
// listing with one more repository at gitlet.MaxPacksPerRepo, the reading the
// compactor raises the alarm from without that many packs pushed, and counting
// every listing.
type atTheBound struct {
	gitletcompact.Estate
	listings *atomic.Int64
}

// boundRepo is the repository atTheBound names.
const boundRepo = "at-the-bound"

func (e atTheBound) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if q.Kind != gitlet.ReadCompactionDue {
		return e.Estate.Read(ctx, q)
	}
	e.listings.Add(1)
	return gitlet.ReadAnswer{Due: []gitlet.CompactionCandidate{{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(boundRepo)).String(), Name: boundRepo,
		LivePacks: gitlet.MaxPacksPerRepo,
	}}}, nil
}

// THE COMPACTION-BEHIND ALARM FIRES ON ITS NODE through a running node's
// wiring: a STARTED Gitlet node (only a started host beats its presence, and
// only a listed node maintains a repository) whose compactor finds a
// repository at the pack bound fills its reading's compaction half, and
// code_compaction_behind stands naming it. The compactor's FIRST pass names
// it: it starts once its runtime is published and passes once this node is listed.
//
// Mutations: leave the compactor's half out of the reading (codeReading), and
// the alarm never fires; pass at Start without waiting to be listed (compact's
// awaitListed), and the first pass finds no listing carrying this node, so the
// repository is named a listing later.
func TestTheCompactionBehindAlarmFiresOnItsNode(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, gitCompany))
	var listings atomic.Int64
	e, err := engine.New(t.Context(), engine.WithCompactorEstate(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     gitlettest.HookCommand(t),
	}, func(inner gitletcompact.Estate) gitletcompact.Estate {
		return atTheBound{Estate: inner, listings: &listings}
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// THE COMPACTOR'S READING, until it names the repository: the first beat is
	// heard within about a second and the pass follows within a
	// gitletcompact.ListedRecheck; the deadline allows one Poll more only so a
	// pass too early fails on its count, not the clock.
	deadline := time.Now().Add(gitletcompact.Poll + time.Minute)
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
	inner   gitlet.Publisher
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
  backend: gitlet
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

// THE BUSY ALARM FIRES ON ITS DATA NODE through a running node's wiring: the
// committer on a data node WITHOUT the gitlet role (no git cache, deciding the
// pushes rendezvous ranks it first for) refuses busy the proposals a full
// queue cannot hold and one that outwaited its asker in another repository's
// queue, and code_commit_busy stands naming the repository with the most. Each
// queue waits behind its repository's first batch, held at its publish.
//
// Mutations: read the committer only where a git cache stands (codeReading's
// early return), and the alarm never fires here; leave the name out of the
// reading, and the detail names the id; carry the node's share as the
// proposals that waited, and the reading says none waited.
func TestTheCommitBusyAlarmFiresOnItsDataNode(t *testing.T) {
	t.Parallel()
	const past = 3
	b := gitletNodeBootstrap(t, []string{"data", "ingress", "seats", "workers"})
	held := &heldPublisher{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e, err := engine.New(t.Context(), engine.WithCommitterPublisher(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, busyCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	}, func(inner gitlet.Publisher) gitlet.Publisher {
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
	if _, hosts := e.GitletHost(); hosts {
		t.Fatal("the premise: a node without the gitlet role serves no git, and holds no git cache")
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
		answer, err := seam.Read(t.Context(), gitlet.ReadQuery{Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator,
			OperatorID: "op-1", Seat: "founder"}, Kind: gitlet.ReadRepos,
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
	stale := func(repo string, i int) gitlet.Proposal {
		return gitlet.Proposal{
			PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
			Principal: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1", Seat: "founder"},
			Via:       gitlet.ViaGit, Node: "node-a", ReceivedAt: time.Now().UTC(),
			Commands: []gitlet.Command{{Ref: fmt.Sprintf("refs/heads/q%04d", i),
				Old: "1111111111111111111111111111111111111111", New: "2222222222222222222222222222222222222222"}},
		}
	}
	if err := stale(web, 0).Validate(); err != nil {
		t.Fatalf("the premise: the proposal is one a committer would decide: %v", err)
	}
	type answer struct {
		res gitlet.ProposeResult
		err error
	}
	answers := make(chan answer, gitlet.CommitterInbox+past+2)
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

	// A PROPOSAL THAT OUTWAITS ITS ASKER behind api's held batch is answered
	// busy; its deadline is a batch and its answer plus two seconds of room
	// for a loaded machine.
	short, cancel := context.WithTimeout(t.Context(), gitlet.CommitBudget+gitlet.ReplyMargin+2*time.Second)
	defer cancel()
	if res, err := c.Propose(short, stale(api, 1)); err != nil || res.Answer.Refused != gitlet.RefusalBusy ||
		!strings.Contains(res.Answer.Detail, "stops waiting") {
		t.Fatalf("a proposal that waited out its asker's attempt answered %+v, %v; want busy", res.Answer, err)
	}

	// THE QUEUE FILLED BEHIND THE HELD BATCH, and every proposal past it
	// answered busy at once.
	for i := 1; i <= gitlet.CommitterInbox+past; i++ {
		go propose(web, i)
	}
	for range past {
		select {
		case a := <-answers:
			if a.err != nil || a.res.Answer.Refused != gitlet.RefusalBusy ||
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
	for range gitlet.CommitterInbox + 2 {
		select {
		case <-answers:
		case <-time.After(time.Minute):
			t.Fatal("a queued proposal was never answered after the batch ahead of it was let through")
		}
	}
}

// serveWeb waits for gitCompany's `web` to exist on e, binds `ops` to the
// founder ([bindOps]) and serves Gitlet behind b's guard over e's identity
// directory, as the API mounts it, returning the URL git reaches `web` at.
func serveWeb(t *testing.T, b *config.Bootstrap, e *engine.Engine) string {
	t.Helper()
	h, ok := e.GitletHost()
	if !ok {
		t.Fatal("the node did not build Gitlet")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	waitFor(t, "the declared repository to exist", func() bool {
		answer, err := seam.Read(t.Context(), gitlet.ReadQuery{Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"}, Kind: gitlet.ReadRepos,
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

// THE BACKLOG ALARM BORROWS EACH CHECK'S OWN TIMEOUT: of two attempts queued
// together two minutes ago, one of a check given a minute and one given ten,
// the data node's reading counts the first alone, naming it, and
// code_checks_backlog stands at its next evaluation. The node runs no git, so
// no runner claims either: the backlog is measured from the rows, which is how
// a fleet with no live runner still raises it.
//
// Mutation: judge every attempt at one threshold of the rule's own, and the
// two are counted together or not at all, red.
func TestTheBacklogAlarmBorrowsEachChecksTimeout(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, []string{"data", "ingress", "seats", "workers"})
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	if _, hosts := e.GitletHost(); hosts {
		t.Fatal("the premise: a node without the gitlet role runs no check, so nothing here claims one")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	var web string
	waitFor(t, "the declared repository to exist", func() bool {
		answer, err := seam.Read(t.Context(), gitlet.ReadQuery{Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator,
			OperatorID: "op-1", Seat: "founder"}, Kind: gitlet.ReadRepos,
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
		res, err := seam.Checks(t.Context(), gitlet.ChecksRequest{
			OpID: statelog.NewOpID(time.Now(), "code_check_dispatch"),
			Queued: &gitlet.CheckQueued{CheckKey: gitlet.CheckKey{RepoID: web,
				OID: "1111111111111111111111111111111111111111", Target: "refs/heads/main", Name: c.name, Attempt: 1},
				Purpose: gitlet.PurposeTip,
				Def:     &gitlet.CheckDef{Name: c.name, Run: "make " + c.name, TimeoutSeconds: c.timeout},
				Reason:  gitlet.QueueDispatch, QueuedAt: queued}})
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
