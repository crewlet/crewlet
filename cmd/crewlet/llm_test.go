package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// bundleProvider is a cli-agent provider with a credential directory of its
// own and nothing else — import and export never run the vendor's binary, so
// none has to exist.
func bundleProvider(t *testing.T) []cliAgentProvider {
	t.Helper()
	p, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: "custom", StateDir: t.TempDir(),
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "true", "complete_args": []any{"-p"}, "model_args": []any{},
			"output":                "text",
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	return []cliAgentProvider{{key: "sub", provider: p}}
}

// EXPORT WITHOUT IMPORT IS A DEAD END: the documented way to move a login
// onto another host ended at a blob in a terminal, because nothing read it
// back.
func TestALoginBundleRoundTripsThroughTheCLI(t *testing.T) {
	t.Parallel()
	source := bundleProvider(t)
	creds := filepath.Join(source[0].provider.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"moved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := source[0].provider.ExportBundle()
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	target := bundleProvider(t)
	var out bytes.Buffer
	if err := importLLM(target, "sub", strings.NewReader(blob+"\n"), &out); err != nil {
		t.Fatalf("importLLM: %v", err)
	}
	restored, err := os.ReadFile(
		filepath.Join(target[0].provider.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatalf("the bundle restored nothing: %v", err)
	}
	if string(restored) != `{"token":"moved"}` {
		t.Errorf("restored %q", restored)
	}
	if !strings.Contains(out.String(), "doctor sub") {
		t.Errorf("the run does not say how to verify it: %s", out.String())
	}
}

// AN EXISTING LOGIN IS NOT OVERWRITTEN, and the refusal is REPORTED rather
// than swallowed: a host that has been running holds the fresher refresh
// token, and "nothing happened" and "restored" look identical from outside.
func TestImportingOverAnExistingLoginIsRefusedLoudly(t *testing.T) {
	t.Parallel()
	target := bundleProvider(t)
	creds := filepath.Join(target[0].provider.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"already here"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := importLLM(target, "sub", strings.NewReader("anything"), &out)
	if err == nil {
		t.Fatal("a bundle was restored over a live login")
	}
	if !strings.Contains(err.Error(), "logout sub") {
		t.Errorf("the error does not name the way out: %v", err)
	}
	// AND THE LIVE CREDENTIAL IS UNTOUCHED.
	got, readErr := os.ReadFile(creds)
	if readErr != nil || string(got) != `{"token":"already here"}` {
		t.Errorf("the existing login was disturbed: %q %v", got, readErr)
	}
}

// AN EMPTY STDIN NAMES THE PIPE, because the natural mistake is running the
// command with nothing feeding it.
func TestImportingNothingSaysHowToPipeABundle(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := importLLM(bundleProvider(t), "sub", strings.NewReader("   \n"), &out)
	if err == nil {
		t.Fatal("an empty stdin imported")
	}
	if !strings.Contains(err.Error(), "llm export sub |") {
		t.Errorf("the error does not show the pipe: %v", err)
	}
}

// -print-token WRITES A CREDENTIAL TO STDOUT and must refuse a terminal: a
// token in a scrollback outlives the command, and a screen-share or a shell
// history outlives the scrollback.
func TestATerminalIsRecognisedSoACredentialIsNotPrintedIntoOne(t *testing.T) {
	t.Parallel()
	// /dev/null is a character device — the same class as a tty — so it
	// exercises the branch without needing one.
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	if !isTerminal(dev) {
		t.Error("a character device was not recognised as a terminal")
	}

	// A PIPE AND A BUFFER ARE NOT, or the flag would refuse the one use it
	// exists for.
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer was treated as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if isTerminal(w) {
		t.Error("a pipe was treated as a terminal")
	}
	file := filepath.Join(t.TempDir(), "token")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("a regular file was treated as a terminal")
	}
}

// QUIET IS A DEFAULT, NOT A CEILING. A half-applied migration is exactly the
// run whose detail an operator needs.
func TestOperatorCommandsTakeTheirLevelFromTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want string
	}{
		{"", "WARN"},
		{"debug", "DEBUG"},
		{"error", "ERROR"},
		// A TYPO RESOLVES TO THE DEFAULT rather than failing: a bad log
		// level must never be why an operator cannot run a migration.
		{"shout", "WARN"},
	} {
		t.Run("CREWLET_LOG_LEVEL="+tc.set, func(t *testing.T) {
			t.Setenv("CREWLET_LOG_LEVEL", tc.set)
			if got := operatorLogLevel().String(); got != tc.want {
				t.Errorf("level = %s, want %s", got, tc.want)
			}
		})
	}
}

// --- doctor over an anthropic entry ------------------------------------------

// fakeClaude is an Anthropic endpoint for the doctor: it serves a model record
// on the Models API and answers every round with the given stream.
type fakeClaude struct {
	mu   sync.Mutex
	seen []string
	// parents is the `traceparent` each request carried, in order.
	parents []string
}

func (f *fakeClaude) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

func (f *fakeClaude) traceparents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.parents)
}

