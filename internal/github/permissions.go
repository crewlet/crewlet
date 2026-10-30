package github

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// What one agent's GitHub App may do, in GitHub's own terms.
//
// # No Crewlet words
//
// GitHub has no roles for an app. It has PERMISSIONS, each held at No access,
// Read-only or Read and write, and an installation that covers some or all of
// an organization's repositories. So that is what a seat stores and what the
// dashboard shows: the permission keys GitHub's API takes, at the levels its
// API spells, under the names its settings page uses. The three tiers this
// package used to offer ([Tier]) survive only as the source the migration
// reads ([TierPermissions]).
//
// # A closed catalogue
//
// A seat may hold only the permissions in [SeatPermissions]. Every other
// GitHub App permission is either one an agent has no use for or one that
// lets it escape its own limits ([Denied]), and a catalogue that grew by
// accident would be the second kind. Metadata is not in it because GitHub
// grants it to every app and every token needs it ([Permissions.Token]).

// Level is how much of one permission an app holds, as GitHub's API spells
// it. The zero value is No access. It is the company document's own type, so
// the document, the wire and this package cannot spell a grant two ways.
type Level = config.GitHubAccess

const (
	// LevelNone is No access: the key is absent from what GitHub is sent.
	LevelNone Level = ""

	// LevelRead is Read-only.
	LevelRead = config.GitHubRead

	// LevelWrite is Read and write.
	LevelWrite = config.GitHubWrite
)

// LevelLabel is a level as GitHub's settings page names it.
func LevelLabel(l Level) string {
	switch l {
	case LevelRead:
		return "Read-only"
	case LevelWrite:
		return "Read and write"
	default:
		return "No access"
	}
}

// Permission is one GitHub App permission an agent seat may hold.
type Permission struct {
	// Key is GitHub's API key, the one a manifest, a token request and an
	// installation all use.
	Key string

	// Name is GitHub's own name for it, as its settings page shows it.
	Name string

	// Levels is what GitHub offers for it besides No access, lowest first.
	Levels []Level
}

// Offers reports whether GitHub offers level for this permission. No access
// is always offered.
func (p Permission) Offers(level Level) bool {
	return level == LevelNone || slices.Contains(p.Levels, level)
}

// The keys of the permissions an agent seat may hold.
const (
	PermissionContents     = "contents"
	PermissionIssues       = "issues"
	PermissionPullRequests = "pull_requests"
	PermissionChecks       = "checks"
	PermissionActions      = "actions"
	PermissionDeployments  = "deployments"

	// PermissionWorkflows is the permission a push needs to change a file
	// under `.github/workflows`. GitHub offers it only as Read and write.
	PermissionWorkflows = "workflows"

	// PermissionMetadata is granted to every app and every token: without
	// it a token cannot resolve a repository at all. Never offered, never
	// stored, always sent.
	PermissionMetadata = "metadata"
)

// SeatPermissions is the closed catalogue, in the order the Agents tab lists
// it. A form's picker and the validator both read this list, so a value the
// form offers and a value the document accepts cannot diverge.
var SeatPermissions = []Permission{
	{Key: PermissionContents, Name: "Contents", Levels: []Level{LevelRead, LevelWrite}},
	{Key: PermissionIssues, Name: "Issues", Levels: []Level{LevelRead, LevelWrite}},
	{Key: PermissionPullRequests, Name: "Pull requests", Levels: []Level{LevelRead, LevelWrite}},
	{Key: PermissionChecks, Name: "Checks", Levels: []Level{LevelRead, LevelWrite}},
	{Key: PermissionActions, Name: "Actions", Levels: []Level{LevelRead, LevelWrite}},
	// OFFERED SO NOTHING IS LOST IN THE MIGRATION: every tier carried
	// Deployments (Read-only), which is how an agent reads a deployment's
	// status, and a migrated seat keeps it.
	{Key: PermissionDeployments, Name: "Deployments", Levels: []Level{LevelRead, LevelWrite}},
	// WRITE ONLY, which is GitHub's rule rather than this engine's: the
	// permission exists to let a push change a workflow file, so there is
	// nothing for a read to mean.
	{Key: PermissionWorkflows, Name: "Workflows", Levels: []Level{LevelWrite}},
}

