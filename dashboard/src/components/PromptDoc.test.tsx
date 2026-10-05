/**
 * THE OUTLINE IS DERIVED, ONE SECTION IS READ, AND THE RECORD IS STILL
 * REACHABLE.
 *
 * Three claims, and the screen is worth nothing without any of them. A
 * prompt's sections are whatever its builder marked out — its section map, or
 * failing that its headings — so this file may not name one: a case asserting
 * "Review has a self_iterate section" would pass on a component that
 * hardcoded the list and go on passing after the engine renamed it. Only the
 * section being read is mounted, so a 30 kB prompt is never a 30 kB page. And
 * the reading view is a RENDERING, so the bytes stay one control away: this
 * surface is where an operator reproduces a turn from.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PromptRecord } from "./PromptDoc.tsx";
import type { PromptSection } from "~/protocol/index.ts";

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

const bytes = (s: string) => new TextEncoder().encode(s).length;

function draw(
  over: {
    phase?: string;
    system?: string;
    user?: string;
    systemSections?: PromptSection[] | null;
    userSections?: PromptSection[] | null;
  } = {},
) {
  return render(
    <PromptRecord
      phase={over.phase ?? "review"}
      system={over.system ?? SYSTEM}
      user={over.user ?? ""}
      systemSections={over.systemSections ?? null}
      userSections={over.userSections ?? null}
    />,
  );
}

/** The outline: the one listbox the reading view draws. */
const outline = () => screen.getByRole("listbox", { name: /Sections of/ });

/** Every row of the outline, by its words. */
function rows(): string[] {
  return within(outline())
    .getAllByRole("option")
    .map((o) => o.textContent ?? "");
}

/** Choose a section by the words its row starts with. */
function choose(title: RegExp) {
  fireEvent.click(within(outline()).getByRole("option", { name: title }));
}

/** The section being read. */
const reader = () => screen.getByRole("region");

/** Switch to the byte-for-byte view. */
function showSource() {
  fireEvent.click(screen.getByRole("radio", { name: "Source" }));
}

