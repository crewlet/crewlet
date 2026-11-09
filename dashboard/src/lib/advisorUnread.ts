/**
 * What each source the advisor could not read leaves out (`unread` on
 * `GET /advisor`), in the words Settings › Advisor and the attention queue
 * both say it with, so a short list never reads as all clear in either.
 */

const UNREAD_WORDS: Readonly<Record<string, string>> = {
  integrations:
    "The integrations' last check couldn't be read, so anything they need from you is missing.",
  work: "The last 7 days of work couldn't be read, so the patterns the engine finds in it are missing.",
  ledger:
    "The agents' turns couldn't be read, so the patterns the engine finds in them are missing.",
  alarms:
    "The engine's alarms haven't been checked since it started, so whether a backup is missing can't be said yet.",
};

/** One sentence per unread source, in the answer's order; a source with no words of its own is named. */
export function unreadSentences(unread: readonly string[]): string[] {
  return unread.map(
    (key) =>
      UNREAD_WORDS[key] ?? `The advisor couldn't read ${key}, so what it would raise is missing.`,
  );
}
