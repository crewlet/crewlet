package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// PersistDecider is the post-turn classifier that decides what, if anything,
// a completed turn leaves behind in the seat's diary.
//
// It is the DETERMINISTIC half of the learning stack: it runs whether or not
// the turn's own model remembered to call reflect_and_persist, so a durable
// fact does not fall on the floor because a planner forgot. What to keep is
// still a model's judgement — compressing a turn into a fact that is useful
// next week needs one — so the pass is a single tool-less completion on the
// seat's auxiliary (cheap) model, and its answer is one of four tiers:
//
//   - NOOP: nothing durable. The common case, and the one the prompt tells
//     the model to pick when unsure.
//   - DOC: a STANDING RULE the team should follow. No diary write at all —
//     the tier exists so the system can say "that is not memory, it is
//     policy", and the lifecycle event is the audit trail an operator routes
//     into the team's documentation.
//   - LONG: a personal fact worth remembering indefinitely.
//   - SHORT: a personal fact with a known expiry.
//
// Both writing tiers land at AGENT scope. Unit and org scope are not
// categories this produces: a fact that belongs to a unit belongs in that
// unit's documentation, which is what DOC is for.
type PersistDecider struct {
	models Models
	diary  DiaryStore

	timeout   time.Duration
	maxTokens int
	now       func() time.Time
	newID     func() string
}

// Models resolves the model one seat's auxiliary work runs on.
//
// One method, and deliberately the phase registry's own signature so
// *phase.Registry satisfies it as written: an adapter in the engine's wiring
// would be a second place for "which model does reflection use" to be
// decided, and the answer has to be the same one the config validator checks.
type Models interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

// DiaryStore is the seat's diary, as much of it as the decider touches.
//
// Read as well as write: the read is the DEDUP signal. Without the facts the
// seat already knows in front of it, the classifier re-derives the same
// preference from every turn that counterparty appears in, and a diary fills
// with paraphrases of one fact that then crowd the real ones out of the
// Plan-phase digest.
type DiaryStore interface {
	Write(ctx context.Context, e DiaryEntry) error
	Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]DiaryEntry, error)
}

// PersistSource is stamped on every row the decider writes, so a row written
// post-turn is distinguishable from one the turn's own model wrote through
// reflect_and_persist. They are the same tiers reached two different ways,
// and only the provenance tells an operator which path is producing the
// memory a seat is acting on.
const PersistSource = "persist_decider"

const (
	// shortTTLDefaultDays is what a SHORT tier gets when the model
	// proposes no usable duration. A month outlives the operational
	// context the tier is for — a sprint, an incident, someone's leave —
	// without pinning a fact into the prompt for a quarter.
	shortTTLDefaultDays = 30

	// shortTTLMaxDays caps what the model may propose. Beyond half a year
	// a fact is not "short-lived with a known expiry", it is a LONG the
	// model mislabelled, and an uncapped proposal turns one
	// misclassification into a row that sits in the prompt effectively
	// for ever.
	shortTTLMaxDays = 180

	// dedupPoolLimit bounds the memories rendered into the prompt for
	// dedup. The signal saturates well before this: what the classifier
	// needs is to recognise the candidate fact, not to see the whole
	// pool, and an agent whose diary has grown to hundreds of rows would
	// otherwise pay for all of them on every completed turn.
	dedupPoolLimit = 50
)

const (
	// DefaultAuxTokens caps the classifier's output.
	//
	// Generous for an answer that is one small JSON object because the
	// cap covers THINKING as well: on an extended-thinking model a tight
	// cap is spent reasoning, the call returns with output_tokens at the
	// cap and content empty, and every turn then degrades to NOOP with no
	// error anywhere to say why.
	DefaultAuxTokens = 5000

	// DefaultAuxTimeout bounds one auxiliary call.
	//
	// A dispatcher pass runs inside the completed-turn consumer, so a
	// provider that hangs stops the company's reflection entirely rather
	// than just this turn's. 60s is sized for an HTTP round trip to a
	// small fast model; a backend whose own per-call budget is larger —
	// a locally launched coding CLI, where process start alone costs
	// seconds — needs [PersistOptions.CallTimeout] raised to match, or
	// every one of its calls is cut off here and silently reported as
	// "nothing to persist".
	DefaultAuxTimeout = 60 * time.Second

	// auxTemperature keeps the classification reproducible. Not zero: the
	// tier is a judgement, and greedy decoding on a judgement makes the
	// model commit hard to a first token it would otherwise reconsider.
	// Every learning classifier runs at this value.
	auxTemperature = 0.2
)

