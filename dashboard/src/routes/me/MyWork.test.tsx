/**
 * Whose day this screen is showing, how it says so, and what a tab promises.
 *
 * # Whose day
 *
 * The screen is read two ways — a person reading their own day, and an
 * operator reading a report's — and one wording cannot serve both. "Nothing is
 * waiting on them" on your own screen reads as a page describing somebody
 * else, which is precisely the confusion the `viewer` question exists to end.
 *
 * It matters more here than anywhere: this screen used to fall back to the
 * ALPHABETICALLY FIRST SEAT, so a page titled "My work" showed every reader a
 * stranger's day with no indication that it had guessed.
 *
 * # What a tab promises
 *
 * The seven claims were stacked cards, each ABSENT when empty, so the page's
 * shape changed with the day and a person with two hundred assignments never
 * saw their asks. The promise that no claim can crowd out another is kept by
 * the STRIP now — every tab carries its count, always — so these cases are
 * about the counts being there, being honest about what they count, and a tab
 * with nothing in it still saying its own name.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { MyWork } from "./MyWork.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

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

/** The assignments, which are the tracker's own question rather than a block. */
const noWork = { items: [], groups: [], total_hint: 0, complete: true };

const ada = {
  operator_id: "ops-1",
  operator: true,
  handle: "ada",
  name: "Ada Okonkwo",
  kind: "human",
};

/** One task with the fields a row and a band are decided from. */
function task(over: Partial<WorkSummary> = {}): WorkSummary {
  return {
    id: over.key ?? "t1",
    key: over.key ?? "ENG-1",
    project: "ENG",
    title: "Ship the thing",
    type: "task",
    status: "todo",
    updated: "2031-04-16T09:00:00Z",
    version: 1,
    ...over,
  } as WorkSummary;
}

function mount() {
  return render(
    <Router>
      <MyWork />
    </Router>,
  );
}

/** One tab's own label, as the strip draws it. */
function tabNamed(word: string): HTMLElement | undefined {
  return screen.getAllByRole("tab").find((el) => (el.textContent ?? "").startsWith(word));
}

// ---------------------------------------------------------------------------
// Whose day
// ---------------------------------------------------------------------------

// THE VIEWER DECIDES WHOSE DAY IT IS, with no handle in the URL. Before the
// `viewer` question existed there was nothing for this to resolve from, and
// the screen picked the first seat in the roster.
test("with no handle it shows the viewer's own day, and says it is theirs", async () => {
  serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await waitFor(() => expect(screen.getByText("yours")).toBeTruthy());
  // SECOND PERSON on your own day. Awaited separately: the viewer resolves
  // first and the day is a second round trip, so the badge is on screen a
  // render before the panels are.
  await waitFor(() => expect(screen.getByText("Nothing is assigned to you")).toBeTruthy());
});

// AN OPERATOR READING SOMEBODY ELSE'S DAY is a real thing to do, and the
// screen has to say so: a page called "My work" showing a colleague's without
// naming them is how a reader acts on work that is not theirs.
test("an explicit handle names whose day it is, in the third person", async () => {
  location.hash = "#/me?handle=rui";
  serving({ viewer: ada, work_my_work: { ...emptyDay, handle: "rui" }, work_items: noWork });
  mount();
  await waitFor(() => expect(screen.getByText("Rui Santos’s day")).toBeTruthy());
  await waitFor(() => expect(screen.getByText("Nothing is assigned to them")).toBeTruthy());
  expect(screen.queryByText("yours")).toBeNull();
});

// AND THE THREE VIEWER STATES TAKE THREE SENTENCES. Only one of them is
// anybody's fault, and the other two have different remedies: a credential,
// and a line of company configuration.
test("an unbound token says what to bind, not that something is broken", async () => {
  serving({
    viewer: { operator_id: "ops-7", operator: true, handle: "", name: "", kind: "" },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/not bound to a person/)).toBeTruthy());
  // The id is NAMED, because it is the value that goes in the config.
  expect(screen.getByText(/ops-7/)).toBeTruthy();
});

