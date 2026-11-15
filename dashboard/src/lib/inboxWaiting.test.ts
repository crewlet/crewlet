import { describe, expect, test } from "vitest";

import { waitingCount, waitingWords } from "./inboxWaiting.ts";

// THE COUNTS ARE MESSAGES, NOT PEOPLE: the priority lane holds messages a
// person wrote, so four people who wrote 79 messages read "79 people waiting"
// in a company of four. Mutation: word `priority` as people again and the
// first case goes red.
describe("what waits in a seat's inbox, in words", () => {
  test("messages from people are said first, as messages, apart from the rest", () => {
    expect(waitingWords({ priority: 79, background: 9 })).toEqual([
      { text: "79 messages from people", person: true },
      { text: "9 other notices", person: false },
    ]);
    expect(waitingWords({ priority: 1, background: 1 }).map((w) => w.text)).toEqual([
      "1 message from a person",
      "1 other notice",
    ]);
  });

  test("notices alone are not 'other', and agree in number", () => {
    expect(waitingWords({ priority: 0, background: 3 }).map((w) => w.text)).toEqual(["3 notices"]);
    expect(waitingWords({ priority: 0, background: 1 }).map((w) => w.text)).toEqual(["1 notice"]);
    expect(waitingWords({ priority: 2, background: 0 }).map((w) => w.text)).toEqual([
      "2 messages from people",
    ]);
  });

  test("an empty mailbox, or no report at all, says nothing", () => {
    expect(waitingWords({ priority: 0, background: 0 })).toEqual([]);
    expect(waitingWords(null)).toEqual([]);
    expect(waitingWords(undefined)).toEqual([]);
  });
});

// ONE QUIET COUNT on a line that must hold still, saying what it counts, with
// the words it folds in its tooltip, and nothing at all for an empty or
// unreported mailbox. A bare "22 waiting" said neither what waits nor where.
test("what waits as one count of what is queued, the words in its detail", () => {
  expect(waitingCount({ priority: 13, background: 9 })).toEqual({
    text: "22 queued",
    detail: "Queued in its inbox: 13 messages from people · 9 other notices",
  });
  expect(waitingCount({ priority: 0, background: 4 })).toEqual({
    text: "4 queued",
    detail: "Queued in its inbox: 4 notices",
  });
  expect(waitingCount({ priority: 0, background: 0 })).toBeNull();
  expect(waitingCount(null)).toBeNull();
  expect(waitingCount(undefined)).toBeNull();
});
