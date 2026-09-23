package learning

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// How this package reads JSON out of a model's answer.
//
// # One ladder, four workers
//
// Four passes here ask a model for JSON and have to cope with the answer it
// actually sends: the persistence classifier, the counterparty profiler, the
// skill synthesizer and the skill refiner, plus the compaction summary. They
// had THREE different recovery rules between them — one stripped a code fence
// and nothing else, one took the span from the first brace to the last and
// never unfenced, one did neither — so which malformations a worker survived
// was an accident of which helper its author happened to reach for.
//
// That is invisible from any single worker: a pass whose parser does not know
// about fences declines every fenced answer, and a declined answer is the same
// observable as a model with nothing to say. The pass just quietly stops
// producing, at whatever rate that model fences.
//
// # The rules, in order
//
// A candidate list rather than a single cleaned string, because each step is a
// GUESS and the earlier ones must not lose to the later. The whole text comes
// first so a clean answer parses exactly as sent; only what fails falls
// through.
//
//  1. the trimmed text itself
//  2. with an outer code fence removed
//  3. the span from the first `{` to the last `}` of (2)
//
// Step 3 last because it is the most destructive: it will happily carve a
// brace pair out of prose that was never JSON, and a caller decides what to do
// with the result. Steps that produce nothing new are dropped, so the common
// case is a one-element list and no extra decode.

// modelJSONCandidates is the ordered list of substrings worth trying to decode
// as the model's intended JSON, best guess first.
//
// Never empty for a non-empty input: the trimmed text is always the first
// candidate, so a caller that finds nothing has a real refusal to report
// rather than an empty loop.
func modelJSONCandidates(raw string) []string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	out := []string{text}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		// Not a set: the list is at most three long and order is the
		// contract, so a scan is both the cheapest and the only way to
		// keep the first occurrence winning.
		for _, seen := range out {
			if seen == candidate {
				return
			}
		}
		out = append(out, candidate)
	}
	unfenced := stripFence(text)
	add(unfenced)
	if start, end := strings.Index(unfenced, "{"), strings.LastIndex(unfenced, "}"); start >= 0 && end > start {
		add(unfenced[start : end+1])
	}
	return out
}

// stripFence unwraps the code fence a model puts around JSON it was told to
// answer bare.
//
// A model told "JSON only" fences it anyway often enough that a worker whose
// parser forgot the case silently declines every fenced answer, which looks
// exactly like a model that never has anything to say. Only the OUTER fence,
// and only when both ends are present — a lone "```" inside a body is content,
// and a procedure that legitimately quotes a shell block would lose it.
func stripFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return trimmed
	}
	// Past the opening fence and its language tag, which is whatever the
	// model wrote on the rest of that first line.
	rest := trimmed[len("```"):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	} else {
		// A one-line fence: "```{}```" has no body line to skip past.
		rest = strings.TrimPrefix(rest, "json")
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "```"))
}

// # What a worker may print when the ladder fails

// modelAnswerDetail is how much of a model's answer reaches the log line an
// operator actually SEES.
//
// 400 BYTES, which is the answer
// [github.com/crewlet/crewlet/internal/httpx.RefusalDetail] reached for the
// same question — what a refusal may paste into a log line or an error
// message — and its reasoning transfers whole: the useful content of a
// refusal is a sentence, and past a few hundred bytes the text has stopped
// explaining the failure and become one.
//
// ONE CONSTANT FOR THE WHOLE PACKAGE, which is what it was not: four bare
// 200s typed into log calls — the persistence classifier, the counterparty
// profiler, the skill refiner and the skill synthesizer — beside the
// compaction error's named 400, which already carried the paragraph above and
// a line saying to HOIST IT rather than spell it a third time. All five answer
// the identical question, how much of an answer that failed
// [modelJSONCandidates] goes somewhere a person reads, and there is no honest
// reason for the classifier's ceiling to differ from the synthesizer's. The
// four that were literals had no reason attached to them at all, which is the
// other half of why they could differ from each other without anyone noticing.
//
// Spelled here rather than imported because this package deliberately depends
// on nothing but the store and the small shared grammars — but httpx's own doc
// records this exact number drifting into six spellings, each of whose
// comments claimed to match the other five, so if a package outside this one
// ever needs it, hoist it again rather than writing a sixth.
const modelAnswerDetail = 400

// answerLogFields renders the TWO log lines one unusable model answer is
// reported on: `seen` for the line an operator reads, whose quote is bounded
// and MARKED, and `whole` for the debug twin that carries the answer entire.
//
// A FUNCTION BECAUSE THE TWO LINES ARE A PAIR, the same reason
// [compactedLogFields] is one: a bounded quote is only a shortening if the
// rest is somewhere, and a model answer that failed to decode has NO other
// copy anywhere in this system — no row is written, no event carries a
// completion's content, and the provider keeps nothing this process can ask
// for. Written as two hand-rolled log calls the debug half is what gets
// forgotten, and then the quote in the visible line is the value being
// destroyed rather than shortened. Returned as values rather than logged here
// so the pairing is exercisable by a test that needs no log sink:
// TestTheVisibleAnswerLineIsBoundedAndTheDebugTwinIsWhole.
//
// The debug half is bounded by the CALL's own max_tokens (the per-worker
// `*_budget_tokens`, a few thousand — on the order of 16 KB), which is far too
// much for a line an operator has to see and exactly right for the one they
// turn on once they have seen it.
func answerLogFields(answer string, fields ...any) (seen, whole []any) {
	return quotedPair("response", answer, modelAnswerDetail, fields...)
}

// quotedPair is the pair of log lines a value with no other copy is reported
// on: `seen` carries the caller's fields and key set to value cut at limit
// bytes by [textcut.Ellipsis], which marks the cut; `whole` carries the same
// fields and value entire, for the debug twin. [answerLogFields] is one use;
// the persist decider's unkept notes and observed directives are the others.
func quotedPair(key, value string, limit int, fields ...any) (seen, whole []any) {
	// CLONED, not appended in place: both results extend the same caller's
	// slice, and appending twice to one backing array lets the second write
	// overwrite the first result's last pair.
	seen = append(slices.Clone(fields), key, textcut.Ellipsis(value, limit))
	whole = append(slices.Clone(fields), key, value)
	return seen, whole
}

// logUnusableAnswer reports an answer no worker could use, on both lines.
//
// The visible line keeps the caller's own event name, because what an operator
// greps for is the pass that stopped producing; the debug twin is that name
// plus `_answer`, so the two are found together and neither has to be
// remembered separately.
//
// WARN for the visible line: a worker that has stopped decoding is
// indistinguishable, in every count it reports, from a model with nothing to
// say — the second needs no attention and the first needs it now.
func logUnusableAnswer(ctx context.Context, event, answer string, fields ...any) {
	seen, whole := answerLogFields(answer, fields...)
	log.WarnContext(ctx, event, seen...)
	log.DebugContext(ctx, event+"_answer", whole...)
}
