// Package tokens rolls per-phase LLM spend up into the breakdown a dashboard
// renders: by phase, by model, by worker, by agent and by turn.
//
// ONE IMPLEMENTATION, and that is the whole point of the package existing.
// This aggregation had three copies once — the REST endpoint's, a
// re-implementation in the browser, and whatever a reconnect left behind — and
// a reader routinely saw a refresh disagree with the page it replaced. Now the
// projection HOLDS records and this folds them, so the live rollup and the
// queried one cannot differ.
//
// A leaf package, importing nothing from Crewlet, because both ends need it:
// the live projection (which holds the records) and the event store (which
// answers for a window other than the live one). Either of those importing the
// other would be a cycle, and a second record type to bridge them would be the
// same duplication one directory further out.
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
	b.Calls++
	// A NEGATIVE price is not a rebate, it is a bad payload, and summing it
	// would silently reduce a company's reported spend. Only a positive one
	// counts, and only a positive one is priced.
	if r.CostUSD > 0 {
		b.CostUSD += r.CostUSD
		b.PricedCalls++
	}
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

// WorkerRow is the per-worker breakdown of a rollup — the background duties
// (reflection, summarisation) that spend tokens outside any seat's turn.
type WorkerRow struct {
	Worker string `json:"worker"`
	Bucket
}

// AgentRow is one seat's spend, split by phase.
//
// ONE ROW PER AGENT ID, and Role and Handle are what that seat is called. The
// row was keyed on the role name, which is prose two seats may share, so two
// "Engineer"s were one row carrying both seats' spend under whichever id was
// folded last. A record carrying no id — nothing this engine publishes today
// — keeps a row under the name it does carry, which is the most a record like
// that can be attributed to.
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
	// Role and Handle name the seat the run belongs to — its CURRENT name
	// and handle where [Options.Seats] knows its id, and otherwise the name
	// its records carry — for the reason [AgentRow] gives.
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
	// a window anchored at now, and a time-range control produces two edges
	// that need not be: "1 June to 8 June" is not any number of days back
	// from this afternoon, and a rollup that could only say "7 days" put a
	// heading over the figures that disagreed with the chart beside them
	// the moment a reader named their own window.
	//
	// Until is EXCLUSIVE, matching every other half-open window in this
	// engine, so two adjacent rollups share their boundary instant without
	// either losing it or counting it twice.
	Since string `json:"since"`
	Until string `json:"until"`

	// AgentID is the one seat this covers, by its agent id, or empty for
	// the whole company.
	AgentID string `json:"agent_id"`

	Totals   Bucket      `json:"totals"`
	ByPhase  []PhaseRow  `json:"by_phase"`
	ByModel  []ModelRow  `json:"by_model"`
	ByWorker []WorkerRow `json:"by_worker"`
	ByAgent  []AgentRow  `json:"by_agent"`
	ByTurn   []TurnRow   `json:"by_turn"`

	// AggregatedThrough is the latest timestamp this rollup counted.
	//
	// THE ROLLUP'S OWN FRESHNESS, rendered as "counted through": a total
	// with no instant beside it cannot be told from a stale one. It is not
	// a baseline anything folds onto. The client used to do that, with this
	// as the watermark it skipped already-counted events by; the server
	// holds the records and re-folds the whole window now, which is what
	// leaves exactly one implementation of the aggregation.
	AggregatedThrough string `json:"aggregated_through"`
}

// Seat is what the org chart says about one agent seat NOW, for the rows that
// name it: the handle a row links to, the name it prints, and the unit a
// per-unit band files it under.
//
// It is handed in rather than read off the records because a record carries
// what the seat was called when the phase ran — a rename's worth out of date
// — and this package is a leaf that has never seen a chart.
type Seat struct {
	Handle string
	Name   string

	// UnitKey and UnitName are the unit the seat sits in DIRECTLY, and empty
	// for a seat at the root of the chart. The key is the band; the name is
	// what the band is called, and never its key, since two units may share
	// one.
	UnitKey  string
	UnitName string
}

// Seats is the chart's seats by agent id — the key every record names its
// seat by.
type Seats map[string]Seat

