// @vitest-environment node
import { afterEach, describe, expect, test } from "vitest";
import { pinZoneForTest } from "~/lib/prefs.ts";
import type { CodePushCommand, CodePushRow } from "~/protocol/index.ts";
import {
  credentialWords,
  dayAside,
  dayCounts,
  pushSubject,
  pushWords,
  pusherParams,
  type PushPart,
} from "./pushWords.ts";

afterEach(() => pinZoneForTest(""));

const ZERO = "0".repeat(40);
const A = "a".repeat(40);
const B = "b".repeat(40);
const CHANGE = { repo: "web", ref: "web!41", title: "cache the cart badge", state: "merged" };

const cmd = (ref: string, extra: Partial<CodePushCommand> = {}): CodePushCommand => ({
  ref,
  old: A,
  new: B,
  result: "ok",
  ...extra,
});

function row(commands: CodePushCommand[], extra: Partial<CodePushRow> = {}): CodePushRow {
  return {
    push_id: "p",
    repo_id: "id-web",
    position: { stream: "CREWLET_CODE_LOG", generation: 1, seq: 1 },
    at: "2026-11-16T10:00:00Z",
    actor: "frontend",
    actor_kind: "agent",
    via: "git",
    commands,
    received_at: "2026-11-16T10:00:00Z",
    ...extra,
  };
}

/** A sentence as a reader reads it: a chip by its short name, a change by its reference. */
const said = (parts: PushPart[]) =>
  parts
    .map((p) =>
      typeof p === "string"
        ? p
        : "ref" in p
          ? `[${p.ref.replace(/^refs\/(heads|tags)\//, "")}]`
          : "gone" in p
            ? p.gone
            : `[${p.change.ref}]`,
    )
    .join("");

const main = "refs/heads/main";
const branch = "refs/heads/sre/drain";
const tag = "refs/tags/v2026.11.2";

describe("a push's sentence", () => {
  // Mutation: count the refs as commits, and "pushed 1 commit" reads the ref count.
  test.each<[string, CodePushRow, string]>([
    ["moved with commits", row([cmd(main)], { introduced_count: 3 }), "pushed 3 commits to [main]"],
    ["moved with one", row([cmd(main)], { introduced_count: 1 }), "pushed 1 commit to [main]"],
    ["moved with none", row([cmd(main)]), "moved [main]"],
    [
      "forced",
      row([cmd(main, { forced: true })], { introduced_count: 2 }),
      "force-pushed 2 commits to [main]",
    ],
    ["forced with none", row([cmd(main, { forced: true })]), "force-pushed [main]"],
    [
      "a new branch",
      row([cmd(branch, { old: ZERO })], { introduced_count: 1 }),
      "pushed 1 commit to a new branch [sre/drain]",
    ],
    [
      "a new branch, no commit",
      row([cmd(branch, { old: ZERO })]),
      "created the branch [sre/drain]",
    ],
    ["a tag", row([cmd(tag, { old: ZERO })]), "created the tag [v2026.11.2]"],
    ["a branch deleted", row([cmd(branch, { new: ZERO })]), "deleted the branch sre/drain"],
    ["a tag deleted", row([cmd(tag, { new: ZERO })]), "deleted the tag v2026.11.2"],
    ["unchanged", row([cmd(main, { result: "noop" })]), "pushed [main] unchanged"],
    [
      "refused, offered",
      row([cmd(main, { result: "ng", reason: "protected_direct" })], { offered: 1 }),
      "tried to push 1 commit to [main]",
    ],
    [
      "refused, nothing offered said",
      row([cmd(main, { result: "ng", reason: "protected_direct" })]),
      "tried to push to [main]",
    ],
    [
      "several refs",
      row([cmd(main), cmd(branch, { old: ZERO })], { introduced_count: 4 }),
      "pushed 4 commits to 2 refs",
    ],
    [
      "several refs, counted beyond the brief",
      row([cmd(tag, { old: ZERO })], { omitted: { created: 999 } }),
      "updated 1,000 refs",
    ],
    [
      "several refs, every one refused",
      row([cmd(main, { result: "ng" }), cmd(branch, { result: "ng" })], { offered: 3 }),
      "tried to push 3 commits to 2 refs",
    ],
    ["a merge", row([cmd(main)], { via: "merge", change: CHANGE }), "merged [web!41] into [main]"],
    [
      "a queue landing",
      row([cmd(main)], { via: "queue", change: CHANGE }),
      "landed [web!41] on [main] from its queue",
    ],
    [
      "a merge an older engine named no change of",
      row([cmd(main)], { via: "merge" }),
      "merged a change into [main]",
    ],
    [
      "a landing an older engine named no change of",
      row([cmd(main)], { via: "queue" }),
      "landed its queue's train on [main]",
    ],
    [
      "a push that merged a change",
      row([cmd(main)], { change: CHANGE, introduced_count: 1 }),
      "pushed 1 commit to [main], merging [web!41]",
    ],
    [
      "an expiry",
      row([cmd(branch, { new: ZERO })], { via: "expiry" }),
      "deleted the idle seat branch sre/drain",
    ],
    [
      "expiries",
      row([cmd(branch, { new: ZERO }), cmd(main, { new: ZERO })], { via: "expiry" }),
      "deleted 2 idle seat branches",
    ],
    [
      "an import",
      row([cmd(tag)], { via: "import", omitted: { created: 999 } }),
      "imported 1,000 refs",
    ],
    [
      "a mirror's sync",
      row([cmd(main), cmd(tag)], { via: "mirror" }),
      "synced 2 refs from the repository it mirrors",
    ],
  ])("%s", (_, push, words) => {
    expect(said(pushWords(push).parts)).toBe(words);
  });

  // Mutation: read an absent offered as 0, and the sentence says "0 commits".
  test("never says zero commits", () => {
    for (const offered of [undefined, 0]) {
      const push = row([cmd(main, { result: "ng" })], offered === undefined ? {} : { offered });
      expect(said(pushWords(push).parts)).not.toMatch(/\b0 commits?\b/);
    }
  });

  test("counts the refused refs of a push that stored others, and none of one refused whole", () => {
    const some = row([cmd(main, { result: "ng" }), cmd(branch)], { omitted: { refused: 2 } });
    expect([pushWords(some).attempt, pushWords(some).refused]).toEqual([false, 3]);
    const all = row([cmd(main, { result: "ng" })]);
    expect([pushWords(all).attempt, pushWords(all).refused]).toEqual([true, 0]);
  });
});

