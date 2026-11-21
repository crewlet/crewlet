package mcp

import "strings"

// THE per-role MCP instance grammar.
//
// A `shared: false` server is a TEMPLATE: every role declaring mcp_env for it
// gets its own child with that seat's credentials. Two roles must never share
// one, since the credentials ARE the seat's identity and a shared process
// would let either act as the other, invisibly.
//
// The instance name keeps them apart: it keys the bridge's client index, names
// the child in logs, and, stripped by ServerName, is the BARE name the model
// types into list_mcp_server_tools. A disagreement raises nothing; the lookup
// misses. So it is defined once, here, and TestInstanceSeparatorHasOneDefinition
// fails on a hand-built "::" elsewhere in this package.
const instanceSeparator = "::"

// InstanceName builds the per-role instance name for a server.
//
// Spaces in the role name become underscores. The role name reaches this from
// YAML, where "Senior Dev" and "Product Manager" are ordinary, and the result
// is used as a process label and a map key that operators read in logs.
func InstanceName(server, role string) string {
	return server + instanceSeparator + strings.ReplaceAll(role, " ", "_")
}

// ServerName strips the per-role suffix from an instance name.
//
// The model never sees an instance name. The catalogue in its system prompt
// lists servers by the BARE name from the mcp_env keys ("github"), and that is
// what it types into list_mcp_server_tools — so every surface that buckets
// tools by server has to strip the suffix first. A shared server, which has no
// suffix, passes through unchanged.
func ServerName(instance string) string {
	base, _, found := strings.Cut(instance, instanceSeparator)
	if !found {
		return instance
	}
	return base
}

// RoleOfInstance returns the role part of an instance name, or "" for a shared
// server. It is the inverse half of InstanceName and exists so nothing else
// has to split on the separator to answer "whose child is this?".
func RoleOfInstance(instance string) string {
	_, role, found := strings.Cut(instance, instanceSeparator)
	if !found {
		return ""
	}
	return role
}
