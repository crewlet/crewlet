package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

// A SURFACE THAT AUTHENTICATED WITH NOBODY DOES NOT REPORT READY.
//
// `integrations.github.token` is optional and the connect form does not ask
// for it, both deliberately: routing needs nothing from it, because each
// agent's own app answers who is participating in a thread. What it IS still
// needed for is the one thing `provisioning` asks for — a hook on an
// organization or on a list of repositories — and with no token the pass
// reads nothing and writes nothing.
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
			Provisioning:  &config.GitHubProvisioning{Org: "crewbed"},
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
			!strings.Contains(f.Detail, "provisioning") {
			t.Errorf("the finding names no way out:\n%s", f.Detail)
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

// AND A COMPANY THAT ASKED FOR NO HOOK IS STILL SILENT.
//
// Each agent's own app carries its own webhook in its own manifest, so a
// company with no `provisioning` block wants no organization-wide hook at
// all — that is how the connect form sets GitHub up. Reporting one there
// would put a permanent finding on the ordinary shape.
func TestAPassAskedForNoHookReportsNoRegistrar(t *testing.T) {
	t.Parallel()
	res, err := github.Reconcile(context.Background(), github.Options{
		Config:      &config.GitHub{Enabled: true, WebhookSecret: "s"},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range res.Findings() {
		if f.Subject == "integrations.github.token" {
			t.Errorf("a company that asked for no hook was told it has no "+
				"credential to register one with:\n%s", f.Detail)
		}
	}
}

// AN AGENT WITH NO APP IS ACTION REQUIRED, NOT A GREEN CARD.
//
// The engine builds its seat list from the seats carrying an
// `integrations.github` block, because that block is where an app's id, slug
// and key are recorded — so a seat that has never had one is absent from the
// pass's input and the "no app" arm cannot be reached for it. The card read
// Connected over an agent that could do nothing.
//
// APPROVAL_REQUIRED, so the phase is awaiting_admin and the actor is a
// person: the engine can do nothing at all until somebody creates the app at
// GitHub, and "Setting up agents" would wait for an act nobody is performing.
func TestAgentsWithNoAppAreReportedAsNeedingAPerson(t *testing.T) {
	t.Parallel()
	if github.SeatsWithNoApp(nil) != nil {
		t.Error("a company whose every agent has an app reported one anyway")
	}
	f := github.SeatsWithNoApp([]string{"sre-lead"})
	if f == nil {
		t.Fatal("an agent with no app of its own was reported as nothing")
	}
	if f.Kind != integration.FindingApprovalRequired {
		t.Errorf("reported as %s, want approval_required: the engine can "+
			"never create an app, so no phase owed to the engine is honest",
			f.Kind)
	}
	if phase, actor := f.Kind.Verdict(); phase == integration.PhaseReady ||
		actor == integration.ActorEngine {
		t.Errorf("verdict is %s/%s, which reads as nothing to do", phase, actor)
	}
	if !strings.Contains(f.Detail, "sre-lead") {
		t.Errorf("the finding does not name the agent:\n%s", f.Detail)
	}
}

// AND MANY OF THEM ARE ONE FINDING, WITH THE WHOLE LIST REACHABLE.
//
// Creating an app is the same act for every seat that has none, so N of them
// is one sentence rather than N rows on the card. The sentence names three,
// because `detail` is the card's status line and the engine caps it — a
// fifty-agent company would otherwise be a wall cut off mid-handle — and the
// rest travels in Subjects.
func TestManyAgentsWithNoAppAreOneReadableFinding(t *testing.T) {
	t.Parallel()
	handles := []string{"a", "b", "c", "d", "e", "f"}
	f := github.SeatsWithNoApp(handles)
	if f == nil {
		t.Fatal("six agents with no app reported nothing")
	}
	if len(f.Detail) > integration.MaxDetailLength {
		t.Errorf("the sentence is %d characters, over the cap that would cut "+
			"it mid-handle", len(f.Detail))
	}
	// COUNTED OVER THE SUBJECTS THEMSELVES, not over the separators: the
	// closing instruction has commas of its own.
	named := 0
	for _, handle := range handles {
		if strings.Contains(f.Detail, " "+handle+",") ||
			strings.Contains(f.Detail, " "+handle+" ") {
			named++
		}
	}
	if named > integration.ListedExamples {
		t.Errorf("the sentence names %d of the %d handles, so it is becoming "+
			"the list rather than naming examples of it:\n%s",
			named, len(handles), f.Detail)
	}
	if named == 0 {
		t.Errorf("the sentence names none of them, so a reader cannot tell "+
			"which agents it is about:\n%s", f.Detail)
	}
	if !strings.Contains(f.Detail, "3 more") {
		t.Errorf("the sentence does not say how many it left out:\n%s", f.Detail)
	}
	if len(f.Subjects) != len(handles) {
		t.Errorf("Subjects = %v, want every handle: the ones the sentence "+
			"leaves out are then unreachable from any screen", f.Subjects)
	}
}
