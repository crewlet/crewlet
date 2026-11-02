package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

// A SURFACE THAT COULD NOT REGISTER WHAT IT WAS ASKED FOR DOES NOT REPORT
// READY.
//
// `integrations.github.token` is optional and the connect form does not ask
// for it, both deliberately: routing needs nothing from it, because each
// agent's own app answers who is participating in a thread. What it IS still
// needed for is a hook the company DEMANDED — `org_webhook: true`, which has
// no fallback, or a `repos` list, which no agent's own app covers — and with
// no token the pass reads nothing and writes nothing.
//
// It said so in NOTES, and notes are not findings. Measured on a live
// connect: `phase: ready`, `phase_label: Connected`, `findings: []`,
// `routes: true`, `secret_usable: true` — and exactly one webhook on the
// organization, belonging to a different deployment and never triggered.
func TestAPassWithNoCredentialToRegisterWithReportsIt(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled:       true,
			WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org:        "crewbed",
				OrgWebhook: config.ContainerWebhookRequire,
			},
		},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found bool
	for _, f := range res.Findings() {
		if f.Subject != "integrations.github.token" {
			continue
		}
		found = true
		if f.Kind != integration.FindingIngressBlocked {
			t.Errorf("reported as %s, want ingress_blocked: the company asked "+
				"for a hook and there is nothing to register one with", f.Kind)
		}
		// THE ORGANIZATION IT ASKED ABOUT, so an operator knows which
		// request went unanswered rather than only that one did.
		if !strings.Contains(f.Detail, "crewbed") {
			t.Errorf("the finding does not name what was asked for:\n%s", f.Detail)
		}
		// AND BOTH WAYS OUT, because the form offers no field for this.
		if !strings.Contains(f.Detail, "integrations.github.token") ||
			!strings.Contains(f.Detail, "own app") {
			t.Errorf("the finding names no way out:\n%s", f.Detail)
		}
		// SHORT ENOUGH TO READ. It is the card's one-line status, so a
		// paragraph there is a paragraph nobody reads.
		if len(f.Detail) > 200 {
			t.Errorf("the status line is %d characters:\n%s", len(f.Detail), f.Detail)
		}
	}
	if !found {
		t.Fatalf("findings = %v: a pass that read nothing at GitHub and "+
			"registered nothing reports ready", res.Findings())
	}
	if phase, _ := integration.Classify(res.Findings()).Phase, 0; phase == integration.PhaseReady {
		t.Error("the surface classifies as ready")
	}
}

// AND A NAMED REPOSITORY IS ASKED FOR TOO.
//
// The other half of "a hook this credential must register": no agent's own
// app covers a repository the company named, so `repos` is a request with no
// fallback behind it, exactly like `org_webhook: true`.
func TestNamedRepositoriesWithNoCredentialAreReported(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org:   "crewbed",
				Repos: []string{"crewbed/infraflow"},
			},
		},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found bool
	for _, f := range res.Findings() {
		if f.Subject != "integrations.github.token" {
			continue
		}
		found = true
		if !strings.Contains(f.Detail, "crewbed/infraflow") {
			t.Errorf("the finding does not name the repository:\n%s", f.Detail)
		}
		// NOT THE ORGANIZATION, which this company left on the default
		// mode and therefore did not demand a hook on.
		if strings.Contains(f.Detail, " crewbed:") ||
			strings.Contains(f.Detail, "on crewbed,") {
			t.Errorf("the finding names the organization, which was not "+
				"asked for:\n%s", f.Detail)
		}
	}
	if !found {
		t.Fatalf("findings = %v: a named repository was left unhooked in "+
			"silence", res.Findings())
	}
}

