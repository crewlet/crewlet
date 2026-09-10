package jira_test

import (
	"context"
	"errors"
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
// [integrationtest] states [integration.Reconciler]'s safety contract as
// seven cases, and until this file existed its only caller was a stub whose
// Run returned (nil, nil): "a converged pass writes nothing" was true because
// nothing happened, and the promise about what a pass does to somebody's Jira
// instance was still checked by nobody. What follows stands a real instance
// up in the state the loop spends its life in and drives the real
// [jira.Reconcile] against it, counting every request that instance would
// have to have undone.

// convergedBase is the address this deployment is reachable on.
const convergedBase = "https://engine.example.com"

// conformer drives the REAL reconcile through [integration.Reconciler].
//
// TEST-ONLY, deliberately. The loop reaches this package through
// engine.passConverger wrapping engine.jiraPass, so a second adapter in
// production code here would be an implementation with no caller. What it
// owes is to be that pass's Run rather than a paraphrase of it, and it is:
// [jira.Reconcile], then [jira.Result.Findings] on success, and the error
// handed back untouched otherwise — which is exactly what jiraPass.Run does
// once it has resolved the base URL and the token.
type conformer struct{ opts jira.Options }

func (*conformer) Kind() integration.Kind { return integration.KindJira }

func (c *conformer) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := jira.Reconcile(ctx, c.opts)
	if err != nil {
		return nil, err
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

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
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
// zero is zero. So this asserts what "converged" is supposed to mean before
// anything is certified over it: one pass, nothing to report, and a hook the
// pass actually reached and left alone.
func assertConverged(t *testing.T, inst *instance, recorder *sink, opts jira.Options) {
	t.Helper()
	before := inst.mutations() + recorder.records()
	res, err := jira.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("the fixture is not converged, it is broken: %v", err)
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

// THE CONTRACT, AGAINST THE RECONCILE THE LOOP RUNS.
func TestTheJiraReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	inst, recorder, opts := convergedWorld(t)
	assertConverged(t, inst, recorder, opts)

	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: opts}
		},
		// BOTH ESTATES. The instance's own writes are the visible half;
		// the sink is the other one integrationtest names explicitly —
		// "a pass that re-seals a seat's credential through the fleet's
		// sealed store on every converged run is writing just as surely,
		// and the harness has to see it". Jira's re-sealable value is the
		// webhook signing secret, and re-minting it is the outage this
		// package's own doc opens with.
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
	assertConverged(t, inst, recorder, opts)

	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(integrationtest.TB) integration.Reconciler {
			return &conformer{opts: opts}
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
// TestFindingsReportASeatWithNoAccount.
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
