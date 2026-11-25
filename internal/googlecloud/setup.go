package googlecloud

import (
	"cmp"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// Requirements says what company still needs for Google Cloud. A nil block
// answers every field absent, so the form still renders.
//
// ONE PASS, ONE SAVE: the federation key is made when the form opens
// (MintFirst), so the key's row carries the block ([CloudShellBlock]) at once,
// drawn from the identity project as typed (a live view the form redraws); the
// block prints the provider, which is pasted, and one save stores both.
func Requirements(company string, in *config.GoogleCloud, resolve func(string) (string, bool)) []setup.Requirement {
	var project, provider, key string
	var enabled, atlas bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			project, provider, key = in.Provisioning.IdentityProject, in.Provisioning.Provider,
				in.Provisioning.FederationKey
			atlas = in.Provisioning.AtlasRead
		}
	}
	keyPresent, keyResolved := setup.Resolution(key, resolve)
	keyHeld := keyPresent && keyResolved != nil && *keyResolved
	suggested := SuggestedProject(company)
	reqs := []setup.Requirement{
		{
			Field:      "identity_project",
			Label:      "Identity project ID",
			Kind:       setup.KindID,
			ConfigPath: "integrations.google_cloud.provisioning.identity_project",
			Required:   true,
			Connect:    true,
			Default:    suggested,
			Format:     "6 to 30 lowercase letters, digits and hyphens, starting with a letter",
			Shape:      setup.ShapeGoogleProject,
			Hint:       vendortext.Plain("A new project kept for Crewlet alone. Its ID can differ from its name."),
			More: vendortext.Of(vendortext.W("The block below creates it if it does not exist. To use one you made "+
				"on your "), vendortext.W("new project page").At(newProjectURL), vendortext.W(", type the ID Google "+
				"shows under its name. Crewlet acts in it as "+config.GoogleCloudProvisioner("{identity_project}")+
				", which the block makes.")),
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
			// credential at all. FIRST, so the block shows before any save.
			Mintable:  true,
			MintFirst: true,
			Shape:     setup.ShapeES256Key,
			Hint:      vendortext.Of(vendortext.W("Run this block in "), vendortext.W("Cloud Shell").At(cloudShellURL), vendortext.W(".")),
			More: vendortext.Plain("If the block stops, its last line starts \"" + strings.TrimSpace(StopPrefix) +
				"\" and says what to do: run gcloud auth login when Cloud Shell has no active account, or type the ID " +
				"of a new, empty project when this one runs workloads, holds service accounts Crewlet did not make " +
				"or cannot be created. It is safe to run again. Crewlet made this key when you started connecting " +
				"and sealed it as " + FederationKeySecret + "; Google holds only its public half. The block stays " +
				"the same when you turn on another Google product."),
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
			CheckWith:  []string{"identity_project", "federation_key"},
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
				"It reads no configuration, label, environment or data. Turning it on adds Atlas's lines to the " +
				"Cloud Shell block above: run it, again if you ran it before, then save. To undo it, turn this off " +
				"and delete the " + AtlasAccountID + " service account in the identity project."),
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
		case "identity_project":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(project, resolve)
		case "federation_key":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(key, resolve)
			if keyHeld {
				// WHAT IS SAVED, else the suggestion the form opens with; the
				// form redraws it from what is typed ([Draw]).
				drawn := cmp.Or(setup.Deref(project, resolve), suggested)
				reqs[i].Public = cloudShellView(company, drawn, setup.Deref(key, resolve), atlas)
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

// Draw is the block drawn from what the form holds ([setup.Drawer]): the
// identity project and See more in Atlas as typed, and the key Crewlet made.
func Draw(d setup.Draw) (*setup.PublicView, error) {
	if d.Field != "federation_key" {
		return nil, setup.NotDrawable("Google Cloud", d.Field)
	}
	key := strings.TrimSpace(d.Value("federation_key"))
	if key == "" {
		return nil, setup.Unusable("Open the form again so Crewlet makes the federation key the block uploads.")
	}
	block, err := CloudShellBlock(d.Company, d.Value("identity_project"), key, d.Value("atlas_read") == "true")
	if err != nil {
		return nil, setup.Unusable("Type the identity project's ID to see the block.")
	}
	return liveView(key, block)
}

// The console's new project page, and Cloud Shell opened over the console.
const (
	newProjectURL = "https://console.cloud.google.com/projectcreate"
	cloudShellURL = "https://console.cloud.google.com/?cloudshell=true"
)

// Guide is what the connect form says around its fields: one pass, in order,
// from a project ID to the provider the Cloud Shell block prints.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet gives each agent its own service account and issues its token for an hour at a " +
			"time. No service account key is made or downloaded.",
		Prepare: []setup.Step{
			{
				Text: vendortext.Plain("Keep the suggested project ID, or type the ID of an empty project you " +
					"made for Crewlet."),
				Yields: "identity_project",
			},
			{
				Text: vendortext.Of(vendortext.W("Copy the block and run it in "), vendortext.W("Cloud Shell").At(cloudShellURL),
					vendortext.W(". It creates the project if needed and uploads Crewlet's public key.")),
			},
			{Text: vendortext.Plain("Paste the last line the block prints, then save."), Yields: "provider"},
		},
	}
}
