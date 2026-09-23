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
// THAT RULE IS ABOUT RESULTS, AND ONLY RESULTS. Everything the RENDER cuts —
// argument values, whole arguments, read lines past the cap, a round's
// produced text, a failed call's error — is reachable whole through
// `recall_iteration` (internal/agent/builtin), which reads this same
// [Iteration] record within the turn that owns it. The line between the two is
// re-runnability: a read's answer can have MOVED since, so replaying a stale
// copy is worse than re-reading it, while an argument the model already sent
// is fixed, spent, and recoverable from nowhere else. See budgets.go, where
// every number now rests on that.
//
// The package imports nothing from crewlet. The turn context, the prompt
// builder and the API layer all hold ledger values, and a ledger that dragged
// the provider stack behind it would be held by all three.
package ledger

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
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
	slices.SortFunc(order, func(a, b sized) int {
		return cmp.Or(cmp.Compare(a.cost, b.cost), cmp.Compare(a.key, b.key))
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
//
// It walks BACK from the end rather than materialising []rune(text), so the
// work is proportional to what is kept rather than to what is passed in. The
// caller hands it a whole tool-loop transcript, which is megabytes on a long
// Execute phase — a rune slice of that allocates four bytes per character of
// input to keep a few thousand of them, on every prompt render.
func elideTail(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	i := len(text)
	for n := 0; n < limit && i > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:i])
		i -= size
	}
	if i == 0 {
		return text
	}
	return "…" + strings.TrimLeft(text[i:], " \t\n\r")
}

// elideMiddle keeps the first and last runes of text within limit, with a
// marked gap between them.
//
// Both walks are the ones [elide] and [elideTail] take — forward from the
// start and back from the end, each a rune at a time — so it counts what those
// count and cuts where they would, and the gap never splits a character.
func elideMiddle(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	headRunes := limit - limit/2
	i := 0
	for n := 0; n < headRunes && i < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	j := len(text)
	for n := 0; n < limit/2 && j > i; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:j])
		j -= size
	}
	return strings.TrimRight(text[:i], " \t\n\r") + " … " +
		strings.TrimLeft(text[j:], " \t\n\r")
}

// Elide trims text to limit runes with a visible ellipsis.
//
// Exported for callers outside this package that need the same marked,
// rune-safe trim — today the extension judge, bounding a failed call's error
// in the log it is shown. Two trimming functions would eventually disagree
// about where a limit falls and whether the cut is marked.
//
// NOT [textcut.Ellipsis], which is the tree's other shared head cut, and the
// difference is deliberate: that one counts BYTES and reads a limit of 0 as
// empty, where this one counts RUNES and reads 0 as unbounded — the contract
// Review's single-iteration evidence log depends on. See textcut's package
// doc, which says the same thing from the other side.
//
// NOT for content. Every caller here bounds a string whose length is set by
// something outside the engine; the draft, the round's own account of it and
// the reviewer's notes are carried whole (see budgets.go).
func Elide(text string, limit int) string { return elide(text, limit) }

// ElideTail trims text to its LAST limit runes, marked.
//
// The counterpart of [Elide], and the choice between them is about where the
// value's payoff sits rather than about taste. A head cut is right for a value
// that leads with what identifies it — an error chain that wraps outward, a
// title. A TAIL cut is right for a value that leads with the working and ends
// with the answer: a round's produced text, where the draft follows the
// thinking that produced it, so a head-preserving cut keeps the reasoning and
// drops the deliverable.
//
// Exported for the extension judge, whose "What it last said" block was doing
// the opposite of what its own heading promised: it head-cut the tool loop's
// aggregate text, so the block named "last" rendered what the phase said
// FIRST — and the sentence that most distinguishes a phase about to finish
// from one thrashing is the one it ends on.
//
// A limit of 0 or less means unbounded, the same contract [Elide] carries.
func ElideTail(text string, limit int) string { return elideTail(text, limit) }

// ElideMiddle keeps the HEAD and the TAIL of text within limit runes, with
// " … " marking what was left out between them.
//
// The third answer to where a value's payoff sits: at both ends. An error
// chain wraps outward, so its first words say what failed, and it ends in the
// cause it wrapped — a provider's refusal, say — which is usually what says
// what to change. [Elide] keeps the first and drops the second; [ElideTail]
// the reverse. Half the budget goes to each end, the odd rune to the head.
//
// Exported for the sub-agent runner, which reports each worker's failure to
// the parent model this way. A limit of 0 or less means unbounded, the same
// contract [Elide] carries.
func ElideMiddle(text string, limit int) string { return elideMiddle(text, limit) }
