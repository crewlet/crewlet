package appstoreconnect

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// defaultConnection is the connection the form fills for a company that names
// none yet.
const defaultConnection = "main"

// slotWords are each key slot's label and what it unlocks, as the form shows
// them.
var slotWords = map[string][2]string{
	"admin":             {"Admin key", "Users, invitations and every account change, each asked of an administrator."},
	"app_manager":       {"App Manager key", "Versions, submissions, prices, in-app purchases and external TestFlight."},
	"developer":         {"Developer key", "Builds and TestFlight; with access to certificates, identifiers and profiles, signing and devices."},
	"marketing":         {"Marketing key", "Listings, reviews, custom product pages, in-app events and internal TestFlight. Recommended first."},
	"finance":           {"Finance key", "Sales, finance and analytics reports."},
	"sales":             {"Sales key", "Sales and analytics reports."},
	"customer_support":  {"Customer Support key", "Replies to customer reviews."},
	"access_to_reports": {"Access to Reports key", "Sales and trends reports."},
}

// Requirements says what this company still needs for App Store Connect, per
// connection it names (or the first one): the team's issuer ID and vendor
// number, and each role's key. A nil block answers every field absent, so the
// form still renders.
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
		base := "integrations." + Kind + ".connections." + id
		issuer := setup.Requirement{Field: id + "_issuer_id", Label: "Issuer ID", Kind: setup.KindID,
			ConfigPath: base + ".issuer_id", Required: true, Connect: true, Shape: setup.ShapeUUID,
			Format: "a UUID", Hint: "Shown above the keys on {link}.",
			More:     "The Account Holder requests App Store Connect API access once, in Users and Access, Integrations.",
			LinkText: "Users and Access, Integrations", VendorURL: "https://appstoreconnect.apple.com/access/integrations/api",
			Blocks: integration.FindingCredentialMissing}
		issuer.Present, issuer.Resolved, issuer.Stored = setup.Held(team.IssuerID, resolve)
		vendor := setup.Requirement{Field: id + "_vendor_number", Label: "Vendor number", Kind: setup.KindID,
			ConfigPath: base + ".vendor_number", Format: "digits",
			Hint: "Optional: on Payments and Financial Reports, for sales reports."}
		vendor.Present, vendor.Resolved, vendor.Stored = setup.Held(team.VendorNumber, resolve)
		reqs = append(reqs, issuer, vendor)
		filled := map[string]*config.AppStoreConnectKey{}
		for _, s := range team.Keys.Slots() {
			filled[s.Name] = s.Key
		}
		for _, slot := range config.AppStoreConnectSlots() {
			words := slotWords[slot]
			var key config.AppStoreConnectKey
			if k := filled[slot]; k != nil {
				key = *k
			}
			keyID := setup.Requirement{Field: id + "_" + slot + "_key_id", Label: words[0] + " ID", Kind: setup.KindID,
				ConfigPath: base + ".keys." + slot + ".key_id", Shape: setup.ShapeAppleKeyID,
				Format: "ten capital letters or digits", Hint: words[1]}
			keyID.Present, keyID.Resolved, keyID.Stored = setup.Held(key.KeyID, resolve)
			private := setup.Requirement{Field: id + "_" + slot + "_private_key", Label: words[0] + " (.p8)",
				Kind: setup.KindSecret, ConfigPath: base + ".keys." + slot + ".private_key",
				SecretName: secretName(id, slot), Shape: setup.ShapeAppleKey,
				Hint:      "A team key with this role, from {link}; paste its .p8 file.",
				CheckNote: "Each key is proven with its team's issuer on the next pass, once saved.",
				LinkText:  "Team Keys", VendorURL: "https://appstoreconnect.apple.com/access/integrations/api"}
			private.Present, private.Resolved, private.Stored = setup.Held(key.PrivateKey, resolve)
			reqs = append(reqs, keyID, private)
		}
	}
	return reqs
}

// secretName is the sealed name of a connection's key for slot.
func secretName(connection, slot string) string {
	return strings.ToUpper("APP_STORE_CONNECT_" + strings.ReplaceAll(connection, "-", "_") + "_" + slot + "_KEY")
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet keeps your team's API keys, one per Apple role, and calls through the key with the " +
			"least access. No agent holds a key.",
	}
}
