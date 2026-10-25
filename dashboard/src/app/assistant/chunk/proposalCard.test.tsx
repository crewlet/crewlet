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
import { decisionFailure } from "./session.ts";
import type { ProposalLocal } from "./turn.ts";
import { RestError } from "~/protocol/rest.ts";

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

function draw(proposal: Partial<AssistantProposalCard>, local?: ProposalLocal) {
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
      <CardBlock event={event} local={local} on={on} opened={false} />
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

// A REFUSED DECISION SAYS "NOTHING CHANGED." ONCE: the card adds what the
// refusal left changed, so the words the drawer keeps for the refusal are its
// sentence alone. Mutation: end the kept words with "Nothing changed." again
// and the card reads it twice.
test("a refused decision's card says nothing changed once", () => {
  const refused = new RestError(409, {
    error: "proposal_digest_mismatch",
    message: "This is not the change you were shown.",
  });
  const { failure } = decisionFailure(refused);
  draw({ scope: "server", state: "failed" }, { pageMoved: false, failure });
  const said = document.body.textContent ?? "";
  expect(said.split("Nothing changed.").length - 1).toBe(1);
  expect(said).toContain("This is not the change you were shown.");
});
