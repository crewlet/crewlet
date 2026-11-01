package main

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
)

// The boot and the import of a Tier B file, through the real engine: the
// decision `crewlet run` takes, the engine it starts from it, and the seed and
// first reconcile that follow — composed as runEngine composes them.

// liveRevisionDoc is a company changed live: one seat, and an embedding model
// three thousand dimensions wide.
const liveRevisionDoc = `{"name":"Live",` +
	`"providers":{"sandbox":{"fake":true},"llm":{"primary":{"model":"claude-sonnet-5",` +
	`"auth":{"mode":"api-key","api_key":"${ANTHROPIC_API_KEY}"}}},` +
	`"embeddings":{"type":"openai","model":"text-embedding-3-large",` +
	`"api_key":"${OPENAI_API_KEY}","dimensions":3072}},` +
	`"roles":[{"name":"CEO","handle":"ceo","llm":"primary"}]}`

// staleCompanyYAML is the month-old file beside it: the seat deleted live is
// still there, so is the narrower embedding model the live change replaced, and
// so are the tracker and the knowledge base the company had not yet taken up.
const staleCompanyYAML = `
name: Stale File
tracker:
  backend: none
knowledge:
  backend: none
providers:
  sandbox: {fake: true}
  llm:
    primary:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "${ANTHROPIC_API_KEY}"}
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: ${OPENAI_API_KEY}
    dimensions: 1536
roles:
  - name: CEO
    handle: ceo
    llm: primary
  - name: CTO
    handle: cto
    llm: primary
`

// bootDecided starts an engine as `crewlet run` does, from the decision over
// the store cfg names — through the same step, which holds the store until the
// engine has opened it ([bootEngine]) — and stops it when the case ends.
func bootDecided(t *testing.T, cfg string, boot *config.Bootstrap, seed tierBSeed) (*engine.Engine, seedPlan) {
	t.Helper()
	e, plan, err := bootEngine(t.Context(), boot, seed, storeAt(cfg), engine.New, quiet())
	if err != nil {
		t.Fatalf("boot the engine: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	return e, plan
}

// storedBoot is a node's Tier A over a store already migrated, as cfg names it.
func storedBoot(t *testing.T, cfg string) *config.Bootstrap {
	t.Helper()
	boot, err := config.LoadBootstrap(cfg, config.EnvOnly())
	if err != nil {
		t.Fatalf("load the Tier A: %v", err)
	}
	seedEngineStore(t, boot)
	return boot
}

// running is the name of the company e runs and its seats' handles, sorted —
// an empty name for none.
func running(e *engine.Engine) (string, []string) {
	c := e.Company()
	if c == nil {
		return "", nil
	}
	var seats []string
	for _, s := range c.Seats() {
		seats = append(seats, s.Handle)
	}
	slices.Sort(seats)
	return c.Config.Name, seats
}

// A STALE -company FILE NEVER BECOMES THE ENGINE'S COMPANY.
//
// The store's revision was changed live: a seat deleted, the embedding model
// widened, the native tracker and knowledge base taken up. The node restarts
// with the old file on its command line, which the seed ignores — and the
// engine used to boot on it anyway, and what a boot builds once per process
// outlived the reconcile that replaced it. Building it opened the store at the
// FILE's embedding width, so every reconcile of the live revision was refused
// ("restart the node to change the width"), every restart did the same, and
// the node served the deleted seat for good; and it started the native halves
// for a company that keeps neither, so the live company ran with no tracker
// and no knowledge base on this node.
//
// Mutation: hand the engine the file whenever one is given, as the old boot
// did, and the engine starts on "Stale File" with its store at 1536.
func TestAStaleSeedNeverBecomesTheEnginesCompany(t *testing.T) {
	t.Parallel()
	cfg := bootstrapForStore(t, t.TempDir())
	boot := storedBoot(t, cfg)
	serveStored(t, cfg, liveRevisionDoc)

	// Every check below reports rather than stops, so a boot on the file
	// says each thing it got wrong.
	e, plan := bootDecided(t, cfg, boot, seedOf(parse(t, staleCompanyYAML)))
	if name, seats := running(e); name != "Live" || !slices.Equal(seats, []string{"ceo"}) {
		t.Errorf("the engine started on %q %v, want the store's Live with its one seat",
			name, seats)
	}
	if width := e.Backends().Store.EmbeddingDim(); width != 3072 {
		t.Errorf("the store opened at width %d, want the stored revision's 3072", width)
	}

	reconciler, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), quiet())
	if err != nil {
		t.Fatalf("startReconciler: %v", err)
	}
	if name, seats := running(e); name != "Live" || !slices.Equal(seats, []string{"ceo"}) {
		t.Errorf("after the first reconcile the node serves %q %v, want Live with its "+
			"one seat", name, seats)
	}
	if posture := reconciler.Posture(t.Context()); posture != configplane.PostureServe {
		t.Errorf("the node's posture is %q, want %q: the fleet's revision applies",
			posture, configplane.PostureServe)
	}
	if e.Tracker() == nil || e.Pages() == nil {
		t.Errorf("the live company keeps Worklet and Doclet, and "+
			"this node runs tracker %v and pages %v: the halves were started for "+
			"another company", e.Tracker() != nil, e.Pages() != nil)
	}
}

