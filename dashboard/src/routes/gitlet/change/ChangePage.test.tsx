/**
 * A change's page reads the change a part at a time over REST, and writes
 * through the act transport — never optimistically.
 *
 * The invariants worth breaking a build over: a review is ONE
 * `code_review_change` of the head revision carrying every finding drafted on
 * its lines and the reviewer's standing findings; a draft is the tab's —
 * kept in sessionStorage until that one press — and forgotten only once the
 * engine has taken the review; nothing on screen moves before the engine
 * answers; a change no longer live offers only what still applies to it, and
 * its author's verdicts are held with the reason; an outdated finding is said
 * under a Disclosure, never dropped; and whom its records woke is said in the
 * voice of somebody else's notice.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { draftsKey } from "~/lib/reviewDrafts.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { blobAnswer, inkedLine } from "~/test/codeScreens.tsx";
import { engineJson } from "~/test/engineAnswer.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { ChangePage } from "./ChangePage.tsx";
import { EDIT_MOVED } from "./Description.tsx";
import { FOLD_FROM_LINES } from "./FileStack.tsx";
import { NOT_AUTHOR_RESOLVE } from "./Finding.tsx";
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
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
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
          // THE WALK OF EVERY FILE'S PATCH ends at once here: each file is read alone.
          if (url.searchParams.get("patches") === "true") {
            return json({
              ...page,
              revision: 2,
              diff: { base: HEAD, head: HEAD, files: [], total: 1, added: 1, deleted: 1 },
            });
          }
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
        case "reviewers":
        case "excerpts":
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

/** Opens the page bar's Review menu and presses its item `name`. */
async function review(name: string) {
  fireEvent.click(await screen.findByRole("button", { name: "Review web!42" }));
  fireEvent.click(await screen.findByRole("menuitem", { name }));
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

  await review("Approve");
  const dialog = await screen.findByRole("dialog");
  // THE REVISION IS NAMED ONCE, by the subtitle; the drafts are "on this revision".
  expect(within(dialog).getByText(/1 drafted finding on this revision/)).toBeTruthy();
  expect(
    within(dialog)
      .getAllByText(/revision 2/i)
      .map((e) => e.textContent),
  ).toEqual(["Revision 2"]);
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
  await review("Request changes");
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
  await review("Approve");
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
  await review("Approve");
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
// file's patch are git reads on the home Gitlet node whose answer moves only
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
  // THE WALK OF EVERY PATCH, and the file it ended without read alone.
  await waitFor(() => expect(askedFor("diff")).toBe(2));
  // THE TAB ROW'S COUNTS ask the head's files beside the tab's own read, on its key.
  expect(askedFor("files")).toBe(2);

  const before = details();
  for (let minute = 0; minute < 3; minute++) {
    await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  }
  await waitFor(() => expect(details()).toBeGreaterThanOrEqual(before + 3));
  expect(askedFor("files")).toBe(2);
  expect(askedFor("diff")).toBe(2);

  version = 6;
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(askedFor("files")).toBe(4));
  await waitFor(() => expect(askedFor("diff")).toBe(4));

  // A RETARGET'S NEW FACTS land in a record of the home's own, after the one
  // that moved the version: the base they record moves the diff too.
  base = "fedcba9876543210fedcba9876543210fedcba98";
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(askedFor("files")).toBe(6));
  await waitFor(() => expect(askedFor("diff")).toBe(6));
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

// A byline is a sentence: its time is said in words, the exact instant kept in its title.
test("the header says when the change was opened in words", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine(detail(), () => json({}));
  mount();
  await waitFor(() => {
    const when = document.querySelector(".code-change-byline .code-byline__when");
    expect(when?.textContent).toBe("yesterday at 11:00");
    expect(when?.getAttribute("title")).toBe(fmtDateTime("2026-10-27T10:00:00Z"));
  });
});

// THE HEADER WEARS THE STATE PILL; the byline's branches are linked chips, the
// rail's plain ones. Mutation: draw the branches as plain names, or drop the
// spaces around "into", and this fails.
test("the header's state is its pill, and its branches are chips in the byline and the rail", async () => {
  stubEngine(detail(), () => json({}));
  mount();
  const byline = await waitFor(() => {
    const chips = [
      ...document.querySelectorAll<HTMLElement>(".code-change-byline .code-branch-chip"),
    ];
    expect(chips).toHaveLength(2);
    return chips;
  });
  const pill = document.querySelector<HTMLElement>('.code-state[data-state="open"]');
  expect(pill?.textContent).toBe("Open");
  expect(pill?.querySelector("svg")).not.toBeNull();
  expect(byline.map((a) => [a.tagName, a.textContent, a.getAttribute("href")])).toEqual([
    ["A", "retry", "#/gitlet/repos/web?ref=retry"],
    ["A", "main", "#/gitlet/repos/web"],
  ]);
  for (const a of byline) expect(a.classList.contains("prose-link")).toBe(true);
  const rail = await waitFor(() => {
    const branches = document.querySelector<HTMLElement>(".code-rail__branches");
    expect(branches).not.toBeNull();
    return branches!;
  });
  const chips = [...rail.querySelectorAll<HTMLElement>(".code-branch-chip")];
  expect(chips.map((c) => c.tagName)).toEqual(["SPAN", "SPAN"]);
  expect(rail.textContent).toBe("retry into main");
});

// THE HEADER IS ONE SENTENCE: the pill, who wants to merge which revision of
// which branch into which, when it was opened and its work item.
// Mutation: draw the time as a cell (no "opened"), or drop the copy, and this fails.
test("the header says who wants to merge which revision of which branch into which, and when", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-27T13:00:00Z"));
  stubEngine(
    detail({ work_item: { backend: "worklet", id: "ENG-415", key: "ENG-415", project: "ENG" } }),
    () => json({}),
  );
  mount();
  const line = await waitFor(() => {
    const said = document.querySelector<HTMLElement>(".code-change-byline");
    expect(said?.textContent).toMatch(/opened 3 hours ago/);
    return said!;
  });
  expect(line.firstElementChild?.querySelector(".code-state")).not.toBeNull();
  expect(line.textContent).toMatch(
    /^OpenEngineer\s*wants to merge revision 2 of retry\s*into main\s*·?opened 3 hours agoENG-415$/,
  );
  expect(within(line).getByRole("button", { name: "Copy the branch name" })).toBeTruthy();
  expect(within(line).getByRole("link", { name: "ENG-415" }).getAttribute("href")).toBe(
    "#/gitlet?work=worklet%3AENG-415&state=all",
  );
  expect(line.querySelector("time")?.getAttribute("title")).toBe(
    fmtDateTime("2026-10-27T10:00:00Z"),
  );
});

// REVIEW IS ONE DEFAULT BUTTON, its menu the three verdicts and Request review,
// each opening its dialog. Mutation: make the trigger primary, and this fails.
test("Review is one default button whose menu holds the verdicts and Request review", async () => {
  stubEngine(detail(), () => json({}));
  mount();
  const trigger = await screen.findByRole("button", { name: "Review web!42" });
  expect(trigger.className).not.toMatch(/primary/);
  for (const button of document.querySelectorAll(".page-actions button")) {
    expect(button.className, button.textContent ?? "").not.toMatch(/primary/);
  }
  fireEvent.click(trigger);
  const items = await screen.findAllByRole("menuitem");
  expect(items.map((i) => i.textContent)).toEqual([
    "Approve",
    "Request changes",
    "Comment",
    "Request review",
  ]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Request review" }));
  expect(await screen.findByRole("dialog", { name: "Request review of web!42" })).toBeTruthy();
});

/** The detail with a conversation of `comments` comments and two reviews. */
const talked = (comments: number) =>
  detail({
    conversation: {
      comments,
      reviews: 2,
      open_findings: 0,
      open_blockers: 0,
      changes_requested: 0,
      version: 1,
      position: POSITION,
    },
  });

// THE TABS AND THEIR COUNTS: Conversation counts comments alone, Revisions the
// head's number, Files changed the head's files (the diffstat note is a layout case).
// Mutation: count reviews in Conversation's figure, and this fails.
test("the tabs are Conversation, Revisions, Checks, Files changed and More, with their counts", async () => {
  stubEngine(talked(3), () => json({}));
  mount();
  const row = await screen.findByRole("tablist", { name: "The change's sections" });
  await waitFor(() =>
    expect(within(row).getByRole("tab", { name: /^Files changed/ }).textContent).toBe(
      "Files changed1",
    ),
  );
  expect(
    within(row)
      .getAllByRole("tab")
      .map((t) => t.textContent),
  ).toEqual(["Conversation3", "Revisions2", "Checks", "Files changed1", "More"]);
});

// THE DIGITS FOLLOW THE TAB ORDER, and one past the end is left alone.
// Mutation: reorder CHANGE_TABS, and this fails.
test("the digits open the change's tabs in their order", async () => {
  stubEngine(detail(), () => json({}));
  mount();
  await screen.findByRole("tabpanel");
  const press = (key: string) =>
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));
    });
  press("2");
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web/changes/42?tab=revisions"));
  press("5");
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web/changes/42?tab=more"));
  press("6");
  await settled(() => location.hash.length);
  expect(location.hash).toBe("#/gitlet/repos/web/changes/42?tab=more");
});

// MORE HOLDS PROVENANCE AND NOTIFIED one under the other; an old key lands on
// the conversation. Mutation: keep a notified tab, and this fails.
test("More holds where the change came from and whom it notified, and an old key lands on Conversation", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=more");
  expect(await screen.findByRole("heading", { name: "Where it came from" })).toBeTruthy();
  expect(screen.getByRole("heading", { name: "Whom its records notified" })).toBeTruthy();
  expect(await screen.findByRole("list", { name: "Whom it notified" })).toBeTruthy();
  cleanup();
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=notified");
  const selected = await screen.findByRole("tab", { selected: true });
  expect(selected.textContent).toMatch(/^Conversation/);
});

// THE COUNTS' FILES READ IS FILES CHANGED'S OWN FIRST QUESTION.
// Mutation: ask the counts with other parameters, and this fails.
test("the files read for the counts is the Files changed tab's own first question", async () => {
  // A REVISION NO OTHER CASE READS: the tab's kept answers outlive a case.
  stubEngine(detail({ revision: 9 }), () => json({}), {
    files: () =>
      engineJson(
        {
          position: POSITION,
          level: "session",
          revision: 9,
          diff: {
            base: HEAD,
            head: HEAD,
            files: [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
            total: 1,
            added: 1,
            deleted: 1,
          },
        },
        200,
        { "Cache-Control": "private, max-age=31536000, immutable", ETag: '"files-2"' },
      ),
  });
  mount();
  await screen.findByRole("tab", { name: /^Files changed/ });
  const files = () => changeAsked.filter((u) => u.searchParams.get("part") === "files");
  await waitFor(() => expect(files()).toHaveLength(1));
  fireEvent.click(screen.getByRole("tab", { name: /^Files changed/ }));
  expect(await screen.findByRole("region", { name: "app.go" })).toBeTruthy();
  expect(files().length).toBeGreaterThan(0);
  for (const u of files()) {
    expect([u.searchParams.get("revision"), u.searchParams.get("since_revision")]).toEqual([
      "9",
      null,
    ]);
  }
});

// THE COUNTS ARE ASKED AGAIN ON FILES CHANGED'S OWN KEY: a base the head's
// facts record anew moves them on Conversation too, once. Mutation: read the
// counts once per head revision, and the tab keeps its old count.
test("a moved base asks the tab row's counts again once", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let base = HEAD;
  let total = 1;
  const revision = () => ({
    n: 2,
    head: HEAD,
    pusher: "dev",
    pusher_kind: "agent",
    position: POSITION,
    at: "2026-10-27T10:00:00Z",
    facts: { target_ref: "refs/heads/main", target_oid: HEAD, base_oid: base, touched: [] },
  });
  stubEngine(detail(), () => json({}), {
    "": () =>
      json({ position: POSITION, level: "session", change: detail(), revisions: [revision()] }),
    files: () =>
      json({
        position: POSITION,
        level: "session",
        revision: 2,
        diff: { base, head: HEAD, files: [], total, added: total, deleted: 0 },
      }),
  });
  mount();
  const tab = await screen.findByRole("tab", { name: /^Files changed/ });
  await waitFor(() => expect(tab.textContent).toBe("Files changed1"));
  const files = () => changeAsked.filter((u) => u.searchParams.get("part") === "files").length;
  const details = () => changeAsked.filter((u) => !u.searchParams.get("part")).length;
  expect(files()).toBe(1);
  let before = details();
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(details()).toBeGreaterThan(before));
  await settled(() => changeAsked.length);
  expect(files()).toBe(1);
  base = "fedcba9876543210fedcba9876543210fedcba98";
  total = 7;
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(tab.textContent).toBe("Files changed7"));
  await settled(() => changeAsked.length);
  expect(files()).toBe(2);
});

