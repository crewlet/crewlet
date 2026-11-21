package jira_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/org"
)

// Where the certification suite binds to the Jira reconcile.
//
// [integrationtest] states [integration.Reconciler]'s safety contract as nine
// cases. This file drives the real [jira.Reconcile] against a real instance in
// the state the loop spends its life in, counting every request to undo.
//
// # TWO WORLDS, because the converged one cannot certify half the suite
//
// Four cases walk the findings list, and a converged pass reports none: over
// the steady state alone they pass whatever this package does.
// [convergedWorld] serves the write-free clause; [outstandingWorld] is a Jira
// somebody has to fix, which the findings cases read.

// convergedBase is the address this deployment is reachable on.
const convergedBase = "https://engine.example.com"

// conformer drives the REAL reconcile through [integration.Reconciler].
//
// TEST-ONLY: the loop reaches this package through engine.jiraPass, so a
// production adapter here would have no caller. Its body is jiraPass.Run's
// from the point inputs are resolved: [jira.Reconcile], then
// [jira.Result.Findings], and on failure the same `engine: jira pass: %w` wrap.
//
// NOT mirrored, so not certified: everything jiraPass does BEFORE that
// (jiraBaseURL and `integrations.jira.token` resolution,
// [integration.ErrNotConfigured], [integration.FindingCredentialMissing]).
// [conformer.Kind] returns the constant jiraPass.Kind() does, so "the kind is
// one this build converges" certifies a constant, not product code.
type conformer struct{ opts jira.Options }

func (*conformer) Kind() integration.Kind { return integration.KindJira }

func (c *conformer) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := jira.Reconcile(ctx, c.opts)
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	return res.Findings(), nil
}

// convergedCompany is an org with nothing outstanding at the instance: every
// non-human seat holds a credential the instance answers for, and the one
// project it names exists.
//
// NOT company(). That fixture is deliberately UNCONVERGED — a seat whose
// token the instance refuses and a seat with no credential at all are the
// whole point of it — and a suite run over it would certify a world that
// reports two identity_failed findings as the steady state, which is a
// different claim from the one being made here.
func convergedCompany() *org.Organization {
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			// One credential under Atlassian's own combined server name
			// and one under a Jira-only one, because both are real
			// configs and a walk that knew one name would report the
			// other seat as holding nothing.
			{Name: "Eng Lead", DeclaredHandle: "lead", Project: "ENG",
				MCPEnv: map[string]map[string]string{
					"atlassian": {"JIRA_API_TOKEN": "lead-token"},
				}},
			// No project of its own: two seats naming one project is an
			// ambiguous lead, which is a real thing to report and a
			// different fixture from this one.
			{Name: "SWE", DeclaredHandle: "swe",
				MCPEnv: map[string]map[string]string{
					"jira": {"JIRA_TOKEN": "swe-token"},
				}},
			// A HUMAN SEAT holds no tool credential and must never be
			// looked up as though it did, so a converged world contains
			// one: a walk that probed it would report an unreachable
			// seat over a company that is fine.
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{AtlassianAccountID: acctFounder}},
		},
	}
	o.Normalize()
	return o
}

// convergedWorld stands up an instance this pass has nothing to do to, and
// the options that read it. Every knob that decides convergence is set
// explicitly, since each fails in a direction that reads as the product's:
//
//   - The seeded hook carries its own name, address and events; the fake
//     defaults all three and would agree with anything.
//   - Value resolves the signing secret, or the pass mints and re-registers.
//   - WebhookBase is set, or the webhook half is skipped and every clause
//     passes VACUOUSLY. RecreateWebhook stays unasked.
//
// The DEPLOYMENT is left to [jira.DeploymentOf], as production does
// (engine.jiraPass sets none); assertConverged asserts Data Center.
func convergedWorld(t *testing.T) (*instance, *sink, jira.Options) {
	t.Helper()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	inst.accounts["Bearer lead-token"] = acctLead
	inst.accounts["Bearer swe-token"] = "acct-swe"
	inst.projects["ENG"] = "Engineering"
	inst.hooks = []map[string]any{{
		"id":      "7",
		"name":    jira.DefaultWebhookName,
		"url":     convergedBase + "/webhooks/jira",
		"enabled": true,
		// CLONED: WebhookEvents is an exported slice, and a fixture that
		// handed the instance the engine's own backing array would let
		// one edit reach both sides of the comparison.
		"events": slices.Clone(jira.WebhookEvents),
	}}

	client, err := jira.NewClient(jira.ClientOptions{URL: inst.URL, Token: "org-token"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := newSink()
	return inst, recorder, jira.Options{
		Client: client,
		Config: &config.Jira{
			URL: inst.URL, Token: "${JIRA_TOKEN}",
			WebhookSecret: "${JIRA_WEBHOOK_SECRET}",
		},
		Org: convergedCompany(),
		Value: func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "the-live-secret"
			}
			return v
		},
		Sink:        recorder,
		WebhookBase: convergedBase,
	}
}

