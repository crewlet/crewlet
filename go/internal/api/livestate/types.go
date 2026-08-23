package livestate

// The wire shapes. Every json tag here is part of the frozen dashboard
// protocol (rewrite/decisions/502-dashboard-wire-protocol.md): the client ships
// unchanged and is the compatibility reference, so a renamed field is a broken
// dashboard, not a refactor.

// Envelope is one serialized event as the dashboard sees it.
//
// Payload stays untyped on purpose. This projection is fed engine events AND
// webhook events through one path — that is what keeps them on the same code
// path rather than in two state machines — and a webhook payload has no Go type
// to decode into. The fields actually read are few and are pulled through the
// accessors below, so the untyped map never spreads past this package.
type Envelope struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Timestamp    string         `json:"timestamp"`
	Source       string         `json:"source"`
	Actor        string         `json:"actor"`
	Summary      string         `json:"summary"`
	Category     string         `json:"category"`
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	Topic        string         `json:"topic"`
	Payload      map[string]any `json:"payload,omitempty"`
}

// FeedRow is one payload-free row of the activity feed.
//
// The shape is named once because the row is built in two places — live from
// the stream and hydrated from the store — and two hand-maintained copies of a
// shape is how a field ends up present on a running dashboard and missing after
// a reload.
type FeedRow struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Timestamp    string `json:"timestamp"`
	Source       string `json:"source"`
	Actor        string `json:"actor"`
	Summary      string `json:"summary"`
	Category     string `json:"category"`
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`
	Topic        string `json:"topic"`
	Failed       bool   `json:"failed"`
}

// ErrorInfo is why a seat stopped.
//
// One shape for every stop — a failed phase, a failed task, an exhausted
// budget, a dead provider, a guard breach — so one panel explains all of them
// rather than each kind of failure needing the reader to know where to look.
type ErrorInfo struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Phase   string `json:"phase"`
	TurnID  string `json:"turn_id"`
	At      string `json:"at"`
	EventID string `json:"event_id"`
}

// LiveCall is the in-flight LLM call: the latest progress round, or the
// placeholder a phase start seeds.
//
// It is held here because agent_turn_progress is STREAM-ONLY — the event-store
// writer drops it — so the durable record of a turn only appears once its phase
// completes. A dashboard that rebuilt from the store on every reconnect would
// lose any call mid-flight the moment someone hit refresh. Holding it here and
// shipping it in the snapshot is what makes the live row survive that.
type LiveCall struct {
	TurnID    string `json:"turn_id"`
	Phase     string `json:"phase"`
	Iteration int    `json:"iteration"`
	Model     string `json:"model"`

	// Trigger is the event that woke this turn, carried so a refresh
	// mid-call still shows the live row's source.
	Trigger map[string]any `json:"trigger"`

	Prompt         string `json:"prompt"`
	PromptMessages []any  `json:"prompt_messages"`
	Response       string `json:"response"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	ToolExecutions []any `json:"tool_executions"`

	RoundNum   int  `json:"round_num"`
	Rounds     int  `json:"rounds"`
	InProgress bool `json:"in_progress"`

	// Failed and Error appear only on a frozen call. A phase that dies
	// mid-call is exactly when an operator most wants to see the call, so
	// it is stamped and kept rather than cleared.
	Failed bool       `json:"failed,omitempty"`
	Error  *ErrorInfo `json:"error,omitempty"`

	UpdatedAt string `json:"updated_at"`
}

// Meter is a seat's or the org's live token budget.
//
// A PROCESS-LIFETIME meter, never to be compared against the 24-hour spend
// rollup or the 7-day per-agent total that sit beside it on the same screen.
type Meter struct {
	Used      int    `json:"used"`
	Max       int    `json:"max"`
	RefusedAt string `json:"refused_at"`
}

// Overlay is the live half of an agent row, merged onto its static config row.
type Overlay struct {
	State            string    `json:"state"`
	RuntimeID        string    `json:"runtime_id"`
	CurrentTask      *string   `json:"current_task"`
	CurrentPhase     *string   `json:"current_phase"`
	CurrentIteration int       `json:"current_iteration"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	LiveCall         *LiveCall `json:"live_call"`

	// LastError is nil once the seat does real work again. It says WHY a
	// seat stopped, rather than leaving a call on screen that never
	// answers.
	LastError *ErrorInfo `json:"last_error"`

	// Budget is nil when there is no meter, which covers two situations
	// that look the same from here: the seat has no per-agent budget, or
	// no engine is reporting at all. Either way a bar drawn without one
	// would be a claim nobody measured.
	Budget *Meter `json:"budget"`

	// AFKReason is ALWAYS present, even when empty. The overlay is merged
	// into a client's row rather than replacing it, so an omitted key
	// reads as "unchanged" — which would leave a recovered agent wearing
	// the reason it was AFK for.
	AFKReason string `json:"afk_reason"`
}

// SandboxEntry is one in-flight detached coding run.
type SandboxEntry struct {
	TurnID      string `json:"turn_id"`
	Role        string `json:"role"`
	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	CodingAgent string `json:"coding_agent"`
	SandboxID   string `json:"sandbox_id"`
	Task        string `json:"task"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`

	// Question and Audience appear once a run pauses on a clarification.
	Question string `json:"question,omitempty"`
	Audience string `json:"audience,omitempty"`
}

// OrgBudget is the org-wide half of the live meter, plus the identity and
// sequence of the engine run reporting it.
type OrgBudget struct {
	MeterID string `json:"meter_id"`
	Seq     int    `json:"seq"`
	Org     Meter  `json:"org"`
}

// Change is what one applied event moved.
//
// The stream service turns this into the push envelopes a dashboard consumes:
// the changed agents' overlays, the sandbox set, the spend rollup. Anything not
// named here did not move and is not re-sent — which is the whole point, since
// a dashboard should mirror this projection rather than re-implement it.
type Change struct {
	Agents    map[string]struct{}
	Sandboxes bool
	Tokens    bool
	Events    bool
	Budget    bool
}

// Moved reports whether anything changed at all.
func (c Change) Moved() bool {
	return len(c.Agents) > 0 || c.Sandboxes || c.Tokens || c.Events || c.Budget
}

func (c *Change) agentMoved(role string) {
	if c.Agents == nil {
		c.Agents = map[string]struct{}{}
	}
	c.Agents[role] = struct{}{}
}

// --- payload accessors ------------------------------------------------- //
//
// Small, total, and in one place. A projection that reached into the map
// inline would grow a different coercion at every call site, and the payloads
// it reads come off a wire where a number may arrive as a float, a string or
// not at all.

// str returns the first non-empty string among keys.
//
// FIRST NON-EMPTY, not first present: several payloads name the same thing two
// ways (role / agent_role, model / provider_key) and the fallback only helps if
// an empty value falls through to it.
func str(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// num reads an integer field.
//
// TWO cases, and only two. float64 is what a JSON round trip produces — every
// payload that crossed a broker arrives that way, since JSON has one number
// type — and int is what an in-process caller building the map directly gives.
// Nothing on this path decodes with UseNumber, so json.Number is not a shape
// these payloads can arrive in and a case for it would be a branch no envelope
// can reach.
func num(payload map[string]any, key string) int {
	switch v := payload[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func flag(payload map[string]any, key string) bool {
	b, _ := payload[key].(bool)
	return b
}

func mapping(payload map[string]any, key string) map[string]any {
	m, _ := payload[key].(map[string]any)
	return m
}

func list(payload map[string]any, key string) []any {
	l, _ := payload[key].([]any)
	return l
}