// LookupPermission finds a catalogue entry by its API key.
func LookupPermission(key string) (Permission, bool) {
	for _, p := range SeatPermissions {
		if p.Key == key {
			return p, true
		}
	}
	return Permission{}, false
}

// Permissions is what one seat's app holds and its tokens carry, by API key.
// An absent key is No access.
type Permissions map[string]Level

// ParsePermissions reads a seat's stored permissions, refusing anything
// GitHub does not offer an agent.
//
// EVERY PROBLEM IS RETURNED, not the first, so a person correcting a document
// sees each at once. The refusals name GitHub's names and levels, because
// those are what the dashboard shows.
//
// Keys and levels are matched as GitHub spells them, lower case. A key
// present with an empty level is No access, which is how a form clears one.
func ParsePermissions(raw map[string]string) (Permissions, []string) {
	out := Permissions{}
	var problems []string
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		level := Level(strings.TrimSpace(raw[key]))
		name := strings.TrimSpace(key)
		perm, known := LookupPermission(name)
		switch {
		case slices.Contains(Denied, name):
			problems = append(problems, fmt.Sprintf(
				"%q is never granted to an agent: it is how an agent would "+
					"widen its own access or reach the repository's secrets", name))
			continue
		case name == PermissionMetadata:
			// Granted to every app by GitHub; storing it says nothing.
			if level != LevelNone && level != LevelRead {
				problems = append(problems, "metadata is always Read-only at GitHub")
			}
			continue
		case !known:
			problems = append(problems, fmt.Sprintf(
				"%q is not a GitHub App permission an agent may hold; the "+
					"choices are %s", name, catalogueNames()))
			continue
		case !perm.Offers(level):
			problems = append(problems, fmt.Sprintf(
				"%s offers %s, not %q", perm.Name, offered(perm), string(level)))
			continue
		}
		if level != LevelNone {
			out[name] = level
		}
	}
	// A WORKFLOW CHANGE IS A PUSH, and GitHub refuses a push from a token
	// that cannot write contents whatever else it holds. Accepting the
	// pair half-set would store a grant that does nothing except widen
	// what a leaked key could ask an owner to accept.
	if out[PermissionWorkflows] == LevelWrite && out[PermissionContents] != LevelWrite {
		problems = append(problems,
			"Workflows (Read and write) needs Contents (Read and write): a "+
				"workflow change is a push, and GitHub refuses a push from a "+
				"token that cannot write contents")
	}
	return out, problems
}

// PermissionsOf reads permissions stored as strings, the way the org model
// carries a seat's (org.GitHubApp.Permissions). An empty level is No access.
func PermissionsOf(stored map[string]string) Permissions {
	out := Permissions{}
	for key, level := range stored {
		if level = strings.TrimSpace(level); level != "" {
			out[strings.TrimSpace(key)] = Level(level)
		}
	}
	return out
}

// Strings is the permissions as the document and GitHub's API store them.
func (p Permissions) Strings() map[string]string {
	out := make(map[string]string, len(p))
	for key, level := range p {
		if level != LevelNone {
			out[key] = string(level)
		}
	}
	return out
}

// Token is what a manifest asks GitHub to register and what a token request
// asks GitHub to mint: these permissions plus Metadata (Read-only), which
// every token needs to resolve a repository.
//
// ONE LIST FOR EVERY ASKER. The manifest an app is created from, the token
// minted for it and the installation it is checked against all read this, so
// an app created for a seat holds exactly what its tokens ask for (D5: the
// registration is the seat's own permissions, never more).
func (p Permissions) Token() map[string]string {
	out := p.Strings()
	out[PermissionMetadata] = string(LevelRead)
	return out
}

// key is the permissions as one canonical string, for a cache key: sorted
// `key=level` pairs, so two maps that grant the same key alike.
func (p Permissions) key() string {
	held := p.Strings()
	pairs := make([]string, 0, len(held))
	for _, name := range slices.Sorted(maps.Keys(held)) {
		pairs = append(pairs, name+"="+held[name])
	}
	return strings.Join(pairs, ",")
}

// Equal reports two permission sets that grant the same.
func (p Permissions) Equal(other Permissions) bool {
	return maps.Equal(p.Strings(), other.Strings())
}

