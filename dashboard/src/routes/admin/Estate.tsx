/**
 * The estate map: which data nodes hold each partition of the replicated
 * estate — the company's tracker, its knowledge base and their search vectors
 * — and the operator's gestures on it.
 *
 * # Layout 0 is what every fleet on this build shows
 *
 * The estate is not divided into partitions yet: every data node holds the
 * whole of it, and there is no map. The screen says so in the sentence the
 * engine renders for every surface, and lists the data nodes that hold it —
 * a node running a build from before the estate lease listed as such, since it
 * holds the whole estate all the same. There is nothing to move, so there is
 * no gesture to offer.
 *
 * # Under a map: the partitions, then the members
 *
 * One tab per space, a row per partition: how many of its copies can answer
 * against how many its target wants, its holders as chips — coloured by what
 * the MAP says each is doing, with what the node's OWN lease reports beside it
 * where the two differ, and marked where the map counts the node gone — its
 * target and the moves in force. Then the members: weight, measured share, what
 * each serves, joins and leaves, its absence counted in the maintainer's ticks,
 * probation, out, the partitions moved off it, and what its store says. The
 * hold, the layout, the epoch and the generation sit above both.
 *
 * # A barred node is readmitted HERE, where the bar is
 *
 * An eviction bars the node from the map as well as from every log it was
 * counted on, and only its READMISSION lifts the bar — once every log has taken
 * the node back; the engine refuses this screen's own put back of a barred node
 * (`barred_member`). So a barred member's row, and the line naming a barred node
 * the map does not hold, offer the readmission itself, in the retention panel's
 * dialog ([GateDialog]) — never a link to the Fleet screen. That panel offers a
 * readmission only for a node whose logs still hold its eviction, so a
 * readmission that took the node back on every log while its map part did not
 * land (coordination unreachable, a newer build's map, or a node that does not
 * serve every partition) left a node barred here and, after a reload, offered
 * nothing there but "Evict…": the bar is known only to the map, and the gesture
 * that lifts it is offered where the map is read.
 *
 * Every number is the engine's own rendering (`queries.RenderEstate`); the
 * suite reads `internal/api/testdata/estate_answer.json` for its fixtures.
 */

import { useEffect, useId, useMemo, useState } from "react";

import {
  Button,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tabs,
  Tag,
  tabId,
} from "@crewlethq/ui";
import {
  AccountTreeGlyph,
  DnsGlyph,
  LayersGlyph,
  ScheduleGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, MeterCell, NumberCell, StatusCell, TextCell } from "~/app/frame/cells.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes, fmtDateTime, plural } from "~/lib/format.ts";
import type { Tone } from "~/ui/primitives.tsx";
import type {
  EstateHolder,
  EstateLease,
  EstateMember,
  EstatePartition,
  FleetEstate,
  PlacedEstate,
  WholeEstate,
  WholeHolder,
} from "~/protocol/index.ts";
import { EstateHoldDialog, EstateMemberDialog, EstateMoveDialog } from "./EstateDialog.tsx";
import { finishable, GateDialog } from "./GateDialog.tsx";
import type { GateGesture } from "./GateDialog.tsx";

/**
 * The map has no push behind it, so it polls — at fifteen seconds, the map
 * maintainer's own tick: a holder moves at most once a tick, so a faster poll
 * would read the same map twice.
 */
const POLL_MS = 15_000;

/** A gesture the screen has open. */
type EstateGesture =
  | { kind: "member"; node: string; out: boolean }
  | { kind: "hold" }
  | { kind: "move"; partition: EstatePartition }
  | { kind: "cancel"; partition: string; node: string };

export function Estate() {
  const { data, loading, error, refetch } = useQuery("estate", undefined, { pollMs: POLL_MS });
  return (
    <>
      <PageNote>
        Which data nodes hold each partition of the estate — the company&apos;s tracker, knowledge
        base and search vectors. A copy moves make before break: a new holder serves before an old
        one lets go, and the last copy is never let go.
      </PageNote>
      {error && (
        <Callout variant="danger" role="alert">
          <span>
            This poll failed ({error}). What is below is the last reading that succeeded; do not
            read it as now.
          </span>
        </Callout>
      )}
      {loading && !data && <Skeleton variant="text" rows={4} label="Loading" />}
      {data && <EstateView estate={data} onChanged={refetch} />}
    </>
  );
}

