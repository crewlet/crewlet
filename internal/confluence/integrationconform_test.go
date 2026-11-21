package confluence_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
)

// The Confluence half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop runs, over instances the pass has already
// brought into line (a stub returning (nil, nil) made "a converged pass writes
// nothing" hold because nothing happened).
//
// A converged Confluence reports NO findings, so cases walking the findings
// list would assert nothing; [integrationtest.Reconciler.Outstanding] is an
// instance that REFUSES A REGISTRATION (403: an account that cannot administer
// webhooks), reported as [integration.FindingIngressBlocked]. It cannot be a
// refused credential: the identity probe runs first and [integration.Observe]
// turns its error into FindingCredentialRejected before any findings list.

// world is a Confluence instance plus the pass itself as
// [integration.Reconciler] sees it. Converged or outstanding is a property of
// the instance, not of this type.
//
// Both deployments are driven: Reconcile forks on the CLIENT's deployment, and
// a 127.0.0.1 address reads as Data Center. A write counts by ROUTE and
// includes every [provision.TokenSink] Record (on Data Center the only sign of
// a pass re-minting the never-listed secret). Mid-walk cancellation, which the
// identity probe hides, is pinned by the TestACancelled* tests in hooks_test.go
// and TestAPassWithNoBaseOnADeadContextIsAFaultRatherThanHealth.
type world struct {
	site *cloudSite
	sink *countingSink
	opts confluence.Options
}

// Kind names the surface. A constant rather than anything derived from the
// options, which is what makes the suite's stability clause an assertion
// about the adapter rather than about a field.
func (*world) Kind() integration.Kind { return integration.KindConfluence }

// Reconcile runs the real pass and maps its answer the way the loop's own
// adapter does.
//
// IT MIRRORS internal/engine's confluencePass.Run DELIBERATELY: a pass that
// returned an error is a fault and carries no findings, and a pass that
// returned is exactly res.Findings(). The one thing it does not mirror is how
// the client is built, and that is not a choice — the engine's adapter never
// sets ClientOptions.Deployment, so against a 127.0.0.1 test server it can
// only ever reach the Data Center branch.
func (w *world) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := confluence.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// mutations is what this world has received, at the instance and at the
// sealed store alike.
func (w *world) mutations() int { return w.site.mutations() + w.sink.writes() }

// countingSink is [recordingSink] that also counts what it was asked to seal.
//
// A SEALED VALUE IS A WRITE, on the suite's own terms: "anything a person
// would have to undo" is wider than a request to the third-party app, and a
// pass that re-mints on every run is the exact failure the converged clause
// exists to catch. Counting only the instance would miss it in the direction
// that matters, because a fresh token is sealed BEFORE the hooks that carry
// it are re-registered.
type countingSink struct {
	*recordingSink
	mu      sync.Mutex
	records int
}

func (s *countingSink) Record(ctx context.Context, name, value string) error {
	s.mu.Lock()
	s.records++
	s.mu.Unlock()
	return s.recordingSink.Record(ctx, name, value)
}

func (s *countingSink) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records
}

