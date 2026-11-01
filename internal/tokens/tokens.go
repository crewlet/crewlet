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
// bucketed by [BucketDaily]) — every data node's day, so the answer is the
// same on whichever node is asked and still includes a node that has left the
// fleet (ADR-0020). The two are folded into the same [Rollup] and [Bucket], so
// a reader moving from "the last 24 hours" to "the last 7 days" compares like
// with like. What a daily row cannot answer it does not pretend to: a turn and
// an hour are not in it, so a windowed answer has no `by_turn` and a series
// has no hourly bucket — per-turn spend is the turn list's `sort=-tokens`.
//
// # A seat is its id, and is named from the chart
//
// Every per-seat row and band is keyed on the seat's AGENT ID — derived from
// its handle (ADR-0013), so every node computes it alike — and a per-unit band
// on the unit's KEY. What a row is CALLED comes from [Seats], the org as the
// caller holds it now: keyed on a role name, two seats sharing one were one
// row. A seat the org no longer holds keeps the newest name its rows carried
// and no handle, because a handle a removed seat freed may already be somebody
// else's and a wrong link is worse than none.
//
// A PERSON — the human seat an auxiliary record was spent for ([Record.Person])
// — is no agent seat and has no agent id, so their spend is keyed apart from
// every seat's, on the human seat's HANDLE ([PersonKey]), and named from that
// seat where the chart holds it. One key per spender, used alike by every fold
// here — the per-seat rows, the provider "used by" list and a series' seat and
// unit bands — so no two of them can count one spender differently.
//
// A leaf in everything but the calendar: it imports [period] and nothing else
// from Crewlet, because both ends need it — the live projection (which holds
// the records) and the query surface (which reads the usage rows). A week
// bucket is the company's ISO week, and only the calendar knows where one
// begins.
//
// BOTH PRODUCERS FILL EVERY FIELD OF [Record], and that is a contract rather
// than a coincidence: the live projection reads the phase record's payload and
// the event store reads the columns schema/0015 and schema/0032 promoted out of
// it, so a field one of them carries and the other drops is a rollup whose
// numbers change when a window crosses the live edge. The prompt-cache counts
// were exactly that until 0032 — summed here and filled by neither — so every
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
	"strings"
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

