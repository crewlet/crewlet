/**
 * An answer and the records it read: no card per read, the first mention of
 * each a chip whose peek holds what the card held, the rest on one Also looked up
 * line, and every other card drawn as it was.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AssistantCardEvent, AssistantTableCard } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { CardBlock, type CardHandlers } from "./blocks.tsx";
import { Answer, TableRows } from "./reads.tsx";
import { sourcesOf } from "./sources.tsx";

afterEach(cleanup);

const handlers = (): CardHandlers => ({
  approve: vi.fn(),
  decline: vi.fn(),
  prepareAgain: vi.fn(),
  openHere: vi.fn(),
  ask: vi.fn(),
  undo: vi.fn(),
});

let n = 0;
const event = (card: AssistantCardEvent["card"]): AssistantCardEvent => ({
  card_id: `c${++n}`,
  message_id: "m",
  card,
});

const crash = (status = "in_progress") =>
  event({
    type: "entity",
    entity: {
      kind: "task",
      id: "GOP-11",
      key: "GOP-11",
      title: "Android 12 crash when lesson audio starts",
      subtitle: "GOP-11",
      facts: [
        { label: "Status", value: status, type: "label" },
        { label: "Version", value: "4", type: "number", detail: true },
      ],
      open: { label: "GOP-11", screen: "item", href: "#/worklet/GOP-11" },
    },
  });
const qa = event({
  type: "entity",
  entity: {
    kind: "seat",
    id: "qa",
    title: "QA",
    subtitle: "qa",
    facts: [{ label: "State", value: "Idle" }],
    open: { label: "QA", screen: "seat", href: "#/company/org/seats/qa" },
  },
});
const waiting = event({
  type: "table",
  table: {
    title: "Decisions waiting on you",
    columns: [
      { key: "task", label: "Task", type: "label" },
      { key: "question", label: "Question", type: "text" },
    ],
    rows: [["GOP-11", "Hold the beta?"]],
    row_count: 1,
    truncated: false,
    as_of: "2026-10-27T09:00:00Z",
    exports: {
      csv: "/assistant/exports/waiting.csv",
      xlsx: "/assistant/exports/waiting.xlsx",
    },
    open: { label: "Inbox", screen: "inbox", href: "#/inbox" },
  },
});

/** The chart a read's seat chip draws its face from: `qa` is an agent with a look. */
function chart() {
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Nimbus",
      roles: [{ name: "QA", handle: "qa", avatar: { character: "flasklet", color: "amber" } }],
    }),
  );
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  return { store, socket };
}

function draw(text: string, cards: AssistantCardEvent[], on = handlers()) {
  render(
    <ClientContext.Provider value={chart()}>
      <Router>
        <Answer
          text={text}
          dimmed={false}
          cards={cards}
          card={(e) => (
            <CardBlock key={e.card_id} event={e} local={undefined} on={on} opened={false} />
          )}
          on={on}
        />
      </Router>
    </ClientContext.Provider>,
  );
  return on;
}

// A SEAT'S RECORD IS ITS FACE: an agent's character, never one glyph for every seat.
test("a seat's record chip wears the seat's face", () => {
  draw("**QA** is idle.", [qa]);
  const face = document.querySelector(".crewlet-avatar");
  expect(face?.classList.contains("crewlet-avatar--character")).toBe(true);
  expect(face?.classList.contains("crewlet-avatar--hue-amber")).toBe(true);
});

test("the first mention of a read record is its chip, and later ones stay words", () => {
  draw("**GOP-11** is QA asking whether to hold the beta. Hold GOP-11 if unsure.", [crash(), qa]);
  const chips = screen.getAllByRole("button");
  expect(chips.map((c) => c.textContent)).toEqual(["GOP-11", "QA"]);
  expect(chips[0]!.getAttribute("aria-label")).toBe(
    "GOP-11, Android 12 crash when lesson audio starts",
  );
  expect(screen.queryByRole("list", { name: "Also looked up" })).toBeNull();
});

test("a record read twice is one chip, drawn from its fresher read", () => {
  draw("GOP-11 waits on you.", [crash("todo"), crash("in_progress")]);
  expect(screen.getAllByRole("button")).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: /GOP-11/ }));
  expect(within(screen.getByRole("dialog")).getByText("In progress")).toBeTruthy();
});

test("what the answer read and never named is on the Also looked up line, and nothing is a card", () => {
  draw("The beta waits on one decision.", [crash(), waiting, qa]);
  const line = screen.getByRole("list", { name: "Also looked up" });
  expect(
    within(line)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["GOP-11", "Decisions waiting on you (1)", "QA"]);
  expect(document.querySelector(".crewlet-status-card")).toBeNull();
  expect(document.querySelector(".crewlet-result-table-card")).toBeNull();
});

