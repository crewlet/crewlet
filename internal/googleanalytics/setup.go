package googleanalytics

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// roleViewerName is Google Analytics' read role, as its Add users page names it.
const roleViewerName = "Viewer"

// adminHome is Google Analytics' web app, whose Admin adds an identity to an
// account or a property.
const adminHome = "https://analytics.google.com/analytics/web/"

// Requirements says what the company still needs for Google Analytics: the
// integration turned on, and the company's reader added to the account at
// Viewer, which a person does in Google Analytics; and, optionally, an
// Administrator's sign-in, with which Crewlet adds each agent's identity
// itself. Each agent's own identity is the Agents tab's: its box shows the
// email to add, the role its level needs, and its check.
func Requirements(c *config.Company, resolve func(string) (string, bool)) []setup.Requirement {
	var enabled bool
	var client *config.GoogleOAuthClient
	if c != nil && c.Integrations.GoogleAnalytics != nil {
		enabled = c.Integrations.GoogleAnalytics.Enabled
		client = c.Integrations.GoogleAnalytics.OAuth
	}
	out := append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Analytics", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: roleViewer, Name: roleViewerName},
			vendortext.Of(vendortext.UI("Admin", "Account access management", "Add users").At(adminHome)),
			vendortext.Of(vendortext.W("enough to list and read every property of the account; it changes "+
				"nothing. Untick "), vendortext.UI("Notify new users by email"), vendortext.W(". To share only "+
				"some properties, add it under "), vendortext.UI("Property access management"),
				vendortext.W(" of each instead.")))...)
	return append(out, googlegrowth.AdminSignIn(string(Kind), "Google Analytics",
		"a Google Analytics Administrator", client, resolve)...)
}

// Guide is what the connect form says around its fields: Google Cloud's
// shared first steps, the company reader on the account, then each agent
// on, by property ID, and its own account added there.
func Guide() setup.Guide {
	w, ui := vendortext.W, vendortext.UI
	steps := googlegrowth.CloudSteps("Google Analytics", "the Analytics Data and Admin APIs")
	add := vendortext.Of(ui("Admin", "Account access management").At(adminHome), w(", press "),
		ui("+"), w(", then "), ui("Add users"))
	steps = append(steps,
		setup.Step{
			Title: "Add the company reader", Where: "Google Analytics",
			Text: append(append(vendortext.Of(w("In ")), add...), w(". Paste the company reader's email "+
				"shown on this form, untick "), ui("Notify new users by email"), w(", choose "), ui(roleViewerName), w(" and press "),
				ui("Add"), w(". To share only some properties, add it under "),
				ui("Property access management"), w(" of each instead.")),
			Result: vendortext.Plain("The account lists the reader with the Viewer role."),
		},
		googlegrowth.AgentsOn("Google Analytics", "Properties"),
		setup.Step{
			Title: "Add each agent's account", Where: "Google Analytics",
			Text: vendortext.Of(w("When the agent's row reads Needs you, add its account to each property its "+
				"identity steps list, under "), ui("Property access management"), w(", at the role they name: "),
				ui(roleViewerName), w(" for Read, "), ui("Editor"), w(" for Operate. With an Administrator signed in "+
					"below, press "), ui("Add it for me"), w(" instead.")),
			Result: vendortext.Plain("The agent's row reads In sync once each property holds that role."),
			Trouble: []setup.Trouble{
				googlegrowth.LateAccount("This email doesn't match a Google Account"),
				{
					Sign: vendortext.Plain("A property you want is not listed under Properties."),
					Fix: vendortext.Of(w("Add the company reader to it first, or type its numeric ID: the number "+
						"under its name in the property picker, or in "), ui("Admin", "Property details"), w(".")),
				},
			},
		},
	)
	return setup.Guide{
		Summary: "Agents use your Google Analytics properties as their own Google identity, at the role you " +
			"give it on each property. No token reaches an agent's box.",
		Prepare: steps,
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
