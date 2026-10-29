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
	// JobReview, JobSubagent, JobAuxiliary and JobJudge are a seat's
	// satellites, written flat (`llm_review`) or inside the `llm` mapping
	// (`llm.review`).
	JobReview    ProviderJob = "review"
	JobSubagent  ProviderJob = "subagent"
	JobAuxiliary ProviderJob = "auxiliary"
	JobJudge     ProviderJob = "judge"
	// JobAssistant is `assistant.llm`, the Dashboard Assistant's chain.
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
	// or ["assistant", "llm"].
	Segments Path `json:"segments"`
	// Chain is the whole chain the field writes, in its order, the key
	// asked about included.
	Chain []string `json:"chain"`
}

// ProviderReferences is every field of the document that names the
// providers.llm entry key, in document order: each seat's `llm` and its
// satellites, at any depth, then `assistant.llm` and
// `assistant.deep_work.llm`.
//
// THE WRITTEN FIELDS, NOT THE RESOLVED CHAINS. A satellite written flat wins
// over the same phase inside the `llm` mapping, so the mapping's chain never
// runs; it is still in the document, still what a removal has to edit, and
// still a typo [Company.validateProviderKeys] reports where it was written.
// Which entry a job RUNS on is resolution's question, answered once by
// phase.ResolveWithSource over the organization this document builds. A field
// is reported whether or not the assistant is enabled: a removal that left a
// disabled assistant naming a missing entry would leave the next person who
// turns it on a refusal they did not cause.
//
// THE MAPPING'S default IS WRITTEN AS `llm` when it is the mapping's only
// phase, and as `llm.default` beside another: the document is stored in that
// form ([PhaseLLM.MarshalYAML]), so that is the field an edit finds.
//
// ONE IMPLEMENTATION with every reader: [Company.validateProviderKeys], the
// removal of an entry, and the account of which seats use one.
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
	add("", JobAssistant, field("assistant.llm"), c.Assistant.LLM)
	if deep := c.Assistant.DeepWork; deep != nil && deep.LLM != "" {
		add("", JobDeepWork, field("assistant.deep_work.llm"), []string{deep.LLM})
	}
	return out
}
