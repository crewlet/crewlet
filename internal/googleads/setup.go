package googleads

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// accessURL is Google Ads' access settings, where a person adds an identity
// to an account.
const accessURL = "https://ads.google.com/aw/accountaccess/users"

// Requirements says what the company still needs for Google Ads: the
// integration turned on, and the company's reader added to each account at
// Read-only, which a person does in Google Ads. Each agent's own identity is
// the Agents tab's: its box shows the email to add, the level it needs, and
// its check.
func Requirements(c *config.Company) []setup.Requirement {
	var enabled bool
	if c != nil && c.Integrations.GoogleAds != nil {
		enabled = c.Integrations.GoogleAds.Enabled
	}
	return append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Ads", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: roleReadOnly, Name: "Read-only"},
			vendortext.Of(vendortext.UI("Admin", "Access and security", "Users", "+").At(accessURL)),
			vendortext.Plain("enough to read every report; it changes nothing."))...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use your Google Ads accounts as their own Google identity, at the level you give it, " +
			"spend held to your caps. No token reaches an agent's box.",
		Prepare: []setup.Step{
			googlegrowth.CloudFirst("the Google Ads API"),
			{Text: vendortext.Of(vendortext.W("Apply for "), vendortext.UI("Explorer"), vendortext.W(" or "),
				vendortext.UI("Basic"), vendortext.W(" access to the Google Ads API on the identity project's API page."))},
			{Text: vendortext.Of(vendortext.W("Add the company reader below to each account at "),
				vendortext.UI("Read-only"), vendortext.W(", and set each account's spending caps before any agent may ask to spend."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, then set up each one's identity there: add " +
				"its email to its accounts, or once to their manager, at the access level its level needs. Crewlet " +
				"checks each agent's access on every pass.")},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check:
// the form holds no credential, and the pass proves each account's access.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("Google Ads", check.Field)
	}
}
