package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// PersistDecider is the post-turn classifier that decides what, if anything,
// a completed turn leaves behind in the seat's diary.
//
// It is the DETERMINISTIC half of the learning stack: it runs whether or not
// the turn's own model remembered to call reflect_and_persist, so a durable
// fact does not fall on the floor because an agent forgot. What to keep is
// still a model's judgement — compressing a turn into a fact that is useful
// next week needs one — so the pass is a single tool-less completion on the
// seat's auxiliary (cheap) model, and its answer is one of four tiers:
//
//   - NOOP: nothing durable. The common case, and the one the prompt tells
//     the model to pick when unsure.
//   - DOC: a STANDING RULE the team should follow. No diary write at all —
//     the tier exists so the system can say "that is not memory, it is
//     policy". The lifecycle event counts it, and the log line carrying the
//     rule ([Directive]) is what an operator routes into the team's
//     documentation.
//   - LONG: a personal fact worth remembering indefinitely.
//   - SHORT: a personal fact with a known expiry.
//
// Both writing tiers land at AGENT scope. Unit and org scope are not
// categories this produces: a fact that belongs to a unit belongs in that
// unit's documentation, which is what DOC is for.
type PersistDecider struct {
	models Models
	diary  DiaryStore

	timeout time.Duration
	now     func() time.Time
	newID   func() string
}

// Models resolves the model one seat's auxiliary work runs on: the engine's
// ONE auxiliary seam, which resolves the seat's auxiliary chain head off the
// phase registry — so "which model does reflection use" is still decided in one
// place, the one the config validator checks — and records and charges every
// completion made through what it hands back.
//
// EVERY CALL STATES ITS ATTRIBUTION — a stage, a purpose, the turn it learns
// from ([Turn.Reflecting] for a reflection worker) — because the seam files
// the spend under it, and an ambient one would charge a worker's call to
// whichever turn last wrote it.
type Models interface {
	Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error)
}

// DiaryStore is the seat's diary, as much of it as the decider touches.
//
// Read as well as write: the read is the DEDUP signal. Without the facts the
// seat already knows in front of it, the classifier re-derives the same
// preference from every turn that counterparty appears in, and a diary fills
// with paraphrases of one fact that then crowd the real ones out of the
// turn-start digest.
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
	// DefaultAuxTimeout bounds one auxiliary call.
	//
	// A dispatcher pass runs inside the completed-turn consumer, so a
	// provider that hangs stops the company's reflection entirely rather
	// than just this turn's. 60s is sized for a small fast model behind its
	// harness: a cold launch costs seconds (`crewlet llm doctor` allows ten for
	// a Node runtime's), which leaves the model fifty for an answer that is
	// one JSON object. A call cut off here is reported as "nothing to
	// persist", which is what [PersistOptions.CallTimeout] is for when the
	// seat's auxiliary model is slower than that.
	DefaultAuxTimeout = 60 * time.Second
)

// PersistOptions configure a decider. The zero value is the shipped one.
type PersistOptions struct {
	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

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
		timeout: opts.CallTimeout, now: opts.Now, newID: opts.NewID,
	}
	if p.timeout <= 0 {
		p.timeout = DefaultAuxTimeout
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
//
// It goes nowhere but the `persist_decider_doc_observed` log line: nothing
// hands it to anyone, so it carries what an operator routing the rule into
// the team's documentation reads there, and nothing else — a field the model
// is asked for and nobody reads is output tokens on every DOC for nothing.
type Directive struct {
	// Content is the rule itself, in the model's words.
	Content string

	// TargetHint is where the model thinks it belongs, "" when it has no
	// sense of that.
	TargetHint string
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
		WorkKey:        t.WorkKey(),
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
	member, err := d.models.Auxiliary(t.Role, t.Reflecting(types.AuxPersistDecider))
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
	// ONE TOKEN FOR THE CALL, minted for it and never a turn's: the turn's
	// messages are somebody else's words, fenced under it, and what this
	// call answers is written into the seat's memory.
	fence := provenance.NewBoundary()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: PersistSystemPrompt},
			{Role: llm.RoleUser, Content: buildPersistPrompt(fence, t, existing)},
		},
		// No tools, deliberately. The contract is a JSON object in the
		// content; a tool on the surface invites a model to call it and
		// answer nothing, and there is no tool this pass could use.
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
		// WHOLE: the contract is one JSON object, and an opening cut away
		// from the part that broke it is a diagnosis that names nothing.
		log.WarnContext(ctx, "persist_decider_unparseable",
			"turn_id", t.Event.TurnID, "response", text)
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
		}
		log.InfoContext(ctx, "persist_decider_doc_observed", "turn_id", t.Event.TurnID,
			"agent_handle", t.Event.AgentHandle, "target_hint", dir.TargetHint,
			"content", dir.Content)
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

	// An unknown tier is a NOOP, not a guess. A model whose `kind` is
	// missing or not one this prompt offers — a {"scope": "org"} that names
	// a scope where a tier goes, say — must not have it read as anything
	// writable.
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
//
// IT STATES WHAT THE PASS NEEDS AND NOTHING THE CODE ALREADY DOES. The SHORT
// tier's default and cap are [coerceTTLDays]'s, so the model is asked for a
// number and the band is enforced after it; a turn that wrote its own memory
// never reaches this prompt ([PersistDecider.Skip]). One example per tier,
// because where DOC ends and LONG begins is the call an example settles, and
// a second one settles nothing the first did not. The attribution example is
// written as [Subject.Label] renders a person — the form of the user
// message's Requester lines and of the memory filter's "Current sender:"
// line, which is what a stored fact is later matched against.
//
// IT ENDS ON THE STANDING SENTENCE ON FENCES ([provenance.StandingInstruction]),
// the one the seat's own prompt carries: what the turn was asked and what was
// said to it reach this call fenced under a token minted for the call
// (ADR-0040), and the sentence is what says a fence is somebody's words.
const PersistSystemPrompt = `You are a post-turn reflection classifier. Read the turn summary and decide
which tier its content falls into:

  NOOP  -- Nothing durable. The common case: pick it when in doubt.

  DOC   -- A STANDING RULE the team or org should follow, which belongs in
           the team's documentation rather than in personal memory
           (e.g. "review every backend PR before merge"). Nothing is
           stored for it.

  LONG  -- A personal fact the agent should remember indefinitely: a
           stakeholder's preference, a convention it met, a durable
           observation (e.g. "The auth service reads a JWT from the
           Authorization header").

  SHORT -- A personal fact with a known expiry: operational context that
           ages out (e.g. "Q2 launch freeze active until end of June").
           Give "ttl_days", how many days it stays useful.

Rules for LONG and SHORT:

1. Declarative facts, not instructions: "Stakeholder X prefers weekly
   digests", not "Always send weekly digests".
2. Attribute the fact to the requester or subject the turn names, as it
   names them: "Sam (slack:U0TESTUSER1) prefers terse replies", not "User
   prefers terse replies". A fact without its "who" loses its meaning.
3. Never persist what will not apply next time: a single ticket ID, one-off
   debug output, ephemeral state.
4. The user prompt may list what is "Already in your memory". Answer NOOP
   when the fact restates an entry there, is a thinner version of one, or
   is an instance a broader entry already covers.

Answer with one JSON object and nothing else:

  {"kind": "NOOP"}
  {"kind": "DOC", "content": "<the rule itself>", "target_hint": "<where it belongs, or empty>"}
  {"kind": "LONG", "content": "<declarative fact>"}
  {"kind": "SHORT", "content": "<declarative fact>", "ttl_days": <int>}
` + "\n" + provenance.StandingInstruction