// assertConverged refuses a fixture that is not the steady state.
//
// Every suite clause is satisfied by a pass never given anything to do (empty
// public base, org with no seats), so it asserts the WALKS RAN: no error, no
// findings and zero writes all hold for `Org: nil`, which skips resolveSeats
// and checkProjects. Each walk must show its work.
//
// It runs as a SUBTEST: a Fatalf on the parent would abort before
// [integrationtest.Run] on a genuine reconcile.go defect and blame the
// fixture.
func assertConverged(t *testing.T, inst *instance, recorder *sink, opts jira.Options) {
	t.Helper()
	before := inst.mutations() + recorder.records()
	res, err := jira.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("the fixture is not converged, it is broken: %v", err)
	}
	// THE DERIVATION, not a pin — see convergedWorld. On Cloud the whole
	// webhook half means something else (those events arrive through the
	// Forge relay) and noIngressReason falls silent, so a fixture that
	// drifted onto Cloud would certify a different pass from the one this
	// file describes.
	if res.Deployment != jira.DataCenter {
		t.Fatalf("the fixture derived deployment %q, so it is not the Data "+
			"Center world every knob above was chosen for", res.Deployment)
	}
	if res.Account == "" {
		t.Fatal("the pass authenticated as nobody, so the org probe that " +
			"guards every read below it did not run")
	}
	// THE SEAT WALK RAN, AND CONVERGED. `Org: nil` returns from
	// resolveSeats before it reads anything, and so does a company of
	// human seats — both of which satisfy every assertion under this one.
	if len(res.Seats) == 0 {
		t.Fatal("the pass walked no seats at all, so a suite run over this " +
			"fixture certifies nothing about the seat walk — which is the " +
			"half of this command that reports who receives no Jira events")
	}
	for _, seat := range res.Seats {
		if !seat.Routes() {
			t.Fatalf("seat %s has no Jira account, so this fixture is a "+
				"company with work outstanding: %s", seat.Handle, seat.Reason)
		}
	}
	// AND THE PROJECT WALK. checkProjects returns early for an org that
	// declares no project, which is the same silent skip one level down.
	if len(res.Projects) == 0 {
		t.Fatal("the pass checked no projects, so the project walk is not " +
			"exercised by anything the suite then certifies")
	}
	for _, project := range res.Projects {
		if !project.Exists {
			t.Fatalf("the instance does not have project %s, so this fixture "+
				"reports a routing path that goes nowhere", project.Key)
		}
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Fatalf("the fixture reports work outstanding, so it is not the "+
			"steady state the suite is about: %+v", findings)
	}
	if report := integration.Classify(res.Findings()); report.Phase != integration.PhaseReady {
		t.Fatalf("the fixture classifies as %s: %+v", report.Phase, report)
	}
	if res.Hooked == "" {
		t.Fatal("the pass registered and converged no hook at all, so the " +
			"write-free clause below would hold of a pass that never reached " +
			"the webhook half")
	}
	if after := inst.mutations() + recorder.records(); after != before {
		t.Fatalf("the fixture took %d write(s) to reach the state it was "+
			"supposed to start in", after-before)
	}
}