// AND THE ORDINARY SHAPE IS SILENT, which is the half that matters more.
//
// The connect form REQUIRES `provisioning.org` — that is where the agents'
// apps are installed — and asks for no token at all. So a block naming an
// organization is not a company wanting an organization-wide webhook, and
// reading it as one put a permanent finding on every company that connects
// GitHub from the dashboard: measured on a live connect, where it survived
// the operator installing the app and read as though the install had not
// taken. `auto` takes an organization hook if one can be had and each agent's
// own app webhook otherwise, and the second is the design rather than a
// degradation.
func TestAPassAskedForNoHookReportsNoRegistrar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pv   *config.GitHubProvisioning
	}{
		{"no provisioning block", nil},
		{"an org and the default mode", &config.GitHubProvisioning{Org: "crewbed"}},
		{"an org the company does not want hooked", &config.GitHubProvisioning{
			Org: "crewbed", OrgWebhook: config.ContainerWebhookNever,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := github.Reconcile(context.Background(), github.Options{
				Config: &config.GitHub{
					Enabled: true, WebhookSecret: "s", Provisioning: tc.pv,
				},
				WebhookBase: "https://engine.example.com",
			})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			for _, f := range res.Findings() {
				if f.Subject == "integrations.github.token" {
					t.Errorf("a company that asked for no hook it must "+
						"register was told it has no credential to register "+
						"one with:\n%s", f.Detail)
				}
			}
		})
	}
}

// THE RECOMMENDED ARRANGEMENT REPORTS NOTHING TO DO.
//
// A company whose agents each carry their own app hears about the
// repositories those apps are installed on, and that is the answer the form
// opens on. It was reported two ways and both were wrong: as an
// `ingress_blocked` finding naming the organization token, which reads as
// Action required over agents receiving events perfectly well, and, from the
// other branch, as "integrations.github.provisioning.repos is empty, so there
// is nothing left to hook", which describes a gap that is not there.
//
// Worse than either: the card saying so spent its time asking for the agents'
// apps to be installed, and an App installation can never carry
// `admin:org_hook`. So the one instruction on screen could not clear the one
// warning on screen, and an operator who did exactly as they were told
// watched nothing change. Measured on a live connect.
func TestTheRecommendedCoverageReportsNothingOutstanding(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org: "crewbed", OrgWebhook: config.ContainerWebhookNever,
			},
		},
		WebhookBase: "https://engine.example.com",
		// THE FACT THAT MAKES IT AN ARRANGEMENT rather than a company
		// receiving nothing: this pass reads the organization and cannot
		// see an agent's app, which is private to that agent.
		SeatApps: 2,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingIngressBlocked {
			t.Errorf("the recommended arrangement reports %s:\n%s", f.Kind, f.Detail)
		}
		if phase, _ := f.Kind.Verdict(); phase != integration.PhaseReady {
			t.Errorf("a finding with phase %s holds the surface out of ready "+
				"over agents that are receiving events:\n%s", phase, f.Detail)
		}
	}
	for _, note := range res.Notes {
		if strings.Contains(note, "nothing left to hook") {
			t.Errorf("the pass describes a gap that is not there: %q", note)
		}
	}
}

// AND IT SAYS WHAT IT COVERS, rather than saying nothing at all.
//
// A note would have been dropped before anything rendered it — the engine's
// pass returns findings and discards notes — so a person would learn the
// limits of their coverage only by noticing the first repository nobody hears
// about. One sentence, on a ready card, naming both halves.
func TestTheCoverageIsStatedOnAReadyCard(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org: "crewbed", OrgWebhook: config.ContainerWebhookNever,
			},
		},
		WebhookBase: "https://engine.example.com",
		SeatApps:    1,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got integration.Finding
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingCoveragePartial {
			got = f
		}
	}
	if got.Detail == "" {
		t.Fatalf("findings = %+v, none of them saying what this company's "+
			"agents hear about", res.Findings())
	}
	// BOTH HALVES. What is covered alone reads as a complete answer; what is
	// not alone reads as a fault.
	for _, want := range []string{"installed on", "none for the rest of crewbed"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the sentence does not mention %q:\n%s", want, got.Detail)
		}
	}
	if got.Title == "" {
		t.Error("the note has no lead for the notes list to set in bold")
	}
	// AND ITS REMEDY IS THE TOKEN, NEVER AN INSTALL.
	//
	// Installing an agent's app cannot widen this and never will — no App
	// carries `admin:org_hook` at any permission — so prescribing one is
	// telling somebody to repeat the thing that did not work. Matched on
	// the remedy rather than on the word "install", which appears in the
	// sentence ("the repositories your agents' apps are installed on")
	// perfectly correctly.
	remedy := got.Remedy
	if remedy == "" {
		t.Fatalf("the note offers no remedy:\n%+v", got)
	}
	if !strings.Contains(remedy, "integrations.github.token") {
		t.Errorf("the remedy does not name the field that widens this:\n%s", remedy)
	}
	if strings.Contains(strings.ToLower(remedy), "install") ||
		strings.Contains(strings.ToLower(remedy), "app") {
		t.Errorf("the remedy asks for an app, which can never carry "+
			"admin:org_hook:\n%s", remedy)
	}
	if phase, actor := got.Kind.Verdict(); phase != integration.PhaseReady ||
		actor != integration.ActorOperator {
		t.Errorf("verdict is %s/%s, want ready and the operator's: widening "+
			"this is a value in the company's own configuration", phase, actor)
	}
}

