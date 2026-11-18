package gitlab

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// Naming shared by the provisioning and the engine's reads.
//
// A provisioned seat's token is the engine's: it is sealed under the name
// the engine derives for the seat (GITLAB_TOKEN_<HANDLE>) by per-seat access
// ([Vendor]), and no document names it. A company that does not provision
// GitLab (D4b) still gives each seat its own token, an external credential,
// under one of [CredentialKeys] in its `mcp_env.gitlab` block, and the engine
// reads it from there for the seat's tools and its identity.

// CredentialKeys are the spellings an external seat token arrives under in
// `mcp_env.gitlab`, in the order they are tried: the glab CLI reads
// GITLAB_TOKEN, an HTTP MCP server a Private-Token header. Exported so every
// reader tries the same list.
var CredentialKeys = []string{
	"GITLAB_TOKEN",
	"GITLAB_PERSONAL_ACCESS_TOKEN",
	"Private-Token",
	"Authorization",
}

// SeatEnv is the mcp_env server whose credentials belong to the code host.
//
// FROM THE ORG MODEL'S OWN TABLE: see [org.EngineReadMCPEnv].
const SeatEnv = org.MCPEnvGitLab

// GITLAB NAMES THE ADDRESS, AND THE ENGINE MUST NOT.
//
// `email` is optional on both service-account routes, and omitting it has
// GitLab generate one under its own noreply domain. A custom address "requires
// confirmation before the account is active, unless the group has a matching
// verified domain": a derived `…@noreply.crewlet.invalid` (reserved TLD, RFC
// 2606) can never be confirmed or verified, so the account stays inactive and
// every token it holds is refused with `403 Your primary email address is not
// confirmed`, looping mint and refusal (see [Client.Tokens]).
//
// A GENERATED ADDRESS IS STILL A ROBOT'S, and one the instance can keep.

// Username is the service-account name for a seat.
//
// FROM THE HANDLE, which is immutable (ADR-0013): GitLab keeps no field of
// this engine's own, so this name is how the next pass recognises the account
// it made.
func Username(p *config.GitLabProvisioning, handle string) string {
	prefix := strings.TrimSpace(p.UsernamePrefix)
	if prefix == "" {
		prefix = "crewlet"
	}
	return prefix + "-" + handle
}
