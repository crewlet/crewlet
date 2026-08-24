package prompts

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// The reference company these tests build prompts for, carried from the
// Python suite's fixture so the assertions transfer one-for-one: a two-seat
// engineering team with a lead, a mission and vision, two policies, and a
// seat wired to two MCP servers.
func acme() *org.Organization {
	o := &org.Organization{
		Name:     "Acme",
		Mission:  "Build great things.",
		Vision:   "Be the best.",
		Policies: []string{"Respect teammates.", "No secrets in code."},
		Units: []*org.OrgUnit{{
			Name:         "Eng Team",
			Type:         org.UnitTypeTeam,
			Purpose:      "Build the thing.",
			Lead:         "Engineering Lead",
			Goals:        []string{"Ship v1.0."},
			SlackChannel: "C_ENG",
			Roles: []*org.Role{
				{
					Name:             "Engineering Lead",
					DeclaredHandle:   "lead",
					Goal:             "Lead the engineering team.",
					Responsibilities: []string{"Guide the team."},
					Manages:          []string{"Engineer"},
				},
				{
					Name:                 "Engineer",
					DeclaredHandle:       "eng",
					Goal:                 "Ship quality code.",
					Responsibilities:     []string{"Write tests."},
					BehavioralGuidelines: []string{"Be concise."},
					MCPEnv: org.MCPEnv{
						"atlassian": {"token": "x"},
						"github":    {"Authorization": "Bearer x"},
					},
				},
			},
		}},
	}
	o.Normalize()
	return o
}

// seatIn is the seat named in org o. It panics on a missing name because a
// fixture that names a seat the chart does not have is a broken test, not a
// case worth branching on.
func seatIn(o *org.Organization, name string) Seat {
	role := o.Role(name)
	if role == nil {
		panic("no such seat: " + name)
	}
	return Seat{Org: o, Role: role, Env: noEnv}
}

func engineer() Seat { return seatIn(acme(), "Engineer") }

func lead() Seat { return seatIn(acme(), "Engineering Lead") }

// noEnv resolves nothing. Every fixture uses it so a roster's contact
// identities never depend on the process environment the whole test binary
// shares — a ${VAR} in a fixture resolves the same on a laptop and in CI.
func noEnv(string) (string, bool) { return "", false }

// mixedAcme is the same company with a human seat holding the lead role —
// the shape the human-colleague sections switch on.
func mixedAcme() *org.Organization {
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.OrgUnit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "Sarah Chen",
			Roles: []*org.Role{
				{
					Name: "Sarah Chen",
					Kind: org.KindHuman,
					Contact: &org.HumanContact{
						SlackUserID:        "U0HUMAN",
						AtlassianAccountID: "5b10-s",
					},
					Availability: "CET business hours; replies within ~4h",
					Backstory:    "20 years in infrastructure.",
					Manages:      []string{"Engineer"},
				},
				{Name: "Engineer", DeclaredHandle: "eng", Goal: "Ship quality code."},
			},
		}},
	}
	o.Normalize()
	return o
}

// fakeCatalogue is a stand-in for the engine's tool-skill registry: the two
// methods prompt assembly needs, with the same trigger semantics (a skill
// fires when the surface carries its tool or its MCP server) and the same
// phase scoping.
//
// It answers in INSERTION order on purpose. The real registry sorts by key,
// and a fake that also sorted would certify nothing about the sort this
// package does for itself.
type fakeCatalogue struct {
	skills []fakeSkill
	vars   map[string]string
}

type fakeSkill struct {
	key       string
	summary   string
	body      string
	required  bool
	tool      string
	mcpServer string
	phases    []Phase
}

func (c *fakeCatalogue) SkillsFor(phase Phase, surface Surface) []Skill {
	var out []Skill
	for _, s := range c.skills {
		phases := s.phases
		if len(phases) == 0 {
			phases = []Phase{PhasePlan} // the registry's default
		}
		if !slices.Contains(phases, phase) {
			continue
		}
		fires := (s.tool != "" && slices.Contains(surface.Tools, s.tool)) ||
			(s.mcpServer != "" && slices.Contains(surface.MCPServers, s.mcpServer))
		if !fires {
			continue
		}
		out = append(out, Skill{Key: s.key, Summary: s.summary, Required: s.required})
	}
	return out
}

func (c *fakeCatalogue) Render(text string) string {
	for name, value := range c.vars {
		text = strings.ReplaceAll(text, "${"+name+"}", value)
	}
	return text
}
