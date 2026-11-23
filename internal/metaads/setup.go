package metaads

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// businessSettings is where a system user is made and its tokens generated.
const businessSettings = "https://business.facebook.com/settings/system-users"

// secretNames are the sealed names of the block's secrets, by field.
var secretNames = map[string]string{"app_secret": "META_ADS_APP_SECRET", "read_token": "META_ADS_READ_TOKEN",
	"operate_token": "META_ADS_OPERATE_TOKEN", "admin_token": "META_ADS_ADMIN_TOKEN",
	"admin_system_user_token": "META_ADS_ADMIN_SYSTEM_USER_TOKEN"}

// Requirements says what this company still needs for Meta Ads: its app's ID
// and secret, the system user's read token, and optionally its operate and
// admin tokens, the admin system user's token that makes each agent's own
// system user, and the business portfolio. A nil block answers every field
// absent, so the form still renders.
func Requirements(in *config.MetaAds, resolve func(string) (string, bool)) []setup.Requirement {
	var b config.MetaAds
	if in != nil {
		b = *in
	}
	base := "integrations." + Kind
	toggle := setup.Requirement{Field: "enabled", Label: "Connect Meta Ads Manager", Kind: setup.KindToggle,
		ConfigPath: base + ".enabled", Required: true, Hidden: true, Default: "true"}
	toggle.Present, toggle.Resolved, toggle.Stored = setup.Toggle(in.GrowthEnabled())
	app := setup.Requirement{Field: "app_id", Label: "App ID", Kind: setup.KindID, ConfigPath: base + ".app_id",
		Required: true, Connect: true, Format: "digits",
		Hint:     "Your Business app's ID, with Marketing API added, on {link}.",
		More:     "Turn on Require App Secret in the app's Advanced settings, so a token is useless without the app secret.",
		LinkText: "Meta for Developers", VendorURL: "https://developers.facebook.com/apps",
		Blocks: integration.FindingCredentialMissing}
	app.Present, app.Resolved, app.Stored = setup.Held(b.AppID, resolve)
	secret := setup.Requirement{Field: "app_secret", Label: "App secret", Kind: setup.KindSecret,
		ConfigPath: base + ".app_secret", SecretName: secretNames["app_secret"], Required: true, Shape: setup.ShapeOpaqueToken,
		Hint:      "On the app's Basic settings, shown once you confirm your password.",
		CheckNote: "The secret is proven with Meta on the next pass, once saved."}
	secret.Present, secret.Resolved, secret.Stored = setup.Held(b.AppSecret, resolve)
	token := func(field, label, hint, more string, required bool) setup.Requirement {
		r := setup.Requirement{Field: field, Label: label, Kind: setup.KindSecret, ConfigPath: base + "." + field,
			SecretName: secretNames[field], Required: required, Shape: setup.ShapeMetaToken, Hint: hint, More: more,
			LinkText: "Business Settings, System users", VendorURL: businessSettings,
			CheckNote: "Each token is proven with Meta on the next pass, once saved."}
		return r
	}
	read := token("read_token", "Read token", "An Employee system user's token with ads_read alone, from {link}.",
		"Generate it from your app, with no permission that writes.", true)
	read.Present, read.Resolved, read.Stored = setup.Held(b.ReadToken, resolve)
	operate := token("operate_token", "Operate token", "Optional: its token with ads_management, for changes, from {link}.",
		"Add pages_read_engagement and pages_show_list; without this token agents only read.", false)
	operate.Present, operate.Resolved, operate.Stored = setup.Held(b.OperateToken, resolve)
	admin := token("admin_token", "Admin token", "Optional: its token with business_management, for allowed account changes, from {link}.",
		"Used only for an account change an administrator allows, never for a read.", false)
	admin.Present, admin.Resolved, admin.Stored = setup.Held(b.AdminToken, resolve)
	maker := token("admin_system_user_token", "Admin system user token",
		"Optional: your Admin system user's token with business_management, from {link}.",
		"Crewlet uses it only to make each agent's own system user and its token, never for an agent's call. "+
			"Needs the business portfolio ID.", false)
	maker.Present, maker.Resolved, maker.Stored = setup.Held(b.AdminSystemUserToken, resolve)
	business := setup.Requirement{Field: "business_id", Label: "Business portfolio ID", Kind: setup.KindID,
		ConfigPath: base + ".business_id", Format: "digits", Hint: "Optional: your business portfolio's ID. Needed for the admin system user token."}
	business.Present, business.Resolved, business.Stored = setup.Held(b.BusinessID, resolve)
	return []setup.Requirement{toggle, app, secret, read, operate, admin, maker, business}
}

// Guide is what the connect form says around its fields.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet keeps one system user's tokens from your own Meta app and makes every call for the " +
			"agents, within each agent's ad accounts. No agent holds a token.",
	}
}