/** The detail with a description. */
const described = (extra: Record<string, unknown> = {}) =>
  detail({ body: "Retry with a backoff.\nUp to three times.", ...extra });

/** Opens the description's menu and presses Edit. */
async function edit() {
  fireEvent.click(await screen.findByRole("button", { name: "More actions on the description" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
  return screen.findByRole("form", { name: "Edit web!42" });
}

// EDIT IS ONE UPDATE of the fields that moved, against the version on screen.
// Mutation: send both fields always, and the second press sends the title too.
test("Edit sends the title and the description in one update against the version on screen", async () => {
  let acted = stubEngine(described(), () => json({ outcome: "applied" }));
  mount();
  let form = await edit();
  const title = within(form).getByRole("textbox", { name: "Title" });
  expect(document.activeElement).toBe(title);
  fireEvent.change(title, { target: { value: "Retry the upload with a backoff" } });
  fireEvent.change(within(form).getByRole("textbox", { name: "Description" }), {
    target: { value: "Three tries." },
  });
  fireEvent.click(within(form).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_update_change",
    args: {
      change: "web!42",
      expected_version: 5,
      title: "Retry the upload with a backoff",
      body: "Three tries.",
    },
  });
  await waitFor(() => expect(screen.queryByRole("form", { name: "Edit web!42" })).toBeNull());
  cleanup();

  acted = stubEngine(described(), () => json({ outcome: "applied" }));
  mount();
  form = await edit();
  fireEvent.change(within(form).getByRole("textbox", { name: "Description" }), {
    target: { value: "Three tries." },
  });
  fireEvent.submit(form);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.args).toEqual({ change: "web!42", expected_version: 5, body: "Three tries." });
});

// EDIT IS HELD while nothing moved, without a title, and when the words on
// screen moved under the reader; a push's version alone holds nothing.
// Mutation: hold on a moved version, and the push case is held.
test("Edit is held while nothing moved, without a title, and when the words changed under the reader", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let version = 5;
  let body = "Retry with a backoff.\nUp to three times.";
  const acted = stubEngine(
    () => detail({ version, body }),
    () => json({ outcome: "applied" }),
  );
  mount();
  const form = await edit();
  const save = within(form).getByRole("button", { name: "Save" });
  await waitFor(() =>
    expect(save.getAttribute("title")).toBe("Change the title or the description first."),
  );
  const title = within(form).getByRole("textbox", { name: "Title" });
  fireEvent.change(title, { target: { value: "  " } });
  expect(save.getAttribute("title")).toBe("A change needs a title.");
  fireEvent.change(title, { target: { value: "Retry, with a backoff" } });
  expect(save.getAttribute("title")).toBeNull();

  // A PUSH moves the version and nothing the reader is editing.
  version = 6;
  const details = () => changeAsked.filter((u) => !u.searchParams.get("part")).length;
  let before = details();
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(details()).toBeGreaterThan(before));
  await settled(() => changeAsked.length);
  expect(save.getAttribute("title")).toBeNull();

  // A SEAT'S EDIT of the description, under the reader.
  body = "Retry three times.";
  before = details();
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(save.getAttribute("title")).toBe(EDIT_MOVED));
  fireEvent.click(save);
  expect(acted).toHaveLength(0);
  fireEvent.click(within(form).getByRole("button", { name: "Start again from the latest" }));
  expect(
    (within(form).getByRole("textbox", { name: "Description" }) as HTMLTextAreaElement).value,
  ).toBe("Retry three times.");
  expect((title as HTMLInputElement).value).toBe("Retry the flaky upload");
});

// A REFUSED EDIT KEEPS THE READER'S WORDS and says why. Mutation: close the
// form on any answer, and the words are lost.
test("a refused edit keeps the reader's words and says the engine's reason", async () => {
  const acted = stubEngine(described(), () =>
    json({ error: "stale_version", detail: "It changed since you loaded it." }, 409),
  );
  mount();
  const form = await edit();
  fireEvent.change(within(form).getByRole("textbox", { name: "Title" }), {
    target: { value: "Mine" },
  });
  fireEvent.click(within(form).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(await within(form).findByText(/changed since you loaded it/i)).toBeTruthy();
  expect(screen.getByRole("form", { name: "Edit web!42" })).toBeTruthy();
  expect((within(form).getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe(
    "Mine",
  );
});

// A FINISHED CHANGE OFFERS NO EDIT, and its link and Quote stay; Quote puts the
// description in the comment box as `> ` lines and focuses it.
// Mutation: offer Edit on every state, and the merged change offers it.
test("a merged or closed change offers no Edit, and Copy link and Quote stay", async () => {
  for (const state of ["merged", "closed"]) {
    stubEngine(described({ state, readiness: undefined }), () => json({}));
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "More actions on the description" }));
    const items = (await screen.findAllByRole("menuitem")).map((i) => i.textContent);
    expect(items, state).toEqual(["Copy link", "Quote"]);
    cleanup();
  }
  stubEngine(described(), () => json({}));
  mount();
  const box = (await screen.findByRole("textbox", { name: "Comment" })) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "Agreed." } });
  fireEvent.click(await screen.findByRole("button", { name: "More actions on the description" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Quote" }));
  await waitFor(() =>
    expect(box.value).toBe("Agreed.\n\n> Retry with a backoff.\n> Up to three times.\n\n"),
  );
  expect(document.activeElement).toBe(box);
});

// ---- The timeline ---------------------------------------------------------- //

const at = (seq: number) => ({ ...POSITION, seq });
const reviewRow = (id: string, seq: number, verdict: string, revision = 1, extra = {}) => ({
  id,
  revision,
  reviewer: "cto",
  reviewer_kind: "agent",
  verdict,
  summary: `Summary of ${id}`,
  findings: 0,
  position: at(seq),
  at: "2026-10-27T11:00:00Z",
  ...extra,
});
const askRow = (reviewer: string, seq: number, extra: Record<string, unknown> = {}) => ({
  reviewer,
  kind: "agent",
  reason: "requested",
  state: "requested",
  requested_by: "dev",
  requested_by_kind: "agent",
  position: at(seq),
  at: "2026-10-27T11:36:00Z",
  ...extra,
});
const revisionRow = (n: number, seq: number) => ({
  n,
  head: HEAD,
  pusher: "dev",
  pusher_kind: "agent",
  position: at(seq),
  at: "2026-10-27T10:00:00Z",
});
const page = (rows: Record<string, unknown>) => () =>
  json({ position: POSITION, level: "session", ...rows });

/** An element's words, each text node apart, as a reader hears them. */
function spoken(el: Element): string {
  const words: string[] = [];
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) words.push(n.textContent ?? "");
  return words
    .join(" ")
    .replace(/\s+/g, " ")
    .replace(/ ([,.])/g, "$1")
    .trim();
}

/** The timeline's entries, in order, by a word each says. */
function timelineWords(): string[] {
  const list = screen.getByRole("list", { name: "What was said and done" });
  return [...list.children].map(spoken);
}

// THE TIMELINE IS THE LOG'S ORDER across every part, the description first.
// Mutation: sort by the records' clock times, and the old push lands last.
test("the timeline starts with the description and draws every record in the order it landed", async () => {
  stubEngine(described(), () => json({}), {
    "": page({ change: described(), revisions: [revisionRow(2, 40), revisionRow(1, 10)] }),
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes")] }),
    comments: page({
      comments: [
        {
          id: "c1",
          thread_id: "c1",
          author: "cto",
          author_kind: "agent",
          body: "Why three?",
          position: at(30),
          at: "2026-10-27T09:00:00Z",
        },
      ],
    }),
    reviewers: page({ reviewers: [askRow("sre", 50)] }),
  });
  mount();
  await screen.findByText("Why three?");
  await waitFor(() => expect(timelineWords()).toHaveLength(6));
  const words = timelineWords();
  expect(words[0]).toMatch(/^Engineer opened this change/);
  expect(words[1]).toMatch(/pushed revision 1/);
  expect(words[2]).toMatch(/requested changes on revision 1/);
  expect(words[3]).toMatch(/commented/);
  expect(words[4]).toMatch(/pushed revision 2/);
  expect(words[5]).toMatch(/asked .*sre for a review/);
});

// A REVIEW'S DISC WEARS ITS VERDICT'S TONE, the verdict said in words beside
// it (rule C7). Mutation: drop the Comment tag, and a comment reads as no verdict.
test("a review's disc wears its verdict's tone, and the verdict is said in words", async () => {
  stubEngine(detail(), () => json({}), {
    reviews: page({
      reviews: [
        reviewRow("r1", 20, "request_changes", 1),
        reviewRow("r2", 30, "approve", 2),
        reviewRow("r3", 40, "comment", 2),
      ],
    }),
  });
  mount();
  await screen.findByText("Summary of r3");
  const rows = [...document.querySelectorAll<HTMLElement>(".code-tl-event")];
  const said = rows.map((li) => [
    li.querySelector(".code-disc")?.getAttribute("data-tone"),
    spoken(li.querySelector(".code-tl-event__said")!),
  ]);
  expect(said).toEqual([
    ["danger", expect.stringMatching(/^CTO requested changes on revision 1/)],
    ["success", expect.stringMatching(/^CTO approved revision 2/)],
    ["neutral", expect.stringMatching(/^CTO reviewed revision 2.*Comment$/)],
  ]);
  for (const disc of document.querySelectorAll(".code-disc")) {
    expect(disc.getAttribute("aria-hidden")).toBe("true");
  }
});

// AN ASK THAT STANDS IS DRAWN WHERE IT WAS MADE, with when; a path owner's
// says which paths. Mutation: draw a reviewed ask, and a fourth row appears.
test("an ask that stands is drawn where it was made, with when, and a path owner's says which paths", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-27T12:00:00Z"));
  stubEngine(detail(), () => json({}), {
    reviewers: page({
      reviewers: [
        askRow("cto", 20),
        askRow("dev", 21, { reason: "path_owner", owned_paths: ["internal/server"] }),
        askRow("sre", 22, {
          reason: "path_owner",
          owned_paths: ["a/**", "b/**", "c/**"],
          owned_paths_omitted: 2,
        }),
        askRow("qa", 23, { reason: "path_owner" }),
        askRow("ops", 24, { state: "reviewed" }),
        askRow("ux", 25, { state: "ended" }),
      ],
    }),
  });
  mount();
  await screen.findByText(/for a review/);
  const asks = [...document.querySelectorAll<HTMLElement>(".code-tl-event__said")].map(spoken);
  expect(asks).toEqual([
    "Engineer asked CTO for a review 24 minutes ago",
    "Engineer was asked to review, as the owner of internal/server 24 minutes ago",
    // A HANDLE THE CHART DOES NOT HOLD is drawn by its initials and its handle.
    "SR sre was asked to review, as the owner of a/**, b/**, c/** and 2 more 24 minutes ago",
    "QA qa was asked to review, as an owner of paths it touches 24 minutes ago",
  ]);
  // EACH GLOB IS CODE in the sentence; "2 more" is words.
  const rows = document.querySelectorAll<HTMLElement>(".code-tl-event__said");
  const globs = (row: HTMLElement) => [...row.querySelectorAll("code")].map((c) => c.textContent);
  expect(globs(rows[1]!)).toEqual(["internal/server"]);
  expect(globs(rows[2]!)).toEqual(["a/**", "b/**", "c/**"]);
});

