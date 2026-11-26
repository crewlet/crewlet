package config

import (
	"fmt"
	"time"
)

// TurnEngine configures the two-stage turn: an executor that decides and acts
// in one agentic run (the seat's harness's own, in a box) and a reviewer that
// judges the record it left, in the engine's own tool loop.
//
// Almost every field is a CAP, a trade between a turn that gives up too early
// and one that burns a budget thrashing. The numbers are defaults an operator
// can move but never absent, which is what a raw zero would mean, so every
// accessor applies the default and a config saying nothing behaves like one
// saying `turn_engine:`.
//
// Unvalidated, a max_iterations of 0 parses cleanly and fails every turn on the
// first guard check with nothing in the file looking wrong. Hence the bounds.
type TurnEngine struct {
	// MaxIterations caps execute-review rounds per turn. Review returning
	// self_iterate increments it; on breach the turn is terminated as
	// failed with a guard-breach event.
	//
	// EACH ITERATION IS A WHOLE EXECUTOR RUN — a box, its setup, a fresh
	// launch of the harness — so this bounds what a turn may spend on
	// re-doing its work, not merely how many reviews it gets.
	MaxIterations int `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty" js:"min=0" desc:"Execute/review iterations per turn."`

	// Delegation bounds every `delegate` call: how many workers run at
	// once, how much of the turn's budget they may spend between them, and
	// how long any of it may take. Nested here rather than beside the
	// top-level workers: block because these are the engine's CAPS and
	// that block is authored content — see [Delegation].
	Delegation Delegation `yaml:"delegation,omitempty" json:"delegation"`

	// DelegationDepthLimit caps colleague-to-colleague chains. A turn
	// triggered by another agent inherits its depth plus one; on breach
	// the turn is terminated as failed.
	DelegationDepthLimit int `yaml:"delegation_depth_limit,omitempty" json:"delegation_depth_limit,omitempty" js:"min=0" desc:"Maximum depth of agent-to-agent delegation chains."`

	// OnboardingMaxToolRounds is the dedicated first-turn onboarding
	// pass's own budget. It runs BEFORE the executor with its own rounds
	// so onboarding never starves the work submission on a first turn.
	// 0 disables onboarding: the dedicated pass is the only place a seat
	// onboards, since the executor never reaches mark_onboarded. Read it
	// through [TurnEngine.OnboardingRounds].
	//
	// A POINTER, because 0 is a setting rather than an absence: a stored
	// revision is decoded onto the defaults and an encoder drops a zero
	// int, so a company that turned onboarding off had it back on — at the
	// default — the first time its revision was read from the store.
	OnboardingMaxToolRounds *int `yaml:"onboarding_max_tool_rounds,omitempty" json:"onboarding_max_tool_rounds,omitempty" js:"min=0" desc:"Rounds for the first-turn onboarding pass (default 10); 0 disables it."`

	// ExtensionEnabled is the master switch for the round-cap extension
	// judge on the ONBOARDING PASS: on exhaustion, a cheap model decides
	// whether the pass is making progress or thrashing, and grants rounds
	// or falls through to the rescue path.
	//
	// Onboarding only, because it is the one pass with a round budget the
	// engine sets and a loop the engine runs: the executor is its harness's own
	// run, capped only where the company or the seat sets a turn cap
	// (providers.sandbox.default_max_turns, role.sandbox.max_turns), and the
	// reviewer holds a single submission tool.
	ExtensionEnabled Toggle `yaml:"extension_enabled,omitempty" json:"extension_enabled,omitzero" desc:"Round-cap extension judge for the onboarding pass (default on)."`

	// OnboardingMaxToolRoundsCeiling is what the judge may grant the
	// onboarding pass up to. Setting it equal to onboarding_max_tool_rounds
	// disables extensions.
	OnboardingMaxToolRoundsCeiling int `yaml:"onboarding_max_tool_rounds_ceiling,omitempty" json:"onboarding_max_tool_rounds_ceiling,omitempty" js:"min=0" desc:"Hard ceiling for onboarding rounds across extensions."`

	// ExtensionRoundStep caps what one judge call may grant the onboarding
	// pass. Repeated exhaustion during an extended pass calls the judge
	// again, so this is per extension rather than per turn.
	ExtensionRoundStep int `yaml:"extension_round_step,omitempty" json:"extension_round_step,omitempty" js:"min=0" desc:"Maximum onboarding rounds one extension call may grant."`

	// ExecutorMinBudgetTokens is the fixed part of a pre-flight floor on EVERY
	// executor launch, first and each relaunch after a self_iterate: what a
	// run's first request carries before its brief (internal/engine's
	// launchFloor). Below that budget, or when it cannot be read, the launch is
	// refused and the seat parks until the window turns over or a ceiling moves,
	// rather than launching a run that dies having produced nothing. A launch
	// continuing a measured session is held to the larger of this and that
	// context. 0 turns the floor off for an unknown context. Default
	// [defaultExecutorMinBudgetTokens]; read it via [TurnEngine.ExecutorFloor].
	//
	// A POINTER for onboarding_max_tool_rounds' reason: an explicit 0 must
	// survive the store, which decodes a revision onto the defaults.
	ExecutorMinBudgetTokens *int `yaml:"executor_min_budget_tokens,omitempty" json:"executor_min_budget_tokens,omitempty" js:"min=0" desc:"Fixed part of an executor launch's budget floor, counted beside the launch's brief: below the two, the launch is refused and the seat parked (default 65000; 0 = no floor for a launch whose session context is unknown)."`

	// ConversationSession is how a seat's executor sessions are kept and
	// continued across a conversation's turns. It is nested here rather
	// than beside it because the turn engine reads it on every turn, so it
	// rides the same live settings cell and hot-reloads through the same
	// path.
	ConversationSession ConversationSession `yaml:"conversation_session,omitempty" json:"conversation_session"`
}

