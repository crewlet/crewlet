package mcp

import "strings"

// THE per-role MCP instance grammar.
//
// A `shared: false` server is a TEMPLATE, not a process. Every role that
// declares mcp_env for it gets its own child, launched with that seat's
// credentials in its environment (stdio) or its headers (HTTP). Two roles must
// never share one child: the credentials in it ARE one seat's identity, and a
// shared process would let either seat act as the other in Jira, Slack, GitHub
// — invisibly, because the tool call looks identical from the engine's side.
//
// The instance name is what keeps them apart, and it is load-bearing in three
// places at once: it keys the bridge's client index, it names the child in
// every log line, and — stripped back down by ServerName — it is the BARE name
// the model sees and types into list_mcp_server_tools. Producer and consumer
// disagreeing about it does not raise anything; the lookup just misses, and a
// server the prompt advertises answers "not configured for this role".
//
// So it is defined once, here, and a guard test fails the build on a
// hand-built "::" anywhere else in this package. That is the same rule the
// queue's topic grammar carries, for the same reason.
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
