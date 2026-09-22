package cliagent

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The CLI runs on the ENGINE host. A provider whose binary is missing there
// must say exactly that, because the config looks correct and the failure
// otherwise surfaces as an unexplained turn failure much later.
func TestDoctorReportsAMissingBinary(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "crewlet-no-such-cli-binary", "complete_args": []any{"-p"},
			"output": "text", "model_args": []any{},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if d.Healthy() {
		t.Fatal("a missing binary was reported as healthy")
	}
	if d.BinaryPath != "not on PATH" {
		t.Errorf("BinaryPath = %q", d.BinaryPath)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "ENGINE host") {
		t.Errorf("the problem does not say where the CLI must be installed:\n%s", joined)
	}
	if d.Smoke != "skipped — no binary to run" {
		t.Errorf("Smoke = %q, want a skip rather than a second failure", d.Smoke)
	}
}

// "No login" on a machine where the CLI plainly works must explain itself, or
// an operator goes looking for a bug that is a missing --from-host.
func TestDoctorNamesAnUnadoptedHostLogin(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostLogin(t, home, `{"token":"personal"}`)

	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
			"model_args": []any{}, "version_args": []any{"-test.run=NoSuchTest"},
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
			"token_env":             "FAKE_OAUTH_TOKEN",
			"capture_token_args":    []any{"setup-token"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
	if d.Credentials != "none on disk" {
		t.Fatalf("Credentials = %q", d.Credentials)
	}
	if len(d.HostLogin) == 0 {
		t.Fatal("the host login was not found, so the report cannot explain itself")
	}
	joined := strings.Join(d.Problems, "\n")
	for _, want := range []string{"--from-host", "--capture-token", "FAKE_OAUTH_TOKEN"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the problem does not offer %q:\n%s", want, joined)
		}
	}
}

// A budget built on estimates is a different promise from one built on the
// vendor's own counts, so the report must not let the difference pass
// silently.
func TestDoctorSaysWhenTokenCountsAreEstimated(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
			"model_args": []any{}, "credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A login on disk, so the estimate is the only finding left.
	credentials := p.Workspace().CredentialsDir()
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "creds.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
	if !strings.Contains(d.TokenUsage, "estimated") {
		t.Errorf("TokenUsage = %q", d.TokenUsage)
	}
	if !strings.Contains(strings.Join(d.Problems, "\n"), "estimates") {
		t.Errorf("an estimating profile was reported without a problem: %v", d.Problems)
	}
}

// The one failure nothing else catches: the CLI answers, and answers with
// PROSE. A seat on such a provider burns a corrective round every single
// turn, and the config looks perfect.
func TestTheSmokeTestCatchesACLIThatCannotProduceAToolCall(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
		},
		Env: map[string]string{helperEnv: "1", "FAKE_STDOUT": "I would read the file."},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.Smoke, "failed") {
		t.Fatalf("Smoke = %q, want a failure", d.Smoke)
	}
	if !strings.Contains(d.Smoke, "corrective round") {
		t.Errorf("the smoke failure does not say what it costs: %q", d.Smoke)
	}
	if d.Healthy() {
		t.Error("a provider that cannot produce a tool call was reported healthy")
	}
}

// The other failure the smoke test is now the ONLY thing that catches: the CLI
// exits 0, bills output tokens and answers with nothing at all.
//
// It used to be an error out of Complete, so the doctor merely relayed it.
// Complete returns an empty completion for it now — an empty answer is a model
// outcome and the tool loop corrects it — which makes this probe the one place
// an operator is told the difference between a CLI that produced prose and one
// that produced silence. Reporting silence as `It said: ""` sends them to the
// envelope contract for a problem only a bigger model fixes.
func TestTheSmokeTestNamesAnEmptyAnswerRatherThanQuotingIt(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "json",
			"text_paths": []any{[]any{"result"}},
			"usage":      map[string]any{"output": []any{[]any{"usage", "output_tokens"}}},
		},
		Env: map[string]string{
			helperEnv:     "1",
			"FAKE_STDOUT": `{"result":"","usage":{"output_tokens":627}}`,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.Smoke, "failed") {
		t.Fatalf("Smoke = %q, want a failure", d.Smoke)
	}
	if !strings.Contains(d.Smoke, "nothing at all") {
		t.Errorf("the smoke failure does not name the silence: %q", d.Smoke)
	}
	// The token count is the EVIDENCE that the model worked and said
	// nothing, which is what tells an operator the fix is the model rather
	// than the prompt.
	if !strings.Contains(d.Smoke, "627") {
		t.Errorf("the smoke failure does not report what the silence cost: %q", d.Smoke)
	}
	if strings.Contains(d.Smoke, `It said: ""`) {
		t.Errorf("silence was reported as an empty quotation: %q", d.Smoke)
	}
}