// buildPersistPrompt renders the turn and the seat's existing memory, every
// word somebody else wrote inside a fence of b and everything else neutralised
// under it (ADR-0040): the classifier's answer is a fact the seat will read
// as its own memory on every later turn, so a message that tells the
// classifier what to remember must read as that sender's words.
func buildPersistPrompt(b provenance.Boundary, t Turn, existing []DiaryEntry) string {
	tools := "(none)"
	if len(t.Event.ToolSequence) > 0 {
		tools = strings.Join(t.Event.ToolSequence, ", ")
	}
	var out strings.Builder
	out.WriteString("Turn summary:\n")
	// ONLY THE ASK NO INTERACTION CARRIES: each interaction is rendered
	// below with its sender, which is what attributing a fact needs.
	describeTurn(&out, b, t, t.Event.Ask, t.Event.AskProvenance)
	out.WriteString("\n- Tools called: ")
	out.WriteString(tools)
	out.WriteString("\n- Outcome: ")
	out.WriteString(provenance.Neutralise(b, t.Event.ReviewOutcome))

	// One requester/message pair per interaction that has an identifiable
	// sender — NOT merged by sender. A coalesced trigger is several
	// messages, and the classifier's job is to attribute a fact to whoever
	// stated it: collapsing four messages into one requester line loses
	// which of them said the thing being remembered.
	for _, in := range t.Event.Interactions {
		who := SubjectOf(in.Sender).Label()
		if who == "" {
			continue
		}
		// A DISPLAY NAME IS THE SENDER'S CHOICE, so it opens no fence.
		out.WriteString("\n- Requester: ")
		out.WriteString(provenance.Neutralise(b, who))
		if body := inboundBody(b, in); body != "" {
			out.WriteString("\n- Inbound message:\n")
			out.WriteString(body)
		}
	}
	out.WriteString(renderExistingMemories(b, existing))
	out.WriteString("\nClassify and emit the JSON object.")
	return out.String()
}

// renderExistingMemories renders the dedup block, "" when there is nothing to
// render — a fresh seat and a seat whose diary could not be read produce the
// same compact prompt rather than a header over an empty list.
//
// Each memory is what an earlier classification wrote, and is neutralised
// under b: it opens and closes no fence of the call.
func renderExistingMemories(b provenance.Boundary, memories []DiaryEntry) string {
	var out strings.Builder
	n := 0
	for _, m := range memories {
		content := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
		if content == "" {
			continue
		}
		if n == 0 {
			out.WriteString("\n\nAlready in your memory:")
		}
		out.WriteString("\n")
		out.WriteString(strconv.Itoa(n))
		out.WriteString(". ")
		out.WriteString(provenance.Neutralise(b, content))
		// A deadline is rendered so the model can tell "the seat knows
		// this and the row is still live" from "the seat knew this until
		// last week" — the second is not a duplicate, it is a fact worth
		// restating.
		if !m.TTLUntil.IsZero() {
			out.WriteString(" (until ")
			out.WriteString(m.TTLUntil.UTC().Format(time.RFC3339))
			out.WriteString(")")
		}
		n++
	}
	if n == 0 {
		return ""
	}
	out.WriteString("\n")
	return out.String()
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

// extractJSONObject reads a model's object out of a response.
//
// Down [modelJSONCandidates], which is the one recovery ladder this package
// has: the prompt forbids prose around the JSON and models emit it anyway — a
// leading "Here's the classification:" or a code fence is not a reason to lose
// a correctly classified fact.
func extractJSONObject(text string) (map[string]any, bool) {
	for _, candidate := range modelJSONCandidates(text) {
		if obj, ok := decodeObject(candidate); ok {
			return obj, true
		}
	}
	return nil, false
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
