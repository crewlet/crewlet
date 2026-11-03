package expo

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// level is one seat level: its name and note in Expo's own role terms, and
// the Expo permissions its robot holds on the account.
type level struct {
	id, name, note string
	perms          []Permission
}

// levels is THE ONE TABLE mapping a seat level to Expo permissions, least
// access first. Expo does not document each role's permissions, so the sets are
// read from its role names for a live account to confirm. Never Admin or Own.
var levels = []level{
	{config.ExpoLevelViewer, "Viewer",
		"Views the account's projects through Expo Go, and changes nothing",
		[]Permission{PermissionView}},
	{config.ExpoLevelRunner, "Runner",
		"Views the account's projects and performs low-risk actions on them",
		[]Permission{PermissionView, PermissionRun}},
	{config.ExpoLevelDeveloper, "Developer",
		"Creates projects, makes builds, releases updates and manages credentials",
		[]Permission{PermissionView, PermissionRun, PermissionPublish}},
	{config.ExpoLevelReleaseManager, "Release Manager",
		"Everything a Developer does, and manages protected resources such as protected update channels",
		[]Permission{PermissionView, PermissionRun, PermissionPublish, PermissionPublishProtected}},
}

// Levels is every seat level, least access first.
func Levels() []string { return config.ExpoLevels() }

// find is the table's row for id.
func find(id string) (level, bool) {
	for _, l := range levels {
		if l.id == id {
			return l, true
		}
	}
	return level{}, false
}

// LevelName is a level as a person reads it, in Expo's role terms.
func LevelName(id string) string {
	if l, ok := find(id); ok {
		return l.name
	}
	return id
}

// LevelNote is the sentence saying what a level allows.
func LevelNote(id string) string {
	l, _ := find(id)
	return l.note
}

// Permissions is the Expo permissions a level grants, sorted, and false for a
// level this build does not offer.
func Permissions(id string) ([]Permission, bool) {
	l, ok := find(id)
	if !ok {
		return nil, false
	}
	return permissionSet(l.perms), true
}

// levelOf is the level whose permissions are exactly held, or "" where none
// is: a robot somebody changed at Expo, which the sync puts back.
func levelOf(held []Permission) string {
	have := permissionSet(held)
	for _, l := range levels {
		if slices.Equal(have, permissionSet(l.perms)) {
			return l.id
		}
	}
	return ""
}

// difference is what want holds that have does not, sorted.
func difference(want, have []Permission) []Permission {
	var out []Permission
	for _, p := range permissionSet(want) {
		if !slices.Contains(have, p) {
			out = append(out, p)
		}
	}
	return out
}
