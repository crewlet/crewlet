package config

import (
	"errors"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

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

// setAsideRemoved takes the keys this build retired out of doc, each with its
// value, when they are ALL that err refuses, and reports whether it did.
//
// err is the parse of doc ([ParseCompanyNode]), so the fault each removed key
// earned carries the authored path to that key ([placeInDocument]), and the
// key is deleted from the mapping the path ends in. The path never passes
// through an alias, as the walk that placed it never follows one, so a key
// inside an anchored block is deleted where the anchor is defined, which is
// every place it is used.
//
// ALL OR NOTHING: a fault of any other kind, or a path doc does not hold,
// leaves doc as it was and answers false, so the caller refuses the file
// exactly as the parse did. A file is read without its retired keys only
// when they are the whole of what is wrong with how it is written.
func setAsideRemoved(doc *yaml.Node, err error) bool {
	type keyAt struct{ mapping, key *yaml.Node }
	var found []keyAt
	for _, leaf := range leafErrors(err) {
		f, ok := leafFault(leaf)
		if !ok || !errors.Is(f.Kind, ErrRemovedField) {
			return false
		}
		mapping, key := keyNodeAt(doc, f.Path)
		if key == nil {
			return false
		}
		found = append(found, keyAt{mapping: mapping, key: key})
	}
	if len(found) == 0 {
		return false
	}
	for _, k := range found {
		// A key two faults name is deleted once.
		if at := slices.Index(k.mapping.Content, k.key); at >= 0 {
			k.mapping.Content = slices.Delete(k.mapping.Content, at, at+2)
		}
	}
	return true
}

// keyNodeAt finds the mapping key path ends in, and the mapping holding it,
// walking doc as [placeInDocument] walks it: a string segment is a mapping
// key and an int a list index. Nil when doc holds no such key.
func keyNodeAt(doc *yaml.Node, path Path) (mapping, key *yaml.Node) {
	if len(path) == 0 {
		return nil, nil
	}
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	for i, segment := range path {
		switch s := segment.(type) {
		case string:
			if n.Kind != yaml.MappingNode {
				return nil, nil
			}
			at := -1
			for j := 0; j+1 < len(n.Content); j += 2 {
				if n.Content[j].Value == s {
					at = j
					break
				}
			}
			if at < 0 {
				return nil, nil
			}
			if i == len(path)-1 {
				return n, n.Content[at]
			}
			n = n.Content[at+1]
		case int:
			if n.Kind != yaml.SequenceNode || s < 0 || s >= len(n.Content) {
				return nil, nil
			}
			n = n.Content[s]
		default:
			return nil, nil
		}
	}
	// The path ended on a list item, which is no key.
	return nil, nil
}
