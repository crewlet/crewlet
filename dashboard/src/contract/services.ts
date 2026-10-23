/**
 * The tracker a company runs: the engine's own (`worklet`), Jira, or none.
 * COPIES of `config.TrackerBackends`, held to them both ways by
 * `internal/api`'s `TestTheDashboardKnowsExactlyTheServicesTheEngineSends`.
 */
type TrackerBackend = "worklet" | "jira" | "none";

/**
 * The knowledge base a company runs: the engine's own (`doclet`), Confluence,
 * or none. COPIES of `config.KnowledgeBackends`, held by the same gate.
 */
type KnowledgeBackend = "doclet" | "confluence" | "none";

/**
 * The chat a company runs: Chatlet (`chatlet`), a vendor's (`vendor`,
 * the Mattermost or Slack it declares, either or both), or none. COPIES of
 * `config.ChatBackends`, held by the same gate. A POSTURE rather than a
 * product: which vendors, a reader holding Chatlet is told on the `viewer`
 * answer (`chat_vendors`).
 */
type ChatBackend = "chatlet" | "vendor" | "none";

/**
 * Which tracker, knowledge base and chat the company runs, as the ENGINE
 * resolved them: `services` on the org projection (`api.OrgServices` in Go),
 * carried by the socket snapshot's `org`, every `org` push and `GET /org`, to
 * every reader. A backend the document leaves empty is resolved there (the
 * vendor whose block is connected, else the engine's own), so a screen never
 * derives it again.
 *
 * WHICH OF THE DASHBOARD'S OWN SURFACES EXIST: a tracker other than `worklet`
 * means the company has no Worklet, a knowledge base other than `doclet` that
 * it has no Doclet, a chat other than `chatlet` that it has no Chatlet,
 * and every question about the one it lacks is refused. Absent from the
 * projection only on `{}`, a node running no company, which is "not known yet"
 * and never "none".
 */
export interface OrgServices {
  tracker: TrackerBackend;
  knowledge: KnowledgeBackend;
  chat: ChatBackend;
}
