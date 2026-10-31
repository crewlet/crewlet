/**
 * The mention feed: the one list that spans the rooms, paged back as every
 * history in the dashboard is.
 *
 * TWO THINGS IT MUST NOT GET WRONG. An older page is asked at this tab's read
 * floor, as every question is (`withFloor`) — a page read from a node that has
 * not applied this tab's own write would page back through a feed without
 * it. And a page the reader went back for STAYS when the newest page is read
 * again: the feed re-reads on every chat frame in the company, and a mention's
 * position never moves, so throwing the older pages away with each re-read
 * left a reader on a busy company unable to reach the bottom of their own
 * mentions — and keeping them without checking that the newest page still
 * reaches them dropped the mention a new one pushed off its foot.
 *
 * THE FIXTURE PAGES AS THE ENGINE DOES: full pages, and a cursor that is the
 * position of a page's last mention, so it moves whenever the page does. A
 * cursor held still while the newest page moved is an answer the engine never
 * gives, and it is exactly the one that hid the dropped mention.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { fmtDateTime } from "~/lib/format.ts";
import { session } from "~/protocol/session.ts";
import { mountChat } from "~/test/chat.tsx";

import { LIVE_REFETCH_MS } from "~/lib/chatLive.ts";
import { Mentions } from "./Mentions.tsx";
import { PAGE } from "./page.ts";

beforeEach(() => {
  rewritten = new Set();
  namedByEdit = new Set();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  location.hash = "#/";
});

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 60 },
};

/** When every mention here was named, when an edit rewrote one, and the
 *  instant the cases that read a row's time pin the clock to — three hours
 *  after the naming and two minutes after the edit, so the two read apart. */
const NAMED_AT = "2026-01-01T00:00:00Z";
const EDITED_AT = "2026-01-01T02:58:00Z";
const NOW = "2026-01-01T03:00:00Z";

/** The messages an edit has rewritten, by the number of the mention on them,
 *  answered as the engine answers them. An edit that STILL names you leaves
 *  the mention dated when you were named and marks it edited (`rewritten`);
 *  an edit that names you where the words before it did not is the moment you
 *  were named, so the mention is dated by the edit (`namedByEdit`). */
let rewritten: Set<number>;
let namedByEdit: Set<number>;

