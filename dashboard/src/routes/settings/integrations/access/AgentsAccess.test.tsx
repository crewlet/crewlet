/**
 * An integration's Agents tab, drawn from the engine's access answer.
 *
 * The invariants: a change is a draft until reviewed, and the review shows
 * the engine's plan and saves against the revision it read; a row's details
 * are five pairs at one size, and a sixth where Crewlet sets anything in the
 * agent's box, with only the actions the seat can take; a seat
 * that is off reads as one sentence; a refusal and a change made by hand each
 * offer their one choice; a reader who may not change access is shown the
 * reading alone; and where the operator manages access, the tab is values
 * only, with one line saying why.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { AccessAnswer, AccessSeatLane } from "~/contract/access.ts";
import { pick } from "~/testing.tsx";
import { AgentsAccess } from "./AgentsAccess.tsx";
import {
  cloudflareAnswer,
  confluenceAnswer,
  datadogAnswer,
  expoAnswer,
  expoRetiredAnswer,
  expoWaitingAnswer,
  githubAnswer,
  gitlabAnswer,
  awsAnswer,
  googleCloudAnswer,
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
  credential: [] as unknown[],
  stepUp: false,
  /** A save refused as stale, once. */
  stale: false,
  /** The revision the engine's plan is drawn from: the document as it stands now. */
  planRevision: "rev-7",
  /** The plans the engine answers, where a case names its own. */
  plans: null as unknown[] | null,
  /** The body of a refusal the plan answers instead, once set. */
  planRefusal: null as Record<string, unknown> | null,
}));

