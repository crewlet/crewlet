package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// THE SNAPSHOT'S BOUNDS. A snapshot is written whole into a told record, so it
// is bounded by design, for any company the document admits: every name is at
// most [ItemMaxBytes], every list at most its cap with the rest counted and
// digested, and a snapshot still past [MaxSnapshotBytes] is shortened further
// in two fixed steps. Folded entries still move the digest, so a change past a
// cap is still told, in general words.

const (
	// MaxSnapshotBytes is the most a snapshot encodes to: under the access
	// log's own bound on one, with room.
	MaxSnapshotBytes = 48 << 10

	// ItemMaxBytes bounds a scope entry, a tool, a server, a level and a
	// setup step's name; a longer one is shortened and keeps a digest of
	// itself, so two never collide.
	ItemMaxBytes = 128

	// LabelMaxBytes bounds a scope entry's label, and NoteMaxBytes a level's
	// note, which a change line ends with.
	LabelMaxBytes = 100
	NoteMaxBytes  = MaxLineBytes

	// PolicyExcerptBytes is as much of a policy's text as a change line can
	// show: the snapshot keeps that and the whole text's digest.
	PolicyExcerptBytes = MaxLineBytes

	// The lists' caps.
	MaxScope    = 16
	MaxTools    = 16
	MaxServers  = 32
	MaxPolicies = 32
	MaxSetup    = 32

	// compactCap is every list's cap once a snapshot is shortened.
	compactCap = 4
)

// excerpt is a policy's text as a change line can show it.
func excerpt(text string) string {
	return textcut.Within(strings.Join(strings.Fields(text), " "), PolicyExcerptBytes)
}

// shortened is s within ItemMaxBytes: s itself, or its start and a digest of
// the whole, which keeps two long names apart.
func shortened(s string) string {
	if len(s) <= ItemMaxBytes {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return textcut.Within(s, ItemMaxBytes-17) + "~" + hex.EncodeToString(sum[:8])
}

// listDigest is the digest of a whole list, in its sorted order.
func listDigest(items []string) string { return digestOf([]byte(strings.Join(items, "\x00"))) }

// fold keeps the first max of a sorted list and counts the rest, digesting
// the whole: a list already folded keeps its digest of the whole.
func fold(list []string, more int, digest string, max int) ([]string, int, string) {
	if len(list) <= max && more == 0 {
		return list, 0, ""
	}
	if digest == "" {
		digest = listDigest(list)
	}
	if len(list) > max {
		more += len(list) - max
		list = list[:max]
	}
	return list, more, digest
}

// caps are one compaction level's list caps.
type caps struct{ scope, tools, servers, policies, setup int }

var (
	fullCaps    = caps{MaxScope, MaxTools, MaxServers, MaxPolicies, MaxSetup}
	compactCaps = caps{compactCap, compactCap, compactCap, compactCap, compactCap}
	digestCaps  = caps{}
)

// bound is s within its bounds: every name shortened, every list folded, and
// shortened further while it encodes past MaxSnapshotBytes. Each level is
// taken from the one before, as a stored snapshot is taken to a coarser level
// to be compared ([Diff]), so both arrive at the same bytes.
func bound(s Snapshot) Snapshot {
	out := shortenItems(s).at(0)
	for level := 1; level <= 2 && len(out.JSON()) > MaxSnapshotBytes; level++ {
		out = out.at(level)
	}
	return out
}

// shortenItems holds every name and every word to its own bound.
func shortenItems(s Snapshot) Snapshot {
	s = s.normal()
	ins := make([]Integration, len(s.Integrations))
	for i, in := range s.Integrations {
		in.Level, in.LevelName = shortened(in.Level), shortened(in.LevelName)
		in.LevelNote = textcut.Within(in.LevelNote, NoteMaxBytes)
		in.Scope = shortenAll(in.Scope)
		if len(in.ScopeLabels) > 0 {
			labels := make(map[string]string, len(in.ScopeLabels))
			for entry, label := range in.ScopeLabels {
				labels[shortened(entry)] = textcut.Within(label, LabelMaxBytes)
			}
			in.ScopeLabels = labels
		}
		ins[i] = in
	}
	s.Integrations = ins
	servers := make([]ToolServer, len(s.MCP))
	for i, ts := range s.MCP {
		ts.Server = shortened(ts.Server)
		ts.Allow, ts.Approve, ts.Blocked = shortenAll(ts.Allow), shortenAll(ts.Approve), shortenAll(ts.Blocked)
		servers[i] = ts
	}
	s.MCP = servers
	setup := make([]SetupStep, len(s.Setup))
	for i, step := range s.Setup {
		step.Name = shortened(step.Name)
		setup[i] = step
	}
	s.Setup = setup
	return s.normal()
}

// shortenAll is every entry of a list within ItemMaxBytes.
func shortenAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = shortened(s)
	}
	return sortedSet(out)
}

