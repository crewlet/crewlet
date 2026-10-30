package slack

import (
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The variables a seat's Slack app is sealed under, named by the engine.
//
// NEVER BY A WRITER. A seat's bot token used to live wherever its
// `bot_token: ${VAR}` pointed, so a document edit could point it at a company
// credential and the next install would overwrite that credential with the
// seat's token (the class of the Datadog incident). The names are derived
// here, from the handle, with [seataccess.SinkName], the one naming function
// the access sync seals under and every reader derives from, so they agree by
// construction (design 3).
//
// ONE TOKEN, TWO READERS. The bot token sealed under [FieldBotToken] is what
// the transport authenticates with and what the seat's Slack tool server is
// handed, so the two can never hold different tokens (design 12.1).

// The fields a seat's app keeps under its own name ([SeatVar]).
const (
	// FieldBotToken is the bot token a workspace install minted.
	FieldBotToken = "BOT_TOKEN"

	// FieldSigningSecret is the secret Slack signs the app's deliveries
	// with, served once at creation.
	FieldSigningSecret = "SIGNING_SECRET"

	// FieldClientSecret is the app's OAuth client secret, served once at
	// creation and needed for every later install of the app.
	FieldClientSecret = "CLIENT_SECRET"
)

// SeatVar is the variable one seat's app value lives under:
// `SLACK_BOT_TOKEN_SENIOR_ENGINEER` for `senior-engineer`'s bot token.
func SeatVar(handle, field string) string {
	return seataccess.SinkName(integration.KindSlack, field, handle)
}

// SeatVars is every variable one seat's app is sealed under, for a removal
// that clears them all.
func SeatVars(handle string) []string {
	return []string{
		SeatVar(handle, FieldBotToken),
		SeatVar(handle, FieldSigningSecret),
		SeatVar(handle, FieldClientSecret),
	}
}

// ConfigTokenVar is the company variable the app-configuration token pair is
// sealed under, as one value ([EncodeConfigToken] says why one).
const ConfigTokenVar = "SLACK_APP_CONFIG_TOKEN"
