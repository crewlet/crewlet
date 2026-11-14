package runner

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A durable agent record keeps each of its variable payloads to a share of one
// message, and says what it left out.
//
// # Why shares
//
// A phase's record ([types.AgentPhaseCompleted]) and a turn's
// ([types.AgentTurnCompleted]) are each ONE EVENT, and an event past the
// queue's ceiling (internal/queue's MaxPayloadBytes, 8 MiB) is refused at its
// publish, which logs and moves on ([emitter.publish]). So a record too large
// does not arrive long. It does not arrive at all, and takes every fact of the
// phase with it: what it spent, what it decided, why it failed, and, for a
// turn's record, the end of the seat's live row.
//
// What grows on a record is what its phase was handed and made: the calls its
// loop made, each with its arguments and its answer (a host phase's MCP tool
// can answer megabytes, and a page of HTML is six bytes a `<` once escaped);
// what its model said between them; and the prompts it was sent. None of them
// is bounded by the number of calls or rounds a phase may make, and a turn's
// record carries every phase's calls at once.
//
// So each such payload has a SHARE of the message, measured as the event
// encodes it (internal/kept's Len: every escape counted), and a payload past
// its share is kept in part with the cut said, never dropped in silence. A
// record within every share is published as it was built, byte for byte:
// nothing below changes an ordinary record.
//
// # The tool list
//
// A record's calls ([types.ToolExecution] rows) past [toolListBytes] are kept
// as the run's own record keeps them (internal/sandbox's bridged call log),
// in two tiers, each tried only where the one before it left the list past
// its share:
//
//  1. EVERY ROW TO THE BRIDGED CALL'S SHARES: its name past
//     sandbox.MaxBridgeCallNameBytes cut, an id its harness gave it that the
//     log would not keep left off (sandbox.ValidCallID), its arguments past
//     sandbox.MaxBridgeArgsBytes kept with their largest values left out
//     (internal/kept's Args) — the newest successful submission's up to
//     sandbox.MaxSubmissionArgsBytes, as the log keeps it — and its answer
//     past sandbox.MaxBridgeOutputBytes kept as its opening, a failed one's
//     error with it. A row of an executor's run already is: the bridge kept
//     it so.
//  2. THE MIDDLE PAST sandbox.MaxBridgeCalls, dropped into a tally by tool
//     ([types.ElidedCalls]: how many calls, how many failed), the first half
//     and the newest half kept: how a phase began and how it ended are what
//     explain it. The tally names at most [maxTallyNames] tools, and counts
//     the calls of any more under no name, which a reader says as calls to
//     tools the record did not keep either: a model can call names no tool
//     has, and a tally that named every one would grow with them.
//
// A record's tally merges with the one its run's record already kept (a
// collected executor's, [phaseResult.RunElided]), and a turn's record merges
// every phase's.

// recordTally is the record a cut payload of a durable agent record says it is
// not kept on.
const recordTally kept.Record = "this record"

// The tool list's share, and what makes it up.
const (
	// toolRowBytes is one row of a record's tool list at every bound of tier
	// 1, as the event encodes it: the bridged call's own share
	// (sandbox.MaxBridgeCallBytes: its name at its worst, its arguments,
	// its answer, every other field and room for more), and its answer a
	// second time, since a failed row carries it as its `error` too.
	toolRowBytes = sandbox.MaxBridgeCallBytes + sandbox.MaxBridgeOutputBytes

	// maxTallyNames is how many tools a record's tally names. SIXTY-FOUR is
	// more than the distinct tools a phase calls in any record this tree's
	// suites or its e2e make, and the census is held anyway because the names
	// are the model's: an unknown name it invents is a row like any other.
	maxTallyNames = 64

	// tallyEntryBytes is one tally entry at its worst: a name at
	// sandbox.MaxBridgeCallNameBytes escaped six-fold and quoted (1,538
	// bytes), its keys and two counts — 1,600 rounded up to leave the
	// counts their digits.
	tallyEntryBytes = 1600

	// toolListBytes is the share of a record's tool list: sandbox.MaxBridgeCalls
	// rows at their worst, the newest submission's room past an ordinary
	// row's arguments, and the tally at its census, its unnamed entry too:
	// 200 × 20 KiB + 56 KiB + 65 × 1,600 bytes, 4,264,000 bytes.
	toolListBytes = sandbox.MaxBridgeCalls*toolRowBytes +
		sandbox.MaxSubmissionArgsBytes - sandbox.MaxBridgeArgsBytes +
		(maxTallyNames+1)*tallyEntryBytes
)

