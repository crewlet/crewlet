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
 * `#/settings/nodes/{id}` addresses ONE node: it is where a row's ⌘-click lands,
 * where the rail's `Open ↗` goes, and where the engine pill in the rail's foot
 * sends a reader who wants to know what this process is doing. The router has
 * always parsed that segment and this file dropped it, so the address rendered
 * the whole fleet under a breadcrumb naming one node — the reader was told
 * where they were by the crumb and shown everywhere by the screen.
 *
 * A node is an object like any other, so it wears an [ObjectHeader] carrying
 * the same facts its peek does, and then answers the three questions the
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
import { uiletTone } from "~/ui/primitives.tsx";

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
  TableKeyCell,
  TableNumberCell,
  TableDateCell,
  TableEntityCell,
  TableStatusCell,
  TableTagsCell,
  peekRowActivation,
} from "@crewlethq/ui";
import {
  ServerGlyph,
  UsersGlyph,
  KeyGlyph,
  CpuGlyph,
  UserGlyph,
  PowerGlyph,
  TargetGlyph,
  SlidersVerticalGlyph,
  TriangleAlertGlyph,
  DatabaseGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { RelatedRow } from "~/app/frame/RelatedRow.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { usePublishFleet } from "~/app/Shell.tsx";
import { useEngineHealth } from "~/lib/store-hooks.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { fmtCount, fmtDuration } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { configStanding, offRevision, type ConfigStanding } from "~/lib/configStanding.ts";
import { standingCaption, standingWords } from "./standingWords.ts";
import type { FleetAnswer, FleetDutyLease, FleetNode, FleetSeatLease } from "~/protocol/index.ts";
import { BrokerKindTag, BrokerMembership } from "./FleetBroker.tsx";

/**
 * The lease table has no push behind it, so it polls — at 15 seconds, chosen
 * against the lease TTL rather than picked: a poll slower than the TTL would
 * show a node as alive after its lease had already expired somewhere else.
 */
export const FLEET_POLL_MS = 15_000;
const POLL_MS = FLEET_POLL_MS;

/** The apply status the control plane reports, as a [Tag] variant. */
const STATUS_TONE: Record<string, "success" | "warning" | "danger" | "neutral"> = {
  ok: "success",
  degraded: "warning",
  error: "danger",
};

/** Seconds on the wire, milliseconds in a [TableNumberCell], converted once. */
function leaseMs(seconds?: number | null): number | null {
  return seconds == null ? null : seconds * 1000;
}

export function Fleet({ node }: { node?: string }) {
  return node ? <NodeScreen id={node} /> : <FleetScreen />;
}

// ---------------------------------------------------------------------------
// The fleet
// ---------------------------------------------------------------------------

/**
 * The Broker column's cell: the kind a node's presence advertises, `unknown`
 * marked as the one reading that is a guess.
 */
export function BrokerCell({ broker }: { broker: string }) {
  return <BrokerKindTag kind={broker} />;
}

function FleetScreen() {
  const seatBadge = useSeatBadgeOf();
  const now = useNow();
  const { data, loading, error, refusal } = useQuery("fleet", undefined, { pollMs: POLL_MS });
  // THE BROKER'S MEMBERSHIP, polled beside the lease table on the same
  // cadence: a member that died is noticed on both at once.
  const broker = useQuery("fleet_broker", undefined, { pollMs: POLL_MS });
  // THE COLUMN'S "n behind on config" IS THIS READING, not a second poll.
  usePublishFleet(data);
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

  // A NODE IS BEHIND WHEN IT DOES NOT RUN THE ACTIVATED REVISION, which its
  // epoch alone does not say: the epoch is the one it ATTEMPTED, so a node
  // that refused the target reports the target's epoch while it runs the one
  // before (`configStanding`). A node that has reported nothing is counted —
  // nothing says it runs the fleet's revision — and its row says it is silent
  // rather than behind.
  const behind = nodes.filter((n) => data && offRevision(data, n));

  return (
    <>
      {/* NO COUNT OF NODES HERE: the Live nodes tile below says it, and the
          bar's chip repeated the same figure an inch above it. Where the
          reader is, is a fact no tile carries. */}
      <PageActions>
        {data?.this_node && <Tag variant="brand">you are on {data.this_node}</Tag>}
      </PageActions>

      {/* TWO DIFFERENT FAILURES. A poll that failed AFTER one succeeded
          leaves a reading on screen, and the banner says it is old. One that
          failed with no reading ever taken has nothing to be the last of: it
          is the refusal alone, drawn by QueryState, with no tiles and no
          empty state under it — a refused read drew "0 nodes" four times and
          then "No nodes are reporting". */}
      {error && data && (
        <Callout variant="danger" role="alert">
          <span>Refresh failed ({error}). Showing the last good reading.</span>
        </Callout>
      )}
      {error && !data && <QueryState error={error} refusal={refusal} loading={false} />}

      {data && (
        <Card padding="none">
          <StatGroup columns={4}>
            <StatCard
              icon={<ServerGlyph size="xs" />}
              label="Live nodes"
              value={nodes.length}
              sub="holding an unexpired lease"
            />
            <StatCard
              icon={<UsersGlyph size="xs" />}
              label="Seats placed"
              value={data?.seats?.length ?? 0}
              sub={
                data?.unmanned_roles?.length
                  ? `${data.unmanned_roles.length} role(s) with no seat running`
                  : "every role has a home"
              }
            />
            <StatCard
              icon={<TriangleAlertGlyph size="xs" />}
              label="Unplaceable"
              value={data?.unplaceable?.length ?? 0}
              sub={
                data?.unplaceable?.length
                  ? "a placement constraint cannot be satisfied"
                  : "nothing is stranded"
              }
            />
            <StatCard
              icon={<SlidersVerticalGlyph size="xs" />}
              label="Behind on config"
              value={behind.length}
              sub={data?.activation ? `target epoch ${data.activation.epoch}` : "no target epoch"}
            />
          </StatGroup>
        </Card>
      )}

      {loading && !data && <Skeleton variant="text" rows={4} label="Loading" />}
      {data && (
        <QueryState
          error={null}
          loading={loading}
          empty={
            nodes.length
              ? undefined
              : {
                  title: "No nodes are reporting",
                  hint: "Only multi-node fleets report here.",
                }
          }
        >
          <Card padding="none">
            <Card.Header icon={<ServerGlyph size="sm" />} count={nodes.length}>
              <Card.Title>Nodes</Card.Title>
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
              onRowActivate={peekRowActivation<FleetNode>((n) =>
                openPeek({ kind: "node", id: n.id }),
              )}
              columns={[
                {
                  key: "id",
                  header: "Node",
                  // THE ONE FLEXIBLE TRACK, as the seat is in the lease tables
                  // below: the row exists to name a node, so the node takes
                  // whatever width is spare and every fact beside it sizes to
                  // its content. Three `1fr` columns (Roles, Seats, In flight)
                  // split the spare width three ways — 150px of air around a
                  // single digit at 1440 while the role chips wrapped onto
                  // three lines, and under a peek each fell to a letter ("R.",
                  // "S.", "I.") and the chips to empty pills.
                  //
                  // AND IT NEVER WRAPS OR CUTS: the floor is its content. An id
                  // is one identifier — given a share, the grid once broke
                  // `demo-node` down the page one character at a time — so
                  // where the row cannot hold every fact, facts give way
                  // (`drop`) rather than the name.
                  floor: "max-content",
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
                  //
                  // ON ONE LINE, OR NOT AT ALL. A chip is a word, and a track
                  // narrower than its chips either stacks them — a row two or
                  // three times its neighbours' height — or squeezes them into
                  // pills with no word in them. So the track is the chips'
                  // own width, and under a peek the column gives way whole:
                  // the node's peek names its roles among its facts.
                  width: "max-content",
                  drop: 4,
                  cell: (n) => <TableTagsCell tags={n.roles} />,
                },
                {
                  key: "broker",
                  header: "Broker",
                  shrink: true,
                  // SECOND TO GIVE WAY, after the lease: what a node's broker
                  // is follows from its config and changes with a restart at
                  // most, the Broker members panel below names every node's
                  // kind again, and the peek carries it among its facts.
                  drop: 2,
                  sortValue: (n) => n.broker,
                  // HOW ITS BROKER TAKES PART, which the roles no longer say: a
                  // node's broker is what its stream block makes it. `unknown`
                  // is a value — a node advertising a kind this build does not
                  // know (a newer build's), which a capacity seal counts as a
                  // member — and it is marked, because it is the one reading
                  // here that is a guess.
                  cell: (n) => <BrokerCell broker={n.broker} />,
                },
                {
                  key: "seats",
                  header: "Seats",
                  align: "right",
                  // A COUNT IS AS WIDE AS ITS HEAD, never a share of the row.
                  shrink: true,
                  sortValue: (n) => n.seats,
                  // A COUNT on the node row; WHICH seats is the placement table
                  // below, and the node's own page, which can name them.
                  cell: (n) => <TableNumberCell value={n.seats} format={fmtCount} />,
                },
                {
                  key: "inflight",
                  header: "In flight",
                  align: "right",
                  shrink: true,
                  // The node's peek says it too ("Running"), so it may give way
                  // — last of the five, being the load an operator scans for.
                  drop: 5,
                  // ABSENT IS NOT ZERO, and the engine is careful to send it
                  // absent: the presence heartbeat carries it only for a node
                  // that publishes one at all. `?? 0` drew a confident idle row
                  // for a process that was simply not saying — the one reading an
                  // operator must never be given for free.
                  sortValue: (n) => n.in_flight ?? null,
                  cell: (n) => <TableNumberCell value={n.in_flight} format={fmtCount} />,
                },
                {
                  key: "posture",
                  header: "Posture",
                  shrink: true,
                  sortValue: (n) => n.posture ?? "",
                  // A TAG RATHER THAN A `TableStatusCell`: the control plane's
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
                  cell: (n) => {
                    const standing = configStanding(data, n);
                    const caption = standingCaption(standing);
                    return (
                      <span className="row gap-1">
                        <Tag variant={STATUS_TONE[n.config_status ?? ""] ?? "neutral"}>
                          {n.config_status || "unknown"}
                        </Tag>
                        {caption && (
                          <span className="t-caption" title={standingTitle(n, data)}>
                            {caption}
                          </span>
                        )}
                      </span>
                    );
                  },
                },
                {
                  key: "lease",
                  header: "Lease",
                  shrink: true,
                  // RIGHT, as a duration is in Seat placement and Company-wide
                  // duties on this same page.
                  align: "right",
                  // FIRST TO GIVE WAY. Every row here holds an unexpired lease
                  // — a node is listed exactly as long as it does — so this
                  // column is a countdown that reads "healthy" by construction
                  // until the moment the row leaves; the node's own page and
                  // peek carry it as "Its own lease". At 1280 the nine
                  // columns want about 840px of a 746px grid, and this is the
                  // one whose absence costs the scan least.
                  drop: 1,
                  sortValue: (n) => n.expires_in ?? null,
                  cell: (n) => (
                    <span title="time until this node's lease expires">
                      <TableNumberCell
                        value={leaseMs(n.expires_in)}
                        format={fmtDuration}
                        emptyLabel="Not measured"
                      />
                    </span>
                  ),
                },
                {
                  key: "up",
                  header: "Up since",
                  shrink: true,
                  align: "right",
                  // The node's peek carries it beside In flight ("Running").
                  drop: 3,
                  sortValue: (n) => n.started_at ?? "",
                  cell: (n) => <TableDateCell value={n.started_at} now={now} />,
                },
              ]}
            />
          </Card>

          {nodes.some((n) => n.config_error) && (
            <Card>
              <Card.Header icon={<TriangleAlertGlyph size="sm" />}>
                <Card.Title>Config apply errors</Card.Title>
              </Card.Header>
              <div className="col gap-2">
                {nodes
                  .filter((n) => n.config_error)
                  .map((n) => (
                    <Callout key={n.id} variant="danger" role="alert">
                      <span>
                        <InlineCode>{n.id}</InlineCode>: {n.config_error}
                      </span>
                    </Callout>
                  ))}
              </div>
            </Card>
          )}

          <div className="grid grid-auto-lg">
            <Card padding="none">
              <Card.Header icon={<UsersGlyph size="sm" />} count={data?.seats?.length ?? 0}>
                <Card.Title>Seat placement</Card.Title>
              </Card.Header>
              <DataGrid<FleetSeatLease>
                name="seats"
                rows={data?.seats ?? []}
                rowKey={(s) => s.handle}
                defaultSort="handle"
                empty={{ title: "No seats are leased" }}
                rowHref={(s) => peekHref({ kind: "seat", id: s.handle })}
                onRowActivate={peekRowActivation<FleetSeatLease>((s) =>
                  openPeek({ kind: "seat", id: s.handle }),
                )}
                columns={[
                  {
                    key: "handle",
                    header: "Seat",
                    sortValue: (s) => s.handle,
                    // NOT `linkedSeat`, and not the seat chip this column used to
                    // draw: both are links and every row here is one now, and an
                    // anchor inside an anchor is markup no browser agrees about.
                    cell: (s) => <TableEntityCell {...plainSeat({ ...seatBadge(s.handle) })} />,
                  },
                  {
                    key: "node",
                    header: "Held by",
                    // CONTENT-SIZED, so the SEAT is the one flexible track. As
                    // a second `1fr` this column took half of what the fixed
                    // columns left — about 155px for a nine-character node id —
                    // and the seat name the row exists to show was the value
                    // cut ("Agent Frontend …") at a 1440 frame. ITS OWN WIDTH,
                    // never capped: a node id is short, and capped it read
                    // "walk-nat…" beside a seat column with room to spare.
                    width: "max-content",
                    sortValue: (s) => s.node,
                    // The NODE, not the lease's `owner` — that is the fencing
                    // token (a node id plus a per-process suffix), and showing
                    // it here would make one node look like several across a
                    // restart.
                    cell: (s) => <HeldBy node={s.node} />,
                  },
                  {
                    key: "since",
                    header: "Since",
                    align: "right",
                    shrink: true,
                    // OLDEST FIRST WHEN ASCENDING, by the instant rather than
                    // its spelling, whose fractional seconds vary in length.
                    sortValue: (s) => Date.parse(s.acquired_at),
                    cell: (s) => <HeldSince lease={s} now={now} />,
                  },
                  {
                    key: "ttl",
                    header: "Lease",
                    align: "right",
                    shrink: true,
                    sortValue: (s) => s.expires_in ?? null,
                    cell: (s) => (
                      <TableNumberCell
                        value={leaseMs(s.expires_in)}
                        format={fmtDuration}
                        emptyLabel="Not measured"
                      />
                    ),
                  },
                ]}
              />
            </Card>

            <Card padding="none">
              <Card.Header icon={<CpuGlyph size="sm" />} count={data?.duties?.length ?? 0}>
                <Card.Title>Company-wide duties</Card.Title>
              </Card.Header>
              <DataGrid<FleetDutyLease>
                name="duties"
                rows={data?.duties ?? []}
                rowKey={(d) => d.duty}
                defaultSort="duty"
                empty={{
                  title: "No singleton duties are leased",
                  hint: "Duties appear once a node takes them.",
                }}
                columns={[
                  {
                    key: "name",
                    header: "Duty",
                    sortValue: (d) => d.duty,
                    cell: (d) => <TableKeyCell value={d.duty} />,
                  },
                  {
                    key: "node",
                    header: "Held by",
                    // Content-sized for the reason the seat table's is: the
                    // duty's name is the flexible track, and as a second `1fr`
                    // this column left "integration-reconci…" cut beside
                    // spare space of its own. Its own width, never capped, for
                    // the reason the seat table's is.
                    width: "max-content",
                    sortValue: (d) => d.node,
                    cell: (d) => <HeldBy node={d.node} />,
                  },
                  {
                    key: "ttl",
                    header: "Lease",
                    align: "right",
                    shrink: true,
                    sortValue: (d) => d.expires_in ?? null,
                    cell: (d) => (
                      <TableNumberCell
                        value={leaseMs(d.expires_in)}
                        format={fmtDuration}
                        emptyLabel="Not measured"
                      />
                    ),
                  },
                ]}
              />
            </Card>
          </div>

          <BrokerMembership
            answer={broker.data ?? undefined}
            error={broker.error}
            refusal={broker.refusal}
            onChanged={broker.refetch}
          />

          {/* `> 0`, NOT the bare length. `0 || 0` is `0`, and React renders a
            zero as the text "0" — so a healthy fleet drew a stray digit under
            the panels, which is the one thing a nodes page must not do:
            an operator reading this page is looking for a number that is
            wrong, and here was one with no label at all. */}
          {((data?.unplaceable?.length ?? 0) > 0 || (data?.unmanned_roles?.length ?? 0) > 0) && (
            <Card>
              <Card.Header icon={<TriangleAlertGlyph size="sm" />}>
                <Card.Title>Not running anywhere</Card.Title>
              </Card.Header>
              <div className="col gap-2">
                {data?.unmanned_roles?.map((r) => (
                  <Callout key={r} variant="warning" icon={<UserGlyph size="md" />}>
                    <span>
                      <strong>{r}</strong> isn’t running on any node. Its work is waiting.
                    </span>
                  </Callout>
                ))}
                {data?.unplaceable?.map((u) => (
                  <Callout key={u.handle} variant="warning" icon={<TargetGlyph size="md" />}>
                    <span>
                      <strong>{u.handle}</strong> can’t be placed:{" "}
                      {u.reason
                        ? u.reason
                        : `no live node matches ${u.placement || "its constraint"}.`}
                    </span>
                  </Callout>
                ))}
              </div>
            </Card>
          )}
        </QueryState>
      )}

      {/* THE REPLICATION HALF IS ONE CLICK AWAY, NAMED. "Why is nothing
          being trimmed" and "which node is behind" are one investigation, and
          it used to be drawn under this table for that reason — which put the
          state log's every domain, its evictions and its snapshot tier under
          the node list of a screen called Nodes. It is Settings › Backups &
          retention now, and the way there is said here, where the question
          starts. A related row rather than a footnote under the last card,
          the shape Company › Principles points at Settings › Limits with. */}
      <RelatedRow
        path={["settings", "backups"]}
        icon={<DatabaseGlyph size="sm" />}
        title="Backups & retention"
        description="Every log's position on every node, its trim and its evictions."
      />
    </>
  );
}

// ---------------------------------------------------------------------------
// One node
// ---------------------------------------------------------------------------

/**
 * Which node holds a lease, in both lease tables alike: a link to that node's
 * page, at the body's weight.
 *
 * ONE TREATMENT. The seat table drew the id as unlinked primary ink, which in
 * the mono face outweighed the seat's own name, and the duty table beside it
 * drew the same id as a link — one value, two looks, on one page. The seat
 * table's excuse was that its row is an anchor and a link inside one is
 * invalid markup; it is not any more — the row's link is an overlay the
 * cells' own links sit above (`DataGrid`), which is how a seat chip inside a
 * peeking row already works — so the node is one click away from either row.
 */
export function HeldBy({ node }: { node: string }) {
  return <TableKeyCell value={node} href={href(["settings", "nodes", node])} />;
}

/**
 * Since when a seat's holder has held it — the tenure's start, which a renewal
 * does not move, so "since 08:02" means the seat has not changed node since
 * 08:02 (`coord.Lease.AcquiredAt`, on the coordination store's clock). Every
 * lease carries one, and it is the START the cell reads, never the lease's
 * end.
 */
export function HeldSince({ lease, now }: { lease: FleetSeatLease; now: number }) {
  return <TableDateCell value={lease.acquired_at} now={now} />;
}

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
  const { data, loading, error, refusal } = useQuery("fleet", undefined, {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  usePublishFleet(data);
  const node = data?.nodes.find((n) => n.id === id);
  const version = useNodeVersion(id, data?.this_node);

  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["settings", "nodes"])}>
            All nodes →
          </a>
        }
      </PageActions>

      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {data &&
          (node ? (
            <>
              <ObjectHeader
                kind="Node"
                icon="server"
                // NO IDENTIFIER BESIDE THE TITLE: a node's id IS its name, and
                // a header that printed it twice would spend its widest line
                // saying one thing.
                title={node.id}
                status={nodeFlags(node, data.this_node)}
                facts={nodeFacts({ node, answer: data, version })}
              />
              <NodePanels node={node} answer={data} now={now} />
              <Card>
                <Card.Header icon={<SlidersVerticalGlyph size="sm" />}>
                  <Card.Title>Placement</Card.Title>
                </Card.Header>
                <div className="col gap-2">
                  {/* ITS ROLES ARE A HEADER FACT (`nodeFacts`), which the peek
                      shares; this card keeps what only the page draws. */}
                  <div className="row wrap gap-2">
                    <span className="t-label">Labels</span>
                    <TableTagsCell
                      tags={Object.entries(node.labels ?? {}).map(([k, v]) => `${k}=${v}`)}
                      max={8}
                    />
                  </div>
                  {node.owner && (
                    <p className="t-caption">
                      Fencing token <InlineCode>{node.owner}</InlineCode>.
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
  const { data, loading, error, refusal } = useQuery("fleet", undefined, {
    enabled: id !== "",
    pollMs: POLL_MS,
  });
  const node = data?.nodes.find((n) => n.id === id);
  const version = useNodeVersion(id, data?.this_node);

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {data &&
          (node ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Node"
                icon="server"
                title={node.id}
                status={nodeFlags(node, data.this_node)}
                facts={nodeFacts({ node, answer: data, version })}
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
 * The facts a node is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader
 * scans the same facts in the same order wherever the object appears, and a
 * header written twice is two orders as soon as somebody adds a sixth fact.
 */
export function nodeFacts({
  node,
  answer,
  version,
}: {
  node: FleetNode;
  /** The activation the whole fleet is converging on. */
  answer: Pick<FleetAnswer, "activation">;
  /** The engine build — known only for the node that served this socket. */
  version?: string;
}): Fact[] {
  const attempted = node.config_epoch ?? 0;
  const target = answer.activation?.epoch ?? 0;
  const standing = configStanding(answer, node);
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
      // THE EPOCH IT ATTEMPTED, not the one it runs: a node that refused the
      // target reports the target's epoch. Whether it runs the fleet's
      // revision is the Lag fact below, which reads the revision ids beside
      // the number — the number alone cannot say, and can even name another
      // revision once a rebuilt coordination store has restarted its count.
      value:
        attempted > 0 ? (
          target > 0 ? (
            `${attempted} of ${target}`
          ) : (
            `${attempted}`
          )
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
        node.appliers_total != null && node.appliers_total > 0
          ? `${node.appliers_ready ?? 0} of ${node.appliers_total} ready`
          : "",
    },
    {
      // WHERE IT STANDS, in words rather than a subtraction: a refusal of the
      // target is at the target's epoch, and "0 epochs behind" would call it
      // current.
      label: "Lag",
      value: lagValue(standing),
    },
    // ZERO SEATS IS A REAL ANSWER — an ingress-only node runs none — and it is
    // the node an operator is most often looking for here, so it must not be
    // the one a `||` turns into a dash.
    { label: "Seats", value: <TableNumberCell value={node.seats} format={fmtCount} /> },
    {
      // WHAT THIS NODE MAY RUN — `ingress`, `seats`, `workers` — which is a
      // different fact from the duties it holds. A FACT rather than a line of
      // the page's Placement card, because the Nodes table gives its Roles
      // column way under an open peek and the peek is then where a reader
      // finds them: a column hidden in favour of the peek has to be in it.
      // A SET, so the chips take two tracks and one line (`Fact.set`).
      label: "Roles",
      value: node.roles.length ? (
        <TableTagsCell tags={node.roles} max={8} />
      ) : (
        <EmptyValue label="This node advertises no roles" />
      ),
      set: true,
    },
    {
      // HOW ITS BROKER TAKES PART — a member of the embedded cluster, a leaf
      // on one, or a client of an external cluster. A fact for the same reason
      // Roles is: the table's Broker column gives way under an open peek, and
      // a column hidden in favour of the peek has to be in it.
      label: "Broker",
      value: <BrokerCell broker={node.broker} />,
    },
    version
      ? {
          label: "Version",
          // ONE TOKEN (`Fact.token`): a build string is read whole or not at
          // all, and in one fact track the clamp broke a pseudo-version as
          // "v0.0.0-" over "20260929172955-…". Two tracks on one line, the
          // whole string in the title where even two are too narrow.
          value: <InlineCode title={version}>{version}</InlineCode>,
          token: true,
        }
      : {
          label: "Version",
          value: <EmptyValue label="Unknown" />,
        },
  ];
}

/** The Lag fact's value: the standing in words, or a worded empty for none. */
function lagValue(standing: ConfigStanding) {
  switch (standing.kind) {
    case "unreported":
      return <EmptyValue label="No configuration applied yet" />;
    case "no-target":
      return <EmptyValue label="No revision activated yet" />;
    case "current":
      return "current";
    default:
      return standingWords(standing);
  }
}

/**
 * What a standing is read off, for a reader who hovers it: the epoch and
 * revision the node attempted, the one it runs, and the fleet's.
 */
function standingTitle(node: FleetNode, answer: Pick<FleetAnswer, "activation">): string {
  const target = answer.activation;
  const attempted = `attempted epoch ${node.config_epoch ?? 0}${
    node.config_revision_id ? ` (${node.config_revision_id})` : ""
  }`;
  const runs = node.config_serving_revision_id
    ? `runs ${node.config_serving_revision_id}`
    : "runs no revision";
  const fleet = target
    ? `the fleet activated epoch ${target.epoch} (${target.revision_id})`
    : "the fleet has activated nothing";
  return `${attempted}, ${runs}; ${fleet}`;
}

/** The Running panel's tone for a standing: a failed apply is critical. */
const STANDING_TONE: Record<ConfigStanding["kind"], "critical" | "positive" | "caution"> = {
  "no-target": "caution",
  unreported: "caution",
  current: "positive",
  behind: "caution",
  refused: "critical",
  degraded: "critical",
  elsewhere: "caution",
};

/**
 * What a node is doing, on its page and in the rail alike: whether it runs
 * the revision the fleet activated, the leases it holds, and the fleet-wide
 * duties that landed on it.
 */
function NodePanels({ node, answer, now }: { node: FleetNode; answer: FleetAnswer; now: number }) {
  const seatBadge = useSeatBadgeOf();
  const { open: openPeek } = usePeekControls();
  const seats = answer.seats.filter((s) => s.node === node.id);
  const duties = answer.duties.filter((d) => d.node === node.id);
  const standing = configStanding(answer, node);

  return (
    <>
      {/* THE PANEL THE ENGINE PILL'S MODAL WAS. It opened on this node alone,
          at 440 px, with no address of its own — so what it answered had to
          land somewhere addressable: what this process is doing NOW, and
          which revision it is doing it on. The second half is the question an
          operator came for; the first is the one they check next. */}
      <Card>
        <Card.Header icon={<PowerGlyph size="sm" />}>
          <Card.Title>Running</Card.Title>
        </Card.Header>
        <div className="col gap-2">
          <div className="row wrap gap-2">
            {/* THE ONE QUESTION THIS PANEL EXISTS FOR, as a state rather than
                as two numbers to subtract. `config_status` beside it is a
                different fact: a node can report `ok` about a revision two
                activations old, and `error` about the active one while it
                runs the one before — which this line names. */}
            <TableStatusCell
              glyph={standing.kind === "current" ? "●" : "○"}
              label={standingWords(standing)}
              title={standingTitle(node, answer)}
              tone={uiletTone(STANDING_TONE[standing.kind])}
            />
            <Tag variant={STATUS_TONE[node.config_status ?? ""] ?? "neutral"}>
              {node.config_status || "unknown"}
            </Tag>
            <span className="spacer" />
            <span className="t-caption">reported</span>
            <TableDateCell value={node.config_reported_at} now={now} />
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
            <TableNumberCell value={node.in_flight} format={fmtCount} />
            <span className="t-label">Up since</span>
            <TableDateCell value={node.started_at} now={now} />
          </div>
          <p className="t-caption">They differ while the node rebuilds.</p>
        </div>
      </Card>

      <Card padding="none">
        <Card.Header
          icon={<KeyGlyph size="sm" />}
          count={seats.length}
          subtitle="its own, and one per seat it holds"
        >
          <Card.Title>Leases</Card.Title>
        </Card.Header>
        <div className="row wrap gap-2" style={{ padding: "var(--spacing-3)" }}>
          <span className="t-label">Its own lease</span>
          <TableNumberCell
            value={leaseMs(node.expires_in)}
            format={fmtDuration}
            emptyLabel="Not measured"
          />
          <span className="t-caption">until expiry</span>
        </div>
        <DataGrid<FleetSeatLease>
          name="node-seats"
          rows={seats}
          rowKey={(s) => s.handle}
          defaultSort="handle"
          empty={{
            title: "This node holds no seats",
            hint: "It may be new, draining, or not in the `seats` role.",
          }}
          rowHref={(s) => peekHref({ kind: "seat", id: s.handle })}
          onRowActivate={peekRowActivation<FleetSeatLease>((s) =>
            openPeek({ kind: "seat", id: s.handle }),
          )}
          columns={[
            {
              key: "handle",
              header: "Seat",
              sortValue: (s) => s.handle,
              // NOT `linkedSeat`: it is a link and this row is one already.
              cell: (s) => <TableEntityCell {...plainSeat({ ...seatBadge(s.handle) })} />,
            },
            {
              key: "since",
              header: "Since",
              align: "right",
              shrink: true,
              sortValue: (s) => Date.parse(s.acquired_at),
              cell: (s) => <HeldSince lease={s} now={now} />,
            },
            {
              key: "ttl",
              header: "Lease",
              align: "right",
              shrink: true,
              sortValue: (s) => s.expires_in ?? null,
              cell: (s) => (
                <TableNumberCell
                  value={leaseMs(s.expires_in)}
                  format={fmtDuration}
                  emptyLabel="Not measured"
                />
              ),
            },
          ]}
        />
      </Card>

      <Card padding="none">
        <Card.Header icon={<CpuGlyph size="sm" />} count={duties.length}>
          <Card.Title>Duties</Card.Title>
        </Card.Header>
        <DataGrid<FleetDutyLease>
          name="node-duties"
          rows={duties}
          rowKey={(d) => d.duty}
          defaultSort="duty"
          empty={{
            title: "No fleet singletons are held here",
            hint: "Most nodes hold none.",
          }}
          columns={[
            {
              key: "name",
              header: "Duty",
              sortValue: (d) => d.duty,
              cell: (d) => <TableKeyCell value={d.duty} />,
            },
            {
              key: "ttl",
              header: "Lease",
              align: "right",
              shrink: true,
              sortValue: (d) => d.expires_in ?? null,
              cell: (d) => (
                <TableNumberCell
                  value={leaseMs(d.expires_in)}
                  format={fmtDuration}
                  emptyLabel="Not measured"
                />
              ),
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
      icon={<ServerGlyph size="xl" />}
      title={`No node called “${id}” holds a lease`}
      description="It may have stopped, or never existed."
    />
  );
}
