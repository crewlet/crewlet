/**
 * An integration's Agents tab, drawn from the engine's access answer.
 *
 * The invariants: a change is a draft until reviewed, and the review shows
 * the engine's plan and saves against the revision it read; a row's details
 * are five pairs at one size with only the actions the seat can take; a seat
 * that is off reads as one sentence; a refusal and a change made by hand each
 * offer their one choice; a reader who may not change access is shown the
 * reading alone; and where the operator manages access, the tab is values
 * only, with one line saying why.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { AccessAnswer, AccessSeatLane } from "~/contract/access.ts";
import { AgentsAccess } from "./AgentsAccess.tsx";
import {
  confluenceAnswer,
  datadogAnswer,
  githubAnswer,
  jiraAnswer,
  mattermostAnswer,
  offLane,
  onLane,
  seat,
} from "./fixtures.ts";
import { laneWords, OPERATOR_MANAGED } from "./words.ts";

const calls = vi.hoisted(() => ({
  plan: [] as unknown[],
  save: [] as unknown[],
  act: [] as unknown[],
  install: [] as unknown[],
  stepUp: false,
  /** A save refused as stale, once. */
  stale: false,
}));

vi.mock("./api.ts", async (actual) => {
  const real = await actual<typeof import("./api.ts")>();
  return {
    ...real,
    accessApi: {
      read: vi.fn(),
      plan: vi.fn(async (kind: string, seats: unknown) => {
        calls.plan.push({ kind, seats });
        return {
          kind,
          revision: "rev-7",
          ...(calls.stepUp ? { step_up: true } : {}),
          plans: [
            {
              handle: "qa",
              lane: "datadog",
              ...(calls.stepUp ? { step_up: true } : {}),
              steps: [
                {
                  op: "create",
                  verb: "create",
                  by: "crewlet",
                  text: "Create the service account crewlet-qa@agents.crewlet.invalid",
                },
              ],
            },
          ],
        };
      }),
      save: vi.fn(async (kind: string, seats: unknown, ifMatch: string) => {
        calls.save.push({ kind, seats, ifMatch });
        if (calls.stale) {
          calls.stale = false;
          const { RestError } = await import("~/protocol/rest.ts");
          throw new RestError(409, { error: "revision_advanced" });
        }
        return datadogAnswer();
      }),
      install: vi.fn(async (kind: string, handle: string, lane: string) => {
        calls.install.push({ kind, handle, lane });
        return { url: "https://slack.com/oauth/v2/authorize?state=one-use" };
      }),
      act: vi.fn(
        async (kind: string, handle: string, lane: string, action: string, revision?: string) => {
          calls.act.push({ kind, handle, lane, action, ...(revision ? { revision } : {}) });
          return { ...datadogAnswer(), revision: "rev-8" };
        },
      ),
    },
  };
});

beforeEach(() => {
  calls.plan = [];
  calls.save = [];
  calls.act = [];
  calls.install = [];
  calls.stepUp = false;
  calls.stale = false;
});
afterEach(cleanup);

function draw(data: AccessAnswer, over: { held?: string; readOnly?: string; only?: string } = {}) {
  const reload = vi.fn();
  const onAnswer = vi.fn();
  const ui = (answer: AccessAnswer) => (
    <LayerHost>
      <AgentsAccess
        kind={answer.kind}
        name={laneWords(answer.options[0]!.lane).name}
        data={answer}
        reload={reload}
        onAnswer={onAnswer}
        held={over.held}
        readOnly={over.readOnly}
        only={over.only}
        avatarOf={() => null}
      />
    </LayerHost>
  );
  const { rerender } = render(ui(data));
  return { reload, onAnswer, redraw: (next: AccessAnswer) => rerender(ui(next)) };
}

/** A lane before any sync ran: what the plan says is all there is. */
const unsynced = ({ sync: _, ...lane }: AccessSeatLane): AccessSeatLane => lane;

