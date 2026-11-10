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

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { Profiler, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { PencilGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { AppAnnouncer } from "~/app/announcer.tsx";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
  type FramePage,
} from "~/app/assistant/context.ts";
import { Router } from "~/app/router.tsx";
import type { AccessAnswer, AccessSeatLane } from "~/contract/access.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { pick } from "~/testing.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { AgentsAccess } from "./AgentsAccess.tsx";
import {
  AccessDraftParts,
  AccessDraftPartsContext,
  useGatheredAccessDraft,
} from "./assistantDraft.ts";
import {
  awsAnswer,
  cloudflareAnswer,
  confluenceAnswer,
  datadogAnswer,
  digitaloceanAnswer,
  digitaloceanWaitingAnswer,
  expoAnswer,
  expoRetiredAnswer,
  expoWaitingAnswer,
  githubAnswer,
  gitlabAnswer,
  googleCloudAnswer,
  jiraAnswer,
  mattermostAnswer,
  neutralAnswer,
  offLane,
  onLane,
  scalewayAnswer,
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
  /** The agents the plan names to review their open work, where a case names them. */
  reviews: null as unknown[] | null,
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
          ...(calls.reviews ? { reviews: calls.reviews } : {}),
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
          verdict: {
            outcome: "works",
            summary: "Works. The token is sealed for this agent.",
            facts: [
              { label: "Robot", value: "crewlet-acme-pm" },
              { label: "Role", value: "Runner" },
              { label: "Sealed as", value: "EXPO_TOKEN_PM" },
            ],
          },
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
  calls.reviews = null;
  calls.stale = false;
  calls.planRevision = "rev-7";
  calls.plans = null;
  calls.planRefusal = null;
});
afterEach(cleanup);
afterEach(resetPageContextForTest);

/** The shapes a glyph draws, compared as drawn: the attributes are every glyph's alike. */
const shapes = (glyph: ReactElement) => {
  const drawn = document.createElement("div");
  drawn.innerHTML = renderToStaticMarkup(glyph);
  return drawn.firstElementChild!.innerHTML;
};
const PENCIL = shapes(<PencilGlyph />);
const PLUS = shapes(<PlusGlyph />);
const glyphIn = (el: Element) => el.querySelector("svg")?.innerHTML;
/** The grid cell a text is drawn in. */
const cellWith = (text: string) => screen.getByText(text).closest<HTMLElement>('[role="cell"]')!;

