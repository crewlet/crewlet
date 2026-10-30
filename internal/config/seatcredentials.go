package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// ONE ANSWER TO "WHICH VARIABLE HOLDS THIS SEAT'S CREDENTIAL".
//
// The chat transports, the webhook signature checks and the GitHub issuer read
// a seat's credential off its identity in the org model, so the identity is
// where engine-owned and external credentials are told apart, once, for every
// reader (D4a, D4b):
//
//   - In ENGINE mode the identity names the seat's own sealed name
//     ([envref.SealedName]) wherever the document names none, which it does
//     not: the engine seals there and nothing else does.
//   - In EXTERNAL mode the identity carries the variable the seat's role
//     position names, exactly as written.
//   - A seat that is off at the integration carries no identity there:
//     `enabled` decides, never a variable's presence.
//
// ONLY ONCE THE INTEGRATION IS MIGRATED onto per-seat access
// ([Company.SeatAccessMigratedFor]). Before, a stored document's blocks have
// no `enabled` and would all read as off, which would take every transport
// down on the upgrade the migration exists to make safe; until then the
// identity is the document's, as written.

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
