// @vitest-environment node

/**
 * What a live view holds, answer by answer — the fold the hook runs, over
 * values. What the hook adds (asking, the chained poll, a new view per job)
 * is the screens' tests'.
 */

import { describe, expect, test } from "vitest";
import { LIVE_OUTPUT_MAX_BYTES } from "~/contract/sandbox.ts";
import type { SandboxOutput, SandboxTailAnswer, SandboxWindow } from "~/protocol/index.ts";
import { EMPTY_VIEW, cursorParams, foldTail, trimFront, type LiveView } from "./useLiveTail.ts";
import { utf8Bytes } from "./format.ts";

function answered(output: SandboxOutput): SandboxTailAnswer {
  return { outcome: "tail", turn_id: "t", launch_id: "L", output };
}

/** A window, from an owner that reads no cursor. */
function tail(output: Partial<SandboxWindow>): SandboxTailAnswer {
  return answered({
    text: "",
    source: "transcript",
    cut: false,
    as_of: "2026-10-06T09:00:00Z",
    finished: false,
    ...output,
  });
}

/** A cursor answer: a reset carrying `text` from `start`, or a delta after it. */
function cursored(
  text: string,
  start: number,
  reset: boolean,
  epoch = "transcript@0",
): SandboxTailAnswer {
  return answered({
    text,
    source: "transcript",
    cut: false,
    as_of: "2026-10-06T09:00:00Z",
    finished: false,
    cursor: true,
    reset,
    epoch,
    start,
    end: start + utf8Bytes(text),
    digest: `d${start + utf8Bytes(text)}`,
    window_bytes: LIVE_OUTPUT_MAX_BYTES,
  });
}

