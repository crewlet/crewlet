/**
 * Where the company's files are placed: the object store's placement map, as
 * the Nodes screen draws it beneath the lease tables.
 *
 * A file of its own rather than a section of `Fleet.tsx`, because it is a
 * second map with its own gestures and its own dialogs, read from the same
 * `fleet` answer — the lease tables say which node runs which seat, and this
 * says which data node holds which chunk, and an operator about to stop a data
 * node needs the second as much as the first.
 */

import { useState, type ReactNode } from "react";

import {
  Button,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Meter,
  Tag,
} from "@crewlethq/ui";
import { ClockGlyph, DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, NumberCell, StatusCell, TextCell } from "~/app/frame/cells.tsx";
import { fmtDateTime, plural } from "~/lib/format.ts";
import type { Tone } from "~/ui/primitives.tsx";
import type { FleetObjectMember, FleetObjects, PlacedObjects } from "~/protocol/index.ts";
import { ObjectsHoldDialog, ObjectsMemberDialog } from "./ObjectsDialog.tsx";

/**
 * The map's epoch, how many copies of each chunk it keeps and how its slots
 * are grouped, in one line — and the SHORTFALL said in words, since a fleet
 * with fewer placeable data nodes than it asks replicas of is holding every
 * file with fewer copies than configured — and how evenly the copies follow
 * the weights, where anything has measured it.
 */
export function placementSummary(objects: PlacedObjects): string {
  const held =
    objects.copies < objects.replicas
      ? `${objects.copies} of ${objects.replicas} copies of every chunk — fewer data nodes than replicas`
      : `${plural(objects.copies, "copy", "copies")} of every chunk`;
  const spread = objects.failure_domain
    ? ` · across ${plural(objects.distinct_domains, objects.failure_domain + " value", objects.failure_domain + " values")}`
    : "";
  const balance = balanceSummary(objects);
  return `epoch ${objects.epoch} · ${held} · ${plural(objects.pgs, "group")}${spread}${balance ? ` · ${balance}` : ""}`;
}

/**
 * How evenly the map spreads its copies over the members' weights, in a few
 * words — or undefined for a map nothing has measured, which is not "even".
 *
 * NOT CONVERGED IS THE ONE AN OPERATOR OPENS THIS FOR. A balance that stops
 * short of its tolerance still writes the best map it measured, so nothing
 * else on the screen says that a member's weight is an intent the map cannot
 * keep; each member's share says which one.
 */
export function balanceSummary(objects: PlacedObjects): string | undefined {
  const b = objects.balance;
  if (!b) return undefined;
  const off = `${b.deviation_percent.toFixed(1)}%`;
  if (b.epoch !== objects.epoch) return "balance not yet measured at this epoch";
  if (b.converged) return `balanced within ${off}`;
  // No balance ran: a split's placement, measured close enough to leave.
  if (b.rounds === 0) return `within ${off} as the split placed it`;
  return `not converged: ${off} after ${plural(b.rounds, "round")}`;
}

/** A gesture the card has open. */
type ObjectsGesture = { kind: "member"; node: string; out: boolean } | { kind: "hold" };

/**
 * The object store's placement map: which data nodes hold the company's file
 * chunks, at what share, and what each of them says about its part — so an
 * operator can read off it whether the next data node may be stopped.
 *
 * THE ABSENT AND PENDING COLUMNS ARE THE ONES AN OPERATOR OPENS THIS FOR. A
 * data node that went away stays in the map for its grace, counted in the
 * maintainer's ticks, before its share is moved — so a restart moves nothing,
 * and a member counting ticks is one whose chunks are a copy short until it
 * returns. And a member whose last repair left chunks pending does not yet
 * hold everything the map places on it.
 *
 * Each state the engine names apart renders apart: an unreadable store is not
 * a fleet with no map, and neither is a map a newer build wrote. And the ONE
 * header is built once for all of them, so a count or a title changed in one
 * branch cannot leave the others behind.
 */
