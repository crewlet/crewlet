/**
 * The integrations room's answer (`integrations`), as the engine's query
 * surface builds it: a row for every surface the company document declares,
 * its traffic counted from the deliveries in the event log, the reconcile
 * loop's record where a pass has reported, and one card per tool rolled up
 * from its surfaces.
 *
 * DERIVED, NOT KEPT. Nothing here is stored beside the company: a row exists
 * because the document has the block, its counts are the delivery rows in the
 * window, and its state is the loop's record (`loop.ts`), so a write anywhere
 * moves this answer as it moves the engine's.
 */

import type {
  IntegrationRow,
  IntegrationsAnswer,
  ReconcileStatus,
} from "~/contract/integrations.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { agentRoles } from "./derive.ts";
import { verdictOf } from "./findings.ts";
import { blockOf, outcomeOf, publicBase, type DemoSurfaceState } from "./loop.ts";
import { usable } from "./refs.ts";
import { phaseLabel, reported, rollup, type SurfaceStatus } from "./rollup.ts";
import { deliversAs, PASS_KINDS, rowInboundPath, TOOLS } from "./vendors.ts";

/** How long the event log keeps a delivery, which bounds the traffic window (`store.EventHistory`). */
const EVENT_HISTORY_MS = 30 * 86_400_000;
/** The rows one listing of the event log reads (`queries.MaxEventPage`). */
const MAX_EVENT_PAGE = 400;

type Block = Record<string, unknown>;

/** A loop record as the row reads it (`queries.reconcileRow`). */
function reconcileRow(state: DemoSurfaceState): ReconcileStatus {
  const r = reported(state);
  return {
    phase: r.phase,
    phase_label: phaseLabel(r.phase),
    actor: r.actor ?? "",
    detail: r.detail ?? "",
    action_url: r.action_url ?? "",
    disconnecting: Boolean(state.disconnecting || state.phase === "disconnecting"),
    outcome: outcomeOf(state.phase, state.actor),
    attempts: state.attempts,
    last_error: state.last_error ?? "",
    findings: state.findings.map((f) => {
      const verdict = verdictOf(f.kind);
      return {
        kind: f.kind,
        subject: f.subject ?? "",
        detail: f.detail ?? "",
        action_url: f.action_url ?? "",
        phase: verdict.phase,
        actor: verdict.actor,
        ...(f.remedy ? { remedy: f.remedy } : {}),
        ...(f.subjects?.length ? { subjects: f.subjects } : {}),
        ...(f.expires_at ? { expires_at: f.expires_at } : {}),
      };
    }),
    last_attempt_at: state.last_attempt_at,
    settled_at: state.settled_at,
    next_attempt_at: state.next_attempt_at,
  } as ReconcileStatus;
}

/** The window the traffic counts are over, and the delivery rows in it (`queries.deliveryTraffic`). */
function traffic(c: DemoCompany, now: Date) {
  const listing = c.events.filter((e) => e.category === "webhook").slice(0, MAX_EVENT_PAGE);
  const more = c.events.filter((e) => e.category === "webhook").length > MAX_EVENT_PAGE;
  let since = new Date(now.getTime() - EVENT_HISTORY_MS);
  if (more) {
    const oldest = Math.min(...listing.map((e) => Date.parse(e.timestamp)));
    since = new Date(Math.min(oldest + 1, now.getTime()));
  }
  const count: Record<string, number> = {};
  const last: Record<string, string> = {};
  for (const row of listing) {
    const at = Date.parse(row.timestamp);
    if (at < since.getTime() || at >= now.getTime()) continue;
    const route = row.tags?.route ?? "";
    count[route] = (count[route] ?? 0) + 1;
    if (!last[route] || at > Date.parse(last[route]!)) last[route] = row.timestamp;
  }
  return { since: since.toISOString(), count, last };
}

/** The roles a surface reaches, by name (`queries.seatsFor`). */
function seatsFor(doc: CompanyDocument, kind: string): string[] {
  const names: string[] = [];
  for (const { role } of allRoles(doc)) {
    const own = (role as { integrations?: Block }).integrations;
    const carries =
      kind === "slack"
        ? Boolean(own?.slack)
        : kind === "mattermost"
          ? Boolean(own?.mattermost)
          : kind === "jira"
            ? Boolean(role.project)
            : kind === "confluence"
              ? Boolean(role.space)
              : kind === "github"
                ? Boolean(own?.github)
                : false;
    if (carries) names.push(role.name);
  }
  return names.sort();
}

/** Every role, people included, which the delivery rosters read (`Company.EachRole`). */
function allRoles(doc: CompanyDocument) {
  const roles: { role: NonNullable<CompanyDocument["roles"]>[number] }[] = [];
  for (const role of doc.roles ?? []) roles.push({ role });
  const walk = (units: CompanyDocument["units"]) => {
    for (const unit of units ?? []) {
      for (const role of unit.roles ?? []) roles.push({ role });
      walk(unit.children);
    }
  };
  walk(doc.units);
  return roles;
}

