package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// contractSchema is a schema of every kind the builtin tools advertise.
var contractSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title":     map[string]any{"type": "string"},
		"status":    map[string]any{"type": "string", "enum": []any{"todo", "done"}},
		"protected": map[string]any{"type": "boolean"},
		"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		"points":    map[string]any{"type": []any{"number", "null"}},
		"labels":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 3},
		"ops": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"op": map[string]any{"type": "string"}, "if_version": map[string]any{"type": "integer"}},
			"required":   []any{"op"},
		}},
		"filters": map[string]any{"type": "object"},
		"files": map[string]any{"type": "object", "properties": map[string]any{},
			"additionalProperties": map[string]any{"type": "object", "properties": map[string]any{
				"content": map[string]any{"type": "string"}}}},
	},
	"required": []any{"title"},
}

func findings(args string) []tools.ArgFault {
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		panic(err)
	}
	return tools.CheckArgs("make_thing", contractSchema, v)
}

// A VALUE THE TOOL WOULD MISREAD IS A MISREAD, named at its path with the shape
// the tool reads there (ADR-0059): a string or a number where a boolean
// belongs (read as false), an object or a list where a string belongs (read
// as ""), a fraction or "5 days" or a word where a whole number belongs
// (truncated, read as 5, replaced by the default), a non-finite or a word
// where a number belongs, anything but a list or a string where a list
// belongs, and anything but an object where one belongs.
//
// Mutation: read a fraction as a whole number and `limit` 2.5 is let through.
func TestEachLossyValueIsAMisreadAtItsPath(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		`{"title": "t", "protected": "true"}`:                       "`protected` is the string \"true\", and make_thing reads true or false there",
		`{"title": "t", "protected": 1}`:                            "`protected` is the number 1, and make_thing reads true or false there",
		`{"title": {"a": 1}}`:                                       "`title` is an object, and make_thing reads a string there",
		`{"title": ["a"]}`:                                          "`title` is a list, and make_thing reads a string there",
		`{"title": "t", "limit": 2.5}`:                              "`limit` is the number 2.5, and make_thing reads a whole number there",
		`{"title": "t", "limit": "5 days"}`:                         "`limit` is the string \"5 days\", and make_thing reads a whole number there",
		`{"title": "t", "limit": "ten"}`:                            "`limit` is the string \"ten\", and make_thing reads a whole number there",
		`{"title": "t", "limit": true}`:                             "`limit` is true, and make_thing reads a whole number there",
		`{"title": "t", "limit": 1e300}`:                            "`limit` is the number 1e300, and make_thing reads a whole number there",
		`{"title": "t", "limit": "1e300"}`:                          "`limit` is the string \"1e300\", and make_thing reads a whole number there",
		`{"title": "t", "points": "NaN"}`:                           "`points` is the string \"NaN\", and make_thing reads a number or null there",
		`{"title": "t", "points": "a few"}`:                         "`points` is the string \"a few\", and make_thing reads a number or null there",
		`{"title": "t", "labels": 3}`:                               "`labels` is the number 3, and make_thing reads a list of strings",
		`{"title": "t", "labels": {"a": 1}}`:                        "`labels` is an object, and make_thing reads a list of strings",
		`{"title": "t", "ops": "write"}`:                            "`ops` is the string \"write\", and make_thing reads a list of objects {if_version, op} there",
		`{"title": "t", "ops": [{"op": "set", "if_version": 1.5}]}`: "`ops[0].if_version` is the number 1.5, and make_thing reads a whole number there",
		`{"title": "t", "filters": "x"}`:                            "`filters` is the string \"x\", and make_thing reads an object there",
		`{"title": "t", "files": {"a.md": {"content": ["x"]}}}`:     "`files.a.md.content` is a list, and make_thing reads a string there",
	} {
		got := findings(args)
		if len(got) == 0 || got[0].Finding != tools.Misread || !strings.Contains(got[0].Message, want) {
			t.Errorf("%s: findings %+v, want a misread saying %q", args, got, want)
		}
	}
}

