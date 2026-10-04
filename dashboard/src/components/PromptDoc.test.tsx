/**
 * THE OUTLINE IS DERIVED, AND THE RECORD IS STILL REACHABLE.
 *
 * Two claims, and the screen is worth nothing without either. A prompt's
 * sections are whatever headings `internal/agent/prompts` wrote, so this file
 * may not name one — a case asserting "Review has a self_iterate section"
 * would pass on a component that hardcoded the list and go on passing after
 * the engine renamed it. And the reading view is a RENDERING, so the bytes
 * have to stay one control away: this surface is where an operator reproduces
 * a turn from, and a view that decoded the markdown and offered nothing else
 * would have traded one half of the job for the other.
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

/** Switch to the byte-for-byte view. */
function showSource() {
  fireEvent.click(screen.getByRole("radio", { name: "Source" }));
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
    expect(screen.getByText(/You are/)).toBeTruthy();
  });

  it("renders that run's markdown rather than printing it", () => {
    // The seat's identity is `**Engineer** at **Acme**`, and a reader decoding
    // asterisks the model was handed already decoded is the whole complaint.
    draw();
    expect(screen.getByText("Engineer").closest("strong")).toBeTruthy();
    expect(screen.getByText("Acme").closest("strong")).toBeTruthy();
  });

  it("keeps a closed section's body out of the card entirely", () => {
    // `lazy`, which here is the behaviour rather than an optimisation: an
    // outline whose every body is mounted is the wall of text it replaces,
    // and a closed fold must not put its text into the card.
    const { container } = draw();
    expect(container.textContent).not.toContain("post_message");
  });

  it("renders a section's markdown once it is opened", () => {
    draw();
    fireEvent.click(screen.getByRole("button", { name: /What the agent did/ }));
    // A LIST ITEM, not a line starting with a hyphen.
    const item = screen.getByText("post_message(...) → success");
    expect(item.closest("li")).toBeTruthy();
  });

  it("offers the whole document as one block, byte for byte", () => {
    // The half the rendering owes back: an operator reproducing a turn needs
    // what was sent, markers and all, in one selection rather than a dozen
    // opens and a dozen select-alls.
    const { container } = draw();
    showSource();
    expect(container.textContent).toContain("You are **Engineer** at **Acme** (reports to: Lead).");
    expect(container.textContent).toContain("## What the agent did");
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
    // hides the eight turns beneath them. Counted on the SOURCE, which is the
    // share of the prompt the model was billed for — not on what was drawn.
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

  it("keeps a fenced block's bytes in the reading view", () => {
    // WHAT MAKES RENDERING SAFE HERE. A tool schema, a JSON example and a
    // contract template travel in fences, and a fence renders to its own
    // block holding the exact text — so the record-sensitive half of a prompt
    // is byte-identical in both views and only the formatting differs.
    draw({ system: '## Contract\n```json\n{"outcome": "**not** bold"}\n```' });
    fireEvent.click(screen.getByRole("button", { name: /Contract/ }));
    expect(screen.getByText('{"outcome": "**not** bold"}')).toBeTruthy();
  });
});

describe("a prompt with no headings", () => {
  it("is rendered as the one document it is", () => {
    // NO OUTLINE TO DRAW, and still markdown: the reading view is one run
    // rather than a fold that would have nothing to be called.
    draw({ system: "a **handwritten** task prompt" });
    expect(screen.getByText("handwritten").closest("strong")).toBeTruthy();
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });

  it("still offers the record, because the two views differ on every document", () => {
    // The switch used to be gated on having headings, on the rule that
    // without them the two views were the same picture under two names. They
    // are not: one decodes the markdown and one is the bytes.
    const { container } = draw({ system: "a **handwritten** task prompt" });
    showSource();
    expect(container.textContent).toContain("a **handwritten** task prompt");
  });
});
