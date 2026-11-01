/**
 * A change's page reads the change a part at a time over REST, and writes
 * through the act transport — never optimistically.
 *
 * The invariants worth breaking a build over: a review is ONE
 * `code_review_change` of the head revision carrying every finding drafted on
 * its lines and the reviewer's standing findings; a draft is the tab's —
 * kept in sessionStorage until that one press — and forgotten only once the
 * engine has taken the review; nothing on screen moves before the engine
 * answers; a change no longer live holds its review controls with the reason;
 * an outdated finding is said under a Disclosure, never dropped; and whom its
 * records woke is said in the voice of somebody else's notice.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { draftsKey } from "~/lib/reviewDrafts.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { ChangePage } from "./ChangePage.tsx";

beforeEach(() => sessionStorage.clear());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
  sessionStorage.clear();
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const HEAD = "0123456789abcdef0123456789abcdef01234567";

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Engineer", handle: "dev" },
  ],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "native", knowledge: "native", code: "native" },
}) as unknown as OrgProjection;

/** The reader: bound to `jane`, offered every change write. */
const VIEWER = {
  ...ADMIN_VIEWER,
  acts: [
    "code_review_change",
    "code_comment_on_change",
    "code_request_review",
    "code_close_change",
    "code_apply_suggestions",
  ],
};

function detail(extra: Record<string, unknown> = {}) {
  return {
    id: "change-1",
    repo_id: "repo-web",
    number: 42,
    title: "Retry the flaky upload",
    state: "open",
    source_ref: "refs/heads/retry",
    target_ref: "refs/heads/main",
    head: HEAD,
    revision: 2,
    owner: "dev",
    owner_kind: "agent",
    opened_at: "2026-10-27T10:00:00Z",
    opened_position: POSITION,
    version: 5,
    position: POSITION,
    repo: "web",
    ref: "web!42",
    authors: [{ author: "dev", kind: "agent", first_revision: 1, position: POSITION }],
    readiness: {
      met: 3,
      total: 5,
      first: "Waits on an approval.",
      preconditions: [{ kind: "approval", state: "unmet", detail: "No counted approval." }],
    },
    ...extra,
  };
}

const PATCH = ["--- a/app.go", "+++ b/app.go", "@@ -1,2 +1,2 @@", " keep", "-old", "+new"].join(
  "\n",
);

/** A finding the reader raised on revision 1 of app.go. */
const MINE = {
  id: "f-mine",
  review_id: "r1",
  raiser: "jane",
  raiser_kind: "person",
  revision: 1,
  commit_oid: HEAD,
  path: "app.go",
  blob_oid: HEAD,
  side: "new",
  line_start: 9,
  line_end: 9,
  severity: "blocker",
  category: "correctness",
  title: "The retry never stops",
  state: "open",
  position: POSITION,
};

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** Every `code_checks` question the engine was asked, newest last. */
const checksAsked: URL[] = [];

