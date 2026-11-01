/**
 * The Doclet landing's arithmetic, without a screen.
 *
 * What these protect:
 *
 *  - BACK-TO-BACK SAVES ARE ONE ROW only when they are one writer's work on
 *    one page on one COMPANY day, and a comment or a new page is never folded.
 *  - THE LANDING NEVER LEADS WITH A PAGE THAT IS GONE.
 *  - A SAVE'S DIFF IS READ BETWEEN THE REVISION IT MADE AND THE ONE BEFORE IT,
 *    found by the instant both carry, and nothing is guessed where this node
 *    no longer holds either.
 *  - A ROW'S DAY IS THE COMPANY'S.
 */

import { describe, expect, test } from "vitest";
import type { PageChange, PageRevision } from "~/protocol/index.ts";
import { byDay, dayOf, foldRuns, leadOf, runPhrase, saveRevisions } from "./landing.ts";

/** A change on page `page`, at `at`, by `actor`, of `kind`. */
function change(id: string, at: string, over: Partial<PageChange> = {}): PageChange {
  return {
    id,
    page_id: "p-1",
    title: "Settlement algorithm",
    container: "FAIR",
    kind: "saved",
    actor: "swe",
    at,
    log_seq: 0,
    ...over,
  };
}

const TOKYO = "Asia/Tokyo";

describe("folding the feed into rows", () => {
  test("back-to-back saves of one page by one writer on one day are one row", () => {
    const runs = foldRuns(
      [
        change("c-3", "2026-10-11T06:52:00Z"),
        change("c-2", "2026-10-11T06:10:00Z"),
        change("c-1", "2026-10-11T02:00:00Z", { page_id: "p-2", title: "Split modes" }),
      ],
      TOKYO,
    );
    expect(runs.map((run) => run.map((c) => c.id))).toEqual([["c-3", "c-2"], ["c-1"]]);
    expect(runPhrase(runs[0]!)).toBe("saved it twice");
    expect(runPhrase(runs[1]!)).toBe("saved it");
  });

  test("three or more saves are counted", () => {
    const runs = foldRuns(
      [
        change("c-3", "2026-10-11T06:52:00Z"),
        change("c-2", "2026-10-11T06:10:00Z"),
        change("c-1", "2026-10-11T05:00:00Z"),
      ],
      TOKYO,
    );
    expect(runs).toHaveLength(1);
    expect(runPhrase(runs[0]!)).toBe("saved it 3 times");
  });

  test.each([
    ["another writer", { actor: "pm" }],
    ["another page", { page_id: "p-2" }],
    ["a comment", { kind: "comment" }],
    ["a new page", { kind: "created" }],
  ])("a save is not folded with %s", (_name, over) => {
    const runs = foldRuns(
      [change("c-2", "2026-10-11T06:10:00Z"), change("c-1", "2026-10-11T05:00:00Z", over)],
      TOKYO,
    );
    expect(runs).toHaveLength(2);
  });

  // ONE NAME PER WRITER, the one the record carries: a credential bound to no
  // seat writes under its login, which is a writer of its own beside any
  // seat, and the engine's own saves are one writer whatever name they carry.
  test("a credential's saves under its login are one writer, apart from a seat's", () => {
    const login = { actor: "ci.docs", actor_kind: "operator" };
    const saves = foldRuns(
      [change("c-2", "2026-10-11T06:10:00Z", login), change("c-1", "2026-10-11T05:00:00Z", login)],
      TOKYO,
    );
    expect(saves).toHaveLength(1);
    const beside = foldRuns(
      [
        change("c-2", "2026-10-11T06:10:00Z", login),
        change("c-1", "2026-10-11T05:00:00Z", { actor_kind: "human" }),
      ],
      TOKYO,
    );
    expect(beside).toHaveLength(2);
  });

  test("the engine's own saves are one writer", () => {
    const engine = { actor: "crewlet", actor_kind: "system" };
    const runs = foldRuns(
      [
        change("c-2", "2026-10-11T06:10:00Z", engine),
        change("c-1", "2026-10-11T05:00:00Z", engine),
      ],
      TOKYO,
    );
    expect(runs).toHaveLength(1);
  });

  // THE COMPANY'S DAY, not UTC's: 14:30 UTC on the 10th is 23:30 in Tokyo,
  // and 15:30 is half past midnight on the 11th.
  test("saves either side of the company's midnight are two rows", () => {
    const runs = foldRuns(
      [change("c-2", "2026-10-10T15:30:00Z"), change("c-1", "2026-10-10T14:30:00Z")],
      TOKYO,
    );
    expect(runs).toHaveLength(2);
    expect(
      foldRuns(
        [change("c-2", "2026-10-10T15:30:00Z"), change("c-1", "2026-10-10T14:30:00Z")],
        "UTC",
      ),
    ).toHaveLength(1);
  });

  test("a single change of each kind reads as the kind's own phrase", () => {
    const one = (kind: string) =>
      runPhrase(foldRuns([change("c", "2026-10-11T00:00:00Z", { kind })], TOKYO)[0]!);
    expect(one("created")).toBe("wrote it");
    expect(one("comment")).toBe("commented");
    expect(one("renamed")).toBe("renamed it");
  });
});

