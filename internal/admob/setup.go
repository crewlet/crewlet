package admob

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// credentialsURL is where a person makes the company's OAuth client.
const credentialsURL = "https://console.cloud.google.com/apis/credentials"

// Requirements says what the company still needs for AdMob: the integration
// on, and its own OAuth client, which the sign-in is made as. A nil block
// answers every field absent, so the form still renders.
func Requirements(c *config.Company, resolve func(string) (string, bool)) []setup.Requirement {
	var block config.AdMob
	if c != nil && c.Integrations.AdMob != nil {
		block = *c.Integrations.AdMob
	}
	var client config.GoogleOAuthClient
	if block.OAuth != nil {
		client = *block.OAuth
	}
	base := "integrations." + string(Kind)
	enabled := setup.Requirement{
		// Hidden and on: connecting a switched-off integration is not a thing
		// anybody means.
		Field: "enabled", Label: "Let agents reach AdMob", Kind: setup.KindToggle,
		ConfigPath: base + ".enabled", Required: true, Hidden: true, Default: "true",
	}
	enabled.Present, enabled.Resolved, enabled.Stored = setup.Toggle(block.Enabled)
	id := setup.Requirement{Field: "client_id", Label: "OAuth client ID", Kind: setup.KindID,
		ConfigPath: base + ".oauth.client_id", Required: true, Connect: true,
		Format: "<project number>-<id>.apps.googleusercontent.com",
		Hint:   "A Web application client from {link}, in a project you control.",
		More: "Add Crewlet's callback address to its authorized redirect URIs, turn on the AdMob API in the " +
			"same project, and publish its consent screen: a client left in Testing loses its sign-in after " +
			"seven days.",
		LinkText: "Google Cloud, Credentials", VendorURL: credentialsURL, Blocks: integration.FindingCredentialMissing}
	id.Present, id.Resolved, id.Stored = setup.Held(client.ClientID, resolve)
	secret := setup.Requirement{Field: "client_secret", Label: "OAuth client secret", Kind: setup.KindSecret,
		ConfigPath: base + ".oauth.client_secret", Required: true,
		Hint:      "The same client's secret, from {link}; AdSense may share it.",
		CheckNote: "The client is proven by the Google sign-in, once saved.",
		LinkText:  "Google Cloud, Credentials", VendorURL: credentialsURL, Blocks: integration.FindingCredentialMissing}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(client.ClientSecret, resolve)
	// THE SIGN-IN IS THE ENGINE'S TO HOLD, never a value: whether it is held
	// is the pass's finding, so the step blocks nothing here.
	consent := setup.Requirement{Field: "google_sign_in", Label: "Sign in with Google", Kind: setup.KindConsent,
		Hint: "As a person who manages the AdMob account; again once agents may change."}
	return []setup.Requirement{enabled, id, secret, consent}
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "A person who manages your AdMob account signs in with Google once, and agents reach AdMob " +
			"through it. No token reaches an agent's box.",
		Prepare: []setup.Step{
			{Text: "In a Google Cloud project you control, turn on the AdMob API and make an OAuth client " +
				"(Web application) with Crewlet's callback address as an authorized redirect URI. Publish its " +
				"consent screen, or make it Internal on Google Workspace."},
			{Text: "Paste the client's ID and secret below, save, then sign in with Google as a person who " +
				"manages the AdMob account: Administrator, Manager, or a custom role with the permissions the " +
				"agents need. Sign in again once an agent is raised to operate, to allow changes."},
			{Text: "Turn agents on from the Agents tab, each on the publisher accounts it may reach."},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check
// alone: the client is proven by the sign-in, and the pass proves each
// account's access through the grant.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("AdMob", check.Field)
	}
}
