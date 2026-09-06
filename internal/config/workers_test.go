package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// Worker templates are FOUNDER-AUTHORED and edited live, so every rule here
// has to fire at load. A malformed template discovered at spawn time costs
// the turn that happened to reach it first — hours after the edit, on
// whichever seat drew the short straw — and reports as a delegate failure
// rather than as the config error it is.

const workerDoc = `
name: Acme
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
    fast:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
%s
roles:
  - name: CEO
    handle: ceo
%s
`

func workerConfig(t *testing.T, workers, roleBody string) (*config.Company, error) {
	t.Helper()
	doc := strings.Replace(workerDoc, "%s", workers, 1)
	doc = strings.Replace(doc, "%s", roleBody, 1)
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		return nil, err
	}
	return c, c.Validate()
}

func TestAWellFormedTemplateValidates(t *testing.T) {
	t.Parallel()
	// The counterfactual first: without it every assertion below passes
	// for a rule that rejects every template.
	c, err := workerConfig(t, `
workers:
  researcher:
    description: reads sources and reports findings with citations
    system_prompt: You research things carefully.
    tools: [confluence_search]
    model: fast
    max_turns: 12
    output:
      type: object
      properties:
        findings: {type: string}
        citations: {type: array, items: {type: string}}
      required: [findings]
`, "")
	if err != nil {
		t.Fatalf("a well-formed template was rejected: %v", err)
	}
	if _, ok := c.Workers["researcher"]; !ok {
		t.Fatalf("the template did not parse: %+v", c.Workers)
	}
}

func TestATemplateMustSayWhatItIsAndWhoItIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"no description", `
workers:
  w:
    system_prompt: s
`, "workers.w.description"},
		{"no system prompt", `
workers:
  w:
    description: d
`, "workers.w.system_prompt"},
		{"a name a model cannot reproduce", `
workers:
  My Worker:
    description: d
    system_prompt: s
`, "workers.My Worker"},
		{"an unconfigured model", `
workers:
  w:
    description: d
    system_prompt: s
    model: gpt-nope
`, "workers.w.model"},
		{"a round cap above the ceiling", `
workers:
  w:
    description: d
    system_prompt: s
    max_turns: 500
`, "workers.w.max_turns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := workerConfig(t, tc.doc, "")
			if err == nil {
				t.Fatal("a malformed template validated cleanly")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not point at %s:\n%v", tc.want, err)
			}
		})
	}
}

// A ROUND CAP ABOVE THE CEILING IS NAMED, not silently clamped. A template is
// an EDIT: an operator who writes 500 and gets 40 has been overruled by a
// number nothing in the file mentions, and will write it again next time.
func TestATemplatesRoundCapIsRefusedRatherThanClamped(t *testing.T) {
	t.Parallel()
	_, err := workerConfig(t, `
workers:
  w:
    description: d
    system_prompt: s
    max_turns: 500
`, "")
	if err == nil {
		t.Fatal("an over-ask validated cleanly")
	}
	// The ceiling is named, so the operator can either lower the ask or
	// raise the ceiling deliberately.
	if !strings.Contains(err.Error(), "max_turns_ceiling") {
		t.Errorf("the refusal does not name the ceiling:\n%v", err)
	}
}

func TestAnOutputSchemaIsCheckedForWhatTheEngineDependsOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"not an object", `
      type: string
`, "output.type"},
		{"no properties", `
      type: object
      properties: {}
`, "output.properties"},
		{"requires a field it does not have", `
      type: object
      properties:
        a: {type: string}
      required: [a, b]
`, "output.required"},
		{"too many fields", `
      type: object
      properties:
        a1: {type: string}
        a2: {type: string}
        a3: {type: string}
        a4: {type: string}
        a5: {type: string}
        a6: {type: string}
        a7: {type: string}
        a8: {type: string}
        a9: {type: string}
        b1: {type: string}
        b2: {type: string}
        b3: {type: string}
        b4: {type: string}
`, "output.properties"},
		{"nested past the limit", `
      type: object
      properties:
        a:
          type: object
          properties:
            b:
              type: object
              properties:
                c:
                  type: object
                  properties:
                    d: {type: string}
`, "workers.w.output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := workerConfig(t, "\nworkers:\n  w:\n    description: d\n"+
				"    system_prompt: s\n    output:\n"+tc.schema, "")
			if err == nil {
				t.Fatal("a schema the submission tool could not use validated cleanly")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not point at %s:\n%v", tc.want, err)
			}
		})
	}
}

