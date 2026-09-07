package confluence

// Where a seat's own Confluence credential lives in its mcp_env.
//
// EXPORTED, and read by both the runtime that authenticates a search as that
// seat and the setup surface that reports whether the seat has one. They were
// private to internal/engine, which meant the screen listing each agent's
// account either got its own copy of these spellings or could not answer at
// all. A second copy of a list like this does not stay equal to the first.

// SeatEnvs are the mcp_env servers a seat's own Confluence credential can
// live under, in the order they are tried.
//
// The same two the tracker reads, because it is the same Atlassian identity
// and the community MCP server covers both products under one entry.
//
//nolint:gochecknoglobals // an immutable list, not state
var SeatEnvs = []string{"atlassian", "confluence"}

// CredentialKeys are the spellings a seat's token arrives under.
//
// A seat WITH one searches as itself and Confluence enforces its own page
// ACLs; a seat without one falls back to the org account, and an unscoped
// search is then refused.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{
	"CONFLUENCE_API_TOKEN", "CONFLUENCE_PERSONAL_TOKEN",
	"CONFLUENCE_TOKEN", "ATLASSIAN_API_TOKEN",
}

// EmailKeys are the spellings a seat's account address arrives under.
//
// Needed because Atlassian Cloud authenticates an API token as Basic
// base64(email:token) and rejects it as a bearer: the same credential,
// refused purely on which scheme carried it.
//
//nolint:gochecknoglobals // an immutable list, not state
var EmailKeys = []string{
	"CONFLUENCE_USERNAME", "CONFLUENCE_EMAIL", "ATLASSIAN_EMAIL",
}
