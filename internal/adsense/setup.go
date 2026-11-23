package adsense

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// credentialsURL is where a person makes the company's OAuth client.
const credentialsURL = "https://console.cloud.google.com/apis/credentials"

// Requirements says what the company still needs for AdSense: the
// integration on, and its own OAuth client, which the sign-in is made as. A
// nil block answers every field absent, so the form still renders.
func Requirements(c *config.Company, resolve func(string) (string, bool)) []setup.Requirement {
	var block config.AdSense
	if c != nil && c.Integrations.AdSense != nil {
		block = *c.Integrations.AdSense
	}
	var client config.GoogleOAuthClient
	if block.OAuth != nil {
		client = *block.OAuth
	}
	base := "integrations." + string(Kind)
	enabled := setup.Requirement{
		// Hidden and on: connecting a switched-off integration is not a thing
		// anybody means.
		Field: "enabled", Label: "Let agents reach AdSense", Kind: setup.KindToggle,
		ConfigPath: base + ".enabled", Required: true, Hidden: true, Default: "true",
	}
	enabled.Present, enabled.Resolved, enabled.Stored = setup.Toggle(block.Enabled)
	id := setup.Requirement{Field: "client_id", Label: "OAuth client ID", Kind: setup.KindID,
		ConfigPath: base + ".oauth.client_id", Required: true, Connect: true,
		Format: "<project number>-<id>.apps.googleusercontent.com",
		Hint: vendortext.Of(vendortext.W("A "), vendortext.UI("Web application"), vendortext.W(" client from "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(", in a project you control.")),
		More: vendortext.Of(vendortext.W("Add Crewlet's callback address to its "), vendortext.UI("Authorized redirect URIs"),
			vendortext.W(", turn on the AdSense Management API in the same project, and publish its consent screen: "+
				"a client left in "), vendortext.UI("Testing"), vendortext.W(" loses its sign-in after seven days.")),
		Blocks: integration.FindingCredentialMissing}
	id.Present, id.Resolved, id.Stored = setup.Held(client.ClientID, resolve)
	secret := setup.Requirement{Field: "client_secret", Label: "OAuth client secret", Kind: setup.KindSecret,
		ConfigPath: base + ".oauth.client_secret", Required: true,
		Hint: vendortext.Of(vendortext.W("The same client's secret, from "), vendortext.UI("Google Cloud", "Credentials").At(credentialsURL),
			vendortext.W("; AdMob may share it.")),
		CheckNote: "The client is proven by the Google sign-in, once saved.",
		Blocks:    integration.FindingCredentialMissing}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(client.ClientSecret, resolve)
	// THE SIGN-IN IS THE ENGINE'S TO HOLD, never a value: whether it is held
	// is the pass's finding, so the step blocks nothing here.
	consent := setup.Requirement{Field: "google_sign_in", Label: "Sign in with Google", Kind: setup.KindConsent,
		Hint: vendortext.Of(vendortext.W("As an AdSense "), vendortext.UI("Administrator"), vendortext.W(" or "),
			vendortext.UI("Standard"), vendortext.W(" user; again once agents may change."))}
	return []setup.Requirement{enabled, id, secret, consent}
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "A person who manages your AdSense account signs in with Google once, and agents reach " +
			"AdSense through it. No token reaches an agent's box.",
		Prepare: []setup.Step{
			{Text: vendortext.Of(vendortext.W("In a Google Cloud project you control, turn on the AdSense Management "+
				"API and make an OAuth client ("), vendortext.UI("Web application"), vendortext.W(") at "),
				vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(" with Crewlet's callback address as an "),
				vendortext.UI("Authorized redirect URIs"), vendortext.W(" entry. Publish its consent screen, or make it "),
				vendortext.UI("Internal"), vendortext.W(" on Google Workspace. An AdSense for Platforms partner also "+
					"has Google turn on the AdSense Platform API there."))},
			{Text: vendortext.Of(vendortext.W("Paste the client's ID and secret below, save, then sign in with Google "+
				"as an "), vendortext.UI("Administrator"), vendortext.W(" or "), vendortext.UI("Standard"),
				vendortext.W(" user of the AdSense account. Sign in again once an agent is raised to operate, to "+
					"allow changes."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, each on the publisher accounts it may reach.")},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check
// alone: the client is proven by the sign-in, and the pass proves each
// account's access through the grant.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("AdSense", check.Field)
	}
}
