package capability

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/textcut"
)

// ChangeKind is what one change to a seat's access is. THE CLOSED LIST: a
// difference between two snapshots that is none of these is no change, which
// is how a credential's rotation, a model, the roster, the mission, a profile,
// a schedule, a budget and an MCP tool's definition stay out of it.
type ChangeKind string

// The change kinds.
const (
	ChangeIntegrationOn     ChangeKind = "integration_on"
	ChangeIntegrationOff    ChangeKind = "integration_off"
	ChangeLevel             ChangeKind = "level_changed"
	ChangePermission        ChangeKind = "permission_changed"
	ChangeScopeAdded        ChangeKind = "scope_added"
	ChangeScopeRemoved      ChangeKind = "scope_removed"
	ChangePendingStarted    ChangeKind = "pending_started"
	ChangePendingCleared    ChangeKind = "pending_cleared"
	ChangeToolState         ChangeKind = "tool_state_changed"
	ChangeServerAdded       ChangeKind = "server_added"
	ChangeServerRemoved     ChangeKind = "server_removed"
	ChangeToolLevel         ChangeKind = "tool_level_changed"
	ChangePolicyAdded       ChangeKind = "policy_added"
	ChangePolicyRemoved     ChangeKind = "policy_removed"
	ChangeSetupStepAdded    ChangeKind = "setup_step_added"
	ChangeSetupStepRemoved  ChangeKind = "setup_step_removed"
	ChangeSetupStepModified ChangeKind = "setup_step_changed"
)

// ChangeKinds is the closed list, in the order a turn is told them.
var ChangeKinds = []ChangeKind{
	ChangeIntegrationOn, ChangeIntegrationOff, ChangeLevel, ChangePermission,
	ChangeScopeAdded, ChangeScopeRemoved, ChangePendingStarted, ChangePendingCleared,
	ChangeToolState, ChangeServerAdded, ChangeServerRemoved, ChangeToolLevel,
	ChangePolicyAdded, ChangePolicyRemoved,
	ChangeSetupStepAdded, ChangeSetupStepRemoved, ChangeSetupStepModified,
}

// Valid reports whether k is one of [ChangeKinds].
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds, k) }

// MaxLineBytes is the longest change line a turn is told.
const MaxLineBytes = 300

// Change is one change to a seat's access, whole: what it is about, the
// values either side, and the line the seat is told.
type Change struct {
	Kind ChangeKind `json:"kind"`

	// Vendor is who the change is about as a person reads it: the vendor's
	// name for an integration, the server's name for an MCP server.
	Vendor string `json:"vendor"`

	// Lane is the integration lane, empty for every other change.
	Lane string `json:"lane,omitempty"`

	// Items are what the change names: scope entries, a permission's name,
	// tool names, a setup step's name.
	Items []string `json:"items,omitempty"`

	// From and To are the values either side, as a person reads them: level
	// names, a permission's grant, a tool state.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// Line is what the seat is told, at most [MaxLineBytes].
	Line string `json:"line"`
}

// Access reports a change to what the seat reaches with its own credentials:
// an integration's or an MCP server's, which a note it kept may contradict.
func (c Change) Access() bool { return c.Lane != "" || c.serverChange() }

func (c Change) serverChange() bool {
	return c.Kind == ChangeServerAdded || c.Kind == ChangeServerRemoved || c.Kind == ChangeToolLevel
}

// Terms is what a note naming this change contains: the vendor's name, the
// lane, the scope entries, GitHub permission names, the level names before and
// after, and an MCP server's name. Nil for a change that is not [Change.Access].
func (c Change) Terms() []string {
	if !c.Access() {
		return nil
	}
	terms := []string{c.Vendor}
	if c.Lane != "" {
		terms = append(terms, c.Lane)
		switch c.Kind {
		case ChangeScopeAdded, ChangeScopeRemoved, ChangePermission,
			ChangePendingStarted, ChangePendingCleared:
			terms = append(terms, c.Items...)
		case ChangeLevel:
			terms = append(terms, c.From, c.To)
		}
	}
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t != "" && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, t) }) {
			out = append(out, t)
		}
	}
	return out
}

