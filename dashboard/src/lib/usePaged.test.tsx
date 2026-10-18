/**
 * A paged history whose newest page keeps moving.
 *
 * THE ONE THING IT MUST NOT DO IS DRAW A HOLE AS CONTINUITY. A held page
 * begins where the newest page ended when it was asked for, and every arrival
 * at the top pushes the oldest entry off the newest page's foot — onto no
 * page at all, unless the hook notices the two no longer meet. These cases
 * are full pages over a keyset cursor, the shape every caller's history has:
 * the cursor is the last entry of a page, and the page before it is whatever
 * is older than that entry.
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { useLayoutEffect, useState } from "react";
import { expect, test, vi } from "vitest";

import { usePaged } from "./usePaged.ts";
import type { QueryResult } from "./useQuery.ts";

/** How many entries one page holds. */
const PAGE = 5;
const PARAMS = { limit: PAGE };

interface Answer {
  entries: { id: string }[];
  next: string;
}

const pick = (answer: Answer) => answer.entries;
const idOf = (entry: { id: string }) => entry.id;
const cursorOf = (answer: Answer) => answer.next;

/** One page of `history` (newest first): the newest, or what is older than
 *  the entry `cursor` names — a keyset cursor, as the engine's are. */
function pageOf(history: readonly number[], cursor = ""): Answer {
  const older = cursor === "" ? history : history.filter((n) => n < Number(cursor));
  const slice = older.slice(0, PAGE);
  return {
    entries: slice.map((n) => ({ id: `e-${n}` })),
    next: older.length > PAGE ? String(slice[slice.length - 1]) : "",
  };
}

/** Numbers from `high` down to `low`. */
const down = (high: number, low: number) =>
  Array.from({ length: high - low + 1 }, (_, i) => high - i);

function answered(data: Answer): QueryResult<Answer> {
  return { data, loading: false, error: null, refusal: null, refetch: () => undefined };
}

/** The hook over a history the test moves, paged back once. */
async function pagedBack(start: readonly number[]) {
  const history = { now: start };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  const view = renderHook(
    ({ first }: { first: QueryResult<Answer> }) =>
      usePaged(first, PARAMS, page, pick, idOf, cursorOf),
    { initialProps: { first: answered(pageOf(history.now)) } },
  );
  act(() => view.result.current.next());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));
  const arrive = (next: readonly number[]) => {
    history.now = next;
    view.rerender({ first: answered(pageOf(history.now)) });
  };
  return { ...view, page, arrive, history };
}

const ids = (items: readonly { id: string }[]) => items.map((entry) => entry.id);

test("an entry pushed off the newest page by an arrival stays on the list", async () => {
  // Twelve entries, two pages drawn: 12..8 and 7..3. Entry 13 arrives and the
  // newest page is 13..9 — so 8 is on neither page unless the gap is read.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(13, 1));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 3).map((n) => `e-${n}`)));
  // AND THE PAGER STILL ENDS WHERE IT DID: the next page is the one before 3.
  expect(result.current.more).toBe(true);
  act(() => result.current.next());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 1).map((n) => `e-${n}`)));
});

test("the list is never drawn with the gap while the gap is being read", async () => {
  const { result, arrive, page, history } = await pagedBack(down(12, 1));
  // The read that fills the gap is held until the test lets it go.
  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  arrive(down(13, 1));
  // AS IT WAS LAST DRAWN — 12..3, whole — and not 13..9 beside 7..3.
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
  act(() => release());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 3).map((n) => `e-${n}`)));
});

test("a gap longer than what was paged back through drops the held pages", async () => {
  // One page held; eleven entries arrive, so the gap is more than a page.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(23, 1));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(23, 19).map((n) => `e-${n}`)));
  // AND THE READER PAGES BACK AGAIN from a list that is whole.
  expect(result.current.more).toBe(true);
  expect(result.current.pageError).toBeNull();
});

test("a gap that could not be read drops the held pages and says why", async () => {
  const { result, arrive, page } = await pagedBack(down(12, 1));
  page.mockImplementationOnce(async () => {
    throw new Error("unavailable");
  });
  arrive(down(13, 1));
  await waitFor(() => expect(result.current.pageError).toBe("unavailable"));
  expect(ids(result.current.items)).toEqual(down(13, 9).map((n) => `e-${n}`));
});

