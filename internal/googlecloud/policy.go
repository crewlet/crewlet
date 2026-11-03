package googlecloud

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// Policy is an allow policy as Google reads and writes it. AuditConfigs ride
// through a write untouched.
type Policy struct {
	Version      int             `json:"version,omitempty"`
	Bindings     []Binding       `json:"bindings,omitempty"`
	Etag         string          `json:"etag,omitempty"`
	AuditConfigs json.RawMessage `json:"auditConfigs,omitempty"`
}

// Binding is one role granted to members, under an optional condition.
type Binding struct {
	Role      string     `json:"role"`
	Members   []string   `json:"members"`
	Condition *Condition `json:"condition,omitempty"`
}

// Condition is a binding's IAM condition.
type Condition struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// held is what a policy grants one member: the roles of its unconditional
// bindings, and those of conditional ones, each sorted once.
type held struct {
	plain, conditional []string
}

// all is every role member holds, by either kind of binding.
func (h held) all() []string {
	out := slices.Concat(h.plain, h.conditional)
	slices.Sort(out)
	return slices.Compact(out)
}

// heldBy is what pol grants who.
func heldBy(pol Policy, who string) held {
	var h held
	for _, b := range pol.Bindings {
		if !slices.Contains(b.Members, who) {
			continue
		}
		if b.Condition == nil {
			h.plain = append(h.plain, b.Role)
		} else {
			h.conditional = append(h.conditional, b.Role)
		}
	}
	slices.Sort(h.plain)
	slices.Sort(h.conditional)
	h.plain, h.conditional = slices.Compact(h.plain), slices.Compact(h.conditional)
	return h
}

// grant adds who to the unconditional binding of each role, making the
// binding where there is none, and reports whether pol changed. Another
// member's binding and every conditional binding stay as they were.
func grant(pol *Policy, who string, roles []string) bool {
	changed := false
	for _, role := range roles {
		i := slices.IndexFunc(pol.Bindings, func(b Binding) bool { return b.Role == role && b.Condition == nil })
		switch {
		case i < 0:
			pol.Bindings = append(pol.Bindings, Binding{Role: role, Members: []string{who}})
			changed = true
		case !slices.Contains(pol.Bindings[i].Members, who):
			pol.Bindings[i].Members = append(pol.Bindings[i].Members, who)
			changed = true
		}
	}
	return changed
}

// revoke takes who out of every binding of each role, conditional ones
// included, drops a binding left with no member, and reports whether pol
// changed.
func revoke(pol *Policy, who string, roles []string) bool {
	changed := false
	out := pol.Bindings[:0]
	for _, b := range pol.Bindings {
		if slices.Contains(roles, b.Role) && slices.Contains(b.Members, who) {
			b.Members = slices.DeleteFunc(slices.Clone(b.Members), func(m string) bool { return m == who })
			changed = true
		}
		if len(b.Members) > 0 {
			out = append(out, b)
		}
	}
	pol.Bindings = out
	return changed
}

// levelCondition is the one shape of a level's condition this package trusts:
// a single hasOnly over string constants and nothing joined to it. A condition
// with more in it (`|| true`) could grant anything, so it is never read as a
// level's list.
var (
	levelCondition = regexp.MustCompile(`^api\.getAttribute\(\s*'iam\.googleapis\.com/modifiedGrantsByRole'\s*,\s*\[\s*\]\s*\)` +
		`\.hasOnly\(\s*\[([^\]]*)\]\s*\)$`)
	listedRole = regexp.MustCompile(`^(?:'([^'\\]+)'|"([^"\\]+)")$`)
)

// conditionRoles is the roles a level condition lists, sorted, and false for
// an expression of any other shape.
func conditionRoles(expression string) ([]string, bool) {
	m := levelCondition.FindStringSubmatch(strings.TrimSpace(expression))
	if m == nil {
		return nil, false
	}
	var out []string
	for _, item := range strings.Split(m[1], ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		q := listedRole.FindStringSubmatch(item)
		if q == nil {
			return nil, false
		}
		out = append(out, q[1]+q[2])
	}
	if len(out) == 0 {
		return nil, false
	}
	slices.Sort(out)
	return out, true
}

// adminRoles are the roles that let their holder grant anything on a project:
// with no condition, the provisioner holding one is not contained there.
//
//nolint:gochecknoglobals // an immutable list, not state
var adminRoles = []string{
	"roles/owner", "roles/editor", "roles/iam.securityAdmin", "roles/resourcemanager.projectIamAdmin",
}

// containment is what a project's policy grants the provisioner: each level
// list its conditional Project IAM Admin bindings hold, the admin roles it
// holds with no condition, and the conditions of any other shape.
type containment struct {
	lists         [][]string
	unconditional []string
	malformed     []string
}

// containmentOf reads the provisioner's grants on one project's policy.
func containmentOf(pol Policy, provisioner string) containment {
	var c containment
	for _, b := range pol.Bindings {
		if !slices.Contains(b.Members, provisioner) {
			continue
		}
		if b.Condition == nil {
			if slices.Contains(adminRoles, b.Role) {
				c.unconditional = append(c.unconditional, b.Role)
			}
			continue
		}
		if b.Role != "roles/resourcemanager.projectIamAdmin" {
			continue
		}
		if roles, ok := conditionRoles(b.Condition.Expression); ok {
			c.lists = append(c.lists, roles)
		} else {
			c.malformed = append(c.malformed, b.Condition.Expression)
		}
	}
	slices.Sort(c.unconditional)
	c.unconditional = slices.Compact(c.unconditional)
	return c
}

// ours reports a project the owner has granted the provisioner anything that
// administers it: the company's, set up or half set up.
func (c containment) ours() bool {
	return len(c.lists) > 0 || len(c.unconditional) > 0 || len(c.malformed) > 0
}

// holds reports a conditional binding listing exactly roles.
func (c containment) holds(roles []string) bool {
	want := slices.Sorted(slices.Values(roles))
	return slices.ContainsFunc(c.lists, func(l []string) bool { return slices.Equal(l, want) })
}
