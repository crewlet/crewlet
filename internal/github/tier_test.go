package github_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// A TYPO COSTS ONE AGENT ITS ACCESS, NOT THE COMPANY ITS CONFIGURATION.
//
// ParseTier falls back rather than failing, so a seat written `full-acess`
// runs read-only instead of taking the whole document down. The second result
// is what lets validation refuse it at the one moment somebody is there to
// read the complaint.
func TestATierTypoFallsBackAndIsStillReported(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]github.Tier{
		"":              github.DefaultTier,
		"read_only":     github.TierReadOnly,
		"full-access":   github.TierFullAccess,
		"  Review  ":    github.TierReview,
		"FULL_ACCESS":   github.TierFullAccess,
		"administrator": github.DefaultTier,
	} {
		got, ok := github.ParseTier(raw)
		if got != want {
			t.Errorf("ParseTier(%q) = %q, want %q", raw, got, want)
		}
		if known := want != github.DefaultTier || raw == "" || got == github.TierReadOnly && raw != "administrator"; !known && ok {
			t.Errorf("ParseTier(%q) reported a typo as known", raw)
		}
	}
	if _, ok := github.ParseTier("administrator"); ok {
		t.Error("a tier nobody defined was reported as known")
	}
	if _, ok := github.ParseTier(""); !ok {
		t.Error("silence is not a typo; it is the default")
	}
}

// A REVIEWER CANNOT CHANGE WHAT IT IS REVIEWING. The tier writes about the
// code and never to it, which is the whole distinction between it and full
// access, and it lives in one map entry.
func TestReviewWritesAboutTheCodeAndNotToIt(t *testing.T) {
	t.Parallel()
	review := github.TierReview.Permissions()
	if review["contents"] != "read" {
		t.Errorf("review holds contents=%q, want read", review["contents"])
	}
	if review["pull_requests"] != "write" {
		t.Errorf("review cannot write a pull request comment: %q", review["pull_requests"])
	}
	if full := github.TierFullAccess.Permissions(); full["contents"] != "write" {
		t.Errorf("full access holds contents=%q, want write", full["contents"])
	}
	if ro := github.TierReadOnly.Permissions(); ro["issues"] != "read" {
		t.Errorf("read only holds issues=%q, want read", ro["issues"])
	}
}

// EVERY TIER CARRIES metadata, because GitHub refuses almost every read
// without it: a token that omits it cannot even resolve a repository, and the
// failure arrives at the call site naming nothing.
func TestEveryTierCarriesMetadata(t *testing.T) {
	t.Parallel()
	for _, tier := range github.Tiers {
		if got := tier.Permissions()["metadata"]; got != "read" {
			t.Errorf("%s holds metadata=%q, want read", tier, got)
		}
	}
}

// NO TIER ASKS FOR ANYTHING DENIED. The two lists are written separately and
// would drift silently: the denied list is what an installation is checked
// against, so a tier quietly asking for one of them would be reported as
// excess on every pass while being exactly what the engine requested.
func TestNoTierAsksForADeniedPermission(t *testing.T) {
	t.Parallel()
	for _, tier := range github.Tiers {
		for name := range tier.Permissions() {
			if slices.Contains(github.Denied, name) {
				t.Errorf("%s asks for %q, which is denied at every tier", tier, name)
			}
		}
	}
}

// AN INSTALLATION IS NOT THE MANIFEST. A person can widen one after the fact,
// and a manifest can be edited before it is submitted, so what the app HOLDS
// is read back and compared rather than assumed.
func TestExcessAndShortfallReadTheInstallationRatherThanTheManifest(t *testing.T) {
	t.Parallel()
	held := map[string]string{
		"metadata":       "read",
		"contents":       "read",
		"issues":         "read",
		"administration": "write",
		"secrets":        "read",
	}
	excess := github.Excess(held)
	if !slices.Equal(excess, []string{"administration", "secrets"}) {
		t.Errorf("Excess = %v, want administration and secrets", excess)
	}
	// A review seat on that installation cannot write what it was set up
	// to write, and GitHub would only say so by refusing its every token.
	short := tierPerms("review", false).Short(held)
	if !slices.Contains(short, "pull_requests") || !slices.Contains(short, "issues") {
		t.Errorf("Short = %v, want the writes review needs", short)
	}
	if slices.Contains(short, "contents") {
		t.Error("review wants contents=read and the installation has it")
	}
	if got := tierPerms("read_only", false).Short(map[string]string{
		"metadata": "read", "contents": "read", "issues": "read",
		"pull_requests": "read", "checks": "read", "actions": "read", "deployments": "read",
	}); len(got) != 0 {
		t.Errorf("a fully granted read-only installation reports a shortfall: %v", got)
	}
}

