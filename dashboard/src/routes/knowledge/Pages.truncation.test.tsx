/**
 * Where the knowledge screens draw a PAGE of an answer and say so.
 *
 * Three reads on these two screens come back bounded and MARKED on the wire,
 * and not one of the markers was read: `PagesAnswer.truncated` (declared with
 * its rule — ASKED, NOT INFERRED — and read only by the sibling container
 * peek), `PageDetail.children_truncated`, and `PageActivityAnswer.next_cursor`.
 * So the browse the product calls "every page" drew the engine's page size as
 * the company's whole knowledge base, a page with four hundred children was
 * headed "Children (50)", and a page saved two hundred times reported a
 * hundred changes as its history.
 *
 * TWO ORDERS, AND THEY ARE NOT INTERCHANGEABLE. `internal/pages` lists pages
 * `ORDER BY p.container, p.title` and reads the change feed `ORDER BY h.version
 * DESC` — so one page is the alphabetically first rows and the other is the
 * newest, and a note that said "the newest" over the listing would send a
 * reader looking for recent writing that was never in the answer.
 *
 * AND A WINDOW IS NOT A START. The browse pages with an offset, so "the first
 * 50" is true of its first window and false of every one after it — a sentence
 * that kept saying it contradicted the range printed beside it. The cases
 * below assert the range and its order on both, and that neither says "the
 * first".
 *
 * A DESTINATION HAS TO BE ABLE TO ANSWER. The "every child" link is only as
 * true as the read it lands on: a page's children are filtered by neither kind
 * nor container, so the browse it opens must narrow by neither.
 *
 * Both directions are asserted: a marker that always drew would put "there are
 * more" on every complete answer in the product.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { PageView, Pages } from "./Pages.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** One socket answering each question with a fixture, and recording the ask.
 *
 * THE PARAMETERS ARE PART OF THE ASK, so the stub takes them: several of the
 * cases below are about what this screen SENDS rather than what it draws — the
 * browse's window is `limit` and `offset` on the wire, and a pager that moves a
 * number in the address bar without moving the read is the defect with controls
 * painted on it. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(
    async (what: string, _params?: Record<string, unknown>) => answers[what as QueryName] ?? {},
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", roles: [] } as never);
  return query;
}

const at = "2026-09-19T12:00:00Z";

/** `internal/pages`' own page size for a listing and for a page's children. */
const LIST = 50;
/** `internal/pages.MaxPageChanges`. */
const FEED = 100;

const summary = (i: number) => ({
  id: `p-${i}`,
  container: "LEAD",
  title: `Page ${i}`,
  status: "published",
  version: 1,
  author: "agent-ceo",
  updated_at: at,
  revision: 1,
});

const page = { ...summary(0), title: "Test Sample Page", body: "" };

/**
 * The chip on the header row a card title sits on.
 *
 * FOUND THROUGH THE TITLE ELEMENT rather than through the text, because this
 * screen's toolbar has a "Pages" segment of its own and a text search matches
 * the control as well as the card it filters.
 */
function count(title: string): string | undefined {
  const header = [...document.querySelectorAll(".crewlet-card__header")].find(
    (h) => h.querySelector(".crewlet-card__title")?.textContent === title,
  );
  expect(header, `no card header carries the title ${title}`).toBeTruthy();
  return (header as HTMLElement).querySelector(".crewlet-count")?.textContent ?? undefined;
}

// ---------------------------------------------------------------------------
// The browse
// ---------------------------------------------------------------------------

test("a browse the engine capped says the count is a floor, and names the window it drew", async () => {
  serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: {
      pages: Array.from({ length: LIST }, (_, i) => summary(i)),
      limit: LIST,
      offset: 0,
      truncated: true,
    },
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(count("Pages")).toBe(`${LIST}+`));
  expect(
    screen.getByText(/Pages 1–50 · ordered by container, then title · there are more/),
  ).toBeTruthy();
  // NOT "the newest", which is the one wrong word available here.
  expect(screen.queryByText(/newest/)).toBeNull();
  // AND NOT "the first" EVEN HERE, where it happens to be true: one sentence
  // for every window is what stops the second one lying, and the range says
  // which window this is without a phrase that only works at offset zero.
  expect(screen.queryByText(/The first/)).toBeNull();
});