// Turn-engine defaults the fields cannot carry themselves, because an unset
// field and an explicit 0 mean different things there.
const (
	// defaultExecutorMinBudgetTokens is 65000 because the floor has to
	// cover the FIRST REQUEST of a run, which the launch cannot shrink: the
	// harness's own prompt and tool definitions, the seat's bridged tools
	// and its standing instructions. For a minimal seat that is 34-46k
	// tokens on most runners, 48k on Claude Code (59k with its tool search
	// off) and at most 64.1k, on Muse Code; a harness deferring every
	// server's tools sends 11-16k (o200k; ADR-0066 has the measure).
	// A floor below one such request refuses nothing it was meant to. It
	// holds no brief: a brief's size is known only once the launch has it,
	// and the launch counts it beside this (internal/engine's launchFloor),
	// so no default here could be right for a one-line ask and a round's
	// hundred calls alike.
	defaultExecutorMinBudgetTokens = 65000

	// defaultOnboardingRounds is the pass's own budget: activate the
	// knowledge-base tools, read the team's onboarding pages, capture the
	// conventions and mark the seat — a handful of calls, with the extension
	// judge granting up to the ceiling a pass that is still reading.
	defaultOnboardingRounds = 10
)

// DefaultTurnEngine is the turn engine's shipped defaults.
func DefaultTurnEngine() TurnEngine {
	return TurnEngine{
		MaxIterations:        3,
		Delegation:           DefaultDelegation(),
		DelegationDepthLimit: 3,
		// 2x the pass's base, which is what an extension is for: a pass
		// that is genuinely progressing gets a second budget, and one that
		// is thrashing hits a wall an operator can see in the numbers.
		OnboardingMaxToolRoundsCeiling: 2 * defaultOnboardingRounds,
		ExtensionRoundStep:             8,
		ConversationSession:            DefaultConversationSession(),
	}
}

// Extends reports whether the onboarding pass's round-cap extension judge is
// on, applying the true default.
func (t *TurnEngine) Extends() bool { return t.ExtensionEnabled.Or(true) }

// OnboardingRounds is the onboarding pass's round budget, applying the
// default: 0 is a pass that never runs.
func (t *TurnEngine) OnboardingRounds() int {
	if t.OnboardingMaxToolRounds == nil {
		return defaultOnboardingRounds
	}
	return *t.OnboardingMaxToolRounds
}

