package gitlab_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/provision"
)

// The GitLab half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in seven clauses and its package doc
// calls the write-counting hook required, because that clause is "the one
// most likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened — a green test over a world nobody had built and a reconciler
// nobody had run. This points the same seven cases at [gitlab.Reconcile],
// over a GitLab that has already been brought into line, and counts what the
// instance receives.
//
// # Both hook paths, because the pass has two
//
// [gitlab.Reconcile] registers ONE hook on the group where the tier serves
// group webhooks, and one hook per `provisioning.projects` entry where it
// does not — and the per-project branch multiplies whatever it does by the
// project count. A harness pinned to the group path certifies half the
// surface, so the suite is driven over both worlds.

// convergedWorld is a GitLab this pass has already converged, plus the pass
// itself as [integration.Reconciler] sees it.
type convergedWorld struct {
	instance *adminInstance
	opts     gitlab.Options
}

// Kind names the surface. [integration.KindGitLab] is a constant rather than
// anything derived, which is what makes the suite's stability clause a real
// assertion about the adapter rather than about a field.
func (*convergedWorld) Kind() integration.Kind { return integration.KindGitLab }

// Reconcile runs the real pass and maps its answer the way the loop's own
// adapter does.
//
// IT MIRRORS internal/engine's gitlabPass.Run DELIBERATELY, including the
// [gitlab.ErrNameReserved] arm — which lives in the engine rather than in
// this package. A harness that skipped it would certify a slightly different
// reconciler from the one the loop drives, and the clause it would get wrong
// is the one about a fault versus a finding.
func (w *convergedWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := gitlab.Reconcile(ctx, w.opts)
	if errors.Is(err, gitlab.ErrNameReserved) {
		return []integration.Finding{{
			Kind: integration.FindingIdentityMissing,
			Detail: "GitLab is still releasing the name of an account it is " +
				"deleting, so this seat's account cannot be created yet: " +
				"the next pass makes it, usually within a minute",
		}}, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// convergedGitLab stands the world up and converges it BY RUNNING THE PASS.
//
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-written fixture that seeds the accounts,
// memberships and hook a converged company has. It is the tempting one and it
// is weaker in the direction that matters: it encodes what the harness author
// BELIEVES a converged GitLab looks like, so a pass whose idea of converged
// has drifted from that belief writes on every run and the fixture calls it
// converged anyway. Running the real pass makes the world converged by
// definition, and then the only question left is the one worth asking —
// whether the SECOND pass writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after New returns.
//
// TWO SEATS AND TWO PROJECTS, because every write this pass made on a
// converged world was per-seat or per-seat-per-project. One of each would
// have counted 1 where the bug's shape is N and N×M, and a harness cannot
// tell "wrote once" from "wrote once per seat" with one seat.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T of the subtest that owns this world.
func convergedGitLab(t *testing.T, tb integrationtest.TB, tune func(*adminInstance)) *convergedWorld {
	t.Helper()
	f := newAdminInstance()
	if tune != nil {
		tune(f)
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	// THE SERVER'S OWN CLIENT, whose transport dies with it — see
	// reconcileWith for why a shared pool breaks parallel tests.
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}

	cfg := enabledGitLab()
	cfg.Provisioning.Projects = []string{"nimbus/api", "nimbus/web"}
	plan := &provision.Plan{}
	for _, seat := range []struct{ handle, tokenVar string }{
		{"swe", "GITLAB_TOKEN_SWE"},
		{"cto", "GITLAB_TOKEN_CTO"},
	} {
		plan.Add(provision.Seat{
			Handle: seat.handle, Role: strings.ToUpper(seat.handle),
			TokenVar: seat.tokenVar,
			Email:    seat.handle + "@noreply.crewlet.invalid",
		})
	}
	sink := newRecordingSink()
	world := &convergedWorld{instance: f, opts: gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		// EVERY ONE OF THESE THE WAY THE ENGINE SETS IT. Each has a
		// setting that makes the pass do LESS than the loop really does,
		// and each would make the converged clause pass for the wrong
		// reason: an empty WebhookBase touches no hook at all, empty
		// Projects adds no project memberships, and a sink where
		// provision.CanMint is false short-circuits the whole pass to
		// NoKeyring without one request. recordingSink has no Mints
		// method, so CanMint answers true.
		WebhookBase: "https://crewlet.example.com", SigningSecret: testSigningSecret,
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}}

	findings, err := world.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that converges the world failed: %v", err)
	}
	if len(findings) != 0 {
		tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
	}
	// AND IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing
	// would leave every case below true about an empty GitLab, which is
	// the exact shape of vacuous pass this file exists to replace.
	if f.mutations() == 0 {
		tb.Fatalf("the seeding pass wrote nothing, so there is no converged " +
			"world here to certify a second pass against")
	}
	for _, name := range []string{"GITLAB_TOKEN_SWE", "GITLAB_TOKEN_CTO"} {
		if sink.value(name) == "" {
			tb.Fatalf("%s holds no token after the seeding pass, so the second "+
				"pass would mint rather than keep", name)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.groupMembers) != 2 {
		tb.Fatalf("group roster = %v, want both seats", f.groupMembers)
	}
	if len(f.hooks)+len(f.projectHooks) == 0 {
		tb.Fatalf("%s", "no webhook was registered, so the hook half of the pass "+
			"is not being exercised")
	}
	return world
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER.
func TestTheGitLabReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, world := range []struct {
		name string
		tune func(*adminInstance)
	}{
		// The Premium / self-managed shape: one hook on the group.
		{"one hook on the group", nil},
		// And the free tier, where a group webhook is accepted and never
		// delivered, so the pass registers one hook per declared project
		// — a branch whose every write is multiplied by the project
		// count.
		{"one hook per project", func(f *adminInstance) { f.plan = "free" }},
	} {
		t.Run(world.name, func(t *testing.T) {
			t.Parallel()
			// Assigned by New and read by Mutations, which the suite
			// calls in that order within one sequential case.
			var instance *adminInstance
			integrationtest.Run(t, integrationtest.Reconciler{
				New: func(tb integrationtest.TB) integration.Reconciler {
					w := convergedGitLab(t, tb, world.tune)
					instance = w.instance
					return w
				},
				Mutations: func() int { return instance.mutations() },
			})
		})
	}
}
