package googleanalytics

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// accessManagementURL is Google Analytics' admin, where a person adds an
// identity to a property.
const accessManagementURL = "https://analytics.google.com/analytics/web/"

// Requirements says what the company still needs for Google Analytics: the
// integration turned on, and the company's reader added to each property at
// Viewer, which a person does in Google Analytics; and, optionally, an
// Administrator's sign-in, with which Crewlet adds each agent's identity
// itself. Each agent's own identity is the Agents tab's: its box shows the
// email to add, the role its level needs, and its check.
func Requirements(c *config.Company) []setup.Requirement {
	var enabled bool
	if c != nil && c.Integrations.GoogleAnalytics != nil {
		enabled = c.Integrations.GoogleAnalytics.Enabled
	}
	out := append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Analytics", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: roleViewer, Name: "Viewer"},
			vendortext.Of(vendortext.UI("Admin", "Property access management", "Add users").At(accessManagementURL)),
			vendortext.Plain("enough to read every report; it changes nothing."))...)
	return append(out, googlegrowth.AdminSignIn(string(Kind), "a Google Analytics Administrator")...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use your Google Analytics properties as their own Google identity, at the role you " +
			"give it on each property. No token reaches an agent's box.",
		Prepare: []setup.Step{
			googlegrowth.CloudFirst("the Analytics Data and Admin APIs"),
			{Text: vendortext.Of(vendortext.W("Add the company reader below to each property at "),
				vendortext.UI("Viewer"), vendortext.W("."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, then set up each one's identity there: add " +
				"its email to its properties at the role its level needs, or press Add it for me once an " +
				"Administrator has signed in below. Crewlet checks each agent's access on every pass.")},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check:
// the form holds no credential. The pass checks the Google identity and the
// two APIs; a property's access is Google's to answer at each call, whose
// refusal names the role the agent's identity needs.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("Google Analytics", check.Field)
	}
}
