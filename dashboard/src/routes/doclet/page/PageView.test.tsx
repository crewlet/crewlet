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
import { movePlaces } from "./Tidy.tsx";
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
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
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
  grants: ["admin", "worklet", "doclet"],
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
  ).toBe("#/doclet/ENG");
  expect(
    within(props as HTMLElement)
      .getByRole("link", { name: "Runbooks" })
      .getAttribute("href"),
  ).toBe("#/doclet/pages/p-0");
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
  ).toBe("#/worklet/ENG-412");
  expect(within(links).getByRole("link", { name: "Scheduler on-call" }).getAttribute("href")).toBe(
    "#/doclet/pages/p-2",
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
  // The title is drawn once.
  expect(screen.getAllByText("Provisioner runbook", { selector: "h1, h2, h3" })).toHaveLength(1);
});

// THE PAGE'S OUTLINE STEPS BY ONE UNDER THE TRAIL'S h1: its title an h2, as
// every object's title is in the frame, and its body's first heading an h3
// whatever `#` it was written with. The title was a second h1 and a body
// written from `##` opened two levels under it. Mutation: draw the title as
// an h1 again, or the body from its own levels, and this goes red.
test("the title is an h2 and the body's sections step from h3", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.getByRole("heading", { name: "Provisioner runbook" }).tagName).toBe("H2");
  expect(screen.queryAllByRole("heading", { level: 1 })).toHaveLength(0);
  expect(screen.getByRole("heading", { name: "When a host fails" }).tagName).toBe("H3");
  expect(screen.getByRole("heading", { name: "Escalation" }).tagName).toBe("H3");
  // And the Connections cards sit under their section's h2.
  expect(screen.getByRole("heading", { name: "Connections" }).tagName).toBe("H2");
  expect(screen.getByRole("heading", { name: /^Read by agents/ }).tagName).toBe("H3");
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
  ).toBe("#/doclet/pages/p-7");
});

// HISTORY SWAPS THE OUTLINE FOR THE REVISIONS and the thread for the changes.
test("History reads the newest revision, lists the others, and shows the page's changes", async () => {
  location.hash = `#/doclet/pages/${ID}`;
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
  expect(changes.textContent).toContain("Saved it");
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

// THE COUNT IS NAMED BY THE NUMBER IT SHOWS, then by who it counts. An
// aria-label of "Watching: you, …" over a button showing "2" was a name that
// did not contain what is on screen (axe label-content-name-mismatch), so a
// person who says "click 2" reached nothing.
test("the watchers count is named by the number it shows, then by who watches", async () => {
  mount(<PageView id={ID} />, {
    page: detail({}, { watchers: ["jane", "swe"] }),
    page_reads: reads(),
  });
  await settle();
  const count = screen.getByRole("button", { name: /^2 watching/ });
  expect(count.hasAttribute("aria-label")).toBe(false);
  expect(count.textContent?.startsWith("2")).toBe(true);
  expect(count.textContent).toContain("you");
});

// A DRAFT HAS A WAY OUT: the page says it is one, above its body, with Publish
// beside it, and the press is `save_page` with the status alone against the
// version on screen. A page saved as a draft used to stay one for good: the
// editor saved a body and nothing else, and nothing in the product named a
// status. A published page draws no notice. Mutation: send the press without
// `status` and the engine saves nothing; draw the notice for every page and
// the control below goes red.
test("a draft says it is one and is published by one press, against the version on screen", async () => {
  mount(<PageView id={ID} />, { page: detail({}, { status: "draft" }), page_reads: reads() });
  await settle();
  const notice = screen
    .getByText("This page is a draft")
    .closest(".crewlet-callout") as HTMLElement;
  // IN A READER'S WORDS, said once: the Kind line says "Draft" and the notice
  // what that means, where both spelled out the search exclusion.
  expect(notice.textContent).toContain("Search leaves it out until it is published.");
  expect(document.querySelector(".knowledge-props")!.textContent).toMatch(/KindDraft$/);
  fireEvent.click(within(notice).getByRole("button", { name: "Publish" }));
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, status: "published" } },
  ]);
  cleanup();

  posted = [];
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.queryByText("This page is a draft")).toBeNull();
  expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
});

