/**
 * What the drawer draws of the engine's cards: a read's peek (the columns the
 * engine chose for its width, values worded by their type as the screens word
 * them, the facts kept for the model left out), a refusal that claims a
 * missing permission only where one is the way past it, a failed step's
 * reason, "Prepare it again" on a card that moved, and a run that took no step
 * drawn without a count.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type {
  AssistantCardEvent,
  AssistantErrorCard,
  AssistantProposalCard,
} from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import {
  CardBlock,
  factText,
  placeOf,
  prepareAgainQuestion,
  ReplyProse,
  runChipOf,
  stepDuration,
  stepsOf,
  type CardHandlers,
} from "./blocks.tsx";
import { readsOf } from "./mentions.ts";
import { ReadChip } from "./reads.tsx";
import type { TurnView } from "./turn.ts";

afterEach(cleanup);

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

/** Draws one card that is not a read. */
function draw(card: AssistantCardEvent["card"], handlers: CardHandlers = on) {
  return render(
    <Router>
      <CardBlock
        event={{ card_id: "c", message_id: "m", card }}
        local={undefined}
        on={handlers}
        opened={false}
      />
    </Router>,
  );
}

/** Draws one read as its chip and opens its peek: these cases are about what a peek holds. */
function peek(card: AssistantCardEvent["card"]) {
  const read = readsOf([{ card_id: "c", message_id: "m", card }]).reads[0]!;
  render(
    <Router>
      <ReadChip read={read} on={on} />
    </Router>,
  );
  fireEvent.click(screen.getByRole("button"));
  return screen.getByRole("dialog");
}

test("a table's peek draws only the columns the engine chose for the drawer, a status in words", () => {
  const dialog = peek({
    type: "table",
    table: {
      title: "Tasks",
      columns: [
        { key: "title", label: "Title", type: "text" },
        { key: "status", label: "Status", type: "label" },
        { key: "key", label: "Key", type: "text", detail: true },
        { key: "due", label: "Due", type: "datetime", detail: true },
      ],
      rows: [["Settle up", "in_progress", "FAIR-1", null]],
      row_count: 1,
      truncated: false,
      as_of: "2026-10-24T09:41:00Z",
    },
  });
  const heads = within(dialog)
    .getAllByRole("columnheader")
    .map((h) => h.textContent);
  expect(heads).toEqual(["Title", "Status"]);
  expect(within(dialog).getByText("In progress")).toBeTruthy();
  expect(within(dialog).queryByText("FAIR-1")).toBeNull();
});

test("a fact is worded by its type, and one kept for the model is not drawn", () => {
  expect(factText({ value: "in_review", type: "label" })).toBe("In review");
  expect(factText({ value: "ada" })).toBe("ada");
  const dialog = peek({
    type: "entity",
    entity: {
      kind: "task",
      id: "FAIR-1",
      title: "Settle up",
      facts: [
        { label: "Status", value: "todo", type: "label" },
        { label: "Version", value: "7", type: "number", detail: true },
      ],
    },
  });
  expect(within(dialog).getByText("To do")).toBeTruthy();
  expect(within(dialog).queryByText("Version")).toBeNull();
});

test("a record with a list under it keeps its facts in its peek: the principles over their policies", () => {
  const dialog = peek({
    type: "entity",
    entity: {
      kind: "unit",
      id: "",
      title: "Company principles",
      facts: [
        { label: "Mission", value: "Help friends share costs fairly." },
        { label: "Vision", value: "Settling up takes seconds." },
      ],
      items: [
        { kind: "unit", id: "1", label: "Money is an integer number of cents." },
        { kind: "unit", id: "2", label: "Miles is the final authority." },
      ],
    },
  });
  expect(within(dialog).getByText("Help friends share costs fairly.")).toBeTruthy();
  expect(within(dialog).getByText("Settling up takes seconds.")).toBeTruthy();
  expect(within(dialog).getByText("Money is an integer number of cents.")).toBeTruthy();
});

