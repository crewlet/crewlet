/**
 * The tracker a company runs: the engine's own (`native`), Jira, or none.
 * COPIES of `config.TrackerBackends`, held to them both ways by
 * `internal/api`'s `TestTheDashboardKnowsExactlyTheServicesTheEngineSends`.
 */
type TrackerBackend = "native" | "jira" | "none";

/**
 * The knowledge base a company runs: the engine's own (`native`), Confluence,
 * or none. COPIES of `config.KnowledgeBackends`, held by the same gate.
 */
type KnowledgeBackend = "native" | "confluence" | "none";

/**
 * Which tracker and knowledge base the company runs, as the ENGINE resolved
 * them: `services` on the org projection (`api.OrgServices` in Go), carried by
 * the socket snapshot's `org`, every `org` push and `GET /org`, to every
 * reader. A backend the document leaves empty is resolved there (the vendor
 * whose block is connected, else the engine's own), so a screen never derives
 * it again.
 *
 * WHICH OF THE DASHBOARD'S OWN SURFACES EXIST: anything other than `native`
 * means the company has no Crewlet tracker (or knowledge base), and every
 * question about it is refused. Absent from the projection only on `{}`, a
 * node running no company, which is "not known yet" and never "none".
 */
export interface OrgServices {
  tracker: TrackerBackend;
  knowledge: KnowledgeBackend;
}
