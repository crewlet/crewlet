package prefetch

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The four blocks with no auxiliary judgement in them — plus one optional
// summary pass over the episodes.

const (
	// recallHits is how many past turns are recalled.
	//
	// Three, and low on purpose: an episode is a whole turn compressed
	// into two sentences, so each one is expensive to read and the fourth
	// most-similar is rarely the one that helps.
	recallHits = 3

	// maxRenderedTraits caps one counterparty's traits.
	//
	// A profiler pass contributes at most a handful of keys, but the store
	// merges them and the set only grows — a counterparty worked with for
	// months accumulates traits without limit, and every one of them
	// landed in every prompt that mentioned them. The cap is on the
	// RENDERING rather than the record: the observations are worth
	// keeping, the prompt is what has a budget. It says how many it
	// dropped, so a reader is not misled into thinking that is all the
	// seat knows.
	maxRenderedTraits = 24
)

// EmptyRecallHint is what the recall block says when the search was skipped
// on a thin trigger.
const EmptyRecallHint = "(no similar prior work surfaced at turn start — " +
	"query your episode history once you know what this task actually " +
	"involves)"

// recallSummarySystemPrompt compresses episode bullets.
const recallSummarySystemPrompt = `You compress an AI agent's record of its own past turns into a short briefing for its next one.

Output format (strict):
- At most 4 short bullet lines, no preamble and no conclusion.
- Each line: what the past turn was about, and what came of it.
- Keep identifiers, tool names and outcomes verbatim.
- Drop anything that does not bear on doing similar work again.

If none of the past turns would help with the current task, output nothing at all.`

// episodeRecall renders similar prior work.
func (f *Fetcher) episodeRecall(ctx context.Context, r Request, vector turnVector) string {
	if f.src.Episodes == nil || r.Seat == nil || strings.TrimSpace(r.Task) == "" {
		return ""
	}
	handle := r.Seat.Handle()
	if handle == "" {
		return ""
	}
	if !r.judgeable() {
		// Same gate as memory and knowledge, and the same reason: a
		// similarity search against a pointer returns the seat's most
		// recent work rather than its most relevant.
		return EmptyRecallHint
	}
	v, ok := vector()
	if !ok {
		// NO FALLBACK TO RECENCY. Episode recall's whole claim is "this
		// resembles what you are doing now"; the three most recent turns
		// carry no such claim, and an executor told they are similar
		// work will treat them as precedent.
		return ""
	}
	hits, err := f.src.Episodes.Recall(ctx, learning.RecallQuery{
		Handle: handle, Embedding: v.Values, Model: v.Model, Limit: recallHits,
	})
	if err != nil {
		log.WarnContext(ctx, "episode_recall_failed", "seat", handle, "error", err.Error())
		return ""
	}
	if len(hits) == 0 {
		return ""
	}

	// ONE DEADLINE FOR EVERY MODEL CALL THE BLOCK MAKES — its rewrites and
	// its summary alike — so the block waits at most one auxiliary call's
	// [AuxTimeout] past the recall, as it did when the summary was all it
	// called, and is never the block every turn's start waits for. Each
	// render already holds its own rewrites to one deadline
	// ([learning.PastTurns]); this one is shared across the summary and
	// the bullets it falls back to, so a summary that did not answer in
	// time is not followed by another thirty seconds of rewrites from the
	// same seat's auxiliary chain: whatever is left of the deadline is
	// theirs, and a text no rewrite reached by then is named by its size.
	ctx, cancel := context.WithTimeout(ctx, AuxTimeout)
	defer cancel()

	// WHICH PATH PAYS WHICH CALL. With the summary on, its model reads the
	// recalled asks and accounts WHOLE — up to [summaryTextBytes] each,
	// which almost every one is under — and its one briefing replaces them,
	// so condensing them to a reader's [learning.EpisodeAccountBytes] first
	// was rewrites spent on text the summary threw away, and a summary
	// written from rewrites instead of from the turns. The raw bullets, each
	// text condensed past that reader's bound, are rendered only where they
	// are what the seat is shown: the summary off, or a summary that did not
	// answer.
	if f.src.SummarizeEpisodes && f.src.Models != nil {
		// ONE TOKEN FOR THE CALL, minted for it and never the turn's: the
		// task and the past asks it reads are both somebody else's words
		// on most turns, fenced under it ([Ask], [learning.RenderPastTurn]).
		token := provenance.NewBoundary()
		whole := joinBullets(f.renderEpisodes(ctx, r, hits, summaryTextBytes, token))
		if whole == "" {
			return ""
		}
		// THE SUMMARY IS OPTIONAL AND ITS FAILURE IS FREE: the raw
		// bullets are a usable block, so a model that is slow or
		// unreachable costs verbosity rather than the block.
		//
		// JUDGED AGAINST WHAT THE TURN WAS ASKED ([Request.Ask]), for the
		// memory filter's reason: the summary keeps "what bears on doing
		// similar work again", and an integration's triage scaffolding
		// is the same on every turn of its surface, so it bears on
		// nothing.
		//
		// THE TASK AND THE PAST ASKS ARRIVE FENCED, under the call's token
		// ([Ask], [learning.RenderPastTurn]): the briefing is written into
		// the seat's system prompt with nothing around it, so words that
		// address the summary — today's or a month-old stranger's — must
		// read as somebody's words, and what the summary writes is
		// neutralised under the turn's token, so nothing it echoes can pass
		// for a fence's line there.
		summary, ok := f.auxCall(ctx, r, types.AuxEpisodeSummary, recallSummarySystemPrompt,
			"Current task:\n"+r.Ask.For(token)+
				"\n\nPast turns by this agent:\n"+whole+
				"\n\nBriefing:")
		if ok && strings.TrimSpace(summary) != "" {
			return provenance.Neutralise(r.Boundary, summary)
		}
	}
	return joinBullets(f.renderEpisodes(ctx, r, hits, learning.EpisodeAccountBytes, r.Boundary))
}