// outstandingCompany is the same company with its provisioning unfinished.
//
// TWO SEATS THAT CANNOT BE REACHED, by the two different routes there are:
// the lead holds a token this instance refuses (a rotated credential nobody
// re-provisioned, which is the everyday one) and the SWE holds no Jira
// credential at all. Both render as `identity_failed` and both are owed by an
// ADMIN, but their Detail comes from opposite branches of resolveSeats, so a
// fixture with only one of them leaves the other's sentence unread.
//
// The human seat stays, for the reason it is in the converged fixture: a walk
// that probed it would report an unreachable seat over a company that is fine,
// and that regression is invisible in a world with no human in it.
func outstandingCompany() *org.Organization {
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "Eng Lead", DeclaredHandle: "lead", Project: "ENG",
				MCPEnv: map[string]map[string]string{
					"atlassian": {"JIRA_API_TOKEN": "rotated-token"},
				}},
			{Name: "SWE", DeclaredHandle: "swe"},
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{AtlassianAccountID: acctFounder}},
		},
	}
	o.Normalize()
	return o
}

// outstandingWorld is a Jira instance somebody has to go and fix.
//
// Broken in both walks a pass reports from, each certifying nothing of the other:
// the SEAT walk (two seats with no account, see outstandingCompany:
// `identity_failed`) and the PROJECT walk (the org names ENG, the instance
// lacks it: `ingress_blocked`). The ingress half runs to the end: no secret
// and no hook, so the first pass mints, seals and registers (WebhookBase is
// set, or ensureWebhook returns early).
//
// NOTHING HERE RAISES, since the findings cases Fatalf on an error: the
// refused seat is refused with a STATUS, an unanswered connection being a
// fault (TestASeatLookupTheInstanceDidNotAnswerIsAFaultNotAMissingAccount).
func outstandingWorld(t *testing.T) (*instance, jira.Options) {
	t.Helper()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	// No project ENG and no account for `rotated-token`: both absences are
	// the fixture, and both are things the instance ANSWERS about.

	client, err := jira.NewClient(jira.ClientOptions{URL: inst.URL, Token: "org-token"})
	if err != nil {
		t.Fatal(err)
	}
	return inst, jira.Options{
		Client: client,
		Config: &config.Jira{
			URL: inst.URL, Token: "${JIRA_TOKEN}",
			WebhookSecret: "${JIRA_WEBHOOK_SECRET}",
		},
		Org: outstandingCompany(),
		Value: func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return ""
			}
			return v
		},
		Sink:        newSink(),
		WebhookBase: convergedBase,
	}
}

// THE WRITE COUNTER HAS TO BE ABLE TO COUNT.
//
// [integrationtest.Reconciler.Mutations] is [instance.mutations] plus
// [sink.records]. A counter that never increments satisfies "a converged pass
// writes nothing" (zero equals zero), so nothing else can tell a pass that
// leaves the instance alone from a harness that stopped watching.
//
// Each of the three writes a pass can make (registering, repointing, removing
// the inbound hook) is driven through the real reconcile and must be counted,
// which also pins instance.mutations' route analysis. The converged row is
// the other half: a counter taking the pass's reads for writes would make the
// clause impossible to satisfy.
func TestTheWriteCounterSeesEveryWriteAJiraPassCanMake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		world func(*instance, *jira.Options)
		want  int
	}{
		{
			name:  "the steady state, where only reads happen",
			world: func(*instance, *jira.Options) {},
			want:  0,
		},
		{
			name:  "registering a hook the instance does not have",
			world: func(inst *instance, _ *jira.Options) { inst.hooks = nil },
			want:  1,
		},
		{
			name: "repointing one at an address that moved",
			world: func(inst *instance, _ *jira.Options) {
				inst.hooks[0]["url"] = "https://moved.example.com/webhooks/jira"
			},
			want: 1,
		},
		{
			name:  "recreating one, which is a delete and a create",
			world: func(_ *instance, opts *jira.Options) { opts.RecreateWebhook = "-recreate-webhook" },
			want:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst, _, opts := convergedWorld(t)
			tc.world(inst, &opts)
			if _, err := jira.Reconcile(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			if got := inst.mutations(); got != tc.want {
				t.Errorf("the instance counted %d write(s), want %d: the "+
					"counter this harness hands integrationtest cannot see "+
					"what a pass did, so the clause it certifies is vacuous",
					got, tc.want)
			}
		})
	}
}

