package slack

import (
	"fmt"
	"slices"
	"strings"
)

// ManifestDiff is how an app's manifest at Slack differs from the one this
// engine builds for it, in the manifest's own names.
type ManifestDiff struct {
	// MissingScopes and ExtraScopes are the bot scopes the app lacks and
	// the ones it holds that the manifest does not ask for.
	MissingScopes []string
	ExtraScopes   []string

	// Settings names every other field that differs: the event
	// subscriptions, the request URL, the redirect URLs, and the three
	// switches the engine depends on.
	Settings []string
}

// Empty reports an app whose manifest matches this engine's.
func (d ManifestDiff) Empty() bool {
	return len(d.MissingScopes) == 0 && len(d.ExtraScopes) == 0 && len(d.Settings) == 0
}

// NeedsReinstall reports a restore whose added scopes a bot token carries
// only after a person installs the app again.
func (d ManifestDiff) NeedsReinstall() bool { return len(d.MissingScopes) > 0 }

// Describe is the difference as a sentence for a drift event.
func (d ManifestDiff) Describe() string {
	var parts []string
	if len(d.MissingScopes) > 0 {
		parts = append(parts, missingScopesSaid+strings.Join(d.MissingScopes, ", "))
	}
	if len(d.ExtraScopes) > 0 {
		parts = append(parts, "scopes added at Slack: "+strings.Join(d.ExtraScopes, ", "))
	}
	if len(d.Settings) > 0 {
		parts = append(parts, "changed at Slack: "+strings.Join(d.Settings, ", "))
	}
	return strings.Join(parts, "; ")
}

// missingScopesSaid opens the part of [ManifestDiff.Describe] that names
// scopes removed at Slack, which is the one difference a restore cannot
// finish alone: a bot token carries a scope added back only after a person
// installs the app again ([driftNeedsReinstall]).
const missingScopesSaid = "scopes removed at Slack: "

// driftNeedsReinstall reports a drift sentence whose restore needs a
// reinstall, read back from what [ManifestDiff.Describe] wrote.
func driftNeedsReinstall(said string) bool { return strings.Contains(said, missingScopesSaid) }

// DiffManifest compares the manifest Slack holds with the one wanted.
//
// ONLY WHAT THE ENGINE DEPENDS ON. Slack's export fills in defaults the
// engine never sets (an app's colour, its long description), so a whole-
// document comparison would report drift on every app. What is compared is
// what changes behaviour: the scopes, the events and where they go, the
// install redirect, and the switches [Manifest] explains.
func DiffManifest(held, want map[string]any) ManifestDiff {
	var diff ManifestDiff
	diff.MissingScopes, diff.ExtraScopes = scopeDiff(ManifestScopes(held), ManifestScopes(want))
	for _, field := range manifestFields {
		if !slices.Equal(field.read(held), field.read(want)) {
			diff.Settings = append(diff.Settings, field.name)
		}
	}
	return diff
}

// scopeDiff is what want has that held lacks, and what held has that want
// does not. Both sorted.
func scopeDiff(held, want []string) (missing, extra []string) {
	for _, scope := range want {
		if !slices.Contains(held, scope) {
			missing = append(missing, scope)
		}
	}
	for _, scope := range held {
		if !slices.Contains(want, scope) {
			extra = append(extra, scope)
		}
	}
	return missing, extra
}

// manifestField is one compared field: its name in Slack's manifest, and how
// to read it as a sorted list of strings.
type manifestField struct {
	name string
	path []string

	// absent is what the field means when the manifest leaves it out:
	// Slack's export omits a switch at its default, which must not read
	// as drift from a manifest that writes the default out.
	absent string
}

func (f manifestField) read(manifest map[string]any) []string {
	var node any = manifest
	for _, key := range f.path {
		object, ok := node.(map[string]any)
		if !ok {
			node = nil
			break
		}
		node = object[key]
	}
	var out []string
	switch value := node.(type) {
	case nil:
		if f.absent != "" {
			return []string{f.absent}
		}
		return nil
	case []any:
		for _, item := range value {
			out = append(out, fmt.Sprint(item))
		}
	case []string:
		out = slices.Clone(value)
	default:
		out = []string{fmt.Sprint(value)}
	}
	slices.Sort(out)
	return out
}

// manifestFields are the fields [DiffManifest] compares besides the scopes.
var manifestFields = []manifestField{
	{name: "bot_events", path: []string{"settings", "event_subscriptions", "bot_events"}},
	{name: "request_url", path: []string{"settings", "event_subscriptions", "request_url"}},
	{name: "redirect_urls", path: []string{"oauth_config", "redirect_urls"}},
	{name: "socket_mode_enabled", path: []string{"settings", "socket_mode_enabled"}, absent: "false"},
	{name: "token_rotation_enabled", path: []string{"settings", "token_rotation_enabled"}, absent: "false"},
	{name: "org_deploy_enabled", path: []string{"settings", "org_deploy_enabled"}, absent: "false"},
}
