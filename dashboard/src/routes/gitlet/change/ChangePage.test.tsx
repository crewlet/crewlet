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
import { engineJson } from "~/test/engineAnswer.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { ChangePage } from "./ChangePage.tsx";
import { CHANGE_POLL_MS } from "./reads.ts";

beforeEach(() => sessionStorage.clear());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
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
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "native" },
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
    "code_merge_change",
    "code_rerun_checks",
    "code_update_change",
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

// THE STUB PLAYS THE ENGINE, so its answers carry the engine's mark: a
// refusal without it reads as a proxy's, an outcome nobody knows.
const json = (payload: unknown, status = 200) => engineJson(payload, status);

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** Every `code_checks` question the engine was asked, newest last. */
const checksAsked: URL[] = [];
/** Every `code_change` question the engine was asked, newest last. */
const changeAsked: URL[] = [];

/** The `standing` part's answer by default: the reader's one standing finding, on one page. */
const ONE_STANDING = (): Response =>
  json({ position: POSITION, level: "session", findings: [MINE] });

/**
 * The engine: its reads by part — the detail `change` answers on every ask,
 * and a part `parts` names as it answers, in place of the default — and each
 * act recorded and answered as `answer` says; a file's patch as `patch`'s
 * pages, page n answered from the cursor `p:<n>`, as the engine cuts one at a
 * line, and a revision's files as `files`' pages, page n answered from the
 * cursor `f:<n>`.
 */
function stubEngine(
  change: Record<string, unknown> | (() => Record<string, unknown>),
  answer: () => Promise<Response> | Response,
  parts: Readonly<Record<string, (url: URL) => Promise<Response> | Response>> = {},
  patch: readonly string[] = [PATCH],
  files: readonly (readonly Record<string, unknown>[])[] = [
    [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
  ],
) {
  changeAsked.length = 0;
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
      changeAsked.push(url);
      const page = { position: POSITION, level: "session" };
      const named = parts[url.searchParams.get("part") ?? ""];
      if (named) return named(url);
      // THE PAGE A CURSOR NAMES, a patch's (`p:<n>`) or a listing's (`f:<n>`).
      const pageAt = Number((url.searchParams.get("cursor") ?? "p:0").slice(2));
      switch (url.searchParams.get("part")) {
        case "files":
          return json({
            ...page,
            ...(pageAt + 1 < files.length ? { next: `f:${pageAt + 1}` } : {}),
            revision: 2,
            diff: {
              base: HEAD,
              head: HEAD,
              files: files[pageAt],
              total: files.flat().length,
              added: 1,
              deleted: 1,
            },
          });
        case "diff":
          return json({
            ...page,
            ...(pageAt + 1 < patch.length ? { next: `p:${pageAt + 1}` } : {}),
            revision: 2,
            diff: {
              base: HEAD,
              head: HEAD,
              files: [],
              total: 1,
              added: 1,
              deleted: 1,
              patch: patch[pageAt],
            },
          });
        case "findings":
          return json({
            ...page,
            revision: 2,
            findings: [MINE],
            placed: { "f-mine": { start: 9, end: 9, outdated: true } },
          });
        case "standing":
          return ONE_STANDING();
        case "comments":
        case "reviews":
          return json(page);
        default:
          return json({
            ...page,
            change: typeof change === "function" ? change() : change,
            revisions: [],
          });
      }
    }),
  );
  return acted;
}

