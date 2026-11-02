/**
 * An engine alarm, said in words.
 *
 * # A rule, not a table
 *
 * The engine sends an alarm as its KIND, `backup_age`, which is the name its
 * log line, its gauge and the published alarm reference all use. Its name in
 * words is that identifier read as words, capitalised at the start ("Backup
 * age"), with the two initialisms the vocabulary carries kept whole ("IVF",
 * "WAL"). A table of titles here would be a second copy of the engine's alarm
 * vocabulary, and the copy that drifts is the one that names the next alarm
 * by nothing; the rule names every kind, today's and the next one's, the way
 * the engine's own log line does.
 *
 * # One alarm per kind, with its logs in it
 *
 * The report raises each kind once, so the kind IS the alarm's identity, as it
 * is on the gauge and in the log line. A condition that is a property of a log
 * names every log it stands on in `domains`, each with its own measurement,
 * rather than once per log with the log pasted into its sentence: that drew a
 * company with no backup as five alarms beside a count of two. So nothing here
 * reads a log's name out of a sentence.
 */

import { humanize } from "~/lib/format.ts";
import type { RetentionTermName } from "~/contract/retention.ts";
import type { RetentionAlarm } from "~/protocol/index.ts";

/** The initialisms in the engine's alarm vocabulary, which read as letters. */
const INITIALISMS = new Set(["ivf", "wal"]);

/** An alarm kind in words: `trim_blocked` is "Trim blocked". */
export function alarmKindTitle(kind: string): string {
  return humanize(kind)
    .split(" ")
    .map((word) => (INITIALISMS.has(word.toLowerCase()) ? word.toUpperCase() : word))
    .join(" ");
}

/** One alarm as a list names it: its kind, which is its identity, and its name. */
export interface NamedAlarm {
  key: string;
  title: string;
}

/**
 * The alarms a list names, the one that has stood LONGEST first.
 *
 * The report gives them in the engine's table order, which asserts no ranking;
 * the health push names the one that has gone unanswered longest (`worst`),
 * and that is the one a short list should lead with. The rest keep the
 * report's order.
 */
export function namedAlarms(
  alarms: readonly Pick<RetentionAlarm, "kind">[],
  worst: string | undefined,
): NamedAlarm[] {
  const named = alarms.map((alarm) => ({ key: alarm.kind, title: alarmKindTitle(alarm.kind) }));
  return [...named.filter((a) => a.key === worst), ...named.filter((a) => a.key !== worst)];
}

/** One log a trim term holds, and what the engine measured on it. */
export interface AlarmLogMeasure {
  domain: string;
  /** The log's own sentence: what it keeps past its replay window. */
  detail: string;
}

/**
 * One line of the logs an alarm stands on: the logs one trim term is holding,
 * or one measurement and the logs it was taken on.
 */
export interface AlarmLogLine {
  /** The trim term holding these logs, where the alarm is about their trim. */
  term?: RetentionTermName;
  /** The logs, in the report's own order. */
  domains: string[];
  /** The earliest instant any of them stopped advancing. */
  since?: string;
  /** The logs' measurement, on a line no term holds. */
  detail?: string;
  /**
   * Every log the term holds with its own measurement, in the report's
   * order, on a line one term holds.
   */
  logs?: AlarmLogMeasure[];
}

/**
 * The logs an alarm stands on, as the lines a screen draws.
 *
 * LOGS ONE TERM HOLDS ARE ONE LINE. Four logs waiting on one backup are one
 * fact with four names in it, and a line each would say it four times; logs
 * held by different terms are different facts and keep a line each. What the
 * grouping must not lose is each held log's OWN measurement: a log is listed
 * under a held trim only once it keeps records past its replay window, and how
 * many is a fact about that log that the earliest `since` cannot carry, so
 * every log rides on the line in `logs` with its own sentence. A measurement
 * no term holds (a log nearing its ceiling, a young fleet's missing backup) is
 * a line of its own, naming the logs the engine took it on: the engine carries
 * a sentence measured alike on several logs once, so this draws what it is
 * given rather than folding repeats of its own.
 */
export function alarmLogLines(alarm: Pick<RetentionAlarm, "domains">): AlarmLogLine[] {
  const lines: AlarmLogLine[] = [];
  for (const d of alarm.domains ?? []) {
    if (!d.term) {
      lines.push({ domains: [...d.domains], detail: d.detail });
      continue;
    }
    const measured = d.domains.map((domain) => ({ domain, detail: d.detail }));
    const line = lines.find((l) => l.term === d.term);
    if (!line) {
      lines.push({ term: d.term, domains: [...d.domains], since: d.since, logs: measured });
      continue;
    }
    line.domains.push(...d.domains);
    (line.logs ??= []).push(...measured);
    if (d.since && (!line.since || Date.parse(d.since) < Date.parse(line.since))) {
      line.since = d.since;
    }
  }
  return lines;
}

/**
 * The value of `at` that lands Settings › Backups & retention on its alarm
 * list, which is where the console lists the alarms: that screen reads it and
 * brings the list into view.
 */
export const ALARMS_AT = "alarms";

/** The address of the alarm list, as a path and its query. */
export function alarmsLink(): { path: string[]; query: Record<string, string> } {
  return { path: ["settings", "backups"], query: { at: ALARMS_AT } };
}
