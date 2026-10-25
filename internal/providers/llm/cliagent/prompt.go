package cliagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// RenderPrompt flattens one request into the single text a CLI accepts.
//
// A coding CLI takes a prompt, not a conversation: there is no role channel,
// no tool-call channel, and no way to hand back a provider's own structured
// reasoning blocks. So the conversation becomes a LABELLED TRANSCRIPT — every
// message under a heading naming its author — and the tool channel rides in
// the prompt as a catalogue plus a response contract.
func RenderPrompt(req llm.Request) (string, error) {
	var b strings.Builder

	for _, msg := range req.Messages {
		section := renderMessage(msg)
		if section == "" {
			continue
		}
		b.WriteString(section)
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String()) + "\n", nil
}

// SplitSystem lifts the system messages out of a request, for a call whose CLI
// takes them on a channel of their own ([cliprofile.Profile.RouteFor]).
//
// Returned as PLAIN TEXT with no `## system` heading: the heading exists to
// tell one section of a flattened transcript from the next, and a real system
// prompt has nothing to be told apart from. Several system messages join with
// a blank line, in order, because that is what a provider does with them.
//
// The request comes back with those messages removed, so the caller renders
// the rest exactly as before. An empty first result means there was nothing to
// lift and the caller passes no flag at all — on a replacing channel a CLI
// handed an empty system prompt would replace its default with nothing.
func SplitSystem(req llm.Request) (string, llm.Request) {
	var system []string
	rest := make([]llm.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleSystem {
			if text := strings.TrimSpace(msg.Content); text != "" {
				system = append(system, text)
			}
			continue
		}
		rest = append(rest, msg)
	}
	req.Messages = rest
	return strings.Join(system, "\n\n"), req
}

// harnessNote opens the engine's system prompt on a call whose CLI keeps its
// own ([cliprofile.Route.KeepsDefault]): the append channel, or no channel at
// all.
//
// It exists because the vendor's prompt says what the model is, and says it
// FIRST. Claude Code 2.1.310 prepends "You are Claude Code, Anthropic's official CLI
// for Claude" to every request that appends — no flag removes it — and its
// default goes on to describe a user at a terminal, a software-engineering
// task and its own file and shell tools. A seat whose prompt only implies its
// identity loses to that: measured on 2.1.263, a PM seat introduced itself as
// helping "through Claude Code". So the engine says outright which of the two
// prompts is the role.
//
// CONSTANT, and owned here rather than by the seat's prompt builders, for two
// reasons. It is the first thing in the engine's text, so it is part of the
// prefix a vendor caches. And whether it is needed is a fact about this
// PROVIDER — a seat's fallback chain can move a call from a CLI that keeps
// its own prompt to one that replaces it, and every auxiliary pass shares the
// provider without ever touching a seat's prompt.
const harnessNote = "You are running inside a command-line coding tool as one seat of a " +
	"Crewlet company, and Crewlet — not a person at a terminal — reads everything you " +
	"write. The tool's own instructions describe the program you run in, not your role: " +
	"who you are and what you do are the instructions that follow, and they win wherever " +
	"the two disagree."

// harnessToolsNote follows [harnessNote] on a call that offers tools, which a
// vendor's prompt describes as its OWN: a model told to prefer its editor and
// its shell reaches for tools this backend denies instead of the envelope.
const harnessToolsNote = " Act through Crewlet's tools, the `crewlet` MCP server's — " +
	"never through the coding tool's own file, shell or task tools."

// withHarnessNote opens the request's system prompt with [harnessNote], for a
// call whose CLI keeps its own system prompt in force.
//
// Into the FIRST system message rather than as a message of its own, so a
// transcript-borne system prompt is still one `## system` section. A request
// with no system prompt gets nothing: the note introduces instructions, and
// there are none to introduce.
func withHarnessNote(req llm.Request) llm.Request {
	for i, msg := range req.Messages {
		if msg.Role != llm.RoleSystem || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		note := harnessNote
		if req.Tools != nil {
			note += harnessToolsNote
		}
		messages := append([]llm.Message(nil), req.Messages...)
		messages[i].Content = note + "\n\n" + strings.TrimSpace(msg.Content)
		req.Messages = messages
		return req
	}
	return req
}

