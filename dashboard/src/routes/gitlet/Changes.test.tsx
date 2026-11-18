/**
 * Gitlet › Changes: the engine's listing over REST, each change Gitlet's row with its card. The
 * listing asks its cards, a state it can walk and an order only where the engine takes one; the
 * facts and the totals are the engine's; Waiting on you is its `needs=me`; a row peeks its change.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Suspense, type ReactElement } from "react";
import { afterEach, beforeAll, expect, test, vi } from "vitest";

import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { afterMicrotasks, settled } from "~/test/settle.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { ChangeStateTag, checksSentence, preconditionTone } from "./ChangeRow.tsx";
import { Changes, changeOrder, changesParams, footWords } from "./Changes.tsx";

beforeAll(() => {
  // jsdom implements no scrolling, and a step scrolls its row into view.
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const NOW = new Date("2026-10-28T12:00:00Z");
const minutesAgo = (m: number) => new Date(NOW.getTime() - m * 60_000).toISOString();

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Engineer", handle: "dev" },
    { name: "Code Reviewer", handle: "reviewer" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

type Pre = { kind: string; state: string; waits_on?: { handle: string; kind: string }[] } & Record<
  string,
  unknown
>;

/** A readiness of these preconditions, in order. */
function readiness(pre: Pre[]) {
  return {
    met: pre.filter((p) => p.state === "met").length,
    total: pre.length,
    first: "engine words",
    preconditions: pre.map((p) => ({ detail: `${p.kind} detail`, ...p })),
  };
}

const READY_BUT_REVIEW = readiness([
  { kind: "facts", state: "met" },
  { kind: "approval", state: "unmet", waits_on: [{ handle: "reviewer", kind: "agent" }] },
  { kind: "check", state: "met" },
  { kind: "mergeable", state: "met" },
]);

/** A change as the listing answers it with its card, with what a case overrides. */
function change(repo: string, number: number, extra: Record<string, unknown> = {}) {
  return {
    id: `id-${repo}-${number}`,
    repo_id: `repo-${repo}`,
    number,
    title: `Change ${number} of ${repo}`,
    state: "open",
    source_ref: "refs/heads/feature",
    target_ref: "refs/heads/main",
    head: "0123456789abcdef0123456789abcdef01234567",
    revision: 1,
    owner: "dev",
    owner_kind: "agent",
    opened_at: minutesAgo(180),
    opened_position: POSITION,
    open_findings: 0,
    updated_at: minutesAgo(2),
    version: 4,
    position: POSITION,
    repo,
    ref: `${repo}!${number}`,
    access: "maintain",
    readiness: READY_BUT_REVIEW,
    card: { comments: 0, reviews: 0, changes_requested: 0 },
    ...extra,
  };
}

/** web!42 as the story tells it: asked of Jane as an owner, changes requested, checks running. */
const WEB_42 = change("web", 42, {
  title: "render: stream the product page's shell",
  revision: 3,
  open_findings: 2,
  source_ref: "refs/heads/frontend/stream-shell",
  work_item: { backend: "worklet", id: "t-415", key: "ENG-415" },
  readiness: readiness([
    { kind: "facts", state: "met" },
    { kind: "owners_approval", state: "unmet", waits_on: [{ handle: "jane", kind: "person" }] },
    { kind: "blocker", state: "met" },
    { kind: "check", state: "computing" },
    { kind: "mergeable", state: "met" },
  ]),
  card: {
    comments: 3,
    reviews: 2,
    changes_requested: 1,
    reviewers: [
      { handle: "reviewer", kind: "agent", verdict: "request_changes", verdict_revision: 1 },
      { handle: "cto", kind: "agent", verdict: "comment", verdict_revision: 3 },
      { handle: "jane", kind: "person", asked: true, reason: "path_owner" },
    ],
    checks: {
      state: "running",
      counts: { passed: 1, running: 1 },
      required: 2,
      required_passed: 1,
    },
  },
});

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
const page = (changes: unknown[], extra: Record<string, unknown> = {}) =>
  json({ changes, position: POSITION, level: "session", ...extra });
const COUNTS = { changes: { live: 5, draft: 1, open: 4, closed: 3, merged: 54 } };

/** The engine: `answer` takes the listing and the box; the counts answer the story's totals. */
function stubFetch(
  answer: (url: URL) => Response | Promise<Response> | null,
  counts: () => Response = () => json({ counts: COUNTS, position: POSITION, level: "session" }),
) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname === "/query/code_repos") {
        return Promise.resolve(answer(url) ?? json({ position: POSITION, level: "stale" }));
      }
      if (url.pathname === "/query/code_counts") return Promise.resolve(counts());
      return Promise.resolve(answer(url) ?? json({}, 404));
    }),
  );
  return asked;
}

/** The list's asks, not the box's (`limit`), nor the counts'. */
const listings = (asked: URL[]) =>
  asked.filter((u) => u.pathname === "/query/code_changes" && !u.searchParams.has("limit"));
const boxAsks = (asked: URL[]) =>
  asked.filter((u) => u.pathname === "/query/code_changes" && u.searchParams.has("limit"));

/** The list answers `rows`; the box answers `box` (none by default). */
const engine =
  (rows: unknown[], box: unknown[] = []) =>
  (url: URL) =>
    url.pathname !== "/query/code_changes"
      ? null
      : url.searchParams.has("limit")
        ? page(box)
        : page(rows);