// PersistOptions configure a decider. The zero value is the shipped one.
type PersistOptions struct {
	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps the classifier's output; zero takes DefaultAuxTokens.
	MaxTokens int

	// Now and NewID are injectable so the suite can pin the clock and the
	// row id. Nil takes the real ones, so the zero-value path is the one
	// that ships.
	Now   func() time.Time
	NewID func() string
}

// NewPersistDecider builds a decider over a model registry and a diary.
func NewPersistDecider(models Models, d DiaryStore, opts PersistOptions) (*PersistDecider, error) {
	if models == nil {
		return nil, fmt.Errorf("learning: the persist decider needs a model registry")
	}
	if d == nil {
		// Refused rather than tolerated: a decider with no diary can
		// still classify, and would then spend an auxiliary call per
		// completed turn to produce a tier it cannot act on.
		return nil, fmt.Errorf("learning: the persist decider needs a diary to write to")
	}
	p := &PersistDecider{
		models: models, diary: d,
		timeout: opts.CallTimeout, maxTokens: opts.MaxTokens,
		now: opts.Now, newID: opts.NewID,
	}
	if p.timeout <= 0 {
		p.timeout = DefaultAuxTimeout
	}
	if p.maxTokens <= 0 {
		p.maxTokens = DefaultAuxTokens
	}
	if p.now == nil {
		p.now = func() time.Time { return time.Now().UTC() }
	}
	if p.newID == nil {
		p.newID = uuid.NewString
	}
	return p, nil
}

// Directive is a standing rule the classifier saw and deliberately did not
// memorise.
type Directive struct {
	// Content is the rule itself, written so a hand-off can quote it
	// verbatim rather than paraphrasing what the agent saw.
	Content string

	// TargetHint is where the model thinks it belongs, "" when it has no
	// sense of that.
	TargetHint string

	// Rationale is one sentence on why it is a rule and not a memory.
	Rationale string
}

// Decision is one classification and whatever it produced.
type Decision struct {
	// Tier is the classification. It is reported even when nothing was
	// written and even when the write failed: the per-agent distribution
	// of tiers is the headline signal for whether learning is working at
	// all, and collapsing every non-write into NOOP would erase the
	// difference between "the model found nothing" and "the model found
	// something the store then dropped".
	Tier types.PersistClassification

	// Entry is the row that landed, zero when none did.
	Entry DiaryEntry

	// Directive is set on the DOC tier and empty otherwise.
	Directive Directive
}

// Persisted reports whether a row actually landed.
func (d Decision) Persisted() bool { return d.Entry.ID != "" }

// Name identifies the worker.
func (d *PersistDecider) Name() string { return PersistSource }

// Skip reports why this turn is not one to classify.
func (d *PersistDecider) Skip(t Turn) string {
	if !t.Settled() {
		return "non_terminal"
	}
	if t.SelfPersisted() {
		// The turn's own model already wrote what it wanted to keep.
		// Classifying again would spend a second call to reach a fact the
		// diary already holds, and the dedup block only suppresses the
		// duplicate if the classifier recognises its own paraphrase.
		return "self_persisted"
	}
	return ""
}

// Reflect runs one classification and reports it as a lifecycle event.
func (d *PersistDecider) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	dec, err := d.Decide(ctx, t)
	ev := types.PersistDeciderCompleted{
		Agent:          t.Event.Agent,
		AgentHandle:    t.Event.AgentHandle,
		RoleName:       t.Event.RoleName,
		TurnID:         t.Event.TurnID,
		Persisted:      dec.Persisted(),
		DocID:          dec.Entry.ID,
		Classification: dec.Tier,
		ReviewOutcome:  t.Event.ReviewOutcome,
	}
	if dec.Persisted() {
		// Scope is stamped from the WRITE, not from the tier: it says
		// where a row that exists can be read back from, so a tier that
		// wrote nothing must leave it empty rather than name a scope
		// holding nothing.
		ev.Scope = types.MemoryScopeAgent
		if !dec.Entry.TTLUntil.IsZero() {
			ev.TTLUntil = dec.Entry.TTLUntil.UTC().Format(time.RFC3339)
		}
	}
	return []events.Payload{ev}, err
}

