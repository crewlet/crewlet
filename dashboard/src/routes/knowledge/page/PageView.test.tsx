/**
 * One knowledge page, whole: its properties, its outline, its connections
 * (every number the engine's), the watch, the star and History in its bar,
 * and the edit a person makes on it as themselves, including the one that
 * races somebody else's save.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { PageView } from "./PageView.tsx";
import { withoutRepeatedTitle } from "./body.ts";
import { linkPageCommand } from "./LinkPicker.tsx";
import { followersOf } from "./Watch.tsx";
import { Composer } from "../Composer.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { crumbsFor, pageSpaceKey, pageUpKey } from "~/app/crumbs.ts";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { noteReader } from "~/lib/reader.ts";
import { resetForTest as resetStars, useStarred } from "~/lib/starred.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { PAGE_TITLE_MAX_BYTES, type PageReadsAnswer } from "~/contract/pages.ts";
import type { PageDetail } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { loadRichEditor, readOnly, retype } from "~/test/richEditor.ts";
import { afterMicrotasks } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// 09:00 on the 25th in Tokyo, still the 24th in UTC.
const NOW = Date.parse("2026-09-25T00:00:00Z");
const ID = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a";

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "knowledge"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["save_page", "comment_on_page", "write_page"],
};

const ORG = {
  name: "Acme",
  timezone: "Asia/Tokyo",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "SWE", handle: "swe", kind: "agent" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
};

const detail = (
  over: Partial<PageDetail> = {},
  page: Record<string, unknown> = {},
): PageDetail => ({
  page: {
    id: ID,
    container: "ENG",
    title: "Provisioner runbook",
    status: "published",
    version: 14,
    author: "jane",
    updated_at: "2026-09-24T22:00:00Z",
    revision: 40,
    watchers: ["jane", "swe"],
    body: "# Provisioner runbook\n\n## When a host fails\n\nCheck the switch.\n\n## Escalation\n\nPage the CTO.",
    ...page,
  } as PageDetail["page"],
  revision: 40,
  skill: false,
  onboarding: false,
  history: [
    {
      version: 14,
      author: "swe",
      created_at: "2026-09-24T22:00:00Z",
      message: "Name the switch",
    },
    { version: 13, author: "cto", created_at: "2026-09-20T10:00:00Z" },
  ],
  ancestors: [{ id: "p-0", container: "ENG", title: "Runbooks" } as never],
  children: [],
  children_total: 0,
  ...over,
});

const reads = (over: Partial<PageReadsAnswer> = {}): PageReadsAnswer => ({
  page: ID,
  since: "2026-08-27",
  until: "2026-09-25",
  days: 30,
  readers: [
    {
      handle: "swe",
      via: "get_page",
      count: 1,
      last_at: "2026-09-24T16:00:00Z",
      last_turn_id: "run-2",
      last_work_item: { id: "t-1", key: "ENG-412", title: "Retry PXE boot", ordinal: 2 },
    },
    {
      handle: "cto",
      via: "search",
      count: 3,
      last_at: "2026-09-24T14:00:00Z",
      last_query: "DHCP lease",
    },
  ],
  readers_total: 2,
  distinct_seats_today: 3,
  elided: 0,
  ...over,
});

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.spyOn(Date, "now").mockReturnValue(NOW);
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_PAGES_LOG@1:99",
          receipt: { id: "f0f0f0f0-1111-4222-8333-444455556666" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  resetStars();
  reloadForTest();
  location.hash = "#/";
});

function mount(children: ReactNode, answers: Record<string, Answer>) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = {
      viewer: JANE,
      inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
      decisions: { handle: "jane", items: [], total: 0, capped: false },
      containers: { containers: [{ key: "ENG", name: "Engineering", pages: 3 }] },
      page_activity: { changes: [] },
      ...answers,
    };
    if (what in all) {
      const answer = all[what];
      return typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => Promise<unknown>)(params ?? {})
        : Promise.resolve(answer);
    }
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { asked };
}

async function settle() {
  await afterMicrotasks();
}

const region = (name: string | RegExp) => screen.getByRole("region", { name });

// A PAGE IS ADDRESSED BY ITS ID, which a rename does not move.
test("the page is read by its id, and its readers by the same id", async () => {
  const { asked } = mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(asked.find((a) => a.kind === "page")?.params).toEqual({ id: ID });
  expect(asked.find((a) => a.kind === "page_reads")?.params).toEqual({ page: ID });
});

// THE PROPERTIES ARE MINIMAL: where, who wrote it, who last saved it and when,
// with the revision and its note on the save's own line.
test("the properties name the seat the author wrote as, and the last save with its revision", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const props = document.querySelector(".knowledge-props")!;
  expect(
    within(props as HTMLElement)
      .getByRole("link", { name: "Engineering" })
      .getAttribute("href"),
  ).toBe("#/knowledge/ENG");
  expect(
    within(props as HTMLElement)
      .getByRole("link", { name: "Runbooks" })
      .getAttribute("href"),
  ).toBe("#/knowledge/pages/p-0");
  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person writes AS their seat,
  // drawn by the chart's name for it rather than the handle.
  expect(props.textContent).toContain("Jane Founder");
  expect(props.textContent).not.toMatch(/\bjane\b/);
  const saved = within(props as HTMLElement).getByTitle("Revision 14: “Name the switch”");
  expect(saved.textContent).toMatch(/^2h ago by SWE/);
  expect(props.textContent).not.toContain("Kind");
});

test("the kind is said where the page is not plain prose, and never in a state's tone", async () => {
  const loads = [
    { handle: "swe", last_at: "2026-09-24T16:00:00Z", count: 7, loaded: 2, offered: 5 },
    { handle: "cto", last_at: "2026-09-24T10:00:00Z", count: 1, loaded: 0, offered: 1 },
  ];
  mount(<PageView id={ID} />, {
    page: detail({ skill: true, onboarding: true, skill_loaded_by: loads }),
    page_reads: reads(),
  });
  await settle();
  const props = document.querySelector(".knowledge-props")!;
  expect(props.textContent).toContain(
    "Onboarding, read first by every new seat · Tool skill, loaded as a skill by SWE in 30 days",
  );
  cleanup();

  // AN EMPTY LIST is the engine saying nobody loaded it; an ABSENT one is a
  // node that does not read the usage domain, which is not the same fact.
  mount(<PageView id={ID} />, {
    page: detail({ skill: true, skill_loaded_by: [] }),
    page_reads: reads(),
  });
  await settle();
  expect(document.querySelector(".knowledge-props")!.textContent).toContain(
    "Tool skill, not loaded in 30 days",
  );
  cleanup();

  mount(<PageView id={ID} />, { page: detail({ skill: true }), page_reads: reads() });
  await settle();
  expect(document.querySelector(".knowledge-props")!.textContent).toMatch(/KindTool skill$/);
});

test("each reader says how it read the page and in which turn, over a named window", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const readers = region("Read by agents");
  expect(within(readers).getByText("last 30 days")).toBeTruthy();
  expect(
    within(readers).getByRole("link", { name: "turn 2 on ENG-412" }).getAttribute("href"),
  ).toBe("#/live/turns/run-2");
  expect(within(readers).getByText(/search: “DHCP lease”/)).toBeTruthy();
  // TWO LINES, NEVER THREE: the count rides the name's line, and the age is
  // held apart from the way-read text so an ellipsis cuts the query.
  const count = within(readers).getByText("3 reads");
  expect(count.closest(".knowledge-reader-head")).not.toBeNull();
  const how = within(readers).getByText(/search: “DHCP lease”/);
  expect(how.classList.contains("knowledge-reader-how")).toBe(true);
  expect(how.getAttribute("title")).toBe(how.textContent);
  const when = how.parentElement?.querySelector(".knowledge-reader-when");
  expect(when?.querySelector("time")).not.toBeNull();
  expect(how.contains(when ?? null)).toBe(false);
});

test("a long list's Show more says what it expands and whether it has", async () => {
  const many = Array.from({ length: 7 }, (_, i) => ({
    handle: `seat-${i}`,
    via: "get_page",
    count: 1,
    last_at: "2026-09-24T16:00:00Z",
  }));
  mount(<PageView id={ID} />, {
    page: detail(),
    page_reads: reads({ readers: many, readers_total: 7 }),
  });
  await settle();
  const readers = region("Read by agents");
  const more = within(readers).getByRole("button", { name: "Show 4 more" });
  expect(more.getAttribute("aria-expanded")).toBe("false");
  const list = document.getElementById(more.getAttribute("aria-controls") ?? "");
  expect(list?.tagName).toBe("UL");
  expect(within(list!).getAllByRole("listitem")).toHaveLength(3);
  fireEvent.click(more);
  expect(more.getAttribute("aria-expanded")).toBe("true");
  expect(within(list!).getAllByRole("listitem")).toHaveLength(7);
});

// A SERVER WITHOUT THE USAGE DOMAIN DOES NOT SAY "NOBODY READ IT".
test("a node that cannot say who read the page draws no readers card", async () => {
  mount(<PageView id={ID} />, {
    page: detail(),
    page_reads: () => Promise.reject(new Error("unknown_query")),
  });
  await settle();
  expect(screen.queryByRole("region", { name: "Read by agents" })).toBeNull();
  expect(screen.queryByText(/No agent has read it/)).toBeNull();
});

test("linked from lists the tasks and the pages that point here", async () => {
  mount(<PageView id={ID} />, {
    page: detail({
      linked_from: {
        tasks: [
          {
            id: "t-1",
            key: "ENG-412",
            title: "Retry PXE boot",
            status: "in_progress",
            via: ["linked_page"],
          },
        ],
        pages: [{ id: "p-2", container: "ENG", title: "Scheduler on-call" }],
        tasks_total: 1,
        pages_total: 1,
      },
    }),
    page_reads: reads(),
  });
  await settle();
  const links = region("Linked from");
  expect(
    within(links)
      .getByRole("link", { name: /ENG-412/ })
      .getAttribute("href"),
  ).toBe("#/work/ENG-412");
  expect(within(links).getByRole("link", { name: "Scheduler on-call" }).getAttribute("href")).toBe(
    "#/knowledge/pages/p-2",
  );
});

// AN INDEX STILL ON ITS FIRST PASS IS NOT "NOTHING LINKS HERE".
test("a node still building its index says so rather than that nothing links here", async () => {
  mount(<PageView id={ID} />, {
    page: detail({ linked_from_status: "building" }),
    page_reads: reads(),
  });
  await settle();
  const links = region("Linked from");
  expect(within(links).getByText("Still indexing. Look again in a few minutes.")).toBeTruthy();
  expect(within(links).queryByText(/No page or task links here/)).toBeNull();
  cleanup();

  mount(<PageView id={ID} />, {
    page: detail({ linked_from_status: "unavailable" }),
    page_reads: reads(),
  });
  await settle();
  expect(
    within(region("Linked from")).getByText("Couldn’t load links. Reload to try again."),
  ).toBeTruthy();
  cleanup();

  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.queryByRole("region", { name: "Linked from" })).toBeNull();
});

test("dropped reads are named as possibly missing, never as this page's", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads({ elided: 4 }) });
  await settle();
  expect(within(region("Read by agents")).getByText("Some reads may be missing.")).toBeTruthy();
});

test("the outline lists the sections, and a body's first line that repeats the title is not drawn", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const toc = screen.getByRole("navigation", { name: "On this page" });
  expect(
    within(toc)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["When a host fails", "Escalation"]);
  // The title is the page's h1 and nowhere else.
  expect(screen.getAllByText("Provisioner runbook", { selector: "h1, h2, h3" })).toHaveLength(1);
});

// A FOLDER IS A PAGE WITH NOTHING WRITTEN IN IT: it says what it groups.
test("a folder says it groups pages, and lists what is filed under it", async () => {
  mount(<PageView id={ID} />, {
    page: detail(
      {
        folder: true,
        children: [{ id: "p-7", container: "ENG", title: "FAIR-2 Money" } as never],
        children_total: 1,
      },
      { body: "" },
    ),
    page_reads: reads(),
  });
  await settle();
  expect(screen.getByText(/A folder that groups pages/)).toBeTruthy();
  const article = screen.getByRole("article", { name: "Provisioner runbook" });
  expect(
    within(article).getAllByRole("link", { name: "FAIR-2 Money" })[0]?.getAttribute("href"),
  ).toBe("#/knowledge/pages/p-7");
});

// HISTORY SWAPS THE OUTLINE FOR THE REVISIONS and the thread for the changes.
test("History reads the newest revision, lists the others, and shows the page's changes", async () => {
  location.hash = `#/knowledge/pages/${ID}`;
  mount(<PageView id={ID} />, {
    page: detail(),
    page_reads: reads(),
    page_revision: (p: Record<string, unknown>) =>
      Promise.resolve({ page: ID, version: p.version, body: `body of ${String(p.version)}` }),
    page_activity: {
      changes: [
        {
          id: "c-1",
          page_id: ID,
          title: "Provisioner runbook",
          kind: "saved",
          actor: "jane",
          actor_kind: "human",
          excerpt: "Adds the drain step",
          at: "2026-09-24T22:00:00Z",
        },
        // A CREDENTIAL BOUND TO NO SEAT writes under its login, a name.
        {
          id: "c-2",
          page_id: ID,
          kind: "renamed",
          actor: "ci.docs",
          actor_kind: "operator",
          at: "2026-09-24T21:00:00Z",
        },
        // THE ENGINE'S OWN CHANGE names nobody.
        {
          id: "c-3",
          page_id: ID,
          kind: "saved",
          actor: "crewlet",
          actor_kind: "system",
          at: "2026-09-24T20:00:00Z",
        },
        // A NEW PAGE'S EXCERPT IS ITS FIRST LINE, here the title's heading.
        {
          id: "c-0",
          page_id: ID,
          title: "Provisioner runbook",
          kind: "created",
          actor: "swe",
          excerpt: "Provisioner runbook",
          at: "2026-09-20T09:00:00Z",
        },
      ],
    },
  });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "History" }));
  await settle();
  const revisions = screen.getByRole("navigation", { name: "Revisions" });
  const pressed = within(revisions)
    .getAllByRole("button")
    .map((b) => [b.textContent?.replace(/\s+/g, " "), b.getAttribute("aria-pressed")]);
  expect(pressed[0]?.[1]).toBe("true");
  expect(pressed[0]?.[0]).toMatch(/rev 14 · SWE/);
  expect(screen.queryByRole("navigation", { name: "On this page" })).toBeNull();
  expect(screen.queryByRole("region", { name: /^Comments/ })).toBeNull();
  const changes = region(/^Changes/);
  expect(within(changes).getByText("Jane Founder")).toBeTruthy();
  expect(within(changes).getByText("ci.docs")).toBeTruthy();
  expect(within(changes).getByText("The engine")).toBeTruthy();
  expect(changes.textContent).not.toContain("crewlet");
  expect(changes.textContent).toContain("saved it");
  // A SAVE'S NOTE IS DRAWN; a new page's first line that only repeats the
  // title is not, since the title heads the page this list is on.
  const notes = [...changes.querySelectorAll(".knowledge-change-note")].map((n) => n.textContent);
  expect(notes).toEqual(["Adds the drain step"]);
});

// THE WATCH IS THE PERSON'S OWN, as a save of nothing but the watch.
test("the eye watches and unwatches as the person, and a mute is not a watch", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const eye = screen.getByRole("button", { name: "Stop watching this page" });
  expect(eye.getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(eye);
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, watch: false } },
  ]);
  cleanup();

  posted = [];
  mount(<PageView id={ID} />, {
    page: detail({}, { watchers: ["jane", "swe"], muted: ["jane"] }),
    page_reads: reads(),
  });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Watch this page" }));
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, watch: true } },
  ]);
  expect(followersOf({ watchers: ["jane", "swe"], muted: ["jane"] })).toEqual(["swe"]);
});

// A WATCH IS FILED UNDER A SEAT, and a credential bound to none subscribes
// nobody (`internal/pages` `Actor.subscriber`): the eye says so rather than
// offering a press the engine answers by moving nothing.
test("the eye is held, with its reason, for a credential bound to no seat", async () => {
  const unbound = {
    login: "svc:docs",
    grants: ["knowledge"],
    handle: "",
    owner: "svc:docs",
    name: "",
    kind: "",
    acts: ["save_page"],
  };
  mount(<PageView id={ID} />, {
    viewer: unbound,
    page: detail({}, { watchers: ["svc:docs", "swe"] }),
    page_reads: reads(),
  });
  await settle();
  const eye = screen.getByRole("button", { name: "Watch this page" });
  expect(eye.getAttribute("aria-pressed")).toBe("false");
  expect(eye.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(eye);
  await settle();
  expect(posted).toEqual([]);
});

function Stars() {
  const stars = useStarred();
  return <output aria-label="stars">{stars.map((s) => s.path.join("/")).join(",")}</output>;
}

// THE STAR IS IN THE PAGE'S MORE MENU, beside its other page actions.
test("More stars the page, and offers the sub-page, History and the address", async () => {
  noteReader("p-1");
  mount(
    <>
      <PageView id={ID} />
      <Stars />
    </>,
    { page: detail(), page_reads: reads() },
  );
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More for “Provisioner runbook”" }));
  const menu = await screen.findByRole("menu");
  expect(
    within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent?.replace(/\s+/g, " ").trim()),
  ).toEqual([
    "Star this pagePinned on Knowledge and kept in the sidebar",
    "New sub-page",
    "History",
    "Copy the page's address",
  ]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: /Star this page/ }));
  await settle();
  expect(screen.getByRole("status", { name: "stars" }).textContent).toBe(`knowledge/pages/${ID}`);
});

// NOR BY NOBODY: stars are kept per reader, so a tab that has not learned who
// reads it offers no star (a star kept in a list nobody owns would be offered
// to whoever signs in next), and the rest of More is unchanged.
test("More offers no star on a tab no reader has claimed", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More for “Provisioner runbook”" }));
  const menu = await screen.findByRole("menu");
  expect(within(menu).queryByRole("menuitem", { name: /Star this page/ })).toBeNull();
  expect(within(menu).getByRole("menuitem", { name: "New sub-page" })).toBeTruthy();
});

// THE CONFLICT FLOW. The page is polled while it is edited; a save that lands
// meanwhile is SEEN before the person presses Save, and Save waits until they
// have chosen to keep editing on top of it, which MERGES their change into
// the draft before the base moves.
function racing(bodies: Record<number, string>) {
  let version = 14;
  location.hash = `#/knowledge/pages/${ID}?edit=1`;
  mount(<PageView id={ID} />, {
    page: () => Promise.resolve(detail({}, { version, body: bodies[version] })),
    page_reads: reads(),
  });
  return {
    save: async (next: number) => {
      version = next;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
    },
  };
}

const box = () => screen.getByRole("textbox", { name: /The body of/ });

beforeAll(() => loadRichEditor());

test("keeping on top of somebody else's save keeps their change in the draft", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  const race = racing({
    14: "## Steps\n\nold\n\n## Escalation\n\npage the CTO",
    15: "## Steps\n\nold\n\n## Escalation\n\npage the CTO\n\nMaya added this line.",
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  await retype(box(), "old", "mine");

  // Somebody saves revision 15, and the page's own poll brings it.
  await race.save(15);
  expect(screen.getByText(/Somebody saved revision 15 while you edited/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  // The shortcut goes through the same gate as the button.
  fireEvent.keyDown(box(), { key: "Enter", ctrlKey: true });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([]);

  fireEvent.click(screen.getByRole("button", { name: "View their change" }));
  expect(screen.getByText("Maya added this line.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep editing on top of it" }));
  // Their line is in the draft the writer is looking at, beside theirs.
  expect(within(box()).getByText("Maya added this line.")).toBeTruthy();
  expect(within(box()).getByText("mine")).toBeTruthy();
  expect(screen.getByText("Revision 15 is merged into your draft.").getAttribute("role")).toBe(
    "status",
  );
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([
    {
      tool: "save_page",
      args: {
        page: ID,
        base_version: 15,
        body: "## Steps\n\nmine\n\n## Escalation\n\npage the CTO\n\nMaya added this line.",
      },
    },
  ]);
  vi.useRealTimers();
});

// WHERE BOTH EDITS CHANGED THE SAME LINES the browser picks nobody.
test("an overlapping change is settled by the person before the base moves", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  const race = racing({ 14: "## Steps\n\nold", 15: "## Steps\n\ntheirs" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  await retype(box(), "old", "mine");
  await race.save(15);

  fireEvent.click(screen.getByRole("button", { name: "Keep editing on top of it" }));
  const resolve = screen.getByRole("region", { name: /changed the same lines/ });
  expect(within(resolve).getByText("theirs")).toBeTruthy();
  expect(within(resolve).getByText("mine")).toBeTruthy();
  // Nothing is pre-chosen, and the draft is held while the clash is open.
  const apply = within(resolve).getByRole("button", { name: "Apply to my draft" });
  expect(apply.getAttribute("aria-disabled")).toBe("true");
  expect(readOnly(box())).toBe(true);
  expect(screen.getByText(/Saved as you, against revision 14/)).toBeTruthy();

  fireEvent.click(within(resolve).getByRole("radio", { name: "Both" }));
  fireEvent.click(apply);
  expect(readOnly(box())).toBe(false);
  expect(screen.getByText(/Saved as you, against revision 15/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 15, body: "## Steps\n\nmine\ntheirs" } },
  ]);
  vi.useRealTimers();
});

test("a comment is written as the signed-in person, and drawn as the seat they wrote as", async () => {
  mount(<PageView id={ID} />, {
    page: detail({
      comments: [
        {
          id: "c-1",
          author: "jane",
          author_kind: "human",
          body: "Is step 2 still right?",
          created_at: "2026-09-24T20:00:00Z",
        } as never,
      ],
    }),
    page_reads: reads(),
  });
  await settle();
  const thread = region(/^Comments/);
  expect(within(thread).getByText("Jane Founder")).toBeTruthy();
  fireEvent.change(screen.getByRole("textbox", { name: /Comment on/ }), {
    target: { value: "Yes, I checked it." },
  });
  fireEvent.click(screen.getByRole("button", { name: "Comment" }));
  await settle();
  expect(posted).toEqual([
    { tool: "comment_on_page", args: { page: ID, body: "Yes, I checked it." } },
  ]);
});

// THE TRAIL IS THE PAGE'S PLACE: the space and the pages above it.
test("the trail names the space and every page above this one", () => {
  const crumbs = crumbsFor(["knowledge", "pages", ID], {
    [ID]: "Provisioner runbook",
    [pageSpaceKey(ID)]: "ENG",
    ENG: "Engineering",
    [pageUpKey(ID, 0)]: "p-0",
    "p-0": "Runbooks",
  });
  expect(crumbs.map((c) => c.label)).toEqual([
    "Knowledge",
    "Engineering",
    "Runbooks",
    "Provisioner runbook",
  ]);
  expect(crumbs[1]?.path).toEqual(["knowledge", "ENG"]);
  expect(crumbs[2]?.path).toEqual(["knowledge", "pages", "p-0"]);
});

// A LINK THE EDITOR MAKES IS THE ADDRESS THE BACKLINKS READ.
test("Link to a page links the page's id address, which a rename does not break", async () => {
  const insertLink = vi.fn();
  const command = linkPageCommand(ID);
  mount(<>{command.panel({ insertLink, close: () => {} })}</>, {
    pages: () =>
      Promise.resolve({
        pages: [
          { id: ID, title: "Node runbook", container: "ENG", folder: false },
          { id: "p-9", title: "Node [drain]", container: "ENG", folder: false },
        ],
      }),
  });
  fireEvent.change(screen.getByRole("textbox", { name: "Link to the page" }), {
    target: { value: "Node" },
  });
  await settle();
  // The page being edited is not offered: a link to itself helps nobody.
  expect(screen.queryByText("Node runbook")).toBeNull();
  fireEvent.click(screen.getByText("Node [drain]"));
  expect(insertLink).toHaveBeenCalledWith("Node [drain]", `${PAGE_ADDRESS_PREFIX}p-9`);
});

test("only an exact repeat of the title on the first line is dropped", () => {
  expect(withoutRepeatedTitle("# Runbook\n\nBody", "Runbook")).toBe("Body");
  expect(withoutRepeatedTitle("## runbook ##\nBody", "Runbook")).toBe("Body");
  expect(withoutRepeatedTitle("# Overview\n\nBody", "Runbook")).toBe("# Overview\n\nBody");
  expect(withoutRepeatedTitle("Body\n# Runbook", "Runbook")).toBe("Body\n# Runbook");
});

// A READER WHO CANNOT CHANGE THE PAGE SEES THE SAME PAGE: Edit, Comment and
// Publish are drawn for everybody and disabled with the sentence that says
// what would change that, for each of the three readers who cannot act.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...JANE, acts: [] }, WRITE_REASONS.not_served],
])("every page write is drawn for %s, disabled with the reason", async (_who, viewer, reason) => {
  location.hash = `#/knowledge/new?space=ENG`;
  mount(
    <>
      <PageView id={ID} />
      <Composer />
    </>,
    { page: detail(), page_reads: reads(), viewer },
  );
  await settle();
  // THE EYE IS NAMED FOR THE READER'S OWN WATCH, which a bound person has.
  for (const name of [
    "Edit",
    "Comment",
    "Publish",
    "Save as draft",
    /^(Watch|Stop watching) this page$/,
  ]) {
    const button = screen.getByRole("button", { name });
    expect(button.getAttribute("aria-disabled"), String(name)).toBe("true");
    expect(reasonOf(button), String(name)).toContain(reason);
  }
  expect(posted).toEqual([]);
});

/** The reason a write control is disabled with, off the kit's described-by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

// A TITLE PAST THE ENGINE'S CAP IS SAID AND HELD, NEVER CUT.
test("a page title past the engine's cap is held whole and said, not cut", async () => {
  location.hash = `#/knowledge/new?space=ENG`;
  mount(<Composer />, { viewer: JANE, pages: { pages: [], limit: 500, total: 0 } });
  await settle();
  const title = screen.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  const typed = "t".repeat(PAGE_TITLE_MAX_BYTES + 44);
  fireEvent.change(title, { target: { value: typed } });
  await settle();
  expect(title.value).toBe(typed);
  expect(document.body.textContent).toContain(
    `${PAGE_TITLE_MAX_BYTES + 44} bytes, but a page's title holds at most ${PAGE_TITLE_MAX_BYTES}.`,
  );
  const publish = screen.getByRole("button", { name: "Publish" });
  expect(publish.getAttribute("aria-disabled")).toBe("true");
  expect(reasonOf(publish)).toContain("holds at most");
  expect(posted).toEqual([]);
});