// Describe is the permissions in GitHub's names, for a sentence: "Contents:
// Read and write, Issues: Read-only". Catalogue order, so two runs over one
// seat produce the same sentence.
func (p Permissions) Describe() string {
	var parts []string
	for _, perm := range SeatPermissions {
		if level := p[perm.Key]; level != LevelNone {
			parts = append(parts, perm.Name+": "+LevelLabel(level))
		}
	}
	if len(parts) == 0 {
		return "No access"
	}
	return strings.Join(parts, ", ")
}

// Raises is the keys whose level rises from p to next, sorted.
//
// The step-up rule and the D5 permission request both start here: a raise is
// what may need GitHub's owner to accept it, a narrowing never does.
func (p Permissions) Raises(next Permissions) []string {
	var out []string
	for key, level := range next {
		if level.Rank() > p[key].Rank() {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// AdministrationClass is the permissions that grant what D10 treats as
// administration on GitHub: Read and write on Contents (it pushes to any
// branch the protection rules allow) or on Workflows (it changes what runs
// with the repository's secrets).
var AdministrationClass = []string{PermissionContents, PermissionWorkflows}

// AdministrationRaises is the administration-class permissions raised to
// Read and write from p to next, sorted. Non-empty means the change needs a
// fresh step-up (D10).
func (p Permissions) AdministrationRaises(next Permissions) []string {
	var out []string
	for _, key := range AdministrationClass {
		if next[key] == LevelWrite && p[key] != LevelWrite {
			out = append(out, key)
		}
	}
	return out
}

// Short is what these permissions need and held does not grant, sorted.
// Metadata is included, because GitHub refuses a token request naming it
// when the installation does not hold it.
//
// Read against an INSTALLATION it is what the organization has still to
// accept; read against the app's REGISTRATION it is what the app's owner has
// still to add on the permissions page (D5).
func (p Permissions) Short(held map[string]string) []string {
	var out []string
	for key, want := range p.Token() {
		if Level(strings.TrimSpace(held[key])).Rank() < Level(want).Rank() {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// Names is keys in GitHub's names, for a sentence. A key outside the
// catalogue (metadata, or a permission an installation was widened to) is
// given as GitHub's key.
func Names(keys []string) string {
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		if perm, ok := LookupPermission(key); ok {
			names = append(names, perm.Name)
			continue
		}
		if key == PermissionMetadata {
			names = append(names, "Metadata")
			continue
		}
		names = append(names, key)
	}
	return strings.Join(names, ", ")
}

// TierPermissions is what a seat stored under the retired tiers holds in
// GitHub's terms: the migration source, and nothing else reads it.
//
// Every permission the tier carried, in the catalogue's keys, so a migrated
// seat's tokens carry exactly what they did: Contents, Issues, Pull requests,
// Checks, Actions and Deployments at the tier's levels, and Workflows where the
// seat opted in at full access. Metadata is granted to every token anyway. A
// tier permission the catalogue did not offer would be dropped silently, which
// a test holds never to happen.
//
// WORKFLOWS ONLY AT FULL ACCESS. The opt-in was a seat's own and never a
// tier's, and the earlier engine granted it to a full-access seat alone, so a
// document a validator never saw that opted a review seat in migrates without
// it: a seat that could not push code had nothing to push a workflow with.
//
// The second result reports a tier this package knew, exactly as
// [ParseTier]'s does, so the migration can report a typo rather than
// silently migrate the default.
func TierPermissions(tier string, workflows bool) (Permissions, bool) {
	parsed, ok := ParseTier(tier)
	held := parsed.Permissions()
	if workflows && parsed == TierFullAccess {
		held[PermissionWorkflows] = string(LevelWrite)
	}
	out := Permissions{}
	for _, perm := range SeatPermissions {
		if level := Level(held[perm.Key]); level != LevelNone {
			out[perm.Key] = level
		}
	}
	return out, ok
}

// catalogueNames is every offered permission's name, for a refusal.
func catalogueNames() string {
	names := make([]string, 0, len(SeatPermissions))
	for _, perm := range SeatPermissions {
		names = append(names, perm.Name)
	}
	return strings.Join(names, ", ")
}

// offered is the levels one permission offers, in GitHub's names.
func offered(perm Permission) string {
	labels := []string{LevelLabel(LevelNone)}
	for _, level := range perm.Levels {
		labels = append(labels, LevelLabel(level))
	}
	if len(labels) == 2 {
		return labels[0] + " or " + labels[1]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " or " + labels[len(labels)-1]
}
