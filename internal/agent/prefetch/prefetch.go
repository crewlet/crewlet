// Package prefetch renders the context blocks a Plan-phase prompt is built
// with: what this seat remembers, what its company has written down, what it
// has done before, and who it is talking to.
package prefetch

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

var log = logging.Get("agent.prefetch")

// # Frozen at turn start, and that is a caching decision
//
// Every block here is resolved ONCE per turn and then held. A turn can run
// its Plan phase several times — a self_iterate loop rebuilds the whole LLM
// conversation — and a prefetch that re-ran would produce a different system
// prompt on each pass. Providers cache on an exact prefix, so a system
// prompt that moves costs the full prompt again on every iteration; worse,
// the planner would see its context change underneath a decision it is in
// the middle of making.
//
// The freeze is STRUCTURAL rather than remembered: the blocks are rendered
// before the turn and handed to the runner as fixed strings, so there is
// nowhere for a second fetch to happen.
//
// # Everything degrades to nothing
//
// A store that is unreachable, a model that is not configured, a filter that
// returns nonsense — every one of them renders an empty block. A turn must
// not die because a wiki was slow, and a planner given no memory plans worse
// than one given the right memory and far better than one given the wrong
// memory. That last point is why there is no recency-only fallback when the
// relevance filter fails: preferring "no memory this turn" over "somebody
// else's memory" is the only safe default for a seat that talks to several
// people.

// Blocks are the rendered sections, one per prompt heading.
//
// Strings rather than structures because that is exactly what the prompt
// takes, and a caller that received data would have to render it — which is
// how two callers come to render the same memory two different ways.
type Blocks struct {
	// PersonalMemory is what this seat has learned and kept, filtered for
	// relevance to this task.
	PersonalMemory string

	// RelevantKnowledge is what the company has written down, searched
	// live at turn time. There is no local index: procedural knowledge
	// lives in the team's knowledge base and the engine searches it on the
	// agent's behalf, so the block always reflects current content.
	RelevantKnowledge string

	// EpisodeRecall is similar work this seat has done before.
	EpisodeRecall string

	// CounterpartyProfile is what this seat has observed about whoever
	// triggered the turn.
	CounterpartyProfile string

	// SynthesizedSkills are the procedures this seat wrote for itself.
	SynthesizedSkills string

	// SkillIDs are the ids behind that block, carried out because a skill
	// OFFERED to a turn is a skill used: the curator ages a skill on when
	// it was last used, so a menu that never reports back would archive
	// the procedures a seat reads every single turn. Ids rather than
	// names, since a rename must not restart the clock.
	SkillIDs []string

	// OnboardingHint renders only for a seat that has not completed
	// onboarding for its current org chain.
	OnboardingHint string
}

// Empty reports that nothing was surfaced at all.
func (b Blocks) Empty() bool {
	return b.PersonalMemory == "" && b.RelevantKnowledge == "" &&
		b.EpisodeRecall == "" && b.CounterpartyProfile == "" &&
		b.SynthesizedSkills == "" && b.OnboardingHint == ""
}

// Request is one turn's worth of context to prefetch against.
type Request struct {
	// Seat is the agent whose prompt this is.
	Seat *org.Role

	// AgentID is the seat's derived runtime id, which is what the diary
	// and the episode store are keyed on.
	AgentID string

	// Org is the company, for the knowledge search's read scope.
	Org *org.Organization

	// Task is the trigger as the turn describes it — what everything here
	// is judged relevant AGAINST.
	Task string

	// Senders are the parties who triggered this turn, in the order they
	// spoke. Several on a coalesced trigger, and every one of them gets a
	// profile: a turn woken by four people is not a turn about the last
	// of them.
	Senders []learning.Subject

	// RequiresRecon says the trigger is a POINTER rather than the
	// context — a webhook naming a thing that changed. It gates the two
	// searches that judge relevance against the trigger text, because
	// filtering against a bare pointer returns noise wearing the shape of
	// relevance.
	RequiresRecon bool

	// TurnID identifies the turn, for the auxiliary calls' telemetry.
	TurnID string
}

