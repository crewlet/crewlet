/**
 * A node's standing on the fleet's revision, in words — what the Nodes screen's
 * Running panel and Lag fact say, and what a crowded row or widget has room
 * for.
 *
 * Beside the Settings screens that say them rather than beside the rule
 * (`lib/configStanding.ts`), which the sidebar reads in the first paint: kept
 * there, every reader downloaded these sentences before the first screen
 * drew, for screens most never open.
 */

import type { ConfigStanding } from "~/lib/configStanding.ts";
import { plural } from "~/lib/format.ts";

/** The first characters of a revision id, as Settings › Configuration shows one. */
const short = (id: string) => id.slice(0, 10);

/** A standing in words, as a node's Running panel and its Lag fact say it. */
export function standingWords(standing: ConfigStanding): string {
  const runs = standing.kind !== "no-target" && standing.serving;
  switch (standing.kind) {
    case "no-target":
      return "no revision activated";
    case "unreported":
      return "no apply reported";
    case "current":
      return "running the active revision";
    case "behind":
      return `${plural(standing.epochs, "epoch")} behind`;
    case "refused":
      return runs
        ? `refused the active revision; runs ${short(standing.serving!)}`
        : "refused the active revision; runs none";
    case "degraded":
      return "applied the active revision only in part";
    case "elsewhere":
      if (standing.servesActive) return "on the active revision, from a replaced activation";
      return runs ? `on another revision, ${short(standing.serving!)}` : "on another revision";
  }
}

/**
 * The few words a crowded place has room for — a row's caption, a widget's
 * tag — or nothing for a node that is current or has no target to be behind.
 */
export function standingCaption(standing: ConfigStanding): string {
  switch (standing.kind) {
    case "behind":
      return "behind";
    case "refused":
      return "refused";
    case "degraded":
      return "partial";
    case "elsewhere":
      return "elsewhere";
    case "unreported":
      return "not reported";
    default:
      return "";
  }
}
