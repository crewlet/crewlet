/**
 * The surfaces agents work on, and whether each one is actually working.
 *
 * Laid out the way the console's own Integrations page is: grouped by the
 * CAPABILITY a surface provides (messaging, tasks, code, knowledge,
 * observability), one bordered list per group, one row per surface with its
 * name and what it is for on the left and its state on the right. A surface
 * this build serves but this company has not configured is still listed, as
 * "not configured", so the reader sees the whole catalogue rather than only
 * the part they already set up.
 *
 * Identity is carried by name, icon and position, never by colour: a
 * vendor's own brand mark would spend a hue on "which integration", which is
 * the one thing the design system's rule forbids. Colour here means STATE
 * only: the reconcile phase, and whether traffic could be verified.
 *
 * Every count is THREE-VALUED (a number, zero, or `null` meaning "this
 * process cannot say"), and so is the reconcile block: `null` is a process
 * with no loop to ask or a surface the loop has not reached, and neither is a
 * claim that the surface is fine.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Empty, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import type { IntegrationRow, ReconcileStatus } from "~/protocol/types.ts";

/**
 * What agents need in order to work, in the order the console asks about
 * them. Each surface answers exactly one capability.
 */
const CAPABILITIES: { id: string; title: string; icon: IconName; description: string }[] = [
  {
    id: "messaging",
    title: "Messaging",
    icon: "message",
    description: "Where agents talk with you and with each other.",
  },
  {
    id: "tasks",
    title: "Task management",
    icon: "target",
    description: "Where work is planned, assigned and tracked.",
  },
  {
    id: "code",
    title: "Code",
    icon: "gitBranch",
    description: "Where agents commit, review and ship.",
  },
  {
    id: "knowledge",
    title: "Knowledge",
    icon: "book",
    description: "Where documentation and decisions live.",
  },
  {
    id: "observability",
    title: "Observability",
    icon: "activity",
    description: "Where agents watch production and respond.",
  },
];

/**
 * The catalogue: every surface this build serves, with the capability it
 * answers and one line on what it is for. The key matches the API row's, so
 * the two join by name.
 */
const CATALOG: {
  key: string;
  capability: string;
  name: string;
  description: string;
  icon: IconName;
}[] = [
  {
    key: "slack",
    capability: "messaging",
    name: "Slack",
    description: "One app per agent; threads and mentions wake seats",
    icon: "message",
  },
  {
    key: "mattermost",
    capability: "messaging",
    name: "Mattermost",
    description: "Self-hosted chat over an outbound websocket, no public URL needed",
    icon: "message",
  },
  {
    key: "jira",
    capability: "tasks",
    name: "Jira",
    description: "Issues routed by assignee, mention, watcher and project lead",
    icon: "target",
  },
  {
    key: "github",
    capability: "code",
    name: "GitHub",
    description: "Review requests, assignments and mentions",
    icon: "gitBranch",
  },
  {
    key: "gitlab",
    capability: "code",
    name: "GitLab",
    description: "Per-agent service accounts; merge requests and pipelines",
    icon: "gitBranch",
  },
  {
    key: "confluence",
    capability: "knowledge",
    name: "Confluence",
    description: "Page and comment events, and the knowledge search seats read",
    icon: "book",
  },
  {
    key: "forge",
    capability: "knowledge",
    name: "Forge relay",
    description: "Atlassian Cloud events relayed through the Forge app",
    icon: "link",
  },
  {
    key: "datadog",
    capability: "observability",
    name: "Datadog",
    description: "Monitor alerts routed by owner tag, with a fallback seat",
    icon: "activity",
  },
];

function Count({ value, label }: { value: number | null | undefined; label: string }) {
  if (value == null) {
    return (
      <span
        className="t-caption faint"
        title="this process cannot answer; it is not serving ingress"
      >
        {label}: unknown
      </span>
    );
  }
  return (
    <span className="t-caption t-num">
      {label}: {value.toLocaleString()}
    </span>
  );
}

/**
 * COLOUR CARRIES STATE, NEVER IDENTITY (see reference/dashboard-design.md), so
 * the tone follows the phase rather than the vendor.
 *
 * `ready` is the only positive one, and `degraded` is caution rather than
 * critical on purpose: agents are still working, which is exactly what
 * separates it from a surface that cannot authenticate at all.
 */
export function phaseTone(phase: string): "positive" | "caution" | "critical" | "info" | "neutral" {
  switch (phase) {
    case "ready":
      return "positive";
    case "degraded":
      return "caution";
    case "unconfigured":
      return "critical";
    case "awaiting_admin":
      return "info";
    case "provisioning":
    case "activating":
      return "neutral";
    default:
      // A phase a newer node wrote. Rendered as-is in a neutral tone
      // rather than guessed at: claiming a surface is fine on the
      // strength of a word this build cannot interpret is the one answer
      // that is certainly wrong.
      return "neutral";
  }
}

