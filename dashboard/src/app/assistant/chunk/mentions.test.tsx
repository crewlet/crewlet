/**
 * Which records an answer names, and where: the matching rule of
 * `mentions.ts`, on the answer's rendered Markdown.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
import { TestClient } from "~/test/client.tsx";
import { mentionsOf, readsOf, type Read } from "./mentions.ts";

let n = 0;
const event = (card: AssistantCardEvent["card"]): AssistantCardEvent => ({
  card_id: `c${++n}`,
  message_id: "m",
  card,
});

const task = (key: string, title: string, status = "todo") =>
  event({
    type: "entity",
    entity: {
      kind: "task",
      id: key,
      key,
      title,
      subtitle: key,
      facts: [{ label: "Status", value: status, type: "label" }],
    },
  });
const seat = (handle: string, name: string) =>
  event({ type: "entity", entity: { kind: "seat", id: handle, title: name, subtitle: handle } });
const table = (title: string) =>
  event({
    type: "table",
    table: {
      title,
      columns: [],
      rows: [],
      row_count: 0,
      truncated: false,
      as_of: "2026-10-27T09:00:00Z",
    },
  });

/** The answer drawn with each chip as `[id|text]`, and the reads it named. */
function mark(markdown: string, reads: readonly Read[]) {
  const { prose, named } = mentionsOf(reads, (m, key) => (
    <b key={key} data-read={m.read.id} data-by={m.by}>
      {`[${m.read.id}|${m.text}]`}
    </b>
  ));
  const nodes = renderMarkdown(markdown, { ...ASSISTANT_WORDS, prose });
  // INSIDE A CLIENT, as every answer is drawn: a handle the answer writes is
  // drawn from the chart in the store.
  const { container } = render(
    <TestClient>
      <div>{nodes}</div>
    </TestClient>,
  );
  return { text: container.textContent ?? "", html: container.innerHTML, named };
}

test("only a record's first mention is its chip; the rest stay words", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  const { text } = mark("GOP-11 crashes. Hold GOP-11, or ship without it.", reads);
  expect(text).toBe("[task:GOP-11|GOP-11] crashes. Hold GOP-11, or ship without it.");
});

test("a key is matched exactly, at its token's edges", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  expect(mark("GOP-110 and GOP-11-b and gop-11 are not it.", reads).named.size).toBe(0);
  expect(mark("Is it GOP-11?", reads).text).toBe("Is it [task:GOP-11|GOP-11]?");
  expect(mark("(GOP-11)", reads).text).toBe("([task:GOP-11|GOP-11])");
  expect(mark("Hold GOP-11.", reads).text).toBe("Hold [task:GOP-11|GOP-11].");
  expect(mark("GOP-11, then", reads).text).toBe("[task:GOP-11|GOP-11], then");
});

test("words in code, a code block or a link are never matched", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  expect(mark("Run `fix GOP-11` first.", reads).named.size).toBe(0);
  expect(mark("```\nGOP-11\n```", reads).named.size).toBe(0);
  expect(mark("See [GOP-11](https://example.com/GOP-11).", reads).named.size).toBe(0);
  const later = mark("See `GOP-11`, then GOP-11 itself.", reads);
  expect(later.text).toBe("See GOP-11, then [task:GOP-11|GOP-11] itself.");
});

test("a task is never matched by its title, which is a sentence", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  expect(mark("The Android 12 crash is open.", reads).named.size).toBe(0);
});

test("a seat is matched by its name, whole and in its own case", () => {
  const { reads } = readsOf([seat("qa", "QA")]);
  expect(mark("QA recommends holding.", reads).text).toBe("[seat:qa|QA] recommends holding.");
  expect(mark("A QAT or qa run, @qa", reads).named.size).toBe(0);
});

