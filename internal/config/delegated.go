package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// DelegatedDecisions is which requests agent seats decide in a person's place, within
// the engine's rules (ADR-0125, internal/delegated). Absent or disabled, every
// request waits on a person as it always did; turning it off sends every
// request still with a delegate to people.
type DelegatedDecisions struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn delegated decisions on. Off puts every request to people at once, and keeps the rules."`

	Rules []DelegatedRule `yaml:"rules,omitempty" json:"rules,omitempty" desc:"Which kinds of request which agent seats decide, and who an escalation is put to. A kind is in at most one rule. At most 16."`
}

// DelegatedRule delegates some kinds of request to one agent seat, or to a
// panel of two that must both allow.
type DelegatedRule struct {
	Kinds []delegated.Kind `yaml:"kinds" json:"kinds" js:"required"`

	Delegates []string `yaml:"delegates" json:"delegates" js:"required" desc:"The agent seat that decides, or two for a panel that must both allow (any disagreement escalates); a panel's two run on different first llm entries. A delegate of server commands holds no SSH access, and a request a delegate made goes to people."`

	EscalateTo []string `yaml:"escalate_to" json:"escalate_to" js:"required" desc:"The people an escalation is put to, beside administrators: at least one. They read what was decided for them, and may approve what the rule's kinds ask as approvers do."`

	SampleOneIn int `yaml:"sample_one_in,omitempty" json:"sample_one_in,omitempty" desc:"One decision in this many is shown to the escalate_to people for a spot-check after the fact: 1 to 1000. Empty is 10."`

	AlwaysEscalatePaths []string `yaml:"always_escalate_paths,omitempty" json:"always_escalate_paths,omitempty" desc:"Repository path globs (docs/**, services/*/billing/**) a merge or deploy touching which goes to people, beside the built-in list: only for a rule of pr_merge or deploy. At most 64, each at most 128 bytes."`
}

// fieldDescriptions is the schema's description of the rule's kinds, which
// name a tool a `desc` tag cannot read from its constant.
func (DelegatedRule) fieldDescriptions() map[string]string {
	return map[string]string{
		"kinds": "The kinds of request this rule delegates: " + string(delegated.KindSSHOperate) +
			" (an agent's command on a server as its operate user), " + string(delegated.KindPRMerge) +
			" (merging a change at the head it was asked about, " + toolname.AskToMerge + ") and " +
			string(delegated.KindDeploy) + " (deploying a commit that holds merged changes, " +
			toolname.AskToDeploy + "). " +
			string(delegated.KindOther) + " is never delegated.",
	}
}

// The bounds of the block.
const (
	delegatedMaxRules       = 16
	delegatedMaxEscalateTo  = 32
	delegatedMaxSampleOneIn = 1000
	// delegatedMaxDelegates is a panel: two seats on two models.
	delegatedMaxDelegates = 2
	// delegatedMaxPaths bounds a rule's own globs, the SSH list's bound: a
	// company's sensitive places are a handful of directories, not a tree.
	delegatedMaxPaths = 64
)

// SampleRate is one in how many of the rule's decisions are spot-checked.
func (r DelegatedRule) SampleRate() int {
	if r.SampleOneIn == 0 {
		return delegated.DefaultSampleOneIn
	}
	return r.SampleOneIn
}

// RuleFor is the rule that delegates kind, by its 1-based number, and false
// where delegation is off or no rule names kind.
func (d DelegatedDecisions) RuleFor(kind delegated.Kind) (DelegatedRule, int, bool) {
	if !d.Enabled || !kind.Delegable() {
		return DelegatedRule{}, 0, false
	}
	for i, r := range d.Rules {
		if slices.Contains(r.Kinds, kind) {
			return r, i + 1, true
		}
	}
	return DelegatedRule{}, 0, false
}