// PAST THE NOTICES' HORIZON an ask's time is said as earlier, never guessed.
// Mutation: draw the time from the position, and "earlier" goes.
test("an ask past the notices' horizon says it was asked earlier, never a time", async () => {
  stubEngine(detail(), () => json({}), {
    reviewers: page({
      reviewers: [
        askRow("cto", 20, { at: undefined }),
        askRow("dev", 21, { at: undefined, reason: "path_owner", owned_paths: ["web/**"] }),
      ],
    }),
  });
  mount();
  await screen.findByText(/for a review/);
  const asks = [...document.querySelectorAll<HTMLElement>(".code-tl-event__said")];
  expect(asks.map(spoken)).toEqual([
    "Engineer asked CTO for a review earlier",
    "Engineer was asked to review earlier, as the owner of web/**",
  ]);
  for (const a of asks) {
    expect(within(a).getByText("earlier").getAttribute("title")).toBe(
      "When this was recorded is not kept here: its notices have passed their horizon, or it told nobody.",
    );
    expect(a.querySelector("time")).toBeNull();
  }
});

// A WITHDRAWN ASK SAYS SO, and a withdrawal tells nobody, so it carries no
// time: none is drawn, not even "earlier". Mutation: drop withdrawn asks from
// the timeline, or say "earlier" of a withdrawal, and this fails.
test("a withdrawn ask says the reviewer is no longer asked", async () => {
  stubEngine(detail(), () => json({}), {
    reviewers: page({ reviewers: [askRow("cto", 20, { state: "withdrawn", at: undefined })] }),
  });
  mount();
  expect(await screen.findByText("is no longer asked to review")).toBeTruthy();
  const row = document.querySelector<HTMLElement>(".code-tl-event__said")!;
  expect(spoken(row)).toBe("CTO is no longer asked to review");
  expect(row.querySelector(".code-byline__when")).toBeNull();
});

// ASKS ONE RECORD MADE share its position, and each is its own row: one
// request naming two reviewers draws both. Mutation: key an entry by its
// position, and React's duplicate key fails the console gate.
test("two asks one record made are two rows", async () => {
  stubEngine(detail(), () => json({}), {
    reviewers: page({ reviewers: [askRow("cto", 20), askRow("sre", 20)] }),
  });
  mount();
  await waitFor(() =>
    expect([...document.querySelectorAll<HTMLElement>(".code-tl-event__said")].map(spoken)).toEqual(
      [
        expect.stringMatching(/^Engineer asked CTO for a review/),
        expect.stringMatching(/^Engineer asked SR sre for a review/),
      ],
    ),
  );
});

// AN ASK AFTER THE REVIEWER'S OWN REVIEW is asked again, and names no asker:
// the engine keeps the first ask's asker and note when it asks again.
// Mutation: say the first asker, and the row reads as theirs at a later time.
test("an ask after the reviewer's review says it was asked again, without the first asker", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-27T12:00:00Z"));
  stubEngine(detail(), () => json({}), {
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1)] }),
    reviewers: page({ reviewers: [askRow("cto", 30, { note: "Look at the retry" })] }),
  });
  mount();
  await screen.findByText("Summary of r1");
  const said = await waitFor(() => {
    const rows = [...document.querySelectorAll<HTMLElement>(".code-tl-event__said")];
    expect(rows).toHaveLength(2);
    return rows.map(spoken);
  });
  expect(said[1]).toBe("CTO was asked to review again 24 minutes ago");
  expect(screen.queryByText("Look at the retry")).toBeNull();
});

// WHO WAS ASKED, REFUSED, is said in the engine's words, never by its code.
// Mutation: print the refusal's code, and "query_failed" is on the page.
test("a refused reviewers read is said without its code", async () => {
  stubEngine(detail(), () => json({}), {
    reviewers: () => json({ error: "query_failed" }, 500),
  });
  mount();
  expect(await screen.findByText("Who was asked to review is unavailable.")).toBeTruthy();
  const timeline = screen.getByRole("region", { name: "Timeline" });
  expect(timeline.textContent).not.toContain("query_failed");
});

// LOAD MORE ASKS EVERY PART'S NEXT PAGE at one press: reviews, comments and
// whom it asked. Mutation: page the reviewers on the rail's press alone.
test("Load more asks the next page of every part that has one", async () => {
  stubEngine(detail(), () => json({}), {
    reviews: (url) =>
      url.searchParams.get("cursor")
        ? page({ reviews: [reviewRow("r2", 60, "comment")] })()
        : page({ reviews: [reviewRow("r1", 20, "comment")], next: "r1" })(),
    reviewers: (url) =>
      url.searchParams.get("cursor")
        ? page({ reviewers: [askRow("dev", 70)] })()
        : page({ reviewers: [askRow("cto", 30)], next: "a1" })(),
  });
  mount();
  await screen.findByText("Summary of r1");
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("Summary of r2")).toBeTruthy();
  await waitFor(() =>
    expect(
      changeAsked
        .filter((u) => u.searchParams.get("cursor"))
        .map((u) => [u.searchParams.get("part"), u.searchParams.get("cursor")])
        .sort(),
    ).toEqual([
      ["reviewers", "a1"],
      ["reviews", "r1"],
    ]),
  );
});

// ---- Findings under their review ------------------------------------------- //

/** Revision 1's two findings, raised by review r1: f-1 with a suggestion, f-2 without. */
const F1 = {
  ...MINE,
  id: "f-1",
  review_id: "r1",
  raiser: "cto",
  raiser_kind: "agent",
  path: "internal/server/routes.go",
  line_start: 19,
  line_end: 19,
  severity: "major",
  category: "security",
  title: "The shell flushes before the CSP nonce is set",
  body: "Set the nonce first.",
  suggestion: '\tmux.Handle("GET /p/{slug}", withCSPNonce(r.Stream("product")))\n',
  raised_at: "2026-10-27T10:00:00Z",
};
const F2 = {
  ...F1,
  id: "f-2",
  path: "internal/render/stream.go",
  line_start: 42,
  line_end: 42,
  severity: "minor",
  category: "correctness",
  title: "The shell is never flushed after the preload",
  suggestion: undefined,
};
const EXCERPTS = {
  "f-1": {
    id: "f-1",
    status: "M",
    lines: [
      { mark: "+", new: 18, text: "\t// reads land" },
      { mark: "+", new: 19, text: '\tmux.Handle("GET /p/{slug}", r.Stream("product"))' },
      { mark: "+", new: 20, text: '\tmux.Handle("GET /c/{slug}", r.Page("category"))' },
    ],
  },
  "f-2": {
    id: "f-2",
    status: "M",
    lines: [
      { mark: " ", old: 41, new: 41, text: "\tw.Write(shell.Head)" },
      { mark: "+", new: 42, text: "\tw.Write(shell.Preload(hero))" },
      { mark: " ", old: 42, new: 43, text: "\treturn s.islands(ctx, w, page)" },
    ],
  },
};

/** The engine with review r1 of revision 1 raising `findings`, and their excerpts as `excerpts` answers. */
function stubFindings(
  findings: readonly Record<string, unknown>[],
  excerpts: (url: URL) => Response,
  extra: Record<string, unknown> = {},
) {
  return stubEngine(detail(extra), () => json({ outcome: "applied" }), {
    reviews: page({
      reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: findings.length })],
    }),
    findings: page({ revision: 2, findings }),
    excerpts,
  });
}

// A REVIEW'S FINDINGS ARE CARDS UNDER IT, each with its excerpt, its own lines
// marked. Mutation: mark every line of the excerpt, and three rows are marked.
test("a review's findings are cards under it, each with its excerpt marked at its lines", async () => {
  stubFindings([F1, F2], page({ excerpts: EXCERPTS, context: 3 }));
  mount();
  const card = await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  const box = screen.getByRole("region", { name: "Findings of the review by cto" });
  expect(spoken(box.firstElementChild!)).toBe("2 findings on revision 1 2 open");
  const excerpt = await within(card).findByRole("group", {
    name: "Lines 18 to 20 of internal/server/routes.go at revision 1, line 19 marked",
  });
  const marked = [...excerpt.querySelectorAll(".crewlet-diff-view__line--picked")];
  expect(marked).toHaveLength(1);
  expect(marked[0]!.textContent).toContain('r.Stream("product")');
  expect(
    within(card)
      .getByRole("link", { name: /routes\.go:19/ })
      .getAttribute("href"),
  ).toBe("#/gitlet/repos/web/changes/42?tab=files&rev=1&file=internal%2Fserver%2Froutes.go");
  expect(within(card).getByText("Major")).toBeTruthy();
  expect(within(card).getByText("Security")).toBeTruthy();
});

// A SUGGESTION IS A SMALL DIFF of the finding's lines and the lines suggested;
// Apply suggestion only on the head's open finding. Mutation: offer it on
// revision 1's finding when the head is 2.
test("a suggestion is a small diff of the finding's lines and the lines suggested", async () => {
  stubFindings([F1], page({ excerpts: EXCERPTS }));
  mount();
  const card = await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  const diff = await within(card).findByRole("group", {
    name: "The lines suggested in place of line 19 of internal/server/routes.go",
  });
  const rows = [...diff.querySelectorAll("[data-kind]")].map((r) => r.getAttribute("data-kind"));
  expect(rows).toEqual(["remove", "add"]);
  expect(diff.textContent).toContain("withCSPNonce");
  expect(within(card).queryByRole("button", { name: "Apply suggestion" })).toBeNull();
  cleanup();
  stubFindings([F1], page({ excerpts: EXCERPTS }), { revision: 1 });
  mount();
  const head = await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  expect(await within(head).findByRole("button", { name: "Apply suggestion" })).toBeTruthy();
});

// A FINDING SAYS WHERE ITS LINES CANNOT BE SHOWN, and why, the rest of it drawn.
// Mutation: draw nothing for a refused excerpts read, and the sentence goes.
test("a finding says where its lines cannot be shown and why", async () => {
  stubFindings([F1, F2], () => json({ error: "bad_params", detail: "unknown part" }, 400));
  mount();
  await screen.findByRole("article", { name: `Finding: ${F2.title}` });
  expect(
    await screen.findAllByText("The lines around it are unavailable on this fleet yet."),
  ).toHaveLength(2);
  expect(screen.getAllByText("Set the nonce first.")).toHaveLength(2);
  cleanup();
  // A READ THAT FAILED is no older fleet: it says so in the engine's words.
  stubFindings([F1], () => json({ error: "query_failed" }, 500));
  mount();
  expect(await screen.findByText("The lines around it are unavailable.")).toBeTruthy();
  expect(screen.queryByText(/on this fleet yet/)).toBeNull();
  cleanup();
  stubFindings(
    [F1, F2],
    page({
      excerpts: {
        "f-1": { id: "f-1", binary: true, lines: [] },
        "f-2": {
          ...EXCERPTS["f-2"],
          more: true,
          lines: [{ mark: "+", new: 42, text_bytes: 12_600 }],
        },
      },
    }),
  );
  mount();
  const f2 = await screen.findByRole("article", { name: `Finding: ${F2.title}` });
  expect(await screen.findByText("A binary file: there are no lines to show.")).toBeTruthy();
  expect(within(f2).getByRole("group").textContent).toContain(
    "A line of 12 KiB, too long to show here",
  );
  // ITS SENTENCE IS WORDS, never painted as code.
  const elided = await waitFor(() => {
    const span = within(f2).getByRole("group").querySelector(".code-excerpt__elided");
    expect(span).not.toBeNull();
    return span!;
  });
  expect(elided.textContent).toBe("A line of 12 KiB, too long to show here");
  expect(within(f2).getByRole("group").querySelector('[class^="code-syn-"]')).toBeNull();
  expect(
    within(f2)
      .getByRole("link", { name: /read them on Files changed/ })
      .getAttribute("href"),
  ).toContain("file=internal%2Frender%2Fstream.go");
  cleanup();
  stubFindings([F1], page({ excerpts: {} }));
  mount();
  expect(
    await screen.findByText("Its lines are shown once its revision's files are recorded."),
  ).toBeTruthy();
});