// A FIRST BOOT'S FILE IS BUILT BY THE FIRST RECONCILE, never by the engine's
// boot: the engine starts unconfigured, the seed imports the file and points
// the fleet at it, and the reconcile that follows — before a seat is claimed —
// applies it with the pointer's own instant.
func TestAFirstBootsFileIsBuiltByTheFirstReconcile(t *testing.T) {
	t.Parallel()
	cfg := bootstrapForStore(t, t.TempDir())
	boot := storedBoot(t, cfg)

	e, plan := bootDecided(t, cfg, boot, seedOf(parse(t, companyYAML)))
	if name, _ := running(e); name != "" {
		t.Fatalf("the engine started on %q, want it unconfigured until the file is "+
			"imported", name)
	}
	if _, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), quiet()); err != nil {
		t.Fatalf("startReconciler: %v", err)
	}
	if name, seats := running(e); name != "Acme" || !slices.Equal(seats, []string{"ceo", "cto"}) {
		t.Fatalf("after the first reconcile the node serves %q %v, want the file's Acme",
			name, seats)
	}
	target, found, err := e.Backends().Fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("no pointer: found=%v err=%v", found, err)
	}
	if at := e.Company().ActivatedAt; !at.Equal(target.At) {
		t.Errorf("the company was applied at %s, want the pointer's %s", at, target.At)
	}
}

// A MANAGED NODE WITH AN EMPTY STORE RUNS NO COMPANY, whatever file sits
// beside it: the managing system's first write is what configures it. The
// engine used to boot on the file the import then dropped, and with no
// revision and no pointer nothing ever replaced it — so the node claimed the
// file's seats while logging that the file is never imported.
func TestAManagedNodeOnAnEmptyStoreRunsNoCompany(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := managedBootstrap(t, dir)
	boot := storedBoot(t, cfg)
	seed, err := managedSeed(boot, companyFile(t, dir, "company.yaml", nil), false, "")
	if err != nil {
		t.Fatal(err)
	}

	e, plan := bootDecided(t, cfg, boot, seed)
	if _, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), quiet()); err != nil {
		t.Fatalf("startReconciler: %v", err)
	}
	if name, seats := running(e); name != "" {
		t.Errorf("a managed node runs %q %v from a file it ignores, want no company",
			name, seats)
	}
	if revisions, err := e.Backends().Store.Configs().List(t.Context(), 0, 0); err != nil ||
		len(revisions) != 0 {
		t.Errorf("a managed node's store holds %d revisions (%v), want none", len(revisions), err)
	}
	if _, found, err := e.Backends().Fleet.Target(t.Context()); err != nil || found {
		t.Errorf("a managed node pointed the fleet at a revision (%v, %v), want none", found, err)
	}
}

