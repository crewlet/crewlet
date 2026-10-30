package github

import (
	"slices"
	"strings"
)

// How much of a repository one agent may do.
//
// THREE TIERS, and the same three the control plane's console offers, because
// an operator who sets a seat to Review there and reads "Review" here is
// looking at one decision rather than two that happen to share a word. The
// vocabulary is the contract; the permission maps below are this engine's
// reading of it.

// Tier is how much of a repository an agent may do.
type Tier string

const (
	// TierReadOnly reads code and issues and changes nothing.
	TierReadOnly Tier = "read_only"

	// TierReview reads the code and writes ABOUT it: issues, pull request
	// comments, reviews. Deliberately not the code itself, so a reviewer
	// cannot change what it is reviewing.
	TierReview Tier = "review"

	// TierFullAccess branches, commits, opens pull requests and issues and
	// writes checks. Never administration.
	TierFullAccess Tier = "full_access"
)

// DefaultTier is what a seat gets when its configuration is silent.
//
// THE LEAST ACCESS, because the silent case is a company that has not thought
// about this seat yet, and the cost of guessing low is an agent that asks for
// more. The cost of guessing high is an agent that can push to a branch
// nobody meant it to touch.
const DefaultTier = TierReadOnly

// Tiers is the closed set, for a form's picker and for validation, so a value
// the form offers and a value the config accepts cannot diverge.
var Tiers = []Tier{TierReadOnly, TierReview, TierFullAccess}

// ParseTier reads a tier, reporting whether it was one.
//
// It falls back to the default rather than failing, so a typo costs an agent
// access rather than costing the company its whole configuration. The second
// result is what lets a validator refuse the typo anyway, at the one moment a
// person is there to read the complaint.
func ParseTier(raw string) (Tier, bool) {
	value := Tier(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, "-", "_"))))
	switch {
	case value == "":
		return DefaultTier, true
	case slices.Contains(Tiers, value):
		return value, true
	default:
		return DefaultTier, false
	}
}

// Label is the tier as a person writes it.
func (t Tier) Label() string {
	switch t {
	case TierReadOnly:
		// HYPHENATED, which is the control plane's spelling and so the
		// one on the roster this screen is the counterpart of. The
		// tier-picker in the console writes "Read only"; the pill beside
		// an agent's name writes "Read-only", and two engines labelling
		// one agent differently is what a reader notices first.
		return "Read-only"
	case TierReview:
		return "Review"
	case TierFullAccess:
		return "Full access"
	default:
		return string(t)
	}
}

// Hint is the one line a form puts under the tier, matching the console's.
func (t Tier) Hint() string {
	switch t {
	case TierReadOnly:
		return "Reads code and issues, changes nothing."
	case TierReview:
		return "Reads code and writes about it: issues, comments, reviews."
	case TierFullAccess:
		return "Branches, commits, pull requests, issues and checks. No administration."
	default:
		return ""
	}
}

// Permissions is what this tier asks for on an installation token, in
// GitHub's own permission keys and levels.
//
// AN ALLOW LIST, because GitHub's token endpoint takes the permissions to
// grant: anything absent is simply not on the token. There is no catalogue to
// subtract from and so nothing to forget, which is the opposite of a deny
// list and much the safer direction to be wrong in.
//
// `metadata` is on every tier because GitHub requires it for almost every
// read. Without it a token cannot resolve a repository at all.
//
// `actions` IS READ ON EVERY TIER, and never write. Read is what lets an
// agent see why a check failed and read the job's log, which is most of what
// fixing a red pull request takes. Write re-runs, cancels and dispatches
// workflows, a deploy among them, and starting a release is a person's act
// here, never an agent's.
//
// `workflows` is on no tier. It is what lets a push change a file under
// `.github/workflows`, and a full-access seat opts in to it on its own
// ([Access]).
func (t Tier) Permissions() map[string]string {
	switch t {
	case TierReview:
		return map[string]string{
			"metadata": "read",
			// READ, on purpose. A reviewer writes about the code and not
			// to it, so it cannot change what it is reviewing.
			"contents":      "read",
			"issues":        "write",
			"pull_requests": "write",
			"checks":        "read",
			"actions":       "read",
			"deployments":   "read",
		}
	case TierFullAccess:
		return map[string]string{
			"metadata":      "read",
			"contents":      "write",
			"issues":        "write",
			"pull_requests": "write",
			"checks":        "write",
			"actions":       "read",
			"deployments":   "read",
		}
	default:
		return map[string]string{
			"metadata":      "read",
			"contents":      "read",
			"issues":        "read",
			"pull_requests": "read",
			"checks":        "read",
			"actions":       "read",
			"deployments":   "read",
		}
	}
}