export function ObjectPlacement({
  objects,
  now,
  onChanged,
}: {
  objects?: FleetObjects;
  now: number;
  /** Called after every gesture, so the screen re-reads the map. */
  onChanged?: () => void;
}) {
  const [open, setOpen] = useState<ObjectsGesture | null>(null);
  if (!objects) return null;
  const placed = objects.state === "placed" ? objects : undefined;
  const changed = () => onChanged?.();
  const header = (
    <Card.Header
      icon={<DatabaseGlyph size="sm" />}
      count={placed?.members.length ?? 0}
      subtitle={placed ? placementSummary(placed) : undefined}
      actions={
        placed && (
          <Button variant="ghost" size="small" onClick={() => setOpen({ kind: "hold" })}>
            {placed.hold ? "Release hold" : "Hold map"}
          </Button>
        )
      }
    >
      Object placement
    </Card.Header>
  );
  if (!placed) {
    return (
      <Card>
        {header}
        {objects.state === "unavailable" ? (
          <Callout variant="warning" role="alert">
            <span>
              The placement map could not be read from the coordination store. Files already stored
              are where they were; this screen cannot say where that is.
            </span>
          </Callout>
        ) : objects.state === "no_map" ? (
          <EmptyState
            size="compact"
            icon={<DatabaseGlyph size="xl" />}
            title="No placement map yet"
            description="No data node has been placed on, so no file can be stored. A map is written within seconds of the first data node joining."
          />
        ) : (
          <Callout variant="warning" role="alert">
            <span>
              The placement map was written by a newer build than this one, which does not read it
              and does not overwrite it. Finish the upgrade to see it.
            </span>
          </Callout>
        )}
      </Card>
    );
  }

  const notices = placementNotices(placed);
  return (
    <Card padding="none">
      {header}
      {notices.length > 0 && (
        <Card.Body padding="md">
          <div className="col gap-2">{notices}</div>
        </Card.Body>
      )}
      <DataGrid<FleetObjectMember>
        name="object-placement"
        rows={placed.members}
        rowKey={(m) => m.node}
        defaultSort="node"
        empty={{
          title: "The map places on no data node",
          hint: "A data node joins the map at the weight its store.objects.weight gives it.",
        }}
        columns={[
          {
            key: "node",
            header: "Data node",
            sortValue: (m) => m.node,
            cell: (m) => <KeyCell value={m.node} path={["settings", "nodes", m.node]} />,
          },
          {
            key: "domain",
            // THE LABEL THE MAP SPREADS COPIES ACROSS, as the head — `zone`
            // tells a reader what the values below are where "Domain" does
            // not. The phone's card layout reads `label` instead.
            header: placed.failure_domain ?? "Domain",
            label: "Failure domain",
            shrink: true,
            sortValue: (m) => m.domain ?? "",
            cell: (m) =>
              m.domain ? (
                <TextCell>{m.domain}</TextCell>
              ) : (
                <EmptyValue
                  label={
                    placed.failure_domain
                      ? `carries no ${placed.failure_domain} label, so it is a domain of its own`
                      : "the map spreads copies across no label"
                  }
                />
              ),
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
              <span className="t-num" title="its measured share of every copy the map places">
                {m.share_percent.toFixed(1)}%
              </span>
            ),
          },
          {
            key: "presence",
            header: "Presence",
            sortValue: (m) => (m.out ? -1 : (m.absence?.ticks ?? 0)),
            cell: (m) => <MemberPresence member={m} now={now} held={placed.hold !== undefined} />,
          },
          {
            key: "health",
            header: "Store",
            shrink: true,
            sortValue: (m) => m.health?.used_percent ?? -1,
            cell: (m) => <MemberHealth member={m} />,
          },
          {
            key: "pending",
            header: "Pending",
            align: "right",
            shrink: true,
            sortValue: (m) => (placeable(m) ? (m.repair?.pending ?? -1) : (m.strays ?? -1)),
            cell: (m) => <MemberPending member={m} epoch={placed.epoch} />,
          },
          {
            key: "gesture",
            header: "",
            label: "Take out or put back",
            shrink: true,
            cell: (m) => (
              <span className="row gap-1">
                {/* A MEMBER ON PROBATION CAN BE VOUCHED FOR as well as taken
                    out: putting it back ends its probation at once, the
                    operator standing in for the ticks. */}
                {m.probation && !m.out && (
                  <Button
                    variant="ghost"
                    size="small"
                    onClick={() => setOpen({ kind: "member", node: m.node, out: false })}
                  >
                    Put back now
                  </Button>
                )}
                <Button
                  variant="ghost"
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
      {placed.removed.length > 0 && (
        <Card.Body padding="md">
          <div className="col gap-2">
            {placed.removed.map((r) => (
              <Callout key={r.node} variant="info" role="status">
                <span className="row wrap gap-2 baseline">
                  <span>
                    <InlineCode>{r.node}</InlineCode> was removed for being {r.reason}
                    {r.detail ? ` (${r.detail})` : ""} and has not been seen for{" "}
                    {plural(r.gone, "tick")}. The next time it is seen present and healthy it
                    rejoins on probation — read from at once, placed on after {r.placed_after_ticks}{" "}
                    ticks in a row; gone {Math.max(r.forget_after_ticks - r.gone, 0)} more, it is
                    forgotten and joins as a new node would.
                  </span>
                  <Button
                    variant="ghost"
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
      {open?.kind === "member" && (
        <ObjectsMemberDialog
          node={open.node}
          out={open.out}
          onDone={changed}
          onClose={() => setOpen(null)}
        />
      )}
      {open?.kind === "hold" && (
        // THE HOLD IN FORCE AS THE DIALOG OPENS decides which gesture it is
        // for; the dialog keeps that, so the re-read after its own gesture
        // lands cannot turn it into the opposite one.
        <ObjectsHoldDialog hold={placed.hold} onDone={changed} onClose={() => setOpen(null)} />
      )}
    </Card>
  );
}

/**
 * Whether the map places copies on a member: neither taken out nor on
 * probation — the one question every rule the map places by asks.
 */
function placeable(m: FleetObjectMember): boolean {
  return !m.out && !m.probation;
}

/**
 * The map-wide facts that are not a member's: a hold in force, groups a copy
 * short right now, copies that must share a failure domain — and what a
 * member's scrub found that nothing else on the card would show: chunks its
 * disk would not read are stepped past, so a count that keeps rising is the
 * only sign of a disk failing chunk by chunk.
 */
function placementNotices(placed: PlacedObjects): ReactNode[] {
  const notices: ReactNode[] = [];
  if (placed.hold) {
    notices.push(
      <Callout key="hold" variant="warning" role="status" icon={<ClockGlyph size="md" />}>
        <span>
          <strong>Held</strong> until {fmtDateTime(placed.hold.until)} by{" "}
          <InlineCode>{placed.hold.by}</InlineCode>
          {placed.hold.reason ? ` (${placed.hold.reason})` : ""}: no member is removed however long
          it is gone.
        </span>
      </Callout>,
    );
  }
  if (placed.degraded_groups > 0) {
    notices.push(
      <Callout key="degraded" variant="danger" role="alert">
        <span>
          <strong>
            {placed.degraded_groups} of {plural(placed.pgs, "group")}
          </strong>{" "}
          have a copy on a member that is gone or whose store has failed — each is a copy short
          until that member returns or the map moves it.
        </span>
      </Callout>,
    );
  }
  if (placed.domain_limited && placed.failure_domain) {
    notices.push(
      <Callout key="domains" variant="warning" role="status">
        <span>
          Fewer {placed.failure_domain} values ({placed.distinct_domains}) than copies (
          {placed.copies}): some groups keep two copies in one {placed.failure_domain}, and losing
          it costs them both.
        </span>
      </Callout>,
    );
  }
  for (const m of placed.members) {
    const sc = m.scrub;
    if (!sc) continue;
    if (sc.unreadable > 0 || sc.rotten > 0) {
      notices.push(
        <Callout key={`scrub-${m.node}`} variant="warning" role="status">
          <span>
            <InlineCode>{m.node}</InlineCode>&apos;s scrub found {plural(sc.unreadable, "chunk")}{" "}
            its disk would not read and {sc.rotten} rotten this cycle. Each one the store could
            remove is fetched again by repair from a good copy; a count of unreadable chunks that
            keeps rising is a disk failing chunk by chunk.
          </span>
        </Callout>,
      );
    }
    if (sc.error) {
      notices.push(
        <Callout key={`scrub-error-${m.node}`} variant="warning" role="status">
          <span>
            <InlineCode>{m.node}</InlineCode>&apos;s scrub stopped ({sc.error}) and retries shortly.
          </span>
        </Callout>,
      );
    }
  }
  return notices;
}

/**
 * Whether a member is there, out, or counting ticks gone — the absence as a
 * fraction of the ticks that remove it, since "gone for three minutes" means
 * nothing without "of ten".
 */
function MemberPresence({
  member: m,
  now,
  held,
}: {
  member: FleetObjectMember;
  now: number;
  held: boolean;
}) {
  const a = m.absence;
  const p = m.probation;
  const out = m.out ? (
    <StatusCell
      glyph="◌"
      label="out"
      tone="neutral"
      title={
        m.out_by
          ? `taken out by ${m.out_by}${m.out_reason ? ` — ${m.out_reason}` : ""}`
          : "taken out: placed on nothing while it serves what it holds"
      }
    />
  ) : null;
  // ON PROBATION IS NOT "PRESENT": the node is back and read from, but placed
  // on nothing until the ticks trust it — and a tick that misses it removes it
  // again, however far it had got.
  const probation = p ? (
    <>
      <StatusCell
        glyph="◐"
        label={`probation ${p.present}/${p.placed_after_ticks}`}
        tone="caution"
        title={`removed for being ${p.reason}${p.detail ? ` (${p.detail})` : ""} and back: read from and repaired from, placed on nothing until it has been present ${p.placed_after_ticks} ticks in a row (${p.present} so far); a tick that misses it removes it again`}
      />
      <TickMeter
        value={p.present}
        max={p.placed_after_ticks}
        label="Ticks present on probation"
        polarity="progress"
      />
    </>
  ) : null;
  if (!a) {
    return (
      <span className="row gap-1">
        {out}
        {probation}
        {m.live ? (
          placeable(m) && <StatusCell glyph="●" label="present" tone="positive" />
        ) : (
          <StatusCell
            glyph="○"
            label="no lease"
            tone="caution"
            title="it holds no objects lease: the map's maintainer counts it gone from its next tick"
          />
        )}
      </span>
    );
  }
  const back = m.live && a.present > 0;
  const label = back
    ? `back ${a.present}/${a.clear_after_ticks}`
    : `${a.reason} ${a.ticks}/${a.out_after_ticks}`;
  return (
    <span className="row gap-1">
      {out}
      {probation}
      <StatusCell
        glyph={back ? "◐" : "○"}
        label={label}
        tone="caution"
        title={
          back
            ? `back for ${a.present} of the ${a.clear_after_ticks} ticks that clear its absence; it keeps the ${a.ticks} ticks it counted gone until then`
            : held
              ? `counted ${a.reason} for ${a.ticks} ticks; the hold keeps it in the map past ${a.out_after_ticks}`
              : `counted ${a.reason} for ${a.ticks} of the ${a.out_after_ticks} ticks that remove it${a.detail ? ` — ${a.detail}` : ""}`
        }
      />
      <TickMeter
        value={back ? a.present : a.ticks}
        max={back ? a.clear_after_ticks : a.out_after_ticks}
        label={
          back ? "Ticks back of those that clear its absence" : "Ticks gone of those that remove it"
        }
        polarity={back ? "progress" : "spent"}
      />
      <DateCell at={a.since} now={now} />
    </span>
  );
}

/**
 * A count of the maintainer's ticks against the count that ends it, as a bar
 * beside the words that say the same: "12/40" is read, the bar is scanned.
 *
 * ALWAYS THE WARNING TONE, never the ramp's own: every count drawn here is a
 * member the map is not placing on as configured, whichever way it is heading.
 * The polarity is what differs — ticks gone fill toward a removal, ticks back
 * toward a member trusted again.
 */
function TickMeter({
  value,
  max,
  label,
  polarity,
}: {
  value: number;
  max: number;
  label: string;
  polarity: "spent" | "progress";
}) {
  return (
    <Meter
      value={value}
      max={Math.max(max, 1)}
      size="compact"
      hideLabel
      label={label}
      valueText={`${value} of ${max}`}
      tone="warning"
      polarity={polarity}
    />
  );
}

/** The member's store, as its own lease says — or nothing, when it said nothing. */
function MemberHealth({ member: m }: { member: FleetObjectMember }) {
  const h = m.health;
  if (!h) return <EmptyValue label={m.live ? "It reported no health" : "It holds no lease"} />;
  const tone: Tone =
    h.state === "ok"
      ? "positive"
      : h.state === "nearfull"
        ? "caution"
        : h.state === "full" || h.state === "failed"
          ? "critical"
          : "neutral";
  return (
    <StatusCell
      glyph={h.state === "ok" ? "●" : "▲"}
      label={`${h.state} · ${h.used_percent.toFixed(0)}%`}
      tone={tone}
      title={h.detail || `${h.used_percent.toFixed(1)}% of its volume is in use`}
    />
  );
}

/**
 * What the member still has to fetch, or — for a member the map places
 * nothing on — the strays it holds: a member taken out sheds them as the
 * others confirm them, and one on probation KEEPS them by rule, for when the
 * map places on it again. A pending count from an older epoch or an
 * unfinished pass is marked, because its zero says nothing about the groups
 * the current map moved.
 */
function MemberPending({ member: m, epoch }: { member: FleetObjectMember; epoch: number }) {
  if (!placeable(m)) {
    if (m.strays == null) return <EmptyValue label="It has not reported its strays" />;
    return m.out ? (
      <NumberCell
        value={m.strays}
        suffix=" strays"
        title="copies it holds beyond what the map places on it; it may be stopped once this is 0 and no member has a chunk pending"
      />
    ) : (
      <NumberCell
        value={m.strays}
        suffix=" kept"
        title="copies it held when it went, kept while it is on probation: the map gives it most of the same groups back once it is trusted"
      />
    );
  }
  const r = m.repair;
  if (!r) return <EmptyValue label="No repair pass reported yet" />;
  const stale = r.epoch !== epoch || !r.completed;
  return (
    <span className="row gap-1" style={{ justifyContent: "flex-end" }}>
      <NumberCell
        value={r.pending}
        title={`${r.held} of ${r.placed} placed chunks held after its last pass${
          r.unreachable ? `; ${r.unreachable} it could not fetch` : ""
        }`}
      />
      {stale && (
        <Tag
          variant="warning"
          title={
            r.completed
              ? `its last pass ran at epoch ${r.epoch}, not the map's ${epoch}`
              : `its last pass at epoch ${r.epoch} did not reach every group`
          }
        >
          {r.completed ? `epoch ${r.epoch}` : "unfinished"}
        </Tag>
      )}
    </span>
  );
}
