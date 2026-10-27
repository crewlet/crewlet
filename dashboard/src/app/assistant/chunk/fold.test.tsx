/**
 * A supporting read is one line the person opens: a table or a record folded
 * to its title, how much it holds, its downloads and its link, remembered open
 * for the page's session. A proposal and a refusal are drawn whole. Each
 * policy of the principles is said once, and a step too quick to time says
 * no time.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { AssistantCardEvent, AssistantEntityCard } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import { CardBlock, entitySummary, stepDuration, type CardHandlers } from "./blocks.tsx";
import { resetCardFolds } from "./fold.ts";

afterEach(() => {
  cleanup();
  resetCardFolds();
});

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

function draw(card: AssistantCardEvent["card"], cardId = "c") {
  return render(
    <Router>
      <CardBlock
        event={{ card_id: cardId, message_id: "m", card }}
        local={undefined}
        on={on}
        opened={false}
      />
    </Router>,
  );
}

const principles: AssistantEntityCard = {
  kind: "unit",
  id: "",
  title: "Company principles",
  facts: [
    { label: "Mission", value: "Build a good relationship." },
    { label: "Vision", value: "Settling up takes seconds." },
  ],
  items: [
    { kind: "unit", id: "1", label: "Money is an integer number of cents." },
    { kind: "unit", id: "2", label: "Miles is the final authority." },
  ],
  items_noun: { one: "policy", many: "policies" },
  open: { label: "Principles", screen: "principles", href: "#/company/principles" },
};

test("a read record is folded to one line that says what it holds, with its link", () => {
  const { container } = draw({ type: "entity", entity: principles });
  const toggle = screen.getByRole("button", {
    name: "Company principles, Mission, vision, 2 policies",
  });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  // THE ROWS ARE PUT AWAY, and the way to the page stays on the line.
  expect(screen.queryByRole("list")).toBeNull();
  const head = container.querySelector<HTMLElement>(".crewlet-card__header-actions")!;
  expect(within(head).getByRole("link", { name: "Principles" })).toBeTruthy();

  fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-expanded")).toBe("true");
  // EACH POLICY ONCE: the card the engine built carries no fact beside a
  // plain value, and the drawer draws each item as one row.
  const rows = within(screen.getByRole("list")).getAllByRole("listitem");
  expect(rows.map((row) => row.textContent)).toEqual([
    "Money is an integer number of cents.",
    "Miles is the final authority.",
  ]);
});

test("what the person opened stays open for the session", () => {
  draw({ type: "entity", entity: principles }, "kept");
  fireEvent.click(screen.getByRole("button", { name: /Company principles/ }));
  cleanup();
  draw({ type: "entity", entity: principles }, "kept");
  expect(
    screen.getByRole("button", { name: "Company principles" }).getAttribute("aria-expanded"),
  ).toBe("true");
  cleanup();
  // Another card is its own: folded until it is opened.
  draw({ type: "entity", entity: principles }, "other");
  expect(
    screen.getByRole("button", { name: /Company principles/ }).getAttribute("aria-expanded"),
  ).toBe("false");
});

test("a read table is folded to its title, its row count, its downloads and its link", () => {
  const { container } = draw({
    type: "table",
    table: {
      title: "Tasks",
      columns: [{ key: "title", label: "Title", type: "text" }],
      rows: [["Settle up"], ["Split the bill"]],
      row_count: 7,
      truncated: true,
      as_of: "2026-10-24T09:41:00Z",
      exports: { csv: "/assistant/cards/c/export.csv", xlsx: "/assistant/cards/c/export.xlsx" },
      open: { label: "Tasks", screen: "work", href: "#/worklet/tasks" },
    },
  });
  expect(screen.getByRole("button", { name: "Tasks, 7 rows" }).getAttribute("aria-expanded")).toBe(
    "false",
  );
  expect(screen.queryByRole("table")).toBeNull();
  const head = container.querySelector<HTMLElement>(".crewlet-card__header-actions")!;
  expect(within(head).getByRole("button", { name: /CSV/ })).toBeTruthy();
  expect(within(head).getByRole("button", { name: /XLSX/ })).toBeTruthy();
});

test("a proposal and a refusal are drawn whole, never folded", () => {
  draw({
    type: "error",
    error: {
      refusal: { code: "forbidden", message: "You need the admin role." },
      phrase: "read the fleet",
    },
  });
  expect(screen.queryByRole("button", { expanded: false })).toBeNull();
  expect(screen.getByText(/You need the admin role\./)).toBeTruthy();
  cleanup();
  draw({
    type: "proposal",
    proposal: {
      proposal_id: "p",
      scope: "server",
      tool: "update_company_principles",
      title: "Update the principles",
      changes: [
        {
          field: "mission",
          kind: "text",
          label: "Mission",
          words: [{ op: "removed", text: "test" }],
        },
      ],
      digest: `sha256:${"a".repeat(64)}`,
      expires_at: "2026-10-24T10:41:00Z",
      state: "proposed",
      approve_label: "Approve and update",
    },
  });
  expect(screen.queryByRole("button", { expanded: false })).toBeNull();
  expect(screen.getByRole("button", { name: "Approve and update" })).toBeTruthy();
});

test("a folded record's summary: its subtitle, else its facts' names, and its count", () => {
  expect(entitySummary(principles)).toBe("Mission, vision, 2 policies");
  expect(
    entitySummary({ kind: "task", id: "FAIR-1", title: "Settle up", subtitle: "FAIR-1" }),
  ).toBe("FAIR-1");
  expect(
    entitySummary({
      kind: "page",
      id: "p",
      title: "Runbook",
      facts: [
        { label: "Status", value: "draft" },
        { label: "URL", value: "x" },
        { label: "Version", value: "4", detail: true },
      ],
      items: [{ kind: "page", id: "c1", label: "Looks right." }],
    }),
  ).toBe("Status, URL, 1 entry");
  expect(entitySummary({ kind: "unit", id: "", title: "Empty" })).toBeUndefined();
});

test("a step too quick to time says no time, and one not yet ended none either", () => {
  expect(stepDuration(undefined)).toBeUndefined();
  expect(stepDuration(30)).toBeUndefined();
  expect(stepDuration(99)).toBeUndefined();
  // THE PRODUCT'S ONE DURATION ([fmtDuration]): milliseconds under a second.
  expect(stepDuration(100)).toBe("100ms");
  expect(stepDuration(400)).toBe("400ms");
  expect(stepDuration(12_400)).toBe("12s");
  expect(stepDuration(845_000)).toBe("14m 5s");
});
