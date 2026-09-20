/**
 * THE OUTLINE IS DERIVED, AND THE RECORD IS UNCHANGED.
 *
 * Two claims, and the screen is worth nothing without either. A prompt's
 * sections are whatever headings `internal/agent/prompts` wrote, so this file
 * may not name one — a case asserting "Review has a self_iterate section"
 * would pass on a component that hardcoded the list and go on passing after
 * the engine renamed it. And a section's body is the SOURCE, so what the fold
 * shows has to be the bytes the model was handed rather than a reading of
 * them: this surface is where an operator reproduces a turn from.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PromptRecord } from "./PromptDoc.tsx";

afterEach(cleanup);

// A system prompt shaped like the ones the engine builds: a lead line, then a
// flat run of `##` sections. Written here rather than imported, because what
// is asserted below is the SHAPE — a document with sections — and pinning it
// to today's prompt text would make this a test of the prompt.
const SYSTEM = [
  "You are **Engineer** at **Acme** (reports to: Lead).",
  "",
  "## REVIEW phase",
  "Judge the work below.",
  "- **done** — it meets the ask.",
  "",
  "## What the agent did",
  "- post_message(...) → success",
  "",
  "## What the agent produced",
  "Posted the summary.",
].join("\n");

function draw(over: { phase?: string; system?: string; user?: string } = {}) {
  return render(
    <PromptRecord
      phase={over.phase ?? "review"}
      system={over.system ?? SYSTEM}
      user={over.user ?? ""}
    />,
  );
}

/** Every fold the outline drew, by the words on its trigger. */
function folds(): string[] {
  return screen.getAllByRole("button").map((b) => b.textContent ?? "");
}

describe("a prompt with headings", () => {
  it("draws one fold per heading, named by the heading", () => {
    draw();
    const triggers = folds();
    expect(triggers.some((t) => t.startsWith("REVIEW phase"))).toBe(true);
    expect(triggers.some((t) => t.startsWith("What the agent did"))).toBe(true);
    expect(triggers.some((t) => t.startsWith("What the agent produced"))).toBe(true);
  });

  it("shows the run before the first heading without asking for a click", () => {
    // It has no title, so there is nothing to call the control that would
    // hide it — and it is the first thing the model read.
    draw();
    expect(screen.getByText(/You are \*\*Engineer\*\*/)).toBeTruthy();
  });

  it("keeps a closed section's body out of the card entirely", () => {
    // `lazy`, which here is the behaviour rather than an optimisation: an
    // outline whose every body is mounted is the wall of text it replaces,
    // and a closed fold must not put its text into the card.
    const { container } = draw();
    expect(container.textContent).not.toContain("post_message(...) → success");
  });

  it("shows a section's source verbatim once it is opened", () => {
    draw();
    fireEvent.click(screen.getByRole("button", { name: /What the agent did/ }));
    // VERBATIM: the bullet's own markdown, not this app's rendering of it.
    expect(screen.getByText("- post_message(...) → success")).toBeTruthy();
  });

  it("offers the whole document as one block, byte for byte", () => {
    // The escape hatch the outline owes: an operator copying a prompt to
    // reproduce a turn must not have to open a dozen folds to do it.
    const { container } = draw();
    fireEvent.click(screen.getByRole("radio", { name: "Whole" }));
    expect(container.textContent).toContain("Judge the work below.");
    expect(container.textContent).toContain("- post_message(...) → success");
    expect(container.textContent).toContain("Posted the summary.");
  });

  it("keeps a ledger's entries inside the block that wrote them", () => {
    // The executor's user message writes one `###` per prior turn inside its
    // conversation block. Hoisted to the top, those read as blocks of the
    // message in their own right, sitting between it and the ask.
    draw({
      system: "",
      user: [
        "## Earlier in this conversation",
        "Do not repeat a reply you already gave.",
        "",
        "### 2026-08-20T09:30",
        "You replied: shipped it",
        "",
        "## Task",
        "any update?",
      ].join("\n"),
    });
    // Only the two peers are on the outline; the turn is inside the first.
    expect(folds()).toHaveLength(2);
    expect(folds()[0]?.startsWith("Earlier in this conversation")).toBe(true);
    expect(folds()[1]?.startsWith("Task")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: /Earlier in this conversation/ }));
    expect(screen.getByRole("button", { name: /2026-08-20T09:30/ })).toBeTruthy();
  });

  it("sizes a section by everything under it, sub-sections included", () => {
    // A reader looking for where a heavy prompt's weight went is asking about
    // the BLOCK. A parent reporting only its own body reports its rules and
    // hides the eight turns beneath them.
    draw({ system: "## Ledger\nrules\n\n### Turn one\n" + "x".repeat(400) });
    expect(screen.getByRole("button", { name: /Ledger/ }).textContent).toContain("405");
  });

  it("names both halves of the record and folds each on its own headings", () => {
    draw({ user: "## Task\npost the summary\n\n## Already done earlier in this turn\n- one" });
    expect(screen.getByText("System")).toBeTruthy();
    expect(screen.getByText("User")).toBeTruthy();
    expect(folds().some((t) => t.startsWith("Task"))).toBe(true);
    expect(folds().some((t) => t.startsWith("Already done earlier in this turn"))).toBe(true);
  });
});

describe("a prompt with no headings", () => {
  it("is drawn as the one block it has always been", () => {
    const { container } = draw({ system: "a task prompt somebody wrote by hand" });
    expect(container.textContent).toContain("a task prompt somebody wrote by hand");
    // NO VIEW SWITCH. With nothing to split on, the two views are the same
    // picture under two names, and a control that changes nothing is worse
    // than no control.
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });
});
