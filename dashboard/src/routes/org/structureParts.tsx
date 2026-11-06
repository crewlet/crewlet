/**
 * What Structure's tabs draw over the org: the table's status chips and the
 * chart's status legend, which count the agents the tab draws and filter by
 * one status kept in the address, the one banner a model limit stopping seats
 * raises, and what a chip matching nobody says.
 */

import {
  Button,
  EmptyState,
  SegmentedControl,
  StatusLegend,
  type StatusLegendItem,
} from "@crewlethq/ui";
import { useSectionCounts } from "~/app/Shell.tsx";
import { LimitCallout } from "~/components/LimitRemedy.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { entryLimits, limitDetail, limitTitle } from "~/lib/providerLimit.ts";
import {
  countsOf,
  type LegendFilter,
  type SeatLive,
  type StatusCounts,
  type StatusFilter,
} from "~/lib/seatLive.ts";
import { toneOf } from "~/lib/seats.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import { sameValue } from "~/protocol/equal.ts";
import type { Remedy } from "~/protocol/remedy.ts";
import { useBuilder } from "./builder/BuilderContext.tsx";
import { useStructure } from "./builder/useCharts.ts";
import { liveHandleOf, useChartFilter, useLiveValue, useStatusFilter } from "./structureLive.tsx";

/**
 * The Table tab's count: the seats the builder draws, so a lead's tab counts
 * the units they lead as the rows and chips do (it counted the whole company),
 * and nothing before the org arrives, when a 0 is a claim nobody made.
 */
export function useDrawnSeatCount(): void {
  const api = useBuilder();
  const structure = useStructure(api.state);
  const loaded = useLiveValue((live) => live?.loaded ?? false);
  let seats = 0;
  for (const view of structure.nodes.values()) if (view.type === "seat") seats += 1;
  useSectionCounts(loaded && structure.nodes.size > 0 ? { table: String(seats) } : {});
}

/** One status a reader can filter by: its words, and how many of the drawn agents it keeps. */
interface StatusItem {
  value: LegendFilter;
  label: string;
  count: (c: StatusCounts) => number;
}

/** The four states, in the order a person reads them, as both the chips and the legend key them. */
const STATE_ITEMS: readonly StatusItem[] = [
  { value: "working", label: "Working", count: (c) => c.working },
  { value: "needs", label: "Needs you", count: (c) => c.needs },
  { value: "stopped", label: "Paused or stopped", count: (c) => c.paused + c.stopped },
  { value: "idle", label: "Idle", count: (c) => c.idle },
];

/**
 * The counts over what the tab draws (a lead's draft holds the units they lead,
 * so their chips count only those), or null before the first state: a row of
 * zeros is a claim nobody made.
 */
function useDrawnCounts(): StatusCounts | null {
  const api = useBuilder();
  const structure = useStructure(api.state);
  return useLiveValue((live) => {
    if (!live?.loaded) return null;
    const drawn: SeatLive[] = [];
    for (const view of structure.nodes.values()) {
      if (view.type !== "seat") continue;
      const s = live.live.get(liveHandleOf(view));
      if (s) drawn.push(s);
    }
    return countsOf(drawn);
  }, sameValue);
}

/** The table's status chips, each a filter of what it draws and kept in the address (`show`). */
export function StatusChips() {
  const counts = useDrawnCounts();
  const [show, setShow] = useStatusFilter();
  if (!counts) return null;
  return (
    <div className="structure-chips">
      <SegmentedControl
        label="Show agents"
        semantics="radio"
        layout="chips"
        size="sm"
        value={show}
        onValueChange={(next) => setShow(next as StatusFilter)}
        options={[
          { value: "all", label: "All agents", count: counts.all },
          ...STATE_ITEMS.map((item) => ({
            value: item.value,
            label: item.label,
            count: item.count(counts),
          })),
          ...(counts.offline > 0
            ? [{ value: "offline", label: "No state yet", count: counts.offline }]
            : []),
          // NAMED APART FROM THE CONFIGURATION'S CHECK, which is about the document.
          { value: "problems", label: "Agents with problems", count: counts.problems },
        ]}
      />
    </div>
  );
}

const agentCount = (n: number) => (n === 1 ? "1 agent" : `${n} agents`);

/**
 * The chart's status legend, inside the canvas beside its controls: one item
 * per state, its dot in the tone the cards draw it in, pressed to grey every
 * seat it leaves out. It keys the four states alone (`LEGEND_FILTERS`).
 */
export function StatusLegendBar() {
  const counts = useDrawnCounts();
  const [show, setShow] = useChartFilter();
  if (!counts) return null;
  const items: StatusLegendItem[] = STATE_ITEMS.map((item) => ({
    key: item.value,
    label: item.label,
    tone: toneOf(item.value),
    count: item.count(counts),
  }));
  return (
    <StatusLegend
      label="Filter by status"
      items={items}
      value={show === "all" ? null : show}
      onValueChange={(key) => setShow(key ? (key as StatusFilter) : "all")}
      countLabel={agentCount}
    />
  );
}

const FILTER_EMPTY: Readonly<Record<Exclude<StatusFilter, "all">, string>> = {
  working: "No agent is on a turn right now.",
  needs: "Nothing needs you right now.",
  stopped: "No agent is paused or stopped.",
  idle: "No agent is idle.",
  offline: "Every agent has a state from the engine.",
  problems: "No agent has a problem.",
};

/** A chip that matches no agent says so, with the way back to every agent. */
export function FilterEmpty() {
  const [show, setShow] = useStatusFilter();
  if (show === "all") return null;
  return (
    <EmptyState
      size="compact"
      title={FILTER_EMPTY[show]}
      description="The chips above count each state; choose another, or show every agent."
      action={
        <Button size="small" variant="secondary" onClick={() => setShow("all")}>
          Show all agents
        </Button>
      }
    />
  );
}

/**
 * A model limit, said once above the seats it stops, in the Inbox's own words
 * (`lib/providerLimit.ts`), with how many seats wait for it and the remedy the
 * reader may take.
 */
/** Where a model's limit is lifted: the company's models. */
const MODELS: Remedy = { setting: "model_providers" };

export function LimitBanner() {
  const agents = useAgents();
  const now = useNow();
  const limits = entryLimits(agents);
  if (limits.length === 0) return null;
  return (
    <div className="structure-limits">
      {limits.map((limit) => (
        <LimitCallout
          key={limit.key}
          variant={limit.parked.length > 0 ? "warning" : "info"}
          title={limitTitle(limit)}
          remedy={MODELS}
          label="Open models"
        >
          {limitDetail(limit, now)}{" "}
          {limit.parked.length > 0 &&
            `${plural(limit.parked.length, "seat")} ${limit.parked.length === 1 ? "waits" : "wait"} for it.`}
        </LimitCallout>
      ))}
    </div>
  );
}
