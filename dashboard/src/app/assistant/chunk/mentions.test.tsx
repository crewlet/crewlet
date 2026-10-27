/**
 * Which records an answer names, and where: the matching rule of
 * `mentions.ts`, on the answer's rendered Markdown.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";
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
  const { container } = render(<div>{nodes}</div>);
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
  expect(reads[0]!.event).toBe(fresher);
  expect(others).toEqual([]);
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
