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
			Label:      projectLabel,
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
			Label:      keyLabel,
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
			Label:      providerLabel,
			Kind:       setup.KindID,
			ConfigPath: "integrations.google_cloud.provisioning.provider",
			Required:   true,
			Connect:    true,
			PrintedBy:  "federation_key",
			Checkable:  true,
			CheckWith:  []string{"identity_project", "federation_key"},
			Format:     ProviderName(projectNumber),
			Shape:      setup.ShapeGoogleProvider,
			Hint:       vendortext.Plain("The last line the Cloud Shell block prints."),
			More: vendortext.Of(vendortext.W("It names the identity project by its number, not its ID. Find it later "+
				"under "), vendortext.UI("Workload Identity Pools").At(ProvidersURL),
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

// The form's labels, which the steps say as the form does, and the
// placeholder the provider's format and the steps name the project number by.
const (
	projectLabel  = "Identity project ID"
	keyLabel      = "Federation key"
	providerLabel = "Workload identity provider"
	projectNumber = "<project number>"
)

// shellPlace is where the block runs, besides Crewlet's own form.
const shellPlace = "Cloud Shell"

// The sections of the Google Cloud page the steps send a person to, which
// TestEveryDocsSectionTheStepsLinkIsOnThePage holds to docs/.
const (
	docsURL    = "https://docs.crewlet.ai/engine/latest/integrations/google-cloud/"
	prepareDoc = docsURL + "#prepare-a-project-for-crewlet"
	undoDoc    = docsURL + "#undo-the-cloud-shell-block"
)

// Guide is what the connect form says around its fields: one pass, in order,
// from the identity project to an agent turned on, each step's place, what
// shows it worked, and what people hit there.
func Guide() setup.Guide {
	w, ui := vendortext.W, vendortext.UI
	return setup.Guide{
		Summary: "Crewlet gives each agent its own service account and issues its token for an hour at a " +
			"time. No service account key is made or downloaded.",
		Prepare: []setup.Step{
			{
				Title: "Choose the identity project", Where: setup.Here,
				Text: vendortext.Of(w("In "), ui(projectLabel), w(", keep the suggested ID, or type the ID of a new, "+
					"empty project you made on your "), w("new project page").At(newProjectURL), w(". It must hold "+
					"nothing else, as Crewlet's provisioner may manage every service account in it.")),
				Result: vendortext.Of(ui(keyLabel), w(" shows the Cloud Shell block for that project, with a "),
					ui("Copy"), w(" button.")),
				Trouble: []setup.Trouble{{
					Sign: vendortext.Plain("Crewlet refuses the ID."),
					Fix: vendortext.Plain("Use the project's ID, not its name: Google shows it under the name, " +
						"and `gcloud projects list` prints it."),
				}},
				Yields: "identity_project",
			},
			{
				Title: "Run the block in Cloud Shell", Where: shellPlace,
				Text: vendortext.Of(w("Copy the block whole with that button, open "), w("Cloud Shell").At(cloudShellURL),
					w(", paste it and press Enter. It creates the project if it does not exist, runs for about a "+
						"minute and is safe to run again.")),
				Result: vendortext.Plain("Its last line is the provider, `" + ProviderName(projectNumber) + "`, " +
					"with the project's number, not its ID."),
				Trouble: []setup.Trouble{
					{
						Sign: vendortext.Plain("It says Cloud Shell has no active account."),
						Fix: vendortext.Plain("Run `gcloud auth login`, check `gcloud auth list` marks your account " +
							"active, and run the block again."),
					},
					{
						Sign: vendortext.Plain("It stops because the project runs workloads or holds a service " +
							"account Crewlet did not make."),
						Fix: vendortext.Plain("It changed nothing there. Choose a new, empty project in step 1 and " +
							"copy the block again."),
					},
					{
						Sign: vendortext.Plain("It stops with another `" + StopPrefix + "...` line."),
						Fix:  vendortext.Plain("The line says what failed, under Google's own error. Fix that and run the block again."),
					},
					{
						Sign: vendortext.Plain("An older block ran in a project that already runs something."),
						Fix:  vendortext.Of(w("Remove what it made there, as "), w("the docs list").At(undoDoc), w(".")),
					},
				},
			},
			{
				Title: "Paste the provider", Where: setup.Here,
				Text: vendortext.Of(w("Paste that last line into "), ui(providerLabel), w(", press "),
					ui("Test connection"), w(", then press "), ui("Save"), w(".")),
				Result: vendortext.Of(w("The form closes, and within about a minute the status goes from "),
					ui("Needs attention"), w(" to "), ui("Setting up"), w(" to "), ui("Connected"), w(".")),
				Trouble: []setup.Trouble{
					{
						Sign: vendortext.Plain("Crewlet says it is not a workload identity provider's full name."),
						Fix:  vendortext.Plain("Paste the whole last line, which starts with projects/ and a number."),
					},
					{
						Sign: vendortext.Plain("The test says Google refused Crewlet's federation token."),
						Fix:  vendortext.Plain("Run the block again so the provider holds Crewlet's current key."),
					},
				},
				Yields: "provider",
			},
			{
				Title: "Prepare each project agents work in", Where: shellPlace,
				Text: vendortext.Of(w("Connected gives no agent any access yet. As an Owner of each project agents "+
					"should work in, run the "), w("Prepare a project").At(prepareDoc), w(" block with `IDENTITY` set "+
					"to the identity project's ID and `TARGET` to that project.")),
				Result: vendortext.Of(w("The test lists the project under "), ui(offeredLabel), w(".")),
				Trouble: []setup.Trouble{{
					Sign: vendortext.Plain("The test says a project is not offered."),
					Fix:  vendortext.Plain("It names what the project lacks. Run the block on it again as an Owner."),
				}},
			},
			{
				Title: "Turn agents on", Where: setup.Here,
				Text: vendortext.Of(w("On the "), ui("Agents"), w(" tab, turn "), ui("On"), w(" each agent that "+
					"should use Google Cloud, choose its "), ui("Access"), w(" level and "), ui("Projects"),
					w(", then press "), ui("Review changes"), w(" and "), ui("Save and sync"), w(".")),
				Result: vendortext.Plain("Crewlet creates each agent's service account in the identity project and " +
					"grants its level on its projects. An agent you do not turn on has no access."),
			},
		},
	}
}
