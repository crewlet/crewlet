package capability

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// MaxLines is the most bullet lines either section renders.
const MaxLines = 20

// The two sections' headings and opening sentences.
const (
	YourAccessHeading = "## Your access"
	yourAccessIntro   = "What you can reach with your own credentials now. This is current on " +
		"every turn: where anything you remember, or anything earlier in this conversation, " +
		"disagrees, this wins."

	ChangesHeading = "## What changed since your last turn"
	changesIntro   = "Your access changed. These lines are current and replace anything you " +
		"remember, or that earlier messages in this conversation say, about the same access. " +
		"Keep each corrected fact with `reflect_and_persist`."
)

// YourAccess renders the "## Your access" section: one line per granted
// integration and one naming those not granted, at most [MaxLines]. Empty for
// a company that connects no integration. A function of the snapshot alone, so
// it is byte-stable while the digest is.
func YourAccess(s Snapshot) string {
	s = s.normal()
	if len(s.Integrations) == 0 && len(s.NotGranted) == 0 {
		return ""
	}
	var lines []string
	for _, in := range s.Integrations {
		lines = append(lines, "- "+integrationLine(in))
	}
	room := MaxLines
	if len(s.NotGranted) > 0 {
		room--
	}
	if len(lines) > room {
		rest := len(lines) - (room - 1)
		lines = append(lines[:room-1], fmt.Sprintf("- (and %d more integrations)", rest))
	}
	if len(s.NotGranted) > 0 {
		names := make([]string, 0, len(s.NotGranted))
		for _, lane := range s.NotGranted {
			names = append(names, seataccess.LaneName(seataccess.Lane(lane)))
		}
		lines = append(lines, "- Not granted to you: "+strings.Join(names, ", ")+".")
	}
	return strings.Join(append([]string{YourAccessHeading, yourAccessIntro}, lines...), "\n")
}

// integrationLine is one granted integration, in its vendor's words.
func integrationLine(in Integration) string {
	head := seataccess.LaneName(seataccess.Lane(in.Lane))
	if len(in.Scope) > 0 {
		head += " (" + scopeList(in.Scope, in.ScopeLabels) + ")"
	}
	if !in.On {
		return head + ": granted, and your account is not active yet, so its tools are unavailable."
	}
	var parts []string
	if in.Level != "" {
		level := nameOr(in.LevelName, in.Level) + "."
		if in.LevelNote != "" {
			level += " " + in.LevelNote
		}
		parts = append(parts, level)
	}
	if len(in.PermissionNames) > 0 || len(in.Permissions) > 0 {
		parts = append(parts, permissionWords(in)...)
	}
	if len(parts) == 0 {
		parts = append(parts, "your own account.")
	}
	if tool := toolSentence(seataccess.ToolState(in.Tool), false); tool != "" {
		parts = append(parts, capitalized(tool))
	}
	return head + ": " + strings.Join(parts, " ")
}

// permissionWords is GitHub's grants in effect, what is requested and not in
// effect yet, and what is not granted, each in GitHub's names.
func permissionWords(in Integration) []string {
	keys := slices.Sorted(maps.Keys(in.PermissionNames))
	for key := range in.Permissions {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	pending := map[string]config.GitHubAccess{}
	for _, entry := range pendingOf(in.Pending) {
		key, access := pendingEntry(entry)
		pending[key] = access
	}
	var held, requested, none []string
	for _, key := range keys {
		name := nameOr(in.PermissionNames[key], key)
		access := config.GitHubAccess(in.Permissions[key])
		if access != "" {
			held = append(held, name+" "+access.Label())
		}
		if want, ok := pending[key]; ok {
			requested = append(requested, name+" "+want.Label())
		} else if access == "" {
			none = append(none, name)
		}
	}
	var out []string
	if len(held) > 0 {
		out = append(out, strings.Join(held, ", ")+".")
	} else {
		out = append(out, "No permissions in effect.")
	}
	if len(requested) > 0 {
		out = append(out, "Requested and not in effect yet, until an organization owner accepts "+
			"the app's new permissions: "+strings.Join(requested, ", ")+".")
	}
	if len(none) > 0 {
		out = append(out, "Not granted: "+strings.Join(none, ", ")+".")
	}
	return out
}

// ChangesBlock renders the "## What changed since your last turn" section: at
// most [MaxLines] change lines and a line counting the rest. Empty for no
// changes.
func ChangesBlock(changes []Change) string {
	if len(changes) == 0 {
		return ""
	}
	lines := []string{ChangesHeading, changesIntro}
	shown := changes
	if len(shown) > MaxLines {
		shown = shown[:MaxLines]
	}
	for _, c := range shown {
		lines = append(lines, "- "+c.Line)
	}
	if rest := len(changes) - len(shown); rest > 0 {
		noun := "changes"
		if rest == 1 {
			noun = "change"
		}
		lines = append(lines, fmt.Sprintf("- (and %d more %s: the rest is under \"Your access\")",
			rest, noun))
	}
	return strings.Join(lines, "\n")
}

func capitalized(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
