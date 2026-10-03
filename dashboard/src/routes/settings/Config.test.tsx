/**
 * The Config screen renders what the ENGINE sends, not what a type says.
 *
 * Both fixtures below are copied from the emitters: `configapi.meta` for a
 * revision (`revision_id`, `created_by`, `is_active`) and `configapi.Change`
 * for a diff line (`kind`, one of added / removed / changed). The screen read
 * `id`, `author`, `active` and `op`, none of which any answer has ever
 * carried, so every revision row rendered `undefined` and threw on the first
 * `.slice(10)`, and every diff line rendered unstyled.
 *
 * The absence of this file is why that shipped. A shape declared from memory
 * and never rendered against a real answer is a screen nobody has seen.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { changeValue, ConfigScreen, RevisionAuthor, RevisionPeek } from "./Config.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

// EXACTLY what internal/api/configapi/answers.go returns for these two
// questions. Change the server and this fixture has to change with it, which
// is the whole point of writing it out rather than building it from the type.
const revisions = [
  {
    revision_id: "01JCFGAAAA0000000000000001",
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    created_by_kind: "operator",
    source: "api",
    summary: "connect datadog",
    is_active: true,
  },
  {
    revision_id: "01JCFGBBBB0000000000000002",
    created_at: "2026-08-22T15:00:00Z",
    created_by: "node-a",
    created_by_kind: "node",
    source: "file",
    summary: "first import",
    is_active: false,
  },
];

const diff = {
  from: "01JCFGBBBB0000000000000002",
  to: "01JCFGAAAA0000000000000001",
  changes: [
    { path: "integrations.datadog.route_to", kind: "added", to: "sre-lead" },
    { path: "integrations.gitlab.url", kind: "changed", from: "a", to: "b" },
    { path: "integrations.slack", kind: "removed", from: {} },
  ],
  changes_total: 3,
};

// The same answer for a comparison too long to send whole: the listing is a
// page of it and `changes_total` is how many there are. The server used to
// report the cut as a pathless CHANGE inside `changes`, which this screen
// drew as a blank path turning undefined into a sentence.
const cutDiff = {
  ...diff,
  changes_total: 512,
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// The engine's answer for a collection when nothing is active: nothing at all.
// `queries/config.go` returns nil for ErrNoActiveRevision and the envelope
// omits an absent `data`, so the client settles on `undefined` — which is why
// it is the default here rather than a case's own argument: a default of
// `undefined` cannot be passed back in, and a helper that quietly substituted
// the empty collection for it would test the opposite of what it said.
/** Every question the screen asked, with its parameters. */
let asked: { what: string; params: Record<string, unknown> | undefined }[] = [];

