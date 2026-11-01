package harnessprofile

import (
	"strings"
	"testing"
)

// loadCustom loads a fully declared custom profile with extra fields merged in.
func loadCustom(extra map[string]any) (Profile, error) {
	base := map[string]any{
		"binary":        "my-cli",
		"complete_args": []any{"run"},
		"output":        "jsonl",
		"text_paths":    []any{[]any{"text"}},
	}
	for k, v := range extra {
		base[k] = v
	}
	return Load("custom", base)
}

// refused asserts a profile is refused with a message carrying every want.
func refused(t *testing.T, extra map[string]any, wants ...string) {
	t.Helper()
	_, err := loadCustom(extra)
	if err == nil {
		t.Fatalf("accepted %v", extra)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A LOGIN IN THE HARNESS'S OWN DATABASE NEEDS BOTH HALVES. An export with no
// import leaves every seat's home with an empty database, and an import with
// no export can never write back the token a call refreshed — which is how a
// whole fleet is logged out at the next expiry.
func TestACredentialStoreNeedsBothHalves(t *testing.T) {
	t.Parallel()
	refused(t, map[string]any{"credential_store": map[string]any{
		"export_args": []any{"auth", "export"},
	}}, "credential_store.import_args")
	refused(t, map[string]any{"credential_store": map[string]any{
		"import_args": []any{"auth", "import"},
	}}, "credential_store.export_args")
	if _, err := loadCustom(map[string]any{"credential_store": map[string]any{
		"export_args": []any{"auth", "export"},
		"import_args": []any{"auth", "import"},
	}}); err != nil {
		t.Errorf("a complete credential store was refused: %v", err)
	}
}

// The document is a credential, so it travels on stdin and stdout; a `{file}`
// would put it at a path the engine then has to remember to remove.
func TestACredentialStoreTakesNoPath(t *testing.T) {
	t.Parallel()
	refused(t, map[string]any{"credential_store": map[string]any{
		"export_args": []any{"auth", "export"},
		"import_args": []any{"auth", "import", "{file}"},
	}}, "{file}", "stdin")
}

// The shared directory keeps a credential file and the document under their
// base names, so a credential file named like the document would overwrite it
// there and in every bundle.
func TestACredentialFileMayNotShadowTheDocument(t *testing.T) {
	t.Parallel()
	refused(t, map[string]any{
		"credential_paths": []any{".my-cli/" + CredentialStoreDocument},
		"credential_store": map[string]any{
			"export_args": []any{"auth", "export"},
			"import_args": []any{"auth", "import"},
		},
	}, CredentialStoreDocument)
}

// A USAGE COMMAND MUST BE ABOUT THIS CALL. A seat's home is shared by every
// call running in it at once, so a report about "the latest session" can be a
// sub-agent's; the id travels from session_args to usage_args or the report
// is not this call's.
func TestAUsageCommandIsAskedAboutTheCallsOwnSession(t *testing.T) {
	t.Parallel()
	usage := map[string]any{
		"input":  []any{[]any{"tokens", "input"}},
		"output": []any{[]any{"tokens", "output"}},
	}
	refused(t, map[string]any{
		"session_args": []any{"--session"},
	}, "session_args", "{session}")
	refused(t, map[string]any{
		"session_args": []any{"--session", "{session}"},
		"usage_args":   []any{"usage"},
		"usage":        usage,
	}, "usage_args", "{session}")
	refused(t, map[string]any{
		"usage_args": []any{"usage", "{session}"},
		"usage":      usage,
	}, "session_args")
	refused(t, map[string]any{
		"session_args": []any{"--session", "{session}"},
		"usage_args":   []any{"usage", "{session}"},
	}, "usage.input and usage.output")
	refused(t, map[string]any{
		"session_args":    []any{"--session", "{session}"},
		"usage_args":      []any{"usage", "{session}"},
		"usage_file_args": []any{"--usage-file", "{usage_file}"},
		"usage":           usage,
	}, "usage_args and usage_file_args")

	p, err := loadCustom(map[string]any{
		"session_args": []any{"--session", "ses_{session}"},
		"usage_args":   []any{"usage", "ses_{session}"},
		"usage":        usage,
	})
	if err != nil {
		t.Fatalf("a usage command asked about the call's session was refused: %v", err)
	}
	// A report read apart from stdout is the vendor's own count whatever
	// stdout is, so the doctor must say so — on a text profile too.
	p.Output = OutputText
	if !p.ReadsUsage() {
		t.Error("a profile with a usage command reports estimated usage")
	}
}

// A DISCOVERED SYSTEM-PROMPT FILE IS FOUND BY WHERE IT IS AND SELECTED BY THE
// ARGV. A discovered file with no name writes the default bare file no harness
// looks for; one with no argv is found and never used; one whose argv carries
// the path or the text as well is two channels for one prompt — and the text,
// `{system}`, is the seat's whole system prompt on argv, where every account on
// the machine reads it; a name that leaves the working directory writes the
// prompt somewhere else; and a name that is a DIRECTORY fails every call.
func TestADiscoveredSystemPromptFileIsFoundAndSelected(t *testing.T) {
	t.Parallel()
	agent := []any{"--agent", "seat"}
	discovered := func(args []any, name string) map[string]any {
		channel := map[string]any{"file": map[string]any{"name": name, "discovered": "selected"}}
		if args != nil {
			channel["args"] = args
		}
		return map[string]any{"system_prompt": channel}
	}
	refused(t, discovered(agent, ""), "system_prompt.file is discovered but its name is empty")
	refused(t, discovered(agent, "../outside.md"), "stay inside")
	// Out and back in by the name of the directory a stand-in root used to
	// be called, which the stand-in passed and every real call refused.
	refused(t, discovered(agent, "x/../../seat/agents/seat.md"), "stay inside")
	refused(t, discovered(agent, "/etc/agents/seat.md"), "relative")
	// A directory where a file goes: `.` and `x/..` are the working
	// directory itself, which a write fails on with EISDIR, and a trailing
	// separator writes a regular FILE where the harness looks for a directory of
	// agents — every call then fails "Agent not found".
	for _, name := range []string{".", "./", ".agents/", ".agents/..", "x/../", ".agents/seat.md/."} {
		refused(t, discovered(agent, name), "must name a FILE", name)
	}
	refused(t, discovered([]any{"--agent-file", "{file}"}, ".agents/seat.md"), "{file}")
	refused(t, discovered([]any{"--prompt", "{system}"}, ".agents/seat.md"), "{system}")
	refused(t, map[string]any{"system_prompt": map[string]any{
		"env":  "SYSTEM_MD",
		"file": map[string]any{"name": ".agents/seat.md", "discovered": "selected"},
	}}, "system_prompt.args is empty")

	p, err := loadCustom(discovered(agent, ".agents/seat.md"))
	if err != nil {
		t.Fatalf("a discovered file selected by its argv was refused: %v", err)
	}
	if !p.SystemPrompt.WritesFile() {
		t.Error("a discovered system-prompt file is not reported as a file channel")
	}
	// A path that merely PASSES THROUGH a directory element is still a file,
	// and must stay accepted: the rule is about where it ends.
	for _, name := range []string{"seat.md", "./.agents/seat.md", "a/../.agents/seat.md"} {
		if _, err := loadCustom(discovered(agent, name)); err != nil {
			t.Errorf("%q names a file inside the directory and was refused: %v", name, err)
		}
	}
	// The directory a discovered name carries is the one place a name may
	// have one: an ordinary {file} channel's name stays bare.
	refused(t, map[string]any{"system_prompt": map[string]any{
		"args": []any{"--agent-file", "{file}"},
		"file": map[string]any{"name": ".agents/seat.md"},
	}}, "system_prompt.file.name", "plain file name")
}

// A CALL TAKES THE CHANNEL ITS KIND OF WORK WANTS. A seat at work keeps the
// vendor's harness prompt, which is written for an agent and moves with every
// model the vendor ships, and adds its own after it; a text function replaces
// it, because its system prompt is the whole instruction and its answer is
// parsed. Each falls back to the other rather than to the transcript — a seat's
// instructions there arrive as a user message — and only a profile with
// neither leaves the text in the transcript, under the harness's own prompt.
func TestACallTakesTheChannelItsKindOfWorkWants(t *testing.T) {
	t.Parallel()
	replace := map[string]any{"args": []any{"--system-prompt", "{file}"}}
	appendTo := map[string]any{"args": []any{"--append-system-prompt", "{file}"}}
	always := map[string]any{"file": map[string]any{"name": ".cfg/AGENTS.md", "discovered": "always"}}
	type want struct {
		field string // "" is the transcript
		keeps bool
		flag  string // the channel's first argument, "" for none
	}
	for name, tc := range map[string]struct {
		channels    map[string]any
		agent, side want
	}{
		"both channels": {
			channels: map[string]any{"system_prompt": replace, "append_system_prompt": appendTo},
			agent:    want{"append_system_prompt", true, "--append-system-prompt"},
			side:     want{"system_prompt", false, "--system-prompt"},
		},
		"only the replacing one": {
			channels: map[string]any{"system_prompt": replace},
			agent:    want{"system_prompt", false, "--system-prompt"},
			side:     want{"system_prompt", false, "--system-prompt"},
		},
		"only the appending one": {
			channels: map[string]any{"append_system_prompt": appendTo},
			agent:    want{"append_system_prompt", true, "--append-system-prompt"},
			side:     want{"append_system_prompt", true, "--append-system-prompt"},
		},
		"an appending file the harness always reads": {
			channels: map[string]any{"system_prompt": replace, "append_system_prompt": always},
			agent:    want{"append_system_prompt", true, ""},
			side:     want{"system_prompt", false, "--system-prompt"},
		},
		"neither": {
			channels: map[string]any{},
			agent:    want{"", true, ""},
			side:     want{"", true, ""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := loadCustom(tc.channels)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for _, call := range []struct {
				agent bool
				want  want
			}{{true, tc.agent}, {false, tc.side}} {
				route := p.RouteFor(call.agent)
				flag := ""
				if len(route.Channel.Args) > 0 {
					flag = route.Channel.Args[0]
				}
				if got := (want{route.Field, route.KeepsDefault, flag}); got != call.want {
					t.Errorf("agent=%v: route = %+v, want %+v", call.agent, got, call.want)
				}
				if declared := route.Channel.Declared(); declared != (call.want.field != "") {
					t.Errorf("agent=%v: the route's channel reports declared=%v for field %q",
						call.agent, declared, call.want.field)
				}
			}
		})
	}
}

// A FILE THE HARNESS READS ON EVERY RUN IS A CHANNEL ON ITS OWN. OpenCode reads the
// AGENTS.md in its configuration directory without being asked, so a channel
// made of that file alone, with no argv, is declared and writes a file — while
// a file handed over by path, or one the argv must select, declares nothing
// until the argv does, and the zero channel declares nothing at all.
func TestAFileTheHarnessAlwaysReadsDeclaresItsChannelAlone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		channel             Channel
		declared, writeFile bool
	}{
		"read on every run": {
			channel:  Channel{File: &SystemPromptFile{Name: ".cfg/AGENTS.md", Discovered: DiscoveryAlways}},
			declared: true, writeFile: true,
		},
		"found, never selected": {
			channel:  Channel{File: &SystemPromptFile{Name: ".cfg/agents/seat.md", Discovered: DiscoverySelected}},
			declared: false, writeFile: true,
		},
		"handed over by path, with no argv to hand it": {
			channel:  Channel{File: &SystemPromptFile{Name: "seat.md"}},
			declared: false, writeFile: false,
		},
		"the zero channel": {channel: Channel{}, declared: false, writeFile: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.channel.Declared(); got != tc.declared {
				t.Errorf("Declared() = %v, want %v", got, tc.declared)
			}
			if got := tc.channel.WritesFile(); got != tc.writeFile {
				t.Errorf("WritesFile() = %v, want %v", got, tc.writeFile)
			}
		})
	}
	if _, err := loadCustom(map[string]any{"append_system_prompt": map[string]any{
		"file": map[string]any{"name": ".cfg/AGENTS.md", "discovered": "always"},
	}}); err != nil {
		t.Errorf("a file the harness reads on every run, with no argv, was refused: %v", err)
	}
}

