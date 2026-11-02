import { describe, expect, test } from "vitest";

import { waitingWords } from "./inboxWaiting.ts";

describe("what waits in a seat's inbox, in words", () => {
  test("a person waiting is said first, apart from the rest, and agrees in number", () => {
    expect(waitingWords({ priority: 1, background: 4 })).toEqual([
      { text: "1 person waiting", person: true },
      { text: "4 waiting", person: false },
    ]);
    expect(waitingWords({ priority: 2, background: 1 }).map((w) => w.text)).toEqual([
      "2 people waiting",
      "1 waiting",
    ]);
  });

  test("an empty mailbox, or no report at all, says nothing", () => {
    expect(waitingWords({ priority: 0, background: 0 })).toEqual([]);
    expect(waitingWords({ priority: 0, background: 3 }).map((w) => w.text)).toEqual(["3 waiting"]);
    expect(waitingWords(null)).toEqual([]);
    expect(waitingWords(undefined)).toEqual([]);
  });
});