// THE FINDINGS AND THEIR EXCERPTS ARE READ AGAIN ONLY WHEN SOMEBODY REVIEWS:
// a push and a conversation version moved ask neither. Mutation: key either
// on the change's or the conversation's version, and every push asks it again.
test("the findings and their excerpts are asked again only when somebody reviews", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let version = 5;
  let conversation = { reviews: 1, spoken_at: "2026-10-27T11:00:00Z", version: 1 };
  const facts = () => ({
    comments: 0,
    open_findings: 1,
    open_blockers: 0,
    changes_requested: 1,
    position: POSITION,
    ...conversation,
  });
  stubEngine(
    () => detail({ version, conversation: facts() }),
    () => json({}),
    {
      reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: 1 })] }),
      findings: page({ revision: 2, findings: [F1] }),
      excerpts: page({ excerpts: EXCERPTS }),
    },
  );
  mount();
  await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  const asked = () => changeAsked.filter((u) => u.searchParams.get("part") === "excerpts").length;
  const findings = () =>
    changeAsked.filter((u) => u.searchParams.get("part") === "findings").length;
  const details = () => changeAsked.filter((u) => !u.searchParams.get("part")).length;
  expect([asked(), findings()]).toEqual([1, 1]);
  version = 6;
  conversation = { ...conversation, version: 2 };
  const before = details();
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(details()).toBeGreaterThan(before));
  await settled(() => changeAsked.length);
  expect([asked(), findings()]).toEqual([1, 1]);
  conversation = { reviews: 2, spoken_at: "2026-10-27T12:00:00Z", version: 3 };
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(asked()).toBe(2));
  await settled(() => changeAsked.length);
  expect(findings()).toBe(2);
});

// LOAD MORE PAGES THE FINDINGS AND THEIR EXCERPTS together, with the same cursor.
// Mutation: page the findings alone, and the second card has no lines.
test("Load more asks the findings' and the excerpts' next pages with the same cursor", async () => {
  stubEngine(detail(), () => json({}), {
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: 2 })] }),
    findings: (url) =>
      url.searchParams.get("cursor")
        ? page({ revision: 2, findings: [F2] })()
        : page({ revision: 2, findings: [F1], next: "f-1" })(),
    excerpts: (url) =>
      url.searchParams.get("cursor")
        ? page({ excerpts: { "f-2": EXCERPTS["f-2"] } })()
        : page({ excerpts: { "f-1": EXCERPTS["f-1"] }, next: "f-1" })(),
  });
  mount();
  await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  expect(screen.getByText("1 more on a later page.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  const f2 = await screen.findByRole("article", { name: `Finding: ${F2.title}` });
  expect(await within(f2).findByRole("group", { name: /^Lines 41 to 43/ })).toBeTruthy();
  const later = changeAsked.filter((u) => u.searchParams.get("cursor") === "f-1");
  expect(later.map((u) => u.searchParams.get("part")).sort()).toEqual(["excerpts", "findings"]);
});

// A REFUSED FINDINGS READ IS SAID, never counted as findings on a later page.
// Mutation: draw the review's findings box from the counts alone, and it
// says "2 more on a later page" with no Load more to reach them.
test("a refused findings read says its findings are unavailable", async () => {
  stubEngine(detail(), () => json({}), {
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: 2 })] }),
    findings: () => json({ error: "query_failed" }, 500),
  });
  mount();
  const box = await screen.findByRole("region", { name: "Findings of the review by cto" });
  expect(await within(box).findByText("Its findings are unavailable.")).toBeTruthy();
  expect(spoken(box.firstElementChild!)).toBe("2 findings on revision 1");
  expect(box.textContent).not.toMatch(/later page|loaded/);
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});

// RESOLVE ANSWERS A STANDING FINDING IN ONE PRESS, an author's; Reply opens its box.
// Mutation: send Resolve without its resolution, and this fails.
test("Resolve answers a standing finding in one press, and Reply opens its box", async () => {
  const authors = [
    { author: "dev", kind: "agent", first_revision: 1, position: POSITION },
    { author: "jane", kind: "person", first_revision: 2, position: POSITION },
  ];
  const acted = stubEngine(detail({ authors }), () => json({ outcome: "applied" }), {
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: 1 })] }),
    findings: page({ revision: 2, findings: [F1] }),
  });
  mount("#/gitlet/repos/web/changes/42", STEWARD);
  const card = await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  expect(within(card).queryByRole("textbox", { name: "Reply" })).toBeNull();
  fireEvent.click(within(card).getByRole("button", { name: "Reply" }));
  expect(document.activeElement).toBe(within(card).getByRole("textbox", { name: "Reply" }));
  const resolve = within(card).getByRole("button", { name: "Resolve" });
  await waitFor(() => expect(resolve.getAttribute("title")).toBeNull());
  fireEvent.click(resolve);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_resolve_finding",
    args: { change: "web!42", findings: ["f-1"], resolution: "resolved" },
  });
});

// RESOLVED IS AN AUTHOR'S ANSWER, which the engine refuses anybody else: a
// reader who did not write the change finds Resolve held with why.
// Mutation: hold Resolve only on a resolved finding, and it is live here.
test("Resolve is held for a reader who did not write the change", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }), {
    reviews: page({ reviews: [reviewRow("r1", 20, "request_changes", 1, { findings: 1 })] }),
    findings: page({ revision: 2, findings: [F1] }),
  });
  mount("#/gitlet/repos/web/changes/42", STEWARD);
  const card = await screen.findByRole("article", { name: `Finding: ${F1.title}` });
  const resolve = within(card).getByRole("button", { name: "Resolve" });
  await waitFor(() => expect(resolve.getAttribute("title")).toBe(NOT_AUTHOR_RESOLVE));
  expect(resolve.getAttribute("aria-disabled")).toBe("true");
});

// FILES CHANGED DRAWS THE SAME CARD, with when the finding was raised.
// Mutation: draw the card without its byline on Files changed.
test("Files changed draws the same finding card, with when it was raised", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-27T12:00:00Z"));
  stubEngine(detail(), () => json({}), {
    findings: page({
      revision: 2,
      findings: [{ ...MINE, raised_at: "2026-10-27T10:00:00Z" }],
      placed: { "f-mine": { start: 9, end: 9, outdated: true } },
    }),
  });
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");
  fireEvent.click(await screen.findByText("1 outdated on this file"));
  const card = await screen.findByRole("article", { name: `Finding: ${MINE.title}` });
  expect(spoken(card.querySelector(".code-finding__by")!)).toMatch(
    /raised it on revision 1 2 hours ago$/,
  );
});

// A FINISHED CHANGE OFFERS ONLY WHAT STILL APPLIES: a merged change takes no
// review, no ask and no close from anybody, so its bar draws none of them and
// its Revert stands where the merge controls were; a closed one offers its
// Reopen alone. Mutation: draw the live bar for every state, and the verdicts
// stand beside the Merged badge again.
test("a merged change offers no review, no ask and no close, only its revert", async () => {
  stubEngine(detail({ state: "merged", readiness: undefined }), () => json({}));
  mount();
  await screen.findByRole("button", { name: "Revert" });
  for (const name of ["Review web!42", "Close change", "Reopen"]) {
    expect(screen.queryByRole("button", { name }), name).toBeNull();
  }
});

test("a closed change offers its reopen alone", async () => {
  const acted = stubEngine(detail({ state: "closed", readiness: undefined }), () =>
    json({ outcome: "applied" }),
  );
  mount();
  const reopen = await screen.findByRole("button", { name: "Reopen" });
  for (const name of ["Review web!42", "Close change"]) {
    expect(screen.queryByRole("button", { name }), name).toBeNull();
  }
  await waitFor(() => expect(reopen.getAttribute("title")).toBeNull());
  fireEvent.click(reopen);
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Reopen" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_close_change",
    args: { change: "web!42", expected_version: 5, reopen: true },
  });
});

// AN AUTHOR'S REVIEW COUNTS FOR NOTHING, so the engine takes none: the Review
// menu holds the author's verdicts with the reason, and Request review stays open.
// Mutation: draw the verdicts for every reader, and the author is offered Approve.
test("the change's author finds the verdicts held in the Review menu, with why", async () => {
  stubEngine(
    detail({
      authors: [
        { author: "dev", kind: "agent", first_revision: 1, position: POSITION },
        { author: "jane", kind: "person", first_revision: 2, position: POSITION },
      ],
    }),
    () => json({}),
  );
  mount();
  await screen.findByRole("tabpanel");
  fireEvent.click(await screen.findByRole("button", { name: "Review web!42" }));
  for (const name of ["Approve", "Request changes", "Comment"]) {
    const verdict = await screen.findByRole("menuitem", { name: new RegExp(`^${name}`) });
    await waitFor(() => expect(verdict.getAttribute("aria-disabled"), name).toBe("true"));
    expect(verdict.textContent).toContain(
      "You wrote this change, so your review would not count. Ask somebody else to review it.",
    );
  }
  const ask = screen.getByRole("menuitem", { name: /^Request review/ });
  expect(ask.getAttribute("aria-disabled")).not.toBe("true");
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

  const list = await screen.findByRole("tree", { name: "Files changed" });
  expect(await within(list).findByRole("treeitem", { name: /app\.go/ })).toBeTruthy();
  expect(within(list).queryByRole("treeitem", { name: /z\.go/ })).toBeNull();
  // ONE PRESS AT THE TREE'S FOOT AND ONE AT THE STACK'S.
  expect(screen.getAllByRole("button", { name: "Load more files" })).toHaveLength(2);
  fireEvent.click(screen.getAllByRole("button", { name: "Load more files" })[0]!);

  expect(await within(list).findByRole("treeitem", { name: /z\.go/ })).toBeTruthy();
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
test("More names each recipient with why they heard, in the third person", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=more");
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
  const close = await screen.findByRole("button", { name: "Close change" });
  await waitFor(() => expect(close.getAttribute("title")).toBe(reason));
  fireEvent.click(screen.getByRole("button", { name: "Review web!42" }));
  for (const name of ["Approve", "Request changes", "Comment", "Request review"]) {
    const item = await screen.findByRole("menuitem", { name: new RegExp(`^${name}`) });
    expect(item.getAttribute("aria-disabled"), name).toBe("true");
    expect(item.textContent, name).toContain(reason);
  }
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  expect(screen.getByRole("button", { name: "Comment" }).getAttribute("title")).toBe(reason);
  fireEvent.click(screen.getByRole("button", { name: "More actions on the description" }));
  const editItem = await screen.findByRole("menuitem", { name: /^Edit/ });
  expect(editItem.getAttribute("aria-disabled")).toBe("true");
  expect(editItem.textContent).toContain(reason);
  cleanup();

  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go", viewer);
  // THE OUTDATED FINDING, under its Disclosure: opened, its reply is drawn.
  fireEvent.click(await screen.findByText("1 outdated on this file"));
  fireEvent.click(await screen.findByRole("button", { name: "Reply", expanded: false }));
  const reply = screen
    .getAllByRole("button", { name: "Reply" })
    .find((b) => b.getAttribute("aria-expanded") === null)!;
  await waitFor(() => expect(reply.getAttribute("title")).toBe(reason));
  for (const name of ["Resolve", /^Answer the finding/]) {
    const held = screen.getByRole("button", { name });
    expect(held.getAttribute("title"), String(name)).toBe(reason);
  }
  // FINISH REVIEW'S VERDICTS, held the same.
  fireEvent.click(screen.getByRole("button", { name: /^Finish your review of web!42/ }));
  for (const name of ["Approve", "Request changes", "Comment"]) {
    const item = await screen.findByRole("menuitem", { name: new RegExp(`^${name}`) });
    expect(item.getAttribute("aria-disabled"), name).toBe("true");
    expect(item.textContent, name).toContain(reason);
  }
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
// for review is in the bar, one code_update_change against the
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
  // THE MERGE BOX SAYS SO across its head: what a draft is, and its step.
  const box = await screen.findByRole("region", { name: "Merge" });
  expect(within(box).getByText("A draft merges nowhere")).toBeTruthy();
  expect(
    within(box).getByText("A draft merges nowhere. Mark it ready for review first."),
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

// REVIEW STAYS IN VIEW ON A PHONE: the bar hides `page-action-folds` at that
// width, and Close change is in its More menu. Mutation: fold Review, and this fails.
test("Close change folds on a phone, and Review stays", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }));
  mount();
  const trigger = await screen.findByRole("button", { name: "Review web!42" });
  expect(trigger.closest(".page-action-folds")).toBeNull();
  const close = await screen.findByRole("button", { name: "Close change" });
  expect(close.closest(".page-action-folds")).not.toBeNull();
});

