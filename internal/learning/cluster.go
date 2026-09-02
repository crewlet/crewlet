package learning

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"

	"github.com/google/uuid"
)

// Clustered synthesis: the pattern a seat repeats, rather than the turn it
// just finished.
//
// # Why single-turn induction is not enough
//
// The inline synthesizer sees one turn and asks "was that a procedure?". It
// catches the turn that was obviously one — five tools in a shape a model can
// describe — and misses the shape a seat arrives at over a fortnight: three
// tools, unremarkable on any single turn, run the same way eleven times. That
// is the more valuable skill, because eleven repetitions is evidence a single
// turn cannot offer, and it is invisible from inside any one of them.
//
// So this pass reads the seat's recent turns, groups them by how similar
// their tool runs are, and drafts from a GROUP. `scheduler_enabled`,
// `cluster_min_size`, `cluster_jaccard_threshold` and `episode_fetch_limit`
// all validated and were read by nothing before it existed.
//
// # One draft per seat per pass, largest cluster first
//
// Not every qualifying cluster. Each draft costs an auxiliary completion and
// the pass runs on a daily cadence, so a seat with three real patterns learns
// them over three days rather than spending three calls in one tick — and the
// strongest evidence goes first. A pass that drafted everything would also
// make the per-seat cap reachable in a single tick, which is the one way a
// catalogue fills with drafts nobody asked for.

// ClusterInterval is how often the clustering pass runs when
// `scheduler_interval_seconds` says nothing. Hourly, matching that field's
// shipped 3600.
//
// Its own constant rather than the curator's, even though both are loops over
// the roster: they tick for unrelated reasons — the curator ages a catalogue
// against a 30-day stale window, this one waits for repetitions to accumulate
// — and sharing a constant would make tuning either silently move the other.
//
// Hourly is cheap because the expensive half is CONDITIONAL. A tick costs one
// indexed scan per seat; the auxiliary call happens only when a cluster
// crossed `cluster_min_size` and is not already a skill, which after the
// first tick is rare. In exchange, a seat that has just repeated a procedure
// for the third time learns it within the hour rather than the next day.
const ClusterInterval = time.Hour

// DefaultClusterWindow is how far back a pass looks when
// `cluster_window_hours` says nothing. Seven days, matching that field.
//
// The SECOND bound, and it is not redundant with episode_fetch_limit: that
// one bounds a busy seat, whose last 200 turns are a week of work anyway.
// This one bounds a QUIET seat, whose last 200 turns can span a year — and
// clustering those would draft a skill from a procedure it abandoned in the
// spring, presented as what it does now.
const DefaultClusterWindow = 7 * 24 * time.Hour

// SkillCluster is one group of turns that ran the same way.
type SkillCluster struct {
	// Sequence is the representative tool run — the member the others
	// were measured against, so the skill's stored ToolSequence is a run
	// that actually happened rather than a union nobody performed.
	Sequence []string

	// TurnIDs are the turns in the cluster, newest first.
	TurnIDs []string

	// Episodes are the members themselves, for the prompt.
	Episodes []Episode
}

// Size is how many turns converged on this shape.
func (c SkillCluster) Size() int { return len(c.Episodes) }

