package capability

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Version is the snapshot shape this build writes. A snapshot evolves by
// addition only, so an older one decodes into this shape.
const Version = 1

// Setup step origins ([SetupStep.From]).
const (
	SetupFromCompany = "company"
	SetupFromSeat    = "seat"
)

// Words is how one lane's grants read in its vendor's own terms: its levels,
// its permissions and the names of its scope entries.
type Words struct {
	Levels      []seataccess.Level
	Permissions []seataccess.PermissionSpec
	ScopeNames  map[string]string
}

// WordsOf is lane's words as this build knows them with no session: the levels
// its vendor fixes ([seataccess.KnownLevels]) and the permissions its catalog
// declares. A vendor that lists its levels live answers none, and its level
// is named by its id unless the caller hands in a live read.
func WordsOf(kind integration.Kind, lane seataccess.Lane) Words {
	var w Words
	w.Levels, _ = seataccess.KnownLevels(kind, lane)
	if v, ok := seataccess.Lookup(kind); ok {
		if spec, ok := v.Catalog().Lane(lane); ok {
			w.Permissions = spec.Permissions
		}
	}
	return w
}

// ToolServer is one MCP server the seat reaches, with its tools by the level
// each runs at, as the seat's tool surface holds them.
type ToolServer struct {
	Server  string   `json:"server"`
	Allow   []string `json:"allow"`
	Approve []string `json:"approve"`
	Blocked []string `json:"blocked"`

	// Default is the level of every tool the lists do not name, empty where
	// each such tool runs at the level its server's hints give it.
	Default string `json:"default,omitempty"`
}

// Input is everything a seat's snapshot is read from.
type Input struct {
	// Company is the active document, and Handle the seat's.
	Company *config.Company
	Handle  string

	// Observed is what each lane's last sync found at the vendor for the
	// seat (the access ledger's record); a lane absent here holds no account.
	Observed map[seataccess.Lane]seataccess.Observed

	// Words is each lane's vendor words; a lane absent here reads [WordsOf].
	Words map[seataccess.Lane]Words

	// Tools is the seat's MCP servers and their tools' levels.
	Tools []ToolServer
}

// Integration is one lane the seat is granted.
type Integration struct {
	Kind string `json:"kind"`
	Lane string `json:"lane"`

	// On is whether the seat's account exists and is active: granted and not
	// on is an account still being made, or one turned off at the vendor.
	On bool `json:"on"`

	Level     string `json:"level,omitempty"`
	LevelName string `json:"level_name,omitempty"`
	LevelNote string `json:"level_note,omitempty"`

	// Permissions is GitHub's grants in effect, by key; PermissionNames
	// every permission the vendor offers, by key, so a renderer can say
	// what is not granted.
	Permissions     map[string]string `json:"permissions,omitempty"`
	PermissionNames map[string]string `json:"permission_names,omitempty"`

	Scope       []string          `json:"scope,omitempty"`
	ScopeLabels map[string]string `json:"scope_labels,omitempty"`

	// Tool is [seataccess.ToolState], and Pending the grants requested and
	// not in effect yet, as sorted `key:access` entries joined by commas.
	Tool    string `json:"tool"`
	Pending string `json:"pending"`
}

// Policy is one company policy, whole, and its digest.
type Policy struct {
	Digest string `json:"digest"`
	Text   string `json:"text"`
}

// SetupStep is one box setup step the seat's runs get: where it is declared,
// its name, and the digest of its brief (never its files or commands).
type SetupStep struct {
	From  string `json:"from"`
	Name  string `json:"name"`
	Brief string `json:"brief,omitempty"`
}

// Snapshot is what one seat can reach now.
type Snapshot struct {
	V            int           `json:"v"`
	Integrations []Integration `json:"integrations"`
	NotGranted   []string      `json:"not_granted"`
	MCP          []ToolServer  `json:"mcp"`
	Policies     []Policy      `json:"policies"`
	Setup        []SetupStep   `json:"setup"`
}