// WORKFLOW WRITES ARE AN OPT-IN OF FULL ACCESS AND OF NOTHING ELSE.
//
// GitHub refuses a push touching `.github/workflows` without them, and the
// same grant lets a branch run a workflow it rewrote with the repository's
// secrets, so no tier carries them by default and a seat that cannot write
// code cannot be handed them by a document a validator never saw.
func TestWorkflowWritesAreAFullAccessOptInOnly(t *testing.T) {
	t.Parallel()
	for _, tier := range github.Tiers {
		if got := tier.Permissions()[github.PermissionWorkflows]; got != "" {
			t.Errorf("%s carries workflows=%q with nobody opting in", tier, got)
		}
		if got := (github.Access{Tier: tier}).Permissions()[github.PermissionWorkflows]; got != "" {
			t.Errorf("%s without the opt-in carries workflows=%q", tier, got)
		}
		opted := github.Access{Tier: tier, Workflows: true}
		got := opted.Permissions()[github.PermissionWorkflows]
		switch tier {
		case github.TierFullAccess:
			if got != "write" || !opted.WritesWorkflows() {
				t.Errorf("an opted-in full_access seat carries workflows=%q", got)
			}
		default:
			if got != "" || opted.WritesWorkflows() {
				t.Errorf("an opted-in %s seat carries workflows=%q", tier, got)
			}
		}
		// THE TIER'S OWN LIST IS UNCHANGED by the opt-in, so opting in
		// adds one grant and widens nothing else.
		for name, level := range tier.Permissions() {
			if opted.Permissions()[name] != level {
				t.Errorf("%s: the opt-in moved %s from %q to %q", tier, name, level, opted.Permissions()[name])
			}
		}
	}
	if slices.Contains(github.Denied, github.PermissionWorkflows) {
		t.Error("workflows is a grant a seat may opt in to, and it is on the denied list")
	}
}

// ACTIONS ARE READ ON EVERY TIER AND WRITTEN ON NONE.
//
// Read is how an agent learns why its pull request is red. Write re-runs and
// dispatches workflows, a deploy among them, which is a person's act.
func TestEveryTierReadsActionsAndNoneWritesThem(t *testing.T) {
	t.Parallel()
	for _, tier := range github.Tiers {
		access := github.Access{Tier: tier, Workflows: true}
		if got := access.Permissions()["actions"]; got != "read" {
			t.Errorf("%s holds actions=%q, want read", tier, got)
		}
	}
}

// A SEAT AT WORKFLOWS WHOSE INSTALLATION LACKS IT IS SHORT OF IT, which is
// how an app registered before the raise is told apart from one that is
// complete.
func TestAWorkflowsGrantTheInstallationDoesNotHoldIsShort(t *testing.T) {
	t.Parallel()
	held := tierPerms("full_access", false).Token()
	want := tierPerms("full_access", true)
	if got := want.Short(held); !slices.Equal(got, []string{github.PermissionWorkflows}) {
		t.Errorf("Short = %v, want workflows alone", got)
	}
	held[github.PermissionWorkflows] = "write"
	if got := want.Short(held); len(got) != 0 {
		t.Errorf("an installation holding workflows reports a shortfall: %v", got)
	}
	// A READ GRANT IS NOT THE WRITE the seat asks for.
	held[github.PermissionWorkflows] = "read"
	if got := want.Short(held); !slices.Equal(got, []string{github.PermissionWorkflows}) {
		t.Errorf("Short over workflows=read = %v, want workflows", got)
	}
}
