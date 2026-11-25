/**
 * What each source the advisor could not read leaves out (`unread` on `GET /advisor`),
 * in the words the attention queue uses.
 */

const UNREAD_WORDS: Readonly<Record<string, string>> = {
  integrations:
    "The integrations' last check couldn't be read, so anything they need from you is missing.",
  work: "The last 7 days of work couldn't be read, so the patterns the engine finds in it are missing.",
  ledger:
    "The agents' turns couldn't be read, so the patterns the engine finds in them are missing.",
  pauses:
    "The agents' pauses couldn't be read, so an agent the engine paused for a failure may be missing.",
  runs: "The week's failed runs couldn't all be read, so a sandbox that ran out of memory or runs that ended on their round cap may be missing.",
  budget_stops:
    "The week's budget stops couldn't be read, so an agent its budget stopped may be missing.",
  merges:
    "The week's merge decisions couldn't be read, so a repository whose merges wait for want of CI may be missing.",
  atlas: "The atlas couldn't be read, so what in it needs a person is missing.",
  atlas_sources:
    "The atlas's sources couldn't be read, so one that stopped reading may be missing.",
  alarms:
    "The engine's alarms couldn't be read, as right after a start, so whether a backup is missing can't be said yet.",
  fires:
    "The week's schedule fires couldn't be read, so the patterns the engine finds in them are missing.",
  holds: "The conversations the engine holds couldn't be read, so one it holds may be missing.",
  findings: "The Advisor's findings couldn't be read, so a notice about one may be missing.",
  alarm_episodes:
    "The engine's alarm history couldn't be read, so an alarm that keeps coming back may be missing.",
  flow: "How long work items stood in each state couldn't be read, so work that has stopped moving may be missing.",
  schedule_skips:
    "The schedules' skipped runs couldn't be read, so a schedule that keeps skipping may be missing.",
  staffing:
    "The agents' busy and stopped time couldn't be read in full, so advice to add or remove an agent may be missing.",
  capacity:
    "The agents' waits on their models' usage limits couldn't be read, so capacity lost to a limit may be missing.",
  growth_signals:
    "What the nodes met at the growth vendors couldn't be read, so a clock off from a vendor's, or a vendor API a node was told is retiring, may be missing.",
  oauth_grants:
    "The company's OAuth sign-ins couldn't be read, so a client that still lists an earlier address of this engine may be missing.",
};

/** What each unread source is called in a sentence that names it. */
const UNREAD_NAMES: Readonly<Record<string, string>> = {
  integrations: "the integrations' last check",
  work: "the last 7 days of work",
  ledger: "the agents' turns",
  pauses: "the agents' pauses",
  runs: "the week's failed runs",
  budget_stops: "the week's budget stops",
  merges: "the week's merge decisions",
  atlas: "the atlas",
  atlas_sources: "the atlas's sources",
  alarms: "the engine's alarms",
  fires: "the week's schedule fires",
  holds: "the conversations the engine holds",
  findings: "the Advisor's findings",
  alarm_episodes: "the engine's alarm history",
  flow: "how long work items stood in each state",
  schedule_skips: "the schedules' skipped runs",
  staffing: "the agents' busy and stopped time",
  capacity: "the agents' waits on their models' usage limits",
  growth_signals: "what the nodes met at the growth vendors",
  oauth_grants: "the company's OAuth sign-ins",
};

/**
 * What the advisor could not read, named: "The advisor couldn't read the
 * engine's alarms and the last 7 days of work". "Couldn't read everything"
 * said nothing a reader could act on or check against what was missing.
 */
/** Every unread word this build can name, for the gate that holds it to the engine's. */
export const UNREAD_KNOWN: readonly string[] = Object.keys(UNREAD_WORDS);

export function unreadTitle(unread: readonly string[]): string {
  return `The advisor couldn't read ${unreadNames(unread)}`;
}

/** What could not be read, named and joined: "the atlas and the agents' turns". */
export function unreadNames(unread: readonly string[]): string {
  const names = unread.map((key) => UNREAD_NAMES[key] ?? key);
  return names.length <= 1
    ? (names[0] ?? "everything")
    : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

/** One sentence per unread source, in the answer's order; a source with no words of its own is named. */
export function unreadSentences(unread: readonly string[]): string[] {
  return unread.map(
    (key) =>
      UNREAD_WORDS[key] ?? `The advisor couldn't read ${key}, so what it would raise is missing.`,
  );
}
