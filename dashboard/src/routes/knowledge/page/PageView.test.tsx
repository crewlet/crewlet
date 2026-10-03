/**
 * One knowledge page, whole: what it says about its readers, its links and its
 * skill loads — every number the engine's — and the edit a person makes on it
 * as themselves, including the one that races somebody else's save.
 */

import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { PageView, withoutRepeatedTitle } from "./PageView.tsx";
import { NewPageDialog } from "../NewPage.tsx";
import { pageLink } from "./Editor.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { crumbsFor, pageSpaceKey, pageUpKey } from "~/app/crumbs.ts";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { PageReadsAnswer } from "~/contract/pages.ts";
import type { PageDetail } from "~/protocol/index.ts";

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

// The viewer answer as the engine sends it: who, the grants they carry, the
// seat the directory binds them to, and the changes it makes for them.
const JANE = {
  login: "jane.doe",
  grants: ["state:read", "knowledge:write"],
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
    author: "cto",
    updated_at: "2026-09-24T22:00:00Z",
    revision: 40,
    body: "# Provisioner runbook\n\n## When a host fails\n\nCheck the switch.\n\n## Escalation\n\nPage the CTO.",
    ...page,
  } as PageDetail["page"],
  revision: 40,
  skill: false,
  onboarding: false,
  history: [{ version: 14, author: "swe", created_at: "2026-09-24T22:00:00Z" }],
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
      // 01:00 on the 25th in Tokyo: today.
      last_at: "2026-09-24T16:00:00Z",
      last_turn_id: "run-2",
      last_work_item: { id: "t-1", key: "ENG-412", title: "Retry PXE boot", ordinal: 2 },
    },
    {
      handle: "cto",
      via: "search",
      count: 3,
      // 23:00 on the 24th in Tokyo: yesterday, though it is the 24th in UTC.
      last_at: "2026-09-24T14:00:00Z",
      last_query: "DHCP lease",
    },
  ],
  readers_total: 2,
  // THE ENGINE'S COUNT, which a list cut by its cap cannot be counted from.
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
          receipt: {},
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
  reloadForTest();
  location.hash = "#/";
});

function mount(children: ReactNode, answers: Record<string, Answer>) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = {
      viewer: JANE,
      work_inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
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
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

// A PAGE IS ADDRESSED BY ITS ID, which a rename does not move.
test("the page is read by its id, and its readers by the same id", async () => {
  const { asked } = mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(asked.find((a) => a.kind === "page")?.params).toEqual({ id: ID });
  expect(asked.find((a) => a.kind === "page_reads")?.params).toEqual({ page: ID });
});

// "READ BY n AGENTS TODAY" IS THE ENGINE'S COUNT, cut on the company's day: the
// list carries two readers and one of them read on the company's yesterday,
// and the number drawn is still the engine's three.
test("the read-by count is the engine's, on the company's day", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.getByText("read by 3 agents today")).toBeTruthy();
});

test("each reader says how it read the page and in which turn", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const readers = screen.getByRole("region", { name: /^Read by agents/ });
  expect(
    within(readers).getByRole("link", { name: "turn 2 on ENG-412" }).getAttribute("href"),
  ).toBe("#/live/turns/run-2");
  expect(within(readers).getByText(/search: “DHCP lease”/)).toBeTruthy();
  // TWO LINES, NEVER THREE: the count rides the name's line, and the age is
  // held apart from the way-read text so an ellipsis cuts the query rather
  // than wrapping the age ("12 / reads · 1h ago") onto a line of its own.
  const count = within(readers).getByText("3 reads");
  expect(count.closest(".kpage-reader-head")).not.toBeNull();
  const how = within(readers).getByText(/search: “DHCP lease”/);
  expect(how.classList.contains("kpage-reader-how")).toBe(true);
  expect(how.getAttribute("title")).toBe(how.textContent);
  const when = how.parentElement?.querySelector(".kpage-reader-when");
  expect(when?.querySelector("time")).not.toBeNull();
  expect(how.contains(when ?? null)).toBe(false);
});

// THE WINDOW IS NAMED beside the page bar's "today": the list is 30 days, the
// count is the company's day, and unlabelled the two read as a contradiction.
test("the readers section names the window it lists", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const readers = screen.getByRole("region", { name: /^Read by agents/ });
  expect(within(readers).getByRole("heading").textContent).toBe("Read by agents · last 30 days");
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
  const readers = screen.getByRole("region", { name: /^Read by agents/ });
  const more = within(readers).getByRole("button", { name: "Show 2 more" });
  expect(more.getAttribute("aria-expanded")).toBe("false");
  const list = document.getElementById(more.getAttribute("aria-controls") ?? "");
  expect(list?.tagName).toBe("UL");
  expect(within(list!).getAllByRole("listitem")).toHaveLength(5);
  fireEvent.click(more);
  expect(more.getAttribute("aria-expanded")).toBe("true");
  expect(within(list!).getAllByRole("listitem")).toHaveLength(7);
});