function mount(ui: ReactElement, hash = "#/gitlet", viewer: object = ADMIN_VIEWER) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>{ui}</Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const theList = () => screen.findByRole("list", { name: "Changes" });
const rowOf = (list: HTMLElement, title: string) =>
  within(list).getByRole("link", { name: title }).closest<HTMLElement>("li")!;

// Mutation: ask without cards or the order, or link a row anywhere but its peek.
test("the changes are the engine's listing, open by default, asked with their cards and by when they last moved, each row peeking its change", async () => {
  const asked = stubFetch(engine([WEB_42, change("api", 7)]));
  mount(<Changes />);
  const list = await theList();
  const listing = listings(asked)[0]!;
  expect(listing.searchParams.get("state")).toBe("open");
  expect(listing.searchParams.get("cards")).toBe("true");
  expect(listing.searchParams.get("sort")).toBe("updated");
  const title = within(list).getByRole("link", { name: "Change 7 of api" });
  expect(title.getAttribute("href")).toBe("#/gitlet/repos/api/changes/7");
  const modified = new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true });
  title.dispatchEvent(modified);
  expect(modified.defaultPrevented).toBe(false);
  fireEvent.click(title);
  expect(decodeURIComponent(location.hash)).toContain("peek=change:api!7");
});

// Mutation: drop a part of the row, or say a time as a cell's short form.
test("a row is a code host's row: its state glyph, its title and tags, the line that places it and its facts", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  stubFetch(engine([WEB_42]));
  mount(<Changes />);
  const row = rowOf(await theList(), "render: stream the product page's shell");
  expect(within(row).getByRole("img", { name: "Open" }).dataset.state).toBe("open");
  expect(within(row).getByText("Waiting on you")).toBeTruthy();
  expect(within(row).getByText("2 open findings")).toBeTruthy();
  const meta = row.querySelector(".code-change-meta")!;
  expect(meta.textContent).toContain("web!42");
  expect(meta.textContent).toContain("opened 3 hours ago by");
  expect(
    within(meta as HTMLElement)
      .getByRole("link", { name: /Engineer/ })
      .getAttribute("href"),
  ).toBe("#/gitlet?owner=dev");
  const chips = [...meta.querySelectorAll(".code-branch-chip")];
  expect(chips.map((c) => c.textContent)).toEqual(["frontend/stream-shell", "main"]);
  expect(chips.map((c) => c.getAttribute("href"))).toEqual([
    "#/gitlet/repos/web?ref=frontend%2Fstream-shell",
    "#/gitlet/repos/web?ref=main",
  ]);
  expect(meta.textContent).toContain("revision 3");
  const work = within(meta as HTMLElement).getByRole("link", { name: "ENG-415" });
  const query = new URLSearchParams(work.getAttribute("href")!.split("?")[1]);
  expect(Object.fromEntries(query)).toEqual({ state: "all", work: "worklet:t-415" });
  const updated = row.querySelector(".code-change-updated time")!;
  expect(updated.textContent).toBe("2 minutes ago");
  expect(updated.getAttribute("title")).not.toBe("");
  expect(within(row).getByRole("link", { name: "3 comments on web!42" })).toBeTruthy();
});

// Mutation: name passing checks, count every check where some are required, or drop the failed.
test("the checks fact counts the required checks and names the failed ones, required or not", async () => {
  expect(
    checksSentence({
      state: "passed",
      counts: { passed: 2, failed: 1 },
      required: 2,
      required_passed: 2,
      failing: [{ name: "lint", state: "failed", required: false }],
    }),
  ).toBe("2 of 2 required checks passed; lint failed, and is not required.");
  expect(
    checksSentence({
      state: "failing",
      counts: { passed: 1, failed: 1 },
      required: 2,
      required_passed: 1,
      failing: [{ name: "e2e", state: "failed", required: true }],
    }),
  ).toBe("e2e failed (required).");
  // THE UNNAMED REST JOIN THE LAST GROUP NAMED, and after a required one are said as failed alone.
  const failing = [
    { name: "e2e", state: "failed", required: true },
    { name: "lint", state: "failed", required: false },
  ];
  const rollup = { state: "failing", counts: { passed: 1, failed: 4 }, required: 2 };
  expect(checksSentence({ ...rollup, required_passed: 1, failing, failing_more: 2 })).toBe(
    "e2e failed (required); lint and 2 more failed, and are not required.",
  );
  expect(
    checksSentence({
      ...rollup,
      required_passed: 1,
      failing: failing.slice(0, 1),
      failing_more: 2,
    }),
  ).toBe("e2e failed (required), and 2 more failed.");
  expect(
    checksSentence({
      state: "running",
      counts: { passed: 1, running: 1 },
      required: 2,
      required_passed: 1,
    }),
  ).toBe("1 of 2 required checks passed; 1 check is running.");
  expect(
    checksSentence({
      state: "passed",
      counts: { passed: 3 },
      required: 0,
      required_passed: 0,
      more: true,
    }),
  ).toBe("3 of 3 checks passed. Counted over the checks one read holds.");
  stubFetch(
    engine([
      WEB_42,
      change("web", 9, { card: { comments: 0, reviews: 0, changes_requested: 0 } }),
      change("web", 8, { card: undefined }),
    ]),
  );
  mount(<Changes />);
  const list = await theList();
  expect(
    rowOf(list, "render: stream the product page's shell").querySelector(".code-change-checks")
      ?.textContent,
  ).toContain("1/2");
  expect(
    within(rowOf(list, "Change 9 of web")).getByText(/No check of its head is kept/),
  ).toBeTruthy();
  // A CARD NOT ANSWERED is said, never a zero.
  expect(
    within(rowOf(list, "Change 8 of web")).getAllByText("Not answered").length,
  ).toBeGreaterThan(0);
});

