package cliprofile

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

// A LOGIN IN THE CLI'S OWN DATABASE NEEDS BOTH HALVES. An export with no
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
// ARGV. A discovered file with no name writes the default bare file no CLI
// looks for; one with no argv is found and never used; one whose argv carries
// the path or the text as well is two channels for one prompt — and the text,
// `{system}`, is the seat's whole system prompt on argv, where every account on
// the machine reads it; a name that leaves the working directory writes the
// prompt somewhere else; and a name that is a DIRECTORY fails every call.
func TestADiscoveredSystemPromptFileIsFoundAndSelected(t *testing.T) {
	t.Parallel()
	discovered := func(name string) map[string]any {
		return map[string]any{"name": name, "discovered": true}
	}
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent", "seat"},
		"system_prompt_file": discovered(""),
	}, "name is empty")
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent", "seat"},
		"system_prompt_file": discovered("../outside.md"),
	}, "stay inside")
	// Out and back in by the name of the directory a stand-in root used to
	// be called, which the stand-in passed and every real call refused.
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent", "seat"},
		"system_prompt_file": discovered("x/../../seat/agents/seat.md"),
	}, "stay inside")
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent", "seat"},
		"system_prompt_file": discovered("/etc/agents/seat.md"),
	}, "relative")
	// A directory where a file goes: `.` and `x/..` are the working
	// directory itself, which a write fails on with EISDIR, and a trailing
	// separator writes a regular FILE where the CLI looks for a directory of
	// agents — every call then fails "Agent not found".
	for _, name := range []string{".", "./", ".agents/", ".agents/..", "x/../", ".agents/seat.md/."} {
		refused(t, map[string]any{
			"system_prompt_args": []any{"--agent", "seat"},
			"system_prompt_file": discovered(name),
		}, "must name a FILE", name)
	}
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent-file", "{file}"},
		"system_prompt_file": discovered(".agents/seat.md"),
	}, "{file}")
	refused(t, map[string]any{
		"system_prompt_args": []any{"--prompt", "{system}"},
		"system_prompt_file": discovered(".agents/seat.md"),
	}, "{system}")
	refused(t, map[string]any{
		"system_prompt_env":  "SYSTEM_MD",
		"system_prompt_file": discovered(".agents/seat.md"),
	}, "system_prompt_args is empty")

	p, err := loadCustom(map[string]any{
		"system_prompt_args": []any{"--agent", "seat"},
		"system_prompt_file": discovered(".agents/seat.md"),
	})
	if err != nil {
		t.Fatalf("a discovered file selected by its argv was refused: %v", err)
	}
	if !p.WritesSystemPromptFile() {
		t.Error("a discovered system-prompt file is not reported as a file channel")
	}
	// A path that merely PASSES THROUGH a directory element is still a file,
	// and must stay accepted: the rule is about where it ends.
	for _, name := range []string{"seat.md", "./.agents/seat.md", "a/../.agents/seat.md"} {
		if _, err := loadCustom(map[string]any{
			"system_prompt_args": []any{"--agent", "seat"},
			"system_prompt_file": discovered(name),
		}); err != nil {
			t.Errorf("%q names a file inside the directory and was refused: %v", name, err)
		}
	}
	// The directory a discovered name carries is the one place a name may
	// have one: an ordinary {file} channel's name stays bare.
	refused(t, map[string]any{
		"system_prompt_args": []any{"--agent-file", "{file}"},
		"system_prompt_file": map[string]any{"name": ".agents/seat.md"},
	}, "plain file name")
}
