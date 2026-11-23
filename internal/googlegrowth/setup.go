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

// credentialsURL is where a person makes the company's OAuth client.
const credentialsURL = "https://console.cloud.google.com/apis/credentials"

// AdminSignIn is the optional admin sign-in a product with an API for adding
// identities offers (Play, Analytics): the company's own OAuth client, and
// the sign-in of who, an admin there, so Crewlet adds each agent's identity
// itself. Nothing blocks without it: a person adds each identity by hand.
func AdminSignIn(kind, name, who string, client *config.GoogleOAuthClient,
	resolve func(string) (string, bool)) []setup.Requirement {
	var held config.GoogleOAuthClient
	if client != nil {
		held = *client
	}
	base := "integrations." + kind + ".oauth"
	id := setup.Requirement{Field: "client_id", Label: "OAuth client ID (optional)", Kind: setup.KindID,
		ConfigPath: base + ".client_id", Format: "<project number>-<id>.apps.googleusercontent.com",
		Hint: "A Web application client from {link}, in a project you control.",
		More: "With it, " + who + " signs in once and Crewlet adds each agent's identity in " + name +
			" itself; without it you add each one by hand. Add Crewlet's callback address to its authorized " +
			"redirect URIs and publish its consent screen: a client left in Testing loses its sign-in after " +
			"seven days.",
		LinkText: "Google Cloud, Credentials", VendorURL: credentialsURL}
	id.Present, id.Resolved, id.Stored = setup.Held(held.ClientID, resolve)
	secret := setup.Requirement{Field: "client_secret", Label: "OAuth client secret (optional)",
		Kind: setup.KindSecret, ConfigPath: base + ".client_secret",
		Hint:      "The same client's secret, from {link}; another Google product may share it.",
		CheckNote: "The client is proven by the sign-in, once saved.",
		LinkText:  "Google Cloud, Credentials", VendorURL: credentialsURL}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(held.ClientSecret, resolve)
	consent := setup.Requirement{Field: "google_sign_in", Label: "Admin sign-in (optional)",
		Kind: setup.KindConsent, Hint: "As " + who + "; Crewlet then adds each agent's identity for you."}
	return []setup.Requirement{id, secret, consent}
}