/** The estate question's answer, in whichever of its five states it is. */
export function EstateView({
  estate,
  onChanged,
}: {
  estate: FleetEstate;
  /** Called after every gesture, so the screen re-reads the map. */
  onChanged?: () => void;
}) {
  switch (estate.state) {
    case "placed":
      return <PlacedEstateView estate={estate} onChanged={onChanged} />;
    case "whole":
      return <WholeEstateView estate={estate} />;
    case "no_map":
      return (
        <Card>
          <EmptyState
            size="compact"
            icon={<AccountTreeGlyph size="xl" />}
            title="No estate map yet"
            description={estate.detail}
          />
        </Card>
      );
    default:
      // UNAVAILABLE AND UNREADABLE, each in the engine's own sentence — a
      // store that would not answer and a newer build's map are two
      // different trips, and neither is an empty map.
      return (
        <Callout variant="warning" role="alert">
          <span>{estate.detail}</span>
        </Callout>
      );
  }
}

/**
 * Layout 0: the sentence every surface gives it, and the data nodes that each
 * hold the whole estate.
 */
function WholeEstateView({ estate }: { estate: WholeEstate }) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<AccountTreeGlyph size="sm" />}
        count={estate.holders.length}
        subtitle={`layout ${estate.layout} · one partition, ${estate.partition}`}
      >
        Estate
      </Card.Header>
      <Card.Body padding="md">
        <Callout variant="info" role="status">
          <span>{capitalized(estate.detail)}.</span>
        </Callout>
      </Card.Body>
      <DataGrid<WholeHolder>
        name="estate-whole"
        rows={estate.holders}
        rowKey={(h) => h.node}
        defaultSort="node"
        empty={{
          title: "No live data node",
          hint: "Nothing holds the estate from here: a data node that starts holds the whole of it.",
        }}
        columns={[
          {
            key: "node",
            header: "Data node",
            sortValue: (h) => h.node,
            cell: (h) => <KeyCell value={h.node} path={["admin", "fleet", h.node]} />,
          },
          {
            key: "copy",
            header: "Its copy",
            cell: (h) =>
              h.lease ? (
                <ReportCell reports={h.reports} />
              ) : (
                <EmptyValue label="It runs a build from before the estate lease, and holds the whole estate all the same" />
              ),
          },
          {
            key: "store",
            header: "Store",
            cell: (h) => <StoreCell lease={h.lease} live={h.lease !== undefined} />,
          },
          {
            key: "free",
            header: "Free",
            align: "right",
            shrink: true,
            sortValue: (h) => h.lease?.free_bytes ?? -1,
            cell: (h) =>
              h.lease ? <TextCell>{fmtBytes(h.lease.free_bytes)}</TextCell> : <EmptyValue />,
          },
        ]}
      />
    </Card>
  );
}

