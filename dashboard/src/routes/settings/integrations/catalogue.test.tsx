/**
 * What the reconcile status may and may not claim.
 *
 * This screen's whole posture is that it never invents health: an idle
 * integration and a broken one look identical in the traffic counters, so a
 * green dot derived from anything but a real finding is reporting the weather.
 * The reconcile block is the first thing on the screen that CAN make a health
 * claim, which is exactly why what it does with an absent or unreadable
 * answer is worth pinning.
 */

import { existsSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import {
  CATALOG,
  IN_FLIGHT,
  disconnectOrder,
  docHref,
  entryState,
  phaseTone,
} from "./catalogue.tsx";
import {
  INTEGRATION_TOOLS,
  type IntegrationRow,
  type IntegrationTool,
  type IntegrationToolState,
} from "~/contract/integrations.ts";
import { repoRoot } from "~/test/engineFiles.ts";

afterEach(cleanup);

// THE TONE NAMES ARE THE DESIGN SYSTEM'S HERE AND BELOW — `success`,
// `warning`, `danger` where this screen used to spell them `positive`,
// `caution`, `critical`. What is asserted is unchanged: which phases may read
// as healthy, and which of the two unhealthy steps each one takes. `Tag` has
// no variant under the old names, so a stale spelling would draw the neutral
// pill and say nothing about it — which is why these are assertions rather
// than a convention.
//
// A phase a newer node wrote must never read as healthy. Claiming a surface
// is fine on the strength of a word this build cannot interpret is the one
// answer that is certainly wrong.
test("an unknown phase is never positive", () => {
  expect(phaseTone("something_a_newer_build_wrote")).not.toBe("success");
  expect(phaseTone("")).not.toBe("success");
  expect(phaseTone("ready")).toBe("success");
});

// Degraded is a warning rather than a danger, and the distinction is real:
// agents are still working, which is what separates it from a surface that
// cannot authenticate at all.
test("the tone follows how much is not working", () => {
  expect(phaseTone("degraded")).toBe("warning");
  expect(phaseTone("unconfigured")).toBe("danger");
  // EVERY IN-PROGRESS PHASE IS AMBER. Each one is an integration that does
  // not work yet, and neutral is the tone this screen uses for a state
  // nobody needs to come back to.
  for (const phase of ["provisioning", "activating", "awaiting_admin", "disconnecting"]) {
    expect(phaseTone(phase)).toBe("warning");
  }
});

// --- one row per tool ----------------------------------------------------- //

const atlassian = CATALOG.find((e) => e.key === "atlassian")!;
const slack = CATALOG.find((e) => e.key === "slack")!;

function rowsOf(...rows: IntegrationRow[]): Map<string, IntegrationRow> {
  return new Map(rows.map((r) => [r.key, r]));
}

/**
 * The engine's roll-up for one tool, as the answer's `tools` carries it.
 *
 * THE RULES ARE NOT TESTED HERE. Which surface wins, when a ready phase still
 * needs a person and what a surface no pass converges reads as are
 * `integration.Rollup`'s, pinned by `TestTheRollupTable` in Go; what this file
 * holds is what the screen does with the answer.
 */
function rolled(
  key: keyof typeof INTEGRATION_TOOLS,
  state: IntegrationToolState,
  label: string,
  reason = "",
): IntegrationTool[] {
  return [{ key, surfaces: [...INTEGRATION_TOOLS[key]], state, label, reason }];
}

// THE CARD'S WORD IS THE ENGINE'S. The label arrives decided, and the screen
// neither re-derives it from the rows nor overrides it.
test("a card's tag is the engine's roll-up label", () => {
  const state = entryState(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
    rolled("atlassian", "attention", "Action required", "swe has no Jira account"),
  );
  expect(state.tag).toBe("Action required");
  expect(state.tone).toBe("warning");
  expect(state.reason).toBe("swe has no Jira account");
});

// NOT CONFIGURED, PAUSED and CONNECTING are three different facts. Absent is
// nobody set it up, paused is somebody switched it off on purpose, and the
// two used to collapse into the state most likely to be mistaken for a
// mistake.
test("absent, paused and connecting are told apart", () => {
  // THE ENGINE'S WORD, NEUTRAL AND OUTLINED: a tile in a grid with no pill
  // under its name is a hole a reader has to explain, and neutral is the one
  // register here that reports nothing wrong.
  const absent = entryState(slack, rowsOf(), rolled("slack", "not_in_use", "Not in use"));
  expect([absent.tag, absent.tone, absent.outline]).toEqual(["Not in use", "neutral", true]);
  // AND A ROLL-UP STILL DESCRIBING A SURFACE THE ROWS NO LONGER CARRY draws
  // nothing: the rows are the fresher half, and the tile's action is Connect.
  expect(entryState(slack, rowsOf(), rolled("slack", "connected", "Connected")).tag).toBe("");
  const paused = entryState(
    slack,
    rowsOf({ key: "slack", configured: true, enabled: false }),
    rolled("slack", "not_in_use", "Paused"),
  );
  expect([paused.tag, paused.tone, paused.outline]).toEqual(["Paused", "neutral", true]);
  // Configured with no report behind it is NOT connected, and a card in
  // motion is amber and offers nothing.
  const connecting = entryState(
    slack,
    rowsOf({ key: "slack", configured: true, enabled: true }),
    rolled("slack", "not_connected", "Connecting"),
  );
  expect([connecting.tag, connecting.tone, connecting.busy]).toEqual([
    "Connecting",
    "warning",
    true,
  ]);
});

// CONNECTED IS ONLY EVER GREEN, and green is only ever connected: a word and a
// colour saying opposite things leave a reader to guess which to believe.
test("only a connected tool is drawn green", () => {
  const rows = rowsOf({ key: "github", configured: true });
  const github = CATALOG.find((e) => e.key === "github")!;
  for (const state of ["attention", "not_connected", "connected", "not_in_use"] as const) {
    const drawn = entryState(github, rows, rolled("github", state, "x"));
    expect([state, drawn.tone === "success"]).toEqual([state, state === "connected"]);
  }
});

// AN ANSWER WITH NO ROLL-UP INVENTS NOTHING. A configured tool the answer did
// not judge is drawn as unknown and offers nothing, rather than as whatever a
// second classifier here would have guessed.
test("a configured tool with no roll-up is not given a state", () => {
  const state = entryState(atlassian, rowsOf({ key: "jira", configured: true }), []);
  expect(state.tag).toBe("Status unavailable");
  expect(state.tone).not.toBe("success");
  expect(state.busy).toBe(true);
});

// THE CATALOGUE IS THE ENGINE'S GROUPING. Every card is one tool of
// `INTEGRATION_TOOLS`, in its surface order, so the header's roll-up and the
// body's surfaces are about the same things.
test("every card is one engine tool with its surfaces", () => {
  expect(CATALOG.map((e) => e.key).sort()).toEqual(Object.keys(INTEGRATION_TOOLS).sort());
  for (const entry of CATALOG) {
    expect(entry.surfaces.map((s) => s.key)).toEqual([
      ...INTEGRATION_TOOLS[entry.key as keyof typeof INTEGRATION_TOOLS],
    ]);
  }
});

// LEARN MORE OPENS A PAGE THE SITE BUILDS. The address is composed from the
// entry's `doc`, so no grep for a URL can see it: a tool whose page is named
// wrongly, or a page renamed under the catalogue, is a 404 the reader meets
// and nothing else would. The site publishes `docs/<path>.md` at
// `/engine/<version>/<path>/`, every page a directory, so the slashless
// spelling is a redirect on every click.
test("every Learn more names an engine page, spelled as the site serves it", () => {
  for (const entry of CATALOG) {
    const page = join(repoRoot(), "docs", "integrations", `${entry.doc}.md`);
    expect(existsSync(page), `${entry.key}: ${page}`).toBe(true);
    expect(docHref(entry)).toBe(`https://docs.crewlet.ai/engine/latest/integrations/${entry.doc}/`);
  }
});

// A TEARDOWN IS AMBER AND OFFERS NOTHING, the same as every other state the
// engine is working through. It was the one in-flight state drawn neutral on
// the card while its own surface drew amber below it.
test("a disconnect in progress offers nothing", () => {
  const going = entryState(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({
      key: "datadog",
      configured: true,
      reconcile: { phase: "ready", disconnecting: true },
    }),
    rolled("datadog", "not_connected", "Disconnecting"),
  );
  expect([going.tag, going.tone, going.busy]).toEqual(["Disconnecting", "warning", true]);
});

// --- the action slot -------------------------------------------------------- //

import { asSentence } from "~/lib/format.ts";
import {
  actionFor,
  deliveryEvent,
  formBlocked,
  sectionsFor,
  ROTATE_FINDINGS,
} from "./catalogue.tsx";
import type { Entry } from "./catalogue.tsx";
import type { SetupToolState } from "~/protocol/types.ts";
import type { ReconcileFinding } from "~/contract/integrations.ts";

function toolState(over: Partial<SetupToolState>): SetupToolState {
  return {
    key: "github",
    configured: true,
    enabled: true,
    satisfied: true,
    form_complete: true,
    requirements: [],
    ...over,
  };
}

const expiring: ReconcileFinding = {
  kind: "credential_expiring",
  subject: "admin_token",
  detail: "The GitLab admin token expires in 3 days.",
  expires_at: "2026-10-02T00:00:00Z",
};

/** The one action, by kind: what a tile offers, from (state, listing, rows, findings). */
function kindOf(...args: Parameters<typeof actionFor>): string {
  return actionFor(...args).kind;
}

// EVERY TILE HAS EXACTLY ONE ACTION, and which one is a pure function of what
// the engine says. There is no null: a tile that offered nothing sent a reader
// looking for the control that would tell them what to do, and a working tool
// still has somewhere to go — its own page.
test("the action follows the state", () => {
  const ready = entryState(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
    rolled("atlassian", "connected", "Connected"),
  );
  // NOTHING CONFIGURED: Connect.
  expect(kindOf(ready, [toolState({ configured: false })], false, [], false)).toBe("connect");
  // A FORM LEFT UNFINISHED: Continue.
  expect(
    kindOf(ready, [toolState({ satisfied: false, form_complete: false })], true, [], false),
  ).toBe("continue");
  // A WORKING TOOL: Manage, which goes to its page and writes nothing.
  expect(kindOf(ready, [toolState({})], true, [], false)).toBe("manage");
  // A FAULT A NEW CREDENTIAL DOES NOT FIX is still Manage: what is wrong and
  // where to fix it are on the page, beside the settings the square opens.
  const owed = entryState(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "degraded", actor: "admin", detail: "x" },
    }),
    rolled("atlassian", "attention", "Action required", "x"),
  );
  expect(kindOf(owed, [toolState({})], true, [], false)).toBe("manage");
});

