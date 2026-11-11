package capability

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/toolname"
)

// MaxLines is the most bullet lines either section renders.
const MaxLines = 20

// The two sections' headings and opening sentences.
const (
	YourAccessHeading = "## Your access"
	yourAccessIntro   = "What you can reach with your own credentials now. Crewlet narrows " +
		"every credential you hold to the grant an administrator sets for you in Crewlet, so " +
		"a vendor's own settings never widen it and only an administrator changes it: ask " +
		"one for more, with `" + toolname.RequestAccess + "` where your tools include it. " +
		"This is current on every turn: where anything you remember, or anything earlier in " +
		"this conversation, disagrees, this wins."

	ChangesHeading = "## What changed since your last turn"
	changesIntro   = "Your access changed. These lines are current and replace anything you " +
		"remember, or that earlier messages in this conversation say, about the same access. " +
		"Keep each corrected fact with `" + toolname.ReflectAndPersist + "`."
)

// YourAccess renders the "## Your access" section: one line per granted
// integration, one per box the seat's runs use, and one naming the
// integrations not granted, at most [MaxLines]. Empty for a company that
// connects no integration and runs no box. A function of the snapshot alone, so
// it is byte-stable while the digest is.
func YourAccess(s Snapshot) string {
	s = s.normal()
	if len(s.Integrations) == 0 && len(s.NotGranted) == 0 && len(s.Boxes) == 0 {
		return ""
	}
	var lines []string
	for _, in := range s.Integrations {
		lines = append(lines, "- "+integrationLine(in))
	}
	room := MaxLines - len(s.Boxes)
	if len(s.NotGranted) > 0 {
		room--
	}
	if len(lines) > room {
		rest := len(lines) - (room - 1)
		lines = append(lines[:room-1], fmt.Sprintf("- (and %d more integrations)", rest))
	}
	for _, b := range s.Boxes {
		lines = append(lines, "- "+boxLine(b, len(s.Boxes) > 1, voiceOf(ForSeat)))
	}
	if len(s.NotGranted) > 0 {
		names := make([]string, 0, len(s.NotGranted))
		for _, lane := range s.NotGranted {
			names = append(names, seataccess.LaneName(seataccess.Lane(lane)))
		}
		lines = append(lines, "- Not in your Crewlet grant: "+strings.Join(names, ", ")+
			". An administrator grants an integration under Settings, Integrations, then "+
			"its Agents tab.")
	}
	return strings.Join(append([]string{YourAccessHeading, yourAccessIntro}, lines...), "\n")
}

// integrationLine is one granted integration, in its vendor's words.
func integrationLine(in Integration) string {
	head := seataccess.LaneName(seataccess.Lane(in.Lane))
	switch {
	case len(in.Scope) > 0 && in.ScopeMore > 0:
		head += fmt.Sprintf(" (%s, and %d more)", scopeList(in.Scope, in.ScopeLabels), in.ScopeMore)
	case len(in.Scope) > 0:
		head += " (" + scopeList(in.Scope, in.ScopeLabels) + ")"
	case in.ScopeMore > 0:
		head += fmt.Sprintf(" (%d scope entries)", in.ScopeMore)
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
	parts = append(parts, grantSentence(seataccess.Lane(in.Lane)))
	if tool := toolSentence(seataccess.ToolState(in.Tool), false, voiceOf(ForSeat)); tool != "" {
		parts = append(parts, capitalized(tool))
	}
	for _, hc := range in.Commands {
		parts = append(parts, commandsSentence(hc))
	}
	return head + ": " + strings.Join(parts, " ")
}

// grantSentence is where a lane's level, permissions and scope are decided:
// the seat's Crewlet grant, and the screen an administrator changes it on.
func grantSentence(lane seataccess.Lane) string {
	return "Your Crewlet grant sets this; an administrator changes it under " +
		seataccess.GrantPlace(lane) + "."
}

// commandsSentence is one SSH host's read commands, as the seat types them.
func commandsSentence(hc HostCommands) string {
	run := make([]string, 0, len(hc.Run))
	for _, line := range hc.Run {
		run = append(run, "`"+line+"`")
	}
	out := "Read commands on " + hc.Host + ": " + strings.Join(run, ", ")
	if hc.More > 0 {
		out += fmt.Sprintf(", and %d more", hc.More)
	}
	return out + "."
}

// permissionWords is GitHub's grants in effect, what is requested and not in
// effect yet, and what the grant leaves out (noting what the app already
// holds), each in GitHub's names.
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
			none = append(none, name+appHoldsWords(in, key))
		}
	}
	var out []string
	if len(held) > 0 {
		out = append(out, strings.Join(held, ", ")+".")
	} else {
		out = append(out, "No permissions in effect.")
	}
	if len(requested) > 0 {
		out = append(out, "Requested in your Crewlet grant and not in effect yet, until an "+
			"organization owner accepts the app's new permissions at GitHub: "+
			strings.Join(requested, ", ")+".")
	}
	if len(none) > 0 {
		out = append(out, "Not in your Crewlet grant: "+strings.Join(none, ", ")+".")
	}
	return out
}

// appHoldsWords is what a permission the grant leaves out waits on at GitHub,
// where the seat's app already holds it: nothing up to the level the app
// holds, and an organization owner past it. Empty where the app holds none.
func appHoldsWords(in Integration, key string) string {
	switch held := config.GitHubAccess(in.AppHolds[key]); held {
	case config.GitHubWrite:
		return " (the app already holds it at " + held.Label() + ", so nothing at GitHub waits)"
	case config.GitHubRead:
		return " (the app already holds it at " + held.Label() + ", so that waits on nothing at " +
			"GitHub; " + config.GitHubWrite.Label() + " waits on an organization owner accepting it there)"
	}
	return ""
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