function mount(hash = "#/gitlet/repos/web/changes/42", viewer: unknown = VIEWER) {
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

/** A finding the reader raised before MINE, which the standing read's first page holds. */
const EARLIER = { ...MINE, id: "f-earlier", title: "The backoff never grows" };

// THE REVIEW CARRIES EVERY STANDING FINDING, read to the standing part's last
// page: a review of a later revision supersedes each it leaves out, so one on
// a page the dialog never read would be superseded unseen. Mutation: stop the
// standing read after its first page, and f-mine is not carried.
test("a review carries a standing finding on the standing read's second page", async () => {
  const acted = stubEngine(
    detail(),
    () => json({ outcome: "applied", position: "CREWLET_CODE_LOG@1:13" }),
    {
      standing: (url) =>
        url.searchParams.get("cursor") === "f-earlier"
          ? json({ position: POSITION, level: "session", findings: [MINE] })
          : json({ position: POSITION, level: "session", findings: [EARLIER], next: "f-earlier" }),
    },
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Request changes" }));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("The retry never stops");
  expect(within(dialog).getByText("The backoff never grows")).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Request changes" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.args.carries).toEqual(["f-earlier", "f-mine"]);
  const asked = changeAsked.filter((u) => u.searchParams.get("part") === "standing");
  expect(asked.map((u) => u.searchParams.get("cursor"))).toEqual([null, "f-earlier"]);
});

// NO REVIEW LEAVES WHILE THE STANDING READ IS OUT: sent then, it carries
// none and supersedes every one. The press is held with why, and the
// dialog's form submitted (Enter) sends nothing either. Mutation: drop the
// hold while the read is out (the button's and submit's), and the review is
// sent.
test("a review is held while the reader's standing findings are being read", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }), {
    standing: () => new Promise<Response>(() => {}),
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
  const dialog = await screen.findByRole("dialog");
  const press = within(dialog).getByRole("button", { name: "Approve" });
  await waitFor(() =>
    expect(press.getAttribute("title")).toMatch(/Reading your standing findings/),
  );
  fireEvent.click(press);
  fireEvent.submit(within(dialog).getByRole("textbox", { name: "Summary" }).closest("form")!);
  await settled(() => vi.mocked(fetch).mock.calls.length);
  expect(acted).toHaveLength(0);
});

// A REFUSED STANDING READ HOLDS THE REVIEW with the reason, until the reader
// reads them again and the read lands. Mutation: release the hold on a
// refusal, and the review is sent carrying nothing.
test("a refused standing read holds the review until it is read again", async () => {
  let refuse = true;
  const acted = stubEngine(
    detail(),
    () => json({ outcome: "applied", position: "CREWLET_CODE_LOG@1:13" }),
    {
      standing: () =>
        refuse ? json({ error: "query_failed", message: "the read failed" }, 500) : ONE_STANDING(),
    },
  );
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
  const dialog = await screen.findByRole("dialog");
  const press = within(dialog).getByRole("button", { name: "Approve" });
  await waitFor(() => expect(press.getAttribute("title")).toMatch(/were not read/));
  fireEvent.click(press);
  await settled(() => vi.mocked(fetch).mock.calls.length);
  expect(acted).toHaveLength(0);

  refuse = false;
  fireEvent.click(within(dialog).getByRole("button", { name: "Read them again" }));
  await within(dialog).findByText("The retry never stops");
  fireEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.args.carries).toEqual(["f-mine"]);
});

// A DIFF IS READ ONCE FOR WHAT IT IS A FUNCTION OF: a revision's files and a
// file's patch are git reads on the home git node whose answer moves only
// with the revision's head and the base its facts record, so minutes on the
// Files tab ask neither again while the detail says neither has moved — and a
// version the detail moves asks both again, as a base its facts record anew
// does. Mutation: poll the files or diff part on a clock again, or drop the
// re-ask on the version or on the facts, and this fails.
test("the Files tab asks for a diff again only when the change moves", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let version = 5;
  let base = HEAD;
  const facts = () => ({
    n: 2,
    head: HEAD,
    pusher: "dev",
    pusher_kind: "agent",
    position: POSITION,
    at: "2026-10-27T10:00:00Z",
    facts: { target_ref: "refs/heads/main", target_oid: HEAD, base_oid: base, touched: ["app.go"] },
  });
  stubEngine(
    () => detail({ version }),
    () => json({}),
    {
      "": () =>
        json({
          position: POSITION,
          level: "session",
          change: detail({ version }),
          revisions: [facts()],
        }),
    },
  );
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");
  expect(await screen.findByText("1 outdated on this file")).toBeTruthy();
  const askedFor = (part: string) =>
    changeAsked.filter((u) => u.searchParams.get("part") === part).length;
  const details = () => changeAsked.filter((u) => !u.searchParams.get("part")).length;
  await waitFor(() => expect(askedFor("diff")).toBe(1));
  expect(askedFor("files")).toBe(1);

  const before = details();
  for (let minute = 0; minute < 3; minute++) {
    await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  }
  await waitFor(() => expect(details()).toBeGreaterThanOrEqual(before + 3));
  expect(askedFor("files")).toBe(1);
  expect(askedFor("diff")).toBe(1);

  version = 6;
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(askedFor("files")).toBe(2));
  await waitFor(() => expect(askedFor("diff")).toBe(2));

  // A RETARGET'S NEW FACTS land in a record of the home's own, after the one
  // that moved the version: the base they record moves the diff too.
  base = "fedcba9876543210fedcba9876543210fedcba98";
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(askedFor("files")).toBe(3));
  await waitFor(() => expect(askedFor("diff")).toBe(3));
});

