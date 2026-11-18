package runner

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A durable agent record holds each variable payload (its calls, its model's
// prose, its prompts, none bounded by a phase's caps) to a SHARE of one
// message, measured as the event encodes it (internal/kept's Len), and says
// what it cut. A phase's or turn's record is ONE event, and one past
// internal/queue's MaxPayloadBytes is refused at publish ([emitter.publish]):
// it does not arrive at all, with every fact of the phase. A record within
// every share is published byte for byte as built.
//
// The tool list past [toolListBytes] is kept as internal/sandbox's bridged
// call log keeps a run's, each tier only where the one before left it past its
// share: tier 1 holds every row to the bridged call's shares ([keptRow]); tier
// 2 drops the middle past sandbox.MaxBridgeCalls into a tally ([keptList]).

// recordTally is the record a cut payload of a durable agent record says it is
// not kept on.
const recordTally kept.Record = "this record"

// The tool list's share, and what makes it up.
const (
	// toolRowBytes is one tier-1 row at its worst as the event encodes it: the
	// bridged call's share (sandbox.MaxBridgeCallBytes) plus its answer again,
	// which a failed row also carries as its `error`.
	toolRowBytes = sandbox.MaxBridgeCallBytes + sandbox.MaxBridgeOutputBytes

	// maxTallyNames is how many tools a record's tally names: more than any
	// phase in this tree's suites or e2e calls, held anyway because the names
	// are the model's, and it can invent any number.
	maxTallyNames = 64

	// tallyEntryBytes is one tally entry at its worst: a name at
	// sandbox.MaxBridgeCallNameBytes escaped six-fold and quoted (1,538
	// bytes), its keys and two counts, rounded up.
	tallyEntryBytes = 1600

	// toolListBytes is sandbox.MaxBridgeCalls rows at their worst, the newest
	// submission's room past an ordinary row's arguments, and the tally at its
	// census plus its unnamed entry: 200 × 20 KiB + 56 KiB + 65 × 1,600 bytes,
	// 4,264,000 bytes.
	toolListBytes = sandbox.MaxBridgeCalls*toolRowBytes +
		sandbox.MaxSubmissionArgsBytes - sandbox.MaxBridgeArgsBytes +
		(maxTallyNames+1)*tallyEntryBytes
)

// RecordCalls is a record's tool list as [keptExecutions] keeps it: the rows it
// lists, and how many calls it does not, in all and by tool ([keptTally]), the
// record's `tool_executions`, `calls_elided` and `elided_calls`.
type RecordCalls struct {
	Rows   []types.ToolExecution
	Elided int
	ByTool []types.ElidedCalls
}

// keptExecutions is a record's tool list held to [toolListBytes] (the file
// comment's tiers), merged with tally: the calls its run's record already left
// out ([phaseResult.RunElided]), counted in Elided too; a turn's record merges
// every phase's. A list within its share is kept as it is; rows is never
// changed, a cut row being a copy.
func keptExecutions(rows []types.ToolExecution, tally []types.ElidedCalls) RecordCalls {
	return keptList(rows, recordList, tally)
}

// listRule is how a list of calls is kept to its share: the share, the most
// rows it lists past it, how many of those are its first (the rest its
// newest), and how one row is held to the bridged call's shares, the newest
// successful submission told apart.
type listRule struct {
	share, rows, head int
	row               func(row types.ToolExecution, submission bool) types.ToolExecution
}

// recordList is a durable record's rule: the first and newest halves of
// sandbox.MaxBridgeCalls, as the run's own record keeps a long run's, since
// how a phase began and ended are what explain it.
var recordList = listRule{share: toolListBytes, rows: sandbox.MaxBridgeCalls,
	head: sandbox.MaxBridgeCalls / 2, row: keptRow}

// keptList is rows held to rule's share: whole within it; past it every row
// held by rule.row; past that the rows between the first rule.head and the
// newest dropped into the tally, as few as fit the share less the tally's own
// room, never more than rule.rows listed. A row past what the rows before it
// left of the share is dropped like the rest.
func keptList(rows []types.ToolExecution, rule listRule, tally []types.ElidedCalls) RecordCalls {
	total := tallied(tally)
	if listBytes(rows) <= rule.share {
		return RecordCalls{Rows: rows, Elided: total, ByTool: keptTally(tally)}
	}
	kept := make([]types.ToolExecution, len(rows))
	submission := newestSubmission(rows)
	for i, row := range rows {
		kept[i] = rule.row(row, i == submission)
	}
	if listBytes(kept) <= rule.share {
		return RecordCalls{Rows: kept, Elided: total, ByTool: keptTally(tally)}
	}
	// The rows' budget is the share less the brackets and the tally at its
	// largest (every row counted), measured before any row is chosen so no
	// choice can outgrow it.
	all := slices.Clone(tally)
	for _, row := range kept {
		all = append(all, droppedCall(row))
	}
	budget := rule.share - 2 - tallyBytes(keptTally(all))
	n := len(kept)
	used, head := 0, 0
	for head < min(rule.head, n) && used+mapBytes(kept[head])+1 <= budget {
		used += mapBytes(kept[head]) + 1
		head++
	}
	newest := 0
	for head+newest < min(rule.rows, n) {
		i := n - 1 - newest
		if used+mapBytes(kept[i])+1 > budget {
			break
		}
		used += mapBytes(kept[i]) + 1
		newest++
	}
	dropped := kept[head : n-newest]
	tally = slices.Clip(tally)
	for _, row := range dropped {
		tally = append(tally, droppedCall(row))
	}
	out := make([]types.ToolExecution, 0, head+newest)
	out = append(out, kept[:head]...)
	out = append(out, kept[n-newest:]...)
	return RecordCalls{Rows: out, Elided: total + len(dropped), ByTool: keptTally(tally)}
}

// droppedCall is row as a tally counts it: one call of its tool, failed or not.
func droppedCall(row types.ToolExecution) types.ElidedCalls {
	name, _ := row["name"].(string)
	failed := 0
	if ok, _ := row["success"].(bool); !ok {
		failed = 1
	}
	return types.ElidedCalls{Name: name, Calls: 1, Failed: failed}
}

// tallyBytes is what a tally weighs as the event encodes it.
func tallyBytes(tally []types.ElidedCalls) int {
	if len(tally) == 0 {
		return 0
	}
	raw, err := json.Marshal(tally)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(raw)
}

// listBytes is what rows weigh as the event encodes them.
func listBytes(rows []types.ToolExecution) int {
	if len(rows) == 0 {
		return 0
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		// The publish would refuse it too: weighing it past every share keeps
		// it off the record rather than losing the record.
		return int(^uint(0) >> 1)
	}
	return len(raw)
}

// keptRow is a copy of row held to the bridged call's shares (tier 1): its
// identity and arguments as [liveRow] holds them, and its answer, a failed
// row's error too, past sandbox.MaxBridgeOutputBytes kept as its opening. An
// executor's row already is so: the bridge kept it.
func keptRow(row types.ToolExecution, submission bool) types.ToolExecution {
	out := liveRow(row, submission)
	for _, key := range []string{"result", "error"} {
		if answer, ok := out[key].(string); ok {
			if text, whole := recordTally.Text(answer, sandbox.MaxBridgeOutputBytes); whole > 0 {
				out[key] = text
			}
		}
	}
	return out
}

// keptIdentity is a copy of row with its name held to
// sandbox.MaxBridgeCallNameBytes and a harness's id the bridged call log would
// not keep left off (sandbox.ValidCallID).
func keptIdentity(row types.ToolExecution) types.ToolExecution {
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
	return out
}

// newestSubmission is the index of rows' newest successful submission, or -1:
// the call that handed the work in, kept to its own larger share.
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

// keptTally is tally folded by tool and held to [maxTallyNames]: names to
// sandbox.MaxBridgeCallNameBytes in name order, then one unnamed entry for the
// calls of the tools past the census (the fewest called) and of tools no
// record kept, so a model's invented names cannot grow it. Nil for no calls.
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
		// The most-called tools keep their names: a census past its bound is
		// a model calling invented names, once each.
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
	// responseBytes is a run's report at its bound (sandbox.MaxRunTextBytes)
	// escaped at its worst and quoted, so a collected run's report is never
	// cut on its record; a host phase's text past it keeps its opening. A
	// review's final artifact is held to it where made ([finalArtifactBytes]).
	responseBytes = 6*sandbox.MaxRunTextBytes + 2

	// promptBytes is each of a record's two prompts, outline included: several
	// times the widest prompt this tree's suites build. Past it is a company
	// document at length or a very long prior-work ledger, and each part keeps
	// its opening ([keptPrompt]). The prompt the model was sent is unchanged.
	promptBytes = 256 << 10

	// narrationBytes is a record's model prose, every round's together: a long
	// phase's whole; past it each round keeps an opening ([keptNarration]).
	narrationBytes = 256 << 10

	// notesBytes is a review's notes as bounded where made
	// ([reviewProseBytes]), and every other phase's verdict text to the same.
	notesBytes = reviewProseBytes

	// namesBytes is each of a record's two tool-name lists: five hundred names
	// at coord.MaxMCPToolNameBytes, several times any one listing; past it the
	// list keeps its first names and counts the rest.
	namesBytes = 64 << 10

	// maxRecordRounds is how many rounds a record lists, each with everything
	// of it ([keptRounds]); past it the first and newest halves. A company
	// sets round caps without a ceiling (a worker's `max_turns`, onboarding's
	// and a judge's extension); this is over ten times the largest default
	// cap, a worker's twenty.
	maxRecordRounds = 256
)

