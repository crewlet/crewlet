package googleanalytics

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Google Analytics' half of per-seat access is the shape every Google growth
// product shares (googlegrowth.SeatAccess): its levels and its properties.

func init() {
	seataccess.Register(googlegrowth.SeatAccess{
		Product: Kind, Lane: seataccess.LaneGoogleAnalytics, Name: "Google Analytics", Levels: levels(),
		Scope: func(c *config.Company) []string {
			if c.Integrations.GoogleAnalytics == nil {
				return nil
			}
			return c.Integrations.GoogleAnalytics.Properties
		},
	})
}

// levels is the three levels, least first. Operate and administer each raise
// what the agent may do, so granting either needs a step-up.
func levels() []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Reports and every setting, nothing changed. Add the " +
			"agent's identity to each property at Viewer or above."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus reversible " +
			"changes made at once and recorded: key events, custom definitions, audiences, settings. Needs " +
			"Marketer or Editor on the property."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus asking " +
			"an administrator to approve each account, access, link, key or irreversible change. Needs Editor, " +
			"or Administrator for access."},
	}
}
