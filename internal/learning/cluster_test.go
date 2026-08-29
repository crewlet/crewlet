package learning_test

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT THE CLUSTERED PASS IS FOR.
//
// The inline synthesizer sees one turn and asks "was that a procedure?". It
// misses the shape a seat arrives at over a fortnight — three tools,
// unremarkable on any single turn, run the same way eleven times. That is the
// more valuable skill, because repetition is evidence a single turn cannot
// offer. `scheduler_enabled`, `cluster_min_size`, `cluster_jaccard_threshold`
// and `episode_fetch_limit` all validated and were read by nothing before
// this pass existed.

// clusterer builds a synthesizer with the clustered path wired.
func clusterer(t *testing.T, db *store.DB, answer string,
	opts learning.SynthesizerOptions,
) (*learning.Synthesizer, *auxProvider) {
	t.Helper()
	p := &auxProvider{replies: []llm.Completion{{Content: answer}}}
	opts.Episodes = learning.NewEpisodes(db)
	s, err := learning.NewSynthesizer(&stubModels{p: p}, learning.NewSkills(db), opts)
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	return s, p
}

// clusterSeat is the role a pass runs for.
var clusterSeat = &org.Role{Name: "Dev"}

// writeEpisodes appends n turns with the given tool run.
func writeEpisodes(t *testing.T, db *store.DB, n int, outcome string, tools ...string) {
	t.Helper()
	eps := learning.NewEpisodes(db)
	base := time.Now().UTC().Add(-time.Duration(n) * time.Hour)
	for i := range n {
		_, err := eps.Append(t.Context(), learning.Episode{
			ID: episodeID(), Handle: "dev", Role: "Dev",
			TurnID:      "turn-" + strings.Join(tools, "") + "-" + itoa(i),
			StartedAt:   base.Add(time.Duration(i) * time.Hour),
			EndedAt:     base.Add(time.Duration(i)*time.Hour + time.Minute),
			TaskSummary: "ship release " + itoa(i), PlanSummary: "cut, tag, announce",
			ToolSequence: tools, ReviewOutcome: outcome, Kind: learning.KindRaw,
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// A REPEATED SHAPE BECOMES A SKILL — the pass whose absence made every
// cluster_* knob inert.
func TestARepeatedToolRunIsDistilledIntoASkill(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 4, "done", "fetch", "build", "tag", "announce")
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})

	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want the SkillSynthesized event", len(out))
	}
	ev, ok := out[0].(types.SkillSynthesized)
	if !ok {
		t.Fatalf("payload = %T", out[0])
	}
	if ev.Trigger != types.SynthesisClustered {
		t.Fatalf("Trigger = %q, want %q — the clustered event was published as "+
			"a single-turn one", ev.Trigger, types.SynthesisClustered)
	}
	if ev.ClusterSize != 4 {
		t.Fatalf("ClusterSize = %d, want 4", ev.ClusterSize)
	}
	if ev.TurnID != "" {
		t.Fatalf("TurnID = %q — a cluster has no single causing turn", ev.TurnID)
	}

	stored, found, err := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if err != nil || !found {
		t.Fatalf("Get: %v found=%v", err, found)
	}
	if len(stored.SourceEpisodeIDs) != 4 {
		t.Fatalf("SourceEpisodeIDs = %v, want every member of the cluster",
			stored.SourceEpisodeIDs)
	}
	if strings.Join(stored.ToolSequence, ",") != "fetch,build,tag,announce" {
		t.Fatalf("ToolSequence = %v — the stored run must be one that happened",
			stored.ToolSequence)
	}
	if p.calls != 1 {
		t.Fatalf("calls = %d, want one draft per pass", p.calls)
	}
}

// A CLUSTER UNDER cluster_min_size EARNS NOTHING, and costs no model call.
// Two turns that ran the same tools is a coincidence.
func TestASmallClusterEarnsNoSkillAndNoCall(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 2, "done", "fetch", "build", "tag", "announce")
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})

	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 0 || p.calls != 0 {
		t.Fatalf("payloads = %d, calls = %d — a cluster of 2 under a min of 3 "+
			"produced work", len(out), p.calls)
	}
}

// THE COMPANY'S OWN min_size IS WHAT APPLIES, not the package default.
func TestTheConfiguredClusterMinimumIsWhatApplies(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 2, "done", "fetch", "build", "tag", "announce")
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 2,
	})

	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 1 || p.calls != 1 {
		t.Fatalf("payloads = %d, calls = %d — a company that lowered "+
			"cluster_min_size to 2 got the default of 3", len(out), p.calls)
	}
}