test("a browse that saw the whole container draws no caution", async () => {
  // THE CONTROL. A container holding exactly the limit holds every page it
  // has, which is why the flag is read rather than `pages.length >= limit`.
  serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: LIST }] },
    pages: {
      pages: Array.from({ length: LIST }, (_, i) => summary(i)),
      limit: LIST,
      offset: 0,
      truncated: false,
    },
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(count("Pages")).toBe(`${LIST}`));
  expect(screen.queryByText(/there are more/)).toBeNull();
});

test("the browse asks the engine for one page's children when the address says so", async () => {
  // THE DESTINATION THE DETAIL LINKS AT, exercised end to end: a `parent` in
  // the address has to reach the engine as a filter, or the "every child"
  // link lands on an unfiltered browse that looks like it worked.
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: { pages: [summary(1)], limit: LIST, offset: 0, truncated: false },
    page: { page, history: [], ancestors: [], children: [] },
  });
  location.hash = "#/knowledge/LEAD?parent=p-0";
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() =>
    expect(query.mock.calls.some((c) => c[0] === "pages" && c[1]?.parent === "p-0")).toBe(true),
  );
  // AND THE READER IS TOLD THE LIST IS NARROWED, by a control they can undo.
  expect(await screen.findByText(/Children of Test Sample Page/)).toBeTruthy();
});

// ---------------------------------------------------------------------------
// One page: its children, and its history
// ---------------------------------------------------------------------------

test("a page with more children than the detail carries says so, and where the rest are", async () => {
  serving({
    page: {
      page,
      history: [],
      ancestors: [],
      children: Array.from({ length: LIST }, (_, i) => summary(i + 1)),
      children_truncated: true,
    },
  });
  render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
  await waitFor(() => expect(count("Children")).toBe(`${LIST}+`));
  expect(screen.getByText(/The first 50 children in title order; there are more\./)).toBeTruthy();
  // THE REST ARE ONE CLICK, not a sentence about a tool a person cannot call —
  // and the destination has to be able to show them all, which is two things:
  // the browse pages with `offset` (asserted below), and it is asked for every
  // KIND. `internal/pages.Reader.Get` applies no skills predicate to a page's
  // children while the browse defaults to prose, so without `kind=all` the
  // "every child" link landed on a list holding FEWER rows than the card it
  // was under.
  const link = screen.getByRole("link", { name: /Every child of this page/ });
  expect(link.getAttribute("href")).toContain("parent=p-0");
  expect(link.getAttribute("href")).toContain("kind=all");
});

test("a child filed in another container is linked under its own", async () => {
  // NOTHING KEEPS A CHILD IN ITS PARENT'S CONTAINER: a create stores the
  // parent id without checking it against the container, and `write_page` lets
  // a model name `container` and `parent` independently. `internal/pages`
  // locates `CONTAINER/Title` by matching BOTH, so a child addressed under its
  // parent's container reaches a different page of that title or none at all —
  // a link that goes somewhere wrong rather than one that fails.
  serving({
    page: {
      page,
      history: [],
      ancestors: [],
      children: [{ ...summary(1), container: "ENG", title: "Deploy Runbook" }],
    },
  });
  render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
  const link = await screen.findByRole("link", { name: "Deploy Runbook" });
  expect(link.getAttribute("href")).toBe("#/knowledge/ENG/Deploy%20Runbook");
});

test("a page whose children all fit is counted plainly", async () => {
  serving({
    page: {
      page,
      history: [],
      ancestors: [],
      children: [summary(1), summary(2)],
    },
  });
  render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
  await waitFor(() => expect(count("Children")).toBe("2"));
  expect(screen.queryByText(/there are more/)).toBeNull();
  expect(screen.queryByRole("link", { name: /Every child of this page/ })).toBeNull();
});

test("a change feed with a cursor behind it is not the page's whole history", async () => {
  serving({
    page: { page, history: [], ancestors: [], children: [] },
    page_activity: {
      changes: Array.from({ length: FEED }, (_, i) => ({
        id: `h-${i}`,
        page_id: "p-0",
        kind: "save",
        actor: "agent-ceo",
        at,
        log_seq: i,
      })),
      next_cursor: "100",
    },
  });
  render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
  await waitFor(() => expect(count("Activity")).toBe(`${FEED}+`));
  // NEWEST HERE, because this read IS ordered by version descending — the
  // other half of why the slice is a parameter rather than a constant.
  expect(screen.getByText(/The newest 100 changes; there are more\./)).toBeTruthy();
});

