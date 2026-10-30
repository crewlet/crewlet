/**
 * The page header: its trail, its star, its section tabs, and who is working
 * now. A page carries no Copy link: the command palette's "Copy link to this
 * page" is the one place a page's address is copied from (`lib/pageLink.ts`).
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { AppShell } from "@crewlethq/ui";

import {
  PageHeader,
  PageMore,
  SectionColumn,
  SectionTabs,
  StarPage,
  Trail,
  WorkingNow,
} from "./PageHeader.tsx";
import { ProjectSwitcherProvider } from "./ProjectSwitcher.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WORKSPACES } from "~/app/nav.ts";
import { Router } from "~/app/router.tsx";
import { noteReader } from "~/lib/reader.ts";
import { starsKey } from "~/lib/starred.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { installWindow } from "~/testing.tsx";

/** A company running every service built into Crewlet. */
const EVERY = { tracker: true, knowledge: true, chat: true } as const;

afterEach(cleanup);

// A STAR IS A SHORTCUT TO A PAGE, so it is offered on a page and nowhere else.
describe("the star", () => {
  // STARS ARE KEPT PER READER, so these cases are read by somebody.
  beforeEach(() => noteReader("p-1"));
  afterEach(() => sessionStorage.clear());

  test("is offered on an object's page", () => {
    render(<StarPage path={["worklet", "ENG-42"]} label="ENG-42" workspace="worklet" />);
    expect(screen.getByRole("button", { name: "Keep in Starred" })).toBeTruthy();
  });

  // NOR BY NOBODY: a tab that has not learned who reads it has no list to
  // keep a star in, and one kept in a list nobody owns would be offered to
  // whoever signs in next.
  test("is not offered to a tab nobody is reading yet", () => {
    sessionStorage.clear();
    render(<StarPage path={["worklet", "ENG-42"]} label="ENG-42" workspace="worklet" />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  // The store drops a kept path that does not resolve, so a star pressed on
  // Not Found filled and then vanished on the next read, with nothing said.
  test("is not offered on an address that is no page", () => {
    render(<StarPage path={["worklet", "projects"]} label="Not found" workspace="worklet" />);
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

  // "ALREADY IN THE SIDEBAR" IS A FACT ABOUT THE SIDEBAR, never about the
  // path's length: every row's landing — the empty fragment being Home's — is
  // one, and the reader's Account, one segment long and with no row at all,
  // is not. Its star was disabled with that reason on the one page a reader
  // looking for where Account lives most needs a way to keep.
  test("is unavailable on exactly the addresses that are sidebar rows", () => {
    const rows = WORKSPACES.filter((w) => w.place !== "menu");
    expect(rows.length).toBeGreaterThan(5);
    for (const path of [[], ...rows.map((w) => w.path)]) {
      render(<StarPage path={path} label="Row" workspace="home" />);
      const star = screen.getByRole("button", { name: "Keep in Starred" });
      expect(star.getAttribute("aria-disabled"), `#/${path.join("/")}`).toBe("true");
      cleanup();
    }
  });

  test("keeps the reader's Account, which has no row, like any other page", () => {
    render(<StarPage path={["account"]} label="Account" workspace="account" />);
    const star = screen.getByRole("button", { name: "Keep in Starred" });
    expect(star.getAttribute("aria-disabled")).toBeNull();
    expect(screen.queryByText("Already in the sidebar")).toBeNull();
    fireEvent.click(star);
    expect(localStorage.getItem(starsKey("p-1"))).toContain('"account"');
    expect(screen.getByRole("button", { name: "Remove from Starred" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove from Starred" }));
  });

  // THE PHONE'S MORE ASKS THE SAME QUESTION, so the two can never disagree.
  test("the phone's More offers the Account's star, and a row's unavailable", () => {
    const more = (path: string[]) => {
      render(<PageMore path={path} label="Here" workspace="account" entries={[]} />);
      fireEvent.click(screen.getByRole("button", { name: "More on this page" }));
      const item = screen.getByRole("menuitem", { name: /Keep in Starred/ });
      const disabled = item.getAttribute("aria-disabled") === "true";
      cleanup();
      return disabled;
    };
    expect(more(["account"])).toBe(false);
    expect(more(["settings", "models"])).toBe(true);
  });

  // A SCREEN WHOSE OWN MENU CARRIES THE STAR (a knowledge page's More) gets
  // none from the frame, inline or in the phone's More: one control per
  // action on screen.
  test("is drawn by the frame unless the screen's own menu carries it", () => {
    location.hash = "#/worklet/ENG-42";
    const header = (star?: boolean) => {
      const store = new Store();
      const socket = new LiveSocket(store);
      return render(
        <Router>
          <ClientContext.Provider value={{ store, socket }}>
            <AppShell
              topbar={
                <PageHeader
                  runs={EVERY}
                  grants={null}
                  crumbs={[]}
                  row={undefined}
                  counts={{}}
                  title="ENG-42"
                  workspace="worklet"
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
            { label: "Worklet", path: ["worklet"], icon: "square-kanban" },
            { label: "Core platform", path: ["worklet", "ENG"], tag: "ENG" },
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
    location.hash = "#/worklet/ENG";
    render(
      frame(
        <Trail
          crumbs={[
            { label: "Worklet", path: ["worklet"], icon: "square-kanban" },
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
    location.hash = "#/worklet/tasks";
    const work = WORKSPACES.find((w) => w.key === "worklet")!;
    render(
      frame(
        <AppShell
          topbar={
            <PageHeader
              runs={EVERY}
              grants={null}
              crumbs={[
                { label: "Worklet", path: ["worklet"], icon: "square-kanban" },
                { label: "All projects", switch: "project" },
              ]}
              row={work}
              counts={{}}
              title="All projects"
              workspace="worklet"
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
      ["Projects", "#/worklet"],
      ["Tasks", "#/worklet/tasks"],
      ["History", "#/worklet/history"],
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
                runs={EVERY}
                grants={null}
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
// THE ARITHMETIC IS THE KIT'S (uilet's Tabs, `overflow="menu"`), and its suite
// holds the order a tab leaves in. What stays here is that this strip folds
// by it, its "More" names the workspace and navigates, and a folded link is
// out of reach.
describe("the section tabs fold what does not fit into More", () => {
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
          : this.hasAttribute("data-tabs-more")
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
            runs={EVERY}
            grants={null}
          />
        </Router>,
      );
    }
    const strip = () => screen.getByRole("navigation", { name: "My work sections" });
    // THE TABS ON THE STRIP, by section: a folded one stays in the document,
    // out of the flow, so it is left out here.
    const drawn = () =>
      [...strip().querySelectorAll("a[data-section]:not(.is-folded)")].map((a) =>
        a.getAttribute("data-section"),
      );

    // 800 of room: More (70) and six tabs (720) is 790; seven tabs alone are
    // 840.
    test("seven tabs in 800 of room draw six and fold the last into More", async () => {
      lay(800);
      mount(["me"]);
      await waitFor(() => expect(drawn()).toHaveLength(6));
      expect(drawn().at(-1)).toBe("watching");
      // THE FOLDED TAB IS OUT OF REACH on the strip, and in the menu instead.
      const folded = strip().querySelector("a.is-folded")!;
      expect(folded.getAttribute("aria-hidden")).toBe("true");
      expect(screen.queryByRole("link", { name: /Checklist/ })).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "More sections of My work" }));
      const item = await screen.findByRole("menuitem", { name: /Checklist/ });
      expect(item.textContent).toContain("0");
      fireEvent.click(item);
      await waitFor(() => expect(location.hash).toBe("#/me/checklist"));
    });

    test("on a folded section, its own tab is drawn in the last place that fits", async () => {
      lay(800);
      mount(["me", "checklist"]);
      await waitFor(() => expect(drawn()).toHaveLength(6));
      expect(drawn().at(-1)).toBe("checklist");
      expect(drawn()).not.toContain("watching");
    });

    test("a strip with room for every tab draws no More", async () => {
      lay(2000);
      mount(["me"]);
      await waitFor(() => expect(drawn()).toHaveLength(7));
      expect(screen.queryByRole("button", { name: /^More sections of / })).toBeNull();
    });
  });
});

// A TAB'S FIGURE IS THE KIT'S COUNT beside its name, as an object's own tabs
// draw theirs, so the two rows of tabs in the product read one way. And a
// figure nobody published is not drawn.
test("a section's figure rides beside its name, and only if published", () => {
  const org = WORKSPACES.find((w) => w.key === "org")!;
  render(
    <Router>
      <SectionTabs
        row={org}
        path={["company", "org"]}
        query={new URLSearchParams()}
        counts={{ roster: "8", teams: "6" }}
        runs={EVERY}
        grants={null}
      />
    </Router>,
  );
  const roster = screen.getByRole("link", { name: /Roster/ });
  expect(roster.textContent).toBe("Roster8");
  expect(screen.getByRole("link", { name: /Teams/ }).textContent).toBe("Teams6");
  expect(screen.getByRole("link", { name: /Chart/ }).textContent).toBe("Chart");
});

// ON A PHONE SETTINGS' COLUMN IS A PICKER, and whether it is open says whether
// the reader has chosen a section. Folded on every arrival, Settings' own
// address showed a phone reader one row naming its first section, and its
// four groups only behind a tap nothing told them to make. A deep link still
// arrives folded, since the section it names starts right under the picker,
// and any pick folds it. Mutation: start folded everywhere, or
// leave the list open after a pick that moves no path, and a case goes red.
describe("Settings' section picker on a phone", () => {
  const settings = WORKSPACES.find((w) => w.key === "settings")!;
  let window: ReturnType<typeof installWindow>;
  beforeEach(() => {
    window = installWindow(390);
  });
  afterEach(() => window.restore());

  const column = (path: string[]) => (
    <SectionColumn
      row={settings}
      path={path}
      query={new URLSearchParams()}
      grants={null}
      figures={{}}
      runs={EVERY}
    />
  );
  const toggle = () => document.querySelector<HTMLButtonElement>(".crewlet-section-frame__toggle")!;
  const list = () => document.getElementById(toggle().getAttribute("aria-controls") ?? "")!;
  const row = (at: string) => list().querySelector<HTMLAnchorElement>(`a[href="${at}"]`)!;
  /** What the toggle SHOWS, its screen-reader words left out. */
  const shown = () => {
    const copy = toggle().cloneNode(true) as HTMLElement;
    copy.querySelectorAll(".crewlet-visually-hidden").forEach((n) => n.remove());
    return copy.textContent?.replace(/\s+/g, " ").trim();
  };

  test("arriving at Settings' own address opens the list, every group in it", () => {
    render(column(["settings", "models"]));
    expect(toggle().getAttribute("aria-expanded")).toBe("true");
    expect(toggle().closest(".crewlet-section-frame__nav")?.hasAttribute("data-open")).toBe(true);
    expect(row("#/settings/spend")).not.toBeNull();
    expect(row("#/settings/storage")).not.toBeNull();
  });

  test("a link to a section arrives folded, and says it is a picker", () => {
    render(column(["settings", "people"]));
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
    expect(shown()).toBe("Section · People");
  });

  test("a pick folds it, the section the reader is already on included", () => {
    render(column(["settings", "models"]));
    fireEvent.click(row("#/settings/models"));
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
  });

  test("a pick that lands on Settings' own address leaves it folded; an arrival nobody picked opens it", () => {
    const view = render(column(["settings", "people"]));
    fireEvent.click(row("#/settings/models"));
    view.rerender(column(["settings", "models"]));
    expect(toggle().getAttribute("aria-expanded"), "a pick of Models & keys").toBe("false");
    view.rerender(column(["settings", "nodes"]));
    expect(toggle().getAttribute("aria-expanded"), "a link to Nodes").toBe("false");
    // THE SIDEBAR'S SETTINGS ROW, or Back: no pick in this list made it.
    view.rerender(column(["settings", "models"]));
    expect(toggle().getAttribute("aria-expanded"), "an arrival at Settings").toBe("true");
  });

  // A SECTION READING A SERVICE THE COMPANY DOES NOT RUN IS NOT DRAWN: Expensive
  // tasks names its tasks by Worklet's keys, so where the company runs
  // none the Spend group is the readings alone. Mutation: draw the column's
  // sections unfiltered and the row is there.
  test("a section reading a service the company does not run is not drawn", () => {
    render(
      <SectionColumn
        row={settings}
        path={["settings", "spend"]}
        query={new URLSearchParams()}
        grants={null}
        figures={{}}
        runs={{ tracker: false, knowledge: true, chat: true }}
      />,
    );
    expect(row("#/settings/spend")).not.toBeNull();
    expect(row("#/settings/spend/tasks")).toBeNull();
  });

  test("beside a section there is room for the list, and nothing is opened for the reader", () => {
    window.restore();
    window = installWindow(1440);
    render(column(["settings", "models"]));
    expect(toggle().getAttribute("aria-expanded")).toBe("false");
  });
});

// A TAB THE READER CANNOT OPEN IS LOCKED, as the sidebar's rows and the
// Settings column's are: Live's Turns, Agent-to-agent and Event log looked
// open to a Worklet-only reader and then refused. Mutation: drop the grants
// from the tabs and no tab names what it needs.
test("a section tab the reader cannot open names the access it needs", () => {
  const live = WORKSPACES.find((w) => w.key === "live")!;
  const locked = live.sections.filter((s) => s.grants && !s.grants.includes("worklet"));
  expect(locked.length).toBeGreaterThan(0);
  render(
    <Router>
      <SectionTabs
        row={live}
        path={live.sections[0]!.path}
        query={new URLSearchParams()}
        counts={{}}
        runs={EVERY}
        grants={["worklet"]}
      />
    </Router>,
  );
  for (const s of locked) {
    const tab = document.querySelector(`[data-section="${s.key}"]`)!;
    expect(tab.textContent).toMatch(/Needs Admin access/);
  }
  const open = document.querySelector(`[data-section="${live.sections[0]!.key}"]`)!;
  expect(open.textContent).not.toMatch(/needs/);
});
