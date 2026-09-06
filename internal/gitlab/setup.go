package gitlab

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before GitLab events reach a seat, and
// before this engine can create the accounts those seats act as.
//
// # This is the first vendor whose pass CREATES something a person owns
//
// A GitHub pass registers a hook. A GitLab pass creates a service account per
// agent, mints a token on each, adds them to a group and registers a webhook.
// Everything it creates is visible to the whole group and outlives the run,
// so the credential that authorises it is asked for on EVERY pass and never
// stored: a group Owner token held permanently is a standing power to create
// accounts, where the same token asked for once is a grant with an end.
//
// That credential is therefore not in this list. It is [OperatorCredential],
// which the pass declares through Needs, and which the setup surface collects
// as a transient field.

// Requirements says what this company still needs for GitLab.
func Requirements(in *config.GitLab, resolve func(string) (string, bool)) []setup.Requirement {
	var url, signing, token, group, prefix string
	var enabled bool
	var accessLevel config.GitLabAccessLevel
	if in != nil {
		enabled, url = in.Enabled, in.URL
		signing, token = in.SigningSecret, in.Token
		if p := in.Provisioning; p != nil {
			group, prefix, accessLevel = p.Group, p.UsernamePrefix, p.AccessLevel
		}
	}

	reqs := []setup.Requirement{
		{
			Field:      "enabled",
			Label:      "Accept GitLab deliveries",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.gitlab.enabled",
			Required:   true,
			Help: "Off leaves the configuration in place and the route closed, " +
				"which is how you pause the integration without losing its setup.",
		},
		{
			Field:      "url",
			Label:      "GitLab instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.gitlab.url",
			Required:   true,
			Help:       "Leave this as https://gitlab.com unless you run GitLab yourself.",
			Format:     "https://gitlab.example.com",
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "signing_secret",
			Label:      "Webhook signing secret",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.gitlab.signing_secret",
			SecretName: "GITLAB_SIGNING_SECRET",
			Required:   true,
			// MINTABLE, and it has a SHAPE the engine must produce: GitLab
			// signs with the decoded 32 bytes, so a value that is not
			// whsec_ over base64 of exactly that cannot be the key for any
			// delivery, however non-empty. A person typing one would get
			// it wrong.
			Mintable: true,
			Help: "Every delivery is signed with this and checked at the edge. " +
				"GitLab signs with the decoded bytes, so it has a shape rather " +
				"than being any string, which is why the engine generates it.",
			Format: "whsec_ then standard base64 of a 32-byte key",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "token",
			Label:      "Read token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.gitlab.token",
			SecretName: "GITLAB_TOKEN",
			Required:   false,
			Help: "Read-only, and optional: without it a merge request still " +
				"reaches its reviewer, and nobody else participating in the " +
				"thread hears anything, because the engine cannot look them up.",
			Where:     "A personal access token with read_api.",
			VendorURL: "https://gitlab.com/-/user_settings/personal_access_tokens",
		},
		{
			Field:      "provisioning.group",
			Label:      "Group",
			Kind:       setup.KindID,
			ConfigPath: "integrations.gitlab.provisioning.group",
			Required:   false,
			Help: "The top-level group the service accounts join and whose " +
				"projects they work in. Without it there is nothing to " +
				"provision, and the setup pass has nothing to do.",
			Blocks: integration.FindingIngressBlocked,
		},
		{
			Field:      "provisioning.access_level",
			Label:      "Membership level",
			Kind:       setup.KindChoice,
			ConfigPath: "integrations.gitlab.provisioning.access_level",
			Required:   false,
			Choices:    accessChoices(),
			Help: "What each agent's account may do in the group. Developer can " +
				"push and open merge requests; maintainer can also merge and " +
				"administer projects.",
		},
		{
			Field:      "provisioning.username_prefix",
			Label:      "Username prefix",
			Kind:       setup.KindText,
			ConfigPath: "integrations.gitlab.provisioning.username_prefix",
			Required:   false,
			Help: "Put in front of every service account's username, so the " +
				"accounts this engine created are recognisable in a group that " +
				"has others.",
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(url)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(signing, resolve)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Held(token, resolve)
	reqs[4].Present, reqs[4].Resolved, reqs[4].Stored = setup.Plain(group)
	reqs[5].Present, reqs[5].Resolved, reqs[5].Stored = setup.Plain(string(accessLevel))
	reqs[6].Present, reqs[6].Resolved, reqs[6].Stored = setup.Plain(prefix)
	return reqs
}

// OperatorCredential is the transient administrator token a pass runs as.
//
// ASKED FOR EVERY TIME AND NEVER STORED. It can create accounts and mint
// tokens on them, which is a standing power if it is kept and a grant with an
// end if it is not. The command line refuses to persist it for the same
// reason, reading it from the environment only.
func OperatorCredential() setup.Requirement {
	return setup.Requirement{
		Field:    "operator_credential",
		Label:    "Group Owner token",
		Kind:     setup.KindSecret,
		Required: true,
		Help: "Used for this run and not kept. It creates the service accounts, " +
			"mints their tokens and registers the webhook, so it needs to " +
			"belong to somebody who owns the group.",
		Where:     "Create a legacy personal access token with the full api scope.",
		VendorURL: "https://gitlab.com/-/user_settings/personal_access_tokens",
	}
}

// accessChoices are the membership levels, from the config package's own list
// so a value it accepts and a value this offers can never diverge.
func accessChoices() []setup.Choice {
	labels := map[config.GitLabAccessLevel]string{
		config.GitLabDeveloper:  "Developer: push and open merge requests",
		config.GitLabMaintainer: "Maintainer: also merge and administer projects",
	}
	out := make([]setup.Choice, 0, len(config.GitLabAccessLevels))
	for _, level := range config.GitLabAccessLevels {
		label := labels[level]
		if label == "" {
			label = string(level)
		}
		out = append(out, setup.Choice{Value: string(level), Label: label})
	}
	return out
}
