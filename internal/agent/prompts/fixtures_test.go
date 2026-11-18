package prompts

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// The reference company: a two-seat team with a lead, mission, vision, two
// policies and a seat wired to two MCP servers. Every prompt assertion here is
// written against it, so changing it moves every expected string at once.
func acme() *org.Organization {
	o := &org.Organization{
		Name:     "Acme",
		Mission:  "Build great things.",
		Vision:   "Be the best.",
		Policies: []string{"Respect teammates.", "No secrets in code."},
		Units: []*org.Unit{{
			Name:    "Eng Team",
			Type:    org.UnitTypeTeam,
			Purpose: "Build the thing.",
			Lead:    "lead",
			Goals:   []string{"Ship v1.0."},
			Channel: "C_ENG",
			Roles: []*org.Role{
				{
					Name:             "Engineering Lead",
					DeclaredHandle:   "lead",
					Goal:             "Lead the engineering team.",
					Responsibilities: []string{"Guide the team."},
					Manages:          []string{"eng"},
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

// seatIn is the seat with this HANDLE in org o (what [org.Organization.Role]
// resolves; a display name gets renamed). It panics on a missing one: that is
// a broken fixture, not a case.
func seatIn(o *org.Organization, handle string) Seat {
	role := o.Role(handle)
	if role == nil {
		panic("no such seat: " + handle)
	}
	return Seat{Org: o, Role: role, Env: noEnv}
}

func engineer() Seat { return seatIn(acme(), "eng") }

func lead() Seat { return seatIn(acme(), "lead") }

// noEnv resolves nothing, so a fixture's ${VAR} never depends on the test
// binary's shared environment and resolves the same on a laptop and in CI.
func noEnv(string) (string, bool) { return "", false }

// mixedAcme is the same company with a human seat holding the lead role —
// the shape the human-colleague sections switch on.
func mixedAcme() *org.Organization {
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name: "Eng Team",
			Type: org.UnitTypeTeam,
			Lead: "sarah-chen",
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
					Manages:      []string{"eng"},
				},
				{Name: "Engineer", DeclaredHandle: "eng", Goal: "Ship quality code."},
			},
		}},
	}
	o.Normalize()
	return o
}

// fakeCatalogue stands in for the tool-skill registry, with its trigger
// semantics (a skill fires when the surface carries its tool or MCP server)
// and phase scoping.
//
// It answers in INSERTION order on purpose: the real registry sorts, and a
// fake that also sorted would certify nothing about this package's own sort.
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
			phases = []Phase{PhaseExecute} // the registry's default
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

// profiledReports is a lead with n direct reports, each with a realistic
// profile, every other one human so both roster renderings are exercised.
//
// IT IS THE FIXTURE THE ROSTER ALLOWANCE WAS MEASURED AGAINST:
// rosterAllowanceTokens' per-profile figures and every roster number in
// budget_test.go are measurements of this prose, so editing it moves them all.
func profiledReports(n int) *org.Organization {
	lead := &org.Role{
		Name:           "Engineering Lead",
		DeclaredHandle: "lead",
		Goal:           "Lead the engineering team.",
	}
	roles := []*org.Role{lead}
	for i := range n {
		name := fmt.Sprintf("Report %03d", i)
		report := &org.Role{
			Name: name,
			Backstory: "Ten years building payment and ledger systems at " +
				"high-volume marketplaces, most recently owning settlement " +
				"for a two-sided market. Joined to make reconciliation " +
				"something nobody downstream has to think about.",
			Goal: "Keep the ledger correct and the settlement window under one hour.",
			Responsibilities: []string{
				"Own the ledger service and its schema",
				"Review every migration that touches money",
				"Carry the settlement pager one week in four",
			},
		}
		if i%2 == 1 {
			report.Kind = org.KindHuman
			report.Contact = &org.HumanContact{SlackUserID: fmt.Sprintf("U0REPORT%03d", i)}
			report.Availability = "CET business hours; replies within about four hours"
		} else {
			report.DeclaredHandle = fmt.Sprintf("report-%03d", i)
		}
		// THE HANDLE is what a manages entry resolves; a human report
		// declares none, so org.Role.Handle derives it from the name.
		lead.Manages = append(lead.Manages, report.Handle())
		roles = append(roles, report)
	}
	o := &org.Organization{
		Name: "Acme",
		Units: []*org.Unit{{
			Name:  "Eng Team",
			Type:  org.UnitTypeTeam,
			Lead:  "lead",
			Roles: roles,
		}},
	}
	o.Normalize()
	return o
}

// bigLead is the seat every roster-allowance case is written against: a lead
// whose team cannot fit, so all three rungs of the ladder are reachable.
func bigLead(n int) Seat { return seatIn(profiledReports(n), "lead") }