// AND SO DOES THE SEALED STORE'S HALF.
//
// The other estate integrationtest names explicitly, and the one a counter
// keyed on HTTP requests would miss entirely: re-sealing the signing secret
// on every converged pass is a write a person has to undo, and it never
// touches the instance. [sink.records] is asserted to move where a pass
// mints, and [sink.flushed] with it, because a sealed value that is never
// completed is the defect TestAPassThatMintedAndThenFailedStillCompletesTheSink
// exists for and a Flush counter stuck at zero would hide it.
func TestTheSealCounterSeesAMintAndItsCompletion(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	inst.hooks = nil
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}

	if _, err := jira.Reconcile(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := recorder.records(); got != 1 {
		t.Errorf("the sink counted %d seal(s) over a pass that minted one, so "+
			"a credential re-sealed on every tick would be invisible to the "+
			"clause that exists to catch it", got)
	}
	if got := recorder.flushed(); got != 1 {
		t.Errorf("the sink counted %d completion(s), so a pass that sealed a "+
			"value and never announced it would read as a correct one", got)
	}
}

// assertOutstanding refuses an outstanding world that is quietly converged,
// or outstanding for a reason it did not intend.
//
// THE MIRROR OF assertConverged. [integrationtest]'s anti-vacuity case asks
// for one finding and one person owing something, which a pass that bailed
// after the seat walk still satisfies. So the fixture states exactly which
// three findings it expects.
//
// Every one is owed by a PERSON: "a finding a person must act on says what to
// do" skips engine-owed findings, and would run over nothing.
func assertOutstanding(t *testing.T, inst *instance, opts jira.Options) {
	t.Helper()
	res, err := jira.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("the outstanding world raised rather than reporting, so the "+
			"findings cases will read a fault instead of findings: %v", err)
	}
	want := []integration.Finding{
		// The seat walk twice, the project walk — in Findings' own order,
		// which two passes have to agree on.
		{Kind: integration.FindingIdentityFailed, Subject: "lead"},
		{Kind: integration.FindingIdentityFailed, Subject: "swe"},
		{Kind: integration.FindingIngressBlocked, Subject: "ENG"},
	}
	got := res.Findings()
	if len(got) != len(want) {
		t.Fatalf("the outstanding world reports %d finding(s), want the %d "+
			"this fixture is built to produce:\n got: %+v\nwant: %+v",
			len(got), len(want), got, want)
	}
	for i, f := range got {
		if f.Kind != want[i].Kind || f.Subject != want[i].Subject {
			t.Errorf("finding %d is %s on %q, want %s on %q", i,
				f.Kind, f.Subject, want[i].Kind, want[i].Subject)
		}
		if _, actor := f.Kind.Verdict(); !actor.WaitsOnAPerson() {
			t.Errorf("%s on %q is owed by %s rather than by a person, so the "+
				"case about what an operator is told skips it",
				f.Kind, f.Subject, actor)
		}
	}
	// AND THE INGRESS HALF REACHED ITS LAST STEP: a secret minted into the
	// variable the config names, sealed, and the one hook registered with
	// it. A world whose pass stopped before that would leave every clause
	// certifying half a reconciler.
	if want := convergedBase + "/webhooks/jira"; res.Hooked != want {
		t.Errorf("Hooked = %q, want the hook this pass registers at %q", res.Hooked, want)
	}
	if got := inst.mutations(); got != 1 {
		t.Errorf("the first pass made %d write(s) at the instance, want the one "+
			"registration", got)
	}
	if sealed := opts.Sink.(*sink).value("JIRA_WEBHOOK_SECRET"); sealed == "" {
		t.Error("the hook was registered with no secret sealed for the route to verify with")
	}
}

