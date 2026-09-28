/**
 * Copy link says what the browser did, including when it said no.
 *
 * The engine serves the dashboard over plain HTTP on `api.port` unless
 * something terminates TLS in front of it, and an insecure origin has NO
 * `navigator.clipboard` at all — so the ordinary deployment is the one where
 * the write cannot even be attempted. The button used to call it through an
 * optional chain, which skipped the whole thing: no copy, no word, a button
 * that looked broken.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import {
  Breadcrumb,
  CopyLink,
  SectionTabs,
  StarPage,
  WorkingNow,
  foldTabs,
} from "./PageHeader.tsx";
import { WORKSPACES } from "~/app/nav.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

const had = Object.getOwnPropertyDescriptor(navigator, "clipboard");

function clipboard(value: unknown): void {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value });
}

afterEach(() => {
  cleanup();
  if (had) Object.defineProperty(navigator, "clipboard", had);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
});

async function press(): Promise<void> {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Copy link|Copied|Blocked/ }));
  });
}

describe("Copy link", () => {
  test("an insecure origin, with no clipboard at all, says Blocked", async () => {
    clipboard(undefined);
    render(<CopyLink />);
    await press();
    expect(screen.getByRole("button").textContent).toBe("Blocked");
  });

  test("a write the browser refuses says Blocked", async () => {
    clipboard({ writeText: vi.fn().mockRejectedValue(new Error("NotAllowedError")) });
    render(<CopyLink />);
    await press();
    expect(screen.getByRole("button").textContent).toBe("Blocked");
  });

  test("a write that lands copies the whole URL and says Copied", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    clipboard({ writeText });
    render(<CopyLink />);
    await press();
    expect(writeText).toHaveBeenCalledWith(location.href);
    expect(screen.getByRole("button").textContent).toBe("Copied");
  });
});

// A STAR IS A SHORTCUT TO A PAGE, so it is offered on a page and nowhere else.
describe("the star", () => {
  test("is offered on an object's page", () => {
    render(<StarPage path={["work", "ENG-42"]} label="ENG-42" workspace="work" />);
    expect(screen.getByRole("button", { name: "Keep in Starred" })).toBeTruthy();
  });

  // The store drops a kept path that does not resolve, so a star pressed on
  // Not Found filled and then vanished on the next read, with nothing said.
  test("is not offered on an address that is no page", () => {
    render(<StarPage path={["work", "tasks"]} label="Not found" workspace="work" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  // A WORKSPACE'S OWN PAGE is a sidebar row already, so the star cannot keep
  // it — but it is drawn there, unavailable and saying why, because it was
  // drawn on every section of a workspace but the first and the header's
  // controls moved between two tabs of one workspace.
  test("is drawn on a workspace's own page, unavailable, with the reason", () => {
    render(<StarPage path={["me"]} label="My work" workspace="me" />);
    const star = screen.getByRole("button", { name: "Keep in Starred" });
    expect(star.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText("Already in the sidebar")).toBeTruthy();
    fireEvent.click(star);
    expect(localStorage.getItem("crewlet_starred") ?? "[]").toBe("[]");
  });
});

// THE TRAIL IS A TAB STOP ONLY WHILE IT HAS SOMEWHERE TO SCROLL. It was one
// on every page, and a trail that fits (nearly every trail) took a Tab press
// that did nothing.
describe("the breadcrumb", () => {
  const crumbs = [
    { label: "Live", path: ["live"] },
    { label: "Turns", path: ["live", "turns"] },
    { label: "Replied in the thread about the observability pipeline." },
  ];
  const had = {
    scroll: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollWidth"),
    client: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth"),
  };
  function widths(scroll: number, client: number): void {
    Object.defineProperty(HTMLElement.prototype, "scrollWidth", {
      configurable: true,
      get: () => scroll,
    });
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => client,
    });
  }
  afterEach(() => {
    for (const [key, d] of [
      ["scrollWidth", had.scroll],
      ["clientWidth", had.client],
    ] as const) {
      if (d) Object.defineProperty(HTMLElement.prototype, key, d);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[key];
    }
  });
  const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });

  test("a trail that fits takes no tab stop", () => {
    widths(400, 400);
    render(<Breadcrumb crumbs={crumbs} />);
    expect(trail().hasAttribute("tabindex")).toBe(false);
  });

  test("a trail wider than its box is a tab stop, so the keyboard can scroll it", () => {
    widths(697, 494);
    render(<Breadcrumb crumbs={crumbs} />);
    expect(trail().getAttribute("tabindex")).toBe("0");
  });

  // EACH LABEL IS ITS OWN BOX, the one `.crumb-text` ellipsises: text straight
  // inside the flex part is an anonymous flex item no ellipsis reaches.
  test("every crumb's words sit in the box that ellipsises, the page's h1 included", () => {
    widths(400, 400);
    render(<Breadcrumb crumbs={crumbs} />);
    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1.querySelector(":scope > .crumb-text")?.textContent).toBe(crumbs[2]!.label);
    for (const link of trail().querySelectorAll("a.crumb-link")) {
      expect(link.querySelector(":scope > .crumb-text")).not.toBeNull();
    }
  });
});

// ON A PROJECT'S PAGE THE HEADER COUNTS THE SEATS ON ITS WORK — the task each
// running turn is charged to — and elsewhere every seat working anywhere. A
// seat on OPS work is not "on ENG" because the reader is looking at ENG.
describe("who is working", () => {
  function mount(project: string) {
    const store = new Store();
    store.applyAgents([
      {
        id: "a1",
        role: "SWE",
        activity: "working",
        live_call: { work_item: { key: "ENG-1", project: "ENG" } },
      },
      {
        id: "a2",
        role: "SRE",
        activity: "working",
        live_call: { work_item: { key: "OPS-2", project: "OPS" } },
      },
      // BETWEEN CALLS the turn still carries its task, and still counts.
      {
        id: "a3",
        role: "PM",
        activity: "working",
        turn: { work_item: { key: "ENG-7", project: "ENG" } },
      },
      { id: "a4", role: "CTO", activity: "idle" },
    ] as never);
    const socket = new LiveSocket(store);
    return render(
      <Router>
        <ClientContext.Provider value={{ store, socket }}>
          <WorkingNow project={project} />
        </ClientContext.Provider>
      </Router>,
    );
  }

  test("a project's page counts the seats on that project's work alone", () => {
    const { container } = mount("ENG");
    expect(container.querySelector(".working-now")?.textContent).toContain("2 agents on ENG");
  });

  test("every other page counts every working seat", () => {
    const { container } = mount("");
    expect(container.querySelector(".working-now")?.textContent).toContain("3 agents working");
  });

  test("a project nobody is working on draws nothing", () => {
    const { container } = mount("LEAD");
    expect(container.querySelector(".working-now")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The section tabs
// ---------------------------------------------------------------------------

// WHAT DOES NOT FIT FOLDS INTO "MORE", from the end, and the tab the reader is
// on is always drawn. A strip that scrolled behind a fade cut "Checklist 0" at
// 1280 with nothing to say a section was past it.
describe("the section tabs fold what does not fit into More", () => {
  // Seven tabs of 100, 20 apart: 820 in all.
  const widths = [100, 100, 100, 100, 100, 100, 100];

  test("a strip that fits folds nothing", () => {
    expect(foldTabs({ widths, current: 0, space: 820, gap: 20, more: 60 })).toEqual([]);
    expect(foldTabs({ widths, current: 0, space: 819, gap: 20, more: 60 })).not.toEqual([]);
  });

  test("past the space, the last tabs fold and More takes their room", () => {
    // 700 of room: More (60) + five tabs and their gaps (5 × 120 = 600) = 660;
    // a sixth would be 780.
    expect(foldTabs({ widths, current: 0, space: 700, gap: 20, more: 60 })).toEqual([5, 6]);
  });

  test("the tab the reader is on is drawn, taking the last place that fits", () => {
    expect(foldTabs({ widths, current: 6, space: 700, gap: 20, more: 60 })).toEqual([4, 5]);
  });

  // A RUN FROM THE START, never a pick: a narrow tab further along does not
  // jump the queue, or the strip's order would change with its width.
  test("a narrower tab further along does not skip ahead of a wider one", () => {
    expect(
      foldTabs({ widths: [100, 100, 300, 20], current: 0, space: 400, gap: 20, more: 60 }),
    ).toEqual([2, 3]);
  });

  describe("drawn", () => {
    const row = WORKSPACES.find((w) => w.key === "me")!;
    const had = {
      rect: HTMLElement.prototype.getBoundingClientRect,
      client: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth"),
    };
    // EVERY TAB 120 WIDE, "More" 70, and the strip `space` wide; no gap.
    function lay(space: number): void {
      HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
        const w = this.hasAttribute("data-section")
          ? 120
          : this.hasAttribute("data-section-more")
            ? 70
            : 0;
        return {
          width: w,
          height: 20,
          top: 0,
          left: 0,
          right: w,
          bottom: 20,
          x: 0,
          y: 0,
        } as DOMRect;
      };
      Object.defineProperty(HTMLElement.prototype, "clientWidth", {
        configurable: true,
        get(this: HTMLElement) {
          return this.classList.contains("section-tabs") ? space : 0;
        },
      });
    }
    afterEach(() => {
      HTMLElement.prototype.getBoundingClientRect = had.rect;
      if (had.client) Object.defineProperty(HTMLElement.prototype, "clientWidth", had.client);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>).clientWidth;
      location.hash = "#/";
    });
    function mount(path: string[]) {
      return render(
        <Router>
          <SectionTabs
            row={row}
            path={path}
            query={new URLSearchParams()}
            counts={{ queue: "2", checklist: "0" }}
          />
        </Router>,
      );
    }
    const strip = () => screen.getByRole("navigation", { name: "My work sections" });
    const drawn = () =>
      [...strip().querySelectorAll("a.section-tab:not([data-folded])")].map(
        (a) => a.querySelector("span")?.textContent,
      );

    // 800 of room: More (70) and six tabs (720) is 790; seven tabs alone are
    // 840.
    test("seven tabs in 800 of room draw six and fold the last into More", async () => {
      lay(800);
      mount(["me"]);
      await waitFor(() => expect(drawn()).toHaveLength(6));
      expect(drawn().at(-1)).toBe("Watching");
      // THE FOLDED TAB IS OUT OF REACH on the strip, and in the menu instead.
      const folded = strip().querySelector("a[data-folded]")!;
      expect(folded.getAttribute("aria-hidden")).toBe("true");
      expect(screen.queryByRole("link", { name: /Checklist/ })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "More" }));
      const item = await screen.findByRole("menuitem", { name: /Checklist/ });
      expect(item.textContent).toContain("0");
      fireEvent.click(item);
      await waitFor(() => expect(location.hash).toBe("#/me/checklist"));
    });

    test("on a folded section, its own tab is drawn in the last place that fits", async () => {
      lay(800);
      mount(["me", "checklist"]);
      await waitFor(() => expect(drawn()).toHaveLength(6));
      expect(drawn().at(-1)).toBe("Checklist");
      expect(drawn()).not.toContain("Watching");
    });

    test("a strip with room for every tab draws no More", async () => {
      lay(2000);
      mount(["me"]);
      await waitFor(() => expect(drawn()).toHaveLength(7));
      expect(screen.queryByRole("button", { name: "More" })).toBeNull();
    });
  });
});