// RecordCalls is a record's tool list as [keptExecutions] keeps it: the rows
// it lists, and the calls it does not — how many, and by tool how many and how
// many of them failed ([keptTally]) — which the record carries as its
// `tool_executions`, `calls_elided` and `elided_calls`.
type RecordCalls struct {
	Rows   []types.ToolExecution
	Elided int
	ByTool []types.ElidedCalls
}

// keptExecutions is a record's tool list held to [toolListBytes] as the event
// encodes it (see the file comment), merged with tally — what the list's own
// record already kept of calls it does not list, counted in Elided too. A list
// within its share is kept as it is.
//
// rows is never changed: a row it cuts is a copy.
func keptExecutions(rows []types.ToolExecution, tally []types.ElidedCalls) RecordCalls {
	total := tallied(tally)
	if listBytes(rows) <= toolListBytes {
		return RecordCalls{Rows: rows, Elided: total, ByTool: keptTally(tally)}
	}
	kept := make([]types.ToolExecution, len(rows))
	submission := newestSubmission(rows)
	for i, row := range rows {
		kept[i] = keptRow(row, i == submission)
	}
	if listBytes(kept) <= toolListBytes || len(kept) <= sandbox.MaxBridgeCalls {
		return RecordCalls{Rows: kept, Elided: total, ByTool: keptTally(tally)}
	}
	head := sandbox.MaxBridgeCalls / 2
	tail := len(kept) - (sandbox.MaxBridgeCalls - head)
	dropped := kept[head:tail]
	tally = slices.Clip(tally)
	for _, row := range dropped {
		name, _ := row["name"].(string)
		failed := 0
		if ok, _ := row["success"].(bool); !ok {
			failed = 1
		}
		tally = append(tally, types.ElidedCalls{Name: name, Calls: 1, Failed: failed})
	}
	out := make([]types.ToolExecution, 0, sandbox.MaxBridgeCalls)
	out = append(out, kept[:head]...)
	out = append(out, kept[tail:]...)
	return RecordCalls{Rows: out, Elided: total + len(dropped), ByTool: keptTally(tally)}
}

// listBytes is what rows weigh as the event encodes them.
func listBytes(rows []types.ToolExecution) int {
	if len(rows) == 0 {
		return 0
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		// A row the encoder refuses is refused by the publish too, whatever
		// its size; weighing it as past every share keeps it out of a
		// record rather than taking the record with it.
		return int(^uint(0) >> 1)
	}
	return len(raw)
}

// keptRow is row held to the bridged call's shares (tier 1 of the file
// comment), its arguments to the submission's share where it is the list's
// newest successful submission. A copy: row is not changed.
func keptRow(row types.ToolExecution, submission bool) types.ToolExecution {
	out := make(types.ToolExecution, len(row))
	for k, v := range row {
		out[k] = v
	}
	if name, ok := out["name"].(string); ok {
		out["name"] = textcut.Within(name, sandbox.MaxBridgeCallNameBytes)
	}
	if id, ok := out["call_id"].(string); ok && !sandbox.ValidCallID(id) {
		delete(out, "call_id")
	}
	argsShare := sandbox.MaxBridgeArgsBytes
	if submission {
		argsShare = sandbox.MaxSubmissionArgsBytes
	}
	if args, ok := out["arguments"].(string); ok {
		if text, whole := recordTally.Args(args, argsShare); whole > 0 {
			out["arguments"] = text
		}
	}
	for _, key := range []string{"result", "error"} {
		if answer, ok := out[key].(string); ok {
			if text, whole := recordTally.Text(answer, sandbox.MaxBridgeOutputBytes); whole > 0 {
				out[key] = text
			}
		}
	}
	return out
}