function mention(seq: number) {
  const kept = rewritten.has(seq);
  const named = namedByEdit.has(seq);
  return {
    message_id: `m-${seq}`,
    channel_id: "room-1",
    channel_name: "general",
    channel_kind: "public",
    author: "bo",
    excerpt: kept || named ? `said again, named you ${seq}` : `named you ${seq}`,
    at: named ? EDITED_AT : NAMED_AT,
    ...(kept || named ? { edited_at: EDITED_AT } : {}),
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

/** Pin the clock every relative time reads to [NOW]. Only `Date` is faked:
 *  the feed's own timers, and every wait below, run as they do in a tab. */
function pinClock() {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(NOW));
}

/** The time a mention's row draws — what it reads, and the instant its title
 *  names — found by the row's excerpt. */
function drawnTime(excerpt: RegExp) {
  const row = screen.getByText(excerpt).closest(".chat-hit");
  const time = [...(row?.querySelectorAll("span[title]") ?? [])].find(
    (span) => span.textContent !== "edited",
  );
  return { reads: time?.textContent, title: time?.getAttribute("title") };
}

/** The feed's whole history, newest first, as the fake engine pages it. */
let history: number[];

/** One page of it, as the engine answers: the newest, or what is older than
 *  the mention the cursor names — and the cursor is the position of the
 *  page's last mention, so it moves whenever the page does. */
function pageOf(cursor?: string) {
  const before = cursor ? Number(cursor.split(":")[1]) : Infinity;
  const older = history.filter((seq) => seq < before);
  const slice = older.slice(0, PAGE);
  return {
    ...served,
    position: { ...served.position, seq: history[0] },
    handle: "ada",
    mentions: slice.map(mention),
    ...(older.length > PAGE
      ? { next_cursor: `CREWLET_CHAT_LOG@1:${slice[slice.length - 1]}` }
      : {}),
  };
}

/** Every mention the feed draws, by the number its excerpt names, in order. */
const drawnMentions = () =>
  [...document.querySelectorAll(".chat-hit")].map((row) =>
    Number(/named you (\d+)$/.exec(row.textContent ?? "")?.[1]),
  );

/** Numbers from `high` down to `low`. */
const down = (high: number, low: number) =>
  Array.from({ length: high - low + 1 }, (_, i) => high - i);

test("an older page is asked at the read floor and kept across a live re-read", async () => {
  // THIS TAB WROTE TO THE CHAT LOG: every chat question names that floor.
  session.written("chat", "CREWLET_CHAT_LOG@1:55", []);
  // A FULL NEWEST PAGE and ten mentions before it.
  history = down(PAGE + 10, 1);
  const answer = vi.fn(async (_what: string, params?: Record<string, unknown>) =>
    pageOf(params?.cursor as string | undefined),
  );
  const { store } = mountChat(<Mentions nameOf={(handle) => handle} />, { answer });
  await screen.findByText(/named you 60$/);

  fireEvent.click(screen.getByRole("button", { name: /older mentions/i }));
  await screen.findByText(/named you 3$/);
  expect(answer).toHaveBeenCalledWith("chat_mentions", {
    limit: PAGE,
    cursor: "CREWLET_CHAT_LOG@1:11",
    read_level: "session",
    min_position: "CREWLET_CHAT_LOG@1:55",
  });

  // A FRAME ANYWHERE re-reads the newest page — and somebody naming this
  // person again pushes mention 11 off its foot. It is on no page the
  // reader asked for, and it stays on the feed: a mention that silently
  // dropped out of a list that reads as whole is the one failure a feed may
  // not have.
  history = down(PAGE + 11, 1);
  act(() => {
    store.applyChatChange({
      channel_id: "room-9",
      op: "post",
      op_id: "op-61",
      message_id: "m-61",
      channel_seq: 1,
      at: "2026-01-01T00:01:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 61 },
    } as never);
  });
  await screen.findByText(/named you 61$/);
  await waitFor(() => expect(screen.getByText(/named you 11$/)).toBeTruthy());
  // EVERY ONE OF THEM, ONCE.
  for (const seq of down(PAGE + 11, 1)) {
    expect(screen.getAllByText(new RegExp(`named you ${seq}$`))).toHaveLength(1);
  }
});

test("an older page that could not be read says why in every pager's words", async () => {
  const answer = vi.fn(async (_what: string, params?: Record<string, unknown>) => {
    if (params?.cursor) throw new Error("unavailable");
    history = down(PAGE + 9, 1);
    return pageOf();
  });
  mountChat(<Mentions nameOf={(handle) => handle} />, { answer });
  await screen.findByText(/named you 59$/);
  fireEvent.click(screen.getByRole("button", { name: /older mentions/i }));
  // THE REFUSAL TABLE'S OWN SENTENCE for the code, not the code in brackets.
  await waitFor(() => expect(document.querySelector(".crewlet-callout")).not.toBeNull());
  expect(screen.queryByText(/\(unavailable\)/)).toBeNull();
  expect(screen.getByText(/named you 59$/)).toBeTruthy();
});

/** The feed paged back once over a full newest page and ten older mentions,
 *  and every page asked since, by the cursor it named ("" for the newest). */
async function pagedBack() {
  history = down(PAGE + 10, 1);
  const answer = vi.fn(async (_what: string, params?: Record<string, unknown>) =>
    pageOf(params?.cursor as string | undefined),
  );
  const { store } = mountChat(<Mentions nameOf={(handle) => handle} />, { answer });
  await screen.findByText(/named you 60$/);
  fireEvent.click(screen.getByRole("button", { name: /older mentions/i }));
  await screen.findByText(/named you 3$/);
  const asked = () => answer.mock.calls.map(([, params]) => String(params?.cursor ?? ""));
  const before = asked().length;
  /** One frame, and the newest page's re-read it causes. */
  const frame = async (over: Record<string, unknown>) => {
    const reads = asked().filter((cursor) => cursor === "").length;
    act(() => {
      store.applyChatChange({
        channel_id: "room-1",
        op: "post",
        op_id: `op-${String(over.op)}-${String(over.message_id)}`,
        at: "2026-01-01T00:01:00Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 61 },
        ...over,
      } as never);
    });
    await waitFor(() =>
      expect(asked().filter((cursor) => cursor === "").length).toBeGreaterThan(reads),
    );
  };
  return { asked: () => asked().slice(before), frame };
}

test("a mention taken down on a page the reader went back for leaves it", async () => {
  // A DELETE TAKES THE MENTION AWAY — the engine removes a message's mention
  // rows with its body, so a message somebody took back no longer names you.
  // Kept as first read, the page went on showing the deleted words for as
  // long as the feed was open.
  const { asked, frame } = await pagedBack();
  history = history.filter((seq) => seq !== 5);
  await frame({ op: "delete", message_id: "m-5" });
  await waitFor(() => expect(screen.queryByText(/named you 5$/)).toBeNull());
  expect(screen.getByText(/named you 4$/)).toBeTruthy();
  expect(asked().sort()).toEqual(["", "CREWLET_CHAT_LOG@1:11"]);
});

test("an edit that still names you rewrites the mention where it was, marked edited", async () => {
  // A MENTION IS THE MOMENT YOU WERE NAMED, and fixing the words around your
  // name is not naming you again: the engine leaves the mention where it was,
  // and the page holding it is read again for its new words — kept as first
  // read, the feed went on drawing words the author had replaced.
  pinClock();
  const { asked, frame } = await pagedBack();
  rewritten.add(5);
  await frame({ op: "edit", message_id: "m-5" });
  await screen.findByText(/said again, named you 5$/);
  expect(screen.queryByText(/: named you 5$/)).toBeNull();
  // DATED WHEN YOU WERE NAMED, not by the edit: a typo fixed this minute is
  // not news, and a row that read "2m ago" would present it as some.
  expect(drawnTime(/said again, named you 5$/)).toEqual({
    reads: "3h ago",
    title: fmtDateTime(NAMED_AT),
  });
  // IN ITS PLACE, every other mention around it as it was.
  expect(drawnMentions()).toEqual(down(PAGE + 10, 1));
  // MARKED, as the room's own row is: the words are not the ones that named
  // you.
  const row = screen.getByText(/said again, named you 5$/).closest(".chat-hit");
  expect(row?.textContent).toMatch(/edited/);
  expect(row?.querySelector(`span[title="${fmtDateTime(EDITED_AT)}"]`)?.textContent).toBe("edited");
  expect(
    [...document.querySelectorAll(".chat-hit")].filter((hit) =>
      /edited/.test(hit.textContent ?? ""),
    ),
  ).toHaveLength(1);
  expect(asked().sort()).toEqual(["", "CREWLET_CHAT_LOG@1:11"]);
});

test("an edit that no longer names you takes the mention off a page the reader went back for", async () => {
  const { asked, frame } = await pagedBack();
  history = history.filter((seq) => seq !== 6);
  await frame({ op: "edit", message_id: "m-6" });
  await waitFor(() => expect(screen.queryByText(/named you 6$/)).toBeNull());
  expect(drawnMentions()).toEqual(down(PAGE + 10, 1).filter((seq) => seq !== 6));
  expect(asked().sort()).toEqual(["", "CREWLET_CHAT_LOG@1:11"]);
});

test("an edit that names you anew arrives at the top, as a post that named you would", async () => {
  // THE WORDS BEFORE IT DID NOT NAME YOU, so the edit is the moment you were
  // named: the engine places the mention at the edit, which is above every
  // page the reader has read — and it is the newest page that finds it, read
  // again on the frame like any other. The page held below it is not asked
  // again: the frame names a message on none of them.
  pinClock();
  const { asked, frame } = await pagedBack();
  history = [PAGE + 11, ...history];
  namedByEdit.add(PAGE + 11);
  await frame({ op: "edit", message_id: `m-${PAGE + 11}` });
  await screen.findByText(new RegExp(`said again, named you ${PAGE + 11}$`));
  await waitFor(() => expect(drawnMentions()).toEqual(down(PAGE + 11, 1)));
  expect(asked()).not.toContain("CREWLET_CHAT_LOG@1:11");
  // DATED BY THE EDIT, which is when you were named: dated when the words
  // were first said, the newest row of a feed ordered by naming would read
  // as old as the ones beneath it.
  expect(drawnTime(new RegExp(`said again, named you ${PAGE + 11}$`))).toEqual({
    reads: "2m ago",
    title: fmtDateTime(EDITED_AT),
  });
  // AND EVERY MENTION BELOW IT STILL WHEN IT WAS NAMED.
  expect(drawnTime(/: named you 5$/)).toEqual({ reads: "3h ago", title: fmtDateTime(NAMED_AT) });
});

test("an erase in a room takes its mentions off a page the reader went back for", async () => {
  const { asked, frame } = await pagedBack();
  history = history.filter((seq) => seq !== 4);
  await frame({ op: "erase" });
  await waitFor(() => expect(screen.queryByText(/named you 4$/)).toBeNull());
  expect(screen.getByText(/named you 3$/)).toBeTruthy();
  expect(asked().sort()).toEqual(["", "CREWLET_CHAT_LOG@1:11"]);
});

test("a frame naming nothing on a held page reads none of them again", async () => {
  // ONLY WHAT A FRAME NAMES: a reaction is not drawn here, a message nobody
  // paged to is on no held page, and an erase in another room reaches none
  // of this one's mentions.
  const { asked, frame } = await pagedBack();
  await frame({ op: "react", message_id: "m-5" });
  await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 20));
  await frame({ op: "delete", message_id: "m-900" });
  await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 20));
  await frame({ op: "erase", channel_id: "room-9" });
  expect(asked().every((cursor) => cursor === "")).toBe(true);
});

