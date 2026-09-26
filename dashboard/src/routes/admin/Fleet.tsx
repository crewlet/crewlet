/**
 * The fleet: which node holds which seat, and whether every node is running
 * the configuration the fleet has agreed on.
 *
 * This screen is read when nodes are dying, which is exactly when the API
 * answering it is least reliable — so it is the one screen that must SAY when
 * its own poll failed rather than showing the last reading as if it were now.
 *
 * # Two screens behind one export
 *
 * `#/admin/fleet/{id}` addresses ONE node: it is where a row's ⌘-click lands,
 * where the rail's `Open ↗` goes, and where the engine pill in the rail's foot
 * sends a reader who wants to know what this process is doing. The router has
 * always parsed that segment and this file dropped it, so the address rendered
 * the whole fleet under a breadcrumb naming one node — the reader was told
 * where they were by the crumb and shown everywhere by the screen.
 *
 * A node is an object like any other, so it wears an [ObjectHeader] carrying
 * the same five facts its peek does, and then answers the three questions the
 * lease table is the only place to answer: what it holds, which duties landed
 * on it, and whether it has applied the revision the fleet activated.
 *
 * # Every fact here comes from the LEASE TABLE, not from a /health probe
 *
 * `/health` answers about the node that served it, so behind a load balancer a
 * refresh tells a different story each time. `fleet` reads the rows, so every
 * node computes the same answer. The one exception is the build version, which
 * is a property of a PROCESS rather than of a row — see [NodePeek] for the
 * single place it is read and the rule that keeps it honest.
 */

import { useMemo } from "react";