// A SERVER WITHOUT THE USAGE DOMAIN DOES NOT SAY "NOBODY READ IT".
test("a node that cannot say who read the page draws no readers section", async () => {
  mount(<PageView id={ID} />, {
    page: detail(),
    page_reads: () => Promise.reject(new Error("unknown_query")),
  });
  await settle();
  expect(screen.queryByRole("region", { name: /^Read by agents/ })).toBeNull();
  expect(screen.queryByText(/No agent has read it/)).toBeNull();
});

test("the skill pill is drawn only on a tool-skill page, from both ways it loads", async () => {
  const loads = [
    { handle: "swe", last_at: "2026-09-24T16:00:00Z", count: 7, loaded: 2, offered: 5 },
    { handle: "cto", last_at: "2026-09-24T10:00:00Z", count: 1, loaded: 0, offered: 1 },
  ];
  mount(<PageView id={ID} />, {
    page: detail({ skill: true, skill_loaded_by: loads }),
    page_reads: reads(),
  });
  await settle();
  expect(screen.getByText("Loaded as a skill by SWE")).toBeTruthy();
  cleanup();

  mount(<PageView id={ID} />, {
    page: detail({ skill: false, skill_loaded_by: loads }),
    page_reads: reads(),
  });
  await settle();
  expect(screen.queryByText(/as a skill by/)).toBeNull();
});

test("an unloaded skill says so, and a node that cannot say draws the bare pill", async () => {
  // An EMPTY list is the engine saying nobody loaded it; an ABSENT one is a
  // node that does not read the usage domain, which is not the same fact.
  mount(<PageView id={ID} />, {
    page: detail({ skill: true, skill_loaded_by: [] }),
    page_reads: reads(),
  });
  await settle();
  expect(screen.getByText("Tool skill · not loaded in 30 days")).toBeTruthy();
  cleanup();

  mount(<PageView id={ID} />, { page: detail({ skill: true }), page_reads: reads() });
  await settle();
  expect(screen.getByText("Tool skill")).toBeTruthy();
  expect(screen.queryByText(/not loaded/)).toBeNull();
});

// A TASK LINKS BY ITS ADDRESS, NEVER ITS KEY: the engine flags a task whose key
// another task claimed first, and `#/work/ENG-412` opens the CLAIMANT — so the
// duplicate is reached by its id. Both rows print the same key, which is why
// each is found by its title.
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
          {
            id: "t-2",
            key: "ENG-412",
            key_collision: true,
            title: "Retry PXE boot (dup)",
            status: "todo",
            via: ["description"],
          },
        ],
        pages: [{ id: "p-2", container: "ENG", title: "Scheduler on-call" }],
        tasks_total: 2,
        pages_total: 1,
      },
    }),
    page_reads: reads(),
  });
  await settle();
  const links = screen.getByRole("region", { name: "Linked from" });
  const task = (title: string) =>
    within(links)
      .getByText(title, { selector: ".kpage-link-title" })
      .closest("a")
      ?.getAttribute("href");
  expect(task("Retry PXE boot")).toBe("#/work/ENG-412");
  expect(task("Retry PXE boot (dup)")).toBe("#/work/t-2");
  expect(within(links).getByRole("link", { name: "Scheduler on-call" }).getAttribute("href")).toBe(
    "#/knowledge/pages/p-2",
  );
});

// AN INDEX STILL ON ITS FIRST LAP IS NOT "NOTHING LINKS HERE": the engine
// names why it sent no list, and the rail says that instead of the empty
// claim. A node with no index at all says nothing about links.
test("a node still building its index says so rather than that nothing links here", async () => {
  mount(<PageView id={ID} />, {
    page: detail({ linked_from_status: "building" }),
    page_reads: reads(),
  });
  await settle();
  const links = screen.getByRole("region", { name: "Linked from" });
  expect(within(links).getByText(/still building its search index/)).toBeTruthy();
  expect(within(links).queryByText(/No page or task links here/)).toBeNull();
  cleanup();

  mount(<PageView id={ID} />, {
    page: detail({ linked_from_status: "unavailable" }),
    page_reads: reads(),
  });
  await settle();
  expect(
    within(screen.getByRole("region", { name: "Linked from" })).getByText(
      /could not read what links here/,
    ),
  ).toBeTruthy();
  cleanup();

  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  expect(screen.queryByRole("region", { name: "Linked from" })).toBeNull();
});

// THE CAP NOTE IS COMPANY-WIDE, and worded so: the engine's `elided` counts
// what the cap dropped on every page, so it says the list MAY be short.
test("dropped reads are named as possibly missing, never as this page's", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads({ elided: 4 }) });
  await settle();
  const readers = screen.getByRole("region", { name: /^Read by agents/ });
  expect(within(readers).getByText(/this list may be missing some/)).toBeTruthy();
});

test("the outline lists the sections, and a body's first line that repeats the title is not drawn", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  const toc = screen.getByRole("region", { name: "On this page" });
  expect(
    within(toc)
      .getAllByRole("button")
      .map((b) => b.textContent),
  ).toEqual(["When a host fails", "Escalation"]);
  // The title is the page's h1 and nowhere else.
  expect(screen.getAllByText("Provisioner runbook", { selector: "h1, h2, h3" })).toHaveLength(1);
});