// Models resolves the model a seat's auxiliary work runs on.
//
// The phase registry's own signature, so *phase.Registry satisfies it as
// written — the same seam the learning workers take, and for the same
// reason: an adapter here would be a second place deciding which model
// answers a seat's cheap questions.
type Models interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

// Diary is the seat's own memory, as much of it as this package reads.
type Diary interface {
	Recall(ctx context.Context, agentID string, q learning.RecallQuery, now time.Time) ([]learning.DiaryHit, error)
	Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]learning.DiaryEntry, error)
}

// Episodes is the seat's record of past turns.
type Episodes interface {
	Recall(ctx context.Context, q learning.RecallQuery) ([]learning.Hit, error)
}

// Counterparties is what this seat has observed about other people.
type Counterparties interface {
	Get(ctx context.Context, observer string, subject learning.Subject) (learning.Profile, bool, error)
}

// Skills are the procedures this seat has written for itself.
type Skills interface {
	List(ctx context.Context, handle string, opts learning.ListOptions) ([]learning.Skill, error)
}

// Onboarding says whether this seat has been through its first-turn pass.
type Onboarding interface {
	Onboarded(ctx context.Context, agentID, chainHash string) (bool, error)
}

// Sources are the stores the blocks are rendered from.
//
// EVERY ONE IS OPTIONAL. A nil source renders an empty block, which is what
// a company with reflection off, or no knowledge backend, or no database
// actually has — and each of those is a supported configuration rather than
// a degraded one.
type Sources struct {
	Diary          Diary
	Knowledge      knowledge.Searcher
	Episodes       Episodes
	Counterparties Counterparties
	Skills         Skills
	Onboarding     Onboarding

	// Models answers the auxiliary calls: the memory relevance filter,
	// the knowledge query, the episode summary. Nil turns all three off,
	// which for the first two means an empty block — see the package
	// comment on why there is no unfiltered fallback.
	Models Models

	// Embed turns text into a vector for the similarity searches. Nil
	// falls back to recency alone, which is a real degradation rather
	// than a failure: recent memories are still this seat's memories.
	Embed func(ctx context.Context, text string) ([]float32, error)

	// SummarizeEpisodes is the operator's switch for whether episode hits
	// are passed through the auxiliary model. It gates ONLY that call —
	// the Python engine's version of this knob was wired by passing a nil
	// provider pool, which silently disabled the memory and knowledge
	// filters too.
	SummarizeEpisodes bool
}

// Fetcher renders the blocks for a turn.
type Fetcher struct {
	src Sources
	now func() time.Time
}

// New builds a fetcher. The zero Sources is valid and renders nothing.
func New(src Sources) *Fetcher { return &Fetcher{src: src, now: time.Now} }

// Fetch renders every block, concurrently.
//
// IN PARALLEL because they are independent and each is a round trip: run in
// sequence, a turn's start would cost the sum of an embedding call, three
// auxiliary completions and two database reads before the planner sees
// anything. Wall clock here is the slowest one, not the total.
//
// It never returns an error. Each block reports its own failure into the
// log and renders empty — see the package comment.
func (f *Fetcher) Fetch(ctx context.Context, r Request) Blocks {
	if f == nil || r.Seat == nil {
		return Blocks{}
	}
	var (
		blocks Blocks
		wg     sync.WaitGroup
	)
	spawn := func(render func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			render()
		}()
	}
	run := func(into *string, render func() string) {
		spawn(func() {
			defer recoverInto(into)
			*into = render()
		})
	}
	run(&blocks.PersonalMemory, func() string { return f.personalMemory(ctx, r) })
	run(&blocks.RelevantKnowledge, func() string { return f.relevantKnowledge(ctx, r) })
	run(&blocks.EpisodeRecall, func() string { return f.episodeRecall(ctx, r) })
	run(&blocks.CounterpartyProfile, func() string { return f.counterpartyProfile(ctx, r) })
	// Its own spawn rather than a run(), because it is the one block that
	// reports something back besides its prose.
	spawn(func() {
		defer recoverSkills(&blocks.SynthesizedSkills, &blocks.SkillIDs)
		blocks.SynthesizedSkills, blocks.SkillIDs = f.synthesizedSkills(ctx, r)
	})
	run(&blocks.OnboardingHint, func() string { return f.onboardingHint(ctx, r) })
	wg.Wait()
	return blocks
}