test("a short change feed is counted as the whole history it is", async () => {
  serving({
    page: { page, history: [], ancestors: [], children: [] },
    page_activity: {
      changes: [{ id: "h-1", page_id: "p-0", kind: "save", actor: "agent-ceo", at, log_seq: 1 }],
    },
  });
  render(
    <Router>
      <PageView container="LEAD" title="Test Sample Page" />
    </Router>,
  );
  await waitFor(() => expect(count("Activity")).toBe("1"));
  expect(screen.queryByText(/there are more/)).toBeNull();
});

// ---------------------------------------------------------------------------
// The browse's own window
// ---------------------------------------------------------------------------

/** Every `pages` ask the screen made, oldest first. */
function asks(query: ReturnType<typeof serving>): Record<string, unknown>[] {
  return query.mock.calls
    .filter((c) => c[0] === "pages")
    .map((c) => (c[1] ?? {}) as Record<string, unknown>);
}

const filled = {
  pages: Array.from({ length: LIST }, (_, i) => summary(i)),
  limit: LIST,
  offset: 0,
  truncated: true,
};

test("the browse names its window on the wire and Next moves it", async () => {
  // THE DEFECT THIS REPLACES: the screen sent neither `limit` nor `offset`, so
  // the engine's default answered the alphabetically first fifty of a container
  // for ever — and the note under the grid could only offer filters that
  // NARROW, which is not the same as reaching the fifty-first page by title.
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: filled,
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(asks(query).length).toBeGreaterThan(0));
  // THE FIRST WINDOW IS STATED AND STARTS AT ZERO: the offset is omitted
  // rather than sent as 0, which is the same window said twice.
  expect(asks(query)[0]?.limit).toBe(LIST);
  expect(asks(query)[0]?.offset).toBeUndefined();
  expect(screen.getByText(/Pages 1–50/)).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Next" }));
  await waitFor(() => expect(asks(query).some((p) => p.offset === LIST)).toBe(true));
  // AND THE WINDOW IS A PLACE, so it is in the address and survives a reload.
  expect(location.hash).toContain("offset=50");
  expect(screen.getByText(/Pages 51–100 · ordered by container, then title/)).toBeTruthy();
  // THE SENTENCE MOVED WITH THE WINDOW. "The first 50 pages in title order"
  // printed beside "Pages 51–100" is a marker contradicting the range next to
  // it, which is worse than no marker at all.
  expect(screen.queryByText(/The first/)).toBeNull();
});

test("a window past the first page can go back, and the first page cannot", async () => {
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: filled,
  });
  location.hash = "#/knowledge/LEAD?offset=50";
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(asks(query).some((p) => p.offset === LIST)).toBe(true));
  const back = screen.getByRole("button", { name: "Previous" });
  expect((back as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(back);
  // BACK TO THE FIRST WINDOW, which is the key DELETED rather than set to 0 —
  // the same address a reader arrives on, so the two spellings of page one do
  // not read as two places.
  await waitFor(() => expect(location.hash).not.toContain("offset"));
  expect((screen.getByRole("button", { name: "Previous" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
});

test("a complete first page draws no pager at all", async () => {
  // THE CONTROL, in the direction a pager loses: controls that always draw put
  // a disabled Next under every short list in the product.
  serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 2 }] },
    pages: { pages: [summary(1), summary(2)], limit: LIST, offset: 0, truncated: false },
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(count("Pages")).toBe("2"));
  expect(screen.queryByRole("button", { name: "Next" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Previous" })).toBeNull();
});

test("a window past the end of the list says so rather than calling the container empty", async () => {
  // A BOOKMARK ON PAGE NINE outlives the pages behind it. "No pages here" over
  // that state tells a reader the container they are looking at is empty, which
  // is the one wrong thing this screen can say — and `QueryState` renders its
  // empty state INSTEAD of the card, which is why the pager lives outside it.
  serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 10 }] },
    pages: { pages: [], limit: LIST, offset: 400, truncated: false },
  });
  location.hash = "#/knowledge/LEAD?offset=400";
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  expect(await screen.findByText(/Nothing on this page of the list/)).toBeTruthy();
  expect(screen.queryByText(/No pages here/)).toBeNull();
  expect((screen.getByRole("button", { name: "Previous" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
});

test("narrowing the list starts that list at its first page", async () => {
  // A WINDOW BELONGS TO THE LIST IT WAS TAKEN OVER. Carried onto a narrower
  // one it answers nothing at all, which on this screen reads exactly like a
  // filter that matched nothing.
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: filled,
  });
  location.hash = "#/knowledge/LEAD?offset=50";
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(asks(query).some((p) => p.offset === LIST)).toBe(true));
  fireEvent.click(screen.getByRole("radio", { name: "Tool skills" }));
  await waitFor(() => expect(location.hash).toContain("kind=skills"));
  expect(location.hash).not.toContain("offset");
  await waitFor(() =>
    expect(asks(query).some((p) => p.skills === true && p.offset === undefined)).toBe(true),
  );
});

