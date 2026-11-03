package expo

import "github.com/crewlet/crewlet/internal/config"

// level is one seat level: Expo's robot role for it, its name in Expo's
// website and that website's description of it.
type level struct {
	id, name, note string
	role           Role
}

// levels is THE ONE TABLE mapping a seat level to the Expo role its robot
// holds, least access first. A robot is compared by its Role, never by the
// permissions a role happens to carry. Admin and Owner are in no level.
var levels = []level{
	{config.ExpoLevelViewer, "Viewer", "View all projects", RoleViewOnly},
	{config.ExpoLevelRunner, "Runner", "View all projects and perform low-risk actions on them", RoleRunner},
	{config.ExpoLevelDeveloper, "Developer",
		"Create builds, publish updates to unprotected channels and manage credentials", RoleDeveloper},
	{config.ExpoLevelReleaseManager, "Release Manager",
		"Everything a Developer does, and publish to protected channels", RoleReleaseManager},
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

// LevelName is a level as Expo's website names the role.
func LevelName(id string) string {
	if l, ok := find(id); ok {
		return l.name
	}
	return id
}

// LevelNote is Expo's own sentence for what a level's role allows.
func LevelNote(id string) string {
	l, _ := find(id)
	return l.note
}

// RoleOf is the Expo role a level's robot holds, and false for a level this
// build does not offer.
func RoleOf(id string) (Role, bool) {
	l, ok := find(id)
	return l.role, ok
}

// levelOf is the level whose role is role, or "" for a role no level is
// (Admin, Owner or a custom role).
func levelOf(role Role) string {
	for _, l := range levels {
		if l.role == role {
			return l.id
		}
	}
	return ""
}

// roleName is a role as Expo's website names it, for a sentence.
func roleName(role Role) string {
	if id := levelOf(role); id != "" {
		return LevelName(id)
	}
	switch role {
	case RoleAdmin:
		return "Admin"
	case RoleOwner:
		return "Owner"
	case RoleCustom:
		return "custom role"
	default:
		return string(role)
	}
}