// A CREDENTIAL THE VENDOR REFUSED OR IS ABOUT TO IS ROTATE TOKEN, and it
// outranks an unfinished form: it lapses on a date whether or not anybody
// looks, and a half-connected Atlassian has no date on it.
test("an expiring or refused credential is rotated", () => {
  const owed = entryState(
    CATALOG.find((e) => e.key === "gitlab")!,
    rowsOf({
      key: "gitlab",
      configured: true,
      reconcile: { phase: "ready", findings: [expiring] },
    }),
    rolled("gitlab", "attention", "Credential expiring", expiring.detail),
  );
  const both = [toolState({ key: "gitlab", form_complete: false, satisfied: false })];
  expect(kindOf(owed, both, true, [expiring], false)).toBe("rotate");
  const refused = { ...expiring, kind: "credential_rejected", expires_at: undefined };
  expect(kindOf(owed, both, true, [refused], false)).toBe("rotate");
  // AND ONLY THOSE TWO: a MISSING credential has nothing held to rotate, and
  // the empty field is what Continue opens.
  expect([...ROTATE_FINDINGS].sort()).toEqual(["credential_expiring", "credential_rejected"]);
  expect(kindOf(owed, both, true, [], false)).toBe("continue");
});

// THE ROWS ARE THE FRESHER HALF, IN BOTH DIRECTIONS.
//
// The two halves of this screen arrive separately, and the rows are the
// quicker answer to "does this company have this". A disconnect the listing
// has not caught up with still offers Connect; a connect it has not caught up
// with offers Manage, never a Connect or a Continue beside a tag reading
// Connected.
test("a tile the rows say is gone offers to connect it again", () => {
  const github = CATALOG.find((e) => e.key === "github")!;
  const gone = entryState(github, rowsOf(), rolled("github", "not_in_use", "Not in use"));
  expect(kindOf(gone, [toolState({ key: "github", configured: true })], false, [], false)).toBe(
    "connect",
  );
  const fresh = entryState(
    github,
    rowsOf({ key: "github", configured: true }),
    rolled("github", "connected", "Connected"),
  );
  expect(kindOf(fresh, [toolState({ key: "github", configured: false })], true, [], false)).toBe(
    "manage",
  );
});

