/**
 * Settings › Gitlet reads four answers the engine already serves and draws
 * Gitlet's part of each. The invariants worth breaking a build over: the
 * alarms it lists are Gitlet's kinds and no other, the longest-standing
 * first, and a node that evaluated no table is named apart rather than read
 * as quiet; an unset policy field is drawn as the default it reads as,
 * said to be the default, never as blank; the Branch protection card asks the
 * engine for the protected repositories a page at a time, never one page of
 * every repository filtered here; and a repository's fold reads its gates
 * only once it is opened, once, asked for by name.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import {
  LiveSocket,
  Store,
  type CodeRepoSummary,
  type CompanyDocument,
  type FleetAnswer,
  type FleetNode,
  type OrgProjection,
} from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import {
  BranchProtection,
  RepoProtection,
  codeAlarmsOf,
  codePolicyOf,
  protectionLines,
} from "./Gitlet.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const node = (id: string, alarms?: FleetNode["alarms"]): FleetNode =>
  ({
    id,
    roles: ["data", "gitlet"],
    broker: "embedded",
    ...(alarms ? { alarms } : {}),
  }) as FleetNode;

const fleet = (nodes: FleetNode[]): FleetAnswer =>
  ({
    nodes,
    seats: [],
    duties: [],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "a",
    target_epoch: 1,
  }) as FleetAnswer;

// GITLET'S ALARMS AND NO OTHER, the longest-standing first, and a node
// that evaluated no table is listed apart — never as a node with nothing
// firing. Mutation: keep a kind Gitlet does not raise, drop the sort,
// or count a node with no `alarms` as quiet, and this fails.
test("the alarms are Gitlet's kinds, oldest first, and a node that said nothing is named apart", () => {
  const got = codeAlarmsOf(
    fleet([
      node("git-1", [
        { kind: "apply_lag", since: "2026-10-30T08:00:00Z" },
        { kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
      ]),
      node("git-2", [{ kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" }]),
      node("booting"),
      node("quiet", []),
    ]),
  );
  expect(got.firing).toEqual([
    { node: "git-2", kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" },
    { node: "git-1", kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
  ]);
  expect(got.silent).toEqual(["booting"]);
});

// AN UNSET FIELD READS AS THE ENGINE'S DEFAULT, and says it is one; a set one
// is the document's value. Mutation: draw an unset field as zero or empty, or
// mark a set one as the default, and this fails.
test("an unset scratch policy and identity domain read as the engine's defaults, marked as defaults", () => {
  const unset = codePolicyOf({ code: { backend: "gitlet" } } as unknown as CompanyDocument);
  expect(unset.idleDays).toEqual({ value: 7, set: false });
  expect(unset.maxPerSeat).toEqual({ value: 50, set: false });
  expect(unset.identityDomain).toEqual({ value: "seats.invalid", set: false });

  const set = codePolicyOf({
    code: {
      backend: "gitlet",
      scratch: { idle_days: 3, max_per_seat: 0 },
      identity_domain: "acme.example",
    },
  } as unknown as CompanyDocument);
  expect(set.idleDays).toEqual({ value: 3, set: true });
  expect(set.maxPerSeat).toEqual({ value: 0, set: true });
  expect(set.identityDomain).toEqual({ value: "acme.example", set: true });

  expect(codePolicyOf(null).idleDays).toEqual({ value: 7, set: false });
});

// THE GATE PER PATTERN: each rule says what a merge into it must meet, read
// from the repository's detail by its own pattern; a rule the detail gives
// no gate SAYS it has none; and before the detail answers nothing is claimed
// either way. Mutation: match a gate to another rule's pattern, drop the "no
// merge gate" line, or claim none before the detail answered, and this fails.
test("each protected rule says its own gate, or that it has none once the detail answered", () => {
  const rules = [
    { pattern: "refs/heads/main", direct_push: "none" },
    { pattern: "refs/heads/release/*", direct_push: "persons", allow_force: true },
  ];
  const gate = {
    merge_by: "maintainers",
    approvals: "one",
    checks: ["unit"],
    landing: { depth: 4, batch: 4 },
    delete_source: "seats",
  };
  expect(protectionLines(rules, undefined)).toEqual([
    { pattern: "refs/heads/main", says: "nobody pushes to it directly" },
    {
      pattern: "refs/heads/release/*",
      says: "only a person pushes to it directly, forced pushes allowed",
    },
  ]);
  const read = protectionLines(rules, [{ pattern: "refs/heads/main", gate }]);
  expect(read[0]?.says).toBe(
    "nobody pushes to it directly; a merge into it is merged by its maintainers, one approval, 1 check pass, landed by its queue, each change tested merged on the branch before it lands",
  );
  expect(read[1]?.says).toBe(
    "only a person pushes to it directly, forced pushes allowed; no merge gate",
  );
});

// WHO SET EACH RULE AND WHEN, by its own pattern: a rule a person or a seat
// last changed names them, a declared rule names the company document rather
// than a blank decider, and before the detail answers no source is claimed.
// Mutation: match a source to another rule's pattern, or drop the config
// arm, and this fails.
test("each protected rule says who last set it and when, once the detail answered", () => {
  const rules = [
    { pattern: "refs/heads/main", direct_push: "none" },
    { pattern: "refs/heads/release/*", direct_push: "persons" },
  ];
  expect(protectionLines(rules, undefined)[0]?.source).toBeUndefined();
  const read = protectionLines(
    rules,
    [],
    [
      { pattern: "refs/heads/main", set_by_kind: "config", set_at: "2026-10-01T09:00:00Z" },
      {
        pattern: "refs/heads/release/*",
        set_by: "founder",
        set_by_kind: "operator",
        set_at: "2026-10-02T09:00:00Z",
      },
    ],
  );
  expect(read[0]?.source).toMatch(/^Set by the company document, \S/);
  expect(read[1]?.source).toMatch(/^Set by founder, \S/);
});

/** A declared repository whose main branch takes no direct push. */
const REPO = {
  id: "id-web",
  name: "web",
  status: "active",
  managed_by: "config",
  kind: "project",
  owner: { kind: "unit", ref: "engineering" },
  visibility: "company",
  search: "indexed",
  default_branch: "main",
  protections: [{ pattern: "refs/heads/main", direct_push: "none" }],
} as unknown as CodeRepoSummary;