// THE SAME PRESS IS IN MORE, where a phone folds the bar, and a published
// page's More holds the way back to a draft.
test("More publishes a draft, and takes a published page back to one", async () => {
  mount(<PageView id={ID} />, { page: detail({}, { status: "draft" }), page_reads: reads() });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: /^More for/ }));
  expect(screen.queryByRole("menuitem", { name: /Move back to draft/ })).toBeNull();
  fireEvent.click(screen.getByRole("menuitem", { name: /^Publish/ }));
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, status: "published" } },
  ]);
  cleanup();

  posted = [];
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: /^More for/ }));
  expect(screen.queryByRole("menuitem", { name: /^Publish/ })).toBeNull();
  fireEvent.click(screen.getByRole("menuitem", { name: /Move back to draft/ }));
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, status: "draft" } },
  ]);
});

// THE ENGINE DECIDES WHO PUBLISHES (the page's creator, its space's lead, an
// admin), and its refusal is said in the notice the press was made from.
test("a publish the engine refuses says why, in the notice", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            error: "forbidden",
            message: "Not allowed.",
            detail: "Only the page's creator, the lead of its space or an admin publishes it.",
          }),
          { status: 403, headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" } },
        ),
    ),
  );
  mount(<PageView id={ID} />, { page: detail({}, { status: "draft" }), page_reads: reads() });
  await settle();
  const notice = screen
    .getByText("This page is a draft")
    .closest(".crewlet-callout") as HTMLElement;
  fireEvent.click(within(notice).getByRole("button", { name: "Publish" }));
  await settle();
  expect(within(notice).getByText(/Only the page's creator/)).toBeTruthy();
  expect(screen.getByText("This page is a draft")).toBeTruthy();
});

/** Every request a tidy gesture sends, answered as the engine answers a landed page write. */
function recordRest(): { method: string; url: string; body: unknown; key: string | null }[] {
  const sent: { method: string; url: string; body: unknown; key: string | null }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      sent.push({
        method: (init.method ?? "GET").toUpperCase(),
        url: String(url),
        body: typeof init.body === "string" ? JSON.parse(init.body) : undefined,
        key: ((init.headers ?? {}) as Record<string, string>)["Idempotency-Key"] ?? null,
      });
      return new Response(
        JSON.stringify({
          id: ID,
          title: "Provisioner runbook",
          container: "ENG",
          outcome: "applied",
          position: "CREWLET_PAGES_LOG@1:120",
          op_id: "k",
        }),
        { status: 200, headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" } },
      );
    }),
  );
  return sent;
}

const openMore = async (item: RegExp) => {
  fireEvent.click(screen.getByRole("button", { name: /^More for/ }));
  fireEvent.click(await screen.findByRole("menuitem", { name: item }));
};

