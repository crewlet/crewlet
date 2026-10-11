/**
 * Who each question and each push is served to (ADR-0031): the engine's own
 * classification, declared here so the dashboard can hold its two shells to
 * it.
 *
 * ONE WITHHOLDING RULE decides it, the engine's: a member reads what the
 * company PUBLISHED — its work, its pages, its chart, who is working on what —
 * and an admin also reads what the machine PROCESSED (prompts, transcripts,
 * diaries, events, spend) and how it is RUN (nodes, keys, configuration). A
 * reach is `open` (anybody who reaches the port), `public` (the company's
 * public face), `member` or `admin`.
 *
 * WHY THE DASHBOARD CARRIES A COPY: the person shell is a property, not a
 * hope — every screen reachable from a person-shell chunk asks only what a
 * member is served (`app/shell.test.ts` walks the import graph and refuses
 * anything above `member`). That gate needs the engine's answer for every
 * name, and the engine owns it, so each table is held against the engine
 * both ways by ONE Go test: `QUESTION_REACH` by `internal/api/queries`'s
 * `TestTheDashboardKnowsEveryQuestionsReach`, over the registry every seam
 * registers, and `PUSH_REACH` by `internal/api/stream`'s
 * `TestTheDashboardKnowsEveryPushKindsReach`, over every broadcast kind. A
 * question the engine moves to `admin` turns a person screen that asks it red
 * here, before a member's tab is refused it.
 *
 * The act tools' reach is `ACTIONS`' own `reach` column (`actions.ts`), held
 * there by the same idiom.
 */

import type { Reach } from "./access.ts";
import type { PushKind } from "./wire.ts";

/** Every question the engine registers, and the reach it answers it at. */
export const QUESTION_REACH = {
  a2a_channels: "admin",
  access: "admin",
  agent: "admin",
  agent_episode: "admin",
  agent_memory: "admin",
  backups: "admin",
  budgets: "admin",
  colleague: "member",
  company_feed: "member",
  config: "admin",
  config_audit: "admin",
  config_diff: "admin",
  config_entities: "admin",
  containers: "member",
  conversations: "admin",
  credential_pool: "admin",
  decisions: "member",
  event: "admin",
  event_series: "admin",
  events: "admin",
  fleet: "admin",
  fleet_broker: "admin",
  integrations: "admin",
  knowledge: "member",
  live_call: "admin",
  mcp_servers_status: "admin",
  memory_overview: "admin",
  page: "member",
  page_activity: "member",
  page_reads: "admin",
  page_revision: "member",
  pages: "member",
  phases: "admin",
  retention: "admin",
  sandbox_runs: "admin",
  sandbox_tail: "admin",
  schedule_runs: "admin",
  schedules: "member",
  seat_activity: "member",
  token_series: "admin",
  tokens: "admin",
  trace: "admin",
  turn: "admin",
  turn_stories: "member",
  turn_story: "member",
  turns: "admin",
  viewer: "open",
  work_activity: "member",
  work_catalogue: "member",
  work_comments: "member",
  work_files: "member",
  work_flow: "member",
  work_inbox: "member",
  work_item: "member",
  work_items: "member",
  work_my_work: "member",
  work_person: "member",
  work_project: "member",
  work_projects: "member",
  work_routing: "member",
  work_saved_views: "member",
  work_search: "member",
  work_views: "member",
  work_workload: "member",
} as const satisfies Record<string, Reach>;

/**
 * Every kind the engine BROADCASTS, and the lowest reach it is pushed to — a
 * snapshot, a query's answer and the pong are sent to one client and have no
 * row. A kind pushed to a member may still carry less for one (the `agents`
 * row without its live call, the `budget` state without its figures): the
 * richer variant is the admin's, and which slice a screen reads it from is
 * `protocol/store.ts`'s `SLICES`.
 */
export const PUSH_REACH = {
  event: "admin",
  agents: "member",
  seats: "member",
  sandboxes: "admin",
  tokens: "admin",
  budget: "member",
  schedules: "member",
  org: "public",
  tools: "admin",
  health: "open",
} as const satisfies Partial<Record<PushKind, Reach>>;
