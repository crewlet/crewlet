/**
 * Gitlet's one list: the grid's keys, its peek, Load more, the reader's days and
 * the address, held before any screen leans on them.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Suspense, useRef, type ReactNode } from "react";
import { GitCommitVerticalGlyph } from "@crewlethq/icons/glyphs";
import { afterEach, beforeAll, describe, expect, test, vi } from "vitest";

import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { Router } from "~/app/router.tsx";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { expectConsole } from "~/test/console.ts";
import {
  CodeList,
  useDayGroups,
  useListKeys,
  useListSort,
  type CodeListProps,
  type ListDay,
} from "./CodeList.tsx";

beforeAll(() => {
  // jsdom implements no scrolling, and a step scrolls its row into view.
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  pinZoneForTest("");
  vi.useRealTimers();
  location.hash = "#/";
});

interface Row {
  id: string;
  title: string;
  at: string;
}

const row = (n: number, at: string): Row => ({ id: `r${n}`, title: `Row ${n}`, at });

/** Six rows over two of the reader's days, three each, newest first. */
const ROWS: Row[] = [
  row(1, "2026-11-15T15:00:00Z"),
  row(2, "2026-11-15T12:00:00Z"),
  row(3, "2026-11-15T09:00:00Z"),
  row(4, "2026-11-14T15:00:00Z"),
  row(5, "2026-11-14T12:00:00Z"),
  row(6, "2026-11-14T09:00:00Z"),
];

const atOf = (r: Row) => r.at;
const headingOf = (day: ListDay | null) =>
  day === null
    ? "Undated"
    : day.ago === 0
      ? "Today"
      : day.ago === 1
        ? "Yesterday"
        : `On ${day.key}`;
const change = (r: Row): ObjectRef => ({ kind: "change", id: `web!${r.id.slice(1)}` });

type Props = Partial<CodeListProps<Row>> & { list?: Row[] };

function Timeline({ list = ROWS, ...props }: Props) {
  const groups = useDayGroups(list, atOf, headingOf);
  return (
    <CodeList<Row>
      groups={groups}
      look="timeline"
      spine={GitCommitVerticalGlyph}
      rowKey={(r) => r.id}
      rowHref={props.peek ? undefined : (r) => `#/x/${r.id}`}
      aria-label="History of main"
      title={(r) => r.title}
      meta={(r) => <button type="button">Copy {r.id}</button>}
      {...props}
    />
  );
}

const mount = (ui: ReactNode, hash = "#/x") => {
  location.hash = hash;
  return render(<Router>{ui}</Router>);
};

const link = (name: string) => screen.getByRole("link", { name });
const cursorRow = (container: HTMLElement) =>
  [...container.querySelectorAll<HTMLElement>(".code-list__row[data-cursor]")].map(
    (li) => li.dataset.key,
  );
const press = (key: string, init: KeyboardEventInit = {}) =>
  fireEvent.keyDown(window, { key, ...init });

/** Every click the document sees, its link's href and whether a handler took it. */
function clicks(): { seen: { href: string | null; prevented: boolean }[]; stop: () => void } {
  const seen: { href: string | null; prevented: boolean }[] = [];
  const watch = (e: MouseEvent) => {
    const href = (e.target as Element).closest("a")?.getAttribute("href") ?? null;
    seen.push({ href, prevented: e.defaultPrevented });
    e.preventDefault();
  };
  document.addEventListener("click", watch);
  return { seen, stop: () => document.removeEventListener("click", watch) };
}