function mount(hash: string, answer: unknown = diff, entities?: unknown) {
  location.hash = hash;
  const store = new Store();
  const socket = new LiveSocket(store);
  // The screen's only data path. Stubbed rather than driven through a fake
  // server, because what is under test is the rendering of an answer whose
  // shape is already pinned by the Go side.
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    asked.push({ what, params });
    return Promise.resolve(
      what === "config_audit"
        ? revisions
        : what === "config_diff"
          ? answer
          : what === "config_entities"
            ? typeof entities === "function"
              ? (entities as (p?: Record<string, unknown>) => unknown)(params)
              : entities
            : {},
    );
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ConfigScreen />
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  asked = [];
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("a revision row renders the server's own field names", async () => {
  mount("#/config?lens=audit");

  // The id: truncated to ten characters, which is what threw when the field
  // was read under a name the server does not send.
  expect(await screen.findByText("01JCFGAAAA")).toBeDefined();
  expect(screen.getByText("01JCFGBBBB")).toBeDefined();
  expect(screen.getByText("connect datadog")).toBeDefined();
  // The author, WHAT the author is, and the active marker. The kind is the
  // revision's own word: a node's seed reads as the node's, never as a
  // person's.
  expect(screen.getByText("founder")).toBeDefined();
  expect(screen.getByText("node-a")).toBeDefined();
  expect(screen.getByText("operator")).toBeDefined();
  expect(screen.getByText("node")).toBeDefined();
  expect(screen.getAllByText("active").length).toBe(1);
});

test("a diff line carries the kind the server sends", async () => {
  const { container } = mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  // data-kind is what the stylesheet selects on. Under the old `data-op` with
  // add / remove / replace, every line rendered with no tone at all.
  const kinds = [...container.querySelectorAll(".config-diff-line")].map((el) =>
    el.getAttribute("data-kind"),
  );
  expect(kinds).toEqual(["added", "changed", "removed"]);
  // AND IN WORDS: the mark is a glyph and a tint, neither of which a reader
  // who hears the list receives.
  expect(
    [...container.querySelectorAll(".config-diff-mark .sr-only")].map((el) => el.textContent),
  ).toEqual(["added", "changed", "removed"]);
  // And the value column reads the side that exists for that kind.
  expect(screen.getByText('"sre-lead"')).toBeDefined();
  expect(screen.getByText('"a" → "b"')).toBeDefined();
  // NOTHING SAYS IT WAS CUT, because it was not: changes_total equals the
  // number of lines, and a "3 of 3 shown" note would be noise on every diff.
  expect(screen.queryByText(/shown/)).toBeNull();
});

// A DIFF IS DRAWN IN ITS OWN TRACKS, and nothing in it is cut. The config
// list spelled `.diff-line`, which the knowledge page's line diff also spells
// later in the sheet, so the cascade handed every change the page diff's
// line-number tracks (`3.5ch 3.5ch 2ch`): at 1440 the path got 23px and the
// value 13px, and every change read "− mc… {…". jsdom lays nothing out, so
// the cascade over the rendered lines is what is read — with every sheet the
// dashboard loads, in the order it loads them.
test("a diff's path and value are drawn whole, in the diff's own tracks", async () => {
  const read = (name: string) => readFileSync(join(process.cwd(), "src/styles", name), "utf8");
  const style = document.createElement("style");
  style.textContent = ["tokens.css", "base.css", "components.css", "frame.css", "screens.css"]
    .map(read)
    .join("\n");
  document.head.append(style);
  try {
    const { container } = mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001", {
      ...diff,
      changes: [
        {
          path: "mcp_servers[1]",
          kind: "removed",
          from: { name: "docs", command: "uvx", args: ["mcp-server-docs", "--port", "0"] },
        },
      ],
      changes_total: 1,
    });
    const path = await screen.findByText("mcp_servers[1]");
    const list = container.querySelector<HTMLElement>(".config-diff")!;
    const line = path.closest<HTMLElement>(".config-diff-line")!;
    expect(getComputedStyle(list).gridTemplateColumns).toBe(
      "calc(2ch + var(--spacing-2)) fit-content(40%) minmax(0, 1fr)",
    );
    // EVERY ROW IS THE LIST'S OWN TRACKS, so the values start at one edge.
    expect(getComputedStyle(line).gridTemplateColumns).toBe("subgrid");
    const value = line.querySelector<HTMLElement>(".config-diff-value")!;
    for (const cell of [path, value]) {
      expect(cell.classList.contains("truncate"), cell.className).toBe(false);
      expect(getComputedStyle(cell).textOverflow).not.toBe("ellipsis");
      expect(getComputedStyle(cell).overflowWrap).toBe("anywhere");
    }
    // THE VALUE KEEPS THE DOCUMENT'S SHAPE: an object indented, line by line.
    expect(getComputedStyle(value).whiteSpace).toBe("pre-wrap");
    expect(value.textContent).toBe(
      JSON.stringify(
        { name: "docs", command: "uvx", args: ["mcp-server-docs", "--port", "0"] },
        null,
        2,
      ),
    );
  } finally {
    style.remove();
  }
});

describe("a change's value", () => {
  test("is JSON, so a string and the setting it spells stay two things", () => {
    expect(changeValue({ path: "a", kind: "added", to: "true" })).toBe('"true"');
    expect(changeValue({ path: "a", kind: "added", to: true })).toBe("true");
    expect(changeValue({ path: "a", kind: "changed", from: 1, to: 2 })).toBe("1 → 2");
  });

  // `omitempty` on an `any` drops a JSON null, so a setting changed from null
  // arrives with no `from` — which stringifies to `undefined`.
  test("reads an absent side as the null it was", () => {
    expect(changeValue({ path: "a", kind: "changed", to: "x" })).toBe('null → "x"');
  });

  test("puts the arrow at the head of its own line after an object", () => {
    expect(changeValue({ path: "a", kind: "changed", from: { n: 1 }, to: "x" })).toBe(
      '{\n  "n": 1\n}\n→ "x"',
    );
  });
});

