package adsense

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
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
	base := "integrations." + string(Kind)
	enabled := setup.Requirement{
		// Hidden and on: connecting a switched-off integration is not a thing
		// anybody means.
		Field: "enabled", Label: "Let agents reach AdSense", Kind: setup.KindToggle,
		ConfigPath: base + ".enabled", Required: true, Hidden: true, Default: "true",
	}
	enabled.Present, enabled.Resolved, enabled.Stored = setup.Toggle(block.Enabled)
	ask := googlegrowth.OAuthClientAsk{Kind: string(Kind), Client: block.OAuth, Required: true,
		Purpose: vendortext.Of(vendortext.W("The client's ID, from "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(".")),
		SignIn: vendortext.Of(vendortext.W("As an AdSense "), vendortext.UI("Administrator"),
			vendortext.W(" or "), vendortext.UI("Standard"), vendortext.W(" user; again once agents may change."))}
	return append([]setup.Requirement{enabled}, googlegrowth.OAuthClient(ask, resolve)...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "A person who manages your AdSense account signs in with Google once, and agents reach " +
			"AdSense through it. No token reaches an agent's box.",
		Prepare: []setup.Step{
			platformPartner(googlegrowth.ClientStep("AdSense Management API")),
			{Text: vendortext.Of(vendortext.W("Paste the client's ID and secret below, press "), vendortext.UI("Connect"),
				vendortext.W(", then press "), vendortext.UI("Sign in"), vendortext.W(" and sign in with Google as an "), vendortext.UI("Administrator"), vendortext.W(" or "), vendortext.UI("Standard"),
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

// platformPartner adds to step what an AdSense for Platforms partner also does
// in the client's project.
func platformPartner(step setup.Step) setup.Step {
	step.Text = append(step.Text, vendortext.W(" An AdSense for Platforms partner also has Google turn on the "+
		"AdSense Platform API there."))
	return step
}
