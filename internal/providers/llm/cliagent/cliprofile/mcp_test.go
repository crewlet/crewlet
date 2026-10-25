package cliprofile

import (
	"strings"
	"testing"
)

// routed is a custom profile declaring everything a run with tools reads: a
// tool route, a request record on stdout and a cap — in the shapes the
// shipped claude-code profile uses — for a case to break one field of.
func routed() map[string]any {
	event := func(kind string) []any {
		return []any{map[string]any{"path": []any{"type"}, "equals": kind}}
	}
	return map[string]any{
		"binary": "cli", "complete_args": []any{"-p"}, "output": "jsonl",
		"event_type_path": []any{"type"}, "text_events": []any{"result"},
		"text_paths": []any{[]any{"result"}},
		"mcp": map[string]any{
			"seed": []any{map[string]any{"path": "crewlet-mcp.json", "in": "work",
				"content": `{"mcpServers":{"crewlet":{"command":{relay_command_json},"args":{relay_args_json},"timeout":{tool_timeout_ms}}}}`}},
			"args":                 []any{"--mcp-config", "{work}/crewlet-mcp.json"},
			"allow_args":           []any{"--allowedTools", "mcp__crewlet__*"},
			"tool_timeout_seconds": 600,
			"relists":              true,
			"failure_events": []any{map[string]any{
				"match":  []any{map[string]any{"path": []any{"type"}, "equals": "result"}, map[string]any{"path": []any{"is_error"}, "equals": true}},
				"fields": []any{[]any{"subtype"}},
			}},
		},
		"requests": map[string]any{
			"source": "stdout", "host_prefix": "mcp__crewlet__",
			"stops": map[string]any{"end_turn": "end", "tool_use": "tool_use"},
			"events": []any{
				map[string]any{"match": event("start"), "opens": true, "id": []any{"id"}},
				map[string]any{"match": event("assistant"), "id": []any{"id"}, "blocks": map[string]any{
					"path": []any{"content"},
					"each": []any{
						map[string]any{"match": event("text"), "text": []any{"text"}},
						map[string]any{"match": event("tool_use"), "call": map[string]any{"name": []any{"name"}}},
					},
				}},
			},
		},
		"cap": map[string]any{"exit_code": 1, "match": event("capped")},
	}
}

// load is routed with overrides merged over it, as Load merges them over a
// built-in: mappings key by key, everything else replaced.
func load(overrides map[string]any) (Profile, error) {
	base := routed()
	mergeInto(base, overrides)
	return Load("custom", base)
}

// The control: the declaration every case breaks one field of loads.
func TestARoutedProfileLoads(t *testing.T) {
	t.Parallel()
	p, err := load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.MCP == nil || p.Requests == nil || p.Cap == nil || p.MCP.ToolTimeout().Minutes() != 10 {
		t.Errorf("loaded %+v / %+v / %+v", p.MCP, p.Requests, p.Cap)
	}
}

