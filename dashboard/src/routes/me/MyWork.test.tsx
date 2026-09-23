/**
 * Whose day this screen is showing, and how it says so.
 *
 * The screen is read two ways — a person reading their own day, and an
 * operator reading a report's — and one wording cannot serve both. "Nothing
 * has reached them" on your own inbox reads as a screen describing somebody
 * else, which is precisely the confusion the `viewer` question exists to end.
 *
 * It matters more here than anywhere: this screen used to fall back to the
 * ALPHABETICALLY FIRST SEAT, so a page titled "My work" showed every reader a
 * stranger's day with no indication that it had guessed.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { MyWork } from "./MyWork.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkInboxNotice } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [
      { name: "Ada Okonkwo", handle: "ada", kind: "human", contact: { slack_user_id: "U0A" } },
      { name: "Rui Santos", handle: "rui", kind: "human", contact: { slack_user_id: "U0R" } },
    ],
  } as never);
  return query;
}

const emptyDay = {
  handle: "ada",
  priorities: [],
  assigned: [],
  asked_of_me: [],
  checklist_items: [],
  collaborating: [],
  watching_recent: [],
  unblocked_recent: [],
  complete: true,
};

const emptyInbox = { handle: "ada", notices: [], primary_reasons: [], unread: 0, primary: 0 };

function mount() {
  return render(
    <Router>
      <MyWork />
    </Router>,
  );
}

// THE VIEWER DECIDES WHOSE DAY IT IS, with no handle in the URL. Before the
// `viewer` question existed there was nothing for this to resolve from, and
// the screen picked the first seat in the roster.
test("with no handle it shows the viewer's own day, and says it is theirs", async () => {
  serving({
    viewer: {
      login: "ops-1",
      grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
      handle: "ada",
      name: "Ada Okonkwo",
      kind: "human",
    },
    work_my_work: emptyDay,
    work_inbox: emptyInbox,
  });
  mount();
  await waitFor(() => expect(screen.getByText("yours")).toBeTruthy());
  // SECOND PERSON on your own day. Awaited separately: the viewer resolves
  // first and the day is a second round trip, so the badge is on screen a
  // render before the panels are.
  await waitFor(() => expect(screen.getByText("Nothing has reached you")).toBeTruthy());
});

// AN OPERATOR READING SOMEBODY ELSE'S DAY is a real thing to do, and the
// screen has to say so: a page called "My work" showing a colleague's without
// naming them is how a reader acts on work that is not theirs.
test("an explicit handle names whose day it is, in the third person", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: {
      login: "ops-1",
      grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
      handle: "ada",
      name: "Ada Okonkwo",
      kind: "human",
    },
    work_my_work: { ...emptyDay, handle: "rui" },
    work_inbox: { ...emptyInbox, handle: "rui" },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Rui Santos’s day")).toBeTruthy());
  await waitFor(() => expect(screen.getByText("Nothing has reached them")).toBeTruthy());
  expect(screen.queryByText("yours")).toBeNull();
});

// AND THE THREE VIEWER STATES TAKE THREE SENTENCES. Only one of them is
// anybody's fault, and the other two have different remedies: a credential,
// and a line of company configuration.
test("an unbound token says what to bind, not that something is broken", async () => {
  serving({
    viewer: {
      login: "ops-7",
      grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
      handle: "",
      name: "",
      kind: "",
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/not bound to a seat/)).toBeTruthy());
  // The login is NAMED, because it is the value `crewlet iam bind` takes.
  expect(screen.getByText(/ops-7/)).toBeTruthy();
});

test("no credential at all is a different sentence from an unbound one", async () => {
  serving({ viewer: { login: "", grants: [], handle: "", name: "", kind: "" } });
  mount();
  await waitFor(() => expect(screen.getByText(/No credential is presented/)).toBeTruthy());
  expect(screen.queryByText(/not bound to a seat/)).toBeNull();
});

/** One notice, with everything a row draws. */
function notice(over: Partial<WorkInboxNotice> = {}): WorkInboxNotice {
  return {
    record_id: "r1",
    log_seq: 1,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at: "2031-04-16T09:00:00Z",
    reason: "assignee",
    primary: true,
    addressed: false,
    kind: "task_assigned",
    subject_id: "s1",
    subject_key: "ENG-1",
    excerpt: "Ship the thing",
    read: true,
    ...over,
  };
}

const ada = {
  login: "ops-1",
  grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
  handle: "ada",
  name: "Ada Okonkwo",
  kind: "human",
};

/** The page bar's whose-day pill, as it is drawn for one reader. */
async function whoseTagClass(hash: string, day: string, label: string): Promise<string> {
  location.hash = hash;
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: day },
    work_inbox: { ...emptyInbox, handle: day },
  });
  mount();
  const pill = (await screen.findByText(label)).closest(".crewlet-tag");
  const drawn = pill?.className ?? "";
  cleanup();
  return drawn;
}