// Diff is every change from old to now, in the order a turn is told them:
// integrations by lane, then MCP servers, policies and box setup.
//
// AGAINST WHAT THE SEAT WAS LAST TOLD, never the previous revision: a revert
// is a change back, and an unchanged revision applied again is nothing.
func Diff(old, now Snapshot) []Change {
	// AT ONE LEVEL: a snapshot shortened to fit is compared with the other
	// shortened alike, so what one level leaves out is never told as gone.
	level := max(old.Compact, now.Compact)
	old, now = old.at(level), now.at(level)
	var out []Change
	out = append(out, integrationChanges(old, now)...)
	out = append(out, serverChanges(old.MCP, now.MCP)...)
	out = append(out, foldedChange(ChangeServerAdded, ChangeServerRemoved, "Tool servers",
		old.MCP, now.MCP, old.MCPMore, now.MCPMore, old.MCPDigest, now.MCPDigest,
		"tool servers changed past the ones you are told by name.")...)
	out = append(out, policyChanges(old.Policies, now.Policies)...)
	out = append(out, foldedChange(ChangePolicyAdded, ChangePolicyRemoved, "Policy",
		old.Policies, now.Policies, old.PoliciesMore, now.PoliciesMore, old.PoliciesDigest, now.PoliciesDigest,
		"the company's policies changed past the ones you are told here; your system prompt holds them all.")...)
	out = append(out, setupChanges(old.Setup, now.Setup)...)
	out = append(out, foldedChange(ChangeSetupStepModified, ChangeSetupStepModified, "Box setup",
		old.Setup, now.Setup, old.SetupMore, now.SetupMore, old.SetupDigest, now.SetupDigest,
		"steps changed past the ones you are told by name.")...)
	for i := range out {
		out[i].Line = textcut.Within(out[i].Line, MaxLineBytes)
	}
	return out
}

func integrationChanges(old, now Snapshot) []Change {
	before := map[string]Integration{}
	for _, in := range old.Integrations {
		before[in.Lane] = in
	}
	after := map[string]Integration{}
	for _, in := range now.Integrations {
		after[in.Lane] = in
	}
	lanes := slices.Sorted(maps.Keys(before))
	for lane := range after {
		if _, ok := before[lane]; !ok {
			lanes = append(lanes, lane)
		}
	}
	slices.Sort(lanes)
	var out []Change
	for _, lane := range lanes {
		was, had := before[lane]
		is, has := after[lane]
		vendor := seataccess.LaneName(seataccess.Lane(lane))
		base := Change{Vendor: vendor, Lane: lane}
		switch {
		case has && is.On && (!had || !was.On):
			c := base
			c.Kind, c.To = ChangeIntegrationOn, is.LevelName
			c.Line = vendor + ": you now have your own account" + grantedAt(is) + "."
			out = append(out, c)
			continue
		case had && was.On && !(has && is.On):
			c := base
			c.Kind, c.From = ChangeIntegrationOff, was.LevelName
			c.Line = vendor + ": no longer granted to you."
			if has {
				c.Line = vendor + ": your account is not active now, so its tools are unavailable."
			}
			out = append(out, c)
			continue
		case !(had && was.On && has && is.On):
			// NEITHER SIDE CAN ACT: a grant whose account is not made yet,
			// or one turned off at the vendor, changes nothing the seat can
			// use until it is on.
			continue
		}
		out = append(out, laneChanges(base, was, is)...)
	}
	return out
}

// grantedAt is " at <level>, in <scope>" for a lane just granted.
func grantedAt(in Integration) string {
	var b strings.Builder
	if in.LevelName != "" {
		b.WriteString(" at " + in.LevelName)
	}
	if len(in.Scope) > 0 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(" in " + scopeList(in.Scope, in.ScopeLabels))
	}
	return b.String()
}

