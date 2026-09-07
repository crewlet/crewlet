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
		return "Read only"
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

// Shortfall is what a tier needs and an installation does not hold.
//
// The other half of the same question, and the one an operator can act on: an
// app installed with fewer permissions than its tier asks for mints tokens
// that are refused by the calls the tier exists to allow, and GitHub says so
// only at the call site.
func Shortfall(tier Tier, held map[string]string) []string {
	var out []string
	for name, want := range tier.Permissions() {
		got := strings.TrimSpace(held[name])
		if got == "" || (want == "write" && got == "read") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