// A KEYWORD THE ENGINE DOES NOT KNOW IS PASSED THROUGH. The engine does not
// implement JSON Schema — it hands the map to the provider, which does — so
// refusing an unrecognised keyword would make the engine the bottleneck on
// somebody else's schema support.
func TestAnUnknownSchemaKeywordIsNotRefused(t *testing.T) {
	t.Parallel()
	if _, err := workerConfig(t, `
workers:
  w:
    description: d
    system_prompt: s
    output:
      type: object
      additionalProperties: false
      properties:
        verdict:
          type: string
          enum: [pass, fail]
          x-vendor-hint: whatever
      required: [verdict]
`, ""); err != nil {
		t.Errorf("a schema using keywords the engine does not read was rejected: %v", err)
	}
}

// A SEAT NAMING A TEMPLATE THAT DOES NOT EXIST GETS NOTHING at runtime: the
// visibility filter drops the name and the executor is offered a shorter
// list, with no signal anywhere that a worker was meant to be there.
func TestASeatNamingAnUnknownWorkerIsRefused(t *testing.T) {
	t.Parallel()
	_, err := workerConfig(t, `
workers:
  researcher:
    description: d
    system_prompt: s
`, "    workers: [researcher, reviewerr]")
	if err == nil {
		t.Fatal("a seat naming an unknown worker validated cleanly")
	}
	for _, want := range []string{"roles[0].workers[1]", "reviewerr", "researcher"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestAnUnknownWorkerIsRefusedInsideAUnitToo(t *testing.T) {
	t.Parallel()
	// The hierarchy nests to any depth, and a rule that only walks the
	// root's roles is a rule that does not apply to most companies.
	// ParseCompany validates as it parses, so the rule can fire at either
	// stage. Which one is not the point — that the config is refused is.
	c, err := config.ParseCompany([]byte(`
name: Acme
workers:
  researcher:
    description: d
    system_prompt: s
units:
  - name: Engineering
    children:
      - name: Platform
        roles:
          - name: Dev
            handle: dev
            workers: [ghost]
`))
	if err == nil {
		err = c.Validate()
	}
	if err == nil {
		t.Fatal("a nested seat's unknown worker validated cleanly")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the error does not name the worker:\n%v", err)
	}
}

// AN EMPTY VISIBILITY LIST MEANS EVERY TEMPLATE. A company that publishes
// three workers wants its seats using them; requiring each seat to opt in
// turns a shared library into per-seat copy-paste.
func TestWorkerVisibilityDefaultsToEveryTemplateAndNarrowsWhenNamed(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(`
name: Acme
workers:
  researcher:
    description: d
    system_prompt: s
  auditor:
    description: d
    system_prompt: s
roles:
  - name: CEO
    handle: ceo
  - name: Dev
    handle: dev
    workers: [auditor]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := config.WorkerNames(c.WorkersFor("ceo")); len(got) != 2 {
		t.Errorf("a seat that named none sees %v, want both", got)
	}
	if got := config.WorkerNames(c.WorkersFor("dev")); len(got) != 1 || got[0] != "auditor" {
		t.Errorf("a seat that named one sees %v", got)
	}
	// A HANDLE THAT NAMES NO SEAT SEES NOTHING, which is a different fact
	// from "named none": collapsing them would hand an unknown handle the
	// whole library.
	if got := config.WorkerNames(c.WorkersFor("nobody")); len(got) != 0 {
		t.Errorf("an unknown handle was given %v", got)
	}
}

func TestWorkersAreVisibleToNestedSeatsToo(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(`
name: Acme
workers:
  researcher:
    description: d
    system_prompt: s
  auditor:
    description: d
    system_prompt: s
units:
  - name: Engineering
    children:
      - name: Platform
        roles:
          - name: Dev
            handle: dev
            workers: [researcher]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := config.WorkerNames(c.WorkersFor("dev")); len(got) != 1 || got[0] != "researcher" {
		t.Errorf("a nested seat's visibility = %v", got)
	}
}

// A TURN HOLDS ITS OWN COPY. The live config cell is replaced wholesale by an
// apply, and a turn holding the live map would be reading a schema the next
// apply is free to mutate underneath it.
func TestCloningATemplateMapDetachesItFromTheLiveConfig(t *testing.T) {
	t.Parallel()
	live := map[string]config.Worker{"w": {
		Description: "d", SystemPrompt: "s",
		Tools: []string{"read_file"},
		Output: map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "string"}},
		},
	}}
	held := config.CloneWorkers(live)

	live["w"].Tools[0] = "slack_post"
	props, _ := live["w"].Output["properties"].(map[string]any)
	props["a"] = map[string]any{"type": "number"}

	if held["w"].Tools[0] != "read_file" {
		t.Errorf("the tool list is shared: %v", held["w"].Tools)
	}
	heldProps, _ := held["w"].Output["properties"].(map[string]any)
	nested, _ := heldProps["a"].(map[string]any)
	if nested["type"] != "string" {
		t.Errorf("the schema is shared: %+v", held["w"].Output)
	}
	if config.CloneWorkers(nil) != nil {
		t.Error("cloning nothing produced a map")
	}
}