// A DIFF IS READ FOR ITS CHANGES: with no file in the address the tab opens
// on the first one, as a commit and a comparison do. Mutation: wait for a
// press, and the first screen is a list of names over an empty pane.
test("a change's Files tab opens on its first file", async () => {
  stubEngine(detail(), () => json({}));
  mount("#/gitlet/repos/web/changes/42?tab=files");
  expect(await screen.findByRole("region", { name: "app.go" })).toBeTruthy();
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

/** The reader, served the change's later writes as well. */
const STEWARD = {
  ...VIEWER,
  acts: [
    ...VIEWER.acts,
    "code_rebase_change",
    "code_dequeue_change",
    "code_revert_change",
    "code_resolve_finding",
  ],
};

// A MERGED CHANGE IS REVERTED BY A CHANGE OF ITS OWN, confirmed in the page,
// merged too only where the person asks. Mutation: send on the first press, or
// send `merge` unasked, and this fails.
test("a merged change is reverted by a change of its own, merged only when asked", async () => {
  const acted = stubEngine(detail({ state: "merged", readiness: undefined }), () =>
    json({ outcome: "applied" }),
  );
  mount("#/gitlet/repos/web/changes/42", STEWARD);

  const revert = await screen.findByRole("button", { name: "Revert" });
  await waitFor(() => expect(revert.getAttribute("title")).toBeNull());
  fireEvent.click(revert);
  // NOT AN ALERT: a revert opens a change and moves nothing it does not merge.
  let dialog = await screen.findByRole("dialog", { name: "Revert web!42?" });
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Open the revert" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_revert_change", args: { change: "web!42" } });

  fireEvent.click(revert);
  dialog = await screen.findByRole("dialog", { name: "Revert web!42?" });
  fireEvent.click(within(dialog).getByRole("checkbox", { name: /Merge the revert/ }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Revert and merge" }));
  await waitFor(() => expect(acted).toHaveLength(2));
  expect(acted[1]).toEqual({
    tool: "code_revert_change",
    args: { change: "web!42", merge: true },
  });
});

// A REBASE IS ASKED FROM THE HEAD ON SCREEN, after a confirmation that says
// it moves the branch by a forced push. Mutation: drop expected_head, or send
// on the first press, and this fails.
test("a rebase is confirmed, then asked from the head on screen", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/gitlet/repos/web/changes/42", STEWARD);

  const rebase = await screen.findByRole("button", { name: "Rebase" });
  await waitFor(() => expect(rebase.getAttribute("title")).toBeNull());
  fireEvent.click(rebase);
  const dialog = await screen.findByRole("dialog", { name: "Rebase web!42 onto main?" });
  expect(dialog.textContent).toMatch(/forced push/);
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Rebase" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_rebase_change",
    args: { change: "web!42", expected_head: HEAD },
  });
});

// TAKE OUT OF THE QUEUE IS DRAWN ON EVERY LIVE CHANGE and held where the page
// read it in no queue. Mutation: hide it, or let it press, and this fails.
test("a change in no queue holds Take out of the queue with the reason", async () => {
  stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/gitlet/repos/web/changes/42", STEWARD);
  const out = await screen.findByRole("button", { name: "Take web!42 out of the queue" });
  await waitFor(() => expect(out.getAttribute("title")).toMatch(/not in main's merge queue/));
});

// A FINDING IS ANSWERED UNDER ITSELF: one code_resolve_finding naming it, the
// answer chosen and the note. Mutation: name another finding, or drop the
// note, and this fails.
test("a finding is answered under itself, with the answer chosen and a note", async () => {
  const acted = stubEngine(detail(), () => json({ outcome: "applied" }));
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go", STEWARD);

  fireEvent.click(await screen.findByText("1 outdated on this file"));
  const open = await screen.findByRole("button", { name: /Answer the finding/ });
  await waitFor(() => expect(open.getAttribute("title")).toBeNull());
  const finding = open.getAttribute("aria-label") ?? "";
  fireEvent.click(open);
  const form = await screen.findByRole("form", { name: finding });
  fireEvent.click(within(form).getByRole("combobox", { name: "Answer" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Won't fix" }));
  fireEvent.change(within(form).getByRole("textbox", { name: "Note" }), {
    target: { value: "Out of scope here" },
  });
  fireEvent.click(within(form).getByRole("button", { name: "Send answer" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.tool).toBe("code_resolve_finding");
  expect(acted[0]!.args).toMatchObject({
    change: "web!42",
    resolution: "wont_fix",
    note: "Out of scope here",
  });
  expect(acted[0]!.args.findings).toHaveLength(1);
});

// A CHANGE'S DIFF IS COLOURED FROM ITS BLOBS, each side read by the whole id
// its listing names, so a hunk opening inside a comment reads as one.
// Mutation: pass no tokens to the change's DiffView.
test("a change's diff colours each side from its blob, a hunk inside a comment as comment", async () => {
  const OLD = "1".repeat(40);
  const NEW = "2".repeat(40);
  const go = (edit: string) =>
    ["package app", "", "/*", "notes", edit, "*/", "func Later() {}"].join("\n") + "\n";
  stubEngine(
    detail(),
    () => json({ outcome: "applied" }),
    {},
    [
      [
        "--- a/app.go",
        "+++ b/app.go",
        "@@ -5,3 +5,3 @@",
        "-old note",
        "+new note",
        " */",
        " func Later() {}",
      ].join("\n"),
    ],
    [[{ path: "app.go", status: "M", added: 1, deleted: 1, old_oid: OLD, new_oid: NEW }]],
  );
  const engine = vi.mocked(fetch);
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_blob") return engine(input, init);
      asked.push(url);
      return blobAnswer(url, { [OLD]: { text: go("old note") }, [NEW]: { text: go("new note") } });
    }),
  );
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");

  await waitFor(() =>
    expect(inkedLine("add", "new note")).toEqual(["[new]", "code-syn-comment: note"]),
  );
  expect(inkedLine("remove", "old note")).toEqual(["[old]", "code-syn-comment: note"]);
  expect(inkedLine("same", "func Later() {}")).toContain("code-syn-keyword:func");
  expect(asked.map((u) => u.searchParams.get("ref")).sort()).toEqual([OLD, NEW]);
});

// A FILE PAST THE LISTING'S FIRST PAGE, named by the address, is listed by the
// pages after it and coloured from the oids its entry carries.
// Mutation: drop the arrival's paging, and app.go is never drawn.
test("a file the listing has not reached is listed for the address and coloured", async () => {
  const OLD = "1".repeat(40);
  const NEW = "2".repeat(40);
  const go = (edit: string) => ["package app", "", "func A() {", edit, "}"].join("\n") + "\n";
  const patch = [
    "--- a/app.go",
    "+++ b/app.go",
    "@@ -3,3 +3,3 @@",
    " func A() {",
    "-\treturn 1",
    "+\treturn 2",
    " }",
  ].join("\n");
  const app = { path: "app.go", status: "M", added: 1, deleted: 1, old_oid: OLD, new_oid: NEW };
  stubEngine(
    detail(),
    () => json({ outcome: "applied" }),
    {
      diff: (url) =>
        json({
          position: POSITION,
          level: "session",
          revision: 2,
          diff: {
            base: HEAD,
            head: HEAD,
            files: [app],
            total: 2,
            added: 1,
            deleted: 1,
            // OTHER.GO'S PATCH IS ITS OWN, so the line found is app.go's.
            patch: url.searchParams.get("file") === "app.go" ? patch : "",
          },
        }),
    },
    [patch],
    [[{ path: "other.go", status: "M", added: 1, deleted: 1 }], [app]],
  );
  const engine = vi.mocked(fetch);
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_blob") return engine(input, init);
      return blobAnswer(url, {
        [OLD]: { text: go("\treturn 1") },
        [NEW]: { text: go("\treturn 2") },
      });
    }),
  );
  mount("#/gitlet/repos/web/changes/42?tab=files&file=app.go");

  await waitFor(() => expect(inkedLine("same", "func A() {")).toContain("code-syn-keyword:func"));
  expect(inkedLine("add", "\treturn 2")).toContain("code-syn-keyword:return");
});

// ---- Files changed: every file on one page --------------------------------- //

/** Revision 2's five files, in the listing's (git's) order. */
const STACK_FILES = [
  {
    path: "internal/render/page.go",
    status: "M",
    added: 2,
    deleted: 1,
    old_oid: "o1",
    new_oid: "n1",
  },
  {
    path: "internal/render/stream.go",
    status: "M",
    added: 3,
    deleted: 0,
    old_oid: "o2",
    new_oid: "n2",
  },
  {
    path: "internal/server/routes.go",
    status: "M",
    added: 1,
    deleted: 1,
    old_oid: "o3",
    new_oid: "n3",
  },
  {
    path: "web/src/product/Page.tsx",
    status: "M",
    added: 1,
    deleted: 1,
    old_oid: "o4",
    new_oid: "n4",
  },
  { path: "web/src/product/Shell.tsx", status: "A", added: 2, deleted: 0, new_oid: "n5" },
];

/** Each file's whole patch, as a read of it alone answers. */
const STACK_PATCHES: Record<string, string> = {
  "internal/render/page.go": "@@ -1,2 +1,3 @@\n keep\n-old\n+new\n+more\n",
  "internal/render/stream.go": "@@ -1,1 +1,4 @@\n ctx\n+a\n+b\n+c\n",
  "internal/server/routes.go": "@@ -14,2 +14,2 @@ func New()\n keep14\n-old15\n+new15\n",
  "web/src/product/Page.tsx": "@@ -1,1 +1,1 @@\n-before\n+after\n",
  "web/src/product/Shell.tsx": "@@ -0,0 +1,2 @@\n+shell one\n+shell two\n",
};

/** The walk: page one ends inside stream.go, page two holds the rest. */
const WALK = [
  [
    {
      file: 0,
      path: STACK_FILES[0]!.path,
      status: "M",
      patch: STACK_PATCHES["internal/render/page.go"],
    },
    {
      file: 1,
      path: STACK_FILES[1]!.path,
      status: "M",
      patch: "@@ -1,1 +1,4 @@\n ctx\n+a\n+b\n",
      continues: true,
    },
  ],
  [
    { file: 1, path: STACK_FILES[1]!.path, status: "M", patch: "+c\n" },
    ...STACK_FILES.slice(2).map((f, i) => ({
      file: i + 2,
      path: f.path,
      status: f.status,
      // SHELL.TSX WALKS AS OTHER TEXT than its read alone, so a redraw shows.
      patch:
        f.path === "web/src/product/Shell.tsx"
          ? "@@ -0,0 +1,2 @@\n+walk one\n+walk two\n"
          : STACK_PATCHES[f.path],
    })),
  ],
];

/** Every `code_blob` question asked, newest last. */
const blobsAsked: URL[] = [];
/** Blobs past the syntax bound, so expanding them reads a range. */
const BIG_BLOBS = new Set<string>();
/** Blobs whose ranges the engine refuses. */
const REFUSE_RANGES = new Set<string>();
/** Walk cursors the engine refuses once, as a cursor it no longer resumes. */
const REFUSE_WALK_ONCE = new Set<string>();
/** Files whose read alone the engine refuses. */
const REFUSE_ALONE = new Set<string>();

/**
 * The engine for the stack: the listing `files`, the walk `walk` (cursor `w:<n>`)
 * or `walkRefused`, each file alone from STACK_PATCHES, and blobs by id.
 */
function stubStack({
  files = STACK_FILES,
  walk = WALK,
  walkRefused = false,
  parts = {},
  change = detail(),
}: {
  files?: readonly Record<string, unknown>[];
  walk?: readonly (readonly Record<string, unknown>[])[];
  walkRefused?: boolean;
  parts?: Readonly<Record<string, (url: URL) => Promise<Response> | Response>>;
  change?: Record<string, unknown> | (() => Record<string, unknown>);
} = {}) {
  blobsAsked.length = 0;
  const acted = stubEngine(
    change,
    () => json({ outcome: "applied" }),
    {
      findings: page({ revision: 2, findings: [] }),
      diff: (url) => {
        const at = { position: POSITION, level: "session", revision: 2 };
        const diff = { base: HEAD, head: HEAD, total: files.length, added: 9, deleted: 3 };
        if (url.searchParams.get("patches") === "true") {
          if (walkRefused) return json({ error: "bad_params", detail: "unknown key patches" }, 400);
          if (REFUSE_WALK_ONCE.delete(url.searchParams.get("cursor") ?? "")) {
            return json({ error: "bad_params", detail: "cursor refused" }, 400);
          }
          const n = Number((url.searchParams.get("cursor") ?? "w:0").slice(2));
          return json({
            ...at,
            ...(n + 1 < walk.length ? { next: `w:${n + 1}` } : {}),
            diff: { ...diff, files: [], patches: walk[n] },
          });
        }
        const path = url.searchParams.get("file") ?? "";
        if (REFUSE_ALONE.has(path))
          return json({ error: "not_found", detail: "no such file" }, 404);
        return json({
          ...at,
          diff: {
            ...diff,
            files: files.filter((f) => f.path === path),
            patch: STACK_PATCHES[path] ?? "",
          },
        });
      },
      ...parts,
    },
    [PATCH],
    [files],
  );
  const fetched = globalThis.fetch as ReturnType<typeof vi.fn>;
  const engine = fetched.getMockImplementation()! as (
    input: RequestInfo | URL,
    init?: RequestInit,
  ) => Promise<Response>;
  fetched.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    if (url.pathname !== "/query/code_blob") return engine(input, init);
    blobsAsked.push(url);
    const ref = url.searchParams.get("ref") ?? "";
    if (REFUSE_RANGES.has(ref) && url.searchParams.has("end_line")) {
      return json({ error: "not_found", detail: "no such blob" }, 404);
    }
    const from = Number(url.searchParams.get("start_line") || 1);
    const to = Number(url.searchParams.get("end_line") || 30);
    const text = Array.from({ length: to - from + 1 }, (_, i) => `line ${from + i}`).join("\n");
    return json({
      position: POSITION,
      level: "session",
      blob: {
        oid: ref,
        size: BIG_BLOBS.has(ref) ? 300_000 : 900,
        lines: 30,
        start_line: from,
        end_line: to,
        text: `${text}\n`,
      },
    });
  });
  return acted;
}

