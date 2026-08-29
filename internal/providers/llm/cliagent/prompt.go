package cliagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// RenderPrompt flattens one request into the single text a CLI accepts.
//
// A coding CLI takes a prompt, not a conversation: there is no role channel,
// no tool-call channel, and no way to hand back a provider's own structured
// reasoning blocks. So the conversation becomes a LABELLED TRANSCRIPT — every
// message under a heading naming its author — and the tool channel rides in
// the prompt as a catalogue plus a response contract.
//
// Thinking blocks are deliberately dropped rather than rendered. They are one
// provider's opaque structures, valid only when handed back to that same
// provider on its own wire; pasting them into a prompt for a different model
// is prose that looks like data.
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

	if len(req.Tools) > 0 {
		specs := make([]toolSpec, 0, len(req.Tools))
		for _, tool := range req.Tools {
			specs = append(specs, toolSpec{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			})
		}
		catalogue, err := RenderTools(specs)
		if err != nil {
			return "", err
		}
		b.WriteString("\n")
		b.WriteString(catalogue)
		b.WriteString("\n")
		// "none" means the caller has offered tools but does not want one
		// this round, so the contract is the permissive form; "required"
		// is the turn engine's corrective re-prompt and says so.
		b.WriteString(RenderContract(req.ToolChoice == "required"))
	}

	return strings.TrimSpace(b.String()) + "\n", nil
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
		body.WriteString("\n\n")
		body.WriteString(strings.TrimSpace(msg.Content))
		body.WriteString("\n")
	default:
		role := msg.Role
		if role == "" {
			role = llm.RoleUser
		}
		fmt.Fprintf(&body, "## %s\n\n", role)
		if msg.ReasoningContent != "" {
			// Kept, unlike thinking blocks: this is prose the model
			// wrote, and it reads as prose to any other model.
			body.WriteString(strings.TrimSpace(msg.ReasoningContent))
			body.WriteString("\n\n")
		}
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