// Decide runs one classification pass, writing the row when the tier calls
// for one.
//
// It ALWAYS returns a Decision. An error accompanies it when something went
// wrong producing it — an unreachable model, a refused write — and the tier
// still reports what was concluded before the failure, so a caller can tell a
// LONG that failed to land from a turn with nothing in it.
func (d *PersistDecider) Decide(ctx context.Context, t Turn) (Decision, error) {
	member, err := d.models.Head(t.Role, phase.Auxiliary)
	if err != nil {
		return Decision{Tier: types.PersistNOOP}, fmt.Errorf("learning: no auxiliary model: %w", err)
	}

	now := d.now()
	// Failure here degrades to a prompt with no dedup block rather than
	// skipping the turn: running without the block is the behaviour that
	// shipped before dedup existed, and it writes a possible duplicate
	// where the alternative silently drops every fact for as long as the
	// store is unhappy.
	existing, err := d.diary.Recent(ctx, t.Event.Agent, now, dedupPoolLimit)
	if err != nil {
		log.WarnContext(ctx, "persist_decider_dedup_unavailable",
			"turn_id", t.Event.TurnID, "agent_id", t.Event.Agent, "error", err)
		existing = nil
	}

	call, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: PersistSystemPrompt},
			{Role: llm.RoleUser, Content: buildPersistPrompt(t, existing)},
		},
		// No tools, deliberately. The contract is a JSON object in the
		// content; a tool on the surface invites a model to call it and
		// answer nothing, and there is no tool this pass could use.
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   d.maxTokens,
	})
	if err != nil {
		return Decision{Tier: types.PersistNOOP},
			fmt.Errorf("learning: persist decider on %s: %w", member.Key, err)
	}
	var text string
	if completion != nil {
		text = strings.TrimSpace(completion.Content)
	}
	if text == "" {
		log.DebugContext(ctx, "persist_decider_empty_response", "turn_id", t.Event.TurnID)
		return Decision{Tier: types.PersistNOOP}, nil
	}
	// Some models answer the bare sentinel instead of the JSON contract.
	// Accepted because the answer is unambiguous and the alternative is
	// logging a parse warning on the single most common outcome.
	if strings.HasPrefix(strings.ToUpper(text), string(types.PersistNOOP)) {
		return Decision{Tier: types.PersistNOOP}, nil
	}

	parsed, ok := extractJSONObject(text)
	if !ok {
		// The preview is the only diagnosis available for a model that
		// has stopped honouring the contract — a bare tier count would
		// say classification collapsed to NOOP without saying why.
		// Capped because the response can carry the turn's own content.
		log.WarnContext(ctx, "persist_decider_unparseable",
			"turn_id", t.Event.TurnID, "response", preview(text, 200))
		return Decision{Tier: types.PersistNOOP}, nil
	}

	tier := types.PersistClassification(strings.ToUpper(strings.TrimSpace(stringField(parsed, "kind"))))
	if tier == types.PersistNOOP {
		return Decision{Tier: types.PersistNOOP}, nil
	}
	content := strings.TrimSpace(stringField(parsed, "content"))
	if content == "" {
		// A tier with nothing to store is not that tier. Checked before
		// the dispatch below so an empty DOC does not fire a directive
		// naming no rule, and an empty LONG does not reach a write the
		// store would refuse.
		log.DebugContext(ctx, "persist_decider_empty_content", "turn_id", t.Event.TurnID, "kind", string(tier))
		return Decision{Tier: types.PersistNOOP}, nil
	}

	switch tier {
	case types.PersistDoc:
		dir := Directive{
			Content:    content,
			TargetHint: strings.TrimSpace(stringField(parsed, "target_hint")),
			Rationale:  strings.TrimSpace(stringField(parsed, "rationale")),
		}
		log.InfoContext(ctx, "persist_decider_doc_observed", "turn_id", t.Event.TurnID,
			"agent_handle", t.Event.AgentHandle, "target_hint", dir.TargetHint,
			"content", preview(dir.Content, 120))
		return Decision{Tier: types.PersistDoc, Directive: dir}, nil

	case types.PersistLong:
		entry, err := d.write(ctx, t, content, DiaryLong, time.Time{}, now)
		return Decision{Tier: types.PersistLong, Entry: entry}, err

	case types.PersistShort:
		// A SHORT is never written as a LONG when the model proposes no
		// expiry: the tier the model chose is the tier that is stored,
		// and the missing half is filled with the default band. Storing
		// it as a LONG would silently promote operational context —
		// "Sarah is OOO until Friday" — into a fact the seat repeats for
		// ever, and the diary refuses a short row with no deadline
		// precisely so that mistake cannot be made quietly.
		ttl := now.Add(time.Duration(coerceTTLDays(parsed["ttl_days"])) * 24 * time.Hour)
		entry, err := d.write(ctx, t, content, DiaryShort, ttl, now)
		return Decision{Tier: types.PersistShort, Entry: entry}, err
	}

	// An unknown tier is a NOOP, not a guess. A model answering
	// {"scope": "org"} — the shape an older three-scope prompt asked for
	// — must not have "org" read as anything writable.
	log.WarnContext(ctx, "persist_decider_unknown_tier", "turn_id", t.Event.TurnID, "kind", string(tier))
	return Decision{Tier: types.PersistNOOP}, nil
}