/** The `diff` questions asked: the walk's by cursor ("" first), each file alone by path. */
function diffAsked() {
  const diffs = changeAsked.filter((u) => u.searchParams.get("part") === "diff");
  return {
    walk: diffs
      .filter((u) => u.searchParams.get("patches") === "true")
      .map((u) => u.searchParams.get("cursor") ?? ""),
    alone: diffs.filter((u) => u.searchParams.has("file")).map((u) => u.searchParams.get("file")),
  };
}

/** A stand-in IntersectionObserver whose sections come near only when a case says so. */
class NearBy {
  static made: NearBy[] = [];
  targets: Element[] = [];
  constructor(private readonly said: IntersectionObserverCallback) {
    NearBy.made.push(this);
  }
  observe(el: Element) {
    this.targets.push(el);
  }
  unobserve() {}
  disconnect() {
    this.targets = [];
  }
  takeRecords() {
    return [];
  }
  /** Say the sections of `paths` came within one view. */
  static near(...paths: string[]) {
    for (const o of NearBy.made) {
      const entries = o.targets
        .filter((t) => paths.includes((t as HTMLElement).dataset.path ?? ""))
        .map(
          (target) => ({ target, isIntersecting: true }) as unknown as IntersectionObserverEntry,
        );
      if (entries.length > 0) act(() => o.said(entries, o as unknown as IntersectionObserver));
    }
  }
}

/** Sections come near only when a case says so. */
function holdSectionsFar() {
  NearBy.made = [];
  vi.stubGlobal("IntersectionObserver", NearBy);
}

/** A file's section, named by its head's path. */
const section = (path: string) => screen.findByRole("region", { name: path });

// EVERY LISTED FILE IS A SECTION, in git's order, under its head. Mutation:
// sort the sections by path, and Page.tsx comes before Shell.tsx still but
// routes.go no longer follows stream.go... the order list fails.
test("every listed file is a section of the stack, in the listing's order, each under its head", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  await section("web/src/product/Shell.tsx");
  const regions = screen
    .getAllByRole("region")
    .filter((r) => r.classList.contains("code-file"))
    .map((r) => r.getAttribute("data-path"));
  expect(regions).toEqual(STACK_FILES.map((f) => f.path));
  const routes = await section("internal/server/routes.go");
  expect(within(routes).getByRole("heading", { level: 3 }).textContent).toBe(
    "internal/server/routes.go",
  );
  expect(within(routes).getByText("Modified")).toBeTruthy();
  expect(
    within(routes).getByRole("img", {
      name: "1 line added and 1 removed in internal/server/routes.go",
    }),
  ).toBeTruthy();
});

// THE WALK SERVES THE STACK, never every file read alone. Mutation: read every
// section alone, and five file= questions are asked.
test("the walk's first page draws the files it holds and no file is read alone", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const page1 = await section("internal/render/page.go");
  await within(page1).findByText("more");
  await settled();
  expect(diffAsked()).toEqual({ walk: [""], alone: [] });
  const shell = await section("web/src/product/Shell.tsx");
  expect(shell.querySelector("[aria-busy]")).not.toBeNull();
});

// THE NEXT PAGE WHEN ITS FILE COMES NEAR, not before. Mutation: ask every page
// on mount, and the walk's cursor is asked before the section is near.
test("the walk's next page is asked when the first file it has not finished comes near", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  await within(await section("internal/render/page.go")).findByText("more");
  await settled();
  expect(diffAsked().walk).toEqual([""]);
  NearBy.near("internal/render/stream.go");
  await within(await section("internal/server/routes.go")).findByText("new15");
  expect(diffAsked()).toEqual({ walk: ["", "w:1"], alone: [] });
});

// A FILE GOING ON PAST THE PAGE SAYS HOW MANY OF ITS CHANGED LINES ARE LEFT.
// Mutation: count its context line too, and it says "Load more lines".
test("a file going on past the walk's page says how many lines are left and loads them", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const stream = await section("internal/render/stream.go");
  fireEvent.click(await within(stream).findByRole("button", { name: "Load 1 more line" }));
  await within(stream).findByText("c");
  expect(diffAsked().walk).toEqual(["", "w:1"]);
  expect(within(stream).queryByRole("button", { name: /Load .*more line/ })).toBeNull();
});

// A FAILED PAGE OF THE WALK IS ASKED AGAIN ONLY BY ITS PRESS, a refused cursor
// after the held pages are read again. Mutation: drop the pageError guard, and
// the refused page is asked again at once, without end.
test("a refused page of the walk is asked once, said, and asked again by its press", async () => {
  holdSectionsFar();
  REFUSE_WALK_ONCE.add("w:1");
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const stream = await section("internal/render/stream.go");
  await within(stream).findByText("ctx");
  NearBy.near("internal/render/stream.go");
  expect(
    await within(stream).findByText("Could not read the next page (bad_params). Try again."),
  ).toBeTruthy();
  await settled();
  await settled();
  expect(diffAsked().walk).toEqual(["", "w:1"]);
  fireEvent.click(within(stream).getByRole("button", { name: "Load 1 more line" }));
  await within(stream).findByText("c");
  expect(diffAsked().walk.filter((c) => c === "w:1")).toHaveLength(2);
});

// A JUMP PAST THE WALK READS THE FILE ALONE, and the walk never redraws it.
// Mutation: let the walk's text replace the read alone, and its rows are drawn twice.
test("a jump past the walk reads that file alone and the walk's later page does not redraw it", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const tree = await screen.findByRole("tree", { name: "Files changed" });
  fireEvent.click(await within(tree).findByRole("treeitem", { name: /Shell\.tsx/ }));
  const shell = await section("web/src/product/Shell.tsx");
  await within(shell).findByText("shell one");
  expect(diffAsked().alone).toEqual(["web/src/product/Shell.tsx"]);
  expect(document.activeElement).toBe(within(shell).getByRole("heading", { level: 3 }));
  expect(new URLSearchParams(location.hash.split("?")[1]).get("file")).toBe(
    "web/src/product/Shell.tsx",
  );
  NearBy.near("internal/render/stream.go");
  await within(await section("internal/server/routes.go")).findByText("new15");
  expect(within(shell).getAllByText("shell one")).toHaveLength(1);
  expect(within(shell).queryByText("walk one")).toBeNull();
});

// THE ADDRESS'S FILE OPENS THE STACK THERE. Mutation: drop the arrival jump, and nothing is read alone.
test("file= on arrival opens the stack at that file, read alone", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files&file=web/src/product/Page.tsx");
  const tsx = await section("web/src/product/Page.tsx");
  await within(tsx).findByText("after");
  expect(diffAsked().alone).toEqual(["web/src/product/Page.tsx"]);
  expect(document.activeElement).toBe(within(tsx).getByRole("heading", { level: 3 }));
});

// A LARGE FILE STARTS FOLDED. Mutation: drop FOLD_FROM_LINES, and it is drawn at once.
test("a file past the fold starts folded and is read only when asked", async () => {
  const big = {
    path: "web/src/generated/schema.ts",
    status: "M",
    added: 900,
    deleted: 300,
    new_oid: "n9",
  };
  STACK_PATCHES[big.path] = "@@ -1,1 +1,1 @@\n-x\n+generated\n";
  stubStack({ files: [big], walk: [[]] });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const schema = await section(big.path);
  expect(await within(schema).findByText(/This file changes 1,200 lines\./)).toBeTruthy();
  await settled();
  expect(diffAsked().alone).toEqual([]);
  fireEvent.click(within(schema).getByRole("button", { name: "Show its changes" }));
  await within(schema).findByText("generated");
  expect(diffAsked().alone).toEqual([big.path]);
  expect(FOLD_FROM_LINES).toBe(400);
});

// EACH STATE WITHOUT LINES SAYS WHAT BECAME OF ITS FILE. Mutation: draw a
// deleted file's View the file, and the deleted section has a link.
test("a file with no lines to draw says what became of it", async () => {
  const files = [
    { path: "web/public/hero.png", status: "M", added: 0, deleted: 0, binary: true },
    {
      path: "internal/render/shell.go",
      old_path: "internal/render/old.go",
      status: "R",
      added: 0,
      deleted: 0,
    },
    {
      path: "scripts/deploy.sh",
      status: "M",
      added: 0,
      deleted: 0,
      old_mode: "100644",
      new_mode: "100755",
    },
    {
      path: "vendor/lib",
      status: "M",
      added: 1,
      deleted: 1,
      old_mode: "160000",
      new_mode: "160000",
      old_oid: "1a2b3c4d5e",
      new_oid: "5d6e7f8a9b",
    },
    { path: "web/src/legacy.ts", status: "D", added: 0, deleted: 1, old_oid: "o7" },
  ];
  STACK_PATCHES["web/src/legacy.ts"] = "@@ -1,1 +0,0 @@\n-gone\n";
  stubStack({
    files,
    walk: [
      [{ file: 4, path: "web/src/legacy.ts", status: "D", patch: "@@ -1,1 +0,0 @@\n-gone\n" }],
    ],
  });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  expect(
    await within(await section("web/public/hero.png")).findByText(/A binary file/),
  ).toBeTruthy();
  expect(
    within(await section("internal/render/old.go → internal/render/shell.go")).getByText(
      "Renamed from internal/render/old.go, its lines unchanged.",
    ),
  ).toBeTruthy();
  expect(
    within(await section("scripts/deploy.sh")).getByText("It became executable."),
  ).toBeTruthy();
  expect(
    within(await section("vendor/lib")).getByText(/A submodule moved from 1a2b3c4 to 5d6e7f8/),
  ).toBeTruthy();
  const legacy = await section("web/src/legacy.ts");
  await within(legacy).findByText("gone");
  expect(within(legacy).queryByRole("link", { name: "View the file" })).toBeNull();
  expect(legacy.querySelector("[data-gap]")).toBeNull();
});

