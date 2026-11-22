/**
 * Which Home board is drawn: the engine's wherever the reader signs in, with
 * this browser's copy only for the wait, a seatless sign-in, or a one-time move.
 */
import { describe, expect, test } from "vitest";
import { answerOf, drawnOf, fits, localOf, MAX_BOARD_BYTES, type Local } from "./kept.ts";
import type { Board } from "./model.ts";

const note = (id: string, text: string) => ({
  id,
  kind: "note",
  text,
  tone: "amber",
  align: "start",
  size: "md",
  w: 4,
  h: null,
});
const SHIPPED: Board = { widgets: [] };
const MINE = { widgets: [note("w-mine", "mine")] } as Board;
const THEIRS = { widgets: [note("w-theirs", "theirs")] } as Board;

describe("what this browser holds", () => {
  test("a bare board is one kept here alone, read as untrusted input", () => {
    const local = localOf({ widgets: [note("w-a", "a"), { kind: "script" }] }, SHIPPED);
    expect(local.kind).toBe("browser");
    expect(local.kind === "browser" && local.board.widgets.map((w) => w.id)).toEqual(["w-a"]);
  });

  test("the engine's last answer is a cache only with its seat and revision", () => {
    expect(localOf({ v: 1, seat: "pm", revision: 4, board: MINE }, SHIPPED)).toMatchObject({
      kind: "cache",
      seat: "pm",
      revision: 4,
    });
    for (const raw of [
      { v: 1, seat: "", revision: 4, board: MINE },
      { v: 1, seat: "pm", revision: 0, board: MINE },
      { v: 2, seat: "pm", revision: 4, board: MINE },
      "board",
      null,
    ]) {
      expect(localOf(raw, SHIPPED).kind).toBe("none");
    }
  });
});

describe("the engine's answer", () => {
  test("an answer this build cannot read is no answer", () => {
    expect(answerOf({} as never)).toBeNull();
    expect(answerOf({ seat: "pm", board: null, revision: 1.5 })).toBeNull();
    expect(answerOf({ seat: "pm", board: MINE, revision: 3 })).toEqual({
      seat: "pm",
      board: MINE,
      revision: 3,
    });
  });
});

describe("the board drawn", () => {
  const cache = (revision: number, board = MINE, seat = "pm"): Local => ({
    kind: "cache",
    v: 1,
    seat,
    revision,
    board,
  });

  test("before the engine answers, the last answer kept here is drawn, never the shipped board", () => {
    expect(drawnOf(null, cache(4), SHIPPED)).toMatchObject({
      board: MINE,
      revision: 4,
      home: "unknown",
    });
    expect(drawnOf(null, { kind: "none" }, SHIPPED)).toMatchObject({ board: SHIPPED, revision: 0 });
  });

  test("the engine's board is the reader's, whatever this browser kept", () => {
    const answer = { seat: "pm", board: THEIRS, revision: 7 };
    for (const local of [
      cache(4),
      { kind: "browser", board: MINE } as Local,
      { kind: "none" } as Local,
    ]) {
      const drawn = drawnOf(answer, local, SHIPPED);
      expect(drawn.board.widgets.map((w) => w.id)).toEqual(["w-theirs"]);
      expect(drawn).toMatchObject({ revision: 7, home: "engine", migrate: null });
    }
  });

  test("this tab's own save is drawn until the answer catches up with it, and a kept copy never is", () => {
    const answer = { seat: "pm", board: THEIRS, revision: 7 };
    const landed = { seat: "pm", revision: 8, board: MINE };
    expect(drawnOf(answer, { kind: "none" }, SHIPPED, landed)).toMatchObject({
      board: MINE,
      revision: 8,
    });
    expect(
      drawnOf({ seat: "pm", board: null, revision: 0 }, { kind: "none" }, SHIPPED, {
        ...landed,
        revision: 1,
      }),
    ).toMatchObject({ board: MINE, revision: 1 });
    expect(drawnOf(answer, { kind: "none" }, SHIPPED, { ...landed, seat: "cto" }).revision).toBe(7);
    expect(drawnOf(answer, cache(9), SHIPPED).revision).toBe(7);
  });

  test("a board kept here alone moves onto an engine that holds none, and only then", () => {
    const none = { seat: "pm", board: null, revision: 0 };
    expect(drawnOf(none, { kind: "browser", board: MINE }, SHIPPED)).toMatchObject({
      board: MINE,
      revision: 0,
      migrate: MINE,
    });
    expect(drawnOf(none, cache(4), SHIPPED)).toMatchObject({ board: SHIPPED, migrate: null });
  });

  test("a sign-in holding no seat keeps its board in this browser", () => {
    const seatless = { seat: "", board: null, revision: 0 };
    expect(drawnOf(seatless, { kind: "browser", board: MINE }, SHIPPED)).toMatchObject({
      board: MINE,
      home: "browser",
      migrate: null,
    });
    expect(drawnOf(seatless, cache(4), SHIPPED)).toMatchObject({ board: SHIPPED, home: "browser" });
  });
});

test("a board past the engine's bound is held back before it is sent", () => {
  expect(fits(MINE)).toBe(true);
  const huge = { widgets: [note("w-huge", "x".repeat(MAX_BOARD_BYTES))] } as Board;
  expect(fits(huge)).toBe(false);
});
