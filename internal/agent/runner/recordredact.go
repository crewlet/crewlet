package runner

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A durable record's words are REDACTED BEFORE THEY ARE CUT (the package doc,
// "What a record holds of somebody's words"), here, as the record is built:
// a credential a cut runs across, cut first, leaves a head no rule knows.

// recordedText is s as a record holds it within share: redacted whole, then
// its opening kept ([keptOpening]). Only share and one marker past it of the
// redaction are read ([redact.Prefix]), where every marker a cut falls in is
// whole; the size a cut states is the redaction's, as an executor's record
// states it (internal/executorkeep's Excerpt).
func recordedText(s string, share int) string {
	shown, whole := redact.Prefix(s, share+redact.LongestMarker())
	return keptOpening(shown, whole, share)
}

// keptOpening is the text whose first bytes are shown, whole bytes long, held
// to share as the event encodes it ([kept.Record.TextOf]), and NEVER CUT INSIDE
// A MARKER the redaction wrote: one cut in part reads as the text's own words
// ("ACTED:github-token]"), so the cut moves back to where the markers begin
// ([redact.MarkersAround]). shown holds at least share bytes, or all of it.
func keptOpening(shown string, whole, share int) string {
	text, cut := recordTally.TextOf(shown, whole, share)
	if cut == 0 {
		return text
	}
	if opening, ok := recordTally.Opening(text, cut); ok {
		if begin, _, inside := redact.MarkersAround(shown, len(opening)); inside {
			text, _ = recordTally.TextOf(shown[:begin], whole, share)
		}
	}
	return text
}

// recordedPrompt is a prompt as a record holds it: redacted ([redactedPrompt]),
// then held to its share ([keptPrompt]).
func recordedPrompt(p prompts.Prompt) prompts.Prompt { return keptPrompt(redactedPrompt(p)) }

// redactedPrompt is p's text redacted whole, its outline rewritten to the
// redaction part by part where the parts' redactions put together are the
// whole's. Where they are not (a credential the rules read across two parts),
// the record keeps the whole's redaction and no outline, which a reader falls
// back from to the prompt's headings, as it does from one that does not tile.
func redactedPrompt(p prompts.Prompt) prompts.Prompt {
	whole := redact.Secrets(p.Text)
	if whole == p.Text {
		return p
	}
	if len(p.Sections) == 0 || !p.Valid() {
		return prompts.Prompt{Text: whole}
	}
	out := prompts.Prompt{Text: whole, Sections: slices.Clone(p.Sections)}
	var joined strings.Builder
	at := 0
	for i, s := range p.Sections {
		end := at + s.Bytes
		if i == len(p.Sections)-1 {
			// A tail no part names is the last part's, as Valid reads it.
			end = len(p.Text)
		}
		part := redact.Secrets(p.Text[at:end])
		out.Sections[i].Bytes = len(part)
		joined.WriteString(part)
		at = end
	}
	if joined.String() != whole {
		return prompts.Prompt{Text: whole}
	}
	return out
}

// recordedNarration is a record's model prose, each round's redacted. WHOLE,
// never an opening ([redact.Prefix]): the rounds share one budget by what each
// weighs ([keptNarration]), and one round is what a model wrote in one
// answer, far inside the length Prefix reads whole at.
func recordedNarration(narr []toolloop.Narration) []types.RoundNarration {
	redacted := slices.Clone(narr)
	for i := range redacted {
		redacted[i].Content = redact.Secrets(redacted[i].Content)
	}
	return roundNarration(redacted)
}

// recordedExecutions is a record's tool calls with each call's answer and
// arguments redacted ([recordedOutput], [tools.RedactedArgs]): a builtin's, an MCP
// server's and an A2A agent's answers alike, which all arrive as the loop's
// [toolloop.Execution.Output]. The model was handed them as they came.
func recordedExecutions(execs []toolloop.Execution) []types.ToolExecution {
	redacted := slices.Clone(execs)
	for i := range redacted {
		redacted[i].Output = recordedOutput(redacted[i].Output, redacted[i].OutputBytes)
		redacted[i].Args = tools.RedactedArgs(redacted[i].Args)
	}
	return toolExecutions(redacted)
}

// recordedOutput is a call's answer redacted whole, as a list within its share
// keeps it ([keptExecutions]). An answer past the whole list's share can be
// kept only as a row's opening ([keptRow]), so it is that opening, read off
// the redaction's ([keptOpening]) rather than redacted whole. One its run's
// log cut, outBytes long, is [recordedCut].
func recordedOutput(out string, outBytes int) string {
	if outBytes > 0 {
		return recordedCut(out, outBytes)
	}
	shown, whole := redact.Prefix(out, toolListBytes+redact.LongestMarker())
	if whole > len(shown) {
		return keptOpening(shown, whole, sandbox.MaxBridgeOutputBytes)
	}
	return shown
}

// recordedCut is the answer of an executor run's call whose log kept only its
// opening, raw ([sandbox.OutputOpening]), of whole bytes: the redaction of the
// opening's SETTLED lines alone ([redact.Settled]), which is the opening of
// the whole's redaction, since a credential the log's cut ran across left a
// head no rule knows. The cut it states leaves out the bytes the log measured
// past those lines, unread and so unredacted. A text the log's marker does
// not read back off is settled as it stands.
func recordedCut(out string, whole int) string {
	opening, ok := sandbox.OutputOpening(out, whole)
	if !ok {
		opening = out
	}
	settled := opening[:redact.Settled(opening)]
	shown := redact.Secrets(settled)
	return keptOpening(shown, len(shown)+max(whole-len(settled), 1), sandbox.MaxBridgeOutputBytes)
}