// summaryTextBytes bounds one recalled turn's ask, and its account, as the
// episode summary's input, where [learning.EpisodeAccountBytes] bounds each as
// the block a seat reads.
//
// ONE AUXILIARY CALL'S INPUT ROOM, shared by the texts recalled: a sixth of
// [compact.ChunkBytes] — the most one auxiliary completion is handed, about
// sixteen thousand tokens — so three turns' asks and accounts together fit one
// call, and only a text past about eleven kilobytes, a task description or a
// final answer pages long, is condensed before the summary reads it.
const summaryTextBytes = compact.ChunkBytes / (2 * recallHits)

// renderEpisodes renders the recalled turns, each one's ask and account
// condensed where it is past textBytes — through [learning.PastTurns], the
// one rendering the query_episodes tool shares, so at most [compact.Parallel]
// rewrites run at once and all of them are held to one deadline: the render's
// own [learning.EpisodeRewriteTimeout], or ctx's when sooner — and each turn
// rendered by [learning.RenderPastTurn], its ask fenced under b as the label it
// was asked with says.
func (f *Fetcher) renderEpisodes(ctx context.Context, r Request, hits []learning.Hit,
	textBytes int, b provenance.Boundary,
) []string {
	episodes := make([]learning.Episode, len(hits))
	for i, hit := range hits {
		episodes[i] = hit.Episode
	}
	turns := learning.PastTurns(ctx, episodes, f.src.Compact.For(r.Seat, r.Aux), textBytes)
	bullets := make([]string, len(hits))
	for i, hit := range hits {
		bullets[i] = learning.RenderPastTurn(b, hit.Episode, turns[i])
	}
	return bullets
}

// counterpartyProfile renders what this seat has observed about whoever
// triggered the turn.
//
// ONE BLOCK PER DISTINCT SENDER. A coalesced trigger is several people
// speaking, and rendering only the latest would hand the executor a profile
// of whoever happened to speak last while it answers all of them.
func (f *Fetcher) counterpartyProfile(ctx context.Context, r Request) string {
	if f.src.Counterparties == nil || r.Seat == nil || len(r.Senders) == 0 {
		return ""
	}
	observer := r.Seat.Handle()
	if observer == "" {
		return ""
	}
	var (
		blocks []string
		seen   = map[learning.Subject]bool{}
	)
	for _, subject := range r.Senders {
		if !subject.Valid() || seen[subject] {
			continue
		}
		seen[subject] = true
		profile, ok, err := f.src.Counterparties.Get(ctx, observer, subject)
		if err != nil {
			log.WarnContext(ctx, "counterparty_lookup_failed", "observer", r.Seat.Handle(),
				"error", err.Error())
			continue
		}
		if !ok {
			// Nobody has been profiled yet, which is the ordinary case
			// for a first interaction and not worth a line saying so.
			continue
		}
		blocks = append(blocks, renderProfile(r.Boundary, profile))
	}
	// "\n\n", not "\n": each element is a MULTI-LINE block, and a single
	// newline runs the second sender's opening line straight onto the end of
	// the first sender's. The char budget used to hide that by rendering only
	// one profile — it admitted the first block and dropped the rest — which
	// silently negated this function's whole reason to exist: ONE BLOCK PER
	// DISTINCT SENDER, so a turn woken by four people is not a turn about
	// whichever of them spoke last.
	return strings.Join(blocks, "\n\n")
}

