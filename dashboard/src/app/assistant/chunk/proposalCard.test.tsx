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
    scope: "Worklet list filters",
    args: { type: "object" },
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
        hint: "Restore it in Worklet.",
      },
      retryable: false,
    },
  });
  expect(screen.getByText(/The project is archived\./)).toBeTruthy();
  expect(screen.getByText("Restore it in Worklet.")).toBeTruthy();
});

// A REFUSED DECISION IS SAID ABOVE THE DECISION while the card still waits,
// in the engine's sentence with its next step, and never followed by "Nothing
// changed.", which a failed card says once, of itself. Mutation: keep the
// refusal as the card's failure again and it is never shown on a pending card.
test("a refused decision is said above the decision, and a failure once", () => {
  const refused = new RestError(409, {
    error: "proposal_digest_mismatch",
    message: "This is not the change you were shown.",
  });
  const { refusal } = decisionFailure(refused);
  draw({ scope: "server", state: "proposed" }, { pageMoved: false, failure: null, refusal });
  let said = document.body.textContent ?? "";
  expect(said).toContain(refusal.sentence);
  expect(said).not.toContain("Nothing changed.");
  cleanup();

  draw(
    { scope: "page", state: "failed" },
    { pageMoved: false, failure: "The page refused it.", refusal: null },
  );
  said = document.body.textContent ?? "";
  expect(said.split("Nothing changed.").length - 1).toBe(1);
});

// A CHANGE NOBODY COULD CONFIRM NEVER SAYS "NOTHING CHANGED.": the card's own
// unknown outcome speaks for it. Mutation: drop failureOutcome and the card
// claims nothing changed of a write that may have landed.
test("an unconfirmed change never says nothing changed", () => {
  draw({ scope: "server", state: "unknown" });
  expect(document.body.textContent ?? "").not.toContain("Nothing changed.");
});

test("a change that removes fields leads with them, above every row", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Change through the API",
    removes: ["Project", "Space"],
    changes: [
      { kind: "fact", field: "request", label: "Request", to: "PUT /config/units/core" },
      {
        kind: "added",
        field: "roles.analyst.name",
        label: "Roles › analyst › name",
        to: "Data Analyst",
      },
    ],
    approve_label: "Approve and send",
  });
  const warning = screen.getByText(
    "This also removes Project and Space. Approve only if you asked for that.",
  );
  const firstRow = screen.getByText("PUT /config/units/core");
  expect(warning.compareDocumentPosition(firstRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

test("a change that removes nothing says nothing about removing", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Change through the API",
    changes: [{ kind: "fact", field: "request", label: "Request", to: "PUT /config/units/core" }],
    approve_label: "Approve and send",
  });
  expect(screen.queryByText(/This also removes/)).toBeNull();
});

// A VALUE IS DRAWN AS WHAT IT IS (changeValue.tsx): a list as a list, never
// one line joined with commas; text across lines as a preview of the page,
// its headings and bullets kept and no live link; a request's body as code;
// a colour as its chip beside the hex a screen reader hears. Mutation: draw
// a row's `to` as its text, or drop the block.
test("a list, a page of text, a body and a colour each read as what they are", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Change through the API",
    changes: [
      {
        kind: "added",
        field: "roles.qa.responsibilities",
        label: "Roles › qa › responsibilities",
        to: "Review the tests, Approve or send back",
        items: ["Review the tests", "Approve or send back"],
      },
      {
        kind: "added",
        field: "content",
        label: "Page",
        to: "# Proposed team\n\n## Roles\n\n- **Product Manager**: owns the brief\n- [a link](https://example.com)",
      },
      { kind: "fact", field: "request.body", label: "Request body", to: '{\n  "name": "QA"\n}' },
      {
        kind: "changed",
        field: "colors.ground",
        label: "Dark ground colour",
        from: "#090019",
        to: "#14102B",
      },
    ],
  });
  const rows = [...document.querySelectorAll(".crewlet-change-list__row")];
  const row = (label: string) => rows.find((r) => r.querySelector("dt")?.textContent === label)!;

  const list = row("Roles › qa › responsibilities");
  expect(list.classList.contains("crewlet-change-list__row--block")).toBe(true);
  expect([...list.querySelectorAll("li")].map((li) => li.textContent)).toEqual([
    "Review the tests",
    "Approve or send back",
  ]);

  const page = row("Page");
  // THE DRAWER'S OWN HEADING RHYTHM: a heading of the page is a heading
  // line, never a heading of the dashboard's outline.
  expect([...page.querySelectorAll(".md-heading")].map((h) => h.textContent)).toEqual([
    "Proposed team",
    "Roles",
  ]);
  expect(page.querySelectorAll("li")).toHaveLength(2);
  expect(page.querySelector("a")).toBeNull();

  const body = row("Request body");
  expect(body.classList.contains("crewlet-change-list__row--block")).toBe(true);
  expect(body.querySelector("pre, code")?.textContent).toContain('"name": "QA"');

  const colour = row("Dark ground colour");
  expect(colour.querySelectorAll(".crewlet-color-chip")).toHaveLength(2);
  expect(colour.querySelector("ins")?.textContent).toContain("#14102B");
  expect(colour.classList.contains("crewlet-change-list__row--block")).toBe(false);
});