// A COMPANY THAT NAMED REPOSITORIES STILL HAS THEM HOOKED.
//
// The form dropped that question; the config did not lose the answer. Every
// repository a company names is still hooked, `org_webhook: false` still
// means what it meant, and the note about an empty list is still there for
// the company it is true of — no app anywhere and nothing registered.
func TestTheNamedRepositoryPathIsUnchanged(t *testing.T) {
	t.Parallel()
	pv := &config.GitHubProvisioning{
		Org: "crewbed", OrgWebhook: config.ContainerWebhookNever,
		Repos: []string{"crewbed/api", "crewbed/web"},
	}
	if got := github.TargetsOf(pv); len(got) != 2 {
		t.Fatalf("TargetsOf = %v, want both named repositories", got)
	}
	// AND WITH NO APPS THERE IS NO ARRANGEMENT TO DESCRIBE. The coverage
	// sentence claims a company's events arrive through its agents' own
	// apps; a company with none is not in that arrangement, it is receiving
	// nothing — which the roster's own "no app of its own" finding says.
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org: "crewbed", OrgWebhook: config.ContainerWebhookNever,
			},
		},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Coverage.Kind != "" {
		t.Errorf("a company with no apps was told its agents' apps cover "+
			"something: %+v", res.Coverage)
	}
}

// A COMPANY LEFT ON THE OLD DEFAULT IS NOT DEGRADED WHILE ITS AGENTS WORK.
//
// `Default` is a suggestion and never a stored value, so changing which
// answer the form opens on moves nobody who already stored `org_webhook:
// "true"` with no token — and that was every company connected from the
// dashboard while the strictest mode was the default. They are not migrated:
// rewriting an explicit answer is the engine overruling a decision somebody
// may have made on purpose, which is the thing this codebase removes
// everywhere else.
//
// What was wrong was the REPORT, not the value. `ingress_blocked` claims
// nothing can be delivered, and that is false the whole time each agent's own
// app is delivering — so the card read Action required beside a seat row
// reading `satisfied: true, ready`, and the remedy it offered (install the
// apps) was the one act that could never clear it.
func TestTheOldDefaultIsReportedAsCoverageNotAsABlock(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org: "crewbed", OrgWebhook: config.ContainerWebhookRequire,
			},
		},
		WebhookBase: "https://engine.example.com",
		SeatApps:    1,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingIngressBlocked {
			t.Errorf("a company whose agents are receiving events reports %s "+
				"and is held out of ready:\n%s", f.Kind, f.Detail)
		}
	}
	if res.Coverage.Kind == "" {
		t.Error("the card says nothing at all about what this company's " +
			"agents do and do not hear about")
	}
	if phase := integration.Classify(res.Findings()).Phase; phase != integration.PhaseReady {
		t.Errorf("the surface classifies as %s over agents that are working", phase)
	}
	// AND WITH NO APPS IT IS STILL A BLOCK, which is the other half: that
	// company asked for a hook, has nothing to register one with, and no
	// agent receiving anything either.
	bare, err := github.Reconcile(context.Background(), github.Options{
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s",
			Provisioning: &config.GitHubProvisioning{
				Org: "crewbed", OrgWebhook: config.ContainerWebhookRequire,
			},
		},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var blocked bool
	for _, f := range bare.Findings() {
		if f.Kind == integration.FindingIngressBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("a company that demanded a hook, cannot register one and has "+
			"no app anywhere reports %+v", bare.Findings())
	}
}
