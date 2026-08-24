package skills

import "github.com/crewlet/crewlet/internal/logging"

// Admitting a container's pages into a skill catalogue.
//
// The half of the sync that decides WHAT is a skill, kept apart from the
// half that decides where the pages came from. Two callers need it — the
// engine's own sync worker and the publishing CLI's `resync` — and a second
// copy would eventually admit a page one of them rejected, which is the
// difference between "the operator sees this skill loaded" and "the engine
// loads it".

var admitLog = logging.Get("agent.skills")

// Page is one knowledge-base page, as the skill sync reads it.
//
// Backend-neutral: a backend with no version concept stamps zero, and the
// text is already flattened to what a model reads. Nothing here knows
// whether it came from a wiki or a tracker.
type Page struct {
	ID      string
	Title   string
	Version int
	Text    string
}

// Admission is what one walk found, for the report.
type Admission struct {
	// Pages is how many the container held.
	Pages int
	// Ordinary is how many were not skills at all — a project home page,
	// an operator's notes. Expected, and not a failure.
	Ordinary int
	// Undecodable names pages that LOOK like skills and did not parse.
	// Worth reporting: somebody wrote a trigger and got the rest wrong,
	// and the only other symptom is guidance that never appears.
	Undecodable []string
}

// Admit turns a container's pages into the skills it holds.
//
// A page that fails to parse costs that page. The alternative — refusing
// the whole walk — would take a company's entire catalogue away over one
// operator's typo, and the catalogue is what makes agents follow this
// company's conventions rather than their model's defaults.
func Admit(pages []Page) ([]Skill, Admission) {
	out := make([]Skill, 0, len(pages))
	report := Admission{Pages: len(pages)}
	for _, page := range pages {
		if !IsSkill(page.Text) {
			report.Ordinary++
			continue
		}
		skill, err := Parse(page.Text, Source{PageID: page.ID, Version: page.Version})
		if err != nil {
			admitLog.Warn("skill_page_undecodable", "page", page.ID,
				"title", page.Title, "error", err.Error())
			report.Undecodable = append(report.Undecodable, page.Title)
			continue
		}
		out = append(out, skill)
	}
	return out, report
}
