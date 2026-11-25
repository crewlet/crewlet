package googleplay

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Google Play's half of per-seat access is the shape every Google growth
// product shares (googlegrowth.SeatAccess): its levels and its apps.

func init() {
	seataccess.Register(googlegrowth.SeatAccess{
		Product: Kind, Lane: seataccess.LaneGooglePlay, Name: "Google Play", Levels: levels,
		Identity: identity, Adds: true, List: list, Typed: config.GooglePlayPackagePattern,
		Scope: func(c *config.Company) []string {
			if c.Integrations.GooglePlay == nil {
				return nil
			}
			return c.Integrations.GooglePlay.Apps
		},
	})
}

// levels is the three levels, least first. Operate and administer each raise
// what the agent may do, so granting either needs a step-up.
func levels() []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Reviews, listings, releases, products, vitals and " +
			"reports, nothing changed. Invite the agent's identity with View app information on each app."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus draft releases, " +
			"halted rollouts, tester groups and draft products made at once and recorded, and asking an " +
			"administrator to approve each reply, listing, image, release, price and refund. Needs the app " +
			"permissions of what it does: Reply to reviews, Manage store presence, Release to testing tracks or " +
			"to production, Manage testing tracks, Manage orders."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus asking " +
			"an administrator to approve user, access, signing key, tax, deletion and other irreversible changes. " +
			"Needs Admin for users and access."},
	}
}
