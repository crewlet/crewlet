// Package tokens rolls LLM spend up into the breakdown a dashboard renders:
// by phase, by model, by provider entry, by worker, by agent — and, for the
// live window alone, by turn.
//
// ONE IMPLEMENTATION, and that is the whole point of the package existing.
// This aggregation had three copies once — the REST endpoint's, a
// re-implementation in the browser, and whatever a reconnect left behind — and
// a reader routinely saw a refresh disagree with the page it replaced. Now the
// projection HOLDS records and this folds them, so the live rollup and the
// queried one cannot differ.
//
// # Two inputs, one breakdown
//
// The LIVE window is a list of phase records the projection holds ([Record],
// folded by [Aggregate]). Every NAMED window is company days read from the
// replicated `usage` domain ([Cell] and [SeatDay], folded by [FoldDaily] and
// bucketed by [BucketDaily]) — every node's day, so the answer is the same on
// whichever node is asked and still includes a node that has left the fleet
// (ADR-0020). The two are folded into the same [Rollup] and [Bucket], so a
// reader moving from "the last 24 hours" to "the last 7 days" compares like
// with like. What a daily row cannot answer it does not pretend to: a turn and
// an hour are not in it, so a windowed answer has no `by_turn` and a series
// has no hourly bucket — per-turn spend is the turn list's `sort=-tokens`.
//
// A leaf in everything but the calendar: it imports [period] and nothing else
// from Crewlet, because both ends need it — the live projection (which holds
// the records) and the query surface (which reads the usage rows). A week
// bucket is the company's ISO week, and only the calendar knows where one
// begins.
//
// BOTH PRODUCERS FILL EVERY FIELD OF [Record], and that is a contract rather
// than a coincidence: the live projection reads the phase record's payload and
// the event store reads the columns schema/0015 and schema/0030 promoted out of
// it, so a field one of them carries and the other drops is a rollup whose
// numbers change when a window crosses the live edge. The prompt-cache counts
// were exactly that until 0030 — summed here and filled by neither — so every
// bucket's cache share read zero.
//
// THE CACHE COUNTS ARE A BREAKDOWN OF THE INPUT, never an addition: every
// backend reports input_tokens with the cached prefix included, so a reader
// divides cache_read_tokens by input_tokens and adds neither cache count to
// anything. See [Bucket.CacheReadTokens].
package tokens

import (
	"cmp"
	"slices"
	"time"
)

// DefaultRecentTurns caps the per-turn table.
//
// The table is a TAIL of recent activity, not a history: a busy org produces
// hundreds of turns a day, and a rollup carrying all of them would put a
// megabyte of nested buckets on every socket that asked for a window. Fifty is
// what fits on a screen a reader will actually scroll.
const DefaultRecentTurns = 50

// MaxRecentTurns bounds what a caller may ask for. A request for ten thousand
// turns is a request to aggregate the whole window into one frame.
const MaxRecentTurns = 500

// Record is one completed phase's spend — the aggregator's input, and the
// shape both producers hand it.
type Record struct {
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`

	AgentID   string `json:"agent_id"`
	AgentRole string `json:"agent_role"`

	Phase string `json:"phase"`
	// HostPhase is the phase a nested call ran under: an auxiliary
	// learning worker's own LLM call, or the round-cap extension judge.
	HostPhase string `json:"host_phase"`
	// Worker names the auxiliary worker, and is set only when Phase is
	// "auxiliary" — which is why the worker rollup keys on the pair.
	Worker string `json:"worker"`
	Model  string `json:"model"`
	// ProviderKey is the configured provider entry that served the call.
	// NOT the same question as Model: a fallback chain serves several
	// models under one key, and one model can be configured under several
	// keys, so "which of our entries did this" is answerable only here.
	// Model falls back to it on a phase that named no model (the producers
	// do that, not this package); this is always the key itself.
	ProviderKey string `json:"provider_key,omitempty"`

	// TurnID is the RUN the phase belonged to, and WorkKey the unit of
	// work behind it. Both, because a turn that fails without acting is
	// redelivered and runs again: the run is what a cost row IS — each
	// attempt really spent its tokens — and the work key is the only thing
	// that says the two rows are attempts at one trigger. See ADR-0017.
	TurnID    string `json:"turn_id"`
	WorkKey   string `json:"work_key,omitempty"`
	Iteration int    `json:"iteration"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// provider's prompt cache served and stored on this phase — see
	// [Bucket.CacheReadTokens] for the contract a reader divides by.
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	// CostUSD is what the phase's own provider billed, in dollars, and it
	// is set on a MINORITY of records: only a subscription coding CLI
	// reports a price, so every native-provider phase carries zero.
	//
	// Which is why [Bucket] counts PricedCalls beside the sum. A total of
	// zero over zero priced calls means nobody said what this cost; a total
	// of zero over three means three runs were billed nothing. Collapsing
	// those two into one number is how a dashboard comes to render "$0.00"
	// under a company that has never had a price reported at all.
	CostUSD float64 `json:"cost_usd"`
}

