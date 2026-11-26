package googleanalytics

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// seatAccess is Google Analytics' half of per-seat access, the shape every
// Google growth product shares (googlegrowth.SeatAccess): its levels and its
// properties. A value the compiler lays out, so registering it at init
// allocates nothing of its own.
var seatAccess = googlegrowth.SeatAccess{Product: Kind, Lane: seataccess.LaneGoogleAnalytics,
	Name: "Google Analytics", Levels: levels, Scope: propertiesOf, Identity: identity, Adds: true, List: list,
	Typed: config.GoogleAnalyticsPropertyPattern}

func init() { seataccess.Register(&seatAccess) }

// propertiesOf is the properties the company names, nil where it names none.
func propertiesOf(c *config.Company) []string {
	if c.Integrations.GoogleAnalytics == nil {
		return nil
	}
	return c.Integrations.GoogleAnalytics.Properties
}

// levels is the three levels, least first. Operate and administer each raise
// what the agent may do, so granting either needs a step-up.
func levels() []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Reports and every setting, nothing changed. Add the " +
			"agent's identity to each property at Viewer or above."},
		{ID: seataccess.GrowthLevelID(string(growth.LevelOperate), config.GoogleAnalyticsMarketer),
			Name: "Marketer", Administers: true, Note: "Operate held to Marketer's changes: audiences, key events, " +
				"attribution and event create rules, made at once and recorded. Needs Marketer on the property, " +
				"which only a person adds, and Crewlet cannot read it back."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus reversible " +
			"changes made at once and recorded: key events, custom definitions, audiences, property and stream " +
			"settings. Needs Editor on the property."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus asking " +
			"an administrator to approve each account, access, link, key, data retention or irreversible change. " +
			"Needs Editor, or Administrator for access."},
	}
}
