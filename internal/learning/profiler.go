package learning

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// ProfilerSource names the worker in a pass result and in its logs.
const ProfilerSource = "counterparty_profiler"

// Profiler observes who a seat dealt with and what it learned about them.
//
// # It runs where the persist decider does not
//
// The decider is gated on a SETTLED turn, because a fact learned from work
// the agent is about to reattempt may be contradicted by the reattempt.
// Observing a counterparty is not that kind of fact: who spoke to this seat,
// and how they write, does not depend on whether the seat finished the job.
// So this runs on a self_iterate round too, and the interaction counter it
// moves is what makes "seen daily" and "seen once" distinguishable.
//
// # An empty patch is a real observation
//
// The model is asked what it learned that is not already on file, and the
// common answer is nothing. That still records the interaction: the store
// separates LastUpdatedAt (every observation) from LastCorroboratedAt (only
// a non-empty patch), and a counterparty seen constantly whose traits have
// stopped moving is a different fact from one not seen at all — which is
// exactly what the Plan-phase prefetch demotes on.
type Profiler struct {
	models       Models
	counterparts *Counterparties
	timeout      time.Duration
	maxTokens    int
	maxSubjects  int
	maxBodyChars int
	now          func() time.Time
}

// ProfilerOptions configures the worker.
type ProfilerOptions struct {
	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps one patch; zero takes DefaultProfilerTokens.
	MaxTokens int

	Now func() time.Time
}

const (
	// DefaultProfilerTokens caps one traits patch.
	//
	// FAR SMALLER than the classifier's, because the contract is a flat
	// object of short scalar traits — "prefers bullet points", "works
	// Sydney hours" — and a model that needs more than this is writing
	// prose into a field the prefetch renders as a one-line bullet.
	DefaultProfilerTokens = 700

	// maxProfiledSubjects bounds how many parties one turn profiles.
	//
	// Each is its own auxiliary call, so an unbounded list makes a
	// heavily coalesced trigger — a busy channel merged over a batching
	// window — cost a call per speaker while the seat's next turn waits
	// for its inbox. Eight covers every real conversation; past that the
	// turn was woken by a room, not by people, and the tail speakers
	// contributed a line each.
	maxProfiledSubjects = 8

	// maxProfiledBody caps how much of one message reaches the prompt.
	//
	// A trigger body can be a whole diff or a pasted log. What this pass
	// reads it for is HOW someone writes and what they ask for, which the
	// opening is entirely sufficient for.
	maxProfiledBody = 2000
)

// ProfilerSystemPrompt is the contract for one traits patch.
//
// It states the shape twice — once as a rule, once as an example — because
// the failure this pass has is not refusal but prose: a model that answers
// "Sam seems to prefer short replies" instead of {"reply_length":"short"}
// produces a patch that parses to nothing, silently, on every turn.
const ProfilerSystemPrompt = `You are maintaining a working profile of one person a colleague deals with.

Answer with a JSON object and nothing else: a flat map of trait name to a short
scalar value. Trait names are lowercase snake_case. Values are short strings,
numbers or booleans — never nested objects, never sentences.

Record only DURABLE working preferences you can support from the messages
shown: how they like to be replied to, what they consistently ask for, their
working hours, the surface they prefer, their role or team if they state it.

Do NOT record: what happened in this one exchange, your opinion of them,
anything about their mood, anything you inferred from a single word, or
anything already listed as known.

If you learned nothing durable that is not already known, answer exactly {}.

Example of a good answer:
{"reply_style":"bullet points","timezone":"Europe/Berlin","owns":"billing service"}`

// NewProfiler builds the worker over a profile store.
func NewProfiler(models Models, c *Counterparties, opts ProfilerOptions) (*Profiler, error) {
	if models == nil {
		return nil, fmt.Errorf("learning: the counterparty profiler needs a model registry")
	}
	if c == nil {
		return nil, fmt.Errorf("learning: the counterparty profiler needs a profile store to write to")
	}
	p := &Profiler{
		models: models, counterparts: c,
		timeout: opts.CallTimeout, maxTokens: opts.MaxTokens,
		maxSubjects: maxProfiledSubjects, maxBodyChars: maxProfiledBody,
		now: opts.Now,
	}
	if p.timeout <= 0 {
		p.timeout = DefaultAuxTimeout
	}
	if p.maxTokens <= 0 {
		p.maxTokens = DefaultProfilerTokens
	}
	if p.now == nil {
		p.now = func() time.Time { return time.Now().UTC() }
	}
	return p, nil
}

// Name implements [Worker].
func (p *Profiler) Name() string { return ProfilerSource }

