// Package tokens rolls LLM spend records up into the breakdown a dashboard
// renders: by phase, by model, by worker, by agent and by turn.
//
// ONE IMPLEMENTATION, and that is the whole point of the package existing.
// Both the live projection and the event store hand their records to the fold
// here, so the live rollup and the queried one are the same arithmetic and a
// refresh cannot disagree with the page it replaced. A copy in the browser, or
// one per surface, is a second answer to the same question that is free to
// drift from the first.
//
// A leaf package, importing nothing from Crewlet, because both ends need it:
// the live projection (which holds the records) and the event store (which
// answers for a window other than the live one). Either of those importing the
// other would be a cycle, and a second record type to bridge them would be the
// same duplication one directory further out.
//
// # What a record is
//
// One spend event, as either producer read it: a turn's phase record
// (`agent_phase_completed`), or one auxiliary completion
// (`auxiliary_call_completed`), which internal/engine publishes for every
// completion the learning workers and the turn-start prefetch make. A record
// carries its own model split ([Record.Models]) and says when part of its spend
// was never reported ([Record.Unreported]), and the fold honours both:
// `by_model` is what each completion reported rather than the first model a
// phase met, and a total that is only a floor carries a count saying so.
package tokens

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"
)

// DefaultRecentTurns caps the per-turn table.
//
// The table is a TAIL of recent activity, not a history: a busy org produces
// hundreds of turns a day, and a rollup carrying all of them would put a
// megabyte of nested buckets on every socket that asked for a window. Fifty is
// what fits on a screen a reader will actually scroll.
//
// # Where the turns past the cut are
//
// Every figure but the table still counts them ([Rollup.TurnsTotal] says how
// many there were), and the rows themselves are reached by asking again: the
// `tokens` question in internal/api/queries takes `recent_turns` up to
// [MaxRecentTurns], and `since`/`until` to name a narrower half-open window,
// which folds every turn with a phase inside it — a turn straddling an edge
// from only its phases inside the window.
const DefaultRecentTurns = 50

// MaxRecentTurns bounds what a caller may ask for. A request for ten thousand
// turns is a request to aggregate the whole window into one frame; a window
// holding more than this is read in narrower windows, as [DefaultRecentTurns]
// describes.
const MaxRecentTurns = 500

