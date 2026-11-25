package admob

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// Requirements says what the company still needs for AdMob: the integration
// on, and a person's Google sign-in, made as the company's sign-in client
// (googlegrowth.SignInClient). A nil block answers every field absent.
func Requirements(c *config.Company) []setup.Requirement {
	var block config.AdMob
	if c != nil && c.Integrations.AdMob != nil {
		block = *c.Integrations.AdMob
	}
	base := "integrations." + string(Kind)
	enabled := setup.Requirement{
		// Hidden and on: connecting a switched-off integration is not a thing
		// anybody means.
		Field: "enabled", Label: "Let agents reach AdMob", Kind: setup.KindToggle,
		ConfigPath: base + ".enabled", Required: true, Hidden: true, Default: "true",
	}
	enabled.Present, enabled.Resolved, enabled.Stored = setup.Toggle(block.Enabled)
	signIn := googlegrowth.SignIn(googlegrowth.SignInAsk{Kind: string(Kind), Required: true,
		SignIn: vendortext.Plain("As a person who manages the AdMob account; again once agents may change.")})
	return append([]setup.Requirement{enabled}, signIn...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "A person who manages your AdMob account signs in with Google once, and agents reach AdMob " +
			"through it. No token reaches an agent's box.",
		Prepare: []setup.Step{
			googlegrowth.ClientStep("AdMob API"),
			{Text: vendortext.Of(vendortext.W("Paste the client's ID and secret below, press "), vendortext.UI("Connect"),
				vendortext.W(", then press "), vendortext.UI("Sign in"), vendortext.W(" and sign in with Google "+
					"as a person who manages the AdMob account: "), vendortext.UI("Administrator"), vendortext.W(", "),
				vendortext.UI("Manager"), vendortext.W(", or a custom role with the permissions the agents need. "+
					"Sign in again once an agent is raised to operate, to allow changes."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, each on the publisher accounts it may reach.")},
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
