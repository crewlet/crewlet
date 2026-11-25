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
// connection names no identity project.
func ReaderEmail(c *config.Company) string {
	if c == nil {
		return ""
	}
	identity, ok := c.Integrations.GoogleCloud.Identity()
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

// CloudSteps are the first steps of a product whose agents sign in as their
// own Google identities: Google Cloud connected, then the product turned on
// here, which has Crewlet turn on apis in the identity project and make the
// company reader. setupapi answers whether Google Cloud is connected
// ([setup.Step.Connected]).
func CloudSteps(name, apis string) []setup.Step {
	w, ui := vendortext.W, vendortext.UI
	return []setup.Step{
		{
			Title: "Connect Google Cloud first", Where: setup.Here, Needs: integration.KindGoogleCloud,
			Text: vendortext.Plain("Connect Google Cloud first; no project needs preparing for " + name + "."),
		},
		{
			Title: "Turn " + name + " on", Where: setup.Here,
			Text: vendortext.Of(w("Press "), ui("Connect"), w(" here. Crewlet turns on "+apis+" in the "+
				"identity project and makes the company reader.")),
		},
	}
}

// AgentsOn is the step turning agents on, where each chooses its scope from
// scope, said in the product's words.
func AgentsOn(name, scope string) setup.Step {
	w, ui := vendortext.W, vendortext.UI
	return setup.Step{
		Title: "Turn agents on", Where: setup.Here,
		Text: vendortext.Of(w("On the "), ui("Agents"), w(" tab, turn "), ui("On"), w(" each agent that should "+
			"use "+name+", choose its "), ui("Access"), w(" level and "), ui(scope), w(", then press "),
			ui("Review changes"), w(" and "), ui("Save and sync"), w(".")),
		Result: vendortext.Plain("The agent's row reads Getting ready for about a minute, then Needs you, and " +
			"shows its own account, which Crewlet made in the identity project."),
	}
}

// LateAccount is the trouble of adding an agent's account at the product
// right after Crewlet made it, which Google does not know yet.
func LateAccount(said string) setup.Trouble {
	return setup.Trouble{
		Sign: vendortext.Plain("It says `" + said + "`."),
		Fix:  vendortext.Plain("Google is still making the account. Wait a minute or two and add it again."),
	}
}

// ClientStep is the step making the OAuth client a consent product signs in
// as, in a project where api is turned on.
func ClientStep(api string) setup.Step {
	return setup.Step{Text: vendortext.Of(vendortext.W("In a Google Cloud project you control, turn on the "+
		api+" and make a "), vendortext.UI("Web application"), vendortext.W(" OAuth client at "),
		vendortext.UI("Google Cloud", "Credentials").At(credentialsURL),
		vendortext.W(" with the redirect URI and audience the form gives."))}
}

// AdminSignIn is the optional admin sign-in a product with an API for adding
// identities offers (Play, Analytics): the company's own OAuth client, and
// the sign-in of who, an admin there, so Crewlet adds each agent's identity
// itself. Nothing blocks without it: a person adds each identity by hand.
func AdminSignIn(kind, name, who string, client *config.GoogleOAuthClient,
	resolve func(string) (string, bool),
) []setup.Requirement {
	return OAuthClient(OAuthClientAsk{
		Kind: kind, Client: client,
		Purpose: vendortext.Plain("Lets " + who + " sign in so Crewlet adds each agent itself."),
		More: vendortext.Plain("Without it, you add each agent's identity in " + name +
			" yourself. Another Google product may share this client."),
		SignIn: vendortext.Plain("As " + who + "; Crewlet then adds each agent's identity for you."),
	}, resolve)
}

// OAuthClientAsk is one consent product's words around its OAuth client.
type OAuthClientAsk struct {
	// Kind is the integration's kind, whose block holds the client.
	Kind   string
	Client *config.GoogleOAuthClient
	// Required is a product reached only through the sign-in (AdMob,
	// AdSense); otherwise the client and the sign-in are optional.
	Required bool
	// Purpose is the hint under the client ID, More what follows it.
	Purpose, More vendortext.Text
	// SignIn is the hint under the sign-in: who signs in.
	SignIn vendortext.Text
}

// OAuthClient is the company's own OAuth client and its sign-in, asked the
// same way by every consent product (googleauth.Consent): the redirect URI to
// register, which setupapi fills from the engine ([setup.FillRedirects]), the
// client's ID and secret, and the sign-in.
func OAuthClient(ask OAuthClientAsk, resolve func(string) (string, bool)) []setup.Requirement {
	var held config.GoogleOAuthClient
	if ask.Client != nil {
		held = *ask.Client
	}
	var blocks integration.FindingKind
	if ask.Required {
		blocks = integration.FindingCredentialMissing
	}
	base := "integrations." + ask.Kind + ".oauth"
	redirect := setup.Requirement{Field: "redirect_uri", Label: "Authorized redirect URI", Kind: setup.KindRedirect,
		Required: ask.Required, Connect: ask.Required,
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
	id := setup.Requirement{Field: "client_id", Label: "OAuth client ID", Kind: setup.KindID,
		ConfigPath: base + ".client_id", Required: ask.Required, Connect: ask.Required,
		Format: "<project number>-<id>.apps.googleusercontent.com", Hint: ask.Purpose, More: ask.More,
		Blocks: blocks}
	id.Present, id.Resolved, id.Stored = setup.Held(held.ClientID, resolve)
	secret := setup.Requirement{Field: "client_secret", Label: "OAuth client secret", Kind: setup.KindSecret,
		ConfigPath: base + ".client_secret", Required: ask.Required, Connect: ask.Required,
		Hint: vendortext.Of(vendortext.W("The same client's secret, from "),
			vendortext.UI("Google Cloud", "Credentials").At(credentialsURL),
			vendortext.W("; other Google products may share it.")),
		CheckNote: "The client is proven by the Google sign-in, once saved.", Blocks: blocks}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(held.ClientSecret, resolve)
	label := "Admin sign-in"
	if ask.Required {
		label = "Sign in with Google"
	}
	// THE SIGN-IN IS THE ENGINE'S TO HOLD, never a value: whether it is held is
	// the pass's finding, so it blocks nothing here ([setup.Kind.AtVendor]).
	consent := setup.Requirement{Field: "google_sign_in", Label: label, Kind: setup.KindConsent,
		Required: ask.Required, Connect: ask.Required, Hint: ask.SignIn}
	return []setup.Requirement{redirect, id, secret, consent}
}