// And it passes on one that can, or the check is a permanent red light.
func TestTheSmokeTestPassesOnAWorkingEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	reply := "```json\n{\"message\":\"\",\"tool_calls\":" +
		"[{\"name\":\"crewlet_smoke\",\"arguments\":{\"ok\":true}}]}\n```"
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
			"usage": map[string]any{"input": []any{[]any{"in"}}},
		},
		Env: map[string]string{helperEnv: "1", "FAKE_STDOUT": reply},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if !strings.HasPrefix(d.Smoke, "ok") {
		t.Fatalf("Smoke = %q", d.Smoke)
	}
}

// The report is what an operator reads before a deploy, so every line the
// docs promise has to be in it.
func TestTheReportCarriesEveryLineTheDocsPromise(t *testing.T) {
	t.Parallel()
	d := Diagnosis{
		Provider: "subscription", Agent: "claude-code", Binary: "/usr/local/bin/claude",
		BinaryPath: "/usr/local/bin/claude", Version: "2.0.31 (Claude Code)",
		WrittenFor: "Claude Code CLI 2.x", StateDir: "/var/lib/crewlet/llm-cli/subscription",
		Credentials: "present", TokenEnv: "set", TokenUsage: "reported by CLI",
		Smoke: "ok — 812 in / 34 out",
	}
	var out strings.Builder
	d.Render(&out)
	for _, want := range []string{
		"provider", "cli agent", "binary", "version", "written for",
		"state dir", "credentials", "token env", "token usage", "smoke test", "problems",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing the %q line:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "problems      : none") {
		t.Errorf("a healthy report does not say so:\n%s", out.String())
	}
}

// THE QUOTE MUST HOLD THE FINDING IT EXISTS TO SHOW. [saidShown] is not a
// round number picked for looking sensible: the smoke test quotes what the CLI
// said *instead of* a parseable tool call, and the thing an operator has to
// recognise in it is a tool-call envelope whose keys this build does not look
// under — the finding that sends them to cli.overrides rather than to a bigger
// model. A budget that cuts the block before its shape is readable turns the
// one line that could diagnose the entry into a line that cannot.
//
// The block is measured rather than imagined: [renderPriorCalls] renders the
// exact shape the contract asks a model for, so the smoke tool's own call
// through it is the artifact the budget is sized against. Both budgets this
// constant replaced fail here — 120 cannot hold the bare block, 200 cannot
// hold it once a model puts a sentence in front of it, which is the ordinary
// case.
func TestTheProbeQuoteHoldsTheFindingItExistsToShow(t *testing.T) {
	t.Parallel()
	block := renderPriorCalls([]llm.ToolCall{{
		Name: "crewlet_smoke", Arguments: map[string]any{"ok": true},
	}})
	// A model that misreads the envelope contract answers in prose and puts
	// the block inside it; it does not emit a bare document.
	said := "Sure — I'll confirm the tool channel works by calling the smoke tool.\n\n" +
		block + "\nThat call reports the tool channel is working end to end."
	if saidShown < len(block) {
		t.Fatalf("saidShown = %d, which cannot hold the %d-byte envelope block "+
			"the finding is recognised by:\n%s", saidShown, len(block), block)
	}
	// THE WHOLE REPLY, not just the block: a budget that holds the block
	// bare and cuts it the moment a model frames it in a sentence holds it
	// in no case that actually occurs. That is precisely what the 200 this
	// constant replaced did — the block alone is 129 bytes and one lead
	// sentence puts it at 202.
	if quoted, _, _ := probeQuote("smoke", said); quoted != said {
		t.Errorf("saidShown = %d cuts a %d-byte reply that is one envelope "+
			"block with a sentence either side, which is the ordinary shape "+
			"of this finding:\n%s", saidShown, len(said), quoted)
	}
	// And it is still a QUOTE rather than the whole reply: a budget wide
	// enough to never cut is a budget that is not one, and the marker is
	// what tells a reader the rest exists.
	long := said + strings.Repeat(" and more prose.", 64)
	if shown, _, _ := probeQuote("smoke", long); !strings.HasSuffix(shown, "…") {
		t.Error("a reply past the budget is not marked as shortened")
	}
}