/** The engine: its reads by part, and each act recorded and answered as `answer` says. */
function stubEngine(change: Record<string, unknown>, answer: () => Promise<Response> | Response) {
  const acted: Acted[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: decodeURIComponent(url.pathname.split("/operator/act/")[1] ?? ""),
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return answer();
      }
      if (url.pathname === "/query/code_checks") {
        checksAsked.push(url);
        return json({
          checks: [
            {
              repo_id: "id-web",
              repo: "web",
              oid: HEAD,
              target: "refs/heads/main",
              name: "unit",
              attempt: 1,
              state: "failed",
              purpose: "head",
              required: true,
              duration_ms: 64_000,
              finished_at: "2026-10-27T10:05:00Z",
            },
          ],
          position: POSITION,
          level: "session",
        });
      }
      if (url.pathname === "/query/code_change_routing") {
        return json({
          routed: {
            change: { repo: "web", ref: "web!42", title: "x", state: "open" },
            opened_at: "2026-10-27T10:00:00Z",
            records: [
              {
                record_id: "op-1",
                position: POSITION,
                at: "2026-10-27T10:00:00Z",
                recipients: [
                  { handle: "cto", kind: "agent", reason: "review_requested", addressed: true },
                ],
              },
            ],
            delivery: "reached",
            retained_from: "2026-10-01T00:00:00Z",
          },
          position: POSITION,
          level: "session",
        });
      }
      if (url.pathname !== "/query/code_change") return json({}, 404);
      const page = { position: POSITION, level: "session" };
      switch (url.searchParams.get("part")) {
        case "files":
          return json({
            ...page,
            revision: 2,
            diff: {
              base: HEAD,
              head: HEAD,
              files: [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
              total: 1,
              added: 1,
              deleted: 1,
            },
          });
        case "diff":
          return json({
            ...page,
            revision: 2,
            diff: {
              base: HEAD,
              head: HEAD,
              files: [],
              total: 1,
              added: 1,
              deleted: 1,
              patch: PATCH,
            },
          });
        case "findings":
          return json({
            ...page,
            revision: 2,
            findings: [MINE],
            placed: { "f-mine": { start: 9, end: 9, outdated: true } },
          });
        case "comments":
        case "reviews":
          return json(page);
        default:
          return json({ ...page, change, revisions: [] });
      }
    }),
  );
  return acted;
}

function mount(hash = "#/code/repos/web/changes/42", viewer: unknown = VIEWER) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <ChangePage repo="web" number={42} />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

// ONE REVIEW, ONE PRESS: Approve sends one code_review_change of the head
// revision with every finding drafted on its lines and the reader's standing
// findings it carries — and nothing moves until the engine answers, the
// drafts forgotten only then. Mutation: send each draft as a write of its
// own, review another revision, or forget the drafts before the answer, and
// this fails.
test("Approve is one review of the head carrying the drafts and the standing findings", async () => {
  let answer!: (r: Response) => void;
  const acted = stubEngine(detail(), () => new Promise<Response>((resolve) => (answer = resolve)));
  sessionStorage.setItem(
    draftsKey("change-1", 2),
    JSON.stringify([
      {
        id: "d1",
        path: "app.go",
        side: "new",
        line_start: 2,
        line_end: 2,
        severity: "minor",
        category: "style",
        title: "Name it",
        body: "",
      },
    ]),
  );
  mount();

  fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(/1 drafted finding on revision 2/)).toBeTruthy();
  await within(dialog).findByText("The retry never stops");
  fireEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_review_change",
    args: {
      change: "web!42",
      revision: 2,
      verdict: "approve",
      findings: [
        {
          path: "app.go",
          side: "new",
          line_start: 2,
          line_end: 2,
          severity: "minor",
          category: "style",
          title: "Name it",
        },
      ],
      carries: ["f-mine"],
    },
  });
  // NOT OPTIMISTIC: until the engine answers, the drafts are kept and the
  // dialog stays.
  expect(sessionStorage.getItem(draftsKey("change-1", 2))).not.toBeNull();
  expect(screen.getByRole("dialog")).toBeTruthy();
  await act(async () => answer(json({ outcome: "applied", position: "CREWLET_CODE_LOG@1:13" })));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(sessionStorage.getItem(draftsKey("change-1", 2))).toBeNull();
});

// A CHANGE NO LONGER LIVE takes no review: each control stays drawn, held with
// the reason. Mutation: hide them, or let them press, and this fails.
test("a merged change holds its review controls with the reason", async () => {
  stubEngine(detail({ state: "merged", readiness: undefined }), () => json({}));
  mount();
  const approve = await screen.findByRole("button", { name: "Approve" });
  expect(approve.getAttribute("title")).toMatch(/This change is merged/);
  expect(screen.getByRole("button", { name: "Close" }).getAttribute("title")).toMatch(/final/);
});

