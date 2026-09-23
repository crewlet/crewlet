/**
 * The older-pages walk, and the one rule a page in flight has to keep: it
 * lands in the walk that asked for it or nowhere.
 *
 * A fetch outlives the render that started it, and the reader can end the walk
 * while it is out — by asking another question, or by going back to the
 * newest. An answer appended after that put the old question's rows, its
 * frozen first page and its cursor back under the new one, and froze it.
 */

import { act, renderHook } from "@testing-library/react";
import { useLayoutEffect } from "react";
import { expect, test } from "vitest";
import { useOlderPages } from "./paging.ts";
import type { OlderPage } from "./paging.ts";

type Row = { id: string };
const keyOf = (row: Row) => row.id;
const rows = (...ids: string[]): Row[] => ids.map((id) => ({ id }));

/** A fetch the test answers when it chooses to. */
function deferred() {
  let answer!: (page: OlderPage<Row, string>) => void;
  let refuse!: (err: Error) => void;
  const promise = new Promise<OlderPage<Row, string>>((resolve, reject) => {
    answer = resolve;
    refuse = reject;
  });
  return { fetch: () => promise, answer, refuse };
}

function mount(restart: unknown) {
  return renderHook(({ q }) => useOlderPages<Row, string>(keyOf, q), {
    initialProps: { q: restart },
  });
}

test("an older page freezes the first page it continues", async () => {
  const { result } = mount("all");
  const page = deferred();
  let done!: Promise<void>;
  act(() => {
    done = result.current.loadOlder(rows("a1", "a2"), "c1", page.fetch);
  });
  expect(result.current.paging).toBe(true);
  await act(async () => {
    page.answer({ rows: rows("a2", "a3"), next: "c2" });
    await done;
  });
  expect(result.current.frozen).toBe(true);
  expect(result.current.paging).toBe(false);
  // THE FROZEN PAGE, whatever the live one has since become — and each row once.
  expect(result.current.rows(rows("new", "a1")).map(keyOf)).toEqual(["a1", "a2", "a3"]);
  expect(result.current.more(null)).toBe(true);
});

test("a new question never commits a render holding the old one's pages", async () => {
  // WHAT EACH COMMITTED RENDER DREW, recorded before anything is painted: a
  // walk ended in an effect instead commits one render with the old pages.
  const drawn: { q: string; ids: string[] }[] = [];
  const { result, rerender } = renderHook(
    ({ q }) => {
      const pager = useOlderPages<Row, string>(keyOf, q);
      useLayoutEffect(() => {
        drawn.push({ q, ids: pager.rows(rows(`${q}-first`)).map(keyOf) });
      });
      return pager;
    },
    { initialProps: { q: "all" } },
  );
  await act(() =>
    result.current.loadOlder(rows("all-first"), "c1", async () => ({
      rows: rows("all-older"),
      next: "c2",
    })),
  );
  rerender({ q: "unread" });
  expect(drawn.filter((d) => d.q === "unread").map((d) => d.ids)).toEqual([["unread-first"]]);
});

test("a page that answers after the question changed is dropped", async () => {
  const { result, rerender } = mount("all");
  const page = deferred();
  let done!: Promise<void>;
  act(() => {
    done = result.current.loadOlder(rows("a1", "a2"), "c1", page.fetch);
  });
  rerender({ q: "unread" });
  expect(result.current.rows(rows("u1")).map(keyOf)).toEqual(["u1"]);
  expect(result.current.paging).toBe(false);
  await act(async () => {
    page.answer({ rows: rows("a0-old-scope"), next: "old-cursor" });
    await done;
  });
  expect(result.current.frozen).toBe(false);
  expect(result.current.rows(rows("u1")).map(keyOf)).toEqual(["u1"]);
  expect(result.current.paging).toBe(false);
  // THE NEW QUESTION'S OWN CURSOR, not the one the old answer carried.
  expect(result.current.more(null)).toBe(false);
});

test("a page that answers after 'back to the newest' is dropped", async () => {
  const { result } = mount("all");
  const page = deferred();
  let done!: Promise<void>;
  act(() => {
    done = result.current.loadOlder(rows("a1"), "c1", page.fetch);
  });
  act(() => result.current.backToNewest());
  await act(async () => {
    page.answer({ rows: rows("a0"), next: "c2" });
    await done;
  });
  expect(result.current.frozen).toBe(false);
  expect(result.current.rows(rows("b1")).map(keyOf)).toEqual(["b1"]);
});

test("a refusal that answers after the question changed is dropped", async () => {
  const { result, rerender } = mount("all");
  const page = deferred();
  let done!: Promise<void>;
  act(() => {
    done = result.current.loadOlder(rows("a1"), "c1", page.fetch);
  });
  rerender({ q: "unread" });
  await act(async () => {
    page.refuse(new Error("unavailable"));
    await done;
  });
  expect(result.current.error).toBeNull();
});

test("the head is the same array for as long as the freeze lasts", async () => {
  const { result } = mount("all");
  const first = rows("a1");
  await act(() => result.current.loadOlder(first, "c1", async () => ({ rows: [], next: null })));
  expect(result.current.head(rows("later"))).toBe(first);
  act(() => result.current.backToNewest());
  const live = rows("later");
  expect(result.current.head(live)).toBe(live);
});