// A PAGE IS RENAMED FROM ITS OWN SCREEN, through the engine's rename route,
// keyed so a retry after a lost answer is the same rename. More offered no
// rename, no labels, no move and no trash: the engine served each and the
// dashboard reached none. Mutation: post the rename unkeyed and the key is
// missing; send the title it already has and nothing should be sent.
test("Rename sends the new title to the page's rename route, keyed", async () => {
  const sent = recordRest();
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  await openMore(/^Rename/);
  const dialog = screen.getByRole("dialog", { name: "Rename “Provisioner runbook”" });
  expect(within(dialog).getByRole("button", { name: "Rename" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  fireEvent.change(within(dialog).getByLabelText("Title"), {
    target: { value: "Provisioning runbook" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await settle();
  const rename = sent.find((s) => s.method === "POST");
  expect(rename?.url).toContain(`/doclet/pages/${ID}/rename`);
  expect(rename?.body).toEqual({ title: "Provisioning runbook" });
  expect(rename?.key).toMatch(/^[0-9a-f-]{36}$/);
  expect(screen.queryByRole("dialog", { name: /Rename/ })).toBeNull();
});

// THE TRASH IS THE PAGE'S DELETE, confirmed first, and keyed.
test("Move to trash asks first, then deletes the page through its route", async () => {
  const sent = recordRest();
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  await openMore(/^Move to trash/);
  const dialog = screen.getByRole("dialog", { name: "Move “Provisioner runbook” to the trash?" });
  expect(sent.filter((s) => s.method !== "GET")).toEqual([]);
  fireEvent.click(within(dialog).getByRole("button", { name: "Move to trash" }));
  await settle();
  const trash = sent.find((s) => s.method === "DELETE");
  expect(trash?.url).toContain(`/doclet/pages/${ID}`);
  expect(trash?.key).toBeTruthy();
});

// A PAGE IN THE TRASH SAYS SO, with the way back. Restore is its route; an
// admin is offered Delete permanently, which sends the title typed back and a
// reason, and nothing until both are given. Mutation: offer no notice on a
// trashed page and there is no way to restore one from the dashboard.
test("a page in the trash offers Restore, and an admin Delete permanently with the title typed back", async () => {
  const sent = recordRest();
  mount(<PageView id={ID} />, { page: detail({}, { status: "trashed" }), page_reads: reads() });
  await settle();
  const notice = screen
    .getByText("This page is in the trash")
    .closest(".crewlet-callout") as HTMLElement;
  fireEvent.click(within(notice).getByRole("button", { name: "Restore" }));
  await settle();
  expect(sent.find((s) => s.method === "POST")?.url).toContain(`/doclet/pages/${ID}/restore`);

  fireEvent.click(within(notice).getByRole("button", { name: "Delete permanently" }));
  const dialog = screen.getByRole("dialog", { name: "Delete “Provisioner runbook” permanently?" });
  const purge = () => within(dialog).getByRole("button", { name: "Delete permanently" });
  expect(purge().getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(within(dialog).getByRole("textbox", { name: /Provisioner runbook|confirm/i }), {
    target: { value: "Provisioner runbook" },
  });
  expect(purge().getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(within(dialog).getByLabelText("Why"), { target: { value: "Duplicate" } });
  fireEvent.click(purge());
  await settle();
  const sentPurge = sent.find((s) => s.url.includes("/purge"));
  const url = new URL(sentPurge!.url, "http://engine.test");
  expect(url.pathname).toBe(`/doclet/pages/${ID}/purge`);
  expect(url.searchParams.get("confirm")).toBe("Provisioner runbook");
  expect(url.searchParams.get("reason")).toBe("Duplicate");
  // A PUBLISHED PAGE DRAWS NO TRASH NOTICE.
  cleanup();
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.queryByText("This page is in the trash")).toBeNull();
});

// A PAGE IN THE TRASH READS AND TAKES NO CHANGE. It offered Edit, the watch
// eye and a working comment box, so a deleted page could be rewritten,
// followed and discussed. Every control in the bar that changes it is held
// with the one reason, the thread reads with no field, Restore stays the way
// back, and History still reads. Mutation: draw the bar or the thread without
// the page's hold and this fails.
test("a page in the trash holds Edit, the watch and a sub-page with one reason, and takes no comment", async () => {
  mount(<PageView id={ID} />, { page: detail({}, { status: "trashed" }), page_reads: reads() });
  await settle();
  const held = (el: HTMLElement) => el.getAttribute("aria-disabled") === "true";
  const edit = screen.getByRole("button", { name: "Edit" });
  expect(held(edit)).toBe(true);
  expect(edit.getAttribute("title")).toBe("It is in the trash. Restore it first.");
  expect(held(screen.getByRole("button", { name: /watching this page$|^Watch this page/i }))).toBe(
    true,
  );
  // THE THREAD READS, and takes no comment: no field to type into.
  expect(screen.queryByRole("textbox", { name: /Comment/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Comment" })).toBeNull();
  expect(held(screen.getByRole("button", { name: /History/ }))).toBe(false);
  await openMore(/^New sub-page/);
  expect(posted).toEqual([]);
  expect(location.hash).not.toContain("doclet/new");
});

// A MOVE OFFERS EVERY PAGE OF THE SPACE BUT THE PAGE AND WHAT IS UNDER IT,
// which the engine refuses as a parent, and saves the parent with save_page.
// THE LIST IS THE DIALOG'S BODY, on screen in the dialog under its field, so
// nothing it draws covers the dialog's own Cancel and Move: a popup picker's
// panel opened down over the foot. Mutation: put the popup Select back, and
// no option is in the dialog until a trigger is pressed.
test("Move files the page under another page of its space, never under itself", async () => {
  const pages = [
    {
      id: ID,
      title: "Provisioner runbook",
      parent_id: "p-0",
      container: "ENG",
      status: "published",
    },
    { id: "p-0", title: "Runbooks", container: "ENG", status: "published" },
    { id: "p-1", title: "Under it", parent_id: ID, container: "ENG", status: "published" },
    { id: "p-2", title: "Onboarding", container: "ENG", status: "published" },
  ];
  expect(movePlaces(pages, ID).map((p) => p.id)).toEqual(["p-0", "p-2"]);
  mount(<PageView id={ID} />, {
    page: detail({}, { parent_id: "p-0" }),
    page_reads: reads(),
    pages: { pages, limit: 500, total: 4 },
  });
  await settle();
  await openMore(/^Move(?! back| to)/);
  const dialog = screen.getByRole("dialog", { name: "Move “Provisioner runbook”" });
  await settle();
  const list = within(dialog).getByRole("listbox", { name: "The page it goes under" });
  const offered = within(list)
    .getAllByRole("option")
    .map((o) => o.textContent ?? "");
  expect(offered.some((o) => o.includes("Under it"))).toBe(false);
  expect(offered.some((o) => o.includes("Top of the space"))).toBe(true);
  // WHERE IT IS NOW IS MARKED, and is what Move is held on.
  expect(within(list).getByRole("option", { name: /Runbooks/ }).textContent).toContain("Current");
  expect(within(dialog).getByRole("button", { name: "Move" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  // THE FIELD FILTERS THE LIST by words from a title.
  fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "onboard" } });
  await settle();
  expect(
    within(list)
      .getAllByRole("option")
      .map((o) => o.textContent ?? ""),
  ).toEqual(["Onboarding"]);
  fireEvent.click(within(list).getByRole("option", { name: /Onboarding/ }));
  await settle();
  fireEvent.click(within(dialog).getByRole("button", { name: "Move" }));
  await settle();
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, parent: "p-2" } },
  ]);
});

// A WATCH IS FILED UNDER A SEAT, and a credential bound to none subscribes
// nobody (`internal/doclet` `Actor.subscriber`): the eye says so rather than
// offering a press the engine answers by moving nothing.
test("the eye is held, with its reason, for a credential bound to no seat", async () => {
  const unbound = {
    login: "svc:docs",
    grants: ["doclet"],
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

// THE STAR IS IN THE PAGE'S MORE MENU, beside its other page actions, and a
// published page's way back to a draft.
test("More stars the page, and offers the sub-page, a draft, History and the address", async () => {
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
    "Star this pagePinned on Doclet and kept in the sidebar",
    "New sub-page",
    "Move back to draftSearch leaves it out until it is published again",
    "Rename",
    "Labels",
    "MoveUnder another page of its space",
    "Move to trash",
    "History",
    "Copy the page's address",
  ]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: /Star this page/ }));
  await settle();
  expect(screen.getByRole("status", { name: "stars" }).textContent).toBe(`doclet/pages/${ID}`);
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
function racing(bodies: Record<number, string>, page: Record<string, unknown> = {}) {
  let version = 14;
  location.hash = `#/doclet/pages/${ID}?edit=1`;
  mount(<PageView id={ID} />, {
    page: () => Promise.resolve(detail({}, { ...page, version, body: bodies[version] })),
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

// EDITING A DRAFT, Save keeps it a draft and Publish is the save and the
// status in ONE write, so a draft is never published as the text before the
// last edit. A publish with no edit sends no body: the status moves and no
// revision is written. Mutation: publish with a second write after the save
// and two requests are posted; always send the body and the unedited publish
// writes a revision that changed nothing.
test("editing a draft saves it as a draft, or publishes what is written in one write", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  racing({ 14: "## Steps\n\nold" }, { status: "draft" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  // NOTHING EDITED: Save is held, Publish is not.
  expect(screen.getByRole("button", { name: "Save draft" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
  fireEvent.click(screen.getByRole("button", { name: "Publish" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, status: "published" } },
  ]);
  cleanup();

  posted = [];
  racing({ 14: "## Steps\n\nold" }, { status: "draft" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  await retype(box(), "old", "mine");
  fireEvent.click(screen.getByRole("button", { name: "Save draft" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([
    { tool: "save_page", args: { page: ID, base_version: 14, body: "## Steps\n\nmine" } },
  ]);
  cleanup();

  posted = [];
  racing({ 14: "## Steps\n\nold" }, { status: "draft" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  await retype(box(), "old", "mine");
  fireEvent.click(screen.getByRole("button", { name: "Publish" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([
    {
      tool: "save_page",
      args: { page: ID, base_version: 14, status: "published", body: "## Steps\n\nmine" },
    },
  ]);
  vi.useRealTimers();
});

// A PUBLISHED PAGE'S EDITOR IS AS IT WAS: one Save, no Publish.
test("editing a published page offers Save alone", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  racing({ 14: "## Steps\n\nold" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Save draft" })).toBeNull();
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

// A PERSON EDITS AND TAKES DOWN THEIR OWN REMARK ON A PAGE, through the
// engine's PATCH and DELETE on the comment's address, each keyed. The thread
// offered only Reply, though the engine served both. Somebody else's remark
// offers neither. Mutation: mark every remark editable and the other seat's
// offers Edit; send the edit through the act route and no PATCH is made.
test("a person edits and deletes their own page comment, keyed, and nobody else's", async () => {
  const sent = recordRest();
  mount(<PageView id={ID} />, {
    page: detail({
      comments: [
        {
          id: "c-1",
          author: "jane",
          author_kind: "human",
          body: "Is step 2 still right?",
          created_at: "2026-09-24T20:00:00Z",
          updated_at: "2026-09-24T20:00:00Z",
        } as never,
        {
          id: "c-2",
          author: "swe",
          author_kind: "agent",
          body: "It is.",
          created_at: "2026-09-24T20:05:00Z",
          updated_at: "2026-09-24T20:05:00Z",
          reply_to: "c-1",
        } as never,
      ],
    }),
    page_reads: reads(),
  });
  await settle();
  const thread = region(/^Comments/);
  const [mine, theirs] = within(thread).getAllByRole("listitem");
  expect(within(theirs!).queryByRole("button", { name: /Edit|Delete/ })).toBeNull();

  fireEvent.click(within(mine!).getByRole("button", { name: "Edit your comment" }));
  const field = within(mine!).getByRole("textbox", { name: "Edit your comment" });
  expect((field as HTMLTextAreaElement).value).toBe("Is step 2 still right?");
  fireEvent.change(field, { target: { value: "Is step 2 still right after the move?" } });
  fireEvent.click(within(mine!).getByRole("button", { name: "Save" }));
  await settle();
  const patch = sent.find((s) => s.method === "PATCH");
  expect(patch?.url).toContain(`/doclet/pages/${ID}/comments/c-1`);
  expect(patch?.body).toEqual({ body: "Is step 2 still right after the move?" });
  expect(patch?.key).toMatch(/^[0-9a-f-]{36}$/);

  fireEvent.click(
    within(mine!).getByRole("button", { name: "Delete the comment by Jane Founder" }),
  );
  fireEvent.click(
    within(within(mine!).getByRole("group", { name: /Delete this comment/ })).getByRole("button", {
      name: "Delete",
    }),
  );
  await settle();
  const removal = sent.find((s) => s.method === "DELETE");
  expect(removal?.url).toContain(`/doclet/pages/${ID}/comments/c-1`);
  expect(removal?.key).toMatch(/^[0-9a-f-]{36}$/);
});

// A PAGE IN THE TRASH TAKES NO CHANGE TO A REMARK EITHER: the engine refuses
// an edit or a removal of a comment on a trashed page as it refuses a new one,
// so the reader's own remark there reads and offers neither. Mutation: mark
// the reader's remark editable whatever the page's hold, and it offers Edit.
test("a page in the trash offers no Edit or Delete on the reader's own comment", async () => {
  mount(<PageView id={ID} />, {
    page: detail(
      {
        comments: [
          {
            id: "c-1",
            author: "jane",
            author_kind: "human",
            body: "Is step 2 still right?",
            created_at: "2026-09-24T20:00:00Z",
            updated_at: "2026-09-24T20:00:00Z",
          } as never,
        ],
      },
      { status: "trashed" },
    ),
    page_reads: reads(),
  });
  await settle();
  const [mine] = within(region(/^Comments/)).getAllByRole("listitem");
  expect(within(mine!).getByText("Is step 2 still right?")).toBeTruthy();
  expect(within(mine!).queryByRole("button", { name: /Edit|Delete/ })).toBeNull();
});

// A UUID IS NOTHING A PERSON READS: until the page answers, and when it never will, a noun.
test("the trail never prints a page's id", () => {
  expect(crumbsFor(["doclet", "pages", ID]).at(-1)).toEqual({ label: "Page" });
  const crumbs = crumbsFor(["doclet", "pages", ID], { [pageUpKey(ID, 0)]: "p-0" });
  expect(crumbs.map((c) => c.label)).toEqual(["Doclet", "Page", "Page"]);
});

// THE TRAIL IS THE PAGE'S PLACE: the space and the pages above it.
test("the trail names the space and every page above this one", () => {
  const crumbs = crumbsFor(["doclet", "pages", ID], {
    [ID]: "Provisioner runbook",
    [pageSpaceKey(ID)]: "ENG",
    ENG: "Engineering",
    [pageUpKey(ID, 0)]: "p-0",
    "p-0": "Runbooks",
  });
  expect(crumbs.map((c) => c.label)).toEqual([
    "Doclet",
    "Engineering",
    "Runbooks",
    "Provisioner runbook",
  ]);
  expect(crumbs[1]?.path).toEqual(["doclet", "ENG"]);
  expect(crumbs[2]?.path).toEqual(["doclet", "pages", "p-0"]);
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
  location.hash = `#/doclet/new?space=ENG`;
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
  location.hash = `#/doclet/new?space=ENG`;
  mount(<Composer />, { viewer: JANE, pages: { pages: [], limit: 500, total: 0 } });
  await settle();
  const title = screen.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  const typed = "t".repeat(PAGE_TITLE_MAX_BYTES + 44);
  fireEvent.change(title, { target: { value: typed } });
  await settle();
  expect(title.value).toBe(typed);
  expect(document.body.textContent).toContain("This page's title is 44 characters too long.");
  const publish = screen.getByRole("button", { name: "Publish" });
  expect(publish.getAttribute("aria-disabled")).toBe("true");
  expect(reasonOf(publish)).toContain("too long");
  expect(posted).toEqual([]);
});

// A PAGE'S BAR IS THE PAGE'S OWN ACTIONS, and so is the composer's. The trail
// is the one thing on a bar that gives way, and a search field with its
// ranking menu beside a page's watch, History, Edit and More took the room its
// ancestors needed: at 1440 "Doclet" fell to "D…", and at 390 the bar took
// three rows. Mutation: draw `DocletBar` on either screen and this fails.
test("a page's bar and the composer's hold their own actions and no search", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
  expect(screen.getByRole("button", { name: /History/ })).toBeTruthy();
  expect(screen.queryByRole("search")).toBeNull();
  expect(screen.queryByRole("searchbox", { name: "Search pages" })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Rank by/ })).toBeNull();

  cleanup();
  location.hash = "#/doclet/new?space=ENG";
  mount(<Composer />, { viewer: JANE, pages: { pages: [], limit: 500, total: 0 } });
  await settle();
  expect(screen.getByRole("button", { name: /Save as draft/ })).toBeTruthy();
  expect(screen.queryByRole("search")).toBeNull();
  expect(screen.queryByRole("button", { name: /^Rank by/ })).toBeNull();
});
