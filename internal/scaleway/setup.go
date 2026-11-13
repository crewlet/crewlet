package scaleway

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// Requirements says what this company still needs for Scaleway. A nil block
// answers every field absent, so the form still renders.
func Requirements(in *config.Scaleway, resolve func(string) (string, bool)) []setup.Requirement {
	var org, access, secret string
	var enabled, atlasOn bool
	if in != nil {
		enabled, atlasOn = in.Enabled, in.Atlas
		if in.Provisioning != nil {
			org, access, secret = in.Provisioning.OrganizationID, in.Provisioning.AccessKey, in.Provisioning.SecretKey
		}
	}
	reqs := []setup.Requirement{
		{
			Field:      "organization_id",
			Label:      "Organization ID",
			Kind:       setup.KindID,
			ConfigPath: "integrations.scaleway.provisioning.organization_id",
			Required:   true,
			Connect:    true,
			Format:     "A UUID",
			Shape:      setup.ShapeUUID,
			Hint:       "Shown in your {link}.",
			LinkText:   "Organization settings",
			VendorURL:  OrganizationURL,
			Blocks:     integration.FindingCredentialMissing,
		},
		{
			Field:      "access_key",
			Label:      "Access key",
			Kind:       setup.KindID,
			ConfigPath: "integrations.scaleway.provisioning.access_key",
			Required:   true,
			Connect:    true,
			Format:     "SCW followed by 17 capital letters or digits",
			Shape:      setup.ShapeScalewayAccessKey,
			Hint:       "The crewlet-provisioner API key's access key, starting with SCW.",
			More: "Create the application on your {link}, with one policy at Organization scope granting " +
				words.And(asSaid(provisionerSets)) + ".",
			LinkText:  "IAM applications page",
			VendorURL: ApplicationsURL,
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "secret_key",
			Label:      "Secret key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.scaleway.provisioning.secret_key",
			SecretName: ProvisionerSecret,
			Required:   true,
			Connect:    true,
			Checkable:  true,
			CheckWith:  []string{"organization_id", "access_key"},
			// A PASTE IS CHECKED AGAINST SCALEWAY'S SHAPE before it is
			// stored or tested: every secret key is a UUID.
			Shape:     setup.ShapeUUID,
			Hint:      "Shown once, when you create the API key on your {link}.",
			More:      "Give the API key an expiry of 30 days or less, and rotate it before it ends.",
			LinkText:  "API keys page",
			VendorURL: APIKeysURL,
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			// THE ONE STEP OF "SEE MORE IN ATLAS" (ADR-0144): the provisioner
			// key above can make the application itself, so nothing is
			// created by hand at Scaleway. Off unless a person turns it on.
			Field:      "atlas",
			Label:      "See more in Atlas",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.scaleway.atlas",
			Default:    "false",
			Hint:       "Shows servers, databases, buckets and DNS in Atlas, read-only.",
			More: "Crewlet makes a read-only IAM application named crewlet-atlas for it, with its own " +
				"policy and API key, on your {link}. It holds " + words.And(asSaid(AtlasSets())) +
				" on every project and nothing else, and no agent uses it. Turn this off to delete the " +
				"application, its policy and its key.",
			LinkText:  "IAM applications page",
			VendorURL: ApplicationsURL,
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Manage agent applications",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.scaleway.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	for i := range reqs {
		switch reqs[i].Field {
		case "organization_id":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(org, resolve)
		case "access_key":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(access, resolve)
		case "secret_key":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(secret, resolve)
		case "atlas":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(atlasOn)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Guide is what the connect form says around its fields; the key's reach,
// which no condition can narrow, is its caution, said once.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet gives each agent its own Scaleway IAM application and API key, and rotates, " +
			"narrows and deletes them for you.",
		Prepare: []setup.Step{
			{Text: "Create an IAM application named crewlet-provisioner on your {link}.", Link: "access_key"},
			{Text: "Attach one policy to it at Organization scope, with:", Grants: slices.Clone(provisionerSets)},
			{
				Text: "Create an API key for the application on your {link}, expiring in 30 days or less.",
				Link: "secret_key", Yields: "secret_key",
			},
			{Text: "Paste the keys here and test them."},
		},
		Caution: &setup.Caution{
			Title: "This key administers IAM for your whole Organization",
			Body: "It can mint keys for, and grant anything to, any application there, and no condition " +
				"can limit it to the agents' applications. Keep its expiry short and rotate it before it ends.",
		},
	}
}

// asSaid is permission set names as prose names them, unquoted: a sentence
// that tells a person what to grant names each set as Scaleway's console does.
func asSaid(sets []string) []words.Said {
	out := make([]words.Said, len(sets))
	for i, set := range sets {
		out[i] = words.Said(set)
	}
	return out
}