// unbuildableCompanyYAML is a company valid on paper that no node without the
// key can build: its one seat's executor runs in an E2B box, whose key
// resolves to nothing.
const unbuildableCompanyYAML = `
name: Unbuildable
providers:
  sandbox:
    e2b:
      api_key: "${CREWLET_TEST_E2B_KEY_NEVER_SET}"
  llm:
    coder: {harness: claude-code, model: sonnet, run_in: e2b}
roles:
  - name: SWE
    handle: swe
    llm: coder
`

// A FIRST BOOT WHOSE FILE THIS NODE CANNOT BUILD IS REFUSED WITH NOTHING
// WRITTEN, through the engine's own build.
//
// The engine starts unconfigured to import its file, so the first build of
// that file is the first reconcile's — after the seed has stored it and pointed
// the fleet at it. Unchecked, the boot went on: the first reconcile's refusal
// is a warning, the node served no company while its log said it was serving
// the one it booted with, and every restart was refused on the revision the
// first one had written, with the corrected file ignored because a company now
// existed.
//
// Mutation: hand seedCompany a build that accepts everything and the fleet is
// pointed at the file.
func TestAFirstBootThisNodeCannotBuildIsRefusedWithNothingWritten(t *testing.T) {
	t.Parallel()
	cfg := bootstrapForStore(t, t.TempDir())
	boot := storedBoot(t, cfg)

	e, plan := bootDecided(t, cfg, boot, seedOf(parse(t, unbuildableCompanyYAML)))
	_, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), quiet())
	if err == nil || !strings.Contains(err.Error(), "company.yaml") ||
		!strings.Contains(err.Error(), "api_key") {
		t.Fatalf("startReconciler = %v, want the build's refusal naming the file", err)
	}
	if revisions, _ := e.Backends().Store.Configs().List(t.Context(), 0, 0); len(revisions) != 0 {
		t.Errorf("%d revisions stored for a file this node cannot build, want none",
			len(revisions))
	}
	if _, found, err := e.Backends().Fleet.Target(t.Context()); err != nil || found {
		t.Errorf("the fleet was pointed at a company this node cannot build (%v, %v)",
			found, err)
	}
}