// THE CONTRACT, AGAINST THE RECONCILE THE LOOP RUNS.
func TestTheJiraReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	outInst, outOpts := outstandingWorld(t)

	// SUBTESTS, not Fatalf on the parent: a failed premise must not stop
	// the suite from producing its own diagnostic. See assertConverged.
	t.Run("the converged fixture is the steady state", func(t *testing.T) {
		assertConverged(t, inst, recorder, opts)
	})
	t.Run("the outstanding fixture reports the two walks", func(t *testing.T) {
		assertOutstanding(t, outInst, outOpts)
	})

	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: opts}
		},
		Outstanding: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: outOpts}
		},
		// BOTH ESTATES, AND ONLY THE CONVERGED ONE'S. The instance's own
		// writes are the visible half; the sink is the other one
		// integrationtest names explicitly — "a pass that re-seals a
		// seat's credential through the fleet's sealed store on every
		// converged run is writing just as surely, and the harness has to
		// see it". Jira's re-sealable value is the webhook signing
		// secret, and re-minting it is the outage this package's own doc
		// opens with. The outstanding world has its own instance and a
		// sink that cannot record, so nothing it does can move this
		// counter — which is what the suite means by sampling it only
		// around a converged pass.
		Mutations: func() int { return inst.mutations() + recorder.records() },
	})
}

// AND AGAIN OVER THE WINDOW A MINTING PASS LEAVES BEHIND.
//
// `${VAR}` resolves from a SNAPSHOT taken at apply time, so between a pass
// sealing a fresh signing secret and something rebuilding that snapshot,
// Value answers nothing for a variable the sealed store holds. That window is
// still a converged world — the instance has the hook, signed with the key
// this deployment holds — and it is the one where re-minting is most
// tempting and most destructive: each rotation moves the instance further
// from the value the running engine verifies with, on the loop's timer.
func TestTheJiraReconcilerMeetsTheContractOnASecretOnlyTheSinkHolds(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	if err := recorder.Record(
		context.Background(), "JIRA_WEBHOOK_SECRET", "the-sealed-secret"); err != nil {
		t.Fatal(err)
	}
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}
	outInst, outOpts := outstandingWorld(t)

	t.Run("the converged fixture is the steady state", func(t *testing.T) {
		assertConverged(t, inst, recorder, opts)
	})
	// THE SAME OUTSTANDING WORLD, and that is correct rather than lazy:
	// what differs between these two runs is where the CONVERGED world's
	// signing secret lives, which is a claim about writes. The findings
	// cases read the outstanding world, and nothing about the sealed-value
	// window changes what a Jira with two unreachable seats and a missing
	// project reports.
	t.Run("the outstanding fixture reports the two walks", func(t *testing.T) {
		assertOutstanding(t, outInst, outOpts)
	})

	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: opts}
		},
		Outstanding: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: outOpts}
		},
		Mutations: func() int { return inst.mutations() + recorder.records() },
	})
}

// A SECOND PASS DOES NOT MINT OVER A SECRET THIS DEPLOYMENT ALREADY SEALED.
//
// [integration.Reconciler]'s contract says it in as many words — "check what
// the sink recorded (provision.TokenSink.Value) and keep a working
// credential" — and this pass was the one that did not. It asked only the
// resolver, which answers from a snapshot taken at apply time, so every pass
// in the window after a mint minted a SECOND secret, sealed it over the
// first, and re-registered the hook with it. The engine's own webhook route
// verifies with the snapshot, so the instance was signing with a key nothing
// on this side held, and the next pass did it again.
func TestASecondPassDoesNotMintOverASecretThisDeploymentAlreadySealed(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	// Nothing registered yet, and a resolver that never gains the value —
	// which is the state a node is in when it has just sealed one.
	inst.hooks = nil
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}

	if _, err := jira.Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if recorder.records() != 1 || len(inst.created) != 1 {
		t.Fatalf("the premise is wrong: the first pass sealed %d value(s) and "+
			"registered %d hook(s)", recorder.records(), len(inst.created))
	}
	minted := recorder.value("JIRA_WEBHOOK_SECRET")

	if _, err := jira.Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := recorder.records(); got != 1 {
		t.Errorf("the second pass sealed a value over the one already held: "+
			"%d records", got)
	}
	if recorder.value("JIRA_WEBHOOK_SECRET") != minted {
		t.Error("the signing secret was rotated by a pass nobody asked to " +
			"rotate anything, so the instance now signs with a key this " +
			"deployment does not hold")
	}
	if len(inst.created) != 1 || len(inst.updated) != 0 {
		t.Errorf("the second pass rewrote the hook: created %v, updated %v",
			inst.created, inst.updated)
	}
}

