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
// `.github/workflows`, and a full-access seat opted in to it on its own
// ([TierPermissions]).
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