// newestSubmission is the index of rows' newest successful submission, or -1:
// the call the phase handed its work in with, which a reader reads whole as
// the run's record keeps it.
func newestSubmission(rows []types.ToolExecution) int {
	for i := len(rows) - 1; i >= 0; i-- {
		name, _ := rows[i]["name"].(string)
		ok, _ := rows[i]["success"].(bool)
		if ok && (name == SubmitWorkTool || name == SubmitReviewTool) {
			return i
		}
	}
	return -1
}

// tallied is how many calls a tally counts.
func tallied(tally []types.ElidedCalls) int {
	n := 0
	for _, e := range tally {
		n += max(e.Calls, 0)
	}
	return n
}

// keptTally is tally folded by tool and held to its census
// ([maxTallyNames]): the tools in name order, each name to
// sandbox.MaxBridgeCallNameBytes, the calls of the tools past the census —
// the ones with the fewest calls — counted under no name, and the calls whose
// tool no record kept last. Nil for a tally of no calls.
func keptTally(tally []types.ElidedCalls) []types.ElidedCalls {
	byName := map[string]*types.ElidedCalls{}
	for _, e := range tally {
		if e.Calls <= 0 {
			continue
		}
		name := textcut.Within(e.Name, sandbox.MaxBridgeCallNameBytes)
		entry, ok := byName[name]
		if !ok {
			entry = &types.ElidedCalls{Name: name}
			byName[name] = entry
		}
		entry.Calls += e.Calls
		entry.Failed += min(max(e.Failed, 0), e.Calls)
	}
	if len(byName) == 0 {
		return nil
	}
	unnamed := types.ElidedCalls{}
	if entry, ok := byName[""]; ok {
		unnamed = *entry
		delete(byName, "")
	}
	named := make([]types.ElidedCalls, 0, len(byName))
	for _, entry := range byName {
		named = append(named, *entry)
	}
	if len(named) > maxTallyNames {
		// THE TOOLS CALLED MOST KEEP THEIR NAMES: a census past its bound
		// is a model calling names no tool has, once each, beside the tools
		// it worked with.
		slices.SortFunc(named, func(a, b types.ElidedCalls) int {
			return cmp.Or(cmp.Compare(b.Calls, a.Calls), cmp.Compare(a.Name, b.Name))
		})
		for _, e := range named[maxTallyNames:] {
			unnamed.Calls += e.Calls
			unnamed.Failed += e.Failed
		}
		named = named[:maxTallyNames]
	}
	slices.SortFunc(named, func(a, b types.ElidedCalls) int { return cmp.Compare(a.Name, b.Name) })
	if unnamed.Calls > 0 {
		named = append(named, unnamed)
	}
	return named
}

// The shares of a record's other variable payloads, each as the event encodes
// it. Their sum with the tool list's is held under one message by
// [TestAPhaseRecordAtEveryBoundFitsOneMessage].
const (
	// responseBytes is a record's response: a run's own report at its bound
	// (sandbox.MaxRunTextBytes) escaped at its worst and quoted, so a
	// collected run's report is never cut on its record, and a host phase's
	// model text past it keeps its opening. A review's final artifact is held
	// to the same where it is made ([finalArtifactBytes]).
	responseBytes = 6*sandbox.MaxRunTextBytes + 2

	// promptBytes is each of a record's two prompts, its outline with it.
	// A QUARTER MIB is several times the widest prompt this tree's suites
	// build, its seat's whole profile and every turn-start block among it;
	// what goes past it is a company document's own words at length, or the
	// prior-work ledger of a turn of very many rounds, and what the record
	// keeps of each part is its opening ([keptPrompt]). The prompt the model
	// was sent is not changed: this is the record's copy.
	promptBytes = 256 << 10

	// narrationBytes is what a record keeps of its model's prose between
	// calls, every round's together. A QUARTER MIB is a long phase's
	// narration whole; past it every round keeps an opening, the longest cut
	// first ([keptNarration]).
	narrationBytes = 256 << 10

	// notesBytes is a record's notes: what a review's own notes may weigh
	// where they are made ([reviewProseBytes]), and every other phase's
	// verdict text — a judge's reason, a worker's refused tools — to the same.
	notesBytes = reviewProseBytes

	// namesBytes is each of a record's two lists of tool names. 64 KIB is
	// five hundred names at the longest an MCP server's tool may have
	// (coord.MaxMCPToolNameBytes), several times what any one listing holds,
	// and a record of a seat that holds more lists the first of them and
	// says how many more.
	namesBytes = 64 << 10

	// maxRecordRounds is how many of its rounds a record lists: their
	// provider calls, prose, harness runs and the notes read in them, and
	// the calls made in them. A phase's rounds are bounded by caps a company
	// sets without a ceiling (a worker's `max_turns`, the onboarding pass's
	// and the ceiling a judge may extend it to), and a record past this
	// keeps the first half and the newest half, saying how many it left out.
	// TWO HUNDRED AND FIFTY-SIX is more than ten times the largest cap this
	// tree defaults to, a worker's twenty.
	maxRecordRounds = 256
)