// renderProfile renders one counterparty.
//
// THE SUBJECT HEADER IS NOT OPTIONAL. This block arrives with no
// conversational context around it, so a list of traits with no name on it
// tells the executor what somebody prefers without saying who, which is
// worse than nothing, because it invites applying it to whoever is asking.
//
// NEUTRALISED under fence, the turn's token (ADR-0040): the name is the one
// the person chose and every trait is what the profiler's model concluded
// from their words, so nothing here may open or close one of the turn's
// fences.
func renderProfile(fence provenance.Boundary, p learning.Profile) string {
	return provenance.Neutralise(fence, profileText(p))
}

// profileText is [renderProfile]'s text before it is neutralised.
func profileText(p learning.Profile) string {
	var b strings.Builder
	b.WriteString("Subject: " + firstNonEmpty(p.Subject.Label(), "(unknown)"))
	if p.Subject.Platform != "" {
		b.WriteString("\nPlatform: " + p.Subject.Platform)
	}
	b.WriteString("\n")

	if len(p.Traits) == 0 {
		b.WriteString("Observed by you: (no traits yet)\n")
	} else {
		b.WriteString("Observed by you:\n")
		// SORTED, because a map's order is randomised and a profile that
		// reorders itself between two otherwise-identical turns breaks
		// the provider's prompt cache for nothing.
		keys := slices.Sorted(maps.Keys(p.Traits))
		shown := min(len(keys), maxRenderedTraits)
		for _, key := range keys[:shown] {
			b.WriteString("  - " + key + ": " + renderTrait(p.Traits[key]) + "\n")
		}
		if dropped := len(keys) - shown; dropped > 0 {
			b.WriteString("  … and " + strconv.Itoa(dropped) +
				" more observed traits\n")
		}
	}
	b.WriteString("(interactions: " + strconv.Itoa(p.InteractionCount))
	if !p.LastUpdatedAt.IsZero() {
		b.WriteString(", last updated: " + p.LastUpdatedAt.UTC().Format("2006-01-02"))
	}
	b.WriteString(")")
	return b.String()
}

// renderTrait renders one trait value, whatever shape the model invented.
func renderTrait(value any) string {
	switch v := value.(type) {
	case string:
		return collapse(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, collapse(fmt.Sprint(item)))
		}
		return strings.Join(parts, ", ")
	default:
		return collapse(fmt.Sprint(v))
	}
}

// synthesizedSkills renders the procedures this seat wrote for itself.
func (f *Fetcher) synthesizedSkills(ctx context.Context, r Request) (string, []string) {
	if f.src.Skills == nil || r.Seat == nil {
		return "", nil
	}
	handle := r.Seat.Handle()
	if handle == "" {
		return "", nil
	}
	// ARCHIVED EXCLUDED, STALE KEPT. Archived is "aged out of the
	// catalogue" and bringing one back is an operator's decision; stale is
	// a marker on a skill that still works and revives the moment it is
	// used, so hiding it is how a useful skill starves.
	skills, err := f.src.Skills.List(ctx, handle, learning.ListOptions{})
	if err != nil {
		log.WarnContext(ctx, "skills_list_failed", "seat", handle, "error", err.Error())
		return "", nil
	}
	if len(skills) == 0 {
		return "", nil
	}
	// EVERY SKILL, no list cap. The block is a MENU — a name and a
	// description per line, and the seat loads the one it wants — so its
	// cost is a line each, not a body each. The cap that used to sit here
	// kept the first twelve in whatever order the store returned, and the
	// ids this function reports back are what resets a skill's staleness
	// clock: a skill past the cut was never offered, so it was never used,
	// so it went stale and archived. That is the starvation the branch
	// above refuses to do to stale skills, done instead by position.
	bullets := make([]string, 0, len(skills)+1)
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		line := renderSkill(r.Boundary, skill)
		if line == "" {
			// A skill with no name renders nothing, so it was not
			// offered and must not be reported as used.
			continue
		}
		bullets = append(bullets, line)
		ids = append(ids, skill.ID)
	}
	rendered := joinBullets(bullets)
	if rendered == "" {
		return "", nil
	}
	// THE MENU NEEDS ITS VERB. A list of skill names with no instruction
	// is a list the model reads and does not act on — it has to be told
	// that loading one is a thing it can do.
	return rendered + "\nLoad any of these by name with your skill tool " +
		"before doing the work it covers.", ids
}

// renderSkill renders one skill as a menu line, its name and description
// neutralised under b, the turn's token (ADR-0040): both are what the
// synthesizer's model wrote after reading the turns it drew on.
func renderSkill(b provenance.Boundary, s learning.Skill) string {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return ""
	}
	line := "- **" + provenance.Neutralise(b, name) + "**"
	if description := collapse(s.Description); description != "" {
		line += ": " + provenance.Neutralise(b, description)
	}
	if s.State == learning.SkillStale {
		// MARKED, not hidden. The seat can still load it — loading is
		// what revives it — and knowing it has gone unused is exactly
		// the context for deciding whether to.
		line += " _(unused lately)_"
	}
	return line
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