// Mutation: tone a person's ask as info, a failed check as warning, count anything but the engine's,
// name the meter in the engine's sentence, or the reader in the third person.
test("the readiness meter tones each condition by where it stands, named in the dashboard's words", async () => {
  const p = (kind: string, state: string, who?: string) => ({
    kind,
    state,
    detail: "",
    ...(who ? { waits_on: [{ handle: who, kind: who === "jane" ? "person" : "agent" }] } : {}),
  });
  expect(preconditionTone(p("facts", "met"))).toBe("success");
  expect(preconditionTone(p("facts", "computing"))).toBe("info");
  expect(preconditionTone(p("approval", "unmet", "reviewer"))).toBe("info");
  expect(preconditionTone(p("owners_approval", "unmet", "jane"))).toBe("warning");
  expect(preconditionTone(p("approval", "unmet"))).toBe("warning");
  expect(preconditionTone(p("check", "unmet", "dev"))).toBe("danger");
  expect(preconditionTone(p("mergeable", "unmet"))).toBe("danger");
  stubFetch(
    engine([
      WEB_42,
      change("web", 45, {
        state: "draft",
        readiness: readiness([{ kind: "draft", state: "unmet" }]),
      }),
      change("web", 41, { state: "merged", readiness: undefined }),
    ]),
  );
  mount(<Changes />);
  const list = await theList();
  const said =
    "3 of 5 conditions to merge met. Revision 3 touches paths whose owners have not all approved. Waiting on you.";
  const meter = within(rowOf(list, "render: stream the product page's shell")).getByRole("img", {
    name: said,
  });
  expect(meter.closest(".code-readiness")?.getAttribute("title")).toBe(said);
  expect(
    [...meter.querySelectorAll<HTMLElement>(".crewlet-segmented-meter__segment")].map(
      (s) => s.dataset.tone,
    ),
  ).toEqual(["success", "warning", "success", "info", "success"]);
  expect(
    within(rowOf(list, "render: stream the product page's shell")).getByText("3/5"),
  ).toBeTruthy();
  expect(
    within(rowOf(list, "Change 45 of web")).getByText("Draft", { selector: ".code-change-draft" }),
  ).toBeTruthy();
  expect(rowOf(list, "Change 41 of web").querySelector(".code-readiness")).toBeNull();
});

// A STATE THIS BUILD DOES NOT KNOW is said by its word on a dashed circle and an outlined pill.
// Mutation: index the state's glyphs whatever the state, and the row and the pill throw.
test("a change in a state this build does not know is drawn by its word", async () => {
  stubFetch(engine([change("web", 90, { state: "archived", readiness: undefined })]));
  mount(<Changes />);
  const row = rowOf(await theList(), "Change 90 of web");
  const lead = within(row).getByRole("img", { name: "archived" });
  expect(lead.classList.contains("code-state-glyph")).toBe(true);
  expect(lead.dataset.state).toBeUndefined();
  expect(lead.querySelector("svg")).not.toBeNull();
  cleanup();
  const { container } = render(<ChangeStateTag state="archived" />);
  expect(container.textContent).toBe("archived");
  expect(container.querySelector(".code-state")).toBeNull();
});

// Mutation: drop the verdict, corner a comment, or ring a face that is not the reader's.
test("a reviewer's face wears the kit's verdict corner, and the reader's own face its ring", async () => {
  const many = change("web", 50, {
    card: {
      comments: 0,
      reviews: 4,
      changes_requested: 0,
      reviewers: ["cto", "dev", "reviewer", "jane"].map((handle) => ({
        handle,
        kind: "agent",
        verdict: "approve",
        verdict_revision: 1,
      })),
      reviewers_omitted: 1,
    },
  });
  stubFetch(engine([WEB_42, many, change("web", 51)]));
  mount(<Changes />);
  const list = await theList();
  const faces = rowOf(list, "render: stream the product page's shell").querySelector<HTMLElement>(
    ".code-change-faces",
  )!;
  expect(faces.querySelectorAll(".crewlet-avatar__status--changes-requested")).toHaveLength(1);
  expect(faces.querySelectorAll(".crewlet-avatar__status--approved")).toHaveLength(0);
  expect(faces.querySelectorAll('[class*="ring-brand"]')).toHaveLength(1);
  expect(faces.getAttribute("aria-label")).toBe(
    "Reviewers: Code Reviewer requested changes on revision 1; CTO commented on revision 3; You were asked to review: it waits on you",
  );
  expect(within(rowOf(list, "Change 50 of web")).getByText("+2")).toBeTruthy();
  expect(
    within(rowOf(list, "Change 51 of web")).getByRole("img", { name: "No reviewer asked" }),
  ).toBeTruthy();
});

