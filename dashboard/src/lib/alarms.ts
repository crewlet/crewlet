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
 * # An alarm raised per domain names the domain
 *
 * One kind raised on the tracker's log and on the pages' log is two alarms,
 * so its name says which: "Trim blocked: tracker". The retention report
 * writes such an alarm's detail as `{domain}: {detail}`, and the domain is
 * recognised against the domains the SAME report lists, never by the shape of
 * the text: a volume's path is also a word before a colon.
 */

import { humanize } from "~/lib/format.ts";

/** The initialisms in the engine's alarm vocabulary, which read as letters. */
const INITIALISMS = new Set(["ivf", "wal"]);

/** An alarm kind in words: `trim_blocked` is "Trim blocked". */
export function alarmKindTitle(kind: string): string {
  return humanize(kind)
    .split(" ")
    .map((word) => (INITIALISMS.has(word.toLowerCase()) ? word.toUpperCase() : word))
    .join(" ");
}

/** The domain an alarm was raised on, where its detail names one the report lists. */
export function alarmDomain(
  detail: string | undefined,
  domains: readonly string[],
): string | undefined {
  if (!detail) return undefined;
  return domains.find((domain) => domain !== "" && detail.startsWith(`${domain}: `));
}

/** An alarm's name: its kind in words, and the domain it was raised on. */
export function alarmTitle(
  alarm: { kind: string; detail?: string },
  domains: readonly string[] = [],
): string {
  const title = alarmKindTitle(alarm.kind);
  const domain = alarmDomain(alarm.detail, domains);
  return domain ? `${title}: ${domain}` : title;
}

/** One alarm as a list names it: a key that tells two of one kind apart, and its name. */
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
 * report's order. The KEY is the kind and the detail together, because a kind
 * raised per domain arrives once per domain under one kind.
 */
export function namedAlarms(
  alarms: readonly { kind: string; detail: string }[],
  domains: readonly string[],
  worst: string | undefined,
): NamedAlarm[] {
  const named = alarms.map((alarm) => ({
    key: `${alarm.kind}:${alarm.detail}`,
    title: alarmTitle(alarm, domains),
    leads: alarm.kind === worst,
  }));
  return [...named.filter((a) => a.leads), ...named.filter((a) => !a.leads)].map(
    ({ key, title }) => ({ key, title }),
  );
}

/**
 * The value of `at` that lands Settings › Backups & retention on its alarm
 * list, which is where the console lists the alarms: that screen reads it and
 * brings the list into view, below the backups and the banners where a reader
 * arriving at the top of the page had to find it.
 */
export const ALARMS_AT = "alarms";

/** The address of the alarm list, as a path and its query. */
export function alarmsLink(): { path: string[]; query: Record<string, string> } {
  return { path: ["settings", "backups"], query: { at: ALARMS_AT } };
}
