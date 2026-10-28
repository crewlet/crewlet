package config

// TurnEngine configures the two-stage turn: an executor that decides and acts
// in one agentic run — the seat's coding CLI's own, in a box — and a reviewer
// that judges the record it left, in the engine's own tool loop.
//
// Almost every field here is a CAP, and every cap is a trade between a turn
// that gives up too early and one that burns a budget thrashing. The
// numbers are defaults an operator can move; what they must not be is
// absent, which is what a zero would mean if these were read raw. Every
// accessor below applies the default, so a config that says nothing and a
// config that says `turn_engine:` behave identically.
//
// Unvalidated, a max_iterations of 0 parses cleanly and fails every turn in
// the company on the first guard check, with nothing in the file looking
// wrong. Hence the bounds below.
type TurnEngine struct {
	// MaxIterations caps execute-review rounds per turn. Review returning
	// self_iterate increments it; on breach the turn is terminated as
	// failed with a guard-breach event.
	//
	// EACH ITERATION IS A WHOLE EXECUTOR RUN — a box, its setup, a fresh
	// launch of the CLI — so this bounds what a turn may spend on
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
	// engine sets and a loop the engine runs: the executor is its CLI's own
	// run, capped by providers.sandbox.default_max_turns, and the reviewer
	// holds a single submission tool.
	ExtensionEnabled Toggle `yaml:"extension_enabled,omitempty" json:"extension_enabled,omitzero" desc:"Round-cap extension judge for the onboarding pass (default on)."`

	// OnboardingMaxToolRoundsCeiling is what the judge may grant the
	// onboarding pass up to. Setting it equal to onboarding_max_tool_rounds
	// disables extensions.
	OnboardingMaxToolRoundsCeiling int `yaml:"onboarding_max_tool_rounds_ceiling,omitempty" json:"onboarding_max_tool_rounds_ceiling,omitempty" js:"min=0" desc:"Hard ceiling for onboarding rounds across extensions."`

	// ExtensionRoundStep caps what one judge call may grant the onboarding
	// pass. Repeated exhaustion during an extended pass calls the judge
	// again, so this is per extension rather than per turn.
	ExtensionRoundStep int `yaml:"extension_round_step,omitempty" json:"extension_round_step,omitempty" js:"min=0" desc:"Maximum onboarding rounds one extension call may grant."`

	// ExecutorMinBudgetTokens is a pre-flight floor on EVERY executor
	// launch — a turn's first and each relaunch after a self_iterate:
	// below this much budget left, or when the budget cannot be read, the
	// launch is refused and the seat parks until the window turns over or
	// a ceiling moves, rather than launching a run that dies mid-way having
	// produced nothing. 0 turns the floor off. See
	// [defaultExecutorMinBudgetTokens] for where the default comes from,
	// and read it through [TurnEngine.ExecutorFloor].
	//
	// A POINTER for onboarding_max_tool_rounds' reason: an explicit 0 must
	// survive the store, which decodes a revision onto the defaults.
	ExecutorMinBudgetTokens *int `yaml:"executor_min_budget_tokens,omitempty" json:"executor_min_budget_tokens,omitempty" js:"min=0" desc:"Refuse an executor launch, and park the seat, below this remaining token budget (default 25000; 0 = no floor)."`

	// ConversationSession is the cross-turn ledger. It is nested here
	// rather than beside it because the turn engine reads it on every
	// turn, so it rides the same live settings cell and hot-reloads
	// through the same path.
	ConversationSession ConversationSession `yaml:"conversation_session,omitempty" json:"conversation_session"`
}