test("mentions a joined room makes visible are read in between the pages they land on", async () => {
  // JOINING A PRIVATE ROOM MAKES EVERY MENTION SAID IN IT VISIBLE, anywhere
  // in the feed: each page read again holds as many mentions over a shorter
  // range, so it ends short of the page after it. Put in place as read, the
  // feed drew the mentions between that end and the next page's start as
  // though they did not exist.
  history = down(320, 1).filter((seq) => seq % 2 === 0);
  const answer = vi.fn(async (_what: string, params?: Record<string, unknown>) =>
    pageOf(params?.cursor as string | undefined),
  );
  const { store } = mountChat(<Mentions nameOf={(handle) => handle} />, { answer });
  await screen.findByText(/named you 320$/);
  fireEvent.click(screen.getByRole("button", { name: /older mentions/i }));
  await screen.findByText(/named you 122$/);
  fireEvent.click(screen.getByRole("button", { name: /older mentions/i }));
  await screen.findByText(/named you 22$/);

  history = down(320, 1);
  act(() => {
    store.applyChatChange({
      channel_id: "room-7",
      op: "members",
      op_id: "op-join",
      at: "2026-01-01T00:01:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 321 },
    } as never);
  });
  // EVERY MENTION FROM THE TOP TO THE FOOT OF WHAT IS HELD, ONCE — a hole
  // drawn as continuity is the one failure a feed may not have.
  await waitFor(() => expect(drawnMentions()).toEqual(down(320, 72)));
  expect(screen.getByRole("button", { name: /older mentions/i })).toBeTruthy();
});