// FINDINGS AND COMMENTS PAST ONE PAGE ARE LOADED on the Files tab as the
// reader asks: the pages run oldest first, so what is left off is the newest,
// and a reviewer who cannot find what they raised re-raises it. Load more asks
// the findings' page after the cursor the first ended at, of the revision on
// screen, draws the later page's finding on the line ITS page placed it on —
// leaving the first page's outdated one where it was — and is gone once none
// is left. Mutation: drop the pager, ask the second page without its cursor,
// or place a finding from the first page's placings alone, and this fails.
test("the Files tab loads a change's findings past one page when the reader asks", async () => {
  const asked: URL[] = [];
  stubEngine(detail(), () => json({}), {
    findings: (url) => {
      asked.push(url);
      const later = url.searchParams.get("cursor") === "f-mine";
      return json({
        position: POSITION,
        level: "session",
        revision: 2,
        findings: later
          ? [{ ...MINE, id: "f-late", title: "The newest finding", line_start: 2, line_end: 2 }]
          : [MINE],
        placed: later
          ? { "f-late": { start: 2, end: 2 } }
          : { "f-mine": { start: 9, end: 9, outdated: true } },
        ...(later ? {} : { next: "f-mine" }),
      });
    },
  });
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");
  expect(await screen.findByText("1 outdated on this file")).toBeTruthy();
  expect(screen.queryByText("The newest finding")).toBeNull();
  fireEvent.click(await screen.findByRole("button", { name: "Load more findings and comments" }));
  expect(await screen.findByText("The newest finding")).toBeTruthy();
  expect(screen.getByText("1 outdated on this file")).toBeTruthy();
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Load more findings and comments" })).toBeNull(),
  );
  const second = asked.find((u) => u.searchParams.get("cursor") === "f-mine")!;
  expect(second.searchParams.get("revision")).toBe("2");
});