test("a newest page that still meets the held page asks nothing more", async () => {
  // An edit to an entry on the newest page moves nothing: its cursor is the
  // one the held page was asked under.
  const { result, arrive, page } = await pagedBack(down(12, 1));
  arrive(down(12, 1));
  await new Promise((r) => setTimeout(r, 20));
  expect(page).toHaveBeenCalledTimes(1);
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
});

test("a history that ends before the held page keeps only what is still there", async () => {
  // Everything older than the newest page was erased: the held page holds
  // entries that no longer exist, and the newest page says nothing is older.
  const { result, arrive } = await pagedBack(down(12, 1));
  arrive(down(12, 8));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(12, 8).map((n) => `e-${n}`)));
  expect(result.current.more).toBe(false);
});

test("a refreshed page is read again under its own cursor and replaced in place", async () => {
  // AN ENTRY ON A HELD PAGE CAN CHANGE OR GO — a chat mention's message
  // erased — and a page read once draws it as it was for as long as the
  // screen is open. The caller says so; the page is asked again where it began.
  const { result, page, history } = await pagedBack(down(12, 1));
  history.now = down(12, 1).filter((n) => n !== 5);
  act(() => result.current.refresh());
  await waitFor(() =>
    expect(ids(result.current.items)).toEqual(
      [12, 11, 10, 9, 8, 7, 6, 4, 3, 2].map((n) => `e-${n}`),
    ),
  );
  expect(page).toHaveBeenLastCalledWith("8");
  // AND THE PAGER NOW BEGINS WHERE THE PAGE ENDS.
  expect(result.current.more).toBe(true);
});

test("a refresh asks only the pages its caller names", async () => {
  const { result, page } = await pagedBack(down(12, 1));
  const asked = page.mock.calls.length;
  act(() => result.current.refresh(() => false));
  await new Promise((r) => setTimeout(r, 20));
  expect(page).toHaveBeenCalledTimes(asked);
});

test("a re-read asked while one is out reads again after it, and its word is kept", async () => {
  // THE FIRST MAY HAVE READ BEFORE THE CHANGE THE SECOND WAS ASKED FOR, so
  // the second is not folded into it: it reads once the first has landed,
  // and what it read is what stays.
  const { result, page, history } = await pagedBack(down(12, 1));
  const held: ((answer: Answer) => void)[] = [];
  page.mockImplementation(
    (_cursor: string) => new Promise<Answer>((resolve) => held.push(resolve)),
  );
  act(() => result.current.refresh());
  await waitFor(() => expect(held).toHaveLength(1));
  history.now = down(12, 1).filter((n) => n !== 6);
  act(() => result.current.refresh());
  await act(async () => held[0]!(pageOf(down(12, 1), "8")));
  await waitFor(() => expect(held).toHaveLength(2));
  await act(async () => held[1]!(pageOf(history.now, "8")));
  expect(ids(result.current.items)).not.toContain("e-6");
});

test("re-reads asked before their turn are one read of each page", async () => {
  // A BURST OF CHANGES IS ONE RE-READ: a second asked while the first waits
  // for its turn widens it instead of asking the same pages twice.
  const { result, page } = await pagedBack(down(12, 1));
  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(down(12, 1), cursor));
      }),
  );
  // A PAGE ON ITS WAY holds the line, so both re-reads wait behind it.
  act(() => result.current.next());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  act(() => result.current.refresh((_answer, place) => place.last));
  act(() => result.current.refresh());
  act(() => release());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(4));
  await new Promise((r) => setTimeout(r, 20));
  expect(page.mock.calls.slice(2).map(([cursor]) => cursor)).toEqual(["8", "3"]);
});

test("a re-read hands the first page to the caller's own question", async () => {
  // THE FIRST PAGE IS THE CALLER'S QUESTION, so a change that reached it is
  // the caller's refetch rather than a page this hook asks under a cursor.
  const history = { now: down(12, 1) };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  const refetch = vi.fn();
  const view = renderHook(() =>
    usePaged({ ...answered(pageOf(history.now)), refetch }, PARAMS, page, pick, idOf, cursorOf),
  );
  act(() => view.result.current.refresh((_answer, place) => place.first && place.last));
  await waitFor(() => expect(refetch).toHaveBeenCalledTimes(1));
  expect(page).not.toHaveBeenCalled();
});

