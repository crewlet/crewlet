/**
 * What the drawer draws of the engine's cards: the columns the engine chose for
 * its width, values worded by their type as the screens word them, the facts
 * kept for the model left out, a failed step's reason, "Prepare it again" on a
 * card that moved, and a run that took no step drawn without a count.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AssistantCardEvent, AssistantProposalCard } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import {
  CardBlock,
  factText,
  prepareAgainQuestion,
  ReplyProse,
  runChipOf,
  stepsOf,
  type CardHandlers,
} from "./blocks.tsx";
import type { TurnView } from "./turn.ts";

afterEach(cleanup);

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
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

const proposal = {
  proposal_id: "p",
  scope: "server",
  tool: "update_task",
  title: "Update task",
  changes: [],
  digest: `sha256:${"a".repeat(64)}`,
  expires_at: "2026-10-24T10:41:00Z",
  state: "stale",
  approve_label: "Approve and update",
} as AssistantProposalCard;

test("a card that moved offers Prepare it again, which asks for the change afresh", () => {
  const prepareAgain = vi.fn();
  draw({ type: "proposal", proposal }, { ...on, prepareAgain });
  fireEvent.click(screen.getByRole("button", { name: /Prepare it again/ }));
  expect(prepareAgain).toHaveBeenCalledWith(proposal);
  expect(prepareAgainQuestion(proposal)).toContain("Update task");
});

test("a pending card does not offer Prepare it again", () => {
  draw({ type: "proposal", proposal: { ...proposal, state: "proposed" } });
  expect(screen.queryByRole("button", { name: /Prepare it again/ })).toBeNull();
});

const base: TurnView = {
  key: "k",
  id: "t",
  opKey: "o",
  phase: "completed",
  seq: 1,
  question: "q",
  page: null,
  askedAt: "2026-10-24T09:41:00Z",
  stepsDone: 0,
  steps: [],
  replies: [],
  cards: [],
  proposals: {},
  actions: [],
  actionStates: {},
  done: { state: "completed", duration_ms: 1000, steps: 0 },
  error: null,
  refusal: null,
  stopping: false,
};

test("a failed step says why, in the engine's words", () => {
  const [step] = stepsOf({
    ...base,
    steps: [
      {
        step_id: "s",
        index: 1,
        tool: "update_task",
        label: "Preparing a change to FAIR-2",
        state: "failed",
        started_at: "2026-10-24T09:41:00Z",
        refusal: {
          code: "stale",
          message: "FAIR-2 changed since it was read.",
          hint: "Read it again.",
        },
      },
    ],
  });
  expect(step!.detail).toContain("FAIR-2 changed since it was read. Read it again.");
});

test("a run that took no step carries no count", () => {
  expect(runChipOf(base, Date.parse("2026-10-24T09:41:05Z")).meta).toBeUndefined();
});

test("a reply's paragraphs are blocks in a div the Markdown rhythm spaces", () => {
  const { container } = render(<ReplyProse text={"First paragraph.\n\nSecond paragraph."} />);
  const root = container.firstElementChild!;
  expect(root.tagName).toBe("DIV");
  expect(root.classList.contains("prose") && root.classList.contains("md")).toBe(true);
  expect(root.querySelectorAll("p")).toHaveLength(2);
});
