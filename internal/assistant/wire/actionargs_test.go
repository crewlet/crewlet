package wire_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// ceilingArgs is the Budgets screen's ceiling, as the screen declares it: a
// seat it draws or none for the company, one of three windows, and a value.
const ceilingArgs = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["period", "ceiling"],
  "properties": {
    "seat": {"type": "string", "enum": ["qa", "engineer"], "description": "A seat's handle; leave it out for the company"},
    "period": {"type": "string", "enum": ["day", "week", "month"]},
    "ceiling": {"type": ["string", "null"], "maxLength": 32}
  },
  "examples": [{"seat": "qa", "period": "day", "ceiling": "1.5M"}, {"period": "month", "ceiling": null}]
}`

// opsArgs is a list of operations told apart by their `op`, and a family of
// members by pattern.
const opsArgs = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["ops"],
  "properties": {
    "ops": {"type": "array", "minItems": 1, "maxItems": 3, "items": {"oneOf": [
      {"type": "object", "additionalProperties": false, "required": ["op", "seat"],
       "properties": {"op": {"type": "string", "const": "remove"}, "seat": {"type": "string"}}},
      {"type": "object", "additionalProperties": false, "required": ["op", "in", "data"],
       "properties": {"op": {"type": "string", "const": "add_seat"}, "in": {"type": "string"},
                      "data": {"type": "object"}}}
    ]}},
    "filters": {"type": "object", "additionalProperties": false,
      "patternProperties": {"^f\\.[a-z0-9_-]{1,64}$": {"type": ["string", "null"]}}}
  }
}`

func decode(t *testing.T, raw string) *wire.ActionArgs {
	t.Helper()
	args, err := wire.DecodeActionArgs([]byte(raw))
	if err != nil {
		t.Fatalf("a schema the screen declares: %v", err)
	}
	return args
}

func value(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A PROPOSAL IS HELD TO THE SCREEN'S OWN ARGUMENTS, and a refusal says what
// they are.
//
// The QA run's guesses at a ceiling ({handle, limit, window}, then {scope,
// value, window}) are each refused naming the member the screen does not
// take and the ones it does, so the model's next call can be right; a value
// out of the screen's set names the set. Mutation: let an object take
// undeclared members, or drop the enum check, and a guess passes.
func TestAnActionsArgumentsRefuseAGuessInTheScreensOwnTerms(t *testing.T) {
	t.Parallel()
	ceiling := decode(t, ceilingArgs)
	for name, c := range map[string]struct {
		value string
		want  string
	}{
		"the first guess": {`{"handle": "qa", "limit": 1500000, "window": "day"}`,
			`args takes no "handle"; it takes ceiling, period, seat`},
		"the second guess": {`{"scope": "qa", "value": "1.5M", "window": "day"}`,
			`args takes no "scope"; it takes ceiling, period, seat`},
		"a seat the screen does not draw": {`{"seat": "nobody", "period": "day", "ceiling": "1M"}`,
			`args.seat is one of "qa", "engineer", not "nobody"`},
		"a window the screen has not": {`{"seat": "qa", "period": "year", "ceiling": "1M"}`,
			`args.period is one of "day", "week", "month"`},
		"a value of the wrong type": {`{"period": "day", "ceiling": 3}`,
			`args.ceiling is text or null`},
		"a required member left out": {`{"seat": "qa", "ceiling": "1M"}`,
			`args needs "period"`},
	} {
		err := ceiling.Check(value(t, c.value))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.want)
		}
	}
	for _, ok := range []string{`{"seat": "qa", "period": "day", "ceiling": "1.5M"}`,
		`{"period": "week", "ceiling": null}`} {
		if err := ceiling.Check(value(t, ok)); err != nil {
			t.Errorf("%s was refused: %v", ok, err)
		}
	}
}