test("a record's peek says its key once, its facts, and opens its page in place", async () => {
  const on = draw("GOP-11 waits.", [crash()]);
  const chip = screen.getByRole("button", { name: /GOP-11/ });
  fireEvent.click(chip);
  const dialog = screen.getByRole("dialog", {
    name: "GOP-11 Android 12 crash when lesson audio starts",
  });
  expect(dialog.textContent!.split("GOP-11").length - 1).toBe(1);
  expect(within(dialog).getByText("In progress")).toBeTruthy();
  expect(within(dialog).queryByText("Version")).toBeNull();
  const open = within(dialog).getByRole("link", { name: "Open GOP-11" });
  expect(open.getAttribute("href")).toBe("#/worklet/GOP-11");
  fireEvent.click(open, { button: 0 });
  expect(on.openHere).toHaveBeenCalledWith(
    expect.objectContaining({ path: ["worklet", "GOP-11"] }),
  );
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("a chat room's chip is its name, and its peek says it is a room and opens it", () => {
  const id = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";
  const launch = event({
    type: "entity",
    entity: {
      kind: "room",
      id,
      key: "#launch",
      title: "#launch",
      subtitle: "Friday's release",
      open: { label: "Open room", screen: "chatlet", href: `#/chatlet/${id}` },
    },
  });
  draw("#launch is waiting on the launch copy.", [launch]);
  expect(screen.queryByRole("list", { name: "Also looked up" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /#launch/ }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("Room")).toBeTruthy();
  const open = within(dialog).getByRole("link", { name: "Open #launch" });
  expect(open.getAttribute("href")).toBe(`#/chatlet/${id}`);
});

const page = (url: string, title = "Origin rules") =>
  event({
    type: "entity",
    entity: {
      kind: "web_page",
      id: url,
      title,
      facts: [
        { label: "Address", value: url },
        { label: "Read", value: "2026-11-14T09:30:00Z", type: "datetime" },
      ],
    },
  });

test("a web page the engine read is cited under Sources, not on the Also looked up line", () => {
  draw("Overrides need the Enterprise plan.", [crash(), page("https://docs.example.com/rules")]);
  const line = screen.getByRole("list", { name: "Also looked up" });
  expect(
    within(line)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["GOP-11"]);
  const sources = screen.getByRole("region", { name: "Sources" });
  const link = within(sources).getByRole("link", { name: /Origin rules/ });
  expect(link.getAttribute("href")).toBe("https://docs.example.com/rules");
  expect(within(sources).getByText("docs.example.com")).toBeTruthy();
  expect(within(sources).getByText(/^Read /)).toBeTruthy();
});

test("a source opens only through the stop that names its host, and only when asked", () => {
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  draw("Overrides need the Enterprise plan.", [page("https://docs.example.com/rules")]);
  const sources = screen.getByRole("region", { name: "Sources" });
  fireEvent.click(within(sources).getByRole("link", { name: /Origin rules/ }));
  expect(open).not.toHaveBeenCalled();
  const stop = screen.getByRole("dialog", { name: "Open an external link?" });
  expect(within(stop).getByText(/Ask Crewlet read this page/)).toBeTruthy();
  expect(within(stop).getAllByText(/docs\.example\.com/).length).toBeGreaterThan(0);
  fireEvent.click(within(stop).getByRole("button", { name: "Open link" }));
  expect(open).toHaveBeenCalledWith(
    "https://docs.example.com/rules",
    "_blank",
    "noopener,noreferrer",
  );
  open.mockRestore();
});

test("a page's title in the answer stays words: the site wrote it, so the page is cited, not named", () => {
  draw("Origin rules says overrides need the Enterprise plan.", [
    page("https://docs.example.com/rules"),
  ]);
  expect(screen.queryByRole("button", { name: /Origin rules/ })).toBeNull();
  expect(screen.queryByRole("list", { name: "Also looked up" })).toBeNull();
  const sources = screen.getByRole("region", { name: "Sources" });
  expect(within(sources).getByRole("link", { name: /Origin rules/ })).toBeTruthy();
});

test("a page read in parts is one source, and an address that is not https is none", () => {
  const sources = sourcesOf([
    page("https://docs.example.com/rules"),
    page("https://docs.example.com/rules", "Origin rules, part two"),
    page("http://docs.example.com/plain"),
    page("javascript:alert(1)"),
    crash(),
  ]);
  expect(sources.map((s) => [s.url.href, s.title])).toEqual([
    ["https://docs.example.com/rules", "Origin rules, part two"],
  ]);
});

test("a table's peek holds its rows, its downloads and the way to its page", () => {
  draw("", [waiting]);
  fireEvent.click(screen.getByRole("button", { name: "Decisions waiting on you (1)" }));
  const dialog = screen.getByRole("dialog", { name: "Decisions waiting on you" });
  expect(
    within(dialog)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Task", "Question"]);
  expect(within(dialog).getByRole("button", { name: /CSV/ })).toBeTruthy();
  expect(within(dialog).getByRole("button", { name: /XLSX/ })).toBeTruthy();
  expect(
    within(dialog)
      .getByRole("link", { name: /Open in Inbox/ })
      .getAttribute("href"),
  ).toBe("#/inbox");
});

test("an address that is not this dashboard's own draws no way there", () => {
  const elsewhere = event({
    type: "entity",
    entity: {
      kind: "task",
      id: "GOP-12",
      key: "GOP-12",
      title: "Somewhere else",
      open: { label: "GOP-12", screen: "item", href: "https://example.com/GOP-12" },
    },
  });
  draw("GOP-12 is elsewhere.", [elsewhere]);
  fireEvent.click(screen.getByRole("button", { name: /GOP-12/ }));
  expect(within(screen.getByRole("dialog")).queryByRole("link")).toBeNull();
});

test("a read too large to show is listed by what it read, and its peek opens its screen and says what narrows it", () => {
  const cut = event({
    type: "oversize",
    oversize: {
      title: 'Matching tasks, "brand identity"',
      open: { label: "Search", screen: "worklet-search", href: "#/worklet/search" },
      narrow: ["project", "status", "the search words"],
    },
  });
  draw("Nothing in Crewlet holds a brand identity yet.", [cut]);
  const line = screen.getByRole("list", { name: "Also looked up" });
  const chip = within(line).getByRole("button");
  expect(chip.textContent).toBe('Matching tasks, "brand identity" (too large to show here)');
  expect(chip.getAttribute("title")).toBe(chip.textContent);
  fireEvent.click(chip);
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("none of it is shown here");
  expect(dialog.textContent).toContain("narrowed by project, status or the search words");
  expect(
    within(dialog)
      .getByRole("link", { name: /Open in Search/ })
      .getAttribute("href"),
  ).toBe("#/worklet/search");
  expect(document.body.textContent).not.toContain("open it in the app");
});

test("a list read is one chip, named by what it read and how many rows, however many rows it holds", () => {
  const twenty = event({
    type: "table",
    table: {
      title: "Tasks, GP",
      columns: [{ key: "key", label: "Task", type: "label" }],
      rows: Array.from({ length: 20 }, (_, i) => [`GP-${i + 1}`]),
      row_count: 20,
      truncated: false,
      as_of: "2026-10-27T09:00:00Z",
    },
  });
  draw("Twenty tasks are open.", [twenty]);
  expect(
    within(screen.getByRole("list", { name: "Also looked up" })).getAllByRole("button"),
  ).toHaveLength(1);
  expect(screen.getByRole("button", { name: "Tasks, GP (20)" })).toBeTruthy();
});

test("every card that is not a read is drawn as it was, after the answer", () => {
  const notice = event({
    type: "notice",
    notice: { tone: "info", text: "Showing the first 3 rows." },
  });
  draw("GOP-11 waits.", [crash(), notice]);
  expect(screen.getByText("Showing the first 3 rows.")).toBeTruthy();
});

test("a page of a list says so on its chip and in its peek, never in a notice beside it", () => {
  const page = event({
    type: "table",
    table: {
      title: "Turns",
      columns: [{ key: "turn", label: "Turn", type: "text" }],
      rows: [["t1"], ["t2"], ["t3"]],
      row_count: 3,
      truncated: false,
      more: true,
      as_of: "2026-10-27T09:00:00Z",
      open: { label: "Turns", screen: "turns", href: "#/live/turns" },
    },
  });
  const cut = event({
    type: "table",
    table: {
      title: "Matching tasks",
      columns: [{ key: "key", label: "Task", type: "label" }],
      rows: [["GP-1"], ["GP-2"]],
      row_count: 210,
      truncated: true,
      as_of: "2026-10-27T09:00:00Z",
    },
  });
  draw("", [page, cut]);
  const chips = within(screen.getByRole("list", { name: "Also looked up" })).getAllByRole("button");
  expect(chips.map((c) => c.textContent)).toEqual([
    "Turns (first 3)",
    "Matching tasks (first 2 of 210)",
  ]);
  fireEvent.click(chips[0]!);
  expect(screen.getByRole("dialog").textContent).toContain(
    "First 3 rows are shown here; Turns shows all of them.",
  );
  expect(document.querySelector(".crewlet-callout")).toBeNull();
});

test("a record whose list is a page says so in its peek", () => {
  const thread = event({
    type: "entity",
    entity: {
      kind: "task",
      id: "GOP-11",
      key: "GOP-11",
      title: "Android 12 crash when lesson audio starts",
      items: [
        { id: "c1", label: "Seen on a Pixel 6", fact: "qa" },
        { id: "c2", label: "Fixed in 4.2", fact: "dev" },
      ],
      items_more: true,
      open: { label: "GOP-11", screen: "item", href: "#/worklet/GOP-11" },
    },
  });
  draw("GOP-11 has two recent comments.", [thread]);
  fireEvent.click(screen.getByRole("button", { name: /GOP-11/ }));
  expect(screen.getByText("Showing the first 2. Its page shows the rest.")).toBeTruthy();
});

// A TABLE READ IS THE KIT'S GRID, compact for the drawer, its total a summary
// row on the same tracks. It was the kit's `Table`, scoped to a handful of
// rows, with no hover value on a cut word.
const TABLE = {
  title: "Tokens by seat",
  columns: [
    { key: "seat", label: "Seat", type: "text" },
    { key: "state", label: "State", type: "label" },
    { key: "tokens", label: "Tokens", type: "tokens" },
    { key: "id", label: "Id", type: "text", detail: true },
  ],
  rows: [
    ["Product manager", "working", 1200, "a1"],
    ["Designer", "paused", 300, "b2"],
  ],
  total: ["Total", null, 1500, null],
  row_count: 2,
  truncated: false,
  as_of: "2026-11-06T10:00:00Z",
} as unknown as AssistantTableCard;

// Mutation: draw it with the kit's `Table` again, and there is no grid, no
// compact density and no summary row.
test("a read is a compact grid of the drawer's columns, the detail column left out", () => {
  render(<TableRows table={TABLE} />);
  const grid = screen.getByRole("table", { name: "Tokens by seat" });
  expect(grid.closest("[data-density]")?.getAttribute("data-density")).toBe("compact");
  const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
  expect(heads).toEqual(["Seat", "State", "Tokens"]);
  expect(screen.queryByText("a1")).toBeNull();
});

test("the total is a summary row under the rows, on the same columns", () => {
  render(<TableRows table={TABLE} />);
  const total = document.querySelector<HTMLElement>(".crewlet-data-grid__row--summary")!;
  expect(total.dataset.position).toBe("bottom");
  expect(total.textContent).toContain("Total");
  expect(total.textContent).toContain("1,500");
});

const access = (url: string) =>
  event({
    type: "entity",
    entity: {
      kind: "seat",
      id: "sre",
      title: "Site Reliability Engineer",
      subtitle: "Cloudflare",
      facts: [{ label: "Add at the vendor first", value: "Origin Rules: Edit" }],
      outside: { label: "Add to the company's connection at Cloudflare", url },
    },
  });

test("a step a read names outside Crewlet opens only through the interstitial, naming where it goes", async () => {
  draw("The Site Reliability Engineer needs it.", [
    access("https://dash.cloudflare.com/profile/api-tokens"),
  ]);
  fireEvent.click(screen.getByRole("button", { name: /Site Reliability Engineer/ }));
  const peek = screen.getByRole("dialog", { name: /Site Reliability Engineer/ });
  expect(within(peek).queryByRole("link", { name: /Add to the company/ })).toBeNull();
  fireEvent.click(
    within(peek).getByRole("button", { name: "Add to the company's connection at Cloudflare" }),
  );
  const stop = await screen.findByRole("dialog", { name: "Open an external link?" });
  expect(within(stop).getByText("dash.cloudflare.com")).toBeTruthy();
  expect(within(stop).getByText(/the vendor's own page, outside Crewlet/)).toBeTruthy();
});

test("an outside step whose address is not an absolute web address draws nothing", () => {
  draw("The Site Reliability Engineer needs it.", [access("javascript:alert(1)")]);
  fireEvent.click(screen.getByRole("button", { name: /Site Reliability Engineer/ }));
  const peek = screen.getByRole("dialog", { name: /Site Reliability Engineer/ });
  expect(within(peek).queryByRole("button", { name: /Add to the company/ })).toBeNull();
});