// at is s shortened to compaction level: 0 folds every list at its cap; 1
// also drops the words a renderer can do without (level notes, permission
// names and scope labels) and folds at four; 2 keeps no list entry and no
// policy text, only counts and digests. A snapshot is never shortened back.
func (s Snapshot) at(level int) Snapshot {
	s = s.normal()
	level = max(level, s.Compact)
	c := fullCaps
	switch level {
	case 1:
		c = compactCaps
	case 2:
		c = digestCaps
	}
	s.Compact = level
	ins := make([]Integration, len(s.Integrations))
	for i, in := range s.Integrations {
		if level >= 1 {
			in.LevelNote, in.PermissionNames, in.ScopeLabels = "", nil, nil
		}
		in.Scope, in.ScopeMore, in.ScopeDigest = fold(in.Scope, in.ScopeMore, in.ScopeDigest, c.scope)
		if len(in.Scope) == 0 {
			in.Scope = nil
		}
		for entry := range in.ScopeLabels {
			if !containsString(in.Scope, entry) {
				delete(in.ScopeLabels, entry)
			}
		}
		ins[i] = in
	}
	s.Integrations = ins
	servers := make([]ToolServer, len(s.MCP))
	for i, ts := range s.MCP {
		servers[i] = foldTools(ts, c.tools)
	}
	var names []string
	byName := map[string]ToolServer{}
	for _, ts := range servers {
		names = append(names, serverKey(ts))
		byName[serverKey(ts)] = ts
	}
	kept, more, digest := fold(names, s.MCPMore, s.MCPDigest, c.servers)
	s.MCP, s.MCPMore, s.MCPDigest = make([]ToolServer, 0, len(kept)), more, digest
	for _, key := range kept {
		s.MCP = append(s.MCP, byName[key])
	}
	policies := make([]string, 0, len(s.Policies))
	byDigest := map[string]Policy{}
	for _, p := range s.Policies {
		if level >= 2 {
			p.Text = ""
		}
		policies = append(policies, p.Digest)
		byDigest[p.Digest] = p
	}
	keptPolicies, pmore, pdigest := fold(policies, s.PoliciesMore, s.PoliciesDigest, c.policies)
	s.Policies, s.PoliciesMore, s.PoliciesDigest = make([]Policy, 0, len(keptPolicies)), pmore, pdigest
	for _, d := range keptPolicies {
		s.Policies = append(s.Policies, byDigest[d])
	}
	steps := make([]string, 0, len(s.Setup))
	byStep := map[string]SetupStep{}
	for _, step := range s.Setup {
		key := step.From + "\x00" + step.Name + "\x00" + step.Brief
		steps = append(steps, key)
		byStep[key] = step
	}
	keptSteps, smore, sdigest := fold(steps, s.SetupMore, s.SetupDigest, c.setup)
	s.Setup, s.SetupMore, s.SetupDigest = make([]SetupStep, 0, len(keptSteps)), smore, sdigest
	for _, key := range keptSteps {
		s.Setup = append(s.Setup, byStep[key])
	}
	return s.normal()
}

// foldTools folds a server's three lists at max each, digesting all three.
func foldTools(ts ToolServer, max int) ToolServer {
	total := len(ts.Allow) + len(ts.Approve) + len(ts.Blocked)
	if ts.More == 0 && len(ts.Allow) <= max && len(ts.Approve) <= max && len(ts.Blocked) <= max {
		return ts
	}
	if ts.ToolsDigest == "" {
		ts.ToolsDigest = listDigest(append(append(append([]string{"allow"}, ts.Allow...), "approve"),
			append(append(ts.Approve, "blocked"), ts.Blocked...)...))
	}
	cut := func(list []string) []string {
		if len(list) > max {
			return list[:max]
		}
		return list
	}
	ts.Allow, ts.Approve, ts.Blocked = cut(ts.Allow), cut(ts.Approve), cut(ts.Blocked)
	ts.More += total - len(ts.Allow) - len(ts.Approve) - len(ts.Blocked)
	return ts
}

// serverKey is a server's whole entry, which its fold digests.
func serverKey(ts ToolServer) string {
	return ts.Server + "\x00" + ts.Default + "\x00" + strings.Join(ts.Allow, ",") + "\x00" +
		strings.Join(ts.Approve, ",") + "\x00" + strings.Join(ts.Blocked, ",") + "\x00" + ts.ToolsDigest
}

// containsString reports whether a sorted list holds s.
func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
