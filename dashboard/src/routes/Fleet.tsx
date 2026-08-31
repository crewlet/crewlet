/**
 * The fleet: which node holds which seat, and whether every node is running
 * the configuration the fleet has agreed on.
 *
 * This screen is read when nodes are dying, which is exactly when the API
 * answering it is least reliable — so it is the one screen that must SAY when
 * its own poll failed rather than showing the last reading as if it were now.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Badge, Empty, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtDuration, relTime, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { FleetNode } from "~/protocol/index.ts";

/**
 * The lease table has no push behind it, so it polls — at 15 seconds, chosen
 * against the lease TTL rather than picked: a poll slower than the TTL would
 * show a node as alive after its lease had already expired somewhere else.
 */
const POLL_MS = 15_000;

const STATUS_TONE: Record<string, "positive" | "caution" | "critical" | "neutral"> = {
  ok: "positive",
  degraded: "caution",
  error: "critical",
};

export function Fleet() {
  const now = useNow();
  const { data, loading, error } = useQuery("fleet", undefined, { pollMs: POLL_MS });

  const nodes = data?.nodes ?? [];
  // A node is behind when it has APPLIED an older epoch than the one the
  // activation pointer names. Epoch 0 means it has not reported at all, which
  // is a different thing from being behind and is called out on the row.
  const behind = nodes.filter((n) => data && (n.config_epoch ?? 0) < data.target_epoch);

  return (
    <>
      <ScreenHead
        title="Fleet"
        sub="Seat ownership is a lease with a fencing epoch — no two nodes ever run one seat. A node that cannot reach the configuration it should be running releases its seats rather than serving stale work."
        badges={
          <>
            <Badge outline>{plural(nodes.length, "node")}</Badge>
            {data?.this_node && <Badge tone="accent">you are on {data.this_node}</Badge>}
          </>
        }
      />

      {error && (
        <div className="banner critical">
          <Icon name="alert" size="sm" />
          <span>
            This poll failed ({error}). What is below is the last reading that succeeded — on this
            screen above all, do not read it as now.
          </span>
        </div>
      )}

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat
            icon="server"
            label="Live nodes"
            value={nodes.length}
            sub="holding an unexpired lease"
          />
          <Stat
            icon="users"
            label="Seats placed"
            value={data?.seats?.length ?? 0}
            sub={
              data?.unmanned_roles?.length
                ? `${data.unmanned_roles.length} role(s) with no seat running`
                : "every role has a home"
            }
          />
          <Stat
            icon="alert"
            label="Unplaceable"
            value={data?.unplaceable?.length ?? 0}
            sub={
              data?.unplaceable?.length
                ? "a placement constraint cannot be satisfied"
                : "nothing is stranded"
            }
          />
          <Stat
            icon="sliders"
            label="Behind on config"
            value={behind.length}
            sub={data?.target_epoch ? `target epoch ${data.target_epoch}` : "no target epoch"}
          />
        </StatRow>
      </Panel>

      {loading && !data && <Skeleton rows={4} />}
      <QueryState
        error={null}
        loading={loading}
        empty={
          nodes.length
            ? undefined
            : {
                title: "No nodes are reporting",
                hint: "A single-node company with local coordination has no lease table to read — this screen is for a fleet.",
              }
        }
      >
        <Panel title="Nodes" icon="server" count={nodes.length} padding="none">
          <DataTable<FleetNode>
            rows={nodes}
            rowKey={(n) => n.id}
            defaultSort={{ key: "id", dir: "asc" }}
            isFailed={(n) => n.config_status === "error"}
            columns={[
              {
                key: "id",
                header: "Node",
                sortValue: (n) => n.id,
                cell: (n) => (
                  <span className="row gap-1">
                    <code className="inline">{n.id}</code>
                    {n.id === data?.this_node && <Badge tone="accent">this one</Badge>}
                    {n.draining && <Badge tone="caution">draining</Badge>}
                  </span>
                ),
              },
              {
                key: "roles",
                header: "Duties",
                sortValue: (n) => n.roles.join(","),
                cell: (n) => (
                  <span className="row wrap gap-1">
                    {n.roles.map((r) => (
                      <Badge key={r} outline>
                        {r}
                      </Badge>
                    ))}
                  </span>
                ),
              },
              {
                key: "seats",
                header: "Seats",
                align: "right",
                sortValue: (n) => n.seats,
                // A COUNT on the node row; WHICH seats is the placement table
                // below, which is the one that can name them.
                cell: (n) => n.seats,
              },
              {
                key: "inflight",
                header: "In flight",
                align: "right",
                sortValue: (n) => n.in_flight ?? 0,
                cell: (n) => n.in_flight ?? 0,
              },
              {
                key: "posture",
                header: "Posture",
                shrink: true,
                sortValue: (n) => n.posture ?? "",
                cell: (n) =>
                  n.posture ? (
                    <Badge tone={n.posture === "serve" ? "positive" : "caution"}>{n.posture}</Badge>
                  ) : (
                    <span className="faint">—</span>
                  ),
              },
              {
                key: "config",
                header: "Config",
                shrink: true,
                sortValue: (n) => n.config_status ?? "",
                cell: (n) => (
                  <span className="row gap-1">
                    <Badge tone={STATUS_TONE[n.config_status ?? ""] ?? "neutral"}>
                      {n.config_status || "unknown"}
                    </Badge>
                    {data && (n.config_epoch ?? 0) < data.target_epoch && (
                      <span
                        className="t-caption faint"
                        title={`applied epoch ${n.config_epoch ?? 0}, target ${data.target_epoch}`}
                      >
                        behind
                      </span>
                    )}
                  </span>
                ),
              },
              {
                key: "lease",
                header: "Lease",
                shrink: true,
                sortValue: (n) => n.expires_in ?? 0,
                cell: (n) =>
                  n.expires_in != null ? (
                    <span className="t-num t-caption" title="time until this node's lease expires">
                      {fmtDuration(n.expires_in * 1000)}
                    </span>
                  ) : (
                    <span className="faint">—</span>
                  ),
              },
              {
                key: "up",
                header: "Up since",
                shrink: true,
                sortValue: (n) => n.started_at ?? "",
                cell: (n) =>
                  n.started_at ? (
                    <span className="t-caption" title={fmtDateTime(n.started_at)}>
                      {relTime(n.started_at, now)}
                    </span>
                  ) : (
                    <span className="faint">—</span>
                  ),
              },
            ]}
          />
        </Panel>

        {nodes.some((n) => n.config_error) && (
          <Panel title="Config apply errors" icon="alert">
            <div className="col gap-2">
              {nodes
                .filter((n) => n.config_error)
                .map((n) => (
                  <div key={n.id} className="banner critical">
                    <Icon name="alert" size="sm" />
                    <span>
                      <code className="inline">{n.id}</code> — {n.config_error}
                    </span>
                  </div>
                ))}
            </div>
          </Panel>
        )}

        <div className="grid grid-auto-lg">
          <Panel
            title="Seat placement"
            icon="users"
            count={data?.seats?.length ?? 0}
            padding="none"
          >
            <DataTable
              rows={data?.seats ?? []}
              rowKey={(s) => s.handle}
              defaultSort={{ key: "handle", dir: "asc" }}
              empty={{ title: "No seats are leased" }}
              columns={[
                {
                  key: "handle",
                  header: "Seat",
                  sortValue: (s) => s.handle,
                  cell: (s) => <SeatChip name={s.handle} handle={s.handle} />,
                },
                {
                  key: "node",
                  header: "Held by",
                  sortValue: (s) => s.node,
                  // The NODE, not the lease's `owner` — that is the fencing
                  // token (a node id plus a per-process suffix), and showing
                  // it here would make one node look like several across a
                  // restart.
                  cell: (s) => <code className="inline">{s.node}</code>,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  shrink: true,
                  sortValue: (s) => s.expires_in ?? 0,
                  cell: (s) =>
                    s.expires_in != null ? (
                      fmtDuration(s.expires_in * 1000)
                    ) : (
                      <span className="faint">—</span>
                    ),
                },
              ]}
            />
          </Panel>

          <Panel
            title="Company-wide duties"
            icon="cpu"
            count={data?.duties?.length ?? 0}
            padding="none"
          >
            <DataTable
              rows={data?.duties ?? []}
              rowKey={(d) => d.duty}
              defaultSort={{ key: "duty", dir: "asc" }}
              empty={{
                title: "No singleton duties are leased",
                hint: "The retention sweep and the scheduler are fleet singletons — exactly one node runs each.",
              }}
              columns={[
                { key: "name", header: "Duty", sortValue: (d) => d.duty, cell: (d) => d.duty },
                {
                  key: "node",
                  header: "Held by",
                  sortValue: (d) => d.node,
                  cell: (d) => <code className="inline">{d.node}</code>,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  shrink: true,
                  sortValue: (d) => d.expires_in ?? 0,
                  cell: (d) =>
                    d.expires_in != null ? (
                      fmtDuration(d.expires_in * 1000)
                    ) : (
                      <span className="faint">—</span>
                    ),
                },
              ]}
            />
          </Panel>
        </div>

        {(data?.unplaceable?.length || data?.unmanned_roles?.length) && (
          <Panel title="Not running anywhere" icon="alert">
            <div className="col gap-2">
              {data?.unmanned_roles?.map((r) => (
                <div key={r} className="banner caution">
                  <Icon name="user" size="sm" />
                  <span>
                    <strong>{r}</strong> has no seat running on any node. Work published to its
                    mailbox waits there — a durable subscription retains it — but nothing is
                    consuming it.
                  </span>
                </div>
              ))}
              {data?.unplaceable?.map((u) => (
                <div key={u.handle} className="banner caution">
                  <Icon name="target" size="sm" />
                  <span>
                    <strong>{u.handle}</strong> cannot be placed
                    {u.placement ? ` — it is pinned to ${u.placement}` : ""}
                    {u.reason ? `: ${u.reason}` : ", and no live node satisfies its constraint."}
                  </span>
                </div>
              ))}
            </div>
          </Panel>
        )}
      </QueryState>
    </>
  );
}