// AN OPERATION IS HELD TO THE SHAPE ITS `op` NAMES, and a family member to
// its pattern.
//
// A list of operations is a oneOf told apart by a const member: a refusal
// speaks of the operation the model meant (a remove that carries `data`), and
// an unknown operation names every one the screen takes. Mutation: check a
// oneOf against its first variant only, and a valid add_seat is refused.
func TestAnOperationIsHeldToTheShapeItsOpNames(t *testing.T) {
	t.Parallel()
	ops := decode(t, opsArgs)
	for name, c := range map[string]struct{ value, want string }{
		"an operation the screen does not make": {`{"ops": [{"op": "rename", "seat": "qa"}]}`,
			`args.ops[0].op is one of "remove", "add_seat"`},
		"a remove carrying an add's member": {`{"ops": [{"op": "remove", "seat": "qa", "data": {}}]}`,
			`args.ops[0] takes no "data"`},
		"too many operations": {`{"ops": [{"op": "remove", "seat": "a"}, {"op": "remove", "seat": "b"}, ` +
			`{"op": "remove", "seat": "c"}, {"op": "remove", "seat": "d"}]}`, `holds at most 3 entries`},
		"a member outside the family": {`{"ops": [{"op": "remove", "seat": "qa"}], "filters": {"g.x": "1"}}`,
			`args.filters takes no "g.x"`},
	} {
		err := ops.Check(value(t, c.value))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.want)
		}
	}
	good := `{"ops": [{"op": "add_seat", "in": "core", "data": {"name": "Writer", "handle": "writer"}}, ` +
		`{"op": "remove", "seat": "qa"}], "filters": {"f.priority": "high", "f.owner": null}}`
	if err := ops.Check(value(t, good)); err != nil {
		t.Errorf("a well-formed list of operations was refused: %v", err)
	}
}

// A SCHEMA THE ENGINE WOULD READ DIFFERENTLY FROM THE SCREEN IS REFUSED.
//
// A keyword outside the vocabulary (it would be ignored, and the engine would
// take what the screen refuses), a pattern RE2 cannot compile, a required
// member never declared, a schema past its bounds, and an example its own
// schema refuses (the model would copy it) each make the declaration
// malformed. Mutation: decode leniently, or skip the examples, and its case
// validates.
func TestADeclaredSchemaIsHeldToTheVocabulary(t *testing.T) {
	t.Parallel()
	deep := `{"type": "object"}`
	for range 10 {
		deep = `{"type": "object", "properties": {"x": ` + deep + `}}`
	}
	for name, raw := range map[string]string{
		"an unknown keyword":         `{"type": "object", "minProperties": 1}`,
		"an unknown type":            `{"type": "date"}`,
		"a pattern RE2 refuses":      `{"type": "object", "patternProperties": {"(?<=a)b": {}}}`,
		"a required member unnamed":  `{"type": "object", "required": ["seat"], "properties": {}}`,
		"a schema nested too deep":   deep,
		"an example it refuses":      `{"type": "object", "additionalProperties": false, "examples": [{"x": 1}]}`,
		"examples below the top":     `{"type": "object", "properties": {"x": {"type": "string", "examples": ["a"]}}}`,
		"a description with a break": `{"type": "string", "description": "one\u0000two"}`,
	} {
		if _, err := wire.DecodeActionArgs([]byte(raw)); !errors.Is(err, wire.ErrInvalid) {
			t.Errorf("%s validated (%v)", name, err)
		}
	}
	decode(t, ceilingArgs)
	decode(t, opsArgs)
}