// newWorld builds the pass's own options against a site, for one deployment.
//
// It runs nothing: what makes a world converged or outstanding is what the
// site does to the passes that follow, and both callers below need this same
// wiring first.
//
// THE VALUE CLOSURE READS THE SINK, which is what a restarted engine does:
// the first pass mints into the config's ${VAR} and seals it, and every pass
// after it resolves that same value. A closure that answered empty would have
// the pass mint afresh and re-point every hook — the single most likely way
// to make this harness fail for a reason that is about the harness.
func newWorld(t *testing.T, site *cloudSite, deployment confluence.Deployment) *world {
	t.Helper()
	sink := &countingSink{recordingSink: newSink()}

	var (
		client   *confluence.Client
		cfg      *config.Confluence
		variable = "CONFLUENCE_WEBHOOK_TOKEN"
	)
	if deployment == confluence.Cloud {
		client = cloudClient(t, site)
		cfg = &config.Confluence{
			URL: site.URL + "/wiki", Token: "t",
			WebhookToken: "${CONFLUENCE_WEBHOOK_TOKEN}",
		}
	} else {
		client = dataCenterClient(t, site)
		variable = "CONFLUENCE_WEBHOOK_SECRET"
		cfg = &config.Confluence{
			URL: site.URL, Token: "t",
			WebhookSecret: "${CONFLUENCE_WEBHOOK_SECRET}",
		}
	}
	ref := "${" + variable + "}"

	return &world{site: site, sink: sink, opts: confluence.Options{
		Client: client,
		// UNRESOLVED, as the engine passes it: a minted value goes INTO
		// this ${VAR}, so the reference has to survive to be minted into.
		Config: cfg,
		Value: func(v string) string {
			if v != ref {
				return v
			}
			held, _, _ := sink.Value(context.Background(), variable)
			return held
		},
		Sink: sink,
		// A NON-EMPTY BASE, because an empty one registers nothing and
		// reports nothing: every case below would then be true of a pass
		// that never looked at a hook.
		WebhookBase: "https://engine.example.com",
	}}
}

// convergedConfluence stands an instance up and converges it BY RUNNING THE
// PASS.
//
// A hand-seeded listing encodes what the author BELIEVES converged looks like;
// any field it gets wrong makes the pass write every run. Running the real
// pass makes it converged by definition. Its writes are not counted:
// [integrationtest.Run] baselines Mutations() after Converged returns.
//
// BOTH t AND tb: [integrationtest.TB] has no Cleanup, so tb reports the case
// while the server's lifetime hangs off the subtest's own *testing.T.
func convergedConfluence(
	t *testing.T, tb integrationtest.TB, deployment confluence.Deployment,
) *world {
	t.Helper()
	w := newWorld(t, newCloudSite(t), deployment)

	findings, err := w.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that converges the world failed: %v", err)
	}
	if len(findings) != 0 {
		tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
	}
	// AND IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing
	// would leave every case below true about an empty instance, which is
	// the exact shape of vacuous pass this file exists to replace.
	if w.site.mutations() == 0 {
		tb.Fatalf("the seeding pass wrote nothing, so there is no converged " +
			"world here to certify a second pass against")
	}
	want := hooksPerPass(deployment)
	if got := len(w.site.snapshot()); got != want {
		tb.Fatalf("the seeding pass left %d hook(s) registered, want %d", got, want)
	}
	if w.sink.writes() == 0 {
		tb.Fatalf("the seeding pass sealed nothing, so the next pass cannot " +
			"demonstrate that it keeps a value rather than minting another")
	}
	return w
}

// outstandingConfluence stands up an instance that REFUSES one event's
// registration, and converges everything else about it by running the pass.
//
// The credential can read the instance but may not administer webhooks for
// that event: [integration.FindingIngressBlocked], owed by an ADMIN. The pass
// runs once here so the suite meets the steady state: an un-run world would
// register seven hooks on the first pass and none on the second, and the churn
// clause would assert about fresh versus converged. A pass over it MAY write.
func outstandingConfluence(
	t *testing.T, tb integrationtest.TB, deployment confluence.Deployment,
) *world {
	t.Helper()
	site := newCloudSite(t)
	// The refusal is in place BEFORE the first pass, so no pass ever sees
	// this event registered and there is nothing for a later one to find.
	site.refuse = refusedEvent
	w := newWorld(t, site, deployment)

	findings, err := w.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that stands the outstanding world up failed: %v", err)
	}
	// THE WORLD PROVES ITSELF, and specifically: for the reason claimed.
	// The suite's own anti-vacuity clause asks only that SOMETHING is
	// reported, which a site that had started refusing every request would
	// also satisfy — leaving three cases green over a fixture that broke in
	// a way nobody chose. So the shape is asserted here, where the failure
	// names the fixture rather than the vendor.
	if got, want := len(findings), 1; got != want {
		tb.Fatalf("the outstanding world reported %d finding(s), want %d: %+v",
			got, want, findings)
	}
	if findings[0].Kind != integration.FindingIngressBlocked {
		tb.Fatalf("the outstanding world reports %q rather than a blocked "+
			"ingress, so it is outstanding for a reason this harness did not "+
			"choose: %+v", findings[0].Kind, findings[0])
	}
	if want := blockedSubject(deployment); findings[0].Subject != want {
		tb.Fatalf("the blocked ingress names %q rather than %q",
			findings[0].Subject, want)
	}
	// AND EVERYTHING ELSE IS CONVERGED, which is what makes this the
	// steady state rather than a broken instance: on Cloud the other seven
	// events are hooked, on Data Center the single hook is the refused one
	// and there is nothing else to hook.
	if got, want := len(w.site.snapshot()), hooksPerPass(deployment)-1; got != want {
		tb.Fatalf("the outstanding world holds %d hook(s), want %d — every "+
			"event but the refused one", got, want)
	}
	return w
}