// A LATER PAGE THAT FAILS IS SAID BESIDE THE PRESS THAT ASKS IT AGAIN, and
// what the timeline holds stays drawn: a press that did nothing visible left
// the reader nothing to do but reload. Mutation: drop the notice, and this
// fails.
test("the conversation says when its next page could not be read", async () => {
  stubEngine(detail(), () => json({}), {
    reviews: (url) =>
      url.searchParams.get("cursor") === "r1"
        ? json({ error: "query_failed" }, 500)
        : json({
            position: POSITION,
            level: "session",
            reviews: [
              {
                id: "r1",
                revision: 1,
                reviewer: "jane",
                reviewer_kind: "person",
                verdict: "comment",
                summary: "A first look",
                findings: 0,
                position: { ...POSITION, seq: 20 },
                at: "2026-10-28T10:00:00Z",
              },
            ],
            next: "r1",
          }),
  });
  mount();
  expect(await screen.findByText("A first look")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(
    await screen.findByText("Could not read the next page (query_failed). Try again."),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();
  expect(screen.getByText("A first look")).toBeTruthy();
});

// THE FILES TAB SAYS IT TOO: a later page of findings that fails is said
// beside the press that asks it again, and the findings held stay drawn.
// Mutation: drop the notice, and this fails.
test("the Files tab says when the findings' next page could not be read", async () => {
  stubEngine(detail(), () => json({}), {
    findings: (url) =>
      url.searchParams.get("cursor") === "f-mine"
        ? json({ error: "query_failed" }, 500)
        : json({
            position: POSITION,
            level: "session",
            revision: 2,
            findings: [MINE],
            placed: { "f-mine": { start: 9, end: 9, outdated: true } },
            next: "f-mine",
          }),
  });
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");
  fireEvent.click(await screen.findByRole("button", { name: "Load more findings and comments" }));
  expect(
    await screen.findByText("Could not read the next page (query_failed). Try again."),
  ).toBeTruthy();
  expect(screen.getByText("1 outdated on this file")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Load more findings and comments" })).toBeTruthy();
});

// THE TIMELINE IS EVERY PAGE THE READER LOADED: the engine pages a change's
// reviews oldest first, so the newest review is on a later page, and Load
// more draws it in the timeline beside the first. Mutation: draw the first
// page alone, or say that more exist without reading them, and the newest
// review is never drawn.
test("the conversation draws a change's newest review once the reader loads its next page", async () => {
  const review = (id: string, seq: number, summary: string) => ({
    id,
    revision: 1,
    reviewer: "jane",
    reviewer_kind: "person",
    verdict: "comment",
    summary,
    findings: 0,
    position: { ...POSITION, seq },
    at: "2026-10-28T10:00:00Z",
  });
  stubEngine(detail(), () => json({}), {
    reviews: (url) =>
      url.searchParams.get("cursor") === "r1"
        ? json({
            position: POSITION,
            level: "session",
            reviews: [review("r2", 30, "A second look")],
          })
        : json({
            position: POSITION,
            level: "session",
            reviews: [review("r1", 20, "A first look")],
            next: "r1",
          }),
  });
  mount();
  expect(await screen.findByText("A first look")).toBeTruthy();
  expect(screen.queryByText("A second look")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("A second look")).toBeTruthy();
  expect(screen.getByText("A first look")).toBeTruthy();
  await waitFor(() => expect(screen.queryByRole("button", { name: "Load more" })).toBeNull());
});

// THE REVIEWS AND COMMENTS ARE ASKED AGAIN ONLY WHEN SOMEBODY WROTE: their
// rows are written by a review, a comment or a resolved thread alone, so a
// move of the change and its conversation that nobody wrote — a push to its
// head, the mergeability its home records again each time the target branch
// moves — asks neither again, however many pages the reader holds, and a
// record somebody wrote (`spoken_at`) asks every page held again. Mutation:
// key the two parts on the conversation's version again, or drop the re-ask
// on `spoken_at`, and this fails.
test("the conversation asks its reviews and comments again only when somebody wrote", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let version = 5;
  let conversationVersion = 3;
  let spokenAt = "2026-10-28T10:00:00Z";
  const conversation = () => ({
    comments: 0,
    reviews: 2,
    open_findings: 0,
    open_blockers: 0,
    changes_requested: 0,
    version: conversationVersion,
    position: POSITION,
    spoken_at: spokenAt,
  });
  const review = (id: string, seq: number, summary: string) => ({
    id,
    revision: 1,
    reviewer: "jane",
    reviewer_kind: "person",
    verdict: "comment",
    summary,
    findings: 0,
    position: { ...POSITION, seq },
    at: "2026-10-28T10:00:00Z",
  });
  stubEngine(
    () => detail({ version, conversation: conversation() }),
    () => json({}),
    {
      reviews: (url) =>
        url.searchParams.get("cursor") === "r1"
          ? json({
              position: POSITION,
              level: "session",
              reviews: [review("r2", 30, "A second look")],
            })
          : json({
              position: POSITION,
              level: "session",
              reviews: [review("r1", 20, "A first look")],
              next: "r1",
            }),
    },
  );
  mount();
  expect(await screen.findByText("A first look")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("A second look")).toBeTruthy();
  const asked = (part: string, cursor = "") =>
    changeAsked.filter(
      (u) => u.searchParams.get("part") === part && (u.searchParams.get("cursor") ?? "") === cursor,
    ).length;
  const details = () => changeAsked.filter((u) => !u.searchParams.get("part")).length;
  expect(asked("reviews")).toBe(1);
  expect(asked("reviews", "r1")).toBe(1);
  expect(asked("comments")).toBe(1);

  // A PUSH AND A MERGEABILITY RECORDED AGAIN, three minutes running: the
  // detail moves both versions, and nobody wrote anything.
  for (let minute = 0; minute < 3; minute++) {
    version++;
    conversationVersion++;
    const before = details();
    await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
    await waitFor(() => expect(details()).toBeGreaterThan(before));
  }
  expect(asked("reviews")).toBe(1);
  expect(asked("reviews", "r1")).toBe(1);
  expect(asked("comments")).toBe(1);

  // A THREAD RESOLVED, which adds no row and moves only the instant: every
  // page held is asked again.
  conversationVersion++;
  spokenAt = "2026-10-28T11:00:00Z";
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(asked("reviews")).toBe(2));
  await waitFor(() => expect(asked("reviews", "r1")).toBe(2));
  await waitFor(() => expect(asked("comments")).toBe(2));
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
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");

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

// A CHANGE'S PATCH IS READ WHOLE, A PAGE AT A TIME: a file whose patch runs
// past one answer is drawn on as the reader asks, each later page the `diff`
// part's own question from the cursor the page before ended at, at the same
// revision. Mutation: ask the next page without its cursor, or draw the first
// page's text alone, and this fails.
test("a change's patch past one page draws the rest of its hunks when the reader asks", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }), {}, [
    `${PATCH}\n`,
    ["@@ -40,2 +40,2 @@", " still", "-alpha", "+omega"].join("\n"),
  ]);
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");

  fireEvent.click(await screen.findByRole("button", { name: "Load more lines" }));

  expect(await screen.findByRole("button", { name: "Line 41: pick it" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Line 2: pick it" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more lines" })).toBeNull();
  const second = changeAsked.find((u) => u.searchParams.get("cursor") === "p:1");
  expect(second?.searchParams.get("part")).toBe("diff");
  expect(second?.searchParams.get("file")).toBe("app.go");
  expect(second?.searchParams.get("revision")).toBe("2");
});

