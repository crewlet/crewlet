/**
 * The retention report's alarms as the ENGINE renders them, in each state the
 * backup alarm takes: `internal/api/testdata/retention_alarms.json`, which
 * `make retention-alarms` regenerates from the report `/query/retention`
 * answers with, over every log the engine registers.
 *
 * The backup alarm went per log — each log aged against its own backup, the
 * headline one log's own measurement, a held log's line its backup's sentence
 * followed by its hold's — while three suites went on drawing the node-wide
 * alarm they had typed for themselves, with a sentence the engine no longer
 * writes. They passed, because a fixture typed beside the screen agrees with
 * the screen whatever the engine sends. A suite drawing an alarm reads it here.
 *
 *  - `young`: no backup at all, minutes after the first boot. Every log is
 *    named, none of them held past its window, so no entry carries a term.
 *  - `held`: no backup at all, and every log's trim held by the backup term
 *    past its window, each keeping a count of its own past it.
 *  - `held_beside_a_blocked_trim`: as `held`, but the pages log is backed up
 *    and held by another term, so it is `trim_blocked`'s.
 *  - `stale`: one log's copy is older than the policy; the rest are covered.
 */

import { engineFile } from "~/test/engineFiles.ts";
import type { RetentionAlarm } from "~/protocol/index.ts";

export type AlarmScenario = "young" | "held" | "held_beside_a_blocked_trim" | "stale";

interface Golden {
  at: string;
  alarms: Record<AlarmScenario, RetentionAlarm[]>;
}

const golden = engineFile<Golden>("internal/api/testdata/retention_alarms.json");

/** The instant every scenario was evaluated at, as the clock a screen renders ages by. */
export const ENGINE_ALARMS_NOW = Date.parse(golden.at);

/** One scenario's alarms, in the report's order: a copy a case may change. */
export function engineAlarms(scenario: AlarmScenario): RetentionAlarm[] {
  const alarms = golden.alarms[scenario];
  if (!alarms) throw new Error(`the golden has no scenario named ${scenario}`);
  return structuredClone(alarms);
}

/** One scenario's alarm of one kind — the backup alarm unless a case names another. */
export function engineAlarm(scenario: AlarmScenario, kind = "backup_age"): RetentionAlarm {
  const alarm = engineAlarms(scenario).find((a) => a.kind === kind);
  if (!alarm) throw new Error(`the golden's ${scenario} scenario raises no ${kind}`);
  return alarm;
}
