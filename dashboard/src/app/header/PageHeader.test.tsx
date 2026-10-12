/**
 * The page header: its trail, its star, its section tabs, and who is working
 * now. A page carries no Copy link: the command palette's "Copy link to this
 * page" is the one place a page's address is copied from (`lib/pageLink.ts`).
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { AppShell } from "@crewlethq/ui";

import { PageHeader, SectionTabs, StarPage, Trail, WorkingNow, noteFits } from "./PageHeader.tsx";
import { ProjectSwitcherProvider } from "./ProjectSwitcher.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { foldTabs } from "../frame/tabFit.ts";
import { WORKSPACES } from "~/app/nav.ts";
import { Router } from "~/app/router.tsx";
import { noteReader } from "~/lib/reader.ts";
import { starsKey } from "~/lib/starred.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

afterEach(cleanup);

// A STAR IS A SHORTCUT TO A PAGE, so it is offered on a page and nowhere else.
describe("the star", () => {
  // STARS ARE KEPT PER READER, so these cases are read by somebody.
  beforeEach(() => noteReader("p-1"));
  afterEach(() => sessionStorage.clear());

  test("is offered on an object's page", () => {
    render(<StarPage path={["work", "ENG-42"]} label="ENG-42" workspace="work" />);
    expect(screen.getByRole("button", { name: "Keep in Starred" })).toBeTruthy();
  });

  // NOR BY NOBODY: a tab that has not learned who reads it has no list to
  // keep a star in, and one kept in a list nobody owns would be offered to
  // whoever signs in next.
  test("is not offered to a tab nobody is reading yet", () => {
    sessionStorage.clear();
    render(<StarPage path={["work", "ENG-42"]} label="ENG-42" workspace="work" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  // The store drops a kept path that does not resolve, so a star pressed on
  // Not Found filled and then vanished on the next read, with nothing said.
  test("is not offered on an address that is no page", () => {
    render(<StarPage path={["work", "projects"]} label="Not found" workspace="work" />);
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
    expect(localStorage.getItem(starsKey("p-1")) ?? "[]").toBe("[]");
  });

  // A SCREEN WHOSE OWN MENU CARRIES THE STAR (a knowledge page's More) gets
  // none from the frame, inline or in the phone's More: one control per
  // action on screen.
  test("is drawn by the frame unless the screen's own menu carries it", () => {
    location.hash = "#/work/ENG-42";
    const header = (star?: boolean) => {
      const store = new Store();
      const socket = new LiveSocket(store);
      return render(
        <Router>
          <ClientContext.Provider value={{ store, socket }}>
            <AppShell
              topbar={
                <PageHeader
                  crumbs={[]}
                  row={undefined}
                  counts={{}}
                  title="ENG-42"
                  workspace="work"
                  {...(star === undefined ? {} : { star })}
                />
              }
            />
          </ClientContext.Provider>
        </Router>,
      );
    };
    const drawn = header();
    expect(screen.getAllByRole("button", { name: "Keep in Starred" }).length).toBeGreaterThan(0);
    drawn.unmount();

    const carried = header(false);
    expect(screen.queryByRole("button", { name: /Starred/ })).toBeNull();
    // NOTHING TO OFFER IS NO MENU: the phone's More has neither the star nor
    // an entry of the screen's.
    expect(carried.container.querySelector(".page-more")).toBeNull();
    location.hash = "#/";
  });
});

// THE TRAIL IS THE KIT'S BREADCRUMB (its floor and its tab stop are held by
// the kit's own suite), built from the route table's crumbs, and Work's last
// crumb is the switcher's button.
describe("the trail", () => {
  const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });

  function frame(children: ReactNode) {
    const store = new Store();
    return (
      <Router>
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <ViewerProvider>
            <ProjectSwitcherProvider>{children}</ProjectSwitcherProvider>
          </ViewerProvider>
        </ClientContext.Provider>
      </Router>
    );
  }

  afterEach(() => {
    location.hash = "#/";
  });

  test("ancestors lead back out and the page's own name is its heading", () => {
    render(
      <Router>
        <Trail
          crumbs={[
            { label: "Live", path: ["live"], icon: "activity" },
            { label: "Turns", path: ["live", "turns"] },
            { label: "Replied in the thread about the observability pipeline." },
          ]}
        />
      </Router>,
    );
    expect(
      within(trail())
        .getAllByRole("link")
        .map((a) => [a.textContent, a.getAttribute("href")]),
    ).toEqual([
      ["Live", "#/live"],
      ["Turns", "#/live/turns"],
    ]);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(
      "Replied in the thread about the observability pipeline.",
    );
  });

  test("a project's crumb carries its key as the kit's chip, and a raw key is mono", () => {
    render(
      <Router>
        <Trail
          crumbs={[
            { label: "Work", path: ["work"], icon: "square-kanban" },
            { label: "Core platform", path: ["work", "ENG"], tag: "ENG" },
            { label: "ENG-42", mono: true },
          ]}
        />
      </Router>,
    );
    const project = within(trail()).getByRole("link", { name: "ENG Core platform" });
    expect(project.querySelector(".crewlet-key-chip")?.textContent).toBe("ENG");
    expect(screen.getByRole("heading", { level: 1 }).classList.contains("is-mono")).toBe(true);
  });

  // THE HEADING IS THE CONTROL: a reader landing on the page's name by
  // heading is on the button that changes it.
  test("Work's scope crumb is the switcher's button, as the page's heading", () => {
    location.hash = "#/work/ENG";
    render(
      frame(
        <Trail
          crumbs={[
            { label: "Work", path: ["work"], icon: "square-kanban" },
            { label: "Core platform", tag: "ENG", switch: "project", archived: true },
          ]}
        />,
      ),
    );
    const heading = screen.getByRole("heading", { level: 1 });
    const button = within(heading).getByRole("button", { name: "ENG Core platform Archived" });
    expect(button.getAttribute("aria-haspopup")).toBe("dialog");
    expect(button.getAttribute("title")).toBe("Switch project (G then P)");
  });

  test("All projects draws its lenses beside the trail, as links, with no star", () => {
    location.hash = "#/work/tasks";
    const work = WORKSPACES.find((w) => w.key === "work")!;
    render(
      frame(
        <AppShell
          topbar={
            <PageHeader
              crumbs={[
                { label: "Work", path: ["work"], icon: "square-kanban" },
                { label: "All projects", switch: "project" },
              ]}
              row={work}
              counts={{}}
              title="All projects"
              workspace="work"
            />
          }
        />,
      ),
    );
    const lenses = screen.getByRole("navigation", { name: "Lens" });
    expect(
      within(lenses)
        .getAllByRole("link")
        .map((a) => [a.textContent?.replace(/\d+/g, "").trim(), a.getAttribute("href")]),
    ).toEqual([
      ["Projects", "#/work"],
      ["Tasks", "#/work/tasks"],
      ["History", "#/work/history"],
    ]);
    expect(within(lenses).getByRole("link", { name: /Tasks/ }).getAttribute("aria-current")).toBe(
      "page",
    );
    // NO SECOND ROW: Work's sections are these lenses, not tabs under the bar.
    expect(document.querySelector(".section-tabs")).toBeNull();
    expect(screen.queryByRole("button", { name: "Keep in Starred" })).toBeNull();
  });
});

// WHO IS WORKING IS HOME'S, the way the Main artboard draws it: the sidebar's
// Live row carries the count on every other page, and the chip drawn in
// every bar sat between a task's trail and its actions and pushed a profile's
// controls past a phone's edge.
describe("who is working", () => {
  function mount(working: boolean | undefined) {
    const store = new Store();
    store.applySeats([
      {
        agent_id: "a1",
        id: "a1",
        role: "Agent SWE",
        activity: "working",
        live_call: { work_item: { key: "ENG-1", project: "ENG" }, versions: ZERO_VERSIONS },
      },
      { agent_id: "a2", id: "a2", role: "SRE", activity: "working" },
      { agent_id: "a4", id: "a4", role: "CTO", activity: "idle" },
    ] as never);
    const socket = new LiveSocket(store);
    return render(
      <Router>
        <ClientContext.Provider value={{ store, socket }}>
          <AppShell
            topbar={
              <PageHeader
                crumbs={[]}
                row={undefined}
                counts={{}}
                title="Somewhere"
                workspace="home"
                {...(working === undefined ? {} : { working })}
              />
            }
          />
        </ClientContext.Provider>
      </Router>,
    );
  }

  test("a page that asks for it counts every working seat", () => {
    const { container } = mount(true);
    expect(container.querySelector(".working-now")?.textContent).toContain("2 agents working");
  });

  test("every other page draws no chip at all", () => {
    expect(mount(undefined).container.querySelector(".working-now")).toBeNull();
    cleanup();
    expect(mount(false).container.querySelector(".working-now")).toBeNull();
  });

  test("nobody working draws nothing, even where it is asked for", () => {
    const store = new Store();
    store.applySeats([{ agent_id: "a4", id: "a4", role: "CTO", activity: "idle" }] as never);
    const { container } = render(
      <Router>
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <WorkingNow />
        </ClientContext.Provider>
      </Router>,
    );
    expect(container.querySelector(".working-now")).toBeNull();
  });

  // THE SEAT'S OWN INITIALS, the ones its crumb and its profile draw: handed
  // the role as written, "Agent SWE" was AS here and SW everywhere else.
  test("a working seat wears the badge it wears everywhere else", () => {
    const { container } = mount(true);
    const faces = [...container.querySelectorAll(".working-now .crewlet-avatar")].map(
      (a) => a.textContent,
    );
    expect(faces).toContain("SW");
    expect(faces).not.toContain("AS");
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

// A TAB'S FIGURE IS A PLAIN NUMBER in the tab's quiet ink, as every approved
// artboard draws it ("Roster 8") — never a pill, which read as a second
// control inside each tab. And a figure nobody published is not drawn.
test("a section's figure is a plain number beside its name, and only if published", () => {
  const agents = WORKSPACES.find((w) => w.key === "agents")!;
  render(
    <Router>
      <SectionTabs
        row={agents}
        path={["agents"]}
        query={new URLSearchParams()}
        counts={{ roster: "8", schedules: "6" }}
      />
    </Router>,
  );
  const roster = screen.getByRole("link", { name: /Roster/ });
  const figure = roster.querySelector(".section-tab-count");
  expect(figure?.textContent).toBe("8");
  expect(roster.querySelector(".count-chip")).toBeNull();
  expect(screen.getByRole("link", { name: /Schedules/ }).textContent).toContain("6");
  expect(
    screen.getByRole("link", { name: /Teams/ }).querySelector(".section-tab-count"),
  ).toBeNull();
});

// THE READING NOTE GIVES WAY TO THE TABS: a sentence about how to read a
// workspace takes no room a place would need, so it is drawn only where every
// tab fits beside it — and a note the sheet draws at no width has nothing to
// fit.
describe("the reading note at the strip's end", () => {
  const widths = [100, 100, 100];
  test("is drawn where every tab and the note fit", () => {
    // 3 tabs and 2 gaps are 320; a gap and a 200 note make 530.
    expect(noteFits({ widths, space: 530, gap: 10, note: 200 })).toBe(true);
  });
  test("gives way before any tab would fold", () => {
    expect(noteFits({ widths, space: 529, gap: 10, note: 200 })).toBe(false);
  });
  test("a note drawn at no width is not drawn", () => {
    expect(noteFits({ widths, space: 2000, gap: 10, note: 0 })).toBe(false);
  });
});