// Bucket is an accumulated total. Embedded rather than nested, because the
// wire shape spreads it into each row: {"phase": "execute", "total_tokens":
// 150, …}, not {"phase": "execute", "bucket": {…}}.
type Bucket struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	Calls        int `json:"calls"`

	// CacheReadTokens and CacheWriteTokens sum the prompt cache's share of
	// the calls in this bucket.
	//
	// THE CONTRACT A READER DIVIDES BY: input_tokens ALREADY INCLUDES the
	// cached prefix — every backend reports it that way (the Anthropic one
	// adds its cache counts back into the input total, OpenAI's prompt count
	// never excluded them) — so these are a BREAKDOWN of InputTokens and
	// never an addition to it. The cache's share of a bucket's input is
	// cache_read_tokens / input_tokens, and adding the two double-counts the
	// prefix. TotalTokens is input plus output, as it always was.
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	// CostUSD sums what the calls in this bucket were billed, and
	// PricedCalls counts how many of them said anything at all.
	//
	// TWO FIELDS, because a price is reported by one backend and not by the
	// rest (see [Record.CostUSD]) — so a currency total is meaningless
	// without the count of records behind it, and a reader that shows a
	// dollar figure over PricedCalls == 0 is stating a price nobody quoted.
	CostUSD     float64 `json:"cost_usd"`
	PricedCalls int     `json:"priced_calls"`
}

func (b *Bucket) add(r Record) {
	b.InputTokens += r.InputTokens
	b.OutputTokens += r.OutputTokens
	b.TotalTokens += r.TotalTokens
	b.CacheReadTokens += r.CacheReadTokens
	b.CacheWriteTokens += r.CacheWriteTokens
	b.Calls++
	// A NEGATIVE price is not a rebate, it is a bad payload, and summing it
	// would silently reduce a company's reported spend. Only a positive one
	// counts, and only a positive one is priced.
	if r.CostUSD > 0 {
		b.CostUSD += r.CostUSD
		b.PricedCalls++
	}
}

// merge adds another bucket's totals to this one — EVERY field of it.
//
// One function rather than a field list at each call site, because a list
// written out beside the struct is the one that forgets the field added last:
// the series' residual summed tokens, calls and price and dropped both cache
// counts, so the "other" band of every chart claimed its calls were never
// served from cache.
func (b *Bucket) merge(o Bucket) {
	b.InputTokens += o.InputTokens
	b.OutputTokens += o.OutputTokens
	b.TotalTokens += o.TotalTokens
	b.CacheReadTokens += o.CacheReadTokens
	b.CacheWriteTokens += o.CacheWriteTokens
	b.Calls += o.Calls
	b.CostUSD += o.CostUSD
	b.PricedCalls += o.PricedCalls
}

// PhaseRow is the per-phase breakdown of a rollup.
type PhaseRow struct {
	Phase string `json:"phase"`
	Bucket
}

// ModelRow is the per-model breakdown of a rollup. It is built from each
// completion's own reported model, never from a provider's configured name:
// a fallback chain serves several models under one key.
type ModelRow struct {
	Model string `json:"model"`
	Bucket
}

