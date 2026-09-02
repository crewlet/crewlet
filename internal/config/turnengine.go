package config

// TurnEngine configures the two-stage turn: an executor that decides and acts
// in one agentic loop, and a reviewer that judges the record it left.
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

	// MaxToolRounds caps rounds within one executor run. A round is one LLM
	// call plus the tools it emits. On breach the executor exits and the
	// reviewer sees the exhaustion, which typically forces another
	// iteration.
	//
	// It is the ONE round cap a turn's own work has now: the executor
	// plans, discovers and acts in one conversation, so the budget the
	// three-phase engine split across plan_max_tool_rounds (16) and
	// max_tool_rounds (20) is one number. The reviewer has no knob — it
	// holds a single submission tool, so its budget is a structural fact
	// rather than an operator preference.
	MaxToolRounds int `yaml:"max_tool_rounds,omitempty" json:"max_tool_rounds,omitempty" js:"min=0" desc:"Tool rounds within one executor run."`

	// OnboardingMaxToolRounds is the dedicated first-turn onboarding
	// pass's own budget. It runs BEFORE the executor with its own rounds
	// so onboarding never starves the work submission on a first turn.
	// 0 disables the dedicated pass.
	OnboardingMaxToolRounds int `yaml:"onboarding_max_tool_rounds,omitempty" json:"onboarding_max_tool_rounds,omitempty" js:"min=0" desc:"Rounds for the first-turn onboarding pass; 0 disables it."`

	// ExtensionEnabled is the master switch for the round-cap extension
	// judge: on exhaustion, a cheap model decides whether the phase is
	// making progress or thrashing, and grants rounds or falls through to
	// the rescue path.
	ExtensionEnabled Toggle `yaml:"extension_enabled,omitempty" json:"extension_enabled,omitzero" desc:"Round-cap extension judge (default on)."`

	// The ceilings the judge may grant up to, per phase. Setting one equal
	// to that phase's base round count disables extensions for that phase
	// without disabling them for the others.
	ExecuteMaxToolRoundsCeiling    int `yaml:"execute_max_tool_rounds_ceiling,omitempty" json:"execute_max_tool_rounds_ceiling,omitempty" js:"min=0" desc:"Hard ceiling for executor rounds across extensions."`
	OnboardingMaxToolRoundsCeiling int `yaml:"onboarding_max_tool_rounds_ceiling,omitempty" json:"onboarding_max_tool_rounds_ceiling,omitempty" js:"min=0" desc:"Hard ceiling for onboarding rounds across extensions."`

	// ExtensionRoundStep caps what one judge call may grant. Repeated
	// exhaustion during an extended run calls the judge again, so this is
	// per extension rather than per turn.
	ExtensionRoundStep int `yaml:"extension_round_step,omitempty" json:"extension_round_step,omitempty" js:"min=0" desc:"Maximum rounds one extension call may grant."`

	// SandboxMinBudgetTokens is a pre-flight floor: refuse to launch a
	// coding run below it, rather than launching one that dies mid-run
	// having produced nothing.
	SandboxMinBudgetTokens int `yaml:"sandbox_min_budget_tokens,omitempty" json:"sandbox_min_budget_tokens,omitempty" js:"min=0" desc:"Refuse a coding run below this remaining budget."`

	// ConversationSession is the cross-turn ledger. It is nested here
	// rather than beside it because the turn engine reads it on every
	// turn, so it rides the same live settings cell and hot-reloads
	// through the same path.
	ConversationSession ConversationSession `yaml:"conversation_session,omitempty" json:"conversation_session"`
}

// DefaultTurnEngine is the turn engine's shipped defaults.
func DefaultTurnEngine() TurnEngine {
	return TurnEngine{
		MaxIterations:        3,
		Delegation:           DefaultDelegation(),
		DelegationDepthLimit: 3,
		// 24 = the 16 the planner had plus the 20 the actor had, minus
		// the round each spent on its own submission and the re-reads the
		// actor made of what the planner had already fetched. One
		// conversation does the discovery once: measured against the
		// three-phase engine's own logs, a turn that took 16+20 spends
		// closer to 20 when the plan is not thrown away between them, and
		// 24 leaves headroom before the extension judge is consulted.
		MaxToolRounds:           24,
		OnboardingMaxToolRounds: 10,
		// 2x each phase's base, which is what an extension is for: a phase
		// that is genuinely progressing gets a second budget, and one that
		// is thrashing hits a wall an operator can see in the numbers.
		ExecuteMaxToolRoundsCeiling:    48,
		OnboardingMaxToolRoundsCeiling: 20,
		ExtensionRoundStep:             8,
		SandboxMinBudgetTokens:         2000,
		ConversationSession:            DefaultConversationSession(),
	}
}

// Extends reports whether the round-cap extension judge is on, applying
// the true default.
func (t *TurnEngine) Extends() bool { return t.ExtensionEnabled.Or(true) }

func (t *TurnEngine) validate(path string) error {
	var p problems

	positive := []struct {
		name  string
		value int
	}{
		{"max_iterations", t.MaxIterations},
		{"delegation_depth_limit", t.DelegationDepthLimit},
		{"max_tool_rounds", t.MaxToolRounds},
		{"extension_round_step", t.ExtensionRoundStep},
	}
	for _, f := range positive {
		if f.value < 1 {
			p.add(at(path, f.name), ErrOutOfRange,
				"must be at least 1, got %d — a cap of zero fails every turn "+
					"on its first guard check", f.value)
		}
	}

	nonNegative := []struct {
		name  string
		value int
	}{
		{"sandbox_min_budget_tokens", t.SandboxMinBudgetTokens},
		// 0 is meaningful here: it disables the dedicated onboarding pass.
		{"onboarding_max_tool_rounds", t.OnboardingMaxToolRounds},
	}
	for _, f := range nonNegative {
		if f.value < 0 {
			p.add(at(path, f.name), ErrOutOfRange, "must not be negative, got %d", f.value)
		}
	}

	p.wrap(t.Delegation.validate(at(path, "delegation")))

	// A ceiling below its own base is not a smaller budget, it is a
	// contradiction: the phase starts above the ceiling the judge may
	// grant up to, so the extension path can only ever refuse. Equal is
	// the documented way to disable extensions for one phase.
	ceilings := []struct {
		name, baseName string
		value, base    int
	}{
		{"execute_max_tool_rounds_ceiling", "max_tool_rounds", t.ExecuteMaxToolRoundsCeiling, t.MaxToolRounds},
		{"onboarding_max_tool_rounds_ceiling", "onboarding_max_tool_rounds", t.OnboardingMaxToolRoundsCeiling, t.OnboardingMaxToolRounds},
	}
	for _, c := range ceilings {
		if c.value < c.base {
			p.add(at(path, c.name), ErrOutOfRange,
				"must be at least %s (%d), got %d — set them equal to disable "+
					"extensions for this phase", c.baseName, c.base, c.value)
		}
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

func (c *ConversationSession) validate(path string) error {
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