test("a chat room is matched by its name with its #, which is its key", () => {
  // THE ENGINE KEYS A ROOM BY ITS NAME (`#launch`), so the rule is the key's:
  // exact, at the edges of its token, and never inside a longer name.
  const room = event({
    type: "entity",
    entity: { kind: "room", id: "r1", key: "#launch", title: "#launch" },
  });
  const { reads } = readsOf([room]);
  expect(mark("#launch is waiting on the launch copy.", reads).text).toBe(
    "[room:r1|#launch] is waiting on the launch copy.",
  );
  expect(mark("The launch copy waits in #launch-plan.", reads).named.size).toBe(0);
});

test("a direct conversation, which has no name, is never matched by its heading", () => {
  // A ROOM WITH NO NAME HAS NO KEY: it is headed by its people, which are
  // words an answer writes about anything, so it is listed under the answer.
  const direct = event({
    type: "entity",
    entity: { kind: "room", id: "r2", title: "Direct conversation with Ada Lovelace" },
  });
  const { reads } = readsOf([direct]);
  expect(mark("Direct conversation with Ada Lovelace is quiet.", reads).named.size).toBe(0);
});

test("a table and a record of no kind are never matched in words", () => {
  const { reads } = readsOf([
    table("Workload"),
    event({ type: "entity", entity: { id: "gophrase", title: "GoPhrase" } }),
  ]);
  expect(mark("The Workload of GoPhrase is light.", reads).named.size).toBe(0);
});

test("the earliest mention wins, and at one place the longer", () => {
  const { reads } = readsOf([seat("swe", "Senior SWE"), seat("swe-web", "Senior SWE, Web")]);
  const { text } = mark("Senior SWE, Web asked Senior SWE.", reads);
  expect(text).toBe("[seat:swe-web|Senior SWE, Web] asked [seat:swe|Senior SWE].");
});

test("a mention inside emphasis and a list is still found, and the markup kept", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash"), task("GOP-2", "Lesson flow")]);
  const { html, named } = mark("**GOP-11** waits.\n\n- touches GOP-2", reads);
  expect(named).toEqual(new Set(["task:GOP-11", "task:GOP-2"]));
  expect(html).toMatch(/<strong>(<span>)?<b data-read="task:GOP-11" data-by="key">/);
  expect(html).toMatch(/<li>.*touches <b data-read="task:GOP-2"/);
});

test("a record read twice is one read, in the order first read, with the fresher card", () => {
  const first = task("GOP-11", "Android 12 crash", "todo");
  const fresher = task("GOP-11", "Android 12 crash", "in_progress");
  const { reads, others } = readsOf([first, seat("qa", "QA"), fresher, table("Workload")]);
  expect(reads.map((r) => r.id)).toEqual(["task:GOP-11", "seat:qa", "table:Workload\u0000"]);
  expect(reads[2]!.label).toBe("Workload (0)");
  expect(reads[0]!.event).toBe(fresher);
  expect(others).toEqual([]);
});

test("two reads of one tool, told apart by their scope, are two reads", () => {
  // THE ENGINE SAYS A READ'S SCOPE IN ITS TITLE (two direct conversations by
  // their people, one search by two authors), and the drawer keeps one read
  // per title, so the whole title, scope and all, is what tells them apart.
  const { reads } = readsOf([
    table("Messages, with Ada Lovelace"),
    table("Messages, with Backend engineer"),
  ]);
  expect(reads.map((r) => r.id)).toEqual([
    "table:Messages, with Ada Lovelace\u0000",
    "table:Messages, with Backend engineer\u0000",
  ]);
});

test("every card that is not a read is left as it came", () => {
  const notice = event({
    type: "notice",
    notice: { tone: "info", text: "Showing the first 3 rows." },
  });
  const { reads, others } = readsOf([notice, task("GOP-11", "Android 12 crash")]);
  expect(reads).toHaveLength(1);
  expect(others).toEqual([notice]);
});

test("a key the turn never read stays words", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  expect(mark("GOP-12 is unrelated.", reads).named.size).toBe(0);
});

const unit = (key: string, name: string) =>
  event({ type: "entity", entity: { kind: "unit", id: key, title: name } });
const page = (id: string, title: string) =>
  event({ type: "entity", entity: { kind: "page", id, title } });

