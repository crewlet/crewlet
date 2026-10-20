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
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
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
// holds the push audited as the founder, by repository — which is the host
// built over the engine's own Persons, Audit, Estate and Objects.
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
		acted.ActorSeat != "founder" || acted.Outcome != types.AuditApplied ||
		acted.Repo != "web" || acted.OperatorID != "ops" {
		t.Fatalf("the push was audited as %+v, want the founder's applied git_push to web", acted)
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
	deadline := time.Now().Add(cache.MidxDebounce + engine.AlarmInterval + time.Minute)
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

// serveWeb waits for gitCompany's declared repository to exist on e, and
// serves e's own code host behind b's guard, as the API mounts it: the URL a
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

	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, h.InfoRefs())
	mux.Handle(host.PatternUploadPack, h.UploadPack())
	mux.Handle(host.PatternReceivePack, h.ReceivePack())
	srv := httptest.NewServer(auth.New(b).Middleware(mux))
	t.Cleanup(srv.Close)
	return srv.URL + "/git/web.git"
}