// A LINE PICKED ON THE HEAD DRAFTS A FINDING INTO THE TAB, never to the
// engine; and a finding whose lines changed is said under a Disclosure.
// Mutation: send the draft as a write, or drop the outdated finding, and
// this fails.
test("a finding drafted on a line is the tab's until a review, and an outdated one is said", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/code/repos/web/changes/42?tab=files&file=app.go");

  expect(await screen.findByText("1 outdated on this file")).toBeTruthy();
  fireEvent.click(await screen.findByRole("button", { name: "Line 2: pick it" }));
  const form = await screen.findByRole("form", { name: "A finding on line 2" });
  fireEvent.change(within(form).getByRole("textbox", { name: "Title" }), {
    target: { value: "Name it" },
  });
  fireEvent.click(within(form).getByRole("button", { name: "Add to review" }));

  expect(await screen.findByRole("article", { name: "Drafted finding: Name it" })).toBeTruthy();
  const kept = JSON.parse(sessionStorage.getItem(draftsKey("change-1", 2)) ?? "[]") as {
    title: string;
    line_start: number;
  }[];
  expect(kept.map((d) => [d.title, d.line_start])).toEqual([["Name it", 2]]);
  expect(acted).toHaveLength(0);
});

// A CHANGE'S CHECKS ARE THE ATTEMPTS THAT JUDGE IT: its head's, asked by the
// change's number on its repository — never a listing of the repository's —
// each with its state in its verdict's words. Mutation: ask the repository's
// listing (drop `change=`), and this fails.
test("the Checks tab lists the attempts that judge the change, asked by its number", async () => {
  checksAsked.length = 0;
  stubEngine(detail(), () => json({}));
  mount("#/code/repos/web/changes/42?tab=checks");
  const grid = await screen.findByRole("table", { name: /Checks of 0123456/ });
  expect(within(grid).getByText("unit")).toBeTruthy();
  expect(within(grid).getByText("failed")).toBeTruthy();
  expect(checksAsked.length).toBeGreaterThan(0);
  for (const u of checksAsked) {
    expect(u.searchParams.get("repo")).toBe("web");
    expect(u.searchParams.get("change")).toBe("42");
  }
});

// WHOM IT WOKE IS SAID IN SOMEBODY ELSE'S VOICE: a recipient is named with
// why they heard, never "asked for your review" beside a colleague's name.
test("the Woke tab names each recipient with why they heard, in the third person", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/code/repos/web/changes/42?tab=woke");
  const list = await screen.findByRole("list", { name: "Whom it woke" });
  expect(within(list).getByText(/asked to review/)).toBeTruthy();
  expect(within(list).queryByText(/your review/)).toBeNull();
});

// EVERY WRITE IS DRAWN FOR EVERY READER, held with the reason it cannot act:
// the page bar's verdicts, the conversation's composer and a finding's reply.
// Mutation: hide a control from a reader who cannot act, and this fails.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...VIEWER, acts: [] }, WRITE_REASONS.not_served],
])("the change's writes are drawn for %s, held with the reason", async (_who, viewer, reason) => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/code/repos/web/changes/42", viewer);
  await screen.findByRole("tabpanel");
  for (const name of ["Approve", "Request changes", "Request review", "Close"]) {
    const button = await screen.findByRole("button", { name });
    await waitFor(() => expect(button.getAttribute("title"), name).toBe(reason));
  }
  for (const button of screen.getAllByRole("button", { name: "Comment" })) {
    expect(button.getAttribute("title")).toBe(reason);
  }
  cleanup();

  mount("#/code/repos/web/changes/42?tab=files&file=app.go", viewer);
  // THE OUTDATED FINDING, under its Disclosure: opened, its reply is drawn.
  fireEvent.click(await screen.findByText("1 outdated on this file"));
  const reply = await screen.findByRole("button", { name: "Reply" });
  await waitFor(() => expect(reply.getAttribute("title")).toBe(reason));
  expect(acted).toHaveLength(0);
});