// THE BOOT HOLDS ITS STORE FROM THE READ IT DECIDES ON UNTIL THE ENGINE HAS
// OPENED IT, and gives it back once the engine has.
//
// `crewlet run` decides what the engine boots on by reading this node's store,
// closes it, and opens it for the engine only once the broker is up — seconds
// later, on a cluster join. An offline `crewlet config import` landing in that
// gap used to write the very row the boot had decided on, and the first tick
// then cleared the operator's change under a false "the fleet moved on". So
// the case stands a SECOND PROCESS in the gap, the one place the claim can be
// seen from: every check inside this process shares it. There, the import
// meets the starting node — it takes the node's API, as it does beside a
// running engine, and the listener `crewlet run` binds before it decides
// answers `503 starting` — and a command with no way through the API is
// refused naming this process; the store stays as the boot read it. Once the
// engine has opened the file the boot's share is given back, and the engine's
// alone refuses the command; once the engine has stopped nothing does, and the
// import is written.
//
// Mutations, each run red: give back the claim [storeAt] takes as soon as it
// has it, or take none, and the import is written in the gap; drop the claim
// from the plan [decideFile] makes, and it outlives the engine, so the import
// after the stop is sent to the node instead of written; give it back in
// [bootEngine] before the open rather than after, and the import is written in
// the gap.
func TestTheBootHoldsItsStoreUntilTheEngineHasOpenedIt(t *testing.T) {
	t.Parallel()
	// THE STARTING NODE'S LISTENER, bound before the boot decides (step 5
	// of the boot), answering every route it will serve `503 starting`.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := bootstrapForURL(t, "http://"+listener.Addr().String())
	boot := storedBoot(t, cfg)
	starting, err := api.NewBoot(boot, "cli-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(starting.Handler())
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	// THE OPERATOR'S CREDENTIAL, so the import's way through the node is
	// taken rather than refused for want of one.
	operator := []string{apiTokenEnv + "=" + cliFixtureToken}

	// THE STOPPED NODE'S STORE: a company it ran, and an offline import over
	// it — the offer this start publishes.
	dir := t.TempDir()
	base := serveStored(t, cfg, runnableRevisionDoc)
	renamed := func(name string) func(string) string {
		return func(string) string {
			return strings.Replace(runnableRevisionDoc, `"Nimbus"`, `"`+name+`"`, 1)
		}
	}
	if _, errs, err := configCmd(t, cfg, "import",
		companyFile(t, dir, "offer.yaml", renamed("Nimbus Offline"))); err != nil {
		t.Fatalf("the offline import before the start: %v (%s)", err, errs)
	}
	offered := bootRevisionID(t, cfg)
	later := companyFile(t, dir, "later.yaml", renamed("Nimbus Later"))
	holder := fmt.Sprintf("pid %d ", os.Getpid())

	// THE GAP: the decision has read the store, and the engine has not
	// opened it.
	inTheGap := func(ctx context.Context, opts engine.Options) (*engine.Engine, error) {
		verdict, said := offlineCommand(ctx, t, operator, "config", "import", later, "-config", cfg)
		if verdict != offlineFailed || !strings.Contains(said, "503") ||
			!strings.Contains(said, "starting") {
			t.Errorf("an offline import while the node starts ended %s, want it sent to the "+
				"starting node and answered 503 starting: %s", verdict, said)
		}
		verdict, said = offlineCommand(ctx, t, operator, "config", "activate", base, "-config", cfg)
		if verdict != offlineLocked || !strings.Contains(said, holder) {
			t.Errorf("an offline activate while the node starts ended %s, want it refused by "+
				"the lock, naming this process (%s): %s", verdict, holder, said)
		}
		return engine.New(ctx, opts)
	}
	e, plan, err := bootEngine(t.Context(), boot, tierBSeed{}, storeAt(cfg), inTheGap, quiet())
	if err != nil {
		t.Fatalf("boot the engine: %v", err)
	}
	stop := sync.OnceFunc(func() { e.Stop(context.WithoutCancel(t.Context())) })
	t.Cleanup(stop)
	if plan.boot.ID != offered || plan.boot.Offer == nil {
		t.Fatalf("the engine booted on %s (offer %+v), want the offline offer %s",
			plan.boot.ID, plan.boot.Offer, offered)
	}
	if row := bootRow(t, e.Backends().Store); row.ID != offered || row.Offer == nil {
		t.Errorf("this node boots on %s (offer %+v) once started, want the offer %s the "+
			"boot read, untouched", row.ID, row.Offer, offered)
	}

	// THE ENGINE HOLDS THE FILE NOW, with the boot's share given back.
	if verdict, said := offlineCommand(t.Context(), t, operator, "config", "activate", base,
		"-config", cfg); verdict != offlineLocked || !strings.Contains(said, holder) {
		t.Errorf("an offline activate beside the running engine ended %s, want it refused "+
			"by the lock, naming this process: %s", verdict, said)
	}

	// AND ONCE IT HAS STOPPED NOTHING HOLDS IT: the boot's claim did not
	// outlive the engine it handed the file to.
	stop()
	if verdict, said := offlineCommand(t.Context(), t, operator, "config", "import", later,
		"-config", cfg); verdict != offlineWrote {
		t.Errorf("an offline import once the engine stopped ended %s, want it written: %s",
			verdict, said)
	}
}
