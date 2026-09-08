package gitlab

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// What an operator has to supply before GitLab events reach a seat, and
// before this engine can create the accounts those seats act as.
//
// # This is the first third-party app whose pass CREATES something a person owns
//
// A GitHub pass registers a hook. A GitLab pass creates a service account per
// agent, mints a token on each, adds them to a group and registers a webhook.
// Everything it creates is visible to the whole group and outlives the run,
// and so does the credential that authorises it: see [AdminCredential] for
// why it is held rather than asked for each time, and what holding it buys.

// Requirements says what this company still needs for GitLab.
func Requirements(in *config.GitLab, resolve func(string) (string, bool)) []setup.Requirement {
	var url, signing, group string
	var enabled bool
	if in != nil {
		enabled, url, signing = in.Enabled, in.URL, in.SigningSecret
		if p := in.Provisioning; p != nil {
			group = p.Group
		}
	}

	reqs := []setup.Requirement{
		{
			Field:      "enabled",
			Label:      "Accept GitLab deliveries",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.gitlab.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
		{
			Field:      "url",
			Connect:    true,
			Label:      "GitLab instance",
			Kind:       setup.KindURL,
			ConfigPath: "integrations.gitlab.url",
			Required:   true,
			Default:    "https://gitlab.com",
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
			// AND IT IS THE ONE FIELD WITH A SHAPE. GitLab accepts this
			// in exactly one form and computes its HMAC over the decoded
			// bytes, so a plain token here is a secret that cannot match
			// any delivery: see [setup.ShapeSigningKey].
			Shape:  setup.ShapeSigningKey,
			Help:   "Signs every delivery, and has a shape, so the engine generates it.",
			Format: "whsec_ then standard base64 of a 32-byte key",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field:      "provisioning.group",
			Connect:    true,
			Label:      "Top-level group",
			Kind:       setup.KindID,
			ConfigPath: "integrations.gitlab.provisioning.group",
			Required:   true,
			// THE LINK IS IN THE WORDS, on the phrase it is about. A
			// trailing "Open GitLab" makes a reader work out which of
			// several pages the form meant.
			Help: "Find your [top-level group](https://gitlab.com/dashboard/groups). " +
				"Each Crewlet agent becomes a service account inside it.",
			Blocks: integration.FindingIngressBlocked,
		},
	}

	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Plain(url)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(signing, resolve)
	reqs[3].Present, reqs[3].Resolved, reqs[3].Stored = setup.Plain(group)
	// The administrator credential, appended rather than declared inline
	// with the rest because it is the one whose value this function has to
	// resolve through the same seam every other secret uses.
	admin := AdminCredential("")
	if in != nil && in.Provisioning != nil {
		admin = AdminCredential(in.Provisioning.AdminToken)
		admin.Present, admin.Resolved, admin.Stored = setup.Held(in.Provisioning.AdminToken, resolve)
	}
	reqs = append(reqs, admin)
	return reqs
}

// AdminCredential is the group Owner token this third-party app's provisioning and its
// teardown both authenticate with.
//
// HELD, not transient, and that is a deliberate reversal. It used to be asked
// for on every run and dropped straight after, because a token that can
// create service accounts is a standing power once it is kept. What that cost
// is a DISCONNECT: removing an account needs the authority that created it,
// so with nothing held there was no way to take one away from here, and every
// service account this engine made outlived the integration that made it.
//
// Sealed like every other credential — the value goes to the fleet's secret
// store and the document gets a ${VAR} — and named in the orphaned list when
// the integration is disconnected, so an operator knows exactly what to
// revoke afterwards.
func AdminCredential(stored string) setup.Requirement {
	return setup.Requirement{
		Field:      "admin_token",
		Connect:    true,
		Label:      "Access token",
		Kind:       setup.KindSecret,
		ConfigPath: "integrations.gitlab.provisioning.admin_token",
		Required:   true,
		Present:    stored != "",
		Stored:     stored,
		// ONE SENTENCE carrying its own link and naming the scope as the
		// literal it is. It was three clauses and a trailing "Open
		// GitLab": what to do, why, and where, in that order, when what
		// a reader needs first is the page that issues the thing.
		// THE FORM THAT MAKES ONE, not the list of the ones that exist.
		// The token has to be a LEGACY personal access token, which is a
		// separate form on that page, so the settings index left a reader
		// to find it.
		Help: "[Create a legacy personal token]" +
			"(https://gitlab.com/-/user_settings/personal_access_tokens/legacy/new) " +
			"with the full `api` scope, belonging to somebody who owns the group.",
	}
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Each agent gets its own service account in your group, so it owns " +
		"what it builds. Connecting needs a token from somebody who owns " +
		"that group, and it is kept so the accounts can be removed again."
}