// A REVISION'S FILES ARE LISTED A PAGE AT A TIME, ALL OF THEM: Load more
// files asks the `files` part from the cursor the page before ended at, at
// the same revision, and the files it lists open like any other. Mutation:
// draw the first page alone, or ask the next without its cursor, and this
// fails.
test("a change's files past one page are listed when the reader asks", async () => {
  stubEngine(
    detail(),
    () => json({ outcome: "applied" }),
    {},
    [PATCH],
    [
      [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
      [{ path: "z.go", status: "A", added: 2, deleted: 0 }],
    ],
  );
  mount("#/gitlet/repos/web/changes/42?tab=files");

  const list = await screen.findByRole("list", { name: "Files changed" });
  expect(await within(list).findByRole("button", { name: /app\.go/ })).toBeTruthy();
  expect(within(list).queryByRole("button", { name: /z\.go/ })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Load more files" }));

  expect(await within(list).findByRole("button", { name: /z\.go/ })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more files" })).toBeNull();
  const second = changeAsked.find((u) => u.searchParams.get("cursor") === "f:1");
  expect(second?.searchParams.get("part")).toBe("files");
  expect(second?.searchParams.get("revision")).toBe("2");
});

// A CHANGE'S CHECKS ARE THE ATTEMPTS THAT JUDGE IT: its head's, asked by the
// change's number on its repository — never a listing of the repository's —
// each with its state in its verdict's words. Mutation: ask the repository's
// listing (drop `change=`), and this fails.
test("the Checks tab lists the attempts that judge the change, asked by its number", async () => {
  checksAsked.length = 0;
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=checks");
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
test("the Notified tab names each recipient with why they heard, in the third person", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=notified");
  const list = await screen.findByRole("list", { name: "Whom it notified" });
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
  mount("#/gitlet/repos/web/changes/42", viewer);
  await screen.findByRole("tabpanel");
  for (const name of ["Approve", "Request changes", "Request review", "Close"]) {
    const button = await screen.findByRole("button", { name });
    await waitFor(() => expect(button.getAttribute("title"), name).toBe(reason));
  }
  for (const button of screen.getAllByRole("button", { name: "Comment" })) {
    expect(button.getAttribute("title")).toBe(reason);
  }
  cleanup();

  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go", viewer);
  // THE OUTDATED FINDING, under its Disclosure: opened, its reply is drawn.
  fireEvent.click(await screen.findByText("1 outdated on this file"));
  const reply = await screen.findByRole("button", { name: "Reply" });
  await waitFor(() => expect(reply.getAttribute("title")).toBe(reason));
  expect(acted).toHaveLength(0);
});

// A COMMENT ON THE CHANGE SENDS AS EVERY MESSAGE BOX DOES: Enter sends,
// Shift+Enter is a new line, an empty box sends nothing, and the field's
// description names the keys.
test("a comment on the change sends on Enter, and Shift+Enter is a new line", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  const field = (await screen.findByRole("textbox", { name: "Comment" })) as HTMLTextAreaElement;
  const described = (field.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain("Enter to send, Shift plus Enter for a new line");
  expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(false);
  fireEvent.change(field, { target: { value: "Looks right" } });
  expect(fireEvent.keyDown(field, { key: "Enter", shiftKey: true })).toBe(true);
  expect(acted).toHaveLength(0);
  fireEvent.keyDown(field, { key: "Enter" });
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_comment_on_change",
    args: { change: "web!42", body: "Looks right" },
  });
});

// A DRAFT'S NEXT STEP IS BEING MARKED READY, and the page offers it: Ready
// for review is the bar's primary action, one code_update_change against the
// version on screen; and a draft is offered no merge. Mutation: drop the
// action, send another version, or draw the merge controls on a draft, and
// this fails.
test("a draft is marked ready for review from the page bar, and offers no merge", async () => {
  const acted = stubEngine(
    detail({
      state: "draft",
      readiness: {
        met: 0,
        total: 1,
        first: 'web!42 is a draft: its owner seat "dev" marks it ready for review',
        preconditions: [
          {
            kind: "draft",
            state: "unmet",
            waits_on: [{ handle: "dev", kind: "agent" }],
            detail: 'web!42 is a draft: its owner seat "dev" marks it ready for review',
          },
        ],
      },
    }),
    () => json({ outcome: "applied" }),
  );
  mount();
  const ready = await screen.findByRole("button", { name: "Ready for review" });
  expect(
    await screen.findByText(
      "0 of 1 condition to merge met. A draft merges nowhere. Mark it ready for review first.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Merge" })).toBeNull();
  expect(screen.queryByRole("radio", { name: "Squash" })).toBeNull();
  await waitFor(() => expect(ready.getAttribute("title")).toBeNull());
  fireEvent.click(ready);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_update_change",
    args: { change: "web!42", expected_version: 5, draft: false },
  });
});