// A TOOL ROUTE THAT CANNOT START THE SERVER, OR THAT HANDS ONE RUN ANOTHER'S
// SOCKET, DOES NOT LOAD — refused by name, at `crewlet validate` and every
// write, rather than at a seat's first phase. And neither does a request record
// no line could be read by, or a cap nothing could tell.
//
// Mutation: drop any one rule from validateMCP, validateRequests or
// validateCap, and its case loads.
func TestABrokenToolRouteIsRefusedByName(t *testing.T) {
	t.Parallel()
	server := func(content string) map[string]any {
		return map[string]any{"seed": []any{map[string]any{"path": "m.json", "in": "work", "content": content}},
			"args": []any{"--mcp-config", "{work}/m.json"}}
	}
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"no relay command": {map[string]any{"mcp": server(`{"args":{relay_args_json}}`)},
			"names no relay command"},
		"no relay arguments": {map[string]any{"mcp": server(`{"command":{relay_command_json}}`)},
			"names no relay arguments"},
		"no tool timeout": {map[string]any{"mcp": map[string]any{"tool_timeout_seconds": 0}},
			"mcp.tool_timeout_seconds is 0"},
		"a seed in the seat's home": {map[string]any{"mcp": map[string]any{"seed": []any{map[string]any{
			"path": "crewlet-mcp.json", "in": "home",
			"content": `{"command":{relay_command_json},"args":{relay_args_json}}`}}}},
			`mcp.seed[0].in is "home"`},
		"a seed with no scope": {map[string]any{"mcp": map[string]any{"seed": []any{map[string]any{
			"path": "crewlet-mcp.json", "content": `{"command":{relay_command_json},"args":{relay_args_json}}`}}}},
			`mcp.seed[0].in is ""`},
		"a seed climbing out": {map[string]any{"mcp": map[string]any{"seed": []any{map[string]any{
			"path": "../m.json", "in": "work",
			"content": `{"command":{relay_command_json},"args":{relay_args_json}}`}}}},
			"mcp.seed[0].path"},
		"a seed on the project-root marker": {map[string]any{"mcp": map[string]any{"seed": []any{map[string]any{
			"path": ProjectRootMarker, "in": "work",
			"content": `{"command":{relay_command_json},"args":{relay_args_json}}`}}}},
			"mcp.seed[0].path"},
		"a misspelt placeholder": {map[string]any{"mcp": map[string]any{
			"args": []any{"--mcp-config", "{workdir}/crewlet-mcp.json"},
		}}, "{workdir}"},
		"an isolation variable in its env": {map[string]any{"mcp": map[string]any{
			"env": map[string]any{"HOME": "/elsewhere"},
		}}, "mcp.env names HOME"},
		"failure events in no stream": {map[string]any{
			"output": "json", "event_type_path": []any{}, "text_events": []any{}, "requests": nil, "cap": nil,
		}, "mcp.failure_events is set but output is \"json\""},
		"a record on stdout that is not a stream": {map[string]any{
			"output": "json", "event_type_path": []any{}, "text_events": []any{}, "cap": nil,
			"mcp": map[string]any{"failure_events": []any{}},
		}, "requests.source is stdout but output is \"json\""},
		"a record under the home naming no session": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/record.jsonl", "in": "home",
		}}, "does not name the run's own file through {session}"},
		"a record naming a session nothing gives the run": {map[string]any{"requests": map[string]any{
			"source": "file", "file": "{session}.jsonl", "in": "work",
		}}, "session_args does not give the run one"},
		"calls with no host prefix": {map[string]any{"requests": map[string]any{"host_prefix": ""}},
			"requests.host_prefix is empty"},
		"a stop reason the engine has no word for": {map[string]any{"requests": map[string]any{
			"stops": map[string]any{"max_tokens": "cap"},
		}}, "requests.stops.max_tokens"},
		"a rule that reads nothing": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"opens": true, "match": []any{map[string]any{"path": []any{"type"}, "equals": "x"}}},
				map[string]any{"match": []any{map[string]any{"path": []any{"type"}, "equals": "y"}}}},
		}}, "requests.events[1] reads nothing"},
		"a match with nothing to compare": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"opens": true, "match": []any{map[string]any{"path": []any{"type"}}}}},
		}}, "requests.events[0].match[0].equals"},
		"rules that never begin a request": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"text": []any{"t"}, "match": []any{map[string]any{"path": []any{"type"}, "equals": "x"}}}},
		}}, "neither opens a request nor names one by id"},
		"a cap that says nothing": {map[string]any{"cap": map[string]any{"match": []any{}, "exit_code": nil}},
			"cap names neither exit_code nor match"},
		"a record with no route": {map[string]any{"mcp": nil}, "requests is set but mcp is not"},
		"a failure marker of digits": {map[string]any{"failure_markers": []any{"429"}},
			"failure_markers[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := load(tc.overrides)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// THE TOOL ROUTE IS OVERRIDABLE LIKE EVERY OTHER FIELD: its mappings merge key
// by key, so an operator moving the tool timeout keeps the rest of the route,
// and null removes it outright — for an entry kept for text functions alone.
func TestTheToolRouteMergesLikeEveryOtherField(t *testing.T) {
	t.Parallel()
	p, err := load(map[string]any{"mcp": map[string]any{"tool_timeout_seconds": 900}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.MCP.ToolTimeoutSeconds != 900 || len(p.MCP.Seed) != 1 || len(p.MCP.Args) != 2 || !p.MCP.Relists {
		t.Errorf("mcp after the override = %+v, want the declared route with the new timeout", p.MCP)
	}
	bare, err := load(map[string]any{"mcp": nil, "requests": nil})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bare.MCP != nil || bare.Requests != nil {
		t.Errorf("null left the route in place: %+v", bare.MCP)
	}
}

// A CREDENTIAL IS NO PART OF THE TOOL ROUTE: its environment is set on every
// run with tools whatever auth.mode says, and stored unredacted.
func TestTheToolRoutesEnvCarriesNoCredential(t *testing.T) {
	t.Parallel()
	p, err := load(map[string]any{"mcp": map[string]any{"env": map[string]any{"VENDOR_API_KEY": "sk-..."}}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = p.ValidateCredentials("custom")
	if err == nil || !strings.Contains(err.Error(), `mcp.env names "VENDOR_API_KEY"`) {
		t.Errorf("err = %v, want the credential refused by name", err)
	}
}

// A RUN'S VALUES ARE SUBSTITUTED AS THE FORMAT NEEDS THEM: raw on argv, and as
// JSON — which a TOML document reads as well — inside a configuration file,
// with nothing a shell or a parser would read twice.
func TestTheToolRouteRendersARunsValues(t *testing.T) {
	t.Parallel()
	m := &MCP{ToolTimeoutSeconds: 600}
	v := MCPValues{RelayCommand: `/opt/crew let/crewlet"x`, RelayArgs: []string{"mcp-relay", "/tmp/s<1>.sock"},
		Socket: "/tmp/s<1>.sock", Work: "/w"}
	got := m.Render(`{"command":{relay_command_json},"args":{relay_args_json},"argv":{relay_argv_json},`+
		`"timeout":{tool_timeout_ms},"secs":{tool_timeout_seconds}} {relay_command} {socket} {work}/x`, v)
	want := `{"command":"/opt/crew let/crewlet\"x","args":["mcp-relay","/tmp/s<1>.sock"],` +
		`"argv":["/opt/crew let/crewlet\"x","mcp-relay","/tmp/s<1>.sock"],"timeout":600000,"secs":600} ` +
		`/opt/crew let/crewlet"x /tmp/s<1>.sock /w/x`
	if got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}
