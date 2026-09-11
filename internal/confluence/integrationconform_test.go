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

// The Confluence half of [integration.Reconciler]'s safety contract,
// certified against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in nine clauses and its package doc
// calls the write-counting hook REQUIRED, because that clause is "the one
// most likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened — a green test over a world nobody had built and a reconciler
// nobody had run. This points the cases at [confluence.Reconcile], over
// instances the pass has already brought into line, and counts what they
// receive.
//
// # TWO WORLDS, because a converged one cannot certify half the clauses
//
// A converged Confluence reports NO findings — [confluence.Result.Findings]
// only emits for a hook that could not be established — so every case that
// walks the findings list walked an empty one and asserted nothing about this
// integration. That was measured rather than argued: overlaying the suite
// with a "this findings list is empty" check reddened three cases on BOTH
// deployments. The contract's own count was seven clauses certified where
// four carried weight.
//
// So [integrationtest.Reconciler.Outstanding] is a world where the instance
// REFUSES A REGISTRATION — a 403 from /rest/webhooks/1.0/webhook, which is
// what an account without the permission to administer webhooks gets — and
// the pass reports [integration.FindingIngressBlocked]: degraded, owed by the
// administrator who can grant that permission, and a sentence naming the
// event class that now reaches nobody.
//
// It is a registration rather than the other obvious choice, a credential the
// instance refuses, and that is forced rather than preferred: Confluence's
// pass probes its identity FIRST and returns an error when that is refused,
// because nothing else it went on to report would be trustworthy. The spine
// turns that error into [integration.FindingCredentialRejected] itself, in
// [integration.Observe], so it never reaches a reconciler's findings list and
// cannot be the world here — the suite's cases would fail on the error before
// they read anything. A refused registration is the one genuinely outstanding
// condition this pass reports in the shared vocabulary.
//
// # Both deployments, because the pass has two
//
// [confluence.Reconcile] forks on the client's deployment and the two halves
// are different code with different converged predicates: Cloud walks
// [confluence.WebhookEvents] and keeps a hook per event, Data Center keeps
// the single signed hook named crewlet:all. A harness pinned to one certifies
// half the surface, so the suite is driven over both. The fork is on the
// CLIENT rather than on any address, which is why each world builds its own —
// an httptest address is 127.0.0.1 and reads as Data Center, so the Cloud
// branch is only reachable from a client that says so.
//
// # What counts as a write here, and why it is counted by route
//
// Two things a person would have to undo, and the suite's package doc names
// both: a mutation at the instance, and a value sealed into this
// deployment's own store.
//
//   - At the instance, the mutating routes are POST
//     /rest/webhooks/1.0/webhook (register) and PUT and DELETE on
//     /rest/webhooks/1.0/webhook/{id} (re-point, remove). GET on either, and
//     GET /rest/api/user/current, are reads and are not counted. Counted by
//     ROUTE rather than by method because the suite says so and because the
//     distinction is real elsewhere in this org's own APIs — Atlassian models
//     workspace discovery as a POST that discovers rather than writes — so a
//     method-keyed counter is a habit that transfers wrongly. The price of
//     that rule is paid in the fake: a route counted as read-only has to
//     REFUSE a write rather than serve it, which is why its identity arm
//     answers 405 to anything but a GET.
//   - At this deployment, every [provision.TokenSink] Record. That is the
//     one this integration most needs counted, and on Data Center it is the
//     ONLY one that can see the failure: the signing secret is never returned
//     by a listing and takes no part in the converged comparison, so a pass
//     that re-minted it would rotate the key the engine verifies deliveries
//     with while touching nothing at the instance at all.
//
// # What the cancellation clause certifies HERE, and what it does not
//
// "A cancelled pass reports a fault rather than health" hands the pass a
// context that is already dead, so for this vendor it is answered by
// [confluence.Client.Me] — the identity probe is the pass's first request and
// it fails outright. That IS the contract's claim and it holds, but it means
// the suite never reaches the fold that makes a pass cut short MID-WALK a
// fault. Nothing here would notice if that fold were deleted.
//
// That half is pinned beside this file instead, by name, because it needs a
// context that dies at a chosen point rather than before the first request:
// TestACancelledCloudPassOverAConvergedSiteReportsAFaultRatherThanHealth,
// TestACancelledCloudPassDoesNotBlameTheInstance,
// TestACancelledDataCenterPassOverAConvergedInstanceReportsAFault and
// TestAPassWithNoBaseOnADeadContextIsAFaultRatherThanHealth.

// world is a Confluence instance plus the pass itself as
// [integration.Reconciler] sees it. Converged or outstanding is a property of
// the instance, not of this type.
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
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-seeded listing carrying the eight hooks a
// converged Cloud site holds. It is the tempting one and it is weaker in the
// direction that matters: it encodes what the harness author BELIEVES
// converged looks like, and every field the belief gets wrong — an id that
// is only ever read off the tail of `self`, a token query the seeder wrote in
// a different order, an `enabled` nobody thought to set — makes the pass
// write on every run while the fixture calls the world converged. Running the
// real pass makes the world converged by definition, and leaves only the
// question worth asking: whether the SECOND pass writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after Converged returns.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T of the subtest that owns this world.
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
// # Why this condition, out of everything that can be wrong
//
// It is the one an operator most needs told and the engine cannot fix: the
// org credential can read the instance (so the pass runs and reports rather
// than faulting) and may not administer webhooks for that event (so the
// delivery path is blocked until somebody with the permission grants it).
// That is [integration.FindingIngressBlocked] exactly — degraded, owed by an
// ADMIN — and the finding carries the instance's own refusal so the person
// reading it knows what to grant.
//
// # Steady-state outstanding, not first-pass outstanding
//
// The pass is run once here, so the world the suite's cases meet is the one
// the loop actually sits in: everything that CAN be hooked is hooked, and the
// same finding comes back every few minutes for as long as nobody acts. A
// world handed over un-run would have its first pass register seven hooks and
// its second register none, which is a world in flux rather than an
// outstanding one — and the churn clause would be asserting about the
// difference between a fresh instance and a converged one rather than about
// findings that flap.
//
// The refusal is not counted anywhere and does not need to be: a pass over
// this world is ALLOWED to write, because it has work to do.
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
// [cloudSite.writes] counts by ROUTE, which is the rule integrationtest's
// package doc sets and which this file's own doc restates: some third-party
// apps model a listing as a POST, so a method-keyed counter makes the
// converged-pass-writes-nothing clause impossible to satisfy.
//
// The price is that a route counted as read-only has to actually BE read-only.
// The identity route matched on the path alone, so a mutating call there would
// have been served as a read and counted as NOTHING — the same hole the
// route-keyed rule exists to close, inverted, and the one place in this fake
// where a future write would be invisible rather than merely miscounted.
// Nothing in the pass does that today; this is what keeps it that way.
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
// The suite's own cases would be satisfied by a world whose finding appears
// once: each of them builds a fresh world and passes over it at most twice.
// What the loop actually does is pass over this instance every few minutes
// for the life of the deployment, and the two failures that matter are
// invisible in two passes — a pass that quietly gives up reporting the block,
// and one that re-registers the seven working hooks each time it meets the
// refused one.
//
// Here rather than in the suite because it is this vendor's own risk: the
// Cloud walk records a refusal per event and carries on, so "the pass keeps
// going" and "the pass keeps reporting" are two different claims about the
// same loop.
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
