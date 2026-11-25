package googlecloud

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// Requirements says what company still needs for Google Cloud. A nil block
// answers every field absent, so the form still renders.
//
// ONE PASS, IN ORDER: the provisioner is entered and saved, which mints the
// federation key; the Cloud Shell block uploads the key's public half and
// prints the provider; the provider is pasted and tested. So the provider is
// printed by the key's row, which carries the block ([CloudShellBlock]) once
// the key is held, rendered for company's issuer and subject and the growth
// products the company turns on.
func Requirements(
	company string, in *config.GoogleCloud, growth []googleauth.Product, resolve func(string) (string, bool),
) []setup.Requirement {
	var provisioner, provider, key string
	var enabled, atlas bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			provisioner, provider, key = in.Provisioning.Provisioner, in.Provisioning.Provider,
				in.Provisioning.FederationKey
			atlas = in.Provisioning.AtlasRead
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
			Hint:       vendortext.Plain("The service account Crewlet acts as, in a project kept for it."),
			More: vendortext.Of(vendortext.W("Every agent's service account is made in that project. Create it on your "),
				vendortext.W("new project page").At(newProjectURL), vendortext.W(", save, then run the block below in "),
				vendortext.W("Cloud Shell").At(cloudShellURL), vendortext.W("; it creates this account if it does not exist.")),
			Blocks: integration.FindingCredentialMissing,
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
			Hint:     vendortext.Of(vendortext.W("Run this block in "), vendortext.W("Cloud Shell").At(cloudShellURL), vendortext.W(".")),
			More: vendortext.Plain("Crewlet made this key when you saved, and sealed it as " + FederationKeySecret + ". " +
				"Google holds only its public half; the private half is never shown or exported."),
			CheckNote: "It is tested with the provider.",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field:      "provider",
			Label:      "Workload identity provider",
			Kind:       setup.KindID,
			ConfigPath: "integrations.google_cloud.provisioning.provider",
			Required:   true,
			Connect:    true,
			PrintedBy:  "federation_key",
			Checkable:  true,
			CheckWith:  []string{"provisioner", "federation_key"},
			Format:     "projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>",
			Shape:      setup.ShapeGoogleProvider,
			Hint:       vendortext.Plain("The last line the Cloud Shell block prints."),
			More: vendortext.Of(vendortext.W("Find it later under "), vendortext.UI("Workload Identity Pools").At(ProvidersURL),
				vendortext.W(". Test checks federation, the provisioner's grants, the "+
					"identity project's APIs and every project prepared for Crewlet.")),
			Blocks: integration.FindingApprovalRequired,
		},
		{
			// OPTIONAL, AND NEVER IN THE WAY: no agent depends on it, and on
			// only adds an account the atlas alone acts as.
			Field: "atlas_read", Label: "See more in Atlas", Kind: setup.KindToggle,
			ConfigPath: "integrations.google_cloud.provisioning.atlas_read", Default: "false",
			Hint: vendortext.Plain("Lets Atlas list what each project runs, as a service account of its own."),
			More: vendortext.Plain("Atlas then shows each project's instances, databases, Cloud Run services, functions, buckets " +
				"and topics, by name and type, read hourly as " + AtlasAccountID + " through Cloud Asset Inventory. " +
				"It reads no configuration, label, environment or data. One step: turn this on and save, then run " +
				"the Cloud Shell block above again. To undo it, turn this off and delete the " + AtlasAccountID +
				" service account in the identity project."),
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
				reqs[i].Public = cloudShellView(company, setup.Deref(provisioner, resolve), setup.Deref(key, resolve), atlas,
					growth)
				if reqs[i].Public != nil {
					reqs[i].Public.DrawnFrom = []string{"provisioner", "atlas_read"}
				}
			}
		case "provider":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(provider, resolve)
		case "atlas_read":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(atlas)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// The console's new project page, and Cloud Shell opened over the console.
const (
	newProjectURL = "https://console.cloud.google.com/projectcreate"
	cloudShellURL = "https://console.cloud.google.com/?cloudshell=true"
)

// Guide is what the connect form says around its fields: one pass, in order,
// from an empty project to the provider the Cloud Shell block prints.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet gives each agent its own service account and issues its token for an hour at a " +
			"time. No service account key is made or downloaded.",
		Prepare: []setup.Step{
			{Text: vendortext.Of(vendortext.W("Create an empty project for Crewlet's service accounts on your "),
				vendortext.W("new project page").At(newProjectURL), vendortext.W("."))},
			{
				Text:   vendortext.Plain("Enter the provisioner's address here and save, so Crewlet makes its key."),
				Yields: "provisioner",
			},
			{
				Text: vendortext.Of(vendortext.W("Run the block Crewlet shows in "), vendortext.W("Cloud Shell").At(cloudShellURL),
					vendortext.W(". It creates the provisioner if needed and uploads Crewlet's public key.")),
			},
			{Text: vendortext.Plain("Paste the last line the block prints, then test it."), Yields: "provider"},
		},
	}
}