// Mutation: drop a tag, tone the waiting queue entry as info, or misname the ejection.
test("the title's tags say the queue, an ejection, changes requested, open findings and a draft", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  const queued = (n: number, place: number, state: string) =>
    change("web", n, {
      readiness: readiness([
        { kind: "facts", state: "met" },
        {
          kind: "queue",
          state: "computing",
          queue: { entry_id: `e${n}`, target_ref: "refs/heads/main", state, place },
        },
      ]),
    });
  const ejected = change("api", 17, {
    card: {
      comments: 0,
      reviews: 0,
      changes_requested: 0,
      ejected: { reason: "check_failed", at: minutesAgo(20), checks: ["e2e"] },
    },
  });
  stubFetch(
    engine([
      WEB_42,
      queued(44, 1, "testing"),
      queued(43, 2, "waiting"),
      ejected,
      change("web", 45, { state: "draft" }),
    ]),
  );
  mount(<Changes />);
  const list = await theList();
  const first = within(rowOf(list, "Change 44 of web"))
    .getByText("In the queue · #1")
    .closest("[title]")!;
  expect(first.getAttribute("title")).toBe("First in main's merge queue, being tested");
  const second = within(rowOf(list, "Change 43 of web"))
    .getByText("In the queue · #2")
    .closest("[title]")!;
  expect(second.getAttribute("title")).toBe("Second in main's merge queue, waiting");
  expect(second.className).not.toBe(first.className);
  const out = within(rowOf(list, "Change 17 of api"))
    .getByText("Taken out of the queue")
    .closest("[title]")!;
  expect(out.getAttribute("title")).toBe("Taken out of main's queue 20 minutes ago: e2e failed");
  const asked = within(rowOf(list, "render: stream the product page's shell"))
    .getByText("Changes requested")
    .closest("[title]")!;
  expect(asked.getAttribute("title")).toBe("Code Reviewer requested changes on revision 1");
  expect(
    within(rowOf(list, "Change 45 of web")).getByText("Draft", { selector: ".code-change-tags *" }),
  ).toBeTruthy();
});

// Mutation: read Waiting on you from the card alone, or from the readiness alone.
test("Waiting on you comes from the card and from the readiness", async () => {
  const fromReadiness = change("web", 60, {
    readiness: readiness([
      { kind: "approval", state: "unmet", waits_on: [{ handle: "jane", kind: "person" }] },
    ]),
  });
  stubFetch(engine([WEB_42, fromReadiness, change("web", 61)]));
  mount(<Changes />);
  const list = await theList();
  expect(
    within(rowOf(list, "render: stream the product page's shell")).getByText("Waiting on you"),
  ).toBeTruthy();
  expect(within(rowOf(list, "Change 60 of web")).getByText("Waiting on you")).toBeTruthy();
  expect(within(rowOf(list, "Change 61 of web")).queryByText("Waiting on you")).toBeNull();
});

// Mutation: count the tabs from the rows, or ask the counts without the picked repository.
test("the state tabs count the scope from code_counts, and a picked repository narrows both", async () => {
  const asked = stubFetch(engine([WEB_42]));
  mount(<Changes />);
  const tabs = await screen.findByRole("radiogroup", { name: "Change state" });
  expect(await within(tabs).findByText("54")).toBeTruthy();
  for (const word of ["Open", "Draft", "Merged", "Closed"]) {
    expect(within(tabs).getByRole("radio", { name: new RegExp(word) })).toBeTruthy();
  }
  expect(within(tabs).getByText("4")).toBeTruthy();
  expect(within(tabs).getByText("3")).toBeTruthy();
  const counts = asked.filter((u) => u.pathname === "/query/code_counts");
  expect(counts[0]!.searchParams.get("parts")).toBe("changes");
  expect(counts[0]!.searchParams.has("repo")).toBe(false);
  cleanup();
  const again = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?repo=web");
  await theList();
  const narrowed = again.filter((u) => u.pathname === "/query/code_counts")[0]!;
  expect(narrowed.searchParams.get("repo")).toBe("web");
  expect(listings(again)[0]!.searchParams.get("repo")).toBe("web");
});

// Mutation: let a refused count take the list with it, or draw a zero.
test("a refused count leaves the tabs without figures and the list whole", async () => {
  stubFetch(engine([WEB_42]), () =>
    json({ error: "unavailable", detail: "this node does not answer it yet" }, 503),
  );
  mount(<Changes />);
  const list = await theList();
  expect(
    within(list).getByRole("link", { name: "render: stream the product page's shell" }),
  ).toBeTruthy();
  const tabs = screen.getByRole("radiogroup", { name: "Change state" });
  expect(within(tabs).queryByText("0")).toBeNull();
  expect(
    within(tabs).getByRole("radio", { name: /Open/ }).closest("[title]")?.getAttribute("title"),
  ).toBe("Counts are unavailable on this fleet yet");
});

// Mutation: offer every state where nothing narrows, or send a tab as anything but the state.
test("a state tab is the engine's state, and Every state is offered only where something narrows the walk", async () => {
  const asked = stubFetch(engine([WEB_42]));
  mount(<Changes />);
  const tabs = await screen.findByRole("radiogroup", { name: "Change state" });
  expect(within(tabs).queryByRole("radio", { name: "Every state" })).toBeNull();
  fireEvent.click(within(tabs).getByRole("radio", { name: /Merged/ }));
  expect(location.hash).toContain("state=merged");
  await settled(() => listings(asked).length);
  expect(listings(asked).at(-1)!.searchParams.get("state")).toBe("merged");
  cleanup();
  stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?repo=web");
  const narrowed = await screen.findByRole("radiogroup", { name: "Change state" });
  expect(within(narrowed).getByRole("radio", { name: "Every state" })).toBeTruthy();
});

/** Picks a sort through the Sort select. */
function pickSort(name: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "Sort" }));
  fireEvent.mouseDown(screen.getByRole("option", { name }));
}