// ExecutorFloor is the executor launch floor's fixed part, applying the
// default: the remaining token budget below which a launch is refused before
// its brief is counted beside it. 0 is no floor for a launch whose context is
// not known.
func (t *TurnEngine) ExecutorFloor() int {
	if t.ExecutorMinBudgetTokens == nil {
		return defaultExecutorMinBudgetTokens
	}
	return *t.ExecutorMinBudgetTokens
}

func (t *TurnEngine) validate(path Path) error {
	var p problems

	// EACH CAP'S BOUND IS DECLARED ONCE, in settingRanges, which a screen
	// reads too ([Range]).
	for _, f := range []struct {
		name  string
		value int
	}{
		{"max_iterations", t.MaxIterations},
		{"delegation_depth_limit", t.DelegationDepthLimit},
		{"extension_round_step", t.ExtensionRoundStep},
	} {
		p.within(at(path, f.name), float64(f.value),
			"a cap of zero fails every turn on its first guard check")
	}
	// 0 is meaningful for both: it turns the launch floor off for a launch
	// whose context is not known, and disables the dedicated onboarding pass.
	p.within(at(path, "executor_min_budget_tokens"), float64(t.ExecutorFloor()), "")
	p.within(at(path, "onboarding_max_tool_rounds"), float64(t.OnboardingRounds()), "")

	p.wrap(t.Delegation.validate(at(path, "delegation")))

	// A ceiling below its own base is not a smaller budget, it is a
	// contradiction: the pass starts above the ceiling the judge may grant
	// up to, so the extension path can only ever refuse. Equal is the
	// documented way to disable extensions.
	//
	// IN A PERSON'S WORDS TOO: Settings › Limits changes a first turn's
	// rounds, and a person raising them past the ceiling is told what the
	// ceiling is, not a key.
	if t.OnboardingMaxToolRoundsCeiling < t.OnboardingRounds() {
		p.addSaying(at(path, "onboarding_max_tool_rounds_ceiling"), ErrOutOfRange,
			fmt.Sprintf("Tool rounds on a first turn can be at most %d, the most an "+
				"extension may grant a first turn.", t.OnboardingMaxToolRoundsCeiling),
			"must be at least onboarding_max_tool_rounds (%d), got %d: set them "+
				"equal to disable extensions", t.OnboardingRounds(),
			t.OnboardingMaxToolRoundsCeiling)
	}

	p.wrap(t.ConversationSession.validate(at(path, "conversation_session")))
	return p.err()
}