// recoverInto turns a panic in one block's renderer into an empty block.
//
// The blocks run concurrently, so an unrecovered panic in any of them takes
// the whole PROCESS down rather than the turn — and these renderers read
// stores, decode model output and index into slices whose length came from
// an LLM. That is a wide enough surface that "this must never panic" is a
// hope rather than a guarantee, and the guarantee is worth more than the
// stack trace.
func recoverInto(into *string) {
	if r := recover(); r != nil {
		log.Error("prefetch_block_panicked", "panic", r)
		*into = ""
	}
}

// recoverSkills is [recoverInto] for the one block with two outputs.
//
// BOTH are cleared: ids naming skills whose menu never rendered would be
// reported as used by a turn that was never told about them, which is the
// one way this list can lie to the curator.
func recoverSkills(into *string, ids *[]string) {
	if r := recover(); r != nil {
		log.Error("prefetch_block_panicked", "panic", r)
		*into, *ids = "", nil
	}
}

// auxCall runs one auxiliary completion for a seat.
//
// ONE PLACE, so the timeout, the temperature and the "no tools" rule are the
// same for all three callers. A tool on the surface invites a model to call
// it and answer nothing, and there is no tool any of these passes could use.
func (f *Fetcher) auxCall(ctx context.Context, seat *org.Role, system, user string, maxTokens int) (string, bool) {
	if f.src.Models == nil {
		return "", false
	}
	member, err := f.src.Models.Head(seat, phase.Auxiliary)
	if err != nil {
		log.DebugContext(ctx, "prefetch_no_auxiliary_model", "seat", seat.Handle(), "error", err)
		return "", false
	}
	call, cancel := context.WithTimeout(ctx, AuxTimeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, auxRequest(system, user, maxTokens))
	if err != nil {
		log.WarnContext(ctx, "prefetch_auxiliary_call_failed", "seat", seat.Handle(),
			"model", member.Key, "error", err.Error())
		return "", false
	}
	if completion == nil {
		// A provider answering (nil, nil) is a contract violation, and
		// checked rather than left to the recover: the recover exists for
		// what nobody foresaw, and reaching it here would report a panic
		// where the honest answer is "this backend returned nothing".
		log.WarnContext(ctx, "prefetch_auxiliary_returned_nothing", "seat", seat.Handle(),
			"model", member.Key)
		return "", false
	}
	text := strings.TrimSpace(completion.Content)
	if text == "" {
		// An empty answer from a THINKING model usually means the cap
		// was spent reasoning: the call returns with output at the cap
		// and nothing visible. Reported with both numbers, because the
		// fix is a bigger cap and nothing else says so.
		log.WarnContext(ctx, "prefetch_auxiliary_answered_nothing", "seat", seat.Handle(),
			"model", member.Key, "output_tokens", completion.OutputTokens,
			"max_tokens", maxTokens)
		return "", false
	}
	return text, true
}

// budget caps a rendered block's characters.
//
// Applied per BULLET rather than by truncating the block, so a block never
// ends mid-sentence: a reader — a model — that hits a truncated final line
// cannot tell whether the thought was completed elsewhere.
func budget(bullets []string, chars int) string {
	rendered, _ := budgetN(bullets, chars)
	return rendered
}

// budgetN is [budget] plus how many bullets survived.
//
// The count exists for the one caller that has a PARALLEL list to trim —
// the skill menu's ids, which are reported back as used. Counting lines in
// the rendered text instead would be wrong the first time a description
// contained a newline, and would be wrong silently.
func budgetN(bullets []string, chars int) (string, int) {
	var (
		kept []string
		used int
	)
	for _, bullet := range bullets {
		if bullet == "" {
			continue
		}
		if used+len(bullet) > chars && len(kept) > 0 {
			break
		}
		kept = append(kept, bullet)
		used += len(bullet) + 1
	}
	return strings.Join(kept, "\n"), len(kept)
}
