/**
 * A wave bounds one kind of read to `size` in flight: the rest wait in the
 * order they asked, a slot given back starts the next, and an ask withdrawn
 * while it waits is never granted.
 */

import { describe, expect, test } from "vitest";

import { createWave } from "./wave.ts";

describe("a wave of reads", () => {
  test("grants at most its size at once, and the next as each is given back", () => {
    const wave = createWave(2);
    const granted: number[] = [];
    const backs = [0, 1, 2, 3].map((n) => wave.take(() => granted.push(n)));
    expect(granted).toEqual([0, 1]);
    backs[0]!();
    expect(granted).toEqual([0, 1, 2]);
    // GIVEN BACK TWICE, a slot frees once: a second call starts nobody.
    backs[0]!();
    expect(granted).toEqual([0, 1, 2]);
    backs[1]!();
    expect(granted).toEqual([0, 1, 2, 3]);
  });

  test("an ask withdrawn while it waits is never granted and holds no slot", () => {
    const wave = createWave(1);
    const granted: string[] = [];
    const first = wave.take(() => granted.push("first"));
    const gone = wave.take(() => granted.push("gone"));
    wave.take(() => granted.push("last"));
    gone();
    first();
    expect(granted).toEqual(["first", "last"]);
  });
});
