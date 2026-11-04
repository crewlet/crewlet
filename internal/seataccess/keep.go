package seataccess

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// ErrNoScalewayLevel refuses keeping a Scaleway policy that is no level's: a
// seat names a level, and none of them grants what the policy grants.
var ErrNoScalewayLevel = errors.New("the agent's policy grants what no Scaleway level grants: " +
	"save the level it should hold here, and Crewlet sets its policy to that")

// ErrNoCloudflareLevel refuses keeping a Cloudflare token whose policy is no
// level's: a seat names a level, and none of them holds what it holds.
var ErrNoCloudflareLevel = errors.New("the token's permissions are not those of any Cloudflare level: " +
	"save the level it should hold here, and Crewlet narrows it to that")

// ErrNoGoogleCloudLevel refuses keeping a service account's roles that are no
// level's: different on two projects, or roles no level grants.
var ErrNoGoogleCloudLevel = errors.New("the agent's roles are not one Google Cloud level on every project: " +
	"save the level it should hold here, and Crewlet sets its roles to that")

// ErrNoExpoLevel refuses keeping an Expo robot's permissions when they are no
// level's: a seat names a level, and none of them holds what it holds.
var ErrNoExpoLevel = errors.New("the robot's Expo permissions match no level: " +
	"save the level it should hold here, and Crewlet moves it to that")

// ErrNoDigitalOceanLevel refuses keeping a DigitalOcean token's scopes when
// they are no level's: DigitalOcean cannot change a token's scopes, so the seat
// needs a token made with a level's scopes.
var ErrNoDigitalOceanLevel = errors.New("the token's DigitalOcean scopes match no level: " +
	"choose the level it should hold here, and paste a token made with that level's scopes")

// ErrNoAWSLevel refuses keeping an IAM role's policies when they are no
// level's: a seat names a level, and none of them is what it holds.
var ErrNoAWSLevel = errors.New("the role's AWS policies match no level: " +
	"save the level it should hold here, and Crewlet attaches that level's policies")

// ErrSeveralRoles refuses keeping a Datadog account's roles when it holds more
// than one: a seat names one role, and keeping one of several would take the
// others away.
var ErrSeveralRoles = errors.New("the account holds several Datadog roles, and an agent " +
	"holds one: leave it one role at Datadog, or save the role it should hold here")

// KeptAccess is one seat's lane as the vendor holds it, in the document's
// terms: what "Keep the vendor's setting" (after drift, D2) and "Keep off"
// (after the account was turned off at the vendor, D3) write, so the next sync
// finds nothing to restore. want is the lane as the document holds it now, and
// have what the vendor shows.
//
// EACH VENDOR'S SETTING IN ITS OWN TERMS:
//   - an account turned off is kept off, and keeps the access the document
//     names, so turning it back on restores that rather than nothing;
//   - Slack holds no level, permissions or scope here (its scopes are its
//     app's), so only on or off is kept;
//   - GitHub keeps the app's permissions, every other vendor its level;
//   - a lane that narrows by scope keeps the vendor's scope.
func KeptAccess(lane Lane, want Desired, have Observed) (Desired, error) {
	kept := want.Normalized()
	kept.Enabled = have.Exists && have.Enabled
	if !kept.Enabled || lane == LaneSlack {
		return kept, nil
	}
	switch lane {
	case LaneGitHub:
		kept.Permissions = maps.Clone(have.Permissions)
	case LaneDatadog:
		if len(strings.Fields(have.Level)) > 1 {
			return Desired{}, ErrSeveralRoles
		}
		kept.Level = have.Level
	case LaneCloudflare:
		if !slices.Contains(config.CloudflareLevels(), have.Level) {
			return Desired{}, ErrNoCloudflareLevel
		}
		kept.Level = have.Level
	case LaneExpo:
		if !slices.Contains(config.ExpoLevels(), have.Level) {
			return Desired{}, ErrNoExpoLevel
		}
		kept.Level = have.Level
	case LaneDigitalOcean:
		if !slices.Contains(config.DigitalOceanLevels(), have.Level) {
			return Desired{}, ErrNoDigitalOceanLevel
		}
		kept.Level = have.Level
	case LaneScaleway:
		if !slices.Contains(config.ScalewayLevels(), have.Level) {
			return Desired{}, ErrNoScalewayLevel
		}
		kept.Level = have.Level
	case LaneAWS:
		if !slices.Contains(config.AWSLevels(), have.Level) {
			return Desired{}, ErrNoAWSLevel
		}
		kept.Level = have.Level
	case LaneGoogleCloud:
		if !slices.Contains(config.GoogleCloudLevels(), have.Level) {
			return Desired{}, ErrNoGoogleCloudLevel
		}
		kept.Level = have.Level
	default:
		kept.Level = have.Level
	}
	if scoped(lane) {
		kept.Scope = have.Scope
	}
	return kept.Normalized(), nil
}

// scoped reports a lane that narrows by scope, by its vendor's catalog; a lane
// whose vendor this build does not register is taken as one, since the
// vendor's scope is what it shows.
func scoped(lane Lane) bool {
	vendor, ok := Lookup(lane.Kind())
	if !ok {
		return true
	}
	spec, ok := vendor.Catalog().Lane(lane)
	return !ok || spec.Scoped
}
