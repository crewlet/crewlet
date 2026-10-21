package main

import (
	"context"
	"slices"
	"testing"

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
	`"providers":{"llm":{"primary":{"model":"claude-sonnet-5",` +
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
// the store cfg names, and stops it when the case ends.
func bootDecided(t *testing.T, cfg string, boot *config.Bootstrap, seed tierBSeed) (*engine.Engine, seedPlan) {
	t.Helper()
	plan, err := decideSeed(t.Context(), boot, seed, storeAt(cfg), quiet())
	if err != nil {
		t.Fatalf("decide the seed: %v", err)
	}
	opts, err := bootOptions(boot, plan)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(t.Context(), opts)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
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
	activateStored(t, cfg, liveRevisionDoc)

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
		t.Errorf("the live company keeps the native tracker and knowledge base, and "+
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
	if _, found, err := e.Backends().Store.Configs().Active(t.Context()); err != nil || found {
		t.Errorf("a managed node's store holds an active revision (%v, %v), want none", found, err)
	}
	if _, found, err := e.Backends().Fleet.Target(t.Context()); err != nil || found {
		t.Errorf("a managed node pointed the fleet at a revision (%v, %v), want none", found, err)
	}
}
