package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/accessmigrate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// A PASS FLUSHES TWICE, once after its vendor's company-level work and once
// after its seats' access sync, and the second flush's seals reach this node
// too: it asks for a re-activation again, which the republisher's window turns
// into the trailing run. So does a value an off cleared: a seat turned off must
// not go on starting its tool with the credential its off revoked.
func TestEveryFlushAfterASealOrAClearRebuilds(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)
	armed := func() bool {
		e.republish.mu.Lock()
		defer e.republish.mu.Unlock()
		return e.republish.pending != nil
	}

	sink := &refreshingSink{TokenSink: &failingFlushSink{}, engine: e}
	ctx := t.Context()
	if err := sink.Record(ctx, "JIRA_AGENT_TOKEN", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	_ = sink.Flush(ctx)
	w.awaitReloads(t, 1)
	if err := sink.Record(ctx, "DATADOG_APP_KEY_SRE", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	_ = sink.Flush(ctx)
	if !armed() {
		t.Fatal("a second flush after a seal asked for no re-activation")
	}
	if err := sink.Forget(ctx, "DATADOG_APP_KEY_SRE"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !sink.changed {
		t.Fatal("a cleared value left the snapshot marked current")
	}
	_ = sink.Flush(ctx)
	if sink.changed {
		t.Fatal("a flush left the snapshot marked stale")
	}
}

// A SURFACE WITH NO PER-SEAT ACCESS keeps its own pass's answer: the seat half
// is not configured there, which is not a fault.
func TestWithSeatAccessKeepsTheCompanyAnswerWhereNoVendorIsRegistered(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	want := []integration.Finding{{Kind: integration.FindingIngressPending, Detail: "hooks pending"}}
	got, err := e.withSeatAccess(t.Context(), integration.KindJira, setup.PassInput{},
		func(context.Context, setup.PassInput) ([]integration.Finding, error) { return want, nil })
	if err != nil || len(got) != 1 || got[0].Detail != "hooks pending" {
		t.Fatalf("withSeatAccess = %+v, %v; want the company findings as they are", got, err)
	}
	boom := errors.New("vendor unreachable")
	if _, err := e.withSeatAccess(t.Context(), integration.KindJira, setup.PassInput{},
		func(context.Context, setup.PassInput) ([]integration.Finding, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("a company-level fault = %v, want it raised", err)
	}
	if _, err := e.seatAccessPlan(nil, integration.KindJira); !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("seatAccessPlan with no vendor = %v, want ErrNotConfigured", err)
	}
}

// EVERY INTEGRATION THAT MANAGES PER-SEAT ACCESS HAS ITS VENDOR REGISTERED in
// this build, through the packages the engine imports: one left out would
// manage nobody's account while its Agents tab offered every seat.
func TestEveryAccessKindHasARegisteredVendor(t *testing.T) {
	t.Parallel()
	for _, kind := range seataccess.Kinds() {
		vendor, ok := seataccess.Lookup(kind)
		if !ok {
			t.Errorf("%s manages per-seat access and no vendor is registered for it", kind)
			continue
		}
		if got := vendor.Catalog().Kind; got != kind {
			t.Errorf("the %s vendor's catalog names %s", kind, got)
		}
		for _, field := range vendor.Catalog().Credentials {
			if !slices.Contains(config.SeatSealedFields(string(kind)), field) {
				t.Errorf("%s seals %s, which config.SeatSealedFields does not list, so "+
					"the document rules would not know it for a seat's own", kind, field)
			}
		}
	}
}

// ONE WRITER PER SEAT CREDENTIAL: a pass's company half that writes an agent's
// own sealed name is refused, and the pass fails rather than racing the access
// sync for the same key. A company credential passes through, and a dry run's
// nil sink stays nil.
//
// Mutation: hand the company half the pass's own sink, and the seat's key is
// written by two writers again.
func TestAPassHasOneWriterOfASeatsCredential(t *testing.T) {
	t.Parallel()
	company := specCompany(t, provisioningDoc(true))
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)
	sink := newSealingTestSink()

	mintsTheSeats := func(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
		return nil, in.Sink.Record(ctx, "DATADOG_APP_KEY_OPS", "minted-by-the-company-half")
	}
	_, err := e.withSeatAccess(t.Context(), integration.KindDatadog, setup.PassInput{Sink: sink}, mintsTheSeats)
	if !errors.Is(err, errSeatCredentialOwned) || !strings.Contains(err.Error(), "ops") {
		t.Fatalf("withSeatAccess = %v, want the seat's own credential refused, naming the agent", err)
	}
	if v, ok, _ := sink.Value(t.Context(), "DATADOG_APP_KEY_OPS"); ok || v != "" {
		t.Fatal("the company half sealed the agent's credential")
	}

	guarded := e.seatCredentialGuard(integration.KindDatadog, sink)
	if err := guarded.Record(t.Context(), "DATADOG_WEBHOOK_SECRET", "s3cret"); err != nil {
		t.Fatalf("a company credential was refused: %v", err)
	}
	if v, _, _ := sink.Value(t.Context(), "DATADOG_WEBHOOK_SECRET"); v != "s3cret" {
		t.Error("a company credential did not reach the sink")
	}
	if err := guarded.Forget(t.Context(), "DATADOG_APP_KEY_OPS"); !errors.Is(err, errSeatCredentialOwned) {
		t.Errorf("Forget of a seat's credential = %v, want it refused", err)
	}
	if got := e.seatCredentialGuard(integration.KindDatadog, nil); got != nil {
		t.Errorf("a dry run's nil sink became %T, which reads as permission to write", got)
	}
}

// EVERY PASS OF AN INTEGRATION WITH PER-SEAT ACCESS RUNS THROUGH
// [Engine.withSeatAccess], which is what puts the guard between its company
// half and the sink. A pass that called its company half directly would be a
// second writer nobody refuses.
func TestEveryAccessPassRunsThroughTheSeatAccessWrapper(t *testing.T) {
	t.Parallel()
	want := map[string]integration.Kind{}
	for _, pass := range (&Engine{}).setupPasses() {
		if _, ok := seataccess.Lookup(pass.Kind()); ok {
			want[reflect.TypeOf(pass).Elem().Name()] = pass.Kind()
		}
	}
	if len(want) < len(seataccess.Kinds()) {
		t.Fatalf("passes for %v, fewer than the %d access kinds", want, len(seataccess.Kinds()))
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	wrapped := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Run" || fn.Body == nil {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			recv, ok := star.X.(*ast.Ident)
			if !ok {
				continue
			}
			if _, access := want[recv.Name]; !access {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "withSeatAccess" {
					wrapped[recv.Name] = true
				}
				return true
			})
		}
	}
	for pass, kind := range want {
		if !wrapped[pass] {
			t.Errorf("%s (%s) has per-seat access and its Run does not go through "+
				"withSeatAccess, so its company half writes unguarded", pass, kind)
		}
	}
}