// THE LARGEST CLUSTER GOES FIRST. One draft per pass, so which one it is
// decides what the seat learns today rather than in three days.
func TestTheStrongestPatternIsDraftedFirst(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	// THE BIG CLUSTER WRITTEN FIRST, so it is the OLDER one. The pass reads
	// newest-first, so without an explicit sort the smaller, more recent
	// cluster would be the one it reaches — which is exactly the mistake
	// this case exists to catch.
	writeEpisodes(t, db, 6, "done", "fetch", "build", "tag", "announce")
	writeEpisodes(t, db, 3, "done", "triage", "reproduce", "patch")
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})

	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want exactly one draft per pass", len(out))
	}
	if got := out[0].(types.SkillSynthesized).ClusterSize; got != 6 {
		t.Fatalf("ClusterSize = %d, want the 6-turn cluster, not the 3-turn one", got)
	}
	if p.calls != 1 {
		t.Fatalf("calls = %d — a pass drafted more than one cluster", p.calls)
	}
	// And the prompt was about the run that repeated six times.
	prompt := p.seen[0].Messages[len(p.seen[0].Messages)-1].Content
	if !strings.Contains(prompt, "fetch -> build -> tag -> announce") {
		t.Fatalf("the prompt is about the wrong cluster:\n%s", prompt)
	}
}

// A CLUSTER THE SEAT HAS ALREADY LEARNED IS NOT A REASON TO STOP: the next
// pattern down may be the one it has not.
func TestAnAlreadyLearnedClusterYieldsToTheNextOne(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 6, "done", "fetch", "build", "tag", "announce")
	writeEpisodes(t, db, 4, "done", "triage", "reproduce", "patch")
	skills := learning.NewSkills(db)
	now := time.Now().UTC()
	if err := skills.Insert(t.Context(), learning.Skill{
		ID: "known", AgentHandle: "dev", Name: "cut-a-release",
		Description: "ship it", Content: "1. run the pipeline",
		ToolSequence: []string{"fetch", "build", "tag", "announce"},
		CreatedAt:    now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	s, p := clusterer(t, db, `{"name":"triage-a-bug","description":"find it",`+
		`"content":"1. reproduce\n2. patch"}`, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})

	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d — the pass stopped at the cluster it already knew", len(out))
	}
	if got := out[0].(types.SkillSynthesized).SkillName; got != "triage-a-bug" {
		t.Fatalf("drafted %q, want the pattern the seat had not learned", got)
	}
	if p.calls != 1 {
		t.Fatalf("calls = %d — the known cluster reached the model", p.calls)
	}
}

// ONLY RAW, SETTLED TURNS WITH ENOUGH TOOLS TAKE PART. A compacted row is
// already a summary of a cluster, an unsettled turn is work the agent judged
// incomplete, and a short run is a step rather than a procedure.
func TestTheClusterIgnoresRowsThatAreNotEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(t *testing.T, db *store.DB)
	}{
		{"self_iterate turns", func(t *testing.T, db *store.DB) {
			writeEpisodes(t, db, 5, "self_iterate", "fetch", "build", "tag", "announce")
		}},
		{"runs under min_tool_calls", func(t *testing.T, db *store.DB) {
			writeEpisodes(t, db, 5, "done", "fetch", "build")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := newStore(t)
			c.write(t, db)
			s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
				MinToolCalls: 3, ClusterMinSize: 3,
			})
			out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
			if err != nil {
				t.Fatalf("ClusterPass: %v", err)
			}
			if len(out) != 0 || p.calls != 0 {
				t.Fatalf("payloads = %d, calls = %d — %s were counted as evidence",
					len(out), p.calls, c.name)
			}
		})
	}
}

// DISSIMILAR RUNS DO NOT POOL. Without the threshold every turn a seat ever
// took would be one cluster, and the "procedure" drafted from it would be
// the seat's job description.
func TestUnrelatedRunsDoNotPoolIntoOneCluster(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	for _, tools := range [][]string{
		{"fetch", "build", "tag"},
		{"triage", "reproduce", "patch"},
		{"read", "summarize", "post"},
		{"query", "chart", "share"},
	} {
		writeEpisodes(t, db, 2, "done", tools...)
	}
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})
	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 0 || p.calls != 0 {
		t.Fatalf("payloads = %d, calls = %d — eight unrelated turns pooled into "+
			"a cluster", len(out), p.calls)
	}
}

