package engine_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// logless is a company that keeps nothing native, the base of each revision
// these cases activate. Their node boots with NO company, because Gitlet is
// built by the runtime its FIRST company's apply brings up; turning Gitlet on
// under a running runtime only says native_backend_needs_restart.
const logless = `
name: Acme
tracker:
  backend: none
knowledge:
  backend: none
roles:
  - name: SWE
    handle: swe
`

// AN APPLIED EPOCH LOGS ITS CROSS-TIER WARNINGS ONCE, AND A REFUSED APPLY NONE.
//
// Here Tier A's cache budget sits below the default repository size limit,
// beside a revision that hosts code. The warning is said once for the epoch,
// not again at the next tick, and never while the apply is REFUSED (the
// cache's directory holds somebody else's file, so Gitlet cannot be built),
// which every retry would repeat for a revision the node is not serving.
//
// Mutation: warn whatever the apply's status, and the refused ticks log it.
func TestAnAppliedEpochLogsItsTierWarnings(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t) // an activation hosts code on this node
	logs := &logBuffer{}
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Gitlet.CacheMaxBytes = config.GitletCacheMaxBytesFloor
	})
	stray := filepath.Join(b.Gitlet.CacheDirFor(b.Store.Path), "not-the-caches")
	if err := os.MkdirAll(filepath.Dir(stray), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("an operator's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := unconfiguredEngineWith(t, engine.Options{Bootstrap: b, GitHook: gitlettest.HookCommand(t)})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})

	epoch := p.activate(t.Context(), t, logless+"code:\n  backend: gitlet\n")
	for range 2 {
		if err := p.recon.Tick(t.Context()); err == nil {
			t.Fatal("the premise: the apply built Gitlet over a directory holding another's file")
		}
	}
	if got := logs.records(t, "config_tier_warning"); len(got) != 0 {
		t.Fatalf("a refused apply logged %v", got)
	}

	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("the apply, its directory cleared: %v", err)
		}
	}
	if _, ok := e.GitletHost(); !ok {
		t.Fatal("the apply that brought the runtime up did not build Gitlet")
	}
	got := logs.records(t, "config_tier_warning")
	if len(got) != 1 {
		t.Fatalf("%d cross-tier warnings for one applied epoch and a second tick, want 1: %v",
			len(got), got)
	}
	if got[0]["path"] != "gitlet.cache_max_bytes" || got[0]["epoch"] != float64(epoch) ||
		!strings.Contains(got[0]["detail"].(string), "repo_max_bytes") {
		t.Fatalf("the warning is %v, want the cache budget's, on epoch %d", got[0], epoch)
	}
}

// A GITLET NODE COMPARES THE BUDGET IT DERIVED WITH THE REPOSITORY LIMIT. With
// gitlet.cache_max_bytes unset, the budget comes from the cache's volume,
// which neither document knows. Here the volume has 64 GiB free, 48 of them
// the broker's, so the budget is half of the 16 GiB left less the spools (a
// disk the case sets, never the machine's), and a company allowing a
// one-tebibyte repository, the largest limit, is told the cache cannot hold it.
//
// Mutation: hand the warning the documents' budget rather than the one the
// node derived, and nothing is said.
func TestAGitletNodeComparesItsDerivedBudget(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t) // an activation hosts code on this node
	const gib = int64(1) << 30
	logs := &logBuffer{}
	e := unconfiguredEngineWith(t, engine.WithCacheDisk(engine.Options{
		GitHook: gitlettest.HookCommand(t)}, 64*gib, 48*gib))
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	p.activate(t.Context(), t, logless+"code:\n  backend: gitlet\n  limits:\n"+
		"    repo_max_bytes: 1099511627776\n")
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the apply: %v", err)
	}
	if _, ok := e.GitletHost(); !ok {
		t.Fatal("the apply that brought the runtime up did not build Gitlet")
	}
	got := logs.records(t, "config_tier_warning")
	if len(got) != 1 || got[0]["path"] != "gitlet.cache_max_bytes" ||
		!strings.Contains(got[0]["detail"].(string), "derived from gitlet.cache_dir's volume") {
		t.Fatalf("warnings %v, want one saying the derived budget cannot hold a repository "+
			"at its limit", got)
	}
}

// A NODE BOOTED ON A COMPANY SAYS EACH CROSS-TIER WARNING ONCE, at the apply of
// the activation it booted with: the reconciler's first tick applies that very
// activation, so a boot line as well would say every warning twice.
//
// NOT PARALLEL: the case swaps the process-wide logger the boot writes to, and
// gives the reconciler none of its own so that it writes there too.
//
// Mutation: log the boot company's warnings in engine.New as well, and the
// count is two.
func TestABootedCompanysTierWarningsAreSaidOnce(t *testing.T) {
	gittest.RequireNode(t) // the booted company hosts code on this node
	logs := &logBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	hosting := logless + "code:\n  backend: gitlet\n"
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Gitlet.CacheMaxBytes = config.GitletCacheMaxBytesFloor
	})
	e := newEngine(t, engine.Options{Bootstrap: b, Company: parsedCompany(t, hosting),
		GitHook: gitlettest.HookCommand(t)})
	p := planeFor(t, e)
	p.activate(t.Context(), t, hosting)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the initial tick: %v", err)
	}
	if got := logs.records(t, "config_tier_warning"); len(got) != 1 {
		t.Fatalf("a boot and its initial tick said %d cross-tier warnings, want the one "+
			"once: %v", len(got), got)
	}
}
