package cliagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// probeTimeout caps a version or status probe.
//
// Far shorter than a completion's cap because these do no model work: they
// print a string and exit. Ten seconds still covers a cold Node runtime's
// first start on a loaded host, and a probe that hung for the completion
// timeout would make `doctor` look wedged when the answer is "the binary is
// broken".
const probeTimeout = 10 * time.Second

// smokePrompt is what the smoke test asks for. Short, unambiguous, and
// answerable with one tool call — a profile can look perfect and still not
// produce a parseable tool call, which is the whole reason the smoke test
// runs a real one rather than just a completion.
const smokePrompt = "Call the crewlet_smoke tool with ok set to true. Reply with only the JSON block."

// Diagnosis is what `crewlet llm doctor` reports about one provider.
//
// A struct rather than printed text so the command can render it and a test
// can assert on it — a doctor whose output only exists as fmt calls is one
// nobody writes a regression test for.
type Diagnosis struct {
	Provider   string
	Agent      string
	Model      string
	Binary     string
	BinaryPath string
	Version    string
	WrittenFor string
	StateDir   string

	// Credentials is what the shared directory holds.
	Credentials string
	// HostLogin names a login on this machine that has NOT been adopted,
	// so "no login" on a box where the CLI plainly works explains itself.
	HostLogin []string
	// TokenEnv reports whether the headless token variable is resolved.
	TokenEnv string
	// TokenUsage is "reported by CLI" or "estimated", because a budget
	// built on estimates is a different promise.
	TokenUsage string
	// Smoke is the result of a real completion with a real tool.
	Smoke string

	Problems []string
}

// Diagnose measures one provider end to end.
//
// It never returns an error: every failure it can find is a LINE in the
// report, because an operator running `doctor` wants the whole picture, and a
// command that stopped at the first problem would hide the three behind it.
func (p *Provider) Diagnose(ctx context.Context, smoke bool) Diagnosis {
	d := Diagnosis{
		Provider: p.key, Agent: p.agent, Model: p.model,
		Binary: p.profile.Binary, WrittenFor: p.profile.WrittenFor,
		StateDir: p.ws.Root(),
	}

	path, err := exec.LookPath(p.profile.Binary)
	switch {
	case err != nil:
		d.BinaryPath = "not on PATH"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"%q is not on this host's PATH — the CLI runs on the ENGINE host, so it "+
				"must be installed here; set cli.overrides.binary to an absolute path "+
				"if it lives somewhere unusual", p.profile.Binary))
	default:
		d.BinaryPath = path
		d.Version = p.probeVersion(ctx)
		if d.Version == "" {
			d.Problems = append(d.Problems, fmt.Sprintf(
				"%s %s printed no version — the profile may not match this build; "+
					"it was written for %s", path, strings.Join(p.profile.VersionArgs, " "),
				orNone(p.profile.WrittenFor)))
		}
	}

	files := p.ws.LoginFiles()
	switch {
	case len(files) > 0:
		d.Credentials = "present"
	default:
		d.Credentials = "none on disk"
		d.HostLogin = p.HostLogin("")
	}

	switch {
	case p.profile.TokenEnv == "":
		d.TokenEnv = "n/a — this CLI mints no headless token"
	case p.auth.Token != "":
		d.TokenEnv = "set"
	default:
		d.TokenEnv = "unset"
	}

	if len(p.profile.Usage.Input) > 0 || len(p.profile.Usage.Output) > 0 {
		d.TokenUsage = "reported by CLI"
	} else {
		d.TokenUsage = "estimated (4 characters per token)"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"the %q profile reads no usage figures, so budgets for seats on this "+
				"provider run on estimates", p.agent))
	}

	if d.Credentials == "none on disk" && d.TokenEnv != "set" {
		problem := fmt.Sprintf("no login of its own for %q", p.key)
		if len(d.HostLogin) > 0 {
			problem += fmt.Sprintf(
				", but this machine has one at %s — adopt it with "+
					"`crewlet llm login %s --from-host`", strings.Join(d.HostLogin, ", "), p.key)
			if len(p.profile.CaptureTokenArgs) > 0 {
				problem += fmt.Sprintf(", or mint a headless %s with `--capture-token` "+
					"(preferred: no shared refresh token)", p.profile.TokenEnv)
			}
		} else {
			problem += fmt.Sprintf(" — run `crewlet llm login %s`", p.key)
		}
		d.Problems = append(d.Problems, problem)
	}

	switch {
	case !smoke:
		d.Smoke = "skipped (-no-smoke)"
	case d.BinaryPath == "not on PATH":
		d.Smoke = "skipped — no binary to run"
	default:
		d.Smoke = p.smokeTest(ctx)
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
	}
	return d
}