describe("keys", () => {
  // Mutation: step without focusing, or let k run past the top.
  test("j and k move the cursor and focus, across a day's heading, and clamp at each end", () => {
    const { container } = mount(<Timeline />);
    press("j");
    expect(document.activeElement).toBe(link("Row 1"));
    expect(cursorRow(container)).toEqual(["r1"]);
    press("j");
    press("j");
    press("j");
    expect(document.activeElement).toBe(link("Row 4"));
    press("k");
    expect(cursorRow(container)).toEqual(["r3"]);
    for (let i = 0; i < 5; i++) press("k");
    expect(cursorRow(container)).toEqual(["r1"]);
    for (let i = 0; i < 9; i++) press("j");
    expect(cursorRow(container)).toEqual(["r6"]);
    expect(document.activeElement).toBe(link("Row 6"));
  });

  // Mutation: keep the cursor where j left it when the reader focuses another row.
  test("focus follows the reader: a row focused by Tab or a click is where j goes on from", () => {
    const { container } = mount(<Timeline />);
    act(() => link("Row 4").focus());
    expect(cursorRow(container)).toEqual(["r4"]);
    press("j");
    expect(document.activeElement).toBe(link("Row 5"));
  });

  // Mutation: dispatch a click on Enter whatever is focused.
  test("Enter on a focused link is the browser's; Enter elsewhere opens the cursor row's link", () => {
    mount(<Timeline />);
    const { seen, stop } = clicks();
    try {
      press("j");
      press("Enter");
      expect(seen).toEqual([]);
      act(() => link("Row 1").blur());
      press("Enter");
      expect(seen).toEqual([{ href: "#/x/r1", prevented: false }]);
    } finally {
      stop();
    }
  });

  // Mutation: read j inside a field.
  test("a key typed in a field moves nothing", () => {
    const { container } = mount(
      <>
        <input aria-label="Find" />
        <Timeline />
      </>,
    );
    const field = screen.getByRole("textbox", { name: "Find" });
    field.focus();
    fireEvent.keyDown(field, { key: "j" });
    expect(cursorRow(container)).toEqual([]);
    expect(document.activeElement).toBe(field);
  });

  // Mutation: drive the keys from every list on screen, ignore the kit's driver, or drop the
  // list's pointer claim (a click Safari does not focus).
  test("one driver: the first mounted, then the one pointed at, then the one focused", () => {
    const seen: string[] = [];
    const { container } = mount(
      <>
        <Timeline />
        <DataGrid<Row>
          rows={ROWS}
          rowKey={(r) => r.id}
          onRowActivate={(r) => seen.push(r.id)}
          columns={[{ key: "title", header: "Title", cell: (r) => r.title }]}
        />
      </>,
    );
    press("j");
    expect(cursorRow(container)).toEqual(["r1"]);
    fireEvent.pointerDown(container.querySelector(".crewlet-data-grid")!);
    press("j");
    press("j");
    expect(cursorRow(container)).toEqual(["r1"]);
    act(() => (document.activeElement as HTMLElement | null)?.blur());
    press("Enter");
    expect(seen).toEqual(["r2"]);
    act(() => link("Row 3").focus());
    press("j");
    expect(cursorRow(container)).toEqual(["r4"]);
    fireEvent.pointerDown(container.querySelector(".crewlet-data-grid")!);
    fireEvent.pointerDown(container.querySelector('[data-key="r5"] .code-list__meta')!);
    press("j");
    expect(cursorRow(container)).toEqual(["r5"]);
  });

  // Mutation: register an empty list as a driver.
  test("an empty list drives nothing", () => {
    const seen: string[] = [];
    mount(
      <>
        <Timeline list={[]} />
        <DataGrid<Row>
          rows={ROWS}
          rowKey={(r) => r.id}
          onRowActivate={(r) => seen.push(r.id)}
          columns={[{ key: "title", header: "Title", cell: (r) => r.title }]}
        />
      </>,
    );
    press("j");
    press("Enter");
    expect(seen).toEqual(["r1"]);
  });

  // Mutation: hold the cursor as an index.
  test("a row gained at the top keeps the cursor on the reader's row", () => {
    const { container, rerender } = mount(<Timeline />);
    press("j");
    press("j");
    rerender(
      <Router>
        <Timeline list={[row(0, "2026-11-15T16:00:00Z"), ...ROWS]} />
      </Router>,
    );
    expect(cursorRow(container)).toEqual(["r2"]);
  });

  // 38's tables: a row is found by its key on any element.
  test("useListKeys steps through a table's rows by their keys", () => {
    function Table() {
      const root = useRef<HTMLTableElement>(null);
      useListKeys({ root, keys: ["a", "b"] });
      return (
        <table ref={root}>
          <tbody>
            {["a", "b"].map((k) => (
              <tr key={k} data-key={k}>
                <td>
                  <a data-list-link="" href={`#/t/${k}`}>
                    Branch {k}
                  </a>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      );
    }
    mount(<Table />);
    press("j");
    press("j");
    expect(document.activeElement).toBe(link("Branch b"));
  });
});

describe("the peek", () => {
  // Mutation: navigate on a plain press, or prevent a modified one.
  test("a plain press peeks, a modified one is the browser's, and Enter elsewhere peeks", () => {
    mount(<Timeline peek={change} />, "#/gitlet");
    const title = link("Row 2");
    expect(title.getAttribute("href")).toBe("#/gitlet/repos/web/changes/2");
    const { seen, stop } = clicks();
    try {
      fireEvent.click(title, { metaKey: true });
    } finally {
      stop();
    }
    expect(seen).toEqual([{ href: "#/gitlet/repos/web/changes/2", prevented: false }]);
    expect(location.hash).not.toContain("peek=");
    expect(fireEvent.click(title)).toBe(false);
    expect(decodeURIComponent(location.hash)).toContain("peek=change:web!2");
  });

  test("Enter with a modifier on the cursor row is left to the browser", () => {
    mount(<Timeline peek={change} />, "#/gitlet");
    const { seen, stop } = clicks();
    try {
      press("j");
      act(() => link("Row 1").blur());
      press("Enter", { shiftKey: true });
      expect(seen).toEqual([{ href: "#/gitlet/repos/web/changes/1", prevented: false }]);
    } finally {
      stop();
    }
    press("Enter");
    expect(decodeURIComponent(location.hash)).toContain("peek=change:web!1");
  });

  // Mutation: mark by row index, or mark every row.
  test("the peeked row is marked, and no other", () => {
    const { container } = mount(<Timeline peek={change} />, "#/gitlet?peek=change:web!3");
    expect(link("Row 3").getAttribute("aria-current")).toBe("true");
    expect(link("Row 2").getAttribute("aria-current")).toBeNull();
    const marked = [...container.querySelectorAll<HTMLElement>("li[data-current]")];
    expect(marked.map((li) => li.dataset.key)).toEqual(["r3"]);
  });

  // Mutation: publish no order, and the rail has no stepper.
  test("] moves the peek to the next loaded row, and focus with it", async () => {
    const item = (r: Row): ObjectRef => ({ kind: "item", id: `ENG-${r.id.slice(1)}` });
    location.hash = "#/worklet/ENG?peek=item:ENG-3";
    await act(async () => {
      render(
        <Router>
          <PeekNeighbours>
            <Timeline peek={item} />
            <Suspense fallback={null}>
              <PeekHost />
            </Suspense>
          </PeekNeighbours>
        </Router>,
      );
    });
    act(() => link("Row 3").focus());
    await act(async () => {
      fireEvent.keyDown(document.activeElement!, { key: "]" });
    });
    await waitFor(() => expect(decodeURIComponent(location.hash)).toContain("peek=item:ENG-4"));
    expect(document.activeElement).toBe(link("Row 4"));
  });

  // Mutation: publish the order whatever neighbours says, and ] steps where the host did not ask.
  test("a list that leaves its neighbours to its host publishes no order", async () => {
    const item = (r: Row): ObjectRef => ({ kind: "item", id: `ENG-${r.id.slice(1)}` });
    location.hash = "#/worklet/ENG?peek=item:ENG-3";
    await act(async () => {
      render(
        <Router>
          <PeekNeighbours>
            <Timeline peek={item} neighbours={false} />
            <Suspense fallback={null}>
              <PeekHost />
            </Suspense>
          </PeekNeighbours>
        </Router>,
      );
    });
    act(() => link("Row 3").focus());
    await act(async () => {
      fireEvent.keyDown(document.activeElement!, { key: "]" });
    });
    expect(decodeURIComponent(location.hash)).toContain("peek=item:ENG-3");
  });

  // Mutation: ignore isSelected.
  test("isSelected marks a row the same way without a peek", () => {
    const { container } = mount(<Timeline isSelected={(r) => r.id === "r5"} />);
    expect(link("Row 5").getAttribute("aria-current")).toBe("true");
    expect(container.querySelector("li[data-current]")?.getAttribute("data-key")).toBe("r5");
  });
});

describe("a host that opens in place", () => {
  // Mutation: prevent every click, or open on a modified Enter.
  test("a host that opens a row in place takes its plain press and Enter, and a modified press keeps the link", () => {
    const onOpen = vi.fn();
    mount(<Timeline onOpen={onOpen} />, "#/gitlet");
    const { seen, stop } = clicks();
    try {
      fireEvent.click(link("Row 2"), { ctrlKey: true });
      expect(onOpen).not.toHaveBeenCalled();
      expect(seen).toEqual([{ href: "#/x/r2", prevented: false }]);
      expect(fireEvent.click(link("Row 2"))).toBe(false);
      expect(onOpen).toHaveBeenCalledTimes(1);
      expect(onOpen.mock.calls[0]![0]).toMatchObject({ id: "r2" });
      press("j");
      act(() => link("Row 1").blur());
      press("Enter", { shiftKey: true });
      expect(onOpen).toHaveBeenCalledTimes(1);
      expect(seen.at(-1)).toEqual({ href: "#/x/r1", prevented: false });
    } finally {
      stop();
    }
    press("Enter");
    expect(onOpen).toHaveBeenCalledTimes(2);
    expect(onOpen.mock.calls[1]![0]).toMatchObject({ id: "r1" });
    expect(location.hash).toBe("#/gitlet");
  });
});

describe("Load more", () => {
  const paged = (over: Partial<NonNullable<Props["paged"]>> = {}) => ({
    more: true,
    next: vi.fn(),
    paging: false,
    pageError: null,
    ...over,
  });

  // Mutation: draw the press with no page after, or drop the busy state.
  test("is drawn only with more, says its words, and is busy while a page is on its way", () => {
    const first = mount(<Timeline paged={paged({ more: false })} />);
    expect(screen.queryByRole("button", { name: "Load older commits" })).toBeNull();
    first.unmount();
    const p = paged();
    const { rerender } = mount(<Timeline paged={p} moreLabel="Load older commits" />);
    fireEvent.click(screen.getByRole("button", { name: "Load older commits" }));
    expect(p.next).toHaveBeenCalledTimes(1);
    rerender(
      <Router>
        <Timeline paged={{ ...p, paging: true }} moreLabel="Load older commits" />
      </Router>,
    );
    expect(screen.getByRole("group", { name: "History of main" }).getAttribute("aria-busy")).toBe(
      "true",
    );
    expect(
      screen.getByRole("button", { name: "Load older commits" }).getAttribute("aria-busy"),
    ).toBe("true");
  });

  // Mutation: draw the failure in the press's place.
  test("a page that failed is said beside the press, which asks again", () => {
    const p = paged({ pageError: "unavailable" });
    mount(<Timeline paged={p} moreLabel="Load older commits" />);
    expect(screen.getByRole("alert").textContent).toContain("Could not read the next page");
    fireEvent.click(screen.getByRole("button", { name: "Load older commits" }));
    expect(p.next).toHaveBeenCalledTimes(1);
  });

  // Mutation: leave focus on the body when the press goes away.
  test("the press that loads the last page moves focus to the first row it added", () => {
    const p = paged();
    const { rerender } = mount(<Timeline list={ROWS.slice(0, 3)} paged={p} />);
    const more = screen.getByRole("button", { name: "Load more" });
    act(() => more.focus());
    fireEvent.click(more);
    rerender(
      <Router>
        <Timeline list={ROWS} paged={{ ...p, more: false }} />
      </Router>,
    );
    expect(document.activeElement).toBe(link("Row 4"));
  });

  // Mutation: clear the press only when rows came, or leave focus on the body.
  test("an empty last page keeps focus in the list, and a later live row moves nothing", () => {
    const p = paged();
    const top = ROWS.slice(0, 3);
    const { rerender } = mount(<Timeline list={top} paged={p} foot="3 rows loaded" />);
    const more = screen.getByRole("button", { name: "Load more" });
    act(() => more.focus());
    fireEvent.click(more);
    const again = (list: Row[], over: Partial<NonNullable<Props["paged"]>>) =>
      rerender(
        <Router>
          <Timeline list={list} paged={{ ...p, ...over }} foot="3 rows loaded" />
        </Router>,
      );
    again(top, { paging: true });
    again(top, { more: false });
    expect(document.activeElement).toBe(link("Row 3"));
    again([row(0, "2026-11-15T16:00:00Z"), ...top], { more: false });
    expect(document.activeElement).toBe(link("Row 3"));
  });

  // Mutation: focus nothing when the list is empty.
  test("an empty list whose last page brought nothing gives focus to its foot", () => {
    const p = paged();
    const { rerender } = mount(<Timeline list={[]} paged={p} foot="No rows" />);
    const more = screen.getByRole("button", { name: "Load more" });
    act(() => more.focus());
    fireEvent.click(more);
    rerender(
      <Router>
        <Timeline list={[]} paged={{ ...p, more: false }} foot="No rows" />
      </Router>,
    );
    expect(document.activeElement).toBe(screen.getByRole("status"));
  });

  // Mutation: draw the foot only while paged.
  test("the foot is a status, with or without more", () => {
    const { rerender } = mount(<Timeline foot="6 rows loaded" paged={paged()} />);
    expect(screen.getByRole("status").textContent).toBe("6 rows loaded");
    rerender(
      <Router>
        <Timeline foot="6 rows loaded" paged={paged({ more: false })} />
      </Router>,
    );
    expect(screen.getByRole("status").textContent).toBe("6 rows loaded");
  });

  // 37's page of no match with a cursor: an empty list still pages on, its label free to change.
  test("an empty list with more draws its press and its foot, and its label changes in place", () => {
    const p = paged();
    const { rerender } = mount(
      <Timeline list={[]} paged={p} moreLabel="Keep searching" foot="No message holds it" />,
    );
    const root = screen.getByRole("group", { name: "History of main" });
    expect(screen.getByRole("button", { name: "Keep searching" })).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("No message holds it");
    rerender(
      <Router>
        <Timeline list={[]} paged={p} moreLabel="Load older commits" foot="Searched 40" />
      </Router>,
    );
    expect(screen.getByRole("group", { name: "History of main" })).toBe(root);
    expect(screen.getByRole("button", { name: "Load older commits" })).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("Searched 40");
  });
});

describe("the reader's days", () => {
  const headings = () =>
    within(screen.getByRole("group", { name: "History of main" }))
      .getAllByRole("heading", { level: 3 })
      .map((h) => h.textContent);

  // Mutation: group by day across the list, re-sorting it.
  test("each run of one day is a heading, its list named by it; a day met again opens another", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-15T18:00:00Z"));
    mount(<Timeline list={[...ROWS, row(7, "2026-11-15T08:00:00Z")]} />);
    expect(headings()).toEqual(["Today", "Yesterday", "Today"]);
    expect(screen.getAllByRole("list", { name: "Yesterday" })).toHaveLength(1);
    expect(
      within(screen.getAllByRole("list", { name: "Today" })[1]!).getByText("Row 7"),
    ).toBeTruthy();
  });

  // Mutation: cut days in UTC, or the machine's zone.
  test("the reader's zone decides the day", () => {
    const pair = [row(1, "2026-11-15T16:00:00Z"), row(2, "2026-11-15T14:00:00Z")];
    pinZoneForTest("Europe/Berlin");
    const berlin = mount(<Timeline list={pair} />);
    expect(headings()).toHaveLength(1);
    berlin.unmount();
    pinZoneForTest("Asia/Tokyo");
    mount(<Timeline list={pair} />);
    expect(headings()).toHaveLength(2);
  });

  // Mutation: read the day once, so "Today" never turns over.
  test("a heading turns over at the reader's midnight, through the one clock", () => {
    pinZoneForTest("Europe/Berlin");
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-11-15T22:59:00Z"));
    mount(<Timeline list={[row(1, "2026-11-15T10:00:00Z")]} />);
    expect(headings()).toEqual(["Today"]);
    act(() => {
      vi.advanceTimersByTime(90_000);
    });
    expect(headings()).toEqual(["Yesterday"]);
  });

  // Mutation: drop a row whose time does not parse.
  test("a row whose time does not parse is grouped under no day", () => {
    mount(<Timeline list={[row(1, "not a time")]} />);
    expect(headings()).toEqual(["Undated"]);
  });
});

describe("the address", () => {
  function Sort({ name }: { name?: string }) {
    const [sort, setSort] = useListSort(name);
    return (
      <>
        <output aria-label={name ?? "sort"}>{sort}</output>
        <button onClick={() => setSort("name")}>by name</button>
        <button onClick={() => setSort("")}>default</button>
      </>
    );
  }

  // Mutation: read one key for every list, or write the default.
  test("useListSort reads sort= or sort.<name>=, and the default writes no key", () => {
    mount(
      <>
        <Sort />
        <Sort name="repos" />
      </>,
      "#/x?sort=-at&sort.repos=size",
    );
    expect(screen.getByRole("status", { name: "sort" }).textContent).toBe("-at");
    expect(screen.getByRole("status", { name: "repos" }).textContent).toBe("size");
    fireEvent.click(screen.getAllByRole("button", { name: "default" })[0]!);
    expect(location.hash).not.toMatch(/[?&]sort=/);
    expect(location.hash).toContain("sort.repos=size");
  });

  // Mutation: let a list and a grid share a key unreported.
  test("a list and a grid on one key are reported", () => {
    expectConsole("error", /Two grids read the address key "sort"/);
    mount(
      <>
        <Sort />
        <DataGrid<Row>
          rows={ROWS}
          rowKey={(r) => r.id}
          columns={[{ key: "title", header: "Title", cell: (r) => r.title }]}
        />
      </>,
    );
  });
});

describe("roles", () => {
  // Mutation: name the row by its whole text, or let the spine be read.
  test("a timeline is a named group, a card the named list, a row named by its title", () => {
    const { container, unmount } = mount(<Timeline />);
    expect(screen.getByRole("group", { name: "History of main" })).toBeTruthy();
    expect(link("Row 1")).toBeTruthy();
    const nodes = container.querySelectorAll(".code-list__node");
    expect(nodes.length).toBe(2);
    for (const node of nodes) expect(node.getAttribute("aria-hidden")).toBe("true");
    unmount();
    mount(
      <CodeList<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        rowHref={(r) => `#/x/${r.id}`}
        aria-label="Repositories"
        title={(r) => r.title}
      />,
    );
    expect(screen.queryByRole("group")).toBeNull();
    expect(
      within(screen.getByRole("list", { name: "Repositories" })).getAllByRole("listitem"),
    ).toHaveLength(6);
  });

  // Mutation: draw the head outside the card, or the rows outside it, and this fails.
  test("a card's head sits inside the card, above its rows", () => {
    const { container } = mount(
      <CodeList<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        aria-label="Changes"
        title={(r) => r.title}
        head={<span>Open 4</span>}
      />,
    );
    const card = container.querySelector(".code-list__card");
    expect(card?.firstElementChild?.className).toBe("code-list__head");
    expect(card?.querySelector(":scope > ol.code-list__rows")).not.toBeNull();
    expect(within(card as HTMLElement).getByText("Open 4")).toBeTruthy();
  });

  // Mutation: draw the placeholder beside rows, or outside the card's head, and this fails.
  test("the placeholder is drawn under the head where there is no row, and only there", () => {
    const placeholder = <p>Loading changes</p>;
    const props = {
      rowKey: (r: Row) => r.id,
      "aria-label": "Changes",
      title: (r: Row) => r.title,
      head: <span>Open 4</span>,
      placeholder,
    };
    const { container, rerender } = mount(<CodeList<Row> rows={[]} {...props} />);
    const held = container.querySelector(".code-list__card > .code-list__placeholder");
    expect(held?.textContent).toBe("Loading changes");
    expect(screen.queryByRole("list")).toBeNull();
    rerender(
      <Router>
        <CodeList<Row> rows={ROWS} {...props} />
      </Router>,
    );
    expect(screen.queryByText("Loading changes")).toBeNull();
    expect(screen.getByRole("list", { name: "Changes" })).toBeTruthy();
  });

  // Mutation: drop an empty group, draw `last` before the rows, or the aside inside the heading.
  test("a boxed list draws each group as a card with its first and last parts, and keeps j and k across groups", () => {
    const { container } = mount(
      <CodeList<Row>
        look="boxes"
        groups={[
          {
            key: "web",
            heading: "web into main",
            aside: <button type="button">More on web</button>,
            first: <span>Train base of web</span>,
            last: <span>How web lands</span>,
            rows: ROWS.slice(0, 2),
          },
          { key: "docs", heading: "docs into main", last: <span>Nothing queued</span>, rows: [] },
          { key: "api", heading: "api into main", rows: ROWS.slice(2, 3) },
        ]}
        rowKey={(r) => r.id}
        rowHref={(r) => `#/x/${r.id}`}
        aria-label="Merge queues"
        lead={(r) => r.id}
        title={(r) => r.title}
      />,
    );
    expect(container.querySelector(".code-list--boxes")).not.toBeNull();
    const boxes = [...container.querySelectorAll<HTMLElement>(".code-list__group")];
    expect(boxes.map((b) => b.querySelector(".code-list__heading")?.textContent)).toEqual([
      "web into main",
      "docs into main",
      "api into main",
    ]);
    expect([...boxes[0]!.children].map((el) => el.className)).toEqual([
      "code-list__band",
      "code-list__first",
      "code-list__rows",
      "code-list__last",
    ]);
    expect(boxes[0]!.querySelector(".code-list__band > .code-list__aside button")).not.toBeNull();
    expect(boxes[0]!.querySelector(".code-list__heading button")).toBeNull();
    expect(within(boxes[1]!).getByText("Nothing queued")).toBeTruthy();
    expect(within(boxes[1]!).queryByRole("list")).toBeNull();
    expect(screen.getByRole("list", { name: "web into main" })).toBeTruthy();
    act(() => link("Row 2").focus());
    press("j");
    expect(document.activeElement).toBe(link("Row 3"));
  });

  // Mutation: draw something for a call with neither rows nor groups.
  test("a list handed both rows and groups, or neither, is a programming error", () => {
    expect(() =>
      mount(<CodeList<Row> rowKey={(r) => r.id} aria-label="Nothing" title={(r) => r.title} />),
    ).toThrow(/exactly one/);
  });
});
