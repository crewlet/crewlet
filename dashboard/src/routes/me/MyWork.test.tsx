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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { MyWork } from "./MyWork.tsx";
import { Router } from "~/app/router.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

// THE FRAME'S ONE COVERAGE SLOT, stood in for so a case can say WHAT was
// published rather than only that something was drawn: the state bar renders
// nothing at all on a healthy answer, by design, so the published fact is the
// only thing a test can hold.
vi.mock("~/app/Shell.tsx", () => ({ usePageCoverage: vi.fn() }));

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.mocked(usePageCoverage).mockClear();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>>, org: unknown = flatOrg) {
  const query = vi.fn(
    async (what: string, _params?: Record<string, unknown>) => answers[what as QueryName] ?? {},
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue(org as never);
  return query;
}

/** Two seats and no derived block: every reporting line is UNKNOWN. */
const flatOrg = {
  name: "Acme",
  roles: [
    { name: "Ada Okonkwo", handle: "ada", kind: "human", contact: { slack_user_id: "U0A" } },
    { name: "Rui Santos", handle: "rui", kind: "human", contact: { slack_user_id: "U0R" } },
  ],
};

/** The same company with the ENGINE's own hierarchy: Ada leads Rui, not Bo. */
const derivedSeat = (handle: string, name: string, reports: string[] = []) => ({
  handle,
  name,
  kind: "human",
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: reports.length ? reports : null,
  auto_reports: null,
  onboarding_chain: null,
});
const ledOrg = {
  name: "Acme",
  roles: [
    { name: "Ada Okonkwo", handle: "ada", kind: "human" },
    { name: "Rui Santos", handle: "rui", kind: "human" },
    { name: "Bo Nakamura", handle: "bo", kind: "human" },
  ],
  units: [],
  derived: {
    units: [],
    seats: [
      derivedSeat("ada", "Ada Okonkwo", ["rui"]),
      derivedSeat("rui", "Rui Santos"),
      derivedSeat("bo", "Bo Nakamura"),
    ],
  },
};

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
  // NAMED IN THE BANNER, as a link to that person's own seat — the picker
  // below lists every seat by name too, so the assertion is on the one that is
  // a way somewhere.
  await waitFor(() => expect(screen.getByRole("link", { name: /Rui Santos/ })).toBeTruthy());
  expect(screen.getByText("their day")).toBeTruthy();
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

/** The banner's whose-day pill, as it is drawn for one reader. */
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
  const theirs = await whoseTagClass("#/me?handle=rui", "rui", "their day");
  expect(own).toContain("crewlet-tag--neutral");
  expect(theirs).toBe(own);
});

// AND THE BAND NAMES THE PERSON AND THE WAY TO THEIR SEAT. A page called "My
// work" on a company's first morning is seven zeros over one empty panel
// unless something on it is true before any count is: who this is, what their
// seat is, and where that seat's own page is.
test("the banner names whose day it is and links to their seat", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: noWork,
    work_person: { handle: "rui", version: 1, held: true, complete: true },
  });
  mount();
  const chip = await screen.findByRole("link", { name: /Rui Santos/ });
  expect(chip.getAttribute("href")).toBe("#/company/people/rui");
  expect(screen.getByText("rui")).toBeTruthy();
  expect(screen.getByText("their day")).toBeTruthy();
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
  // AND NO TRAILING SPACE EITHER: the label IS the accessible name, so
  // "Assigned " is a name with a word nobody wrote at the end of it.
  expect(tabNamed("Assigned")?.textContent).toBe("Assigned");
});

// ---------------------------------------------------------------------------
// What a tab draws
// ---------------------------------------------------------------------------

// THE ASSIGNED TAB IS THE WORK LIST, narrowed to one person — not a second
// renderer. Written twice it had no Filter menu, no Display menu, no scope
// switch, no chips, no count line and no way past its two hundredth row, and
// each of those is a rule the work list already keeps.
test("the assigned tab draws the work list's own toolbar", async () => {
  serving({ viewer: ada, work_my_work: emptyDay, work_items: { ...noWork, items: [task()] } });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  expect(screen.getByRole("button", { name: "Filter" })).toBeTruthy();
  expect(screen.getByText("Open")).toBeTruthy();
  expect(screen.getByText("1 item")).toBeTruthy();
});

