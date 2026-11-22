package googleanalytics

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
)

// accessManagementURL is Google Analytics' admin, where a person adds an
// identity to a property.
const accessManagementURL = "https://analytics.google.com/analytics/web/"

// Requirements says what the company still needs for Google Analytics: the
// integration turned on, and the company's reader added to each property at
// Viewer, which a person does in Google Analytics. Each agent's own identity
// is the Agents tab's: its row shows the email to add and the role its level
// needs.
func Requirements(c *config.Company) []setup.Requirement {
	var enabled bool
	if c != nil && c.Integrations.GoogleAnalytics != nil {
		enabled = c.Integrations.GoogleAnalytics.Enabled
	}
	return append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Analytics", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: roleViewer, Name: "Viewer"},
			setup.IdentityWhere{URL: accessManagementURL, Words: "Admin, Property access management, Add users"},
			"enough to read every report; it changes nothing.")...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use your Google Analytics properties as their own Google identity, at the role you " +
			"give it on each property. No token reaches an agent's box.",
		Prepare: []setup.Step{
			{Text: "Connect Google Cloud first; no project needs preparing. Its Cloud Shell block, run again, " +
				"turns on the Analytics Data and Admin APIs and makes the company reader."},
			{Text: "Add the company reader below to each property at Viewer."},
			{Text: "Turn agents on from the Agents tab, then add each one's email to its properties at the role " +
				"its level needs: Viewer for read, Marketer or Editor for operate, Editor for administer."},
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
