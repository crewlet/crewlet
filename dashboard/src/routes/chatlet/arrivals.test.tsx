import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import type { ChatMessageView } from "~/protocol/index.ts";

const said = vi.hoisted(() => [] as string[]);
vi.mock("@crewlethq/ui", async (original) => ({
  ...(await original<typeof import("@crewlethq/ui")>()),
  announce: (text: string) => said.push(text),
}));

const { arrivalWords, SAID_CHARS, useArrivals } = await import("./arrivals.ts");

beforeEach(() => {
  said.length = 0;
});
afterEach(cleanup);

const NAMES: Record<string, string> = { ada: "Ada Park", bo: "Bo Lind", cy: "Cy Rowe" };
const nameOf = (handle: string) => NAMES[handle] ?? handle;

function view(
  seq: number,
  author: string,
  body: string,
  over: Record<string, unknown> = {},
): ChatMessageView {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author,
      author_kind: "human",
      body,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  } as ChatMessageView;
}

function mount(list: string, views: ChatMessageView[], ready = true) {
  return renderHook(
    (props: { list: string; views: ChatMessageView[]; ready: boolean }) =>
      useArrivals(props.list, props.views, props.ready, "ada", nameOf),
    { initialProps: { list, views, ready } },
  );
}

// Mutation: announce the first answer, and opening a room reads its history out.
test("the first answer is history: nothing is said", () => {
  mount("room-1", [view(2, "bo", "hello"), view(1, "cy", "hi")]);
  expect(said).toEqual([]);
});

test("a message that arrives after it is read out once, as plain words", () => {
  const hook = mount("room-1", [view(1, "bo", "hello")]);
  hook.rerender({
    list: "room-1",
    views: [view(2, "bo", "The **build** is green"), view(1, "bo", "hello")],
    ready: true,
  });
  expect(said).toEqual(["Bo Lind: The build is green"]);
  hook.rerender({
    list: "room-1",
    views: [view(2, "bo", "The **build** is green"), view(1, "bo", "hello")],
    ready: true,
  });
  expect(said).toHaveLength(1);
});

test("the reader's own words, events, deletions and older pages say nothing", () => {
  const hook = mount("room-1", [view(5, "bo", "hello")]);
  hook.rerender({
    list: "room-1",
    views: [
      view(8, "cy", "", { deleted_at: "2026-01-01T00:01:00Z" }),
      view(7, "bo", "", { event: { kind: "joined" } }),
      view(6, "ada", "mine"),
      view(5, "bo", "hello"),
      view(2, "cy", "an older page"),
    ],
    ready: true,
  });
  expect(said).toEqual([]);
});

test("several at once are counted, with who said them", () => {
  const hook = mount("room-1", [view(1, "bo", "hello")]);
  hook.rerender({
    list: "room-1",
    views: [view(4, "bo", "c"), view(3, "cy", "b"), view(2, "bo", "a"), view(1, "bo", "hello")],
    ready: true,
  });
  expect(said).toEqual(["3 new messages from Bo Lind and Cy Rowe"]);
});

test("moving to another room starts again without reading it out", () => {
  const hook = mount("room-1", [view(1, "bo", "hello")]);
  hook.rerender({ list: "room-2", views: [view(9, "cy", "elsewhere")], ready: true });
  expect(said).toEqual([]);
});

test("nothing is said before the list is read, and the wait is not an arrival", () => {
  const hook = mount("room-1", [], false);
  hook.rerender({ list: "room-1", views: [view(3, "bo", "hello")], ready: true });
  expect(said).toEqual([]);
});

test("a long message is cut to a sentence's length", () => {
  const long = "word ".repeat(80);
  const words = arrivalWords([view(1, "bo", long)], nameOf);
  expect(words.startsWith("Bo Lind: word")).toBe(true);
  expect(words.length).toBeLessThanOrEqual("Bo Lind: ".length + SAID_CHARS);
  expect(words.endsWith("…")).toBe(true);
});