test("the All kind is reachable, and asks the engine for every kind", async () => {
  // IT WAS SPELLED AS THE EMPTY STRING and was reachable by neither route: a
  // click wrote nothing to the address (the router's own three-state rule, now
  // covered by `app/params.test.tsx`) and an anchor could not carry it either,
  // because `buildHash` drops an empty value from a query record. The detail's
  // "every child" link depends on this state existing — a page's children are
  // not filtered by kind at all.
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: filled,
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(asks(query)[0]?.skills).toBe(false));
  fireEvent.click(screen.getByRole("radio", { name: "All" }));
  await waitFor(() => expect(location.hash).toContain("kind=all"));
  // NO `skills` KEY AT ALL, which is the third state: the engine reads an
  // absent pointer as "every kind" and a `false` as "everything but skills".
  await waitFor(() => expect(asks(query).some((p) => !("skills" in p))).toBe(true));
});

test("a browse scoped to one page's children narrows by neither container nor kind", async () => {
  // THE OTHER AXIS THE `kind=all` CASE ABOVE COVERS. `internal/pages.Reader.Get`
  // collects a page's children on `p.parent_id` alone — no kind predicate and
  // no container predicate — so a browse that kept its own container term
  // answered with fewer rows than the card the link sits under, and a child
  // filed in another container was reachable by an agent's `list_pages` and by
  // nobody reading the dashboard.
  const query = serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 400 }] },
    pages: { pages: [summary(1)], limit: LIST, offset: 0, truncated: false },
    page: { page, history: [], ancestors: [], children: [] },
  });
  location.hash = "#/knowledge/LEAD?parent=p-0&kind=all";
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(asks(query).some((a) => a.parent === "p-0")).toBe(true));
  expect(asks(query).every((a) => !("container" in a))).toBe(true);
  expect(asks(query).every((a) => !("skills" in a))).toBe(true);
  // AND THE CONTROL THAT WOULD CONTRADICT IT IS NOT DRAWN: a container picker
  // showing LEAD chosen names a filter this read is not applying.
  expect(screen.queryByLabelText("Container")).toBeNull();

  // THE OTHER DIRECTION, which is what makes the two assertions above mean
  // something: clearing the chip returns the picker AND the container term.
  fireEvent.click(screen.getByRole("button", { name: /Children of/ }));
  await waitFor(() => expect(screen.queryByLabelText("Container")).toBeTruthy());
  await waitFor(() => expect(asks(query).some((a) => a.container === "LEAD")).toBe(true));
});

test("the browse draws its window in the order the engine carved it in", async () => {
  // THE NUMBERS UNDER THE GRID ARE POSITIONS. "Pages 51–100 · ordered by
  // container, then title" describes `ORDER BY p.container, p.title`, which is
  // the only ordering the `pages` read has and the only one an offset means
  // anything in — so the rows have to be drawn in it. Sorted newest-first
  // before drawing, the bar printed a range in the alphabet over rows arranged
  // by the clock, and neither half said so.
  serving({
    containers: { containers: [{ key: "LEAD", name: "Leadership", pages: 2 }] },
    pages: {
      pages: [
        { ...summary(1), title: "Alpha", updated_at: "2026-01-01T00:00:00Z" },
        { ...summary(2), title: "Beta", updated_at: "2026-09-19T12:00:00Z" },
      ],
      limit: LIST,
      offset: 0,
      truncated: false,
    },
  });
  render(
    <Router>
      <Pages container="LEAD" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("Alpha")).toBeTruthy());
  const titles = [...document.querySelectorAll('.grid-row [data-label="Title"]')].map((cell) =>
    (cell.textContent ?? "").trim(),
  );
  // THE FIXTURE IS THE ENGINE'S ORDER AND THE OPPOSITE OF THE CLOCK'S: Alpha
  // sorts first by title and is the older of the two, so a screen re-sorting
  // by `updated` would put Beta on top.
  expect(titles).toEqual(["Alpha", "Beta"]);
});