// THE PER-SEAT CAP IS CHECKED BEFORE ANYTHING ELSE. A seat with no room pays
// for no scan, no clustering and no call.
func TestAFullCatalogueCostsNoClusteringWork(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 6, "done", "fetch", "build", "tag", "announce")
	skills := learning.NewSkills(db)
	now := time.Now().UTC()
	for i := range 2 {
		if err := skills.Insert(t.Context(), learning.Skill{
			ID: "s" + itoa(i), AgentHandle: "dev", Name: "skill-" + itoa(i),
			Description: "d", Content: "c", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3, MaxSkillsPerAgent: 2,
	})
	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 0 || p.calls != 0 {
		t.Fatalf("payloads = %d, calls = %d — a seat at its cap did clustering work",
			len(out), p.calls)
	}
}

// A DECLINE IS NOT AN ERROR. Similar tools do not always mean the same work,
// and a pass that failed on that would report a company whose clustering is
// permanently broken.
func TestAClusterTheModelDeclinesWritesNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeEpisodes(t, db, 5, "done", "fetch", "build", "tag", "announce")
	s, _ := clusterer(t, db, "{}", learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3,
	})
	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("payloads = %d, want none", len(out))
	}
	if n, err := learning.NewSkills(db).Count(t.Context(), "dev", learning.ListOptions{}); err != nil || n != 0 {
		t.Fatalf("skills = %d (err %v), want none written", n, err)
	}
}

// TURNS OLDER THAN THE WINDOW ARE NOT EVIDENCE. episode_fetch_limit bounds a
// BUSY seat, whose last 200 turns are a week of work anyway; the window bounds
// a QUIET one, whose last 200 turns can span a year. Without it a pass drafts
// a skill from a procedure the seat abandoned in the spring and presents it as
// what it does now.
func TestTurnsOutsideTheWindowAreNotClustered(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	writeAgedEpisodes(t, db, 5, 40*24*time.Hour, "fetch", "build", "tag", "announce")
	s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3, ClusterWindow: 7 * 24 * time.Hour,
	})
	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 0 || p.calls != 0 {
		t.Fatalf("payloads = %d, calls = %d — turns 40 days old were clustered "+
			"under a 7-day window", len(out), p.calls)
	}

	// The same turns inside a wider window DO cluster, so the case is about
	// the window rather than about the fixture never qualifying.
	wide, wideP := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
		MinToolCalls: 3, ClusterMinSize: 3, ClusterWindow: 90 * 24 * time.Hour,
	})
	out, err = wide.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil {
		t.Fatalf("ClusterPass: %v", err)
	}
	if len(out) != 1 || wideP.calls != 1 {
		t.Fatalf("payloads = %d, calls = %d — a 90-day window still excluded "+
			"40-day-old turns", len(out), wideP.calls)
	}
}

// A SYNTHESIZER WITH NO EPISODE STORE ANSWERS EMPTY rather than failing: that
// is the shape one built for the inline path alone takes, and the inline path
// needs no episodes at all.
func TestAClusterPassWithoutEpisodesIsANoop(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	s := synthesizer(t, db, goodDraft, learning.SynthesizerOptions{})
	out, err := s.ClusterPass(t.Context(), clusterSeat, "dev")
	if err != nil || len(out) != 0 {
		t.Fatalf("ClusterPass = %v, %v — want a silent no-op", out, err)
	}
}

// writeAgedEpisodes appends n turns that all ended `age` ago.
func writeAgedEpisodes(t *testing.T, db *store.DB, n int, age time.Duration, tools ...string) {
	t.Helper()
	eps := learning.NewEpisodes(db)
	base := time.Now().UTC().Add(-age)
	for i := range n {
		_, err := eps.Append(t.Context(), learning.Episode{
			ID: episodeID(), Handle: "dev", Role: "Dev",
			TurnID:    "aged-" + itoa(i),
			StartedAt: base, EndedAt: base.Add(time.Minute),
			TaskSummary: "ship release " + itoa(i), PlanSummary: "cut, tag, announce",
			ToolSequence: tools, ReviewOutcome: "done", Kind: learning.KindRaw,
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// itoa spells a small index without pulling strconv into every fixture.
func itoa(i int) string { return strconv.Itoa(i) }

// episodeID is unique across a parallel run: the table's primary key is the
// id, and two cases writing "ep-0" would silently share a row.
var episodeIDs atomic.Int64

func episodeID() string { return "ep-" + strconv.FormatInt(episodeIDs.Add(1), 10) }