vi.mock("./api.ts", async (actual) => {
  const real = await actual<typeof import("./api.ts")>();
  return {
    ...real,
    accessApi: {
      read: vi.fn(),
      plan: vi.fn(async (kind: string, seats: unknown) => {
        calls.plan.push({ kind, seats });
        if (calls.planRefusal) {
          const { RestError } = await import("~/protocol/rest.ts");
          throw new RestError(400, calls.planRefusal);
        }
        return {
          kind,
          revision: calls.planRevision,
          ...(calls.stepUp ? { step_up: true } : {}),
          plans: calls.plans ?? [
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
                {
                  op: "mint",
                  verb: "mint",
                  by: "crewlet",
                  text: "Mint an application key, sealed as DATADOG_APP_KEY_QA",
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
      credential: vi.fn(async (kind: string, handle: string, lane: string, value: string) => {
        calls.credential.push({ kind, handle, lane, value });
        return {
          detail: "Works. Robot crewlet-gophrase-pm, Runner on gophrase. Sealed as EXPO_TOKEN_PM.",
          access: { ...expoAnswer(), revision: "rev-8" },
        };
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
  calls.credential = [];
  calls.stepUp = false;
  calls.stale = false;
  calls.planRevision = "rev-7";
  calls.plans = null;
  calls.planRefusal = null;
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

// THE REVIEW, SHORT: the change as labels and values in one grid, so "On
// Datadog" and "Role at Datadog" line up, then the next sync's steps in order,
// each said with who takes it before its label, and no lead-in sentence above
// them. Mutation: draw the change inline again and the grid goes.
test("the review lays the change out as labels and values, then the steps by who takes them", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const steps = await screen.findByRole("list", { name: "What the next sync does at Datadog" });
  expect(steps.tagName).toBe("OL");
  expect(
    within(steps)
      .getAllByRole("listitem")
      .map((li) => li.textContent),
  ).toEqual([
    "Crewlet: Create the service account crewlet-qa@agents.crewlet.invalid",
    "Crewlet: Mint an application key, sealed as DATADOG_APP_KEY_QA",
  ]);
  const review = screen.getByRole("region", { name: "QA on Datadog" });
  const change = review.querySelector(".crewlet-change-list")!;
  expect(change.className).not.toContain("crewlet-change-list--inline");
  expect(
    [...change.querySelectorAll(".crewlet-change-list__label")].map((l) => l.textContent),
  ).toEqual(["On Datadog", "Role at Datadog"]);
  expect(screen.queryByText(/the next sync will:/)).toBeNull();
});

// A VARIABLE A STEP NAMES IS CODE, the name a reader looks up on the Secrets
// screen, and the mark beside a step is decoration: who takes it is said in
// the words. Mutation: render the step's text as plain text and the name is
// prose again.
test("a variable a step names is drawn as code, and the mark is hidden", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const steps = await screen.findByRole("list", { name: "What the next sync does at Datadog" });
  expect(within(steps).getByText("DATADOG_APP_KEY_QA").tagName).toBe("CODE");
  for (const mark of steps.querySelectorAll(".agent-access-actor")) {
    expect(mark.getAttribute("aria-hidden")).toBe("true");
  }
});

// THE DOCUMENT MOVED SINCE THE TAB READ IT: after a first connect, the access
// migration writes a revision of its own, and a save sent with the tab's older
// one was refused as stale. The plan names the revision it was drawn from, so
// the tab reads again before Save is offered, keeping the draft, and saves
// against the revision it then read. Mutation: drop the comparison and the
// save goes out with rev-7.
test("a plan drawn from a newer revision reads the tab again before Save is offered", async () => {
  calls.planRevision = "rev-8";
  const { reload, redraw } = draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await screen.findByText("Create the service account crewlet-qa@agents.crewlet.invalid");
  await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  const save = () => screen.getByRole("button", { name: "Save and sync" }) as HTMLButtonElement;
  expect(save().getAttribute("aria-disabled") === "true" || save().disabled).toBe(true);
  redraw({ ...datadogAnswer(), revision: "rev-8" });
  await waitFor(() => expect(calls.plan).toHaveLength(2));
  await waitFor(() =>
    expect(save().getAttribute("aria-disabled") === "true" || save().disabled).toBe(false),
  );
  fireEvent.click(save());
  await waitFor(() => expect(calls.save).toHaveLength(1));
  expect(calls.save[0]).toMatchObject({ ifMatch: "rev-8" });
  expect(calls.plan[1]).toEqual(calls.plan[0]);
  expect(reload).toHaveBeenCalledTimes(1);
  expect(screen.queryByText(/Somebody changed these agents/)).toBeNull();
});

// CLOUDFLARE'S ACCOUNT IS THE TOKEN ITSELF: the details name it, the level it
// holds in the tab's own words, and the variable it is sealed in, never a value.
test("a Cloudflare seat's details name its token, its permissions and its tool", () => {
  draw(cloudflareAnswer());
  details("SRE", "Cloudflare");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="SRE on Cloudflare"]')!);
  expect(facts.getByText("crewlet-gophrase-sre")).toBeTruthy();
  expect(facts.getByText("Token permissions")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("CLOUDFLARE_API_TOKEN_SRE")).toBeTruthy();
  expect(facts.getByText("Live, Cloudflare MCP server, one per agent")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
});

// A LEVEL THE CONNECTION CANNOT GRANT YET is offered, never as a choice that
// would only fail: disabled, with the engine's reason as its second line.
test("a Cloudflare level the provisioner cannot grant is offered disabled, saying why", () => {
  draw(cloudflareAnswer());
  fireEvent.click(screen.getByRole("combobox", { name: "SRE's Cloudflare access" }));
  const configure = screen.getByRole("option", { name: /Configure/ });
  expect(configure.getAttribute("aria-disabled")).toBe("true");
  expect(configure.textContent).toContain(
    "The Configure level is unavailable until the provisioner token holds Zone Settings: Edit and Response Compression: Edit",
  );
  expect(screen.getByRole("option", { name: /Operate/ }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

test("an off Cloudflare seat says turning it on mints a token limited to the company's zones", () => {
  draw(cloudflareAnswer());
  details("PM", "Cloudflare");
  expect(
    screen.getByText(
      "Off. Turning it on mints an account API token for the agent with the chosen permissions, limited to the company's zones.",
    ),
  ).toBeTruthy();
});

// A CLOUDFLARE TOKEN IS DELETED, NOT DISABLED: nothing is left at Cloudflare
// to turn back on, so the review says a new token is minted instead.
test("turning a Cloudflare seat off says the token is deleted and a new one minted on return", async () => {
  draw(cloudflareAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on Cloudflare" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      /Crewlet disables the agent's API token, so it stops working at once, then deletes it/,
    ),
  ).toBeTruthy();
});

test("rotating a Cloudflare token says it is rolled, and the old value stops working", async () => {
  draw(cloudflareAnswer());
  details("SRE", "Cloudflare");
  fireEvent.click(screen.getByRole("button", { name: "Rotate credential" }));
  const dialog = screen.getByRole("alertdialog", { name: "Rotate SRE's Cloudflare credential?" });
  expect(within(dialog).getByText(/rolls the token to a new value/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate credential" }));
  await waitFor(() =>
    expect(calls.act).toEqual([
      { kind: "cloudflare", handle: "sre", lane: "cloudflare", action: "rotate" },
    ]),
  );
});

// AN EXPO SEAT IS A ROBOT USER: the details name it, the role it holds in
// Expo's own word, the variable its token is sealed in and the CLI it drives.
test("an Expo seat's details name its robot, its role and the CLI it uses", () => {
  draw(expoAnswer());
  details("QA", "Expo");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="QA on Expo"]')!);
  expect(facts.getByText("crewlet-gophrase-qa")).toBeTruthy();
  expect(facts.getByText("Role at Expo")).toBeTruthy();
  expect(facts.getByText("Developer")).toBeTruthy();
  expect(facts.getByText("EXPO_TOKEN_QA")).toBeTruthy();
  expect(facts.getByText("Live, EAS CLI in its box, as EXPO_TOKEN")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
  expect(screen.queryByText("No Expo tool for its agents")).toBeNull();
});

// AN EXPO SEAT'S TOOL IS ITS BOX, NOT A SERVER: one whose sandbox env hands the
// EAS CLI nothing is told the line to add there, never to declare a server.
test("an Expo agent with no tool is told to set EXPO_TOKEN in its sandbox env", () => {
  draw(expoAnswer({ tool: "no_server" }));
  expect(screen.getByText("No Expo tool for its agents")).toBeTruthy();
  const advice = screen.getByText(/QA has an account at Expo and nothing to use it with/);
  expect(advice.textContent).toContain('Add EXPO_TOKEN: "${EXPO_TOKEN_QA}" to QA\'s sandbox.env');
  expect(advice.textContent).not.toMatch(/tool server/);
});

// AN EXPO ROBOT IS DELETED BY A PERSON: Crewlet drops the agent's token at
// once, and the review says the robot is the person's to delete at Expo.
test("turning an Expo seat off says the token goes at once and the robot is yours to delete", async () => {
  draw(expoAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Expo" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      /Crewlet deletes the agent's sealed token at once.*you delete its robot at Expo/,
    ),
  ).toBeTruthy();
});

// AN EXPO ROTATE IS A PASTE: the sheet opens at "Paste it here" for the robot
// the seat has, and the engine takes the paste as the rotate, so the rotate
// seat action is never called. Mutation: route rotate to the dialog again.
test("rotating an Expo token opens the sheet at the paste and never calls the rotate action", async () => {
  draw(expoAnswer());
  details("QA", "Expo");
  fireEvent.click(screen.getByRole("button", { name: "Rotate credential" }));
  expect(screen.queryByRole("alertdialog")).toBeNull();
  const sheet = screen.getByRole("dialog", { name: "New token for QA" });
  fireEvent.change(within(sheet).getByLabelText("Token"), {
    target: { value: "robot-token-for-tests" },
  });
  fireEvent.click(within(sheet).getByRole("button", { name: "Check and save" }));
  await waitFor(() =>
    expect(calls.credential).toEqual([
      { kind: "expo", handle: "qa", lane: "expo", value: "robot-token-for-tests" },
    ]),
  );
  expect(calls.act).toEqual([]);
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

// A ROW WITH NOTHING TO SAY HAS NOTHING UNDER IT. The message was handed to
// the table as an element, which is never null, so every row drew an empty
// accessory row (11px and a rule) beneath it. Mutation: hand the table the
// element again and the first count goes to three.
test("only a row with something waiting draws a row under it", () => {
  draw(datadogAnswer());
  expect(document.querySelectorAll(".crewlet-data-table__accessory-row")).toHaveLength(0);
  cleanup();
  draw(
    datadogAnswer({
      sync: {
        at: new Date().toISOString(),
        outcome: "failed",
        steps: [],
        error: {
          said: "Datadog refused the role change.",
          remedy: "Give that user User Access Manage at Datadog, then retry.",
          actor: "you",
          step: "rerole",
        },
      },
    }),
  );
  expect(document.querySelectorAll(".crewlet-data-table__accessory-row")).toHaveLength(1);
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
  // The engine now plans from the revision the colleague's save wrote.
  calls.planRevision = "rev-8";
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

// AN EXPO AGENT WAITS ON A PERSON FOR ITS ROBOT: the row says Needs you, and
// its one control opens the Set up sheet, which holds Expo's page and the
// paste, so the row offers no "Done, check again" a paste would make pointless.
// Mutation: drop the lane's handoff words and the row falls back to Open Expo.
test("an Expo agent waiting on its robot reads Needs you, with Set up opening the sheet", () => {
  draw(expoWaitingAnswer());
  expect(screen.getByText("Needs you")).toBeTruthy();
  expect(screen.getByText("Waiting on a person")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Done, check again" })).toBeNull();
  expect(screen.queryByRole("link", { name: /Open Expo/ })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Set up" }));
  expect(screen.getByRole("dialog", { name: "Set up PM on Expo" })).toBeTruthy();
});

// THE PASTE GOES TO THE ENGINE FOR THAT SEAT, and the tab draws the answer it
// returns, which is the seat in sync. Mutation: adopt nothing and the row
// still reads Needs you behind the sheet.
test("a pasted token is checked for its seat and the tab draws the engine's answer", async () => {
  const { onAnswer } = draw(expoWaitingAnswer("mint"));
  fireEvent.click(screen.getByRole("button", { name: "Paste a new token" }));
  const sheet = screen.getByRole("dialog", { name: "New token for PM" });
  fireEvent.click(within(sheet).getByRole("button", { name: "Continue" }));
  fireEvent.change(within(sheet).getByLabelText("Token"), {
    target: { value: "robot-token-for-tests" },
  });
  fireEvent.click(within(sheet).getByRole("button", { name: "Check and save" }));
  await waitFor(() =>
    expect(calls.credential).toEqual([
      { kind: "expo", handle: "pm", lane: "expo", value: "robot-token-for-tests" },
    ]),
  );
  await waitFor(() => expect(onAnswer).toHaveBeenCalled());
  const adopted = onAnswer.mock.calls[0]![0] as AccessAnswer;
  expect(adopted.revision).toBe("rev-8");
  expect(adopted.kind).toBe("expo");
  expect(within(sheet).getByText(/^Works\. Robot crewlet-gophrase-pm/)).toBeTruthy();
});

test("a role changed at Expo is fixed from the row's Fix the role", () => {
  draw(expoWaitingAnswer("rerole"));
  fireEvent.click(screen.getByRole("button", { name: "Fix the role" }));
  expect(screen.getByRole("dialog", { name: "PM's role on Expo" })).toBeTruthy();
});

// DELETING THE ROBOT AT EXPO IS THE PERSON'S, and nothing is pasted for it:
// the row names the step, links to Expo and checks again when it is done.
test("an Expo robot to delete at Expo is a plain wait with Open Expo, not the sheet", () => {
  const answer = expoWaitingAnswer("mint");
  const pm = answer.seats[0]!.lanes.expo!;
  pm.sync!.error = {
    ...pm.sync!.error!,
    step: "remove",
    said: "Delete robot crewlet-gophrase-pm at Expo",
  };
  draw(answer);
  expect(screen.getByText("Delete robot crewlet-gophrase-pm at Expo.")).toBeTruthy();
  expect(screen.getByRole("link", { name: /Open Expo/ })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Done, check again" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Set up|Paste/ })).toBeNull();
});

// EXPO'S TURN-ON IS MOSTLY A PERSON'S, and the review says so step by step:
// two steps marked with a person, Crewlet's check last, and the footer saying
// what the marks mean. Mutation: drop the person mark and the steps read as Crewlet's.
test("turning an Expo agent on reviews the robot and the token as a person's steps", async () => {
  calls.plans = [
    {
      handle: "pm",
      lane: "expo",
      steps: [
        {
          op: "create",
          verb: "create",
          by: "you",
          text: "Add a robot named `crewlet-gophrase-pm` with the Viewer role at Expo",
          action_url: "https://expo.dev/accounts/gophrase/settings/access-tokens",
        },
        {
          op: "create",
          verb: "mint",
          by: "you",
          text: "Create an access token for it, then paste it here",
        },
        {
          op: "create",
          verb: "mint",
          by: "crewlet",
          text: "Check the token and seal it as EXPO_TOKEN_PM",
        },
      ],
    },
  ];
  draw(expoAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "PM on Expo" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const dialog = await screen.findByRole("dialog", { name: "Review changes to Expo" });
  const steps = await within(dialog).findAllByRole("listitem");
  expect(steps.map((li) => li.textContent)).toEqual([
    "A person: Add a robot named crewlet-gophrase-pm with the Viewer role at Expo",
    "A person: Create an access token for it, then paste it here",
    "Crewlet: Check the token and seal it as EXPO_TOKEN_PM",
  ]);
  expect(within(steps[2]!).getByText("EXPO_TOKEN_PM").tagName).toBe("CODE");
  expect(
    within(dialog).getByText("Steps marked with a person need somebody at Expo."),
  ).toBeTruthy();
});

// AN OLD TOKEN THAT STILL WORKS IS ONE LINE ON THE ROW: what Expo shows for
// it and when it was made, Open Expo, and a check that reads it again; the
// wait that repeats it every pass is not said a second time. Mutation: drop
// the branch and the row falls back to the wait's sentence alone.
test("an Expo robot's old token still at Expo reads as one line with its label and date", () => {
  draw(expoRetiredAnswer());
  const under = screen
    .getByText("An old token still works at Expo")
    .closest(".agent-access-under")!;
  const line = within(under as HTMLElement).getByText("9f3c1a…");
  expect(line.tagName).toBe("CODE");
  expect(line.parentElement!.textContent).toMatch(/^9f3c1a…, created /);
  expect(
    within(under as HTMLElement).getByText(/Delete it at Expo\. Crewlet notices once it is gone\./),
  ).toBeTruthy();
  expect(
    within(under as HTMLElement)
      .getByRole("link", { name: /Open Expo/ })
      .getAttribute("href"),
  ).toBe("https://expo.dev/accounts/gophrase/settings/access-tokens");
  expect(
    within(under as HTMLElement).getByRole("button", { name: "Done, check again" }),
  ).toBeTruthy();
  expect(screen.queryByText(/Delete robot crewlet-gophrase-qa's other tokens/)).toBeNull();
});

test("several old tokens are counted, and a lane with no words of its own says credentials", () => {
  draw(expoRetiredAnswer(["9f3c1a…", "41be07…"]));
  expect(screen.getByText("2 old tokens still work at Expo")).toBeTruthy();
  expect(screen.getByText(/Delete them at Expo\./)).toBeTruthy();
  cleanup();
  const datadog = datadogAnswer();
  datadog.seats[2]!.lanes.datadog!.observed.retired = [{ label: "app-key-old" }];
  draw(datadog);
  expect(screen.getByText("An old credential still works at Datadog")).toBeTruthy();
  expect(screen.getByText("app-key-old").parentElement!.textContent).toBe("app-key-old");
});

// KEEPING THE ROBOT IS THE PERSON'S CHOICE on the wait to delete it at Expo,
// recorded by the engine. Mutation: drop the release words and it is gone.
test("a robot to delete at Expo can be kept there instead", async () => {
  const answer = expoWaitingAnswer("mint");
  const pm = answer.seats[0]!.lanes.expo!;
  pm.sync!.error = {
    ...pm.sync!.error!,
    step: "remove",
    said: "Delete robot crewlet-gophrase-pm at Expo",
  };
  draw(answer);
  fireEvent.click(screen.getByRole("button", { name: "Keep the robot at Expo" }));
  await waitFor(() =>
    expect(calls.act).toEqual([{ kind: "expo", handle: "pm", lane: "expo", action: "release" }]),
  );
});

test("keeping the robot is offered only on the wait to delete it", () => {
  draw(expoWaitingAnswer("mint"));
  expect(screen.queryByRole("button", { name: "Keep the robot at Expo" })).toBeNull();
});

// KEEPING THE ACCOUNT IS THE LANE'S TO ALLOW: a lane that says it is not
// releasable offers no way to keep it, whatever its words. Mutation: drop the
// lane's flag from the rule and the button comes back.
test("a lane that does not allow keeping the account offers no button to keep it", () => {
  const answer = expoWaitingAnswer("mint");
  answer.options = answer.options.map((o) => ({ ...o, releasable: false }));
  const pm = answer.seats[0]!.lanes.expo!;
  pm.sync!.error = {
    ...pm.sync!.error!,
    step: "remove",
    said: "Delete robot crewlet-gophrase-pm at Expo",
  };
  draw(answer);
  expect(screen.getByText("Delete robot crewlet-gophrase-pm at Expo.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Keep the/ })).toBeNull();
});

// A SCOPE ENTRY IS READ BY ITS NAME: where the vendor names an entry apart from
// the identifier stored (a project's ID), the picker labels it by name with the
// identifier beneath, and the review names it too.
test("a scope entry is picked by its name, with its identifier beneath, and the review names it", async () => {
  const data = jiraAnswer();
  data.options[0]!.scope_names = { ENG: "Engineering", OPS: "Operations" };
  draw(data);
  const picker = document.querySelector<HTMLElement>(`[aria-label="QA's Jira projects"]`)!;
  expect(picker.textContent).toContain("Engineering");
  pick(picker, /Operations/);
  expect(screen.getByRole("option", { name: /Operations/ }).textContent).toContain("OPS");
  fireEvent.keyDown(picker, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("option")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const review = await screen.findByRole("dialog");
  expect(within(review).getByText("Engineering and Operations")).toBeTruthy();
});

// AN OPEN CONNECTION LISTS NOT ALL IT REACHES (a GitHub installation on every
// repository, a GitLab group with no project list), so its picker takes a
// typed entry. The tab once keyed its scope on the list alone and never read
// scope_open, so neither drew a picker. Mutation: drop the open branch and
// both go red.
test.each([
  {
    vendor: "GitHub",
    answer: () => {
      const data = githubAnswer();
      data.options[0] = { ...data.options[0]!, scope: undefined, scope_open: true };
      return data;
    },
    field: "QA's GitHub repositories",
    typed: "gophrase/web",
    sent: { qa: { github: { scope: ["gophrase/backend", "gophrase/web"] } } },
  },
  {
    vendor: "GitLab",
    answer: gitlabAnswer,
    field: "Dev's GitLab projects",
    typed: "gophrase/infra",
    sent: { dev: { gitlab: { scope: ["gophrase/backend", "gophrase/infra"] } } },
  },
])("an open $vendor connection takes a typed entry", async ({ answer, field, typed, sent }) => {
  draw(answer());
  const input = screen.getByRole("combobox", { name: field });
  fireEvent.change(input, { target: { value: typed } });
  fireEvent.keyDown(input, { key: "Enter" });
  expect(screen.getByText("Not saved")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  expect(calls.plan[0]).toMatchObject({ seats: sent });
});

// A PICKER WITH NOTHING TO OFFER SAYS WHY in its place: the list could not be
// read, which says nothing about what exists, or it was read and holds none.
test("a scope list that could not be read, or holds nothing, says so in place of a picker", () => {
  const data = githubAnswer();
  data.seats[0]!.lanes.github!.desired.scope = [];
  data.options[0] = { ...data.options[0]!, scope: undefined, scope_live: false };
  const { redraw } = draw(data);
  expect(screen.getByText("Crewlet could not read your repositories")).toBeTruthy();
  expect(document.querySelector(`[aria-label="QA's GitHub repositories"]`)).toBeNull();
  redraw({ ...data, options: [{ ...data.options[0]!, scope_live: true }] });
  expect(screen.getByText("No repositories to choose from")).toBeTruthy();
});

// A LANE WHERE AN EMPTY SCOPE REACHES NOTHING holds an agent turned on with
// none at the review, and says under its row what to choose.
test("an agent turned on with no scope where none reaches nothing is held at the review", () => {
  const data = githubAnswer();
  data.options[0] = { ...data.options[0]!, scope_required: true };
  data.seats.push(seat("pm", "PM", { github: offLane() }));
  draw(data);
  fireEvent.click(screen.getByRole("switch", { name: "PM on GitHub" }));
  expect(
    screen.getByText(
      "Not in any repository yet, so it can reach nothing. Choose its repositories.",
    ),
  ).toBeTruthy();
  const review = screen.getByRole("button", { name: "Review changes" });
  expect(review.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(review);
  expect(screen.queryByRole("dialog")).toBeNull();
});

// THE ONLY ENTRY THERE IS is taken as a seat is turned on, where an empty scope
// reaches nothing and the list was read just now, so it shows in the review
// like any other change. A seat turned on anywhere else takes nothing.
test("an agent turned on where one entry is all there is takes it", async () => {
  const data = githubAnswer();
  data.options[0] = { ...data.options[0]!, scope: ["gophrase/backend"], scope_required: true };
  data.seats.push(seat("pm", "PM", { github: offLane() }));
  draw(data);
  fireEvent.click(screen.getByRole("switch", { name: "PM on GitHub" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  expect(calls.plan[0]).toMatchObject({
    seats: { pm: { github: { enabled: true, scope: ["gophrase/backend"] } } },
  });
  cleanup();
  calls.plan = [];
  draw({ ...data, options: [{ ...data.options[0]!, scope: ["a/b", "a/c"] }] });
  fireEvent.click(screen.getByRole("switch", { name: "PM on GitHub" }));
  expect(screen.getByRole("button", { name: "Review changes" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});

// A REFUSAL ABOUT ONE SEAT'S CONTROL closes the review and is said beside that
// control, which is marked refused and points at the sentence; a change to the
// seat clears it. Mutation: keep it in the review's banner and this goes red.
test("a refusal about one seat's projects closes the review and is said beside that picker", async () => {
  calls.planRefusal = {
    error: "access_not_offered",
    detail: "Choose at least one project for qa: an agent in no project can reach nothing",
    seat: "qa",
    lane: "jira",
    field: "scope",
  };
  draw(jiraAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "PM on Jira" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const said = await screen.findByText(
    "Choose at least one project for qa: an agent in no project can reach nothing.",
  );
  expect(said.getAttribute("role")).toBe("alert");
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const marked = document.querySelector<HTMLElement>(`[aria-describedby="${said.id}"]`)!;
  expect(marked.getAttribute("aria-invalid")).toBe("true");
  expect(marked.closest(`[aria-label="QA's Jira projects"]`) ?? marked).toBeTruthy();
  fireEvent.click(screen.getByRole("switch", { name: "QA on Jira" }));
  expect(screen.queryByText(/Choose at least one project for qa/)).toBeNull();
});

// A REFUSAL NAMING NO CONTROL stays in the review, where the save was asked for.
test("a refusal naming no control stays in the review", async () => {
  calls.planRefusal = { error: "access_not_offered", detail: "atlassian grants no lane forge" };
  draw(jiraAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "PM on Jira" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const review = await screen.findByRole("dialog");
  expect(await within(review).findByText("Atlassian grants no lane forge.")).toBeTruthy();
});

// WHAT CREWLET PUTS IN AN AGENT'S BOX ITSELF is named in its details, beside
// the lines a person adds, and only where it puts something there.
test("an agent's details name the settings Crewlet puts in its box, and only those", () => {
  const data = expoAnswer();
  const qa = data.seats.find((s) => s.handle === "qa")!.lanes.expo!;
  qa.box = {
    env: [
      ...qa.box!.env,
      { name: "EXPO_PUBLIC_ACCOUNT", value: "gophrase", by: "crewlet" },
      { name: "EAS_NO_VCS", value: "1", by: "crewlet" },
    ],
  };
  draw(data);
  details("QA", "Expo");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="QA on Expo"]')!);
  expect(facts.getByText("Set by Crewlet")).toBeTruthy();
  expect(facts.getByText("EXPO_PUBLIC_ACCOUNT and EAS_NO_VCS")).toBeTruthy();
  expect(facts.getByText("Live, EAS CLI in its box, as EXPO_TOKEN")).toBeTruthy();
  cleanup();
  draw(expoAnswer());
  details("QA", "Expo");
  expect(screen.queryByText("Set by Crewlet")).toBeNull();
});

// A GOOGLE CLOUD SEAT IS A SERVICE ACCOUNT WITH A TOKEN NOBODY HOLDS: the
// details name the account, the level, a token issued at use rather than a
// time no node shares, and what Crewlet sets in the box for gcloud, which the
// box image has to carry. There is nothing to rotate.
test("a Google Cloud seat's details name its account, an issued token and what Crewlet sets for gcloud", () => {
  draw(googleCloudAnswer());
  details("SRE", "Google Cloud");
  const list = document.querySelector<HTMLElement>('dl[aria-label="SRE on Google Cloud"]')!;
  const facts = within(list);
  expect(
    facts.getByText("crewlet-sre-3f9a@gophrase-crewlet-id.iam.gserviceaccount.com"),
  ).toBeTruthy();
  expect(facts.getByText("Roles on its projects")).toBeTruthy();
  expect(facts.getByText("Operator")).toBeTruthy();
  expect(facts.getByText("GOOGLE_CLOUD_ACCESS_TOKEN_SRE").parentElement!.textContent).toBe(
    "GOOGLE_CLOUD_ACCESS_TOKEN_SRE, issued for an hour at a time, at use",
  );
  expect(facts.getByText("Live, Google Cloud MCP servers, one set per agent")).toBeTruthy();
  const box = facts.getByText(
    "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE, CLOUDSDK_CORE_PROJECT and .config/crewlet/gcloud-token",
  );
  expect(box.parentElement!.textContent).toContain(
    ". gcloud token file provided; the Google Cloud CLI must be in the box image",
  );
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

// GOOGLE'S DELAY IS NOT A PERSON'S STEP: a grant Google has not applied yet
// reads as the tool waiting, in the engine's words, and never as Needs you.
test("a Google Cloud seat waiting on Google says so in its tool, never Needs you", () => {
  draw(googleCloudAnswer({ tool: "waiting", tool_reason: "Google is still applying the grant" }));
  expect(screen.queryByText("Needs you")).toBeNull();
  details("SRE", "Google Cloud");
  const facts = within(
    document.querySelector<HTMLElement>('dl[aria-label="SRE on Google Cloud"]')!,
  );
  expect(
    facts.getByText(
      "Waiting: Google is still applying the grant, Google Cloud MCP servers, one set per agent",
    ),
  ).toBeTruthy();
});

// A PROJECT IS NAMED AS GOOGLE NAMES IT, and a company with none set up for
// Crewlet is pointed at the docs section that sets one up.
test("a Google Cloud project reads by its name, and none set up links to preparing one", () => {
  const { redraw } = draw(googleCloudAnswer());
  expect(screen.getByText("GoPhrase Production")).toBeTruthy();
  const none = googleCloudAnswer(undefined, { scope: [], scope_names: {} });
  none.seats[1]!.lanes.google_cloud!.desired.scope = [];
  redraw(none);
  // EVERY ROW SAYS IT, the off one too, since its picker is where it would be.
  expect(screen.getAllByText(/No projects are set up for Crewlet yet/)).toHaveLength(2);
  for (const link of screen.getAllByRole("link", { name: /Prepare a project/ })) {
    expect(link.getAttribute("href")).toBe(
      "https://docs.crewlet.ai/engine/latest/integrations/google-cloud/#prepare-a-project-for-crewlet",
    );
  }
});

// TURNING A GOOGLE CLOUD SEAT OFF KEEPS ITS IDENTITY: the account is disabled
// and its roles removed, so it loses access at once and comes back as itself.
test("turning a Google Cloud seat off says the account is disabled and kept", async () => {
  draw(googleCloudAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on Google Cloud" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      /Crewlet disables the agent's service account, so it loses Google Cloud at once/,
    ),
  ).toBeTruthy();
});

// AN AWS SEAT IS AN IAM ROLE WITH CREDENTIALS NOBODY HOLDS: the details name
// the role, the level, credentials issued for 15 minutes at use, and the two
// files Crewlet puts in the box for the CLI's crewlet profile. Nothing rotates.
test("an AWS seat's details name its role, 15-minute credentials and the CLI's crewlet profile", () => {
  draw(awsAnswer());
  details("SRE", "AWS");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="SRE on AWS"]')!);
  expect(facts.getByText("crewlet/gophrase/seat/crewlet-sre-7c2e")).toBeTruthy();
  expect(facts.getByText("Permissions policies")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("AWS_CREDENTIALS_SRE").parentElement!.textContent).toBe(
    "AWS_CREDENTIALS_SRE, issued for 15 minutes at a time, at use",
  );
  expect(facts.getByText("Live, AWS MCP Server, one per agent")).toBeTruthy();
  const box = facts.getByText(".config/crewlet/aws-credentials.json and .aws/config");
  expect(box.parentElement!.textContent).toContain(
    ". AWS CLI configuration provided in ~/.aws/config as the profile crewlet, so commands use --profile crewlet",
  );
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

// AWS APPLYING A NEW ROLE IS NOT A PERSON'S STEP: it reads as the tool
// waiting, in the engine's words, and never as Needs you.
test("an AWS seat waiting on AWS says so in its tool, never Needs you", () => {
  draw(awsAnswer({ tool: "waiting", tool_reason: "AWS is still applying the change" }));
  expect(screen.queryByText("Needs you")).toBeNull();
  details("SRE", "AWS");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="SRE on AWS"]')!);
  expect(
    facts.getByText("Waiting: AWS is still applying the change, AWS MCP Server, one per agent"),
  ).toBeTruthy();
});

// TURNING AN AWS SEAT OFF DENIES IT EVERYTHING AND KEEPS ITS ROLE, so it stops
// within the time the live run measured and comes back as itself.
test("turning an AWS seat off says it is denied everything within about 2 minutes, role kept", async () => {
  draw(awsAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on AWS" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      /Crewlet attaches AWS's deny-all policy to the agent's role and detaches its level policies, so it stops within about 2 minutes of saving/,
    ),
  ).toBeTruthy();
});