test("no credential at all is a different sentence from an unbound one", async () => {
  serving({ viewer: { operator_id: "", operator: false, handle: "", name: "", kind: "" } });
  mount();
  await waitFor(() => expect(screen.getByText(/No credential is presented/)).toBeTruthy());
  expect(screen.queryByText(/not bound to a person/)).toBeNull();
});

/** The page bar's whose-day pill, as it is drawn for one reader. */
async function whoseTagClass(hash: string, day: string, label: string): Promise<string> {
  location.hash = hash;
  serving({ viewer: ada, work_my_work: { ...emptyDay, handle: day }, work_items: noWork });
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

// ---------------------------------------------------------------------------
// The strip
// ---------------------------------------------------------------------------

// EVERY CLAIM IS A TAB AND EVERY TAB CARRIES ITS COUNT. This is what replaces
// the stacking: an unanswered question is visible as a number on a tab nobody
// has opened, where a card that vanished when empty took its own name with it.
test("every claim is a tab, and its count is on the strip unopened", async () => {
  serving({
    viewer: ada,
    work_items: { ...noWork, total_hint: 7 },
    work_my_work: {
      ...emptyDay,
      asked_of_me: [
        {
          key: "ENG-9",
          title: "t",
          comment: "c1",
          asked_by: "rui",
          asked_at: "2031-04-16T09:00:00Z",
          body: "why?",
          answer_with: "answer_work_question(...)",
        },
      ],
      watching_recent: [task({ key: "ENG-3" }), task({ key: "ENG-4" })],
    },
  });
  mount();
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  const tabs = screen.getAllByRole("tab").map((el) => el.textContent);
  expect(tabs).toEqual([
    "Assigned 7",
    "Priorities 0",
    "Asks 1",
    "Unblocked 0",
    "Collaborating 0",
    "Watching 2",
    "Checklist 0",
  ]);
});

// A BOUNDED BLOCK'S COUNT IS THE CEILING, NOT THE COMPANY'S. `work_my_work`
// answers twenty rows per claim, so a bare length says `20` on a person holding
// a hundred and thirty — which is the page size drawn as a fact about their
// day. `pageCount` is this product's own idiom for that.
test("a claim at the engine's bound says so rather than reporting the bound", async () => {
  serving({
    viewer: ada,
    work_items: { ...noWork, total_hint: 0 },
    work_my_work: {
      ...emptyDay,
      collaborating: Array.from({ length: 20 }, (_, i) => task({ key: `ENG-${i}` })),
    },
  });
  mount();
  await waitFor(() => expect(tabNamed("Collaborating")).toBeTruthy());
  expect(tabNamed("Collaborating")?.textContent).toBe("Collaborating 20+");
});

// AND ASSIGNED ESCAPES IT, by asking the tracker's own question: `total_hint`
// is a count over the matching set rather than over a page, so the tab says
// what the person actually holds.
test("the assignments are the tracker's count, not a block's page", async () => {
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, assigned: [task()] },
    work_items: { ...noWork, items: [task()], total_hint: 137 },
  });
  mount();
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  expect(tabNamed("Assigned")?.textContent).toBe("Assigned 137");
});

