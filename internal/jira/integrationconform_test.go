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
	"github.com/crewlet/crewlet/internal/provision"
)

// Where the certification suite binds to the Jira reconcile.
//
// [integrationtest] states [integration.Reconciler]'s safety contract as nine
// cases, and until this file existed its only caller was a stub whose Run
// returned (nil, nil): "a converged pass writes nothing" was true because
// nothing happened, and the promise about what a pass does to somebody's Jira
// instance was still checked by nobody. What follows stands a real instance
// up in the state the loop spends its life in and drives the real
// [jira.Reconcile] against it, counting every request that instance would
// have to have undone.
//
// # TWO WORLDS, because the converged one cannot certify half the suite
//
// Four of the nine cases walk the findings list, and a converged pass reports
// no findings — that is what converged MEANS. Driven over the steady state
// alone they iterate an empty slice and pass whatever this package does, so
// the file's first version put seven green ticks on a vendor that had been
// checked about two of them. [convergedWorld] is still the world the
// write-free clause is about; [outstandingWorld] below is a Jira somebody has
// to go and fix, and it is what the findings cases actually read.

// convergedBase is the address this deployment is reachable on.
const convergedBase = "https://engine.example.com"

// conformer drives the REAL reconcile through [integration.Reconciler].
//
// TEST-ONLY, deliberately. The loop reaches this package through
// engine.passConverger wrapping engine.jiraPass, so a second adapter in
// production code here would be an implementation with no caller.
//
// # What it mirrors, and — precisely — what it does not
//
// The body is jiraPass.Run's body from the point that pass has resolved its
// inputs: [jira.Reconcile], then [jira.Result.Findings] on success, and on
// failure the same `engine: jira pass: %w` wrap the pass applies, which is
// kept rather than paraphrased so that anything reading the error with
// errors.Is or errors.As reads what the loop would.
//
// What is NOT mirrored is everything jiraPass does BEFORE that, and it is not
// small: resolving the base URL through jiraBaseURL and
// `integrations.jira.token`, the [integration.ErrNotConfigured] early return
// for a company with no jira block, and the single
// [integration.FindingCredentialMissing] it answers when the base or the
// token resolve to nothing. None of that is certified here.
//
// Nor is [conformer.Kind]. It returns the same package-level constant
// jiraPass.Kind() returns, so the two move together if the constant is
// renamed — but engine's is the one the loop keys jira's status on, its
// receiver is unexported, and nothing in THIS package names a kind at all.
// So "the kind is one this build converges" and "the kind does not change
// across passes" certify a constant rather than product code, and saying so
// here is the honest alternative to implying otherwise.
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
			{Name: "Eng Lead", DeclaredHandle: "lead", JiraProject: "ENG",
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
// the options that read it.
//
// EVERY KNOB THAT DECIDES CONVERGENCE IS SET HERE, EXPLICITLY, because each
// one fails in a direction that reads as the product's fault:
//
//   - The seeded hook carries its own name, its own address and its own event
//     list. The fake defaults all three, so a fixture that said nothing would
//     agree with whatever the reconcile asked it — a converged registration
//     nobody wrote down.
//   - Value resolves the signing secret. Left unresolved it is not a
//     converged world at all: the pass mints, seals and re-registers, and
//     "a converged pass writes nothing" goes red over a fixture rather than
//     over the code.
//   - WebhookBase is set. Left empty the pass skips the webhook half
//     entirely and every clause passes VACUOUSLY, which is the failure this
//     whole file exists to end. assertConverged below refuses that fixture.
//   - RecreateWebhook stays false: it is the operator's rotate gesture and
//     forces a delete and a create by design.
//
// The DEPLOYMENT is the one knob deliberately left underived, and that is
// also why: engine.jiraPass builds its client with no Deployment field, so
// production infers Cloud or Data Center from the address alone. Pinning
// jira.DataCenter here would make the harness disagree with the pass it
// claims to mirror on the value that selects the API path AND decides
// whether a missing public base is reported at all. So the fixture lets
// [jira.DeploymentOf] derive it, exactly as production does, and
// assertConverged asserts the derivation landed on Data Center rather than
// trusting it.
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
// THE HARNESS PROVES ITS OWN PREMISE FIRST. Every clause of the suite is
// satisfied by a world where the pass did nothing because it was never given
// anything to do, and the cheapest way to get one is a knob set wrong: an
// empty public base skips the hook half, an org with no seats skips the walk.
// The suite cannot see that — it asks the world how many writes it took, and
// zero is zero.
//
// # It asserts the WALKS RAN, not merely that nothing was reported
//
// Its first version checked no error, no findings, PhaseReady, a non-empty
// Hooked and zero writes — and every one of those is satisfied by
// `Org: nil`, which skips resolveSeats and checkProjects entirely. It
// advertised refusing "an org with no seats" and checked nothing of the kind.
// So each walk now has to show its work: an org account, at least one seat
// that routes, at least one project that exists, and every one of them
// converged. A fixture that reaches the webhook half and nothing else is
// refused here rather than certified by the suite.
//
// # And it runs as a SUBTEST, so a real regression is still diagnosed
//
// It used to Fatalf on the parent, which meant that a genuine defect in
// reconcile.go — a converged pass that mints — aborted the test before
// [integrationtest.Run] was ever called: zero subtests, and the only message
// printed was this file complaining about its own fixture, which sends the
// reader to the harness instead of to the code. The precondition is a claim
// about the fixture and the suite is a claim about the product; they are
// reported separately so that both are.
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
			{Name: "Eng Lead", DeclaredHandle: "lead", JiraProject: "ENG",
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
// # Why this world and not an easier one
//
// [integrationtest.Reconciler.Outstanding] asks for a world where something
// is wrong IN A WAY A PERSON MUST ACT ON, and the cheap way to satisfy that
// is one broken thing. This deployment is broken in all three of the ways a
// Jira pass can report, because the three come from three different walks and
// a world that exercises one certifies nothing about the other two:
//
//   - THE SEAT WALK. Two seats with no account — see outstandingCompany —
//     which is `identity_failed`, owed by the admin who can issue a token.
//   - THE PROJECT WALK. The org names ENG and this instance does not have it,
//     which is `ingress_blocked` on the key: every issue routed by that
//     project reaches nobody, and Jira answers the same 404 for a project a
//     credential may not browse.
//   - THE INGRESS HALF. The signing secret resolves to nothing and the sink
//     is [provision.ReadOnly] — a node with no keyring — so the pass has
//     nothing to sign deliveries with, mints nothing and registers nothing.
//     That is `ingress_blocked` on `integrations.jira.webhook_secret`.
//
// WebhookBase IS SET even so, and deliberately. The obvious way to produce an
// ingress finding is to leave the public base empty, and it is the wrong one
// here: ensureWebhook then returns before it reads anything, so the entire
// webhook half — the secret resolution, the keyring check, the guard that
// registers no hook without a key to sign it — is skipped, and a suite driven
// over that world would be certifying a pass that stopped early. The keyring
// route reaches all of it and stops at the last possible moment.
//
// NOTHING HERE RAISES, which is a property and not an accident: the four
// findings cases Fatalf on an error, so a world that failed mid-pass would
// report as a broken harness rather than as an outstanding integration. The
// org credential answers, the refused seat is refused with a STATUS (a
// connection the instance never answers is a fault, by design — see
// TestASeatLookupTheInstanceDidNotAnswerIsAFaultNotAMissingAccount), the
// project 404s, and ReadOnly answers rather than erroring.
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
		// A NODE WITH NO KEYRING. The loop hands a pass this sink when it
		// cannot seal anything, and it is the one shape that produces an
		// ingress finding with the public base set.
		Sink:        provision.ReadOnly(),
		WebhookBase: convergedBase,
	}
}