test("a record's peek keeps the list under it, named by the record", () => {
  const dialog = peek({
    type: "entity",
    entity: {
      kind: "task",
      id: "FAIR-1",
      title: "Settle up",
      items: [{ kind: "task", id: "c1", label: "Looks right to me.", fact: "ada" }],
    },
  });
  expect(within(dialog).getByText("Looks right to me.")).toBeTruthy();
  expect(within(dialog).getByRole("list", { name: "Settle up" })).toBeTruthy();
});

test("a step too quick to time says no time, and one not yet ended none either", () => {
  expect(stepDuration(undefined)).toBeUndefined();
  expect(stepDuration(30)).toBeUndefined();
  expect(stepDuration(99)).toBeUndefined();
  // THE PRODUCT'S ONE DURATION ([fmtDuration]): milliseconds under a second.
  expect(stepDuration(100)).toBe("100ms");
  expect(stepDuration(400)).toBe("400ms");
  expect(stepDuration(12_400)).toBe("12s");
  expect(stepDuration(845_000)).toBe("14m 5s");
});

test("a read handed to the card drawer is a caller's mistake, said as one", () => {
  expect(() =>
    draw({ type: "entity", entity: { kind: "task", id: "FAIR-1", title: "Settle up" } }),
  ).toThrow(/drawn by its answer/);
});

/** A read of the fleet the engine refused with `refusal`, as its error card. */
function refused(refusal: AssistantErrorCard["refusal"]) {
  return draw({ type: "error", error: { refusal, phrase: "read the fleet" } });
}

test("a refusal access would clear names the access, and offers a request for it", async () => {
  let written = "";
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => ((written = t), Promise.resolve()) },
  });
  refused({
    code: "unauthorized",
    message: "You don't have the access this needs (Admin). Ask an admin for it.",
    reason: "no_grant",
    grants: ["admin"],
  });
  expect(screen.getByText("You do not have permission to read the fleet.")).toBeTruthy();
  expect(screen.getByText(/^You need Admin access for this\. Ask Crewlet acts/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Copy a request to an admin" }));
  await waitFor(() => expect(written).toBe("Please give me Admin access to read the fleet."));
});

// THE RULE DECIDES WHETHER ACCESS IS THE WAY PAST, NEVER THE GRANTS ALONE: a
// stale proof names the grants that admitted the person, an artifact the admin
// override beside its owner's share, and the rest name none. Each is the
// engine's own sentence for its rule, with no missing permission claimed and
// nothing to ask an admin for. Mutation: decide by the grants alone and the
// first two rows fail; claim the permission or offer the request whatever the
// rule, and every row fails.
test.each<[string, AssistantErrorCard["refusal"]]>([
  [
    "a proof to confirm",
    {
      code: "step_up_required",
      message: "Confirm it's you to do this, then try again.",
      reason: "step_up",
      grants: ["admin"],
    },
  ],
  [
    "a role on an artifact below the verb's",
    {
      code: "unauthorized",
      message:
        "Your role on this doesn't allow that. Whoever owns it can share it with you at a higher one.",
      reason: "role",
      grants: ["admin"],
    },
  ],
  [
    "a verb the assistant never takes",
    {
      code: "unauthorized",
      message: "The assistant never does this, so it stays with you.",
      reason: "assist_refused",
      grants: [],
    },
  ],
  [
    "a change the assistant can only propose",
    {
      code: "unauthorized",
      message: "The assistant can do this only as a change you approve.",
      reason: "assist_scope",
      grants: [],
    },
  ],
  [
    "a verb only an agent takes",
    {
      code: "unauthorized",
      message: "Only an agent can do this.",
      reason: "seat_only",
      grants: [],
    },
  ],
  [
    "an agent acting for somebody who may not",
    {
      code: "unauthorized",
      message: "The person this agent is working for can't do this, so the agent can't either.",
      reason: "ceiling",
      grants: [],
    },
  ],
])("%s is the engine's own sentence, with no request for access", (_, refusal) => {
  refused(refusal);
  expect(screen.getByText(refusal.message)).toBeTruthy();
  expect(screen.queryByText(/permission to/)).toBeNull();
  expect(screen.queryByText(/acts with your access/)).toBeNull();
  expect(screen.queryByRole("button", { name: "Copy a request to an admin" })).toBeNull();
});

