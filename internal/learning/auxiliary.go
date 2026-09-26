package learning

import (
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// Attribution is whom an auxiliary completion serves: the worker asking, and —
// where the work answers one — the turn it answers.
//
// It exists so that spend can be ATTRIBUTED. The engine records every
// completion made through a model it resolved for a learning worker or the
// prefetch (internal/engine's learningbudget.go), and a record that said only
// "the auxiliary model spent this on this seat" could not tell a company whose
// profiler is expensive from one whose skill synthesis is. So the caller says
// who it is, explicitly, at the one point every auxiliary call passes through
// ([auxiliary]) — never through the context, which a goroutine shares with
// whatever captured it.
type Attribution struct {
	// Worker is the asking worker's name: a reflection worker's [Worker.Name],
	// or one of the names below for the work that is not a reflection worker.
	Worker string

	// TurnID and WorkKey name the turn the call serves, and are empty for a
	// background pass, which serves none.
	TurnID  string
	WorkKey string
}

// The names of the auxiliary callers that are not reflection workers — a
// reflection worker's calls are recorded under its [Worker.Name].
const (
	// ClusterWorker is the skill synthesizer's clustering pass: a background
	// pass over a seat's recent episodes, drafting a skill from a repeated
	// procedure rather than from one turn.
	ClusterWorker = "skill_clustering"

	// PromotionWorker is the skill promotion pass, which lifts a procedure
	// several seats of a unit learned into the unit's knowledge base.
	PromotionWorker = "skill_promotion"

	// CompactionWorker is the episode lifecycle's compaction pass, which
	// folds a cluster of a seat's old episodes into one summary.
	CompactionWorker = "episode_compaction"

	// PrefetchWorker is the turn-start context assembly: the episode
	// briefing, the knowledge query and the memory filter a turn is given
	// before it starts.
	PrefetchWorker = "prefetch"

	// RecallWorker is the same assembly re-run on demand from inside a turn,
	// by the tools that re-filter a seat's memory and search its episodes.
	RecallWorker = "recall"
)

// Attributing is a [Models] that records the spend of every completion made
// through a member it resolved, and so has to be told whom each resolution
// serves.
//
// A SEPARATE INTERFACE rather than a second argument to Head, because Models is
// deliberately the phase registry's own signature: *phase.Registry satisfies it
// as written, so what a worker's model IS is decided in one place, the one the
// config validator checks. For binds the attribution and nothing else — the
// Models it returns resolves exactly as its receiver does — so there is no
// second opinion about which model a seat's cheap work runs on.
type Attributing interface {
	Models
	For(who Attribution) Models
}

// auxiliary resolves role's auxiliary model for the work who names.
//
// A WORKER RESOLVES ITS AUXILIARY MODEL HERE, never through Head: this is how a
// Models that records spend is told whom each call serves, and a call resolved
// through Head directly is recorded as nobody's. A Models that records nothing
// — the bare registry a test hands in — is used as it is.
func auxiliary(models Models, role *org.Role, who Attribution) (chain.Member, error) {
	if attributing, ok := models.(Attributing); ok {
		models = attributing.For(who)
	}
	return models.Head(role, phase.Auxiliary)
}

// attribution is this turn's attribution for worker.
func (t Turn) attribution(worker string) Attribution {
	return Attribution{Worker: worker, TurnID: t.Event.TurnID, WorkKey: t.WorkKey()}
}