// A CARD IN MOTION OFFERS NOTHING TO WRITE.
//
// Between a connect and the loop's first report, and between asking for a
// disconnect and its finishing, nothing a person writes moves the tile — a
// Continue beside "Connecting" invited somebody to act on a card whose state
// was about to change under them. Manage writes nothing, and where the tool is
// going is exactly what they want to watch.
test("a tile the engine is mid-flight on offers only its page", () => {
  const connecting = entryState(
    atlassian,
    rowsOf({ key: "atlassian", configured: true }),
    rolled("atlassian", "not_connected", "Connecting"),
  );
  expect(connecting.tag).toBe("Connecting");
  expect(
    kindOf(
      connecting,
      [toolState({ key: "atlassian" }), toolState({ configured: false })],
      true,
      [expiring],
      false,
    ),
  ).toBe("manage");
  const going = entryState(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready", disconnecting: true } }),
    rolled("atlassian", "not_connected", "Disconnecting"),
  );
  expect(kindOf(going, [toolState({ configured: false })], true, [], false)).toBe("manage");
});

// A TOOL IS COMPLETE ONLY WHEN EVERY CONFIGURED SURFACE IS, and a surface
// nobody configured on a partly-connected tool is something to Continue:
// connecting the Atlassian organization alone left the card with nothing on
// screen that would add the products.
test("one unfinished surface makes the whole tool unfinished", () => {
  const ready = entryState(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready" } }),
    rolled("atlassian", "connected", "Connected"),
  );
  const tools = (confluence: Partial<SetupToolState>) => [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", ...confluence }),
  ];
  expect(
    kindOf(
      ready,
      tools({ configured: true, satisfied: false, form_complete: false }),
      true,
      [],
      false,
    ),
  ).toBe("continue");
  expect(
    kindOf(
      ready,
      tools({ configured: false, satisfied: false, form_complete: false }),
      true,
      [],
      false,
    ),
  ).toBe("continue");
  expect(kindOf(ready, tools({ configured: true, satisfied: true }), true, [], false)).toBe(
    "manage",
  );
});

