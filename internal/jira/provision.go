package jira

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Which seats hold a tracker identity, and where that identity lives.
//
// # The engine names no variable of its own
//
// A seat's Jira credential lives in its `mcp_env.jira` block, under whichever
// key that seat's tool stack reads — the community Atlassian MCP servers want
// JIRA_API_TOKEN beside JIRA_USERNAME, an HTTP server takes an Authorization
// header. So the scan looks under the keys the tools already use rather than
// inventing CREWLET_JIRA_TOKEN_<seat>, which would be a variable nothing
// reads.
//
// ONE list, exported, read by both the engine's identity resolution and the
// reconcile's report. A second copy beside either of them is how the two come
// to look under different keys — and the failure is silent, because a seat
// whose credential was not found looks exactly like a seat that has none.

// SeatEnvs are the mcp_env servers whose credentials belong to the tracker,
// in the order they are tried.
//
// TWO, because Atlassian's own MCP server covers Jira AND Confluence, so the
// documented entry for it is named `atlassian` and holds one credential for
// both products. A deployment running a Jira-only server names it `jira`
// instead. Looking under one name only would find nothing on half the
// configs that exist — silently, because a seat whose credential was not
// found is indistinguishable from a seat that has none.
var SeatEnvs = []string{"atlassian", "jira"}

// CredentialKeys are the spellings a seat's token arrives under, in the
// order they are tried.
var CredentialKeys = []string{
	"JIRA_API_TOKEN",
	"JIRA_PERSONAL_TOKEN",
	"JIRA_TOKEN",
	"ATLASSIAN_API_TOKEN",
	"Authorization",
}

// EmailKeys are the spellings a seat's account address arrives under.
//
// Needed because Jira Cloud authenticates an API token as Basic
// base64(email:token) and rejects it as a bearer — the same credential,
// refused purely on which scheme carried it. A seat with a token and no
// address is a Data Center PAT, which is the bearer case.
var EmailKeys = []string{
	"JIRA_USERNAME",
	"JIRA_EMAIL",
	"ATLASSIAN_EMAIL",
}

// Credential is a seat's own tracker identity, as its tools hold it.
type Credential struct {
	Token string
	// Email empty is meaningful: it selects bearer authentication, which
	// is what a Data Center personal access token wants.
	Email string
}

// Held reports a credential there is something to authenticate with.
func (c Credential) Held() bool { return c.Token != "" }

// CredentialOf reads a seat's tracker credential.
//
// The resolver arrives as a plain function rather than the config resolver
// itself, so this package stays out of the config import graph and the scan
// is testable against a map.
//
// A HUMAN seat holds none and must never be looked up as though it did: it
// is addressable through its own contact block, which the party registry
// registers from the org.
func CredentialOf(seat *org.Role, value func(string) string) Credential {
	if seat == nil || seat.IsHuman() || value == nil {
		return Credential{}
	}
	var cred Credential
	block := seatBlock(seat, value)
	for _, key := range CredentialKeys {
		raw := strings.TrimSpace(value(block[key]))
		if raw == "" {
			continue
		}
		// "Authorization: Bearer <pat>" carries the credential behind a
		// scheme. Stripping it is what lets one config shape work through
		// both an HTTP MCP server and this lookup. A Basic header is left
		// alone: its payload is already email:token and re-encoding it
		// would produce a credential that authenticates as nobody.
		cred.Token = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
		break
	}
	for _, key := range EmailKeys {
		if address := strings.TrimSpace(value(block[key])); address != "" {
			cred.Email = address
			break
		}
	}
	return cred
}

// seatBlock is the first mcp_env server that holds a tracker token.
//
// The BLOCK is chosen by whether it holds a token, not by name order alone:
// a company with both entries has the credential in exactly one of them, and
// picking the empty one by position would report a seat with a working
// credential as having none.
func seatBlock(seat *org.Role, value func(string) string) map[string]string {
	for _, name := range SeatEnvs {
		block := seat.MCPEnv[name]
		for _, key := range CredentialKeys {
			if strings.TrimSpace(value(block[key])) != "" {
				return block
			}
		}
	}
	return nil
}

// ProjectsOf lists every Jira project key the org declares, sorted and
// upper-cased.
//
// Units and seats both, because both can name one: a unit says which project
// it owns, and a root seat says where it files. The reconcile checks all of
// them, because a project key with a typo in it is a routing gap that
// produces no error anywhere — the webhook arrives, the key matches nothing,
// and the issue reaches nobody.
func ProjectsOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	seen := map[string]bool{}
	for u := range o.AllUnits() {
		if key := org.NormalizeScope(u.Project); key != "" {
			seen[key] = true
		}
	}
	for seat := range o.AllRoles() {
		if key := org.NormalizeScope(seat.Project); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