// A REFUSED WALK IS SAID ONCE, and no section reads itself without a press.
// Mutation: read sections alone on the refusal, and file= questions arrive unasked.
test("a refused walk says so once, and each file is read when its reader asks", async () => {
  stubStack({ walkRefused: true });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  expect(
    await screen.findByText(/Every file's patch on one page is unavailable on this fleet yet/),
  ).toBeTruthy();
  await settled();
  expect(diffAsked().alone).toEqual([]);
  const routes = await section("internal/server/routes.go");
  fireEvent.click(within(routes).getByRole("button", { name: "Show its changes" }));
  await within(routes).findByText("new15");
  expect(diffAsked().alone).toEqual(["internal/server/routes.go"]);
});

// A READ ALONE REFUSED SAYS SO WITH RETRY, and a patch the walk cut short says
// the rest is left out. Mutation: draw the refusal without Retry, and the file
// cannot be read again.
test("a file whose read alone is refused says so with Retry, and a patch cut short says so", async () => {
  REFUSE_ALONE.add("internal/server/routes.go");
  stubStack({ walkRefused: true });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const routes = await section("internal/server/routes.go");
  fireEvent.click(await within(routes).findByRole("button", { name: "Show its changes" }));
  expect(
    await within(routes).findByText(/Its changes could not be read: no such file\./),
  ).toBeTruthy();
  REFUSE_ALONE.delete("internal/server/routes.go");
  fireEvent.click(within(routes).getByRole("button", { name: "Retry" }));
  await within(routes).findByText("new15");
  cleanup();
  const cut = {
    file: 0,
    path: STACK_FILES[0]!.path,
    status: "M",
    patch: "@@ -1,2 +1,3 @@\n keep\n",
  };
  stubStack({ walk: [[{ ...cut, continues: true, rest_omitted: true }]] });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const pageGo = await section("internal/render/page.go");
  expect(
    await within(pageGo).findByText(/The rest of this file's patch holds a line too long/),
  ).toBeTruthy();
  expect(within(pageGo).queryByRole("button", { name: /Load .*more line/ })).toBeNull();
});

// A MOVED DIFF ASKS EVERY HELD PAGE OF THE WALK AGAIN and forgets the lines
// revealed on it. Mutation: keep revealed lines across the move, and line 13 stays.
test("a moved diff asks every held page of the walk again and forgets the lines revealed", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let version = 5;
  BIG_BLOBS.add("n3");
  stubStack({ change: () => detail({ version }) });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  // NO OBSERVER IN JSDOM: every section is near, so the walk reaches its last page.
  const routes = await section("internal/server/routes.go");
  await within(routes).findByText("new15");
  fireEvent.click(
    await within(routes).findByRole("button", { name: "Expand 13 hidden lines above line 14" }),
  );
  await waitFor(() => expect(routes.textContent).toContain("line 13"));
  expect(diffAsked().walk).toEqual(["", "w:1"]);
  version = 6;
  await act(async () => void vi.advanceTimersByTime(CHANGE_POLL_MS));
  await waitFor(() => expect(diffAsked().walk).toEqual(["", "w:1", "", "w:1"]));
  await waitFor(() => expect(routes.textContent).not.toContain("line 13"));
  BIG_BLOBS.delete("n3");
});

// A HUNK OPENS WITH GIT'S HEADER, and Expand reads the hidden lines of the
// newer blob by its id, one range, numbered on both sides. Mutation: number the
// revealed lines on the newer side only, and the older number is missing.
test("a hunk opens with git's header and Expand reveals hidden lines from the newer blob by id", async () => {
  BIG_BLOBS.add("n3");
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const routes = await section("internal/server/routes.go");
  await within(routes).findByText("@@ -14,2 +14,2 @@ func New()");
  fireEvent.click(
    await within(routes).findByRole("button", { name: "Expand 13 hidden lines above line 14" }),
  );
  await waitFor(() => expect(routes.textContent).toContain("line 13"));
  const ranged = blobsAsked.filter((u) => u.searchParams.has("end_line"));
  expect(ranged).toHaveLength(1);
  expect(Object.fromEntries(ranged[0]!.searchParams)).toMatchObject({
    ref: "n3",
    start_line: "1",
    end_line: "13",
  });
  expect(ranged[0]!.searchParams.has("path")).toBe(false);
  const first = [...routes.querySelectorAll(".crewlet-diff-view__line")][0]!;
  expect(
    [...first.querySelectorAll(".crewlet-diff-view__number")].map((n) => n.textContent),
  ).toEqual(["1", "1"]);
  BIG_BLOBS.delete("n3");
});

// A COLOURED FILE'S BLOB IS HELD ALREADY, so expanding it asks nothing more,
// and the lines after its last hunk expand once its length is known.
// Mutation: always read a range, and a ranged blob question is asked.
test("a coloured file expands from the blob its colouring holds, the lines after its last hunk too", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const pageGo = await section("internal/render/page.go");
  fireEvent.click(
    await within(pageGo).findByRole("button", { name: "Expand all 27 hidden lines below line 3" }),
  );
  await waitFor(() => expect(pageGo.textContent).toContain("line 30"));
  expect(blobsAsked.filter((u) => u.searchParams.has("end_line"))).toEqual([]);
});

// A GAP'S LINES REFUSED SAY SO IN ITS PLACE, and nothing else changes.
test("a gap whose lines are refused says so in its place", async () => {
  BIG_BLOBS.add("n3");
  REFUSE_RANGES.add("n3");
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const routes = await section("internal/server/routes.go");
  fireEvent.click(
    await within(routes).findByRole("button", { name: "Expand 13 hidden lines above line 14" }),
  );
  expect(
    await within(routes).findByText(/These lines are unavailable: no such blob\./),
  ).toBeTruthy();
  BIG_BLOBS.delete("n3");
  REFUSE_RANGES.delete("n3");
});

// THE LINES AFTER THE LAST HUNK OF A BLOB OF UNKNOWN LENGTH are read from the
// last line shown, so the answer says the length: no line past the end, and no
// refusal. Mutation: ask from the first hidden line, and the range starts at 29.
test("the lines after the last hunk of a file without colouring stop at its blob's end", async () => {
  const java = {
    path: "src/Main.java",
    status: "M",
    added: 1,
    deleted: 1,
    old_oid: "oj",
    new_oid: "nj",
  };
  const walkOf = (patch: string) => [[{ file: 0, path: java.path, status: "M", patch }]];
  stubStack({ files: [java], walk: walkOf("@@ -28,1 +28,1 @@\n-old\n+new\n") });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const main = await section(java.path);
  fireEvent.click(
    await within(main).findByRole("button", { name: "Expand hidden lines below line 28" }),
  );
  await waitFor(() => expect(main.textContent).toContain("line 30"));
  expect(main.textContent).not.toContain("line 31");
  const ranged = blobsAsked.filter((u) => u.searchParams.has("end_line"));
  expect(ranged.map((u) => u.searchParams.get("start_line"))).toEqual(["28"]);
  expect(within(main).queryByRole("button", { name: /below line/ })).toBeNull();
  cleanup();
  stubStack({ files: [java], walk: walkOf("@@ -30,1 +30,1 @@\n-old\n+new\n") });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const ends = await section(java.path);
  fireEvent.click(
    await within(ends).findByRole("button", { name: "Expand hidden lines below line 30" }),
  );
  await waitFor(() =>
    expect(within(ends).queryByRole("button", { name: /below line/ })).toBeNull(),
  );
  expect(ends.textContent).not.toContain("unavailable");
});

// THE TOOLBAR'S SPLIT SETS EVERY FILE; A FILE'S MENU SETS IT ALONE, held for
// the visit. Mutation: write the file's choice to the address, and layout= moves.
test("a file's menu draws that file alone split, and its head says so", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const tsx = await section("web/src/product/Page.tsx");
  await within(tsx).findByText("after");
  fireEvent.click(
    within(tsx).getByRole("button", { name: "More actions on web/src/product/Page.tsx" }),
  );
  fireEvent.click(await screen.findByRole("menuitem", { name: "View this file split" }));
  expect(within(tsx).getByText("Split")).toBeTruthy();
  expect(new URLSearchParams(location.hash.split("?")[1]).has("layout")).toBe(false);
  const routes = await section("internal/server/routes.go");
  expect(within(routes).queryByText("Split")).toBeNull();
});

// T IS GO TO FILE, over the diff's own files. Mutation: bind a keymap row of
// its own, and the keymap's one bare t is no longer code.find alone.
test("t focuses Go to file, and choosing a file brings its head to the top", async () => {
  holdSectionsFar();
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  fireEvent.keyDown(document.body, { key: "t" });
  expect(document.activeElement).toBe(field);
  fireEvent.change(field, { target: { value: "shell" } });
  const options = await screen.findAllByRole("option");
  expect(options[0]!.textContent).toBe("web/src/product/Shell.tsx");
  fireEvent.keyDown(field, { key: "Enter" });
  const shell = await section("web/src/product/Shell.tsx");
  await waitFor(() =>
    expect(document.activeElement).toBe(within(shell).getByRole("heading", { level: 3 })),
  );
  expect(new URLSearchParams(location.hash.split("?")[1]).get("file")).toBe(
    "web/src/product/Shell.tsx",
  );
});

// A FILE'S STATUS IS ITS INK'S HOOK AND ITS WORD. Mutation: drop the word from
// the row's name, and the name check fails.
test("the tree draws each file's status in its ink's hook and in words, with its counts", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const tree = await screen.findByRole("tree", { name: "Files changed" });
  const routes = await within(tree).findByRole("treeitem", {
    name: "internal/server/routes.go, modified, 1 line added and 1 removed",
  });
  expect(routes.querySelector(".code-ftree__status")!.getAttribute("data-status")).toBe("M");
  const shell = within(tree).getByRole("treeitem", { name: /Shell\.tsx, added/ });
  expect(shell.querySelector(".code-ftree__status")!.getAttribute("data-status")).toBe("A");
  expect(shell.querySelector(".code-ftree__counts")!.textContent).toBe("+2");
});

// THE TREE MARKS THE FILE WHOSE HEAD IS STUCK as the stack scrolls. Mutation:
// mark the file the address names, and the current row never moves.
test("the tree marks the file whose head is stuck at the top as the stack scrolls", async () => {
  stubStack();
  const shell = document.body.appendChild(document.createElement("div"));
  shell.id = "screen-scroll";
  mount("#/gitlet/repos/web/changes/42?tab=files");
  await section("internal/server/routes.go");
  const tops: Record<string, number> = {
    "internal/render/page.go": -900,
    "internal/render/stream.go": -500,
    "internal/server/routes.go": -10,
    "web/src/product/Page.tsx": 300,
    "web/src/product/Shell.tsx": 700,
  };
  for (const r of document.querySelectorAll<HTMLElement>("section[data-path]")) {
    r.getBoundingClientRect = () => ({ top: tops[r.dataset.path!] ?? 0 }) as DOMRect;
  }
  vi.stubGlobal("requestAnimationFrame", (run: FrameRequestCallback) => {
    run(0);
    return 1;
  });
  fireEvent.scroll(shell);
  const tree = screen.getByRole("tree", { name: "Files changed" });
  await waitFor(() =>
    expect(
      within(tree).getByRole("treeitem", { current: true }).getAttribute("aria-label"),
    ).toMatch(/^internal\/server\/routes\.go/),
  );
  shell.remove();
});

