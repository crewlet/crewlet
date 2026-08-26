package github

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// Which seats hold a code-host identity, and where that identity lives.
//
// # The engine names no variable of its own
//
// A seat's GitHub credential lives in its `mcp_env.github` block, under
// whichever key that seat's tool stack reads — the official MCP server takes
// an Authorization header, the `gh` CLI reads GH_TOKEN, most everything else
// reads GITHUB_TOKEN. So the scan looks under the keys the tools already use
// rather than inventing CREWLET_GITHUB_TOKEN_<seat>, which would be a
// variable nothing reads.
//
// ONE list, exported, read by both the engine's identity resolution and the
// provisioner's report. A second copy beside either of them is how the two
// come to look under different keys — and the failure is silent, because a
// seat whose credential was not found looks exactly like a seat that has
// none.

// SeatEnv is the mcp_env server whose credentials belong to the code host.
const SeatEnv = "github"

// CredentialKeys are the spellings a seat's token arrives under, in the
// order they are tried.
var CredentialKeys = []string{
	"GITHUB_TOKEN",
	"GITHUB_PERSONAL_ACCESS_TOKEN",
	"GH_TOKEN",
	"Authorization",
}

// CredentialOf reads a seat's code-host credential.
//
// The resolver arrives as a plain function rather than the config resolver
// itself, so this package stays out of the config import graph and the scan
// is testable against a map.
//
// A HUMAN seat holds none and must never be looked up as though it did: it
// is addressable through its own contact block — `github_login` — which the
// party registry registers from the org.
func CredentialOf(seat *org.Role, value func(string) string) string {
	if seat == nil || seat.IsHuman() || value == nil {
		return ""
	}
	block := seat.MCPEnv[SeatEnv]
	for _, key := range CredentialKeys {
		raw := strings.TrimSpace(value(block[key]))
		if raw == "" {
			continue
		}
		// "Authorization: Bearer <token>" carries the credential behind a
		// scheme. Stripping it is what lets one config shape work through
		// both an HTTP MCP server and this lookup. GitHub's older `token`
		// scheme is stripped too: it is the same credential wearing the
		// word the API used to want.
		raw = strings.TrimPrefix(raw, "Bearer ")
		raw = strings.TrimPrefix(raw, "token ")
		return strings.TrimSpace(raw)
	}
	return ""
}

// SeatCredentials are the distinct credentials the company's agent seats
// hold, sorted.
//
// DISTINCT because several seats may legitimately share one — a company
// mid-migration, or one that has not provisioned per-seat accounts yet — and
// resolving the same token once per seat would spend N requests to learn one
// answer. Sorted so a boot's log lines are diffable against the next one's.
func SeatCredentials(o *org.Organization, value func(string) string) []string {
	if o == nil {
		return nil
	}
	tokens := map[string]bool{}
	for seat := range o.AllRoles() {
		if token := CredentialOf(seat, value); token != "" {
			tokens[token] = true
		}
	}
	return slices.Sorted(maps.Keys(tokens))
}

// Target is one place a webhook can be registered: an organization, or a
// single repository.
type Target struct {
	// Org is set for an organization-level hook.
	Org string
	// Owner and Repo are set for a repository hook.
	Owner, Repo string
}

// IsOrg reports an organization-level target.
func (t Target) IsOrg() bool { return t.Org != "" }

// String is the target as an operator would write it.
func (t Target) String() string {
	if t.IsOrg() {
		return t.Org
	}
	return t.Owner + "/" + t.Repo
}

// TargetsOf is every repository the provisioning block names, in config
// order with duplicates removed.
//
// The ORGANIZATION IS NOT ONE OF THEM. Whether the org gets its own hook is
// a decision the reconcile makes from the mode and what the credential may
// do, so folding it in here would make the list mean two different things
// depending on a value this function cannot see.
func TargetsOf(pv *config.GitHubProvisioning) []Target {
	if pv == nil {
		return nil
	}
	var (
		out  []Target
		seen = map[string]bool{}
	)
	for _, entry := range pv.Repos {
		owner, name, ok := strings.Cut(strings.TrimSpace(entry), "/")
		owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
		if !ok || owner == "" || name == "" {
			// The config validator refuses this shape, so reaching it
			// means a caller built the block by hand. Skipped rather
			// than guessed: "owner" alone names nothing on GitHub.
			continue
		}
		key := strings.ToLower(owner + "/" + name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Target{Owner: owner, Repo: name})
	}
	return out
}
