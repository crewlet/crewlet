/**
 * A repository's rail beside its code at the root: About as sentences, its open changes, who
 * pushes to it and its pushes on the reader's own days, its mirror and its newest tag.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { fmtBytes } from "~/lib/format.ts";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { RepositoryPage } from "./RepositoryPage.tsx";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  pinZoneForTest("");
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const HEAD = "c71d0e9f0123456789abcdef0123456789abcdef";
const PAGE = { position: POSITION, level: "session" };

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "Frontend Engineer", handle: "frontend" },
    { name: "Backend Engineer", handle: "backend" },
    { name: "Site Reliability Engineer", handle: "sre" },
    { name: "CTO", handle: "cto" },
    { name: "Code Reviewer", handle: "reviewer" },
  ],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

/** The repository as `code_repo` answers it, with what a case overrides. */
function detail(extra: Record<string, unknown> = {}) {
  return {
    id: "id-web",
    name: "web",
    status: "active",
    managed_by: "config",
    kind: "project",
    owner: { kind: "unit", ref: "engineering" },
    visibility: "company",
    search: "indexed",
    default_branch: "main",
    description: "The storefront",
    refs: 3,
    live_packs: 4,
    live_bytes: 19_293_798,
    live_objects: 1204,
    position: POSITION,
    access: "maintain",
    head: HEAD,
    pushes: 214,
    last_push_at: "2026-11-16T09:35:00Z",
    branches: 6,
    tags: 3,
    tombstones: 0,
    created_at: "2026-11-01T10:00:00Z",
    protections: [{ pattern: "refs/heads/main", direct_push: "none" }],
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

type Answer = (url: URL) => Response;

/** The engine: the repository, a root holding a README and a LICENSE, and `answers` for the rest. */
function stubEngine(answers: Record<string, Answer> = {}, repo = detail()) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      const own = answers[url.pathname.replace("/query/", "")];
      if (own) return own(url);
      switch (url.pathname) {
        case "/query/code_repo":
          return json({ repo, ...PAGE });
        case "/query/code_tree":
          return json({
            repo: "web",
            kind: "tree",
            resolved: { ref: { ref: "refs/heads/main", oid: HEAD } },
            tree: {
              tree: HEAD,
              entries: [
                { mode: "100644", type: "blob", oid: HEAD, path: "LICENSE", size: 20 },
                { mode: "100644", type: "blob", oid: HEAD, path: "README.md", size: 20 },
              ],
              total: 2,
            },
            ...PAGE,
          });
        case "/query/code_blob":
          return json({
            repo: "web",
            kind: "blob",
            blob: {
              path: url.searchParams.get("path"),
              oid: HEAD,
              size: 9,
              lines: 1,
              text: "# web\n",
            },
          });
        case "/query/code_changes":
          return json({ ...PAGE });
      }
      return new Promise<Response>(() => {});
    }),
  );
  return asked;
}

/** The facts read answered with `facts`, the page's own read with the repository alone. */
const withFacts =
  (facts: Record<string, unknown>, repo = detail()): Answer =>
  (url) =>
    json({ repo: { ...repo, ...(url.searchParams.has("day_bounds") ? facts : {}) }, ...PAGE });

function mount(hash = "#/gitlet/repos/web") {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <RepositoryPage repo="web" />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const reads = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);
const section = (name: string) => screen.findByRole("region", { name });
/** A fact's whole sentence, found by its first words. */
const fact = async (start: RegExp) =>
  (await screen.findByText(start)).closest(".code-repo-rail__fact")!;