// THE LENS'S SUBJECT COMES FIRST. Under the history, the changes began below
// fifteen revisions — off the screen at 1440 × 900 — so "See its changes"
// landed on a list and showed no change at all.
test("the changes sit above the revisions they are picked from", async () => {
  const { container } = mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");
  const path = await screen.findByText("integrations.datadog.route_to");
  const table = await screen.findByText("first import");
  expect(
    path.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING,
    "the changes precede the table",
  ).toBeTruthy();
  expect(container.querySelector("#config-changes")).not.toBeNull();
});

// A ROW PRESSED BELOW THE CHANGES BRINGS THEM BACK: a revision picked forty
// rows down changed a card the reader could not see. Only when it is off
// screen, and only on the Diff lens — the History lens has no changes to show.
describe("a revision pressed on the Diff lens", () => {
  let scrolled: Element[] = [];
  let headTop = -400;
  beforeEach(() => {
    scrolled = [];
    headTop = -400;
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      writable: true,
      value(this: Element) {
        scrolled.push(this);
      },
    });
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const top = this.id === "config-changes" ? headTop : 0;
      return { top, bottom: top + 40, left: 0, right: 0, width: 0, height: 40 } as DOMRect;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
  });

  test("brings the changes on screen when they are off it", async () => {
    mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");
    await screen.findByText("first import");
    fireEvent.click(screen.getByRole("link", { name: /first import/ }));
    expect(scrolled.map((el) => el.id)).toEqual(["config-changes"]);
  });

  test("leaves the page where it is when they are already on screen", async () => {
    headTop = 100;
    mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");
    await screen.findByText("first import");
    fireEvent.click(screen.getByRole("link", { name: /first import/ }));
    expect(scrolled).toEqual([]);
  });

  test("moves nothing on the History lens", async () => {
    mount("#/config?lens=audit");
    await screen.findByText("first import");
    fireEvent.click(screen.getByRole("link", { name: /first import/ }));
    expect(scrolled).toEqual([]);
  });
});

test("a cut diff says how many changes there are", async () => {
  mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001", cutDiff);

  // The count is the COMPARISON's, not the listing's — a screen that showed
  // three would be reporting the response budget as the answer.
  expect(await screen.findByText(/3 of 512 shown/)).toBeDefined();
  // And where the whole thing is: the CLI writes to a terminal, which has no
  // response budget, so it prints every change.
  expect(screen.getByText("crewlet config diff")).toBeDefined();
});

// NO ACTIVE REVISION IS NOT AN EMPTY COLLECTION, and the engine says so by
// answering nothing at all: `queries/config.go` returns nil for
// ErrNoActiveRevision and the envelope omits the field, so this arrives as
// `undefined` with no error. Read as an empty list it produced the one
// sentence that cannot be true on a deployment before its first import —
// "the active revision declares none of these" — while the Active lens on the
// same screen answered the same state correctly.
test("a collection with no active revision says so, not that the revision declares none", async () => {
  mount("#/config?lens=entities", diff, undefined);

  expect(await screen.findByText("No company configuration is active")).toBeDefined();
  expect(screen.queryByText(/declares none of these/)).toBeNull();
});

// THE CONTROL. An active revision that genuinely declares no seats is a
// different fact and keeps its own wording — without this, collapsing both
// into the no-revision sentence would pass the case above.
test("an empty collection under an active revision still reads as empty", async () => {
  mount("#/config?lens=entities", diff, { kind: "roles", ids: [] });

  expect(await screen.findByText("Nothing in this collection")).toBeDefined();
  expect(screen.queryByText("No company configuration is active")).toBeNull();
});

// `entity` is a URL key, so a shared or bookmarked link lands on the panel in
// that same state — where `JSON.stringify(undefined ?? null)` printed the
// literal word `null` as though it were the entity's own slice of the
// document.
test("a deep link to an entity with no active revision renders no literal null", async () => {
  mount("#/config?lens=entities&entity=founder", diff, undefined);

  expect(await screen.findAllByText("No company configuration is active")).toBeDefined();
  expect(screen.queryByText("null")).toBeNull();
});

// ---------------------------------------------------------------------------
// Which side a diff is read against
// ---------------------------------------------------------------------------

/** The side each diff the screen asked for was compared against. */
const againstAsked = () =>
  asked.filter((q) => q.what === "config_diff").map((q) => q.params?.against);