// serveClaude starts it. calls is whether a round calls the smoke tool or
// answers in prose.
func serveClaude(t *testing.T, calls bool) (*fakeClaude, string) {
	t.Helper()
	f := &fakeClaude{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		f.parents = append(f.parents, r.Header.Get("traceparent"))
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"claude-opus-5-5","type":"model","display_name":"Claude Opus 5.5",`+
				`"created_at":"2026-01-01T00:00:00Z","max_tokens":128000,"max_input_tokens":1000000,`+
				`"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},`+
				`"enabled":{"supported":false}}},"effort":{"supported":true,"low":{"supported":true},`+
				`"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},`+
				`"max":{"supported":true}}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			block := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"no"}}`
			stop := "end_turn"
			if calls {
				block = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use",` +
					`"id":"tu_1","name":"crewlet_smoke","input":{"ok":true}}}`
				stop = "tool_use"
			}
			for _, e := range [][2]string{
				{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message",` +
					`"role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,` +
					`"usage":{"input_tokens":10,"output_tokens":1}}}`},
				{"content_block_start", block},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"` + stop + `"},` +
					`"usage":{"output_tokens":5}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// inProcess is the doctor's tracing as a case runs it: nothing configured, so
// its spans go to the provider internal/tracing's init installs, which
// tracingtest records on. The production half, [traceDoctor], configures the
// process's providers, which no case in a shared binary may do.
func inProcess(context.Context) (func(), error) { return func() {}, nil }

// doctorCompany writes a company whose providers are the given block, and
// returns the flags pointing doctor at it. The bootstrap path does not exist,
// so references resolve from the environment alone.
func doctorCompany(t *testing.T, providers string) []string {
	t.Helper()
	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  llm:\n"+providers+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: claude\n")
	return []string{"-company", company, "-config", filepath.Join(dir, "absent.yaml")}
}

func anthropicEntry(key, model, baseURL string) string {
	return fmt.Sprintf("    %s:\n      type: anthropic\n      model: %s\n      base_url: %s\n", key, model, baseURL)
}

const openAIEntry = "    gpt:\n      type: openai\n      model: gpt-5\n"

// AN ANTHROPIC ENTRY IS EXAMINED, which it never used to be: a model the
// capability table has drifted from is refused on every call, and nothing else
// before a seat's first turn would say so. The record is read and a round is
// sent, and an openai entry beside it is passed over rather than reported on.
func TestDoctorExaminesAnAnthropicEntry(t *testing.T) {
	t.Parallel()
	api, url := serveClaude(t, true)
	args := append([]string{"doctor"}, doctorCompany(t,
		anthropicEntry("claude", "claude-opus-5-5", url)+openAIEntry)...)
	var out, notes bytes.Buffer
	if err := llmCommand(args, &out, &notes, inProcess); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"provider      : claude\n",
		"type          : anthropic\n",
		"models api    : served — Claude Opus 5.5 (claude-opus-5-5)",
		"smoke test    : ok — called crewlet_smoke",
		"problems      : none\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "gpt") {
		t.Errorf("an openai entry was reported on:\n%s", out.String())
	}
	if got, want := api.saw(), []string{"GET /v1/models/claude-opus-5-5", "POST /v1/messages"}; !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

// EACH ENTRY IS EXAMINED IN A ROOT SPAN OF ITS OWN, and its calls nest beneath
// it: the Anthropic SDK's span of the Models API read and of the smoke round,
// each the parent the `traceparent` it sent names — which is what the Claude
// API's own spans nest under. Before the doctor opened a span, each call was a
// root of its own, and with nothing configured its sampled `traceparent` named
// a parent no backend received. An entry with problems ends its span failed.
//
// Mutation: diagnose under the caller's context instead of the span's, and
// each call's traceparent names a trace holding no llm.doctor.
func TestDoctorExaminesEachEntryInARootSpanOfItsOwn(t *testing.T) {
	t.Parallel()
	tracingtest.WatchNamed(doctorSpan)
	calls, callsURL := serveClaude(t, true)
	prose, proseURL := serveClaude(t, false)
	args := append([]string{"doctor"}, doctorCompany(t,
		anthropicEntry("claude", "claude-opus-5-5", callsURL)+
			anthropicEntry("talker", "claude-opus-5-5", proseURL))...)
	var out, notes bytes.Buffer
	if err := llmCommand(args, &out, &notes, inProcess); err == nil {
		t.Fatalf("doctor passed an entry that answers in prose:\n%s", out.String())
	}

	traces := map[string]bool{}
	for _, tc := range []struct {
		provider string
		api      *fakeClaude
		status   codes.Code
	}{
		{"claude", calls, codes.Unset},
		{"talker", prose, codes.Error},
	} {
		parents := tc.api.traceparents()
		if len(parents) != 2 {
			t.Fatalf("%s: %d requests, want the Models API read and the smoke round", tc.provider, len(parents))
		}
		traceID := traceOf(t, parents[0])
		if traceOf(t, parents[1]) != traceID {
			t.Errorf("%s: its two calls were sent in two traces, %v", tc.provider, parents)
		}
		if traces[traceID] {
			t.Errorf("%s was examined in another entry's trace", tc.provider)
		}
		traces[traceID] = true

		doctor := tracingtest.Only(t, traceID, doctorSpan)
		if doctor.Parent().IsValid() {
			t.Errorf("%s: the doctor's span has a parent, want a root", tc.provider)
		}
		if got := tracingtest.Attr(doctor, "crewlet.provider"); got != tc.provider {
			t.Errorf("crewlet.provider = %q, want %q", got, tc.provider)
		}
		if got := doctor.Status().Code; got != tc.status {
			t.Errorf("%s: the doctor's span ended %v, want %v", tc.provider, doctor.Status(), tc.status)
		}
		for _, name := range []string{"anthropic.models.retrieve", "anthropic.messages.create"} {
			call := tracingtest.Only(t, traceID, name)
			if call.Parent().SpanID() != doctor.SpanContext().SpanID() {
				t.Errorf("%s: %s is not beneath the doctor's span", tc.provider, name)
			}
		}
	}
}

// THE DOCTOR IS TRACED FROM THE `OTEL_*` ENVIRONMENT, as `crewlet run` is: a
// value that can never work is refused by name before any entry is examined,
// rather than the command tracing by a rule nobody chose. Through [run], the
// production entry, so the case is of the wiring rather than of a seam — a
// refused sampler changes no global, so the binary's other cases are
// unaffected.
//
// Mutation: drop the doctor's trace(ctx) call, and the command examines the
// entry and passes.
func TestDoctorIsTracedFromTheOTelEnvironment(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	api, url := serveClaude(t, true)
	var out, errOut bytes.Buffer
	err := run(append([]string{"llm", "doctor"},
		doctorCompany(t, anthropicEntry("claude", "claude-opus-5-5", url))...), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "OTEL_TRACES_SAMPLER") {
		t.Fatalf("err = %v, want the sampler refused by name", err)
	}
	if got := api.saw(); len(got) != 0 {
		t.Fatalf("the refused command still called the API: %v", got)
	}
}

// traceOf is the trace id a `traceparent` header names.
func traceOf(t *testing.T, traceparent string) string {
	t.Helper()
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		t.Fatalf("traceparent %q is not a W3C trace context", traceparent)
	}
	return parts[1]
}