// write records one row and returns what landed, zero on failure.
func (d *PersistDecider) write(
	ctx context.Context, t Turn, content string, kind DiaryKind, ttl, now time.Time,
) (DiaryEntry, error) {
	entry := DiaryEntry{
		ID:       d.newID(),
		AgentID:  t.Event.Agent,
		Kind:     kind,
		Content:  content,
		TTLUntil: ttl,
		Source:   PersistSource,
		TurnID:   t.Event.TurnID,
		// The columns carry kind, ttl, source and turn id; what stays in
		// the metadata blob is the rest of the provenance a reader of the
		// row needs to place it — which outcome produced it, and which
		// handle the derived agent id belonged to at the time.
		Metadata: map[string]any{
			"review_outcome": t.Event.ReviewOutcome,
			"agent_handle":   t.Event.AgentHandle,
		},
		CreatedAt: now,
	}
	if len(content) > MaxContentChars {
		// SKIPPED, and said out loud. The tool path refuses an over-long
		// note so the model can tighten it; there is nobody to ask here, so
		// the honest move is to drop the row rather than store a note whose
		// tail the seat will never read back — and to log it, because a
		// classifier that keeps producing documents is a prompt to fix.
		log.WarnContext(ctx, "persist_decider_note_oversized",
			"turn_id", t.Event.TurnID, "agent_handle", t.Event.AgentHandle,
			"chars", len(content), "max", MaxContentChars,
			"detail", "the note was dropped rather than stored half-written")
		return DiaryEntry{}, nil
	}
	if err := d.diary.Write(ctx, entry); err != nil {
		return DiaryEntry{}, fmt.Errorf("learning: persist %s for %s: %w",
			kind, t.Event.AgentHandle, err)
	}
	// The deadline reads as "" rather than as year one on a LONG row: a
	// timestamp in the log line is a fact about the row, and 0001-01-01 is
	// an absent field wearing a value.
	deadline := ""
	if !ttl.IsZero() {
		deadline = ttl.UTC().Format(time.RFC3339)
	}
	log.InfoContext(ctx, "persist_decider_stored", "turn_id", t.Event.TurnID, "doc_id", entry.ID,
		"kind", string(kind), "agent_handle", t.Event.AgentHandle, "ttl_until", deadline)
	return entry, nil
}

