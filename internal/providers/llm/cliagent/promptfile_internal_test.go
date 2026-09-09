package cliagent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE TRANSCRIPT MUST NOT REACH ARGV WHEN THE CLI OFFERS A FILE.
//
// A rendered prompt is the largest and most sensitive thing this backend
// hands a CLI: the seat's identity, the tool catalogue, the conversation and
// every tool result in it. On argv all of that is readable through
// /proc/<pid>/cmdline by every account on the machine, and bounded by ARG_MAX
// (256 KB on macOS) — the ceiling the copilot and grok profiles already live
// under because their vendors offer nothing else.
//
// Asked from inside the child, because the per-call directory is removed on
// release: a parent opening the path afterwards would be reading a file the
// CLI never saw, and a file written after the spawn or unreadable to the
// child looks identical from outside.
func TestFileModeKeepsTheTranscriptOffArgvAndPrivate(t *testing.T) {
	// The leading "--" is for the FAKE, not for any real profile: the fake
	// CLI is this test binary, and Go's flag package would reject
	// `--prompt-file` as an unknown test flag. "--" ends its flag parsing
	// and leaves the rest in os.Args, which is exactly what the probe reads.
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--prompt-file"}, map[string]any{
		"prompt_mode": "file",
		"prompt_args": []any{"--", "--prompt-file", "{file}"},
	})
	secret := "the seat's whole identity and the customer's name"
	got, err := ask(t, p, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: secret}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	argv, rest, ok := strings.Cut(got.Content, "|")
	if !ok {
		t.Fatalf("the fake CLI did not report a readable prompt file: %q", got.Content)
	}
	mode, encoded, ok := strings.Cut(rest, "|")
	if !ok {
		t.Fatalf("the fake CLI did not report a readable prompt file: %q", got.Content)
	}
	if !strings.Contains(argv, "--prompt-file") {
		t.Errorf("argv = %q, want the flag the profile declared", argv)
	}
	if strings.Contains(argv, secret) {
		t.Errorf("the transcript is on argv, where /proc/<pid>/cmdline exposes it: %q", argv)
	}
	if mode != "600" {
		t.Errorf("prompt file mode = %s, want 600: the box is a directory other "+
			"accounts on the machine can reach", mode)
	}
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding what the child read: %v", err)
	}
	if !strings.Contains(string(body), secret) {
		t.Errorf("the child read %q, which is not the rendered prompt", body)
	}
}

// The path lands in the PER-CALL working directory, which is created empty
// for one call and removed on release — so a prompt cannot outlive the call
// that needed it or reach the next one. Pinned separately from the system
// prompt's file because they are two files: a phase that has both writes
// both, and one path serving two arguments would hand the CLI its system
// prompt as the user turn.
func TestPromptFileLandsBesideTheSystemPromptAndIsNotIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	promptArgv, err := promptArgs([]string{"--prompt-file", "{file}"}, "the user turn", dir)
	if err != nil {
		t.Fatal(err)
	}
	systemArgv, err := systemArgs([]string{"--system-prompt-file", "{file}"}, "the identity", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(promptArgv) != 2 || len(systemArgv) != 2 {
		t.Fatalf("prompt = %v, system = %v", promptArgv, systemArgv)
	}
	if filepath.Dir(promptArgv[1]) != dir {
		t.Errorf("the prompt landed at %q, outside the per-call directory %q", promptArgv[1], dir)
	}
	if promptArgv[1] == systemArgv[1] {
		t.Fatalf("both files are %q, so one overwrites the other", promptArgv[1])
	}
	body, err := os.ReadFile(promptArgv[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the user turn" {
		t.Errorf("the prompt file holds %q", body)
	}
}

// A file-mode profile with nothing to substitute the path into would run the
// CLI with NO PROMPT — which a vendor answers by opening an interactive
// session or printing usage, neither of which looks like the configuration
// error it is. Refused at load, naming the field to set.
func TestFileModeWithoutAFilePlaceholderIsRefused(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]any{
		"no prompt_args at all": nil,
		"a flag with no path":   {"--prompt-file"},
		"the argv placeholder":  {"--prompt", "{system}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load("custom", map[string]any{
				"binary": "x", "complete_args": []any{"exec"}, "output": "text",
				"prompt_mode": "file", "prompt_args": args,
			})
			if err == nil || !strings.Contains(err.Error(), "{file}") {
				t.Errorf("err = %v, want a refusal naming {file}", err)
			}
		})
	}
}