// AND THE WHOLE ANSWER TRAVELS WITH THE CUT, which is the only reason cutting
// it is allowed at all. [probeQuote] hands back both halves together so that
// keeping the quote while losing the recovery is not something a later edit
// can do by accident.
func TestAProbeQuoteKeepsTheWholeAnswerBesideTheCut(t *testing.T) {
	t.Parallel()
	said := strings.Repeat("x", saidShown) + "TAIL-BEYOND-THE-BUDGET"
	shown, _, kept := probeQuote("smoke", "  "+said+"  ")

	if kept == nil {
		t.Fatal("a cut quote kept nothing, so the whole answer is gone")
	}
	if kept.Answer != said {
		t.Errorf("the recoverable half was altered: %d bytes, want %d",
			len(kept.Answer), len(said))
	}
	if kept.Probe != "smoke" {
		t.Errorf("the kept answer does not name the probe it came from: %q", kept.Probe)
	}
	if strings.Contains(shown, "TAIL-BEYOND-THE-BUDGET") {
		t.Fatalf("the quote was not cut at all, so this case proves nothing "+
			"about the pairing: %d bytes", len(shown))
	}
	// MARKED, or a reader takes the remainder for the whole reply and goes
	// looking for a fault in a sentence the CLI never finished.
	if !strings.HasSuffix(shown, "…") {
		t.Errorf("a cut quote is not marked as one: %q", shown)
	}
	if !strings.HasPrefix(kept.Answer, strings.TrimSuffix(shown, "…")) {
		t.Errorf("the quote is not the opening of the answer it was cut from")
	}
	// AND Shown IS THE FIGURE ACTUALLY QUOTED, not the budget restated: a
	// report that says "the first 400" over a 398-byte quote is wrong in
	// the one place this package is asking to be believed about lengths.
	if want := len(strings.TrimSuffix(shown, "…")); kept.Shown != want {
		t.Errorf("Shown = %d, but the quote carries %d bytes", kept.Shown, want)
	}

	// A reply inside the budget is quoted verbatim, with no marker for a
	// cut that did not happen, and nothing kept — there is nothing to
	// recover, and a `probe replies` block for an uncut quote is noise.
	shown, rest, kept := probeQuote("web", "  NO-WEB  ")
	if shown != "NO-WEB" || rest != "" || kept != nil {
		t.Errorf("a short reply was altered: shown %q, rest %q, kept %v", shown, rest, kept)
	}
}

// AND THE BUDGET IS COUNTED IN BYTES ON A RUNE BOUNDARY, which is what makes
// the figure the report prints true for a reply that is not ASCII. A model
// explaining a failed fetch in a language with multi-byte characters is the
// ordinary case, not a corner of one: a bare said[:saidShown] would split the
// character on the boundary and the report would print bytes no terminal can
// render.
func TestAProbeQuoteCutsAMultiByteReplyOnACharacter(t *testing.T) {
	t.Parallel()
	// Three bytes per rune, so the budget lands INSIDE a character
	// whenever saidShown is not a multiple of three.
	said := strings.Repeat("ネ", saidShown)
	shown, _, kept := probeQuote("web", said)
	if kept == nil {
		t.Fatal("a reply well past the budget kept nothing")
	}
	quoted := strings.TrimSuffix(shown, "…")
	if !utf8.ValidString(quoted) {
		t.Errorf("the quote is not valid UTF-8: %q", quoted)
	}
	if len(quoted) > saidShown {
		t.Errorf("the quote is %d bytes, past the %d-byte budget", len(quoted), saidShown)
	}
	if kept.Shown != len(quoted) {
		t.Errorf("Shown = %d over a %d-byte quote, so the report would name a "+
			"boundary the cut did not fall on", kept.Shown, len(quoted))
	}
	if kept.Answer != said {
		t.Error("the recoverable half was shortened too, so it recovers nothing")
	}
}