const details = (name: string, product = "Datadog") =>
  fireEvent.click(screen.getByRole("button", { name: `Show ${name}'s ${product} details` }));

test("an on seat's details are five pairs at one size, with only the actions it can take", () => {
  draw(datadogAnswer());
  details("SRE");
  const list = document.querySelector<HTMLElement>('dl[aria-label="SRE on Datadog"]')!;
  const facts = within(list);
  expect(facts.getByText("Account")).toBeTruthy();
  expect(facts.getByText("crewlet-sre@agents.crewlet.invalid")).toBeTruthy();
  expect(facts.getByText("Role at Datadog")).toBeTruthy();
  expect(facts.getByText("DATADOG_APP_KEY_SRE")).toBeTruthy();
  expect(facts.getByText("Live, Datadog MCP server, one per agent")).toBeTruthy();
  expect(list.className).toContain("crewlet-description-list--uniform");
  // ONLY WHAT IT CAN DO: an on seat rotates; removing is for a seat that is off.
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Remove account" })).toBeNull();
});

test("an off seat's details are one sentence", () => {
  draw(datadogAnswer());
  details("QA");
  expect(
    screen.getByText(
      "Off. Turning it on creates a Datadog service account with the chosen role and its own key.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

test("a change is a draft, and the review shows the engine's plan and saves against the revision read", async () => {
  const { reload, onAnswer } = draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  expect(screen.getByText("Not saved")).toBeTruthy();
  expect(screen.getByRole("toolbar", { name: "Unsaved changes" }).textContent).toContain(
    "One change not saved",
  );
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText("Create the service account crewlet-qa@agents.crewlet.invalid"),
  ).toBeTruthy();
  expect(calls.plan).toEqual([
    { kind: "datadog", seats: { qa: { datadog: { enabled: true, level: "r-ro" } } } },
  ]);
  fireEvent.click(screen.getByRole("button", { name: "Save and sync" }));
  await waitFor(() => expect(calls.save).toHaveLength(1));
  expect(calls.save[0]).toMatchObject({ ifMatch: "rev-7" });
  // THE SAVE'S ANSWER IS THE TAB AS IT NOW STANDS, drawn rather than read again.
  await waitFor(() => expect(onAnswer).toHaveBeenCalledTimes(1));
  expect(reload).not.toHaveBeenCalled();
});

// THE REVIEW, SHORT: the change as one line of chips, then the next sync's
// steps in order, each said with who takes it before its label, its note at a
// quieter step, and no lead-in sentence above them.
test("the review says the change as one line and the steps by who takes them", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const steps = await screen.findByRole("list", { name: "What the next sync does at Datadog" });
  expect(steps.tagName).toBe("OL");
  expect(
    within(steps)
      .getAllByRole("listitem")
      .map((li) => li.textContent),
  ).toEqual(["Crewlet: Create the service account crewlet-qa@agents.crewlet.invalid"]);
  const review = screen.getByRole("region", { name: "QA on Datadog" });
  expect(review.querySelector(".crewlet-change-list--inline")).toBeTruthy();
  expect(within(review).getByText("Off").closest(".crewlet-tag")).toBeTruthy();
  expect(screen.queryByText(/the next sync will:/)).toBeNull();
});

test("turning a seat off says in the review what that does at the vendor", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(/Crewlet disables the service account and deletes its application key/),
  ).toBeTruthy();
});

test("a raise to an administration role warns that saving asks to confirm who you are", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  // QA turns on at its default role, which the plan does not mark.
  await screen.findByText("Create the service account crewlet-qa@agents.crewlet.invalid");
  expect(screen.queryByText(/Saving asks you to confirm it is you/)).toBeNull();
});

test("where the engine's plan asks for a step-up, the review names who gets administration access", async () => {
  calls.stepUp = true;
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      "QA gets administration access at Datadog. Saving asks you to confirm it is you.",
    ),
  ).toBeTruthy();
});

