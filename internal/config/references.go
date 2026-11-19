package config

import (
	"slices"
)

// ProviderJob is the work a written providers.llm reference points at a model:
// a seat's executor or one of its satellites, or the assistant's chat or its
// deep work.
type ProviderJob string

const (
	// JobExecutor is a seat's `llm`: its executor's chain, and the chain
	// every satellite that names none falls back to.
	JobExecutor ProviderJob = "executor"
	// JobReview is a seat's reviewer chain, written flat (`llm_review`) or
	// inside the `llm` mapping (`llm.review`), as every satellite is.
	JobReview ProviderJob = "review"
	// JobSubagent is a seat's delegated workers' chain.
	JobSubagent ProviderJob = "subagent"
	// JobAuxiliary is a seat's side passes' chain (memory, summaries).
	JobAuxiliary ProviderJob = "auxiliary"
	// JobJudge is a seat's onboarding judge's chain.
	JobJudge ProviderJob = "judge"
	// JobAssistant is `assistant.model`, the model Ask Crewlet runs on.
	JobAssistant ProviderJob = "assistant"
	// JobDeepWork is `assistant.deep_work.llm`.
	JobDeepWork ProviderJob = "deep_work"
)

// ProviderJobs is the closed set.
var ProviderJobs = []ProviderJob{
	JobExecutor, JobReview, JobSubagent, JobAuxiliary, JobJudge, JobAssistant, JobDeepWork,
}

// Valid reports whether j is one of [ProviderJobs].
func (j ProviderJob) Valid() bool { return slices.Contains(ProviderJobs, j) }

// ProviderReference is one field of the document that names providers.llm
// entries: where it is written, whose it is, the job it points at a model,
// and the whole chain it writes.
type ProviderReference struct {
	// Seat is the handle of the seat that wrote it, and "" for the
	// assistant's two fields.
	Seat string `json:"seat"`
	// Job is the work the field points at a model.
	Job ProviderJob `json:"job"`
	// Segments is the WRITTEN field, as a problem's segments are:
	// ["units", 0, "roles", 2, "llm_review"], ["roles", 0, "llm", "review"]
	// or ["assistant", "model"].
	Segments Path `json:"segments"`
	// Chain is the whole chain the field writes, in its order, the key
	// asked about included.
	Chain []string `json:"chain"`
}

// ProviderReferences is every field of the document that names the
// providers.llm entry key, in document order: each seat's `llm` and its
// satellites, at any depth, then `assistant.model` and
// `assistant.deep_work.llm`.
//
// It reports the WRITTEN fields, not resolved chains: a flat satellite beats
// the same phase in the `llm` mapping, but the shadowed field is still what a
// removal must edit (phase.ResolveWithSource says what a job runs on). The
// mapping's default is `llm` when alone and `llm.default`
// beside another, the stored form ([PhaseLLM.MarshalYAML]).
func (c *Company) ProviderReferences(key string) []ProviderReference {
	var out []ProviderReference
	for _, ref := range c.providerReferences() {
		if slices.Contains(ref.Chain, key) {
			out = append(out, ref)
		}
	}
	return out
}

// providerReferences is every written field that names providers.llm
// entries, whatever entries it names: [Company.ProviderReferences] before it
// is narrowed to one key. A field written empty is no reference.
func (c *Company) providerReferences() []ProviderReference {
	var out []ProviderReference
	add := func(seat string, job ProviderJob, path Path, chain []string) {
		if len(chain) == 0 {
			return
		}
		out = append(out, ProviderReference{Seat: seat, Job: job, Segments: path,
			Chain: slices.Clone(chain)})
	}
	for role, path := range c.EachRole() {
		seat := role.IdentityKey()
		llm := at(path, "llm")
		executor := llm
		if !role.LLM.onlyDefault() {
			executor = at(llm, "default")
		}
		add(seat, JobExecutor, executor, role.LLM.Default)
		add(seat, JobReview, at(llm, "review"), role.LLM.Review)
		add(seat, JobSubagent, at(llm, "subagent"), role.LLM.Subagent)
		add(seat, JobAuxiliary, at(llm, "auxiliary"), role.LLM.Auxiliary)
		add(seat, JobJudge, at(llm, "judge"), role.LLM.Judge)
		add(seat, JobReview, at(path, "llm_review"), role.LLMReview)
		add(seat, JobSubagent, at(path, "llm_subagent"), role.LLMSubagent)
		add(seat, JobAuxiliary, at(path, "llm_auxiliary"), role.LLMAuxiliary)
		add(seat, JobJudge, at(path, "llm_judge"), role.LLMJudge)
	}
	if c.Assistant.Model != "" {
		add("", JobAssistant, field("assistant.model"), []string{c.Assistant.Model})
	}
	if deep := c.Assistant.DeepWork; deep != nil && deep.LLM != "" {
		add("", JobDeepWork, field("assistant.deep_work.llm"), []string{deep.LLM})
	}
	return out
}
