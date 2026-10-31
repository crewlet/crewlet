/**
 * A repository's page reads the repository's row and its content over REST,
 * each at the revision and the path its address names.
 *
 * The invariants worth breaking a build over: the content is read at the
 * address's `ref=` and `path=` and nothing else; a Permalink names the commit
 * the ref resolved to, whole; the lines an address picks are the lines marked,
 * and pressing a line's number writes them to the address; the blame is read
 * at the commit the file was; a branch opens its files; the history is read
 * at the revision and the path; and a declared repository's settings are
 * shown, never edited, with the way to Settings › Configuration.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { RepositoryPage } from "./RepositoryPage.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const HEAD = "0123456789abcdef0123456789abcdef01234567";
const OTHER = "89abcdef0123456789abcdef0123456789abcdef";

const ORG = withDerived({
  name: "Acme",
  roles: [{ name: "CTO", handle: "cto" }],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "native", knowledge: "native", code: "native" },
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
    live_packs: 2,
    live_bytes: 2048,
    live_objects: 40,
    position: POSITION,
    access: "maintain",
    head: HEAD,
    pushes: 5,
    last_push_at: "2026-10-27T10:00:00Z",
    branches: 2,
    tags: 1,
    tombstones: 0,
    created_at: "2026-10-01T10:00:00Z",
    protections: [{ pattern: "refs/heads/main", direct_push: "persons" }],
    gates: [
      {
        pattern: "refs/heads/main",
        gate: {
          merge_by: "writers",
          approvals: "one",
          landing: { policy: "pre_land", depth: 1, batch: 1 },
          delete_source: "seats",
        },
      },
    ],
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** A read of content by a ref, as the engine answers one: bare, with the ref it resolved. */
const byRef = (url: URL, part: Record<string, unknown>) => ({
  repo: "web",
  ...part,
  ...(url.searchParams.get("ref")?.length === 40
    ? {}
    : {
        resolved: {
          ref: { ref: `refs/heads/${url.searchParams.get("ref") ?? "main"}`, oid: HEAD },
        },
        position: POSITION,
      }),
});

/**
 * The engine: the repository, its refs, a root holding `src/` and a README, a
 * file of three lines, its blame and a history — and every request made.
 */
function stubEngine(repo: Record<string, unknown> = detail()) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      const p = url.searchParams;
      switch (url.pathname) {
        case "/auth/session":
          return new Promise<Response>(() => {});
        case "/query/code_repo":
          return json({ repo, position: POSITION, level: "session" });
        case "/query/code_refs":
          return json({
            refs:
              p.get("prefix") === "refs/tags/"
                ? [{ ref: "refs/tags/v1", oid: OTHER, position: POSITION }]
                : [
                    { ref: "refs/heads/feature", oid: OTHER, position: POSITION },
                    { ref: "refs/heads/main", oid: HEAD, position: POSITION },
                  ],
            position: POSITION,
            level: "session",
          });
        case "/query/code_tree":
          return json(
            byRef(url, {
              kind: "tree",
              tree: {
                tree: OTHER,
                entries: [
                  { mode: "100644", type: "blob", oid: OTHER, path: "README.md", size: 20 },
                  { mode: "040000", type: "tree", oid: OTHER, path: "src" },
                ],
                total: 2,
              },
            }),
          );
        case "/query/code_blob":
          if (p.get("path") === "README.md") {
            return json(
              byRef(url, {
                kind: "blob",
                blob: {
                  path: "README.md",
                  oid: OTHER,
                  size: 20,
                  lines: 1,
                  text: "# Hello storefront\n",
                },
              }),
            );
          }
          if (p.get("path") === "src") {
            return json(
              byRef(url, {
                kind: "tree",
                tree: {
                  tree: OTHER,
                  path: "src",
                  entries: [
                    { mode: "100644", type: "blob", oid: OTHER, path: "src/app.go", size: 30 },
                  ],
                  total: 1,
                },
              }),
            );
          }
          return json(
            byRef(url, {
              kind: "blob",
              blob: {
                path: "src/app.go",
                oid: OTHER,
                size: 30,
                lines: 3,
                start_line: 1,
                end_line: 3,
                text: "package app\n\nfunc Run() {}\n",
              },
            }),
          );
        case "/query/code_blame":
          return json(
            byRef(url, {
              kind: "blame",
              blame: {
                path: "src/app.go",
                start_line: 1,
                end_line: 3,
                lines: [
                  { line: 1, commit: HEAD, orig_line: 1, text: "package app" },
                  { line: 2, commit: HEAD, orig_line: 2, text: "" },
                  { line: 3, commit: OTHER, orig_line: 3, text: "func Run() {}" },
                ],
                commits: {
                  [HEAD]: { author: "Ada", authored_at: "2026-10-20T10:00:00Z", subject: "Start" },
                  [OTHER]: { author: "Grace", authored_at: "2026-10-21T10:00:00Z", subject: "Run" },
                },
              },
            }),
          );
        case "/query/code_log":
          return json(
            byRef(url, {
              kind: "log",
              ...(p.get("cursor") ? {} : { next: "c1" }),
              log: {
                commits: p.get("cursor")
                  ? [
                      {
                        oid: OTHER,
                        author: "Grace",
                        authored_at: "2026-10-21T10:00:00Z",
                        committed_at: "2026-10-21T10:00:00Z",
                        subject: "Older work",
                      },
                    ]
                  : [
                      {
                        oid: HEAD,
                        author: "Ada",
                        authored_at: "2026-10-22T10:00:00Z",
                        committed_at: "2026-10-22T10:00:00Z",
                        subject: "Start the storefront",
                      },
                    ],
                scanned: 1,
              },
              pushed_by: p.get("cursor")
                ? {}
                : {
                    [HEAD]: {
                      push_id: "p1",
                      position: POSITION,
                      at: "2026-10-22T10:00:00Z",
                      principal: { kind: "agent", seat: "cto" },
                      via: "git",
                    },
                  },
            }),
          );
        case "/query/code_pushes":
          return json({ position: POSITION, level: "session" });
      }
      return json({}, 404);
    }),
  );
  return asked;
}

