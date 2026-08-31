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