/** Whether a drawn list is one run of the history, with nothing missing
 *  between its first entry and its last. */
const whole = (items: readonly { id: string }[]) =>
  items.every(
    (entry, i) => i === 0 || Number(items[i - 1]!.id.slice(2)) - Number(entry.id.slice(2)) === 1,
  );

test("entries that appear inside a held page are read in before it is drawn", async () => {
  // A PRIVATE ROOM JOINED: the mentions said in it become visible across
  // the whole feed. Every page read again holds as many entries over a
  // shorter range, so each now ends short of the page after it, and what
  // it let go of is on no page — a hole drawn as continuity unless the
  // range is read in.
  const evens = down(40, 1).filter((n) => n % 2 === 0);
  const { result, arrive, history } = await pagedBack(evens);
  act(() => result.current.next());
  await waitFor(() => expect(result.current.items).toHaveLength(3 * PAGE));
  history.now = down(40, 1);
  act(() => result.current.refresh());
  arrive(down(40, 1));
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(40, 17).map((n) => `e-${n}`)));
  // AND THE PAGER STILL ENDS WHERE THE LAST PAGE DOES.
  expect(result.current.more).toBe(true);
});

test("a re-read page is never drawn short of the next before the gap is read", async () => {
  // Three pages drawn over the evens 24..2; every odd then becomes visible.
  const evens = down(24, 1).filter((n) => n % 2 === 0);
  const { result, page, history } = await pagedBack(evens);
  act(() => result.current.next());
  await waitFor(() => expect(result.current.items).toHaveLength(evens.length));
  const before = ids(result.current.items);
  history.now = down(24, 1);
  // Both held pages are read again; the read of the gap the first of them
  // now ends short of is held until the case lets it go.
  let release: () => void = () => undefined;
  page.mockImplementationOnce(async (cursor: string) => pageOf(history.now, cursor));
  page.mockImplementationOnce(async (cursor: string) => pageOf(history.now, cursor));
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  act(() => result.current.refresh());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(5));
  expect(page).toHaveBeenLastCalledWith("11");
  // AS IT WAS LAST DRAWN until the gap has been read — never the re-read
  // page, 15..11, beside the held one under it, 5..1.
  expect(ids(result.current.items)).toEqual(before);
  act(() => release());
  await waitFor(() =>
    expect(ids(result.current.items)).toEqual(
      [24, 22, 20, 18, 16, ...down(15, 1)].map((n) => `e-${n}`),
    ),
  );
});

test("a gap under a re-read page longer than the pages after it drops them", async () => {
  // BOUNDED BY WHAT IT WOULD SAVE: one page after the re-read one, and a
  // gap more than a page long — the reader pages on again from a list that
  // is whole.
  const sparse = down(60, 1).filter((n) => n % 4 === 0);
  const { result, history } = await pagedBack(sparse);
  act(() => result.current.next());
  await waitFor(() => expect(result.current.items).toHaveLength(3 * PAGE));
  history.now = down(60, 1);
  act(() => result.current.refresh());
  await waitFor(() => expect(result.current.items).toHaveLength(3 * PAGE - PAGE));
  // The newest page as it was, then the re-read first held page and the
  // gap under it read in; the last held page, past the bound, is gone.
  const drawn = result.current.items.slice(PAGE);
  expect(whole(drawn)).toBe(true);
  expect(result.current.more).toBe(true);
});

