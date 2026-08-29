package skills_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
)

const admissibleSkill = `---
key: review
title: Reviewing
summary: How we review.
phases: [execute]
trigger:
  mcp_server: gitlab
---

Read the diff.
`

// ORDINARY PAGES ARE NOT FAILURES. A knowledge container holds a project
// home page and an operator's notes beside its skills, and a walk that
// reported those as broken would bury the one page that actually is.
func TestAnOrdinaryPageIsCountedNotFailed(t *testing.T) {
	t.Parallel()
	loaded, report := skills.Admit([]skills.Page{
		{ID: "1", Title: "Reviewing", Text: admissibleSkill},
		{ID: "2", Title: "Project home", Text: "Welcome to the project."},
	})
	if len(loaded) != 1 || loaded[0].Key != "review" {
		t.Fatalf("loaded %+v", loaded)
	}
	if report.Ordinary != 1 {
		t.Errorf("ordinary = %d", report.Ordinary)
	}
	if len(report.Undecodable) != 0 {
		t.Errorf("an ordinary page was reported as broken: %v", report.Undecodable)
	}
	if report.Pages != 2 {
		t.Errorf("pages = %d", report.Pages)
	}
}

// A PAGE THAT DECLARES A TRIGGER AND DOES NOT PARSE costs that page and is
// NAMED. Refusing the whole walk would take a company's entire catalogue
// away over one operator's typo.
func TestABrokenSkillCostsOnlyItself(t *testing.T) {
	t.Parallel()
	loaded, report := skills.Admit([]skills.Page{
		{ID: "1", Title: "Reviewing", Text: admissibleSkill},
		{ID: "2", Title: "Broken", Text: strings.Replace(admissibleSkill,
			"phases: [execute]", "phases: [executed]", 1)},
	})
	if len(loaded) != 1 {
		t.Fatalf("loaded %+v", loaded)
	}
	if len(report.Undecodable) != 1 || report.Undecodable[0] != "Broken" {
		t.Errorf("undecodable = %v", report.Undecodable)
	}
}

// PROVENANCE TRAVELS WITH THE SKILL, so a loaded skill can be traced back
// to the page an operator edits.
func TestTheSourcePageIsCarriedOntoTheSkill(t *testing.T) {
	t.Parallel()
	loaded, _ := skills.Admit([]skills.Page{
		{ID: "page-7", Title: "Reviewing", Version: 3, Text: admissibleSkill},
	})
	if len(loaded) != 1 {
		t.Fatalf("loaded %+v", loaded)
	}
	if loaded[0].SourcePageID != "page-7" || loaded[0].SourcePageVersion != 3 {
		t.Errorf("provenance = %q/%d", loaded[0].SourcePageID, loaded[0].SourcePageVersion)
	}
}
