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
  ask: () => {},
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

test("what a change takes away leads the card, above every row", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Remove seat QA Engineer",
    summary: "This removes seat QA Engineer from unit Product and Engineering.",
    note: "The previous version stays in the configuration history, so this can be reverted.",
    risk: "destructive",
    changes: [
      { kind: "removed", field: "roles.qa.name", label: "QA Engineer › Name", from: "QA Engineer" },
    ],
    approve_label: "Approve and remove",
  });
  const summary = screen.getByText(
    "This removes seat QA Engineer from unit Product and Engineering.",
  );
  const firstRow = screen.getByText("QA Engineer › Name");
  expect(summary.compareDocumentPosition(firstRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  screen.getByText(
    "The previous version stays in the configuration history, so this can be reverted.",
  );
});

test("a change that removes nothing says nothing about removing", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Add seat Technical Writer",
    changes: [
      {
        kind: "added",
        field: "roles.tw.name",
        label: "Technical Writer › Name",
        to: "Technical Writer",
      },
    ],
    approve_label: "Approve and add",
  });
  expect(screen.queryByText(/removes/)).toBeNull();
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
    title: "Change unit Product and Engineering",
    changes: [
      {
        kind: "added",
        field: "roles.qa.responsibilities",
        label: "QA Engineer › Responsibilities",
        to: "Review the tests, Approve or send back",
        items: ["Review the tests", "Approve or send back"],
      },
      {
        kind: "added",
        field: "content",
        label: "Page",
        to: "# Proposed team\n\n## Roles\n\n- **Product Manager**: owns the brief\n- [a link](https://example.com)",
      },
      { kind: "added", field: "settings", label: "Settings", to: '{\n  "name": "QA"\n}' },
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

  const list = row("QA Engineer › Responsibilities");
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

  const body = row("Settings");
  expect(body.classList.contains("crewlet-change-list__row--block")).toBe(true);
  expect(body.querySelector("pre, code")?.textContent).toContain('"name": "QA"');

  const colour = row("Dark ground colour");
  expect(colour.querySelectorAll(".crewlet-color-chip")).toHaveLength(2);
  expect(colour.querySelector("ins")?.textContent).toContain("#14102B");
  expect(colour.classList.contains("crewlet-change-list__row--block")).toBe(false);
});

// A COMPANY CHANGE STILL WAITING IS NOT "SAVED": the subtitle says where it
// lands in words true at its state. Mutation: say "Saved" on a pending card.
test("a pending company change does not say it is saved", () => {
  draw({ scope: "server", state: "proposed", title: "Add seat Technical Writer" });
  expect(screen.queryByText("Saved for the company")).toBeNull();
  screen.getByText("For the company, once approved");
});

// THE EXACT REQUEST IS AN ADMINISTRATOR'S: the engine sends it to them alone,
// and the card draws it after every other row. Mutation: drop the technical
// rows, or draw a request the engine did not send.
test("the technical details are drawn when the engine sends them, and only then", () => {
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Add seat Technical Writer",
    changes: [
      { kind: "added", field: "roles.tw.name", label: "Technical Writer › Name", to: "TW" },
    ],
    technical: { method: "PUT", path: "/config/units/core", body: '{\n  "id": "core"\n}' },
  });
  screen.getByText("PUT /config/units/core");
  cleanup();
  draw({
    scope: "server",
    tool: "propose_api_change",
    title: "Add seat Technical Writer",
    changes: [
      { kind: "added", field: "roles.tw.name", label: "Technical Writer › Name", to: "TW" },
    ],
  });
  expect(screen.queryByText(/technical details/)).toBeNull();
});

// A PAGE'S BODY IS DRAWN AS A LINE DIFF, as the page's History draws it: its
// headings and bullets stay lines, and an unchanged line ("suffices.") is never
// shown as removed and added again. Mutation: draw a multi-line text change as
// a word diff.
test("a change to a page's body reads as a line diff, like its History", () => {
  const before = "# Plan\n\nOne owner suffices.\n- Ship it";
  const after = "# Plan\n\n## TL;DR\n- One owner\n\nOne owner suffices.\n- Ship it";
  draw({
    scope: "server",
    tool: "edit_page",
    title: "Edit page",
    changes: [
      {
        kind: "text",
        field: "content",
        label: "Page",
        from: before,
        to: after,
        words: [
          { op: "same", text: "# Plan\n\n" },
          { op: "added", text: "## TL;DR\n- One owner\n\n" },
          { op: "same", text: "One owner suffices.\n- Ship it" },
        ],
      },
    ],
  });
  const diff = screen.getByRole("group", { name: "Page" });
  const removed = diff.querySelectorAll(".crewlet-diff-view__line--remove");
  const added = [...diff.querySelectorAll(".crewlet-diff-view__line--add")].map(
    (line) => line.querySelector(".crewlet-diff-view__text")?.textContent,
  );
  expect(removed).toHaveLength(0);
  expect(added.map((text) => text?.trim())).toEqual(["## TL;DR", "- One owner", ""]);
});