// Skip implements [Worker].
//
// NOT gated on Settled — see the type comment. What it is gated on is having
// somebody to observe: an internal trigger (a schedule, a sandbox
// completion, an A2A ask) carries no interactions, and a pass over it would
// spend a call to profile nobody.
func (p *Profiler) Skip(t Turn) string {
	if t.Event.AgentHandle == "" {
		// Profiles are OBSERVER-scoped. Without a handle there is nobody
		// for the observation to belong to.
		return "no_observer"
	}
	if len(p.subjectsOf(t)) == 0 {
		return "no_counterparties"
	}
	return ""
}

// Reflect implements [Worker].
//
// Every subject is attempted even when an earlier one failed, and the errors
// are JOINED rather than returned on the first: a coalesced trigger's four
// senders are four independent observations, and losing three of them
// because the first party's model call timed out is a worse answer than
// three profiles and a reported failure.
func (p *Profiler) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	subjects := p.subjectsOf(t)
	out := make([]events.Payload, 0, len(subjects))
	var failures []error
	for _, s := range subjects {
		ev, err := p.observe(ctx, t, s)
		if err != nil {
			failures = append(failures, err)
		}
		if ev != nil {
			out = append(out, ev)
		}
	}
	if len(failures) > 0 {
		return out, fmt.Errorf("learning: profiling %d of %d counterparties: %w",
			len(failures), len(subjects), errors.Join(failures...))
	}
	return out, nil
}