// THE MUSE PROFILE'S ARGV MUST PARSE, and only the real binary can say so.
//
// Two of its flags are the reason this test exists. `--disable-shell` and
// `--disable-write` are the tool denials this backend depends on, and they
// are absent from the vendor's own documented flag list — they are read off a
// working adapter verified against 0.2.1. A build that dropped them stops at
// argument parsing, and that is the failure this catches on any machine with
// the CLI installed.
//
// Skipped unless a `muse` is on PATH, and it asserts on ARGUMENT PARSING
// alone: the CLI stops for want of a credential, which is exactly far enough
// to prove the flags were understood.
func TestTheMuseProfileArgvParsesAgainstTheRealCLI(t *testing.T) {
	binary, err := exec.LookPath("muse")
	if err != nil {
		t.Skip("no muse on PATH")
	}
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}

	dir := t.TempDir()
	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "muse-spark-1.2"))
	}
	rendered, err := promptArgs(p.PromptArgs, "say hello", dir)
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, rendered...)

	cmd := exec.Command(binary, args...) //nolint:gosec // args come from the shipped profile
	cmd.Dir = dir
	// The launcher's own environment, minus anything that would sign this
	// in: the point is to reach authentication, not to spend a plan.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "MUSE_NO_AUTO_UPDATE=1"}
	combined, _ := cmd.CombinedOutput()
	got := string(combined)

	// clap reports an argument problem before it reaches authentication.
	for _, refusal := range []string{
		"unexpected argument",
		"unrecognized",
		"a value is required",
		"invalid value",
		"error: unknown",
	} {
		if strings.Contains(got, refusal) {
			t.Fatalf("the profile's argv does not parse (%q):\nargs: %v\n%s",
				refusal, args, got)
		}
	}
}

// THE LAUNCHER'S AUTO-UPDATER MUST BE OFF.
//
// The `muse` on PATH is a launcher shell script, not the agent: left alone it
// checks for a release every hour and forks a background download that
// outlives the call. Three things follow, and none of them is the operator's
// mistake to make — it swaps the binary under a running fleet with nothing to
// say so, it writes its check stamp beside the binary and therefore OUTSIDE
// the seat home this backend isolates, and the download is a child the
// process-group teardown can kill mid-write.
func TestTheMuseProfilePinsTheBinaryItWasGiven(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	if p.Env["MUSE_NO_AUTO_UPDATE"] != "1" {
		t.Errorf("env = %v, want MUSE_NO_AUTO_UPDATE=1: the launcher upgrades "+
			"itself hourly and forks the download outside the seat's home", p.Env)
	}
}