// A TOOL WITH NO FORM IS LEARN MORE, AND A REFUSED FORM IS STILL CONNECT.
//
// Where this build answers no setup state for a tool and nothing refused the
// read, there is no form a Connect could open, and the page that says how is
// its documentation. Where the read was REFUSED, the form exists and the
// reader lacks the grants — so the action stays Connect, disabled with the
// sentence that names them (a write control is never hidden).
test("a tool with no form behind it is learned about, not connected", () => {
  const nothing = entryState(slack, rowsOf(), rolled("slack", "not_in_use", "Not in use"));
  expect(kindOf(nothing, [], false, [], false)).toBe("learn");
  expect(kindOf(nothing, [], false, [], true)).toBe("connect");
  expect(formBlocked([], true)).toBe(
    "Setting up integrations needs Admin access. An admin can give it to you.",
  );
  expect(formBlocked([], false)).toMatch(/no form to open/);
  expect(formBlocked([{}], true)).toBeUndefined();
});

// THE ENGINE'S REASONS ARE WRITTEN TO BE EMBEDDED, lower-case and without a
// full stop, so a tile that leads with one makes it a sentence: the shared
// `asSentence`, which every other screen printing an engine clause uses. A
// copy of its own here capitalised and stopped there, so a tile's sentence
// ended without the full stop every description beside it carries.
test("a reason leads a tile as a sentence", () => {
  expect(asSentence("the third-party app refused this integration's credential")).toBe(
    "The third-party app refused this integration's credential.",
  );
  expect(asSentence("")).toBe("");
});

// A DELIVERY ROW SHOWS WHAT THE PROVIDER CALLED IT, whichever edge it came in
// on: the engine files a row under its edge's prefix and the provider's own
// event name, and the operator is matching the name against their console.
test("a delivery's event drops the engine's filing prefix", () => {
  expect(deliveryEvent("webhook:push")).toBe("push");
  expect(deliveryEvent("forge:avi:jira:created:issue")).toBe("avi:jira:created:issue");
  expect(deliveryEvent("socket:posted")).toBe("posted");
});

// --- a per-seat vendor ------------------------------------------------------ //

// SLACK GETS A SECTION PER SEAT, because each agent has its own app and so its
// own bot token. A seat with no app yet is exactly the seat somebody opened
// this dialog to give one to, so it is listed too.
test("a per-seat vendor becomes one section per agent", () => {
  const slackTool = toolState({
    key: "slack",
    configured: true,
    requirements: [],
    // WHAT MAKES IT PER-SEAT. Every app carries a roster now; this flag is
    // the difference between a list of agents to read and a form per agent.
    seats_required: true,
    seats: [
      { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
      { handle: "cto", name: "CTO", requirements: [], satisfied: false },
    ],
  });
  const sections = sectionsFor(slack, new Map([["slack", slackTool]]));
  expect(sections.map((s) => s.seat)).toEqual(["sre-lead", "cto"]);
  expect(sections.map((s) => s.name)).toEqual(["SRE Lead", "CTO"]);
});

// AND AN INFORMATIONAL ROSTER DOES NOT. A Datadog seat holds a credential
// written in its mcp_env, which this dialog does not edit: splitting the form
// into one section per agent gave every one of them a copy of the company's
// own fields to fill in.
test("an informational roster stays one section", () => {
  const datadog = CATALOG.find((e) => e.key === "datadog")!;
  const tool = toolState({
    key: "datadog",
    configured: true,
    seats: [
      { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true },
      { handle: "cto", name: "CTO", requirements: [], satisfied: false },
    ],
  });
  const sections = sectionsFor(datadog, new Map([["datadog", tool]]));
  expect(sections.map((s) => s.name)).toEqual(["Datadog"]);
  expect(sections.map((s) => s.seat)).toEqual([undefined]);
});

// A company whose Slack block exists and whose seats hold no app is not
// connected: nothing can post.
// A PER-SEAT APP'S WORK IS ON THE AGENT'S ROW, not on the card.
//
// This asserted a Continue button, which opens the company form: a form that
// can neither create an agent's app nor install it, because both are clicks
// at the third-party app on the agent's own row. It was a control leading
// nowhere on the one card with real work outstanding. The card still SAYS so,
// through the tag its reconcile phase drives.
test("a per-seat app leaves the work to the agent's own row", () => {
  // SETTLED, because this is about what the action IS. A card between a
  // connect and the loop's first report offers nothing at all, which is a
  // different rule with its own test.
  const state = entryState(
    slack,
    rowsOf({ key: "slack", configured: true, reconcile: { phase: "ready" } }),
    rolled("slack", "connected", "Connected"),
  );
  // GITHUB'S SHAPE: unfinished, and with nothing left to type. Both acts that
  // produce an agent's app happen at GitHub, from that agent's own row, so a
  // GitHub seat carries no requirements and the dialog has no box a Continue
  // button could take anybody to.
  const action = actionFor(
    state,
    [
      toolState({
        key: "github",
        configured: true,
        satisfied: false,
        form_complete: true,
        seats_required: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false, enrolled: true }],
      }),
    ],
    true,
    [],
    false,
  );
  // MANAGE: the page, where the agent's own row carries its Create button.
  expect(action.kind).toBe("manage");

  // AND SLACK'S: unfinished because a seat's own credential is unanswered,
  // which is a box in this very dialog. Suppressing the button there would
  // leave the agent's app with no way to be filled in at all.
  const typeable = actionFor(
    state,
    [
      toolState({
        key: "slack",
        configured: true,
        satisfied: false,
        form_complete: false,
        seats_required: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false }],
      }),
    ],
    true,
    [],
    false,
  );
  expect(typeable.label).toBe("Continue");

  // AND AN INFORMATIONAL ROSTER IS NOT WORK OUTSTANDING. Every app lists its
  // agents now, and most of those credentials are an upgrade on an app that
  // already works: reading any roster as unfinished put a Continue button on
  // every connected card.
  const informational = actionFor(
    state,
    [
      toolState({
        key: "datadog",
        configured: true,
        satisfied: true,
        seats: [{ handle: "cto", requirements: [], satisfied: false }],
      }),
    ],
    true,
    [],
    false,
  );
  expect(informational.kind).toBe("manage");
});

