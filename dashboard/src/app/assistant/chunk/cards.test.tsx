/**
 * What the drawer draws of the engine's cards: the columns the engine chose for
 * its width, values worded by their type as the screens word them, and the
 * facts kept for the model left out.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import { CardBlock, factText, type CardHandlers } from "./blocks.tsx";

afterEach(cleanup);

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  openHere: () => {},
  undo: () => {},
};

function draw(card: AssistantCardEvent["card"], handlers: CardHandlers = on) {
  return render(
    <Router>
      <CardBlock
        event={{ card_id: "c", message_id: "m", card }}
        local={undefined}
        on={handlers}
        opened={false}
      />
    </Router>,
  );
}

test("a table draws only the columns the engine chose for the drawer, a status in words", () => {
  draw({
    type: "table",
    table: {
      title: "Tasks",
      columns: [
        { key: "title", label: "Title", type: "text" },
        { key: "status", label: "Status", type: "label" },
        { key: "key", label: "Key", type: "text", detail: true },
        { key: "due", label: "Due", type: "datetime", detail: true },
      ],
      rows: [["Settle up", "in_progress", "FAIR-1", null]],
      row_count: 1,
      truncated: false,
      as_of: "2026-10-24T09:41:00Z",
    },
  });
  const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
  expect(heads).toEqual(["Title", "Status"]);
  expect(screen.getByText("In progress")).toBeTruthy();
  expect(screen.queryByText("FAIR-1")).toBeNull();
});

test("a fact is worded by its type, and one kept for the model is not drawn", () => {
  expect(factText({ value: "in_review", type: "label" })).toBe("In review");
  expect(factText({ value: "ada" })).toBe("ada");
  draw({
    type: "entity",
    entity: {
      kind: "task",
      id: "FAIR-1",
      title: "Settle up",
      facts: [
        { label: "Status", value: "todo", type: "label" },
        { label: "Version", value: "7", type: "number", detail: true },
      ],
    },
  });
  expect(screen.getByText("To do")).toBeTruthy();
  expect(screen.queryByText("Version")).toBeNull();
});