// WHAT ONE SAVE CHANGED is its revision against its PARENT. Against the active
// revision, a save that is active now is byte-identical to itself, so a "View
// changes" link that named only the revision opened an empty diff under a note
// about credential rotation.
test("a link naming the side to compare against reads the diff against it", async () => {
  mount(
    "#/settings/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  expect(againstAsked()).toEqual(["01JCFGBBBB0000000000000002"]);
  expect(screen.getByText("against revision 01JCFGBBBB")).toBeDefined();
});

// THE CONTROL, and the half that keeps the case above from passing on a screen
// that simply forwards whatever the URL holds: a row picked from the history
// is a question about the ACTIVE document again, so it drops the side a link
// named rather than carrying somebody else's comparison into it.
test("a revision picked from the history is compared with the active one again", async () => {
  mount(
    "#/settings/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );

  // THE ROW'S OWN LINK, which is what a reader presses: the grid draws a real
  // anchor over each row so a revision can be opened in a tab, and a plain
  // left click on it peeks and points the diff.
  await screen.findByText("first import");
  fireEvent.click(screen.getByRole("link", { name: /first import/ }));
  await waitFor(() => expect(location.hash).not.toContain("against="));
  expect(location.hash).toContain("revision=01JCFGBBBB0000000000000002");
  await waitFor(() => expect(againstAsked().at(-1)).toBe("active"));
  expect(screen.getByText("against the active revision")).toBeDefined();
});

// AND THE ORDINARY CASE NAMES NO SIDE, so every link that wants the plain
// comparison carries no parameter at all.
test("with no side named the diff is read against the active revision", async () => {
  mount("#/settings/config?lens=diff&revision=01JCFGAAAA0000000000000001");

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  expect(againstAsked()).toEqual(["active"]);
  expect(screen.getByText("against the active revision")).toBeDefined();
});

// A REVISION NOBODY RECORDED AN AUTHOR FOR SAYS SO. It was adopted from an
// older engine's pointer, which named nobody — and "not recorded" is a
// different fact from "the engine wrote it" or from an empty name.
test("a revision with no recorded author says so rather than naming anybody", () => {
  const { container } = render(
    <RevisionAuthor
      revision={{
        revision_id: "r",
        summary: "",
        source: "fleet",
        created_by: "",
        created_by_kind: "",
        created_at: "2026-08-22T15:00:00Z",
      }}
    />,
  );
  expect(container.textContent).toContain("Not recorded");
  expect(container.querySelector(".crewlet-tag")).toBeNull();
});

// THE LIST SAYS WHICH ONE IS OPEN, to a reader who cannot see the fill: the
// picked handle is a pressed toggle and every other is not, and each carries
// its whole handle in its title because the list track is fixed and cuts one
// longer than it.
test("the picked entity is the one pressed toggle in the list", async () => {
  mount("#/config?lens=entities&entity=agent-ceo", diff, (params?: Record<string, unknown>) =>
    params?.id
      ? { kind: "roles", id: params.id, entity: { handle: params.id } }
      : { kind: "roles", ids: ["agent-ai-systems-engineer", "agent-ceo"] },
  );

  const picked = (await screen.findByRole("button", { pressed: true })).closest("button");
  const other = screen.getByText("agent-ai-systems-engineer").closest("button");
  expect(picked?.textContent).toBe("agent-ceo");
  expect(other?.getAttribute("aria-pressed")).toBe("false");
  expect(other?.getAttribute("title")).toBe("agent-ai-systems-engineer");
  expect(picked?.closest(".entity-pick-list")).not.toBeNull();
});

// A PICKED ROW IS A SELECTION, NOT THE PAGE'S PRIMARY ACTION. Drawn as the
// kit's primary button it filled solid violet under the pointer with a code
// chip inside that kept its own grey ink — 1.5:1 in the light theme, 2.5:1 in
// the dark. The list's rows are the one list-row shape, picked or not, and a
// handle is the row's own ink rather than a chip with a colour of its own.
test("the picked entity is drawn as a list selection, never the primary button", async () => {
  mount("#/config?lens=entities&entity=agent-ceo", diff, (params?: Record<string, unknown>) =>
    params?.id
      ? { kind: "roles", id: params.id, entity: { handle: params.id } }
      : { kind: "roles", ids: ["agent-ai-systems-engineer", "agent-ceo"] },
  );

  const picked = await screen.findByRole("button", { pressed: true });
  const rows = [...document.querySelectorAll(".entity-pick-list > button")];
  expect(rows).toHaveLength(2);
  for (const row of rows) {
    expect(row.className, "a list row takes the kit's button variants").toBe("entity-pick-row");
    expect(row.querySelector("code"), "a handle inside a chip with its own ink").toBeNull();
  }
  expect(picked.classList.contains("crewlet-btn--primary")).toBe(false);
});

// SUMMARY IS THE HISTORY'S ONE FLEXIBLE TRACK. Revision and Summary were both
// `1fr`, so a ten-character id took ~340px at 1440 while the summary beside it
// was cut ("Raise Agent SWE's daily token ceiling from 100k to …"), and under
// a peek the active id itself was cut ("cebf4afb…"). The id and its pill are
// bounded, so that column is exactly its content; When and By size to theirs.
test("the summary is the one flexible track, and the revision is exactly its own width", async () => {
  const { container } = mount("#/config?lens=audit");
  expect(await screen.findByText("connect datadog")).toBeDefined();
  const grid = container.querySelector<HTMLElement>(".grid-wrap")!;
  const heads = [...grid.querySelectorAll(".grid-head > .grid-th")].map((h) =>
    h.textContent?.trim(),
  );
  expect(heads).toEqual(["When", "Revision", "Summary", "By"]);
  const tracks =
    grid.style.gridTemplateColumns.match(/minmax\([^)]*\)|fit-content\([^)]*\)|\S+/g) ?? [];
  expect(tracks).toEqual([
    "fit-content(20%)",
    "max-content",
    "minmax(12rem, 1fr)",
    "fit-content(20%)",
  ]);
});