// FINISH REVIEW IS THE TAB'S ONE PRIMARY, counting the head's drafts; its
// verdicts are held for an author. Mutation: make the page bar's Review primary too.
test("Finish review is the tab's one primary, counts the drafts, and opens the review", async () => {
  stubStack();
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
      {
        id: "d2",
        path: "app.go",
        side: "new",
        line_start: 3,
        line_end: 3,
        severity: "minor",
        category: "style",
        title: "Again",
        body: "",
      },
    ]),
  );
  mount("#/gitlet/repos/web/changes/42?tab=files");
  // THE COUNT IS IN THE BUTTON'S NAME, its figure drawn hidden. Mutation: drop it from the label.
  const finish = await screen.findByRole("button", {
    name: "Finish your review of web!42, 2 drafted findings",
  });
  expect(finish.querySelector(".code-finish__count")!.textContent).toBe("2");
  const primaries = [...document.querySelectorAll("button")].filter((b) =>
    b.className.includes("primary"),
  );
  expect(primaries).toEqual([finish]);
  fireEvent.click(finish);
  fireEvent.click(await screen.findByRole("menuitem", { name: "Approve" }));
  expect(await screen.findByRole("dialog", { name: /Approve web!42/ })).toBeTruthy();
  cleanup();
  stubStack({
    change: detail({
      owner: "jane",
      authors: [{ author: "jane", kind: "person", first_revision: 1, position: POSITION }],
    }),
  });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  fireEvent.click(await screen.findByRole("button", { name: /^Finish your review of web!42/ }));
  const approve = await screen.findByRole("menuitem", { name: /Approve/ });
  expect(approve.getAttribute("aria-disabled")).toBe("true");
});

// A FINDING ON A LINE SUGGESTS ITS CHANGE AS A SMALL DIFF of its own lines,
// and its line wears the picked mark. Mutation: hand the card no own lines,
// and the suggestion's removed row goes.
test("a finding on a line suggests its change as a diff of its own lines, and its line is marked", async () => {
  const onRoutes = { ...F1, revision: 2, line_start: 15, line_end: 15, suggestion: "fixed15\n" };
  stubStack({
    parts: {
      findings: page({
        revision: 2,
        findings: [onRoutes],
        placed: { "f-1": { start: 15, end: 15 } },
      }),
    },
  });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const routes = await section("internal/server/routes.go");
  const card = await within(routes).findByRole("article", { name: `Finding: ${F1.title}` });
  const diff = within(card).getByRole("group", {
    name: "The lines suggested in place of line 15 of internal/server/routes.go",
  });
  expect(
    [...diff.querySelectorAll("[data-kind]")].map((r) => [
      r.getAttribute("data-kind"),
      r.querySelector(".crewlet-diff-view__text")?.textContent,
    ]),
  ).toEqual([
    ["remove", "new15"],
    ["add", "fixed15"],
  ]);
  const marked = [...routes.querySelectorAll(":scope .crewlet-diff-view__line--picked")].filter(
    (l) => !card.contains(l),
  );
  expect(marked.map((l) => l.querySelector(".crewlet-diff-view__text")?.textContent)).toEqual([
    "new15",
  ]);
});

/** The findings counted by path: one open on routes.go, one on stream.go raised on revision 1, two dismissed. */
const FINDING_PATHS = page({
  finding_paths: [
    { path: "internal/server/routes.go", state: "open", revision: 2, findings: 1 },
    { path: "internal/render/stream.go", state: "open", revision: 1, findings: 1 },
    { path: "web/src/product/Page.tsx", state: "dismissed", revision: 1, findings: 2 },
  ],
});

/** The stack's sections drawn, by path. */
const sectionPaths = () =>
  [...document.querySelectorAll<HTMLElement>("section.code-file")].map((s) => s.dataset.path);

/** The tree's file rows, by the path their name starts with. */
const treePaths = () =>
  [...document.querySelectorAll<HTMLElement>('[role="treeitem"][aria-label]')].map(
    (r) => r.getAttribute("aria-label")!.split(",")[0],
  );

// FILTER FILES NARROWS THE TREE AND THE STACK, and says how many it shows.
// Mutation: filter the tree only, and the stack keeps five sections.
test("Filter files narrows the tree and the stack, and says how many it shows", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files&q=render");
  await section("internal/render/stream.go");
  expect(sectionPaths()).toEqual(["internal/render/page.go", "internal/render/stream.go"]);
  expect(treePaths()).toEqual(["internal/render/page.go", "internal/render/stream.go"]);
  expect(screen.getAllByText("Showing 2 of 5 files").length).toBeGreaterThan(0);
  fireEvent.click(screen.getAllByRole("button", { name: "Clear filters" })[0]!);
  await waitFor(() => expect(sectionPaths()).toHaveLength(5));
  expect(new URLSearchParams(location.hash.split("?")[1]).has("q")).toBe(false);
});

// THE DIFF'S OWN EXTENSIONS, with their counts. Mutation: list a fixed set of
// extensions, and .tsx's count is not the diff's.
test("Filter options offer the diff's own extensions with their counts, and hide one unchecked", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files");
  await section("web/src/product/Shell.tsx");
  fireEvent.click(screen.getByRole("button", { name: "Filter options" }));
  const options = await screen.findByRole("dialog", { name: "Filter options" });
  expect(within(options).getByRole("checkbox", { name: ".go 3" })).toBeTruthy();
  fireEvent.click(within(options).getByRole("checkbox", { name: ".tsx 2" }));
  await waitFor(() => expect(sectionPaths()).toHaveLength(3));
  expect(new URLSearchParams(location.hash.split("?")[1]).get("ext")).toBe("go");
});

// A FILE WITH NO EXTENSION is its own box, written "." in ext=, and the last box
// checked stays. Mutation: write it as "", and the Makefile is hidden by .go alone.
test("a file with no extension stays shown when another extension is unchecked, and one box stays", async () => {
  const makefile = {
    path: "Makefile",
    status: "M",
    added: 1,
    deleted: 0,
    old_oid: "o6",
    new_oid: "n6",
  };
  stubStack({ files: [...STACK_FILES, makefile] });
  mount("#/gitlet/repos/web/changes/42?tab=files");
  await section("Makefile");
  fireEvent.click(screen.getByRole("button", { name: "Filter options" }));
  const options = await screen.findByRole("dialog", { name: "Filter options" });
  fireEvent.click(within(options).getByRole("checkbox", { name: ".tsx 2" }));
  await waitFor(() => expect(sectionPaths()).toHaveLength(4));
  expect(sectionPaths()).toContain("Makefile");
  expect(new URLSearchParams(location.hash.split("?")[1]).get("ext")).toBe(".,go");
  const none = within(options).getByRole("checkbox", {
    name: "No extension 1",
  }) as HTMLInputElement;
  expect(none.checked).toBe(true);
  fireEvent.click(within(options).getByRole("checkbox", { name: ".go 3" }));
  await waitFor(() => expect(sectionPaths()).toEqual(["Makefile"]));
  expect(none.disabled).toBe(true);
});

// WITH FINDINGS KEEPS THE FILES A STANDING FINDING WAS RAISED ON, by path, and
// the tree badges them. Mutation: count resolved findings, and Page.tsx stays.
test("With findings keeps the files with a standing finding, and the tree badges them", async () => {
  stubStack({ parts: { finding_paths: FINDING_PATHS } });
  mount("#/gitlet/repos/web/changes/42?tab=files&only=findings");
  await section("internal/server/routes.go");
  await waitFor(() =>
    expect(sectionPaths()).toEqual(["internal/render/stream.go", "internal/server/routes.go"]),
  );
  const tree = screen.getByRole("tree", { name: "Files changed" });
  expect(
    within(tree).getByRole("treeitem", {
      name: "internal/server/routes.go, modified, 1 line added and 1 removed, 1 open finding",
    }),
  ).toBeTruthy();
  expect(document.querySelector(".code-files__counts")!.textContent).toBe("2 open findings");
});

// A REFUSED COUNT BY PATH draws no badge and holds the option with why.
test("With findings is held with the unavailable sentence where findings by file are refused", async () => {
  stubStack({
    parts: { finding_paths: () => json({ error: "bad_params", detail: "unknown part" }, 400) },
  });
  mount("#/gitlet/repos/web/changes/42?tab=files&only=findings");
  await section("internal/server/routes.go");
  expect(
    await screen.findByText("Findings by file are unavailable on this fleet yet."),
  ).toBeTruthy();
  // ONLY=FINDINGS IS HELD TOO: every file stays. Mutation: apply it while
  // refused, and the stack is empty.
  expect(sectionPaths()).toHaveLength(5);
  fireEvent.click(screen.getByRole("button", { name: "Filter options" }));
  const held = await screen.findByRole("checkbox", { name: /With findings/ });
  expect((held as HTMLInputElement).disabled).toBe(true);
  expect((held as HTMLInputElement).checked).toBe(false);
  expect(document.querySelector(".code-ftree__badge")).toBeNull();
});

// A JUMP TO A FILE A FILTER HIDES clears the filters first (a toast says so).
// Mutation: jump without clearing, and the file has no section to go to.
test("going to a file a filter hides clears the filters", async () => {
  stubStack();
  mount("#/gitlet/repos/web/changes/42?tab=files&q=render");
  await section("internal/render/page.go");
  const field = screen.getByRole("combobox", { name: "Go to file" });
  fireEvent.change(field, { target: { value: "shell" } });
  await screen.findAllByRole("option");
  fireEvent.keyDown(field, { key: "Enter" });
  await section("web/src/product/Shell.tsx");
  expect(new URLSearchParams(location.hash.split("?")[1]).has("q")).toBe(false);
});

// A DRAFT IS A DASHED CARD EDITED IN PLACE, keeping its id. Mutation: let Edit
// add a second draft, and two are held.
test("a draft is a dashed card with Edit and Drop, and Edit saves it in place", async () => {
  stubStack();
  sessionStorage.setItem(
    draftsKey("change-1", 2),
    JSON.stringify([
      {
        id: "d1",
        path: "internal/server/routes.go",
        side: "new",
        line_start: 15,
        line_end: 15,
        severity: "minor",
        category: "correctness",
        title: "Name the route",
        body: "",
        suggestion: "renamed15\n",
      },
      {
        id: "d2",
        path: "internal/server/routes.go",
        side: "new",
        line_start: 3,
        line_end: 3,
        severity: "nit",
        category: "style",
        title: "Off the patch",
        body: "",
      },
    ]),
  );
  mount("#/gitlet/repos/web/changes/42?tab=files");
  const routes = await section("internal/server/routes.go");
  // A DRAFT ON A LINE THE PATCH DOES NOT DRAW is drawn above it. Mutation: draw
  // drafts only under drawn lines, and this one is nowhere.
  expect(await within(routes).findByText("1 draft on lines not shown")).toBeTruthy();
  expect(
    within(routes).getByRole("article", { name: "Drafted finding: Off the patch" }),
  ).toBeTruthy();
  const card = await within(routes).findByRole("article", {
    name: "Drafted finding: Name the route",
  });
  expect(card.hasAttribute("data-draft")).toBe(true);
  expect(within(card).getByText("sent with your review")).toBeTruthy();
  const diff = within(card).getByRole("group", { name: /The lines suggested in place of line 15/ });
  expect([...diff.querySelectorAll("[data-kind]")].map((r) => r.getAttribute("data-kind"))).toEqual(
    ["remove", "add"],
  );
  // CANCEL GIVES THE FOCUS BACK TO EDIT. Mutation: drop the refocus, and the body has it.
  fireEvent.click(within(card).getByRole("button", { name: "Edit" }));
  fireEvent.click(
    within(
      await within(routes).findByRole("form", { name: "Edit the finding on line 15" }),
    ).getByRole("button", { name: "Cancel" }),
  );
  const again = await within(routes).findByRole("article", {
    name: "Drafted finding: Name the route",
  });
  await waitFor(() =>
    expect(document.activeElement).toBe(within(again).getByRole("button", { name: "Edit" })),
  );
  fireEvent.click(within(again).getByRole("button", { name: "Edit" }));
  const form = await within(routes).findByRole("form", { name: "Edit the finding on line 15" });
  const title = within(form).getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  expect(title.value).toBe("Name the route");
  fireEvent.change(title, { target: { value: "Name the route for what it serves" } });
  fireEvent.click(within(form).getByRole("button", { name: "Save" }));
  expect(
    await within(routes).findByRole("article", {
      name: "Drafted finding: Name the route for what it serves",
    }),
  ).toBeTruthy();
  const kept = JSON.parse(sessionStorage.getItem(draftsKey("change-1", 2)) ?? "[]") as {
    id: string;
    title: string;
  }[];
  expect(kept.map((d) => [d.id, d.title])).toEqual([
    ["d1", "Name the route for what it serves"],
    ["d2", "Off the patch"],
  ]);
});