// A PROBLEM IS A NON-ZERO EXIT, because doctor is what a deploy script gates
// on.
func TestDoctorFailsOnAnAnthropicEntryThatDoesNotCall(t *testing.T) {
	t.Parallel()
	_, url := serveClaude(t, false)
	args := append([]string{"doctor", "claude"}, doctorCompany(t, anthropicEntry("claude", "claude-opus-5-5", url))...)
	var out, notes bytes.Buffer
	err := llmCommand(args, &out, &notes, inProcess)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 providers have problems") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "answered in prose") {
		t.Errorf("the report does not say why:\n%s", out.String())
	}
}

// AN ENTRY THAT DOES NOT BUILD IS REPORTED BESIDE THE OTHERS, not instead of
// them: the engine would refuse the document over it, and an operator wants
// that and every other entry's state from one run.
func TestDoctorReportsAnEntryThatDoesNotBuildBesideTheOthers(t *testing.T) {
	t.Parallel()
	_, url := serveClaude(t, true)
	args := append([]string{"doctor"}, doctorCompany(t,
		anthropicEntry("claude", "claude-opus-5-5", url)+
			anthropicEntry("broken", `"${CREWLET_DOCTOR_TEST_UNSET_MODEL}"`, url))...)
	var out, notes bytes.Buffer
	err := llmCommand(args, &out, &notes, inProcess)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 providers have problems") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"smoke test    : ok — called crewlet_smoke",
		"provider      : broken\nproblems:\n  - it does not build",
		"CREWLET_DOCTOR_TEST_UNSET_MODEL resolved to nothing",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// AN ENTRY DOCTOR DOES NOT EXAMINE IS REFUSED BY NAME, rather than answered
// with a report of nothing — and a document with none it examines says so.
func TestDoctorNamesWhatItDoesNotExamine(t *testing.T) {
	t.Parallel()
	_, url := serveClaude(t, true)
	var out, notes bytes.Buffer
	err := llmCommand(append([]string{"doctor", "gpt"},
		doctorCompany(t, anthropicEntry("claude", "claude-opus-5-5", url)+openAIEntry)...), &out, &notes, inProcess)
	if err == nil || !strings.Contains(err.Error(), `provider "gpt" is type "openai"`) {
		t.Errorf("err = %v", err)
	}
	err = llmCommand(append([]string{"doctor", "nobody"},
		doctorCompany(t, anthropicEntry("claude", "claude-opus-5-5", url))...), &out, &notes, inProcess)
	if err == nil || !strings.Contains(err.Error(), `no provider "nobody" (doctor examines claude)`) {
		t.Errorf("err = %v", err)
	}

	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  llm:\n"+openAIEntry+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: gpt\n")
	err = llmCommand([]string{"doctor", "-company", company, "-config", filepath.Join(dir, "absent.yaml")}, &out, &notes, inProcess)
	if err == nil || !strings.Contains(err.Error(), "declares no cli-agent or anthropic providers") {
		t.Errorf("err = %v", err)
	}
}