const proposal = {
  proposal_id: "p",
  scope: "server",
  tool: "update_task",
  title: "Update task",
  changes: [],
  digest: `sha256:${"a".repeat(64)}`,
  expires_at: "2026-10-24T10:41:00Z",
  state: "stale",
  approve_label: "Approve and update",
} as AssistantProposalCard;

test("a card that moved offers Prepare it again, which asks for the change afresh", () => {
  const prepareAgain = vi.fn();
  draw({ type: "proposal", proposal }, { ...on, prepareAgain });
  fireEvent.click(screen.getByRole("button", { name: /Prepare it again/ }));
  expect(prepareAgain).toHaveBeenCalledWith(proposal);
  expect(prepareAgainQuestion(proposal)).toContain("Update task");
});

test("a pending card does not offer Prepare it again", () => {
  draw({ type: "proposal", proposal: { ...proposal, state: "proposed" } });
  expect(screen.queryByRole("button", { name: /Prepare it again/ })).toBeNull();
});

const base: TurnView = {
  key: "k",
  id: "t",
  phase: "completed",
  seq: 1,
  question: "q",
  page: null,
  attachments: [],
  askedAt: "2026-10-24T09:41:00Z",
  answeredAt: null,
  stepsDone: 0,
  steps: [],
  replies: [],
  cards: [],
  proposals: {},
  actions: [],
  actionStates: {},
  done: { state: "completed", duration_ms: 1000, steps: 0 },
  error: null,
  refusal: null,
  stopping: false,
};

test("a failed step says why, in the engine's words", () => {
  const [step] = stepsOf(
    {
      ...base,
      steps: [
        {
          step_id: "s",
          index: 1,
          tool: "update_task",
          label: "Preparing a change to FAIR-2",
          state: "failed",
          started_at: "2026-10-24T09:41:00Z",
          refusal: {
            code: "stale",
            message: "FAIR-2 changed since it was read.",
            hint: "Read it again.",
          },
        },
      ],
    },
    false,
  );
  expect(step!.detail).toContain("FAIR-2 changed since it was read. Read it again.");
});

// A STEP'S LINE NAMES NO TOOL, AND ITS REQUEST ONLY TO AN ADMINISTRATOR: a
// person reads "Listing tasks", never "list_tasks · GET /worklet". Mutation:
// put the tool back in the detail, or show the request to everybody.
test("a step's line names no tool, and its request only to an administrator", () => {
  const listed: TurnView = {
    ...base,
    steps: [
      {
        step_id: "s",
        index: 1,
        tool: "list_tasks",
        label: "Listing tasks",
        request: { method: "GET", route: "/worklet" },
        state: "done",
        started_at: "2026-10-24T09:41:00Z",
      },
    ],
  };
  const [person] = stepsOf(listed, false);
  expect(person!.label).toBe("Listing tasks");
  expect(person!.detail).toBeUndefined();
  const [admin] = stepsOf(listed, true);
  expect(admin!.detail).toBe("GET /worklet");
  expect(JSON.stringify([person, admin])).not.toContain("list_tasks");
});

test("a run that took no step carries no count", () => {
  expect(runChipOf(base, Date.parse("2026-10-24T09:41:05Z")).meta).toBeUndefined();
});

test("a reply's paragraphs are blocks in a div the Markdown rhythm spaces", () => {
  const { container } = render(<ReplyProse text={"First paragraph.\n\nSecond paragraph."} />);
  const root = container.firstElementChild!;
  expect(root.tagName).toBe("DIV");
  expect(root.classList.contains("prose") && root.classList.contains("md")).toBe(true);
  expect(root.querySelectorAll("p")).toHaveLength(2);
});

// A REFUSED READ THAT IS NOT ABOUT ACCESS never says "You do not have
// permission". Mutation: draw every error card as a permission refusal.
test("a refusal that is not about access is not drawn as one", () => {
  draw({
    type: "error",
    error: {
      refusal: { code: "not_found", message: "The record this needs could not be found." },
      phrase: "read the page",
    },
  });
  expect(screen.queryByText(/do not have permission/)).toBeNull();
  expect(screen.getByText(/could not be found/)).toBeTruthy();
});