// Mutation: send a sort the engine refuses, write the default into the address, misstate
// the walk's own order, or sort an owner's listing by its last move by default.
test("Sort lists a live state by its last move or its opening, and says the walk's own order where it cannot", async () => {
  const none = {
    state: "open",
    needs: "",
    repo: "",
    owner: "",
    reviewer: "",
    work: "",
    window: "",
  };
  const byMove = { send: "updated", choosable: true, shown: "updated", byDefault: "updated" };
  expect(changeOrder(none, "")).toEqual(byMove);
  expect(changeOrder(none, "opened")).toEqual({ ...byMove, send: "opened", shown: "opened" });
  // AN OWNER OR A WORK ITEM keeps its own index, by opening, unless the reader asks otherwise.
  expect(changeOrder({ ...none, state: "draft", owner: "dev" }, "")).toEqual({
    send: "opened",
    choosable: true,
    shown: "opened",
    byDefault: "opened",
  });
  expect(changeOrder({ ...none, work: "worklet:t-1" }, "updated").send).toBe("updated");
  expect(changeOrder({ ...none, reviewer: "jane" }, "opened")).toMatchObject({
    send: undefined,
    shown: "Newest opened",
    why: "Newest opened first: what waits on somebody is listed by when each change was opened",
  });
  expect(changeOrder({ ...none, needs: "person" }, "").shown).toBe("Newest opened");
  expect(changeOrder({ ...none, needs: "me" }, "")).toMatchObject({
    send: undefined,
    shown: "Reviews, then fixes",
  });
  expect(changeOrder({ ...none, state: "merged" }, "")).toMatchObject({
    send: undefined,
    why: "Newest opened first: ended changes are listed by when they were opened",
  });
  expect(changeOrder({ ...none, state: "merged", window: "7d" }, "")).toMatchObject({
    send: undefined,
    why: "Most recently moved first: a window lists its changes by when they last moved",
  });
  // AN OWNER LEADS AN ENDED STATE'S WALK ahead of the window, by opening.
  expect(changeOrder({ ...none, state: "merged", window: "7d", owner: "dev" }, "").shown).toBe(
    "Newest opened",
  );
  const asked = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?reviewer=jane");
  await theList();
  expect(listings(asked)[0]!.searchParams.has("sort")).toBe(false);
  expect(
    screen.getByRole("combobox", { name: "Sort" }).getAttribute("aria-disabled") ?? "",
  ).not.toBe("false");
  cleanup();
  const merged = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?state=merged");
  await theList();
  expect(listings(merged)[0]!.searchParams.has("sort")).toBe(false);
  cleanup();
  const newest = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?sort=opened");
  await theList();
  expect(listings(newest)[0]!.searchParams.get("sort")).toBe("opened");
  // EACH DEFAULT WRITES NO KEY: the address holds the reader's choice alone.
  pickSort("Sort: Recently updated");
  expect(location.hash).not.toContain("sort=");
  pickSort("Sort: Newest");
  expect(location.hash).toContain("sort=opened");
  cleanup();
  const owned = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?owner=dev");
  await theList();
  expect(listings(owned)[0]!.searchParams.get("sort")).toBe("opened");
  pickSort("Sort: Recently updated");
  expect(location.hash).toContain("sort=updated");
  pickSort("Sort: Newest");
  expect(location.hash).not.toContain("sort=");
});

// Mutation: read the first page alone, and the list says "none" over a change it never read.
test("a short sorted page is filled before the list says anything", async () => {
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.has("limit")) return page([]);
    return url.searchParams.get("cursor") === "c2" ? page([WEB_42]) : page([], { next: "c2" });
  });
  mount(<Changes />);
  const list = await theList();
  expect(
    within(list).getByRole("link", { name: "render: stream the product page's shell" }),
  ).toBeTruthy();
  expect(listings(asked).some((u) => u.searchParams.get("cursor") === "c2")).toBe(true);
  expect(screen.queryByText("No open changes")).toBeNull();
});

// Mutation: offer handles rather than the chart's names, or drop the address's seat.
test("the Owner and Reviewer pickers narrow by a seat from the chart", async () => {
  const asked = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?owner=dev");
  await theList();
  expect(listings(asked)[0]!.searchParams.get("owner")).toBe("dev");
  expect(document.querySelector('[aria-label="Owner"]')?.textContent).toContain("Engineer");
  expect(document.querySelector('[aria-label="Reviewer"]')?.textContent).toContain("Reviewer");
});

// Mutation: keep density in a preference, draw compact rows at the default padding, or write the default.
test("the density toggle keeps compact rows in the address", async () => {
  stubFetch(engine([WEB_42]));
  mount(<Changes />);
  const list = await theList();
  expect(list.closest(".code-list--compact")).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: "Compact rows" }));
  expect(location.hash).toContain("density=compact");
  expect((await theList()).closest(".code-list--compact")).not.toBeNull();
  // THE DEFAULT WRITES NO KEY.
  fireEvent.click(screen.getByRole("radio", { name: "Comfortable rows" }));
  expect(location.hash).not.toContain("density=");
});