describe("the row the landing leads with", () => {
  test("is the newest whose page is still there", () => {
    const runs = foldRuns(
      [
        change("c-3", "2026-10-11T06:52:00Z", { page_id: "gone", title: "" }),
        change("c-2", "2026-10-11T06:10:00Z", { page_id: "p-2", title: "Split modes" }),
      ],
      TOKYO,
    );
    expect(leadOf(runs)).toBe(1);
  });

  test("is none when no row has a page to open", () => {
    expect(leadOf([])).toBe(-1);
    expect(leadOf(foldRuns([change("c", "2026-10-11T06:10:00Z", { title: "" })], TOKYO))).toBe(-1);
  });
});

describe("the revisions a save's diff is read between", () => {
  const history: PageRevision[] = [
    { version: 4, author: "swe", created_at: "2026-10-11T06:52:00.123456Z" },
    { version: 3, author: "swe", created_at: "2026-10-11T06:10:00Z" },
    { version: 1, author: "swe", created_at: "2026-10-10T09:00:00Z" },
  ];

  // THE ACTIVITY ROW AND THE REVISION CARRY ONE INSTANT, written by one
  // record, though each may be spelled to a different precision.
  test("are the one the save made and the one before it, found by their instant", () => {
    const got = saveRevisions(change("c-2", "2026-10-11T06:52:00.123Z"), history);
    expect(got?.made.version).toBe(4);
    expect(got?.before.version).toBe(3);
    expect(got?.current).toBe(true);
  });

  test("say when a later save has replaced the body the save made", () => {
    const got = saveRevisions(change("c-1", "2026-10-11T06:10:00Z"), history);
    expect(got).toMatchObject({ made: { version: 3 }, before: { version: 1 }, current: false });
  });

  test("are none for anything but a save, or where this node holds neither", () => {
    expect(
      saveRevisions(change("c", "2026-10-11T06:52:00.123Z", { kind: "comment" }), history),
    ).toBeNull();
    expect(saveRevisions(change("c", "2026-10-11T07:00:00Z"), history)).toBeNull();
    // THE OLDEST REVISION HELD has nothing before it on this node.
    expect(saveRevisions(change("c", "2026-10-10T09:00:00Z"), history)).toBeNull();
  });
});

describe("the day a row is drawn under", () => {
  // 09:00 on the 11th in Tokyo.
  const now = Date.parse("2026-10-11T00:00:00Z");

  test("is the company's Today, Yesterday, or the date before them", () => {
    expect(dayOf("2026-10-10T23:00:00Z", now, TOKYO)).toBe("Today");
    expect(dayOf("2026-10-10T13:00:00Z", now, TOKYO)).toBe("Yesterday");
    expect(dayOf("2026-10-09T01:00:00Z", now, TOKYO)).toMatch(/Oct 0?9/);
  });

  test("groups rows in order under the day of each row's newest change", () => {
    const runs = foldRuns(
      [
        change("c-3", "2026-10-10T23:00:00Z"),
        change("c-2", "2026-10-10T13:00:00Z", { kind: "comment" }),
        change("c-1", "2026-10-10T12:00:00Z", { page_id: "p-2", title: "Split modes" }),
      ],
      TOKYO,
    );
    expect(byDay(runs, now, TOKYO).map((d) => [d.day, d.runs.length])).toEqual([
      ["Today", 1],
      ["Yesterday", 2],
    ]);
  });
});