// AN OPEN IN APP CARD NAMES THE PLACE WITHOUT A RAW ID: the trail stops at the
// screen, and the card's title names the record. Mutation: keep every crumb.
test("an open in app trail carries no raw id", () => {
  const place = placeOf({
    path: ["live", "turns", "01a175e0-6ced-770f-b6d7-a746e95556e4"],
    query: {},
  });
  expect(place).not.toMatch(/01a175e0/);
  expect(place.length).toBeGreaterThan(0);
});

// A TWO-LETTER HANDLE IS NEVER LOST: a fact holding "qa" and an
// administrator's step request ("GET /agents/qa") are drawn whole. Mutation:
// drop a short value anywhere between the wire and the DOM, and this fails.
test("a two-letter handle is drawn whole", () => {
  const event: AssistantCardEvent = {
    card_id: "e2",
    message_id: "m",
    card: {
      type: "entity",
      entity: {
        kind: "task",
        id: "FAIR-10",
        title: "Write the regression tests",
        facts: [
          { label: "Assignee", value: "qa" },
          { label: "Reviewer", value: "pm" },
        ],
      },
    },
  };
  const { unmount } = render(
    <Router>
      <ReadChip read={readsOf([event]).reads[0]!} on={on} />
    </Router>,
  );
  fireEvent.click(screen.getByRole("button"));
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain("Assigneeqa");
  expect(dialog.textContent).toContain("Reviewerpm");
  unmount();
  const [step] = stepsOf(
    {
      ...base,
      steps: [
        {
          step_id: "s",
          index: 1,
          tool: "get_seat",
          label: "Reading QA Engineer",
          request: { method: "GET", route: "/agents/qa" },
          state: "done",
          started_at: "2026-10-24T09:41:00Z",
        },
      ],
    },
    true,
  );
  expect(step!.detail).toBe("GET /agents/qa");
});