// EACH SYSTEM-PROMPT CHANNEL IS HELD TO ONE SHAPE, AND THE REFUSAL NAMES THE
// CHANNEL. The same rules on both fields, because an operator overrides either:
// one carrier per channel (a variable and an argv would hand the harness the text
// twice), never `{file}` and `{system}` together (the second puts the text the
// first kept private on argv), a discovery the engine knows, an argv that
// selects a selected file, and no path handed over for a file the harness finds by
// its location.
func TestEachSystemPromptChannelIsHeldToOneShape(t *testing.T) {
	t.Parallel()
	discovered := func(how string) map[string]any {
		return map[string]any{"name": ".cfg/agents/seat.md", "discovered": how}
	}
	for _, field := range []string{"system_prompt", "append_system_prompt"} {
		for name, tc := range map[string]struct {
			channel map[string]any
			want    string // "" is accepted; otherwise after the field name
		}{
			"an argv and a variable": {
				channel: map[string]any{"args": []any{"--sp", "{file}"}, "env": "VENDOR_SYSTEM_MD"},
				want:    ".args and " + field + ".env are both set",
			},
			"a path and the text on one argv": {
				channel: map[string]any{"args": []any{"--sp-file", "{file}", "--sp", "{system}"}},
				want:    ".args names both {file} and {system}",
			},
			"a discovery the engine does not know": {
				channel: map[string]any{"args": []any{"--agent", "seat"}, "file": discovered("sometimes")},
				want:    `.file.discovered "sometimes"`,
			},
			"a selected file nothing selects": {
				channel: map[string]any{"file": discovered("selected")},
				want:    ".file is discovered: selected but " + field + ".args is empty",
			},
			"a discovered file handed over by path as well": {
				channel: map[string]any{"args": []any{"--agent-file", "{file}"}, "file": discovered("selected")},
				want:    ".file is discovered but " + field + ".args carries {file} or {system}",
			},
			"a discovered file whose text is on argv as well": {
				channel: map[string]any{"args": []any{"--prompt", "{system}"}, "file": discovered("always")},
				want:    ".file is discovered but " + field + ".args carries {file} or {system}",
			},
			"a discovered file named by a variable as well": {
				channel: map[string]any{"env": "VENDOR_SYSTEM_MD", "file": discovered("always")},
				want:    ".file is discovered but " + field + ".env is set",
			},
			"a file the harness always reads, with no argv": {
				channel: map[string]any{"file": discovered("always")},
			},
		} {
			t.Run(field+"/"+name, func(t *testing.T) {
				t.Parallel()
				_, err := loadCustom(map[string]any{field: tc.channel})
				if tc.want == "" {
					if err != nil {
						t.Fatalf("refused: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("accepted %v", tc.channel)
				}
				if !strings.Contains(err.Error(), field+tc.want) {
					t.Errorf("the refusal does not say %q: %v", field+tc.want, err)
				}
				// "system_prompt.args" is a suffix of the other field's
				// name, so a refusal of the replacing channel must not
				// be the appending one's.
				if field == "system_prompt" && strings.Contains(err.Error(), "append_system_prompt") {
					t.Errorf("a refusal of system_prompt names append_system_prompt: %v", err)
				}
			})
		}
	}
}

// ONE FILE NAMED BY BOTH CHANNELS IS ONE WRITE, AND STILL A PATH A SEED MAY NOT
// TAKE. A call takes one channel or the other, so a vendor whose agent file
// carries the prompt with or without its own (Kimi's, with and without
// `${base_prompt}`) writes that file once per call, and two channels naming it
// are no collision. A seed at the same path is: it is replaced before the harness
// ever reads it — and it is refused once, under the first channel's name.
func TestBothChannelsMayNameOneFileButASeedMayNot(t *testing.T) {
	t.Parallel()
	agentFile := func(flag string) map[string]any {
		return map[string]any{"args": []any{flag, "{file}"}, "file": map[string]any{"name": "seat.md"}}
	}
	for name, overrides := range map[string]map[string]any{
		"one agent file": {
			"system_prompt":        agentFile("--agent-file"),
			"append_system_prompt": agentFile("--append-agent-file"),
		},
		"the default file name": {
			"system_prompt":        map[string]any{"args": []any{"--system-prompt-file", "{file}"}},
			"append_system_prompt": map[string]any{"args": []any{"--append-system-prompt-file", "{file}"}},
		},
	} {
		if _, err := loadCustom(overrides); err != nil {
			t.Errorf("%s: two channels naming one file were refused as a collision: %v", name, err)
		}
	}

	seeded := map[string]any{
		"system_prompt":        agentFile("--agent-file"),
		"append_system_prompt": agentFile("--append-agent-file"),
		"seed_files":           []any{map[string]any{"path": "seat.md", "in": "work", "content": "x"}},
	}
	_, err := loadCustom(seeded)
	if err == nil {
		t.Fatal("a seed at the system prompt's own path was accepted")
	}
	if want := `system_prompt.file.name "seat.md" is the file seed_files[0].path names too`; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not say %q: %v", want, err)
	}
	if strings.Contains(err.Error(), "append_system_prompt.file.name") {
		t.Errorf("one file was refused once per channel naming it: %v", err)
	}
	// The appending channel alone claims the path under its own name.
	delete(seeded, "system_prompt")
	if _, err := loadCustom(seeded); err == nil ||
		!strings.Contains(err.Error(), `append_system_prompt.file.name "seat.md" is the file seed_files[0].path names too`) {
		t.Errorf("a seed at the appending channel's file: err = %v, want it refused under that channel", err)
	}
}

// THE ENGINE OWNS THE CALL'S `.git`. Every call's working directory is made a
// project root of its own by a `.git` file the engine writes, so a seed at that
// path would be overwritten before the harness started — or, written after it,
// would put a real repository's pointer where the confinement belongs. Refused
// under a label naming the marker, so the operator is told the path is not
// theirs to seed rather than that two of their own fields collide.
func TestASeedMayNotClaimTheCallsProjectRootMarker(t *testing.T) {
	t.Parallel()
	_, err := loadCustom(map[string]any{
		"seed_files": []any{map[string]any{"path": ProjectRootMarker, "in": "work", "content": "gitdir: /srv/repo/.git\n"}},
	})
	if err == nil {
		t.Fatal("a work seed at the call's project-root marker was accepted")
	}
	if want := "the call's project-root marker"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name %q: %v", want, err)
	}
	// The same path in the seat's HOME is the operator's: the marker is
	// written only where the harness runs.
	if _, err := loadCustom(map[string]any{
		"seed_files": []any{map[string]any{"path": ProjectRootMarker, "in": "home", "content": "x"}},
	}); err != nil {
		t.Errorf("a home seed at %s was refused: %v", ProjectRootMarker, err)
	}
}