// COLOUR CARRIES STATE, NEVER IDENTITY, and whose day this is is identity. The
// pill was drawn `success` — the positive status hue — for your own day and the
// neutral outline for anybody else's, so the one thing it separated was two
// people.
//
// ASSERTED AS "THE TWO BRANCHES ARE DRAWN IDENTICALLY" rather than "it is not
// green", because a hue is not the only way chrome can carry identity: the
// appearance tracked whose day it was too, and a fix that neutralised the
// variant alone would leave a filled pill against a hairline one and this test
// would still pass.
test("the whose-day pill is drawn the same for both, so only the words differ", async () => {
  const own = await whoseTagClass("#/me", "ada", "yours");
  const theirs = await whoseTagClass("#/me?handle=rui", "rui", "Rui Santos’s day");
  expect(own).toContain("crewlet-tag--neutral");
  expect(theirs).toBe(own);
});

/** The count pill on the card with this title, as a reader meets it. */
function pillOn(title: string): Element | null | undefined {
  return screen.getByText(title).closest(".crewlet-card")?.querySelector(".crewlet-count");
}

// THE PILL IS THE ROWS UNDER IT, AND IT SAYS WHAT IT COUNTS.
//
// The header drew the unread tally in the count pill and opened its subtitle
// with the row count: two page-scoped numbers, neither labelled, and on a quiet
// day both of them `0`. `Card.Header`'s slot passes no `label` to `Count`, so
// the number had no accessible name either.
test("the inbox pill counts the notices it drew, and names what it counts", async () => {
  serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_inbox: {
      ...emptyInbox,
      unread: 1,
      primary_reasons: ["assignee"],
      notices: [
        notice({ record_id: "r1", read: false }),
        notice({ record_id: "r2" }),
        notice({ record_id: "r3" }),
      ],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Reached you")).toBeTruthy());
  const pill = pillOn("Reached you");
  // The rows, not the one unread.
  expect(pill?.querySelector("[aria-hidden]")?.textContent).toBe("3");
  // And it is not a bare digit.
  expect(pill?.textContent).toContain("notices on this page");
  // The other number, worded.
  expect(screen.getByText("1 unread · the 20 most recent")).toBeTruthy();
});

// NOTHING IS CLAIMED ABOUT THE INBOX UNTIL THE INBOX ANSWERS. The card is gated
// on `work_my_work`, a different query, so the header rendered while
// `work_inbox` was still in flight and `?? notices.length` resolved to `0`.
test("the header claims no count while the inbox is still in flight", async () => {
  const query = vi.fn(async (what: string) =>
    what === "work_inbox"
      ? new Promise(() => {})
      : what === "viewer"
        ? ada
        : what === "work_my_work"
          ? emptyDay
          : {},
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "human" }],
  } as never);
  mount();
  await waitFor(() => expect(screen.getByText("Reached you")).toBeTruthy());
  expect(pillOn("Reached you")).toBeNull();
});

// A COLLEAGUE'S DAY IS NOT WRITTEN IN THE SECOND PERSON.
test("a report's day says the wake reasons in the third person", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_inbox: {
      ...emptyInbox,
      handle: "rui",
      notices: [notice()],
      primary_reasons: ["assignee"],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Reached them")).toBeTruthy());
  expect(screen.getByText("assignee")).toBeTruthy();
  expect(screen.queryByText("assigned to you")).toBeNull();
  expect(screen.getByText(/count as primary for them/)).toBeTruthy();
});

// AND THE OTHER DIRECTION, so the fix cannot be "third person everywhere": that
// is a screen telling you about somebody, on the page that is yours.
test("your own day keeps the second person", async () => {
  serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_inbox: { ...emptyInbox, notices: [notice()], primary_reasons: ["assignee"] },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Reached you")).toBeTruthy());
  expect(screen.getByText("assigned to you")).toBeTruthy();
  expect(screen.queryByText("assignee")).toBeNull();
  expect(screen.getByText(/count as primary for you\./)).toBeTruthy();
});

// THE BLOCK OF QUESTIONS IS NOT ADDRESSED TO WHOEVER IS LOOKING.
test("questions put to somebody else are not titled as the reader's", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: ada,
    work_inbox: { ...emptyInbox, handle: "rui" },
    work_my_work: {
      ...emptyDay,
      handle: "rui",
      asked_of_me: [
        {
          key: "ENG-9",
          title: "t",
          comment: "c1",
          asked_by: "ada",
          asked_at: "2031-04-16T09:00:00Z",
          body: "why?",
          answer_with: "x",
        },
      ],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Asked of them")).toBeTruthy());
  expect(screen.queryByText("Asked of you")).toBeNull();
});