// THE LINE AN EXECUTOR READS is built here rather than in the prompt package,
// so the prompt and the delegate tool's own refusal cannot describe one
// worker two different ways.
func TestAWorkerRendersAsOneLineNamingWhatItReturns(t *testing.T) {
	t.Parallel()
	line := config.DescribeWorker("researcher", config.Worker{
		Description: "reads sources\nand reports findings",
		Tools:       []string{"confluence_search", "confluence_get_page"},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"findings":  map[string]any{"type": "string"},
				"citations": map[string]any{"type": "array"},
			},
		},
	})
	for _, want := range []string{"researcher", "reads sources",
		"confluence_search", "citations", "findings"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q is not in the line: %s", want, line)
		}
	}
	// ONE LINE: a multi-line description would break the enumeration it
	// sits in.
	if strings.Contains(line, "\n") {
		t.Errorf("the line wraps: %q", line)
	}
	// And the fields are named in a STABLE order — the line reaches a
	// system prompt, and one whose bytes move between turns costs the
	// provider's cache the whole prefix.
	if strings.Index(line, "citations") > strings.Index(line, "findings") {
		t.Errorf("the returned fields are not sorted: %s", line)
	}
}

func TestDelegationBoundsAreValidated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"no parallelism", "  delegation:\n    max_parallel: 0\n",
			"delegation.max_parallel"},
		{"no tasks", "  delegation:\n    max_tasks_per_call: 0\n",
			"delegation.max_tasks_per_call"},
		{"an expired task cap", "  delegation:\n    task_timeout_seconds: 0\n",
			"delegation.task_timeout_seconds"},
		{"a fraction above one", "  delegation:\n    budget_fraction: 2\n",
			"delegation.budget_fraction"},
		{"a ceiling below the cap",
			"  delegation:\n    max_turns: 20\n    max_turns_ceiling: 5\n",
			"delegation.max_turns_ceiling"},
		{"a task cap above the call cap",
			"  delegation:\n    task_timeout_seconds: 900\n    call_timeout_seconds: 300\n",
			"delegation.task_timeout_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := config.ParseCompany([]byte("name: Acme\nturn_engine:\n" + tc.doc))
			if err == nil {
				err = c.Validate()
			}
			if err == nil {
				t.Fatal("an impossible bound validated cleanly")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not point at %s:\n%v", tc.want, err)
			}
		})
	}
	// The counterfactual: the shipped defaults validate.
	c, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the shipped delegation defaults were rejected: %v", err)
	}
}

// AN ORG CHART AUTHORED BEFORE ITS PROVIDERS is a documented state, and the
// model rule skips entirely rather than firing on every template — the same
// rule a role's provider keys follow.
func TestAnEmptyProviderMapSkipsTheWorkerModelRule(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(`
name: Acme
workers:
  w:
    description: d
    system_prompt: s
    model: not-configured-yet
roles:
  - name: CEO
    handle: ceo
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("an org chart authored before its providers was rejected: %v", err)
	}
}