// laneChanges is what moved on a lane the seat could act on before and can
// now.
func laneChanges(base Change, was, is Integration) []Change {
	var out []Change
	vendor := base.Vendor
	if was.Level != is.Level {
		c := base
		c.Kind, c.From, c.To = ChangeLevel, nameOr(was.LevelName, was.Level), nameOr(is.LevelName, is.Level)
		c.Line = fmt.Sprintf("%s: your level is now %s (was %s).", vendor, c.To, c.From)
		if is.LevelNote != "" {
			c.Line += " " + is.LevelNote
		}
		out = append(out, c)
	}
	names := maps.Clone(is.PermissionNames)
	if names == nil {
		names = map[string]string{}
	}
	for key, name := range was.PermissionNames {
		if _, ok := names[key]; !ok {
			names[key] = name
		}
	}
	wasPending, isPending := pendingOf(was.Pending), pendingOf(is.Pending)
	// A REQUEST THAT CAME INTO EFFECT is told once, as that, rather than
	// again as the permission it moved.
	arrived := map[string]bool{}
	for _, entry := range minus(wasPending, isPending) {
		key, access := pendingEntry(entry)
		arrived[key] = config.GitHubAccess(is.Permissions[key]).Rank() >= access.Rank()
	}
	keys := slices.Sorted(maps.Keys(unionKeys(was.Permissions, is.Permissions)))
	for _, key := range keys {
		from, to := config.GitHubAccess(was.Permissions[key]), config.GitHubAccess(is.Permissions[key])
		if from == to || arrived[key] {
			continue
		}
		name := nameOr(names[key], key)
		c := base
		c.Kind, c.Items, c.From, c.To = ChangePermission, []string{name}, from.Label(), to.Label()
		c.Line = fmt.Sprintf("%s: %s is now %s (was %s).", vendor, name, c.To, c.From)
		out = append(out, c)
	}
	if added := minus(is.Scope, was.Scope); len(added) > 0 {
		c := base
		c.Kind, c.Items = ChangeScopeAdded, added
		c.Line = vendor + ": you can now work in " + scopeList(added, is.ScopeLabels) + "."
		out = append(out, c)
	}
	if removed := minus(was.Scope, is.Scope); len(removed) > 0 {
		c := base
		c.Kind, c.Items = ChangeScopeRemoved, removed
		c.Line = vendor + ": you can no longer work in " + scopeList(removed, was.ScopeLabels) + "."
		out = append(out, c)
	}
	for _, entry := range minus(isPending, wasPending) {
		key, access := pendingEntry(entry)
		name := nameOr(names[key], key)
		c := base
		c.Kind, c.Items, c.To = ChangePendingStarted, []string{name}, access.Label()
		c.Line = fmt.Sprintf("%s: %s %s is requested and not in effect yet: an organization "+
			"owner has to accept the app's new permissions.", vendor, name, access.Label())
		out = append(out, c)
	}
	for _, entry := range minus(wasPending, isPending) {
		key, access := pendingEntry(entry)
		name := nameOr(names[key], key)
		c := base
		c.Kind, c.Items, c.From = ChangePendingCleared, []string{name}, access.Label()
		if arrived[key] {
			c.Line = fmt.Sprintf("%s: %s %s is now in effect.", vendor, name, access.Label())
		} else {
			c.Line = fmt.Sprintf("%s: %s %s is no longer requested.", vendor, name, access.Label())
		}
		out = append(out, c)
	}
	if slices.Equal(was.Scope, is.Scope) && (was.ScopeMore != is.ScopeMore || was.ScopeDigest != is.ScopeDigest) {
		c := base
		c.Kind = ChangeScopeAdded
		if is.ScopeMore < was.ScopeMore {
			c.Kind = ChangeScopeRemoved
		}
		c.Line = fmt.Sprintf("%s: your scope changed past the %d entries you are told by name; "+
			"you now hold %d.", vendor, len(is.Scope), len(is.Scope)+is.ScopeMore)
		out = append(out, c)
	}
	if was.Tool != is.Tool {
		c := base
		c.Kind, c.From, c.To = ChangeToolState, was.Tool, is.Tool
		c.Line = vendor + ": " + toolSentence(seataccess.ToolState(is.Tool), true)
		out = append(out, c)
	}
	return out
}

