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
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { configHunks, ConfigScreen, nodeOnRevision, RevisionPeek } from "./Config.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type ConfigChange } from "~/protocol/index.ts";
import type { FleetNode } from "~/protocol/types.ts";

// EXACTLY what internal/api/configapi/answers.go returns for these two
// questions. Change the server and this fixture has to change with it, which
// is the whole point of writing it out rather than building it from the type.
const revisions = [
  {
    revision_id: "01JCFGAAAA0000000000000001",
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    created_by_kind: "operator",
    operator_id: "",
    source: "api",
    summary: "connect datadog",
    is_active: true,
  },
  {
    revision_id: "01JCFGBBBB0000000000000002",
    created_at: "2026-08-22T15:00:00Z",
    created_by: "node-a",
    created_by_kind: "system",
    operator_id: "",
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

// READ ONLY, SAID FIRST: the note leads the screen as its page note on every
// lens, never a callout under the last card. Mutation: move it back under the
// cards and the note is no longer the content's first element.
test("the screen says it is read only first, as its page note", async () => {
  const { container } = mount("#/config?lens=audit");
  await screen.findByText("01JCFGAAAA");
  const note = screen.getByText(/Read only here/);
  expect(note.closest(".crewlet-page-note")).not.toBeNull();
  expect(note.closest(".crewlet-callout")).toBeNull();
  expect(container.querySelector(".crewlet-page-note")).toBe(note.closest(".crewlet-page-note"));
  expect(
    within(note.closest(".crewlet-page-note") as HTMLElement).getByText("crewlet config import")
      .tagName,
  ).toBe("CODE");
});

test("a revision row renders the server's own field names", async () => {
  mount("#/config?lens=audit");

  // The id: truncated to ten characters, which is what threw when the field
  // was read under a name the server does not send.
  expect(await screen.findByText("01JCFGAAAA")).toBeDefined();
  expect(screen.getByText("01JCFGBBBB")).toBeDefined();
  expect(screen.getByText("connect datadog")).toBeDefined();
  // The author, WHAT the author is, and the active marker. The kind is the
  // revision's own word: a node's seed reads as the engine's (`system`),
  // never as a person's.
  expect(screen.getByText("founder")).toBeDefined();
  expect(screen.getByText("node-a")).toBeDefined();
  expect(screen.getByText("operator")).toBeDefined();
  expect(screen.getByText("system")).toBeDefined();
  expect(screen.getAllByText("active").length).toBe(1);
});

// THE SERVER'S KINDS, AS THE ONE DIFF DRAWS THEM: an added setting is its new
// value added, a removed one its old value removed, and a CHANGED one both, so
// the diff can underline what moved inside it. The kinds were once read under
// a `data-op` with add / remove / replace, which no answer has ever carried.
test("a change is drawn as its setting's lines, each kind said in words", async () => {
  const { container } = mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  const lines = [...container.querySelectorAll<HTMLElement>(".diff-view__line")];
  expect(lines.map((el) => el.getAttribute("data-kind"))).toEqual([
    "add",
    "remove",
    "add",
    "remove",
  ]);
  expect(lines.map((el) => el.querySelector(".diff-view__path")?.textContent)).toEqual([
    "integrations.datadog.route_to",
    "integrations.gitlab.url",
    "integrations.gitlab.url",
    "integrations.slack",
  ]);
  // AND THE VALUE OF THE SIDE EACH LINE IS: the new for an addition, the old
  // for a removal, and both halves of the change on their own lines.
  expect(lines.map((el) => el.querySelector(".diff-view__text")?.textContent)).toEqual([
    '"sre-lead"',
    '"a"',
    '"b"',
    "{}",
  ]);
  // IN WORDS as well as the glyph: the glyph is drawn, the word is what a
  // reader who hears the list receives.
  expect(lines.map((el) => el.querySelector(".diff-view__mark")?.textContent)).toEqual([
    "+Added",
    "−Removed",
    "+Added",
    "−Removed",
  ]);
  // NOTHING SAYS IT WAS CUT, because it was not: changes_total equals the
  // number of changes, and a "3 of 3 shown" note would be noise on every diff.
  expect(screen.queryByText(/shown/)).toBeNull();
});

// A DIFF IS DRAWN IN ITS OWN TRACKS, and nothing in it is cut. The config
// list once shared a class with the knowledge page's line diff, so the
// cascade handed every change that diff's line-number tracks: at 1440 the
// path got 23px and the value 13px. One component draws both now, and a diff
// of settings is KEYED: its path takes the track the numbers take in a diff
// of a text. jsdom lays nothing out, so the cascade over the rendered lines is
// what is read — with every sheet the dashboard loads, in the order it loads
// them: the entry's, then the diff's own, which arrives with the diff.
test("a diff's path and value are drawn whole, in the diff's own tracks", async () => {
  const read = (name: string) => readFileSync(join(process.cwd(), "src/styles", name), "utf8");
  const style = document.createElement("style");
  style.textContent = [
    "tokens.css",
    "base.css",
    "components.css",
    "frame.css",
    "screens.css",
    "diff.css",
  ]
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
    const diffView = container.querySelector<HTMLElement>(".diff-view")!;
    const line = path.closest<HTMLElement>(".diff-view__line")!;
    expect(diffView.classList.contains("diff-view--keyed")).toBe(true);
    // EVERY ROW TAKES THE SAME TRACKS, so the values start at one edge: the
    // path's is the longest path's own width, published once on the diff.
    expect(getComputedStyle(line).gridTemplateColumns).toBe(
      "2ch min(calc(var(--diff-path) * 1ch), 40%) minmax(0, 1fr)",
    );
    expect(diffView.style.getPropertyValue("--diff-path")).toBe(String("mcp_servers[1]".length));
    const value = line.querySelector<HTMLElement>(".diff-view__text")!;
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

describe("a change as the diff draws it", () => {
  /** One change's lines, as `[kind, text]`. */
  const linesOf = (change: ConfigChange) =>
    configHunks([change])[0]!.lines.map((line) => [line.kind, line.text]);

  test("is JSON, so a string and the setting it spells stay two things", () => {
    expect(linesOf({ path: "a", kind: "added", to: "true" })).toEqual([["add", '"true"']]);
    expect(linesOf({ path: "a", kind: "added", to: true })).toEqual([["add", "true"]]);
  });

  // ONE SETTING, TWO LINES, ONE HUNK: the old value removed and the new one
  // added, kept together by their hunk however the next change reads, and
  // both named by the path the diff is keyed by.
  test("is a changed setting's old value removed and its new value added", () => {
    const [hunk, ...rest] = configHunks([{ path: "a.b", kind: "changed", from: 1, to: 2 }]);
    expect(rest).toEqual([]);
    expect(hunk).toEqual({
      skipped: 0,
      lines: [
        { kind: "remove", text: "1", before: null, after: null, path: "a.b" },
        { kind: "add", text: "2", before: null, after: null, path: "a.b" },
      ],
    });
  });

  // `omitempty` on an `any` drops a JSON null, so a setting changed from null
  // arrives with no `from` — which stringifies to `undefined`.
  test("reads an absent side as the null it was", () => {
    expect(linesOf({ path: "a", kind: "changed", to: "x" })).toEqual([
      ["remove", "null"],
      ["add", '"x"'],
    ]);
  });

  test("keeps an object in the document's own indented shape", () => {
    expect(linesOf({ path: "a", kind: "removed", from: { n: 1 } })).toEqual([
      ["remove", '{\n  "n": 1\n}'],
    ]);
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
  expect(screen.getByText(/3 of 512 shown/).textContent).toBe(
    "3 of 512 shown. crewlet config diff shows all.",
  );
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
  const grid = container.querySelector<HTMLElement>(".crewlet-data-grid")!;
  const heads = [...grid.querySelectorAll(".crewlet-data-grid__head > .crewlet-data-grid__th")].map(
    (h) => h.textContent?.trim(),
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
      return this.classList.contains("crewlet-data-grid") ? box : 0;
    });
    // Each head is its column's natural width, laid end to end.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const width = (el: Element) => NATURAL[el.textContent?.trim() ?? ""] ?? 60;
      if (this.classList.contains("crewlet-data-grid__th")) {
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
      const right = this.classList.contains("crewlet-data-grid") ? box : 0;
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
    const grid = container.querySelector<HTMLElement>(".crewlet-data-grid")!;
    return {
      heads: [...grid.querySelectorAll(".crewlet-data-grid__head > .crewlet-data-grid__th")].map(
        (h) => h.textContent?.trim(),
      ),
      hidden: grid.parentElement?.querySelector(".crewlet-data-grid__foot")?.textContent ?? "",
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
    [...document.querySelectorAll(".crewlet-fact-line__label, dt")].filter(
      (el) => el.textContent === label,
    );
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
  const marked = [
    ...container.querySelectorAll(".crewlet-data-grid .crewlet-data-grid__row--selected"),
  ];
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
  const author = container.querySelector<HTMLElement>(
    ".crewlet-data-grid__cell > .revision-author",
  )!;
  expect(author).not.toBeNull();
  expect(author.firstElementChild?.textContent).toBe("founder");
  expect(author.lastElementChild?.classList.contains("crewlet-tag")).toBe(true);
  expect(author.getAttribute("title")).toBe("founder · operator");

  const css = readFileSync(join(process.cwd(), "src/styles/screens.css"), "utf8");
  const at = css.indexOf(".crewlet-data-grid__cell > .revision-author {");
  expect(at, "the one-line author rule is declared").toBeGreaterThan(-1);
  const rule = css.slice(at, css.indexOf("}", at));
  expect(rule).toMatch(/flex-wrap:\s*wrap/);
  expect(rule).toMatch(/overflow:\s*hidden/);
  // ONE LINE, AS TALL WITH THE CHIP AS WITHOUT IT: the box and the name's
  // line are one height, and a height that holds the chip.
  expect(rule).toMatch(/(^|[\s;{])height:\s*var\(--size-control-sm\)/);
  expect(rule).toMatch(/line-height:\s*var\(--size-control-sm\)/);
});

// THE ACTIVE REVISION IS THE RECORD ALONE. The selection tip that sat under
// the block is gone, and nothing is drawn in its place.
test("the active revision carries no note under its block", async () => {
  const { container } = mount("#/config?lens=active");
  await waitFor(() => expect(container.querySelector(".crewlet-codeblock, pre")).not.toBeNull());
  expect(screen.queryByText(/Click into the revision|Ctrl\+A/)).toBeNull();
});

// A NODE IS COUNTED BY THE REVISION IT RUNS. A node that refused a revision
// reports it as its last attempt beside `error` and serves the one before;
// the panel counted it among the nodes on this revision — "report this
// revision", tagged "applied" — while it ran another company.
describe("which nodes run a revision", () => {
  const id = revisions[0]!.revision_id;
  const before = revisions[1]!.revision_id;
  const node = (name: string, over: Partial<FleetNode> = {}): FleetNode => ({
    id: name,
    roles: [],
    broker: "member",
    seats: 0,
    config_epoch: 3,
    config_status: "ok",
    config_revision_id: id,
    config_serving_revision_id: id,
    ...over,
  });
  const refused = node("refused-0", {
    config_status: "error",
    config_error: "env var CREWLET_X is not set",
    config_serving_revision_id: before,
  });
  // Re-activated to rotate a credential, refused, and still running it.
  const rotated = node("rotated-0", {
    config_status: "error",
    config_error: "secret OPENAI_API_KEY is not set",
  });

  /** The revision's peek over a fleet of `nodes`, its pointer naming the revision. */
  function mountPeek(nodes: FleetNode[]) {
    const store = new Store();
    const socket = new LiveSocket(store);
    const fleet = {
      nodes,
      seats: [],
      duties: [],
      unplaceable: [],
      unmanned_roles: [],
      this_node: nodes[0]!.id,
      target_epoch: 3,
      activation: { epoch: 3, revision_id: id, at: "2026-08-23T15:00:00Z", summary: "" },
    };
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      Promise.resolve(what === "config_audit" ? revisions : what === "fleet" ? fleet : {});
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <RevisionPeek id={id} />
        </Router>
      </ClientContext.Provider>,
    );
  }

  test("each node is running it, refused it, running another, or silent", () => {
    expect(nodeOnRevision(node("n"), id)).toBe("here");
    expect(nodeOnRevision(refused, id)).toBe("refused");
    // A PARTIAL APPLY IS A REFUSAL TOO: it never counts as converged.
    expect(
      nodeOnRevision(
        node("n", { config_status: "degraded", config_serving_revision_id: before }),
        id,
      ),
    ).toBe("refused");
    // A refusal of ANOTHER revision is that revision's story: here the node
    // simply runs something else.
    expect(nodeOnRevision(refused, before)).toBe("here");
    expect(
      nodeOnRevision(
        node("n", { config_revision_id: before, config_serving_revision_id: before }),
        id,
      ),
    ).toBe("elsewhere");
    expect(nodeOnRevision({ id: "quiet", roles: [], broker: "member", seats: 0 }, id)).toBe(
      "silent",
    );
    // A REFUSED RE-ACTIVATION OF THE REVISION A NODE RUNS — the credential
    // rotation — names that revision as attempted and as served: the status
    // is what says it was refused. Mutation: read the revision it runs
    // before the refusal and this is "here".
    expect(nodeOnRevision(rotated, id)).toBe("refused");
  });

  // THE ROTATION THAT DID NOT TAKE is said where an operator checks it: a
  // refusal in the danger tone, naming the revision it still runs and why,
  // and not counted among the nodes that run the revision. Mutation: read
  // the revision a node runs before its refusal and node rotated-0 is tagged
  // "runs it" with no reason, and the count reads 2 of 2.
  test("the peek says a refused re-activation of the revision a node runs as a refusal", async () => {
    mountPeek([node("current-0"), rotated]);
    expect(await screen.findByText("1 of 2 nodes run this revision.")).toBeDefined();
    const line = (name: string) => screen.getByText(name).closest<HTMLElement>(".row")!;
    const tag = within(line("rotated-0")).getByText(`refused, runs ${id.slice(0, 10)}`);
    expect(tag.closest(".crewlet-tag")!.className).toContain("crewlet-tag--danger");
    expect(within(line("rotated-0")).getByText("secret OPENAI_API_KEY is not set")).toBeDefined();
    expect(within(line("rotated-0")).queryByText("runs it")).toBeNull();
  });

  test("the peek counts only the nodes that run it, and names what a refusal runs", async () => {
    mountPeek([
      node("current-0"),
      refused,
      node("other-0", { config_revision_id: before, config_serving_revision_id: before }),
    ]);
    expect(await screen.findByText("1 of 3 nodes run this revision.")).toBeDefined();
    const line = (name: string) => screen.getByText(name).closest<HTMLElement>(".row")!;
    expect(within(line("current-0")).getByText("runs it")).toBeDefined();
    const tag = within(line("refused-0")).getByText(`refused, runs ${before.slice(0, 10)}`);
    expect(tag.closest(".crewlet-tag")!.className).toContain("crewlet-tag--danger");
    expect(within(line("refused-0")).getByText("env var CREWLET_X is not set")).toBeDefined();
    expect(within(line("other-0")).getByText(`on ${before.slice(0, 10)}`)).toBeDefined();
    expect(within(line("other-0")).queryByText(/could not apply/)).toBeNull();
  });
});