// A REPOSITORY'S FOLD ASKS ITS DETAIL FOR THE GATES BY NAME: the engine
// answers a repository's question with its row alone unless asked for more,
// and a detail read without them would say "no merge gate" of every rule.
// Mutation: drop `gates` from the question, and this fails.
test("a repository's fold asks for its rules' gates and draws each rule's", async () => {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname !== "/query/code_repo") return new Promise<Response>(() => {});
      const gated = url.searchParams.get("gates") === "true";
      return new Response(
        JSON.stringify({
          repo: {
            ...REPO,
            ...(gated
              ? {
                  gates: [{ pattern: "refs/heads/main", gate: { approvals: "one" } }],
                  rule_sources: [
                    {
                      pattern: "refs/heads/main",
                      set_by: "cto",
                      set_by_kind: "agent",
                      set_at: "2026-10-02T09:00:00Z",
                    },
                  ],
                }
              : {}),
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
  render(<RepoProtection repo={REPO} />);

  expect(await screen.findByText(/a merge into it is .*one approval/)).toBeTruthy();
  expect(screen.getByText(/^Set by cto, /)).toBeTruthy();
  const read = asked.find((u) => u.pathname === "/query/code_repo");
  expect(read?.searchParams.get("gates")).toBe("true");
  expect(read?.searchParams.has("path_owners")).toBe(false);
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };

const ORG = withDerived({
  name: "Acme",
  roles: [{ name: "CTO", handle: "cto" }],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

/** A protected repository as the listing answers it. */
function protectedRepo(name: string) {
  return {
    id: `id-${name}`,
    name,
    status: "active",
    managed_by: "config",
    kind: "project",
    owner: { kind: "unit", ref: "engineering" },
    visibility: "company",
    search: "indexed",
    default_branch: "main",
    position: POSITION,
    access: "maintain",
    protections: [{ pattern: "refs/heads/main", direct_push: "persons" }],
  };
}

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/**
 * The engine's REST answers, by path, and every request made. The frame's own
 * read of who is signed in is left unanswered: the viewer answers on the
 * socket.
 */
function stubFetch(answer: (url: URL) => Response | null) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      return Promise.resolve(answer(url) ?? new Response("{}", { status: 404 }));
    }),
  );
  return asked;
}

/** The card in the frame's readings, read by an administrator. */
function mountCard() {
  location.hash = "#/settings/gitlet";
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
            <BranchProtection />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const asks = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);

// THE PROTECTED REPOSITORIES, A PAGE AT A TIME, AS THE ENGINE FILTERS THEM:
// every listing the card asks names `protected=true`, and a company with
// more protected repositories than a page reaches each through Load more,
// asked with the cursor the page before answered. Mutation: ask the
// unfiltered listing and keep the protected rows here, or draw the first page
// alone, and this fails.
test("the card lists the protected repositories the engine answers, a page at a time", async () => {
  const names = ["api", "docs", "web"];
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_repos") return null;
    if (url.searchParams.get("protected") !== "true") return json({ position: POSITION });
    const after = url.searchParams.get("cursor") ?? "";
    const left = names.filter((n) => n > after);
    const page = left.slice(0, 2);
    return json({
      repos: page.map(protectedRepo),
      ...(left.length > page.length ? { next: page.at(-1) } : {}),
      position: POSITION,
      level: "stale",
    });
  });
  mountCard();

  expect(await screen.findByText("api")).toBeTruthy();
  expect(screen.getByText("docs")).toBeTruthy();
  expect(screen.queryByText("web")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /load more/i }));
  expect(await screen.findByText("web")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();

  const listings = asks(asked, "code_repos");
  expect(listings.length).toBeGreaterThanOrEqual(2);
  expect(listings.every((u) => u.searchParams.get("protected") === "true")).toBe(true);
  expect(listings.some((u) => u.searchParams.get("cursor") === "docs")).toBe(true);
});