// ConversationSession is how a seat's executor sessions are kept and
// continued: each launch's harness session, captured when its run ends and
// named by its turn's row on the executor log (internal/executordomain), is
// continued by the turn's relaunches and by the NEXT turn of its conversation
// — a chat thread, an issue, a pull request, or a schedule, every fire of which
// is a turn of the schedule's conversation of its runner seat — which is also
// told how the conversation's earlier turns ended.
//
// The session is the record of what a seat did in a conversation, transcript
// and all; the engine adds only what a transcript cannot know — how each turn
// ended, where the new turn begins, and that its box is new
// (docs/concepts/conversation-sessions.md).
type ConversationSession struct {
	// Enabled keeps each executor launch's harness session in the object
	// store (internal/executordomain), continues it in the turn's relaunches
	// and in the conversation's next turn, and tells that turn how the
	// conversation's earlier turns ended.
	//
	// On by default. Off, no session is captured, continued or offered back
	// (kept ones included; a running launch is refused at its next call to a
	// session tool, which asks the revision its node runs), no conversation head
	// is read, and no turn is told how earlier ones ended: one safe live kill
	// switch for everything a conversation carries between turns. How each turn
	// ended is still recorded on the executor log, the account of the turn.
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Keep each executor launch's harness session, continue it in the turn's relaunches and the conversation's next turn, and tell that turn how the conversation's earlier turns ended (default on; off keeps and continues no session, offers no tool to read them, and still records how each turn ended)."`

	// RetentionDays is how long a seat's executor sessions are kept past
	// their turn's last instant: a turn's row on the executor log, and the
	// session it names, is deleted this many days after its last record
	// unless a later turn of its conversation took it up first — so a
	// conversation quiet for longer starts its next turn fresh, and a
	// schedule whose fires come further apart than this starts every fire
	// fresh: a schedule is a conversation like any other, and this is how
	// long one is remembered. Read at every pass of the reaper that deletes
	// them, so a revision that moves it is honoured on the next pass rather
	// than the next boot. Thirty days by default, the event store's own
	// horizon, so the engine's memory of a conversation and the telemetry
	// showing what it did there forget together.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0" desc:"Days a seat's executor sessions are kept past their turn's last record, read at every reaper pass (default 30)."`

	// MaxBytesPerSeat is the most object-store bytes one seat's captured
	// executor sessions may hold (internal/executordomain): past it a new
	// capture releases the seat's oldest ENDED turns' sessions, keeping how
	// each ended, and a capture its live turns leave no room for is refused,
	// so that turn's next launch starts fresh. A session a live turn's first
	// launch was handed to continue is HELD, and counted, until that turn
	// keeps its own or ends. It is also the allowance of the seat's saves of
	// running sessions, whose superseded objects the store keeps about a
	// day, so a seat holds up to TWICE it. SIXTY-FOUR MEBIBYTES BY DEFAULT
	// ([DefaultSessionBytesPerSeat]), which says why. The engine reads it
	// off the revision the fleet runs, and every node applies the one
	// number a capture carries.
	MaxBytesPerSeat int64 `yaml:"max_bytes_per_seat,omitempty" json:"max_bytes_per_seat,omitempty" js:"min=51380224;max=68719476736" desc:"Object-store bytes one seat's captured executor sessions may hold; past it the oldest ended turns' sessions are released. It is also the allowance of the seat's saves of running sessions, which the object store keeps for about a day once a newer one replaces them, so a seat holds up to twice it (default 64 MiB, 49 MiB..64 GiB)."`

	// SaveIntervalSeconds is the shortest time between saves of a running
	// executor's session: the node driving its box keeps what
	// the harness has written so far as the turn's newest capture, so a run
	// whose box or node is lost continues from that save. A seat's saves are
	// paced by their size beyond it, so its superseded saves hold at most
	// [ConversationSession.MaxBytesPerSeat] while they wait for the
	// collector; the desc gives the figures. 0 takes [DefaultSaveInterval];
	// any other value outside [MinSaveInterval]..[MaxSaveInterval] is
	// refused, never clamped (a clamp leaves the document saying one thing
	// and the node another). There is no "off" short of `enabled: false`,
	// which keeps no session at all. Each node reads it off its own
	// revision, since a cadence binds only the node driving the box.
	SaveIntervalSeconds int `yaml:"save_interval_seconds,omitempty" json:"save_interval_seconds,omitempty" js:"min=0;max=3600" desc:"Shortest time in seconds between saves of a running executor's session (default 600, 0 takes it, 300..3600). A seat's saves are paced by their size beyond it, so at the default max_bytes_per_seat a session past about 0.4 MiB sealed is saved about every 27 minutes per sealed MiB, twice that once the seat has saved for a day."`
}

// The per-seat session budget's default and bounds.
const (
	// DefaultSessionBytesPerSeat is what an unset max_bytes_per_seat takes:
	// SIXTY-FOUR MEBIBYTES. A session compresses to one or two MiB for a day's
	// work and its text is bounded at forty-eight (executorkeep.SessionMaxBytes),
	// so 64 MiB holds a live turn's session at that bound beside about ten
	// ordinary ones, or thirty to forty alone; a capture at the kept bound (94 MiB
	// in all) is kept only within the seat's budget. A live turn's capped session
	// never needs the head it continued: its capture holds that transcript. The
	// budget shares the bucket with every other object, on the default nats
	// backend a quarter of the broker's limit, about 2.6 GiB at the smallest data
	// volume crewlet-operator admits; a seat at it holds up to twice it while its
	// superseded saves wait for the collector: five seats take a quarter of the
	// bucket, and twenty or more belong on S3 (store.objects.backend).
	DefaultSessionBytesPerSeat int64 = 64 << 20

	// SessionBytesPerSeatFloor is the least a seat may be given: ONE TEXT
	// SESSION AT ITS BOUND, sealed (executorkeep.SealedMaxBytes, a little
	// over 48 MiB), rounded up to a whole mebibyte, so a capture at the text
	// bound is never one the budget refuses outright. A test
	// in internal/executordomain holds the two together, since this package
	// cannot import that one.
	SessionBytesPerSeatFloor int64 = 49 << 20

	// SessionBytesPerSeatCeiling is a typo guard: a thousand times the
	// default is past any bucket a seat could be given.
	SessionBytesPerSeatCeiling int64 = 64 << 30
)

