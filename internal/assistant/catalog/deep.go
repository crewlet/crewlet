package catalog

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// StartDeepWorkTool is the escalation from quick mode into deep work.
const StartDeepWorkTool = "start_deep_work"

// DeepGoalMaxRunes and DeepPlanMaxRunes bound what a run is handed: a goal is
// one task in a sentence or two, and a plan is the steps the assistant
// expects, which the run is free to change.
const (
	DeepGoalMaxRunes = 600
	DeepPlanMaxRunes = 4000
)

// deep is the tool, built and checked once.
var deep = sync.OnceValues(func() (Tool, error) {
	t := Tool{
		Name: StartDeepWorkTool, Kind: KindDeep, Tier: TierRead,
		Phrase: "hand a task to deep work",
		Description: "Hand this task to deep work: a run of a harness with its own " +
			"shell, files, web search and fetch, and image reading, for as long as the task " +
			"needs within its limits. Use it when the task needs research, scripts, " +
			"generated files, the images the person attached, or many steps: for example " +
			"analysing a month of work to propose new teams and roles, or redesigning the " +
			"brand's assets. Do not use it for a single read or a single change. The run " +
			"reaches Crewlet only through the tools you have, as this person, and every " +
			"change it prepares is a proposal the person approves. Its answer is the run's " +
			"report and the files it delivered.",
		schema: object([]string{"goal"}, map[string]*schema{
			"goal": text("The task, in a sentence or two, as the person would recognise "+
				"it.", DeepGoalMaxRunes*4),
			"plan": text("The steps you expect the run to take: what to read, what to "+
				"compute or make, and what to propose.", DeepPlanMaxRunes*4),
		}),
		Label: func(a map[string]any) string {
			goal := strings.TrimSpace(Args(a).String("goal"))
			if goal == "" || utf8.RuneCountInString(goal) > 120 {
				return "Deep work"
			}
			return "Deep work: " + goal
		},
	}
	t.Parameters = t.schema.JSON()
	if t.Name == "" || t.Description == "" || t.Phrase == "" || !t.Kind.Valid() || !t.Tier.Valid() {
		return Tool{}, fmt.Errorf("catalog: %s is declared incompletely", t.Name)
	}
	return t, nil
})

// DeepWorkTool is the escalation tool. It is never in [Tools] or [Offered]:
// the orchestrator offers it on a node and a company that run deep work, and
// never to a deep-work run itself.
func DeepWorkTool() Tool {
	t, err := deep()
	if err != nil {
		panic(err)
	}
	return t
}

// DeepTask is a [StartDeepWorkTool] call's task, checked: its goal and plan,
// each within its bound.
func DeepTask(args map[string]any) (goal, plan string, err error) {
	t := DeepWorkTool()
	if err := t.Check(args); err != nil {
		return "", "", err
	}
	goal = strings.TrimSpace(Args(args).String("goal"))
	plan = strings.TrimSpace(Args(args).String("plan"))
	switch {
	case goal == "":
		return "", "", fmt.Errorf("%w: a deep-work run needs a goal", ErrInvalidArguments)
	case utf8.RuneCountInString(goal) > DeepGoalMaxRunes:
		return "", "", fmt.Errorf("%w: the goal is longer than %d characters", ErrInvalidArguments,
			DeepGoalMaxRunes)
	case utf8.RuneCountInString(plan) > DeepPlanMaxRunes:
		return "", "", fmt.Errorf("%w: the plan is longer than %d characters", ErrInvalidArguments,
			DeepPlanMaxRunes)
	}
	return goal, plan, nil
}