// AND THE BANDS ARE THE ENGINE'S. `due:bucket` is cut against the COMPANY's
// day start, like the row's own overdue flag and every `due=` filter, so the
// heading a task sits under and the flag beside it cannot disagree. Computed
// here, from the browser's own midnight, they could and did.
test("the assigned tab opens on the engine's due bands, soonest first", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  const asked = await waitFor(() => {
    const call = query.mock.calls.find(
      (c) => c[0] === "work_items" && (c[1] as Record<string, unknown>)?.group_by,
    );
    if (!call) throw new Error("the list has not asked yet");
    return call[1] as Record<string, unknown>;
  });
  expect(asked.group_by).toBe("due:bucket");
  expect(asked.sort).toBe("due");
  expect(asked.status_group).toBe("not_started,active");
});

// THE LOCK REACHES THE WIRE AND NOTHING ELSE. It is what the tab IS rather
// than a narrowing somebody chose, so there is no chip to take off, no row in
// the Filter menu and no key on the address — where a key would be a second,
// silent answer to the one question the screen has already answered.
test("the assignee is locked on the wire and absent from the URL and the chips", async () => {
  location.hash = "#/me?handle=rui";
  const query = serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: { ...noWork, items: [task()] },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  const items = query.mock.calls.filter((c) => c[0] === "work_items");
  expect(items.length).toBeGreaterThan(0);
  for (const call of items) {
    expect((call[1] as Record<string, unknown>).assignee).toBe("rui");
  }
  expect(location.hash).not.toContain("assignee=");
  // No chip names it — and the Filter menu does not offer the row, which is
  // asserted on the KEY each row prints rather than on its label, because
  // "Status" is also a column head and a Display option one bar over.
  expect(screen.queryByText("Assignee")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  await waitFor(() => expect(screen.getByText("priority=")).toBeTruthy());
  expect(screen.queryByText("assignee=")).toBeNull();
});

// AND THE WORKSPACE'S SAVED VIEWS ARE NOT THIS PERSON'S CLAIMS. The strip's
// first tab reads "All work", which over one person's list is false, and the
// screen's own seven tabs already name the page inside its content column.
test("the hosted list draws no saved-view strip and asks for none", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await waitFor(() => expect(tabNamed("Assigned")).toBeTruthy());
  expect(screen.queryByText("All work")).toBeNull();
  expect(screen.queryByText("All views →")).toBeNull();
  expect(query.mock.calls.map((c) => c[0])).not.toContain("work_views");
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

/** One day whose queue somebody else put in order. */
const orderedByRui = {
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
};

// A QUEUE SOMEBODY ELSE ORDERED IS STAMPED, and the stamp is the one thing on
// this screen that asks for an acknowledgement: a person who starts the day on
// work they did not choose can see who chose it.
//
// ON THE TAB THEY LANDED ON, which is the half that was missing. The stamp
// lived inside the Priorities panel, so it reached only a reader who had
// already acknowledged it by opening that tab — while their own next change to
// the queue cleared it for good, which makes the miss unrecoverable rather
// than merely late.
test("a queue a lead ordered is announced on the tab nobody opened", async () => {
  serving(orderedByRui);
  mount();
  await waitFor(() => expect(screen.getByText(/put this order in place/)).toBeTruthy());
  expect(screen.getByText(/Rui Santos/)).toBeTruthy();
  // The default tab is Assigned, so the banner is what carried it.
  expect(tabNamed("Assigned")?.getAttribute("aria-selected")).toBe("true");
  // And the way to the tab it is about is offered from here.
  expect(screen.getByRole("button", { name: "Priorities →" })).toBeTruthy();
});

// AND THE TAB ITSELF IS MARKED, because a banner is read once and a strip is
// scanned: the mark is what a reader who scrolled past the banner still sees.
test("the priorities tab carries a mark while somebody else's order stands", async () => {
  serving(orderedByRui);
  mount();
  await waitFor(() => expect(tabNamed("Priorities")).toBeTruthy());
  expect(tabNamed("Priorities")?.querySelector("svg")).toBeTruthy();
  expect(tabNamed("Watching")?.querySelector("svg")).toBeNull();
});

// A QUEUE SOMEBODY ORDERED THEMSELVES IS NOT NEWS. The engine clears the stamp
// on the person's own write, so an absent one is a settled fact rather than a
// missing one, and neither the banner nor the mark is drawn for it.
test("a queue nobody else ordered carries neither banner nor mark", async () => {
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: [task({ key: "ENG-5" })] },
    work_person: { handle: "ada", version: 1, held: true, complete: true },
  });
  mount();
  await waitFor(() => expect(tabNamed("Priorities")).toBeTruthy());
  expect(screen.queryByText(/put this order in place/)).toBeNull();
  expect(tabNamed("Priorities")?.querySelector("svg")).toBeNull();
});