/** A sentence with its first letter raised — the engine's are written to follow a colon. */
function capitalized(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/**
 * The map line: layout, epoch, copies — a shortfall said in words — the
 * failure domain and the balance.
 */
export function estateSummary(e: PlacedEstate): string {
  const copies =
    e.copies < e.replicas
      ? `${e.copies} of ${e.replicas} copies of each partition — fewer data nodes than replicas`
      : `${plural(e.copies, "copy", "copies")} of each partition`;
  const spread = e.failure_domain
    ? ` · across ${plural(e.distinct_domains, e.failure_domain + " value", e.failure_domain + " values")}`
    : "";
  const b = e.balance;
  const balance = !b
    ? ""
    : b.converged
      ? ` · balanced within ${b.deviation_percent.toFixed(1)}%`
      : ` · not converged: ${b.deviation_percent.toFixed(1)}% after ${plural(b.rounds, "round")}`;
  return `layout ${e.layout} · epoch ${e.epoch} · ${copies}${spread}${balance}`;
}

/** A placed map. */
function PlacedEstateView({
  estate: e,
  onChanged,
}: {
  estate: PlacedEstate;
  onChanged?: () => void;
}) {
  const [open, setOpen] = useState<EstateGesture | null>(null);
  const [space, setSpace] = useState<string>(e.spaces[0]?.space ?? "");
  const panelId = useId();
  const changed = () => onChanged?.();
  // THE READMISSIONS STILL TO BE FINISHED, by node, held HERE rather than in
  // the dialog for the retention panel's reason ([GateGesture]): the dialog
  // is unmounted when it closes, and a readmission reopened as a fresh one
  // writes every log the first one reached again. A COMPLETE one is held too,
  // until the map no longer bars the node — between the answer and the poll
  // that shows the bar gone, the row would otherwise offer the gesture just
  // made, under a new id.
  const [readmits, setReadmits] = useState<Record<string, GateGesture>>({});
  const [readmitting, setReadmitting] = useState<string | null>(null);
  const holdReadmit = (node: string, g: GateGesture | null) => {
    setReadmits((all) => {
      const next = { ...all };
      if (g) next[node] = g;
      else delete next[node];
      return next;
    });
    if (g?.answer) changed();
  };
  // WHAT THE MAP NO LONGER BARS IS LET GO OF, whatever its readmission's
  // state: the bar is gone — this gesture's in, or one made elsewhere — so a
  // later eviction's bar is readmitted by a NEW gesture rather than this one
  // reopened.
  const barred = useMemo(() => barredNodes(e), [e]);
  useEffect(() => {
    setReadmits((all) => {
      const kept = Object.fromEntries(Object.entries(all).filter(([node]) => barred.has(node)));
      return Object.keys(kept).length === Object.keys(all).length ? all : kept;
    });
  }, [barred]);
  const rows = useMemo(() => e.partitions.filter((p) => p.space === space), [e.partitions, space]);
  const total = e.partitions.length;

  return (
    <>
      <Card padding="none">
        <Card.Header
          icon={<AccountTreeGlyph size="sm" />}
          count={total}
          subtitle={estateSummary(e)}
          actions={
            <Button variant="tertiary" size="small" onClick={() => setOpen({ kind: "hold" })}>
              {e.hold ? "Release hold" : "Hold map"}
            </Button>
          }
        >
          Estate map
        </Card.Header>
        <StatGroup columns={4}>
          <StatCard
            icon={<WarningGlyph size="xs" />}
            label="Unserved"
            value={e.unserved}
            sub={
              e.unserved
                ? "no copy can answer: every read and write routed there is refused"
                : "every partition has a copy that can answer"
            }
          />
          <StatCard
            icon={<LayersGlyph size="xs" />}
            label="Short of copies"
            value={e.short}
            sub={e.short ? "fewer copies can answer than the target has" : "every target is whole"}
          />
          <StatCard
            icon={<DnsGlyph size="xs" />}
            label="Moving"
            value={e.joining + e.leaving}
            sub={`${plural(e.joining, "joining")}, ${plural(e.leaving, "leaving")}`}
          />
          <StatCard
            icon={<ScheduleGlyph size="xs" />}
            label="Moves in force"
            value={e.moves}
            sub={`generation ${e.generation.slice(0, 8)}`}
          />
        </StatGroup>
        {(e.hold || (e.domain_limited && e.failure_domain)) && (
          <Card.Body padding="md">
            <div className="col gap-2">
              {e.hold && (
                <Callout variant="warning" role="status" icon={<ScheduleGlyph size="md" />}>
                  <span>
                    <strong>Held</strong> until {fmtDateTime(e.hold.until)} by{" "}
                    <InlineCode>{e.hold.by}</InlineCode>
                    {e.hold.reason ? ` (${e.hold.reason})` : ""}: no member is removed however long
                    it is gone.
                  </span>
                </Callout>
              )}
              {e.domain_limited && e.failure_domain && (
                <Callout variant="warning" role="status">
                  <span>
                    Fewer {e.failure_domain} values ({e.distinct_domains}) than copies ({e.copies}):
                    some partitions keep two copies in one {e.failure_domain}, and losing it costs
                    them both.
                  </span>
                </Callout>
              )}
            </div>
          </Card.Body>
        )}
      </Card>

      <Card padding="none">
        <Tabs
          ariaLabel="Spaces"
          variant="underline"
          value={space}
          onValueChange={setSpace}
          panelId={panelId}
          items={e.spaces.map((s) => ({ value: s.space, label: s.space, count: s.partitions }))}
        />
        <div
          className="tabpanel"
          role="tabpanel"
          id={panelId}
          aria-labelledby={tabId(panelId, space)}
          tabIndex={0}
        >
          <DataGrid<EstatePartition>
            name="estate-partitions"
            rows={rows}
            rowKey={(p) => p.id}
            defaultSort="id"
            isFailed={(p) => p.serving === 0}
            empty={{ title: "This space has no partition" }}
            columns={[
              {
                key: "id",
                header: "Partition",
                sortValue: (p) => p.id,
                cell: (p) => <KeyCell value={p.id} />,
              },
              {
                key: "copies",
                header: "Copies",
                shrink: true,
                sortValue: (p) => p.serving - p.wanted,
                cell: (p) => <CopiesCell partition={p} />,
              },
              {
                key: "holders",
                header: "Holders",
                cell: (p) => <HolderChips holders={p.holders} />,
              },
              {
                key: "target",
                header: "Target",
                cell: (p) => <TextCell>{p.target.join(", ")}</TextCell>,
              },
              {
                key: "moves",
                header: "Moves",
                cell: (p) =>
                  p.moves?.length ? (
                    <span className="row wrap gap-1">
                      {p.moves.map((m) => (
                        <Button
                          key={m.node}
                          variant="tertiary"
                          size="small"
                          title={`moved off ${m.node} by ${m.by}${m.reason ? ` — ${m.reason}` : ""}${
                            m.waiting
                              ? `; waiting — no other member can hold its copy, so ${m.node} holds it until one returns`
                              : ""
                          }: cancel it`}
                          onClick={() => setOpen({ kind: "cancel", partition: p.id, node: m.node })}
                        >
                          Off {m.node}
                          {m.waiting ? " · waiting" : ""} · cancel
                        </Button>
                      ))}
                    </span>
                  ) : (
                    <EmptyValue label="No move in force" />
                  ),
              },
              {
                key: "gesture",
                header: "",
                label: "Move a copy",
                shrink: true,
                cell: (p) => (
                  <Button
                    variant="tertiary"
                    size="small"
                    disabled={p.holders.length === 0}
                    onClick={() => setOpen({ kind: "move", partition: p })}
                  >
                    Move a copy
                  </Button>
                ),
              },
            ]}
          />
        </div>
      </Card>

      <Card padding="none">
        <Card.Header icon={<DnsGlyph size="sm" />} count={e.members.length}>
          Members
        </Card.Header>
        <DataGrid<EstateMember>
          name="estate-members"
          rows={e.members}
          rowKey={(m) => m.node}
          defaultSort="node"
          empty={{
            title: "The map places on no data node",
            hint: "A data node joins the map at the weight its store.estate.weight gives it.",
          }}
          columns={[
            {
              key: "node",
              header: "Data node",
              sortValue: (m) => m.node,
              cell: (m) => <KeyCell value={m.node} path={["admin", "fleet", m.node]} />,
            },
            {
              key: "domain",
              header: e.failure_domain ?? "Domain",
              label: "Failure domain",
              shrink: true,
              sortValue: (m) => m.domain ?? "",
              cell: (m) => (m.domain ? <TextCell>{m.domain}</TextCell> : <EmptyValue />),
            },
            {
              key: "weight",
              header: "Weight",
              align: "right",
              shrink: true,
              sortValue: (m) => m.weight,
              cell: (m) => <NumberCell value={m.weight} />,
            },
            {
              key: "share",
              header: "Share",
              align: "right",
              shrink: true,
              sortValue: (m) => m.share_percent,
              cell: (m) => (
                <span
                  className="t-num"
                  title="its measured share of every partition copy the map places"
                >
                  {m.share_percent.toFixed(1)}%
                </span>
              ),
            },
            {
              key: "holds",
              header: "Holds",
              shrink: true,
              sortValue: (m) => m.serving,
              cell: (m) => (
                <TextCell>
                  {m.serving} serving
                  {m.joining ? ` · ${m.joining} joining` : ""}
                  {m.leaving ? ` · ${m.leaving} leaving` : ""}
                </TextCell>
              ),
            },
            {
              key: "presence",
              header: "Presence",
              cell: (m) => <MemberPresence member={m} held={e.hold !== undefined} />,
            },
            {
              key: "store",
              header: "Store",
              cell: (m) => <StoreCell lease={m.lease} live={m.live} />,
            },
            {
              key: "gesture",
              header: "",
              label: "Take out or put back",
              shrink: true,
              cell: (m) =>
                m.barred ? (
                  <ReadmitButton node={m.node} held={readmits[m.node]} onOpen={setReadmitting} />
                ) : (
                  <span className="row gap-1">
                    {m.probation && !m.out && (
                      <Button
                        variant="tertiary"
                        size="small"
                        onClick={() => setOpen({ kind: "member", node: m.node, out: false })}
                      >
                        Put back now
                      </Button>
                    )}
                    <Button
                      variant="tertiary"
                      size="small"
                      onClick={() => setOpen({ kind: "member", node: m.node, out: !m.out })}
                    >
                      {m.out ? "Put back" : "Take out"}
                    </Button>
                  </span>
                ),
            },
          ]}
        />
        {e.barred.length > 0 && (
          <Card.Body padding="md">
            <div className="col gap-2">
              {e.barred.map((b) => (
                <Callout key={b.node} variant="warning" role="status">
                  <span className="row wrap gap-2 baseline">
                    <span>
                      <InlineCode>{b.node}</InlineCode> is barred from the estate map by {b.by}
                      {b.reason ? ` (${b.reason})` : ""}: the map does not hold it, and should it
                      come back it is placed on nothing until it is readmitted — which takes it back
                      on every log, then lifts the bar.
                    </span>
                    <ReadmitButton node={b.node} held={readmits[b.node]} onOpen={setReadmitting} />
                  </span>
                </Callout>
              ))}
            </div>
          </Card.Body>
        )}
        {e.removed.length > 0 && (
          <Card.Body padding="md">
            <div className="col gap-2">
              {e.removed.map((r) => (
                <Callout key={r.node} variant="info" role="status">
                  <span className="row wrap gap-2 baseline">
                    <span>
                      <InlineCode>{r.node}</InlineCode> was removed for being {r.reason}
                      {r.detail ? ` (${r.detail})` : ""} and has not been seen for{" "}
                      {plural(r.gone, "tick")}. The next time it is seen present and healthy it
                      rejoins on probation.
                    </span>
                    <Button
                      variant="tertiary"
                      size="small"
                      onClick={() => setOpen({ kind: "member", node: r.node, out: false })}
                    >
                      Put back now
                    </Button>
                  </span>
                </Callout>
              ))}
            </div>
          </Card.Body>
        )}
      </Card>

      {readmitting && (
        <GateDialog
          node={readmitting}
          evict={false}
          held={readmits[readmitting]}
          onHeld={(g) => holdReadmit(readmitting, g)}
          onClose={() => setReadmitting(null)}
        />
      )}
      {open?.kind === "member" && (
        <EstateMemberDialog
          node={open.node}
          out={open.out}
          onDone={changed}
          onClose={() => setOpen(null)}
        />
      )}
      {open?.kind === "hold" && (
        <EstateHoldDialog
          hold={e.hold}
          generation={e.generation}
          onDone={changed}
          onClose={() => setOpen(null)}
        />
      )}
      {open?.kind === "move" && (
        <EstateMoveDialog
          partition={open.partition.id}
          holders={open.partition.holders.map((h) => h.node)}
          onDone={changed}
          onClose={() => setOpen(null)}
        />
      )}
      {open?.kind === "cancel" && (
        <EstateMoveDialog
          partition={open.partition}
          holders={[open.node]}
          node={open.node}
          cancel
          onDone={changed}
          onClose={() => setOpen(null)}
        />
      )}
    </>
  );
}

/** A partition's copies that can answer, against the copies its target wants. */
function CopiesCell({ partition: p }: { partition: EstatePartition }) {
  const tone: Tone = p.serving === 0 ? "critical" : p.serving < p.wanted ? "caution" : "positive";
  return (
    <MeterCell
      used={p.serving}
      max={Math.max(p.wanted, 1)}
      label={`${p.serving} of ${p.wanted}`}
      tone={tone}
    />
  );
}

/** What the map says each holder is doing, as a tag's colour. */
const HOLDER_TONE: Record<string, "success" | "info" | "warning"> = {
  serving: "success",
  joining: "info",
  leaving: "warning",
};

/**
 * A partition's holders as chips: coloured by what the MAP says each is doing,
 * with what the node's own lease reports beside it where the two differ, and
 * DANGER where the map counts the node gone — a serving holder there is a copy
 * routers reach and nothing answers.
 */
export function HolderChips({ holders }: { holders: EstateHolder[] }) {
  if (holders.length === 0) return <EmptyValue label="Nobody holds it" />;
  return (
    <span className="row wrap gap-1">
      {holders.map((h) => {
        const said =
          h.reports && h.reports !== h.state ? ` · ${h.reports.replaceAll("_", " ")}` : "";
        return (
          <Tag
            key={h.node}
            size="xs"
            variant={h.able ? (HOLDER_TONE[h.state] ?? "neutral") : "danger"}
            title={
              `${h.node} is ${h.state} in the map since epoch ${h.since}` +
              (h.reports ? `; its own lease says ${h.reports}` : "; its lease says nothing of it") +
              (h.able ? "" : "; the map counts the node absent or unhealthy")
            }
          >
            {`${h.node} ${h.state}${said}${h.able ? "" : " · absent"}`}
          </Tag>
        );
      })}
    </span>
  );
}

/** What a lease says of one partition, as a status. */
function ReportCell({ reports }: { reports?: string }) {
  if (!reports) return <EmptyValue label="Its lease says nothing of it" />;
  const tone: Tone =
    reports === "serving" ? "positive" : reports === "faulted" ? "critical" : "caution";
  return (
    <StatusCell
      glyph={reports === "serving" ? "●" : "◐"}
      label={reports.replaceAll("_", " ")}
      tone={tone}
    />
  );
}

/**
 * What a node's estate lease says of its store — or that it holds none. A
 * lease that does not SAY its store is healthy is counted by the map exactly
 * as a failed one, and drawn as one here.
 */
function StoreCell({ lease, live }: { lease?: EstateLease; live: boolean }) {
  if (!live || !lease) return <EmptyValue label="It holds no estate lease" />;
  if (lease.healthy === undefined) {
    return (
      <StatusCell
        glyph="▲"
        label="unsaid"
        tone="critical"
        title="its lease does not say whether its store is healthy, which the map counts as a failed store"
      />
    );
  }
  if (!lease.healthy) {
    return (
      <StatusCell
        glyph="▲"
        label="failed"
        tone="critical"
        title={lease.detail || "its store says it has failed"}
      />
    );
  }
  if (!lease.able) {
    return (
      <StatusCell
        glyph="▲"
        label="not counted"
        tone="caution"
        title={lease.detail || "the map does not count it present: it runs another layout"}
      />
    );
  }
  return <StatusCell glyph="●" label={`ok · ${fmtBytes(lease.free_bytes)} free`} tone="positive" />;
}

/** Every node the map bars: a member barred on its row, and one it does not hold. */
function barredNodes(e: PlacedEstate): Set<string> {
  return new Set([
    ...e.members.filter((m) => m.barred).map((m) => m.node),
    ...e.barred.map((b) => b.node),
  ]);
}

/**
 * What a barred node's readmission button says, from the readmission this
 * screen holds for it — the retention panel's rule ([gateAction]) for its one
 * sign: a gesture still to be finished reopens as itself, and so does a
 * complete one whose bar the map has not dropped yet, rather than a fresh
 * gesture under a new id.
 */
function readmitLabel(held?: GateGesture): string {
  if (held && finishable(held)) return "Finish readmission…";
  if (held?.answer?.complete) return "Readmission sent…";
  return "Readmit…";
}

/**
 * How a barred node is put back: its READMISSION — never this screen's "Put
 * back", which the engine refuses for a barred node (`barred_member`). The bar
 * stands for the eviction on every log the node was counted on, and only the
 * readmission knows when those have all taken it back.
 */
function ReadmitButton({
  node,
  held,
  onOpen,
}: {
  node: string;
  held?: GateGesture;
  onOpen: (node: string) => void;
}) {
  return (
    <Button
      variant="tertiary"
      size="small"
      title={`${node} is barred by an eviction: its readmission takes it back on every log the eviction reached, then lifts the bar`}
      onClick={() => onOpen(node)}
    >
      {readmitLabel(held)}
    </Button>
  );
}

/**
 * Whether a member is there, out, on probation, moved off partitions or
 * counting ticks gone — the absence as a fraction of the ticks that remove it.
 */
function MemberPresence({ member: m, held }: { member: EstateMember; held: boolean }) {
  const parts: React.ReactNode[] = [];
  if (m.barred) {
    parts.push(
      <StatusCell
        key="out"
        glyph="⊘"
        label="barred"
        tone="caution"
        title={`barred${m.out_by ? ` by ${m.out_by}` : ""}${m.out_reason ? ` — ${m.out_reason}` : ""}: placed on nothing, whatever becomes of it, until it is readmitted`}
      />,
    );
  } else if (m.out) {
    parts.push(
      <StatusCell
        key="out"
        glyph="◌"
        label="out"
        tone="neutral"
        title={
          m.out_by
            ? `taken out by ${m.out_by}${m.out_reason ? ` — ${m.out_reason}` : ""}`
            : "taken out: placed on nothing while it serves what it holds"
        }
      />,
    );
  }
  if (m.probation) {
    const p = m.probation;
    parts.push(
      <StatusCell
        key="probation"
        glyph="◐"
        label={`probation ${p.present}/${p.placed_after_ticks}`}
        tone="caution"
        title={`removed for being ${p.reason} and back: placed on nothing until it has been present ${p.placed_after_ticks} ticks in a row`}
      />,
    );
  }
  if (m.moved_off?.length) {
    parts.push(
      <StatusCell
        key="moved"
        glyph="↷"
        label={`moved off ${m.moved_off.length}`}
        tone="neutral"
        title={`an operator moved ${m.moved_off.join(", ")} off it`}
      />,
    );
  }
  const a = m.absence;
  if (a) {
    const back = m.live && a.present > 0;
    parts.push(
      <StatusCell
        key="absence"
        glyph={back ? "◐" : "○"}
        label={
          back
            ? `back ${a.present}/${a.clear_after_ticks}`
            : `${a.reason} ${a.ticks}/${a.out_after_ticks}`
        }
        tone="caution"
        title={
          held
            ? `counted ${a.reason} for ${a.ticks} ticks; the hold keeps it in the map past ${a.out_after_ticks}`
            : `counted ${a.reason} for ${a.ticks} of the ${a.out_after_ticks} ticks that remove it`
        }
      />,
    );
  } else if (!m.live) {
    parts.push(
      <StatusCell
        key="lease"
        glyph="○"
        label="no lease"
        tone="caution"
        title="it holds no estate lease: the map's maintainer counts it gone from its next tick"
      />,
    );
  } else if (!m.out && !m.probation) {
    parts.push(<StatusCell key="present" glyph="●" label="present" tone="positive" />);
  }
  return <span className="row gap-1">{parts}</span>;
}
