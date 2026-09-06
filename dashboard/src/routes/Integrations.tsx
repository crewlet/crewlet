/**
 * The surfaces agents work on, and whether traffic is actually arriving.
 *
 * Every count here is THREE-VALUED — a number, zero, or `null` meaning "this
 * process cannot say". They are not the same fact: a webhook route with zero
 * deliveries is configured and quiet; one this node cannot count is a node
 * that has not been serving ingress. Collapsing them is how a broken
 * integration comes to look healthy.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Empty, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import type { ReconcileStatus } from "~/protocol/types.ts";

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
    <div className="col gap-2">
      <div className="row wrap gap-2" style={{ alignItems: "center" }}>
        <Badge tone={phaseTone(status.phase)} dot>
          {status.phase.replace(/_/g, " ")}
        </Badge>
        {actor && <span className="t-caption faint">{actor}</span>}
      </div>

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

function Count({ value, label }: { value: number | null | undefined; label: string }) {
  if (value == null) {
    return (
      <span
        className="t-caption faint"
        title="this process cannot answer — it is not serving ingress"
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

export function Integrations() {
  // Traffic counters are not pushed, and they move slowly — a minute is the
  // right cadence for "is anything arriving at all".
  const { data, loading, error } = useQuery("integrations", undefined, { pollMs: 60_000 });

  const rows = data?.integrations ?? [];
  const configured = rows.filter((r) => r.configured);

  return (
    <>
      <ScreenHead
        title="Integrations"
        sub="Where the company's work comes from and where its output goes. Each agent acts as itself on these surfaces, with its own credentials."
        badges={
          <Badge outline>
            {configured.length} of {rows.length} configured
          </Badge>
        }
      />

      {data && data.traffic_known === false && (
        <div className="banner neutral">
          <Icon name="info" size="sm" />
          <span>
            This node is not counting traffic — it is not serving the ingress role, so the numbers
            below are unknown rather than zero.
          </span>
        </div>
      )}

      <Panel padding="none">
        <StatRow cols={3}>
          <Stat
            icon="plug"
            label="Configured"
            value={configured.length}
            sub={`of ${rows.length} supported`}
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
          <Stat
            icon="send"
            label="Outbound"
            value={
              data?.traffic_known
                ? rows.reduce((n, r) => n + (r.outbound ?? 0), 0).toLocaleString()
                : "unknown"
            }
            sub="messages the engine sent on a seat's behalf"
          />
        </StatRow>
      </Panel>

      {loading && !data && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "No integrations are known",
                hint: "The engine reports the six it serves once a company configuration is active.",
              }
        }
      >
        <div className="grid grid-auto">
          {rows.map((row) => (
            <Panel
              key={row.key}
              title={row.label || row.key}
              icon={row.configured ? "plug" : "power"}
              actions={
                row.configured ? (
                  <Badge tone="positive" dot>
                    configured
                  </Badge>
                ) : (
                  <Badge outline>not configured</Badge>
                )
              }
            >
              <div className="col gap-2">
                {row.detail && <span className="t-caption">{row.detail}</span>}
                {!row.configured && (
                  <span className="t-caption faint">
                    Nothing routes here. A webhook route with no secret has nothing to verify with
                    and answers 503 rather than accepting a delivery.
                  </span>
                )}
                <div className="row wrap gap-3">
                  <Count value={row.inbound} label="in" />
                  <Count value={row.outbound} label="out" />
                  <Count value={row.routed} label="routed to a seat" />
                </div>
                <Reconcile status={row.reconcile} />
              </div>
            </Panel>
          ))}
        </div>
        {!configured.length && rows.length > 0 && (
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
