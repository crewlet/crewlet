package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Hermes Agent, as the fake plays it (`-z`): the servers its home's
// config.yaml names, `${VAR}` filled from its environment — only those the
// -t list names started, every one if there is no list — and their tools
// offered by name only with the config's tool search off; its config's shell
// hooks run (with hooks_auto_accept), the post_api_request one handed each
// response — the model's own call names, before the harness repairs one, and the
// request's counts, the prompt less its cached part — on stdin, in the call's
// directory; no id with a call; the answer, or why it failed, on stdout; and
// the run's counts in the --usage-file report (measured on 0.21.5; see the
// profile).
func init() {
	fakeHarnesses["hermes"] = fakeHarness{
		server: func() (string, []string, error) {
			config, err := hermesConfig()
			if err != nil {
				return "", nil, err
			}
			if config.Tools.ToolSearch.Enabled != "off" {
				return "", nil, fmt.Errorf("tool search is on, so the server's tools sit behind tool_call")
			}
			started := map[string]hermesServer{}
			only := strings.Split(argAfter("--toolsets"), ",")
			for name, server := range config.MCPServers {
				if argAfter("--toolsets") == "" || slices.Contains(only, name) {
					started[name] = server
				}
			}
			for name := range started {
				if name != "crewlet" {
					fmt.Fprintln(os.Stderr, "fake hermes: started the home's own MCP server", name)
					os.Exit(77)
				}
			}
			crewlet, ok := started["crewlet"]
			if !ok {
				return "", nil, fmt.Errorf("the -t list starts no crewlet server")
			}
			args := make([]string, len(crewlet.Args))
			for i, a := range crewlet.Args {
				args[i] = hermesInterpolate(a)
			}
			return hermesInterpolate(crewlet.Command), args, nil
		},
		recorder: func(fakeScript, []byte) (fakeRecorder, error) {
			config, err := hermesConfig()
			if err != nil {
				return nil, err
			}
			var hook string
			if config.HooksAutoAccept && len(config.Hooks.PostAPIRequest) > 0 {
				hook = config.Hooks.PostAPIRequest[0].Command
			}
			return &hermesRecorder{hook: hook}, nil
		},
	}
}

type hermesServer struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

type hermesConfigFile struct {
	Tools struct {
		ToolSearch struct {
			Enabled string `yaml:"enabled"`
		} `yaml:"tool_search"`
	} `yaml:"tools"`
	HooksAutoAccept bool `yaml:"hooks_auto_accept"`
	Hooks           struct {
		PostAPIRequest []struct {
			Command string `yaml:"command"`
		} `yaml:"post_api_request"`
	} `yaml:"hooks"`
	MCPServers map[string]hermesServer `yaml:"mcp_servers"`
}

func hermesConfig() (hermesConfigFile, error) {
	var config hermesConfigFile
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HERMES_HOME"), "config.yaml")) //nolint:gosec // the fake's home
	if err != nil {
		return config, err
	}
	return config, yaml.Unmarshal(raw, &config)
}

