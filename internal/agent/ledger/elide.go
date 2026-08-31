// Package ledger keeps a seat honest about what it has already done.
//
// Two scopes, same doctrine, one file each:
//
//   - iteration.go — within ONE turn, across its self_iterate rounds. Each
//     round rebuilds its LLM conversation from scratch, so without a record
//     kept outside those conversations a second round starts blind: it cannot
//     tell that round one already posted to Slack, so it plans the post again
//     and the side effect fires twice.
//   - conversation.go — across TURNS of one conversation. The second comment
//     on an issue, the reply three days later in a thread.
//
// Both are built by the engine from data already in hand, never by a
// summarising LLM call. That is the whole design: the failure being prevented
// is a duplicated external side effect, and a summariser that drops the one
// line naming the delivery re-creates exactly that bug — in a place where
// nothing downstream can catch it.
//
// READS ARE MARKED, NOT MERGED WITH WRITES. Tool *results* are deliberately
// not carried across rounds or turns, so a read the next round needs must be
// re-run; reads therefore render with a "(read)" marker and the prompt permits
// re-running exactly those. Telling a model "do not repeat" a jira_get_issue
// would push it to fabricate the data instead. Across turns the rule is
// STRONGER, not weaker — a read from last Tuesday is stale by construction.
//
// The package imports nothing from crewlet. The turn context, the prompt
// builder and the API layer all hold ledger values, and a ledger that dragged
// the provider stack behind it would be held by all three.
package ledger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// elide trims text to limit RUNES with a visible ellipsis.
//
// Runes, not bytes. Slicing a Go string cuts mid-rune and yields invalid
// UTF-8 — which a JSON encoder then replaces with U+FFFD, so a truncated
// Japanese or emoji-bearing argument would reach the model as mojibake rather
// than as a short version of itself. The budgets below are character counts,
// and counting runes is what keeps them meaning that.
//
// A limit of 0 or less means unbounded, which is the contract Review's
// single-iteration evidence log depends on: it stays verbatim.
func elide(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for i := range text {
		if count == limit {
			return strings.TrimRight(text[:i], " \t\n\r") + "…"
		}
		count++
	}
	return text
}

// elideValue elides one argument VALUE, leaving its key intact.
//
// Per-value rather than per-blob because capping the SERIALISED object at N
// chars drops whichever keys sort last — and the discriminating argument
// (channel, key, page_id) is usually the SHORTEST one. A ledger line that kept
// a 400-char message body but lost `channel` would be worse than useless: it
// would look precise while hiding which of two deliveries actually fired.
//
// Values that already fit are returned untouched, so the rendered JSON keeps
// its native types — a number stays a number rather than becoming "42".
func elideValue(value any, limit int) any {
	if s, ok := value.(string); ok {
		return elide(s, limit)
	}
	dumped, err := json.Marshal(value)
	if err != nil {
		// Unmarshalable (a channel, a func, a NaN) — there is nothing to
		// preserve the type of, so fall back to its Go rendering.
		return elide(goString(value), limit)
	}
	if utf8.RuneCount(dumped) <= limit {
		return value
	}
	return elide(string(dumped), limit)
}

// fitArguments serialises args within blobLimit by DROPPING WHOLE KEYS.
//
// Cutting the serialised string instead would remove whichever keys sort last,
// and with several bulky values even a fully per-value-elided object can
// exceed the blob budget — so a plain string cut silently discards the
// trailing identifiers. That is the precise failure elideValue exists to
// prevent, re-introduced one step later.
//
// Keys are admitted shortest-value-first (identifiers are the short ones,
// payload bodies the long ones) and any remainder is reported as "+N more", so
// a trimmed line never reads as complete.
//
// Output key order is json.Marshal's — which for a map is sorted.
// Deterministic rather than dependent on the order the
// model happened to emit its arguments in, so two identical calls render
// identically and a diff of two ledger blocks is readable.
func fitArguments(args map[string]any, blobLimit int) string {
	rendered := marshal(args)
	if blobLimit <= 0 || utf8.RuneCountInString(rendered) <= blobLimit {
		return rendered
	}

	type sized struct {
		key  string
		cost int
	}
	order := make([]sized, 0, len(args))
	for k, v := range args {
		order = append(order, sized{key: k, cost: utf8.RuneCountInString(marshal(map[string]any{k: v}))})
	}
	// Ties broken by name so the admitted set is stable across runs; a map
	// range alone would make "which key got dropped" a coin flip.
	sort.Slice(order, func(i, j int) bool {
		if order[i].cost != order[j].cost {
			return order[i].cost < order[j].cost
		}
		return order[i].key < order[j].key
	})

	kept := make(map[string]any, len(args))
	for _, s := range order {
		candidate := make(map[string]any, len(kept)+1)
		for k, v := range kept {
			candidate[k] = v
		}
		candidate[s.key] = args[s.key]
		// The first key is admitted unconditionally: a single argument that
		// blows the budget on its own still has to render, or the line loses
		// the only identifier it had.
		if len(kept) > 0 && utf8.RuneCountInString(marshal(candidate)) > blobLimit {
			break
		}
		kept[s.key] = args[s.key]
	}

	out := marshal(kept)
	if dropped := len(args) - len(kept); dropped > 0 {
		return out + " +" + itoa(dropped) + " more"
	}
	return out
}

// marshal renders a map as compact JSON, degrading to a Go rendering rather
// than to an error: a ledger line is evidence, and evidence that vanished
// because one argument held an unmarshalable value is the worst outcome
// available.
func marshal(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return goString(v)
	}
	return string(b)
}

// goString renders a value that JSON refuses. Only reached for arguments a
// tool surface should never have produced (a NaN, a cyclic structure); it
// exists so such a value costs a scruffy line rather than the whole record.
func goString(v any) string { return fmt.Sprintf("%v", v) }

// elideTail is elide from the other end: the LAST limit runes, marked.
//
// For content whose payoff is at the end — a round's produced text, where the
// draft follows the thinking that produced it. A head-preserving cut on that
// keeps the reasoning and drops the deliverable.
func elideTail(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return "…" + strings.TrimLeft(string(runes[len(runes)-limit:]), " \t\n\r")
}

// Elide trims text to limit runes with a visible ellipsis.
//
// Exported for callers outside this package that need the same marked,
// rune-safe trim — today the sub-agent runner, bounding the error text it
// reports back to its parent. Two trimming functions would eventually
// disagree about where a limit falls and whether the cut is marked.
//
// NOT for content. Every caller here bounds a string whose length is set by
// something outside the engine; the draft, the plan and the reviewer's notes
// are carried whole (see budgets.go).
func Elide(text string, limit int) string { return elide(text, limit) }
