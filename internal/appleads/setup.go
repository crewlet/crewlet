package appleads

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// defaultConnection is the connection the form fills for a company that names
// none yet.
const defaultConnection = "main"

// apiSettings is where an API user's public key is uploaded and its IDs shown.
const apiSettings = "https://ads.apple.com"

// Requirements says what this company still needs for Apple Ads, per
// connection it names (or the first one): the API user's client, team and key
// IDs and its private key, and optionally its organization. A nil block
// answers every field absent, so the form still renders.
func Requirements(in *config.AppleAds, resolve func(string) (string, bool)) []setup.Requirement {
	ids := in.ConnectionIDs()
	if len(ids) == 0 {
		ids = []string{defaultConnection}
	}
	reqs := []setup.Requirement{{
		Field: "enabled", Label: "Connect Apple Ads", Kind: setup.KindToggle,
		ConfigPath: "integrations." + Kind + ".enabled", Required: true, Hidden: true, Default: "true",
	}}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(in.GrowthEnabled())
	for _, id := range ids {
		var c config.AppleAdsConnection
		if in != nil {
			c = in.Connections[id]
		}
		base := "integrations." + Kind + ".connections." + id
		client := setup.Requirement{Field: id + "_client_id", Label: "Client ID", Kind: setup.KindID,
			ConfigPath: base + ".client_id", Required: true, Connect: true, Shape: setup.ShapeAppleAdsID,
			Format: "SEARCHADS. and an identifier",
			Hint: vendortext.Of(vendortext.W("Shown on "), vendortext.UI("Account Settings", "API").At(apiSettings),
				vendortext.W(" once the public key is uploaded.")),
			More: vendortext.Of(vendortext.W("An account administrator invites an API user with the role agents need; "+
				"signed in as that user, upload the public key of the private key below under "),
				vendortext.UI("Account Settings", "API").At(apiSettings), vendortext.W(".")),
			Blocks: integration.FindingCredentialMissing}
		client.Present, client.Resolved, client.Stored = setup.Held(c.ClientID, resolve)
		team := setup.Requirement{Field: id + "_team_id", Label: "Team ID", Kind: setup.KindID,
			ConfigPath: base + ".team_id", Required: true, Shape: setup.ShapeAppleAdsID,
			Format: "SEARCHADS. and an identifier", Hint: vendortext.Plain("Shown beside the client ID.")}
		team.Present, team.Resolved, team.Stored = setup.Held(c.TeamID, resolve)
		key := setup.Requirement{Field: id + "_key_id", Label: "Key ID", Kind: setup.KindID,
			ConfigPath: base + ".key_id", Required: true, Format: "as Apple shows it",
			Hint: vendortext.Plain("Shown beside the client ID.")}
		key.Present, key.Resolved, key.Stored = setup.Held(c.KeyID, resolve)
		org := setup.Requirement{Field: id + "_org_id", Label: "Organization ID", Kind: setup.KindID,
			ConfigPath: base + ".org_id", Format: "digits",
			Hint: vendortext.Plain("Optional: when set, the pass checks the API user answers this organization.")}
		org.Present, org.Resolved, org.Stored = setup.Held(c.OrgID, resolve)
		private := setup.Requirement{Field: id + "_private_key", Label: "Private key", Kind: setup.KindSecret,
			ConfigPath: base + ".private_key", SecretName: secretName(id), Required: true, Shape: setup.ShapeAppleAdsKey,
			Hint:      vendortext.Plain("The whole file `openssl ecparam` wrote; its public half is uploaded to Apple Ads."),
			CheckNote: "The key is proven with Apple on the next pass, once saved."}
		private.Present, private.Resolved, private.Stored = setup.Held(c.PrivateKey, resolve)
		reqs = append(reqs, client, team, key, org, private)
	}
	return reqs
}

// secretName is the sealed name of a connection's private key.
func secretName(connection string) string {
	return strings.ToUpper("APPLE_ADS_" + strings.ReplaceAll(connection, "-", "_") + "_KEY")
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet keeps your API users' keys and makes every call for the agents, within each user's " +
			"roles. No agent holds a key.",
	}
}