import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  DnsGlyph,
  GroupGlyph,
  KeyGlyph,
  MemoryGlyph,
  PersonGlyph,
  PowerSettingsNewGlyph,
  TargetGlyph,
  TuneGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import {
  DateCell,
  DurationCell,
  KeyCell,
  NumberCell,
  StatusCell,
  TagsCell,
  TextCell,
} from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useEngineHealth } from "~/lib/store-hooks.ts";
import { plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { FleetAnswer, FleetDutyLease, FleetNode, FleetSeatLease } from "~/protocol/index.ts";
import { RetentionPanels } from "./Retention.tsx";

/**
 * The lease table has no push behind it, so it polls — at 15 seconds, chosen
 * against the lease TTL rather than picked: a poll slower than the TTL would
 * show a node as alive after its lease had already expired somewhere else.
 */
const POLL_MS = 15_000;

/** The apply status the control plane reports, as a [Tag] variant. */
const STATUS_TONE: Record<string, "success" | "warning" | "danger" | "neutral"> = {
  ok: "success",
  degraded: "warning",
  error: "danger",
};

/** Seconds on the wire, milliseconds in a [DurationCell] — converted once. */
function leaseMs(seconds?: number | null): number | null {
  return seconds == null ? null : seconds * 1000;
}

export function Fleet({ node }: { node?: string }) {
  return node ? <NodeScreen id={node} /> : <FleetScreen />;
}

// ---------------------------------------------------------------------------
// The fleet
// ---------------------------------------------------------------------------

function FleetScreen() {
  const now = useNow();
  const { data, loading, error } = useQuery("fleet", undefined, { pollMs: POLL_MS });
  const { open: openPeek } = usePeekControls();

  const nodes = useMemo(() => data?.nodes ?? [], [data]);
  // WHAT `[` AND `]` WALK. Published rather than handed to the rail, because
  // only the list knows what it is showing — see `PeekHost`.
  //
  // THE ANSWER'S OWN ORDER, which is the engine's sort by id and therefore the
  // order this table opens in. A column sort lives inside the grid and is not
  // something this screen can read back, so a reader who re-sorts steps in id
  // order instead: one order both halves agree on beats a stepper that claims
  // to follow a sequence it cannot see.
  usePeekNeighbours(useMemo(() => nodes.map((n) => ({ kind: "node", id: n.id })), [nodes]));

  // A node is behind when it has APPLIED an older epoch than the one the
  // activation pointer names. Epoch 0 means it has not reported at all, which
  // is a different thing from being behind and is called out on the row.
  const behind = nodes.filter((n) => data && (n.config_epoch ?? 0) < data.target_epoch);

  return (
    <>
      <PageActions>
        {
          <>
            <Tag appearance="outline">{plural(nodes.length, "node")}</Tag>
            {data?.this_node && <Tag variant="brand">you are on {data.this_node}</Tag>}
          </>
        }
      </PageActions>
      <PageNote>
        Seat ownership is a lease with a fencing epoch, so no two nodes ever run one seat. A node
        that cannot reach the configuration it should be running releases its seats rather than
        serving stale work.
      </PageNote>

      {error && (
        <Callout variant="danger" role="alert">
          <span>
            This poll failed ({error}). What is below is the last reading that succeeded — on this
            screen above all, do not read it as now.
          </span>
        </Callout>
      )}

      <Card padding="none">
        <StatGroup columns={4}>
          <StatCard
            icon={<DnsGlyph size="xs" />}
            label="Live nodes"
            value={nodes.length}
            sub="holding an unexpired lease"
          />
          <StatCard
            icon={<GroupGlyph size="xs" />}
            label="Seats placed"
            value={data?.seats?.length ?? 0}
            sub={
              data?.unmanned_roles?.length
                ? `${data.unmanned_roles.length} role(s) with no seat running`
                : "every role has a home"
            }
          />
          <StatCard
            icon={<WarningGlyph size="xs" />}
            label="Unplaceable"
            value={data?.unplaceable?.length ?? 0}
            sub={
              data?.unplaceable?.length
                ? "a placement constraint cannot be satisfied"
                : "nothing is stranded"
            }
          />
          <StatCard
            icon={<TuneGlyph size="xs" />}
            label="Behind on config"
            value={behind.length}
            sub={data?.target_epoch ? `target epoch ${data.target_epoch}` : "no target epoch"}
          />
        </StatGroup>
      </Card>

      {loading && !data && <Skeleton variant="text" rows={4} label="Loading" />}
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
        <Card padding="none">
          <Card.Header icon={<DnsGlyph size="sm" />} count={nodes.length}>
            Nodes
          </Card.Header>
          <DataGrid<FleetNode>
            rows={nodes}
            rowKey={(n) => n.id}
            defaultSort="id"
            isFailed={(n) => n.config_status === "error"}
            // THE ROW IS A REAL LINK to the node's page, so ⌘-click, the middle
            // button and the status bar all behave — and a plain click opens
            // the node beside the table instead, because this screen is read
            // while something is wrong and losing the fleet to read one node is
            // the navigation an operator can least afford.
            //
            // THE FRAME'S OWN ANSWER to where a node lives, rather than a second
            // copy of the route: the rail's `Open ↗` is built from the same
            // reference, so a row and the panel it opens can never name
            // different pages.
            rowHref={(n) => peekHref({ kind: "node", id: n.id })}
            onRowActivate={peekRow<FleetNode>((n) => openPeek({ kind: "node", id: n.id }))}
            columns={[
              {
                key: "id",
                header: "Node",
                // A NODE ID NEVER WRAPS. It is one identifier, and without
                // this the grid gave the column a share of the width and
                // broke `demo-node` down the page one character at a time —
                // four lines of a name that is one word.
                shrink: true,
                sortValue: (n) => n.id,
                cell: (n) => (
                  <span className="row gap-1">
                    <InlineCode>{n.id}</InlineCode>
                    {n.id === data?.this_node && <Tag variant="brand">this one</Tag>}
                    {n.draining && <Tag variant="warning">draining</Tag>}
                  </span>
                ),
              },
              {
                key: "roles",
                header: "Roles",
                sortValue: (n) => n.roles.join(","),
                // ROLES, NOT "DUTIES", which is what this column said while the
                // panel below it lists the fleet-wide duty leases. They are
                // different facts — `ingress`, `seats` and `workers` are what
                // this node MAY run, and a duty is a singleton somebody has to
                // hold — so a node reading "duties: seats, workers" beside a
                // duties panel naming a different node was one word describing
                // two things.
                cell: (n) => <TagsCell tags={n.roles} />,
              },
              {
                key: "seats",
                header: "Seats",
                align: "right",
                sortValue: (n) => n.seats,
                // A COUNT on the node row; WHICH seats is the placement table
                // below, and the node's own page, which can name them.
                cell: (n) => <NumberCell value={n.seats} />,
              },
              {
                key: "inflight",
                header: "In flight",
                align: "right",
                // ABSENT IS NOT ZERO, and the engine is careful to send it
                // absent: the presence heartbeat carries it only for a node
                // that publishes one at all. `?? 0` drew a confident idle row
                // for a process that was simply not saying — the one reading an
                // operator must never be given for free.
                sortValue: (n) => n.in_flight ?? null,
                cell: (n) => <NumberCell value={n.in_flight} />,
              },
              {
                key: "posture",
                header: "Posture",
                shrink: true,
                sortValue: (n) => n.posture ?? "",
                // A TAG RATHER THAN A `StatusCell`: the control plane's
                // postures are a vocabulary the node itself chooses a word
                // from — `serve`, `hold`, whatever it reports — and a glyph
                // would claim a binary this column does not have.
                cell: (n) =>
                  n.posture ? (
                    <Tag variant={n.posture === "serve" ? "success" : "warning"}>{n.posture}</Tag>
                  ) : (
                    <EmptyValue label="This node has published no presence heartbeat" />
                  ),
              },
              {
                key: "config",
                header: "Config",
                shrink: true,
                sortValue: (n) => n.config_status ?? "",
                cell: (n) => (
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
                sortValue: (n) => n.expires_in ?? null,
                cell: (n) => (
                  <span title="time until this node's lease expires">
                    <DurationCell ms={leaseMs(n.expires_in)} />
                  </span>
                ),
              },
              {
                key: "up",
                header: "Up since",
                shrink: true,
                sortValue: (n) => n.started_at ?? "",
                cell: (n) => <DateCell at={n.started_at} now={now} />,
              },
            ]}
          />
        </Card>

        {nodes.some((n) => n.config_error) && (
          <Card>
            <Card.Header icon={<WarningGlyph size="sm" />}>Config apply errors</Card.Header>
            <div className="col gap-2">
              {nodes
                .filter((n) => n.config_error)
                .map((n) => (
                  <Callout key={n.id} variant="danger" role="alert">
                    <span>
                      <InlineCode>{n.id}</InlineCode> — {n.config_error}
                    </span>
                  </Callout>
                ))}
            </div>
          </Card>
        )}

        <div className="grid grid-auto-lg">
          <Card padding="none">
            <Card.Header icon={<GroupGlyph size="sm" />} count={data?.seats?.length ?? 0}>
              Seat placement
            </Card.Header>
            <DataGrid<FleetSeatLease>
              name="seats"
              rows={data?.seats ?? []}
              rowKey={(s) => s.handle}
              defaultSort="handle"
              empty={{ title: "No seats are leased" }}
              rowHref={(s) => peekHref({ kind: "seat", id: s.handle })}
              onRowActivate={peekRow<FleetSeatLease>((s) =>
                openPeek({ kind: "seat", id: s.handle }),
              )}
              columns={[
                {
                  key: "handle",
                  header: "Seat",
                  sortValue: (s) => s.handle,
                  // NOT `SeatCell`, and not the seat chip this column used to
                  // draw: both are links and every row here is one now, and an
                  // anchor inside an anchor is markup no browser agrees about.
                  cell: (s) => <TextCell icon="memory">{s.handle}</TextCell>,
                },
                {
                  key: "node",
                  header: "Held by",
                  sortValue: (s) => s.node,
                  // The NODE, not the lease's `owner` — that is the fencing
                  // token (a node id plus a per-process suffix), and showing
                  // it here would make one node look like several across a
                  // restart.
                  //
                  // UNLINKED, for the same reason the seat above is: the row is
                  // an anchor. The node is a row in the table above, which is
                  // where this screen sends a reader who wants it.
                  cell: (s) => <KeyCell value={s.node} />,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  shrink: true,
                  sortValue: (s) => s.expires_in ?? null,
                  cell: (s) => <DurationCell ms={leaseMs(s.expires_in)} />,
                },
              ]}
            />
          </Card>

          <Card padding="none">
            <Card.Header icon={<MemoryGlyph size="sm" />} count={data?.duties?.length ?? 0}>
              Company-wide duties
            </Card.Header>
            <DataGrid<FleetDutyLease>
              name="duties"
              rows={data?.duties ?? []}
              rowKey={(d) => d.duty}
              defaultSort="duty"
              empty={{
                title: "No singleton duties are leased",
                hint: "The retention sweep and the scheduler are fleet singletons — exactly one node runs each.",
              }}
              columns={[
                {
                  key: "name",
                  header: "Duty",
                  sortValue: (d) => d.duty,
                  cell: (d) => <KeyCell value={d.duty} />,
                },
                {
                  key: "node",
                  header: "Held by",
                  sortValue: (d) => d.node,
                  // A LINK, unlike the seat table beside it: a duty is not an
                  // object the frame can address, so this row is not an anchor
                  // and the node it names is the only thing in it to open.
                  cell: (d) => <KeyCell value={d.node} path={["admin", "fleet", d.node]} />,
                },
                {
                  key: "ttl",
                  header: "Lease",
                  align: "right",
                  shrink: true,
                  sortValue: (d) => d.expires_in ?? null,
                  cell: (d) => <DurationCell ms={leaseMs(d.expires_in)} />,
                },
              ]}
            />
          </Card>
        </div>

        {/* `> 0`, NOT the bare length. `0 || 0` is `0`, and React renders a
            zero as the text "0" — so a healthy fleet drew a stray digit under
            the panels, which is the one thing a fleet screen must not do:
            an operator reading this page is looking for a number that is
            wrong, and here was one with no label at all. */}
        {((data?.unplaceable?.length ?? 0) > 0 || (data?.unmanned_roles?.length ?? 0) > 0) && (
          <Card>
            <Card.Header icon={<WarningGlyph size="sm" />}>Not running anywhere</Card.Header>
            <div className="col gap-2">
              {data?.unmanned_roles?.map((r) => (
                <Callout key={r} variant="warning" icon={<PersonGlyph size="md" />}>
                  <span>
                    <strong>{r}</strong> has no seat running on any node. Work published to its
                    mailbox waits there — a durable subscription retains it — but nothing is
                    consuming it.
                  </span>
                </Callout>
              ))}
              {data?.unplaceable?.map((u) => (
                <Callout key={u.handle} variant="warning" icon={<TargetGlyph size="md" />}>
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

// ---------------------------------------------------------------------------
// One node
// ---------------------------------------------------------------------------

/**
 * The two tags a node wears beside its title, or NOTHING AT ALL.
 *
 * An empty element would still take a gap in the header row, so a healthy node
 * on a single-node company would sit with a hole where a state it is not in
 * would have been.
 */
function nodeFlags(node: FleetNode, thisNode?: string): React.ReactNode {
  const here = node.id === thisNode;
  if (!here && !node.draining) return undefined;
  return (
    <span className="row gap-1">
      {/* The accent says WHERE THE READER IS, which is the one thing it is
          reserved for; `draining` is a state and takes a state's tone. */}
      {here && <Tag variant="brand">this one</Tag>}
      {node.draining && <Tag variant="warning">draining</Tag>}
    </span>
  );
}

/**
 * One node's own page.
 *
 * It answers from the same `fleet` document the table does, because the whole
 * point of reading the lease table is that every node computes it identically:
 * a per-node query served by whichever process the load balancer picked would
 * describe a different node on every refresh.
 */
export function NodeScreen({ id }: { id: string }) {
  const now = useNow();
  const { data, loading, error } = useQuery("fleet", undefined, {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  const node = data?.nodes.find((n) => n.id === id);
  const version = useNodeVersion(id, data?.this_node);

  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["admin", "fleet"])}>
            All nodes →
          </a>
        }
      </PageActions>

      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={error} loading={loading}>
        {data &&
          (node ? (
            <>
              <ObjectHeader
                kind="Node"
                icon="dns"
                // NO IDENTIFIER BESIDE THE TITLE: a node's id IS its name, and
                // a header that printed it twice would spend its widest line
                // saying one thing.
                title={node.id}
                status={nodeFlags(node, data.this_node)}
                facts={nodeFacts({ node, target: data.target_epoch, version })}
              />
              <NodePanels node={node} answer={data} now={now} />
              <Card>
                <Card.Header icon={<TuneGlyph size="sm" />}>Placement</Card.Header>
                <div className="col gap-2">
                  <div className="row wrap gap-2">
                    <span className="t-label">Roles</span>
                    {/* WHAT THIS NODE MAY RUN — `ingress`, `seats`, `workers` —
                        which is a different fact from the duties it holds. */}
                    <TagsCell tags={node.roles} max={8} />
                  </div>
                  <div className="row wrap gap-2">
                    <span className="t-label">Labels</span>
                    <TagsCell
                      tags={Object.entries(node.labels ?? {}).map(([k, v]) => `${k}=${v}`)}
                      max={8}
                    />
                  </div>
                  {node.owner && (
                    <p className="t-caption">
                      Fencing token <InlineCode>{node.owner}</InlineCode> — this node's id plus a
                      per-process suffix, so a restart is a new owner of the same leases.
                      {node.protocol != null && ` Seat protocol v${node.protocol}.`}
                    </p>
                  )}
                </div>
              </Card>
            </>
          ) : (
            <NoSuchNode id={id} />
          ))}
      </QueryState>
    </>
  );
}

/**
 * One node, in the rail.
 *
 * This is the object the engine pill in the rail's foot used to open as a
 * 440 px modal with no address at all, so it answers at least what that did:
 * what this process is, what it is running, whether its configuration is the
 * one the fleet activated, and what it holds.
 *
 * # It reads the fleet, not the node
 *
 * There is no per-node query, and there should not be: `fleet` is read from
 * the lease table, so every node answers it the same way, while anything
 * served by "the node that took the request" describes whichever process the
 * load balancer picked. Finding one row in that answer is the whole lookup.
 */
export function NodePeek({ id }: { id: string }) {
  const now = useNow();
  const { data, loading, error } = useQuery("fleet", undefined, {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  const node = data?.nodes.find((n) => n.id === id);
  const version = useNodeVersion(id, data?.this_node);

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={error} loading={loading}>
        {data &&
          (node ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Node"
                icon="dns"
                title={node.id}
                status={nodeFlags(node, data.this_node)}
                facts={nodeFacts({ node, target: data.target_epoch, version })}
              />
              <div className="col gap-3">
                <NodePanels node={node} answer={data} now={now} />
              </div>
            </>
          ) : (
            <NoSuchNode id={id} inline />
          ))}
      </QueryState>
    </>
  );
}

/**
 * The build version, which is the one fact here that belongs to a PROCESS
 * rather than to a lease row.
 *
 * The health push describes the node that served the socket and says so — it
 * has a `node` field — so it is read only when the object being read IS that
 * node, and the fact renders as an honest dash on every other. Painting this
 * dashboard's own version onto a peer would answer "is the fleet mid-upgrade"
 * with "no" on exactly the fleet that is.
 */
function useNodeVersion(id: string, thisNode?: string): string | undefined {
  const engine = useEngineHealth();
  return id !== "" && thisNode === id && engine?.node === id ? engine.version : undefined;
}

/**
 * The five facts a node is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader
 * scans the same facts in the same order wherever the object appears, and a
 * header written twice is two orders as soon as somebody adds a sixth fact.
 */
export function nodeFacts({
  node,
  target,
  version,
}: {
  node: FleetNode;
  /** The epoch the whole fleet is converging on. */
  target: number;
  /** The engine build — known only for the node that served this socket. */
  version?: string;
}): Fact[] {
  const applied = node.config_epoch ?? 0;
  const behind = target - applied;
  return [
    {
      label: "Posture",
      // The control plane's own word, never a verdict of this screen's.
      value: node.posture || <EmptyValue label="This node has published no presence heartbeat" />,
    },
    {
      label: "Epoch",
      // EPOCH 0 IS NOT EPOCH ZERO. It is a node that has reported no apply at
      // all, and "0 of 41" would read as a node forty-one revisions behind
      // rather than as one that has not spoken.
      //
      // The epoch rather than the revision id it stands for: the id is on the
      // apply record and not in this answer's declared shape, and a header
      // that named a revision it had to guess at would be worse than one that
      // names the number every row here is compared against.
      value:
        applied > 0 ? (
          `${applied} of ${target}`
        ) : (
          <EmptyValue label="This node has reported no config apply" />
        ),
    },
    {
      // WHETHER ITS COPIES ARE UP, which the epoch above does not say: a node
      // can hold the current revision and still be replaying the log its SQL
      // state is derived from, and a seat placed on it then reads an empty
      // answer as "there is no such item". `internal/coord/nodestatus.go` puts
      // the fact here on purpose — it is the answer to "why is the new node
      // holding nothing" — and deliberately does NOT take the node out of
      // rotation for it.
      //
      // DROPPED, NOT DASHED, when the node published none: `FactLine` omits an
      // empty value, and a dash beside "Posture" and "Epoch" would claim the
      // engine keeps a reading it does not. The node only publishes the pair
      // once the total is non-zero.
      label: "Copies",
      value:
        node.projections_total != null && node.projections_total > 0
          ? `${node.projections_ready ?? 0} of ${node.projections_total} ready`
          : "",
    },
    {
      label: "Lag",
      value:
        applied === 0 ? (
          <EmptyValue label="Nothing to compare against: this node has reported no config apply" />
        ) : behind > 0 ? (
          `${plural(behind, "epoch")} behind`
        ) : (
          "current"
        ),
    },
    // ZERO SEATS IS A REAL ANSWER — an ingress-only node runs none — and it is
    // the node an operator is most often looking for here, so it must not be
    // the one a `||` turns into a dash.
    { label: "Seats", value: <NumberCell value={node.seats} /> },
    {
      label: "Version",
      value: version ?? (
        <EmptyValue label="Only the node serving this dashboard reports its build version" />
      ),
    },
  ];
}

/**
 * What a node is doing, on its page and in the rail alike: whether it has
 * applied the revision the fleet activated, the leases it holds, and the
 * fleet-wide duties that landed on it.
 */
function NodePanels({ node, answer, now }: { node: FleetNode; answer: FleetAnswer; now: number }) {
  const { open: openPeek } = usePeekControls();
  const seats = answer.seats.filter((s) => s.node === node.id);
  const duties = answer.duties.filter((d) => d.node === node.id);
  const applied = node.config_epoch ?? 0;
  const behind = answer.target_epoch - applied;
  const current = applied > 0 && behind <= 0;

  return (
    <>
      {/* THE PANEL THE ENGINE PILL'S MODAL WAS. It opened on this node alone,
          at 440 px, with no address of its own — so what it answered had to
          land somewhere addressable: what this process is doing NOW, and
          which revision it is doing it on. The second half is the question an
          operator came for; the first is the one they check next. */}
      <Card>
        <Card.Header
          icon={<PowerSettingsNewGlyph size="sm" />}
          subtitle="what this process is doing, and the revision it is doing it on"
        >
          Running
        </Card.Header>
        <div className="col gap-2">
          <div className="row wrap gap-2">
            {/* THE ONE QUESTION THIS PANEL EXISTS FOR, as a state rather than
                as two numbers to subtract. `config_status` beside it is a
                different fact: a node can report `ok` about a revision two
                activations old. */}
            <StatusCell
              glyph={current ? "●" : "○"}
              label={
                current
                  ? "running the active revision"
                  : applied > 0
                    ? `${plural(behind, "epoch")} behind`
                    : "no apply reported"
              }
              tone={node.config_status === "error" ? "critical" : current ? "positive" : "caution"}
              title={`applied epoch ${applied}, the fleet has activated ${answer.target_epoch}`}
            />
            <Tag variant={STATUS_TONE[node.config_status ?? ""] ?? "neutral"}>
              {node.config_status || "unknown"}
            </Tag>
            <span className="spacer" />
            <span className="t-caption">reported</span>
            <DateCell at={node.config_reported_at} now={now} />
          </div>
          {node.config_error && (
            <Callout variant="danger" role="alert">
              It could not apply that revision: {node.config_error}
            </Callout>
          )}
          <div className="row wrap gap-2">
            <span className="t-label">In flight</span>
            {/* ABSENT IS NOT ZERO: the count rides the presence heartbeat, and
                a node that publishes none is not a node running no turns. */}
            <NumberCell value={node.in_flight} />
            <span className="t-label">Up since</span>
            <DateCell at={node.started_at} now={now} />
          </div>
          <p className="t-caption">
            A node reports the epoch it has applied; the activation pointer names the one the fleet
            agreed on. The two differ for as long as a node takes to rebuild — and for ever if it
            cannot, which is what the apply error above is.
          </p>
        </div>
      </Card>

      <Card padding="none">
        <Card.Header
          icon={<KeyGlyph size="sm" />}
          count={seats.length}
          subtitle="its own, and one per seat it holds"
        >
          Leases
        </Card.Header>
        <div className="row wrap gap-2" style={{ padding: "var(--space-3)" }}>
          <span className="t-label">Its own lease</span>
          <DurationCell ms={leaseMs(node.expires_in)} />
          <span className="t-caption">
            until expiry — a node that stops renewing loses every seat below to whoever claims it
            next
          </span>
        </div>
        <DataGrid<FleetSeatLease>
          name="node-seats"
          rows={seats}
          rowKey={(s) => s.handle}
          defaultSort="handle"
          empty={{
            title: "This node holds no seats",
            hint: "Placement is greedy and converges in both directions — a node that runs no seats is either new, draining, or not in the `seats` role.",
          }}
          rowHref={(s) => peekHref({ kind: "seat", id: s.handle })}
          onRowActivate={peekRow<FleetSeatLease>((s) => openPeek({ kind: "seat", id: s.handle }))}
          columns={[
            {
              key: "handle",
              header: "Seat",
              sortValue: (s) => s.handle,
              // NOT `SeatCell`: it is a link and this row is one already.
              cell: (s) => <TextCell icon="memory">{s.handle}</TextCell>,
            },
            {
              key: "ttl",
              header: "Lease",
              align: "right",
              shrink: true,
              sortValue: (s) => s.expires_in ?? null,
              cell: (s) => <DurationCell ms={leaseMs(s.expires_in)} />,
            },
          ]}
        />
      </Card>

      <Card padding="none">
        <Card.Header icon={<MemoryGlyph size="sm" />} count={duties.length}>
          Duties
        </Card.Header>
        <DataGrid<FleetDutyLease>
          name="node-duties"
          rows={duties}
          rowKey={(d) => d.duty}
          defaultSort="duty"
          empty={{
            title: "No fleet singletons are held here",
            hint: "Exactly one node runs each — the trim, the maintenance sweep, the scheduler — so most nodes hold none.",
          }}
          columns={[
            {
              key: "name",
              header: "Duty",
              sortValue: (d) => d.duty,
              cell: (d) => <KeyCell value={d.duty} />,
            },
            {
              key: "ttl",
              header: "Lease",
              align: "right",
              shrink: true,
              sortValue: (d) => d.expires_in ?? null,
              cell: (d) => <DurationCell ms={leaseMs(d.expires_in)} />,
            },
          ]}
        />
      </Card>
    </>
  );
}

/**
 * NOT AN EMPTY PANEL, and not a spinner that never resolves.
 *
 * A node id reaches this from a pasted URL and from a bookmark as often as
 * from a row, and the lease table IS the fleet: a node that stopped renewing
 * leaves no row behind at all. So the honest answer names the id that resolved
 * to nothing and says what its absence means, which is the same thing a reader
 * would otherwise conclude from an empty screen — wrongly.
 */
function NoSuchNode({ id, inline }: { id: string; inline?: boolean }) {
  return (
    <EmptyState
      size={inline ? "compact" : "default"}
      icon={<DnsGlyph size="xl" />}
      title={`No node called “${id}” holds a lease`}
      description="Either it never existed, or it stopped renewing and the fleet has since dropped it — a node is live exactly as long as its lease is."
    />
  );
}