// Access is what one seat's tokens may do: its tier, and the one grant no
// tier carries by itself.
//
// WORKFLOWS ARE AN OPT-IN AND NOT A TIER, because GitHub refuses any push
// whose commits touch `.github/workflows` from a token without it, whatever
// else the token holds, and the same grant is what lets an agent's branch run
// a workflow it rewrote. A modified workflow runs on that branch with
// whatever secrets the repository hands its runs, so it is a decision about
// the repository's secrets as much as about its code, and a company takes it
// per seat rather than inheriting it with a tier.
//
// Only full access carries it. Validation refuses the opt-in on any other
// tier, and [Access.WritesWorkflows] asks the tier again, so a document a
// validator never saw still cannot hand workflow writes to a seat that cannot
// write code.
type Access struct {
	Tier Tier

	// Workflows is the seat's opt-in to `workflows: write`.
	Workflows bool
}

// WritesWorkflows reports whether this access carries `workflows: write`:
// opted in, and at full access.
func (a Access) WritesWorkflows() bool {
	return a.Workflows && a.Tier == TierFullAccess
}

// Permissions is what this access asks for: the tier's own list, and
// `workflows: write` where [Access.WritesWorkflows] holds.
//
// ONE LIST FOR EVERY ASKER. The manifest an app is created from, the token
// minted for it and the installation it is checked against all read this, so
// an app created for an opted-in seat holds exactly what its tokens ask for.
func (a Access) Permissions() map[string]string {
	perms := a.Tier.Permissions()
	if a.WritesWorkflows() {
		perms[PermissionWorkflows] = "write"
	}
	return perms
}

// Denied is what an agent's app must never hold, at any tier.
//
// NOT WHAT LIMITS AN AGENT: the tiers above already exclude these by
// omission, and a token is only ever minted with a tier's own list. This is
// what an INSTALLATION is checked against, because the permissions an app
// holds are not the engine's to decide once the app exists. A manifest can be
// edited before it is submitted, and an operator can widen an installation
// afterwards, so the engine reports an app holding any of these rather than
// assuming its own manifest is what got created.
//
// Three groups, and each is a way to escape the tier rather than to exceed
// it. Administration deletes a repository or drops branch protection. Secrets
// and variables hand over every credential the repository holds. Membership
// and organization settings are how an agent would widen its own access.
var Denied = []string{
	"administration",
	"organization_administration",
	"organization_secrets",
	"organization_self_hosted_runners",
	"organization_user_blocking",
	"members",
	"organization_plan",
	"secrets",
	"actions_variables",
	"organization_actions_variables",
	"environments",
}

// Excess is the denied permissions an installation actually holds.
//
// Sorted, so two runs over one installation produce the same sentence and a
// status line does not change every pass.
func Excess(held map[string]string) []string {
	var out []string
	for _, name := range Denied {
		if level := strings.TrimSpace(held[name]); level != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Shortfall is what an access needs and an installation does not hold.
//
// The other half of the same question, and the one an operator can act on: an
// app installed with fewer permissions than its access asks for cannot mint
// the token that access asks for at all, because GitHub refuses a token
// request naming a permission the installation was not granted, and says why
// only at the token endpoint.
func Shortfall(access Access, held map[string]string) []string {
	var out []string
	for name, want := range access.Permissions() {
		got := strings.TrimSpace(held[name])
		if got == "" || (want == "write" && got == "read") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