// answerCopy is what a collected run's copy of its answer on its round's prose
// says of what it does not keep, and answerCopyBytes its share: the record's
// prose share less the round row's own keys, so the copy is never cut again
// where the record keeps its prose ([keptNarration]).
const (
	answerCopy      kept.Record = "this copy; the phase's response holds it whole"
	answerCopyBytes             = narrationBytes - narrationRowRoom

	// narrationRowRoom is a round's prose row less its prose: its round, its
	// flags and their keys, 32 bytes measured at their widest, rounded up.
	narrationRowRoom = 256
)

// keptText is s held to share bytes as the event encodes it: whole within it,
// and past it its opening with the cut said ([recordTally]).
func keptText(s string, share int) string {
	if text, whole := recordTally.Text(s, share); whole > 0 {
		return text
	}
	return s
}

// keptPrompt is p as a record keeps it within [promptBytes], as the event
// encodes the text: whole within it, and past it PART BY PART, every part's
// opening kept and the longest cut first, with the outline rewritten to the
// kept text, so it still tiles it — the dashboard reads a prompt by its
// outline only where the outline tiles it, and falls back to the text's own
// headings otherwise, which is what the outline exists to replace. A prompt
// with no valid outline keeps the opening of its whole text, and none.
func keptPrompt(p prompts.Prompt) prompts.Prompt {
	if kept.Quoted(p.Text) <= promptBytes {
		return p
	}
	if len(p.Sections) == 0 || !p.Valid() {
		return prompts.Prompt{Text: keptText(p.Text, promptBytes)}
	}
	parts := make([]string, len(p.Sections))
	at := 0
	for i, s := range p.Sections {
		parts[i] = p.Text[at : at+s.Bytes]
		at += s.Bytes
	}
	if at < len(p.Text) {
		// A tail no part names is the last part's, as Valid reads it.
		parts[len(parts)-1] += p.Text[at:]
	}
	parts = keptParts(parts, promptBytes-2)
	out := prompts.Prompt{Sections: slices.Clone(p.Sections)}
	var text []byte
	for i, part := range parts {
		out.Sections[i].Bytes = len(part)
		text = append(text, part...)
	}
	out.Text = string(text)
	return out
}

// keptParts is parts held to budget bytes together as the event encodes them,
// each its opening within the one cap that fits them all — the longest cut
// first, the shortest whole — so no part is left out and none is cut further
// than the others need.
func keptParts(parts []string, budget int) []string {
	costs := make([]int, len(parts))
	total := 0
	for i, part := range parts {
		costs[i] = kept.Len(part)
		total += costs[i]
	}
	if total <= budget {
		return parts
	}
	limit := fairCap(costs, budget)
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = part
		if costs[i] > limit {
			// The cap counts the part's quotes, which a part inside one
			// string does not carry: two bytes of slack, and never past.
			out[i] = keptText(part, limit)
		}
	}
	return out
}

// fairCap is the largest cap on each cost that keeps their sum within budget
// (a water-filling: the costs under it count whole, those over it count it),
// never below the room a cut part's marker takes.
func fairCap(costs []int, budget int) int {
	sorted := slices.Clone(costs)
	slices.Sort(sorted)
	spent := 0
	for i, c := range sorted {
		left := len(sorted) - i
		if spent+c*left > budget {
			return max((budget-spent)/left, minCutPart)
		}
		spent += c
	}
	return sorted[len(sorted)-1]
}

// minCutPart is the least a cut part keeps: its marker, which says how much of
// how much is not kept, and an opening of a few words.
const minCutPart = 160

