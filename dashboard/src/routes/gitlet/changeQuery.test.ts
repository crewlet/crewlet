// @vitest-environment node
/** Gitlet › Changes' query: its grammar, its chips, the patch a submit writes, and its words. */

import { afterEach, expect, test, vi } from "vitest";
import { READINESS_BUCKETS } from "~/contract/code.ts";
import {
  READINESS_WORDS,
  filterChips,
  isReadinessBucket,
  parseChangeQuery,
  submitPatch,
  type Filters,
} from "./changeQuery.ts";

afterEach(() => {
  vi.doUnmock("~/contract/code.ts");
  vi.resetModules();
});

const NONE: Filters = {
  state: "open",
  needs: "",
  repo: "",
  owner: "",
  reviewer: "",
  work: "",
  window: "",
  readiness: "",
  text: "",
};

// Mutation: keep qualifiers in the words.
test("words and qualifiers part, each qualifier writing its key", () => {
  expect(parseChangeQuery("is:draft repo:web  author:dev stream shell", "jane")).toEqual({
    filters: { state: "draft", repo: "web", owner: "dev" },
    words: "stream shell",
    unknown: [],
  });
  expect(
    parseChangeQuery("reviewer:cto waiting-on:review readiness:checks updated:7d", "jane").filters,
  ).toEqual({ reviewer: "cto", needs: "review", readiness: "checks", window: "7d" });
});

// Mutation: cut the run at its last colon.
test("work: keeps its own colon", () => {
  expect(parseChangeQuery("work:worklet:t-415", "jane").filters).toEqual({ work: "worklet:t-415" });
});

// Mutation: store @me as written, or let it name nobody silently.
test("@me is the reader's handle, and names nobody for a reader bound to no seat", () => {
  expect(parseChangeQuery("author:@me reviewer:@me", "jane").filters).toEqual({
    owner: "jane",
    reviewer: "jane",
  });
  expect(parseChangeQuery("author:@me", "").problem).toBe(
    "@me names nobody: you are bound to no seat.",
  );
});

// Mutation: let a value the screen does not read through, or hard-code a qualifier's values.
test("a known qualifier's value it does not take is a problem in the screen's words", () => {
  const problem = (text: string) => parseChangeQuery(text, "jane").problem;
  expect(problem("is:soon")).toBe(
    "“soon” is not a state: is: takes draft, open, closed or merged.",
  );
  expect(problem("waiting-on:everyone")).toBe(
    "“everyone” is not what a change waits on: waiting-on: takes review, fix, me or person.",
  );
  expect(problem("readiness:soon")).toBe(
    "“soon” is not where a merge stands: readiness: takes ready, approval, checks or blockers.",
  );
  expect(problem("updated:1y")).toBe("“1y” is not a window: updated: takes 24h, 7d or 30d.");
  expect(problem("repo:Bad_Name!")).toBe("“Bad_Name!” is not a name a repository can have.");
  expect(problem("work:t-415")).toBe(
    "“t-415” is not a work item: work: takes <backend>:<id>, as a task's link writes it.",
  );
});

// Mutation: hard-code a qualifier's values.
test("a qualifier's values are listed from the contract's set", async () => {
  vi.doMock("~/contract/code.ts", async (actual) => ({
    ...(await actual<typeof import("~/contract/code.ts")>()),
    CHANGE_STATES: ["draft", "open", "closed", "merged", "parked"],
  }));
  vi.resetModules();
  const fresh = await import("./changeQuery.ts");
  expect(fresh.parseChangeQuery("is:soon", "jane").problem).toBe(
    "“soon” is not a state: is: takes draft, open, closed, merged or parked.",
  );
});

// Mutation: let the second value win, or submit it.
test("a key named twice is a problem", () => {
  expect(parseChangeQuery("is:open is:draft", "jane").problem).toBe(
    "is: is named twice: a search takes one of each.",
  );
});

// Mutation: drop an unknown qualifier from the words, or not note it.
test("an unknown name:value is words and is noted; a bare name: is a word", () => {
  expect(parseChangeQuery("auther:jane render: cart auther:sam", "jane")).toEqual({
    filters: {},
    words: "auther:jane render: cart auther:sam",
    unknown: ["auther"],
  });
  expect(parseChangeQuery("IS:open 12:30", "jane").unknown).toEqual(["IS"]);
});

// Mutation: drop the default chip, or draw every state's.
test("the chips are is:open where no state is named, none for every state, then the address's keys", () => {
  expect(filterChips(NONE)).toEqual([{ id: "state", label: "is:open" }]);
  expect(filterChips({ ...NONE, state: "all", repo: "web" })).toEqual([
    { id: "repo", label: "repo:web" },
  ]);
  expect(
    filterChips({
      ...NONE,
      work: "worklet:t-415",
      window: "7d",
      readiness: "checks",
      needs: "me",
      repo: "web",
      reviewer: "jane",
      owner: "dev",
      text: "cart",
    }).map((c) => c.label),
  ).toEqual([
    "is:open",
    "author:dev",
    "reviewer:jane",
    "repo:web",
    "waiting-on:me",
    "readiness:checks",
    "updated:7d",
    "work:worklet:t-415",
  ]);
});

// Mutation: drop a key the box does not name, or write the default state.
test("a submit writes the box's keys over the address's, keeps the rest, and q only with words", () => {
  const current = { ...NONE, state: "draft", repo: "web", text: "cart" };
  expect(submitPatch(current, parseChangeQuery("author:dev", "jane"))).toEqual({
    state: "draft",
    owner: "dev",
    reviewer: null,
    repo: "web",
    needs: null,
    readiness: null,
    window: null,
    work: null,
    q: null,
  });
  expect(submitPatch(current, parseChangeQuery("is:open shell", "jane"))).toMatchObject({
    state: null,
    q: "shell",
  });
  // A BUCKET NAMED WITHOUT A STATE lists the open changes, as the picker does.
  expect(submitPatch(current, parseChangeQuery("readiness:ready", "jane")).state).toBeNull();
});

// Mutation: drop a bucket's words.
test("every readiness bucket the engine takes has its words, and nothing else is a bucket", () => {
  for (const bucket of READINESS_BUCKETS) {
    expect(READINESS_WORDS[bucket], bucket).toMatch(/^[A-Z]/);
    expect(isReadinessBucket(bucket)).toBe(true);
  }
  expect(Object.keys(READINESS_WORDS)).toEqual([...READINESS_BUCKETS]);
  expect(isReadinessBucket("person")).toBe(false);
});
