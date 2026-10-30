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
// A service account was created with a derived `crewlet-<handle>@
// noreply.crewlet.invalid`, on the reasoning that GitLab requires an address,
// that it must be unique per account, and that `noreply.` says "this mailbox
// does not exist" — which is right for a robot. Two of those three are false
// and the third is what broke it.
//
// GitLab does NOT require one: `email` is optional on both service-account
// routes, and omitting it has GitLab generate an address under its own noreply
// domain. What its documentation adds is the sentence that matters — "custom
// email addresses require confirmation before the account is active, unless
// the group has a matching verified domain". `crewlet.invalid` is a reserved
// TLD (RFC 2606), so no confirmation mail can ever be delivered and no domain
// can ever be verified: every account this created was permanently inactive.
//
// GitLab then refuses every token such an account holds, with
// `403 Your primary email address is not confirmed`. Measured: a seat's token
// was minted, sealed, refused on the next pass's check, and minted again —
// 144 live tokens with `api` scope over one connect, and 164 in total. That
// loop is fixed at three points (see [Client.Tokens] and the fresh-token probe
// in the vendor's mint); THIS is the point at which it stops starting.
//
// A GENERATED ADDRESS IS STILL A ROBOT'S. GitLab's own is
// `service_account_group_…@noreply.<instance>`, which is the same promise the
// derived one was making and is one the instance can actually keep.

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