// DelegateSeats is every seat an enabled rule names a delegate, each once.
func (d DelegatedDecisions) DelegateSeats() []string {
	if !d.Enabled {
		return nil
	}
	var out []string
	for _, r := range d.Rules {
		for _, h := range r.Delegates {
			if !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}

// EscalationPeople is every person a rule names in escalate_to, each once,
// whether or not delegation is on: they read what delegates decided.
func (d DelegatedDecisions) EscalationPeople() []string {
	var out []string
	for _, r := range d.Rules {
		for _, h := range r.EscalateTo {
			if !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}

// SSHMayApprove is every person, beside administrators, who may approve an
// operate command: the approvers, then the people the ssh_operate rule puts an
// escalation to.
func (c *Company) SSHMayApprove() []string {
	if c == nil {
		return nil
	}
	var out []string
	if s := c.Integrations.SSH; s != nil {
		out = slices.Clone(s.Approvers)
	}
	for _, h := range c.DelegatedDecisions.EscalateFor(delegated.KindSSHOperate) {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// EscalateFor is the people the rule naming kind puts an escalation to,
// whether or not delegation is on: they may approve the kind's requests.
func (d DelegatedDecisions) EscalateFor(kind delegated.Kind) []string {
	for _, r := range d.Rules {
		if slices.Contains(r.Kinds, kind) {
			return slices.Clone(r.EscalateTo)
		}
	}
	return nil
}

// validateDelegatedDecisions holds every rule to delegable kinds each in one rule,
// agent delegates that cannot request what they decide (a panel's two on two
// models), and at least one person to escalate to.
//
// AT LEAST ONE PERSON, never "administrators will do": who administers the
// company is the identity directory's, which changes without a revision, so a
// check of it here would not hold after the write.
func (c *Company) validateDelegatedDecisions(o *org.Organization) error {
	d := c.DelegatedDecisions
	var p problems
	path := field("delegated_decisions")
	if n := len(d.Rules); n > delegatedMaxRules {
		p.add(at(path, "rules"), ErrOutOfRange, "%d rules is more than %d", n, delegatedMaxRules)
	}
	claimed := map[delegated.Kind]int{}
	for i, r := range d.Rules {
		here := idx(at(path, "rules"), i)
		if len(r.Kinds) == 0 {
			p.add(at(here, "kinds"), ErrMissing, "name the kinds this rule delegates: %s",
				words.Or(delegated.DelegableKinds()))
		}
		for j, k := range r.Kinds {
			switch prior, taken := claimed[k]; {
			case !k.Delegable():
				p.add(idx(at(here, "kinds"), j), ErrUnknownValue, "%q is not a kind a rule delegates; give %s",
					k, words.Or(delegated.DelegableKinds()))
			case taken:
				p.add(idx(at(here, "kinds"), j), ErrConflict, "%s is already delegated by rule %d; a kind "+
					"is in one rule", k, prior)
			default:
				claimed[k] = i + 1
			}
		}
		c.checkDelegates(&p, at(here, "delegates"), o, r)
		switch n := len(r.EscalateTo); {
		case n == 0:
			p.add(at(here, "escalate_to"), ErrMissing, "name at least one person an escalation is put to; "+
				"administrators are added to them")
		case n > delegatedMaxEscalateTo:
			p.add(at(here, "escalate_to"), ErrOutOfRange, "%d people is more than %d", n, delegatedMaxEscalateTo)
		}
		seen := map[string]bool{}
		for j, handle := range r.EscalateTo {
			switch role := o.Role(handle); {
			case role == nil || !role.IsHuman():
				p.add(idx(at(here, "escalate_to"), j), ErrUnknownValue, "%q is not a person's seat; only "+
					"people answer an escalation", handle)
			case seen[handle]:
				p.add(idx(at(here, "escalate_to"), j), ErrConflict, "%q is listed twice", handle)
			}
			seen[handle] = true
		}
		if s := r.SampleOneIn; s < 0 || s > delegatedMaxSampleOneIn {
			p.add(at(here, "sample_one_in"), ErrOutOfRange, "%d is outside 1 to %d; leave it empty for %d",
				s, delegatedMaxSampleOneIn, delegated.DefaultSampleOneIn)
		}
		checkRulePaths(&p, at(here, "always_escalate_paths"), r)
	}
	return p.err()
}

// checkDelegates holds a rule's delegates to one agent seat or a panel of two
// on different models, none of which can request a kind the rule delegates.
func (c *Company) checkDelegates(p *problems, path Path, o *org.Organization, r DelegatedRule) {
	switch n := len(r.Delegates); {
	case n == 0:
		p.add(path, ErrMissing, "name the agent seat that decides, or two for a panel")
		return
	case n > delegatedMaxDelegates:
		p.add(path, ErrOutOfRange, "%d delegates is more than a panel of %d", n, delegatedMaxDelegates)
		return
	}
	var seats []*org.Role
	for i, handle := range r.Delegates {
		here := idx(path, i)
		role := o.Role(handle)
		switch {
		case role == nil:
			p.add(here, ErrUnknownValue, "%q is not a seat of this company", handle)
			continue
		case role.IsHuman():
			p.add(here, ErrUnknownValue, "%q is a person's seat; a delegate is an agent seat, and a person "+
				"decides through escalate_to", handle)
			continue
		case slices.Contains(seats, role):
			p.add(here, ErrConflict, "%q is listed twice; a panel is two different seats", handle)
			continue
		}
		for _, k := range r.Kinds {
			if why := c.requests(handle, k); why != "" {
				p.add(here, ErrConflict, "%q %s, so it cannot decide %s requests: a delegate never decides "+
					"a kind it can request", handle, why, k)
			}
		}
		seats = append(seats, role)
	}
	if len(seats) == 2 {
		first, second := firstLLM(seats[0]), firstLLM(seats[1])
		if first == "" || second == "" || first == second {
			p.add(path, ErrConflict, "a panel's two delegates run on different models: give %s and %s "+
				"different first llm entries", seats[0].Handle(), seats[1].Handle())
		}
	}
}

// requests is how handle can request kind, "" where it cannot.
func (c *Company) requests(handle string, kind delegated.Kind) string {
	if kind != delegated.KindSSHOperate {
		return ""
	}
	for role := range c.EachRole() {
		if role.IdentityKey() == handle && role.Integrations.SSH != nil {
			return "holds SSH access (roles[].integrations.ssh)"
		}
	}
	return ""
}

// firstLLM is the first providers.llm entry a seat's executor runs on, "" for
// a seat that names none.
func firstLLM(r *org.Role) string {
	if len(r.LLM) == 0 {
		return ""
	}
	return strings.TrimSpace(r.LLM[0])
}

// checkRulePaths holds a rule's own always-escalate globs to a rule that
// delegates a merge or a deploy, and each to a path glob a repository's paths
// are matched by (gitref.ParsePathGlob).
func checkRulePaths(p *problems, path Path, r DelegatedRule) {
	if len(r.AlwaysEscalatePaths) == 0 {
		return
	}
	if !slices.Contains(r.Kinds, delegated.KindPRMerge) && !slices.Contains(r.Kinds, delegated.KindDeploy) {
		p.add(path, ErrConflict, "paths escalate a merge or a deploy; this rule delegates neither %s nor %s",
			delegated.KindPRMerge, delegated.KindDeploy)
		return
	}
	if n := len(r.AlwaysEscalatePaths); n > delegatedMaxPaths {
		p.add(path, ErrOutOfRange, "%d paths is more than %d", n, delegatedMaxPaths)
		return
	}
	for i, glob := range r.AlwaysEscalatePaths {
		if _, err := gitref.ParsePathGlob(glob); err != nil {
			p.add(idx(path, i), ErrUnknownValue, "%q is not a path glob: %s", glob, err.Error())
		}
	}
}