// PersistSystemPrompt is the classifier's contract.
//
// A const, so it is byte-stable across calls — provider prompt caching keys
// on the prefix, and a prompt assembled per call would miss the cache on
// every completed turn in the company.
const PersistSystemPrompt = `You are a post-turn reflection classifier.

Read the turn summary and decide which of four categories the
turn's content falls into:

  NOOP  -- Nothing durable.  This is the common case.  Use it when
           in doubt.

  DOC   -- The turn surfaced a STANDING RULE that the team or org
           should follow (e.g. "always commit in semantic style",
           "review every backend PR before merge").  Things that
           apply beyond just the agent's personal interactions and
           belong in the team's documentation, not in personal
           memory.  Output DOC; we do NOT memorize these -- the
           agent will be nudged to hand off to a team lead who has
           authority to update the docs.

  LONG  -- A personal fact the agent should remember indefinitely.
           Stakeholder preferences, project conventions the agent
           personally encountered, durable observations:
             - "Stakeholder Sam prefers terse replies"
             - "The auth service uses JWT in the Authorization header"
             - "Sarah Chen reviews backend PRs in the morning"
           Stored at agent scope without an expiry.

  SHORT -- A personal fact with a known expiry: operational context
           that ages out:
             - "Sarah is OOO until 2026-05-08"
             - "Q2 launch freeze active until end of June"
             - "Production rollback in progress; hold non-critical
                deploys"
           When you choose SHORT, propose "ttl_days" -- how many
           days the fact stays useful.  Default 30 if you can't tell.
           Cap 180.

Writing-style rules (for LONG and SHORT):

1. **Declarative facts, not instructions.**
   - "Stakeholder X prefers weekly digests" (yes)
   - "Always send weekly digests" (no)
   - "Deploy script fails when CI is green but Slack webhook is
     misconfigured" (yes)
   - "Check the Slack webhook first" (no)

2. **Always attribute the fact to the named requester / subject when
   one is shown in the turn context.**  Crewlet is multi-party --
   a fact stored without "who" loses its meaning.
   - "User Sam (slack:U0TESTUSER1) prefers replies opened with
     'hey sam'" (yes)
   - "User prefers replies opened with 'hey sam'" (no)

3. **Never persist task-specific details that won't apply next time**
   (a single ticket ID, one-off debug output, ephemeral state).

4. **Never persist anything the turn itself already wrote via a tool.**

5. **Deduplicate against existing memory.**  The user prompt may
   include an "Already in your memory" list of facts the agent
   already remembers.  Return "NOOP" when the candidate fact:
   - restates an existing entry (same fact, possibly paraphrased), or
   - is a thinner / less-specific version of an existing entry
     (e.g. "Sam is OOO this week" when memory already has
     "Sam is OOO Mon-Fri, route backend reviews to Maria"), or
   - is a more-specific instance already covered by a broader
     existing rule.
   Only emit a new "LONG" / "SHORT" when the candidate fact adds
   information the existing entries do not already express.  Dedup
   does not apply to "DOC" -- standing rules are independent of
   personal memory.

For DOC, write "content" as the standing rule itself (so the
manager-handoff hint can include it verbatim) and "target_hint"
if you have a sense of where it should land (e.g. "Engineering /
commit conventions"), else empty.

Output format (strict, JSON only -- no prose before or after):

  {"kind": "NOOP"}

  {"kind": "DOC", "content": "<rule>", "target_hint": "<hint or empty>",
   "rationale": "<one short sentence>"}

  {"kind": "LONG", "content": "<declarative fact>"}

  {"kind": "SHORT", "content": "<declarative fact>",
   "ttl_days": <int>}
`

// buildPersistPrompt renders the turn and the seat's existing memory.
func buildPersistPrompt(t Turn, existing []DiaryEntry) string {
	tools := "(none)"
	if len(t.Event.ToolSequence) > 0 {
		tools = strings.Join(t.Event.ToolSequence, ", ")
	}
	var b strings.Builder
	b.WriteString("Turn summary:\n- Task: ")
	b.WriteString(orElse(t.Event.TaskSummary, "(no description)"))
	b.WriteString("\n- Plan: ")
	b.WriteString(orElse(t.Event.PlanSummary, "(no plan)"))
	b.WriteString("\n- Tools called: ")
	b.WriteString(tools)
	b.WriteString("\n- Outcome: ")
	b.WriteString(t.Event.ReviewOutcome)

	// One requester/message pair per interaction that has an identifiable
	// sender — NOT merged by sender. A coalesced trigger is several
	// messages, and the classifier's job is to attribute a fact to whoever
	// stated it: collapsing four messages into one requester line loses
	// which of them said the thing being remembered.
	for _, in := range t.Event.Interactions {
		who := DescribeSender(in.Sender)
		if who == "" {
			continue
		}
		b.WriteString("\n- Requester: ")
		b.WriteString(who)
		if in.Body != "" {
			b.WriteString("\n- Inbound message: \"")
			b.WriteString(strings.ReplaceAll(strings.TrimSpace(in.Body), "\n", " "))
			b.WriteString("\"")
		}
	}
	b.WriteString(renderExistingMemories(existing))
	b.WriteString("\nClassify and emit the JSON object.")
	return b.String()
}

