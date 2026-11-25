package searchconsole

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// usersURL is Search Console, where a person adds an identity to a property
// under Settings.
const usersURL = "https://search.google.com/search-console/users"

// Requirements says what the company still needs for Search Console: the
// integration turned on, and the company's reader added to each property as a
// Restricted user, which a person does in Search Console. Each agent's own
// identity is the Agents tab's: its box shows the email to add, the
// permission its level needs, and its check.
func Requirements(c *config.Company) []setup.Requirement {
	var enabled bool
	if c != nil && c.Integrations.SearchConsole != nil {
		enabled = c.Integrations.SearchConsole.Enabled
	}
	return append([]setup.Requirement{googlegrowth.Enabled(string(Kind), "Search Console", enabled)},
		googlegrowth.Reader(c, setup.IdentityRole{ID: roleRestricted, Name: "Restricted user"},
			vendortext.Of(vendortext.UI("Settings", "Users and permissions", "Add user").At(usersURL),
				vendortext.W(", on each property")),
			vendortext.Plain("enough to read search performance; it changes nothing."))...)
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents use your Search Console properties as their own Google identity, at the permission " +
			"you give it on each property. No token reaches an agent's box.",
		Prepare: []setup.Step{
			googlegrowth.CloudFirst("the Search Console, Site Verification and Indexing APIs"),
			{Text: vendortext.Of(vendortext.W("Add the company reader below to each property as a "),
				vendortext.UI("Restricted user"), vendortext.W("."))},
			{Text: vendortext.Plain("Turn agents on from the Agents tab, then set up each one's identity there: add " +
				"its email to its properties at the permission its level needs. Crewlet checks each agent's access " +
				"on every pass.")},
		},
	}
}

// Probe answers the connect form's Test button, which has nothing to check:
// the form holds no credential, and each call proves its property's access.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("Search Console", check.Field)
	}
}