// ProviderRow is the per-provider-entry breakdown of a rollup: which of the
// company's configured entries (`providers.llm.<key>`) served the calls, which
// models it answered with, and who leaned on it.
//
// NOT [ModelRow] regrouped. A fallback chain serves several models under one
// key, and one model can be configured under several keys — so "which entry do
// we pay for" and "which model answered" are two questions, and only this row
// answers the first.
type ProviderRow struct {
	// ProviderKey is the configured entry, "unknown" on a call that named
	// none (a record from before node/0030 promoted the column).
	ProviderKey string `json:"provider_key"`

	// Models are every model this entry answered with in the window,
	// biggest first.
	Models []string `json:"models"`

	// Seats are the handles of the TopProviderSeats seats that spent the
	// most through this entry, biggest first; SeatsTotal is how many seats
	// spent through it at all, so a reader can say "and n more" rather than
	// read three names as the whole list.
	Seats      []string `json:"seats"`
	SeatsTotal int      `json:"seats_total"`

	Bucket
}

// TopProviderSeats is how many seats a [ProviderRow] names.
//
// THREE, because the row is read as "used by": a name or three is a sentence
// ("used by lead, coder and 4 more") and a list of every seat is a table the
// per-seat breakdown already is.
const TopProviderSeats = 3

// WorkerRow is the per-worker breakdown of a rollup — the background duties
// (reflection, summarisation) that spend tokens outside any seat's turn.
type WorkerRow struct {
	Worker string `json:"worker"`
	Bucket
}

// AgentRow is one seat's spend, split by phase.
//
// ByPhase is a MAP here and a list at the top level, and the difference is the
// consumers': the top-level list is rendered in token order as a bar, while
// this one is indexed per column of a matrix — `a.by_phase[p]` — so a list
// would make the client search it once per cell.
type AgentRow struct {
	Role    string `json:"role"`
	Handle  string `json:"handle"`
	AgentID string `json:"agent_id"`
	Bucket
	ByPhase map[string]*Bucket `json:"by_phase"`

	// Turns and Failed are how many of this seat's turns ENDED in the
	// window, and how many of those failed — from the usage domain's turn
	// rows, so a named window carries them and the live window does not.
	//
	// POINTERS, because the live window cannot say: it holds phase records,
	// and a count of the distinct turn ids among them is "turns that spent",
	// which is a different number (a turn parked across the window's edge
	// spends in both) presented under the same name. Absent is "this answer
	// does not count turns"; zero is "none ended".
	Turns  *int `json:"turns,omitempty"`
	Failed *int `json:"failed,omitempty"`
}

// TurnRow is one turn's spend, split by phase.
type TurnRow struct {
	// ONE ROW PER RUN, not per unit of work. Each attempt at a redelivered
	// trigger really did spend what it spent, and summing them into one row
	// would charge a turn with another's tokens — which is what the turns
	// list did before the identities were split. WorkKey is what relates
	// them; see ADR-0017.
	TurnID  string `json:"turn_id"`
	WorkKey string `json:"work_key,omitempty"`
	Role    string `json:"role"`
	Handle  string `json:"handle"`
	AgentID string `json:"agent_id"`
	// StartedAt is the earliest phase in the turn and EndedAt the latest.
	// ISO-8601 sorts lexically by time, so these are a plain min and max
	// over strings that were never parsed — which is also what makes them
	// correct for a stamp this process did not produce.
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Bucket
	ByPhase map[string]*Bucket `json:"by_phase"`
}

