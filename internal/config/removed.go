package config

import "strings"

// removedKeys are the keys this build refuses BY NAME, each mapped to what
// to do instead, keyed by the Go type the key was decoded into (see
// [removedKey]) and then the key.
//
// # A removed key is not a typo, and is not reported as one
//
// Both tiers decode with unknown keys refused, which is right: a misspelled
// setting that decoded to nothing is how a deployment runs on half its
// configuration. But the generic refusal tells somebody upgrading a file
// that once carried one of these keys to check its spelling, and no
// spelling of them works. Each entry here names the key, says it was
// removed, and says what replaces it.
//
// ONLY THE KEYS THE IDENTITY AND ACCESS REWRITE AND PER-SEAT ACCESS REMOVED.
// Every one of them changes who can reach the deployment, or what an agent may
// do at a vendor, so the file that still carries one is the file whose author
// most needs to read what changed.
var removedKeys = map[string]string{
	"APIAuth.allow_anonymous_read": "`api.auth.allow_anonymous_read` was " +
		"removed. Remove it; every request now needs a credential. For a " +
		"read-only client, add an `api.auth.tokens` entry with only the apps " +
		"it reads",
	"APIAuth.disabled": "`api.auth.disabled` was removed. Remove it; every " +
		"request now needs a credential. For local development, use " +
		"`crewlet run -dev-principal`",
	"APIAuth.oidc": "`api.auth.oidc` was removed. Remove it; people sign in " +
		"with a password and two-step verification instead",
	"APIAuth.bootstrap": "`api.auth.bootstrap` was removed. Remove it, and " +
		"invite the first person with an `api.auth.tokens` token " +
		"(`crewlet iam invite <address> -seat <handle>`)",
	// PER-SEAT ACCESS moved what an agent may do at a vendor onto the seat
	// (`roles[].integrations.<lane>`), in the vendor's own terms.
	"GitLabProvisioning.access_level": "`integrations.gitlab.provisioning.access_level` " +
		"was removed. Set each agent's GitLab role on the seat instead: " +
		"`roles[].integrations.gitlab.access_level` (10 Guest to 50 Owner)",
	"GitLabProvisioning.access_levels": "`integrations.gitlab.provisioning.access_levels` " +
		"was removed. Set each agent's GitLab role on its own seat instead: " +
		"`roles[].integrations.gitlab.access_level`",
	"GitLabProvisioning.token_scopes": "`integrations.gitlab.provisioning.token_scopes` " +
		"was removed. Remove it; a seat's token scopes follow its GitLab role " +
		"(read_api and read_repository below Developer, api from Developer up)",
	"DatadogProvisioning.role": "`integrations.datadog.provisioning.role` was removed. " +
		"Set each agent's Datadog role on its own seat instead: " +
		"`roles[].integrations.datadog.role` (a role id from GET /api/v2/roles)",
	"RoleGitHub.tier": "`roles[].integrations.github.tier` was removed. Set the agent's " +
		"GitHub App permissions in GitHub's own terms instead: " +
		"`roles[].integrations.github.permissions` (for example contents: write)",
	"RoleGitHub.workflows": "`roles[].integrations.github.workflows` was removed. Grant " +
		"workflow writes as a permission instead: " +
		"`roles[].integrations.github.permissions.workflows: write`",
	"RoleMattermost.channel": "`roles[].integrations.mattermost.channel` was removed. List " +
		"every channel the agent's bot is a member of instead: " +
		"`roles[].integrations.mattermost.channels`",
	"HumanContact.crewlet_operator_id": "`contact.crewlet_operator_id` was " +
		"removed. Remove it, and bind the token to the seat with " +
		"`crewlet iam create -kind machine -login token:<id> -seat <handle> " +
		"-grants none`",
}

// removedKey is how [removedKeys] is addressed: the Go type yaml names in
// its own message ("config.APIAuth"), without its package qualifier, then
// the key.
//
// KEYED ON THE TYPE, not on the bare key, because the same word means
// different things in different blocks: `disabled` under `api.auth:` is the
// removed posture, and a `disabled:` typed anywhere else is an ordinary typo
// and has to read like one.
func removedKey(goType, field string) string {
	if dot := strings.LastIndex(goType, "."); dot >= 0 {
		goType = goType[dot+1:]
	}
	return goType + "." + strings.Trim(field, `"`)
}
