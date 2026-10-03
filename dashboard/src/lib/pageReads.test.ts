/**
 * How a page's readers are said. The counts are the engine's; what is held
 * here is the phrasing, and the one derivation the page does itself — which
 * faces stand beside the engine's "today" — cut on the COMPANY's midnight.
 */

import { expect, test } from "vitest";
import { loadedBy, readToday, readVia } from "./pageReads.ts";
import type { PageReadRow } from "~/contract/pages.ts";

const row = (over: Partial<PageReadRow>): PageReadRow => ({
  handle: "swe",
  via: "get_page",
  count: 1,
  last_at: "2026-09-24T16:00:00Z",
  ...over,
});

test("a direct read names its turn and task; a search names its words", () => {
  const item = { id: "t", key: "ENG-412", title: "Retry", ordinal: 2 };
  expect(readVia(row({ last_work_item: item }))).toBe("turn 2 on ENG-412");
  expect(readVia(row({ last_work_item: { ...item, ordinal: 0 } }))).toBe("a turn on ENG-412");
  expect(readVia(row({}))).toBe("opened the page");
  expect(readVia(row({ via: "search", last_query: " DHCP lease " }))).toBe("search: “DHCP lease”");
  expect(readVia(row({ via: "prefetch", last_query: "rack" }))).toBe("turn-start search: “rack”");
  expect(readVia(row({ via: "skill_loaded" }))).toBe("loaded as a skill");
  // A WAY THIS BUILD HAS NO WORDS FOR is still said, by its own name.
  expect(readVia(row({ via: "from_the_future" }))).toBe("from_the_future");
});

test("today's faces are cut on the company's midnight, not the browser's", () => {
  // 09:00 on the 25th in Tokyo; 00:00 UTC.
  const now = Date.parse("2026-09-25T00:00:00Z");
  const readers = [
    row({ handle: "swe", last_at: "2026-09-24T16:00:00Z" }), // 01:00 on the 25th in Tokyo
    row({ handle: "cto", last_at: "2026-09-24T14:00:00Z" }), // 23:00 on the 24th in Tokyo
    row({ handle: "swe", via: "search", last_at: "2026-09-24T17:00:00Z" }),
  ];
  expect(readToday(readers, now, "Asia/Tokyo")).toEqual(["swe"]);
  // On UTC's clock the same reads are all on the 24th — yesterday.
  expect(readToday(readers, now, "UTC")).toEqual([]);
});

test("a skill says who loaded it, or who it was offered to when nobody did", () => {
  const name = (h: string) => h.toUpperCase();
  const at = "2026-09-24T16:00:00Z";
  expect(
    loadedBy(
      [
        { handle: "swe", last_at: at, count: 3, loaded: 1, offered: 2 },
        { handle: "pm", last_at: at, count: 9, loaded: 0, offered: 9 },
        { handle: "cto", last_at: at, count: 1, loaded: 1, offered: 0 },
        { handle: "ceo", last_at: at, count: 1, loaded: 1, offered: 0 },
      ],
      name,
    ),
  ).toEqual({ verb: "Loaded", names: "SWE, CTO", more: 1 });
  expect(loadedBy([{ handle: "pm", last_at: at, count: 9, loaded: 0, offered: 9 }], name)).toEqual({
    verb: "Offered",
    names: "PM",
    more: 0,
  });
  expect(loadedBy([], name)).toBeNull();
});