func serverChanges(old, now []ToolServer) []Change {
	before := map[string]ToolServer{}
	for _, s := range old {
		before[s.Server] = s
	}
	var out []Change
	for _, s := range now {
		was, had := before[s.Server]
		delete(before, s.Server)
		if !had {
			out = append(out, Change{Kind: ChangeServerAdded, Vendor: s.Server,
				Line: "New tool server: " + s.Server + "."})
			continue
		}
		if c, ok := toolLevelChange(was, s); ok {
			out = append(out, c)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		out = append(out, Change{Kind: ChangeServerRemoved, Vendor: name,
			Line: "Tool server removed: " + name + "."})
	}
	slices.SortStableFunc(out, func(a, b Change) int { return strings.Compare(a.Vendor, b.Vendor) })
	return out
}

// toolLevelChange is what moved among one server's tools: the tools that now
// need an approval, those now offered no more, and those that now run without
// one.
func toolLevelChange(was, is ToolServer) (Change, bool) {
	var parts, items []string
	add := func(tools []string, one, many string) {
		if len(tools) == 0 {
			return
		}
		items = append(items, tools...)
		what := many
		if len(tools) == 1 {
			what = one
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", count(len(tools), "tool"), what, listSome(tools)))
	}
	add(minus(is.Approve, was.Approve), "now needs a person's approval before each call",
		"now need a person's approval before each call")
	gone := minus(union(was.Allow, was.Approve), union(is.Allow, is.Approve))
	add(gone, "is no longer offered to you", "are no longer offered to you")
	add(minus(is.Allow, was.Allow), "now runs without an approval", "now run without an approval")
	if was.Default != is.Default {
		parts = append(parts, "every other tool "+defaultWords(is.Default))
	}
	if len(parts) == 0 && (was.More != is.More || was.ToolsDigest != is.ToolsDigest) {
		parts = append(parts, "tools past the ones you are told by name changed level")
	}
	if len(parts) == 0 {
		return Change{}, false
	}
	return Change{Kind: ChangeToolLevel, Vendor: is.Server, Items: sortedSet(items),
		Line: is.Server + ": " + strings.Join(parts, "; ") + "."}, true
}

// foldedChange is one general change line for a list whose shown entries are
// the same and whose folded rest moved: the kind grown where it holds more,
// shrunk where fewer.
func foldedChange[T any](grown, shrunk ChangeKind, vendor string, was, is []T,
	wasMore, isMore int, wasDigest, isDigest string, words string) []Change {

	if len(was) != len(is) || (wasMore == isMore && wasDigest == isDigest) {
		return nil
	}
	kind := grown
	if isMore < wasMore {
		kind = shrunk
	}
	return []Change{{Kind: kind, Vendor: vendor, Line: vendor + ": " + words}}
}

// defaultWords is what a server's default level means for its unnamed tools.
func defaultWords(level string) string {
	switch config.MCPToolLevel(level) {
	case config.MCPToolAllow:
		return "now runs without an approval"
	case config.MCPToolApprove:
		return "now needs a person's approval before each call"
	case config.MCPToolBlocked:
		return "is no longer offered to you"
	}
	return "now runs at the level its server marks it with"
}

func policyChanges(old, now []Policy) []Change {
	var out []Change
	had := map[string]bool{}
	for _, p := range old {
		had[p.Digest] = true
	}
	has := map[string]bool{}
	for _, p := range now {
		has[p.Digest] = true
		if !had[p.Digest] {
			out = append(out, Change{Kind: ChangePolicyAdded, Vendor: "Policy",
				Line: quoted("Policy added: ", p.Text)})
		}
	}
	for _, p := range old {
		if !has[p.Digest] {
			out = append(out, Change{Kind: ChangePolicyRemoved, Vendor: "Policy",
				Line: quoted("Policy removed: ", p.Text)})
		}
	}
	return out
}

// quoted is prefix and the text in quotes, the text cut so the whole line
// fits [MaxLineBytes].
func quoted(prefix, text string) string {
	room := MaxLineBytes - len(prefix) - len(`""`)
	return prefix + `"` + textcut.Within(strings.Join(strings.Fields(text), " "), room) + `"`
}

func setupChanges(old, now []SetupStep) []Change {
	key := func(s SetupStep) string { return s.From + "\x00" + s.Name }
	before := map[string]SetupStep{}
	for _, s := range old {
		before[key(s)] = s
	}
	var out []Change
	for _, s := range now {
		was, had := before[key(s)]
		delete(before, key(s))
		switch {
		case !had:
			out = append(out, Change{Kind: ChangeSetupStepAdded, Vendor: "Box setup",
				Items: []string{s.Name}, Line: "Box setup added: " + s.Name + "."})
		case was.Brief != s.Brief:
			out = append(out, Change{Kind: ChangeSetupStepModified, Vendor: "Box setup",
				Items: []string{s.Name}, Line: "Box setup changed: " + s.Name + "."})
		}
	}
	for _, s := range old {
		if _, gone := before[key(s)]; gone {
			out = append(out, Change{Kind: ChangeSetupStepRemoved, Vendor: "Box setup",
				Items: []string{s.Name}, Line: "Box setup removed: " + s.Name + "."})
		}
	}
	return out
}

// toolSentence says what a tool state means for the seat, as a clause; first
// is whether it opens a change line, which a state the seat can use says too.
func toolSentence(state seataccess.ToolState, first bool) string {
	switch state {
	case seataccess.ToolServerOff:
		return "your tool server is off, so its tools are unavailable."
	case seataccess.ToolNoServer:
		return "no tool server is set up for it, so it has no tools yet."
	case seataccess.ToolWaiting:
		return "your credential is not set up yet, so its tools are unavailable."
	case seataccess.ToolOff:
		return "its tools are unavailable."
	}
	if first {
		return "your tools for it are available."
	}
	return ""
}

func pendingOf(pending string) []string {
	var out []string
	for _, entry := range strings.Split(pending, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return sortedSet(out)
}

func pendingEntry(entry string) (string, config.GitHubAccess) {
	key, access, _ := strings.Cut(entry, ":")
	return key, config.GitHubAccess(access)
}

// scopeList is entries by their ids, each with its label where it has one.
func scopeList(entries []string, labels map[string]string) string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if label := labels[e]; label != "" {
			out = append(out, e+` "`+label+`"`)
		} else {
			out = append(out, e)
		}
	}
	return strings.Join(out, ", ")
}

// listSome is up to three names, and "..." for the rest.
func listSome(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + ", ..."
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func nameOr(name, id string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return id
}

// minus is what a holds that b does not, sorted.
func minus(a, b []string) []string {
	var out []string
	for _, x := range sortedSet(a) {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func union(a, b []string) []string { return sortedSet(append(slices.Clone(a), b...)) }

func unionKeys(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