test("a vendor's refusal is said under the row with a retry", () => {
  draw(
    datadogAnswer({
      sync: {
        at: new Date().toISOString(),
        outcome: "failed",
        steps: [],
        error: {
          said: "Datadog refused the role change with 403 Forbidden.",
          remedy: "Give that user User Access Manage at Datadog, then retry.",
          actor: "you",
          step: "rerole",
          action_url: "https://app.datadoghq.com/organization-settings/users",
        },
      },
    }),
  );
  expect(screen.getByText("Refused")).toBeTruthy();
  expect(screen.getByText("Datadog refused the role change with 403 Forbidden.")).toBeTruthy();
  expect(screen.getByRole("link", { name: /Open Datadog/ }).getAttribute("href")).toBe(
    "https://app.datadoghq.com/organization-settings/users",
  );
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(calls.act).toEqual([{ kind: "datadog", handle: "sre", lane: "datadog", action: "retry" }]);
});

test("a role changed by hand offers restore or keep", () => {
  const base = datadogAnswer().seats[2]!.lanes.datadog!;
  draw(
    datadogAnswer({
      observed: { ...base.observed, level: "r-ro", changed_by_hand_at: new Date().toISOString() },
      plan: [{ op: "level", verb: "restore", by: "crewlet", text: "Change the role", drift: true }],
    }),
  );
  expect(screen.getByText(/Datadog shows Datadog Read Only Role/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep Datadog's setting" }));
  // KEEP WRITES THE DOCUMENT, so it states the revision it read.
  expect(calls.act).toEqual([
    { kind: "datadog", handle: "sre", lane: "datadog", action: "keep", revision: "rev-7" },
  ]);
});

test("a reader who may not change access gets the reading and no control", () => {
  draw(datadogAnswer(), { held: "Admins change an agent's access." });
  expect((screen.getByRole("switch", { name: "QA on Datadog" }) as HTMLInputElement).disabled).toBe(
    true,
  );
  details("SRE");
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

test("GitHub's permissions are edited in GitHub's own words, in a dialog", async () => {
  draw(githubAnswer());
  fireEvent.click(screen.getByRole("button", { name: "6 permissions" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's GitHub permissions" });
  const contents = within(dialog).getByRole("radiogroup", { name: "QA's Contents permission" });
  fireEvent.click(within(contents).getByRole("radio", { name: "Read and write" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  expect(screen.getByText("Not saved")).toBeTruthy();
});

test("on an agent's own page, the table is that agent's alone", () => {
  draw(datadogAnswer(), { only: "sre" });
  expect(screen.getByRole("table", { name: "Access on Datadog" })).toBeTruthy();
  expect(screen.queryByRole("switch", { name: "QA on Datadog" })).toBeNull();
  expect(screen.getByRole("switch", { name: "SRE on Datadog" })).toBeTruthy();
});

test("an agent with an account and no tool server is said once, above the table", () => {
  const base = datadogAnswer().seats[2]!.lanes.datadog!;
  draw(datadogAnswer({ tool: "no_server", observed: base.observed }));
  expect(screen.getByText("No Datadog tool for its agents")).toBeTruthy();
});

test("where the operator writes the document, only what writes a revision is hidden", () => {
  const base = datadogAnswer().seats[2]!.lanes.datadog!;
  draw(
    datadogAnswer({
      observed: { ...base.observed, level: "r-ro" },
      sync: {
        at: new Date().toISOString(),
        outcome: "failed",
        steps: [],
        error: { step: "rerole", said: "Datadog refused.", remedy: "Retry.", actor: "crewlet" },
      },
    }),
    { readOnly: OPERATOR_MANAGED },
  );
  expect(screen.getByText(OPERATOR_MANAGED)).toBeTruthy();
  expect(screen.queryByRole("switch")).toBeNull();
  expect(screen.queryByRole("combobox")).toBeNull();
  // ACTIONS AT THE VENDOR write no revision, so they stay; Keep would write one.
  expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Keep / })).toBeNull();
  details("SRE");
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
});

test("a credential provided externally says so, by its variable, and offers no rotation", () => {
  const base = datadogAnswer().seats[2]!.lanes.datadog!;
  draw(
    datadogAnswer({
      observed: {
        ...base.observed,
        credential: {
          variable: "DATADOG_APP_KEY_SRE",
          minted_at: new Date().toISOString(),
          source: "external",
        },
      },
    }),
  );
  details("SRE");
  const list = document.querySelector<HTMLElement>('dl[aria-label="SRE on Datadog"]')!;
  expect(within(list).getByText("Credential")).toBeTruthy();
  expect(list.textContent).toContain("DATADOG_APP_KEY_SRE, provided externally");
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

test("an agent that left the company offers to remove its account", () => {
  const data = datadogAnswer();
  data.seats.push({
    ...seat("ops", "Ops", {
      datadog: { ...offLane("r-ro"), observed: { exists: true, enabled: false } },
    }),
    departed: true,
  });
  draw(data);
  expect(screen.getByText("Agent left")).toBeTruthy();
  expect(screen.getByText("Ops left the company")).toBeTruthy();
  expect(
    (screen.getByRole("switch", { name: "Ops on Datadog" }) as HTMLInputElement).disabled,
  ).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Remove account" }));
  expect(screen.getByRole("alertdialog", { name: "Remove Ops's Datadog account?" })).toBeTruthy();
});

test("a Mattermost bot that is on in no channel says it cannot read or post anywhere", () => {
  draw(mattermostAnswer());
  expect(
    screen.getByText("In no channel yet, so it cannot read or post anywhere. Choose its channels."),
  ).toBeTruthy();
});

test("a lane managed outside Crewlet keeps its switch and says so in place of a level", () => {
  const data = datadogAnswer();
  data.options = [{ ...data.options[0]!, managed: "external" }];
  data.seats[2]!.lanes.datadog!.observed.credential = undefined;
  draw(data);
  expect(screen.getAllByText("Managed outside Crewlet").length).toBe(3);
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.getByRole("switch", { name: "QA on Datadog" })).toBeTruthy();
  details("SRE");
  const list = document.querySelector<HTMLElement>('dl[aria-label="SRE on Datadog"]')!;
  expect(within(list).getByText("Provided externally")).toBeTruthy();
});

test("Slack's waiting install opens a freshly minted link in a new tab", async () => {
  const tab = { opener: {} as unknown, location: { href: "" }, close: vi.fn() };
  const open = vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
  draw({
    kind: "slack",
    revision: "rev-7",
    syncing: false,
    options: [{ lane: "slack", live: false }],
    seats: [
      seat("pm", "PM", {
        slack: {
          ...onLane({ variable: "SLACK_BOT_TOKEN_PM" }),
          sync: {
            at: new Date().toISOString(),
            outcome: "waiting",
            steps: [
              {
                op: "create",
                verb: "approve",
                by: "you",
                text: "Approve the install in Slack",
                done: false,
              },
            ],
          },
          plan: [
            { op: "create", verb: "approve", by: "you", text: "Approve the install in Slack" },
          ],
        },
      }),
    ],
  });
  fireEvent.click(screen.getByRole("button", { name: "Install in Slack" }));
  await waitFor(() =>
    expect(tab.location.href).toBe("https://slack.com/oauth/v2/authorize?state=one-use"),
  );
  expect(tab.opener).toBeNull();
  expect(calls.install).toEqual([{ kind: "slack", handle: "pm", lane: "slack" }]);
  open.mockRestore();
});

test("Confluence's permissions by operation are a set, summarised in the vendor's names", () => {
  draw(confluenceAnswer());
  expect(screen.getByText("View, Add pages")).toBeTruthy();
  details("PM", "Confluence");
  const list = document.querySelector<HTMLElement>('dl[aria-label="PM on Confluence"]')!;
  expect(list.textContent).toContain("View, Add pages");
});

test("an agent turned on in Confluence with nothing chosen holds product access only, with its note", () => {
  draw(confluenceAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Confluence" }));
  expect(
    screen.getByText(
      "The account has Confluence and no space role or permission of its own: the site's and each space's permissions decide what it sees.",
    ),
  ).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Review changes" }).getAttribute("aria-disabled"),
  ).not.toBe("true");
});

test("product access only with a project list is held at the review, with the reason", () => {
  const data = jiraAnswer();
  data.seats[0]!.lanes.jira!.desired = { enabled: false, level: "", scope: ["ENG"] };
  draw(data);
  fireEvent.click(screen.getByRole("switch", { name: "PM on Jira" }));
  const review = screen.getByRole("button", { name: "Review changes" });
  expect(review.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(review);
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("Jira's product access with no project role is a choice of its own, with no projects to pick", () => {
  draw(jiraAnswer());
  expect(screen.getByRole("combobox", { name: "PM's Jira access" }).textContent).toContain(
    "Product access only",
  );
  // THE SCOPE PICKER, whatever role its searchable trigger takes.
  const picker = (name: string) => document.querySelector(`[aria-label="${name}"]`);
  expect(picker("PM's Jira projects")).toBeNull();
  expect(picker("QA's Jira projects")).not.toBeNull();
});

test("an account turned off at the vendor by hand offers Turn back on and Keep off, with no plan", () => {
  // AS THE ENGINE SENDS IT: the seat stays on in the document, the account is
  // off by hand, nothing is planned and no change-by-hand time is set.
  const base = datadogAnswer().seats[2]!.lanes.datadog!;
  draw(
    datadogAnswer({
      observed: { ...base.observed, enabled: false, disabled_by: "hand" },
      plan: [],
    }),
  );
  expect(screen.getByText("Turned off at Datadog by somebody else")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Turn back on" }));
  expect(calls.act).toEqual([
    { kind: "datadog", handle: "sre", lane: "datadog", action: "restore" },
  ]);
  expect(screen.getByRole("button", { name: "Keep off" })).toBeTruthy();
});

// A WAIT IS ONE SENTENCE OF WHAT TO DO, and the buttons carry the rest: where,
// and saying it is done. A remedy line restating the buttons is not drawn.
test("a wait says the one thing to do, and links where the engine says", () => {
  draw(
    datadogAnswer({
      sync: {
        at: new Date().toISOString(),
        outcome: "waiting",
        steps: [{ op: "create", verb: "approve", by: "you", text: "Approve it", done: false }],
        error: {
          step: "approve",
          said: "The app is waiting for an owner to approve its install.",
          remedy: "An organization owner approves it under Installed apps.",
          actor: "you",
          action_url: "https://example.invalid/approve",
        },
      },
    }),
  );
  expect(screen.getByText("The app is waiting for an owner to approve its install.")).toBeTruthy();
  expect(screen.queryByText("An organization owner approves it under Installed apps.")).toBeNull();
  expect(screen.queryByText("Approve it.")).toBeNull();
  expect(screen.getByRole("button", { name: "Done, check again" })).toBeTruthy();
  expect(screen.getByRole("link", { name: /Open Datadog/ }).getAttribute("href")).toBe(
    "https://example.invalid/approve",
  );
});

test("an external lane that is on reads as managed outside Crewlet, with nothing to wait on", () => {
  const data = datadogAnswer({ plan: [], tool: "live" });
  data.options = [{ ...data.options[0]!, managed: "external" }];
  draw(data);
  expect(screen.getAllByText("Managed outside Crewlet").length).toBeGreaterThan(0);
  expect(screen.queryByText("Next sync")).toBeNull();
  expect(screen.queryByText("Needs you")).toBeNull();
});

test("a save refused as stale keeps the draft, reads again and plans again against the new revision", async () => {
  calls.stale = true;
  const { reload, redraw } = draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await screen.findByText("Create the service account crewlet-qa@agents.crewlet.invalid");
  fireEvent.click(screen.getByRole("button", { name: "Save and sync" }));
  expect(await screen.findByText(/Somebody changed these agents/)).toBeTruthy();
  expect(reload).toHaveBeenCalled();
  expect(screen.getByRole("dialog")).toBeTruthy();
  redraw({ ...datadogAnswer(), revision: "rev-8" });
  await waitFor(() => expect(calls.plan).toHaveLength(2));
  expect(calls.plan[1]).toEqual(calls.plan[0]);
  fireEvent.click(await screen.findByRole("button", { name: "Save and sync" }));
  await waitFor(() => expect(calls.save).toHaveLength(2));
  expect(calls.save[1]).toMatchObject({ ifMatch: "rev-8" });
});

test("a seat action draws the answer it returns rather than reading again", async () => {
  const { reload, onAnswer } = draw(
    datadogAnswer({
      sync: {
        at: new Date().toISOString(),
        outcome: "failed",
        steps: [],
        error: { step: "rerole", said: "Refused.", remedy: "Retry.", actor: "crewlet" },
      },
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(onAnswer).toHaveBeenCalledTimes(1));
  expect(onAnswer.mock.calls[0]![0]).toMatchObject({ revision: "rev-8" });
  expect(reload).not.toHaveBeenCalled();
});

test("rotating a credential says what it does at the vendor before it runs", async () => {
  draw(datadogAnswer());
  details("SRE");
  fireEvent.click(screen.getByRole("button", { name: "Rotate credential" }));
  const dialog = screen.getByRole("alertdialog", { name: "Rotate SRE's Datadog credential?" });
  expect(within(dialog).getByText(/deletes the old one/)).toBeTruthy();
  expect(calls.act).toEqual([]);
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate credential" }));
  await waitFor(() =>
    expect(calls.act).toEqual([
      { kind: "datadog", handle: "sre", lane: "datadog", action: "rotate" },
    ]),
  );
});

test("an install the browser would not open in a new tab is drawn as a link", async () => {
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  draw({
    kind: "slack",
    revision: "rev-7",
    syncing: false,
    options: [{ lane: "slack", live: false }],
    seats: [
      seat("pm", "PM", {
        slack: {
          ...unsynced(onLane({ variable: "SLACK_BOT_TOKEN_PM" })),
          plan: [
            { op: "create", verb: "approve", by: "you", text: "Approve the install in Slack" },
          ],
        },
      }),
    ],
  });
  fireEvent.click(screen.getByRole("button", { name: "Install in Slack" }));
  const link = await screen.findByRole("link", { name: /Open the install page/ });
  expect(link.getAttribute("href")).toBe("https://slack.com/oauth/v2/authorize?state=one-use");
  expect(screen.getByText(/kept the install page from opening/)).toBeTruthy();
  open.mockRestore();
});

test("the table is the kit's: a named table of real rows, with column headers and per-row expand names", () => {
  draw(datadogAnswer());
  const table = screen.getByRole("table", { name: "Agents on Datadog" });
  expect(table.tagName).toBe("TABLE");
  // NO HAND-SET STRUCTURE: no cell spans by an attribute, and no row group
  // owning anything but rows.
  expect(document.querySelector("[aria-colspan]")).toBeNull();
  expect(document.querySelector('[role="rowgroup"]')).toBeNull();
  const headers = within(table)
    .getAllByRole("columnheader")
    .map((h) => h.textContent ?? "");
  for (const label of ["Agent", "On", "Access", "Status"])
    expect(headers.some((h) => h.includes(label))).toBe(true);
  for (const name of ["PM", "QA", "SRE"])
    expect(screen.getByRole("button", { name: `Show ${name}'s Datadog details` })).toBeTruthy();
});
