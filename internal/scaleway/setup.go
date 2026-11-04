package scaleway

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Requirements says what this company still needs for Scaleway. A nil block
// answers every field absent, so the form still renders.
//
// THE KEY'S REACH IS SAID WHERE IT IS PASTED: no condition can keep a key
// that manages applications to the agents' applications, so the form says
// plainly what it can do and asks for a short expiry.
func Requirements(in *config.Scaleway, resolve func(string) (string, bool)) []setup.Requirement {
	var org, access, secret string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
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
			Help:       "Shown in your",
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
			Help: "The access key of the crewlet-provisioner application's API key, starting with SCW. " +
				"Give that application one policy at Organization scope with IAMApplicationManager, " +
				"IAMPolicyManager and ProjectReadOnly.\nCreate the application on your",
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
			Shape: setup.ShapeUUID,
			Help: "Sealed as " + ProvisionerSecret + ". This key can create IAM applications, mint their " +
				"API keys and write policies anywhere in your Organization, so it can mint keys for, and " +
				"grant anything to, any application there. Treat it as an Organization administrator for " +
				"IAM: no condition can limit it to the agents' applications. Give it a short expiry, such " +
				"as 30 days, and rotate it before it ends.\nCreate the key on your",
			LinkText:  "API keys page",
			VendorURL: APIKeysURL,
			Blocks:    integration.FindingCredentialMissing,
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
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Crewlet gives each agent its own Scaleway IAM application, with one policy at the level " +
		"you choose on the projects you choose and its own API key, and rotates, narrows and deletes " +
		"them for you. Connecting takes the API key of an IAM application that manages applications " +
		"and policies."
}