// BESIDE A PEEK, WHEN GIVES WAY AND THE SUMMARY KEEPS ITS WORDS. With a floor
// of zero the summary was the column that yielded: at the 486px a peek leaves
// a 1440 window, When, the id and its pill and the author were drawn whole and
// the summary kept ~128px ("Add the MCP server…"). Floored, the row that
// cannot hold them all hides When — the peek's own "Created" — and says so.
describe("the revisions table, laid out", () => {
  // As the browser measured each column at its content in the harness: the
  // summary at its 12rem floor, the rest at their heads.
  const NATURAL: Record<string, number> = { When: 75, Revision: 160, Summary: 192, By: 97 };
  let box = 486;
  const real = globalThis.ResizeObserver;
  beforeEach(() => {
    globalThis.ResizeObserver = class {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    } as unknown as typeof ResizeObserver;
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.classList.contains("grid-wrap") ? box : 0;
    });
    // Each head is its column's natural width, laid end to end.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const width = (el: Element) => NATURAL[el.textContent?.trim() ?? ""] ?? 60;
      if (this.classList.contains("grid-th")) {
        const siblings = [...this.parentElement!.children];
        const left = siblings.slice(0, siblings.indexOf(this)).reduce((n, h) => n + width(h), 0);
        return {
          left,
          right: left + width(this),
          width: width(this),
          top: 0,
          bottom: 0,
          height: 0,
        } as DOMRect;
      }
      const right = this.classList.contains("grid-wrap") ? box : 0;
      return { left: 0, right, width: right, top: 0, bottom: 0, height: 0 } as DOMRect;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    globalThis.ResizeObserver = real;
    box = 486;
  });

  async function drawn(width: number) {
    box = width;
    const { container } = mount("#/config?lens=audit");
    expect(await screen.findByText("connect datadog")).toBeDefined();
    const grid = container.querySelector<HTMLElement>(".grid-wrap")!;
    return {
      heads: [...grid.querySelectorAll(".grid-head > .grid-th")].map((h) => h.textContent?.trim()),
      hidden: grid.parentElement?.querySelector(".grid-foot")?.textContent ?? "",
    };
  }

  test("beside a peek it hides When rather than cutting the summary", async () => {
    const at = await drawn(486);
    expect(at.heads).toEqual(["Revision", "Summary", "By"]);
    expect(at.hidden).toContain("Hidden to fit: When");
  });

  test("at a 1280 window it draws every column", async () => {
    const at = await drawn(746);
    expect(at.heads).toEqual(["When", "Revision", "Summary", "By"]);
    expect(at.hidden).not.toContain("Hidden to fit");
  });

  // A COLUMN HIDDEN IN FAVOUR OF THE PEEK HAS TO BE IN IT.
  test("When is the peek's own Created", async () => {
    cleanup();
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      Promise.resolve(what === "config_audit" ? revisions : {});
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <RevisionPeek id={revisions[0]!.revision_id} />
        </Router>
      </ClientContext.Provider>,
    );
    expect((await screen.findAllByText("Created")).length).toBeGreaterThan(0);
  });
});