describe("a prompt with headings", () => {
  it("lists one row per section, named by the heading", () => {
    draw();
    const all = rows();
    expect(all).toHaveLength(4);
    expect(all[0]).toMatch(/^Before the first heading/);
    expect(all[1]).toMatch(/^REVIEW phase/);
    expect(all[2]).toMatch(/^What the agent did/);
    expect(all[3]).toMatch(/^What the agent produced/);
  });

  it("opens on the run before the first heading, without asking for a click", () => {
    // It is the first thing the model read, and nothing may hide it.
    draw();
    expect(within(reader()).getByText(/You are/)).toBeTruthy();
  });

  it("renders that run's markdown rather than printing it", () => {
    draw();
    expect(screen.getByText("Engineer").closest("strong")).toBeTruthy();
    expect(screen.getByText("Acme").closest("strong")).toBeTruthy();
  });

  it("mounts only the section being read", () => {
    // A section not chosen must not put its text into the card: the outline
    // is the point, and every body mounted at once is the wall of text again.
    const { container } = draw();
    expect(container.textContent).not.toContain("post_message");
    expect(container.textContent).not.toContain("Posted the summary.");
  });

  it("reads a chosen section, as the markdown it is", () => {
    draw();
    choose(/^What the agent did/);
    // A LIST ITEM, not a line starting with a hyphen.
    expect(screen.getByText("post_message(...) → success").closest("li")).toBeTruthy();
    expect(within(reader()).queryByText(/You are/)).toBeNull();
  });

  it("keeps a prompt's headings out of the page's outline", () => {
    // A turn with six phases would otherwise put eighty headings from six
    // quoted documents into one screen.
    const { container } = draw({ user: "## Ledger\nrules\n\n### Turn one\nsaid it" });
    choose(/^Ledger/);
    expect(container.querySelectorAll("h1, h2, h3, h4, h5, h6")).toHaveLength(0);
    expect(reader().getAttribute("aria-labelledby")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Ledger" })).toBeTruthy();
  });

  it("nests a ledger's entries inside the block that wrote them", () => {
    // The executor's user message writes one `###` per prior turn inside its
    // conversation block. Hoisted to the outline, those read as blocks of the
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
    expect(rows().map((r) => r.split(/\d/)[0])).toEqual(["Earlier in this conversation", "Task"]);
    const sub = within(reader()).getByText("2026-08-20T09:30");
    expect(sub.closest(".prompt-sub")?.textContent).toContain("You replied: shipped it");
  });

  it("sizes a section in UTF-8 bytes, its heading line and everything under it included", () => {
    // A reader looking for where a heavy prompt's weight went is asking about
    // the BLOCK, in the unit the Context tab counts the prompt in — and the
    // sizes of a half's sections add up to the half.
    const ledger = "## Ledger ☕\nrules\n\n### Turn one\n" + "é".repeat(200) + "\n\n";
    const task = "## Task\nask";
    draw({ system: ledger + task });
    choose(/^Ledger/);
    const caption = reader().textContent ?? "";
    expect(caption).toContain(`${bytes(ledger)} B`);
    expect(bytes(ledger) + bytes(task)).toBe(bytes(ledger + task));
    // The share is of the half, and says which.
    expect(caption).toMatch(/\d+% of the system prompt/);
  });

  it("names both halves of the request and outlines each on its own", () => {
    draw({ user: "## Task\npost the summary\n\n## Already done earlier in this turn\n- one" });
    expect(screen.getByRole("group", { name: /^System prompt/ })).toBeTruthy();
    expect(screen.getByRole("group", { name: /^User message/ })).toBeTruthy();
    expect(rows().some((t) => t.startsWith("Task"))).toBe(true);
    expect(rows().some((t) => t.startsWith("Already done earlier in this turn"))).toBe(true);
  });

  it("keeps a fenced block's bytes in the reading view", () => {
    // WHAT MAKES RENDERING SAFE HERE: a tool schema, a JSON example and a
    // contract template travel in fences, and a fence renders to its own
    // block holding the exact text.
    draw({ system: '## Contract\n```json\n{"outcome": "**not** bold"}\n```' });
    choose(/^Contract/);
    expect(screen.getByText('{"outcome": "**not** bold"}').closest("pre")).toBeTruthy();
  });

  it("renders a title's inline markdown rather than printing its backticks", () => {
    draw({ system: "## Sandbox (the `run_sandbox` tool)\nuse it" });
    const row = within(outline()).getByRole("option", { name: /Sandbox/ });
    expect(row.querySelector("code")?.textContent).toBe("run_sandbox");
    expect(row.textContent).not.toContain("`");
  });

  it("keeps the prompt's own line breaks", () => {
    // An identity block is one fact per line; read as soft wraps it became a
    // single run-on sentence.
    draw({ system: "Name: Engineer\nTeam: Platform\nReports to: Lead" });
    expect(reader().querySelectorAll("br")).toHaveLength(2);
  });

  it("offers the whole document as one block, byte for byte", () => {
    // The half the rendering owes back: an operator reproducing a turn needs
    // what was sent, markers and all, in one selection.
    const { container } = draw({ user: "## Task\r\ndo it" });
    showSource();
    expect(screen.getByRole("region", { name: /system prompt/ }).textContent).toContain(
      "You are **Engineer** at **Acme** (reports to: Lead).",
    );
    expect(container.textContent).toContain("## What the agent did");
    expect(container.textContent).toContain("- post_message(...) → success");
    expect(screen.getByRole("region", { name: /user message/ }).textContent).toBe(
      "## Task\r\ndo it",
    );
  });
});

describe("a prompt whose builder sent a section map", () => {
  const LEAD = "You are **Engineer**.\n\n";
  // A chat trigger's own headings, quoted inside the builder's span: the
  // shape that escaped its section when the outline was cut at every `##`.
  const TURN =
    "## Your turn\nThe message:\n\n## Triage\nfrom the thread\n\n# A pull request title\nbody\n\n";
  const TAIL = "## Contract\nsubmit it";
  const TEXT = LEAD + TURN + TAIL;
  const MAP: PromptSection[] = [
    { key: "identity", title: "Identity", bytes: bytes(LEAD) },
    { key: "turn", title: "Your turn", bytes: bytes(TURN) },
    { key: "contract", title: "Contract", bytes: bytes(TAIL) },
  ];

  it("outlines by the map, with the quoted headings nested inside their span", () => {
    draw({ system: TEXT, systemSections: MAP });
    expect(rows().map((r) => r.replace(/\s*\d.*$/, ""))).toEqual([
      "Identity",
      "Your turn",
      "Contract",
    ]);
    choose(/^Your turn/);
    const subs = [...reader().querySelectorAll(".prompt-sub-title")].map((t) => t.textContent);
    expect(subs).toEqual(["Triage", "A pull request title"]);
    expect(reader().textContent).toContain("from the thread");
  });

  it("falls back to the headings when the map does not tile the prompt", () => {
    // One byte short: a map that would mis-slice is not trusted at all.
    const broken = MAP.map((s, i) => (i === 2 ? { ...s, bytes: s.bytes - 1 } : s));
    draw({ system: TEXT, systemSections: broken });
    const all = rows();
    expect(all.some((r) => r.startsWith("Triage"))).toBe(true);
    expect(all.some((r) => r.startsWith("Identity"))).toBe(false);
  });
});

describe("a prompt with no headings", () => {
  it("is one section, rendered as the one document it is", () => {
    draw({ system: "a **handwritten** task prompt" });
    expect(rows()).toHaveLength(1);
    expect(screen.getByText("handwritten").closest("strong")).toBeTruthy();
  });

  it("still offers the record, because the two views differ on every document", () => {
    const { container } = draw({ system: "a **handwritten** task prompt" });
    showSource();
    expect(container.textContent).toContain("a **handwritten** task prompt");
  });
});

describe("finding in a prompt", () => {
  const USER = "## Beta notes\nbeta once\n\n## Gamma\nnothing here";

  function find(text: string) {
    fireEvent.change(screen.getByRole("searchbox", { name: "Find in this prompt" }), {
      target: { value: text },
    });
  }

  it("counts the matches in every section and says how many, without filtering", () => {
    draw({ system: "## Alpha\nalpha BETA\n\n## Delta\ndelta", user: USER });
    find("beta");
    // The source counts: one in Alpha, two in "Beta notes" (its heading too).
    expect(screen.getByRole("status").textContent).toBe("3 matches in 2 sections");
    expect(rows()).toHaveLength(4);
    const dim = within(outline())
      .getAllByRole("option")
      .filter((o) => o.classList.contains("is-dim"))
      .map((o) => o.textContent?.replace(/\s*\d.*$/, ""));
    expect(dim).toEqual(["Delta", "Gamma"]);
  });

  it("jumps between the sections that match with Enter and Shift+Enter, and clears with Escape", () => {
    draw({ system: "## Alpha\nalpha BETA\n\n## Delta\ndelta", user: USER });
    const box = screen.getByRole("searchbox", { name: "Find in this prompt" });
    find("beta");
    const selected = () =>
      within(outline())
        .getByRole("option", { selected: true })
        .textContent?.replace(/\s*\d.*$/, "");
    expect(selected()).toBe("Alpha");
    fireEvent.keyDown(box, { key: "Enter" });
    expect(selected()).toBe("Beta notes");
    fireEvent.keyDown(box, { key: "Enter" });
    expect(selected()).toBe("Alpha");
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });
    expect(selected()).toBe("Beta notes");
    fireEvent.keyDown(box, { key: "Escape" });
    expect((box as HTMLInputElement).value).toBe("");
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("says when nothing matches", () => {
    draw();
    find("zzz");
    expect(screen.getByRole("status").textContent).toBe("No matches");
  });
});

describe("moving through the outline", () => {
  it("is one tab stop, and the arrows, Home and End move the selection with focus", () => {
    draw({ user: "## Task\nask" });
    const options = within(outline()).getAllByRole("option");
    expect(options.filter((o) => o.tabIndex === 0)).toHaveLength(1);
    options[0]!.focus();
    fireEvent.keyDown(options[0]!, { key: "ArrowDown" });
    expect(document.activeElement).toBe(options[1]);
    expect(options[1]!.getAttribute("aria-selected")).toBe("true");
    // ACROSS THE HALVES: End is the user message's last section.
    fireEvent.keyDown(options[1]!, { key: "End" });
    expect(document.activeElement?.textContent).toMatch(/^Task/);
    fireEvent.keyDown(document.activeElement!, { key: "Home" });
    expect(document.activeElement).toBe(options[0]);
    fireEvent.keyDown(options[0]!, { key: "ArrowUp" });
    expect(document.activeElement).toBe(options[0]);
  });

  it("names where Previous and Next go, and keeps them at the ends", () => {
    draw({ user: "## Task\nask" });
    expect(screen.getByRole("button", { name: /^Next: REVIEW phase/ })).toBeTruthy();
    const previous = screen.getByRole("button", { name: /^Previous/ });
    expect(previous.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(within(outline()).getByRole("option", { name: /What the agent produced/ }));
    // Into the other half, which the label says.
    fireEvent.click(screen.getByRole("button", { name: /^Next: Task \(the user message\)/ }));
    expect(screen.getByRole("region", { name: "Task" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Next/ }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("draws the request as a map nobody has to read, sections in proportion", () => {
    const { container } = draw({ user: "## Task\nask" });
    const map = container.querySelector(".prompt-map")!;
    // A PICTURE of the outline: the outline is the way to it in words.
    expect(map.getAttribute("aria-hidden")).toBe("true");
    const segments = [...map.querySelectorAll<HTMLElement>(".prompt-map-seg")];
    expect(segments).toHaveLength(5);
    // Each as wide as its bytes, and a half's segments are the half.
    const lead = "You are **Engineer** at **Acme** (reports to: Lead).\n\n";
    expect(segments[0]!.style.flexGrow).toBe(String(bytes(lead)));
    const grow = (els: HTMLElement[]) => els.reduce((n, s) => n + Number(s.style.flexGrow), 0);
    expect(grow(segments.slice(0, 4))).toBe(bytes(SYSTEM));
    expect(segments[0]!.classList.contains("is-selected")).toBe(true);
    fireEvent.click(segments[4]!);
    expect(screen.getByRole("region", { name: "Task" })).toBeTruthy();
    expect(segments[4]!.getAttribute("title")).toMatch(/^Task · \d+ B · 100% of the user message$/);
  });
});