// The muse profile's own argv claims, stated once so a later edit that drops
// one has to argue with this rather than with nothing.
func TestTheMuseProfileDeniesItsToolsAndReadsItsFailures(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	joined := strings.Join(p.CompleteArgs, " ")
	// --json is load-bearing for CLASSIFICATION, not for the answer: this
	// CLI prints no failure cause on stderr ("run ended with Failed" and
	// nothing else), so without the event stream a spent plan and a dead
	// login reach this backend as a bare exit 1, no marker fires, and the
	// seat's fallback chain never moves onto a metered key.
	for _, want := range []string{"exec", "--json", "--disable-approval", "--disable-shell", "--disable-write"} {
		if !strings.Contains(joined, want) {
			t.Errorf("complete_args = %v lacks %q", p.CompleteArgs, want)
		}
	}
	if p.Output != OutputJSONL {
		t.Errorf("output = %q, want jsonl", p.Output)
	}
	// --no-session-log is this CLI's isolation: without it every turn
	// leaves a retained event log the next one can resume from.
	if !strings.Contains(joined, "--no-session-log") {
		t.Errorf("complete_args = %v keeps the session log, so one turn's "+
			"conversation reaches the next", p.CompleteArgs)
	}
	// --yolo would disable the sandbox AND trust the workspace, which is
	// what loads a checkout's AGENTS.md, rules and skills.
	if strings.Contains(joined, "--yolo") || strings.Contains(joined, "--trust-workspace") {
		t.Errorf("complete_args = %v trusts the workspace", p.CompleteArgs)
	}
	// The flags are defence in depth; run.toolset is what actually removes
	// the tools from the model's surface, and web_search is the one name
	// kept because web is the tool this backend never denies.
	var settings *SeedFile
	for i := range p.SeedFiles {
		if p.SeedFiles[i].Path == ".config/muse/settings.json" {
			settings = &p.SeedFiles[i]
		}
	}
	if settings == nil {
		t.Fatalf("the profile seeds no settings file, so `bash`, `write_file` and "+
			"`edit_file` stay on the model's surface: %v", p.SeedFiles)
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Run           struct {
			Toolset []string `json:"toolset"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(settings.Content), &doc); err != nil {
		t.Fatalf("the seeded settings file is not JSON: %v", err)
	}
	// Without it every command fails at startup with `malformed settings
	// file`, which would take the whole provider down rather than one call.
	if doc.SchemaVersion != 1 {
		t.Errorf("settings.json schema_version = %d, want 1", doc.SchemaVersion)
	}
	if !slices.Contains(doc.Run.Toolset, "web_search") {
		t.Errorf("run.toolset = %v, want web_search: a subscription seat must not "+
			"have less reach than the same CLI at a terminal", doc.Run.Toolset)
	}
	for _, denied := range []string{"bash", "bash_input", "shell", "read_file", "write_file", "edit_file"} {
		if slices.Contains(doc.Run.Toolset, denied) {
			t.Errorf("run.toolset admits %q", denied)
		}
	}
	// No usage paths, and the profile must not claim otherwise: this
	// stream carries no token counts under any key.
	if p.ReadsUsage() {
		t.Error("the profile claims to read usage from a stream that has none")
	}
}

// THE EVENT FILTER IS WHAT MAKES MUSE'S STREAM READABLE.
//
// Every line is one envelope and `payload.text` carries three different
// things: a token fragment of the reply, a tool's output, and the assembled
// reply. Reading the path alone splices the tool output into the answer and
// then repeats the answer — and this package joins a stream's text hits with
// a newline, so the fragments arrive shredded as well. This drives the real
// envelope shape through the shipped profile.
func TestTheMuseProfileReadsOneAnswerOutOfItsEventStream(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	// The shape captured from a real run: fragments, a tool result, then
	// the assembled answer on the terminal event.
	stream := strings.Join([]string{
		`{"schema_version":1,"sequence":1,"record_type":"event","payload_type":"run.lifecycle.started","payload":{"kind":"run_started"}}`,
		`{"schema_version":1,"sequence":2,"record_type":"status","payload_type":"turn.input.user","payload":{"kind":"turn_input_user","prompt":"the whole transcript"}}`,
		`{"schema_version":1,"sequence":3,"record_type":"event","payload_type":"tool.result","payload":{"kind":"tool_result","call_id":"c1","text":"wrote 5 bytes to /tmp/notes.txt"}}`,
		`{"schema_version":1,"sequence":4,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":"Created"}}`,
		`{"schema_version":1,"sequence":5,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":" the file at workspace"}}`,
		`{"schema_version":1,"sequence":6,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":" root."}}`,
		`{"schema_version":1,"sequence":7,"record_type":"event","payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"Created the file at workspace root."}}`,
	}, "\n")

	got := extract(p, stream)
	if !got.located {
		t.Fatalf("no text path resolved in a real-shaped stream: %+v", got)
	}
	if want := "Created the file at workspace root."; got.text != want {
		t.Errorf("text = %q, want %q — the answer is the terminal event's, whole", got.text, want)
	}
	if strings.Contains(got.text, "wrote 5 bytes") {
		t.Error("a tool's output was spliced into the model's answer")
	}
	if strings.Contains(got.text, "the whole transcript") {
		t.Error("the echoed prompt was read back as the answer")
	}
	if strings.Count(got.text, "Created") != 1 {
		t.Errorf("the answer is repeated: %q", got.text)
	}
	if got.reported {
		t.Error("usage was reported from a stream that carries none")
	}

	// A FAILED run resolves no text — which is what leaves the whole of
	// stdout as the haystack the limit and auth sentinels are matched
	// against, and the failure reason is only ever in there.
	failed := `{"schema_version":1,"sequence":1,"record_type":"event","payload_type":"run.terminal.failed",` +
		`"payload":{"kind":"run_terminal","terminal":"failed",` +
		`"reason":"API error 429 [request_id=x]: Rate limit exceeded. (rate_limit_error) (after 10 provider attempts)"}}`
	if out := extract(p, failed); out.located {
		t.Errorf("a failed run resolved a text path (%q), which would hide the "+
			"reason from the markers", out.text)
	}
	kind, _, matched := classifyMarkers(p, failed, "")
	if !matched || kind != llm.KindRateLimit {
		t.Errorf("the failure reason classified as %v (matched=%v), want a spent "+
			"credential — a FATAL here strands the seat's fallback chain", kind, matched)
	}
}

// The credential is where THIS CLI looks for it, and everything else it
// writes is pruned between turns.
//
// muse is XDG-native — no relocation variable of its own — so the seat home's
// XDG_CONFIG_HOME and XDG_DATA_HOME are what isolate it. A profile that named
// a config_env here would point one of them somewhere the CLI does not read.
func TestTheMuseProfileIsIsolatedByXDGAlone(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	if len(p.ConfigEnv) != 0 {
		t.Errorf("config_env = %v, but this CLI has no relocation variable", p.ConfigEnv)
	}
	for _, want := range []string{".config/muse/auth.json"} {
		if !slices.Contains(p.CredentialPaths, want) {
			t.Errorf("credential_paths = %v lacks %q", p.CredentialPaths, want)
		}
		if !slices.Contains(p.HostCredentialPaths, want) {
			t.Errorf("host_credential_paths = %v lacks %q", p.HostCredentialPaths, want)
		}
	}
	// The data directory WHOLE, because the session logs and the "personal
	// project memory" the vendor documents without locating both live under
	// it, and naming children would leave whatever this build adds next.
	if !slices.Contains(p.VolatilePaths, ".local/share/muse") {
		t.Errorf("volatile_paths = %v does not prune the data directory", p.VolatilePaths)
	}
	// And never the credential: a prune that took .config/muse would sign
	// every seat out between turns.
	for _, path := range p.VolatilePaths {
		for _, credential := range p.CredentialPaths {
			if strings.HasPrefix(credential, strings.TrimSuffix(path, "/")+"/") || credential == path {
				t.Errorf("volatile_paths %q would delete the credential %q", path, credential)
			}
		}
	}
}