// Mutation: draw the box from the list's rows, say the wrong reason, or make every action primary.
test("Waiting on you lists the engine's needs=me with each change's reason, and is absent where nothing waits", async () => {
  const fix = change("web", 70, {
    owner: "jane",
    owner_kind: "person",
    card: {
      comments: 0,
      reviews: 1,
      changes_requested: 1,
      reviewers: [
        { handle: "reviewer", kind: "agent", verdict: "request_changes", verdict_revision: 1 },
      ],
    },
  });
  const findings = change("web", 71, { owner: "jane", owner_kind: "person", open_findings: 2 });
  const requested = change("web", 72, {
    card: {
      comments: 0,
      reviews: 0,
      changes_requested: 0,
      reviewers: [{ handle: "jane", kind: "person", asked: true, reason: "requested" }],
    },
  });
  const asked = stubFetch(engine([change("api", 7)], [WEB_42, fix, findings, requested]));
  mount(<Changes />);
  const box = await screen.findByRole("region", { name: "Waiting on you" });
  const ask = boxAsks(asked)[0]!;
  expect(ask.searchParams.get("needs")).toBe("me");
  expect(ask.searchParams.get("cards")).toBe("true");
  expect(ask.searchParams.get("limit")).toBe("5");
  expect(
    within(box).getByText("Engineer's change touches paths you own: your review is asked"),
  ).toBeTruthy();
  expect(
    within(box).getByText("Code Reviewer requested changes on revision 1: it waits on your fix"),
  ).toBeTruthy();
  expect(within(box).getByText("2 open findings wait on your answer")).toBeTruthy();
  expect(within(box).getByText("Engineer asks your review")).toBeTruthy();
  const review = within(box).getAllByRole("link", { name: "Review" })[0]!;
  expect(review.getAttribute("href")).toBe("#/gitlet/repos/web/changes/42?tab=files");
  expect(review.className).not.toContain("primary");
  expect(within(box).getAllByRole("link", { name: "Open" })).toHaveLength(2);

  cleanup();
  stubFetch(engine([change("api", 7)], [WEB_42]));
  mount(<Changes />);
  const one = await screen.findByRole("region", { name: "Waiting on you" });
  expect(within(one).getByRole("link", { name: "Review" }).className).toContain("primary");

  cleanup();
  const quiet = stubFetch(engine([change("api", 7)], []));
  mount(<Changes />);
  await theList();
  await settled(() => boxAsks(quiet).length);
  expect(screen.queryByRole("region", { name: "Waiting on you" })).toBeNull();
});

// Mutation: ask needs=me of a reader bound to no seat, or give its reason in the engine's sentence.
test("a reader bound to no seat sees what waits on a person instead", async () => {
  const asked = stubFetch(engine([change("api", 7)], [WEB_42]));
  mount(<Changes />, "#/gitlet", { ...ADMIN_VIEWER, handle: "", owner: "" });
  const box = await screen.findByRole("region", { name: "Waiting on a person" });
  expect(
    within(box).getByRole("link", { name: "render: stream the product page's shell" }),
  ).toBeTruthy();
  expect(boxAsks(asked).every((u) => u.searchParams.get("needs") === "person")).toBe(true);
  // ITS REASON IS THE DASHBOARD'S SENTENCE of what is next, never the engine's.
  expect(
    within(box).getByText(
      "Revision 3 touches paths whose owners have not all approved. Waiting on Jane Founder.",
    ),
  ).toBeTruthy();
  expect(box.textContent).not.toContain("engine words");
});

// Mutation: draw nothing where the box was refused.
test("the box says its refusal rather than nothing", async () => {
  stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.has("limit")) {
      return json({ error: "bad_params", detail: "needs=me is not read here" }, 400);
    }
    return page([WEB_42]);
  });
  mount(<Changes />);
  const box = await screen.findByRole("region", { name: "Waiting on you" });
  expect(await within(box).findByText(/needs=me is not read here/)).toBeTruthy();
});

// Mutation: drop the list's keys, let the box take j on landing, or let Enter go anywhere but
// the cursor row's peek.
test("j, k and Enter walk the rows, the page's list on landing", async () => {
  let release = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.has("limit")) return page([change("web", 70)]);
    return held.then(() => page([WEB_42, change("api", 7)]));
  });
  mount(<Changes />);
  // THE BOX'S ROWS ARRIVE FIRST, so which list j drives is never the order they arrived in.
  await screen.findByRole("link", { name: "Change 70 of web" });
  release();
  const list = await theList();
  await within(list).findByRole("link", { name: "Change 7 of api" });
  await afterMicrotasks();
  // NO PRESS YET: the page's list drives the keys, never the box above it.
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(
    within(list).getByRole("link", { name: "render: stream the product page's shell" }),
  );
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(within(list).getByRole("link", { name: "Change 7 of api" }));
  fireEvent.keyDown(window, { key: "k" });
  const cursor = within(list).getByRole("link", {
    name: "render: stream the product page's shell",
  });
  expect(document.activeElement).toBe(cursor);
  // ENTER AWAY FROM THE LINK opens the cursor row's peek; on the link it is the browser's.
  act(() => cursor.blur());
  fireEvent.keyDown(window, { key: "Enter" });
  expect(decodeURIComponent(location.hash)).toContain("peek=change:web!42");
  // THE BOX ONCE THE READER IS IN IT.
  const box = screen.getByRole("list", { name: "Waiting on you" });
  fireEvent.pointerDown(box);
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(within(box).getByRole("link", { name: "Change 70 of web" }));
});

// ] FOLLOWS THE PAGE'S LIST, which publishes the one order; the box publishes none.
// Mutation: let the box publish its order too, and ] from web!42 lands in the box.
test("] from a peeked row steps to the list's next row, past the box", async () => {
  stubFetch(engine([WEB_42, change("api", 7)], [change("web", 70)]));
  location.hash = "#/gitlet?peek=change:web!42";
  await act(async () => {
    mount(
      <PeekNeighbours>
        <Changes />
        <Suspense fallback={null}>
          <PeekHost />
        </Suspense>
      </PeekNeighbours>,
      "#/gitlet?peek=change:web!42",
    );
  });
  const list = await theList();
  await screen.findByRole("link", { name: "Change 70 of web" });
  const first = within(list).getByRole("link", { name: "render: stream the product page's shell" });
  act(() => first.focus());
  await act(async () => {
    fireEvent.keyDown(document.activeElement!, { key: "]" });
  });
  await waitFor(() => expect(decodeURIComponent(location.hash)).toContain("peek=change:api!7"));
});