// --- what a card offers, and what it no longer does ------------------------ //

// DISCONNECT MEANS THE CARD, and the provisioner goes last.
//
// It took the surface that happened to be listed first: on Atlassian the
// organization's account was deleted and its block removed while Jira and
// Confluence were left untouched, under a card still reading Connected. And
// the order is not arbitrary — the organization's credential is what removes
// the accounts, so taking it first would strand what the products still hold.
test("disconnect takes every configured surface, provisioner last", () => {
  rowsOf(
    { key: "atlassian", configured: true },
    { key: "jira", configured: true },
    { key: "confluence", configured: true },
  );
  const sections = [
    // can_provision IS TRUE ON ALL THREE, because the engine registers a
    // pass for each. Saying false for the products is the fixture that let
    // this function ship ordering by a field that cannot discriminate.
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
  ];
  // The catalogue's dependency order reversed. What this asserts is the LAST
  // position; the two products are peers and either order between them takes
  // the same things away.
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual([
    "jira",
    "confluence",
    "atlassian",
  ]);
});

// THE FORGE RELAY IS NOT A SURFACE ANYTHING CAN DISCONNECT, and it used to be
// the first one this asked to.
//
// It is an ingress PATH for Jira and Confluence Cloud rather than a kind:
// `integration.Kinds` does not contain it, so `DELETE /setup/integrations/forge`
// answers 404 — and the setup payload, built from the same set, offers it no
// tool state at all. This function also admitted anything with a TRAFFIC row,
// and the relay has one on every Cloud company. So `forge` sorted ahead of the
// three real surfaces (nothing gave it a section, so it fell in the
// non-provisioning half) and the dialog aborted on its very first request,
// having touched nothing.
test("disconnect leaves out the relay, which is not a kind anything can delete", () => {
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    // NO SECTION FOR forge, which is what the engine actually serves: the
    // setup payload is built from integration.Kinds and the relay is not one.
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order).not.toContain("forge");
  expect(order).toEqual(["jira", "confluence", "atlassian"]);
});

// THE ORGANIZATION GOES LAST EVEN THOUGH EVERY SURFACE PROVISIONS, which is
// the production shape and the one the old fixtures denied.
//
// `can_provision` says a surface has a reconcile pass. The engine registers one
// for Atlassian, Jira and Confluence alike, so partitioning on it put all three
// in the same half and shipped the catalogue's own order — organization FIRST,
// the exact order its doc forbade. The order is the declared dependency
// reversed instead, which cannot collapse.
test("disconnect puts the account-creating surface last however many provision", () => {
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order[order.length - 1]).toBe("atlassian");
  expect(order).toHaveLength(3);
});

// AND A SURFACE NOBODY CONFIGURED IS NOT DELETED. A card lists what a tool
// CAN be made of; a delete against a surface with no block behind it is a
// request with nothing behind it.
test("disconnect skips the surfaces this company does not have", () => {
  rowsOf({ key: "jira", configured: true });
  const sections = [{ name: "Jira", tool: toolState({ key: "jira" }) }];
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual(["jira"]);
});

// --- disconnecting ---------------------------------------------------------- //