// ONE ACTION IN VIEW ON A PHONE: the bar hides `page-action-folds` at that
// width and the same writes are in its More menu, so only the primary action
// is left outside a fold. Mutation: draw the secondary writes bare, and the
// bar runs off a phone's edge again, and this fails.
test("the change's secondary writes fold on a phone, its primary action stays", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  const approve = await screen.findByRole("button", { name: "Approve" });
  expect(approve.closest(".page-action-folds")).toBeNull();
  for (const name of ["Request changes", "Request review", "Close"]) {
    const button = await screen.findByRole("button", { name });
    expect(button.closest(".page-action-folds"), name).not.toBeNull();
  }
});

// A DIFF IS READ FOR ITS CHANGES: with no file in the address the tab opens
// on the first one, as a commit and a comparison do. Mutation: wait for a
// press, and the first screen is a list of names over an empty pane.
test("a change's Files tab opens on its first file", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=files");
  expect(await screen.findByRole("region", { name: "Changes to app.go" })).toBeTruthy();
  expect(screen.queryByText("Choose a file to read its changes.")).toBeNull();
});

// A CHANGE THE ENGINE DOES NOT HOLD IS NAMED, WITH A WAY ON: the
// repository's changes and every repository, never the frame's bare "Not
// found". Mutation: fall through to the generic refusal, and this fails.
test("a change the engine does not hold names itself and offers the repository's changes", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      return json({ error: "not_found" }, 404);
    }),
  );
  mount();
  expect(await screen.findByText("web has no change 42")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open web's changes" }).getAttribute("href")).toBe(
    "#/gitlet?repo=web",
  );
  expect(screen.getByRole("link", { name: "Every repository" }).getAttribute("href")).toBe(
    "#/gitlet/repos",
  );
});

// ---- The merge controls (`Merge.tsx`) ----------------------------------- //

/** A readiness whose every condition is met, for a target whose gate requires `check` or not. */
function ready(withCheck = false) {
  const preconditions = [
    { kind: "approval", state: "met", detail: "Approved by cto." },
    ...(withCheck ? [{ kind: "check", state: "met", detail: "unit passed." }] : []),
    { kind: "merge_authority", state: "met", detail: "jane merges into refs/heads/main." },
  ];
  return {
    readiness: { met: preconditions.length, total: preconditions.length, first: "", preconditions },
  };
}