test("a brief's rows open the address the engine built, and its follow-ups ask in the conversation", () => {
  const openHere = vi.fn();
  const ask = vi.fn();
  draw(
    {
      type: "brief",
      brief: {
        scope: { screen: "inbox", title: "Inbox" },
        about: "QA waits on your answer on ENG-42, and the QA Engineer is stopped.",
        needs: [
          {
            ref: { kind: "seat", id: "qa" },
            text: "Raise the QA Engineer's budget",
            detail: "Stopped 2 hours ago",
            tone: "danger",
            action: {
              kind: "open",
              label: "Open the agent",
              link: { label: "qa", screen: "seat", href: "#/company/org/seats/qa" },
            },
          },
          {
            ref: { kind: "task", id: "ENG-42" },
            text: "Answer ENG-42: which build QA tests",
            tone: "warning",
            action: { kind: "ask", label: "Ask", question: "What did QA test last?" },
          },
        ],
        context: "The beta ships Thursday.",
        follow_ups: ["Why is the QA Engineer out of budget?"],
        as_of: "2026-10-26T09:00:00Z",
      },
    },
    { ...on, openHere, ask },
  );
  expect(screen.getByRole("region", { name: "Brief 2 things need you" })).toBeDefined();
  expect(screen.getByText("Inbox")).toBeDefined();
  expect(
    screen.getByText("QA waits on your answer on ENG-42, and the QA Engineer is stopped."),
  ).toBeDefined();
  expect(screen.getByText("Stopped 2 hours ago")).toBeDefined();
  expect(screen.getByText("The beta ships Thursday.")).toBeDefined();
  const rows = screen.getAllByRole("listitem").filter((row) => row.closest("ol") !== null);
  expect(rows[0]?.textContent).toContain("Do first");
  expect(rows[0]?.textContent).toContain("Raise the QA Engineer's budget");
  fireEvent.click(screen.getByRole("button", { name: "Open the agent" }));
  expect(openHere).toHaveBeenCalledWith(
    expect.objectContaining({ path: ["company", "org", "seats", "qa"] }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Ask" }));
  expect(ask).toHaveBeenCalledWith("What did QA test last?");
  fireEvent.click(screen.getByRole("button", { name: "Why is the QA Engineer out of budget?" }));
  expect(ask).toHaveBeenCalledWith("Why is the QA Engineer out of budget?");
});

test("a brief's verdict is the rows it carries, counted, at every length the engine sends", () => {
  const row = (id: string) => ({
    ref: { kind: "task" as const, id },
    text: `Answer ${id}`,
    tone: "warning" as const,
    action: { kind: "ask" as const, label: "Ask", question: `What is ${id}?` },
  });
  const verdicts = [
    "Nothing needs you",
    "1 thing needs you",
    "2 things need you",
    "3 things need you",
  ];
  verdicts.forEach((verdict, n) => {
    cleanup();
    draw({
      type: "brief",
      brief: {
        scope: { screen: "home", title: "Home" },
        about: "Two agents are working.",
        needs: ["ENG-1", "ENG-2", "ENG-3"].slice(0, n).map(row),
        follow_ups: [],
        as_of: "2026-10-26T09:00:00Z",
      },
    });
    expect(screen.getByRole("region", { name: `Brief ${verdict}` })).toBeDefined();
    expect(screen.queryAllByText("Do first")).toHaveLength(n > 1 ? 1 : 0);
  });
});

test("a brief of one record names it, and nothing waiting is said once, in its head", () => {
  draw({
    type: "brief",
    brief: {
      scope: {
        screen: "inbox",
        title: "Inbox",
        record: { kind: "task", id: "ENG-7", noun: "decision" },
      },
      about: "QA asked which build to test, and you answered.",
      needs: [],
      follow_ups: [],
      as_of: "2026-10-26T09:00:00Z",
    },
  });
  expect(screen.getByRole("region", { name: "Brief Nothing needs you" })).toBeDefined();
  expect(screen.getByText("Decision ENG-7")).toBeDefined();
  expect(screen.getAllByText("Nothing needs you")).toHaveLength(1);
  expect(screen.getByText("QA asked which build to test, and you answered.")).toBeDefined();
});

test("a room's brief draws what was said, each point opening its message in the room", () => {
  const openHere = vi.fn();
  draw(
    {
      type: "brief",
      brief: {
        scope: {
          screen: "chatlet",
          title: "Chatlet",
          record: { kind: "room", id: "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e41" },
        },
        about: "The release waits on GP-12.",
        needs: [],
        points: [
          {
            text: "Sam asks whether 2.4.0 waits for GP-12.",
            source: { channel_id: "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e41", message_id: "m1" },
          },
          {
            text: "Ada reports GP-3 passed 13 of 14 scenarios.",
            source: {
              channel_id: "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e41",
              message_id: "m2",
              thread_root: "m1",
            },
          },
        ],
        follow_ups: [],
        as_of: "2026-10-26T09:00:00Z",
      },
    },
    { ...on, openHere },
  );
  expect(screen.getByText("Sam asks whether 2.4.0 waits for GP-12.")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Open the thread" }));
  expect(openHere).toHaveBeenCalledWith({
    path: ["chatlet", "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e41"],
    query: { message: "m2" },
  });
  expect(screen.getByRole("button", { name: "Open the message" })).toBeDefined();
});

test("a brief's open whose address is not this dashboard's draws no press", () => {
  draw({
    type: "brief",
    brief: {
      scope: { screen: "inbox", title: "Inbox" },
      about: "One row.",
      needs: [
        {
          ref: { kind: "task", id: "ENG-1" },
          text: "A row",
          tone: "warning",
          action: {
            kind: "open",
            label: "Open ENG-1",
            link: { label: "ENG-1", screen: "item", href: "https://example.com/" },
          },
        },
      ],
      follow_ups: [],
      as_of: "2026-10-26T09:00:00Z",
    },
  });
  expect(screen.getByText("A row")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Open ENG-1" })).toBeNull();
});