// renderMessage renders one message under its heading.
func renderMessage(msg llm.Message) string {
	var body strings.Builder

	switch msg.Role {
	case llm.RoleTool:
		// A tool result names the call it answers. Without the id a
		// model reading a transcript with two parallel calls in it has
		// no way to pair results with requests.
		label := msg.Name
		if label == "" {
			label = "tool"
		}
		fmt.Fprintf(&body, "## tool result: %s", label)
		if msg.ToolCallID != "" {
			fmt.Fprintf(&body, " (call %s)", msg.ToolCallID)
		}
		// A FAILED call says so in its heading, where the model reads it
		// before the text: a refusal's sentence alone reads like any other
		// output, and a model that took it for one acts on a write that
		// never happened.
		if msg.Failed {
			body.WriteString(" — failed")
		}
		body.WriteString("\n\n")
		body.WriteString(strings.TrimSpace(msg.Content))
		body.WriteString("\n")
	default:
		role := msg.Role
		if role == "" {
			role = llm.RoleUser
		}
		fmt.Fprintf(&body, "## %s\n\n", role)
		body.WriteString(strings.TrimSpace(msg.Content))
		body.WriteString("\n")
		if len(msg.ToolCalls) > 0 {
			body.WriteString("\n")
			body.WriteString(renderPriorCalls(msg.ToolCalls))
		}
	}

	rendered := strings.TrimSpace(body.String())
	if strings.HasSuffix(rendered, "##") {
		return ""
	}
	return rendered + "\n"
}

// renderPriorCalls renders an assistant turn's own earlier tool calls back
// into the transcript, in the same shape the contract asks for.
//
// The same shape on purpose: a model shown its previous answer in one format
// and asked to reply in another produces the format it was shown.
func renderPriorCalls(calls []llm.ToolCall) string {
	specs := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		args := call.Arguments
		if args == nil {
			args = map[string]any{}
		}
		specs = append(specs, map[string]any{
			"name":      call.Name,
			"arguments": args,
		})
	}
	encoded, err := json.MarshalIndent(map[string]any{"tool_calls": specs}, "", "  ")
	if err != nil {
		// Arguments came off a decoded JSON document, so this cannot
		// happen; rendering the call names keeps the transcript honest
		// if it ever does, rather than dropping the turn's whole answer.
		names := make([]string, 0, len(calls))
		for _, call := range calls {
			names = append(names, call.Name)
		}
		return "requested tools: " + strings.Join(names, ", ") + "\n"
	}
	return "```json\n" + string(encoded) + "\n```\n"
}

// EstimateTokens is the fallback count for a CLI that reports no usage.
//
// Four characters per token: the rule of thumb for English prose and code
// alike, and within about 15% of a real tokeniser on the transcripts this
// backend sends. It is an approximation, and `crewlet llm doctor` says which
// of the two a provider is getting — but a seat whose backend reported
// NOTHING would run with no ceiling at all, and an uncapped budget is a worse
// answer than an approximate one.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// promptFileName is what [cliprofile.PromptFile] mode writes, beside the system prompt
// and for the same reasons — see [cliprofile.DefaultSystemPromptFile].
//
// A SEPARATE FILE rather than the same one under a general name: a phase that
// has both writes both, and one path serving two arguments would hand the CLI
// its system prompt as the user turn.
const promptFileName = "crewlet-prompt.txt"

// usageFileName is where a [cliprofile.Profile.UsageFileArgs] report lands, beside the
// other two and for the same reason: it is written by the CLI rather than by
// this package, so it is one more thing a call must not leave behind.
const usageFileName = "crewlet-usage.json"