// THE BOX LISTS THE CHANGES ITS LISTING KEEPS, NOT THE ROWS ONE PAGE READ.
// Mutation: read the box's first page alone, and it draws nothing over a change that waits.
test("Waiting on you follows next past a page that kept nothing to the change behind it", async () => {
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (!url.searchParams.has("limit")) return page([change("api", 7)]);
    return url.searchParams.get("cursor") === "more" ? page([WEB_42]) : page([], { next: "more" });
  });
  mount(<Changes />);
  const box = await screen.findByRole("region", { name: "Waiting on you" });
  expect(
    within(box).getByRole("link", { name: "render: stream the product page's shell" }),
  ).toBeTruthy();
  const followed = boxAsks(asked).find((u) => u.searchParams.get("cursor") === "more")!;
  expect(followed.searchParams.get("needs")).toBe("me");
  expect(followed.searchParams.get("cards")).toBe("true");
  expect(followed.searchParams.get("limit")).toBe("5");
});

// SHOW THEM ALL ASKS THE BOX'S OWN LISTING, every live state, so a draft it listed stays listed.
// Mutation: write needs= alone, and the list keeps state=open and drops the draft.
test("Show them all lists what the box lists, a draft among them", async () => {
  const draft = change("web", 46, {
    state: "draft",
    card: {
      comments: 0,
      reviews: 0,
      changes_requested: 0,
      reviewers: [{ handle: "jane", kind: "person", asked: true, reason: "requested" }],
    },
  });
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.has("limit")) return page([draft]);
    return page(url.searchParams.get("needs") === "me" ? [draft] : [change("api", 7)]);
  });
  mount(<Changes />);
  const box = await screen.findByRole("region", { name: "Waiting on you" });
  fireEvent.click(within(box).getByRole("button", { name: "Show them all in the list" }));
  expect(location.hash).toContain("needs=me");
  expect(location.hash).toContain("state=all");
  expect(
    await within(await theList()).findByRole("link", { name: "Change 46 of web" }),
  ).toBeTruthy();
  const last = listings(asked).at(-1)!;
  expect(last.searchParams.get("needs")).toBe("me");
  expect(last.searchParams.has("state")).toBe(false);
});

// Mutation: say the scope's total under a filter it does not count, or count the page as the total.
test("the foot says how many it shows of the scope's total, and only of it", async () => {
  const none = {
    state: "open",
    needs: "",
    repo: "",
    owner: "",
    reviewer: "",
    work: "",
    window: "",
  };
  expect(footWords(none, 5, 5, false)).toBe("Showing 5 of 5 open changes");
  expect(footWords({ ...none, state: "merged" }, 25, 54, true)).toBe(
    "Showing 25 of 54 merged changes",
  );
  expect(footWords({ ...none, owner: "dev" }, 2, 4, false)).toBe(
    "Showing 2 open changes that match the filters",
  );
  expect(footWords({ ...none, state: "merged" }, 55, 54, false)).toBe("Showing 55 merged changes");
  expect(footWords(none, 0, 4, false)).toBeUndefined();
  stubFetch(engine([WEB_42]));
  mount(<Changes />);
  await theList();
  expect(await screen.findByText("Showing 1 of 4 open changes")).toBeTruthy();
});

// Mutation: drop the cursor, or ask the next page with other parameters.
test("Load more pages on in the engine's order", async () => {
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.has("limit")) return page([]);
    return url.searchParams.get("cursor") === "p2"
      ? page([change("api", 7)])
      : page([WEB_42], { next: "p2" });
  });
  mount(<Changes />);
  await theList();
  fireEvent.click(await screen.findByRole("button", { name: "Load more changes" }));
  expect(await screen.findByRole("link", { name: "Change 7 of api" })).toBeTruthy();
  const next = listings(asked).find((u) => u.searchParams.get("cursor") === "p2")!;
  expect(next.searchParams.get("cards")).toBe("true");
  expect(next.searchParams.get("sort")).toBe("updated");
});

// NEW CHANGE is the picked repository's comparison, else a picker of the repository in its popover.
// Mutation: link New change anywhere but the comparison, or choose a repository that goes nowhere.
test("New change opens the picked repository's comparison, or picks the repository first", async () => {
  stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?repo=web");
  const link = await screen.findByRole("link", { name: "New change" });
  expect(link.getAttribute("href")).toBe("#/gitlet/repos/web/compare");
  cleanup();
  const repos = json({
    repos: [{ name: "api" }, { name: "web" }],
    position: POSITION,
    level: "session",
  });
  stubFetch((url) =>
    url.pathname === "/query/code_repos" ? repos.clone() : engine([WEB_42])(url),
  );
  mount(<Changes />);
  fireEvent.click(await screen.findByRole("button", { name: "New change" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "web" }));
  expect(location.hash).toBe("#/gitlet/repos/web/compare");
});

// THE WINDOW IS THE ENGINE'S `window=`, and narrows the walk, so every state may be asked beside it.
test("a window is the engine's window, and lets every state be asked", async () => {
  const asked = stubFetch(engine([]));
  mount(<Changes />, "#/gitlet?window=7d&state=all");
  await settled(() => listings(asked).length);
  const listing = listings(asked)[0]!;
  expect(listing.searchParams.get("window")).toBe("7d");
  expect(listing.searchParams.has("state")).toBe(false);
  expect(listing.searchParams.has("sort")).toBe(false);
});

