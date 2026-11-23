package expo

import (
	"context"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// AtlasField is the form's field for the atlas's token.
const AtlasField = "atlas_token"

// Requirements says what this company still needs for Expo: the account name,
// and optionally a view-only robot's token for the atlas. A nil block answers
// every field absent, so the form still renders.
func Requirements(in *config.Expo, resolve func(string) (string, bool)) []setup.Requirement {
	var account, atlasToken string
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			account = in.Provisioning.AccountName
		}
		if in.Atlas != nil {
			atlasToken = in.Atlas.Token
		}
	}
	reqs := []setup.Requirement{
		{
			Field:      "account_name",
			Label:      "Account name",
			Kind:       setup.KindID,
			ConfigPath: "integrations.expo.provisioning.account_name",
			Required:   true,
			Connect:    true,
			Format:     "the name in expo.dev/accounts/<name>",
			Hint: vendortext.Of(vendortext.W("The account or organization your projects belong to, on your "),
				vendortext.W("Expo dashboard").At("https://expo.dev/"), vendortext.W(".")),
			More: vendortext.Plain("As named in `expo.dev/accounts/<name>`. Each agent gets its own robot there when you " +
				"turn it on."),
			Blocks: integration.FindingCredentialMissing,
			// Nothing to test here: the first agent's token proves the name.
			CheckNote: "The first agent's robot token proves the account when it is pasted.",
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field:      "enabled",
			Label:      "Give agents robots",
			Kind:       setup.KindToggle,
			ConfigPath: "integrations.expo.enabled",
			Required:   true,
			Hidden:     true,
			Default:    "true",
		},
		{
			// OPTIONAL, AND NEVER AN AGENT'S: without it every agent works
			// as before and the atlas shows the account alone.
			Field:      AtlasField,
			Label:      "Atlas robot token",
			Kind:       setup.KindSecret,
			ConfigPath: "integrations.expo.atlas.token",
			Checkable:  true,
			CheckWith:  []string{"account_name"},
			Hint: vendortext.Of(vendortext.W("See more in Atlas: a "), vendortext.UI("Viewer"),
				vendortext.W(" robot's token from the "), vendortext.W("access tokens page").At(atlasTokensURL),
				vendortext.W(".")),
			More: vendortext.Of(vendortext.W("Optional. On the account above, add a robot named `"+AtlasRobotName+"` with the "),
				vendortext.UI("Viewer"), vendortext.W(" role, for the atlas alone, and press "),
				vendortext.UI("Create token"), vendortext.W(" on its row. The atlas then shows the "+
					"account's projects and the latest build of each on Android and iOS, read again every "+
					"hour, and reads no build credential, secret or environment value. A robot with any other "+
					"role is refused. To undo it, delete the robot at Expo and clear this field.")),
		},
	}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(account, resolve)
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Toggle(enabled)
	reqs[2].Present, reqs[2].Resolved, reqs[2].Stored = setup.Held(atlasToken, resolve)
	return reqs
}

// atlasTokensURL is the account's access tokens page; the form fills in the
// account's name from its field.
const atlasTokensURL = "https://expo.dev/accounts/{account_name}/settings/access-tokens"

// Guide is what the connect form says around its one field: the account to
// name, then each agent's robot, made at Expo when the agent is turned on.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Each agent acts at Expo as its own robot user, so its builds and updates are " +
			"attributable to it.",
		Prepare: []setup.Step{
			{
				Text: vendortext.Of(vendortext.W("Find the account or organization name on your "),
					vendortext.W("Expo dashboard").At("https://expo.dev/"), vendortext.W(".")),
				Yields: "account_name",
			},
			{Text: vendortext.Plain("Connect, then turn agents on from the Agents tab.")},
			{Text: vendortext.Plain("Create each agent's robot at Expo when asked. The first robot's token proves the account.")},
			{
				Text: vendortext.Of(vendortext.W("Optional, to see more in Atlas: on the "),
					vendortext.W("access tokens page").At(atlasTokensURL),
					vendortext.W(", add one more robot named `"+AtlasRobotName+"`, for the atlas alone, with the role:")),
				Grants: []string{LevelName(config.ExpoLevelViewer)}, Yields: AtlasField,
			},
		},
	}
}

// Probe answers the connect form's Test button for the atlas's token, the one
// credential the form holds: it is checked as [CheckAtlas] checks it before
// every read. Each agent's robot token is checked when it is pasted for its
// seat.
//
// httpClient is nil in production; a test hands one that reaches a fake.
func Probe(httpClient *http.Client) setup.Probe {
	return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
		if check.Field != AtlasField {
			return setup.Verdict{}, setup.NotCheckable("Expo", check.Field)
		}
		token := strings.TrimSpace(check.Value(AtlasField))
		account := strings.TrimSpace(check.Value("account_name"))
		switch {
		case token == "":
			return setup.Verdict{}, setup.Unusable("Paste the atlas robot's token to test it.")
		case account == "":
			return setup.Verdict{}, setup.Unusable("Enter the account name: the robot is checked against it.")
		}
		client, err := NewClient(ClientOptions{Token: token, HTTP: httpClient})
		if err != nil {
			return setup.Verdict{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, setup.VerifyBudget)
		defer cancel()
		robot, err := CheckAtlas(ctx, client, account)
		if refusal, ok := IsRefusal(err); ok {
			return setup.Refused(refusal.Detail, setup.Warning{Action: refusal.Hint}), nil
		}
		if err != nil {
			return setup.Judge(setup.Failure{App: "Expo", Status: Status(err), Err: err}, token), nil
		}
		return setup.Works("Works. The robot views the account and changes nothing.",
			setup.Fact{Label: "Robot", Value: nameOf(robot)},
			setup.Fact{Label: "Role", Value: roleName(RoleViewOnly)},
			setup.Fact{Label: "Account", Value: account}), nil
	}
}
