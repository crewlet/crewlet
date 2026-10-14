package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// gitCompany is a company hosting one repository, `web`, owned by its founder,
// whose operator token `ops` is bound to the founder's seat.
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
    contact:
      crewlet_operator_id: ops
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

// gitNodeBootstrap is a data node's Tier A with a persisted stream, its roles
// mutated by roles (nil leaves them undeclared: every role), and the founder's
// token.
func gitNodeBootstrap(t *testing.T, roles []string) *config.Bootstrap {
	t.Helper()
	return bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(filepath.Dir(b.Store.Path), "stream")
		b.Node.Roles = roles
		b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: opsToken}}
	})
}

// newGitNode boots an engine over b for the company doc, its hook this test
// binary, and stops it at the end of the test.
func newGitNode(t *testing.T, b *config.Bootstrap, doc string) (*engine.Engine, error) {
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

// A GIT NODE BOOTS ONLY ON GIT 2.40 OR NEWER, and the refusal names the role,
// code.native, the version it needs and the git it found — a boot, not a first
// push, is where an operator learns the host's git is too old.
//
// Not parallel: it sets PATH, which every git the process starts reads.
func TestAGitNodeBootsOnlyWithGit240(t *testing.T) {
	old := gittest.Fake(t, "git version 2.39.5", "exit 1")
	t.Setenv("PATH", filepath.Dir(old))
	_, err := newGitNode(t, gitNodeBootstrap(t, nil), gitCompany)
	if err == nil {
		t.Fatal("a git node booted on git 2.39.5")
	}
	for _, want := range []string{`"git"`, "code.native", "2.40", old} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	fresh := gittest.Fake(t, "git version 2.40.0", "exit 1")
	t.Setenv("PATH", filepath.Dir(fresh))
	e, err := newGitNode(t, gitNodeBootstrap(t, nil), gitCompany)
	if err != nil {
		t.Fatalf("a git node on git 2.40.0 did not boot: %v", err)
	}
	if _, ok := e.CodeHost(); !ok {
		t.Fatal("a git node on git 2.40.0 built no code host")
	}
}

// A PERSON PUSHES THROUGH THE ENGINE'S OWN HOST: the founder clones the empty
// repository the company declares, commits and pushes with their own operator
// token as the Basic password, the branch lands, and this node's event store
// holds the push audited as the founder, by repository — which is the host
// built over the engine's own Persons, Audit, Estate and Objects.
func TestAPersonPushesThroughTheEnginesHost(t *testing.T) {
	t.Parallel()
	gittest.Require(t)
	b := gitNodeBootstrap(t, nil)
	e, err := newGitNode(t, b, gitCompany)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, ok := e.CodeHost()
	if !ok {
		t.Fatal("the node built no code host")
	}
	seam, ok := engine.OperatorCode(e)
	if !ok {
		t.Fatal("the node serves no code operations")
	}
	waitFor(t, "the declared repository to exist", func() bool {
		answer, err := seam.Read(t.Context(), code.ReadQuery{Kind: code.ReadRepos,
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

	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, h.InfoRefs())
	mux.Handle(host.PatternUploadPack, h.UploadPack())
	mux.Handle(host.PatternReceivePack, h.ReceivePack())
	srv := httptest.NewServer(auth.New(b).Middleware(mux))
	t.Cleanup(srv.Close)
	url := srv.URL + "/git/web.git"

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
		acted.ActorSeat != "founder" || acted.Outcome != types.AuditApplied ||
		acted.Repo != "web" || acted.OperatorID != "ops" {
		t.Fatalf("the push was audited as %+v, want the founder's applied git_push to web", acted)
	}
}
