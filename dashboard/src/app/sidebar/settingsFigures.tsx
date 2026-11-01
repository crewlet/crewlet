/**
 * The figures beside Settings' sections, and the two answers they come from.
 *
 * # Polled only while Settings is open
 *
 * `fleet` and `retention` are guarded answers with no push behind them, so a
 * figure drawn from one is a poll. The frame is mounted for
 * the life of the tab and React will not let a hook be called conditionally,
 * so the hook runs on every screen — and a hook that polled unconditionally
 * would ask the engine two questions on a timer for the whole life of every
 * tab, on screens that draw none of the answers, and ask a reader without
 * Admin two refusals on the same timer. So each query is ENABLED only while
 * the Settings column is on screen and the section its figure is drawn beside
 * is open to the reader (`nav.ts`'s `sectionOpen`: Nodes and Backups take
 * Admin, which is the answers' own); everywhere else the hook asks nothing.
 *
 * # What each figure is
 *
 *  - **Nodes** — the nodes holding a presence lease, off the health push the
 *    frame already holds. The fleet answer adds the one thing health cannot
 *    say, because health is about THIS node: how many nodes do not run the
 *    revision the fleet activated — the Nodes screen's own "Behind on config"
 *    count, by its one rule (`configStanding`) — drawn as a warning mark and
 *    said in the row's name.
 *  - **Configuration** — the epoch this node applied, in words.
 *  - **Backups & retention**: how many alarms stand, the count the engine
 *    panel shows and the section's own list leads with, drawn only when one
 *    does. It counted the logs whose trim is held, which put a 4 here beside
 *    the rail's 5 for one missing backup.
 *
 * ABSENT IS NOT ZERO throughout: an answer that has not arrived, or was
 * refused, draws no figure rather than a 0.
 */

import { type ReactNode } from "react";
import { StatusDot } from "@crewlethq/ui";

import type { EngineHealth } from "~/contract/health.ts";
import type { FleetAnswer, RetentionReport } from "~/protocol/index.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { offRevision } from "~/lib/configStanding.ts";
import { sectionOpen, workspaceRow } from "../nav.ts";

/** One row's figure, in the column's own three shapes. */
export interface SectionFigure {
  /** Something waiting on the reader, UNREAD — the accent's filled pill. */
  badge?: { value: number | string; label: string };
  /** How many of something the section holds — a quiet figure. */
  count?: { value: number | string; label: string; mark?: ReactNode };
}

/**
 * The cadences, each the one the answer's own screen reasons from.
 *
 * FLEET at 30 s: a node falling behind the activated epoch is a fact for a
 * glance at the column, not a race against the lease TTL — Nodes itself polls
 * at 15 s for that, and while a Nodes screen is open the column draws THAT
 * screen's reading (`usePublishFleet`) rather than polling beside it.
 * RETENTION at 60 s: a domain's floor moves on the trim's own tick, which is
 * minutes.
 */
export const SETTINGS_POLL_MS = {
  fleet: 30_000,
  retention: 60_000,
} as const;

/** The figures, from whichever answers are in hand. Pure, so it is tested alone. */
export function settingsFigures({
  health,
  fleet,
  retention,
}: {
  health: EngineHealth | null | undefined;
  fleet: FleetAnswer | null | undefined;
  retention: RetentionReport | null | undefined;
}): Record<string, SectionFigure> {
  const figures: Record<string, SectionFigure> = {};

  if (health?.nodes !== undefined) {
    // THE NODES SCREEN'S OWN "Behind on config" TILE, to the node: one that
    // does not run the activated revision (`configStanding`). A refused apply
    // reports the TARGET's epoch while the node runs the one before, so an
    // epoch comparison left it out; it is counted here.
    const behind = fleet ? fleet.nodes.filter((n) => offRevision(fleet, n)).length : 0;
    // ONE NODE IS "1 node live", as Backups' row below says its one domain:
    // the figure is read into the row's name.
    const live = health.nodes === 1 ? "node live" : "nodes live";
    figures.nodes = {
      count: {
        value: health.nodes,
        label: behind > 0 ? `${live}, ${behind} behind on config` : live,
        mark: behind > 0 ? <StatusDot tone="warning" /> : undefined,
      },
    };
  }

  // "epoch 2", not "e2": a figure beside a row says what it counts, and a
  // letter and a number is a code the reader has to be told.
  if (health) {
    figures.config = {
      count: {
        value: `epoch ${health.applied_epoch}`,
        label: "the configuration this node applied",
      },
    };
  }

  // THE STANDING ALARMS, the same count the engine panel shows and the
  // section's own list leads with. It counted the logs whose trim is held,
  // which put a 4 here beside a 5 in the rail for one missing backup.
  const alarms = retention?.alarms.length ?? 0;
  if (alarms > 0) {
    figures.backups = {
      count: {
        value: alarms,
        label: alarms === 1 ? "standing alarm" : "standing alarms",
        mark: <StatusDot tone="warning" />,
      },
    };
  }

  return figures;
}

/**
 * Whether a reader holding `grants` may open the Settings section `key` — by
 * the one rule the column draws its locks by (`sectionOpen`), so a figure is
 * never asked for beside a section the column locks: each of these answers is
 * Admin's, and a refusal on a timer is a refusal nobody reads.
 */
function opens(key: string, grants: readonly string[]): boolean {
  const section = workspaceRow("settings")?.sections.find((s) => s.key === key);
  return section !== undefined && sectionOpen(section, grants);
}

/**
 * The column's figures, asking each guarded answer only while `here` (the
 * Settings column is drawn) and only of a reader its section opens for. See
 * the file doc.
 */
export function useSettingsSidebar({
  here,
  grants,
  health,
  published,
}: {
  here: boolean;
  /** What the reader holds — the frame's `useHeldGrants`, empty while nobody has said. */
  grants: readonly string[];
  health: EngineHealth | null | undefined;
  /** The `fleet` answer a screen on the page already polls — see
   *  `usePublishFleet`. While one is published the column asks nothing. */
  published: { answer: FleetAnswer | null } | null;
}): Record<string, SectionFigure> {
  const nodes = here && opens("nodes", grants);
  const backups = here && opens("backups", grants);
  const fleet = useQuery("fleet", undefined, {
    enabled: nodes && published === null,
    pollMs: SETTINGS_POLL_MS.fleet,
  });
  const retention = useQuery("retention", undefined, {
    enabled: backups,
    pollMs: SETTINGS_POLL_MS.retention,
  });
  const figures = settingsFigures({
    health,
    fleet: !nodes ? null : published ? published.answer : fleet.data,
    retention: backups ? retention.data : null,
  });
  // A LOCKED ROW DRAWS NO FIGURE, whatever answer it would come from: Nodes
  // and Configuration read the health push every reader holds, and drew "1"
  // and "epoch 1" beside their locks while the equally locked Backups row
  // drew nothing. Every locked row is drawn the same way.
  return Object.fromEntries(Object.entries(figures).filter(([key]) => opens(key, grants)));
}
