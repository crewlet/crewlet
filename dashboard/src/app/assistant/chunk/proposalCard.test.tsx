/**
 * A page proposal the screen's own gate refuses holds Approve with the
 * screen's sentence before the person presses anything, and a refused
 * proposal draws the route's next step on its own line.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { AssistantCardEvent, AssistantProposalCard } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import {
  registerPage,
  resetPageContextForTest,
  setFramePage,
  type PageActionSpec,
} from "../context.ts";
import { CardBlock, type CardHandlers } from "./blocks.tsx";

afterEach(() => {
  cleanup();
  resetPageContextForTest();
});

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  undo: () => {},
};

function draw(proposal: Partial<AssistantProposalCard>) {
  const event: AssistantCardEvent = {
    card_id: "c",
    message_id: "m",
    card: {
      type: "proposal",
      proposal: {
        proposal_id: "p",
        scope: "page",
        tool: "propose_page_change",
        title: "Filter this list",
        changes: [],
        digest: `sha256:${"a".repeat(64)}`,
        expires_at: "2026-10-24T10:41:00Z",
        state: "proposed",
        approve_label: "Apply to this list",
        ...proposal,
      },
    },
  };
  render(
    <Router>
      <CardBlock event={event} local={undefined} on={on} opened={false} />
    </Router>,
  );
}

test("the screen's gate holds Approve, and says why, before anything is pressed", () => {
  setFramePage({ screen: "me", title: "My work", path: ["me"], params: {}, query: {} });
  const spec: PageActionSpec<unknown> = {
    scope: "Work list filters",
    parse: (raw) => ({ ok: true, value: raw }),
    preview: () => [{ field: "Assignee", next: "Cleared", current: "Jane" }],
    blocked: () =>
      "This list shows one person's work, so that filter stays as it is. Use All work to filter by someone else.",
    apply: () => {},
  };
  registerPage({ actions: { "work.filter.apply": spec } });
  draw({ page_action: { name: "work.filter.apply", payload: { assignee: null } } });
  const approve = screen.getByRole("button", { name: /Apply to this list/ });
  expect(approve.getAttribute("aria-disabled")).toBe("true");
  expect(
    screen.getAllByText(
      "This list shows one person's work, so that filter stays as it is. Use All work to filter by someone else.",
    ).length,
  ).toBeGreaterThan(0);
});

test("a refused proposal draws the route's next step on its own line", () => {
  draw({
    scope: "server",
    state: "failed",
    failure: {
      refusal: {
        code: "invalid",
        message: "The project is archived.",
        hint: "Restore it in Work.",
      },
      retryable: false,
    },
  });
  expect(screen.getByText(/The project is archived\./)).toBeTruthy();
  expect(screen.getByText("Restore it in Work.")).toBeTruthy();
});
