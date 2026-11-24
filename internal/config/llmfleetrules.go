package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// fleetPath is providers.llm_fleets.<key>.
func fleetPath(key string) Path { return entry(field("providers.llm_fleets"), key) }

// validateLLMFleets holds every fleet's shape: its key, a name unique ignoring
// case, its strategy and threshold, and members that are entries of one class
// on one harness, each once. ADMISSION, beside [Company.validateLLMKeys]: a
// stored revision breaking one still routes, its unknown members skipped.
func (c *Company) validateLLMFleets() error {
	var p problems
	fleets := c.Providers.Fleets()
	named := make(map[string]string, len(fleets))
	for _, key := range slices.Sorted(maps.Keys(fleets)) {
		f, path := fleets[key], fleetPath(key)
		if !ValidFleetKey(key) {
			p.addSaying(path, ErrShape, fmt.Sprintf("%s is not a fleet key: use lowercase letters, "+
				"digits and hyphens, e.g. %s.", key, SuggestFleetKey(key, nil)),
				"%q is not a fleet key: lowercase letters, digits and '-', starting and ending with "+
					"a letter or a digit, at most %d characters (%s), e.g. %q",
				key, MaxFleetKeyLen, FleetKeyPattern(), SuggestFleetKey(key, nil))
			continue
		}
		name := strings.TrimSpace(f.Name)
		switch {
		case name == "":
			p.addSaying(at(path, "name"), ErrMissing, "Give the fleet a name.",
				"name the fleet: every picker and status shows it by its name")
		case key == DefaultFleetKey && f.Name != DefaultFleetKey:
			p.addSaying(at(path, "name"), ErrConflict, "The default fleet can't be renamed.",
				"default-fleet's name is its key, %q, and cannot change", DefaultFleetKey)
		case utf8.RuneCountInString(name) > MaxFleetNameLen:
			p.addSaying(at(path, "name"), ErrOutOfRange,
				fmt.Sprintf("Use at most %d characters.", MaxFleetNameLen),
				"a fleet's name is at most %d characters, this is %d",
				MaxFleetNameLen, utf8.RuneCountInString(name))
		default:
			fold := strings.ToLower(name)
			if other, ok := named[fold]; ok {
				p.addSaying(at(path, "name"), ErrConflict,
					fmt.Sprintf("Another fleet is called %s.", fleets[other].Name),
					"%q is also the name of providers.llm_fleets.%s, ignoring case: a person picks "+
						"a fleet by its name, so give each its own", name, other)
				break
			}
			named[fold] = key
		}
		if f.Strategy != "" && !f.Strategy.Valid() {
			p.add(at(path, "strategy"), ErrUnknownValue, "%q is not a strategy: use %s",
				f.Strategy, words.Or(LLMFleetStrategies))
		}
		if t := f.ThresholdPoints; t < 0 || t > MaxFleetThreshold {
			p.add(at(path, "threshold_points"), ErrOutOfRange,
				"must be 1 to %d points, got %d (absent: %d)", MaxFleetThreshold, t, DefaultFleetThreshold)
		}
		p.wrap(c.validateFleetMembers(path, f))
	}
	return p.err()
}

// validateFleetMembers holds a fleet's members to entries of one class on one
// harness, each listed once; the first member sets the class.
func (c *Company) validateFleetMembers(path Path, f LLMFleet) error {
	var p problems
	seen := make(map[string]bool, len(f.Members))
	first := ""
	for i, key := range f.Members {
		at := idx(at(path, "members"), i)
		if seen[key] {
			p.addSaying(at, ErrConflict, key+" is listed twice",
				"%q is a member already: list each entry once", key)
			continue
		}
		seen[key] = true
		if _, ok := c.Providers.LLM[key]; !ok {
			p.addSaying(at, ErrUnknownValue, key+" is not a model under Models",
				"%q is not a providers.llm key: a member is one of %s", key,
				words.Or(slices.Sorted(maps.Keys(c.Providers.LLM))))
			continue
		}
		if first == "" {
			first = key
			continue
		}
		if runs, fleet := c.fleetClasses(key, first); runs != fleet {
			p.addSaying(at, ErrConflict, fmt.Sprintf("Runs %s; this fleet runs %s.", runs, fleet),
				"%q runs %s, and the fleet's first member, %q, runs %s: a fleet's members run one "+
					"model class on one harness, so any member can take any of its seats' work",
				key, runs, first, fleet)
		}
	}
	return p.err()
}

