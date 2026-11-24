package config

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// LLM FLEETS (ADR "LLM fleets"): a named set of providers.llm entries of one
// model class that a seat job or helper runs on as `fleet:<key>`, the engine
// picking the member with room for each new piece of work.

// LLMFleet is one entry under providers.llm_fleets.
type LLMFleet struct {
	// Name is what a person reads; the key never changes, a rename changes this.
	Name string `yaml:"name" json:"name" js:"required;maxlen=60" desc:"What the fleet is called: 1 to 60 characters, unique across fleets ignoring case. default-fleet's name is its key."`
	// Strategy is how new work is spread; empty is balanced.
	Strategy LLMFleetStrategy `yaml:"strategy,omitempty" json:"strategy,omitempty" js:"enum=balanced|sequential" desc:"How new work is spread across the members: balanced (default) keeps every member at a similar share of its limits, plans first; sequential fills the first member until it nears its limit, then the next."`
	// Members are providers.llm keys of one model class and harness; their
	// order is the sequential order.
	Members []string `yaml:"members,omitempty" json:"members,omitempty" desc:"providers.llm keys of one model class on one harness, each once; the order is the sequential order. Empty is valid: a seat on the fleet waits until one joins."`
	// ThresholdPoints is how far, in whole percentage points of usage, a
	// balanced fleet lets a seat job stay on the member it last ran on.
	ThresholdPoints int `yaml:"threshold_points,omitempty" json:"threshold_points,omitempty" js:"min=0;max=50" desc:"Balanced only: how many points of usage above the least-used member a seat job may stay on the member it last ran on, for its cache: 1 to 50. Absent: 5."`
}

// LLMFleetStrategy is how a fleet spreads new work across its members.
type LLMFleetStrategy string

const (
	// FleetBalanced keeps every member at a similar share, plans before keys.
	FleetBalanced LLMFleetStrategy = "balanced"
	// FleetSequential fills the members in list order.
	FleetSequential LLMFleetStrategy = "sequential"
)

// LLMFleetStrategies is the closed set.
var LLMFleetStrategies = []LLMFleetStrategy{FleetBalanced, FleetSequential}

// Valid reports whether s is one of [LLMFleetStrategies]; the caller reads
// empty as balanced first.
func (s LLMFleetStrategy) Valid() bool { return slices.Contains(LLMFleetStrategies, s) }

const (
	// DefaultFleetKey is the fleet every company has: synthesised until written,
	// never deleted, never renamed.
	DefaultFleetKey = "default-fleet"
	// FleetRefPrefix starts a reference to a fleet where one entry key is
	// accepted; a key has no `:` ([ProviderKeyPattern]), so the two never meet.
	FleetRefPrefix = "fleet:"
	// DefaultFleetThreshold is threshold_points when unset.
	DefaultFleetThreshold = 5
	// MaxFleetThreshold bounds threshold_points.
	MaxFleetThreshold = 50
	// MaxFleetNameLen bounds a fleet's name, in characters.
	MaxFleetNameLen = 60
	// MaxFleetKeyLen bounds a fleet's key: a DNS label, as the operator names
	// a resource after it.
	MaxFleetKeyLen = 63
)

// FleetKeyPattern is the grammar of a providers.llm_fleets key, a DNS label.
// Compiled at first use, as [ProviderKeyPattern] is.
var FleetKeyPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
})

// ValidFleetKey reports whether key is in [FleetKeyPattern] and within
// [MaxFleetKeyLen].
func ValidFleetKey(key string) bool {
	return len(key) <= MaxFleetKeyLen && FleetKeyPattern().MatchString(key)
}

// FleetRef is the reference a seat job or helper writes to run on fleet key.
func FleetRef(key string) string { return FleetRefPrefix + key }

// FleetOf is the fleet key a reference names, and whether it names one.
func FleetOf(ref string) (string, bool) { return strings.CutPrefix(ref, FleetRefPrefix) }

// EffectiveStrategy is the fleet's strategy, balanced when unset.
func (f LLMFleet) EffectiveStrategy() LLMFleetStrategy {
	if f.Strategy == "" {
		return FleetBalanced
	}
	return f.Strategy
}

// Threshold is threshold_points, [DefaultFleetThreshold] when unset.
func (f LLMFleet) Threshold() int {
	if f.ThresholdPoints == 0 {
		return DefaultFleetThreshold
	}
	return f.ThresholdPoints
}

// defaultFleet is default-fleet as synthesised before anyone writes it.
func defaultFleet() LLMFleet {
	return LLMFleet{Name: DefaultFleetKey, Strategy: FleetBalanced}
}

// Fleets is providers.llm_fleets as every reading and validation sees it:
// default-fleet included, synthesised while the document does not hold it.
// The caller may modify the map but not the members' slices.
func (p *Providers) Fleets() map[string]LLMFleet {
	out := maps.Clone(p.LLMFleets)
	if out == nil {
		out = make(map[string]LLMFleet, 1)
	}
	if _, ok := out[DefaultFleetKey]; !ok {
		out[DefaultFleetKey] = defaultFleet()
	}
	return out
}

// Fleet is the fleet under key, default-fleet synthesised, and whether there
// is one.
func (p *Providers) Fleet(key string) (LLMFleet, bool) {
	if f, ok := p.LLMFleets[key]; ok {
		return f, true
	}
	if key == DefaultFleetKey {
		return defaultFleet(), true
	}
	return LLMFleet{}, false
}

// FleetKeys is every fleet's key, default-fleet included, sorted.
func (p *Providers) FleetKeys() []string {
	return slices.Sorted(maps.Keys(p.Fleets()))
}

// DefaultFleetWritten reports whether the document holds default-fleet, which
// it does only once a person changed it.
func (p *Providers) DefaultFleetWritten() bool {
	_, ok := p.LLMFleets[DefaultFleetKey]
	return ok
}

// SuggestFleetKey is a key made from name as a create makes one: lowercased,
// every run of other characters a `-`, trimmed, then `-2`, `-3` and on while
// taken says the key is in use.
func SuggestFleetKey(name string, taken func(string) bool) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	base := b.String()
	if base == "" {
		base = "fleet"
	}
	key := trimFleetKey(base, MaxFleetKeyLen)
	for n := 2; taken != nil && taken(key); n++ {
		suffix := "-" + strconv.Itoa(n)
		key = trimFleetKey(base, MaxFleetKeyLen-len(suffix)) + suffix
	}
	return key
}

// trimFleetKey cuts key to n bytes without leaving a trailing `-`.
func trimFleetKey(key string, n int) string {
	if len(key) > n {
		key = key[:n]
	}
	return strings.TrimRight(key, "-")
}

// ModelClass is what a fleet holds one of: the model family a person picks
// by, read from the entry's model id, else the id itself.
func ModelClass(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return "Claude Opus"
	case strings.Contains(m, "sonnet"):
		return "Claude Sonnet"
	case strings.Contains(m, "haiku"):
		return "Claude Haiku"
	case strings.Contains(m, "codex"):
		return "GPT-5 Codex"
	}
	return model
}
