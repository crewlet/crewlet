package atlassian

import (
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Which seats hold an Atlassian identity, and where that identity lives.
//
// # The engine names no variable of its own
//
// A seat's Atlassian credential lives in its `mcp_env` block, under whichever
// key that seat's tool stack reads — the community Atlassian MCP server wants
// JIRA_API_TOKEN beside JIRA_USERNAME, a Confluence-only server wants
// CONFLUENCE_API_TOKEN, an HTTP server takes an Authorization header. So the
// scan looks under the keys the tools already use rather than inventing
// CREWLET_ATLASSIAN_TOKEN_<seat>, which would be a variable nothing reads.
//
// # ONE grammar, and it lives here because it was two
//
// This was written twice: exported in `internal/jira` for the tracker's seat
// identities, and again unexported in `internal/engine` for Confluence's. The
// second copy had drifted — no `Authorization` key, no `Bearer ` strip, and
// it chose the mcp_env block by NAME ORDER rather than by which block holds a
// token, so a company with both an `atlassian` and a `jira` entry could have
// its credential found in one product and not the other. The failure is
// silent: a seat whose credential was not found is indistinguishable from a
// seat that has none. One list, one lookup, both products.

// SeatEnvs are the mcp_env servers whose credentials belong to Atlassian, in
// the order they are tried.
//
// THREE, because one Atlassian credential can be declared under any of them.
// Atlassian's own MCP server covers Jira AND Confluence, so the documented
// entry for it is named `atlassian`; a deployment running a single-product
// server names it `jira` or `confluence` instead. Looking under one name only
// would find nothing on most of the configs that exist.
var SeatEnvs = []string{"atlassian", "jira", "confluence"}

// TokenKeys are the spellings a seat's token arrives under, in the order they
// are tried.
//
// The union of both products' spellings, because one credential serves both
// and a seat that wrote only the Confluence spelling still holds a Jira
// identity. `Authorization` is last: it is the HTTP-MCP shape, and a config
// that carries a named key as well means the named one.
var TokenKeys = []string{
	"ATLASSIAN_API_TOKEN",
	"JIRA_API_TOKEN",
	"JIRA_PERSONAL_TOKEN",
	"JIRA_TOKEN",
	"CONFLUENCE_API_TOKEN",
	"CONFLUENCE_PERSONAL_TOKEN",
	"CONFLUENCE_TOKEN",
	"Authorization",
}

// EmailKeys are the spellings a seat's account address arrives under.
//
// Needed because Atlassian Cloud authenticates an API token as Basic
// base64(email:token) and rejects it as a bearer — the same credential,
// refused purely on which scheme carried it. A seat with a token and no
// address is a Data Center personal access token, which is the bearer case.
var EmailKeys = []string{
	"ATLASSIAN_EMAIL",
	"JIRA_USERNAME",
	"JIRA_EMAIL",
	"CONFLUENCE_USERNAME",
	"CONFLUENCE_EMAIL",
}

// Credential is a seat's own Atlassian identity, as its tools hold it.
//
// COMPARABLE on purpose: the engine caches a resolved account id by
// credential, and a struct of two strings is a map key without a hash of its
// own. A pointer or a slice field here would make that cache silently miss on
// every apply.
type Credential struct {
	Token string
	// Email empty is meaningful: it selects bearer authentication, which is
	// what a Data Center personal access token wants.
	Email string
}

// Held reports a credential there is something to authenticate with.
func (c Credential) Held() bool { return c.Token != "" }

// CredentialOf reads a seat's Atlassian credential.
//
// The resolver arrives as a plain function rather than the config resolver
// itself, so this package stays out of the config import graph and the scan
// is testable against a map.
//
// A HUMAN seat holds none and must never be looked up as though it did: it is
// addressable through its own contact block, which the party registry
// registers from the org.
func CredentialOf(seat *org.Role, value func(string) string) Credential {
	if seat == nil || seat.IsHuman() || value == nil {
		return Credential{}
	}
	var cred Credential
	block := SeatBlock(seat, value)
	for _, key := range TokenKeys {
		raw := strings.TrimSpace(value(block[key]))
		if raw == "" {
			continue
		}
		// "Authorization: Bearer <pat>" carries the credential behind a
		// scheme. Stripping it is what lets one config shape work through
		// both an HTTP MCP server and this lookup. A Basic header is left
		// alone: its payload is already email:token and re-encoding it
		// would produce a credential that authenticates as nobody.
		cred.Token = StripScheme(raw)
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

// SeatBlock is the first mcp_env server that holds an Atlassian token.
//
// The BLOCK is chosen by whether it holds a token, not by name order alone: a
// company with two entries has the credential in exactly one of them, and
// picking the empty one by position would report a seat with a working
// credential as having none.
//
// Exported because the provisioner writes into the same block it reads from —
// a scan that found the credential in one block and minted into another would
// leave the seat authenticating with the value nothing replaced.
func SeatBlock(seat *org.Role, value func(string) string) map[string]string {
	if seat == nil || value == nil {
		return nil
	}
	for _, name := range SeatEnvs {
		block := seat.MCPEnv[name]
		for _, key := range TokenKeys {
			if strings.TrimSpace(value(block[key])) != "" {
				return block
			}
		}
	}
	return nil
}

// StripScheme drops an Authorization scheme so the credential underneath is
// visible.
//
// `Authorization: Bearer ${VAR}` is a whole reference wearing a prefix. The
// engine strips the same prefix when it reads the value, so a scan that did
// not would report a config the engine is perfectly happy with as holding no
// credential — or, in the provisioner, as unprovisionable.
//
// A Basic header is deliberately left alone: its payload is already
// base64(email:token), so there is no ${VAR} under it to mint into and
// stripping the word would hand back a credential that authenticates as
// nobody.
func StripScheme(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "Bearer "))
}