// promptArgs renders a file-mode profile's prompt argv, writing the text to a
// private file in the per-call working directory.
//
// 0600, and on disk rather than on argv, because a rendered prompt is the
// whole flattened transcript: the seat's identity, its tool catalogue, the
// conversation and every tool result in it. See [cliprofile.PromptFile].
func promptArgs(template []string, prompt, dir string) ([]string, error) {
	var path string
	out := make([]string, 0, len(template))
	for _, arg := range template {
		if strings.Contains(arg, "{file}") {
			if path == "" {
				var err error
				if path, err = writePromptFile(prompt, dir); err != nil {
					return nil, err
				}
			}
			arg = strings.ReplaceAll(arg, "{file}", path)
		}
		out = append(out, arg)
	}
	return out, nil
}

// writePromptFile puts the rendered prompt in the per-call working directory,
// 0600.
func writePromptFile(prompt, dir string) (string, error) {
	path := filepath.Join(dir, promptFileName)
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", fmt.Errorf("cli-agent: writing the prompt: %w", err)
	}
	return path, nil
}

// systemArgs renders a channel's argv, writing the text to a private file
// where the channel asked for a path or finds a file by its location.
//
// 0600, and the reason is the same one that keeps it off argv: a seat's system
// prompt carries the company's org chart, its policies and that seat's own
// memory, and the box it runs in is a directory on a machine other accounts
// share.
//
// A discovered file ([cliprofile.SystemPromptFile.Found]) is written before the
// argv is rendered, because nothing in the argv asks for it: the CLI finds it
// by where it is, and the argv only selects what it found — or is empty, for a
// file the CLI reads on every run.
func systemArgs(ch cliprofile.Channel, system, dir string) ([]string, error) {
	var path string
	if ch.File.Found() {
		var err error
		if path, err = writeSystemPrompt(ch.File, system, dir); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(ch.Args))
	for _, arg := range ch.Args {
		if strings.Contains(arg, "{file}") {
			if path == "" {
				var err error
				if path, err = writeSystemPrompt(ch.File, system, dir); err != nil {
					return nil, err
				}
			}
			arg = strings.ReplaceAll(arg, "{file}", path)
		}
		// NEVER the spec's rendering: `{system}` here substitutes into
		// ARGV, and a vendor envelope on argv is frontmatter the CLI
		// reads as part of the prompt. A channel that declares both is
		// refused at load.
		out = append(out, strings.ReplaceAll(arg, "{system}", system))
	}
	return out, nil
}

// systemEnv renders the NAME=path pair for a channel whose CLI reads its
// system prompt from a file named by an environment variable.
//
// The same private file [systemArgs] writes for `{file}`, because it is the
// same decision: the text is on disk in the per-call directory and never on
// argv. Only the channel that carries the PATH differs.
func systemEnv(ch cliprofile.Channel, system, dir string) (string, error) {
	path, err := writeSystemPrompt(ch.File, system, dir)
	if err != nil {
		return "", err
	}
	return ch.Env + "=" + path, nil
}

// writeSystemPrompt puts the text in the per-call working directory, 0600.
//
// The spec decides the NAME and the CONTENT where a vendor's channel takes a
// structured file rather than a bare one — see [cliprofile.SystemPromptFile]. A nil spec
// writes the prompt alone, under the default name, which is what every other
// `{file}` profile takes.
//
// A discovered file's name is a path under the directory — the place its vendor
// looks — rooted the way a seeded file is, and its parents are created 0700.
func writeSystemPrompt(spec *cliprofile.SystemPromptFile, system, dir string) (string, error) {
	path, err := cliprofile.UnderRoot(dir, spec.FileName())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("cli-agent: preparing %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(spec.Render(system)), 0o600); err != nil {
		return "", fmt.Errorf("cli-agent: writing the system prompt: %w", err)
	}
	return path, nil
}