// The bounds of save_interval_seconds, and what an unset one takes.
const (
	// MinSaveInterval is the floor: FIVE MINUTES, sized from the executor
	// log's census (DefaultExecutorLogMaxBytes says it). Every save writes
	// one capture record of about a kibibyte, and a long launch saving at
	// this floor at most doubles its turn's records, about 44 MB a day at
	// the reference company against the default's 32 MB, which the log's
	// ceiling still holds for more than a week of a blocked trim. Below it a
	// saving fleet's records would outgrow the log's sizing.
	MinSaveInterval = 5 * time.Minute

	// DefaultSaveInterval is what an unset or 0 save_interval_seconds takes:
	// TEN MINUTES. A lost run redoes what it did since its newest save, so
	// about this plus one save's take, longer where the pacing by size
	// spaces a large session's saves further apart. A judgment, to be tuned
	// from what a lost run's line reports as the time since its newest save
	// (since_save_s).
	DefaultSaveInterval = 10 * time.Minute

	// MaxSaveInterval is a typo guard: past an hour a save hardly happens
	// before an ordinary run ends, so a larger value is likelier a stray
	// zero or milliseconds than a cadence anybody meant. (Minutes typed as
	// seconds come out below the floor, which refuses them.)
	MaxSaveInterval = time.Hour
)

// DefaultConversationSession is the block's shipped defaults.
func DefaultConversationSession() ConversationSession {
	return ConversationSession{
		RetentionDays:   30,
		MaxBytesPerSeat: DefaultSessionBytesPerSeat,
	}
}

// SessionBudget is the per-seat session budget. A document is decoded onto the
// defaults and validation refuses anything outside the bounds, so a validated
// company always states one; the default stands in for a block built in code
// with none, which is unset rather than a budget of nothing.
func (c *ConversationSession) SessionBudget() int64 {
	if c.MaxBytesPerSeat <= 0 {
		return DefaultSessionBytesPerSeat
	}
	return c.MaxBytesPerSeat
}

// SaveInterval is the shortest time between saves of a running executor's
// session, the default where the field is unset. Validation refuses every
// other value outside the bounds, so this never answers 0 and never a value a
// document did not say.
func (c *ConversationSession) SaveInterval() time.Duration {
	if c.SaveIntervalSeconds <= 0 {
		return DefaultSaveInterval
	}
	return time.Duration(c.SaveIntervalSeconds) * time.Second
}

// KeepsSessions reports whether each executor launch's harness session is
// kept and continued, applying the true default: one switch, so one gesture
// turns off everything a conversation carries between turns — its session,
// and the account of how its earlier turns ended.
func (c *ConversationSession) KeepsSessions() bool { return c.Enabled.Or(true) }

func (c *ConversationSession) validate(path Path) error {
	var p problems
	if c.RetentionDays < 1 {
		p.add(at(path, "retention_days"), ErrOutOfRange,
			"must be at least 1, got %d", c.RetentionDays)
	}
	if b := c.MaxBytesPerSeat; b < SessionBytesPerSeatFloor || b > SessionBytesPerSeatCeiling {
		p.add(at(path, "max_bytes_per_seat"), ErrOutOfRange,
			"must be between %d (one executor session at its bound, sealed) and %d, got %d",
			SessionBytesPerSeatFloor, SessionBytesPerSeatCeiling, b)
	}
	// REFUSED, NEVER CLAMPED, and compared in seconds so a value too large
	// for a Duration cannot wrap into the range.
	if secs := c.SaveIntervalSeconds; secs != 0 &&
		(secs < int(MinSaveInterval/time.Second) || secs > int(MaxSaveInterval/time.Second)) {
		p.add(at(path, "save_interval_seconds"), ErrOutOfRange,
			"must be between %d and %d, got %d: leave it unset for the default of %d "+
				"(enabled: false is what keeps no session at all)",
			int(MinSaveInterval/time.Second), int(MaxSaveInterval/time.Second), secs,
			int(DefaultSaveInterval/time.Second))
	}
	return p.err()
}
