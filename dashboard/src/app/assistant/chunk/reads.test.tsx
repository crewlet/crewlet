/**
 * An answer and the records it read: no card per read, the first mention of
 * each a chip whose peek holds what the card held, the rest on one Also read
 * line, and every other card drawn as it was.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import { CardBlock, type CardHandlers } from "./blocks.tsx";
import { Answer } from "./reads.tsx";

afterEach(cleanup);

const handlers = (): CardHandlers => ({
  approve: vi.fn(),
  decline: vi.fn(),
  prepareAgain: vi.fn(),
  openHere: vi.fn(),
  ask: vi.fn(),
  undo: vi.fn(),
});

let n = 0;
const event = (card: AssistantCardEvent["card"]): AssistantCardEvent => ({
  card_id: `c${++n}`,
  message_id: "m",
  card,
});

const crash = (status = "in_progress") =>
  event({
    type: "entity",
    entity: {
      kind: "task",
      id: "GOP-11",
      key: "GOP-11",
      title: "Android 12 crash when lesson audio starts",
      subtitle: "GOP-11",
      facts: [
        { label: "Status", value: status, type: "label" },
        { label: "Version", value: "4", type: "number", detail: true },
      ],
      open: { label: "GOP-11", screen: "item", href: "#/worklet/GOP-11" },
    },
  });
const qa = event({
  type: "entity",
  entity: {
    kind: "seat",
    id: "qa",
    title: "QA",
    subtitle: "qa",
    facts: [{ label: "State", value: "Idle" }],
    open: { label: "QA", screen: "seat", href: "#/company/org/seats/qa" },
  },
});
const waiting = event({
  type: "table",
  table: {
    title: "Decisions waiting on you",
    columns: [
      { key: "task", label: "Task", type: "label" },
      { key: "question", label: "Question", type: "text" },
    ],
    rows: [["GOP-11", "Hold the beta?"]],
    row_count: 1,
    truncated: false,
    as_of: "2026-10-27T09:00:00Z",
    exports: {
      csv: "/assistant/exports/waiting.csv",
      xlsx: "/assistant/exports/waiting.xlsx",
    },
    open: { label: "Inbox", screen: "inbox", href: "#/inbox" },
  },
});

function draw(text: string, cards: AssistantCardEvent[], on = handlers()) {
  render(
    <Router>
      <Answer
        text={text}
        dimmed={false}
        cards={cards}
        card={(e) => (
          <CardBlock key={e.card_id} event={e} local={undefined} on={on} opened={false} />
        )}
        on={on}
      />
    </Router>,
  );
  return on;
}

test("the first mention of a read record is its chip, and later ones stay words", () => {
  draw("**GOP-11** is QA asking whether to hold the beta. Hold GOP-11 if unsure.", [crash(), qa]);
  const chips = screen.getAllByRole("button");
  expect(chips.map((c) => c.textContent)).toEqual(["GOP-11", "QA"]);
  expect(chips[0]!.getAttribute("aria-label")).toBe(
    "GOP-11, Android 12 crash when lesson audio starts",
  );
  expect(screen.queryByRole("list", { name: "Also read" })).toBeNull();
});

test("a record read twice is one chip, drawn from its fresher read", () => {
  draw("GOP-11 waits on you.", [crash("todo"), crash("in_progress")]);
  expect(screen.getAllByRole("button")).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: /GOP-11/ }));
  expect(within(screen.getByRole("dialog")).getByText("In progress")).toBeTruthy();
});

test("what the answer read and never named is on the Also read line, and nothing is a card", () => {
  draw("The beta waits on one decision.", [crash(), waiting, qa]);
  const line = screen.getByRole("list", { name: "Also read" });
  expect(
    within(line)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["GOP-11", "Decisions waiting on you (1)", "QA"]);
  expect(document.querySelector(".crewlet-status-card")).toBeNull();
  expect(document.querySelector(".crewlet-result-table-card")).toBeNull();
});

test("a record's peek says its key once, its facts, and opens its page in place", async () => {
  const on = draw("GOP-11 waits.", [crash()]);
  const chip = screen.getByRole("button", { name: /GOP-11/ });
  fireEvent.click(chip);
  const dialog = screen.getByRole("dialog", {
    name: "GOP-11 Android 12 crash when lesson audio starts",
  });
  expect(dialog.textContent!.split("GOP-11").length - 1).toBe(1);
  expect(within(dialog).getByText("In progress")).toBeTruthy();
  expect(within(dialog).queryByText("Version")).toBeNull();
  const open = within(dialog).getByRole("link", { name: "Open GOP-11" });
  expect(open.getAttribute("href")).toBe("#/worklet/GOP-11");
  fireEvent.click(open, { button: 0 });
  expect(on.openHere).toHaveBeenCalledWith(
    expect.objectContaining({ path: ["worklet", "GOP-11"] }),
  );
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("a table's peek holds its rows, its downloads and the way to its page", () => {
  draw("", [waiting]);
  fireEvent.click(screen.getByRole("button", { name: "Decisions waiting on you (1)" }));
  const dialog = screen.getByRole("dialog", { name: "Decisions waiting on you" });
  expect(
    within(dialog)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Task", "Question"]);
  expect(within(dialog).getByRole("button", { name: /CSV/ })).toBeTruthy();
  expect(within(dialog).getByRole("button", { name: /XLSX/ })).toBeTruthy();
  expect(
    within(dialog)
      .getByRole("link", { name: /Open in Inbox/ })
      .getAttribute("href"),
  ).toBe("#/inbox");
});

test("an address that is not this dashboard's own draws no way there", () => {
  const elsewhere = event({
    type: "entity",
    entity: {
      kind: "task",
      id: "GOP-12",
      key: "GOP-12",
      title: "Somewhere else",
      open: { label: "GOP-12", screen: "item", href: "https://example.com/GOP-12" },
    },
  });
  draw("GOP-12 is elsewhere.", [elsewhere]);
  fireEvent.click(screen.getByRole("button", { name: /GOP-12/ }));
  expect(within(screen.getByRole("dialog")).queryByRole("link")).toBeNull();
});

test("a read too large to show is listed by what it read, and its peek opens its screen and says what narrows it", () => {
  const cut = event({
    type: "oversize",
    oversize: {
      title: 'Matching tasks, "brand identity"',
      open: { label: "Search", screen: "worklet-search", href: "#/worklet/search" },
      narrow: ["project", "status", "the search words"],
    },
  });
  draw("Nothing in Crewlet holds a brand identity yet.", [cut]);
  const line = screen.getByRole("list", { name: "Also read" });
  const chip = within(line).getByRole("button");
  expect(chip.textContent).toBe('Matching tasks, "brand identity" (too large to show here)');
  expect(chip.getAttribute("title")).toBe(chip.textContent);
  fireEvent.click(chip);
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("none of it is shown here");
  expect(dialog.textContent).toContain("narrowed by project, status or the search words");
  expect(
    within(dialog)
      .getByRole("link", { name: /Open in Search/ })
      .getAttribute("href"),
  ).toBe("#/worklet/search");
  expect(document.body.textContent).not.toContain("open it in the app");
});

test("a list read is one chip, named by what it read and how many rows, however many rows it holds", () => {
  const twenty = event({
    type: "table",
    table: {
      title: "Tasks, GP",
      columns: [{ key: "key", label: "Task", type: "label" }],
      rows: Array.from({ length: 20 }, (_, i) => [`GP-${i + 1}`]),
      row_count: 20,
      truncated: false,
      as_of: "2026-10-27T09:00:00Z",
    },
  });
  draw("Twenty tasks are open.", [twenty]);
  expect(
    within(screen.getByRole("list", { name: "Also read" })).getAllByRole("button"),
  ).toHaveLength(1);
  expect(screen.getByRole("button", { name: "Tasks, GP (20)" })).toBeTruthy();
});

test("every card that is not a read is drawn as it was, after the answer", () => {
  const notice = event({
    type: "notice",
    notice: { tone: "info", text: "Showing the first 3 rows." },
  });
  draw("GOP-11 waits.", [crash(), notice]);
  expect(screen.getByText("Showing the first 3 rows.")).toBeTruthy();
});

test("a page of a list says so on its chip and in its peek, never in a notice beside it", () => {
  const page = event({
    type: "table",
    table: {
      title: "Turns",
      columns: [{ key: "turn", label: "Turn", type: "text" }],
      rows: [["t1"], ["t2"], ["t3"]],
      row_count: 3,
      truncated: false,
      more: true,
      as_of: "2026-10-27T09:00:00Z",
      open: { label: "Turns", screen: "turns", href: "#/live/turns" },
    },
  });
  const cut = event({
    type: "table",
    table: {
      title: "Matching tasks",
      columns: [{ key: "key", label: "Task", type: "label" }],
      rows: [["GP-1"], ["GP-2"]],
      row_count: 210,
      truncated: true,
      as_of: "2026-10-27T09:00:00Z",
    },
  });
  draw("", [page, cut]);
  const chips = within(screen.getByRole("list", { name: "Also read" })).getAllByRole("button");
  expect(chips.map((c) => c.textContent)).toEqual([
    "Turns (first 3)",
    "Matching tasks (first 2 of 210)",
  ]);
  fireEvent.click(chips[0]!);
  expect(screen.getByRole("dialog").textContent).toContain(
    "First 3 rows are shown here; Turns shows all of them.",
  );
  expect(document.querySelector(".crewlet-callout")).toBeNull();
});