// fleetClasses is the class entries a and b run, as a person tells them apart:
// the harness is named beside the class only where the two differ in it.
func (c *Company) fleetClasses(a, b string) (string, string) {
	ea, eb := c.Providers.LLM[a], c.Providers.LLM[b]
	ca, cb := ModelClass(ea.Model), ModelClass(eb.Model)
	if ea.HarnessName() != eb.HarnessName() {
		return ca + " on " + ea.HarnessName(), cb + " on " + eb.HarnessName()
	}
	return ca, cb
}

// FleetClass is the class fleet key runs, its first member's, and "" for a
// fleet with no member under providers.llm.
func (c *Company) FleetClass(key string) string {
	f, _ := c.Providers.Fleet(key)
	for _, member := range f.Members {
		if entry, ok := c.Providers.LLM[member]; ok {
			return ModelClass(entry.Model)
		}
	}
	return ""
}

// fleetReferenceProblem holds a written chain naming a fleet: alone, and a
// fleet the company has. It reports whether the chain names one at all.
func (c *Company) fleetReferenceProblem(p *problems, path Path, chain []string) bool {
	if !slices.ContainsFunc(chain, isFleetRef) {
		return false
	}
	if len(chain) > 1 {
		p.addSaying(path, ErrConflict, "A fleet stands alone: choose the fleet or a list of models",
			"%s names a fleet inside a list: a fleet replaces a hand-ordered fallback list, so "+
				"write fleet:<key> alone, or list providers.llm keys only", strings.Join(chain, ", "))
		return true
	}
	key, _ := FleetOf(chain[0])
	if _, ok := c.Providers.Fleet(key); !ok {
		p.addSaying(path, ErrUnknownValue, key+" is not a fleet",
			"%q names no fleet: providers.llm_fleets has %s", chain[0], words.Or(c.Providers.FleetKeys()))
	}
	return true
}

// isFleetRef reports whether one chain member is a fleet reference.
func isFleetRef(key string) bool {
	_, ok := FleetOf(key)
	return ok
}

// fleetOfChain is the fleet a written chain runs on alone, and whether it
// names one the company has.
func (c *Company) fleetOfChain(chain []string) (string, LLMFleet, bool) {
	if len(chain) != 1 {
		return "", LLMFleet{}, false
	}
	key, ok := FleetOf(chain[0])
	if !ok {
		return "", LLMFleet{}, false
	}
	f, ok := c.Providers.Fleet(key)
	return key, f, ok
}

// FleetMembers is the providers.llm keys a seat job's or helper's model runs
// on: a fleet's members the company has, in their order, or the key itself.
func (c *Company) FleetMembers(ref string) []string {
	key, ok := FleetOf(ref)
	if !ok {
		return []string{ref}
	}
	f, _ := c.Providers.Fleet(key)
	out := make([]string, 0, len(f.Members))
	for _, member := range f.Members {
		if _, ok := c.Providers.LLM[member]; ok {
			out = append(out, member)
		}
	}
	return out
}

// emptyFleetAdvice is [Company.emptyFleetWarnings]' words.
var emptyFleetAdvice = says("Seats on a fleet with no members",
	"These wait until a model joins the fleet. Add a member to it, or pin them to a model.")

// emptyFleetWarnings is one warning per seat or helper with a job on a fleet
// that has no members: valid, and it waits until one joins.
func (c *Company) emptyFleetWarnings() []Warning {
	var out []Warning
	warned := map[[2]string]bool{}
	for _, ref := range c.providerReferences() {
		if ref.Fleet != "" {
			continue
		}
		key, f, ok := c.fleetOfChain(ref.Chain)
		if !ok || len(c.FleetMembers(FleetRef(key))) > 0 {
			continue
		}
		who := c.referenceSubject(ref)
		if warned[[2]string{who, key}] {
			continue
		}
		warned[[2]string{who, key}] = true
		w := emptyFleetAdvice.raise(ref.Segments, fmt.Sprintf("%s has no members yet: %s waits until one joins",
			f.Name, who))
		w.Seat, w.Item = ref.Seat, f.Name
		out = append(out, w)
	}
	return out
}

// referenceSubject is who a written reference's job belongs to, by name.
func (c *Company) referenceSubject(ref ProviderReference) string {
	switch ref.Job {
	case JobAssistant:
		return "Ask Crewlet"
	case JobAdvisor:
		return "Advisor"
	}
	for role := range c.EachRole() {
		if role.IdentityKey() == ref.Seat && role.Name != "" {
			return role.Name
		}
	}
	return ref.Seat
}
