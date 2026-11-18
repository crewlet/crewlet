package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// ONE ANSWER TO "WHICH VARIABLE HOLDS THIS SEAT'S CREDENTIAL".
//
// Chat transports, webhook signature checks and the GitHub issuer read a seat's
// credential off its identity in the org model, where engine-owned and external
// credentials are told apart once (D4a, D4b):
//   - ENGINE mode: the seat's own sealed name ([envref.SealedName]); only the
//     engine seals there, so the document names none.
//   - EXTERNAL mode: the variable the role position names, as written.
//   - A seat off at the integration carries no identity: `enabled` decides.
// ONLY ONCE THE INTEGRATION IS MIGRATED onto per-seat access
// ([Company.SeatAccessMigratedFor]): stored blocks have no `enabled` and would
// read as off, taking every transport down on upgrade.

// bindSeatCredentials settles each agent seat's chat and code-host identities
// as above. It runs where the org model is built ([Company.organization]).
func (c *Company) bindSeatCredentials(o *org.Organization) {
	seats := map[string]*org.Role{}
	for seat := range o.AllRoles() {
		if seat.IsAgent() {
			seats[seat.Handle()] = seat
		}
	}
	sealed := func(kind, field, handle, written string) string {
		if c.documentMode(kind) == SeatCredentialEngine && strings.TrimSpace(written) == "" {
			return "${" + envref.SealedName(kind, field, handle) + "}"
		}
		return written
	}
	for role := range c.EachRole() {
		// BY THE DERIVED HANDLE, which is the org model's key and the one
		// every sealed name is spelled from.
		seat := seats[role.IdentityKey()]
		if seat == nil {
			continue
		}
		h := seat.Handle()
		if c.SeatAccessMigratedFor("slack") {
			if role.SeatAccessEnabled("slack") {
				seat.Slack.BotToken = sealed("slack", "BOT_TOKEN", h, seat.Slack.BotToken)
				seat.Slack.SigningSecret = sealed("slack", "SIGNING_SECRET", h, seat.Slack.SigningSecret)
			} else {
				seat.Slack = org.SlackIdentity{}
			}
		}
		if c.SeatAccessMigratedFor("mattermost") {
			if role.SeatAccessEnabled("mattermost") {
				seat.Mattermost.BotToken = sealed("mattermost", "BOT_TOKEN", h, seat.Mattermost.BotToken)
			} else {
				seat.Mattermost = org.MattermostIdentity{}
			}
		}
		if c.SeatAccessMigratedFor("github") && seat.GitHub != nil && seat.GitHub.Enabled {
			seat.GitHub.PrivateKey = sealed("github", "APP_KEY", h, seat.GitHub.PrivateKey)
			seat.GitHub.WebhookSecret = sealed("github", "APP_WEBHOOK_SECRET", h, seat.GitHub.WebhookSecret)
		}
	}
}
