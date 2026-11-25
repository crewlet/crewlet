package googleads

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// seatAccess is Google Ads' half of per-seat access, the shape every Google
// growth product shares (googlegrowth.SeatAccess): its levels and its
// accounts. A value the compiler lays out, so registering it at init
// allocates nothing of its own.
var seatAccess = googlegrowth.SeatAccess{Product: Kind, Lane: seataccess.LaneGoogleAds, Name: "Google Ads",
	Levels: levels, Scope: customersOf, Identity: identity, List: listAccounts, Typed: config.GoogleAdsCustomerPattern}

func init() { seataccess.Register(&seatAccess) }

// customersOf is the accounts the company names, nil where it names none.
func customersOf(c *config.Company) []string {
	if c.Integrations.GoogleAds == nil {
		return nil
	}
	var out []string
	for _, customer := range c.Integrations.GoogleAds.Customers {
		out = append(out, customer.ID)
	}
	return out
}

// levels is the three levels, least first. Operate and administer each raise
// what the agent may do, so granting either needs a step-up.
func levels() []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Reports, planning and every setting, nothing changed. " +
			"Add the agent's identity to each account at Read-only or above."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus changes that spend " +
			"nothing more, made at once and recorded: pausing, lowering budgets and bids, negatives, labels, " +
			"audiences. Needs Standard on the account."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus asking an " +
			"administrator to approve each change that spends, publishes ad text, removes, or touches users, links " +
			"and billing, spend held to the account's caps. Needs Standard, or Admin for users, links and billing."},
	}
}