// Record is one spend record — a completed phase's, one launch of an
// executor's run, or an auxiliary record's coalesced calls — the aggregator's
// input, and the shape both producers hand it.
type Record struct {
	EventID   string `json:"event_id"`
	Timestamp string `json:"timestamp"`

	AgentID   string `json:"agent_id"`
	AgentRole string `json:"agent_role"`

	// Person is the PERSON an auxiliary record was spent for — the handle
	// of the human seat a person's question was asked from, or of the one
	// leading a unit whose pass ran on its chain — and empty on every other
	// record. A SEAT HANDLE, never an identity estate person id. A person is
	// no agent seat: no agent id, no seat budget and no turns, so the
	// per-seat rollup files the spend under the person ([PersonKey]) rather
	// than under an empty id every person would share.
	Person string `json:"person,omitempty"`

	// Phase is the phase that ran; [PhaseExecute] on a launch's record —
	// the `executor_collected` event, which carries no phase of its own
	// because it is none, and whose spend is the executor's — and
	// [PhaseAuxiliary] on an auxiliary record, the `auxiliary_spend` event,
	// for the same reason.
	Phase string `json:"phase"`
	// HostPhase is the phase a nested call ran under: a delegated worker,
	// or the round-cap extension judge.
	HostPhase string `json:"host_phase"`
	// Worker names what an auxiliary record's calls were FOR — its
	// purpose, `memory_filter` or `condense_thread` — and is set only when
	// Phase is "auxiliary", which is why the worker rollup keys on the
	// pair. A delegated worker's template rides the same field on its
	// `subagent` phase, and is kept out of that rollup by the pair.
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

	// Stage is WHOSE COST an auxiliary record is — `turn`, `reflection`,
	// `background` or `operator` — and empty on a phase's record and a
	// launch's, which are always their turn's. It decides one thing here:
	// whether the record is part of its turn's cost ([Record.InTurn]).
	Stage string `json:"stage,omitempty"`

	// Calls is how many provider calls the record covers: a phase's model
	// rounds, an executor launch's own CLI turns, an auxiliary record's
	// coalesced calls. ONE UNIT under one label everywhere a bucket says "N
	// calls" — it counted RECORDS once, so a forty-turn run was one call
	// beside a coalesced record of seventy rewrites. A phase's count is
	// [PhaseCalls] — none for an executor's own record, whose run's calls
	// are on its launches' records — a launch's is [LaunchCalls], and an
	// auxiliary record coalesces at least the call that produced it, so a
	// bucket sums what each record states.
	Calls int `json:"calls,omitempty"`

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

// StageTurn is the auxiliary stage spent inside a turn, for its work —
// types.AuxStageTurn, named here rather than imported so this package stays a
// leaf. A test holds the two equal.
const StageTurn = "turn"

// InTurn reports whether a record is part of its turn's cost: every phase's,
// every launch of its executor's run, and an auxiliary record of the
// [StageTurn] stage. A TURN'S COST IS DEFINED
// HERE, ONCE — the turn list, the live window's per-turn rows and the Turn
// screen all count what this admits — and the reflection after a turn is not in
// it: it carries the turn's id so the turn's page can draw it, and it is the
// seat's learning rather than what the work cost.
func (r Record) InTurn() bool { return r.Stage == "" || r.Stage == StageTurn }

// PhaseCounts is what a phase record says about the provider calls it covers,
// as [PhaseCalls] reads it off the record's own fields.
type PhaseCounts struct {
	// Phase is the record's `phase`.
	Phase string
	// Rounds is the length of its `rounds` list — one entry per provider
	// call it timed — and RoundsUsed its `rounds_used`.
	Rounds     int
	RoundsUsed int
}

// PhaseCalls is a phase record's provider calls.
//
//   - An EXECUTOR's record (`execute`) covers none. Every executor is its
//     coding CLI's own run: the engine's record of the phase states the run
//     as one round — the launch and its answer — but the engine made no
//     provider call for it, whether the run was collected or its launch was
//     refused. The run's calls are its CLI's, counted on each launch's own
//     record ([LaunchCalls]); counting the round as well put two calls on
//     every turn that nobody made.
//   - Every other phase covers its rounds: its `rounds` list's length where
//     it recorded one, its `rounds_used` where it counted rounds without
//     listing them, and one where neither says (a judge's single call).
//
// ONE RULE FOR BOTH PRODUCERS — the event store's writer and the live
// projection — so a phase counts the same calls on either side of the live
// edge. The executor's phase name is the event catalogue's (events/types),
// spelled here so this package stays a leaf; a test holds the spelling to the
// catalogue's constant, so a renamed phase cannot fall silently to the rounds
// rule.
func PhaseCalls(c PhaseCounts) int {
	switch {
	case c.Phase == PhaseExecute:
		return 0
	case c.Rounds > 0:
		return c.Rounds
	case c.RoundsUsed > 0:
		return c.RoundsUsed
	}
	return 1
}

// LaunchCalls is the provider calls one launch of an executor's run covers,
// off its own record (`executor_collected`): the model turns its CLI's own
// loop took, as the CLI reported them (`cli_turns`), and one where it reported
// none — the one run the engine asked for, which is the most it can say.
//
// ITS OWN RULE BESIDE [PhaseCalls], not a phase word folded into that one: a
// launch is no phase, and its record carries no `phase` to switch on. ONE RULE
// FOR BOTH PRODUCERS all the same — the event store's writer and the live
// projection count a launch by it — so the executor's calls are the same on
// either side of the live edge. A negative count is none reported, as the
// collection that floors a launch's usage reads it.
func LaunchCalls(cliTurns int) int { return max(cliTurns, 1) }

// Bucket is an accumulated total. Embedded rather than nested, because the
// wire shape spreads it into each row: {"phase": "execute", "total_tokens":
// 150, …}, not {"phase": "execute", "bucket": {…}}.
type Bucket struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// Calls is how many PROVIDER CALLS the bucket's spend was made in — see
	// [Record.Calls] for why that, and not a count of records.
	Calls int `json:"calls"`

	// CacheReadTokens and CacheWriteTokens sum the prompt cache's share of
	// the calls in this bucket.
	//
	// THE CONTRACT A READER DIVIDES BY: input_tokens ALREADY INCLUDES the
	// cached prefix — the backend reports it that way, adding a CLI's own
	// cache counts back into its input total — so these are a BREAKDOWN of
	// InputTokens and
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
	b.Calls += r.Calls
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

// WithoutEntries is the rollup for a reader who may not read the company's
// configuration: every figure but the per-entry breakdown, which names the
// company's providers.llm entries and is an admin's, as Settings, Models is.
// The totals and every other breakdown are the same, so nothing a reader is
// told stops adding up. A copy: r is not changed.
func (r Rollup) WithoutEntries() Rollup {
	r.ByProvider = []ProviderRow{}
	r.EntriesWithheld = true
	return r
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
	// ProviderKey is the configured entry, "unknown" on a call whose event
	// named no provider entry.
	ProviderKey string `json:"provider_key"`

	// Models are every model this entry answered with in the window,
	// biggest first.
	Models []string `json:"models"`

	// Seats are the TopProviderSeats seats that spent the most through
	// this entry, biggest first — each by its handle where the chart holds
	// it and by the name its rows carried otherwise, ranked by agent id so
	// a renamed seat is one entry; SeatsTotal is how many seats spent
	// through it at all, so a reader can say "and n more" rather than read
	// three names as the whole list.
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

// WorkerRow is the per-purpose breakdown of the AUXILIARY MODEL's spend: what
// the seats' cheap model was spent on — the turn-start context, each kind of
// compaction, the reflection workers, the background passes, a person's
// answered question. Its rows sum to the records of the `auxiliary` phase
// ([PhaseAuxiliary]), which is PART of the Auxiliary band and not the whole of
// it: [PhaseBand] also files the round-cap judge's phase, the first-turn
// onboarding and any phase this build does not know there, and none of those
// is a purpose here — so the band is this breakdown plus those phases.
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
	// Person marks a PERSON's row rather than a seat's: what the auxiliary
	// model spent for the human seat named by Handle ([Record.Person]). It
	// has no agent id, and on a named window no turns, since a person takes
	// none. Its Handle is ALWAYS set, chart or no chart, because it is the
	// only identity the row has — an agent seat's row is identified by its
	// id and may go without one.
	Person bool `json:"person,omitempty"`
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
	// Role and Handle name the seat the run belongs to — its CURRENT name
	// and handle where [Options.Seats] knows its id, and otherwise the name
	// its records carry — for the reason [AgentRow] gives.
	Role    string `json:"role"`
	Handle  string `json:"handle"`
	AgentID string `json:"agent_id"`
	// StartedAt is the earliest phase in the turn and EndedAt the latest,
	// ordered by INSTANT ([compareStamp]) and never by bytes: RFC3339Nano
	// trims trailing zeros, so a whole-second stamp sorts after every
	// fractional one in its own second when compared as a string.
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

	// AgentID is the one seat this answer was narrowed to, by its agent id —
	// the identity the caller filtered by — absent for the whole company.
	AgentID string `json:"agent_id,omitempty"`

	// Seat is that seat's handle NOW, from [Seats], for a link to it: absent
	// for the whole company and for a seat the chart no longer holds. Read
	// off the chart rather than echoed from the request, because the handle
	// a request typed may have been renamed since and is never what the
	// answer was keyed on.
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

	// EntriesWithheld says ByProvider was left out for a reader who may not
	// read which providers.llm entries the company configures, which is an
	// admin's ([Rollup.WithoutEntries]): its empty list then says nothing
	// about what was spent.
	EntriesWithheld bool `json:"entries_withheld,omitempty"`

	// ByTurn is the tail of recent turns — on the LIVE window only. A
	// company day's row holds no turn, so a named window has none to list
	// and the field is absent there; per-turn spend over any window is the
	// turn list sorted by tokens.
	ByTurn []TurnRow `json:"by_turn,omitempty"`

	// AggregatedThrough is the latest timestamp this rollup counted.
	//
	// THE ROLLUP'S OWN FRESHNESS, what a reader shows as "counted through": a
	// total with no instant beside it cannot be told from a stale one. It is
	// not a baseline anything folds onto. The client used to do that, with
	// this as the watermark it skipped already-counted events by; the server
	// holds the records and re-folds the whole window now, which is what
	// leaves exactly one implementation of the aggregation.
	//
	// A RECORD'S OWN STAMP, never cut to Until. A node whose clock runs fast
	// stamps its records past the window's end — by milliseconds, or by a
	// garbled year — and the live window holds such a record for a day from
	// its ARRIVAL rather than by that stamp, so for that day this reads past
	// Until. Cut to Until it would hide what says a node's clock is wrong,
	// and disagree with every by_turn bound beside it, which carries the
	// record's stamp regardless.
	//
	// The live window's only: a day's row carries no instant per call, so
	// a named window has no watermark to report and says nothing.
	AggregatedThrough string `json:"aggregated_through,omitempty"`
}

// Seat is what the org chart says about one seat NOW, for the rows that name
// it: the handle a row links to, the name it prints, and the unit a per-unit
// band files it under.
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

// Seats is the chart's seats by the key every spend record names its spender
// by: an AGENT seat under its agent id, and a HUMAN seat under [PersonKey] of
// its handle, since a person's spend names the human seat it was made for and
// a human seat has no agent id.
type Seats map[string]Seat

// Options tune one aggregation.
type Options struct {
	// Seats is the chart this node holds ([Seats]), so a per-agent row
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

// handle is the seat's handle now, or "" for one the chart does not hold — and
// for no seat at all.
func (s Seats) handle(agentID string) string {
	if agentID == "" {
		return ""
	}
	return s[agentID].Handle
}

// lookup is what the chart says about the spender filed under this key
// ([spenderKey]), and whether it says anything: an empty key names nobody,
// whatever the map holds under "".
func (s Seats) lookup(key string) (Seat, bool) {
	if key == "" {
		return Seat{}, false
	}
	seat, ok := s[key]
	return seat, ok
}

// spenderKey is the ONE key every fold here files a spender's spend under: a
// person's [PersonKey] when the spend was made for a person, and otherwise the
// seat's [seatKey]. One function, because the per-seat rows, the provider list
// and a series' bands each keyed spend for themselves once, and a person whose
// spend one of them filed under an empty agent id was counted as one spender
// with every other person there and as themselves everywhere else.
func spenderKey(agentID, role, person string) string {
	if person != "" {
		return PersonKey(person)
	}
	return seatKey(agentID, role)
}

// seatKey is the seat a spend row is filed under: its agent id, or — for a row
// carrying none — its role name, marked so that it can never equal an id.
func seatKey(agentID, role string) string {
	if agentID != "" {
		return agentID
	}
	return unnamedSeatPrefix + orUnknown(role)
}

// unnamedSeatPrefix marks a seat key that is a role name rather than an agent
// id. A colon is in no id this engine derives, which is a uuid.
const unnamedSeatPrefix = "role:"

// PersonKey is the key a PERSON's spend is filed under — and the key a caller
// files a HUMAN seat under in [Seats] — from the human seat's handle.
//
// Apart from every seat's key BY ITS SHAPE: an agent id is a uuid and carries
// no colon, and a [seatKey] role marker starts `role:`. Printable rather than
// NUL-separated, because it is a band's key on the wire ([GroupRow.Group],
// [GroupPart.Group]) as well as a map key here.
func PersonKey(handle string) string { return personPrefix + handle }

// personPrefix marks a [PersonKey].
const personPrefix = "person:"

// personHandle is the handle a [PersonKey] was made from, and whether key is
// one.
func personHandle(key string) (string, bool) { return strings.CutPrefix(key, personPrefix) }

// label picks a seat's printed name from the rows filed under it: the chart's
// current name when the chart knows the seat, and otherwise the name on the
// NEWEST row — the one closest to what the seat is called now, whatever order
// the rows arrived in.
//
// ORDER-INDEPENDENT ON A TIE TOO: two rows of one company day may carry two
// names (a rename that day), and a fold that kept whichever arrived first would
// name the seat differently on two nodes asked the same question. The greater
// name wins a tie, which is arbitrary and the same everywhere.
type label struct {
	name, at string
	carried  bool
}

// fold takes one row's name — and the instant or day label it was written at —
// into account and reports the name to print. A row that names nothing says
// nothing about what the seat is called.
func (l *label) fold(name, at string, seat Seat, known bool) string {
	switch c := compareStamp(at, l.at); {
	case known && seat.Name != "":
		l.name = seat.Name
	case name == "":
	case !l.carried, c > 0, c == 0 && name > l.name:
		l.name, l.at, l.carried = name, at, true
	}
	return orUnknown(l.name)
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
// Every bucket is a sum and every instant a min or a max, so no figure here
// depends on the order the records are handed over in — and neither does any
// NAME. A seat's row is keyed on its agent id and a person's on their handle
// ([spenderKey]), so neither is chosen by a record at all; what either is
// CALLED is the chart's name for it now, or, for one the chart no longer holds,
// the name its NEWEST-stamped record carried ([label]), as a named window names
// one by its newest day ([FoldDaily]).
//
// A turn's agent id and work key are the run's own — a run is one turn of one
// seat, for one unit of work — so every record of it that names one names the
// same one, and each is filled from whichever record carries it, a record
// carrying none leaving it be. Never fixed by the FIRST: a turn whose first
// record in window order came without its work key was listed with none, and
// the same records handed over the other way round named it.
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
		Seat:    opts.Seats.handle(opts.AgentID),
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
	agentNames := map[string]*label{}
	turnNames := map[string]*label{}

	for _, r := range records {
		phase := orUnknown(r.Phase)
		model := orUnknown(r.Model)
		key := spenderKey(r.AgentID, r.AgentRole, r.Person)
		seat, known := opts.Seats.lookup(key)

		if laterStamp(r.Timestamp, out.AggregatedThrough) {
			out.AggregatedThrough = r.Timestamp
		}

		out.Totals.add(r)
		bucketFor(byPhase, phase).add(r)
		bucketFor(byModel, model).add(r)
		// The provider list ranks spenders by the same key as every other
		// fold, and names them only once ranked ([seatNames]).
		providers.add(r.ProviderKey, model, key, Bucket{}.plus(r))
		// Keyed on the PAIR, not on the worker alone: Worker is set only
		// on an auxiliary phase, so a bare non-empty check would fold a
		// stray value on some other phase into a worker's total.
		if r.Phase == PhaseAuxiliary && r.Worker != "" {
			bucketFor(byWorker, r.Worker).add(r)
		}

		agent := byAgent[key]
		if agent == nil {
			agent = &AgentRow{AgentID: r.AgentID, Handle: seat.Handle, ByPhase: map[string]*Bucket{}}
			if r.Person != "" {
				// A PERSON IS KEYED APART from every seat, by their own
				// handle: a person's record names no agent of its own, only
				// the role of their seat, and a seat's row keyed on that would
				// take a person's spend as the seat's.
				agent.AgentID, agent.Handle, agent.Person = "", r.Person, true
			}
			byAgent[key] = agent
		}
		agent.Role = labelFor(agentNames, key).fold(r.AgentRole, r.Timestamp, seat, known)
		agent.Bucket.add(r)
		bucketFor(agent.ByPhase, phase).add(r)

		if r.TurnID == "" || !r.InTurn() {
			// A phase with no turn still counts toward every other
			// rollup — it is real spend — but it cannot be attributed to
			// a turn, and inventing a key would make one row per phase.
			// Nor can a reflection's spend, which names the turn it
			// learned from and is not that turn's cost ([Record.InTurn]).
			continue
		}
		turn := byTurn[r.TurnID]
		if turn == nil {
			turn = &TurnRow{
				TurnID:    r.TurnID,
				StartedAt: r.Timestamp, EndedAt: r.Timestamp,
				ByPhase: map[string]*Bucket{},
			}
			byTurn[r.TurnID] = turn
		}
		// FILLED from whichever record carries it, never fixed by the first:
		// each is the run's own and the same on every record of it that
		// names one, so a record without one says nothing about it (see
		// Aggregate).
		if r.WorkKey != "" {
			turn.WorkKey = r.WorkKey
		}
		if r.AgentID != "" {
			turn.AgentID, turn.Handle = r.AgentID, seat.Handle
		}
		turn.Role = labelFor(turnNames, r.TurnID).fold(r.AgentRole, r.Timestamp, seat, known)
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
	out.ByProvider = providers.rows(seatNames(opts.Seats, agentNames))
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
	byTokensThen(out.ByAgent, func(r AgentRow) (int, string) { return r.TotalTokens, agentTie(r) })

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
//
// THE SEAT IS ITS KEY ([seatKey]) while the fold runs and becomes words only in
// [providerFold.rows]: ranked by the name it was called, one seat renamed
// mid-window was two entries splitting its spend, and two seats sharing a name
// were one.
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
// its TopProviderSeats biggest seats, each seat in the words name gives it.
// Never nil, for the reason every list on the rollup is not.
func (f providerFold) rows(name func(seatKey string) string) []ProviderRow {
	out := make([]ProviderRow, 0, len(f))
	for key, acc := range f {
		seats := ranked(acc.seats)
		row := ProviderRow{
			ProviderKey: key,
			Models:      ranked(acc.models),
			SeatsTotal:  len(seats),
			Bucket:      acc.Bucket,
		}
		row.Seats = make([]string, 0, min(len(seats), TopProviderSeats))
		for _, seat := range seats[:min(len(seats), TopProviderSeats)] {
			row.Seats = append(row.Seats, name(seat))
		}
		out = append(out, row)
	}
	byTokensThen(out, func(r ProviderRow) (int, string) { return r.TotalTokens, r.ProviderKey })
	return out
}

// seatNames is how a "used by" list names a spender: its HANDLE where the chart
// holds it, which is what every other surface links by; a person's own handle,
// which is their key's whole content, where it does not; and otherwise the name
// its rows carried — words, never a blank entry and never a bare id.
func seatNames(seats Seats, labels map[string]*label) func(string) string {
	return func(key string) string {
		if seat, ok := seats.lookup(key); ok && seat.Handle != "" {
			return seat.Handle
		}
		if handle, ok := personHandle(key); ok {
			return orUnknown(handle)
		}
		if l := labels[key]; l != nil {
			return orUnknown(l.name)
		}
		return orUnknown("")
	}
}

// ranked is a map's keys, biggest value first, ties on the key.
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

// agentTie is what equal-total rows of a per-seat fold are ordered by: the
// row's NAME, so a reader sees equal rows alphabetically, and then the key the
// fold filed it under ([spenderKey]), since two seats may share a name.
//
// NEVER THE NAME AND THE AGENT ID ALONE: every person row carries one empty
// agent id and two people of one role share a role, so two people with equal
// totals compared equal and came back in Go's randomised map order — which
// [byTokensThen] exists to stop. A person's key is their handle, which they
// share with nobody.
func agentTie(r AgentRow) string {
	person := ""
	if r.Person {
		person = r.Handle
	}
	return r.Role + "\x00" + spenderKey(r.AgentID, r.Role, person)
}

// PhaseAuxiliary is the phase an auxiliary record is filed under, and the one
// whose records carry a worker. Named here rather than imported from the event
// catalogue so this package stays a leaf.
const PhaseAuxiliary = "auxiliary"

// PhaseExecute is the phase an executor launch's record is filed under — the
// executor's own, since a launch is the executor's run doing its round's work.
// Named here for [PhaseAuxiliary]'s reason, and held to the event catalogue's
// constant by a test.
const PhaseExecute = "execute"

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