// refusedEvent is the event the outstanding world's instance will not let
// this credential register.
//
// ONE KNOB FOR BOTH DEPLOYMENTS. A Cloud registration names exactly this
// event and is refused alone, leaving the other seven hooked; Data Center
// registers every event in a single call, which therefore SUBSCRIBES to this
// one and is refused whole. That asymmetry is the integration's own, and it
// is why the finding each branch writes says something different about what
// was lost.
const refusedEvent = "page_created"

// hooksPerPass is how many registrations a fully converged instance of this
// deployment holds: one per event on Cloud, one signed hook for all of them
// on Data Center.
func hooksPerPass(deployment confluence.Deployment) int {
	if deployment == confluence.Cloud {
		return len(confluence.WebhookEvents)
	}
	return 1
}

// blockedSubject is what the refused registration's finding is ABOUT, and the
// two deployments lose different amounts: one event class on Cloud, every
// event on Data Center, whose single hook carries all of them.
func blockedSubject(deployment confluence.Deployment) string {
	if deployment == confluence.Cloud {
		return refusedEvent
	}
	return "all"
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER, on both deployments.
func TestTheConfluenceReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, deployment := range []confluence.Deployment{confluence.Cloud, confluence.DataCenter} {
		t.Run(string(deployment), func(t *testing.T) {
			t.Parallel()
			// Assigned by Converged and read by Mutations, which the suite
			// calls in that order within one sequential case. A Mutations
			// bound to a world captured before the last Converged would
			// measure a world no case is running against — and it is bound
			// to the CONVERGED world alone, because that is the only one
			// the suite samples it around. Nothing counts what a pass over
			// the outstanding world writes, and it writes.
			var converged *world
			integrationtest.Run(t, integrationtest.Reconciler{
				Converged: func(tb integrationtest.TB) integration.Reconciler {
					converged = convergedConfluence(t, tb, deployment)
					return converged
				},
				Outstanding: func(tb integrationtest.TB) integration.Reconciler {
					return outstandingConfluence(t, tb, deployment)
				},
				Mutations: func() int { return converged.mutations() },
			})
		})
	}
}

// A ROUTE THE COUNTER TREATS AS READ-ONLY REFUSES A WRITE.
//
// [cloudSite.writes] counts by ROUTE (integrationtest's package doc: some apps
// model a listing as a POST), so a route counted as read-only has to BE
// read-only. The identity route matched on the path alone, so a mutating call
// there would be served as a read and counted as NOTHING, the one place in
// this fake where a future write would be invisible.
func TestARouteTheCounterTreatsAsReadOnlyRefusesAWrite(t *testing.T) {
	t.Parallel()
	site := newCloudSite(t)
	before := site.mutations()

	resp, err := site.Client().Post(
		site.URL+"/wiki/rest/api/user/current", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Error("the identity route served a write as a read, so a pass that " +
			"started writing there would make no mark on the counter the " +
			"contract's load-bearing clause is measured with")
	}
	if got := site.mutations(); got != before {
		t.Errorf("a refused request was counted as %d write(s)", got-before)
	}
}