// ClusterPass drafts at most one skill from the shapes a seat repeats.
//
// Returns the events to publish, empty when nothing qualified — which is the
// ordinary answer for a seat whose work does not repeat, and is not an error.
func (s *Synthesizer) ClusterPass(ctx context.Context, seat *org.Role, handle string) ([]events.Payload, error) {
	if s.episodes == nil {
		// No episode store was wired, so there is nothing to cluster. A
		// company with learning on always has one; this is the shape a
		// synthesizer built for the inline path alone takes.
		return nil, nil
	}

	// THE CAP FIRST, because it is one indexed count and everything below
	// it — a scan, a clustering, an auxiliary call — is thrown away for a
	// seat that has no room for another skill.
	count, err := s.skills.Count(ctx, handle, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("learning: counting %s's skills: %w", handle, err)
	}
	if count >= s.maxPerAgent {
		log.DebugContext(ctx, "skill_clustering_skipped", "reason", SkipSkillCapV,
			"agent_handle", handle, "skills", count, "cap", s.maxPerAgent)
		return nil, nil
	}

	recent, err := s.episodes.Recent(ctx, handle, s.fetchLimit)
	if err != nil {
		return nil, fmt.Errorf("learning: reading %s's recent episodes: %w", handle, err)
	}
	clusters := clusterEpisodes(recent, s.minToolCalls, s.clusterAt,
		s.now().Add(-s.clusterWindow))

	existing, err := s.skills.ToolSequences(ctx, handle, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("learning: reading %s's tool sequences: %w", handle, err)
	}

	// LARGEST FIRST, and the first that is not already a skill wins. A
	// cluster the seat has already learned is not a reason to stop: the
	// next one down may be the pattern it has not.
	for _, cluster := range clusters {
		if cluster.Size() < s.clusterMin {
			// Sorted by size, so everything after this is smaller too.
			break
		}
		if name, dup := mostSimilar(cluster.Sequence, existing, s.duplicateAt); dup {
			log.DebugContext(ctx, "skill_clustering_skipped", "reason", SkipDuplicateSkill,
				"agent_handle", handle, "similar_to", name, "cluster_size", cluster.Size())
			continue
		}
		return s.draftFromCluster(ctx, seat, handle, cluster)
	}
	log.DebugContext(ctx, "skill_clustering_found_nothing", "agent_handle", handle,
		"episodes", len(recent), "clusters", len(clusters), "min_size", s.clusterMin)
	return nil, nil
}