// EACH PROPERTY ONCE. The peek's header drew Created, By, Source and
// Activated as facts and the provenance group said all four again a hundred
// pixels down; a header and a rail stacked in one column are one reading, so
// the group states them and the header carries identity and state. And the
// ids are VALUES in the mono face — the flag that marks a LABEL as an
// identifier had set "Revision" and "Parent" in mono and the ids in sans.
test("a revision's peek says each property once, its ids in the mono face", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "config_audit"
        ? [{ ...revisions[0]!, parent_revision_id: revisions[1]!.revision_id }, revisions[1]]
        : {},
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <RevisionPeek id={revisions[0]!.revision_id} />
      </Router>
    </ClientContext.Provider>,
  );
  expect(await screen.findByText("Where it came from")).toBeDefined();
  // A PROPERTY'S LABEL is a fact's or a rail row's — the eyebrow above the
  // title names the kind of object, which is not one of its properties.
  const said = (label: string) =>
    [...document.querySelectorAll(".fact-label, dt")].filter((el) => el.textContent === label);
  for (const label of ["Created", "Source", "Activated", "Revision", "Parent"]) {
    expect(said(label), label).toHaveLength(1);
  }
  expect(said("By")).toHaveLength(0);
  for (const id of [revisions[0]!.revision_id, revisions[1]!.revision_id]) {
    expect(screen.getByText(id).classList.contains("mono"), id).toBe(true);
  }
  for (const label of ["Revision", "Parent"]) {
    expect(said(label)[0]!.classList.contains("mono"), label).toBe(false);
  }
});

// THE ROW THE RAIL IS OPEN ON IS MARKED. A row's click sets `revision=` and
// opens the peek together, but `[` and `]` move the peek alone — so a mark
// keyed on `revision=` stayed on the row the reader had stepped away from.
test("the revision the peek is open on is the marked row", async () => {
  const peeked = revisions[1]!.revision_id;
  const { container } = mount(
    `#/config?lens=audit&revision=${revisions[0]!.revision_id}&peek=revision:${peeked}`,
  );
  expect(await screen.findByText("connect datadog")).toBeDefined();
  const marked = [...container.querySelectorAll(".grid-wrap .grid-row.selected")];
  expect(marked).toHaveLength(1);
  expect(marked[0]!.textContent).toContain("first import");
});

// THE NAME OUTRANKS ITS KIND. A narrow By column cut the name to "n…" beside a
// whole `operator` chip. In a revisions table the author wraps and draws one
// line, so the chip — second, and so the first thing to leave the line — goes
// before the name loses a character; the title still carries both.
test("an author's name comes before its kind, on a line the kind leaves first", async () => {
  const { container } = mount("#/config?lens=audit");
  expect(await screen.findByText("connect datadog")).toBeDefined();
  const author = container.querySelector<HTMLElement>(".grid-cell > .revision-author")!;
  expect(author).not.toBeNull();
  expect(author.firstElementChild?.textContent).toBe("founder");
  expect(author.lastElementChild?.classList.contains("crewlet-tag")).toBe(true);
  expect(author.getAttribute("title")).toBe("founder · operator");

  const css = readFileSync(join(process.cwd(), "src/styles/screens.css"), "utf8");
  const at = css.indexOf(".grid-cell > .revision-author {");
  expect(at, "the one-line author rule is declared").toBeGreaterThan(-1);
  const rule = css.slice(at, css.indexOf("}", at));
  expect(rule).toMatch(/flex-wrap:\s*wrap/);
  expect(rule).toMatch(/overflow:\s*hidden/);
  // ONE LINE, AS TALL WITH THE CHIP AS WITHOUT IT: the box and the name's
  // line are one height, and a height that holds the chip.
  expect(rule).toMatch(/(^|[\s;{])height:\s*var\(--size-control-sm\)/);
  expect(rule).toMatch(/line-height:\s*var\(--size-control-sm\)/);
});

// THE NOTE UNDER THE RECORD IS CLEAR OF IT. At the one-step gap the caption
// sat almost flush on the code block's bottom edge and read as its last line;
// every note under a block takes the two-step gap.
test("the note under the active revision keeps its step from the block", async () => {
  mount("#/config?lens=active");
  const note = await screen.findByText(/Click into the revision/);
  const column = note.parentElement!;
  expect(column.classList.contains("gap-2"), column.className).toBe(true);
  expect(column.querySelector(".crewlet-codeblock, pre")).not.toBeNull();
});