// A PASS THAT MINTED AND THEN FAILED STILL COMPLETES THE SINK.
//
// [provision.TokenSink.Flush] is where a run's writes STAND: engine's
// refreshingSink rebuilds the `${VAR}` snapshot there and nowhere else. A pass
// that sealed a signing secret and was then refused by the instance must still
// flush. The next pass reads the sealed value back through
// [provision.TokenSink.Value] and never Records again, so an unflushed value
// is permanent: the snapshot is never rebuilt, the webhook route cannot
// resolve the key, every delivery is refused, and the pass reports Ready.
func TestAPassThatMintedAndThenFailedStillCompletesTheSink(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	inst.hooks = nil
	// The org account can read this instance and may not change it, which
	// is the everyday cause: a token without Administer Jira.
	inst.hookWriteStatus = 403
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}

	if _, err := jira.Reconcile(context.Background(), opts); err == nil {
		t.Fatal("the premise is wrong: an instance that refuses the " +
			"registration did not fail the pass")
	}
	if recorder.records() != 1 || recorder.value("JIRA_WEBHOOK_SECRET") == "" {
		t.Fatalf("the premise is wrong: the pass sealed %d value(s), so there "+
			"is nothing whose completion could matter", recorder.records())
	}
	if got := recorder.flushed(); got != 1 {
		t.Errorf("a pass that sealed a signing secret and then failed "+
			"completed its sink %d time(s): the value is sealed and the "+
			"engine will never rebuild the snapshot that resolves it, so "+
			"every delivery is refused at the edge for ever", got)
	}
}

// AND A PASS CANCELLED AFTER MINTING COMPLETES IT ANYWAY.
//
// The other half, and the one a plain `Flush(ctx)` on the failure path would
// still get wrong: the failure being completed is very often the
// CANCELLATION ITSELF — a node shutting down mid-pass, having just sealed a
// secret — and a completion handed the dead context does nothing at all,
// which is the state above arrived at through the door that was left open.
// It is the same rule this tree applies to every rollback and teardown.
func TestAPassCancelledAfterMintingCompletesTheSinkAnyway(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	inst.hooks = nil
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The pass is cancelled at the instant it seals, which is the narrow
	// window the whole rule is about: the value is durable and the request
	// that would have registered the hook never completes.
	opts.Sink = &cancellingSink{sink: recorder, cancel: cancel}

	if _, err := jira.Reconcile(ctx, opts); err == nil {
		t.Fatal("the premise is wrong: a pass cancelled mid-flight succeeded")
	}
	if recorder.records() != 1 {
		t.Fatalf("the premise is wrong: the pass sealed %d value(s)",
			recorder.records())
	}
	if recorder.flushed() != 1 {
		t.Fatalf("the cancelled pass completed its sink %d time(s)",
			recorder.flushed())
	}
	if !recorder.flushedAlive() {
		t.Error("the completion inherited the cancelled context, so it did " +
			"nothing: the secret is sealed, the snapshot is never rebuilt " +
			"and the deployment cannot verify a delivery signed with it")
	}
}

