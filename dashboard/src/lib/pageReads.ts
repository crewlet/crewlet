/**
 * How a page's readers are said: one line per seat and way of reading, in the
 * words the approved Knowledge page uses — "turn 2 on ENG-412", `search:
 * “DHCP lease”` — and the faces of who read it today.
 *
 * PURE, like `lib/seats.ts`: the counts are the engine's, and what this file
 * adds is only the phrasing, which is what a suite can hold.
 *
 * TWO ANSWERS, TWO READERS. `page.readers` is who read a page and how lately,
 * which every member is served and the page itself draws; `page_reads` is
 * also HOW — the turn each read was in, the words each search used — which is
 * what the machine processed and only an admin is served (ADR-0031), so only
 * the console's view of a page draws it (`routes/admin/agents/PageReads.tsx`).
 */

import type { PageReadRow, SkillLoad, TurnPlace } from "~/contract/pages.ts";
import type { PageReaders } from "~/protocol/index.ts";
import { companyMidnight } from "./range.ts";
import { plural } from "./format.ts";

/**
 * How one reader reached the page, as a phrase.
 *
 * A DIRECT READ NAMES ITS TURN, because "why did this seat open the runbook"
 * is answered by the work it was doing: the run's task and its "Turn n" there,
 * the count the task's own page draws. A SEARCH NAMES ITS QUERY, because the
 * page came up for words the seat chose — which is how a page that keeps
 * surfacing for the wrong question is found.
 */
export function readVia(reader: PageReadRow): string {
  const on = reader.last_work_item ? turnOn(reader.last_work_item) : "";
  const query = reader.last_query?.trim() ? `“${reader.last_query.trim()}”` : "";
  switch (reader.via) {
    case "get_page":
      return on || "opened the page";
    case "search":
      return query ? `search: ${query}` : "found it in a search";
    case "prefetch":
      return query ? `turn-start search: ${query}` : "given it at turn start";
    case "skill_loaded":
      return on ? `loaded as a skill, ${on}` : "loaded as a skill";
    default:
      // A WAY OF READING THIS BUILD HAS NO WORDS FOR is still a read, and its
      // own name is what a reader can look up.
      return reader.via;
  }
}

/**
 * "turn 2 on ENG-412" — or "a turn on ENG-412" when the run's counted row is
 * not the one the engine found, so its ordinal is unknown rather than 0.
 */
function turnOn(item: TurnPlace): string {
  return item.ordinal > 0 ? `turn ${item.ordinal} on ${item.key}` : `a turn on ${item.key}`;
}

/** "3 reads" beside a reader who came back — nothing for a single read. */
export function readCount(reader: PageReadRow): string {
  return reader.count > 1 ? plural(reader.count, "read") : "";
}

/**
 * The seats whose newest read of the page fell on the company's today, most
 * recent first and each once — the faces beside the engine's count.
 *
 * THE COUNT IS NEVER THIS LIST'S LENGTH: the engine counts every seat that
 * read today, including one whose row the list's cap left out, and the
 * number drawn is always its `distinct_seats_today`.
 */
export function readToday(
  readers: readonly { handle: string; last_at: string }[],
  now: number,
  zone: string | undefined,
): string[] {
  const since = companyMidnight(now, zone);
  const out: string[] = [];
  for (const reader of readers) {
    if (Date.parse(reader.last_at) >= since && !out.includes(reader.handle)) {
      out.push(reader.handle);
    }
  }
  return out;
}

/**
 * How many agents read the page on the company's today, from the per-seat list
 * a member is served — the faces themselves, and whether their number is only
 * a FLOOR.
 *
 * A member's answer carries no count of today's readers (that is
 * `page_reads.distinct_seats_today`, an admin's), so the figure is the list's
 * own: exact while the list is whole or some listed reader is older than
 * today — the list is newest first, so every reader of today is on it — and a
 * floor when every listed reader read today and the engine counted more than
 * it listed, since one it left out may have read today too.
 */
export function readersToday(
  readers: PageReaders,
  now: number,
  zone: string | undefined,
): { faces: string[]; floor: boolean } {
  const faces = readToday(readers.seats, now, zone);
  return {
    faces,
    floor: faces.length === readers.seats.length && readers.total > readers.seats.length,
  };
}

/**
 * Who a tool skill reached, as the one line under a page's title: "Loaded as
 * a skill by SWE, CTO +2", counting only the seats that asked for its body —
 * or, when none did, the seats whose phases were offered it.
 */
export function loadedBy(
  loads: SkillLoad[],
  name: (handle: string) => string,
  shown = 2,
): { verb: "Loaded" | "Offered"; names: string; more: number } | null {
  const loaded = loads.filter((l) => l.loaded > 0);
  const who = loaded.length > 0 ? loaded : loads.filter((l) => l.offered > 0);
  if (who.length === 0) return null;
  return {
    verb: loaded.length > 0 ? "Loaded" : "Offered",
    names: who
      .slice(0, shown)
      .map((l) => name(l.handle))
      .join(", "),
    more: Math.max(0, who.length - shown),
  };
}