// Take reads the seat's snapshot from in. Pure: the same input is the same
// snapshot, byte for byte.
func Take(in Input) Snapshot {
	s := Snapshot{V: Version, Integrations: []Integration{}, NotGranted: []string{},
		MCP: []ToolServer{}, Policies: []Policy{}, Setup: []SetupStep{}}
	c := in.Company
	if c == nil {
		return s
	}
	role := roleOf(c, in.Handle)
	for _, lane := range seataccess.Lanes() {
		kind := lane.Kind()
		if !laneConnected(c, lane) || c.SeatAccessPending(string(kind)) {
			continue
		}
		want, present := seataccess.DesiredOf(role, lane)
		if !present || !want.Enabled {
			s.NotGranted = append(s.NotGranted, string(lane))
			continue
		}
		words, ok := in.Words[lane]
		if !ok {
			words = WordsOf(kind, lane)
		}
		s.Integrations = append(s.Integrations,
			integrationOf(c, in.Handle, lane, want, in.Observed[lane], words))
	}
	s.MCP = in.Tools
	s.Policies = policiesOf(c.Policies)
	s.Setup = setupOf(c, role)
	return s.normal()
}

// roleOf is the agent seat with handle, or nil.
func roleOf(c *config.Company, handle string) *config.Role {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return nil
	}
	for role := range c.EachRole() {
		if role.Kind != org.KindHuman && strings.TrimSpace(role.IdentityKey()) == handle {
			return role
		}
	}
	return nil
}

// laneConnected reports whether the company connects lane at all: Jira and
// Confluence each by its own block, every other lane by its kind's.
func laneConnected(c *config.Company, lane seataccess.Lane) bool {
	switch lane {
	case seataccess.LaneJira:
		return c.Integrations.Jira != nil
	case seataccess.LaneConfluence:
		return c.Integrations.Confluence != nil
	}
	return c.SeatAccessConnected(string(lane.Kind()))
}

// integrationOf is one granted lane.
func integrationOf(c *config.Company, handle string, lane seataccess.Lane, want seataccess.Desired,
	have seataccess.Observed, words Words) Integration {

	out := Integration{
		Kind: string(lane.Kind()), Lane: string(lane), On: have.Exists && have.Enabled,
		Level: want.Level, Scope: want.Scope,
		Tool: string(seataccess.ToolStateOf(c, handle, lane, want, have)),
	}
	out.LevelName, out.LevelNote = levelWords(want.Level, words.Levels)
	if len(words.Permissions) > 0 {
		out.PermissionNames = make(map[string]string, len(words.Permissions))
		for _, p := range words.Permissions {
			out.PermissionNames[p.Key] = p.Name
		}
	}
	var pending []string
	for key, access := range want.Permissions {
		effective := access
		if have.Ceiling != nil && have.Ceiling[key].Rank() < access.Rank() {
			effective = have.Ceiling[key]
			pending = append(pending, key+":"+string(access))
		}
		if effective != "" {
			if out.Permissions == nil {
				out.Permissions = map[string]string{}
			}
			out.Permissions[key] = string(effective)
		}
	}
	slices.Sort(pending)
	out.Pending = strings.Join(pending, ",")
	for _, entry := range want.Scope {
		if name := strings.TrimSpace(words.ScopeNames[entry]); name != "" && name != entry {
			if out.ScopeLabels == nil {
				out.ScopeLabels = map[string]string{}
			}
			out.ScopeLabels[entry] = name
		}
	}
	return out
}

// levelWords is a stored level's name and note in the vendor's words: the
// level it is, or the names of each level of a set; the id where the words
// hold none.
func levelWords(level string, levels []seataccess.Level) (name, note string) {
	if level == "" {
		return "", ""
	}
	if i := slices.IndexFunc(levels, func(l seataccess.Level) bool { return l.ID == level }); i >= 0 {
		return levels[i].Name, levels[i].Note
	}
	ids := strings.Fields(level)
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		i := slices.IndexFunc(levels, func(l seataccess.Level) bool { return l.ID == id })
		if i < 0 {
			return level, ""
		}
		names = append(names, levels[i].Name)
	}
	return strings.Join(names, ", "), ""
}