// THE CONFLICT FLOW. The page is polled while it is edited; a save that lands
// meanwhile is SEEN before the person presses Save, and Save waits until they
// have chosen to keep editing on top of it — which MERGES their change into
// the draft before the base moves. Moving the base alone would let the next
// save delete their line with the engine's blessing.
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

const box = () => screen.getByRole("textbox", { name: /The body of/ }) as HTMLTextAreaElement;

test("keeping on top of somebody else's save keeps their change in the draft", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  const race = racing({
    14: "## Steps\n\nold\n\n## Escalation\n\npage the CTO",
    15: "## Steps\n\nold\n\n## Escalation\n\npage the CTO\n\nMaya added this line.",
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  fireEvent.change(box(), {
    target: { value: "## Steps\n\nmine\n\n## Escalation\n\npage the CTO" },
  });

  // Somebody saves revision 15, and the page's own poll brings it.
  await race.save(15);
  expect(screen.getByText(/Somebody saved revision 15 while you edited/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  // ⌘-Enter goes through the same gate as the button.
  fireEvent.keyDown(box(), { key: "Enter", metaKey: true });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(posted).toEqual([]);

  fireEvent.click(screen.getByRole("button", { name: "View their change" }));
  expect(screen.getByText("Maya added this line.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep editing on top of it" }));
  expect(box().value).toBe(
    "## Steps\n\nmine\n\n## Escalation\n\npage the CTO\n\nMaya added this line.",
  );
  expect(screen.getByText(/Revision 15.s change is in your draft/).getAttribute("role")).toBe(
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

// WHERE BOTH EDITS CHANGED THE SAME LINES the browser picks nobody: the base
// stays behind, the clash is shown, and Save waits for the person's answer.
test("an overlapping change is settled by the person before the base moves", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  const race = racing({ 14: "## Steps\n\nold", 15: "## Steps\n\ntheirs" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  fireEvent.change(box(), { target: { value: "## Steps\n\nmine" } });
  await race.save(15);

  fireEvent.click(screen.getByRole("button", { name: "Keep editing on top of it" }));
  const resolve = screen.getByRole("region", { name: /changed the same lines/ });
  expect(within(resolve).getByText("theirs")).toBeTruthy();
  expect(within(resolve).getByText("mine")).toBeTruthy();
  // Nothing is pre-chosen, and the draft is held while the clash is open.
  const apply = within(resolve).getByRole("button", { name: "Apply to my draft" });
  expect((apply as HTMLButtonElement).disabled).toBe(true);
  expect(box().readOnly).toBe(true);
  expect(screen.getByText(/Saved as you, against revision 14/)).toBeTruthy();

  fireEvent.click(within(resolve).getByRole("radio", { name: "Both" }));
  fireEvent.click(apply);
  expect(box().value).toBe("## Steps\n\nmine\ntheirs");
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

test("a comment is written as the signed-in person", async () => {
  mount(<PageView id={ID} />, { page: detail(), page_reads: reads() });
  await settle();
  fireEvent.change(screen.getByRole("textbox", { name: /Comment on/ }), {
    target: { value: "Is step 2 still right?" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Comment" }));
  await settle();
  expect(posted).toEqual([
    { tool: "comment_on_page", args: { page: ID, body: "Is step 2 still right?" } },
  ]);
});

// THE TRAIL IS THE PAGE'S PLACE: the space and the pages above it, from the
// labels the page publishes.
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
test("Link a page writes the page's id address, which a rename does not break", () => {
  expect(pageLink("Node [drain]", "p-9")).toBe(`[Node \\[drain\\]](${PAGE_ADDRESS_PREFIX}p-9)`);
});

test("only an exact repeat of the title on the first line is dropped", () => {
  expect(withoutRepeatedTitle("# Runbook\n\nBody", "Runbook")).toBe("Body");
  expect(withoutRepeatedTitle("## runbook ##\nBody", "Runbook")).toBe("Body");
  expect(withoutRepeatedTitle("# Overview\n\nBody", "Runbook")).toBe("# Overview\n\nBody");
  expect(withoutRepeatedTitle("Body\n# Runbook", "Runbook")).toBe("Body\n# Runbook");
});

// A READER WHO CANNOT CHANGE THE PAGE SEES THE SAME PAGE: Edit, Comment and a
// new page's Write are drawn for everybody and disabled with the sentence
// that says what would change that — for each reader who cannot act. (An
// unbound caller acts too, under their own login: binding adds the seat they
// act as, not the right to act.)
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...JANE, acts: [] }, WRITE_REASONS.not_served],
])("every page write is drawn for %s, disabled with the reason", async (_who, viewer, reason) => {
  location.hash = `#/knowledge/pages/${ID}`;
  mount(
    <>
      <PageView id={ID} />
      <NewPageDialog container="ENG" onClose={() => {}} />
    </>,
    { page: detail(), page_reads: reads(), viewer },
  );
  await settle();
  for (const name of ["Edit", "Comment", "Write the page"]) {
    const button = screen.getByRole("button", { name });
    expect(button.getAttribute("aria-disabled"), name).toBe("true");
    expect(reasonOf(button), name).toContain(reason);
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