// Record is one spend record — the aggregator's input, and the shape both
// producers hand it.
type Record struct {
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`

	AgentID   string `json:"agent_id"`
	AgentRole string `json:"agent_role"`

	Phase string `json:"phase"`
	// HostPhase is the phase a nested call ran under: the executor round
	// that delegated a "subagent" task, or the phase that asked the
	// round-cap extension judge.
	HostPhase string `json:"host_phase"`
	// Worker names the worker behind the call: on a "subagent" record the
	// `workers:` template the delegated task ran, and on an "auxiliary" one
	// the learning worker or the prefetch that made the call. Empty on
	// every other phase, on a delegation that wrote its prompt inline
	// rather than naming a template, and on an auxiliary call whose caller
	// named none. The worker rollup keys on the PAIR; see [workerOf].
	Worker string `json:"worker"`
	// Model is the model the record names: on a phase record the first
	// model a completion of the phase reported, on an auxiliary record the
	// one completion's.
	Model string `json:"model"`

	// Models is what each model that served the record accounts for, when
	// the record carries the split. See [Record.shares] for how it is read:
	// a record without one counts whole under Model, and so does the part
	// of a record's spend its split does not cover.
	Models []ModelSpend `json:"models,omitempty"`

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

	// CostUSD is what the record's own provider billed, in dollars, and it
	// is set on a MINORITY of records: only a coding CLI reports a price,
	// so every native-provider phase carries zero.
	//
	// Which is why [Bucket] counts PricedCalls beside the sum. A total of
	// zero over zero priced calls means nobody said what this cost; a total
	// of zero over three means three runs were billed nothing. Collapsing
	// those two into one number is how a dashboard comes to render "$0.00"
	// under a company that has never had a price reported at all.
	CostUSD float64 `json:"cost_usd"`

	// Unreported says part of this record's spend was never reported: a
	// detached coding run whose agent gave no complete account of its own
	// model calls. The tokens above are then a FLOOR — what the rounds in
	// this process billed, plus whatever the run did report — and never
	// the whole.
	//
	// A flag rather than a zero, because zero is a figure: a run that spent
	// nothing and a run nobody measured would otherwise be one number, and
	// the rollup would state the second as the first. [Bucket.UnreportedCalls]
	// carries it into every figure the record reaches.
	Unreported bool `json:"unreported,omitempty"`
}

// ModelSpend is one model's part of a record's spend.
//
// The wire shape of the `models` list an `agent_phase_completed` carries, which
// is why it has JSON tags: the event store and the live projection both read
// that list off the payload through [DecodeModels], so one decoding rule serves
// every producer.
type ModelSpend struct {
	// Model is the model a completion reported serving the call, or the
	// configured model of the provider that served it where the completion
	// named none. Empty is a model nobody named, which the rollup files
	// under "unknown" like any other unnamed dimension.
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// CostUSD is this model's part of the record's price, where its
	// reporter priced each model separately; zero where it did not.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// DecodeModels reads a record's `models` list off its payload bytes.
//
// A list that does not decode is NO SPLIT, and the record then counts whole
// under its one model — the same reading a record from a build that wrote no
// list gets, so a bad split costs the per-model breakdown its detail and never
// costs the totals a token. Absent and JSON null decode to nil.
func DecodeModels(raw []byte) []ModelSpend {
	if len(raw) == 0 {
		return nil
	}
	var out []ModelSpend
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ModelsOf is [DecodeModels] for a payload already decoded into Go values —
// the `models` entry of a map[string]any, as the live projection holds its
// payloads.
//
// Re-encoded and decoded rather than walked, so the two producers go through
// the one decoding rule instead of a second one written against maps.
func ModelsOf(v any) []ModelSpend {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return DecodeModels(raw)
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

	// UnreportedCalls counts the calls in this bucket whose spend is only
	// partly known (see [Record.Unreported]). Above zero, the token figures
	// beside it are a FLOOR: a renderer states them as "at least", or says
	// how many calls went unmeasured, and never as the whole.
	UnreportedCalls int `json:"unreported_calls"`
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
	if r.Unreported {
		b.UnreportedCalls++
	}
}

// fold adds another bucket's figures to this one, for a residual that stands
// for several rows.
func (b *Bucket) fold(o Bucket) {
	b.InputTokens += o.InputTokens
	b.OutputTokens += o.OutputTokens
	b.TotalTokens += o.TotalTokens
	b.Calls += o.Calls
	b.CostUSD += o.CostUSD
	b.PricedCalls += o.PricedCalls
	b.UnreportedCalls += o.UnreportedCalls
}

// costNoise is the largest difference between a record's price and the sum of
// its split's prices that is read as floating-point summation rather than as a
// part of the price the split does not cover: half a micro-dollar, far below
// any price a reporter quotes and far above what adding a few dozen doubles can
// lose.
const costNoise = 5e-7

// shares splits a record into one record per model that served it — the unit
// the per-model breakdown and the model bands count.
//
// THE SPLIT IS TRUSTED ONLY WHERE IT FITS INSIDE ITS RECORD. A split whose
// tokens or price sum past the record's own figures, or with a negative entry,
// is a bad payload, and counting it would make `by_model` add up to more than
// the totals beside it — so it is ignored and the record counts whole under
// its one model, as a record from a build that wrote no split does.
//
// WHAT A SPLIT DOES NOT COVER COUNTS UNDER THE RECORD'S OWN MODEL: a record
// whose split names part of its spend — a phase that carried rounds from
// before a suspension, written by a build that kept no split for them — loses
// none of the rest. So the shares always sum to the record's own figures, and
// `by_model` always sums to the totals.
//
// AN UNREPORTED PART IS A SHARE OF ITS OWN, under no model's name: nobody
// reported which model spent what nobody reported. It carries no tokens and
// counts one call, so the per-model breakdown has a row that says a record's
// spend went unmeasured instead of folding that into a named model's figures,
// which were measured.
//
// Shares naming one model are merged, so a record counts as one call of each
// model it names however its split was written.
func (r Record) shares() []Record {
	whole := r
	whole.Models, whole.Unreported = nil, false
	out := make([]Record, 0, len(r.Models)+2)
	if !r.splitFits() {
		out = append(out, whole)
	} else {
		covered := whole
		covered.InputTokens, covered.OutputTokens, covered.TotalTokens, covered.CostUSD = 0, 0, 0, 0
		for _, m := range r.Models {
			share := whole
			share.Model = m.Model
			share.InputTokens, share.OutputTokens = m.InputTokens, m.OutputTokens
			share.TotalTokens = m.InputTokens + m.OutputTokens
			share.CostUSD = m.CostUSD
			out = mergeShare(out, share)
			covered.InputTokens += share.InputTokens
			covered.OutputTokens += share.OutputTokens
			covered.TotalTokens += share.TotalTokens
			covered.CostUSD += share.CostUSD
		}
		rest := whole
		rest.InputTokens -= covered.InputTokens
		rest.OutputTokens -= covered.OutputTokens
		rest.TotalTokens -= covered.TotalTokens
		rest.CostUSD -= covered.CostUSD
		if rest.CostUSD <= costNoise {
			rest.CostUSD = 0
		}
		if rest.InputTokens > 0 || rest.OutputTokens > 0 || rest.TotalTokens > 0 || rest.CostUSD > 0 {
			out = mergeShare(out, rest)
		}
	}
	if r.Unreported {
		unmeasured := whole
		unmeasured.Model = ""
		unmeasured.InputTokens, unmeasured.OutputTokens, unmeasured.TotalTokens, unmeasured.CostUSD = 0, 0, 0, 0
		unmeasured.Unreported = true
		out = mergeShare(out, unmeasured)
	}
	return out
}

// splitFits reports whether the record carries a split that fits inside it.
// See [Record.shares].
func (r Record) splitFits() bool {
	if len(r.Models) == 0 {
		return false
	}
	var in, out, total int
	var cost float64
	for _, m := range r.Models {
		if m.InputTokens < 0 || m.OutputTokens < 0 || m.CostUSD < 0 {
			return false
		}
		in += m.InputTokens
		out += m.OutputTokens
		total += m.InputTokens + m.OutputTokens
		cost += m.CostUSD
	}
	return in <= r.InputTokens && out <= r.OutputTokens && total <= r.TotalTokens &&
		cost <= r.CostUSD+costNoise
}

// mergeShare adds a share to the list, into the share already naming its model
// when there is one.
func mergeShare(list []Record, share Record) []Record {
	for i := range list {
		if list[i].Model != share.Model {
			continue
		}
		list[i].InputTokens += share.InputTokens
		list[i].OutputTokens += share.OutputTokens
		list[i].TotalTokens += share.TotalTokens
		list[i].CostUSD += share.CostUSD
		list[i].Unreported = list[i].Unreported || share.Unreported
		return list
	}
	return append(list, share)
}

// PhaseRow is the per-phase breakdown of a rollup.
type PhaseRow struct {
	Phase string `json:"phase"`
	Bucket
}

// ModelRow is the per-model breakdown of a rollup.
//
// Built from what each completion reported serving it, through the record's
// own split ([Record.shares]), never from a provider's configured name alone: a
// fallback chain serves several models under one key, and one phase's rounds
// can be served by more than one of them. A record two models served is a call
// of each, so the rows' calls can sum past the totals' while their tokens sum to
// exactly the totals'.
type ModelRow struct {
	Model string `json:"model"`
	Bucket
}

// WorkerRow is the per-worker breakdown of a rollup: a delegated task's
// `workers:` template, or the learning worker or prefetch behind an auxiliary
// call.
//
// ONE ROW PER PHASE AND NAME, and Phase says which kind of worker the row is.
// A template may be called anything the `workers:` key grammar admits,
// including a learning worker's name, so a row keyed on the name alone would be
// one two workers could sum into.
type WorkerRow struct {
	Phase  string `json:"phase"`
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
}

// TurnRow is one turn's spend, split by phase.
type TurnRow struct {
	// ONE ROW PER RUN, not per unit of work. Each attempt at a redelivered
	// trigger really did spend what it spent, and summing them into one row
	// would charge a turn with another's tokens. WorkKey is what relates
	// them; see ADR-0017.
	TurnID  string `json:"turn_id"`
	WorkKey string `json:"work_key,omitempty"`
	Role    string `json:"role"`
	Handle  string `json:"handle"`
	AgentID string `json:"agent_id"`
	// StartedAt is the earliest record in the turn and EndedAt the latest,
	// compared by instant (see [compareStamp]) and kept as the strings the
	// records carried.
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
	// INSTANTS RATHER THAN A DAY COUNT: a count is a window anchored at
	// now, and a time-range control produces two edges that need not be —
	// "1 June to 8 June" is not any number of days back from this
	// afternoon, and a heading that could only say "7 days" would disagree
	// with the chart beside it the moment a reader named their own window.
	//
	// Until is EXCLUSIVE, matching every other half-open window in this
	// engine, so two adjacent rollups share their boundary instant without
	// either losing it or counting it twice.
	Since string `json:"since"`
	Until string `json:"until"`

	AgentRole string `json:"agent_role"`

	Totals   Bucket      `json:"totals"`
	ByPhase  []PhaseRow  `json:"by_phase"`
	ByModel  []ModelRow  `json:"by_model"`
	ByWorker []WorkerRow `json:"by_worker"`
	ByAgent  []AgentRow  `json:"by_agent"`
	ByTurn   []TurnRow   `json:"by_turn"`

	// TurnsTotal is how many turns the window actually held, against the
	// bounded slice [Rollup.ByTurn] carries. A renderer that shows ByTurn
	// shows this beside it, or it heads a page of the newest with a count
	// that reads as the whole; [DefaultRecentTurns] says where the rest are.
	//
	// WITHOUT IT THE TABLE AND THE FIGURE ABOVE IT DESCRIBE DIFFERENT SETS.
	// `Totals` is computed over every record in the window and the table is
	// a page of the newest, so a reader who asked for fifty and got fifty
	// could not tell fifty-one turns from five thousand — while the number
	// above the table was summed over all of them. The sibling cut in this
	// package already refuses that: a group row past the limit becomes a
	// residual naming how many it stands for (see [GroupRow.Folded]).
	TurnsTotal int `json:"turns_total"`

	// AggregatedThrough is the latest timestamp this rollup counted.
	//
	// THE ROLLUP'S OWN FRESHNESS, rendered as "counted through": a total
	// with no instant beside it cannot be told from a stale one. It is not
	// a baseline anything folds onto — the server holds the records and
	// re-folds the whole window, which is what leaves exactly one
	// implementation of the aggregation.
	AggregatedThrough string `json:"aggregated_through"`
}

// Options tune one aggregation.
type Options struct {
	// Handles maps a role name to its agent handle, so a per-agent row can
	// link back to that seat. Absent leaves the handle blank rather than
	// guessing one — a wrong link is worse than no link.
	Handles map[string]string

	// Since, Until and AgentRole are recorded on the rollup as the window
	// it describes. The caller does the filtering; this only reports it.
	//
	// Each is rendered as it is given, and a zero one renders EMPTY —
	// unbounded on that side. Aggregate never reads the clock to fill one
	// in: every caller here already holds the window it filtered by (the
	// store's own [store.PhaseTokenQuery.Window] hands back both edges),
	// and a fold that read the clock would be a second opinion about a
	// window somebody already decided.
	Since time.Time
	Until time.Time

	AgentRole string

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
		Since:     stamp(opts.Since),
		Until:     stamp(opts.Until),
		AgentRole: opts.AgentRole,
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
	byWorker := map[workerID]*Bucket{}
	byAgent := map[string]*AgentRow{}
	byTurn := map[string]*TurnRow{}

	for _, r := range records {
		phase := orUnknown(r.Phase)
		role := orUnknown(r.AgentRole)

		if laterStamp(r.Timestamp, out.AggregatedThrough) {
			out.AggregatedThrough = r.Timestamp
		}

		out.Totals.add(r)
		bucketFor(byPhase, phase).add(r)
		for _, share := range r.shares() {
			bucketFor(byModel, orUnknown(share.Model)).add(share)
		}
		if id, ok := workerOf(r); ok {
			bucketFor(byWorker, id).add(r)
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
			// A record with no turn still counts toward every other
			// rollup — it is real spend — but it cannot be attributed to
			// a turn, and inventing a key would make one row per record.
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
	for id, b := range byWorker {
		out.ByWorker = append(out.ByWorker, WorkerRow{Phase: id.phase, Worker: id.worker, Bucket: *b})
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
	byTokensThen(out.ByWorker, func(r WorkerRow) (int, string) {
		return r.TotalTokens, workerID{phase: r.Phase, worker: r.Worker}.band()
	})
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
	// COUNTED BEFORE THE CUT, which is the whole of what it is for.
	out.TurnsTotal = len(out.ByTurn)
	if len(out.ByTurn) > limit {
		out.ByTurn = out.ByTurn[:limit]
	}
	return out
}

// The phases whose records may name a worker: a delegated task's phase record,
// and an auxiliary completion's record, which names the learning worker or the
// prefetch that made it. Named here rather than imported from the event
// catalogue so this package stays a leaf; a test holds them to the catalogue's
// values.
const (
	PhaseAuxiliary = "auxiliary"
	PhaseSubagent  = "subagent"
)

// workerID is a worker's identity in a rollup or a series: the kind of worker
// and its name.
type workerID struct {
	phase  string
	worker string
}

// band is the identity as one string, for the one place that needs a string:
// a series band's key, and a tie-break in a sort.
//
// The phase is one of two fixed words with no "/" in it, so the first "/" is
// always the separator whatever the name holds.
func (id workerID) band() string { return id.phase + "/" + id.worker }

// workerOf is the worker a record is the spend of, and whether it is any
// worker's at all.
//
// Keyed on the PHASE as well as the name, for two reasons. A Worker on a phase
// that names none is a stray value, and a bare non-empty check would fold it
// into a worker's total as spend that worker never made. And the two phases
// admitted here may carry one name (see [WorkerRow]), which only the phase
// tells apart.
//
// ONE PREDICATE for [Aggregate] and [Bucketed], which is what keeps the
// rollup's worker rows and the series' worker bands counting the same records.
func workerOf(r Record) (workerID, bool) {
	if r.Worker == "" {
		return workerID{}, false
	}
	switch r.Phase {
	case PhaseAuxiliary, PhaseSubagent:
		return workerID{phase: r.Phase, worker: r.Worker}, true
	}
	return workerID{}, false
}

func bucketFor[K comparable](m map[K]*Bucket, key K) *Bucket {
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
// 03:04:05Z would order AFTER 03:04:05.9Z: a watermark one record stale, or a
// TurnRow whose start and end are inverted.
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