// THE LOSSLESS LENIENCIES ARE NOT FINDINGS, exactly as the tools read them: a
// number or a boolean where a string belongs, one string or a comma-separated
// one where a list belongs, a numeric string where a number belongs, a
// whole-number literal, and null anywhere (absent).
//
// Mutation: refuse a number where a string belongs and `title` 7 is a finding.
func TestEachLosslessValueIsNoFinding(t *testing.T) {
	t.Parallel()
	for _, args := range []string{
		`{"title": 7}`,
		`{"title": true}`,
		`{"title": "t", "labels": "backend"}`,
		`{"title": "t", "labels": "backend, api"}`,
		`{"title": "t", "limit": "42"}`,
		`{"title": "t", "limit": " 7 "}`,
		`{"title": "t", "limit": "5.0"}`,
		`{"title": "t", "limit": 5.0}`,
		`{"title": "t", "limit": 3.0}`,
		`{"title": "t", "points": "2.5"}`,
		`{"title": "t", "points": null, "status": null}`,
		`{"title": "t", "status": "done", "protected": false}`,
		`{"title": "t", "filters": {"anything": [1, {"x": true}]}}`,
		`{"title": "t", "files": {"a.md": {"content": "x"}}}`,
		`{"title": "t", "ops": [{"op": "set", "if_version": "4"}]}`,
	} {
		if got := findings(args); len(got) != 0 {
			t.Errorf("%s: findings %+v, want none", args, got)
		}
	}
}

// WHAT THE TOOL DECIDES ON ITSELF IS OUTSIDE, and an undeclared key is UNKNOWN:
// neither stops the tool.
func TestOutsideAndUnknownAreNamedAndLeftToTheTool(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]struct {
		finding tools.Finding
		says    string
	}{
		`{"status": "done"}`:                               {tools.Outside, "`title` is required: a string"},
		`{"title": "t", "status": "doing"}`:                {tools.Outside, "`status` is \"doing\", and make_thing takes one of todo, done"},
		`{"title": "t", "limit": 0}`:                       {tools.Outside, "`limit` is 0, and make_thing takes at least 1"},
		`{"title": "t", "limit": "500"}`:                   {tools.Outside, "`limit` is 500, and make_thing takes at most 100"},
		`{"title": "t", "labels": "a, b, c, d"}`:           {tools.Outside, "`labels` holds 4 entries, and make_thing takes at most 3"},
		`{"title": "t", "ops": [{"if_version": 1}]}`:       {tools.Outside, "`ops[0].op` is required: a string"},
		`{"title": "t", "tittle": "x"}`:                    {tools.Unknown, "`tittle` is not an argument make_thing takes there, so it was not read: make_thing takes"},
		`{"title": "t", "ops": [{"op": "set", "opp": 1}]}`: {tools.Unknown, "`ops[0].opp` is not an argument make_thing takes there"},
	} {
		got := findings(args)
		if len(got) != 1 || got[0].Finding != want.finding || !strings.Contains(got[0].Message, want.says) {
			t.Errorf("%s: findings %+v, want one saying %q", args, got, want.says)
		}
	}
}

// contractTool is a tool with the contract schema that answers as told.
type contractTool struct {
	ran    atomic.Int64
	answer tools.Result
	name   string
}

func (c *contractTool) Name() string               { return c.name }
func (c *contractTool) Description() string        { return "makes a thing" }
func (c *contractTool) Parameters() map[string]any { return contractSchema }
func (c *contractTool) Call(context.Context, map[string]any) (tools.Result, error) {
	c.ran.Add(1)
	return c.answer, nil
}

func contractSurface(t *testing.T, tool *contractTool, origin string) (*tools.Surface, *countingRecorder) {
	t.Helper()
	r := tools.NewRegistry()
	if err := r.RegisterWith(tool, origin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
		t.Fatal(err)
	}
	rec := &countingRecorder{}
	return tools.NewSurface("execute", r.Snapshot(), []string{tool.name}).WithCounter(rec), rec
}