// toolServers is the seat's servers, each list sorted and deduplicated.
func toolServers(in []ToolServer) []ToolServer {
	byName := map[string]ToolServer{}
	for _, s := range in {
		name := strings.TrimSpace(s.Server)
		if name == "" {
			continue
		}
		prior := byName[name]
		byName[name] = ToolServer{Server: name,
			Allow:   sortedSet(append(prior.Allow, s.Allow...)),
			Approve: sortedSet(append(prior.Approve, s.Approve...)),
			Blocked: sortedSet(append(prior.Blocked, s.Blocked...)),
		}
	}
	out := make([]ToolServer, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[name])
	}
	return out
}

// policiesOf is the company's policies, each whole with its digest.
func policiesOf(policies []string) []Policy {
	out := []Policy{}
	for _, text := range policies {
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, Policy{Digest: digestOf([]byte(text)), Text: text})
		}
	}
	return out
}

// setupOf is the box setup the seat's runs get: the company's steps, then the
// seat's own.
func setupOf(c *config.Company, role *config.Role) []SetupStep {
	out := []SetupStep{}
	add := func(from string, steps []config.SandboxSetupStep) {
		for _, step := range steps {
			name := strings.TrimSpace(step.Name)
			if name == "" {
				continue
			}
			s := SetupStep{From: from, Name: name}
			if brief := strings.TrimSpace(step.Brief); brief != "" {
				s.Brief = digestOf([]byte(brief))
			}
			out = append(out, s)
		}
	}
	if c.Providers.Sandbox != nil {
		add(SetupFromCompany, c.Providers.Sandbox.Setup)
	}
	if role != nil && role.Sandbox != nil {
		add(SetupFromSeat, role.Sandbox.Setup)
	}
	return out
}

// normal is s with every list sorted, which is what makes it canonical.
func (s Snapshot) normal() Snapshot {
	s.Integrations = nonNil(s.Integrations)
	s.MCP = toolServers(s.MCP)
	s.Policies = nonNil(s.Policies)
	s.Setup = nonNil(s.Setup)
	slices.SortFunc(s.Integrations, func(a, b Integration) int { return strings.Compare(a.Lane, b.Lane) })
	for i := range s.Integrations {
		s.Integrations[i].Scope = sortedSet(s.Integrations[i].Scope)
	}
	s.NotGranted = sortedSet(s.NotGranted)
	slices.SortFunc(s.Policies, func(a, b Policy) int { return strings.Compare(a.Digest, b.Digest) })
	s.Policies = slices.CompactFunc(s.Policies, func(a, b Policy) bool { return a.Digest == b.Digest })
	slices.SortFunc(s.Setup, func(a, b SetupStep) int {
		if a.From != b.From {
			return strings.Compare(a.From, b.From)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return s
}

// nonNil is in, or an empty list for nil, so an absent list and an empty one
// encode alike.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// sortedSet is in trimmed, sorted and deduplicated, never nil.
func sortedSet(in []string) []string {
	out := []string{}
	for _, item := range in {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// JSON is the snapshot's canonical encoding: keys sorted at every level,
// lists sorted, no HTML escaping and no trailing newline.
func (s Snapshot) JSON() []byte {
	raw, err := json.Marshal(s.normal())
	if err != nil {
		// Every field is a string, a bool, a number or a map of strings.
		panic(fmt.Sprintf("capability: encode a snapshot: %v", err))
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		panic(fmt.Sprintf("capability: re-read a snapshot: %v", err))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		panic(fmt.Sprintf("capability: encode a snapshot canonically: %v", err))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// Digest is `sha256:` and the hex SHA-256 of [Snapshot.JSON].
func (s Snapshot) Digest() string { return digestOf(s.JSON()) }

// Parse reads a snapshot written by [Snapshot.JSON], as a told record holds it.
func Parse(data []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, fmt.Errorf("capability: read a snapshot: %w", err)
	}
	if s.V < 1 {
		return Snapshot{}, fmt.Errorf("capability: a snapshot carries version %d: every "+
			"snapshot states its version", s.V)
	}
	return s.normal(), nil
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