// keptNarration is a record's model prose, every round's together within
// [narrationBytes] as the event encodes it: whole within it, and past it each
// round's opening within the one cap that fits them all ([keptParts]).
func keptNarration(rows []types.RoundNarration) []types.RoundNarration {
	if listBytes(rows) <= narrationBytes {
		return rows
	}
	contents := make([]string, len(rows))
	for i, row := range rows {
		contents[i], _ = row["content"].(string)
	}
	// THE ROWS' OWN KEYS, rounds and flags, ride beside the prose: measured
	// whole, their room is taken off the prose's.
	shells := 0
	for _, row := range rows {
		bare := maps.Clone(row)
		bare["content"] = ""
		shells += mapBytes(bare) + 1
	}
	contents = keptParts(contents, narrationBytes-shells-2)
	out := make([]types.RoundNarration, len(rows))
	for i, row := range rows {
		out[i] = maps.Clone(row)
		if _, ok := row["content"]; ok {
			out[i]["content"] = contents[i]
		}
	}
	return out
}

// mapBytes is what one map weighs as the event encodes it.
func mapBytes(m map[string]any) int {
	raw, err := json.Marshal(m)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(raw)
}

// keptNames is a record's list of tool names within [namesBytes] as the event
// encodes it: the names in their order while they fit, and how many more the
// list held.
func keptNames(names []string) ([]string, int) {
	used := 2
	for i, name := range names {
		used += kept.Quoted(name) + 1
		if used > namesBytes {
			return names[:i:i], len(names) - i
		}
	}
	return names, 0
}

// recordRounds is a record's rounds as [keptRounds] keeps them.
type recordRounds struct {
	Rounds      []types.PhaseRound
	Invocations []types.PhaseInvocation
	Steers      []types.PhaseSteer
	Narration   []types.RoundNarration
	Calls       []types.ToolExecution
	// Elided is how many rounds the record does not list, and Dropped the
	// calls made in them, for the tool list's tally.
	Elided  int
	Dropped []types.ElidedCalls
}

// keptRounds is a record's rounds within [maxRecordRounds]: every one where
// it ran no more, and past it the first half and the newest half, each kept
// round with its provider call, its prose, the harness runs over it and the
// notes read in it, and the calls made in it; the calls of the rounds it
// leaves out are tallied by tool for the tool list ([keptExecutions]), so a
// round the record does not list is not half drawn. The rounds are the
// numbers the round records carry, the loop's own count.
func keptRounds(r recordRounds) recordRounds {
	var numbers []int
	for _, round := range r.Rounds {
		if !slices.Contains(numbers, round.Round) {
			numbers = append(numbers, round.Round)
		}
	}
	if len(numbers) <= maxRecordRounds {
		return r
	}
	half := maxRecordRounds / 2
	keep := map[int]bool{}
	for _, n := range slices.Concat(numbers[:half], numbers[len(numbers)-(maxRecordRounds-half):]) {
		keep[n] = true
	}
	kept := recordRounds{Elided: len(numbers) - maxRecordRounds}
	for _, round := range r.Rounds {
		if keep[round.Round] {
			kept.Rounds = append(kept.Rounds, round)
		}
	}
	for _, run := range r.Invocations {
		for n := run.FirstRound; n <= run.LastRound; n++ {
			if keep[n] {
				kept.Invocations = append(kept.Invocations, run)
				break
			}
		}
	}
	for _, steer := range r.Steers {
		if keep[steer.Round] {
			kept.Steers = append(kept.Steers, steer)
		}
	}
	for _, row := range r.Narration {
		if keep[roundOf(row)] {
			kept.Narration = append(kept.Narration, row)
		}
	}
	for _, row := range r.Calls {
		if keep[roundOf(row)] {
			kept.Calls = append(kept.Calls, row)
			continue
		}
		name, _ := row["name"].(string)
		failed := 0
		if ok, _ := row["success"].(bool); !ok {
			failed = 1
		}
		kept.Dropped = append(kept.Dropped, types.ElidedCalls{Name: name, Calls: 1, Failed: failed})
	}
	return kept
}

// roundOf is the round a row of a record's calls or prose names, or 0.
func roundOf(row map[string]any) int {
	switch n := row["round"].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
