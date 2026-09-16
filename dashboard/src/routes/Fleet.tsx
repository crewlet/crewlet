/**
 * The fleet: which node holds which seat, and whether every node is running
 * the configuration the fleet has agreed on.
 *
 * This screen is read when nodes are dying, which is exactly when the API
 * answering it is least reliable — so it is the one screen that must SAY when
 * its own poll failed rather than showing the last reading as if it were now.
 */

import { QueryState, RecordTable, SeatChip } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtDuration, plural } from "~/lib/format.ts";
import type { FleetNode } from "~/protocol/index.ts";
import { RetentionPanels } from "./Retention.tsx";
import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  PageHeader,
  RelativeTime,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useNow,
} from "@crewlethq/ui";
import type { Tone } from "@crewlethq/ui";
import {
  DnsGlyph,
  ErrorGlyph,
  GroupGlyph,
  ManufacturingGlyph,
  MemoryGlyph,
  PersonGlyph,
  TargetGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * The lease table has no push behind it, so it polls — at 15 seconds, chosen
 * against the lease TTL rather than picked: a poll slower than the TTL would
 * show a node as alive after its lease had already expired somewhere else.
 */
const POLL_MS = 15_000;

const STATUS_TONE: Record<string, Tone> = {
  ok: "success",
  degraded: "warning",
  error: "danger",
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
      <PageHeader
        title="Fleet"
        description="Seat ownership is a lease with a fencing epoch, so no two nodes ever run one seat. A node that cannot reach the configuration it should be running releases its seats rather than serving stale work."
        badges={
          <>
            <Tag appearance="outline">{plural(nodes.length, "node")}</Tag>
            {data?.this_node && <Tag variant="brand">you are on {data.this_node}</Tag>}
          </>
        }
      />

      {error && (
        <Callout variant="danger" icon={<ErrorGlyph size="sm" />}>
          <span>
            This poll failed ({error}). What is below is the last reading that succeeded — on this
            screen above all, do not read it as now.
          </span>
        </Callout>
      )}

      <StatGroup columns={4}>
        <StatCard
          icon={<DnsGlyph />}
          label="Live nodes"
          value={nodes.length}
          sub="holding an unexpired lease"
        />
        <StatCard
          icon={<GroupGlyph />}
          label="Seats placed"
          value={data?.seats?.length ?? 0}
          sub={
            data?.unmanned_roles?.length
              ? `${data.unmanned_roles.length} role(s) with no seat running`
              : "every role has a home"
          }
        />
        <StatCard
          icon={<WarningGlyph />}
          label="Unplaceable"
          value={data?.unplaceable?.length ?? 0}
          sub={
            data?.unplaceable?.length
              ? "a placement constraint cannot be satisfied"
              : "nothing is stranded"
          }
        />
        <StatCard
          icon={<ManufacturingGlyph />}
          label="Behind on config"
          value={behind.length}
          sub={data?.target_epoch ? `target epoch ${data.target_epoch}` : "no target epoch"}
        />
      </StatGroup>

      {loading && !data && <Skeleton label="Loading the fleet" variant="text" rows={4} />}
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
        <Card as="section" padding="none">
          <Card.Header divided icon={<DnsGlyph size="sm" />} count={nodes.length}>
            <Card.Title>Nodes</Card.Title>
          </Card.Header>
          <RecordTable
            screen="fleet"
            table="nodes"
            rows={nodes}
            getRowKey={(n) => n.id}
            defaultSort={{ key: "id", direction: "asc" }}
            rowTone={(n) => (n.config_status === "error" ? "danger" : null)}
            columns={[
              {
                key: "id",
                header: "Node",
                sortable: true,
                // WHICH NODE cannot be hidden: every other cell here is a fact
                // about a node, and a row of them belonging to nobody is a row
                // nobody can act on.
                hideable: false,
                sortValue: (n) => n.id,
                render: (n) => (
                  <span className="row gap-1">
                    <InlineCode>{n.id}</InlineCode>
                    {n.id === data?.this_node && <Tag variant="brand">this one</Tag>}
                    {n.draining && <Tag variant="warning">draining</Tag>}
                  </span>
                ),
              },
              {
                key: "roles",
                header: "Duties",
                sortable: true,
                sortValue: (n) => n.roles.join(","),
                render: (n) => (
                  <span className="row wrap gap-1">
                    {n.roles.map((r) => (
                      <Tag key={r} appearance="outline">
                        {r}
                      </Tag>
                    ))}
                  </span>
                ),
              },
              {
                key: "seats",
                header: "Seats",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (n) => n.seats,
                // A COUNT on the node row; WHICH seats is the placement table
                // below, which is the one that can name them.
                render: (n) => n.seats,
              },
              {
                key: "inflight",
                header: "In flight",
                align: "right",
                firstDirection: "desc",
                sortable: true,
                sortValue: (n) => n.in_flight ?? 0,
                render: (n) => n.in_flight ?? 0,
              },
              {
                key: "posture",
                header: "Posture",
                shrink: true,
                sortable: true,
                sortValue: (n) => n.posture ?? "",
                render: (n) =>
                  n.posture ? (
                    <Tag variant={n.posture === "serve" ? "success" : "warning"}>{n.posture}</Tag>
                  ) : (
                    <EmptyValue label="Not reported" />
                  ),
              },
              {
                key: "config",
                header: "Config",
                shrink: true,
                sortable: true,
                sortValue: (n) => n.config_status ?? "",
                render: (n) => (
                  <span className="row gap-1">
                    <Tag variant={STATUS_TONE[n.config_status ?? ""] ?? "neutral"}>
                      {n.config_status || "unknown"}
                    </Tag>
                    {data && (n.config_epoch ?? 0) < data.target_epoch && (
                      <span
                        className="t-caption"
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
                sortable: true,
                sortValue: (n) => n.expires_in ?? 0,
                render: (n) =>
                  n.expires_in != null ? (
                    <span className="t-num t-caption" title="time until this node's lease expires">
                      {fmtDuration(n.expires_in * 1000)}
                    </span>
                  ) : (
                    <EmptyValue label="Not reported" />
                  ),
              },
              {
                key: "up",
                header: "Up since",
                shrink: true,
                sortable: true,
                sortValue: (n) => n.started_at ?? "",
                render: (n) =>
                  n.started_at ? (
                    <RelativeTime className="t-caption" value={n.started_at} now={now} />
                  ) : (
                    <EmptyValue label="Not reported" />
                  ),
              },
            ]}
          />
        </Card>

        {nodes.some((n) => n.config_error) && (
          <Card as="section">
            <Card.Header icon={<ErrorGlyph size="sm" />}>
              <Card.Title>Config apply errors</Card.Title>
            </Card.Header>
            <div className="col gap-2">
              {nodes
                .filter((n) => n.config_error)
                .map((n) => (
                  <Callout variant="danger" key={n.id} icon={<ErrorGlyph size="sm" />}>
                    <span>
                      <InlineCode>{n.id}</InlineCode>: {n.config_error}
                    </span>
                  </Callout>
                ))}
            </div>
          </Card>
        )}

        <div className="grid grid-auto-lg">
          <Card as="section" padding="none">
            <Card.Header divided icon={<GroupGlyph size="sm" />} count={data?.seats?.length ?? 0}>
              <Card.Title>Seat placement</Card.Title>
            </Card.Header>
            <RecordTable
              screen="fleet"
              table="seats"
              rows={data?.seats ?? []}
              getRowKey={(s) => s.handle}
              defaultSort={{ key: "handle", direction: "asc" }}
              emptyMessage={
                <EmptyState
                  size="compact"
                  title="No seats are leased"
                  description="A node claims a seat's lease before it runs the seat, so an engine with no company applied has none to claim."
                />
              }
              columns={[
                {
                  key: "handle",
                  header: "Seat",
                  sortable: true,
                  hideable: false,
                  sortValue: (s) => s.handle,
                  render: (s) => <SeatChip name={s.handle} handle={s.handle} />,
                },
                {
                  key: "node",
                  header: "Held by",
                  sortable: true,
                  sortValue: (s) => s.node,
                  // The NODE, not the lease's `owner` — that is the fencing
                  // token (a node id plus a per-process suffix), and showing
                  // it here would make one node look like several across a
                  // restart.
                  render: (s) => <InlineCode>{s.node}</InlineCode>,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  firstDirection: "desc",
                  shrink: true,
                  sortable: true,
                  sortValue: (s) => s.expires_in ?? 0,
                  render: (s) =>
                    s.expires_in != null ? (
                      fmtDuration(s.expires_in * 1000)
                    ) : (
                      <EmptyValue label="Not reported" />
                    ),
                },
              ]}
            />
          </Card>

          <Card as="section" padding="none">
            <Card.Header divided icon={<MemoryGlyph size="sm" />} count={data?.duties?.length ?? 0}>
              <Card.Title>Company-wide duties</Card.Title>
            </Card.Header>
            <RecordTable
              screen="fleet"
              table="duties"
              rows={data?.duties ?? []}
              getRowKey={(d) => d.duty}
              defaultSort={{ key: "duty", direction: "asc" }}
              emptyMessage={
                <EmptyState
                  size="compact"
                  title="No singleton duties are leased"
                  description="The retention sweep and the scheduler are fleet singletons, so exactly one node runs each."
                />
              }
              columns={[
                {
                  key: "name",
                  header: "Duty",
                  sortable: true,
                  hideable: false,
                  sortValue: (d) => d.duty,
                  render: (d) => d.duty,
                },
                {
                  key: "node",
                  header: "Held by",
                  sortable: true,
                  sortValue: (d) => d.node,
                  render: (d) => <InlineCode>{d.node}</InlineCode>,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  firstDirection: "desc",
                  shrink: true,
                  sortable: true,
                  sortValue: (d) => d.expires_in ?? 0,
                  render: (d) =>
                    d.expires_in != null ? (
                      fmtDuration(d.expires_in * 1000)
                    ) : (
                      <EmptyValue label="Not reported" />
                    ),
                },
              ]}
            />
          </Card>
        </div>

        {/* COMPARED, NOT COERCED. `{n && <X/>}` renders the NUMBER when n is
            0, and React draws a bare "0" where the panel would have been: a
            fleet with both lists present and empty, which is every healthy
            fleet, printed a stray zero under its panels. `undefined` is
            skipped, so the bug only showed once the engine answered with the
            arrays it always answers with. */}
        {((data?.unplaceable?.length ?? 0) > 0 || (data?.unmanned_roles?.length ?? 0) > 0) && (
          <Card as="section">
            <Card.Header icon={<WarningGlyph size="sm" />}>
              <Card.Title>Not running anywhere</Card.Title>
            </Card.Header>
            <div className="col gap-2">
              {data?.unmanned_roles?.map((r) => (
                <Callout variant="warning" key={r} icon={<PersonGlyph size="sm" />}>
                  <span>
                    <strong>{r}</strong> has no seat running on any node. Work published to its
                    mailbox waits there — a durable subscription retains it — but nothing is
                    consuming it.
                  </span>
                </Callout>
              ))}
              {data?.unplaceable?.map((u) => (
                <Callout variant="warning" key={u.handle} icon={<TargetGlyph size="sm" />}>
                  <span>
                    <strong>{u.handle}</strong> cannot be placed
                    {u.placement ? ` — it is pinned to ${u.placement}` : ""}
                    {u.reason ? `: ${u.reason}` : ", and no live node satisfies its constraint."}
                  </span>
                </Callout>
              ))}
            </div>
          </Card>
        )}
      </QueryState>

      {/* THE REPLICATION HALF, on this screen rather than its own. "Why is
          nothing being trimmed" and "which node is behind" are the same
          investigation, and a separate screen would make an operator hold two
          polls in their head to answer one question. */}
      <RetentionPanels thisNode={data?.this_node} />
    </>
  );
}
