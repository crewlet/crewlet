package runner

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
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

// recordedDiagnostic is a failure's text as a record holds it: redacted, then
// clipped as every event's diagnostic is ([events.ClipDiagnostic]), keeping
// only as much of the redaction as the clip reads.
func recordedDiagnostic(text string) string {
	opening, _ := redact.Prefix(text, events.MaxDiagnosticBytes+1)
	return events.ClipDiagnostic(opening)
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
// arguments redacted ([recordedOutput], [recordedArgs]): a builtin's, an MCP
// server's and an A2A agent's answers alike, which all arrive as the loop's
// [toolloop.Execution.Output]. The model was handed them as they came.
func recordedExecutions(execs []toolloop.Execution) []types.ToolExecution {
	redacted := slices.Clone(execs)
	for i := range redacted {
		redacted[i].Output = recordedOutput(redacted[i].Output, redacted[i].OutputBytes)
		redacted[i].Args = recordedArgs(redacted[i].Args)
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

// credentialValue is what a string or number under a password's key becomes
// where that key names a container ([redactedValue]): the marker the engine's
// other readers of a key write (internal/sandbox/executor's scrub).
const credentialValue = redact.Marker + "credential]"

// recordedArgs is a call's arguments as a record holds them, REDACTED AS
// VALUES and re-encoded, never as the JSON text, whose redaction takes a
// quoted password's quotes with it and leaves no JSON: every key and string
// redacted as the text it decodes to, and every string or number read beside
// its key too ([redactedScalar]), so a value only its key names a credential
// (a password's, an AWS secret's) goes whole. Each value is read with its own
// key alone: one member's words never redact another's.
func recordedArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	out, _ := redactedValue(args, "", false).(map[string]any)
	return out
}

// redactedValue is v, under key, with every key and string redacted, a copy;
// marked says a key above v names a password ([redact.NamesPassword]), under
// which every string and number is taken out whole. Two keys a redaction makes
// one keep both values, the later named apart.
func redactedValue(v any, key string, marked bool) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			name := redact.Secrets(k)
			for n := 2; ; n++ {
				if _, taken := out[name]; !taken {
					break
				}
				name = fmt.Sprintf("%s (%d)", redact.Secrets(k), n)
			}
			// A CONTAINER under a password's key holds it: its members are
			// read under their own keys, which need not say so.
			out[name] = redactedValue(t[k], k, marked || (holdsValues(t[k]) && redact.NamesPassword(k)))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactedValue(e, key, marked)
		}
		return out
	case nil, bool:
		return t
	}
	if marked {
		return credentialValue
	}
	return redactedScalar(v, key)
}

// holdsValues reports whether v is an object or an array.
func holdsValues(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// redactedScalar is a string or a number under key as a record holds it: a
// string redacted as its own text, and then read again as the one member it
// is, `{"key":value}`, as the rules read a document. Where that reading takes
// out more, the key names the value a credential, and it is the markers the
// member's redaction wrote past the key: the value whole. A value under a
// password's key goes whole whatever the reading.
func redactedScalar(v any, key string) any {
	own := v
	if s, ok := v.(string); ok {
		own = redact.Secrets(s)
	}
	if key == "" || redact.Secrets(key) != key {
		return own
	}
	if taken := takenByKey(own, key); taken != "" {
		return taken
	}
	if redact.NamesPassword(key) {
		return credentialValue
	}
	return own
}

// takenByKey is what the redaction of own, a value already redacted as its
// own text, writes past key as the member under it, where it takes anything
// out: its markers, or [credentialValue] where it wrote none there; "" where
// it takes nothing, the key naming no credential.
func takenByKey(own any, key string) string {
	member, err := plainjson.Marshal(map[string]any{key: own}, "")
	if err != nil {
		return ""
	}
	read := redact.Secrets(string(member))
	if read == string(member) {
		return ""
	}
	name, err := plainjson.Marshal(key, "")
	if err == nil {
		if taken := markersIn(strings.TrimPrefix(read, "{"+string(name)+":")); taken != "" {
			return taken
		}
	}
	return credentialValue
}

// markersIn is the markers the redaction wrote in s, in order, with none of
// s's own words between them ([redact.OwnWords]).
func markersIn(s string) string {
	var b strings.Builder
	at := 0
	for from, words := range redact.OwnWords(s) {
		b.WriteString(s[at:from])
		at = from + len(words)
	}
	b.WriteString(s[at:])
	return b.String()
}
