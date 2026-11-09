package advisor

import (
	"fmt"
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
		words: func(*config.Company, place, config.Warning) (string, string) {
			return "No agent watches the engine's own alarms.",
				"An alarm that fires on a server wakes the agents allowed to read the " +
					"engine's health, and none is, so it reaches nobody until a person " +
					"opens the dashboard."
		},
		fix: healthFix,
	},
	ClassAuxiliaryModel: {
		severity: SeverityNote, area: AreaLearning,
		words: func(c *config.Company, p place, _ config.Warning) (string, string) {
			name, model := p.role.Name, executorModel(c, p.role)
			return fmt.Sprintf("%s learns on the model it works on.", name),
				fmt.Sprintf("Reflection, summaries and memory run after %s's turns on %s, "+
					"the model its own work runs on, so learning costs a share of its "+
					"spend at that model's price.", name, model)
		},
		fix: auxiliaryFix,
	},
	ClassScheduleFrequent: {
		severity: SeverityNote, area: AreaSchedules,
		words: func(_ *config.Company, p place, _ config.Warning) (string, string) {
			s, _ := p.theSchedule()
			gap, _ := config.ScheduleGap(s)
			return fmt.Sprintf("%s runs every %s.", s.Name, config.EveryWords(gap)),
				"Each run starts a turn whether or not anything changed. A check that " +
					"waits for something to happen, such as a deploy or a merge, is " +
					"better as a work item dated for when to look."
		},
		fix: scheduleFix,
	},
	ClassConfigAdvisory: {
		severity: SeverityNote, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return "A setting worth checking.", sentence(w.Message)
		},
		fix: settingFix,
	},
	ClassConfigReference: {
		severity: SeverityCosts, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return "A setting names something that does not exist.", sentence(w.Message)
		},
		fix: settingFix,
	},
	ClassConfigAdmission: {
		severity: SeverityCosts, area: AreaSetup,
		words: func(_ *config.Company, _ place, w config.Warning) (string, string) {
			return "A setting breaks a rule a new change would be refused for.", sentence(w.Message)
		},
		fix: settingFix,
	},
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
		return modelName(c, key)
	}
	return "its executor's model"
}

// modelName is a providers.llm entry as a person reads it: its model, or its
// key where it names none.
func modelName(c *config.Company, key string) string {
	if m := c.Providers.LLM[key].Model; m != "" {
		return m
	}
	return key
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