// THE WRITE COUNTER HAS TO BE ABLE TO COUNT.
//
// [integrationtest.Reconciler.Mutations] is the clause that package calls the
// one most likely to be wrong, and on this side the whole of it is
// [instance.mutations] plus [sink.records]. A counter that never increments
// satisfies "a converged pass writes nothing" perfectly — zero equals zero —
// so nothing in the suite can tell a pass that leaves the instance alone from
// a harness that stopped watching. That is not hypothetical: with the
// increment in instance.serve disabled, every case in this file stayed green,
// including both preconditions. The counter was the one thing here with no
// test behind it, and it is the thing the suite trusts most.
//
// So each of the three writes a Jira pass can make is driven through the real
// reconcile and the counter is asserted to have seen it — which also pins the
// ROUTE analysis instance.mutations writes out, because registering,
// repointing and removing the inbound hook are the whole of what this pass
// can change at an instance. The converged row is the other half: every one
// of these passes also performs three identity lookups, a project read and a
// hook listing, and a counter that took those for writes would make the
// clause impossible to satisfy rather than merely unenforced.
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
			world: func(_ *instance, opts *jira.Options) { opts.RecreateWebhook = true },
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
// THE MIRROR OF assertConverged, and needed for the mirror reason.
// [integrationtest]'s own anti-vacuity case asks for one finding and one
// person owing something, which a world can satisfy by accident and keep
// satisfying while the two walks this fixture is actually about stop running:
// a pass that bailed after the seat walk would still report two
// identity_failed findings and still look like a world with work in it. So
// the fixture states exactly which three walks it expects to hear from, and
// which finding each owes.
//
// Every one of them is owed by a PERSON, which is the second thing the suite
// needs and cannot check: "a finding a person must act on says what to do"
// skips every finding whose actor is the engine, so a world whose findings
// were engine-owed would run that case over nothing while passing the case
// above it.
func assertOutstanding(t *testing.T, inst *instance, opts jira.Options) {
	t.Helper()
	res, err := jira.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("the outstanding world raised rather than reporting, so the "+
			"findings cases will read a fault instead of findings: %v", err)
	}
	want := []integration.Finding{
		// The ingress half, the seat walk twice, the project walk — in
		// Findings' own order, which two passes have to agree on.
		{Kind: integration.FindingIngressBlocked, Subject: "integrations.jira.webhook_secret"},
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
	// AND NOTHING WAS REGISTERED. The ingress finding says this deployment
	// has no key to sign a delivery with; a hook registered anyway would
	// make the instance deliver and the engine's own route refuse every
	// delivery, and the finding would then be describing a world that no
	// longer exists.
	if res.Hooked != "" || inst.mutations() != 0 {
		t.Errorf("a hook was registered with no key to sign it: %q, %d write(s)",
			res.Hooked, inst.mutations())
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
	t.Run("the outstanding fixture reports the three walks", func(t *testing.T) {
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
	// window changes what a Jira with two unreachable seats, a missing
	// project and no keyring reports.
	t.Run("the outstanding fixture reports the three walks", func(t *testing.T) {
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
// [provision.TokenSink.Flush] is where a run's writes STAND, and under the
// reconcile loop that is not ceremony: engine's refreshingSink rebuilds the
// `${VAR}` snapshot there and nowhere else. This pass returned its error
// before reaching it, and the mint happens several frames earlier — so a pass
// that sealed a signing secret and was then refused by the instance left the
// value sealed and unannounced.
//
// # And on THIS pass that is permanent, not a rotation on a timer
//
// Elsewhere the unflushed value is simply re-minted on the next tick, which
// is bad enough. Here the fix that came first makes it worse: the next pass
// reads the sealed value back through [provision.TokenSink.Value] and never
// Records again, so the sink never seals again, so the snapshot is never
// rebuilt — for the life of the deployment. The instance is eventually
// registered with a key the engine's own webhook route cannot resolve, every
// delivery is refused at the edge, and the pass reports Ready every time.
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

// A NODE WITH NO KEYRING REPORTS, IT DOES NOT FAULT FOR EVER.
//
// The loop hands a pass [provision.ReadOnly] when the node cannot seal
// anything, and that sink answers ErrNoSink from Record. This pass went
// straight to Record, so a deployment that had simply not set secrets.keys
// had its Jira pass raise on every tick for the life of the deployment, with
// the dashboard reporting the engine working on it — which is precisely the
// permanent-fault posture ReadOnly was introduced to remove.
func TestANodeWithNoKeyringReportsTheMissingSigningSecretRatherThanFaulting(t *testing.T) {
	t.Parallel()
	inst, _, opts := convergedWorld(t)
	opts.Sink = provision.ReadOnly()
	opts.Value = func(v string) string {
		if v == "${JIRA_WEBHOOK_SECRET}" {
			return ""
		}
		return v
	}

	res, err := jira.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("a node with no keyring faulted rather than reporting: %v", err)
	}
	findings := res.Findings()
	var found *integration.Finding
	for i, f := range findings {
		if f.Subject == "integrations.jira.webhook_secret" {
			found = &findings[i]
		}
	}
	if found == nil {
		t.Fatalf("nothing reports why no hook was registered: %+v", findings)
	}
	if found.Kind != integration.FindingIngressBlocked {
		t.Errorf("kind is %q, want %q — the field's own setup requirement "+
			"declares that this is what an absent value produces",
			found.Kind, integration.FindingIngressBlocked)
	}
	// BOTH WAYS OUT, because either one clears it and an operator who is
	// told only about the keyring cannot use the one they already have.
	for _, want := range []string{"secrets.keys", "integrations.jira.webhook_secret"} {
		if !strings.Contains(found.Detail, want) {
			t.Errorf("the finding does not name %s: %q", want, found.Detail)
		}
	}
	// AND NOTHING WAS REGISTERED. Jira signs a delivery with whatever the
	// hook was registered under, and this deployment holds no key to verify
	// one with, so a hook here would make the instance deliver and the edge
	// refuse every delivery.
	if res.Hooked != "" || inst.mutations() != 0 {
		t.Errorf("a hook was registered with no key to sign it: %q, %d write(s)",
			res.Hooked, inst.mutations())
	}
	if report := integration.Classify(findings); report.Phase == integration.PhaseReady {
		t.Errorf("a deployment nothing can deliver to classified ready: %+v", report)
	}
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
