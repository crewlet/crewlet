/**
 * The figures beside Settings' sections, and the three answers they come from.
 *
 * # Polled only while Settings is open
 *
 * `fleet`, `retention` and `integrations` are guarded answers with no push
 * behind them, so a figure drawn from one is a poll. The frame is mounted for
 * the life of the tab and React will not let a hook be called conditionally,
 * so the hook runs on every screen — and a hook that polled unconditionally
 * would ask the engine three questions on a timer for the whole life of every
 * tab, on screens that draw none of the answers, and ask a reader without the
 * grants three refusals on the same timer. So each query is ENABLED only while
 * the Settings column is on screen and the reader holds the grant of the
 * section its figure is drawn beside (`nav.ts` — Nodes and Backups take
 * `fleet:operate`, Integrations `config:read`, which are the answers' own);
 * everywhere else the hook asks nothing.
 *
 * # What each figure is
 *
 *  - **Integrations** — how many tools the engine's own roll-up says need a
 *    person (`tools[].state === "attention"`), as the row's ATTENTION pill:
 *    it is the one figure in this column that is something waiting on the
 *    reader, and it is a STATE rather than an unread count, so it wears the
 *    warning tone the approved Settings artboard draws it in (its "pill
 *    need") — never the accent the inbox's unread badge fills with. The
 *    roll-up is the engine's — `integration.Rollup` — so this counts the
 *    cards the screen draws amber, and never re-derives a phase.
 *  - **Nodes** — the nodes holding a presence lease, off the health push the
 *    frame already holds. The fleet answer adds the one thing health cannot
 *    say, because health is about THIS node: how many nodes are behind the
 *    epoch the fleet activated — the Nodes screen's own "Behind on config"
 *    count — drawn as a warning mark and said in the row's name.
 *  - **Configuration** — the epoch this node applied, in words.
 *  - **Backups & retention** — how many state-log domains have a trim that
 *    something is holding (`blocked_by`), drawn only when there is one: a
 *    held trim is a log that only grows.
 *
 * ABSENT IS NOT ZERO throughout: an answer that has not arrived, or was
 * refused, draws no figure rather than a 0.
 */

import { type ReactNode } from "react";
import { StatusDot } from "@crewlethq/ui";

import type { IntegrationsAnswer } from "~/contract/integrations.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { FleetAnswer, RetentionReport } from "~/protocol/index.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { grantsOpen, workspaceRow } from "../nav.ts";

/** One row's figure, in the column's own three shapes. */
export interface SectionFigure {
  /** Something waiting on the reader, UNREAD — the accent's filled pill. */
  badge?: { value: number | string; label: string };
  /**
   * Something waiting on the reader that is a STATE — tools the engine says
   * need a person — as the same pill in the warning tone. Colour is state and
   * the accent is where the reader is, so a state in the accent's fill would
   * read as a second unread count. The kit draws a figure's pill and speaks
   * it; the frame repaints this one (`.section-row-attention`).
   */
  attention?: { value: number | string; label: string };
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
 * minutes. INTEGRATIONS at 120 s: a tool moves into
 * `attention` when a reconcile pass says so, and passes follow who has to act
 * rather than a clock, so a faster poll only re-reads the same roll-up.
 */
export const SETTINGS_POLL_MS = {
  fleet: 30_000,
  retention: 60_000,
  integrations: 120_000,
} as const;

/** The figures, from whichever answers are in hand. Pure, so it is tested alone. */
export function settingsFigures({
  health,
  fleet,
  retention,
  integrations,
}: {
  health: EngineHealth | null | undefined;
  fleet: FleetAnswer | null | undefined;
  retention: RetentionReport | null | undefined;
  integrations: IntegrationsAnswer | null | undefined;
}): Record<string, SectionFigure> {
  const figures: Record<string, SectionFigure> = {};

  const attention = integrations?.tools?.filter((t) => t.state === "attention").length ?? 0;
  if (attention > 0) {
    figures.integrations = {
      attention: {
        value: attention,
        label: attention === 1 ? "needs attention" : "need attention",
      },
    };
  }

  if (health?.nodes !== undefined) {
    // THE NODES SCREEN'S OWN "Behind on config" TILE, to the node: an applied
    // epoch older than the one the activation pointer names. A refused apply
    // leaves the node on its older epoch, so it is counted here too.
    const behind = fleet
      ? fleet.nodes.filter((n) => (n.config_epoch ?? 0) < fleet.target_epoch).length
      : 0;
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
  if (health?.applied_epoch !== undefined) {
    figures.config = {
      count: {
        value: `epoch ${health.applied_epoch}`,
        label: "the configuration this node applied",
      },
    };
  }

  const held = retention?.domains.filter((d) => d.blocked_by).length ?? 0;
  if (held > 0) {
    figures.backups = {
      count: {
        value: held,
        label: held === 1 ? "domain whose trim is held" : "domains whose trim is held",
        mark: <StatusDot tone="warning" />,
      },
    };
  }

  return figures;
}

/** Whether a reader holding `grants` may open the Settings section `key`. */
function opens(key: string, grants: readonly string[]): boolean {
  return grantsOpen(workspaceRow("settings")?.sections.find((s) => s.key === key)?.grants, grants);
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
  /** What the viewer holds, as the engine reports it. */
  grants: readonly string[];
  health: EngineHealth | null | undefined;
  /** The `fleet` answer a screen on the page already polls — see
   *  `usePublishFleet`. While one is published the column asks nothing. */
  published: { answer: FleetAnswer | null } | null;
}): Record<string, SectionFigure> {
  const nodes = here && opens("nodes", grants);
  const backups = here && opens("backups", grants);
  const connect = here && opens("integrations", grants);
  const fleet = useQuery("fleet", undefined, {
    enabled: nodes && published === null,
    pollMs: SETTINGS_POLL_MS.fleet,
  });
  const retention = useQuery("retention", undefined, {
    enabled: backups,
    pollMs: SETTINGS_POLL_MS.retention,
  });
  const integrations = useQuery("integrations", undefined, {
    enabled: connect,
    pollMs: SETTINGS_POLL_MS.integrations,
    // CONNECTING HAPPENS IN ANOTHER TAB — the vendor's own app — and coming
    // back is the strongest sign the roll-up moved; see `useQuery`.
    refetchOnFocus: true,
  });
  return settingsFigures({
    health,
    fleet: !nodes ? null : published ? published.answer : fleet.data,
    retention: backups ? retention.data : null,
    integrations: connect ? integrations.data : null,
  });
}
