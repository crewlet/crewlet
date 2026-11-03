package googlecloud

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Requirements says what company still needs for Google Cloud. A nil block
// answers every field absent, so the form still renders.
//
// ONE PASS, IN ORDER: the provisioner is entered and saved, which mints the
// federation key; the Cloud Shell block uploads the key's public half and
// prints the provider; the provider is pasted and tested. So the provider is
// required only once the key is held, and the key's row carries the block
// ([CloudShellBlock]) from then on, rendered for company's issuer and subject.
func Requirements(company string, in *config.GoogleCloud, resolve func(string) (string, bool)) []setup.Requirement {
	var provisioner, provider, key string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			provisioner, provider, key = in.Provisioning.Provisioner, in.Provisioning.Provider,
				in.Provisioning.FederationKey
		}
	}
	keyPresent, keyResolved := setup.Resolution(key, resolve)
	keyHeld := keyPresent && keyResolved != nil && *keyResolved
	reqs := []setup.Requirement{
		{
			Field:      "provisioner",
			Label:      "Provisioner service account",
			Kind:       setup.KindID,
			ConfigPath: "integrations.google_cloud.provisioning.provisioner",
			Required:   true,
			Connect:    true,
			Format:     "crewlet-provisioner@<identity project>.iam.gserviceaccount.com",
			Shape:      setup.ShapeGoogleServiceAccount,
			Help: "The service account Crewlet acts as, in an empty project you create for Crewlet's " +
				"accounts alone: every agent's service account is made there. Save to mint Crewlet's " +
				"key, then run the block below in Cloud Shell, which creates this account if it is " +
				"absent.\nCreate the project on your",
			LinkText:  "new project page",
			VendorURL: "https://console.cloud.google.com/projectcreate",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "federation_key",
			Label:      "Federation key",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.google_cloud.provisioning.federation_key",
			SecretName: FederationKeySecret,
			Required:   true,
			Connect:    true,
			// MINTABLE, because Google needs only its public half: the engine
			// makes the key, seals the private half, and nobody downloads a
			// credential at all.
			Mintable: true,
			Shape:    setup.ShapeES256Key,
			Help: "Crewlet's own key, made when you save and sealed as " + FederationKeySecret + ". " +
				"Google holds only its public half, which the block below uploads to your workload " +
				"identity provider. The private half is never shown or exported.",
			CheckNote: "It is tested with the provider.",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "provider",
			Label:      "Workload identity provider",
			Kind:       setup.KindID,
			ConfigPath: "integrations.google_cloud.provisioning.provider",
			Required:   keyHeld,
			Connect:    true,
			Checkable:  true,
			CheckWith:  []string{"provisioner", "federation_key"},
			Format:     "projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>",
			Shape:      setup.ShapeGoogleProvider,
			Where:      "the last line the block above prints",
			Help: "The provider holding Crewlet's public key, which lets Crewlet act as the provisioner " +
				"and nothing else. Test checks federation, the provisioner's grants, the identity " +
				"project's APIs and every project prepared for Crewlet.\nSee your",
			LinkText:  "workload identity pools",
			VendorURL: ProvidersURL,
			Blocks:    integration.FindingApprovalRequired,
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Manage agent service accounts",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.google_cloud.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
	}
	for i := range reqs {
		switch reqs[i].Field {
		case "provisioner":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(provisioner, resolve)
		case "federation_key":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(key, resolve)
			if keyHeld {
				reqs[i].Public = cloudShellView(company, setup.Deref(provisioner, resolve), setup.Deref(key, resolve))
			}
		case "provider":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(provider, resolve)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Crewlet gives each agent its own Google Cloud service account, with the level you choose on " +
		"the projects you prepare for it, and issues its token for an hour at a time. Connecting takes a " +
		"provisioner service account in an empty project kept for Crewlet's accounts, which Crewlet " +
		"reaches with a key of its own: no service account key is made or downloaded."
}
