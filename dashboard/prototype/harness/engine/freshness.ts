/**
 * The freshness a state-log read is served at.
 *
 * Every tracker and knowledge-base read takes its freshness from the request
 * (`tracker.ParseFreshness`): a level the dashboard surface may ask for, the
 * position the caller's own last write landed at, and a staleness bound. An
 * absent level is the dashboard's own default, `stale` (`statelog.LevelFor`).
 * The demo's copies are applied through the head of every log, so every level
 * is satisfied as asked, and an answer states the level it was asked for, as
 * the engine's does. A malformed request is refused as the engine refuses it.
 */

import type { QueryName, ReadLevel } from "~/protocol/types.ts";
import { has, int, refuse, text, type Params } from "./params.ts";

/** The questions answered from a state log, each of which states the level it was served at. */
export const LEVELLED: ReadonlySet<QueryName> = new Set<QueryName>([
  "work_items",
  "work_item",
  "work_comments",
  "work_item_turns",
  "work_views",
  "work_saved_views",
  "work_files",
  "work_projects",
  "work_project",
  "work_workload",
  "work_activity",
  "work_my_work",
  "work_catalogue",
  "work_person",
  "work_routing",
  "work_flow",
  "work_inbox",
  "company_feed",
  "decisions",
  "pages",
  "page_activity",
]);

/** The levels the dashboard surface may ask for (`statelog.SettableLevels`). */
const DASHBOARD_LEVELS: ReadLevel[] = ["linearizable", "session", "stale", "consistent_prefix"];

/** The level a read is served at, or the refusal of a malformed freshness request. */
export function servedLevel(p: Params): ReadLevel {
  const position = text(p, "min_position").trim();
  if (position && !/^[^@]+@\d+:\d+$/.test(position)) {
    refuse(
      `min_position: "${position}" is not a log position; one reads <stream>@<generation>:<sequence>, and the answer that produced it carries the value to send back.`,
    );
  }
  const bounded = () => {
    for (const key of ["max_lag_seconds", "max_lag_seq"]) {
      if (has(p, key) && int(p, key, 0) < 0) {
        refuse(`${key} is how far behind an answer may be, and a negative bound is not one.`);
      }
    }
  };
  const level = text(p, "read_level").trim();
  if (!level) {
    bounded();
    return "stale";
  }
  if (!(DASHBOARD_LEVELS as string[]).includes(level)) {
    refuse(
      `"${level}" is not a read level: the four are linearizable, session, stale and consistent_prefix.`,
    );
  }
  if (level === "session" && !position) {
    refuse(
      "read_level=session waits for the caller's own last write and none was named: send the position that write answered with as min_position.",
    );
  }
  if (level !== "stale") {
    for (const key of ["max_lag_seq", "max_lag_seconds"]) {
      if (has(p, key)) {
        refuse(
          `${key} bounds how stale an answer may be, and read_level is ${level}, which is not a staleness bound at all.`,
        );
      }
    }
    return level as ReadLevel;
  }
  bounded();
  return level;
}