/** The clock at `iso`, read at once as a tab coming back reads it. */
function clockAt(iso: string) {
  act(() => {
    vi.setSystemTime(new Date(iso));
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

// ABOUT SAYS ITS FACTS AS SENTENCES, every time in words, and repeats no description. Mutation:
// draw `created_at` with fmtDateTime, or the owner by its key, and this fails.
test("About says the repository's facts as sentences, each time in words", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine();
  mount();
  const about = await section("About");
  const owner = await fact(/^Owned by/);
  expect(owner.textContent).toBe("Owned by Engineering");
  expect(
    within(owner as HTMLElement)
      .getByRole("link")
      .getAttribute("href"),
  ).toMatch(/peek=unit%3Aengineering/);
  expect((await fact(/reads it$/)).textContent).toBe("Every seat reads it");
  expect(within(about).getByText("Indexed for code search")).toBeTruthy();
  const size = await fact(/in 4 packs$/);
  expect(size.textContent).toBe(`${fmtBytes(19_293_798)} in 4 packs`);
  expect(size.getAttribute("title")).toBe("1,204 objects");
  expect((await fact(/^Created/)).textContent).toBe("Created 15 days ago");
  expect((await fact(/^Last pushed/)).textContent).toBe("Last pushed 25 minutes ago");
  expect(within(about).getByText("Declared in the company document")).toBeTruthy();
  expect(within(about).queryByText("The storefront")).toBeNull();
  expect(
    within(about)
      .getByRole("link", { name: "Open the repository's settings" })
      .getAttribute("href"),
  ).toBe("#/gitlet/repos/web?tab=settings");
});

// THE PROTECTION IS THE DEFAULT BRANCH'S OWN RULES, in their clauses, and where they are not read
// the repository's count of rules. Mutation: say "protected" off a glob of the client's, and the
// refused case fails.
test("About names the default branch's protection from its rules, and counts the rules where they are not read", async () => {
  const find = (row: Record<string, unknown>) => () =>
    json({
      refs_detail: [{ ref: "refs/heads/main", oid: HEAD, position: POSITION, ...row }],
      ...PAGE,
    });
  const asked = stubEngine({
    code_refs: find({ protection: { patterns: ["refs/heads/main"], direct_push: "none" } }),
  });
  mount();
  expect((await fact(/is protected/)).textContent).toBe(
    "main is protected: no force-push, no delete, pushes through changes only",
  );
  const read = reads(asked, "code_refs").find((u) => u.searchParams.get("detail") === "true")!;
  expect(read.searchParams.get("contains")).toBe("main");

  cleanup();
  stubEngine({ code_refs: find({}) });
  mount();
  expect((await fact(/has no protection rule$/)).textContent).toBe("main has no protection rule");

  cleanup();
  stubEngine({ code_refs: () => engineJson({ error: "unavailable", detail: "no refs" }, 503) });
  mount();
  const counted = await screen.findByRole("link", { name: "1 protection rule" });
  expect(counted.getAttribute("href")).toBe("#/gitlet/repos/web?tab=settings");
});

// README AND LICENSE IN ABOUT ARE THE CARD'S TABS: pressing one chooses it, writes the address and
// takes the focus there. Mutation: link LICENSE without `doc=license`, and this fails.
test("Readme and LICENSE in About open their tab of the README card", async () => {
  stubEngine();
  mount();
  const about = await section("About");
  const license = await within(about).findByRole("link", { name: "LICENSE" });
  expect(license.getAttribute("href")).toBe("#/gitlet/repos/web?doc=license");
  expect(within(about).getByRole("link", { name: "Readme" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web",
  );
  fireEvent.click(license);
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web?doc=license"));
  expect(document.activeElement?.getAttribute("role")).toBe("tab");
  expect(document.activeElement?.textContent).toBe("LICENSE");

  cleanup();
  stubEngine({
    code_tree: () =>
      json({
        repo: "web",
        kind: "tree",
        resolved: { ref: { ref: "refs/heads/main", oid: HEAD } },
        tree: {
          tree: HEAD,
          entries: [{ mode: "100644", type: "blob", oid: HEAD, path: "go.mod", size: 9 }],
          total: 1,
        },
        ...PAGE,
      }),
  });
  mount();
  await screen.findByRole("link", { name: "go.mod" });
  const bare = await section("About");
  expect(within(bare).queryByRole("link", { name: "Readme" })).toBeNull();
  expect(within(bare).queryByRole("link", { name: "LICENSE" })).toBeNull();
});

/** A live change as a listing answers it, waiting on its first unmet precondition. */
function change(
  number: number,
  state: string,
  first: Record<string, unknown> | null,
  owner: string,
) {
  return {
    id: `c${number}`,
    repo_id: "id-web",
    repo: "web",
    ref: `web!${number}`,
    number,
    title: `Change ${number}`,
    state,
    source_ref: `refs/heads/c${number}`,
    target_ref: "refs/heads/main",
    head: HEAD,
    revision: 1,
    owner,
    owner_kind: "agent",
    opened_at: "2026-11-15T10:00:00Z",
    opened_position: POSITION,
    version: 1,
    position: POSITION,
    open_findings: 0,
    updated_at: "2026-11-16T09:00:00Z",
    readiness: {
      met: first ? 2 : 3,
      total: 3,
      first: first ? "Not met yet." : "Ready to merge.",
      preconditions: [
        { kind: "facts", state: "met", detail: "" },
        ...(first ? [{ ...first, detail: "" }] : [{ kind: "check", state: "met", detail: "" }]),
      ],
    },
  };
}

const OPEN = [
  change(
    42,
    "open",
    { kind: "human_approval", state: "unmet", waits_on: [{ handle: "jane", kind: "person" }] },
    "frontend",
  ),
  change(43, "open", { kind: "queue", state: "unmet" }, "backend"),
  change(44, "open", { kind: "check", state: "unmet" }, "sre"),
];
const DRAFTS = [change(45, "draft", { kind: "draft", state: "unmet" }, "frontend")];
const COUNTS = { live: 4, draft: 1, open: 3, closed: 0, merged: 1 };

/** The listing by state: the open page, then the drafts. */
const listing =
  (open: unknown[], drafts: unknown[] = DRAFTS): Answer =>
  (url) =>
    json({ changes: url.searchParams.get("state") === "draft" ? drafts : open, ...PAGE });

// OPEN CHANGES COUNTS THE LIVE ONES FROM THE PAGE'S OWN COUNT, and lists the open first, the drafts
// only where the open page left room. Mutation: ask the drafts when the open page is full, or read
// the count a second time, and this fails.
test("Open changes counts the live changes and lists the open before the drafts, each with its placing word", async () => {
  const asked = stubEngine({
    code_counts: () => json({ counts: { changes: COUNTS }, ...PAGE }),
    code_changes: listing(OPEN),
  });
  mount();
  const open = await section("Open changes");
  await waitFor(() =>
    expect(within(open).getAllByRole("link", { name: /^Change/ })).toHaveLength(4),
  );
  expect(within(open).getByText("4")).toBeTruthy();
  expect(reads(asked, "code_counts")).toHaveLength(1);
  const [first, second] = reads(asked, "code_changes");
  expect(Object.fromEntries(first!.searchParams)).toEqual({
    repo: "web",
    state: "open",
    sort: "updated",
    limit: "5",
  });
  expect(Object.fromEntries(second!.searchParams)).toEqual({
    repo: "web",
    state: "draft",
    sort: "updated",
    limit: "2",
  });
  const rows = within(open).getAllByRole("link", { name: /^Change/ });
  expect(rows.map((r) => r.querySelector(".code-mini__word")?.textContent)).toEqual([
    "waits on you",
    "in the queue",
    "waits on checks",
    "draft",
  ]);
  expect(rows[0]!.querySelector(".code-mini__word")?.getAttribute("data-tone")).toBe("warning");
  expect(rows[1]!.querySelector(".code-mini__word")?.getAttribute("data-tone")).toBe("info");
  expect(rows[0]!.getAttribute("href")).toBe("#/gitlet/repos/web/changes/42");
  expect(within(rows[0]!).getByText("Frontend Engineer")).toBeTruthy();
  expect(within(open).getByRole("link", { name: "View all changes" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=changes",
  );

  cleanup();
  const full = stubEngine({
    code_counts: () => json({ counts: { changes: { ...COUNTS, live: 6, open: 5 } }, ...PAGE }),
    code_changes: listing([
      ...OPEN,
      change(46, "open", null, "cto"),
      change(47, "open", null, "cto"),
    ]),
  });
  mount();
  const fuller = await section("Open changes");
  await waitFor(() =>
    expect(within(fuller).getAllByRole("link", { name: /^Change/ })).toHaveLength(5),
  );
  expect(within(fuller).getAllByText("ready to merge")).toHaveLength(2);
  expect(reads(full, "code_changes").map((u) => u.searchParams.get("state"))).toEqual(["open"]);
});

// NONE, A REFUSED COUNT AND A REFUSED LISTING EACH SAY THEIR OWN WORDS. Mutation: draw a refused
// listing as none open, and this fails.
test("Open changes says none, a refused count, and a refused listing, each in its words", async () => {
  stubEngine({
    code_counts: () =>
      json({ counts: { changes: { ...COUNTS, live: 0, open: 0, draft: 0 } }, ...PAGE }),
  });
  mount();
  const none = await section("Open changes");
  expect(await within(none).findByText("No change is open.")).toBeTruthy();

  cleanup();
  stubEngine({
    code_counts: () => engineJson({ error: "unavailable", detail: "no counts" }, 503),
    code_changes: listing(OPEN),
  });
  mount();
  const uncounted = await section("Open changes");
  await waitFor(() =>
    expect(within(uncounted).getAllByRole("link", { name: /^Change/ })).toHaveLength(3),
  );
  expect(uncounted.querySelector(".code-repo-rail__count")).toBeNull();

  cleanup();
  stubEngine({
    code_changes: () =>
      json({ error: "bad_params", detail: "sort is not a key this node reads" }, 400),
  });
  mount();
  const refused = await section("Open changes");
  expect(
    await within(refused).findByText(
      "Open changes are unavailable: sort is not a key this node reads.",
    ),
  ).toBeTruthy();
});

// WITHOUT A COUNT THE DRAFTS ARE STILL ASKED, never guessed as none, and a refused drafts read
// says so. Mutation: ask the drafts on the count's `draft` alone, and this fails.
test("Open changes asks the drafts when the count is refused, and says a refused drafts read", async () => {
  const refusedCount = () => engineJson({ error: "unavailable", detail: "no counts" }, 503);
  const asked = stubEngine({ code_counts: refusedCount, code_changes: listing([]) });
  mount();
  const open = await section("Open changes");
  expect(await within(open).findByRole("link", { name: /^Change 45/ })).toBeTruthy();
  expect(within(open).queryByText("No change is open.")).toBeNull();
  const draft = reads(asked, "code_changes").find((u) => u.searchParams.get("state") === "draft")!;
  expect(draft.searchParams.get("limit")).toBe("5");

  cleanup();
  stubEngine({
    code_counts: refusedCount,
    code_changes: (url) =>
      url.searchParams.get("state") === "draft"
        ? json({ error: "bad_params", detail: "draft is not a state this node reads" }, 400)
        : json({ changes: [], ...PAGE }),
  });
  mount();
  const refused = await section("Open changes");
  expect(
    await within(refused).findByText(
      "Drafts are unavailable: draft is not a state this node reads.",
    ),
  ).toBeTruthy();
  expect(within(refused).queryByText("No change is open.")).toBeNull();
});

/** Thirty days of pushes ending with the last fourteen the spark draws. */
const SPARK = [...Array<number>(16).fill(2), 9, 14, 6, 1, 3, 12, 17, 11, 8, 15, 19, 7, 13, 8];

// THE PUSHES ARE COUNTED ON THE READER'S OWN DAYS: a +5:45 reader's bounds are their midnights,
// a quarter-hour off UTC's, and the read moves once their day turns. Mutation: build the bounds
// from UTC midnights, or from the machine's zone, and this fails.
test("the pushes are counted on a +5:45 reader's own days, and the spark draws the last 14", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  pinZoneForTest("Asia/Kathmandu");
  const asked = stubEngine({
    code_repo: withFacts({
      spark: SPARK,
      pushers: { from: "2026-10-18T00:00:00Z", until: "2026-11-16T00:00:00Z", top: [] },
    }),
  });
  mount();
  const figure = await screen.findByRole("figure");
  expect(figure.querySelector("figcaption")?.textContent).toBe("Pusheslast 14 days");
  const facts = () => reads(asked, "code_repo").filter((u) => u.searchParams.has("day_bounds"));
  const bounds = facts()[0]!.searchParams.get("day_bounds")!.split(",");
  expect(facts()[0]!.searchParams.get("repo_facts")).toBe("true");
  expect(bounds).toHaveLength(31);
  expect([bounds[0], bounds[29], bounds[30]]).toEqual([
    "2026-10-17T18:15:00Z",
    "2026-11-15T18:15:00Z",
    "2026-11-16T18:15:00Z",
  ]);
  const bars = figure.querySelectorAll(".crewlet-histogram__bar");
  expect(bars).toHaveLength(14);
  expect(bars[0]!.getAttribute("title") ?? bars[0]!.getAttribute("aria-label")).toBe(
    "Nov 03 · 9 pushes",
  );
  expect(bars[13]!.getAttribute("title") ?? bars[13]!.getAttribute("aria-label")).toBe(
    "Today · 8 pushes so far",
  );
  expect(figure.querySelector(".code-spark__ends")?.textContent).toBe("Nov 03Today");
  expect(
    within(figure).getByRole("group", { name: "Pushes a day over the last 14 days" }),
  ).toBeTruthy();

  // WITHIN THE DAY NOTHING NEW IS ASKED; PAST THE READER'S MIDNIGHT, ONCE, A DAY ON.
  clockAt("2026-11-16T18:10:00Z");
  expect(facts()).toHaveLength(1);
  clockAt("2026-11-16T18:20:00Z");
  await waitFor(() => expect(facts()).toHaveLength(2));
  expect(facts()[1]!.searchParams.get("day_bounds")!.split(",")[0]).toBe("2026-10-18T18:15:00Z");
});

// WHO PUSHES HERE NAMES EACH PUSHER BY THEIR SEAT AND GITLET BY ITS MARK, folding the rest into one
// line, and says its days are UTC's. Mutation: draw the handle for the name, or drop `omitted`,
// and this fails.
test("Who pushes here names Gitlet for merges and folds the rest into one line", async () => {
  stubEngine({
    code_repo: withFacts({
      spark: SPARK,
      pushers: {
        from: "2026-10-17T00:00:00Z",
        until: "2026-11-16T00:00:00Z",
        top: [
          { kind: "agent", handle: "frontend", pushes: 88 },
          { kind: "agent", handle: "backend", pushes: 49 },
          { kind: "agent", handle: "sre", pushes: 29 },
          { kind: "gitlet", pushes: 23 },
          { kind: "agent", handle: "cto", pushes: 14 },
          { kind: "person", handle: "jane", pushes: 6 },
          { kind: "agent", handle: "reviewer", pushes: 2 },
        ],
        omitted: 3,
        omitted_pushes: 4,
      },
    }),
  });
  mount();
  await screen.findByText("30 days, UTC");
  const pushers = await section("Who pushes here");
  const rows = await within(pushers).findAllByRole("listitem");
  // EACH ROW'S NAME AND COUNT, the faces' initials aside.
  const said = (r: HTMLElement) =>
    r.querySelector(".code-repo-rail__name")
      ? `${r.querySelector(".code-repo-rail__name")!.textContent} ${r.querySelector(".code-repo-rail__pushes")!.textContent}`
      : r.textContent;
  expect(rows.map(said)).toEqual([
    "Frontend Engineer 88",
    "Backend Engineer 49",
    "Site Reliability Engineer 29",
    "Gitlet 23",
    "CTO 14",
    "jane 6",
    "and 4 more, 6 pushes",
  ]);
  expect(rows[3]!.querySelector(".code-face")).toBeTruthy();
  const days = within(pushers).getByText("30 days, UTC");
  expect(days.getAttribute("title")).toMatch(/^Counted on whole UTC days, Oct 17 to Nov 15\. /);
});

// AN OLDER FLEET'S PUSHES READ AS UNAVAILABLE, never as zero, and cost the page nothing else.
// Mutation: draw an absent spark as a quiet one, and this fails.
test("an older fleet's pushes read as unavailable, never as zero", async () => {
  stubEngine({ code_repo: withFacts({}) });
  mount();
  const unread = await screen.findByText(
    "Who pushes here and pushes by day are not counted on this fleet yet.",
  );
  expect(unread.closest("section")).toBe(await section("Pushes"));
  expect(screen.queryByRole("region", { name: "Who pushes here" })).toBeNull();
  expect(screen.queryByRole("figure")).toBeNull();
  expect(await screen.findByRole("link", { name: "README.md" })).toBeTruthy();

  cleanup();
  stubEngine({
    code_repo: (url) =>
      url.searchParams.has("day_bounds")
        ? json({ error: "bad_params", detail: "day_bounds is not a key this node reads" }, 400)
        : json({ repo: detail(), ...PAGE }),
  });
  mount();
  const refused = await screen.findByText(
    "Pushes by day are unavailable: day_bounds is not a key this node reads.",
  );
  expect(refused.closest("section")).toBe(await section("Pushes"));
  expect(screen.queryByRole("figure")).toBeNull();
  expect(await section("About")).toBeTruthy();
});

// A QUIET REPOSITORY DRAWS NO CHART. Mutation: draw fourteen empty bars, and this fails.
test("a quiet repository draws no chart", async () => {
  stubEngine({
    code_repo: withFacts({
      spark: Array<number>(30).fill(0),
      pushers: { from: "2026-10-17T00:00:00Z", until: "2026-11-16T00:00:00Z" },
    }),
  });
  mount();
  expect(await screen.findByText("No push in the last 14 days.")).toBeTruthy();
  expect(screen.getByText("Nobody pushed in the last 30 days.")).toBeTruthy();
  expect(screen.queryByRole("figure")).toBeNull();
});

// THE MIRROR CARD LINKS THE MIRROR WITH ITS HOST'S MARK, the one mark on the page, and says what
// it publishes and as whom. Mutation: draw the mark beside the header's Push mirror tag, or link an
// ssh address, and this fails.
test("the Mirror card links the push mirror with its host's mark and says what it publishes", async () => {
  const mirrored = (remotes: Record<string, unknown>) => detail({ remotes });
  stubEngine(
    {},
    mirrored({
      mirror_to: { url: "https://github.com/nimbus/web.git", as: "sre", credential: "${TOKEN}" },
    }),
  );
  mount();
  const mirror = await section("Mirror");
  const link = within(mirror).getByRole("link", { name: "github.com/nimbus/web" });
  expect(link.getAttribute("href")).toBe("https://github.com/nimbus/web");
  expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  expect(mirror.querySelector('[data-vendor="github"]')).toBeTruthy();
  expect(document.querySelectorAll("[data-vendor]")).toHaveLength(1);
  expect(within(mirror).getByText(/^Publishes/).textContent).toBe(
    "Publishes main and every tag, as Site Reliability Engineer.",
  );
  expect(document.body.textContent).not.toContain("${TOKEN}");

  cleanup();
  stubEngine(
    {},
    mirrored({
      mirror_to: {
        url: "ssh://git@forge.example.com/acme/web.git",
        as: "sre",
        credential: "",
        refs: ["refs/heads/release/*"],
      },
    }),
  );
  mount();
  const ssh = await section("Mirror");
  expect(within(ssh).queryByRole("link")).toBeNull();
  expect(within(ssh).getByText("ssh://git@forge.example.com/acme/web.git")).toBeTruthy();
  expect(ssh.querySelector("[data-vendor]")).toBeNull();
  expect(within(ssh).getByText(/^Publishes/).textContent).toBe(
    "Publishes refs/heads/release/*, as Site Reliability Engineer.",
  );

  cleanup();
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine(
    {},
    mirrored({
      mirror_from: { url: "https://gitlab.com/acme/web.git" },
      mirror_synced_at: "2026-11-16T07:00:00Z",
    }),
  );
  mount();
  const pull = await section("Mirror");
  expect(within(pull).getByText(/^Follows it/).textContent).toBe(
    "Follows it; last synced 3 hours ago.",
  );
  expect(pull.querySelector('[data-vendor="gitlab"]')).toBeTruthy();

  cleanup();
  stubEngine();
  mount();
  await section("About");
  expect(screen.queryByRole("region", { name: "Mirror" })).toBeNull();
});

const TAG = {
  ref: "refs/tags/v2026.11.2",
  oid: HEAD,
  type: "tag",
  peeled: HEAD,
  peeled_type: "commit",
  date: "2026-11-16T05:00:00Z",
};

// TAGS SHOWS THE NEWEST, LATEST, at its commit and tagged in words, from a read of one. Mutation:
// read the tags of a repository that holds none, and this fails.
test("Tags shows the newest with Latest, at its commit, tagged in words", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  const asked = stubEngine({
    code_tags: () => json({ repo: "web", kind: "tags", tags: { tags: [TAG], total: 3 } }),
  });
  mount();
  const tags = await section("Tags");
  const name = await within(tags).findByRole("link", { name: "v2026.11.2" });
  expect(name.getAttribute("href")).toBe("#/gitlet/repos/web?ref=refs%2Ftags%2Fv2026.11.2");
  expect(within(tags).getByText("Latest")).toBeTruthy();
  expect(within(tags).getByText(/^at/).textContent).toBe("at c71d0e9, tagged 5 hours ago");
  expect(reads(asked, "code_tags")[0]!.searchParams.get("limit")).toBe("1");
  expect(within(tags).getByRole("link", { name: "View all tags" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=branches&view=tags",
  );

  // A NEWER TAG IS READ AGAIN WHEN THE PAGE'S COUNT MOVES, though the tags read is not polled.
  // Mutation: drop the effect's refetch, and this fails.
  cleanup();
  vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  let count = 3;
  const NEWER = { ...TAG, ref: "refs/tags/v2026.11.3", date: "2026-11-16T09:00:00Z" };
  const moving = stubEngine({
    code_repo: (url) =>
      json({
        repo: url.searchParams.has("day_bounds") ? detail() : detail({ tags: count }),
        ...PAGE,
      }),
    code_tags: () =>
      json({ repo: "web", kind: "tags", tags: { tags: [count > 3 ? NEWER : TAG], total: count } }),
  });
  mount();
  const moved = await section("Tags");
  await within(moved).findByRole("link", { name: "v2026.11.2" });
  expect(reads(moving, "code_tags")).toHaveLength(1);
  count = 4;
  await act(async () => void vi.advanceTimersByTime(60_000));
  expect(await within(moved).findByRole("link", { name: "v2026.11.3" })).toBeTruthy();
  expect(reads(moving, "code_tags")).toHaveLength(2);

  // A COUNT OF TAGS WITH NONE IN ITS ANSWER is a read that has not caught up, not none.
  cleanup();
  stubEngine({
    code_tags: () => json({ repo: "web", kind: "tags", tags: { tags: [], total: 0 } }),
  });
  mount();
  expect(await within(await section("Tags")).findByText("No tag is read yet.")).toBeTruthy();

  cleanup();
  const none = stubEngine({}, detail({ tags: 0 }));
  mount();
  expect(await within(await section("Tags")).findByText("No tag yet.")).toBeTruthy();
  expect(reads(none, "code_tags")).toHaveLength(0);

  cleanup();
  stubEngine({
    code_tags: () => engineJson({ error: "unavailable", detail: "No tag index here." }, 503),
  });
  mount();
  expect(
    await within(await section("Tags")).findByText("Tags are unavailable: No tag index here."),
  ).toBeTruthy();
});

// THE RAIL'S READS ARE THE ROOT'S: another tab or a path asks none of them. Mutation: draw the rail
// under a path, and this fails.
test("the rail's reads are made at the root's Code tab only", async () => {
  for (const hash of ["#/gitlet/repos/web?tab=commits", "#/gitlet/repos/web?path=internal"]) {
    const asked = stubEngine();
    mount(hash);
    await screen.findByRole("heading", { name: "Engineering, web" });
    await act(async () => {});
    expect(reads(asked, "code_changes")).toHaveLength(0);
    expect(reads(asked, "code_tags")).toHaveLength(0);
    expect(asked.some((u) => u.searchParams.has("day_bounds"))).toBe(false);
    cleanup();
  }
});
