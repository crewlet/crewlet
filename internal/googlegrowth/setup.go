package googlegrowth

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
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
func Reader(c *config.Company, role setup.IdentityRole, where, hint vendortext.Text) []setup.Requirement {
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

// The Google pages a person makes and sets up the company's OAuth client on.
const (
	credentialsURL = "https://console.cloud.google.com/apis/credentials"
	audienceURL    = "https://console.cloud.google.com/auth/audience"
	audienceDocURL = "https://support.google.com/cloud/answer/15549945"
)

// CloudFirst is the first step of a product whose agents sign in as their own
// Google identities: connecting Google Cloud, whose Cloud Shell block, run
// again, turns on apis and makes the company reader. setupapi answers whether
// Google Cloud is connected ([setup.Step.Connected]).
func CloudFirst(apis string) setup.Step {
	return setup.Step{Needs: integration.KindGoogleCloud, Text: vendortext.Plain("Connect Google Cloud " +
		"first; no project needs preparing. Its Cloud Shell block, run again, turns on " + apis +
		" and makes the company reader.")}
}

// ClientStep is the step turning on api where a consent product's sign-in
// client lives; the client itself is made once, on the Google page.
func ClientStep(api string) setup.Step {
	return setup.Step{Text: vendortext.Of(vendortext.W("Turn on the "+api+" in the Google Cloud project of your "+
		"Google sign-in client, which is set once on the Google page's Settings tab ("),
		vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(" shows its project)."))}
}

// AdminSignIn is the optional admin sign-in a product with an API for adding
// identities offers (Play, Analytics): the sign-in of who, an admin there, so
// Crewlet adds each agent's identity itself. Nothing blocks without it: a
// person adds each identity by hand.
func AdminSignIn(kind, who string) []setup.Requirement {
	return SignIn(SignInAsk{Kind: kind,
		SignIn: vendortext.Plain("As " + who + "; Crewlet then adds each agent's identity for you.")})
}

// SignInAsk is one consent product's words around its Google sign-in.
type SignInAsk struct {
	// Kind is the integration's kind, whose grant the sign-in makes.
	Kind string
	// Required is a product reached only through the sign-in (AdMob,
	// AdSense); otherwise the sign-in is optional.
	Required bool
	// SignIn is the hint under the sign-in: who signs in.
	SignIn vendortext.Text
}

// SignIn is a consent product's Google sign-in, made as the company's sign-in
// client ([SignInClient]). THE SIGN-IN IS THE ENGINE'S TO HOLD, never a value:
// whether it is held is the pass's finding, so it blocks nothing here
// ([setup.Kind.AtVendor]).
func SignIn(ask SignInAsk) []setup.Requirement {
	label := "Admin sign-in"
	if ask.Required {
		label = "Sign in with Google"
	}
	return []setup.Requirement{{Field: "google_sign_in", Label: label, Kind: setup.KindConsent,
		Required: ask.Required, Connect: ask.Required, Hint: ask.SignIn}}
}

// SignInClient is the company's one OAuth client every Google sign-in is made
// as, asked on the Google page apart from the connection ([setup.GroupSignIn]):
// the redirect URI to register, which setupapi fills from the engine
// ([setup.FillRedirects]), and the client's ID and secret.
func SignInClient(gc *config.GoogleCloud, resolve func(string) (string, bool)) []setup.Requirement {
	var held config.GoogleOAuthClient
	if gc != nil && gc.SignInClient != nil {
		held = *gc.SignInClient
	}
	const base = "integrations.google_cloud.sign_in_client"
	redirect := setup.Requirement{Field: "redirect_uri", Label: "Authorized redirect URI", Kind: setup.KindRedirect,
		Group: setup.GroupSignIn,
		Hint: vendortext.Of(vendortext.W("Make a "), vendortext.UI("Web application"), vendortext.W(" client at "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(" with this redirect URI.")),
		More: vendortext.Of(vendortext.W("Leave "), vendortext.UI("Authorized JavaScript origins"),
			vendortext.W(" empty. In "), vendortext.UI("Google Auth Platform", "Audience").At(audienceURL),
			vendortext.W(", choose "), vendortext.UI("Internal"),
			vendortext.W(" if whoever signs in is in the project's organization. Otherwise choose "),
			vendortext.UI("External"), vendortext.W(" and press "), vendortext.UI("Publish app"),
			vendortext.W(", or add them as a test user: Google ends a "), vendortext.UI("Testing"),
			vendortext.W(" app's sign-in after 7 days ("), vendortext.W("Google's guide").At(audienceDocURL),
			vendortext.W(")."))}
	id := setup.Requirement{Field: "sign_in_client_id", Label: "OAuth client ID", Kind: setup.KindID,
		ConfigPath: base + ".client_id", Group: setup.GroupSignIn,
		Format: "<project number>-<id>.apps.googleusercontent.com",
		Hint: vendortext.Of(vendortext.W("The client every Google sign-in is made as, from "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(".")),
		More: vendortext.Plain("AdMob and AdSense sign in as it, and Google Analytics and Google Play use it " +
			"to add agents for you. A product's own oauth block overrides it for that product.")}
	id.Present, id.Resolved, id.Stored = setup.Held(held.ClientID, resolve)
	secret := setup.Requirement{Field: "sign_in_client_secret", Label: "OAuth client secret", Kind: setup.KindSecret,
		ConfigPath: base + ".client_secret", Group: setup.GroupSignIn,
		Hint: vendortext.Of(vendortext.W("The same client's secret, from "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL), vendortext.W(".")),
		CheckNote: "The client is proven by the Google sign-in, once saved."}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(held.ClientSecret, resolve)
	return []setup.Requirement{redirect, id, secret}
}
