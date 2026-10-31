package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A NODE IN A MAINTENANCE MODE SERVES A FILE'S BYTES AND WRITES NONE. The mode
// hands no writer to any surface ([engine.OperatorWorkWriter],
// [engine.OperatorPageWriter], behind the engine's one writer gate), so the
// upload and the removal are absent there, and so is every tracker gesture no
// tool makes (the comment edit and the purge, through the halves' Tracker) and
// every knowledge-base one (the rename and the three destructive verbs,
// through their PageStore); a download is a READ, and a maintenance-mode node
// serves its reads — the file listing beside it among them. Gated on the
// writer, the download answered `no_route` for a file the same node had just
// listed.
//
// The halves are what the human write surface serves from; how it serves the
// download from halves holding the rows and the object store and no writer is
// its own suite's (internal/api/workapi's TestAFilesBytesAreServedWhereNoWriterIs),
// and that [readsOnly] clears every per-actor seam is
// [TestReadsOnlyClearsEveryWriterSeam]'s. This is the half only a running
// engine can show: what a maintenance node hands that surface. A node without
// `data` in the mode is held to the same by internal/e2e's
// TestAStatelessNodeInAMaintenanceModeServesAFileAndTakesNone, beside the data
// member it needs to join.
//
// Mutation: keep FileWriter in [readsOnly] and the maintenance node hands its
// write surface a file writer; clear Files there too and the download has no
// rows to read; hand the Tracker or the PageStore past the engine's writer
// gate in [nativeHalves] and the node serves those writes. Each goes red.
func TestAMaintenanceNodeServesAFilesBytesAndWritesNone(t *testing.T) {
	t.Parallel()
	inService, up := nativeHalves(nodeEngine(t, ""))
	if !up {
		t.Fatal("the premise: a node in service has no native halves")
	}
	if inService.Work.FileWriter == nil || inService.Tracker == nil ||
		inService.PageStore == nil {
		t.Fatalf("the premise: a node in service hands its write surface no "+
			"writer (files %v, tracker %v, pages %v), so a maintenance node "+
			"handing none would prove nothing", inService.Work.FileWriter != nil,
			inService.Tracker != nil, inService.PageStore != nil)
	}

	halves, up := nativeHalves(nodeEngine(t, statelog.ModeMaintenance))
	if !up {
		t.Fatal("a node in a maintenance mode serves no native halves at all")
	}
	if halves.Work.Files == nil || halves.Objects == nil {
		t.Errorf("a node in a maintenance mode does not serve a file's bytes, "+
			"which it lists (rows %v, object store %v)",
			halves.Work.Files != nil, halves.Objects != nil)
	}
	for _, writer := range []struct {
		name string
		held bool
	}{
		{"a file's upload and removal", halves.Work.FileWriter != nil},
		{"the tracker's writes", halves.Work.Writer != nil},
		{"the comment edit and the purge", halves.Tracker != nil},
		{"the knowledge base's writes", halves.Pages.Writer != nil},
		{"the knowledge base's rename and destructive verbs", halves.PageStore != nil},
	} {
		if writer.held {
			t.Errorf("a node in a maintenance mode serves %s, a write", writer.name)
		}
	}
}

// A NODE IN A MAINTENANCE MODE HAS NO RETENTION QUESTION, AND A NODE IN
// SERVICE ANSWERS ONE. [RetentionSource] is the one constructor `crewlet run`
// and the end-to-end harness both wire, so what it answers on each kind of
// node is what each surface serves: a maintenance node arms no trim and has
// no floor it published, and a report of zeros there would claim a perfectly
// trimmed log.
//
// Mutation: answer a source whatever RetentionReport says and the maintenance
// node registers the question; answer nil always and the node in service
// loses it. Each goes red.
func TestARetentionSourceIsAnsweredWhereATrimRuns(t *testing.T) {
	t.Parallel()
	source := RetentionSource(t.Context(), nodeEngine(t, ""))
	if source == nil {
		t.Fatal("a node in service answers no retention question")
	}
	if _, ok := source(t.Context()).(statelog.Report); !ok {
		t.Errorf("a node in service answers its retention question with %T, "+
			"want its trim's report", source(t.Context()))
	}
	if RetentionSource(t.Context(), nodeEngine(t, statelog.ModeMaintenance)) != nil {
		t.Error("a node in a maintenance mode, which arms no trim, answers a " +
			"retention question")
	}
}

// nodeEngine boots a real engine — a data node on an embedded stream in a
// directory of its own, running a two-seat company on the native tracker and
// knowledge base — in mode, as `crewlet run -mode <mode>` does, or in service
// where mode is empty ([bootNode]).
func nodeEngine(t *testing.T, mode statelog.MaintenanceMode) *engine.Engine {
	t.Helper()
	e, _ := bootNode(t, mode)
	return e
}

// bootNode is [nodeEngine] with the Tier A it booted on, for a case that signs
// what the node's fleet signs — a box's token, over its keyring. Its roles are
// undeclared, so every role, git among them; its company hosts no code.
//
// ITS STORE IS SEEDED from the binary's migrated image ([storetest.Seed]), as
// the engine's own SeedStore rule has it: no case here is about migrating.
// And its keyring is drawn through the minter `crewlet secrets keygen` uses,
// because every state-log record is signed under one and a node without one
// refuses to start.
func bootNode(t *testing.T, mode statelog.MaintenanceMode) (*engine.Engine, *config.Bootstrap) {
	t.Helper()
	dir := t.TempDir()
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(dir, "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(dir, "stream")
	// THE BROKER'S RESERVATIONS FROM A FIXED SIZE rather than from whatever
	// this machine's disk has free, which is a fact about the machine and not
	// about the node: derived, a runner with under ten gibibytes free refuses
	// the state logs' floors before the engine is up.
	boot.Stream.StoreMaxBytes = 16 << 30
	boot.API.Auth.Admin = iam.AdminServe
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("no randomness for the keyring: %v", err)
	}
	boot.Secrets.ActiveKeyID = "test"
	boot.Secrets.Keys = []config.SecretKey{{ID: "test", Material: secrets.EncodeKey(key)}}
	if !engine.HoldsEstate(&boot) {
		t.Fatal("the premise: the default bootstrap holds no estate, so this " +
			"node would be no data node")
	}
	storetest.Seed(t, store.EstateNode, boot.Store.Path)
	storetest.Seed(t, store.EstateReplicated,
		store.ReplicatedPath(boot.Store.Path, boot.Store.ReplicatedPath))
	company, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    primary:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: primary
  - name: CTO
    handle: cto
    llm: primary
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: &boot, Company: company, Mode: mode,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e, &boot
}