// AND A LINK TO THE PAGE YOU ARE ON IS A LIE. Opened, the Priorities tab is
// where the control would send the reader, so the banner keeps its sentence
// and drops the control.
test("the banner offers no way to the tab that is already open", async () => {
  location.hash = "#/me?tab=priorities";
  serving(orderedByRui);
  mount();
  await waitFor(() => expect(screen.getByText(/put this order in place/)).toBeTruthy());
  expect(screen.queryByRole("button", { name: "Priorities →" })).toBeNull();
});

// THE COVERAGE OF THE READ THAT IDENTIFIES THIS OBJECT GOES TO THE FRAME, and
// this screen publishes exactly one: the frame holds one slot with one setter,
// so two publishers on one screen is a last-writer-wins race. `#/me`'s object
// is a person, so the person's own record is what the state bar carries; the
// other two reads state their own beside the rows they drew.
test("the page publishes the person's coverage, once", async () => {
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: emptyDay,
    work_person: { handle: "ada", version: 1, held: true, complete: true, read_level: "stale" },
  });
  mount();
  const published = await waitFor(() => {
    const answers = vi
      .mocked(usePageCoverage)
      .mock.calls.map((c) => c[0])
      .filter(Boolean);
    if (answers.length === 0) throw new Error("nothing published yet");
    return answers;
  });
  for (const answer of published) {
    const record = answer as { handle?: string; held?: boolean };
    expect(record.handle).toBe("ada");
    // ASSERTED ON A FIELD ONLY THE PERSON'S RECORD CARRIES. Both personal
    // reads answer under the same handle, so a screen that published the
    // WRONG one would satisfy a handle check and nothing else.
    expect(record.held).toBe(true);
  }
});

// ---------------------------------------------------------------------------
// Whose day to read
// ---------------------------------------------------------------------------

/** The picker's rows, as the listbox draws them once it is open. */
async function pickerRows(): Promise<HTMLElement[]> {
  fireEvent.click(await screen.findByRole("combobox", { name: "Whose day" }));
  return await waitFor(() => {
    const rows = screen.getAllByRole("option");
    if (rows.length === 0) throw new Error("the listbox has not opened");
    return rows;
  });
}

// YOURS, THEN YOUR LINE, THEN ANYBODY. Flat and alphabetical the control
// answered "which of the company's seats" with every seat, in an order that
// has nothing to do with the question — and the two days a reader opens are
// their own and one of their reports'.
test("the picker leads with your own day, then the seats you lead", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  // THE HEADING THROUGH THE ACCESSIBLE NAME, not through the package's own
  // class: a group is labelled by an element rather than by an attribute, and
  // a test reaching for the class would be asserting the drawing.
  const groupOf = (row: HTMLElement) => {
    const id = row.closest("[role=group]")?.getAttribute("aria-labelledby");
    return (id && document.getElementById(id)?.textContent) || "";
  };
  const labelled = rows.map((r) => [groupOf(r), r.textContent ?? ""] as const);
  expect(labelled.find(([, text]) => text.includes("Ada Okonkwo"))?.[0]).toBe("Yours");
  expect(labelled.find(([, text]) => text.includes("Rui Santos"))?.[0]).toBe("Your line");
  expect(labelled.find(([, text]) => text.includes("Bo Nakamura"))?.[0]).toBe("Anybody");
});

