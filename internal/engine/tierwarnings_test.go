package engine_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
)

// logless is a company that runs no state log — every backend a vendor's, no
// code — so a revision that hosts code is the one that brings the native
// runtime, and this node's code host, up at its apply.
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
// The node's own Tier A is half of every such warning — here an explicit
// cache budget below the default repository size limit, beside a revision
// that hosts code — so it is said on the node, at the apply that makes it
// true: once for the epoch, not again at the next tick, which applies nothing.
// And never while the apply is REFUSED — here the runtime it brings up cannot
// build its code host, because the cache's directory holds somebody else's
// file — which every retry would otherwise repeat, for a revision the node is
// not serving.
//
// Mutation: warn whatever the apply's status, and the refused ticks log it.
func TestAnAppliedEpochLogsItsTierWarnings(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t) // an activation hosts code on this node
	logs := &logBuffer{}
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
	})
	stray := filepath.Join(b.Git.CacheDirFor(b.Store.Path), "not-the-caches")
	if err := os.MkdirAll(filepath.Dir(stray), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("an operator's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, engine.Options{Bootstrap: b, Company: parsedCompany(t, logless),
		GitHook: codetest.HookCommand(t)})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})

	epoch := p.activate(t.Context(), t, logless+"code:\n  native: true\n")
	for range 2 {
		if err := p.recon.Tick(t.Context()); err == nil {
			t.Fatal("the premise: the apply built a code host over a directory holding another's file")
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
	if _, ok := e.CodeHost(); !ok {
		t.Fatal("the apply that brought the runtime up built no code host")
	}
	got := logs.records(t, "config_tier_warning")
	if len(got) != 1 {
		t.Fatalf("%d cross-tier warnings for one applied epoch and a second tick, want 1: %v",
			len(got), got)
	}
	if got[0]["path"] != "git.cache_max_bytes" || got[0]["epoch"] != float64(epoch) ||
		!strings.Contains(got[0]["detail"].(string), "repo_max_bytes") {
		t.Fatalf("the warning is %v, want the cache budget's, on epoch %d", got[0], epoch)
	}
}

// A GIT NODE COMPARES THE BUDGET IT DERIVED WITH THE REPOSITORY LIMIT. An unset
// git.cache_max_bytes is the default on every git node, and the budget it
// derives from the cache's volume is the one a repository at its size limit
// must fit in; the documents alone cannot know it, so the node that sized its
// cache says so at the apply. Here the cache's volume has 64 GiB free and the
// broker storing its streams there may still write three quarters of it, so
// the derived budget is half of the 16 GiB left less the spools — read off a
// disk the case sets, never off the machine running it — and a company
// allowing a repository of a tebibyte, the largest limit there is, is told the
// cache cannot hold one.
//
// Mutation: hand the warning the documents' budget rather than the one the
// node derived, and nothing is said.
func TestAGitNodeComparesItsDerivedBudget(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t) // an activation hosts code on this node
	const gib = int64(1) << 30
	logs := &logBuffer{}
	e := newEngine(t, engine.WithCacheDisk(engine.Options{Company: parsedCompany(t, logless),
		GitHook: codetest.HookCommand(t)}, 64*gib, 48*gib))
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	p.activate(t.Context(), t, logless+"code:\n  native: true\n  limits:\n"+
		"    repo_max_bytes: 1099511627776\n")
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the apply: %v", err)
	}
	if _, ok := e.CodeHost(); !ok {
		t.Fatal("the apply that brought the runtime up built no code host")
	}
	got := logs.records(t, "config_tier_warning")
	if len(got) != 1 || got[0]["path"] != "git.cache_max_bytes" ||
		!strings.Contains(got[0]["detail"].(string), "derived from git.cache_dir's volume") {
		t.Fatalf("warnings %v, want one saying the derived budget cannot hold a repository "+
			"at its limit", got)
	}
}

// A NODE BOOTED ON A COMPANY SAYS EACH CROSS-TIER WARNING ONCE, at the apply of
// the activation it booted with. The reconciler's first tick applies that very
// activation — its progress begins at no epoch applied — so a line logged by
// the boot as well said every warning twice on every configured node, where
// every other apply warning was said once.
//
// NOT PARALLEL: the boot logs through the process-wide logger, which this case
// swaps to read it, and the reconciler is given none of its own so that it
// writes there too.
//
// Mutation: log the boot company's warnings in engine.New as well, and the
// count is two.
func TestABootedCompanysTierWarningsAreSaidOnce(t *testing.T) {
	gittest.RequireNode(t) // the booted company hosts code on this node
	logs := &logBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	hosting := logless + "code:\n  native: true\n"
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Git.CacheMaxBytes = config.GitCacheMaxBytesFloor
	})
	e := newEngine(t, engine.Options{Bootstrap: b, Company: parsedCompany(t, hosting),
		GitHook: codetest.HookCommand(t)})
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