// THE OUTSTANDING WORLD IS THE STEADY STATE, NOT A ONE-OFF.
//
// The suite's cases pass over a fresh world at most twice, but the loop
// passes over this instance every few minutes for the deployment's life, and
// two failures are invisible in two passes: one that quietly stops reporting
// the block, and one that re-registers the seven working hooks each time it
// meets the refused one. Here because it is this vendor's own risk: the Cloud
// walk records a refusal per event and carries on, so "keeps going" and
// "keeps reporting" are different claims.
func TestTheOutstandingWorldKeepsReportingAndStaysQuiet(t *testing.T) {
	t.Parallel()
	for _, deployment := range []confluence.Deployment{confluence.Cloud, confluence.DataCenter} {
		t.Run(string(deployment), func(t *testing.T) {
			t.Parallel()
			w := outstandingConfluence(t, t, deployment)
			// The refused registration is retried on every pass — it is
			// the one thing this pass still has work to do about — so what
			// must not grow is the writes to everything ELSE.
			before := w.site.mutations() + w.sink.writes()
			var attempts int

			for pass := range 5 {
				findings, err := w.Reconcile(context.Background())
				if err != nil {
					t.Fatalf("pass %d: %v", pass+2, err)
				}
				if len(findings) != 1 ||
					findings[0].Kind != integration.FindingIngressBlocked {
					t.Fatalf("pass %d stopped reporting the block: %+v",
						pass+2, findings)
				}
				attempts++
			}

			// Every pass retries the one refused registration, and that
			// attempt is refused before the site counts it. Anything else
			// — a re-pointed hook, a re-minted secret — would show up
			// here, which on Data Center is the only place it could: its
			// signing secret never appears in a listing.
			if got := w.site.mutations() + w.sink.writes(); got != before {
				t.Errorf("%d pass(es) over an outstanding world made %d write(s) "+
					"beyond the refused registration itself", attempts, got-before)
			}
			if got, want := len(w.site.snapshot()), hooksPerPass(deployment)-1; got != want {
				t.Errorf("the instance holds %d hook(s) after %d more pass(es), want %d",
					got, attempts, want)
			}
		})
	}
}

// A HARNESS THAT CANNOT SEE A WRITE CERTIFIES NOTHING, so the counter is
// checked against a pass that is KNOWN to write.
//
// [integrationtest.Reconciler.Mutations] is the hook the suite's package doc
// calls the one most likely to be wrong, and the way it goes wrong is silence:
// a counter wired to the wrong site, or reading a field nothing increments,
// reports zero for ever and turns the load-bearing clause into a tautology.
// The suite cannot catch that — a broken counter passes its every case.
//
// So: a pass over a fresh instance must move it, and by a knowable amount —
// one seal, plus one registration per hook the deployment holds.
func TestTheHarnessCounterSeesTheWritesAPassMakes(t *testing.T) {
	t.Parallel()
	for _, deployment := range []confluence.Deployment{confluence.Cloud, confluence.DataCenter} {
		t.Run(string(deployment), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, newCloudSite(t), deployment)
			if got := w.mutations(); got != 0 {
				t.Fatalf("an instance nothing has passed over reports %d write(s)", got)
			}

			if _, err := w.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}

			seal, hooks := 1, hooksPerPass(deployment)
			if got, want := w.mutations(), seal+hooks; got != want {
				t.Errorf("the harness counted %d write(s) for a pass that sealed "+
					"one value and registered %d hook(s), want %d", got, hooks, want)
			}
			// THE TWO HALVES SEPARATELY, because one silent half is the
			// failure: on Data Center the sealed secret takes no part in
			// any listing and a pass that re-minted it would touch the
			// instance not at all.
			if got := w.sink.writes(); got != seal {
				t.Errorf("the sealed value was counted %d time(s), want %d", got, seal)
			}
			if got := w.site.mutations(); got != hooks {
				t.Errorf("the instance counted %d write(s), want %d", got, hooks)
			}
		})
	}
}