// probeVersion runs the CLI's own version command.
func (p *Provider) probeVersion(ctx context.Context) string {
	if len(p.profile.VersionArgs) == 0 {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, p.profile.Binary, p.profile.VersionArgs...) //nolint:gosec // args come from a validated profile
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// smokeTest runs a REAL completion with a REAL tool.
//
// The command that matters, and the reason it is not merely a version probe:
// a profile can look perfect — binary present, login present, flags accepted
// — and still not produce a parseable tool call, because the envelope
// contract is a request to a model rather than a schema the vendor enforces.
// That failure only shows up on the first turn of a real seat otherwise.
func (p *Provider) smokeTest(ctx context.Context) string {
	comp, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: smokePrompt}},
		Tools: []llm.ToolDef{{
			Name:        "crewlet_smoke",
			Description: "Confirm the tool channel works.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":   []any{"ok"},
			},
		}},
		ToolChoice: "required",
	})
	if err != nil {
		return "failed — " + err.Error()
	}
	if len(comp.ToolCalls) == 0 {
		return fmt.Sprintf(
			"failed — the CLI answered but produced no parseable tool call, so seats on "+
				"this provider will burn a corrective round every turn. It said: %q",
			truncate(comp.Content, 200))
	}
	return fmt.Sprintf("ok — %d in / %d out", comp.InputTokens, comp.OutputTokens)
}

// Healthy reports whether the diagnosis found nothing wrong.
func (d Diagnosis) Healthy() bool { return len(d.Problems) == 0 }

// Render writes the report in the fixed-width form the docs show.
func (d Diagnosis) Render(w io.Writer) {
	line := func(label, value string) {
		fmt.Fprintf(w, "%-14s: %s\n", label, value)
	}
	line("provider", d.Provider)
	line("cli agent", d.Agent)
	if d.Model != "" {
		line("model", d.Model)
	}
	line("binary", orNone(d.BinaryPath))
	line("version", orNone(d.Version))
	line("written for", orNone(d.WrittenFor))
	line("state dir", d.StateDir)
	line("credentials", d.Credentials)
	if len(d.HostLogin) > 0 {
		line("host login", strings.Join(d.HostLogin, ", ")+" (not adopted)")
	}
	line("token env", d.TokenEnv)
	line("token usage", d.TokenUsage)
	line("smoke test", d.Smoke)
	if d.Healthy() {
		line("problems", "none")
		return
	}
	fmt.Fprintln(w, "problems:")
	for _, problem := range d.Problems {
		fmt.Fprintf(w, "  - %s\n", problem)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// LoginState is a one-word summary for `crewlet llm list`.
func (p *Provider) LoginState() string {
	switch {
	case p.ws.HasLogin():
		return "credentials"
	case p.auth.Token != "":
		return "token"
	case p.auth.Mode == AuthAPIKey && p.auth.APIKey != "":
		return "api key"
	case p.auth.Mode == AuthInheritEnv:
		if p.profile.TokenEnv != "" && os.Getenv(p.profile.TokenEnv) != "" {
			return "inherited token"
		}
		if p.profile.APIKeyEnv != "" && os.Getenv(p.profile.APIKeyEnv) != "" {
			return "inherited key"
		}
		return "none"
	default:
		return "none"
	}
}