test("a gap under a re-read page that could not be read leaves the page as it was", async () => {
  const evens = down(24, 1).filter((n) => n % 2 === 0);
  const { result, page, history } = await pagedBack(evens);
  act(() => result.current.next());
  await waitFor(() => expect(result.current.items).toHaveLength(evens.length));
  const before = ids(result.current.items);
  history.now = down(24, 1);
  page.mockImplementationOnce(async (cursor: string) => pageOf(history.now, cursor));
  page.mockImplementationOnce(async (cursor: string) => pageOf(history.now, cursor));
  page.mockImplementationOnce(async () => {
    throw new Error("unavailable");
  });
  act(() => result.current.refresh());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(5));
  // THE LAST PAGE, which nothing follows, is put in place; the one above it
  // keeps what it last said, which still meets it.
  await waitFor(() =>
    expect(ids(result.current.items)).toEqual([
      ...before.slice(0, 2 * PAGE),
      ...down(5, 1).map((n) => `e-${n}`),
    ]),
  );
  expect(whole(result.current.items.slice(2 * PAGE))).toBe(true);
  // A RE-READ IS THE SCREEN'S OWN, and its failure is nobody's to be told.
  expect(result.current.pageError).toBeNull();
});

test("a join in flight when a re-read replaces the first held page still lands", async () => {
  // THE RE-READ GAVE THE PAGE THE JOIN WAS REACHING A NEW ANSWER, and a
  // join that matched it by identity threw its read away — the new entry
  // stayed off screen until the first page next changed.
  const { result, arrive, page, history } = await pagedBack(down(12, 1));
  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  arrive(down(13, 1));
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  act(() => result.current.refresh());
  // EVERYTHING THE RE-READ CAN DO WITHOUT THE JOIN, done and drawn first.
  await act(async () => new Promise((r) => setTimeout(r, 20)));
  act(() => release());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(13, 3).map((n) => `e-${n}`)));
});

test("a page asked while a re-read moves the last page's foot is still added", async () => {
  // AN ENTRY ON THE LAST PAGE ERASED while the next page is on its way: the
  // last page's foot moves, and a page appended only where it began under
  // the old foot was dropped — "Older" pressed, and nothing added.
  const { result, page, history } = await pagedBack(down(20, 1));
  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  act(() => result.current.next());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  history.now = down(20, 1).filter((n) => n !== 13);
  act(() => result.current.refresh());
  act(() => release());
  await waitFor(() =>
    expect(ids(result.current.items)).toEqual(
      down(20, 6)
        .filter((n) => n !== 13)
        .map((n) => `e-${n}`),
    ),
  );
  expect(result.current.paging).toBe(false);
});

// THE JOIN NEVER OVERWRITES WHAT LANDED SINCE ITS RENDER. A newest answer is
// committed, and its join runs in the passive effect after — and in between,
// an older page the reader asked for can land and append itself. The join
// used to replace the whole held state with one built from that render, so
// the appended page was dropped: "Older" came back with nothing added, which
// is a press that silently did nothing.
test("an older page that lands between a newest answer and its join is kept", async () => {
  const history = { now: down(16, 1) };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  let move: (first: QueryResult<Answer>) => void = () => undefined;
  // RUN IN THE COMMIT THAT TAKES THE NEW ANSWER — a layout effect, so after
  // the commit and before the join's passive effect.
  let inCommit: (() => void) | null = null;
  const view = renderHook(() => {
    const [first, setFirst] = useState(() => answered(pageOf(history.now)));
    move = setFirst;
    useLayoutEffect(() => {
      inCommit?.();
      inCommit = null;
    }, [first]);
    return usePaged(first, PARAMS, page, pick, idOf, cursorOf);
  });
  act(() => view.result.current.next());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));

  let release: () => void = () => undefined;
  page.mockImplementationOnce(
    (cursor: string) =>
      new Promise<Answer>((resolve) => {
        release = () => resolve(pageOf(history.now, cursor));
      }),
  );
  act(() => view.result.current.next());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));

  // OUTSIDE `act`, so the render and its effects are scheduled as a browser
  // schedules them rather than flushed as one step.
  const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
  const previous = env.IS_REACT_ACT_ENVIRONMENT;
  env.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    inCommit = () => release();
    // The newest page read again, unchanged — a new answer that still joins.
    move(answered(pageOf(history.now)));
    await waitFor(() =>
      expect(ids(view.result.current.items)).toEqual(down(16, 2).map((n) => `e-${n}`)),
    );
  } finally {
    env.IS_REACT_ACT_ENVIRONMENT = previous;
  }
});