// Options tune one aggregation.
type Options struct {
	// Seats is the chart this node holds, by agent id, so a per-agent row
	// links to and prints the seat as it is now. A seat absent from it
	// keeps the name its records carry and no handle, rather than a guessed
	// one — a wrong link is worse than no link.
	Seats Seats

	// Since, Until and AgentID are recorded on the rollup as the window
	// and the seat it describes. The caller does the filtering; this only
	// reports it.
	//
	// Each is rendered as it is given, and a zero one renders EMPTY —
	// unbounded on that side. Aggregate never reads the clock to fill one
	// in: every caller here already holds the window it filtered by (the
	// store's own [store.PhaseTokenQuery.Window] hands back both edges),
	// and a fold that read the clock would be a second opinion about a
	// window somebody already decided.
	Since time.Time
	Until time.Time

	AgentID string

	// RecentTurns caps the per-turn list. Zero takes DefaultRecentTurns.
	RecentTurns int
}

// seatKey is the seat a record's spend is filed under: its agent id, or —
// for a record carrying none — its role name, marked so that it can never
// equal an id.
func seatKey(r Record) string {
	if r.AgentID != "" {
		return r.AgentID
	}
	return unnamedSeatPrefix + orUnknown(r.AgentRole)
}

// unnamedSeatPrefix marks a seat key that is a role name rather than an agent
// id. A colon is in no id this engine derives, which is a uuid.
const unnamedSeatPrefix = "role:"

// label picks a seat's printed name from the records filed under it: the
// chart's current name when the chart knows the seat, and otherwise the name on
// the NEWEST record — the one closest to what the seat is called now, whatever
// order the records arrived in.
type label struct{ name, at string }

// fold takes one record's name into account and reports the name to print.
func (l *label) fold(r Record, seat Seat, known bool) string {
	switch {
	case known && seat.Name != "":
		l.name = seat.Name
	case l.name == "" || laterStamp(r.Timestamp, l.at):
		l.name, l.at = orUnknown(r.AgentRole), r.Timestamp
	}
	return l.name
}

// labelFor is the label kept for key, made on first use.
func labelFor(m map[string]*label, key string) *label {
	l := m[key]
	if l == nil {
		l = &label{}
		m[key] = l
	}
	return l
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
		Since:   stamp(opts.Since),
		Until:   stamp(opts.Until),
		AgentID: opts.AgentID,
		// Never nil. A nil slice marshals to `null`, and the client does
		// `d.by_phase.length` — so an empty window would throw in the
		// browser rather than rendering an empty table.
		ByPhase:  []PhaseRow{},
		ByModel:  []ModelRow{},
		ByWorker: []WorkerRow{},
		ByAgent:  []AgentRow{},
		ByTurn:   []TurnRow{},
	}

	byPhase := map[string]*Bucket{}
	byModel := map[string]*Bucket{}
	byWorker := map[string]*Bucket{}
	byAgent := map[string]*AgentRow{}
	byTurn := map[string]*TurnRow{}
	agentNames := map[string]*label{}
	turnNames := map[string]*label{}

	for _, r := range records {
		phase := orUnknown(r.Phase)
		model := orUnknown(r.Model)
		key := seatKey(r)
		seat, known := opts.Seats[r.AgentID]
		known = known && r.AgentID != ""

		if laterStamp(r.Timestamp, out.AggregatedThrough) {
			out.AggregatedThrough = r.Timestamp
		}

		out.Totals.add(r)
		bucketFor(byPhase, phase).add(r)
		bucketFor(byModel, model).add(r)
		// Keyed on the PAIR, not on the worker alone: Worker is set only
		// on an auxiliary phase, so a bare non-empty check would fold a
		// stray value on some other phase into a worker's total.
		if r.Phase == PhaseAuxiliary && r.Worker != "" {
			bucketFor(byWorker, r.Worker).add(r)
		}

		agent := byAgent[key]
		if agent == nil {
			agent = &AgentRow{AgentID: r.AgentID, Handle: seat.Handle, ByPhase: map[string]*Bucket{}}
			byAgent[key] = agent
		}
		agent.Role = labelFor(agentNames, key).fold(r, seat, known)
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
				StartedAt: r.Timestamp, EndedAt: r.Timestamp,
				ByPhase: map[string]*Bucket{},
			}
			byTurn[r.TurnID] = turn
		}
		// A run is one seat's, so every record of it names the same id; the
		// first that carries one settles the row's seat.
		if turn.AgentID == "" && r.AgentID != "" {
			turn.AgentID, turn.Handle = r.AgentID, seat.Handle
		}
		turn.Role = labelFor(turnNames, r.TurnID).fold(r, seat, known)
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
	// Ties on the name and then the id, since two seats may share a name.
	byTokensThen(out.ByAgent, func(r AgentRow) (int, string) { return r.TotalTokens, r.Role + "\x00" + r.AgentID })

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