// AND EACH ROW SAYS HOW MUCH IS ON THAT DESK, which is the one fact that makes
// the control worth opening and the one it did not carry.
test("a picker row says how much open work is on that desk", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [{ handle: "rui", open: 4 }], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  const text = (name: string) =>
    rows.find((r) => (r.textContent ?? "").includes(name))?.textContent;
  expect(text("Rui Santos")).toContain("4 open items");
  // A HANDLE THE ANSWER DID NOT NAME HOLDS NOTHING, because the read returns a
  // row only for somebody with open work — a real zero rather than a gap.
  expect(text("Bo Nakamura")).toContain("nothing open");
});

// ZERO AND UNKNOWN ARE DIFFERENT. An answer that stopped at its handle cap, or
// one that could not account for every change, has not said that a desk is
// empty — and a row claiming it had would be a lead reassigning work on a
// figure nobody measured.
test("an incomplete workload answer claims no desk is empty", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [{ handle: "rui", open: 4 }], complete: true, truncated: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  for (const row of rows) {
    expect(row.textContent).not.toContain("nothing open");
    expect(row.textContent).not.toContain("open item");
  }
});

// WITHOUT THE ENGINE'S OWN HIERARCHY THERE IS NO LINE TO DRAW. An older engine
// sends no derived block, so who reports to whom is UNKNOWN rather than empty,
// and a "Your line" heading over nothing would be a claim this client cannot
// make.
test("no derived hierarchy draws no line, rather than an empty one", async () => {
  serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_items: noWork,
    work_workload: { rows: [], complete: true },
  });
  mount();
  const rows = await pickerRows();
  const groups = rows.map((r) => {
    const id = r.closest("[role=group]")?.getAttribute("aria-labelledby");
    return (id && document.getElementById(id)?.textContent) || "";
  });
  expect(groups).not.toContain("Your line");
  expect(groups).toContain("Yours");
});

// AND THERE IS NO "PICK SOMEBODY" ROW FOR A READER WHO HAS A DAY. It wrote the
// parameter's own fallback, which the router deletes, so it resolved straight
// back to their own seat and the control re-labelled itself with their name —
// a row that silently refuses. Their way back is the "Yours" row.
test("a bound reader is offered no row that returns them where they are", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  expect(rows.map((r) => r.textContent)).not.toContain("Pick somebody");
});

// AND A READER THE ENGINE WILL REFUSE IS OFFERED NO PICKER AT ALL. Naming
// anybody's handle needs a credential, so for an anonymous reader every row is
// a refusal — and the screen's own sentence named that pick as the remedy.
test("an anonymous reader gets the credential sentence, not a menu of refusals", async () => {
  serving({ viewer: { operator_id: "", operator: false, handle: "", name: "", kind: "" } });
  mount();
  await waitFor(() => expect(screen.getByText(/No credential is presented/)).toBeTruthy());
  expect(screen.queryByRole("combobox", { name: "Whose day" })).toBeNull();
  expect(screen.queryByText(/pick somebody above/)).toBeNull();
});

// THE ORDER IS THE CONTENT, so it is DRAWN. Every other tab on this screen is
// a set somebody has a claim on; Priorities is a sequence somebody decided,
// and as an ordinary run of rows it reads exactly like the Watching list
// beside it — the one thing the tab is about, invisible.
test("the priorities are numbered in the order they were stored", async () => {
  location.hash = "#/me?tab=priorities";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      priorities: [
        task({ key: "ENG-5", title: "first" }),
        task({ key: "ENG-9", title: "second" }),
        task({ key: "ENG-2", title: "third" }),
      ],
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("first")).toBeTruthy());
  const places = [...container.querySelectorAll(".work-cell-ord")].map((el) => el.textContent);
  expect(places).toEqual(["1", "2", "3"]);
  // AND IN THE STORED ORDER, never re-sorted: the keys are not ascending, and
  // a list that sorted them would discard the decision it exists to show.
  const rows = [...container.querySelectorAll(".work-row")].map(
    (el) => el.querySelector(".work-key")?.textContent,
  );
  expect(rows).toEqual(["ENG-5", "ENG-9", "ENG-2"]);
});

// AND NO OTHER CLAIM IS NUMBERED. A place on a list that nobody arranged is a
// rank the engine never stored, read as one somebody did.
test("a claim that is a set rather than a sequence draws no places", async () => {
  location.hash = "#/me?tab=watching";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, watching_recent: [task({ key: "ENG-3" })] },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  expect(container.querySelector(".work-cell-ord")).toBeNull();
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