// A JOIN WAITS FOR THE OLDER PAGE THAT IS OUT, AND THEN DECIDES AGAINST IT
// TOO. The page is asked under the held tail's cursor and answered from the
// history as it was then, and a first page that moves while it is out may
// replace or drop the pages it follows — so the join runs only once the page
// has landed on the tail it was asked after, and decides against everything
// held then: one change at a time, never a seam. Every list these cases draw
// is held to being CONTIGUOUS, render by render, which is the whole of what
// "no hole drawn as continuity" means for a history of consecutive entries.

/** Hold the next read until `release`, answering from the history as it
 *  was when it was ASKED — an answer the engine formed before the move. */
function holdNext(page: ReturnType<typeof vi.fn>, history: { now: readonly number[] }) {
  const gate = { release: () => undefined as void };
  page.mockImplementationOnce((cursor: string) => {
    const asked = history.now;
    return new Promise<Answer>((resolve) => {
      gate.release = () => resolve(pageOf(asked, cursor));
    });
  });
  return gate;
}

/** Whether a drawn list runs down one entry at a time — has no hole in it. */
function contiguous(list: readonly string[]): boolean {
  const ns = list.map((id) => Number(id.slice("e-".length)));
  return ns.every((n, i) => i === 0 || ns[i - 1]! - n === 1);
}

/** The hook over a history the test moves, paged back once, every list it
 *  draws recorded. */
async function drawnBack(start: readonly number[]) {
  const history = { now: start };
  const page = vi.fn(async (cursor: string) => pageOf(history.now, cursor));
  const drawn: string[][] = [];
  const view = renderHook(
    ({ first }: { first: QueryResult<Answer> }) => {
      const paged = usePaged(first, PARAMS, page, pick, idOf, cursorOf);
      drawn.push(ids(paged.items));
      return paged;
    },
    { initialProps: { first: answered(pageOf(history.now)) } },
  );
  act(() => view.result.current.next());
  await waitFor(() => expect(view.result.current.items).toHaveLength(2 * PAGE));
  const arrive = (next: readonly number[]) => {
    history.now = next;
    view.rerender({ first: answered(pageOf(history.now)) });
  };
  return { ...view, page, arrive, history, drawn };
}

test("a join that cuts the held pages short waits for the older page out, and cuts it too", async () => {
  const { result, arrive, page, history, drawn } = await drawnBack(down(12, 1));
  const gate = holdNext(page, history);
  act(() => result.current.next());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  // Everything older than 8 is erased and 13 and 14 arrive. The join waits
  // behind the page before 3, and the list is drawn as it was meanwhile.
  arrive(down(14, 8));
  await new Promise((r) => setTimeout(r, 20));
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
  expect(page).toHaveBeenCalledTimes(2);
  // The page before 3 lands, answered before the erase, on the tail it was
  // asked after. Then the join walks from the newest page: 9..8, and the
  // history ends there — so every held page goes, the late one with them.
  act(() => gate.release());
  await waitFor(() => expect(ids(result.current.items)).toEqual(down(14, 8).map((n) => `e-${n}`)));
  expect(result.current.paging).toBe(false);
  expect(result.current.more).toBe(false);
  for (const items of drawn) expect(contiguous(items), items.join(",")).toBe(true);
});

test("a join that drops the held pages waits for the older page out, which goes with them", async () => {
  const { result, arrive, page, history, drawn } = await drawnBack(down(12, 1));
  const gate = holdNext(page, history);
  act(() => result.current.next());
  await waitFor(() => expect(page).toHaveBeenCalledTimes(2));
  // Eleven arrive: more than the held pages' worth, so the join will drop
  // them — but not before the page that is out has landed.
  arrive(down(23, 1));
  await new Promise((r) => setTimeout(r, 20));
  expect(ids(result.current.items)).toEqual(down(12, 3).map((n) => `e-${n}`));
  act(() => gate.release());
  await waitFor(() => expect(result.current.paging).toBe(false));
  await new Promise((r) => setTimeout(r, 20));
  // Only the newest page is left, whole; the reader pages on from there.
  expect(ids(result.current.items)).toEqual(down(23, 19).map((n) => `e-${n}`));
  expect(result.current.more).toBe(true);
  for (const items of drawn) expect(contiguous(items), items.join(",")).toBe(true);
});