// AN INTEGRATION WHOSE CREDENTIALS ARE HANDED IN IS NEVER SYNCED AT THE VENDOR
// (D4b): the engine does not hold the authority there, so the sync opens no
// session, writes no ledger record, and says once that the agents' accounts
// are managed outside Crewlet. The same company on a deployment the engine
// writes is synced. GitHub on a managed deployment is the case: its apps are
// made outside Crewlet whatever the block says.
//
// Mutation: drop the mode gate, and the managed deployment's sync reaches the
// ledger and the vendor.
func TestAnExternalIntegrationIsNeverSyncedAtTheVendor(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
seat_access_migrated: [github]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  github:
    enabled: true
    webhook_secret: "whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM="
    provisioning: {org: acme}
roles:
  - name: Builder
    handle: builder
    llm: zulu
    integrations:
      github: {enabled: true}
`)
	// NO FLEET STORE: a sync that got past the gate would fail reading the
	// ledger, which is how the test tells the two apart without a vendor.
	managed := &Engine{companyManaged: true}
	managed.epoch.current.Store(company)
	report, err := managed.syncSeatAccess(t.Context(), integration.KindGitHub, seatSync{})
	if err != nil {
		t.Fatalf("a managed deployment's GitHub sync = %v, want it answered without the vendor", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingCoveragePartial ||
		!strings.Contains(report.Findings[0].Title, "managed outside Crewlet") || len(report.Records) != 0 {
		t.Fatalf("report = %+v, want the one managed-outside finding and no record", report)
	}
	written := &Engine{}
	written.epoch.current.Store(company)
	if _, err := written.syncSeatAccess(t.Context(), integration.KindGitHub, seatSync{}); err == nil ||
		errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("an engine-written deployment's GitHub sync = %v, want it to reach the ledger", err)
	}
	// AN INTEGRATION THE COMPANY DOES NOT CONNECT is not configured, and
	// says nothing.
	if _, err := managed.syncSeatAccess(t.Context(), integration.KindDatadog, seatSync{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("an unconnected integration's sync = %v, want it not configured", err)
	}
}

// ONLY A CONNECTED INTEGRATION WAITS ON THE MIGRATION, AND IT SAYS WHY.
//
// The company connects Jira (so Atlassian) and not Datadog. Datadog's pass
// reports nothing about a migration: it is not configured, and there is
// nothing for it to wait on. Atlassian's pass names Atlassian, says its access
// migration has not finished, and gives the reason the migration's last attempt
// here left; once an attempt moves it, the reason is forgotten. Mutation: gate
// on the marker alone, or drop the reason, and a case here fails.
func TestOnlyAConnectedIntegrationReportsTheUnfinishedMigration(t *testing.T) {
	t.Parallel()
	e, c := sealingEngine(t, "https://jira.example.com")
	e.epoch.current.Store(c)
	waiting := func(r seataccess.SyncReport) []string {
		var said []string
		for _, f := range r.Findings {
			if strings.Contains(f.Detail, "access migration has not finished") {
				said = append(said, f.Detail)
			}
		}
		return said
	}

	report, err := e.syncSeatAccess(t.Context(), integration.KindDatadog, seatSync{})
	if err != nil && !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("an unconnected integration's sync = %v", err)
	}
	if said := waiting(report); len(said) != 0 {
		t.Errorf("Datadog, which the company does not connect, reports %q", said)
	}

	why := "Atlassian did not answer when its agents' accounts were read: 503 Service Unavailable"
	e.rememberSeatAccessPending(accessmigrate.Result{
		Pending: map[integration.Kind]error{integration.KindAtlassian: errors.New(why)}})
	report, err = e.syncSeatAccess(t.Context(), integration.KindAtlassian, seatSync{})
	if said := waiting(report); err != nil || len(said) != 1 ||
		!strings.HasPrefix(said[0], "Atlassian's access migration") || !strings.Contains(said[0], why) {
		t.Fatalf("Atlassian's pass said %q, %v; want its unfinished migration with the reason", said, err)
	}

	e.rememberSeatAccessPending(accessmigrate.Result{
		Migrated: []integration.Kind{integration.KindAtlassian}})
	if got := e.seatAccessPendingReason(integration.KindAtlassian); got != "" {
		t.Errorf("a moved integration still waits on %q", got)
	}
}