// hermesInterpolate fills `${VAR}` from the environment, keeping one that is
// unset as written, as the harness does.
func hermesInterpolate(s string) string {
	return regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`).ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := os.LookupEnv(m[2 : len(m)-1]); ok {
			return v
		}
		return m
	})
}

type hermesRecorder struct {
	hook string
	n    int
}

func (h *hermesRecorder) begin(string, []string) {}

func (h *hermesRecorder) asked(i int, r fakeResponse, _ []string) {
	h.n++
	calls := []any{}
	for j, c := range r.Calls {
		name := c.Name
		if c.Asked != "" {
			name = c.Asked
		}
		if !c.Own {
			name = "mcp__crewlet__" + name
		}
		raw, _ := json.Marshal(orEmpty(c.Args))
		calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d_%d", i+1, j), "name": name,
			"arguments": string(raw), "provider_data": nil})
	}
	finish := "stop"
	if len(r.Calls) > 0 {
		finish = "tool_calls"
	}
	// The prompt WITHOUT its cached part, which is counted apart.
	usage := map[string]any{"input_tokens": r.Usage.Input, "output_tokens": r.Usage.Output,
		"cache_read_tokens": r.Usage.CacheRead, "cache_write_tokens": r.Usage.CacheWrite, "reasoning_tokens": 0}
	response := map[string]any{"model": "fake-model-7", "finish_reason": finish, "usage": usage,
		"assistant_message": map[string]any{"role": "assistant", "content": r.Text, "tool_calls": calls}}
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "post_api_request", "tool_name": nil,
		"extra": map[string]any{"api_request_id": fmt.Sprintf("run:api:%d", h.n), "finish_reason": finish,
			"usage": usage, "response": response}})
	if h.hook == "" {
		return
	}
	// The command as the harness runs it: split as a shell would, in the
	// call's directory, the payload on stdin.
	cmd := exec.Command("sh", "-c", h.hook) //nolint:gosec // the profile's own hook
	cmd.Stdin = strings.NewReader(string(payload))
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "fake hermes: the hook failed:", err, string(out))
		os.Exit(78)
	}
}

func (h *hermesRecorder) answered(int, fakeResponse, []string, []string) {}

func (h *hermesRecorder) end(script fakeScript, total fakeUsage, last string) int {
	if path := argAfter("--usage-file"); path != "" {
		raw, _ := json.Marshal(map[string]any{"input_tokens": total.Input, "output_tokens": total.Output,
			"cache_read_tokens": total.CacheRead, "cache_write_tokens": total.CacheWrite})
		_ = os.WriteFile(path, raw, 0o600)
	}
	if script.Exit != 0 {
		fmt.Println(script.Result)
		return script.Exit
	}
	fmt.Println(last)
	return 0
}

// HERMES AGENT'S RUN IS READ FROM THE RECORD ITS OWN HOOK WRITES, AND ITS HOME
// STARTS NO SERVER BUT THE SEAT'S. The seat's server is declared in the home's
// config.yaml — which the harness reads from nowhere else, and which the profile
// rewrites every generation — with its relay command and socket filled from
// the run's environment; the -t list starts it and nothing else, and a text
// function's starts none. Each response reaches the post_api_request hook the
// same file declares, which appends it to a file in the call's directory: its
// prose, its calls as the model named them, and its counts, the prompt less
// its cached part. The harness sends no id with a call, so a call is joined by
// name and arguments; the host is told the harness waits the profile's 600 seconds
// for a call, does not re-read the list, and makes a response's calls one
// after another.
//
// Mutation: drop the route's -t list, and the planted server starts (the fake
// fails the run); turn tool search on, and the harness never reaches the seat's
// tools; drop the hook, and no request is observed; read the record's input
// as the whole prompt, and the first request is charged 100, not 140.
func TestHermesRunIsReadFromTheRecordItsHookWrites(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "hermes", routeCase, func(c *Config) {
		c.Auth = Auth{Mode: AuthSubscription}
		// A server the home's config names beside the seat's — the -t
		// list is what keeps it from starting.
		c.Overrides["seed_files"] = hermesSeedsWith(t, "  planted:\n    command: sh\n")
	})
	host := holdsTheRoute(t, p, llm.ServeOptions{
		ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsInTurn,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
	// Each request is charged its whole prompt as it is read, not only once
	// the run's report settles the total.
	if turns := host.latestTurns(); len(turns) == 0 || turns[0].Usage == nil || turns[0].Usage.Input != 140 {
		t.Errorf("the first request was told as %+v, want its whole prompt of 140", turns)
	}
}

// harnessprofileOf is the shipped profile name.
func harnessprofileOf(t *testing.T, name string) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin(name)
	if !ok {
		t.Fatalf("no built-in %s profile", name)
	}
	return p
}

// hermesSeedsWith is the shipped seeds with servers added to the config's
// mcp_servers block, as an override.
func hermesSeedsWith(t *testing.T, servers string) []any {
	t.Helper()
	p := harnessprofileOf(t, "hermes")
	var out []any
	for _, s := range p.SeedFiles {
		content := s.Content
		if s.Path == ".hermes/config.yaml" {
			if !strings.Contains(content, "mcp_servers:\n") {
				t.Fatal("the shipped config.yaml names no mcp_servers")
			}
			content = strings.Replace(content, "mcp_servers:\n", "mcp_servers:\n"+servers, 1)
		}
		out = append(out, map[string]any{"path": s.Path, "content": content})
	}
	return out
}

// A CALL THE HARNESS REPAIRED IS RECORDED AS THE MODEL ASKED FOR IT. Hermes Agent
// rewrites a call to an unknown tool to the closest listed one and runs that
// (difflib, ratio 0.7 or more), with the model's own arguments: measured, a
// model asking for mcp__crewlet__echoo had echo called. Its stream and its
// session store both show the repaired name; the post_api_request hook fires
// before the repair, so the record names what the model wrote — the name the
// tool loop's host refuses a rewritten call by, rather than running a tool the
// model never asked for.
//
// Mutation: record the repaired name, and the host is told the model asked for
// the tool that ran.
func TestHermesRecordsTheNameTheModelAskedFor(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "hermes", fakeScript{Responses: []fakeResponse{
		{Calls: []fakeCall{{Name: "search", Asked: "serch", Args: map[string]any{"q": "notes"}}}, Usage: fakeUsage{Input: 10, Output: 2}},
		{Text: "done", Usage: fakeUsage{Input: 12, Output: 1}},
	}}, func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	host := newTestHost(t, "search")
	host.handle("search", func(ctx context.Context, _ map[string]any) (string, bool) {
		host.awaitRecorded(ctx, "serch")
		return "3 results", false
	})
	if _, err := p.Complete(t.Context(), phase(host)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, calls, _, _ := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("the host was called %+v, want the repaired search", calls)
	}
	turns := host.latestTurns()
	if len(turns) == 0 || len(turns[0].Calls) != 1 || turns[0].Calls[0].Name != "serch" || !turns[0].Calls[0].Host {
		t.Errorf("the host was told %+v, want the model's own serch, the host's", turns)
	}
}

// HERMES AGENT'S SERVER WAITS AS LONG AS THE HOST IS TOLD IT DOES. Its config
// is the seat's, written once for every call, so the server's timeout there is
// a literal rather than a value rendered per run: it has to be the profile's
// tool_timeout_seconds, or the host answers a slow call later than the harness
// gives up on it (measured: a timeout of 4 gave a 10 s call up at 4.0 s).
//
// Mutation: change either number alone, and this fails.
func TestHermesServerTimeoutIsTheOneTheHostIsTold(t *testing.T) {
	t.Parallel()
	p := harnessprofileOf(t, "hermes")
	for _, s := range p.SeedFiles {
		if s.Path != ".hermes/config.yaml" {
			continue
		}
		var config struct {
			MCPServers map[string]struct {
				Timeout int `yaml:"timeout"`
			} `yaml:"mcp_servers"`
		}
		if err := yaml.Unmarshal([]byte(s.Content), &config); err != nil {
			t.Fatal(err)
		}
		if got := config.MCPServers["crewlet"].Timeout; got != p.MCP.ToolTimeoutSeconds {
			t.Errorf("the config's crewlet server times a call out at %d s, the host is told %d s",
				got, p.MCP.ToolTimeoutSeconds)
		}
		return
	}
	t.Fatal("the profile seeds no .hermes/config.yaml")
}