test("a key in a path or a bare address is part of it, never a mention", () => {
  const { reads } = readsOf([task("GOP-11", "Android 12 crash")]);
  expect(mark("It is at https://x.test/GOP-11 now.", reads).named.size).toBe(0);
  expect(mark("Filed under worklet/GOP-11 and /worklet/GOP-11.", reads).named.size).toBe(0);
  expect(mark("Open ./GOP-11 or ../GOP-11 (/items/GOP-11).", reads).named.size).toBe(0);
  expect(mark("See www.x.test/GOP-11 for it.", reads).named.size).toBe(0);
  // The address is drawn as a model's is on no host of the company's: as
  // written, and no link; the key after it is still the record's mention.
  expect(mark("See https://x.test/a, then GOP-11.", reads).text).toBe(
    "See https://x.test/a, then [task:GOP-11|GOP-11].",
  );
});

test("a one-word name in sentence case is never a sentence's first word, wherever it starts", () => {
  const { reads } = readsOf([unit("design", "Design"), page("p1", "Notes")]);
  expect(mark("Design is late.", reads).named.size).toBe(0);
  expect(mark("Notes from the call are short.", reads).named.size).toBe(0);
  expect(mark("It shipped. Design is late.", reads).named.size).toBe(0);
  expect(mark("**It shipped.** Design is late.", reads).named.size).toBe(0);
  expect(mark("- It shipped\n- Design is late", reads).named.size).toBe(0);
  expect(mark("# Design\n\nIt is late.", reads).named.size).toBe(0);
  expect(mark('"Design is late," she said.', reads).named.size).toBe(0);
  // Anywhere else in a sentence it is the name.
  expect(mark("Ask Design, then read Notes.", reads).text).toBe(
    "Ask [unit:design|Design], then read [page:p1|Notes].",
  );
  expect(mark("**Design** owns it, it said.", reads).named.size).toBe(0);
  expect(mark("It is **Design** that owns it.", reads).named.size).toBe(1);
});

test("a name no ordinary word is spelled like, or of two words, starts a sentence too", () => {
  const { reads } = readsOf([
    unit("brand", "Brand Studio"),
    seat("qa", "QA"),
    seat("gp", "GoPhrase"),
  ]);
  expect(mark("Brand Studio owns it.", reads).text).toBe("[unit:brand|Brand Studio] owns it.");
  expect(mark("QA holds it.", reads).text).toBe("[seat:qa|QA] holds it.");
  expect(mark("GoPhrase ships it.", reads).text).toBe("[seat:gp|GoPhrase] ships it.");
});

test("a name two reads share names neither, and a key still names its record", () => {
  const { reads } = readsOf([seat("qa", "QA"), seat("qa-2", "QA")]);
  expect(reads.length).toBe(2);
  expect(mark("Ask QA about it.", reads).named.size).toBe(0);
  const keyed = readsOf([
    task("GOP-11", "Crash"),
    event({ type: "entity", entity: { kind: "project", id: "a", key: "APP", title: "Mobile" } }),
    event({ type: "entity", entity: { kind: "project", id: "b", key: "WEB", title: "Mobile" } }),
  ]).reads;
  const out = mark("It is in the Mobile project, under APP.", keyed);
  expect(out.text).toBe("It is in the Mobile project, under [project:a|APP].");
});

test("a slash delimits keys: both of two keys either side of it are found", () => {
  const { reads } = readsOf([task("GOP-10", "Login"), task("GOP-11", "Crash")]);
  expect(mark("GOP-10/GOP-11 split.", reads).text).toBe(
    "[task:GOP-10|GOP-10]/[task:GOP-11|GOP-11] split.",
  );
  // ONLY THE FIRST MENTION: a key named before keeps its chip there.
  expect(mark("GOP-11 first, then GOP-10/GOP-11 split.", reads).text).toBe(
    "[task:GOP-11|GOP-11] first, then [task:GOP-10|GOP-10]/GOP-11 split.",
  );
  expect(mark("(GOP-10/GOP-11)", reads).named.size).toBe(2);
});