describe("a live view", () => {
  // IT ACCUMULATES. A reset carries what there is; every later answer carries
  // what was written since, appended, and the cursor moves to its end.
  //
  // Mutation: replace on every answer, and the second line is all it holds.
  test("holds what it was sent, and asks from where it holds through", () => {
    let view: LiveView = EMPTY_VIEW;
    expect(cursorParams(view)).toEqual({ cursor: true });
    view = foldTail(view, cursored("one\n", 0, true));
    view = foldTail(view, cursored("two\n", 4, false));
    expect(view.text).toBe("one\ntwo\n");
    expect(cursorParams(view)).toEqual({
      cursor: true,
      epoch: "transcript@0",
      after: 8,
      digest: "d8",
    });
    expect(view.dropped).toBe(0);
  });

  // A DELTA FROM ZERO FOLLOWS A VIEW HOLDING THROUGH ZERO — the answers a run
  // that has settled nothing yet is sent, read off the wire as the engine
  // writes them (`sandbox.TestACursorAnswerAlwaysStatesItsOffsets` holds the
  // engine to `start` and `end` being there at 0). An offset left out at 0
  // compared as absent, and the first output a run wrote was thrown away and
  // asked for again a poll later.
  test("follows a reading from offset zero", () => {
    const wire = (output: string): SandboxTailAnswer =>
      JSON.parse(
        `{"outcome":"tail","turn_id":"t","launch_id":"L","node":"n2","output":${output}}`,
      ) as SandboxTailAnswer;
    let view = foldTail(
      EMPTY_VIEW,
      wire(
        `{"text":"","source":"none","cut":false,"as_of":"2026-10-06T09:00:00Z","finished":false,` +
          `"window_bytes":262144,"cursor":true,"epoch":"stderr@0","start":0,"end":0,` +
          `"digest":"e3b0c44298fc1c149afbf4c8996fb924","reset":true}`,
      ),
    );
    expect(cursorParams(view)).toEqual({
      cursor: true,
      epoch: "stderr@0",
      after: 0,
      digest: "e3b0c44298fc1c149afbf4c8996fb924",
    });
    view = foldTail(
      view,
      wire(
        `{"text":"cloning\\n","source":"stderr","cut":false,"as_of":"2026-10-06T09:00:03Z",` +
          `"finished":false,"window_bytes":262144,"cursor":true,"epoch":"stderr@0","start":0,` +
          `"end":8,"digest":"d8"}`,
      ),
    );
    expect(view.text).toBe("cloning\n");
    expect(cursorParams(view)).toEqual({ cursor: true, epoch: "stderr@0", after: 8, digest: "d8" });
  });

  // A RESET REPLACES what the view held, and says where it begins.
  test("replaces what it held on a reset", () => {
    let view = foldTail(EMPTY_VIEW, cursored("old\n", 0, true));
    view = foldTail(view, cursored("new\n", 500, true, "transcript@9"));
    expect(view.text).toBe("new\n");
    expect(view.dropped).toBe(500);
    expect(view.epoch).toBe("transcript@9");
  });

  // A DELTA THAT DOES NOT FOLLOW IS NOT SPLICED: the view keeps what it shows
  // and asks for a reset next, rather than showing a fragment as though it
  // continued the text above it.
  test("never splices a delta that does not follow what it holds", () => {
    let view = foldTail(EMPTY_VIEW, cursored("one\n", 0, true));
    view = foldTail(view, cursored("elsewhere\n", 40, false));
    expect(view.text).toBe("one\n");
    expect(cursorParams(view)).toEqual({ cursor: true });
  });

  // IT HOLDS AT MOST WHAT THE RECORD WILL, dropping its FRONT on a line and
  // counting what it dropped.
  test("drops its front on a line once it is past the record's bound, and counts it", () => {
    const line = "x".repeat(99) + "\n";
    let view = foldTail(EMPTY_VIEW, cursored("start\n", 0, true));
    let at = 6;
    for (let i = 0; i < LIVE_OUTPUT_MAX_BYTES / line.length + 10; i++) {
      view = foldTail(view, cursored(line, at, false));
      at += line.length;
    }
    expect(utf8Bytes(view.text)).toBeLessThanOrEqual(LIVE_OUTPUT_MAX_BYTES);
    expect(view.text.startsWith("x")).toBe(true);
    expect(view.text.includes("start")).toBe(false);
    expect(view.dropped + utf8Bytes(view.text)).toBe(at);
    expect(view.end).toBe(at);
  });

  // AN OLDER OWNER'S WINDOW REPLACES, and the view says it holds a window.
  test("holds a window as a window", () => {
    let view = foldTail(EMPTY_VIEW, cursored("held\n", 0, true));
    view = foldTail(view, tail({ text: "the window\n", cut: true }));
    expect(view.text).toBe("the window\n");
    expect(view.mode).toBe("window");
    expect(view.windowBytes).toBe(8 << 10);
    expect(cursorParams(view)).toEqual({ cursor: true });
  });

  // ONLY `not_running` ENDS THE ASKING, and what the view holds stays.
  test("stops on a job that is not running, and on nothing else", () => {
    const held = foldTail(EMPTY_VIEW, cursored("output\n", 0, true));
    for (const outcome of ["launching", "box_paused", "owner_silent", "owner_upgrading"] as const) {
      const view = foldTail(held, { outcome, turn_id: "t", launch_id: "L" });
      expect(view.final, outcome).toBe(false);
      expect(view.text, outcome).toBe("output\n");
    }
    const stopped = foldTail(held, {
      outcome: "not_running",
      turn_id: "t",
      launch_id: "L",
      status: "resumed",
    });
    expect(stopped.final).toBe(true);
    expect(stopped.text).toBe("output\n");
  });
});

describe("trimming a view's front", () => {
  test("cuts on a line and counts bytes, not characters", () => {
    const text = "ああ\n" + "b".repeat(10) + "\n";
    const got = trimFront(text, 12);
    expect(got.text).toBe("b".repeat(10) + "\n");
    expect(got.dropped).toBe(utf8Bytes("ああ\n"));
  });

  test("keeps a single line's own end, on a character, marked", () => {
    const got = trimFront("é".repeat(20), 10);
    expect(got.text.startsWith("…")).toBe(true);
    expect(utf8Bytes(got.text.slice(1))).toBeLessThanOrEqual(10);
    expect(got.dropped + utf8Bytes(got.text.slice(1))).toBe(40);
  });
});