/**
 * Open or close a repository's fold, as a reader's press on its trigger does:
 * the kit's Disclosure, the code log's own, never a native `<details>`.
 */
async function toggle(name: string, open: boolean) {
  const trigger = screen.getByText(name).closest("button");
  if (!trigger) throw new Error(`${name} is drawn in no fold`);
  expect(trigger.closest("details")).toBeNull();
  if ((trigger.getAttribute("aria-expanded") === "true") === open) return;
  await act(async () => {
    fireEvent.click(trigger);
  });
  expect(trigger.getAttribute("aria-expanded")).toBe(String(open));
}

// A FOLD READS ITS REPOSITORY'S GATES ONLY ONCE OPENED, AND ONCE: nothing
// asks a repository's detail until its fold opens — a company with many
// protected repositories would otherwise ask each one's on every visit — the
// opening asks exactly once, for the gates by name, and a fold closed and
// opened again asks nothing more: its answer is kept. Mutation: render each
// fold's detail whether it is open or not, or unmount it when the fold
// closes, and this fails.
test("a repository's fold reads its gates once it is opened, and once", async () => {
  const asked = stubFetch((url) => {
    if (url.pathname === "/query/code_repos") {
      return json({
        repos: [protectedRepo("api"), protectedRepo("web")],
        position: POSITION,
        level: "stale",
      });
    }
    if (url.pathname === "/query/code_repo") {
      return json({
        repo: {
          ...protectedRepo(url.searchParams.get("name") ?? ""),
          gates: [
            {
              pattern: "refs/heads/main",
              gate: { merge_by: "maintainers", approvals: "one", delete_source: "seats" },
            },
          ],
        },
        position: POSITION,
        level: "stale",
      });
    }
    return null;
  });
  mountCard();
  expect(await screen.findByText("web")).toBeTruthy();
  await settled(() => asked.length);
  expect(asks(asked, "code_repo")).toEqual([]);

  await toggle("web", true);
  expect(await screen.findByRole("list", { name: "web's protection rules" })).toBeTruthy();
  await settled(() => asked.length);
  const details = asks(asked, "code_repo");
  expect(details.map((u) => u.searchParams.get("name"))).toEqual(["web"]);
  expect(details[0]?.searchParams.get("gates")).toBe("true");
  expect(details[0]?.searchParams.has("path_owners")).toBe(false);

  await toggle("web", false);
  await toggle("web", true);
  await settled(() => asked.length);
  expect(asks(asked, "code_repo")).toHaveLength(1);
});