/** The delivery secrets a surface verifies with, from the document (`secret_present`'s inputs). */
function deliverySecrets(c: DemoCompany, kind: string, block: Block): unknown[] | null {
  const doc = documentOf(c);
  switch (kind) {
    case "slack":
      // Each agent's signing secret, where the company declares Slack.
      return agentRoles(doc).map(
        ({ role }) =>
          ((role as { integrations?: Block }).integrations?.slack as Block | undefined)
            ?.signing_secret,
      );
    case "github": {
      // An installed agent app's webhook secret, under an enabled company block, or the org's own.
      const apps = agentRoles(doc).map(({ role }) => {
        const app = (role as { integrations?: Block }).integrations?.github as Block | undefined;
        return app && Number(app.installation_id) && block.enabled === true
          ? app.webhook_secret
          : undefined;
      });
      return [...apps, block.webhook_secret];
    }
    case "gitlab":
      return [block.signing_secret];
    case "jira":
      return [block.webhook_secret];
    case "confluence":
      return [block.webhook_secret, block.webhook_token];
    case "datadog":
      return [block.webhook_token];
    default:
      return null;
  }
}

/** The integrations answer: a row per declared surface, a card per tool. */
export function integrationsAnswer(c: DemoCompany, now: Date): IntegrationsAnswer {
  const doc = documentOf(c);
  const seen = traffic(c, now);
  const base = publicBase(c);
  const rows: IntegrationRow[] = [];
  const statuses = new Map<string, SurfaceStatus>();
  const add = (
    kind: string,
    block: Block,
    enabled: boolean,
    detail: Record<string, unknown> = {},
  ) => {
    const outcomes = c.integrations.outcomes[kind] ?? { skipped: 0, coalesced: 0 };
    const secrets = deliverySecrets(c, kind, block)?.filter(
      (v) => typeof v === "string" && v.trim() !== "",
    );
    const secretPresent = secrets === undefined ? null : secrets.length > 0;
    const secretUsable =
      secretPresent === null ? null : secretPresent && secrets!.every((v) => usable(c, v));
    const routes = deliversAs(kind).length > 0 ? true : null;
    const state = c.integrations.states[kind] ?? null;
    const endpoint = state?.endpoint || null;
    const endpointCurrent = endpoint ? endpoint === base : null;
    const row: IntegrationRow = {
      key: kind,
      configured: true,
      enabled,
      inbound: seen.count[kind] ?? 0,
      skipped: outcomes.skipped,
      coalesced: outcomes.coalesced,
      inbound_kind: kind === "atlassian" ? null : kind === "mattermost" ? "websocket" : "webhook",
      inbound_path: kind === "atlassian" ? null : rowInboundPath(kind),
      last_at: seen.last[kind] ?? null,
      routes,
      secret_present: secretPresent,
      secret_usable: secretUsable,
      reconcile: state && reported(state).phase !== "" ? reconcileRow(state) : null,
      endpoint,
      endpoint_current: endpointCurrent,
      seats: seatsFor(doc, kind),
      ...detail,
    };
    rows.push(row);
    statuses.set(kind, {
      key: kind,
      enabled,
      state,
      converges: PASS_KINDS.includes(kind as never),
      secretUsable,
      routes,
      endpointCurrent,
    });
  };
  const block = (kind: string) => blockOf(doc, kind);
  const slack = block("slack");
  if (slack) add("slack", slack, true);
  const mattermost = block("mattermost");
  if (mattermost) add("mattermost", mattermost, true, { url: mattermost.url ?? "" });
  const github = block("github");
  if (github) add("github", github, github.enabled === true);
  const gitlab = block("gitlab");
  if (gitlab) add("gitlab", gitlab, gitlab.enabled === true, { url: gitlab.url ?? "" });
  const jira = block("jira");
  if (jira) add("jira", jira, true, { url: jira.url ?? jira.site_url ?? "" });
  const confluence = block("confluence");
  if (confluence) {
    add("confluence", confluence, true, {
      url: confluence.url ?? confluence.site_url ?? "",
      cloud_path: "/webhooks/confluence/{event}",
    });
  }
  const datadog = block("datadog");
  if (datadog) {
    add("datadog", datadog, datadog.enabled === true, {
      handle_tag: String(datadog.handle_tag ?? "").trim() || "crewlet",
      route_to: datadog.route_to ?? "",
    });
  }
  const atlassian = block("atlassian");
  if (atlassian) {
    add("atlassian", atlassian, true, {
      deployment:
        String(atlassian.deployment ?? "")
          .trim()
          .toLowerCase() === "data_center"
          ? "data_center"
          : "cloud",
      org_id: atlassian.org_id ?? "",
    });
  }
  const forge = (doc.integrations as Block | undefined)?.forge_app_id;
  if (typeof forge === "string" && forge.trim()) add("forge", {}, true, { app_id: forge });
  rows.sort((a, b) => a.key.localeCompare(b.key));
  return {
    integrations: rows,
    tools: TOOLS.map((tool) =>
      rollup(
        tool,
        tool.surfaces.map((s) => statuses.get(s.key)).filter((s): s is SurfaceStatus => Boolean(s)),
      ),
    ),
    traffic_known: true,
    traffic_since: seen.since,
    coverage: { nodes: [{ id: c.engine.health.node, answered: true, error: "" }], complete: true },
  };
}
