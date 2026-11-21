package searchconsole

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Search Console's half of per-seat access is the shape every Google growth
// product shares (googlegrowth.SeatAccess): its levels and its sites.

func init() {
	seataccess.Register(googlegrowth.SeatAccess{
		Product: Kind, Lane: seataccess.LaneSearchConsole, Name: "Search Console", Levels: levels(),
		Scope: func(c *config.Company) []string {
			if c.Integrations.SearchConsole == nil {
				return nil
			}
			return c.Integrations.SearchConsole.Sites
		},
	})
}

// levels is the three levels, least first. Operate and administer each raise
// what the agent may do, so granting either needs a step-up.
func levels() []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Performance, sitemaps, URL inspection and " +
			"verification reads, nothing changed. Add the agent's identity to each property as a Restricted " +
			"user or above."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus submitting " +
			"and removing sitemaps and Indexing API notifications, made at once and recorded. Needs a Full " +
			"user for sitemaps, an Owner for the Indexing API."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus " +
			"asking an administrator to approve each property added or removed, site verified or owner " +
			"changed. Needs an Owner to change owners."},
	}
}