// answerCopy is what a collected run's copy of its answer on its round's prose
// says it cut, and answerCopyBytes its share: the prose share less the round
// row's keys, so [keptNarration] never cuts the copy again.
const (
	answerCopy      kept.Record = "this copy; the phase's response holds it whole"
	answerCopyBytes             = narrationBytes - narrationRowRoom

	// narrationRowRoom is a prose row less its prose: its round, flags and
	// keys, 32 bytes at their widest, rounded up.
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

// keptPrompt is p within [promptBytes]: whole within it, past it PART BY PART
// ([keptParts]) with the outline rewritten to the kept text, since the
// dashboard reads the outline only where it tiles the text and otherwise falls
// back to the headings the outline replaces. A prompt with no valid outline
// keeps its text's opening, and no outline.
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

// keptParts is parts held to budget bytes together, each its opening within
// the one cap that fits them all ([fairCap]), so no part is left out and none
// is cut further than the others need.
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
			// The cap counts quotes a part inside one string lacks: two
			// bytes of slack, never past.
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

// minCutPart is the least a cut part keeps: its marker and a few words.
const minCutPart = 160

// keptNarration is model prose, every round's together, within share
// ([narrationBytes] or [liveNarrationBytes]): whole within it, past it each
// round's opening ([keptParts]).
func keptNarration(rows []types.RoundNarration, share int) []types.RoundNarration {
	if listBytes(rows) <= share {
		return rows
	}
	contents := make([]string, len(rows))
	for i, row := range rows {
		contents[i], _ = row["content"].(string)
	}
	// The rows' own keys come off the prose's room.
	shells := 0
	for _, row := range rows {
		bare := maps.Clone(row)
		bare["content"] = ""
		shells += mapBytes(bare) + 1
	}
	contents = keptParts(contents, share-shells-2)
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

// keptNames is a tool-name list within [namesBytes]: the names in order while
// they fit, and how many more it held.
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

// keptRounds is a record's rounds within [maxRecordRounds], past it the first
// and newest halves, each kept round with its provider call, prose, harness
// runs, notes read and calls; a left-out round's calls go to Dropped for the
// tool list's tally, so no round is half drawn. Rounds are the loop's own
// numbers, as the round records carry them.
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

// # The live frame
//
// A running phase's live frame ([types.AgentTurnProgress]) is republished
// whole five times a second (internal/agent/toolloop's partialInterval) to
// every dashboard open on its seat, and the socket hub queues up to 512 frames
// for a slow client: its shares answer that stream, not one message. Past them
// the frame keeps the NEWEST of what grows, what a reader is watching arrive,
// and says what it cut; the record keeps the first and newest once the phase
// completes. A frame within every share is published as built.
const (
	// liveListBytes is the frame's tool list: a long phase's ordinary calls
	// whole, 1.25 MiB a second at five frames, 128 MiB in a full slow queue.
	liveListBytes = 256 << 10

	// liveNarrationBytes is the committed rounds' prose, every round's
	// together, a long round's whole; the round in flight rides beside it held
	// to its tail ([partialTail]).
	liveNarrationBytes = 64 << 10

	// liveAbandonedBytes is the attempts at the round in flight a provider gave
	// up on partway, the newest liveAbandonedAttempts, each its END within an
	// even part ([liveTail]): what a reader saw last before it died. FOUR is
	// more than a fallback chain gives up in a round short of an outage, and
	// the round's own text replaces them once it completes.
	liveAbandonedBytes    = 16 << 10
	liveAbandonedAttempts = 4

	// liveRunningBytes is the frame's calls in flight, liveRunningArgsBytes
	// each one's arguments: TWO KIB holds the identifiers a reader scans a
	// running call for (its body is on the record once it settles), and
	// THIRTY-TWO KIB a dozen such calls, more than a harness runs at once.
	liveRunningBytes     = 32 << 10
	liveRunningArgsBytes = 2 << 10
)

// liveList is the live frame's rule: only its newest calls that fit, since a
// reader watches what arrives and the record keeps how the phase began.
var liveList = listRule{share: liveListBytes, rows: sandbox.MaxBridgeCalls, row: liveRow}

// liveRow is a live row's identity ([keptIdentity]) and arguments held to the
// bridged call's shares (internal/kept's Args drops the largest values first),
// a submission's to sandbox.MaxSubmissionArgsBytes; its answer is already held
// to its tail ([liveExecutions]).
func liveRow(row types.ToolExecution, submission bool) types.ToolExecution {
	out := keptIdentity(row)
	argsShare := sandbox.MaxBridgeArgsBytes
	if submission {
		argsShare = sandbox.MaxSubmissionArgsBytes
	}
	if args, ok := out["arguments"].(string); ok {
		if text, whole := recordTally.Args(args, argsShare); whole > 0 {
			out["arguments"] = text
		}
	}
	return out
}

// keptRunning is the calls in flight within [liveRunningBytes], names and
// arguments held to their shares, in start order while they fit, and how many
// more were running: an unlisted one reaches the list once it settles.
func keptRunning(calls []types.RunningCall) ([]types.RunningCall, int) {
	used := 2
	for i := range calls {
		calls[i].Name = textcut.Within(calls[i].Name, sandbox.MaxBridgeCallNameBytes)
		if text, whole := recordTally.Args(calls[i].Arguments, liveRunningArgsBytes); whole > 0 {
			calls[i].Arguments = text
		}
		raw, err := json.Marshal(calls[i])
		if err != nil {
			return calls[:i:i], len(calls) - i
		}
		used += len(raw) + 1
		if used > liveRunningBytes {
			return calls[:i:i], len(calls) - i
		}
	}
	return calls, 0
}

// liveTail is s's END within budget bytes, cut on a character and marked at
// its start as internal/textcut's Tail marks it.
func liveTail(s string, budget int) string {
	if kept.Quoted(s) <= budget {
		return s
	}
	room := budget - 2 - kept.Len(tailMarker)
	used, cut := 0, len(s)
	for cut > 0 {
		_, size := utf8.DecodeLastRuneInString(s[:cut])
		cost := kept.Len(s[cut-size : cut])
		if used+cost > room {
			break
		}
		used += cost
		cut -= size
	}
	return tailMarker + s[cut:]
}

// tailMarker is what starts a tail [liveTail] cut.
const tailMarker = "…"