/** Who has to act, phrased for the person reading it. */
function actorLabel(actor: string | undefined): string {
  switch (actor) {
    case "engine":
      return "the engine is working on it";
    case "provider":
      return "the vendor is applying it";
    case "admin":
      return "you, at the vendor";
    case "operator":
      return "you, in the company configuration";
    default:
      return "";
  }
}

/**
 * What the reconcile loop last found for one surface.
 *
 * Renders NOTHING when the status is null, and the silence is the point: a
 * standalone API has no loop to ask and the loop may not have reached this
 * surface yet, and neither of those is a claim that the surface is healthy.
 * A green tick here would be exactly the invented health this screen has
 * always refused to show.
 */
export function Reconcile({ status }: { status: ReconcileStatus | null | undefined }) {
  if (!status) return null;

  const actor = actorLabel(status.actor);
  // The findings the phase was NOT derived from. The report says what to do
  // next and the findings say what is actually wrong, so an operator who
  // fixes the first should not wait a full pass to learn there was a second.
  const rest = (status.findings ?? []).slice(1);

  return (
    <div className="col gap-1">
      {/* The PHASE is not repeated here: the row's state tag on the right is
          its one home, and a second badge two lines below it read as two
          facts. What this block adds is who owes the next step. */}
      {actor && <span className="t-caption faint">{actor}</span>}

      {status.detail && <span className="t-caption">{status.detail}</span>}

      {status.action_url && (
        <a className="t-caption" href={status.action_url} target="_blank" rel="noreferrer">
          Open where this is fixed
        </a>
      )}

      {status.last_error && (
        <span className="t-caption faint" title="the last pass could not read this surface">
          Last pass failed: {status.last_error}
        </span>
      )}

      {rest.length > 0 && (
        <details>
          <summary className="t-caption faint">
            {rest.length} more finding{rest.length === 1 ? "" : "s"}
          </summary>
          <ul className="col gap-1" style={{ margin: "4px 0 0", paddingLeft: 16 }}>
            {rest.map((f, i) => (
              <li key={`${f.kind}:${f.subject ?? ""}:${i}`} className="t-caption">
                {f.detail || `${f.kind.replace(/_/g, " ")}${f.subject ? `: ${f.subject}` : ""}`}
              </li>
            ))}
          </ul>
        </details>
      )}

      <span className="t-caption faint">
        {status.settled_at ? `settled ${fmtDateTime(status.settled_at)}` : "not settled yet"}
        {status.next_attempt_at ? ` · next check ${fmtDateTime(status.next_attempt_at)}` : ""}
      </span>
    </div>
  );
}

/**
 * The state tag on the right of a row, and the one place a row spends colour.
 *
 * The reconcile phase wins when the loop has one, because it is a measured
 * claim. Without one the tag says only what the config says: configured, or
 * paused, or absent. "Connected" is deliberately not a word used here; a
 * configured surface whose deliveries are all refused is not connected, and
 * the counts beside the tag are what say so.
 */
function StateTag({ row }: { row: IntegrationRow | undefined }) {
  if (!row) {
    return <Badge outline>not configured</Badge>;
  }
  if (row.reconcile) {
    return (
      <Badge tone={phaseTone(row.reconcile.phase)} dot>
        {row.reconcile.phase.replace(/_/g, " ")}
      </Badge>
    );
  }
  if (row.enabled === false) {
    return <Badge outline>paused</Badge>;
  }
  return (
    <Badge tone="neutral" dot>
      configured
    </Badge>
  );
}

/**
 * The second line of a configured row: whether a delivery could be verified
 * and routed, which are the two ways a configured surface is silently not
 * working, and the counts that say whether anything is arriving.
 */
function RowFacts({ row }: { row: IntegrationRow }) {
  const facts: { label: string; tone: "positive" | "caution" | "neutral"; title: string }[] = [];
  if (row.secret_usable === false) {
    facts.push({
      label: "secret unresolved",
      tone: "caution",
      title:
        "the config names a secret whose ${VAR} resolved to nothing, so every delivery is refused",
    });
  }
  if (row.routes === false) {
    facts.push({
      label: "routes nowhere",
      tone: "caution",
      title: "deliveries are verified and stored, and no parser turns them into work for a seat",
    });
  }
  return (
    <div className="row wrap gap-2" style={{ alignItems: "center" }}>
      {facts.map((f) => (
        <Badge key={f.label} tone={f.tone} outline title={f.title}>
          {f.label}
        </Badge>
      ))}
      {/*
        THE THREE THE API ACTUALLY ANSWERS WITH. An earlier revision rendered
        `outbound` and `routed`, which GET /integrations has never emitted, so
        both read "unknown" on every row forever, while the two counts that DO
        arrive, and that the endpoint documents as the ones that make
        `inbound` mean anything, were dropped on the floor.
      */}
      <Count value={row.inbound} label="in" />
      <Count value={row.skipped} label="dropped" />
      <Count value={row.coalesced} label="merged" />
      {typeof row.inbound_path === "string" && row.inbound_path && (
        <code className="inline t-caption faint">{String(row.inbound_path)}</code>
      )}
    </div>
  );
}