// HELD WITH THE GATE'S OWN SENTENCE while the gate is unmet, and sent nowhere
// on a press; once the gate holds, ONE code_merge_change of the head ON
// SCREEN (`expected_head`), with the method chosen and nothing the person
// did not choose. Mutation: send another head, drop expected_head, send
// delete_branch unasked, or let an unmet gate's press through, and this fails.
test("Merge is held by the gate's sentence, and sends the head on screen once it holds", async () => {
  let acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  const held = await screen.findByRole("button", { name: "Merge" });
  // THE DASHBOARD'S WORDS for the first unmet condition, never the engine's sentence.
  await waitFor(() =>
    expect(held.getAttribute("title")).toBe("No approval counts on revision 2 yet."),
  );
  fireEvent.click(held);
  expect(acted).toHaveLength(0);
  cleanup();

  acted = stubEngine(detail(ready()), () => json({ outcome: "applied" }));
  mount();
  fireEvent.click(await screen.findByRole("radio", { name: "Squash" }));
  const merge = await screen.findByRole("button", { name: "Merge" });
  await waitFor(() => expect(merge.getAttribute("title")).toBeNull());
  fireEvent.click(merge);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_merge_change",
    args: { change: "web!42", expected_head: HEAD, method: "squash" },
  });
});

// A GATE REQUIRING A CHECK LANDS THROUGH ITS QUEUE, so the control says what
// the press does: Add to queue. Mutation: label it Merge whatever the gate,
// and this fails.
test("a target whose gate requires a check is merged by adding the change to its queue", async () => {
  const acted = stubEngine(detail(ready(true)), () => json({ outcome: "applied" }));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Add to queue" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]?.tool).toBe("code_merge_change");
  expect(screen.queryByRole("button", { name: "Merge" })).toBeNull();
});

// A REFUSAL IN THE CONTROLS' OWN WORDS, keyed on Gitlet's word beside
// the class: a head that moved is said as a push since the page was read,
// never as the class's "changed by somebody else"; and a gate the change does
// not meet yet (`not_ready`) is the engine's words, which name what it still
// waits for and whom. Mutation: draw the class's sentence for head_moved, or
// ours for not_ready, and this fails.
test.each([
  [
    "head_moved",
    { error: "stale_version", refusal: "head_moved", detail: "code_merge_change: moved." },
    409,
    /Somebody pushed to it since you loaded it/,
  ],
  [
    "gate_unmet",
    {
      error: "not_ready",
      refusal: "gate_unmet",
      detail:
        "It does not meet its target's merge gate yet, so nothing was merged or queued. It still waits for an approval of its latest revision. @cto can move it forward.",
    },
    409,
    /It still waits for an approval of its latest revision\. @cto can move it forward\./,
  ],
])("a %s refusal is drawn in its own words", async (_word, body, status, said) => {
  stubEngine(detail(ready()), () => json(body, status));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Merge" }));
  expect(await screen.findByText(said)).toBeTruthy();
});

// OVERRIDE IS A PERSON'S, WITH A REASON: held for anybody else; held in its
// dialog until a reason is given; and then one code_merge_change carrying it.
// Mutation: offer it to a seat, send it without a reason, or drop
// override_reason, and this fails.
test("Override is a person's alone, and carries the reason they give", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/gitlet/repos/web/changes/42", { ...VIEWER, kind: "agent" });
  const seat = await screen.findByRole("button", { name: "Override" });
  await waitFor(() =>
    expect(seat.getAttribute("title")).toBe("Only a person overrides a merge gate."),
  );
  cleanup();

  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Override" }));
  const dialog = await screen.findByRole("dialog");
  const send = within(dialog).getByRole("button", { name: "Merge past the gate" });
  expect(send.getAttribute("title")).toBe("Say why it merges past its gate.");
  fireEvent.change(within(dialog).getByRole("textbox", { name: "Reason" }), {
    target: { value: "Hotfix for the outage" },
  });
  fireEvent.click(send);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_merge_change",
    args: {
      change: "web!42",
      expected_head: HEAD,
      method: "merge",
      override_reason: "Hotfix for the outage",
    },
  });
});

// RE-RUN CHECKS is one code_rerun_checks of every check of the change's head
// that did not pass. Mutation: name the change otherwise, or drop all_failed,
// and this fails.
test("Re-run checks runs every failed check of the change's head again", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Re-run checks" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_rerun_checks",
    args: { change: "web!42", all_failed: true },
  });
});
