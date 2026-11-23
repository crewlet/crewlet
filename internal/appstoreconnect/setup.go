package appstoreconnect

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// defaultConnection is the connection the form fills for a company that names
// none yet.
const defaultConnection = "main"

// Requirements says what this company still needs for App Store Connect: each
// connection's issuer ID (or the first one's), which its agents' own keys sign
// in with. A nil block answers every field absent, so the form still renders.
func Requirements(in *config.AppStoreConnect, resolve func(string) (string, bool)) []setup.Requirement {
	ids := in.ConnectionIDs()
	if len(ids) == 0 {
		ids = []string{defaultConnection}
	}
	enabled := in.GrowthEnabled()
	reqs := []setup.Requirement{{
		Field: "enabled", Label: "Connect App Store Connect", Kind: setup.KindToggle,
		ConfigPath: "integrations." + Kind + ".enabled", Required: true, Hidden: true, Default: "true",
	}}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Toggle(enabled)
	for _, id := range ids {
		var team config.AppStoreConnectTeam
		if in != nil {
			team = in.Connections[id]
		}
		issuer := setup.Requirement{Field: id + "_issuer_id", Label: "Issuer ID", Kind: setup.KindID,
			ConfigPath: "integrations." + Kind + ".connections." + id + ".issuer_id", Required: true, Connect: true,
			Shape: setup.ShapeUUID, Format: "a UUID", Hint: "Shown above the team keys on {link}.",
			LinkText: "Users and Access, Integrations", VendorURL: TeamKeysURL,
			Blocks: integration.FindingCredentialMissing}
		issuer.Present, issuer.Resolved, issuer.Stored = setup.Held(team.IssuerID, resolve)
		reqs = append(reqs, issuer)
	}
	return reqs
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent signs its calls with its own team key, which you hand in on the Agents tab. " +
			"Crewlet keeps no key of the company's.",
	}
}