export function Integrations() {
  // Traffic counters are not pushed, and they move slowly; a minute is the
  // right cadence for "is anything arriving at all".
  const { data, loading, error } = useQuery("integrations", undefined, { pollMs: 60_000 });

  const rows = data?.integrations ?? [];
  const byKey = new Map(rows.map((r) => [r.key, r]));
  const configured = rows.filter((r) => r.configured);
  const attention = rows.filter(
    (r) =>
      r.reconcile &&
      r.reconcile.phase !== "ready" &&
      r.reconcile.actor &&
      r.reconcile.actor !== "engine",
  );

  return (
    <>
      <ScreenHead
        title="Integrations"
        sub="Where the company's work comes from and where its output goes. Each agent acts as itself on these surfaces, with its own credentials."
        badges={
          <Badge outline>
            {configured.length} of {CATALOG.length} configured
          </Badge>
        }
      />

      {data && data.traffic_known === false && (
        <div className="banner neutral">
          <Icon name="info" size="sm" />
          <span>
            This node is not counting traffic; it is not serving the ingress role, so the numbers
            below are unknown rather than zero.
          </span>
        </div>
      )}

      {attention.length > 0 && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span>
            {attention.length === 1 && attention[0]
              ? `${attention[0].key} needs somebody: ${attention[0].reconcile?.detail ?? ""}`
              : `${attention.length} integrations need somebody to act. Each one says who and what below.`}
          </span>
        </div>
      )}

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat
            icon="plug"
            label="Configured"
            value={configured.length}
            sub={`of ${CATALOG.length} this build serves`}
          />
          <Stat
            icon="download"
            label="Inbound"
            value={
              data?.traffic_known
                ? rows.reduce((n, r) => n + (r.inbound ?? 0), 0).toLocaleString()
                : "unknown"
            }
            sub={data?.traffic_since ? `since ${fmtDateTime(data.traffic_since)}` : ""}
          />
          {/*
            WHAT BECAME OF IT, which is the half "Inbound" cannot answer on its
            own: 128 arrived tells a working integration and one whose every
            delivery reaches nobody apart not at all. This tile once summed
            `outbound`, a field GET /integrations does not return, so it read
            "0 messages the engine sent" on a company sending thousands.
          */}
          <Stat
            icon="filter"
            label="Dropped"
            value={
              data?.traffic_known
                ? rows.reduce((n, r) => n + (r.skipped ?? 0), 0).toLocaleString()
                : "unknown"
            }
            sub="deliveries the routing gate woke nobody for"
          />
          <Stat
            icon="layers"
            label="Merged"
            value={
              data?.traffic_known
                ? rows.reduce((n, r) => n + (r.coalesced ?? 0), 0).toLocaleString()
                : "unknown"
            }
            sub="bursts on one conversation that became one turn"
          />
        </StatRow>
      </Panel>

      {loading && !data && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading} empty={undefined}>
        {CAPABILITIES.map((cap) => {
          const entries = CATALOG.filter((c) => c.capability === cap.id);
          return (
            <Panel
              key={cap.id}
              title={cap.title}
              subtitle={cap.description}
              icon={cap.icon}
              padding="none"
            >
              <div className="list">
                {entries.map((entry) => {
                  const row = byKey.get(entry.key);
                  return (
                    <div
                      key={entry.key}
                      className={row ? "list-row int-row" : "list-row int-row int-row-absent"}
                    >
                      <span className="int-row-icon" aria-hidden>
                        <Icon name={entry.icon} size="md" />
                      </span>
                      <div className="int-row-text">
                        <span className="int-row-name">{entry.name}</span>
                        <span className="int-row-desc">{entry.description}</span>
                        {row?.detail && <span className="t-caption faint">{row.detail}</span>}
                        {row && <RowFacts row={row} />}
                        {row && <Reconcile status={row.reconcile} />}
                      </div>
                      <div className="int-row-state">
                        <StateTag row={row} />
                      </div>
                    </div>
                  );
                })}
              </div>
            </Panel>
          );
        })}
        {data && configured.length === 0 && (
          <Empty
            icon="plug"
            title="No integration is connected yet"
            hint="Until one is, the only thing that can wake a seat is a schedule. Connect a chat surface, a tracker or a code host in the company configuration."
          />
        )}
      </QueryState>
    </>
  );
}
