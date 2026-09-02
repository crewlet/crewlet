package learning

import "strings"

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
