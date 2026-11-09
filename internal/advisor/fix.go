package advisor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// FixKind is how a finding is fixed.
type FixKind string

const (
	// FixConfigChange is one entity write per option, previewed through
	// the entity route's dry run and sent by the administrator.
	FixConfigChange FixKind = "config_change"
	// FixOpenSetting is the setting the finding is located at, opened for
	// the administrator to edit: what the engine cannot build a change for.
	FixOpenSetting FixKind = "open_setting"
)

// Fix is what fixes a finding. Options is always a list, empty for a fix
// with no change to choose.
type Fix struct {
	Kind FixKind `json:"kind"`
	// Sentence says what the fix does, in one line.
	Sentence string `json:"sentence"`
	// Unchanged says what the fix leaves as it is, empty where that needs
	// no saying.
	Unchanged string   `json:"unchanged,omitempty"`
	Options   []Option `json:"options"`
	// Setting is the path an open_setting fix opens.
	Setting string `json:"setting,omitempty"`
}

// Option is one choice of a config change.
type Option struct {
	Label  string `json:"label"`
	Change Change `json:"change"`
}

// The entity collections a change writes, as the config surface names them
// (configapi.EntityRoles and configapi.EntityUnits; a test holds them equal).
const (
	EntityRoles = "roles"
	EntityUnits = "units"
)

// Change is one write of one entity: GET /config/{entity}/{id}, Set's
// top-level fields replaced in it, and PUT back to the same path under the
// tag that GET answered, first with `dry_run=true` for the preview and then
// for real, with Summary as the revision's summary.
type Change struct {
	Entity  string         `json:"entity"`
	ID      string         `json:"id"`
	Set     map[string]any `json:"set"`
	Summary string         `json:"summary"`
}

// healthFix offers each agent seat the health grant, the chart's order.
func healthFix(c *config.Company, _ place, _ config.Warning) Fix {
	fix := Fix{
		Kind:      FixConfigChange,
		Sentence:  "Let one agent watch the engine's health and wake on its alarms.",
		Unchanged: "No agent gets any other new access.",
		Options:   []Option{},
	}
	for _, role := range agentSeats(c) {
		handle := role.Seat().Handle()
		fix.Options = append(fix.Options, Option{
			Label: fmt.Sprintf("Let %s watch the engine's health", role.Name),
			Change: Change{Entity: EntityRoles, ID: handle, Set: map[string]any{"health_tools": true},
				Summary: fmt.Sprintf("Let %s watch the engine's health", handle)},
		})
	}
	return fix
}

// auxiliaryFix offers every providers.llm entry on another model as the
// seat's learning model, in the company's provider order. With none, it opens
// the providers, where a smaller model is added first.
func auxiliaryFix(c *config.Company, p place, w config.Warning) Fix {
	handle := p.role.Seat().Handle()
	order := c.Providers.ProviderOrder()
	current := executorEntry(c, p.role)
	fix := Fix{
		Kind:      FixConfigChange,
		Sentence:  fmt.Sprintf("Run %s's learning on a smaller model.", p.role.Name),
		Unchanged: fmt.Sprintf("%s's own work stays on %s.", p.role.Name, executorModel(c, p.role)),
		Options:   []Option{},
	}
	for _, key := range order {
		if current != "" && c.SameModel(key, current) {
			continue
		}
		fix.Options = append(fix.Options, Option{
			Label: fmt.Sprintf("Learn on %s", modelName(c, key)),
			Change: Change{Entity: EntityRoles, ID: handle,
				Set:     map[string]any{"llm_auxiliary": org.ProviderKeys{key}},
				Summary: fmt.Sprintf("Run %s's learning on providers.llm.%s", handle, key)},
		})
	}
	if len(fix.Options) == 0 {
		return Fix{
			Kind:     FixOpenSetting,
			Sentence: "Add a smaller model, then run this agent's learning on it.",
			Options:  []Option{}, Setting: "providers.llm",
		}
	}
	return fix
}

// executorEntry is the providers.llm key a seat's executor runs on first.
func executorEntry(c *config.Company, r *config.Role) string {
	if chain := c.ExecutorChain(r); len(chain) > 0 {
		return chain[0]
	}
	return ""
}

// The hour a less frequent schedule is offered at, on the company's or the
// schedule's own clock: the start of a working day.
const dailyHour = 9

// scheduleFix offers a frequent schedule two changes: once a day at
// [dailyHour] on the days it runs now, or removed.
func scheduleFix(_ *config.Company, p place, _ config.Warning) Fix {
	s, _ := p.theSchedule()
	var entity, id string
	if p.role != nil {
		entity, id = EntityRoles, p.role.Seat().Handle()
	} else {
		entity, id = EntityUnits, p.unit.ID
	}
	daily := slices.Clone(p.schedules())
	daily[p.schedule].Cron = dailyCron(s.Cron)
	removed := slices.Delete(slices.Clone(p.schedules()), p.schedule, p.schedule+1)
	return Fix{
		Kind:      FixConfigChange,
		Sentence:  fmt.Sprintf("Run %s less often, or remove it.", s.Name),
		Unchanged: "Its task stays as it is written.",
		Options: []Option{
			{
				Label: fmt.Sprintf("Run it once a day at %02d:00", dailyHour),
				Change: Change{Entity: entity, ID: id, Set: map[string]any{"schedules": daily},
					Summary: fmt.Sprintf("Run schedule %s once a day", s.Name)},
			},
			{
				Label: "Remove it",
				Change: Change{Entity: entity, ID: id, Set: map[string]any{"schedules": removed},
					Summary: fmt.Sprintf("Remove schedule %s", s.Name)},
			},
		},
	}
}

// dailyCron is expr fired once a day at [dailyHour], on the days of the
// month, months and days of the week it names now.
func dailyCron(expr string) string {
	days := []string{"*", "*", "*"}
	if fields := strings.Fields(expr); len(fields) == 5 {
		days = fields[2:]
	}
	return strings.Join(append([]string{"0", fmt.Sprint(dailyHour)}, days...), " ")
}

// settingFix opens the setting a warning is located at.
func settingFix(_ *config.Company, _ place, w config.Warning) Fix {
	return Fix{Kind: FixOpenSetting, Sentence: "Open the setting and change it.", Options: []Option{}, Setting: w.Path}
}
