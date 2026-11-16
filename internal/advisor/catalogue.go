package advisor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// entry is everything a class decides: how costly it is, where an owner
// looks for it, its words and its fix.
type entry struct {
	severity Severity
	area     Area
	// company marks a finding about the whole company, wherever its
	// warning is located.
	company bool
	words   func(c *config.Company, p place, w config.Warning) (title, why string)
	fix     func(c *config.Company, p place, w config.Warning) Fix
}

// catalogue is every class's entry. TestEveryClassHasAnEntry holds it to
// [config.AdvisoryClasses] and the three kinds of unclassed warning.
var catalogue = map[Class]entry{
	ClassHealthUnheld: {
		severity: SeverityCosts, area: AreaAccess, company: true,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return sentence(w.Title),
				"An alarm that fires on a server wakes the agents allowed to read the " +
					"engine's health, and none is, so it reaches nobody until a person " +
					"opens the dashboard."
		},
		fix: healthFix,
	},
	ClassAuxiliaryModel: {
		severity: SeverityNote, area: AreaLearning,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return sentence(w.Title),
				"Reflection, summaries and memory run after the agent's turns on the model its " +
					"own work runs on, so learning costs a share of its spend at that model's price."
		},
		fix: auxiliaryFix,
	},
	ClassScheduleFrequent: {
		severity: SeverityNote, area: AreaSchedules,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return sentence(w.Title),
				"Each run starts a turn on the agent's models whether or not anything changed. " +
					"A check that waits for something to happen, such as a deploy or a merge, is " +
					"better as a work item dated for when to look."
		},
		fix: scheduleFix,
	},
	ClassDirectBox: {
		severity: SeverityNote, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return sentence(w.Title), w.Person
		},
		fix: settingFix,
	},
	ClassConfigAdvisory: {
		severity: SeverityNote, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return ownWords("A setting worth checking.", w)
		},
		fix: settingFix,
	},
	ClassConfigReference: {
		severity: SeverityCosts, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return ownWords("A setting names something that does not exist.", w)
		},
		fix: settingFix,
	},
	ClassConfigAdmission: {
		severity: SeverityCosts, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return ownWords("A setting breaks a rule a new change would be refused for.", w)
		},
		fix: settingFix,
	},
}

// ownWords is an unclassed warning's title and why: its rule's title and
// person words where it carries them ([config.Warning.Person]), else title and
// the operator's sentence.
func ownWords(title string, w config.Warning) (string, string) {
	why := sentence(w.Message)
	if w.Person != "" {
		why = w.Person
	}
	if w.Title != "" {
		title = sentence(w.Title)
	}
	return title, why
}

// sentence is a warning's message as a sentence: capitalised and stopped.
func sentence(message string) string {
	s := strings.TrimSpace(message)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// executorModel is the model a seat's executor runs on, as a person reads it.
func executorModel(c *config.Company, r *config.Role) string {
	if key := executorEntry(c, r); key != "" {
		return c.ModelName(key)
	}
	return "its executor's model"
}

// agentSeats is every agent seat, in the chart's order.
func agentSeats(c *config.Company) []*config.Role {
	var out []*config.Role
	for role := range c.EachRole() {
		if role.Kind != org.KindHuman {
			out = append(out, role)
		}
	}
	return out
}
