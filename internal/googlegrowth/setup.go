package googlegrowth

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/setup"
)

// Enabled is a product's connect toggle, hidden and on: connecting a switched
// off integration is not a thing anybody means.
func Enabled(kind, name string, on bool) setup.Requirement {
	r := setup.Requirement{Field: "enabled", Label: "Let agents reach " + name, Kind: setup.KindToggle,
		ConfigPath: "integrations." + kind + ".enabled", Required: true, Hidden: true, Default: "true"}
	r.Present, r.Resolved, r.Stored = setup.Toggle(on)
	return r
}

// Reader is the step adding the company's reader at the vendor, at role in
// where; hint is drawn after the role ("Viewer: enough to read ..."). Nothing
// where the Google Cloud connection names no identity to make it in.
func Reader(c *config.Company, role setup.IdentityRole, where setup.IdentityWhere, hint string) []setup.Requirement {
	email := ReaderEmail(c)
	if email == "" {
		return nil
	}
	return []setup.Requirement{{
		Field: "reader", Label: "Company reader", Kind: setup.KindAddIdentity, Hint: hint,
		Identity: &setup.IdentityStep{Email: email, Role: role, Where: where, Level: "company", State: "unknown"},
	}}
}

// ReaderEmail is the company's reader's email, "" where the Google Cloud
// connection names no provisioner to read its identity project from.
func ReaderEmail(c *config.Company) string {
	if c == nil || c.Integrations.GoogleCloud == nil || c.Integrations.GoogleCloud.Provisioning == nil {
		return ""
	}
	identity, ok := config.GoogleCloudServiceAccount(c.Integrations.GoogleCloud.Provisioning.Provisioner)
	if !ok {
		return ""
	}
	return googleauth.AccountEmail(googleauth.ReaderAccountID(c.Name), identity)
}