// NOTHING IS CLAIMED WHILE THE READ IS IN FLIGHT. A zero on the tab a reader
// lands on is "your day is empty", which is a claim — and it is a false one for
// as long as the answer has not arrived.
test("the assigned tab claims no count until the tracker answers", async () => {
  const query = vi.fn(async (what: string) =>
    what === "work_items"
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
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  expect(tabNamed("Assigned")?.textContent).toBe("Assigned ");
});

// ---------------------------------------------------------------------------
// What a tab draws
// ---------------------------------------------------------------------------

// A DAY IS READ BY WHEN, not by status: every task somebody holds is in
// progress or about to be, so a status grouping answers a question nobody
// asked. The bands come from the ROW'S OWN overdue flag and its date.
test("the assignments are banded by when they are due", async () => {
  const today = new Date();
  const later = new Date(today.getFullYear() + 1, 0, 15, 9, 0, 0);
  serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_items: {
      ...noWork,
      total_hint: 3,
      items: [
        task({ key: "ENG-1", overdue: true, due: "2020-01-01T09:00:00Z" }),
        task({ key: "ENG-2", due: later.toISOString() }),
        task({ key: "ENG-3" }),
      ],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Overdue")).toBeTruthy());
  expect(screen.getByText("Later")).toBeTruthy();
  expect(screen.getByText("No date")).toBeTruthy();
  // AND A BAND NOTHING IS IN IS NOT DRAWN: five headings over a person holding
  // three tasks is the page of empty panels this screen was rebuilt to stop.
  expect(screen.queryByText("This week")).toBeNull();
});

// A TAB WITH NOTHING IN IT STILL SAYS ITS OWN NAME. A card that vanished took
// its name with it, so a reader could not tell "nothing here" from "this
// product does not have that" — which a strip cannot do, because the tab is
// still on it.
test("an empty claim says what would be in it", async () => {
  location.hash = "#/me?tab=unblocked";
  serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing here")).toBeTruthy());
  expect(screen.getByText(/has become workable for you/)).toBeTruthy();
});

// THE QUESTIONS PUT TO SOMEBODY ELSE ARE NOT WRITTEN AS THE READER'S. This is
// the one tab where somebody else is blocked on this person rather than the
// other way round, and it is read on a report's day as often as on your own.
test("questions put to somebody else are not addressed to the reader", async () => {
  location.hash = "#/me?handle=rui&tab=asks";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, handle: "rui" },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing is waiting on them")).toBeTruthy());
  expect(screen.queryByText("Nothing is waiting on you")).toBeNull();
});

// AND THE CALL THAT ANSWERS ONE IS OFFERED VERBATIM. The dashboard writes
// nothing, so what it can offer is the gesture somebody's own assistant makes
// on their behalf — and a model handed a comment id still has to compose the
// call, where every one it composes differently is a round spent being refused.
test("an ask carries the call that answers it", async () => {
  location.hash = "#/me?tab=asks";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      asked_of_me: [
        {
          key: "ENG-9",
          title: "Which reader?",
          comment: "c1",
          asked_by: "rui",
          asked_at: "2031-04-16T09:00:00Z",
          body: "parent or replies?",
          answer_with: 'answer_work_question(task: "ENG-9", comment: "c1", body: "…")',
        },
      ],
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Which reader?")).toBeTruthy());
  expect(screen.getByText(/answer_work_question\(task: "ENG-9"/)).toBeTruthy();
});

// A QUEUE SOMEBODY ELSE ORDERED IS STAMPED, and the stamp is the one thing on
// this screen that asks for an acknowledgement: a person who starts the day on
// work they did not choose can see who chose it.
test("a queue a lead ordered says who ordered it", async () => {
  location.hash = "#/me?tab=priorities";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: [task({ key: "ENG-5" })] },
    work_person: {
      handle: "ada",
      priorities_set_by: "rui",
      priorities_set_at: "2031-04-16T09:00:00Z",
      version: 1,
      held: true,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/put this order in place/)).toBeTruthy());
  expect(screen.getByText(/Rui Santos/)).toBeTruthy();
});

// THE INBOX IS NOT ONE OF THE CLAIMS. What REACHED somebody is a different
// question from what is ON them, and it is the landing screen of this product:
// the card that drew it here was the inbox in a narrower column with a smaller
// bound, so a reader met the same notices twice and neither copy was the one
// with the reason facets.
test("what reached somebody is the Inbox, and is not drawn here", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  expect(screen.queryByText("Reached you")).toBeNull();
  expect(query.mock.calls.map((c) => c[0])).not.toContain("work_inbox");
  // And the way to it is a link rather than a copy.
  expect(screen.getByText("Inbox →")).toBeTruthy();
});