// AND THE REPORT SAYS WHERE, because the operator who needs the rest is
// reading the report rather than the source. A `…` with no destination leaves
// them exactly where an unmarked cut would: knowing something is missing and
// having nowhere to go for it.
//
// THE DESTINATION HAS TO EXIST ON THIS RUN, which rules out a debug log line:
// `doctor` is not `run` — it takes no `-log-level` flag and reads no
// `logging:` block, and operatorLogLevel leaves every non-`run` command at
// warn unless $CREWLET_LOG_LEVEL was exported BEFORE the invocation, a lever
// that turns up the next run rather than the one already on the screen. And
// re-running buys three more real completions for a DIFFERENT reply. The
// pointer therefore names the report's own `probe replies` block, and nothing
// that only works after a second, differently-configured run — which is what
// the dead-route list below is checking.
func TestACutProbeQuoteSaysWhereTheRestIs(t *testing.T) {
	t.Parallel()

	shown, rest, kept := probeQuote("smoke", strings.Repeat("x", saidShown)+"BEYOND")
	if rest == "" {
		t.Fatal("a cut quote points the reader nowhere")
	}
	if !strings.Contains(rest, "probe replies") {
		t.Errorf("the pointer does not name the block the rest is in: %q", rest)
	}
	// THE FIGURE IT QUOTES IS THE ONE IT CUT AT, or the sentence sends a
	// reader looking for bytes that are not there.
	if !strings.Contains(rest, strconv.Itoa(len(strings.TrimSuffix(shown, "…")))) {
		t.Errorf("the pointer does not state the bytes actually quoted: %q", rest)
	}
	if !strings.Contains(rest, strconv.Itoa(len(kept.Answer))) {
		t.Errorf("the pointer does not state the size of the whole answer: %q", rest)
	}
	// AND IT NAMES NOTHING THE COMMAND CANNOT DELIVER. Each of these was a
	// route that read like a remedy and reached nothing on a `doctor` run:
	// the first two are config this command never loads, and the last is a
	// log line it never emits at its own level.
	for _, dead := range []string{"logging.level", "-log-level", "CREWLET_LOG_LEVEL",
		"cli_agent_probe_answer"} {
		if strings.Contains(rest, dead) {
			t.Errorf("the pointer names %q, which `doctor` never reaches: %q", dead, rest)
		}
	}

	// AND SAYS NOTHING WHEN NOTHING WAS CUT. A standing sentence about a
	// block at the foot of the report, on every quoted failure, is noise
	// that trains a reader to skip the line it is attached to.
	if _, rest, _ := probeQuote("web", "NO-WEB"); rest != "" {
		t.Errorf("an uncut quote carries a recovery pointer anyway: %q", rest)
	}
}

// AND THE BLOCK IT NAMES IS ACTUALLY PRINTED, with the whole answer in it.
//
// THE POINT OF THE WHOLE PAIRING: the only way to assert on a log line is to
// point the process-wide sink at a buffer, which [logging.Configure]'s own doc
// records as a measured failure and this package runs its cases in parallel.
// A rendered report is assertable.
func TestTheReportPrintsTheWholeAnswerItPromised(t *testing.T) {
	t.Parallel()
	whole := strings.Repeat("x", saidShown) + "BEYOND-THE-BUDGET"
	shown, rest, kept := probeQuote("smoke", whole)
	if kept == nil {
		t.Fatal("nothing was kept, so there is no route to assert")
	}
	d := Diagnosis{
		Provider: "sub", Agent: "claude-code", Mode: "text", Binary: "claude",
		Problems: []string{"failed — it said: " + shown + rest},
		Answers:  []ProbeAnswer{*kept},
	}
	var out bytes.Buffer
	d.Render(&out)
	report := out.String()

	if !strings.Contains(report, "probe replies:") {
		t.Fatalf("the report has no block for the answer it pointed at:\n%s", report)
	}
	if !strings.Contains(report, whole) {
		t.Errorf("the whole answer is not in the report that promised it:\n%s", report)
	}
	if !strings.Contains(report, "smoke — the whole answer") {
		t.Errorf("the block does not say which probe it belongs to:\n%s", report)
	}

	// A HEALTHY REPORT IS UNCHANGED. The block is absent when nothing was
	// cut, or every `doctor` run and every script diffing one pays for a
	// route almost none of them needs.
	out.Reset()
	Diagnosis{Provider: "sub", Agent: "claude-code", Mode: "text"}.Render(&out)
	if strings.Contains(out.String(), "probe replies") {
		t.Errorf("a report with nothing cut carries the recovery block anyway:\n%s",
			out.String())
	}
}

// THE CONSTANT THIS PACKAGE CITES IS THE ONE THAT EXISTS, and it still says
// what [saidShown]'s comment claims it says.
//
// The two are deliberately NOT one import — see [saidShown] — which is exactly
// the shape internal/httpx's own package doc names as the drift that is
// invisible because every copy still compiles. A reviewer reading that comment
// looked for `RefusalDetail`, found `RefusalBytes` (2 KiB, a different
// question) and concluded the citation was invented; this is what stops the
// citation from ever being invented, in either direction.
func TestSaidShownAgreesWithTheRefusalDetailItCites(t *testing.T) {
	t.Parallel()
	if saidShown != httpx.RefusalDetail {
		t.Errorf("saidShown = %d and httpx.RefusalDetail = %d — [saidShown]'s comment "+
			"claims the two answer one question with one number, so either move "+
			"this constant back or rewrite that paragraph to state the real "+
			"relationship", saidShown, httpx.RefusalDetail)
	}
	if saidShown == httpx.RefusalBytes {
		t.Errorf("saidShown = %d is now httpx.RefusalBytes too, so the paragraph "+
			"distinguishing the two constants no longer distinguishes anything",
			saidShown)
	}
}