function mount(hash: string, repo = "web") {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <RepositoryPage repo={repo} />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const reads = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);

// THE OVERVIEW IS THE REPOSITORY'S ROW AND ITS README: the header names the
// repository and its owner off the chart, and the README at the default
// branch's root is read at the commit that listing resolved, rendered.
// Mutation: read the README at the branch rather than the commit the root was
// listed at, and the two could be two revisions — this fails.
test("the overview is the repository's row and the README at the commit its root was listed at", async () => {
  const asked = stubEngine();
  mount("#/code/repos/web");

  expect(await screen.findByRole("heading", { name: "Hello storefront" })).toBeTruthy();
  expect(screen.getByText("Engineering")).toBeTruthy();
  expect(screen.getByText("The storefront")).toBeTruthy();
  const readme = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "README.md");
  expect(readme?.searchParams.get("ref")).toBe(HEAD);
  expect(reads(asked, "code_repo")[0]?.searchParams.get("name")).toBe("web");
});

// A PATH IS ONE READ, AT THE ADDRESS'S REVISION: a directory under the root
// is asked as a file and drawn as the listing it is, each entry opening its
// own path at the same revision. Mutation: drop `ref` from the read, or link
// an entry without it, and this fails.
test("a directory is read at the address's revision and its entries open at it", async () => {
  const asked = stubEngine();
  mount("#/code/repos/web?tab=files&ref=feature&path=src");

  const table = await screen.findByRole("table", { name: "Files" });
  const link = within(table).getByRole("link", { name: "app.go" });
  expect(link.getAttribute("href")).toBe(
    "#/code/repos/web?tab=files&ref=feature&path=src%2Fapp.go",
  );
  const read = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "src");
  expect(read?.searchParams.get("ref")).toBe("feature");
});

// THE LINES AN ADDRESS PICKS ARE MARKED, a press on a number writes its line
// to the address, Shift extends the run, and the Permalink names the commit
// the branch resolved to, whole, with the lines. Mutation: mark by index
// rather than line number, or build the Permalink from the branch, and this
// fails.
test("a file's picked lines are its address, and its Permalink names the commit", async () => {
  stubEngine();
  mount("#/code/repos/web?tab=files&path=src%2Fapp.go&lines=2");

  const line2 = await screen.findByRole("button", { name: "Line 2" });
  expect(line2.getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByRole("button", { name: "Line 1" }).getAttribute("aria-pressed")).toBe("false");
  expect(screen.getByText("func Run() {}")).toBeTruthy();

  const permalink = screen.getByRole("link", { name: "Permalink" });
  expect(permalink.getAttribute("href")).toBe(
    `#/code/repos/web?tab=files&ref=${HEAD}&path=src%2Fapp.go&lines=2`,
  );

  fireEvent.click(screen.getByRole("button", { name: "Line 3" }), { shiftKey: true });
  await waitFor(() => expect(location.hash).toContain("lines=2-3"));
  fireEvent.click(screen.getByRole("button", { name: "Line 1" }));
  await waitFor(() => expect(location.hash).toContain("lines=1"));
  expect(location.hash).not.toContain("lines=1-");
});