// draftFromCluster asks the model for one skill and writes it.
func (s *Synthesizer) draftFromCluster(ctx context.Context, seat *org.Role,
	handle string, cluster SkillCluster,
) ([]events.Payload, error) {
	member, err := s.models.Head(seat, phase.Auxiliary)
	if err != nil {
		return nil, fmt.Errorf("learning: no auxiliary model for skill clustering: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: ClusterSystemPrompt},
			{Role: llm.RoleUser, Content: buildClusterPrompt(cluster)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   s.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: drafting a clustered skill for %s: %w", handle, err)
	}
	draft, ok := parseSkillDraft(completion)
	if !ok {
		// The model looked at eleven similar runs and could not name a
		// procedure. Rarer than the single-turn decline and still not an
		// error: similar tool runs are not always the same work.
		log.DebugContext(ctx, "skill_clustering_declined", "agent_handle", handle,
			"cluster_size", cluster.Size())
		return nil, nil
	}

	at := s.now()
	skill := Skill{
		ID:          uuid.NewString(),
		AgentHandle: handle,
		Name:        draft.Name,
		Description: draft.Description,
		Content:     draft.Content,
		// The REPRESENTATIVE run, not a union of the cluster's tools: the
		// duplicate check compares a stored sequence against a new turn's,
		// and a union nobody performed would match everything loosely.
		ToolSequence:     cluster.Sequence,
		SourceEpisodeIDs: cluster.TurnIDs,
		Version:          1,
		CreatedAt:        at,
		UpdatedAt:        at,
		State:            SkillActive,
	}
	if err := s.skills.Insert(ctx, skill); err != nil {
		if errors.Is(err, ErrSkillExists) {
			log.DebugContext(ctx, "skill_clustering_lost_race", "agent_handle", handle,
				"skill", draft.Name)
			return nil, nil
		}
		return nil, fmt.Errorf("learning: writing %s's clustered skill %q: %w",
			handle, draft.Name, err)
	}

	log.InfoContext(ctx, "skill_synthesized", "agent_handle", handle, "skill", draft.Name,
		"skill_id", skill.ID, "trigger", string(types.SynthesisClustered),
		"cluster_size", cluster.Size(), "tools", len(cluster.Sequence))
	return []events.Payload{types.SkillSynthesized{
		AgentHandle: handle,
		RoleName:    seatName(seat),
		// NO TurnID. The draft came from a cluster rather than from one
		// turn, and naming any single member would put a trace on the
		// event that explains none of the other ten.
		SkillName:   draft.Name,
		SkillID:     skill.ID,
		Trigger:     types.SynthesisClustered,
		ClusterSize: cluster.Size(),
		ToolCount:   len(cluster.Sequence),
	}}, nil
}

// clusterEpisodes groups a seat's turns by how similar their tool runs are.
//
// GREEDY and single-pass over the newest-first listing: each episode joins
// the first cluster whose representative it is similar enough to, or starts
// one. Not k-means and not hierarchical — the question is "did this seat do
// the same thing repeatedly", and a greedy pass answers it in one traversal
// with a representative that is a real run rather than a centroid nobody
// performed.
//
// Only RAW, SETTLED turns with enough tools, inside the window, take part. A
// compacted row is already a summary of a cluster and would count a fold as
// one turn; an unsettled one is work the agent judged incomplete; a turn under
// min_tool_calls is a step rather than a procedure, exactly as in the inline
// path; and a turn older than the window is a procedure the seat may have
// stopped using, which a draft would present as what it does now.
//
// Returned largest first, ties broken by the newest member so a pass is
// deterministic over one snapshot of the table.
func clusterEpisodes(eps []Episode, minTools int, threshold float64,
	since time.Time,
) []SkillCluster {
	var clusters []SkillCluster
	for _, ep := range eps {
		if ep.Kind != KindRaw || len(ep.ToolSequence) < minTools {
			continue
		}
		if ep.EndedAt.Before(since) {
			continue
		}
		if ep.ReviewOutcome != "done" && ep.ReviewOutcome != "failed" {
			continue
		}
		joined := false
		for i := range clusters {
			if toolJaccard(ep.ToolSequence, clusters[i].Sequence) >= threshold {
				clusters[i].Episodes = append(clusters[i].Episodes, ep)
				clusters[i].TurnIDs = append(clusters[i].TurnIDs, ep.TurnID)
				joined = true
				break
			}
		}
		if !joined {
			clusters = append(clusters, SkillCluster{
				Sequence: slices.Clone(ep.ToolSequence),
				TurnIDs:  []string{ep.TurnID},
				Episodes: []Episode{ep},
			})
		}
	}
	slices.SortStableFunc(clusters, func(a, b SkillCluster) int {
		return b.Size() - a.Size()
	})
	return clusters
}

// clusterPromptTurns is how many of a cluster's members reach the prompt.
//
// The model is being asked what the members have in common, and eleven near
// identical turns say no more than five do while costing eleven turns of
// prompt budget. Five is enough to show the shape and the variation in it —
// the same number the compactor's exemplar budget settled on.
const clusterPromptTurns = 5

// ClusterSystemPrompt asks for the procedure a run of turns shares.
//
// Unlike the single-turn prompt it does NOT offer "decline if this was not a
// procedure" as the likely answer: the evidence here is repetition, and a
// model told the expected answer is nothing will give it for turns that
// genuinely converged. It may still decline, and says so in one place.
const ClusterSystemPrompt = `You are given several completed turns by the same
agent whose tool runs were nearly identical. Write the reusable procedure they
share, as a skill that agent can follow next time.

Answer with a JSON object and nothing else:
{"name":"kebab-case-name","description":"one line on when to use it",
 "content":"the procedure, as numbered steps"}

Write for the NEXT occurrence, not about these ones: no ticket numbers, no
names, no dates, nothing specific to any single turn. Say what varies between
them as a decision the reader has to make, not as an example.

Answer exactly {} if the turns share no procedure worth writing down — similar
tools do not always mean the same work.`

// buildClusterPrompt renders the cluster for the model.
func buildClusterPrompt(c SkillCluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d turns ran this tool sequence, or one very close to it:\n%s\n",
		c.Size(), strings.Join(c.Sequence, " -> "))
	b.WriteString("\nThe turns:\n")
	for i, ep := range c.Episodes {
		if i >= clusterPromptTurns {
			fmt.Fprintf(&b, "\n(and %d more with the same shape)\n",
				c.Size()-clusterPromptTurns)
			break
		}
		fmt.Fprintf(&b, "\n%d. Task: %s\n   Plan: %s\n   Tools: %s\n   Outcome: %s\n",
			i+1, orElse(ep.TaskSummary, "(no description)"),
			orElse(ep.PlanSummary, "(no plan)"),
			strings.Join(ep.ToolSequence, " -> "), ep.ReviewOutcome)
	}
	return b.String()
}

// seatName is the role a pass is running for, empty when it has none.
func seatName(seat *org.Role) string {
	if seat == nil {
		return ""
	}
	return seat.Name
}