// ALPHABETICAL, and it replaced a connected-first sort deliberately.
//
// Connected-first reads well the first time and badly afterwards: the order
// changes as an app connects or fails, so a row moves out from under the
// cursor of somebody who came back to the same screen expecting it where it
// was. A name does not move.
test("the catalogue is ordered by name, whatever is connected", () => {
  const names = [...CATALOG].sort((a, b) => a.name.localeCompare(b.name)).map((e) => e.name);
  expect(names).toEqual([...names].sort((a, b) => a.localeCompare(b)));
  // And the order does not depend on state: the same list, connected or not.
  expect(names[0]).toBe("Atlassian");
  expect(names.at(-1)).toBe("Slack");
});

// WHICH PHASES THE SCREEN KEEPS WATCHING, and the rule tying that to what it
// paints. A connect starts work that takes seconds, and the row is the only
// place saying how it went: polled at a minute's cadence, a card said
// "Waiting for the provider" long after it had settled, which reads as the
// connect having done nothing.
//
// AMBER AND IN-FLIGHT ARE NEARLY THE SAME SET, and the exception is the
// point: `degraded` is amber because something is wrong, and it is NOT
// in-flight because nothing resolves it but a person. A phase added to one
// list and not the other is either watched forever or never watched at all.
test("every in-flight phase is amber, and the only amber phase that settles is degraded", () => {
  for (const phase of IN_FLIGHT) {
    expect(phaseTone(phase)).toBe("warning");
  }
  const amber = ["awaiting_admin", "provisioning", "activating", "disconnecting", "degraded"];
  for (const phase of amber) {
    expect(phaseTone(phase)).toBe("warning");
    expect(IN_FLIGHT.has(phase)).toBe(phase !== "degraded");
  }
  expect(phaseTone("ready")).toBe("success");
  expect(IN_FLIGHT.has("ready")).toBe(false);
});

// THE SURFACE THAT DELETES THE ACCOUNTS RECEIVES NO DELIVERIES.
//
// Atlassian's organization has no inbound route: it is where accounts are
// made, not somewhere Atlassian posts to. This filtered on the TRAFFIC rows,
// so the organization was dropped from every disconnect, its teardown never
// ran, and "remove the accounts Crewlet created" removed nothing while
// reporting success. The service accounts stayed in the admin console.
test("disconnect includes a configured surface that has no traffic row", () => {
  // Only the products have rows, which is what the engine reports.
  rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true });
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
  ];
  const order = disconnectOrder(atlassian, new Map(), sections);
  expect(order).toContain("atlassian");
  // AND STILL LAST: the organization's credential is what removes the
  // accounts, so taking it first would strand them.
  expect(order[order.length - 1]).toBe("atlassian");
});

// AND A SURFACE NOBODY CONFIGURED IS STILL LEFT ALONE. A delete against one
// is a request with nothing behind it.
test("disconnect skips a surface this company never configured", () => {
  rowsOf({ key: "jira", configured: true });
  const sections = [
    { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
    // Declared by the catalogue, never configured by this company.
    { name: "Forge relay", tool: toolState({ key: "forge", configured: false }) },
  ];
  expect(disconnectOrder(atlassian, new Map(), sections)).toEqual(["jira"]);
});

// --- an agent's own app, in the two acts a person performs ---------------- //

import { waitFor } from "@testing-library/react";
import { vi } from "vitest";
import type { SetupSeatState } from "~/protocol/types.ts";
import { SeatStep } from "./catalogue.tsx";

const github = CATALOG.find((e) => e.key === "github")!;

/** The gesture each seat's step asks of a person, as the Agents tab draws it beside the seat. */
function roster(...seats: SetupSeatState[]): { container: HTMLElement } {
  return render(
    <>
      {seats.map((seat) => (
        <SeatStep key={seat.handle} app={github.name} toolKey="github" seat={seat} />
      ))}
    </>,
  );
}

function seatOf(over: Partial<SetupSeatState>): SetupSeatState {
  return { handle: "cto", name: "CTO", requirements: [], satisfied: false, ...over };
}

type Sent = { method: string; path: string; body: unknown };

function stubFetch(sent: Sent[], answer: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        path: new URL(String(input), "http://engine.test").pathname,
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      return new Response(JSON.stringify(answer), { status });
    }),
  );
}

/**
 * The form the page submits, kept after it is taken out of the document.
 *
 * jsdom implements no submission, so the spy IS the assertion point: what
 * reaches the code host is exactly this element's action, target and fields.
 */
function capturedForm(): { form: HTMLFormElement | null } {
  const seen: { form: HTMLFormElement | null } = { form: null };
  vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
    this: HTMLFormElement,
  ) {
    seen.form = this;
  });
  return seen;
}

function fieldsOf(form: HTMLFormElement): Record<string, string> {
  return Object.fromEntries([...form.querySelectorAll("input")].map((i) => [i.name, i.value]));
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
});