// observe records one counterparty, returning the event when it landed.
func (p *Profiler) observe(ctx context.Context, t Turn, s subjectMessages) (events.Payload, error) {
	// THE PROFILE IS READ FIRST, and it is what makes the patch a patch:
	// without the known traits in front of it the model re-derives the
	// same preference every turn, and the store's merge then rewrites an
	// identical value while LastCorroboratedAt moves — which reports a
	// counterparty as freshly learned about when nothing was learned.
	existing, _, err := p.counterparts.Get(ctx, t.Event.AgentHandle, s.subject)
	if err != nil {
		// Degraded rather than skipped, the same trade the persist
		// decider makes for its dedup block: a patch written without the
		// known traits may restate one, while skipping writes nothing at
		// all for as long as the store is unhappy.
		log.WarnContext(ctx, "counterparty_profile_unavailable", "observer", t.Event.AgentHandle,
			"subject", s.subject.ExternalID, "error", err.Error())
	}

	traits, err := p.patch(ctx, t, s, existing)
	if err != nil {
		// NO RECORD ON A FAILED CALL. Recording an empty patch here would
		// move the interaction counter and LastUpdatedAt on the strength
		// of a call that never happened, which makes an unreachable model
		// look exactly like a counterparty whose traits have settled.
		return nil, err
	}

	counted, err := p.counterparts.Record(ctx, Observation{
		Observer: t.Event.AgentHandle,
		Subject:  s.subject,
		Traits:   traits,
		WorkKey:  t.Event.TurnID,
		At:       p.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("learning: record observation of %s: %w",
			s.subject.ExternalID, err)
	}
	if !counted {
		// The same work key was already counted — a redelivery, or two
		// nodes racing this turn. The guard worked; announcing it would
		// report an interaction that did not happen.
		log.DebugContext(ctx, "counterparty_already_counted", "observer", t.Event.AgentHandle,
			"subject", s.subject.ExternalID, "work_key", t.Event.TurnID)
		return nil, nil
	}
	return types.CounterpartyProfileUpdated{
		ObserverHandle:    t.Event.AgentHandle,
		RoleName:          t.Event.RoleName,
		TurnID:            t.Event.TurnID,
		SubjectHandle:     s.subject.Handle,
		SubjectExternalID: s.subject.ExternalID,
		SubjectPlatform:   s.subject.Platform,
		SubjectName:       s.subject.Name,
		TraitsPatched:     len(traits),
	}, nil
}

// patch asks the auxiliary model what is newly known about this party.
func (p *Profiler) patch(ctx context.Context, t Turn, s subjectMessages,
	existing Profile,
) (map[string]any, error) {
	member, err := p.models.Head(t.Role, phase.Auxiliary)
	if err != nil {
		return nil, fmt.Errorf("learning: no auxiliary model: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: ProfilerSystemPrompt},
			{Role: llm.RoleUser, Content: p.prompt(s, existing)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   p.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: profiler on %s: %w", member.Key, err)
	}
	var text string
	if completion != nil {
		text = strings.TrimSpace(completion.Content)
	}
	if text == "" {
		// An empty answer is "nothing new", which is the common case and
		// a legitimate observation — not a parse failure.
		return nil, nil
	}
	obj, ok := extractJSONObject(text)
	if !ok {
		// Logged rather than returned: a model writing prose has still
		// told us nothing new, and failing the observation would stop the
		// interaction counter as well.
		log.WarnContext(ctx, "counterparty_patch_unparseable", "turn_id", t.Event.TurnID,
			"subject", s.subject.ExternalID, "response", preview(text, 200))
		return nil, nil
	}
	return scalarTraits(obj), nil
}

// prompt renders one subject's messages and what is already on file.
func (p *Profiler) prompt(s subjectMessages, existing Profile) string {
	var b strings.Builder
	b.WriteString("## The person\n")
	b.WriteString(subjectLine(s.subject))
	b.WriteString("\n\n## Already known about them\n")
	if len(existing.Traits) == 0 {
		b.WriteString("Nothing on file yet.\n")
	} else {
		for _, k := range sortedKeys(existing.Traits) {
			fmt.Fprintf(&b, "- %s: %v\n", k, existing.Traits[k])
		}
	}
	b.WriteString("\n## What they said this time\n")
	for _, body := range s.bodies {
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", collapseLines(truncate(body, p.maxBodyChars)))
	}
	return b.String()
}

// subjectMessages is one party and everything they said this turn.
type subjectMessages struct {
	subject Subject
	bodies  []string
}

// subjectsOf groups a turn's interactions by the party who sent them.
//
// GROUPED rather than one pass per message: two messages from one person in
// a coalesced trigger are one conversation, and profiling them separately
// would spend two calls to ask the same question and move the interaction
// counter — no, the counter is work-keyed, so the second would be dropped
// and its call wasted entirely.
//
// Order is first-spoken, so a bound that trims the tail trims the people who
// contributed least.
func (p *Profiler) subjectsOf(t Turn) []subjectMessages {
	var (
		out   []subjectMessages
		index = map[Subject]int{}
	)
	for _, in := range t.Event.Interactions {
		s := Subject{
			Handle:     strings.TrimSpace(in.Sender.Handle),
			ExternalID: strings.TrimSpace(in.Sender.ExternalID),
			Platform:   strings.TrimSpace(in.Sender.Platform),
			Name:       strings.TrimSpace(in.Sender.DisplayName),
		}
		if !s.Valid() {
			// Nothing to key a profile on. An engine-authored
			// notification has no sender, and observing "" would file
			// every one of them under one profile.
			continue
		}
		if s.Handle == t.Event.AgentHandle {
			// A SEAT DOES NOT PROFILE ITSELF. Its own echoed post can
			// reach it as an interaction, and a self-profile is a bag of
			// traits nothing ever reads.
			continue
		}
		if at, ok := index[s]; ok {
			out[at].bodies = append(out[at].bodies, in.Body)
			continue
		}
		if len(out) >= p.maxSubjects {
			continue
		}
		index[s] = len(out)
		out = append(out, subjectMessages{subject: s, bodies: []string{in.Body}})
	}
	return out
}

// scalarTraits keeps the flat, scalar half of what a model answered.
//
// The contract asks for short scalars, and the store merges whatever it is
// given: a nested object or an array that slipped through would be written
// as a trait the prefetch then renders as Go's default formatting of a map,
// permanently, on every turn that party appears in. Dropping it here is the
// only place that can be caught, since by write time it is well-formed JSON.
func scalarTraits(obj map[string]any) map[string]any {
	if len(obj) == 0 {
		return nil
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		key := strings.TrimSpace(strings.ToLower(k))
		if key == "" {
			continue
		}
		switch value := v.(type) {
		case string:
			// An empty string is a trait the model could not fill in,
			// not a fact — and it would render as a bare bullet.
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				out[key] = truncate(trimmed, maxTraitValue)
			}
		case bool, float64:
			// float64 is every JSON number: encoding/json decodes them
			// all that way, so there is no integer case to add.
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxTraitValue caps one trait's rendered length.
//
// A trait is a working preference — "bullet points", "Europe/Berlin" — and
// the prefetch renders each as one bullet in a budgeted block. A model that
// answers with a paragraph under a snake_case key would otherwise spend that
// whole block on one party.
const maxTraitValue = 200

// subjectLine renders who a party is, for the profiler's prompt.
func subjectLine(s Subject) string {
	parts := make([]string, 0, 3)
	if s.Name != "" {
		parts = append(parts, s.Name)
	}
	if s.Handle != "" {
		parts = append(parts, "@"+s.Handle)
	}
	if s.Platform != "" && s.ExternalID != "" {
		parts = append(parts, s.Platform+" id "+s.ExternalID)
	}
	if len(parts) == 0 {
		return "(unidentified)"
	}
	return strings.Join(parts, " · ")
}

// sortedKeys orders a trait bag so the prompt is stable between turns.
//
// STABLE because the provider caches on an exact prefix: a map iterated in
// Go's randomised order renders differently every turn and misses the cache
// every turn, for a block whose content did not change.
func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// collapseLines folds a message onto one line.
//
// The prompt renders each message as a bullet, and a pasted log with its own
// newlines would otherwise break out of the list and read as instructions
// rather than as quoted content.
func collapseLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