// Turn-engine defaults the fields cannot carry themselves, because an unset
// field and an explicit 0 mean different things there.
const (
	// defaultExecutorMinBudgetTokens is 25000 because the floor has to
	// cover the FIRST REQUEST of a run, which the launch cannot shrink: the
	// CLI's own harness prompt and tool definitions (about 15k input tokens
	// for Claude Code), the seat's bridged tool array beside them, and the
	// seat's system prompt. A floor below one such request refuses nothing
	// it was meant to — the 2000 it replaced was sized for a delegated
	// coding job's brief, and passed every run that then died on its first
	// round.
	defaultExecutorMinBudgetTokens = 25000

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

// ExecutorFloor is the remaining token budget below which an executor
// launch is refused, applying the default: 0 is no floor.
func (t *TurnEngine) ExecutorFloor() int {
	if t.ExecutorMinBudgetTokens == nil {
		return defaultExecutorMinBudgetTokens
	}
	return *t.ExecutorMinBudgetTokens
}

func (t *TurnEngine) validate(path Path) error {
	var p problems

	positive := []struct {
		name  string
		value int
	}{
		{"max_iterations", t.MaxIterations},
		{"delegation_depth_limit", t.DelegationDepthLimit},
		{"extension_round_step", t.ExtensionRoundStep},
	}
	for _, f := range positive {
		if f.value < 1 {
			p.add(at(path, f.name), ErrOutOfRange,
				"must be at least 1, got %d: a cap of zero fails every turn "+
					"on its first guard check", f.value)
		}
	}

	nonNegative := []struct {
		name  string
		value int
	}{
		// 0 is meaningful here: it turns the launch floor off.
		{"executor_min_budget_tokens", t.ExecutorFloor()},
		// 0 is meaningful here: it disables the dedicated onboarding pass.
		{"onboarding_max_tool_rounds", t.OnboardingRounds()},
	}
	for _, f := range nonNegative {
		if f.value < 0 {
			p.add(at(path, f.name), ErrOutOfRange, "must not be negative, got %d", f.value)
		}
	}

	p.wrap(t.Delegation.validate(at(path, "delegation")))

	// A ceiling below its own base is not a smaller budget, it is a
	// contradiction: the pass starts above the ceiling the judge may grant
	// up to, so the extension path can only ever refuse. Equal is the
	// documented way to disable extensions.
	if t.OnboardingMaxToolRoundsCeiling < t.OnboardingRounds() {
		p.add(at(path, "onboarding_max_tool_rounds_ceiling"), ErrOutOfRange,
			"must be at least onboarding_max_tool_rounds (%d), got %d: set them "+
				"equal to disable extensions", t.OnboardingRounds(),
			t.OnboardingMaxToolRoundsCeiling)
	}

	p.wrap(t.ConversationSession.validate(at(path, "conversation_session")))
	return p.err()
}

// ConversationSession is the per-conversation ledger: what a seat already
// did in one conversation — a chat thread, an issue, a pull request —
// carried into the NEXT turn of that conversation as a block on the
// executor's user message.
//
// The cross-turn counterpart of the within-turn prior-work ledger, and
// deliberately STRUCTURED rather than a transcript replay.
type ConversationSession struct {
	// Enabled records and replays completed turns.
	//
	// On by default: the block is deterministic, bounded, and is the only
	// context a thin-trigger turn gets. Turning it off restores the
	// pre-ledger prompt exactly, which is what makes it a safe live kill
	// switch.
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Record and replay per-conversation history (default on)."`

	// MaxEntries is what is KEPT per conversation, trimmed at write time.
	//
	// It is not the only bound, and the split is deliberate: this one bounds
	// the RECORD, and [ledger.InjectedMaxChars] bounds what one prompt
	// SHOWS, by dropping whole entries oldest-first and saying how many.
	// Neither ever cuts inside an entry — the stored row is the only copy of
	// that turn, and a half-recorded reply reads as the whole of what the
	// seat said.
	//
	// There is no injected-side CONFIG pair. Two knobs (injected_max_entries,
	// injected_max_chars) used to sit here; neither was ever threaded to a
	// caller, so both validated, defaulted and documented a truncation that
	// did not happen. The render bound that replaced them is a constant with
	// its reasoning at the definition rather than a knob nobody had cause to
	// set.
	// Larger than what any prompt injects, so the dashboard can show a
	// conversation's history beyond what a single turn carried. The trim
	// is what bounds a DM, whose conversation key is the whole channel and
	// so never stops receiving entries.
	MaxEntries int `yaml:"max_entries,omitempty" json:"max_entries,omitempty" js:"min=0" desc:"Entries kept per conversation."`

	// RetentionDays is how long a conversation is remembered. It matches
	// the event store's own horizon, so the engine's memory of a
	// conversation and the telemetry showing what it did there forget
	// together.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0" desc:"How long a conversation is remembered."`
}

// DefaultConversationSession is the ledger's shipped defaults.
func DefaultConversationSession() ConversationSession {
	return ConversationSession{
		MaxEntries:    20,
		RetentionDays: 30,
	}
}

// Records reports whether the ledger is on, applying the true default.
func (c *ConversationSession) Records() bool { return c.Enabled.Or(true) }

func (c *ConversationSession) validate(path Path) error {
	var p problems
	if c.MaxEntries < 1 {
		p.add(at(path, "max_entries"), ErrOutOfRange, "must be at least 1, got %d", c.MaxEntries)
	}
	if c.RetentionDays < 1 {
		p.add(at(path, "retention_days"), ErrOutOfRange,
			"must be at least 1, got %d", c.RetentionDays)
	}
	return p.err()
}