// Rollup is the whole breakdown.
type Rollup struct {
	// Since and Until describe the WINDOW this covers, so a reader looking
	// at a number knows what it is a number of. Both are RFC3339 instants
	// set by the caller that chose them, not derived here.
	//
	// INSTANTS RATHER THAN A DAY COUNT, which is what this was. A count is
	// a window anchored at now, and a heading has to say where the window
	// sits, not only how long it is.
	//
	// Until is EXCLUSIVE, matching every other half-open window in this
	// engine, so two adjacent rollups share their boundary instant without
	// either losing it or counting it twice. On a named window they are the
	// first instant of its first company day and the first instant after its
	// last.
	Since string `json:"since"`
	Until string `json:"until"`

	// From, To and Days name a NAMED window by its company days — the first
	// and last, inclusive, and how many — which is what it was read by.
	// Absent on the live window, which is a rolling span rather than a run of
	// days.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Days int    `json:"days,omitempty"`

	// Seat is the handle this answer was narrowed to, absent for the whole
	// company.
	Seat string `json:"seat,omitempty"`

	// Horizon is how far back a named window can reach, stated beside the
	// answer; absent on the live window. See [Horizon].
	Horizon *Horizon `json:"horizon,omitempty"`

	Totals     Bucket        `json:"totals"`
	ByPhase    []PhaseRow    `json:"by_phase"`
	ByModel    []ModelRow    `json:"by_model"`
	ByProvider []ProviderRow `json:"by_provider"`
	ByWorker   []WorkerRow   `json:"by_worker"`
	ByAgent    []AgentRow    `json:"by_agent"`

	// ByTurn is the tail of recent turns — on the LIVE window only. A
	// company day's row holds no turn, so a named window has none to list
	// and the field is absent there; per-turn spend over any window is the
	// turn list sorted by tokens.
	ByTurn []TurnRow `json:"by_turn,omitempty"`

	// AggregatedThrough is the latest timestamp this rollup counted.
	//
	// THE ROLLUP'S OWN FRESHNESS, rendered as "counted through": a total
	// with no instant beside it cannot be told from a stale one. It is not
	// a baseline anything folds onto. The client used to do that, with this
	// as the watermark it skipped already-counted events by; the server
	// holds the records and re-folds the whole window now, which is what
	// leaves exactly one implementation of the aggregation.
	//
	// The live window's only: a day's row carries no instant per call, so
	// a named window has no watermark to report and says nothing.
	AggregatedThrough string `json:"aggregated_through,omitempty"`
}

// Options tune one aggregation.
type Options struct {
	// Handles maps a role name to its agent handle, so a per-agent row can
	// link back to that seat. Absent leaves the handle blank rather than
	// guessing one — a wrong link is worse than no link.
	Handles map[string]string

	// Since and Until are recorded on the rollup as the window it
	// describes. The caller does the filtering; this only reports it.
	//
	// Each is rendered as it is given, and a zero one renders EMPTY —
	// unbounded on that side. Aggregate never reads the clock to fill one
	// in: every caller here already holds the window it filtered by (the
	// store's own [store.PhaseTokenQuery.Window] hands back both edges),
	// and a fold that read the clock would be a second opinion about a
	// window somebody already decided.
	Since time.Time
	Until time.Time

	// RecentTurns caps the per-turn list. Zero takes DefaultRecentTurns.
	RecentTurns int
}