// A PAGE CONTEXT DECLARES ARGUMENTS AND REVISIONS ONLY FOR WHAT IT ACCEPTS.
//
// An action's arguments or revision for an action the screen does not accept
// is a page that contradicts itself, and a malformed schema is refused with
// the page context it rides in, so no proposal is ever held to it. A
// revision per action is what [wire.PageContext.RevisionFor] answers, and the
// page's own where the screen states none. Mutation: drop the accepts check
// and the stray declaration validates.
func TestAPageContextDeclaresArgumentsOnlyForWhatItAccepts(t *testing.T) {
	t.Parallel()
	page := func() wire.PageContext {
		return wire.PageContext{Screen: "budgets", Title: "Company › Budgets",
			Params: map[string]string{}, Query: map[string]string{}, Resources: []wire.Resource{},
			SourceRevision: "budgets@1", Accepts: []string{"budgets.ceiling.prefill"},
			ActionArgs: map[string]*wire.ActionArgs{
				"budgets.ceiling.prefill": decode(t, ceilingArgs)},
			ActionRevisions: map[string]string{"budgets.ceiling.prefill": "budgets@screen"}}
	}
	if err := page().Validate(); err != nil {
		t.Fatalf("a page context declaring its action's arguments: %v", err)
	}
	for name, mutate := range map[string]func(*wire.PageContext){
		"arguments for an action it does not accept": func(p *wire.PageContext) {
			p.ActionArgs["work.filter.apply"] = decode(t, ceilingArgs)
		},
		"a revision for an action it does not accept": func(p *wire.PageContext) {
			p.ActionRevisions["work.filter.apply"] = "x"
		},
		"an empty revision": func(p *wire.PageContext) {
			p.ActionRevisions["budgets.ceiling.prefill"] = ""
		},
		"a schema that refuses its own example": func(p *wire.PageContext) {
			closed := false
			p.ActionArgs["budgets.ceiling.prefill"] = &wire.ActionArgs{
				Type: wire.ArgTypes{wire.ArgObject}, AdditionalProperties: &closed,
				Examples: []json.RawMessage{json.RawMessage(`{"x": 1}`)}}
		},
	} {
		p := page()
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, wire.ErrInvalid) {
			t.Errorf("%s validated (%v)", name, err)
		}
	}
	p := page()
	if got := p.RevisionFor("budgets.ceiling.prefill"); got != "budgets@screen" {
		t.Errorf("the prefill's revision is %q, want its own", got)
	}
	if got := p.RevisionFor("draft.set"); got != "budgets@1" {
		t.Errorf("an action with no revision of its own is at %q, want the page's", got)
	}
	// THE PAGE CONTEXT ROUND-TRIPS THROUGH JSON, as a request carries it.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back wire.PageContext
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("the page context as a request carries it: %v", err)
	}
}

// THE ENGINE READS EVERY SCHEMA THE DASHBOARD DECLARES.
//
// `testdata/action_args.json` is every kind of page action's arguments as the
// dashboard's own builders emit them, compared on the dashboard's side
// (`dashboard/src/app/assistant/actionArgs.test.ts`), where each is also fed
// through its screen's real parser. Here each is read by the engine's strict
// decoder, so a keyword the engine would ignore (and then take what the
// screen refuses), or an example the engine's checker refuses, fails. Every
// page action the dashboard declares in ASSISTANT_PAGE_ACTIONS that takes
// arguments is represented. Mutation: decode leniently and an unknown
// keyword the dashboard starts emitting passes unseen.
func TestTheEngineReadsEverySchemaTheDashboardDeclares(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "action_args.json"))
	if err != nil {
		t.Fatal(err)
	}
	var declared map[string]json.RawMessage
	if err := json.Unmarshal(raw, &declared); err != nil {
		t.Fatal(err)
	}
	if len(declared) < 8 {
		t.Fatalf("the fixture holds %d declarations, too few to be the dashboard's", len(declared))
	}
	for id, schema := range declared {
		if !wire.ValidActionID(id) {
			t.Errorf("the fixture names %q, which is not a page action id", id)
		}
		args, err := wire.DecodeActionArgs(schema)
		if err != nil {
			t.Errorf("%s: the engine refuses the dashboard's declaration: %v", id, err)
			continue
		}
		if len(args.Examples) == 0 {
			t.Errorf("%s declares no example for the model to copy", id)
		}
	}
}

// EVERY FAULT OF AN ACTION'S ARGUMENTS IN ONE REFUSAL (ADR-0059): a guess with
// three faults is told all three, each by its member.
//
// Mutation: return from the check at its first fault and only one is named.
func TestEveryFaultOfAnActionsArgumentsIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	ceiling := decode(t, ceilingArgs)
	err := ceiling.Check(value(t, `{"handle": "qa", "seat": "nobody", "ceiling": 3}`))
	if err == nil {
		t.Fatal("three faults were accepted")
	}
	for _, want := range []string{`args takes no "handle"`, `args.seat is one of "qa", "engineer", not "nobody"`,
		`args.ceiling is text or null`, `args needs "period"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}
