package skills

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// The authoring format: YAML frontmatter, then a markdown body.
//
// The same file shape everything else in the ecosystem uses, so a skill is
// something an operator writes in their editor, commits, and publishes — and
// something they can read back off the page the engine is serving.

// frontmatterPattern splits a leading `---` block from the body.
var frontmatterPattern = regexp.MustCompile(`(?s)\A---\s*\n(.*?\n)---\s*\n?(.*)\z`)

// frontmatter is the declared half of a skill.
//
// Decoded STRICTLY: an unknown key is a typo, and a typo in `requred:` is a
// skill that silently stops being enforced. The knowledge base is edited by
// people, so the typo is the likely case rather than the exotic one.
type frontmatter struct {
	Key      string   `yaml:"key"`
	Title    string   `yaml:"title"`
	Summary  string   `yaml:"summary"`
	Phases   []string `yaml:"phases"`
	Trigger  Trigger  `yaml:"trigger"`
	Required *bool    `yaml:"required"`
}

// Source identifies where a skill was read from, for provenance.
type Source struct {
	PageID  string
	Version int
}

// Parse reads a skill from its authored text.
//
// The REQUIRED flag is a pointer in the frontmatter and resolved here, so
// [RequiredByDefault] is the single source of the default: a file that omits
// the key and a skill constructed directly cannot disagree about what
// omitting it means.
func Parse(text string, from Source) (Skill, error) {
	match := frontmatterPattern.FindStringSubmatch(text)
	if match == nil {
		return Skill{}, fmt.Errorf("skills: missing or malformed YAML frontmatter")
	}

	var fm frontmatter
	decoder := yaml.NewDecoder(strings.NewReader(match[1]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fm); err != nil {
		return Skill{}, fmt.Errorf("skills: invalid frontmatter: %w", err)
	}

	phases, err := parsePhases(fm.Phases)
	if err != nil {
		return Skill{}, err
	}
	required := RequiredByDefault
	if fm.Required != nil {
		required = *fm.Required
	}
	skill := Skill{
		Key:               strings.TrimSpace(fm.Key),
		Title:             strings.TrimSpace(fm.Title),
		Summary:           strings.TrimSpace(fm.Summary),
		Body:              strings.TrimSpace(match[2]),
		Trigger:           fm.Trigger,
		Phases:            phases,
		Required:          required,
		SourcePageID:      from.PageID,
		SourcePageVersion: from.Version,
	}
	if err := skill.Validate(); err != nil {
		return Skill{}, err
	}
	return skill, nil
}

// parsePhases turns the declared phase names into the prompt package's own.
//
// A NAME NOBODY RECOGNISES IS AN ERROR rather than a skip: `phases:
// [executed]` would otherwise produce a skill offered in no phase, which
// looks from every angle like a skill nobody wrote.
func parsePhases(names []string) ([]prompts.Phase, error) {
	known := []prompts.Phase{
		prompts.PhasePlan, prompts.PhaseExecute,
		prompts.PhaseReview, prompts.PhaseSubagent,
	}
	var out []prompts.Phase
	for _, name := range names {
		phase := prompts.Phase(strings.ToLower(strings.TrimSpace(name)))
		if !slices.Contains(known, phase) {
			return nil, fmt.Errorf("skills: unknown phase %q (want one of %v)",
				name, known)
		}
		if !slices.Contains(out, phase) {
			out = append(out, phase)
		}
	}
	return out, nil
}

// IsSkill reports whether text is a skill page at all.
//
// The ADMISSION TEST the sync workers share: a knowledge container holds
// ordinary pages beside its skills, and a page with no frontmatter — or with
// frontmatter that names no trigger — is one of those rather than a broken
// skill. Distinguishing them is what keeps a project's home page from
// producing a decode failure on every walk.
func IsSkill(text string) bool {
	match := frontmatterPattern.FindStringSubmatch(text)
	if match == nil {
		return false
	}
	var probe struct {
		Trigger *Trigger `yaml:"trigger"`
	}
	if err := yaml.Unmarshal([]byte(match[1]), &probe); err != nil {
		return false
	}
	return probe.Trigger != nil
}