function draw(
  data: AccessAnswer,
  over: {
    held?: string;
    readOnly?: string;
    only?: string;
    avatarOf?: (handle: string) => AgentAvatar | null;
  } = {},
) {
  const reload = vi.fn();
  const onAnswer = vi.fn();
  // THE APP'S LIVE REGION, as the frame mounts it: the kit's selection bar
  // over the unsaved changes says its count ("One change not saved") through
  // it, and a sentence said with no region mounted is a warning the console
  // gate fails.
  const ui = (answer: AccessAnswer) => (
    <Router>
      <LayerHost>
        <AppAnnouncer />
        <AgentsAccess
          kind={answer.kind}
          name={laneWords(answer.options[0]!.lane).name}
          data={answer}
          reload={reload}
          onAnswer={onAnswer}
          held={over.held}
          readOnly={over.readOnly}
          only={over.only}
          avatarOf={over.avatarOf ?? (() => null)}
        />
      </LayerHost>
    </Router>
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

// THE OFF SENTENCE IS THE LANE'S, NOT THE SEAT'S: said once above the grid
// for every off agent, and an off row with no action opens nothing. Mutation:
// draw it in each row's details and it appears once per off agent.
test("what turning an off agent on does is said once, above the grid", () => {
  draw(datadogAnswer());
  expect(
    screen.getAllByText(
      "2 agents are off. Turning one on creates a Datadog service account with the chosen role and its own key.",
    ),
  ).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Show QA's Datadog details" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Rotate credential" })).toBeNull();
});

// AN OFF AGENT HAS NO PICKER: nothing can be chosen there, and a disabled
// picker with no value read as the kit's "Select an option". Mutation: draw
// the Select for an off seat and its trigger is back in the row.
test("an off agent's access is said in words, not as an empty picker", () => {
  draw(datadogAnswer({ desired: { enabled: true } }));
  expect(screen.queryByRole("combobox", { name: "QA's Datadog access" })).toBeNull();
  expect(screen.queryByText("Select an option")).toBeNull();
  expect(screen.getByRole("combobox", { name: "SRE's Datadog access" }).textContent).toContain(
    "Choose one",
  );
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

// THE REVIEW IS ONE CARD PER AGENT: the change as labels and values in one
// grid, so "On Datadog" and "Role at Datadog" line up, then the next sync's
// steps as a timeline whose rail keeps the order, each node a glyph rather
// than a number and no lead-in sentence above them. Crewlet's own steps carry
// no mark at all. Mutation: draw the steps as a numbered list again and the
// list is no timeline, its nodes numbers.
test("the review is a card per agent, its change as labels and values, then its steps unnumbered", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const steps = await screen.findByRole("list", { name: "What the next sync does at Datadog" });
  expect(steps.className).toContain("crewlet-timeline");
  expect(steps.className).toContain("crewlet-timeline--compact");
  expect(steps.className).not.toContain("crewlet-list--numbered");
  expect([...steps.querySelectorAll(".agent-access-step")].map((step) => step.textContent)).toEqual(
    [
      "Create the service account crewlet-qa@agents.crewlet.invalid",
      "Mint an application key, sealed as DATADOG_APP_KEY_QA",
    ],
  );
  const nodes = [...steps.querySelectorAll(".crewlet-timeline__node")];
  expect(nodes).toHaveLength(2);
  for (const node of nodes) {
    expect(node.textContent).toBe("");
    expect(node.querySelector("svg")?.getAttribute("class")).toContain("crewlet-glyph");
  }
  expect(within(steps).queryByText("You")).toBeNull();
  const review = screen.getByRole("region", { name: "QA on Datadog" });
  expect(review.className).toContain("crewlet-card");
  expect(within(review).getByRole("heading", { name: "QA" })).toBeTruthy();
  expect(within(review).getByText("@qa")).toBeTruthy();
  expect(within(review).getByText("Turned on")).toBeTruthy();
  const change = review.querySelector(".crewlet-change-list")!;
  expect(change.className).not.toContain("crewlet-change-list--inline");
  expect(
    [...change.querySelectorAll(".crewlet-change-list__label")].map((l) => l.textContent),
  ).toEqual(["On Datadog", "Role at Datadog"]);
  expect(screen.queryByText(/the next sync will:/)).toBeNull();
});

// AN AGENT IS DRAWN AS ITSELF: its card wears the face the tab's row wears,
// and its steps take that face's colour. One with no face in the chart draws
// initials and steps in the kit's own ink. Mutation: drop `avatarOf` from the
// review and the character and the hue both go.
test("an agent's card wears its own face, and its steps its own colour", async () => {
  const faces: Record<string, AgentAvatar> = { qa: { character: "hexlet", color: "cyan" } };
  draw(datadogAnswer(), { avatarOf: (handle) => faces[handle] ?? null });
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("switch", { name: "PM on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await screen.findAllByRole("list", { name: "What the next sync does at Datadog" });
  const qa = screen.getByRole("region", { name: "QA on Datadog" });
  const face = qa.querySelector(".crewlet-avatar")!;
  expect(face.className).toContain("crewlet-avatar--character");
  expect(face.className).toContain("crewlet-avatar--hue-cyan");
  expect(within(qa).getByRole("list").className).toContain("crewlet-timeline--hue-cyan");
  const pm = screen.getByRole("region", { name: "PM on Datadog" });
  expect(pm.querySelector(".crewlet-avatar")!.className).not.toContain("crewlet-avatar--character");
  expect(pm.querySelector(".crewlet-timeline")?.className ?? "").not.toContain(
    "crewlet-timeline--hued",
  );
});

// SEVERAL AGENTS ARE SEVERAL CARDS, in the tab's order, each saying its change
// in a word. The one turned off carries the danger edge, and each step that
// takes its access away takes the danger node; the revoke that retires a
// replaced key does not. Mutation: drop the rail and the off card reads like
// any other; mark every revoke and the retiring one turns red.
test("several agents are several cards, and the one turned off is drawn as danger", async () => {
  calls.plans = [
    {
      handle: "qa",
      lane: "datadog",
      steps: [
        { op: "create", verb: "create", by: "crewlet", text: "Create the service account" },
        { op: "create", verb: "mint", by: "crewlet", text: "Mint an application key" },
      ],
    },
    {
      handle: "sre",
      lane: "datadog",
      steps: [
        { op: "level", verb: "rerole", by: "crewlet", text: "Change its role" },
        { op: "level", verb: "revoke", by: "crewlet", text: "Delete the key it replaces" },
      ],
    },
    {
      handle: "pm",
      lane: "datadog",
      steps: [
        { op: "disable", verb: "disable", by: "crewlet", text: "Disable the service account" },
        { op: "revoke", verb: "revoke", by: "crewlet", text: "Delete its application key" },
      ],
    },
  ];
  // PM is on here, so the draft turns it off; QA turns on; SRE drops a level.
  const answer = datadogAnswer();
  answer.seats[0]!.lanes.datadog = onLane({
    variable: "DATADOG_APP_KEY_PM",
    desired: { enabled: true, level: "r-ro" },
  });
  draw(answer);
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("switch", { name: "PM on Datadog" }));
  pick(screen.getByRole("combobox", { name: "SRE's Datadog access" }), /Datadog Standard Role/);
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const dialog = await screen.findByRole("dialog", { name: "Review changes to Datadog" });
  await within(dialog).findAllByRole("list", { name: "What the next sync does at Datadog" });
  const cards = within(dialog).getAllByRole("region");
  expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual([
    "PM on Datadog",
    "QA on Datadog",
    "SRE on Datadog",
  ]);
  const word = (card: HTMLElement) =>
    card.querySelector(".agent-access-review-change")?.textContent;
  const states = (card: HTMLElement) =>
    [...card.querySelectorAll(".crewlet-timeline__item")].map((i) => i.getAttribute("data-state"));
  const [pm, qa, sre] = cards as [HTMLElement, HTMLElement, HTMLElement];
  expect(word(pm)).toBe("Turned off");
  expect(pm.className).toContain("crewlet-card--rail-danger");
  expect(states(pm)).toEqual(["danger", "danger"]);
  expect(within(pm).getByText(/Crewlet disables the service account/)).toBeTruthy();
  expect(word(qa)).toBe("Turned on");
  expect(qa.className).not.toContain("crewlet-card--rail-danger");
  expect(states(qa)).toEqual(["default", "default"]);
  expect(word(sre)).toBe("Level changed");
  expect(sre.className).not.toContain("crewlet-card--rail-danger");
  expect(states(sre)).toEqual(["default", "default"]);
});

// A VARIABLE A STEP NAMES IS CODE, the name a reader looks up on the Secrets
// screen. Mutation: render the step's text as plain text and the name is
// prose again.
test("a variable a step names is drawn as code", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  const steps = await screen.findByRole("list", { name: "What the next sync does at Datadog" });
  expect(within(steps).getByText("DATADOG_APP_KEY_QA").tagName).toBe("CODE");
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
  expect(facts.getByText("crewlet-acme-sre")).toBeTruthy();
  expect(facts.getByText("Token permissions")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("CLOUDFLARE_API_TOKEN_SRE")).toBeTruthy();
  expect(facts.getByText("Live, Cloudflare MCP server, one per agent")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
});

// A LEVEL THE CONNECTION CANNOT GRANT YET is offered, never as a choice that
// would only fail: disabled, with the engine's reason as its second line in
// place of its note. Every other level carries the engine's note.
test("a Cloudflare level the provisioner cannot grant is offered disabled, saying why", () => {
  draw(cloudflareAnswer());
  fireEvent.click(screen.getByRole("combobox", { name: "SRE's Cloudflare access" }));
  const levels = ["Read", "Operate", "Configure", "Secure"];
  const options = screen.getAllByRole("option");
  expect(options.map((o) => levels.find((l) => o.textContent?.startsWith(l)))).toEqual(levels);
  const option = (name: string) => options.find((o) => o.textContent?.startsWith(name))!;
  const [read, configure, secure] = [option("Read"), option("Configure"), option("Secure")];
  expect(secure.getAttribute("aria-disabled")).toBe("true");
  expect(secure.textContent).toContain(
    "The Secure level is unavailable until the provisioner token holds Zone WAF: Edit and Account Filter Lists: Edit",
  );
  expect(secure.textContent).not.toContain("Everything Configure does");
  expect(read.textContent).toContain("firewall and WAF rules, IP lists");
  expect(configure.getAttribute("aria-disabled")).not.toBe("true");
});

// EVERY LEVEL BEFORE IT SCROLLS. A level is a name over a note of up to four
// lines, where the kit sizes its panel for six one-line rows, so Cloudflare's
// picker showed two levels of four. jsdom lays nothing out, so the cascade over
// the panel the picker opens is what is read.
test("the level picker's panel is tall enough for every level and its note", () => {
  const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
  const sheets = [
    read("node_modules/@crewlethq/ui/dist/styles.css"),
    read("src/styles/access.css"),
  ].map((text) => {
    const style = document.createElement("style");
    style.textContent = text;
    document.head.append(style);
    return style;
  });
  try {
    draw(cloudflareAnswer());
    fireEvent.click(screen.getByRole("combobox", { name: "SRE's Cloudflare access" }));
    const panel = screen.getAllByRole("option")[0]!.closest<HTMLElement>(".agent-access-menu");
    expect(panel).not.toBeNull();
    expect(
      getComputedStyle(panel!).getPropertyValue("--crewlet-select-menu-max-height").trim(),
    ).toBe("24rem");
  } finally {
    for (const s of sheets) s.remove();
  }
});

test("an off Cloudflare seat says turning it on mints a token limited to the company's zones", () => {
  draw(cloudflareAnswer());
  expect(
    screen.getByText(
      /Turning (it|one) on mints an account API token for the agent with the chosen permissions, limited to the company's zones\./,
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
  expect(facts.getByText("crewlet-acme-qa")).toBeTruthy();
  expect(facts.getByText("Role at Expo")).toBeTruthy();
  expect(facts.getByText("Developer")).toBeTruthy();
  expect(facts.getByText("EXPO_TOKEN_QA")).toBeTruthy();
  expect(facts.getByText("Live, EAS CLI in its box, as EXPO_TOKEN")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
  expect(screen.queryByText("No Expo tool for its agents")).toBeNull();
});

// AN EXPO SEAT'S TOOL IS ITS BOX, which Crewlet fills: a seat the engine still
// reports without a tool is told about a tool server alone, never a line to add.
test("an Expo agent with no tool is never told to add a sandbox env line", () => {
  draw(expoAnswer({ tool: "no_server" }));
  expect(screen.getByText("No Expo tool for its agents")).toBeTruthy();
  const advice = screen.getByText(/QA has an account at Expo and nothing to use it with/);
  expect(advice.textContent).not.toMatch(/sandbox/);
  expect(advice.textContent).not.toContain("EXPO_TOKEN");
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

// THE SAVE SAYS WHO WILL REVIEW WHAT. An agent whose access the draft changes,
// with open work raised under its access now, is named with its count before
// the save, as the tab names it. Mutation: drop the callout and the review says
// nothing of the questions the change makes stale.
test("the review names each agent that will review its open work after the save", async () => {
  calls.reviews = [{ handle: "qa", open: 3, changes: ["Datadog: its role is now Read Only."] }];
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      "3 open items from QA were raised under its current access; it will review them after you save.",
    ),
  ).toBeTruthy();
});

test("a plan naming nobody to review says nothing of open work", async () => {
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await screen.findByText("Create the service account crewlet-qa@agents.crewlet.invalid");
  expect(screen.queryByText("Open work to review")).toBeNull();
});

// A ROW WITH NOTHING TO SAY HAS NOTHING UNDER IT. The message was handed to
// the table as an element, which is never null, so every row drew an empty
// accessory row (11px and a rule) beneath it. Mutation: hand the table the
// element again and the first count goes to three.
test("only a row with something waiting draws a row under it", () => {
  draw(datadogAnswer());
  expect(document.querySelectorAll(".crewlet-data-grid__subrow--accessory")).toHaveLength(0);
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
  expect(document.querySelectorAll(".crewlet-data-grid__subrow--accessory")).toHaveLength(1);
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
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub permissions" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's GitHub permissions" });
  const contents = within(dialog).getByRole("radiogroup", { name: "QA's Contents permission" });
  fireEvent.click(within(contents).getByRole("radio", { name: "Read and write" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  expect(screen.getByText("Not saved")).toBeTruthy();
});

// PERMISSIONS READ AS A VALUE WITH A PENCIL, the dashboard's mark for a value
// that is changed in place, so a reader sees they can be edited; a read-only
// tab draws the count alone. Mutation: drop the pencil, or draw it read-only.
test("an agent's permissions are a count beside a pencil that edits them, and read-only draws neither", () => {
  draw(githubAnswer());
  expect(screen.getByText("6 permissions")).toBeTruthy();
  const edit = screen.getByRole("button", { name: "Edit QA's GitHub permissions" });
  expect(glyphIn(edit)).toBe(PENCIL);
  expect(edit.getAttribute("title")).toContain("Issues: Read and write");
  expect((edit as HTMLButtonElement).disabled).toBe(false);
  cleanup();
  draw(githubAnswer(), { readOnly: OPERATOR_MANAGED });
  expect(screen.getByText("6 permissions")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /permissions$/ })).toBeNull();
  const table = screen.getByRole("table", { name: "Agents on GitHub" });
  expect([...table.querySelectorAll("svg")].map((g) => g.innerHTML)).not.toContain(PENCIL);
});

// THE PENCIL IS HELD WHERE THE COUNT'S BUTTON WAS: an agent that is off, one
// that has left, and a tab nobody here may change.
test("the permissions pencil is held for an agent that is off or gone, and on a held tab", () => {
  const data = githubAnswer();
  data.seats.push(seat("pm", "PM", { github: offLane() }));
  data.seats.push({
    ...seat("ops", "Ops", {
      github: onLane({
        variable: "GITHUB_APP_KEY_OPS",
        desired: { enabled: true, permissions: { contents: "read" } },
      }),
    }),
    departed: true,
  });
  draw(data);
  const held = (who: string) =>
    (screen.getByRole("button", { name: `Edit ${who}'s GitHub permissions` }) as HTMLButtonElement)
      .disabled;
  expect(held("QA")).toBe(false);
  expect(held("PM")).toBe(true);
  expect(held("Ops")).toBe(true);
  cleanup();
  draw(githubAnswer(), { held: "Admins change an agent's access." });
  expect(held("QA")).toBe(true);
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
  expect(screen.queryByRole("button", { name: /PM's Jira projects$/ })).toBeNull();
  expect(screen.getByRole("button", { name: "Edit QA's Jira projects" })).toBeTruthy();
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

// THE GRID IS THE PRODUCT'S DATA GRID, as Schedules is: named, with sortable
// Agent and Status heads over a list the client holds whole, and a chevron on
// a row with something to open. Mutation: back on the DataTable, there is no
// `.crewlet-data-grid` and no head sorts.
test("the agents are the product's data grid, sortable by agent and status", () => {
  draw(datadogAnswer());
  const table = screen.getByRole("table", { name: "Agents on Datadog" });
  expect(
    table.closest(".crewlet-data-grid") ?? table.querySelector(".crewlet-data-grid__head"),
  ).toBeTruthy();
  const headers = within(table)
    .getAllByRole("columnheader")
    .map((h) => h.textContent ?? "");
  for (const label of ["Agent", "On", "Access", "Status"])
    expect(headers.some((h) => h.includes(label))).toBe(true);
  expect(within(table).getByRole("button", { name: /Agent/ })).toBeTruthy();
  expect(within(table).getByRole("button", { name: /Status/ })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Show SRE's Datadog details" })).toBeTruthy();
});

// AN EXPO AGENT WAITS ON A PERSON FOR ITS ROBOT: the row says Needs you, and
// its one control opens the Set up sheet, which holds Expo's page and the
// paste, so the row offers no "Done, check again" a paste would make pointless.
// Mutation: drop the lane's handoff words and the row falls back to Open Expo.
// A DIGITALOCEAN TOKEN ACTS AS THE PERSON WHO MADE IT: the details say whose
// it is and never the engine's fingerprint of it, and its tool is either the
// MCP server or doctl in the box. Mutation: draw `account_id` as the account.
test("a DigitalOcean seat's details say whose token it is, its level and both routes to its tool", () => {
  draw(digitaloceanAnswer());
  details("SRE", "DigitalOcean");
  const facts = within(
    document.querySelector<HTMLElement>('dl[aria-label="SRE on DigitalOcean"]')!,
  );
  expect(facts.getByText("Alex on acme")).toBeTruthy();
  expect(facts.getByText("Token scopes")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("DIGITALOCEAN_TOKEN_SRE")).toBeTruthy();
  expect(
    facts.getByText(
      "Live, the DigitalOcean MCP server, or doctl in its box, as DIGITALOCEAN_ACCESS_TOKEN",
    ),
  ).toBeTruthy();
  expect(document.body.textContent).not.toContain("hmac-sre");
});

// NO TOOL IS A MISSING SERVER: doctl's token is Crewlet's to set, so the MCP
// server is the one remedy. Mutation: drop tool_server and the server goes unsaid.
test("a DigitalOcean agent with no tool is offered the MCP server", () => {
  draw(digitaloceanAnswer({ tool: "no_server" }));
  expect(screen.getByText("No DigitalOcean tool for its agents")).toBeTruthy();
  const advice = screen.getByText(/SRE has an account at DigitalOcean and nothing to use it with/);
  expect(advice.textContent).toContain("declares no digitalocean tool server. Add it");
  expect(advice.textContent).not.toMatch(/sandbox/);
});

// CREWLET REVOKES A DIGITALOCEAN TOKEN ITSELF, so turning an agent off asks
// the person for nothing. Mutation: Expo's words here and the review says to
// delete something at the vendor.
test("turning a DigitalOcean seat off says Crewlet revokes its token at once", async () => {
  draw(digitaloceanAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on DigitalOcean" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      /Crewlet revokes the agent's token at DigitalOcean, so it stops working at once/,
    ),
  ).toBeTruthy();
});

test("a DigitalOcean agent waiting on its token reads Needs you, with Set up opening the sheet", () => {
  draw(digitaloceanWaitingAnswer());
  expect(screen.getByText("Needs you")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Set up" }));
  expect(screen.getByRole("dialog", { name: "Set up PM on DigitalOcean" })).toBeTruthy();
});

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
  expect(within(sheet).getByText("Works. The token is sealed for this agent.")).toBeTruthy();
  expect(within(sheet).getByText("crewlet-acme-pm")).toBeTruthy();
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
    said: "Delete robot crewlet-acme-pm at Expo",
  };
  draw(answer);
  expect(screen.getByText("Delete robot crewlet-acme-pm at Expo.")).toBeTruthy();
  expect(screen.getByRole("link", { name: /Open Expo/ })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Done, check again" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Set up|Paste/ })).toBeNull();
});

// EXPO'S TURN-ON IS MOSTLY A PERSON'S, and the review says so step by step:
// two steps tagged You, Crewlet's check last with no tag, and the footer
// saying what the tag means. Mutation: drop the tag and the steps read as Crewlet's.
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
          text: "Add a robot named `crewlet-acme-pm` with the Viewer role at Expo",
          action_url: "https://expo.dev/accounts/acme/settings/access-tokens",
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
  const words = (li: HTMLElement) => li.querySelector(".agent-access-step")?.textContent;
  expect(steps.map(words)).toEqual([
    "Add a robot named crewlet-acme-pm with the Viewer role at Expo" + "You",
    "Create an access token for it, then paste it here" + "You",
    "Check the token and seal it as EXPO_TOKEN_PM",
  ]);
  expect(within(steps[0]!).getByText("You").closest(".crewlet-tag")).toBeTruthy();
  expect(within(steps[2]!).getByText("EXPO_TOKEN_PM").tagName).toBe("CODE");
  expect(within(dialog).getByText("Steps marked You need somebody at Expo.")).toBeTruthy();
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
  ).toBe("https://expo.dev/accounts/acme/settings/access-tokens");
  expect(
    within(under as HTMLElement).getByRole("button", { name: "Done, check again" }),
  ).toBeTruthy();
  expect(screen.queryByText(/Delete robot crewlet-acme-qa's other tokens/)).toBeNull();
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
    said: "Delete robot crewlet-acme-pm at Expo",
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
    said: "Delete robot crewlet-acme-pm at Expo",
  };
  draw(answer);
  expect(screen.getByText("Delete robot crewlet-acme-pm at Expo.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Keep the/ })).toBeNull();
});

// A SCOPE READS AS ITS CHOICE, NEVER A FIELD: a chip with no remove, or the
// lane's words for none, beside a pencil where entries are chosen and a plus
// where none is. Mutation: draw the inline picker again, or swap the glyphs.
test("an agent's repositories read as chips beside a pencil, or the lane's words beside a plus", () => {
  const data = githubAnswer();
  data.seats.push(
    seat("pm", "PM", {
      github: onLane({
        variable: "GITHUB_APP_KEY_PM",
        desired: { enabled: true, permissions: { contents: "read" } },
      }),
    }),
  );
  draw(data);
  const edit = screen.getByRole("button", { name: "Edit QA's GitHub repositories" });
  expect(within(cellWith("acme/backend")).getAllByRole("button")).toEqual([edit]);
  expect(glyphIn(edit)).toBe(PENCIL);
  expect(screen.getByText("All installed repositories")).toBeTruthy();
  expect(glyphIn(screen.getByRole("button", { name: "Choose PM's GitHub repositories" }))).toBe(
    PLUS,
  );
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByRole("checkbox")).toBeNull();
});

// THE DIALOG TICKS FROM WHAT THE INSTALLATION REACHES, and what it changes
// waits in the unsaved bar for the review like any other change.
test("the repositories dialog lists what the installation reaches, and a choice waits for review", async () => {
  draw(githubAnswer());
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub repositories" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's GitHub repositories" });
  expect(within(dialog).getAllByRole("checkbox")).toHaveLength(3);
  const box = (name: string) => within(dialog).getByRole("checkbox", { name }) as HTMLInputElement;
  expect(box("acme/backend").checked).toBe(true);
  expect(box("acme/frontend").checked).toBe(false);
  // WHAT IS TICKED IS COUNTED on the choice, and focus starts on it.
  const chosen = within(dialog).getByRole("radio", { name: /^Chosen repositories,\s*1 chosen$/ });
  expect((chosen as HTMLInputElement).checked).toBe(true);
  await waitFor(() => expect(document.activeElement).toBe(chosen));
  // A SHORT LIST IS READ WHOLE, with nothing to search.
  expect(within(dialog).queryByRole("searchbox")).toBeNull();
  fireEvent.click(box("acme/frontend"));
  expect(within(dialog).getByRole("radio", { name: /^Chosen repositories,\s*2 chosen$/ })).toBe(
    chosen,
  );
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.getByText("Not saved")).toBeTruthy();
  // ONE CHIP AND A COUNT, which names the rest.
  const more = within(cellWith("acme/backend")).getByText("1 more repository");
  expect(more.closest("[title]")?.getAttribute("title")).toBe("acme/frontend");
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  expect(calls.plan[0]).toMatchObject({
    seats: { qa: { github: { scope: ["acme/backend", "acme/frontend"] } } },
  });
});

// ALL INSTALLED REPOSITORIES IS A CHOICE OF ITS OWN, and choosing it clears
// the picks: the row then reads it beside a plus, and the review sends none.
test("choosing all installed repositories clears the picks", async () => {
  draw(githubAnswer());
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub repositories" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's GitHub repositories" });
  fireEvent.click(within(dialog).getByRole("radio", { name: "All installed repositories" }));
  expect(within(dialog).queryByRole("checkbox")).toBeNull();
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(screen.getByText("All installed repositories")).toBeTruthy();
  const choose = screen.getByRole("button", { name: "Choose QA's GitHub repositories" });
  expect(glyphIn(choose)).toBe(PLUS);
  // OPENED AGAIN, focus starts on the choice that holds, not the one under it.
  fireEvent.click(choose);
  const again = await screen.findByRole("dialog", { name: "QA's GitHub repositories" });
  const all = within(again).getByRole("radio", { name: "All installed repositories" });
  expect((all as HTMLInputElement).checked).toBe(true);
  await waitFor(() => expect(document.activeElement).toBe(all));
  fireEvent.click(within(again).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  expect(calls.plan[0]).toMatchObject({ seats: { qa: { github: { scope: [] } } } });
});

// A LONG LIST IS SEARCHED, and a connection that lists its reach offers
// nothing outside it: a name it does not list matches nothing and cannot be
// added. Mutation: offer the add on a closed lane and this goes red.
test("a long repository list is searched, and offers only what the installation reaches", async () => {
  const data = githubAnswer();
  const many = Array.from({ length: 12 }, (_, i) => `example/service-${i + 1}`);
  data.options[0] = { ...data.options[0]!, scope: ["acme/backend", ...many] };
  draw(data);
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub repositories" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's GitHub repositories" });
  expect(within(dialog).getAllByRole("checkbox")).toHaveLength(13);
  const find = within(dialog).getByRole("searchbox", { name: "Find a repository" });
  await waitFor(() => expect(document.activeElement).toBe(find));
  fireEvent.change(find, { target: { value: "service-1" } });
  expect(within(dialog).getAllByRole("checkbox")).toHaveLength(4);
  fireEvent.change(find, { target: { value: "example/tools" } });
  expect(within(dialog).queryByRole("checkbox")).toBeNull();
  expect(within(dialog).getByText("No repository matches “example/tools”.")).toBeTruthy();
  expect(within(dialog).queryByRole("button", { name: /^Add/ })).toBeNull();
});

// THE CONTROL IS HELD WHERE THE INLINE FIELD WAS: an agent that is off or
// gone, and a held tab; a read-only tab draws the choice alone.
test("the repositories control is held for an agent that is off or gone, on a held tab, and absent read-only", () => {
  const data = githubAnswer();
  data.seats.push(seat("pm", "PM", { github: offLane() }));
  data.seats.push({
    ...seat("ops", "Ops", {
      github: onLane({
        variable: "GITHUB_APP_KEY_OPS",
        desired: {
          enabled: true,
          permissions: { contents: "read" },
          scope: ["acme/mobile-app"],
        },
      }),
    }),
    departed: true,
  });
  draw(data);
  const held = (name: string) =>
    (screen.getByRole("button", { name }) as HTMLButtonElement).disabled;
  expect(held("Edit QA's GitHub repositories")).toBe(false);
  expect(held("Choose PM's GitHub repositories")).toBe(true);
  expect(held("Edit Ops's GitHub repositories")).toBe(true);
  cleanup();
  draw(githubAnswer(), { held: "Admins change an agent's access." });
  expect(held("Edit QA's GitHub repositories")).toBe(true);
  cleanup();
  draw(githubAnswer(), { readOnly: OPERATOR_MANAGED });
  expect(within(cellWith("acme/backend")).queryByRole("button")).toBeNull();
  expect(screen.queryByRole("button", { name: /GitHub repositories$/ })).toBeNull();
});

// WHERE NO ENTRY REACHES NOTHING the empty list is no choice to offer, so the
// dialog is the list alone, named by the lane's word, and says what is needed.
test("a Scaleway agent's projects are the list alone, with no empty choice", async () => {
  draw(scalewayAnswer());
  fireEvent.click(screen.getByRole("button", { name: "Edit SRE's Scaleway projects" }));
  const dialog = await screen.findByRole("dialog", { name: "SRE's Scaleway projects" });
  expect(within(dialog).queryByRole("radio")).toBeNull();
  const list = within(dialog).getByRole("group", { name: "Projects" });
  expect(within(list).getAllByRole("checkbox").length).toBeGreaterThan(0);
});

// A SCOPE ENTRY IS READ BY ITS NAME: where the vendor names an entry apart from
// the identifier stored (a project's ID), the picker labels it by name with the
// identifier beneath, and the review names it too.
test("a scope entry is picked by its name, with its identifier beneath, and the review names it", async () => {
  const data = jiraAnswer();
  data.options[0]!.scope_names = { ENG: "Engineering", OPS: "Operations" };
  draw(data);
  expect(cellWith("Engineering")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's Jira projects" }));
  const dialog = await screen.findByRole("dialog", { name: "QA's Jira projects" });
  // THE LANE'S OWN WORDS for an empty list and a chosen one.
  expect(within(dialog).getByRole("radio", { name: "No project" })).toBeTruthy();
  expect(within(dialog).getByRole("radio", { name: /^Chosen projects,\s*1 chosen$/ })).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("checkbox", { name: "Operations", description: "OPS" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
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
    who: "QA's GitHub repositories",
    typed: "acme/web",
    sent: { qa: { github: { scope: ["acme/backend", "acme/web"] } } },
  },
  {
    vendor: "GitLab",
    answer: gitlabAnswer,
    who: "Dev's GitLab projects",
    typed: "acme/infra",
    sent: { dev: { gitlab: { scope: ["acme/backend", "acme/infra"] } } },
  },
])("an open $vendor connection takes a typed entry", async ({ answer, who, typed, sent }) => {
  draw(answer());
  fireEvent.click(screen.getByRole("button", { name: `Edit ${who}` }));
  const dialog = await screen.findByRole("dialog", { name: who });
  const find = within(dialog).getByRole("searchbox", { name: /^Find or add a / });
  fireEvent.change(find, { target: { value: typed } });
  fireEvent.click(within(dialog).getByRole("button", { name: `Add “${typed}”` }));
  expect((within(dialog).getByRole("checkbox", { name: typed }) as HTMLInputElement).checked).toBe(
    true,
  );
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
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
  expect(screen.queryByRole("button", { name: /QA's GitHub repositories$/ })).toBeNull();
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
  data.options[0] = { ...data.options[0]!, scope: ["acme/backend"], scope_required: true };
  data.seats.push(seat("pm", "PM", { github: offLane() }));
  draw(data);
  fireEvent.click(screen.getByRole("switch", { name: "PM on GitHub" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  expect(calls.plan[0]).toMatchObject({
    seats: { pm: { github: { enabled: true, scope: ["acme/backend"] } } },
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
  const edit = screen.getByRole("button", { name: "Edit QA's Jira projects" });
  expect(edit.getAttribute("aria-describedby")).toBe(said.id);
  // THE DIALOG SAYS IT TOO, over the list it is about.
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "QA's Jira projects" });
  expect(
    within(dialog).getByText(
      "Choose at least one project for qa: an agent in no project can reach nothing.",
    ),
  ).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
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

// WHAT ELSE CREWLET PUTS IN AN AGENT'S BOX is named in its details, beside the
// credentials its tool reads, and only where it puts something there.
test("an agent's details name the settings Crewlet puts in its box, and only those", () => {
  const data = expoAnswer();
  const qa = data.seats.find((s) => s.handle === "qa")!.lanes.expo!;
  qa.box = {
    env: [
      ...qa.box!.env,
      { name: "EXPO_PUBLIC_ACCOUNT", value: "acme" },
      { name: "EAS_NO_VCS", value: "1" },
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

// A CREDENTIAL ISSUED AT USE, A TOOL WAITING ON THE VENDOR AND A FILE IN THE
// BOX each read in the details as what they are: the token by how it is
// issued, the wait in the engine's words, and the file beside the variables
// Crewlet sets.
test("an agent's details read an issued token, the engine's wait and the files Crewlet sets", () => {
  draw(neutralAnswer({ tool: "waiting", tool_reason: "the vendor is still applying the grant" }));
  expect(screen.queryByText("Needs you")).toBeNull();
  details("SRE", "the vendor");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="SRE on the vendor"]')!);
  expect(facts.getByText("VENDOR_TOKEN_SRE").parentElement!.textContent).toBe(
    "VENDOR_TOKEN_SRE, issued at use",
  );
  expect(
    facts.getByText(
      "Waiting: the vendor is still applying the grant, Its tool server, one per agent",
    ),
  ).toBeTruthy();
  expect(facts.getByText("VENDOR_TOKEN_FILE and .config/crewlet/token")).toBeTruthy();
});

// A SCALEWAY SEAT IS AN IAM APPLICATION: the details name it, its level under
// Scaleway's word for what it holds, the key it is sealed in, its tool by both
// routes, and the settings Crewlet puts in its box.
test("a Scaleway seat's details name its application, its permission sets, its key and its tools", () => {
  draw(scalewayAnswer());
  details("SRE", "Scaleway");
  const facts = within(document.querySelector<HTMLElement>('dl[aria-label="SRE on Scaleway"]')!);
  expect(facts.getByText("crewlet-acme-sre")).toBeTruthy();
  expect(facts.getByText("Permission sets")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("SCALEWAY_SECRET_KEY_SRE")).toBeTruthy();
  expect(
    facts.getByText(
      "Live, the Scaleway MCP server, or scw in its box, as SCW_ACCESS_KEY and SCW_SECRET_KEY",
    ),
  ).toBeTruthy();
  expect(
    facts.getByText(
      "SCW_DEFAULT_ORGANIZATION_ID, SCW_DEFAULT_PROJECT_ID and SCW_DISABLE_CHECK_VERSION",
    ),
  ).toBeTruthy();
  expect(screen.getByRole("button", { name: "Rotate credential" })).toBeTruthy();
});

// A SEAT'S PROJECTS ARE PICKED BY NAME, stored by ID: the picker reads the
// project's name, never its UUID.
test("a Scaleway seat's projects read by name", () => {
  draw(scalewayAnswer());
  const chips = cellWith("acme-prod");
  expect(chips.textContent).not.toContain("11111111");
});

// NO PROJECT REACHES NOTHING: turning a Scaleway agent on with none holds the
// review, in the engine's words, and says under its row what to choose.
test("a Scaleway agent turned on with no project is held at the review", () => {
  draw(scalewayAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "PM on Scaleway" }));
  expect(
    screen.getByText("Not in any project yet, so it can reach nothing. Choose its projects."),
  ).toBeTruthy();
  const review = screen.getByRole("button", { name: "Review changes" });
  expect(review.getAttribute("aria-disabled")).toBe("true");
});

// A SCALEWAY AGENT WITH NO TOOL is told about the server alone, never the key
// or the settings Crewlet sets in its box.
test("a Scaleway agent with no tool is told the server alone", () => {
  draw(scalewayAnswer({ tool: "no_server" }));
  const advice = screen.getByText(/SRE has an account at Scaleway and nothing to use it with/);
  expect(advice.textContent).toContain("the company declares no scaleway tool server");
  expect(advice.textContent).not.toMatch(/SCW_/);
});

// TURNING A SCALEWAY AGENT OFF deletes its key at once, then its policy and
// application, and the review says so before the save.
test("turning a Scaleway seat off says the key goes at once, then its policy and application", async () => {
  draw(scalewayAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "SRE on Scaleway" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(
    await screen.findByText(
      "Crewlet deletes the agent's API key, so it stops working at once, then its policy and IAM application. Turning it back on creates new ones.",
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
  expect(facts.getByText("crewlet/acme/seat/crewlet-sre-7c2e")).toBeTruthy();
  expect(facts.getByText("Permissions policies")).toBeTruthy();
  expect(facts.getByText("Operate")).toBeTruthy();
  expect(facts.getByText("AWS_CREDENTIALS_SRE").parentElement!.textContent).toBe(
    "AWS_CREDENTIALS_SRE, issued for 15 minutes at a time, at use",
  );
  expect(facts.getByText("Live, AWS MCP Server, one per agent")).toBeTruthy();
  const box = facts.getByText(".aws/config and .aws/crewlet-credentials.json");
  expect(box.parentElement!.textContent).toBe(
    ".aws/config and .aws/crewlet-credentials.json. AWS CLI configuration provided in ~/.aws/config as the profile crewlet, so commands use --profile crewlet; the AWS CLI v2 must be in the box image",
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

// A GOOGLE CLOUD SEAT IS A SERVICE ACCOUNT WITH A TOKEN NOBODY HOLDS: the
// details name the account, the level, a token issued at use rather than a
// time no node shares, and what Crewlet sets in the box for gcloud, which the
// box image has to carry. There is nothing to rotate.
test("a Google Cloud seat's details name its account, an issued token and what Crewlet sets for gcloud", () => {
  draw(googleCloudAnswer());
  details("SRE", "Google Cloud");
  const list = document.querySelector<HTMLElement>('dl[aria-label="SRE on Google Cloud"]')!;
  const facts = within(list);
  expect(facts.getByText("crewlet-sre-3f9a@acme-crewlet-id.iam.gserviceaccount.com")).toBeTruthy();
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
  expect(screen.getByText("Acme Production")).toBeTruthy();
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

// AN IDLE TABLE DOES NOT RE-RENDER AS THE CLOCK TICKS. It read the clock at
// the top, so every switch and picker was drawn again once a second for times
// printed in one line of an expanded row. Mutation: read `useNow()` in
// AgentsAccess again and the commits count the ticks.
test("an idle access table does not re-render as the clock ticks", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  try {
    let commits = 0;
    render(
      <Router>
        <LayerHost>
          <Profiler id="access" onRender={() => commits++}>
            <AgentsAccess
              kind="datadog"
              name="Datadog"
              data={datadogAnswer()}
              reload={() => {}}
              held={undefined}
              avatarOf={() => null}
            />
          </Profiler>
        </LayerHost>
      </Router>,
    );
    await afterMicrotasks();
    const settled = commits;
    await act(async () => {
      vi.advanceTimersByTime(5_000);
    });
    expect(commits).toBe(settled);
  } finally {
    vi.useRealTimers();
  }
});

/** The integration's page, as the frame publishes it under the assistant. */
const INTEGRATION_PAGE: FramePage = {
  screen: "integration",
  title: "Settings › Integrations › Datadog",
  path: ["company", "integrations", "datadog"],
  params: {},
  query: {},
};

/** Apply a change Ask Crewlet proposed to the page's draft, as an approval does. */
function propose(values: Record<string, unknown>) {
  const context = pageSnapshot()!.context;
  let result: ReturnType<typeof applyPageAction> | undefined;
  act(() => {
    result = applyPageAction({
      correlation_id: "c1",
      action: "draft.set",
      payload: { values },
      source_revision: context.action_revisions!["draft.set"]!,
    });
  });
  return result;
}

// THE TAB'S DRAFT IS ASK CREWLET'S TO READ: every control a field, and the
// row not saved yet first. A read-only tab, or one held for this reader, has
// no draft to tell. Mutation: declare nothing, or declare a held tab's.
test("tells Ask Crewlet the tab's draft, the row not saved yet first, and nothing where it is held", () => {
  setFramePage(INTEGRATION_PAGE);
  draw(datadogAnswer());
  fireEvent.click(screen.getByRole("switch", { name: "QA on Datadog" }));
  const context = pageSnapshot()!.context;
  expect(context.draft?.fields.slice(0, 2).map((f) => [f.path, f.value])).toEqual([
    ["agent_2.datadog.enabled", "On"],
    ["agent_2.datadog.level", "Datadog Read Only Role"],
  ]);
  expect(context.accepts).toContain("draft.set");
  cleanup();
  draw(datadogAnswer(), { readOnly: OPERATOR_MANAGED });
  expect(pageSnapshot()!.context.draft).toBeUndefined();
  cleanup();
  draw(datadogAnswer(), { held: "Admins change an agent's access." });
  expect(pageSnapshot()!.context.draft).toBeUndefined();
});

// A PROPOSAL IS THE DRAFT THE DIALOGS MAKE: Workflows to Read and write and
// acme/frontend added, once through the permissions and repositories dialogs
// and once as a change Ask Crewlet proposes, reach the review as one draft,
// and the change saves nothing by itself. Mutation: land a proposal by any
// other way than the update path the dialogs call, and the two differ.
test("a change Ask Crewlet proposes reaches the review as the dialogs' own change", async () => {
  setFramePage(INTEGRATION_PAGE);
  draw(githubAnswer());
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub permissions" }));
  let dialog = await screen.findByRole("dialog", { name: "QA's GitHub permissions" });
  const workflows = within(dialog).getByRole("radiogroup", { name: "QA's Workflows permission" });
  fireEvent.click(within(workflows).getByRole("radio", { name: "Read and write" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Edit QA's GitHub repositories" }));
  dialog = await screen.findByRole("dialog", { name: "QA's GitHub repositories" });
  fireEvent.click(within(dialog).getByRole("checkbox", { name: "acme/frontend" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(1));
  cleanup();

  draw(githubAnswer());
  expect(
    propose({
      "agent_1.github.permission.workflows": "Read and write",
      "agent_1.github.scope": ["acme/backend", "acme/frontend"],
    }),
  ).toMatchObject({ outcome: "applied" });
  expect(screen.getByText("Not saved")).toBeTruthy();
  expect(calls.save).toEqual([]);
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  await waitFor(() => expect(calls.plan).toHaveLength(2));
  expect(calls.plan[1]).toEqual(calls.plan[0]);
  expect(calls.plan[0]).toMatchObject({
    seats: {
      qa: {
        github: {
          permissions: { workflows: "write" },
          scope: ["acme/backend", "acme/frontend"],
        },
      },
    },
  });
});

// AN AGENT'S OWN PAGE DRAWS A TAB PER INTEGRATION AND DECLARES ONE DRAFT:
// each tab puts its own into the page's, and declares none of its own, so
// Ask Crewlet reads both rather than whichever tab registered last, and a
// change lands in the tab it names. Mutation: let each tab declare its own,
// or gather none, and one integration's fields are gone; let a gathered tab
// declare its own as well, and a read of a newer revision, which moves that
// tab's own declaration and not the page's, puts it in the page's place.
test("an agent's own page tells Ask Crewlet every tab's draft as one", () => {
  setFramePage({ ...INTEGRATION_PAGE, screen: "seat", path: ["company", "org", "seats", "sre"] });
  const parts = new AccessDraftParts();
  function Gathered() {
    useGatheredAccessDraft(parts, "SRE");
    return null;
  }
  const tabFor = (data: AccessAnswer, name: string) => (
    <AgentsAccess
      kind={data.kind}
      name={name}
      data={data}
      reload={() => {}}
      held={undefined}
      avatarOf={() => null}
      only="sre"
    />
  );
  const page = (datadog: AccessAnswer) => (
    <Router>
      <LayerHost>
        <AppAnnouncer />
        <AccessDraftPartsContext.Provider value={parts}>
          <Gathered />
          {tabFor(datadog, "Datadog")}
          {tabFor(cloudflareAnswer(), "Cloudflare")}
        </AccessDraftPartsContext.Provider>
      </LayerHost>
    </Router>
  );
  const { rerender } = render(page(datadogAnswer()));
  // ONE AGENT, SO ONE PLACE, and each product's key its own integration's.
  const both = [
    "agent_1.datadog.enabled",
    "agent_1.datadog.level",
    "agent_1.cloudflare.enabled",
    "agent_1.cloudflare.level",
  ];
  expect(pageSnapshot()!.context.draft?.fields.map((f) => f.path)).toEqual(both);
  rerender(page({ ...datadogAnswer(), revision: "rev-8" }));
  expect(pageSnapshot()!.context.draft?.fields.map((f) => f.path)).toEqual(both);
  expect(propose({ "agent_1.cloudflare.level": "Configure" })).toMatchObject({
    outcome: "applied",
  });
  expect(screen.getAllByText("Not saved")).toHaveLength(1);
  expect(
    within(screen.getByRole("table", { name: "Access on Cloudflare" })).getByText("Not saved"),
  ).toBeTruthy();
});
