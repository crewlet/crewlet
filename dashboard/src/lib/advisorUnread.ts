/**
 * What each source the advisor could not read leaves out (`unread` on `GET /advisor`),
 * in the words Settings › Advisor and the attention queue both use.
 */

const UNREAD_WORDS: Readonly<Record<string, string>> = {
  integrations:
    "The integrations' last check couldn't be read, so anything they need from you is missing.",
  work: "The last 7 days of work couldn't be read, so the patterns the engine finds in it are missing.",
  ledger:
    "The agents' turns couldn't be read, so the patterns the engine finds in them are missing.",
  pauses:
    "The agents' pauses couldn't be read, so an agent the engine paused for a failure may be missing.",
  runs: "The week's failed runs couldn't all be read, so a sandbox that ran out of memory may be missing.",
  atlas: "The atlas couldn't be read, so what in it needs a person is missing.",
  atlas_sources:
    "The atlas's sources couldn't be read, so one that stopped reading may be missing.",
  alarms:
    "The engine's alarms couldn't be read, as right after a start, so whether a backup is missing can't be said yet.",
};

/** One sentence per unread source, in the answer's order; a source with no words of its own is named. */
export function unreadSentences(unread: readonly string[]): string[] {
  return unread.map(
    (key) =>
      UNREAD_WORDS[key] ?? `The advisor couldn't read ${key}, so what it would raise is missing.`,
  );
}