// A FLUSH THAT FAILED IS REPORTED WITHOUT HIDING WHY THE PASS FAILED.
//
// Two independent facts, and the loop routes on the first: [integration.Reject]
// classifies the pass's own error, and errors.Is against
// [integration.ErrCredentialRejected] decides whether an operator is sent to
// rotate a token or told to wait. Substituting the flush failure sends them
// to the wrong place; dropping it hides a sink this deployment can no longer
// seal into, with a live secret already in it.
func TestAFlushFailureIsReportedWithoutHidingWhyThePassFailed(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	inst.hooks = nil
	inst.hookWriteStatus = 403
	errSinkStuck := errors.New("the sealed store went away")
	recorder.flushErr = errSinkStuck
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}

	_, err := jira.Reconcile(context.Background(), opts)
	if err == nil {
		t.Fatal("neither the refused registration nor the failed completion " +
			"was reported")
	}
	if !errors.Is(err, errSinkStuck) {
		t.Errorf("the failed completion is unreachable from the error, so a "+
			"secret is sealed in a store nothing can reach and nothing says "+
			"so: %v", err)
	}
	if !strings.Contains(err.Error(), "create webhook") {
		t.Errorf("the pass's own failure was replaced by its sink's, so the "+
			"loop routes on the wrong one: %v", err)
	}
}

// cancellingSink seals a value and then takes the pass's context away.
//
// The narrow window [TestAPassCancelledAfterMintingCompletesTheSinkAnyway] is
// about, expressed where it actually happens rather than approximated by
// cancelling before the pass starts — which fails at the org probe, long
// before anything is minted.
type cancellingSink struct {
	*sink
	cancel context.CancelFunc
}

func (s *cancellingSink) Record(ctx context.Context, name, value string) error {
	if err := s.sink.Record(ctx, name, value); err != nil {
		return err
	}
	s.cancel()
	return nil
}

// A SEAT LOOKUP THE INSTANCE DID NOT ANSWER IS A FAULT, NOT A MISSING
// ACCOUNT.
//
// Every failure in the seat walk became the same empty account, which
// [jira.Result.Findings] renders as identity_failed — "this seat has no Jira
// account", owed by an ADMIN and never clearing on its own. That is true of a
// token the instance refused and a fabrication about an instance that never
// answered: one blip, or one node cancelling mid-pass on its way down,
// reported every credentialled seat as an account somebody has to go and
// create. The refused half stays a finding; it is asserted by
// TestFindingsReportASeatWithNoAccount, and it is what the outstanding world
// above is built on.
func TestASeatLookupTheInstanceDidNotAnswerIsAFaultNotAMissingAccount(t *testing.T) {
	t.Parallel()
	inst, _, opts := convergedWorld(t)
	// The instance closes the connection with no response, which is what a
	// dead instance, a dropped network and a cancelled request look like.
	inst.unanswered["Bearer swe-token"] = true

	res, err := jira.Reconcile(context.Background(), opts)
	if err == nil {
		t.Fatalf("an instance that never answered was reported as a seat with "+
			"no Jira account: %+v", res.Findings())
	}
	if !strings.Contains(err.Error(), "swe") {
		t.Errorf("the fault does not name the seat it failed on: %v", err)
	}
	// AND IT IS NOT A REFUSED CREDENTIAL. A refusal never clears on its
	// own and is routed to the operator; a connection that dropped clears
	// without anybody, and sending them to rotate a working token is the
	// wrong half of the same mistake.
	if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("an unanswered request was reported as a refused credential: %v", err)
	}
}

// A CANCELLED PASS RAISES RATHER THAN REPORTING A CONVERGED INSTANCE.
//
// The suite drives this too, and it is worth pinning here as well because
// what protects it is an ordering rather than a check: the org credential
// probe is the pass's first act, so a cancelled context fails before anything
// is read or written. An early return added above that probe — for a company
// with no seats, say — would hand the loop (no findings, nil error), which it
// reads as "this integration is ready" and trusts for a full settled
// interval, from a pass that looked at nothing.
func TestACancelledPassRaisesRatherThanReportingAConvergedInstance(t *testing.T) {
	t.Parallel()
	inst, _, opts := convergedWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := jira.Reconcile(ctx, opts)
	if err == nil {
		t.Fatalf("a cancelled pass answered with findings: %+v", res.Findings())
	}
	if inst.mutations() != 0 {
		t.Errorf("a cancelled pass made %d write(s)", inst.mutations())
	}
}
