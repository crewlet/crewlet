package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The Web API calls a seat's access is applied through.
//
// # Slack has no roles, so access is on or off
//
// Each agent is its own app, holding the bot scopes its manifest lists
// ([BotScopes]); what it reaches is decided per channel, by who invites it.
// So nothing per seat is set at Slack besides whether the app's bot token
// works. Off revokes that token ([Client.Revoke]); back on is a fresh install,
// which only a person can approve. The scopes are observed every pass
// ([Admin.ExportManifest]) so a change made by hand in Slack's console is
// restored from the manifest this engine owns (D2).

// Revoke withdraws this client's bot token, which is the seat's off switch
// (D1): the app and its install record stay, and every call the token makes
// from now on is refused.
//
// The answer says whether Slack revoked it NOW. A token Slack already refuses
// (revoked before, or the app uninstalled by hand) is the outcome asked for,
// so it answers false with no error rather than failing a step that has
// nothing left to do; the caller still clears the sealed value.
func (c *Client) Revoke(ctx context.Context) (bool, error) {
	var out struct {
		Revoked bool `json:"revoked"`
	}
	err := call(ctx, c.http, "auth.revoke", c.token, map[string]any{}, &out)
	var api *APIError
	switch {
	case err == nil:
		return out.Revoked, nil
	case errors.As(err, &api) && tokenDead(api.Code):
		return false, nil
	default:
		return false, err
	}
}

// tokenDead reports the codes Slack answers for a token that no longer
// authenticates as anything. They are what a revoked, uninstalled or
// deactivated bot's token draws, and none of them is fixed by trying again.
func tokenDead(code string) bool {
	switch code {
	case "invalid_auth", "token_revoked", "account_inactive", "not_authed", "token_expired":
		return true
	}
	return false
}

// TokenDead reports an error that says the bot token no longer works, which
// a pass reads as "this seat's app is off or uninstalled at Slack" rather
// than as a fault to retry.
func TokenDead(err error) bool {
	var api *APIError
	return errors.As(err, &api) && tokenDead(api.Code)
}

// ExportManifest reads an app's manifest as Slack holds it now.
//
// THE OBSERVATION HALF OF DRIFT. A person can add or drop a scope in Slack's
// console, and the app then asks for something the manifest this engine
// built does not; reading it back is the only way to know. Authenticated
// with the app-configuration token, like every manifest method.
func (a *Admin) ExportManifest(ctx context.Context, configToken, appID string) (map[string]any, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, fmt.Errorf("slack: apps.manifest.export: no app id")
	}
	var out struct {
		Manifest map[string]any `json:"manifest"`
	}
	if err := a.retrying(ctx, "apps.manifest.export", configToken,
		map[string]any{"app_id": appID}, &out); err != nil {
		return nil, err
	}
	if out.Manifest == nil {
		// AN EMPTY ANSWER IS AN ERROR, for the reason [Client.AppOf]
		// gives: read as success it would report every scope missing
		// and push a manifest over an app nobody could read.
		return nil, fmt.Errorf("slack: apps.manifest.export: %s answered no manifest", appID)
	}
	return out.Manifest, nil
}

// DeleteApp deletes an app this engine created from a manifest.
//
// THE APP IS GONE AT SLACK AFTERWARDS, with its bot user and its install, so
// this is only ever called for a seat that is off and whose removal a person
// confirmed (design 5.3). An app Slack no longer has is the outcome asked for.
func (a *Admin) DeleteApp(ctx context.Context, configToken, appID string) error {
	if strings.TrimSpace(appID) == "" {
		return fmt.Errorf("slack: apps.manifest.delete: no app id")
	}
	err := a.retrying(ctx, "apps.manifest.delete", configToken,
		map[string]any{"app_id": appID}, nil)
	if appGone(err) {
		return nil
	}
	return err
}

// AppGone reports a Slack error meaning the app is not there any more.
func AppGone(err error) bool { return appGone(err) }

// ManifestScopes is the bot scopes a manifest asks for, sorted and without
// repeats. A manifest with none answers nil.
func ManifestScopes(manifest map[string]any) []string {
	oauth, _ := manifest["oauth_config"].(map[string]any)
	scopes, _ := oauth["scopes"].(map[string]any)
	var out []string
	switch bot := scopes["bot"].(type) {
	case []any:
		for _, scope := range bot {
			if name, ok := scope.(string); ok && strings.TrimSpace(name) != "" {
				out = append(out, strings.TrimSpace(name))
			}
		}
	case []string:
		for _, name := range bot {
			if strings.TrimSpace(name) != "" {
				out = append(out, strings.TrimSpace(name))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ScopeDrift compares the scopes an app holds at Slack with [BotScopes]:
// missing is what this engine's manifest asks for and the app lacks, extra is
// what the app holds and the manifest does not ask for. Both sorted.
func ScopeDrift(held []string) (missing, extra []string) {
	return scopeDiff(held, slices.Sorted(slices.Values(BotScopes)))
}

// appGone reports a Slack error meaning the recorded app is not there any
// more — deleted in the console, or moved to a workspace this credential
// cannot see.
//
// # Why it is a set of codes rather than a status
//
// Every Web API answer is a 200 carrying `"ok": false`, so the only signal is
// the code string. These three are what the app-management methods answer for
// an id that does not resolve, and they are not interchangeable with a
// permission refusal: an app this operator may not touch still EXISTS, and
// replacing it would leave two.
func appGone(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.Code {
	case "app_not_found", "invalid_app_id", "invalid_app":
		return true
	}
	return false
}