// renderExistingMemories renders the dedup block, "" when there is nothing to
// render — a fresh seat and a seat whose diary could not be read produce the
// same compact prompt rather than a header over an empty list.
func renderExistingMemories(memories []DiaryEntry) string {
	var b strings.Builder
	n := 0
	for _, m := range memories {
		content := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
		if content == "" {
			continue
		}
		if n == 0 {
			b.WriteString("\n\nAlready in your memory:")
		}
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(n))
		b.WriteString(". ")
		b.WriteString(content)
		// A deadline is rendered so the model can tell "the seat knows
		// this and the row is still live" from "the seat knew this until
		// last week" — the second is not a duplicate, it is a fact worth
		// restating.
		if !m.TTLUntil.IsZero() {
			b.WriteString(" (until ")
			b.WriteString(m.TTLUntil.UTC().Format(time.RFC3339))
			b.WriteString(")")
		}
		n++
	}
	if n == 0 {
		return ""
	}
	b.WriteString("\n")
	return b.String()
}

// DescribeSender renders a counterparty for an auxiliary prompt.
//
// THE one rendering, exported for the read-side memory filter to share: two
// prompts describing the same person differently desynchronise the facts one
// writes from the lines the other filters against, and the filter's job is to
// reject a memory naming somebody else.
func DescribeSender(id types.CanonicalIdentity) string {
	label := id.DisplayName
	if label == "" {
		label = id.Handle
	}
	switch {
	case label != "" && id.ExternalID != "" && id.Platform != "":
		return label + " (" + id.Platform + ":" + id.ExternalID + ")"
	case label != "" && id.ExternalID != "":
		return label + " (" + id.ExternalID + ")"
	case label != "":
		return label
	case id.ExternalID != "" && id.Platform != "":
		return id.Platform + ":" + id.ExternalID
	case id.ExternalID != "":
		return "unknown:" + id.ExternalID
	}
	return ""
}

// coerceTTLDays clamps the model's proposed duration into the supported band.
//
// A JSON number arrives as a float64 and a model that answers "45" arrives as
// a string; both are honoured, because rejecting the string form would demote
// a correctly-classified SHORT to the default month for a formatting choice.
// Anything else — null, a word, a negative — takes the default: the tier was
// chosen, only its duration was not.
func coerceTTLDays(v any) int {
	var days int
	switch n := v.(type) {
	case float64:
		days = int(n)
	case int:
		days = n
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return shortTTLDefaultDays
		}
		days = int(parsed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return shortTTLDefaultDays
		}
		days = parsed
	default:
		return shortTTLDefaultDays
	}
	if days < 1 {
		return shortTTLDefaultDays
	}
	return min(days, shortTTLMaxDays)
}

// extractJSONObject reads the classifier's object out of a response.
//
// The whole response first, then the span from the first { to the last }.
// The prompt forbids prose around the JSON and models emit it anyway — a
// leading "Here's the classification:" is not a reason to lose a correctly
// classified fact.
func extractJSONObject(text string) (map[string]any, bool) {
	if obj, ok := decodeObject(text); ok {
		return obj, true
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	return decodeObject(text[start : end+1])
}

func decodeObject(candidate string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
		return nil, false
	}
	// A JSON `null` unmarshals into a nil map WITHOUT error, so without
	// this it would report as a successfully-read object with no fields.
	//
	// The decision is the same either way — no kind, no content, NOOP — so
	// what this buys is the log line: a model that has started answering
	// `null` shows up at WARN with its response beside it, rather than at
	// DEBUG as "empty content", which is invisible at the default level and
	// sends an operator looking at the prompt instead of at the model.
	// Mutating the check away is CLEAN under test for exactly that reason:
	// the package logger is bound at init, so no test in this package can
	// observe which line fired.
	if obj == nil {
		return nil, false
	}
	return obj, true
}

// stringField reads a field the model may have answered with a non-string.
func stringField(obj map[string]any, key string) string {
	switch v := obj[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// preview truncates for a log line or a prompt, counting RUNES rather than
// bytes: a byte slice through a multi-byte character produces invalid UTF-8,
// which a JSON encoder then replaces or refuses.
func preview(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "..."
}