// A MISREAD IS REFUSED BEFORE THE TOOL RUNS, with every finding of the call in
// the one answer, the misreads first.
//
// Mutation: run the tool before the check and protected "true" is written as
// false.
func TestAMisreadIsRefusedBeforeTheToolRuns(t *testing.T) {
	t.Parallel()
	tool := &contractTool{name: "make_thing", answer: tools.Result{Output: "made"}}
	s, _ := contractSurface(t, tool, tools.OriginBuiltin)
	res, err := s.Execute(context.Background(), llm.ToolCall{Name: "make_thing",
		Arguments: map[string]any{"protected": "true", "status": "doing", "tittle": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ran.Load() != 0 {
		t.Fatal("the tool ran on a value it would misread")
	}
	if !res.Failed || tools.RefusalOf(tools.Result{Failed: res.Failed, Cause: res.Cause}) != tools.RefusalInvalid {
		t.Fatalf("answer %+v, want an invalid refusal", res)
	}
	lines := strings.Split(res.Output, "\n")
	for i, want := range []string{"make_thing did not run, so nothing was done",
		"- `protected` is the string \"true\"", "- `status` is \"doing\"", "- `title` is required",
		"- `tittle` is not an argument"} {
		if i >= len(lines) || !strings.Contains(lines[i], want) {
			t.Errorf("line %d of\n%s\nis not %q", i, res.Output, want)
		}
	}
}

// THE TOOL DECIDES WHAT IS NOT A MISREAD, and its answer carries every finding:
// its refusal lists them all, so one retry answers them; its success notes
// what was not read as sent, and is counted as drift.
//
// Mutation: drop the findings from the refusal and the model learns one per
// attempt again.
func TestTheToolsAnswerCarriesEveryFinding(t *testing.T) {
	t.Parallel()
	refusing := &contractTool{name: "make_thing", answer: tools.Result{Output: "make_thing needs a title",
		Failed: true, Cause: tools.RefusalInvalid}}
	s, rec := contractSurface(t, refusing, tools.OriginBuiltin)
	res, _ := s.Execute(context.Background(), llm.ToolCall{Name: "make_thing",
		Arguments: map[string]any{"status": "doing"}})
	if refusing.ran.Load() != 1 || !strings.Contains(res.Output, "Every argument outside make_thing's schema:\n- `status` is \"doing\"") ||
		!strings.Contains(res.Output, "- `title` is required") {
		t.Errorf("the refusal does not carry every finding: %q", res.Output)
	}
	if rec.count(metrics.ToolArgsOutsideSchema, "make_thing") != 0 {
		t.Error("a refusal was counted as drift")
	}

	succeeding := &contractTool{name: "make_thing", answer: tools.Result{Output: "made"}}
	s, rec = contractSurface(t, succeeding, tools.OriginBuiltin)
	res, _ = s.Execute(context.Background(), llm.ToolCall{Name: "make_thing",
		Arguments: map[string]any{"title": "t", "tittle": "x", "limit": "7"}})
	if res.Failed || !strings.HasPrefix(res.Output, "made\n\nNote: make_thing ran, but these arguments are outside its schema") ||
		!strings.Contains(res.Output, "`tittle` is not an argument") || strings.Contains(res.Output, "`limit`") {
		t.Errorf("the success does not note exactly the unknown argument: %q", res.Output)
	}
	if got := rec.count(metrics.ToolArgsOutsideSchema, "make_thing"); got != 1 {
		t.Errorf("drift counted %d times, want 1", got)
	}

	clean := &contractTool{name: "make_thing", answer: tools.Result{Output: "made"}}
	s, rec = contractSurface(t, clean, tools.OriginBuiltin)
	res, _ = s.Execute(context.Background(), llm.ToolCall{Name: "make_thing",
		Arguments: map[string]any{"title": 7.0, "labels": "a, b"}})
	if res.Output != "made" || rec.count(metrics.ToolArgsOutsideSchema, "make_thing") != 0 {
		t.Errorf("a lossless call was answered %q", res.Output)
	}
}

// A THIRD-PARTY MCP SERVER HOLDS ITS OWN SCHEMA: its calls are not checked here.
func TestAnMCPToolIsNotHeldToTheCheck(t *testing.T) {
	t.Parallel()
	tool := &contractTool{name: "make_thing", answer: tools.Result{Output: "made"}}
	s, _ := contractSurface(t, tool, tools.Origin("vendor"))
	res, _ := s.Execute(context.Background(), llm.ToolCall{Name: "make_thing",
		Arguments: map[string]any{"protected": "true"}})
	if tool.ran.Load() != 1 || res.Output != "made" {
		t.Errorf("an MCP tool's call was checked: ran %d, answered %q", tool.ran.Load(), res.Output)
	}
}

// A SCHEMA THE CHECK CANNOT KEEP IS NAMED, keyword by keyword.
func TestAnUnsupportedSchemaIsNamed(t *testing.T) {
	t.Parallel()
	got := tools.UnsupportedSchema(map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string", "pattern": "^x"},
		"b": map[string]any{"oneOf": []any{}},
		"c": map[string]any{"type": "date"},
	}})
	want := []string{"a: keyword pattern", "b: keyword oneOf", "c: type date"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("unsupported %v, want %v", got, want)
	}
	if got := tools.UnsupportedSchema(contractSchema); len(got) != 0 {
		t.Errorf("the contract schema is unsupported: %v", got)
	}
}

// A NOTE NEVER TOUCHES A TOOL'S OUTPUT (ADR-0059): a write tool answers the JSON
// receipt a caller decodes (the work API, the act transport), so a success
// outside the schema leaves Output byte for byte as the tool wrote it and
// carries the findings in Note, which each renderer places for its reader.
//
// Mutation: append the note to Output and the receipt no longer decodes.
func TestANoteNeverTouchesTheToolsOutput(t *testing.T) {
	t.Parallel()
	const receipt = `{"key": "ENG-9", "outcome": "applied"}`
	tool := &contractTool{name: "make_thing", answer: tools.Result{Output: receipt}}
	res, refused, err := tools.Invoke(context.Background(), tools.Invocation{Name: "make_thing",
		Tool: tool, Args: toolargs.Of(nil, map[string]any{"title": "t", "tittle": "x"}), Where: "operator"})
	if err != nil || refused {
		t.Fatalf("Invoke: refused %v, %v", refused, err)
	}
	if res.Output != receipt {
		t.Errorf("the tool's Output became %q", res.Output)
	}
	if !strings.Contains(res.Note, "`tittle` is not an argument") {
		t.Errorf("the note is %q", res.Note)
	}
	if got := tools.Rendered(res.Result); !strings.HasPrefix(got, receipt+"\n\nNote: make_thing ran") {
		t.Errorf("rendered for a reader of text as %q", got)
	}
}

// seeingTool records the arguments it was handed.
type seeingTool struct {
	contractTool
	saw map[string]any
}

func (s *seeingTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	s.saw = args
	return tools.Result{Output: "made"}, nil
}

// A CLOSED SET IS READ BY ONE RULE (#423 review): trimmed and without regard
// to case ([argval.Choice]), by the check and by the tool, which the surface
// hands the member the set spells. So " DONE " is "done" to both: no finding,
// no note, nothing counted, where the check read it exactly and noted a value
// the tool had accepted as "not read as sent".
//
// Mutation: compare the set exactly in the check, or hand the tool the value
// as sent, and " DONE " is noted and counted.
func TestAClosedSetIsReadByOneRule(t *testing.T) {
	t.Parallel()
	tool := &seeingTool{contractTool: contractTool{name: "make_thing"}}
	rec := &countingRecorder{}
	res, refused, err := tools.Invoke(context.Background(), tools.Invocation{Name: "make_thing", Tool: tool,
		Args: tools.ArgsOf(tool, false, map[string]any{"title": "t", "status": " DONE ", "labels": []any{"a"}}), Counter: rec})
	if err != nil || refused || res.Note != "" {
		t.Fatalf("a case variant of a member answered refused %v, note %q (%v)", refused, res.Note, err)
	}
	if tool.saw["status"] != "done" {
		t.Errorf("the tool was handed status %q, want the member \"done\"", tool.saw["status"])
	}
	if rec.count(metrics.ToolArgsOutsideSchema, "make_thing") != 0 {
		t.Error("a case variant of a member was counted as drift")
	}
	if got := findings(`{"title": "t", "status": "doing"}`); len(got) != 1 {
		t.Errorf("a value no member spells is not a finding: %+v", got)
	}
}

// A FINDING IS A NAMED VALUE a reader off the wire can judge.
func TestAFindingIsANamedValue(t *testing.T) {
	t.Parallel()
	for _, f := range tools.Findings {
		if !f.Valid() || string(f) == "" {
			t.Errorf("%q is not a valid finding", f)
		}
	}
	if tools.Finding("misreed").Valid() || tools.Finding("").Valid() {
		t.Error("a name this build does not have is valid")
	}
}
