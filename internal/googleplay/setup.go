package googleplay

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// usersURL is Play Console's users and permissions, where a person invites an
// identity.
const usersURL = "https://play.google.com/console/developers/users-and-permissions"

// Requirements says what the company still needs for Google Play: the
// integration turned on, and the company's reader invited with View app
// information for every app (account-wide, which the reports bucket needs),
// which a person does in Play Console; and, optionally, a Play Console
// admin's sign-in, with which Crewlet adds each agent's identity itself. Each
// agent's own identity is the Agents tab's: its box shows the email to
// invite, the permissions its level needs, and its check.
func Requirements(c *config.Company, resolve func(string) (string, bool)) []setup.Requirement {
	var enabled bool
	var client *config.GoogleOAuthClient
	if c != nil && c.Integrations.GooglePlay != nil {
		enabled = c.Integrations.GooglePlay.Enabled
		client = c.Integrations.GooglePlay.OAuth
	}
	out := append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Play", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: permView + "_GLOBAL",
			Name: "View app information and download bulk reports (read-only)"},
			vendortext.Of(vendortext.UI("Users and permissions", "Invite new users", "Account permissions").At(usersURL)),
			vendortext.Plain("enough to read every app, vitals and the monthly reports bucket; it changes nothing."))...)
	return append(out, googlegrowth.AdminSignIn(string(Kind), "Play Console", "a Play Console admin", client,
		resolve)...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use Google Play apps as their own Google identity, with the permissions you give " +
			"it; what customers see is asked first. No token reaches an agent's box.",
		Prepare: []setup.Step{
			googlegrowth.CloudFirst("the Google Play Android Developer and Developer Reporting APIs"),
			{Text: vendortext.Of(vendortext.W("Invite the company reader below in "),
				vendortext.UI("Play Console", "Users and permissions").At(usersURL), vendortext.W(" with "),
				vendortext.UI("View app information"), vendortext.W(" for all apps, and name each account's monthly "+
					"reports bucket ("), vendortext.UI("Download reports", "Copy Cloud Storage URI"), vendortext.W(")."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, then set up each one's identity there: invite " +
				"its email in Play Console with the app permissions its level needs, or press Add it for me once a " +
				"Play Console admin has signed in below. Crewlet checks each agent's access on every pass.")},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check:
// the form holds no credential, and the pass proves each app's access.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("Google Play", check.Field)
	}
}