// WHAT A CHANGE WAITS ON IS THE ENGINE'S `needs`, sent as it reads it.
test("the waiting-on filter is the engine's needs", async () => {
  const asked = stubFetch(engine([]));
  mount(<Changes />, "#/gitlet?needs=me");
  expect(await screen.findByText("Nothing waits on you")).toBeTruthy();
  expect(listings(asked)[0]!.searchParams.get("needs")).toBe("me");
  // THE LIST IS ITSELF needs=me, so no box asks it again.
  expect(boxAsks(asked)).toEqual([]);
});

// EVERY STATE WHERE NOTHING NARROWS THE WALK IS SAID IN PLACE, NEVER SENT.
test("every state with nothing narrowing the walk is said in place and never asked", async () => {
  const asked = stubFetch(engine([]));
  mount(<Changes />, "#/gitlet?state=all");
  expect(await screen.findByText(/Every state is listed for one repository/)).toBeTruthy();
  await settled(() => asked.length);
  expect(listings(asked)).toHaveLength(0);
  // THE TABS STAY, so the reader can pick a state from where they are.
  expect(screen.getByRole("radiogroup", { name: "Change state" })).toBeTruthy();
});

// THE PARAMETERS ARE THE ENGINE'S FILTERS, and what cannot be asked says why.
// Mutation: send a pairing validateChanges refuses (a need beside an ended state, needs=me
// beside an owner or a reviewer, a fix or a person beside a reviewer).
test("a listing's parameters are the engine's filters, and a name no repository has is said", () => {
  const none = {
    state: "open",
    needs: "",
    repo: "",
    owner: "",
    reviewer: "",
    work: "",
    window: "",
  };
  expect(changesParams(none)).toEqual({ params: { state: "open" } });
  expect(changesParams({ ...none, state: "all", repo: "web" })).toEqual({
    params: { repo: "web" },
  });
  expect(changesParams({ ...none, needs: "person", owner: "dev" })).toEqual({
    params: { state: "open", needs: "person", owner: "dev" },
  });
  expect(changesParams({ ...none, repo: "Web" })).toHaveProperty("unasked");
  expect(changesParams({ ...none, needs: "soon" })).toHaveProperty("unasked");
  expect(changesParams({ ...none, state: "all" })).toHaveProperty("unasked");
  expect(changesParams({ ...none, state: "all", window: "24h" })).toEqual({
    params: { window: "24h" },
  });
  expect(changesParams({ ...none, window: "1y" })).toHaveProperty("unasked");
  // A PAIRING THE ENGINE REFUSES is said in the screen's words, never sent.
  for (const refused of [
    { ...none, needs: "me", owner: "dev" },
    { ...none, needs: "me", reviewer: "cto" },
    { ...none, needs: "fix", reviewer: "cto" },
    { ...none, needs: "person", reviewer: "cto" },
    { ...none, needs: "review", state: "merged" },
    { ...none, needs: "fix", state: "closed", owner: "dev" },
  ]) {
    expect(changesParams(refused), JSON.stringify(refused)).toHaveProperty("unasked");
  }
  expect(changesParams({ ...none, needs: "review", reviewer: "cto" })).toEqual({
    params: { state: "open", needs: "review", reviewer: "cto" },
  });
});

// Mutation: send needs=me beside an owner, and the list draws the engine's refusal.
test("a pairing the engine refuses is said in place and never asked", async () => {
  const asked = stubFetch(engine([WEB_42]));
  mount(<Changes />, "#/gitlet?needs=me&owner=dev");
  expect(await screen.findByText(/Waiting on me names no owner or reviewer/)).toBeTruthy();
  await settled(() => asked.length);
  expect(listings(asked)).toHaveLength(0);
});

// AN OWNER'S ASK NAMES THE PATHS THE READER OWNS, and facts older than them say less, never a guess.
// Mutation: drop the paths, count the omitted ones wrong, or guess paths where none are named.
test("an owner's ask names the paths the reader owns, and says less where its facts predate them", async () => {
  const asked = (n: number, owned_paths?: string[], owned_paths_omitted?: number) =>
    change("web", n, {
      card: {
        comments: 0,
        reviews: 0,
        changes_requested: 0,
        reviewers: [
          {
            handle: "jane",
            kind: "person",
            asked: true,
            reason: "path_owner",
            owned_paths,
            owned_paths_omitted,
          },
        ],
      },
    });
  stubFetch(
    engine(
      [asked(80, ["internal/server"])],
      [
        asked(80, ["internal/server"]),
        asked(81, ["internal/server", "internal/render"]),
        asked(82, ["internal/server", "internal/render"], 2),
        asked(83),
      ],
    ),
  );
  mount(<Changes />);
  const box = await screen.findByRole("region", { name: "Waiting on you" });
  expect(
    within(box).getByText(
      "Engineer's change touches internal/server, which you own: your review is asked",
    ),
  ).toBeTruthy();
  expect(
    within(box).getByText(
      "Engineer's change touches internal/server and internal/render, which you own: your review is asked",
    ),
  ).toBeTruthy();
  expect(
    within(box).getByText(
      "Engineer's change touches internal/server, internal/render and 2 more, which you own: your review is asked",
    ),
  ).toBeTruthy();
  expect(
    within(box).getByText("Engineer's change touches paths you own: your review is asked"),
  ).toBeTruthy();
  const list = await theList();
  const faces = rowOf(list, "Change 80 of web").querySelector(".code-change-faces")!;
  expect(faces.getAttribute("aria-label")).toBe(
    "Reviewers: You were asked to review: it waits on you, as the owner of internal/server",
  );
});
