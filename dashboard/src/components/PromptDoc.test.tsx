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
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PromptRecord } from "./PromptDoc.tsx";
import { outlineHalf } from "~/lib/promptmap.ts";
import type { PromptSection } from "~/protocol/index.ts";
import { sheetRules } from "~/test/sheets.ts";

// THE OUTLINE, WATCHED: a pass-through, so every case below reads the real
// outline, and the memo case can count how often a record was re-outlined.
vi.mock("~/lib/promptmap.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/promptmap.ts")>();
  return { ...real, outlineHalf: vi.fn(real.outlineHalf) };
});

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

  it("draws a title's link as its words inside a row, and as a link only in the reader", () => {
    // An option's children are presentational to assistive tech, and a click
    // on a link inside one navigates instead of choosing the row.
    draw({ system: "## See [the guide](https://example.com/guide)\nread it" });
    const row = within(outline()).getAllByRole("option")[0]!;
    expect(row.querySelector("a")).toBeNull();
    expect(row.textContent).toMatch(/^See the guide/);
    fireEvent.click(row);
    // THE CONTROL: the reader's own title is not a control, and keeps it.
    expect(within(reader()).getByRole("link", { name: "the guide" })).toBeTruthy();
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

  it("keeps a heading a headless span quotes first, nested under the builder's title", () => {
    // A worker's task the executor wrote, opening with its own "## Goal": as
    // the span's own heading, those words were drawn nowhere.
    const task = "## Goal\nDo X\n\n## Steps\n1. a\n\n";
    const rules = "## Worker rules\nstay a leaf";
    draw({
      system: "",
      user: task + rules,
      userSections: [
        { key: "task", title: "Task", bytes: bytes(task), headed: false },
        { key: "worker_rules", title: "Worker rules", bytes: bytes(rules), headed: true },
      ],
    });
    expect(screen.getByRole("region", { name: "Task" })).toBeTruthy();
    const subs = [...reader().querySelectorAll(".prompt-sub-title")].map((t) => t.textContent);
    expect(subs).toEqual(["Goal", "Steps"]);
    expect(reader().textContent).toContain("Do X");
  });

  it("is not re-outlined when a push brings the same map again", () => {
    // Every push rebuilds a record from the wire, so the map is a new array
    // on each of a streaming phase's five frames a second.
    const outlined = vi.mocked(outlineHalf);
    const fresh = () => MAP.map((s) => ({ ...s }));
    const view = render(
      <PromptRecord phase="review" system={TEXT} user="" systemSections={fresh()} />,
    );
    const before = outlined.mock.calls.length;
    view.rerender(<PromptRecord phase="review" system={TEXT} user="" systemSections={fresh()} />);
    expect(outlined.mock.calls.length).toBe(before);
    // THE CONTROL: a map that says something else is read again.
    const renamed = fresh().map((s, i) => (i === 0 ? { ...s, title: "Who you are" } : s));
    view.rerender(<PromptRecord phase="review" system={TEXT} user="" systemSections={renamed} />);
    expect(outlined.mock.calls.length).toBe(before + 1);
    expect(rows()[0]).toMatch(/^Who you are/);
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

  it("names that section for the whole half, since no heading follows it", () => {
    // "Before the first heading" claimed a heading the prompt does not have.
    draw({ system: "a **handwritten** task prompt" });
    expect(rows()[0]).toMatch(/^The whole system prompt/);
    expect(screen.getByRole("region", { name: "The whole system prompt" })).toBeTruthy();
  });

  it("names each half by its own shape", () => {
    // A headed system prompt keeps its lead "before the first heading"; the
    // plain user message beside it is the whole of that half.
    draw({ user: "plain trigger text" });
    const all = rows();
    expect(all[0]).toMatch(/^Before the first heading/);
    expect(all.at(-1)).toMatch(/^The whole user message/);
    fireEvent.click(within(outline()).getByRole("option", { name: /What the agent produced/ }));
    // Named once: the whole half needs no "(the user message)" after it.
    expect(screen.getByRole("button", { name: "Next: The whole user message" })).toBeTruthy();
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
    // And the rows that match lift, so the two sets differ by more than a
    // step of grey.
    const hit = within(outline())
      .getAllByRole("option")
      .filter((o) => o.classList.contains("is-hit"))
      .map((o) => o.textContent?.replace(/\s*\d.*$/, ""));
    expect(hit).toEqual(["Alpha", "Beta notes"]);
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

  const selected = () =>
    within(outline())
      .getByRole("option", { selected: true })
      .textContent?.replace(/\s*\d.*$/, "");

  it("takes the reader to a section that matches when the one shown has none", () => {
    draw({ system: "## Alpha\nalpha\n\n## Delta\ndelta", user: USER });
    find("gam");
    expect(selected()).toBe("Gamma");
    // A section that still matches as the query grows is kept.
    find("gamma");
    expect(selected()).toBe("Gamma");
    // THE CONTROL: a query the section on screen holds moves nothing.
    find("");
    choose(/^Delta/);
    find("delta");
    expect(selected()).toBe("Delta");
  });

  it("stops following once the reader chooses a section during the query", () => {
    draw({ system: "## Alpha\nalpha\n\n## Delta\ndelta", user: USER });
    find("e");
    choose(/^Alpha/);
    // Alpha holds no "eta" and "Beta notes" does, but the reader chose Alpha
    // while this query was open.
    find("eta");
    expect(selected()).toBe("Alpha");
    // Clearing ends the query, and the next one follows again.
    fireEvent.keyDown(screen.getByRole("searchbox", { name: "Find in this prompt" }), {
      key: "Escape",
    });
    find("nothing h");
    expect(selected()).toBe("Gamma");
  });
});

/**
 * The marks in the reader. jsdom has no CSS Custom Highlight API, so the
 * registry and `Highlight` are stood in for here, recording the ranges a
 * highlight was built from: without them this path never runs under test.
 */
describe("marking what a find matched", () => {
  class FakeHighlight {
    ranges: Range[];
    constructor(...ranges: Range[]) {
      this.ranges = ranges;
    }
  }
  let registry: Map<string, FakeHighlight>;
  const g = globalThis as unknown as { CSS?: object; Highlight?: unknown };
  const saved = { CSS: g.CSS, Highlight: g.Highlight };

  beforeEach(() => {
    registry = new Map();
    // A CSS object carrying everything the real one has, plus the registry.
    g.CSS = Object.assign(Object.create(saved.CSS ?? null) as object, { highlights: registry });
    g.Highlight = FakeHighlight;
  });
  afterEach(() => {
    // Unmount FIRST, while the stand-ins are still there to be cleared.
    cleanup();
    g.CSS = saved.CSS;
    g.Highlight = saved.Highlight;
  });

  const marked = () => (registry.get("prompt-find")?.ranges ?? []).map((r) => r.toString());
  const PROMPT = "## Alpha\nalpha beta beta\n\n## Delta\ndelta beta";
  function find(text: string, at = 0) {
    fireEvent.change(screen.getAllByRole("searchbox", { name: "Find in this prompt" })[at]!, {
      target: { value: text },
    });
  }

  it("marks every match in the section shown, and only there", () => {
    draw({ system: PROMPT });
    find("beta");
    expect(marked()).toEqual(["beta", "beta"]);
  });

  it("re-takes the marks when the reader moves to another section", () => {
    draw({ system: PROMPT });
    find("beta");
    choose(/^Delta/);
    expect(marked()).toEqual(["beta"]);
  });

  it("clears the marks when the query is cleared, and when the view goes", () => {
    draw({ system: PROMPT });
    find("beta");
    fireEvent.keyDown(screen.getByRole("searchbox", { name: "Find in this prompt" }), {
      key: "Escape",
    });
    expect(registry.has("prompt-find")).toBe(false);
    find("beta");
    expect(registry.has("prompt-find")).toBe(true);
    showSource();
    expect(registry.has("prompt-find")).toBe(false);
  });

  it("pools two open prompts into the one highlight, and drops one's when it goes", () => {
    const first = render(<PromptRecord phase="execute" system={PROMPT} user="" />);
    render(<PromptRecord phase="review" system={"## Other\nalpha"} user="" />);
    find("beta", 0);
    find("alpha", 1);
    expect(marked()).toEqual(["beta", "beta", "alpha"]);
    first.unmount();
    expect(marked()).toEqual(["alpha"]);
  });

  it("marks a title that is the section's own heading line, as the count does", () => {
    draw({ system: "## Beta notes\nbeta once\n\n## Gamma\nnothing here" });
    find("notes");
    expect(screen.getByRole("status").textContent).toBe("1 match in 1 section");
    expect(marked()).toEqual(["notes"]);
    find("beta");
    expect(marked()).toEqual(["Beta", "beta"]);
  });

  it("never marks words the source does not hold — a builder's title, the reader's own", () => {
    const text = "You are **Engineer**.\n\n";
    draw({
      system: text,
      systemSections: [{ key: "identity", title: "Identity", bytes: bytes(text) }],
    });
    find("identity");
    expect(screen.getByRole("status").textContent).toBe("No matches");
    expect(marked()).toEqual([]);
    // Nor the size and share, nor where Previous and Next go.
    find("system prompt");
    expect(marked()).toEqual([]);
  });
});

describe("taking the reader to a match", () => {
  const LONG = "## Alpha\nfirst line\n\nalpha beta\n\n## Delta\ndelta\n\nthen beta here";

  it("brings the first match of the section Enter lands on into view", () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView");
    try {
      draw({ system: LONG });
      const box = screen.getByRole("searchbox", { name: "Find in this prompt" });
      fireEvent.change(box, { target: { value: "beta" } });
      // Typing marks; it does not move the page under the box being typed in.
      expect(scrolled).not.toHaveBeenCalled();
      fireEvent.keyDown(box, { key: "Enter" });
      expect(screen.getByRole("region", { name: "Delta" })).toBeTruthy();
      expect(scrolled).toHaveBeenCalledTimes(1);
      expect(scrolled.mock.instances[0]).toBeInstanceOf(Element);
      expect((scrolled.mock.instances[0] as unknown as Element).textContent).toContain(
        "then beta here",
      );
      expect(scrolled.mock.calls[0]?.[0]).toEqual({ block: "nearest" });
      // Enter on the only remaining match brings it back into view again.
      fireEvent.change(box, { target: { value: "then beta" } });
      fireEvent.keyDown(box, { key: "Enter" });
      expect(scrolled).toHaveBeenCalledTimes(2);
    } finally {
      scrolled.mockRestore();
    }
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

  it("never draws a half narrower than its segments, which would spill past the map", () => {
    // jsdom lays nothing out, so this is read in the sheet: a half's byte
    // share of a phone's width can be 4px while its segments need 11, and a
    // half allowed to shrink to nothing (`min-width: 0`) lets them overflow.
    const rule = (selector: string) =>
      sheetRules()
        .filter((r) => r.file === "screens.css" && r.selector === selector)
        .map((r) => r.body)
        .join(";");
    expect(rule(".prompt-map-seg")).toMatch(/min-width:\s*\d/);
    expect(rule(".prompt-map-half")).toMatch(/flex-basis:\s*0/);
    expect(rule(".prompt-map-half")).not.toMatch(/min-width/);
  });
});
