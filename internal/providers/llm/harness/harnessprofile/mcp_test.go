package harnessprofile

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
			"concurrency":          "read_only",
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
	if p.MCP == nil || p.Requests == nil || p.Cap == nil || p.MCP.ToolTimeout().Minutes() != 10 ||
		p.MCP.Concurrency != ConcurrencyReadOnly {
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
		"no concurrency": {map[string]any{"mcp": map[string]any{"concurrency": nil}},
			`mcp.concurrency is ""`},
		"a concurrency the engine has no word for": {map[string]any{"mcp": map[string]any{"concurrency": "parallel"}},
			`mcp.concurrency is "parallel"`},
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
		}, "mcp.failure_events is set but a run with tools prints \"json\""},
		"a record on stdout that is not a stream": {map[string]any{
			"output": "json", "event_type_path": []any{}, "text_events": []any{}, "cap": nil,
			"mcp": map[string]any{"failure_events": []any{}},
		}, "requests.source is stdout but a run with tools prints \"json\""},
		"a record on stdout a run with tools does not print": {map[string]any{
			"mcp": map[string]any{"output": "text", "failure_events": []any{}}, "cap": nil,
		}, "requests.source is stdout but a run with tools prints \"text\""},
		"a route output with no name": {map[string]any{"mcp": map[string]any{"output": "yaml"}},
			`mcp.output "yaml"`},
		"a record under the home naming no session": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/record.jsonl", "in": "home",
		}}, "does not name the run's own file through {session}"},
		"a record naming a session nothing gives the run": {map[string]any{"requests": map[string]any{
			"source": "file", "file": "{session}.jsonl", "in": "work",
		}}, "session_args does not give the run one"},
		"calls with no host prefix": {map[string]any{"requests": map[string]any{"host_prefix": ""}},
			"neither requests.host_prefix nor requests.host_namespace"},
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
		}}, "neither opens a request, names one by id, nor closes one"},
		"a whole event naming no request": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"opens": true, "whole": true, "text": []any{"t"},
				"match": []any{map[string]any{"path": []any{"type"}, "equals": "x"}}}},
		}}, "requests.events[0] is whole but names no id"},
		"a namespace nothing says is the host's": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"opens": true, "match": []any{map[string]any{"path": []any{"type"}, "equals": "x"}},
				"call": map[string]any{"name": []any{"name"}, "namespace": []any{"ns"}}}},
		}}, "reads a call's namespace but requests.host_namespace is empty"},
		"a host namespace no call is read in": {map[string]any{"requests": map[string]any{
			"host_namespace": "mcp__crewlet",
		}}, "requests.host_namespace is set but no call rule reads a namespace"},
		"an owner of a stdout record": {map[string]any{"requests": map[string]any{
			"owner": map[string]any{"path": []any{"cwd"}, "equals": "{work}"},
		}}, `requests.owner is set but requests.source is "stdout"`},
		"an owner line with nothing to compare": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/*.jsonl", "in": "home",
			"owner": map[string]any{"path": []any{"cwd"}, "equals": "{work}",
				"match": []any{map[string]any{"path": []any{"type"}}}},
		}}, "requests.owner.match[0].equals"},
		"a condition that is both a value and a prefix": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/*.jsonl", "in": "home",
			"owner": map[string]any{"path": []any{"cwd"}, "equals": "{work}",
				"match": []any{map[string]any{"path": []any{"type"}, "equals": "bind", "prefix": "bi"}}},
		}}, "requests.owner.match[0] sets both equals and prefix"},
		"an owner the run cannot know": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/*.jsonl", "in": "home",
			"owner": map[string]any{"path": []any{"thread"}, "equals": "{thread}"},
		}}, `requests.owner.equals is "{thread}"`},
		"an owner naming a session nothing gives the run": {map[string]any{"requests": map[string]any{
			"source": "file", "file": ".cli/*.jsonl", "in": "home",
			"owner": map[string]any{"path": []any{"session"}, "equals": "{session}"},
		}}, "requests.owner.equals is {session} but session_args does not give the run one"},
		"a block rule shadowing the rules after it": {map[string]any{"requests": map[string]any{
			"events": []any{map[string]any{"opens": true, "match": []any{map[string]any{"path": []any{"type"}, "equals": "x"}},
				"blocks": map[string]any{"path": []any{"content"}, "each": []any{
					map[string]any{"text": []any{"text"}},
					map[string]any{"match": []any{map[string]any{"path": []any{"type"}, "equals": "call"}},
						"call": map[string]any{"name": []any{"name"}}},
				}}}},
		}}, "requests.events[0].blocks.each[0] matches every block"},
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

// A RECORD THE GLOB CANNOT NAME IS STILL A RECORD: one under the seat's home
// whose first line says whose it is loads without {session} in its glob, and
// rules that only end requests begin them too — the shapes Codex's rollout and
// Qwen Code's stream need, which no rule of theirs opens a request in.
//
// Mutation: refuse a home glob without {session} whatever the owner, or a
// record with no rule that opens one, and these are refused.
func TestARecordNamedByItsFirstLineOrEndedByItsRulesLoads(t *testing.T) {
	t.Parallel()
	is := func(kind string) []any { return []any{map[string]any{"path": []any{"type"}, "equals": kind}} }
	p, err := load(map[string]any{"requests": map[string]any{
		"source": "file", "file": ".cli/sessions/*/rollout-*.jsonl", "in": "home",
		"owner": map[string]any{"path": []any{"payload", "cwd"}, "equals": "{work}"},
		"events": []any{
			map[string]any{"match": is("message"), "text": []any{"text"}},
			map[string]any{"match": is("usage"), "closes": true, "usage": map[string]any{"input": []any{[]any{"in"}}}},
		},
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if o := p.Requests.Owner; o == nil || o.Equals != "{work}" || !p.Requests.Events[1].Closes {
		t.Errorf("loaded %+v", p.Requests)
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

// A VALUE THE HARNESS'S OWN SERVER MINTS IS NO STORED CREDENTIAL. OpenCode's
// `serve` prints a password as it starts, and the run is handed it in
// OPENCODE_PASSWORD: the profile holds only the placeholder, so the rule above —
// about a secret the profile would hold — does not apply to it. A placeholder
// with anything written beside it is a value the profile holds again.
func TestAPasswordTheHarnessServerMintsMayRideTheRoutesEnv(t *testing.T) {
	t.Parallel()
	served := func(value string) map[string]any {
		return map[string]any{"mcp": map[string]any{
			"lifecycle": "serve",
			"serve": map[string]any{
				"args":  []any{"serve"},
				"ready": []any{map[string]any{"prefix": "server password", "as": "password"}},
			},
			"env": map[string]any{"OPENCODE_PASSWORD": value},
		}}
	}
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"{password}", true}, {"hunter2", false}, {"x{password}", false}} {
		p, err := load(served(tc.value))
		if err != nil {
			t.Fatalf("%q: Load: %v", tc.value, err)
		}
		err = p.ValidateCredentials("custom")
		if (err == nil) != tc.ok {
			t.Errorf("OPENCODE_PASSWORD=%q: err = %v, want refused %v", tc.value, err, !tc.ok)
		}
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

// served is routed on the serve lifecycle, in the shape OpenCode 2.1 was
// measured to need: a server printing its address and a password, a poll of
// its MCP servers, and a run and a usage command that are its clients.
func served() map[string]any {
	is := func(path []any, value any) []any {
		return []any{map[string]any{"path": path, "equals": value}}
	}
	return map[string]any{
		"session_args": []any{"--session", "ses_{session}"},
		"usage_args":   []any{"report", "ses_{session}"},
		"usage": map[string]any{
			"input": []any{[]any{"tokens", "input"}}, "output": []any{[]any{"tokens", "output"}},
		},
		"mcp": map[string]any{
			"lifecycle":     "serve",
			"complete_args": []any{"run", "--server", "{url}"},
			"usage_args":    []any{"report", "--server", "{url}", "ses_{session}"},
			"env":           map[string]any{"HARNESS_SERVER_PASSWORD": "{password}"},
			"serve": map[string]any{
				"args": []any{"serve", "--port", "0"},
				"ready": []any{
					map[string]any{"prefix": "server listening on ", "as": "url"},
					map[string]any{"prefix": "server password ", "as": "password"},
				},
				"poll": map[string]any{
					"args":     []any{"api", "--server", "{url}", "mcp.list"},
					"items":    []any{"data"},
					"match":    is([]any{"name"}, "crewlet"),
					"starting": is([]any{"status", "status"}, "pending"),
				},
			},
		},
	}
}

// loadServed is served, then overrides, merged over routed.
func loadServed(overrides map[string]any) (Profile, error) {
	base := routed()
	mergeInto(base, served())
	mergeInto(base, overrides)
	return Load("custom", base)
}

// The control for the serve lifecycle: its every field loads, and its ready
// values are placeholders in what the run is handed and nowhere before.
func TestAServedRouteLoads(t *testing.T) {
	t.Parallel()
	p, err := loadServed(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := p.MCP
	if !m.Serves() || m.Serve.Poll == nil || len(m.Serve.Ready) != 2 || len(m.CompleteArgs) != 3 || len(m.UsageArgs) != 4 {
		t.Fatalf("loaded %+v / %+v", m, m.Serve)
	}
	if !m.ReadyPlaceholder("--server={url}") || m.ReadyPlaceholder("{work}/x") {
		t.Error("ReadyPlaceholder does not tell a ready value from the route's own")
	}
	got := m.Render("{url} {password} {work}", MCPValues{Work: "/w",
		Ready: map[string]string{"url": "http://127.0.0.1:4096", "password": "pw"}})
	if got != "http://127.0.0.1:4096 pw /w" {
		t.Errorf("rendered %q", got)
	}
}

// A SERVER THE ENGINE CANNOT START, OR CANNOT TELL IS UP, DOES NOT LOAD — and
// neither does a value it prints put where it is read before it is printed.
//
// Mutation: drop any one rule from validateServe, or let seeds take a ready
// value, and its case loads.
func TestABrokenServeLifecycleIsRefusedByName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"an unknown lifecycle": {map[string]any{"mcp": map[string]any{"lifecycle": "daemon"}},
			`mcp.lifecycle "daemon"`},
		"serve with no server": {map[string]any{"mcp": map[string]any{"serve": nil}},
			"mcp.lifecycle is serve but mcp.serve is not set"},
		"a server on the run lifecycle": {map[string]any{"mcp": map[string]any{"lifecycle": "run"}},
			`mcp.serve is set but mcp.lifecycle is "run"`},
		"a server with no arguments": {map[string]any{"mcp": map[string]any{"serve": map[string]any{"args": []any{}}}},
			"mcp.serve.args is empty"},
		"a server nothing says is up": {map[string]any{"mcp": map[string]any{
			"serve": map[string]any{"ready": []any{}, "poll": nil},
			// Nothing printed, so nothing for the rest to name.
			"complete_args": []any{"run"}, "usage_args": []any{"report", "ses_{session}"}, "env": nil,
		}}, "names neither ready nor poll"},
		"a ready value with no prefix": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"ready": []any{map[string]any{"prefix": " ", "as": "url"}, map[string]any{"prefix": "pw ", "as": "password"}},
		}}}, "mcp.serve.ready[0].prefix is empty"},
		"a ready value that is not a word": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"ready": []any{map[string]any{"prefix": "at ", "as": "URL"}, map[string]any{"prefix": "pw ", "as": "password"}},
		}}}, `mcp.serve.ready[0].as "URL"`},
		"a ready value shadowing the route's": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"ready": []any{map[string]any{"prefix": "at ", "as": "url"}, map[string]any{"prefix": "dir ", "as": "work"}},
		}}}, `mcp.serve.ready[1].as "work"`},
		"a ready value shadowing the session": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"ready": []any{map[string]any{"prefix": "at ", "as": "url"}, map[string]any{"prefix": "s ", "as": "session"}},
		}}}, `mcp.serve.ready[1].as "session"`},
		"a ready value named twice": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"ready": []any{map[string]any{"prefix": "at ", "as": "url"}, map[string]any{"prefix": "on ", "as": "url"}},
		}}}, `mcp.serve.ready[1].as "url" names a value an earlier entry already does`},
		"a ready value in a seed": {map[string]any{"mcp": map[string]any{"seed": []any{map[string]any{
			"path": "crewlet-mcp.json", "in": "work",
			"content": `{"command":{relay_command_json},"args":{relay_args_json},"url":"{url}"}`}}}},
			"mcp.seed[0].content carries {url}, a value the server prints once it is up"},
		"a ready value in the server's own arguments": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"args": []any{"serve", "--password", "{password}"},
		}}}, "mcp.serve.args[2] carries {password}"},
		"a poll with no command": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"poll": map[string]any{"args": []any{}},
		}}}, "mcp.serve.poll.args is empty"},
		"a poll with no list": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"poll": map[string]any{"items": []any{}},
		}}}, "mcp.serve.poll.items is empty"},
		"a poll that picks no entry": {map[string]any{"mcp": map[string]any{"serve": map[string]any{
			"poll": map[string]any{"match": []any{}},
		}}}, "mcp.serve.poll.match is empty"},
		"a misspelt ready value": {map[string]any{"mcp": map[string]any{"complete_args": []any{"run", "--server", "{uri}"}}},
			"mcp.complete_args[2] carries {uri}"},
		"a ready value on the run lifecycle": {map[string]any{"mcp": map[string]any{
			"lifecycle": "run", "serve": nil,
		}}, "mcp.complete_args[2] carries {url}"},
		"a usage command replacing none": {map[string]any{"usage_args": nil},
			"mcp.usage_args is set but usage_args is not"},
		"a usage command about no session": {map[string]any{"mcp": map[string]any{
			"usage_args": []any{"report", "--server", "{url}"},
		}}, "mcp.usage_args carries no {session} placeholder"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadServed(tc.overrides)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// AN OVERRIDE OF THE SERVE LIFECYCLE IS A COPY: overriding one entry's server
// leaves the profile it was built from as it was.
func TestAServedRouteIsClonedWhole(t *testing.T) {
	t.Parallel()
	p, err := loadServed(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := p.Clone()
	c.MCP.CompleteArgs[0] = "x"
	c.MCP.UsageArgs[0] = "x"
	c.MCP.Serve.Args[0] = "x"
	c.MCP.Serve.Ready[0].As = "x"
	c.MCP.Serve.Poll.Args[0] = "x"
	c.MCP.Serve.Poll.Items[0] = "x"
	c.MCP.Serve.Poll.Match[0].Path[0] = "x"
	c.MCP.Serve.Poll.Starting[0].Path[0] = "x"
	m := p.MCP
	if m.CompleteArgs[0] != "run" || m.UsageArgs[0] != "report" || m.Serve.Args[0] != "serve" ||
		m.Serve.Ready[0].As != "url" || m.Serve.Poll.Args[0] != "api" || m.Serve.Poll.Items[0] != "data" ||
		m.Serve.Poll.Match[0].Path[0] != "name" || m.Serve.Poll.Starting[0].Path[0] != "status" {
		t.Errorf("a clone's edit reached the original: %+v / %+v", m, m.Serve.Poll)
	}
}