describe("a push's subject", () => {
  // Mutation: make the merger the subject, and a merge reads as the CTO's push.
  test.each<[string, Partial<CodePushRow>, ReturnType<typeof pushSubject>]>([
    ["merge", { via: "merge", actor: "cto" }, { kind: "gitlet" }],
    ["queue", { via: "queue", actor: "cto" }, { kind: "gitlet" }],
    ["import", { via: "import", actor: "system", actor_kind: "system" }, { kind: "gitlet" }],
    ["mirror", { via: "mirror", actor: "system", actor_kind: "system" }, { kind: "gitlet" }],
    ["expiry", { via: "expiry", actor: "system", actor_kind: "system" }, { kind: "gitlet" }],
    ["a seat's push", {}, { kind: "seat", handle: "frontend" }],
    [
      "a person's push through a token",
      { actor: "pat:jane-laptop", actor_kind: "operator", actor_seat: "jane" },
      { kind: "seat", handle: "jane" },
    ],
    [
      "a credential bound to no seat",
      { actor: "token:deploy", actor_kind: "operator" },
      { kind: "credential", login: "token:deploy" },
    ],
  ])("%s", (_, extra, subject) => {
    expect(pushSubject(row([cmd(main)], extra))).toEqual(subject);
  });

  test("a credential is named by its kind", () => {
    expect(
      ["pat:a", "token:b", "session:c", "other"].map((login) => credentialWords(login)),
    ).toEqual(["Personal access token", "Deployment token", "Signed-in session", "Credential"]);
  });
});

describe("what by= asks", () => {
  const kinds: Record<string, string> = { jane: "person", frontend: "agent" };
  test.each<[string, Record<string, string>]>([
    ["", {}],
    ["gitlet", { pusher_kind: "gitlet" }],
    ["frontend", { pusher_kind: "agent", pusher: "frontend" }],
    ["jane", { pusher_kind: "person", pusher: "jane" }],
    ["unheard-of", { pusher_kind: "agent", pusher: "unheard-of" }],
  ])("by=%j", (by, params) => {
    expect(pusherParams(by, (h) => kinds[h])).toEqual(params);
  });
});

describe("a day's heading count", () => {
  // Mutation: say the loaded count alone, and a partly loaded day reads as whole.
  test.each<[number, number | undefined, string]>([
    [8, 8, "8"],
    [2, 13, "2 of 13"],
    [3, undefined, "3 loaded"],
    [9, 8, "9"],
  ])("%i loaded of %s counted says %j", (loaded, counted, text) => {
    expect(dayAside(loaded, counted, "Nov 14").text).toBe(text);
  });

  test("names the day's count and how many are loaded on its title", () => {
    expect(dayAside(2, 13, "Nov 14").title).toBe("13 pushes on Nov 14, 2 of them loaded");
  });

  test("keys each count by the reader's day its bound starts", () => {
    pinZoneForTest("Asia/Kathmandu");
    const counts = dayCounts(
      ["2026-11-14T18:15:00Z", "2026-11-15T18:15:00Z", "2026-11-16T18:15:00Z"],
      [13, 8],
    );
    expect([...counts]).toEqual([
      ["2026-11-15", 13],
      ["2026-11-16", 8],
    ]);
    expect(dayCounts(["2026-11-14T18:15:00Z"], undefined).size).toBe(0);
  });
});