// AN AGENT WITH NO APP OF ITS OWN CANNOT ACT AS ITSELF, and the roster is
// where that is answered: one app is one bot identity, so a card that says
// Connected over a seat with nothing is reporting the half that cannot be
// acted on. The button is what the engine's own step asks for.
test("a seat that needs an app of its own gets the button that creates one", () => {
  roster(
    seatOf({
      step: "create_app",
      tier: "read_only",
      // THE LABEL COMES WITH THE TIER, always: the dashboard is embedded in
      // the binary that serves it, so the two are never a version apart and
      // the screen never has to restate a vocabulary Go owns.
      tier_label: "Read-only",
      detail: "no app of its own yet",
    }),
  );

  expect(screen.getByRole("button", { name: "Create app on GitHub" })).toBeTruthy();
  // AND NOT THE OTHER STEP. They are two acts, and an operator shown both at
  // once has no way to know which one they are on.
  expect(screen.queryByRole("link", { name: "Install on GitHub" })).toBeNull();
});

// THE MANIFEST GOES AS A FORM POST, NEVER AS A FETCH.
//
// The request carries the operator's OWN session at the code host, which is
// what decides whether they may create an app on that organization, and the
// answer is a page the host renders for them to confirm. A fetch has neither:
// it would arrive as the dashboard rather than as the person, and the
// confirmation page would come back as a string with nowhere to display it.
test("the manifest reaches the code host as a form the operator's browser sends", async () => {
  const sent: Sent[] = [];
  const manifest = { name: "Crewlet CTO", url: "https://crewlet.ai", public: false };
  stubFetch(sent, {
    seat: "cto",
    tier: "review",
    action_url: "https://github.com/organizations/acme/settings/apps/new",
    manifest,
    state: "st-1",
  });
  const seen = capturedForm();
  roster(seatOf({ step: "create_app" }));

  fireEvent.click(screen.getByRole("button", { name: "Create app on GitHub" }));
  await waitFor(() => expect(seen.form).not.toBeNull());

  // ONE REQUEST, and it is the engine's. Nothing this dashboard sends reaches
  // the code host: a second entry here would be the fetch this must not be.
  expect(sent).toEqual([
    { method: "POST", path: "/setup/integrations/github/app", body: { seat: "cto" } },
  ]);

  const form = seen.form!;
  expect(form.method).toBe("post");
  expect(form.getAttribute("action")).toBe(
    "https://github.com/organizations/acme/settings/apps/new",
  );
  // A NEW TAB, so the flow's landing page does not take the dashboard away.
  expect(form.target).toBe("_blank");

  const fields = fieldsOf(form);
  // STRINGIFIED. A manifest handed to a form field as an object is the text
  // "[object Object]", which the host refuses with no clue why.
  expect(JSON.parse(fields.manifest!)).toEqual(manifest);
  expect(fields.state).toBe("st-1");
});

// THE SECOND ACT IS A LINK, because there is an address for it: the install
// page is built from the slug the host returned, which is a thing this engine
// can name and send somebody to.
test("an app nobody has installed is a link to the page that installs it", () => {
  roster(
    seatOf({
      step: "install_app",
      action_url: "https://github.com/apps/crewlet-cto/installations/new",
    }),
  );

  const link = screen.getByRole("link", { name: "Install on GitHub" });
  expect(link.getAttribute("href")).toBe("https://github.com/apps/crewlet-cto/installations/new");
  // A NEW TAB, so the card an operator started from is still behind it.
  expect(link.getAttribute("target")).toBe("_blank");
  expect(screen.queryByRole("button", { name: /Create app/ })).toBeNull();
});

// A CONTROL IS NEVER INVENTED. A step a newer node wrote is one this build
// has no act for, and a seat with nothing outstanding needs no control at
// all: guessing at either is worse than drawing nothing.
test("a step this build cannot perform draws no control", () => {
  roster(
    seatOf({ handle: "cto", step: "authorize_app" }),
    seatOf({ handle: "swe", name: "SWE", satisfied: true }),
    // INSTALL WITH NO ADDRESS. The install page is built from the slug the
    // host returned, so a seat whose app the engine cannot name has nowhere
    // to send anybody, and an anchor to nothing is a control that lies.
    seatOf({ handle: "sre", name: "SRE", step: "install_app" }),
  );

  expect(screen.queryByRole("button", { name: /Create app/ })).toBeNull();
  expect(screen.queryByRole("link", { name: /Install on/ })).toBeNull();
});

// A REFUSAL LANDS ON THE ROW THAT ASKED. The engine refuses this one for
// reasons an operator can fix (no public address is set, so an app created
// now would carry a delivery address that cannot be changed afterwards), and
// a button that quietly did nothing would leave them pressing it again.
test("the engine's refusal is reported beside the agent it was refused for", async () => {
  const sent: Sent[] = [];
  stubFetch(sent, { error: "no_external_url", hint: "set api.external_url first" }, 409);
  roster(seatOf({ step: "create_app" }));

  fireEvent.click(screen.getByRole("button", { name: "Create app on GitHub" }));
  expect(await screen.findByText("set api.external_url first")).toBeTruthy();
});

// --- the listing, and when it is worth reading again ---------------------- //

import { act } from "@testing-library/react";
import { useSetup } from "./catalogue.tsx";

