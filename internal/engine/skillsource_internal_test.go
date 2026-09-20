package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
)

// WHERE A COMPANY'S SKILLS COME FROM IS DERIVED FROM THE APPLIED DOCUMENT AND
// THE WIRING THAT IS RUNNING, and the sync loop turns a change of that answer
// into a walk (and the previous source's retirement). What each case here
// pins is the three fields the loop compares, plus whether this node can read
// the source at all: a source that reports itself readable and is not would
// walk nothing and say nothing.
func TestTheSkillSourceFollowsTheAppliedCompanyAndTheWiring(t *testing.T) {
	t.Parallel()
	wired := confluenceParts{
		parser:      &confluence.Parser{},
		pages:       &confluence.Client{},
		base:        "https://wiki.example.com/wiki",
		skillsSpace: "TS",
	}
	for _, tc := range []struct {
		name       string
		company    *config.Company
		parts      confluenceParts
		backend    string
		container  string
		location   string
		readable   bool
		unreadable bool
	}{{
		name:      "an unconfigured node reads nothing and says nothing",
		company:   nil,
		container: "",
	}, {
		name: "a company on Confluence reads the space its wiring is on",
		company: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeConfluence},
			Integrations: config.Integrations{
				Confluence: &config.Confluence{URL: "https://wiki.example.com"},
			},
		},
		parts:     wired,
		backend:   "confluence",
		container: "TS",
		location:  "https://wiki.example.com/wiki",
		readable:  true,
	}, {
		name: "a Confluence with no organization credential cannot be read",
		company: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeConfluence},
			Integrations: config.Integrations{
				Confluence: &config.Confluence{URL: "https://wiki.example.com"},
			},
		},
		parts: confluenceParts{
			parser: &confluence.Parser{}, base: "https://wiki.example.com/wiki",
			skillsSpace: "TS",
		},
		backend:    "confluence",
		container:  "TS",
		location:   "https://wiki.example.com/wiki",
		unreadable: true,
	}, {
		name: "a Confluence that is not wired on this node cannot be read",
		company: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeConfluence},
			Integrations: config.Integrations{
				Confluence: &config.Confluence{URL: "https://wiki.example.com"},
			},
		},
		backend:    "confluence",
		container:  config.DefaultSkillsContainer,
		unreadable: true,
	}, {
		name: "a company on the native backend without one running cannot be read",
		company: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeNative},
		},
		backend:    "native",
		container:  config.DefaultSkillsContainer,
		unreadable: true,
	}, {
		name: "a company with no knowledge backend turns skills off",
		company: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
		},
		backend:   "none",
		container: "",
	}, {
		name: "an empty skills container turns skills off",
		company: &config.Company{
			Knowledge: config.Knowledge{
				Backend: config.KnowledgeConfluence, SkillsContainer: new(string),
			},
			Integrations: config.Integrations{
				Confluence: &config.Confluence{URL: "https://wiki.example.com"},
			},
		},
		parts:     wired,
		backend:   "confluence",
		container: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{}
			e.notify.confluence = tc.parts
			var company *Company
			if tc.company != nil {
				company = &Company{Config: tc.company}
			}

			src := e.skillSource(company)
			if src.Backend != tc.backend || src.Container != tc.container ||
				src.Location != tc.location {
				t.Errorf("source = backend %q container %q location %q, want %q/%q/%q",
					src.Backend, src.Container, src.Location,
					tc.backend, tc.container, tc.location)
			}
			if readable := src.Walk != nil; readable != tc.readable {
				t.Errorf("walkable = %v, want %v", readable, tc.readable)
			}
			if (src.Page != nil) != tc.readable {
				t.Errorf("single-page read = %v, want %v", src.Page != nil, tc.readable)
			}
			if unreadable := src.Unreadable != ""; unreadable != tc.unreadable {
				t.Errorf("unreadable reason %q, want one: %v", src.Unreadable, tc.unreadable)
			}
		})
	}
}