// THE BLAME IS READ AT THE COMMIT THE FILE WAS, and each run of lines one
// commit changed last is headed by that commit and its author once.
// Mutation: blame the branch, or head every line, and this fails.
test("the blame is read at the file's commit and heads each run of lines once", async () => {
  const asked = stubEngine();
  mount("#/code/repos/web?tab=files&path=src%2Fapp.go&lens=blame");

  const table = await screen.findByRole("table", { name: /Who last changed each line/ });
  expect(within(table).getAllByText("Ada")).toHaveLength(1);
  expect(within(table).getAllByText("Grace")).toHaveLength(1);
  await waitFor(() => expect(reads(asked, "code_blame").length).toBeGreaterThan(0));
  expect(reads(asked, "code_blame")[0]?.searchParams.get("ref")).toBe(HEAD);
});

// A BRANCH OPENS ITS FILES: the default branch at the bare Files address, any
// other by its name. Mutation: link the default branch by name, or every
// branch to the default, and this fails.
test("the branches are the engine's refs, each opening its files", async () => {
  stubEngine();
  mount("#/code/repos/web?tab=branches");

  const grid = await screen.findByRole("table", { name: "Branches of web" });
  expect(within(grid).getByText("default")).toBeTruthy();
  const links = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(links).toContain("#/code/repos/web?tab=files&ref=feature");
  expect(links).toContain("#/code/repos/web?tab=files");
});

// THE HISTORY IS READ AT THE REVISION AND THE PATH, with who pushed each
// commit from the page that named it, and Load older asks the cursor.
// Mutation: drop the path, read who pushed off the commit's author, or link a
// commit anywhere but its own page, and this fails.
test("the history is read at the revision and the path, with who pushed each commit", async () => {
  const asked = stubEngine();
  mount("#/code/repos/web?tab=commits&ref=feature&path=src");

  const grid = await screen.findByRole("table", { name: "History of feature" });
  expect(within(grid).getByText("Start the storefront")).toBeTruthy();
  expect(within(grid).getByText("CTO")).toBeTruthy();
  // EACH COMMIT OPENS ITS OWN PAGE, by its whole id.
  expect(
    within(grid)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toContain(`#/code/repos/web/commits/${HEAD}`);
  const first = reads(asked, "code_log")[0];
  expect(first?.searchParams.get("ref")).toBe("feature");
  expect(first?.searchParams.get("path")).toBe("src");

  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await within(grid).findByText("Older work")).toBeTruthy();
  expect(reads(asked, "code_log").some((u) => u.searchParams.get("cursor") === "c1")).toBe(true);
});

// A DECLARED REPOSITORY'S SETTINGS ARE SHOWN, NEVER EDITED: the rules and
// their gates as the detail carries them, and the way to Settings ›
// Configuration, where they are changed. Mutation: offer an edit, or drop
// the link, and this fails.
test("a declared repository's settings are shown and lead to the configuration", async () => {
  stubEngine();
  mount("#/code/repos/web?tab=settings");

  expect(await screen.findByText("refs/heads/main")).toBeTruthy();
  expect(screen.getByText(/one approval/)).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open the configuration" }).getAttribute("href")).toBe(
    "#/settings/config",
  );
  expect(screen.queryByRole("textbox")).toBeNull();
});

// A NAME NO REPOSITORY CAN HAVE IS SAID IN PLACE and never asked about.
// Mutation: ask the engine anyway, and this fails.
test("a name no repository can have is said in place and never asked", async () => {
  const asked = stubEngine();
  mount("#/code/repos/Not_A_Repo", "Not_A_Repo");
  expect(await screen.findByText("Not a repository")).toBeTruthy();
  expect(reads(asked, "code_repo")).toHaveLength(0);
});
