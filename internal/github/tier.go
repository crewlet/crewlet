package github

import (
	"slices"
	"strings"
)

// The three tiers a seat's GitHub access was once stored as.
//
// RETIRED: a seat now stores GitHub's own permissions ([Permissions]), and
// nothing grants by tier. What remains here is what the migration reads
// ([TierPermissions]): the closed set and each tier's permission list.
// [Denied] and [Excess] are not about tiers at all and are kept here beside
// the lists they were written against.

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
//
// EXACT: each tier in its one spelling, with no case, space or hyphen folded.
// A tier comes from a seat's role.code.access, which takes exactly these
// three, so a reading that folded would be a second, more lenient idea of
// what a tier is — and the one place a value the field refuses could still be
// read as a tier rather than as the default.
func ParseTier(raw string) (Tier, bool) {
	value := Tier(raw)
	switch {
	case value == "":
		return DefaultTier, true
	case slices.Contains(Tiers, value):
		return value, true
	default:
		return DefaultTier, false
	}
}

// Permissions is what this tier asks for on an installation token, in
// GitHub's own permission keys and levels.
//
// AN ALLOW LIST, because GitHub's token endpoint takes the permissions to
// grant: anything absent is not on the token, the safer direction to be
// wrong in. `metadata` is on every tier: GitHub needs it to resolve a repo.
//
// `actions` IS READ ON EVERY TIER, never write: read shows why a check failed;
// write re-runs, cancels and dispatches workflows (a release is a person's act).
//
// `workflows` is on no tier: it lets a push change a file under
// `.github/workflows`, and a full-access seat opts in alone ([TierPermissions]).
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

// Denied is what an agent's app must never hold, at any tier.
//
// NOT WHAT LIMITS AN AGENT: a token is only minted with a tier's own list.
// This is what an INSTALLATION is checked against, since a manifest can be
// edited before submission and an operator can widen an installation
// afterwards, so the engine reports an app holding any of these rather than
// assuming its own manifest got created.
//
// Three groups, each a way to escape the tier: administration (deleting a
// repository, dropping branch protection), secrets and variables (every
// credential the repository holds), and membership and organization settings
// (widening its own access).
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