/** The hook alone: it reaches for `rest` and for no context at all. */
function Probe() {
  const setup = useSetup();
  return (
    <span data-testid="probe">
      {setup.loading ? "loading" : "ready"}:{[...setup.byKey.keys()].join(",")}
    </span>
  );
}

// AN AGENT'S APP IS SET UP AT THE CODE HOST, IN ANOTHER TAB.
//
// This listing carries the roster and nothing pushes it, so a seat that had a
// Create button kept offering it after the app existed, and an operator who
// had done exactly what the card asked was told to do it again. The tab
// coming back is the one moment this screen can know something may have
// happened while it was not being read.
test("the roster is read again when the tab comes back", async () => {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      calls.push(new URL(String(input), "http://engine.test").pathname);
      return new Response(JSON.stringify({ tools: [toolState({ key: "github" })] }), {
        status: 200,
      });
    }),
  );
  render(<Probe />);
  await waitFor(() => expect(calls).toEqual(["/setup/integrations"]));

  // A TAB GOING AWAY IS NOT A REASON TO READ ANYTHING. Nobody is looking.
  Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  expect(calls.length).toBe(1);

  Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  // QUIETLY: there is already an answer on screen, and re-reading with the
  // skeleton would blank every card each time somebody switched tabs.
  expect(screen.getByTestId("probe").textContent).toBe("ready:github");
  await waitFor(() => expect(calls.length).toBe(2));
});

// ONE AGENT, ONE ROW.
//
// A per-seat app contributes one section per agent and they all carry the same
// tool, so walking the sections listed the whole roster once per section: a
// company with one agent saw it twice in the disconnect dialog, and a company
// with ten would have seen a hundred rows.
test("the disconnect roster lists each agent once", () => {
  const tool: SetupToolState = {
    key: "github",
    configured: true,
    enabled: true,
    satisfied: true,
    form_complete: true,
    seats_required: true,
    manage_path: "Delete GitHub App",
    // A COMPANY BLOCK AS WELL AS SEATS, which is GitHub's real shape and
    // what makes the sections outnumber the agents.
    requirements: [
      {
        field: "org",
        label: "Organization",
        kind: "id",
        config_path: "integrations.github.provisioning.org",
        required: true,
        present: true,
      },
    ],
    seats: [
      {
        handle: "sre-lead",
        name: "SRE Lead",
        satisfied: true,
        requirements: [],
        manage_url: "https://github.com/settings/apps/acme-sre/advanced",
      },
    ],
  };
  const entry: Entry = {
    key: "github",
    name: "GitHub",
    description: "Code and pull requests",
    vendor: "github",
    surfaces: [{ key: "github", name: "GitHub" }],
    doc: "github",
  };
  const sections = sectionsFor(entry, new Map([["github", tool]]));
  // The sections themselves are one per agent plus the company block, which
  // is the shape the dialog used to walk.
  expect(sections.length).toBeGreaterThan(1);

  const seats = [...new Map(sections.map((s) => [s.tool.key, s.tool])).values()].flatMap(
    (t) => t.seats ?? [],
  );
  expect(seats.map((s) => s.handle)).toEqual(["sre-lead"]);
});

// DISCONNECT IS NEVER A SILENT NO-OP, AND THE ROWS ARE WHAT KEEPS IT FROM
// BEING ONE.
//
// The Disconnect button is rendered from the ROWS; `sections` comes from a
// separate `GET /setup/integrations` that can 401 for want of an operator
// token. With the key set taken from `sections` alone, that window rendered a
// button whose dialog computed an empty list, issued no DELETE, and then
// closed exactly as it does after a real teardown — an operator told the
// integration was being removed while nothing had been asked of anything.
test("a configured surface is disconnectable even when the setup listing is unreadable", () => {
  const rows = rowsOf({ key: "atlassian", configured: true }, { key: "jira", configured: true });
  expect(disconnectOrder(atlassian, rows, [])).toEqual(["jira", "atlassian"]);
});

// AND A SURFACE NEITHER SOURCE KNOWS IS STILL NOT DISCONNECTED, so the
// fallback does not turn the whole catalogue into a teardown list.
test("an unconfigured surface stays out of the disconnect order", () => {
  expect(disconnectOrder(atlassian, rowsOf({ key: "jira", configured: true }), [])).toEqual([
    "jira",
  ]);
});

// THE LOOP'S CLOCK, as the fact line says it: a next sync whose time has passed
// is due or overdue, never a time "ago".
test("the sync fact says when the next sync is, and never puts it in the past", async () => {
  const { syncWords } = await import("./catalogue.tsx");
  const now = Date.parse("2026-10-29T12:00:00Z");
  const at = (minutes: number) => new Date(now + minutes * 60_000).toISOString();
  expect(syncWords(at(-30), at(10), now)).toBe("30m ago · next in 10m");
  expect(syncWords(at(-30), at(0), now)).toBe("30m ago · next due now");
  expect(syncWords(at(-30), at(-20), now)).toBe("30m ago · next overdue by 20m");
  expect(syncWords(null, null, now)).toBe("Not yet");
});