// Aggregate folds records into the breakdown.
//
// Order-independent: every bucket is a sum and the two timestamps are a min
// and a max, so records may arrive in any order. That matters because they do
// — the live window is append-ordered by arrival and the store's is by
// (time, id) descending.
func Aggregate(records []Record, opts Options) Rollup {
	limit := opts.RecentTurns
	switch {
	case limit <= 0:
		limit = DefaultRecentTurns
	case limit > MaxRecentTurns:
		limit = MaxRecentTurns
	}

	out := Rollup{
		Since: stamp(opts.Since),
		Until: stamp(opts.Until),
		// Never nil. A nil slice marshals to `null`, and the client does
		// `d.by_phase.length` — so an empty window would throw in the
		// browser rather than rendering an empty table.
		ByPhase:  []PhaseRow{},
		ByModel:  []ModelRow{},
		ByWorker: []WorkerRow{},
		ByAgent:  []AgentRow{},
		ByTurn:   []TurnRow{},
	}
	providers := providerFold{}

	byPhase := map[string]*Bucket{}
	byModel := map[string]*Bucket{}
	byWorker := map[string]*Bucket{}
	byAgent := map[string]*AgentRow{}
	byTurn := map[string]*TurnRow{}

	for _, r := range records {
		phase := orUnknown(r.Phase)
		model := orUnknown(r.Model)
		role := orUnknown(r.AgentRole)

		if laterStamp(r.Timestamp, out.AggregatedThrough) {
			out.AggregatedThrough = r.Timestamp
		}

		out.Totals.add(r)
		bucketFor(byPhase, phase).add(r)
		bucketFor(byModel, model).add(r)
		// The seat a provider row names is its HANDLE where the org has
		// one, which is what every other surface links by, and the role
		// otherwise — a name, never a blank entry in "used by".
		seat := opts.Handles[role]
		if seat == "" {
			seat = role
		}
		providers.add(r.ProviderKey, model, seat, Bucket{}.plus(r))
		// Keyed on the PAIR, not on the worker alone: Worker is set only
		// on an auxiliary phase, so a bare non-empty check would fold a
		// stray value on some other phase into a worker's total.
		if r.Phase == PhaseAuxiliary && r.Worker != "" {
			bucketFor(byWorker, r.Worker).add(r)
		}

		agent := byAgent[role]
		if agent == nil {
			agent = &AgentRow{Role: role, Handle: opts.Handles[role], ByPhase: map[string]*Bucket{}}
			byAgent[role] = agent
		}
		// The LATEST id seen wins: a seat's runtime id changes across
		// sessions, and the current one is what a cross-link must use.
		if r.AgentID != "" {
			agent.AgentID = r.AgentID
		}
		agent.Bucket.add(r)
		bucketFor(agent.ByPhase, phase).add(r)

		if r.TurnID == "" {
			// A phase with no turn still counts toward every other
			// rollup — it is real spend — but it cannot be attributed to
			// a turn, and inventing a key would make one row per phase.
			continue
		}
		turn := byTurn[r.TurnID]
		if turn == nil {
			turn = &TurnRow{
				TurnID: r.TurnID, WorkKey: r.WorkKey,
				Role: role, Handle: opts.Handles[role],
				StartedAt: r.Timestamp, EndedAt: r.Timestamp,
				ByPhase: map[string]*Bucket{},
			}
			byTurn[r.TurnID] = turn
		}
		if r.AgentID != "" {
			turn.AgentID = r.AgentID
		}
		if r.Timestamp != "" {
			if turn.StartedAt == "" || laterStamp(turn.StartedAt, r.Timestamp) {
				turn.StartedAt = r.Timestamp
			}
			if turn.EndedAt == "" || laterStamp(r.Timestamp, turn.EndedAt) {
				turn.EndedAt = r.Timestamp
			}
		}
		turn.Bucket.add(r)
		bucketFor(turn.ByPhase, phase).add(r)
	}

	for phase, b := range byPhase {
		out.ByPhase = append(out.ByPhase, PhaseRow{Phase: phase, Bucket: *b})
	}
	for model, b := range byModel {
		out.ByModel = append(out.ByModel, ModelRow{Model: model, Bucket: *b})
	}
	for worker, b := range byWorker {
		out.ByWorker = append(out.ByWorker, WorkerRow{Worker: worker, Bucket: *b})
	}
	for _, a := range byAgent {
		out.ByAgent = append(out.ByAgent, *a)
	}
	out.ByProvider = providers.rows()
	for _, t := range byTurn {
		out.ByTurn = append(out.ByTurn, *t)
	}

	// Sorted HERE so the consumer does not have to. Ties break on the
	// row's own name rather than being left to Go's map order, which is
	// randomised — two aggregations of identical records would otherwise
	// order their zero-token rows differently every time, which makes a
	// diff of two captures unreadable and a golden test impossible.
	byTokensThen(out.ByPhase, func(r PhaseRow) (int, string) { return r.TotalTokens, r.Phase })
	byTokensThen(out.ByModel, func(r ModelRow) (int, string) { return r.TotalTokens, r.Model })
	byTokensThen(out.ByWorker, func(r WorkerRow) (int, string) { return r.TotalTokens, r.Worker })
	byTokensThen(out.ByAgent, func(r AgentRow) (int, string) { return r.TotalTokens, r.Role })

	// Turns are NEWEST FIRST, not biggest first: the table is a tail of
	// recent activity, and ordering it by size would pin one expensive
	// turn to the top for as long as it stayed in the window.
	slices.SortFunc(out.ByTurn, func(a, b TurnRow) int {
		// By INSTANT, for the reason [compareStamp] carries: a plain byte
		// compare puts a whole-second stamp after every fractional one in
		// the same second, so the newest turn is the one that happens to
		// have landed on a round nanosecond.
		if c := compareStamp(b.EndedAt, a.EndedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.TurnID, b.TurnID)
	})
	if len(out.ByTurn) > limit {
		out.ByTurn = out.ByTurn[:limit]
	}
	return out
}

// plus is this bucket with one record added — the shape a fold that is handed
// buckets rather than records (the provider fold) takes a record in.
func (b Bucket) plus(r Record) Bucket {
	b.add(r)
	return b
}

