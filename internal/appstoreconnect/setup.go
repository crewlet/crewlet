package appstoreconnect

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// defaultConnection is the connection the form fills for a company that names
// none yet.
const defaultConnection = "main"

// slotWords are each key slot's role and what a key with it unlocks, as the
// form's role picker shows them.
var slotWords = map[string][2]string{
	"admin":             {"Admin", "Users and keys; used only for changes an administrator allows."},
	"app_manager":       {"App Manager", "Versions, submissions, prices, in-app purchases and external TestFlight."},
	"developer":         {"Developer", "Builds and TestFlight; with that access, certificates, profiles and devices."},
	"marketing":         {"Marketing", "Listings, reviews, custom product pages, in-app events and internal TestFlight."},
	"finance":           {"Finance", "Sales, finance and analytics reports."},
	"sales":             {"Sales", "Sales and analytics reports."},
	"customer_support":  {"Customer Support", "Replies to customer reviews."},
	"access_to_reports": {"Access to Reports", "Sales and trends reports."},
}

// firstSlot is the role a team's first key is recommended at: it reads what
// most agents need and changes nothing customers see.
const firstSlot = "marketing"

// keySet is a connection's API keys as one list the form adds to, a key at a
// time, by role: at least one, as config requires.
func keySet(connection, slot string) *setup.Set {
	words := slotWords[slot]
	return &setup.Set{
		ID: connection + "_keys", Label: "API keys", Item: "key", Add: "Add a key", Pick: "Role", Min: 1,
		Hint: vendortext.Of(vendortext.W("A key ID and `.p8` file per Apple role: at least one, "),
			vendortext.UI(slotWords[firstSlot][0]), vendortext.W(" first.")),
		Entry: setup.Choice{Value: slot, Label: words[0], Hint: words[1]},
	}
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
			Format: "a UUID", Hint: vendortext.Of(vendortext.W("Shown above the keys on "),
				vendortext.UI("Users and Access", "Integrations").At(TeamKeysURL), vendortext.W(".")),
			More: vendortext.Of(vendortext.W("The Account Holder requests App Store Connect API access once, in "),
				vendortext.UI("Users and Access", "Integrations").At(TeamKeysURL), vendortext.W(".")),
			Blocks: integration.FindingCredentialMissing}
		issuer.Present, issuer.Resolved, issuer.Stored = setup.Held(team.IssuerID, resolve)
		vendor := setup.Requirement{Field: id + "_vendor_number", Label: "Vendor number", Kind: setup.KindID,
			ConfigPath: base + ".vendor_number", Format: "digits",
			Hint: vendortext.Of(vendortext.W("Optional: on "), vendortext.UI("Payments and Financial Reports"),
				vendortext.W(", for sales reports."))}
		vendor.Present, vendor.Resolved, vendor.Stored = setup.Held(team.VendorNumber, resolve)
		reqs = append(reqs, issuer, vendor)
		filled := map[string]*config.AppStoreConnectKey{}
		for _, s := range team.Keys.Slots() {
			filled[s.Name] = s.Key
		}
		// THE RECOMMENDED ROLE FIRST, then the declared order: the form's
		// role picker offers them in this order.
		slots := append([]string{firstSlot}, slices.DeleteFunc(config.AppStoreConnectSlots(),
			func(slot string) bool { return slot == firstSlot })...)
		for _, slot := range slots {
			var key config.AppStoreConnectKey
			if k := filled[slot]; k != nil {
				key = *k
			}
			set := keySet(id, slot)
			keyID := setup.Requirement{Field: id + "_" + slot + "_key_id", Label: "Key ID", Kind: setup.KindID,
				ConfigPath: base + ".keys." + slot + ".key_id", Shape: setup.ShapeAppleKeyID, Set: set, Connect: true,
				Format: "ten capital letters or digits", Hint: vendortext.Of(vendortext.W("Shown beside the key on "),
					vendortext.UI("Users and Access", "Integrations").At(TeamKeysURL), vendortext.W("."))}
			keyID.Present, keyID.Resolved, keyID.Stored = setup.Held(key.KeyID, resolve)
			private := setup.Requirement{Field: id + "_" + slot + "_private_key", Label: "Private key (.p8)",
				Kind: setup.KindSecret, ConfigPath: base + ".keys." + slot + ".private_key",
				SecretName: secretName(id, slot), Shape: setup.ShapeAppleKey, Set: set, Connect: true,
				Hint:      vendortext.Plain("Apple offers the `.p8` file once: choose it, or paste it whole."),
				CheckNote: "Each key is proven with its team's issuer on the next pass, once saved."}
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
		Summary: "Crewlet keeps your team's API keys and makes each call through the least-privileged key " +
			"that can make it. No agent holds a key.",
	}
}
