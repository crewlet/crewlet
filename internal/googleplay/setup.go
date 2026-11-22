package googleplay

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
)

// usersURL is Play Console's users and permissions, where a person invites an
// identity.
const usersURL = "https://play.google.com/console/developers/users-and-permissions"

// Requirements says what the company still needs for Google Play: the
// integration turned on, and the company's reader invited with View app
// information for every app (account-wide, which the reports bucket needs),
// which a person does in Play Console. Each agent's own identity is the
// Agents tab's: its row shows the email to invite and the permissions its
// level needs.
func Requirements(c *config.Company) []setup.Requirement {
	var enabled bool
	if c != nil && c.Integrations.GooglePlay != nil {
		enabled = c.Integrations.GooglePlay.Enabled
	}
	return append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Google Play", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: permView + "_GLOBAL",
			Name: "View app information and download bulk reports (all apps)"},
			setup.IdentityWhere{URL: usersURL, Words: "Users and permissions, Invite new users, Account permissions"},
			"enough to read every app, vitals and the monthly reports bucket; it changes nothing.")...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use Google Play apps as their own Google identity, with the permissions you give " +
			"it; what customers see is asked first. No token reaches an agent's box.",
		Prepare: []setup.Step{
			{Text: "Connect Google Cloud first; no project needs preparing. Its Cloud Shell block, run again, turns " +
				"on the Google Play Android Developer and Developer Reporting APIs and makes the company reader."},
			{Text: "Invite the company reader below in Play Console with View app information for all apps, and " +
				"name each account's monthly reports bucket (Download reports, Copy Cloud Storage URI)."},
			{Text: "Turn agents on from the Agents tab, then invite each one's email in Play Console with the app " +
				"permissions its level needs: View app information for read; add Release to testing tracks, " +
				"Release to production, Manage store presence, Reply to reviews and Manage testing tracks for " +
				"operate; Manage orders and subscriptions or Admin only where it asks for refunds or user changes."},
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