// providerFold accumulates the per-provider rows, for both inputs: a live
// record and a company day's cell each land here as one (key, model, seat,
// bucket), so the two answers cannot rank "used by" differently.
type providerFold map[string]*providerAcc

type providerAcc struct {
	Bucket
	models map[string]int
	seats  map[string]int
}

func (f providerFold) add(key, model, seat string, b Bucket) {
	key = orUnknown(key)
	acc := f[key]
	if acc == nil {
		acc = &providerAcc{models: map[string]int{}, seats: map[string]int{}}
		f[key] = acc
	}
	acc.merge(b)
	acc.models[model] += b.TotalTokens
	acc.seats[seat] += b.TotalTokens
}

// rows are the providers biggest first, each with its models biggest first and
// its TopProviderSeats biggest seats. Never nil, for the reason every list on
// the rollup is not.
func (f providerFold) rows() []ProviderRow {
	out := make([]ProviderRow, 0, len(f))
	for key, acc := range f {
		seats := ranked(acc.seats)
		row := ProviderRow{
			ProviderKey: key,
			Models:      ranked(acc.models),
			SeatsTotal:  len(seats),
			Bucket:      acc.Bucket,
		}
		row.Seats = seats[:min(len(seats), TopProviderSeats)]
		out = append(out, row)
	}
	byTokensThen(out, func(r ProviderRow) (int, string) { return r.TotalTokens, r.ProviderKey })
	return out
}

// ranked is a map's keys, biggest value first, ties on the name.
func ranked(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(m[b], m[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return keys
}

// PhaseAuxiliary is the phase whose records carry a worker. Named here rather
// than imported from the event catalogue so this package stays a leaf.
const PhaseAuxiliary = "auxiliary"

func bucketFor(m map[string]*Bucket, key string) *Bucket {
	b := m[key]
	if b == nil {
		b = &Bucket{}
		m[key] = b
	}
	return b
}

// orUnknown gives a dimension a name when the event carried none.
//
// "unknown" rather than "": an empty key renders as a blank row a reader
// cannot tell from a rendering bug, and it collides with nothing — whereas
// dropping the record would lose real spend from the totals.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// byTokensThen sorts rows biggest-first, breaking ties on a stable name.
func byTokensThen[T any](rows []T, key func(T) (int, string)) {
	slices.SortFunc(rows, func(a, b T) int {
		at, an := key(a)
		bt, bn := key(b)
		if c := cmp.Compare(bt, at); c != 0 {
			return c
		}
		return cmp.Compare(an, bn)
	})
}

// laterStamp reports whether a is after b, by INSTANT rather than by bytes.
//
// These stamps are RFC3339Nano, which trims trailing zeros — so a whole-second
// stamp has no fractional part at all and its 'Z' (0x5A) sorts after the '.'
// (0x2E) of every fractional stamp in the same second. Compared as strings,
// 03:04:05Z therefore ordered AFTER 03:04:05.9Z.
//
// The load-bearing comment on this comparison stated the opposite premise —
// that RFC3339Nano sorts lexicographically — and used it to justify never
// parsing. What it costs is bounded by the sub-second gap, so it is a display
// defect rather than corruption: a watermark one phase stale, or a TurnRow
// whose start and end are inverted. It is still wrong, and it disagreed with
// livestate's own stamp comparison one package over.
//
// Falls back to a byte comparison when either side does not parse, which is
// what livestate's stamp does and for the same reason: an unparseable stamp
// still has to order somewhere deterministic.
func laterStamp(a, b string) bool { return compareStamp(a, b) > 0 }

// compareStamp orders two stamps the way [laterStamp] compares them, as the
// three-valued answer a sort needs.
//
// It is the primitive and laterStamp is the predicate, rather than the other
// way round, because the newest-first turn sort needs the middle value:
// written as two laterStamp calls it would say "equal" for every pair whose
// bytes differ but whose instants do not, and the tie would then break on the
// turn id — which is the ordering this comparison exists to stop.
func compareStamp(a, b string) int {
	if a == b {
		return 0
	}
	at, aerr := time.Parse(time.RFC3339Nano, a)
	bt, berr := time.Parse(time.RFC3339Nano, b)
	if aerr == nil && berr == nil {
		return at.Compare(bt)
	}
	return cmp.Compare(a, b)
}

// stamp renders a window edge, or the empty string for an unbounded one.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
