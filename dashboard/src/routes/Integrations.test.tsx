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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import {
  CATALOG,
  EntryRow,
  IN_FLIGHT,
  Reconcile,
  byConfiguredThenName,
  disconnectOrder,
  phaseTone,
  rollUp,
} from "./Integrations.tsx";
import type { IntegrationRow } from "~/protocol/types.ts";

afterEach(cleanup);

// NULL RENDERS NOTHING. It is either a process with no loop to ask or a
// surface the loop has not reached, and neither is a claim that the surface
// is fine. Anything drawn here would be the invented health this screen
// refuses to show.
test("a surface with no status renders nothing", () => {
  const { container } = render(<Reconcile status={null} />);
  expect(container.innerHTML).toBe("");

  const absent = render(<Reconcile status={undefined} />);
  expect(absent.container.innerHTML).toBe("");
});

// A phase a newer node wrote must never read as healthy. Claiming a surface
// is fine on the strength of a word this build cannot interpret is the one
// answer that is certainly wrong.
test("an unknown phase is never positive", () => {
  expect(phaseTone("something_a_newer_build_wrote")).not.toBe("positive");
  expect(phaseTone("")).not.toBe("positive");
  expect(phaseTone("ready")).toBe("positive");
});

// Degraded is caution rather than critical, and the distinction is real:
// agents are still working, which is what separates it from a surface that
// cannot authenticate at all.
test("the tone follows how much is not working", () => {
  expect(phaseTone("degraded")).toBe("caution");
  expect(phaseTone("unconfigured")).toBe("critical");
  // EVERY IN-PROGRESS PHASE IS AMBER. Each one is an integration that does
  // not work yet, and neutral is the tone this screen uses for a state
  // nobody needs to come back to.
  for (const phase of ["provisioning", "activating", "awaiting_admin", "disconnecting"]) {
    expect(phaseTone(phase)).toBe("caution");
  }
});

// The detail is the sentence that saves somebody reading logs, and the link
// is where they go to act on it.
test("a blocked surface says what to do and where", () => {
  render(
    <Reconcile
      status={{
        phase: "degraded",
        actor: "admin",
        detail: "swe has no Jira account, so no issue reaches it",
        action_url: "https://jira.example.com/admin",
        findings: [{ kind: "identity_failed", subject: "swe" }],
      }}
    />,
  );
  expect(screen.getByText(/swe has no Jira account/)).toBeTruthy();
  expect(screen.getByText(/you, at the third-party app/)).toBeTruthy();
  expect(screen.getByRole("link").getAttribute("href")).toBe("https://jira.example.com/admin");
});

// THE FINDINGS BEHIND THE HEADLINE. The report says what to do next and the
// findings say what is actually wrong, so an operator who fixes the first
// should not wait a full pass to learn there were four more.
test("the findings the phase was not derived from are still reachable", () => {
  render(
    <Reconcile
      status={{
        phase: "degraded",
        actor: "admin",
        detail: "ceo needs maintainer on api-gateway",
        findings: [
          { kind: "grant_short", subject: "ceo", detail: "ceo needs maintainer" },
          { kind: "grant_excess", subject: "cto", detail: "cto holds owner on api-gateway" },
        ],
      }}
    />,
  );
  expect(screen.getByText(/1 more finding/)).toBeTruthy();
  expect(screen.getByText(/cto holds owner/)).toBeTruthy();
});

// A pass that could not read the surface is a FAULT, not a finding, and it is
// labelled as one rather than rendered as something about the vendor.
test("a failed pass is reported as a failed pass", () => {
  render(
    <Reconcile
      status={{ phase: "activating", actor: "engine", last_error: "502 from the instance" }}
    />,
  );
  expect(screen.getByText(/Last pass failed: 502 from the instance/)).toBeTruthy();
});

// --- one row per tool ----------------------------------------------------- //

const atlassian = CATALOG.find((e) => e.key === "atlassian")!;
const slack = CATALOG.find((e) => e.key === "slack")!;

function rowsOf(...rows: IntegrationRow[]): Map<string, IntegrationRow> {
  return new Map(rows.map((r) => [r.key, r]));
}

// A TOOL IS THE LEAST READY OF ITS SURFACES. Atlassian is one row over three
// engine surfaces, and a row that reported the first surface it found would
// call the tool ready while its Jira was refusing every credential.
test("a tool reports its least ready surface, and names it", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "confluence", configured: true, reconcile: { phase: "ready" } },
      {
        key: "jira",
        configured: true,
        reconcile: { phase: "degraded", actor: "admin", detail: "swe has no Jira account" },
      },
    ),
  );
  expect(state.tag).toBe("degraded");
  expect(state.tone).toBe("caution");
  expect(state.status).toBe("Jira: swe has no Jira account");
  expect(state.attention).toBe(true);
});

// A tool with one surface does not prefix its own name: "Slack: ..." on the
// Slack row says nothing.
test("a single-surface tool does not name itself", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true, secret_usable: false }));
  expect(state.tag).toBe("Connecting");
  expect(state.status).toMatch(/^the webhook secret did not resolve/);
  expect(state.attention).toBe(true);
});

// A ready tool says nothing under its name. The tag is the claim, and a
// status line repeating "ready" would be the invented reassurance this
// screen refuses to show.
test("a ready tool has no status line", () => {
  const state = rollUp(
    atlassian,
    rowsOf({ key: "jira", configured: true, reconcile: { phase: "ready", detail: "3 seats" } }),
  );
  expect(state.tag).toBe("ready");
  expect(state.status).toBeUndefined();
  expect(state.attention).toBe(false);
});

// NOT CONFIGURED, PAUSED and CONFIGURED are three different facts. Absent is
// nobody set it up, paused is somebody switched it off on purpose, and the
// two used to collapse into the state most likely to be mistaken for a
// mistake.
test("absent, paused and connecting are told apart", () => {
  // NO TAG at all: the Connect button beside it is the whole message, and a
  // chip saying "not connected" on every unconfigured row reads as a fault
  // list rather than a catalogue.
  expect(rollUp(slack, rowsOf()).tag).toBe("");
  expect(rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: false })).tag).toBe(
    "Paused",
  );
  // Configured with no report behind it is NOT "connected": there is no
  // measured claim, and inventing one is the whole failure this screen is
  // built to avoid. It is the window between connecting and the loop's first
  // pass, and it says so.
  const connecting = rollUp(slack, rowsOf({ key: "slack", configured: true, enabled: true }));
  expect(connecting.tag).toBe("Connecting");
  expect(connecting.tone).toBe("caution");
});

// A phase a newer node wrote outranks ready and is outranked by every phase
// this build knows is broken, so it never reads as healthy and never hides a
// real problem behind it.
test("an unknown phase is never the tool's ready state", () => {
  const unknown = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "ready" } },
      { key: "confluence", configured: true, reconcile: { phase: "something_new" } },
    ),
  );
  expect(unknown.tag).toBe("something new");
  expect(unknown.tone).not.toBe("positive");

  const broken = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "x" } },
      { key: "confluence", configured: true, reconcile: { phase: "something_new" } },
    ),
  );
  expect(broken.tag).toBe("degraded");
});

// A CARD'S BODY IS ITS AGENTS, not a restatement of its header.
//
// It opened with a row per surface — "Jira, /webhooks/jira, Connected" —
// under a header already saying Connected, so the first thing a reader saw
// on opening a card was the engine repeating the tag above it and naming a
// route nobody has to paste any more, now that the loop registers the hook.
test("a working surface adds nothing to the card", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        {
          key: "jira",
          configured: true,
          inbound: 4,
          inbound_path: "/webhooks/jira",
          reconcile: { phase: "ready" },
        },
        { key: "confluence", configured: true, inbound: 1, reconcile: { phase: "ready" } },
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.queryByText("/webhooks/jira")).toBeNull();
  expect(screen.queryByText("Jira")).toBeNull();
  expect(screen.queryByText("Confluence")).toBeNull();
});

// AND A BROKEN ONE STILL SAYS SO, AND SAYS WHICH. The header rolls up to the
// least ready surface, so a tool with two of them has two answers and only
// one fits in the tag.
test("a faulted surface says what is wrong and which surface it is", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf(
        { key: "jira", configured: true, reconcile: { phase: "ready" } },
        {
          key: "confluence",
          configured: true,
          secret_usable: false,
          reconcile: { phase: "degraded", detail: "the webhook secret did not resolve" },
        },
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getByText("Confluence")).toBeTruthy();
  expect(screen.getByText("secret unresolved")).toBeTruthy();
  // And the surface that works is not listed beside it.
  expect(screen.queryByText("Jira")).toBeNull();
});

// A TOOL NOBODY SET UP HAS NOTHING TO DISCLOSE, so it gets no disclosure: a
// chevron that opens an empty box is a control that lies.
test("an absent tool is a plain card with no disclosure and no badge", () => {
  const { container } = render(<EntryRow entry={slack} rows={rowsOf()} />);
  // NO STATUS BADGE. Nothing has been configured, so there is nothing to
  // report; the Connect button is the whole message.
  expect(container.querySelector(".badge")).toBeNull();
  expect(screen.queryByRole("button", { name: /details/i })).toBeNull();
  // And it still says what the tool is for, because the catalogue is what
  // tells a reader the engine serves it at all.
  expect(screen.getByText("Team communication")).toBeTruthy();
});

// A READY PHASE DOES NOT SILENCE A REFUSED WEBHOOK SECRET.
//
// The loop says nothing about ingress at all: the Jira and GitHub passes run
// with no webhook base, and secret_usable is computed separately from what
// the process resolved. So the two answer different questions, and a row that
// stopped at the phase put a green tag over a surface nothing could reach.
test("a ready tool still reports a refused secret", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "github")!,
    rowsOf({
      key: "github",
      configured: true,
      secret_usable: false,
      reconcile: { phase: "ready" },
    }),
  );
  expect(state.tag).toBe("ready");
  expect(state.status).toMatch(/^the webhook secret did not resolve/);
  expect(state.attention).toBe(true);
});

// And an unrouted surface, the other way a configured tool is silently not
// working, is reported on a ready phase too.
test("a ready tool still reports that nothing routes", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({ key: "datadog", configured: true, routes: false, reconcile: { phase: "ready" } }),
  );
  expect(state.status).toMatch(/nothing routes them to a seat$/);
  expect(state.attention).toBe(true);
});

// ATTENTION MEANS A PERSON IS NEEDED, which is the actor's question, not the
// phase's. Amber beside a neutral tag told an operator to act while the
// engine was still working.
test("only a phase a person owes draws attention", () => {
  const owed = ["admin", "operator"];
  const notOwed = ["engine", "provider"];
  for (const actor of owed) {
    const state = rollUp(
      atlassian,
      rowsOf({
        key: "jira",
        configured: true,
        reconcile: { phase: "degraded", actor, detail: "x" },
      }),
    );
    expect([actor, state.attention]).toEqual([actor, true]);
  }
  for (const actor of notOwed) {
    const state = rollUp(
      atlassian,
      rowsOf({
        key: "jira",
        configured: true,
        reconcile: { phase: "activating", actor, detail: "Atlassian is applying it" },
      }),
    );
    expect([actor, state.attention]).toEqual([actor, false]);
    // The line is still shown; it is just not dressed as a problem.
    expect(state.status).toBe("Jira: Atlassian is applying it");
  }
});

// THE LEAST READY SURFACE IS THE ENGINE'S OWN ORDER, integration.Phases: a
// degraded integration is still working and one still coming up is not, so
// activating outranks degraded rather than the reverse.
test("the phase order is the engine's", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "partly" } },
      {
        key: "confluence",
        configured: true,
        reconcile: { phase: "activating", detail: "coming up" },
      },
    ),
  );
  expect(state.tag).toBe("activating");
  expect(state.status).toBe("Confluence: coming up");
});

// --- the action slot -------------------------------------------------------- //

import { actionFor, sectionsFor } from "./Integrations.tsx";
import type { SetupToolState } from "~/protocol/types.ts";

function toolState(over: Partial<SetupToolState>): SetupToolState {
  return {
    key: "github",
    configured: true,
    enabled: true,
    satisfied: true,
    requirements: [],
    ...over,
  };
}

// THE ACTION IS A PURE FUNCTION of what the engine says, and of the same
// inputs the state tag beside it is derived from. Two derivations would
// eventually tell an operator two different things in one row.
test("the action follows the state", () => {
  const ready = rollUp(atlassian, rowsOf({ key: "jira", configured: true }));
  expect(actionFor(ready, [toolState({ configured: false })])?.label).toBe("Connect");
  expect(actionFor(ready, [toolState({ satisfied: false })])?.label).toBe("Continue");
  // A WORKING TOOL OFFERS NOTHING. Nothing a person does moves it, and the
  // Manage button that used to sit here read as a card's primary action
  // while saying only "nobody is needed".
  expect(actionFor(ready, [toolState({})])).toBeNull();

  // A FAULT IS NOT AN ACTION. A row a person owes something on says so in
  // its tag and its status line; what to do about it is the settings the
  // gear opens. A Fix button beside Disconnect, appearing and disappearing
  // as an integration breaks and recovers, carried nothing the line above
  // it did not already say.
  const owed = rollUp(
    atlassian,
    rowsOf({
      key: "jira",
      configured: true,
      reconcile: { phase: "degraded", actor: "admin", detail: "x" },
    }),
  );
  expect(actionFor(owed, [toolState({})])).toBeNull();
});

// A TOOL IS COMPLETE ONLY WHEN EVERY CONFIGURED SURFACE IS. Atlassian with
// Jira set up and Confluence half done is neither Connect nor finished: it is
// a tool with something left to do, and reading only the first surface would
// have called it done.
test("one unfinished surface makes the whole tool unfinished", () => {
  const ready = rollUp(atlassian, rowsOf({ key: "jira", configured: true }));
  const mixed = actionFor(ready, [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", configured: true, satisfied: false }),
  ]);
  expect(mixed?.label).toBe("Continue");

  // AND A SURFACE NOBODY HAS CONFIGURED IS SOMETHING TO CONTINUE, not to
  // connect.
  //
  // It does not make the tool UNFINISHED — a company using Jira and not
  // Confluence is not half broken, and the tag says what the connected
  // surfaces are doing — but it is something a person can act on, and
  // connecting the organization alone left the Atlassian card with no
  // button at all: nothing on screen would add the products.
  //
  // The WORD matters: this said Connect, beside a tag reading Connected, so
  // the card claimed both at once. Continue is what this screen already says
  // about a tool with something left to do.
  const partial = actionFor(ready, [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", configured: false, satisfied: false }),
  ]);
  expect(partial?.label).toBe("Continue");

  // A tool whose every surface is connected and working offers nothing.
  const done = actionFor(ready, [
    toolState({ key: "jira", configured: true, satisfied: true }),
    toolState({ key: "confluence", configured: true, satisfied: true }),
  ]);
  expect(done).toBeNull();
});

// A tool this build knows nothing about offers nothing: a button that
// discovers on a press that there is no surface behind it is worse than none.
test("a tool with no setup surface offers no action", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  expect(actionFor(state, [])).toBeNull();
});

// --- a per-seat third-party app ------------------------------------------------------ //

// SLACK GETS A SECTION PER SEAT, because each agent has its own app and so its
// own bot token. A seat with no app yet is exactly the seat somebody opened
// this dialog to give one to, so it is listed too.
test("a per-seat app becomes one section per agent", () => {
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

// AND THE ROSTER IS COUNTED ONCE. A per-seat app contributes one section per
// agent, all carrying the same tool, so reading the seats off every section
// rendered each agent as many times as there were sections.
test("a roster is listed once however many sections carry it", () => {
  render(
    <EntryRow
      entry={slack}
      rows={rowsOf({ key: "slack", configured: true })}
      sections={sectionsFor(
        slack,
        new Map([
          [
            "slack",
            toolState({
              key: "slack",
              configured: true,
              requirements: [],
              seats_required: true,
              seats: [{ handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied: true }],
            }),
          ],
        ]),
      )}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Slack details/ }));
  expect(screen.getAllByText("SRE Lead").length).toBe(1);
});

// A company whose Slack block exists and whose seats hold no app is not
// connected: nothing can post.
test("a per-seat app with no seat set up is unfinished", () => {
  const state = rollUp(slack, rowsOf({ key: "slack", configured: true }));
  const action = actionFor(state, [
    toolState({
      key: "slack",
      configured: true,
      satisfied: true,
      // LOAD-BEARING, which is Slack's whole shape and nobody else's.
      seats_required: true,
      seats: [{ handle: "cto", requirements: [], satisfied: false }],
    }),
  ]);
  expect(action?.label).toBe("Continue");

  // AND AN INFORMATIONAL ROSTER IS NOT WORK OUTSTANDING. Every app lists its
  // agents now, and most of those credentials are an upgrade on an app that
  // already works: reading any roster as unfinished put a Continue button on
  // every connected card.
  const informational = actionFor(state, [
    toolState({
      key: "datadog",
      configured: true,
      satisfied: true,
      seats: [{ handle: "cto", requirements: [], satisfied: false }],
    }),
  ]);
  expect(informational).toBeNull();
});

// --- what a card offers, and what it no longer does ------------------------ //

// CONNECTING IS THE PERMISSION, so nothing on a connected card asks for one
// again. The reconcile loop registers the hooks and creates the accounts on
// its own timer now, which took the meaning out of every button that used to
// stand for a permission a person had to grant a second time: Run setup and
// Recheck per surface, and Manage in the header beside Disconnect.
test("a connected card offers no pass controls", () => {
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        { name: "Jira", tool: toolState({ key: "jira", can_provision: true }) },
        { name: "Confluence", tool: toolState({ key: "confluence", can_provision: true }) },
      ]}
      onConnect={() => {}}
      onDisconnect={() => {}}
    />,
  );

  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  for (const gone of ["Run setup", "Recheck", "Manage"]) {
    expect(screen.queryByRole("button", { name: gone })).toBeNull();
  }
  expect(screen.getByRole("button", { name: "Disconnect" })).toBeTruthy();
});

// SETTINGS SIT BESIDE THE DISCLOSURE, as a square the size of the chevron.
//
// At the foot of the open card it was an always-available control behind a
// disclosure and one scroll away; as a labelled button in the header it
// competed with Disconnect for the reader's eye. An icon square does
// neither, and it is available without opening the card.
test("settings open from the header, beside the chevron", () => {
  let opened = 0;
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "datadog")!}
      rows={rowsOf({ key: "datadog", configured: true })}
      sections={[{ name: "Datadog", tool: toolState({ key: "datadog" }) }]}
      onConnect={() => {
        opened += 1;
      }}
      onDisconnect={() => {}}
    />,
  );

  // Closed, without opening the card at all.
  fireEvent.click(screen.getByRole("button", { name: "Datadog settings" }));
  expect(opened).toBe(1);
});

// AND A TOOL NOBODY HAS CONNECTED HAS NO SETTINGS TO OPEN: its Connect
// button is the whole of what it offers.
test("an unconnected tool offers no settings square", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "gitlab")!}
      rows={rowsOf()}
      sections={[{ name: "GitLab", tool: toolState({ key: "gitlab", configured: false }) }]}
      onConnect={() => {}}
    />,
  );
  expect(screen.queryByRole("button", { name: "GitLab settings" })).toBeNull();
  expect(screen.getByRole("button", { name: "Connect" })).toBeTruthy();
});

// THE AGENTS ARE THE POINT, so the card lists them.
//
// Only Slack had a roster, because it was the only app with a route per seat
// and the roster was built from that. Every other card opened on a surface
// row and nothing else: Connected, over no answer at all to which of this
// company's agents can actually work in the app.
test("the card lists each agent and what it holds", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "datadog")!}
      rows={rowsOf({ key: "datadog", configured: true })}
      sections={[
        {
          name: "Datadog",
          tool: toolState({
            key: "datadog",
            seats: [
              {
                handle: "sre-lead",
                name: "SRE Lead",
                requirements: [],
                present: true,
                satisfied: true,
                detail: "mcp_env.datadog.DD_APP_KEY",
              },
              {
                handle: "cto",
                name: "CTO",
                requirements: [],
                present: false,
                satisfied: false,
                detail: "not in Datadog yet, created on the next sync",
              },
            ],
          }),
        },
      ]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Datadog details/ }));
  expect(screen.getByText("SRE Lead")).toBeTruthy();
  expect(screen.getByText("CTO")).toBeTruthy();
  // AND WHAT EACH ONE HOLDS. A roster of names with no state is a list of
  // agents, not an answer about the integration.
  expect(screen.getByText("mcp_env.datadog.DD_APP_KEY")).toBeTruthy();
  expect(screen.getByText("not in Datadog yet, created on the next sync")).toBeTruthy();
});

// ONE ROW PER AGENT, whatever the card is made of.
//
// A company with one agent saw three rows on Atlassian — Organization, Jira,
// Confluence — because every surface reports its own roster and the card
// listed them all. They are one person's one account: Atlassian is where it
// is created, and the two products are what it then works in.
test("an agent is listed once however many surfaces report it", () => {
  const seat = (detail: string, satisfied: boolean) => [
    { handle: "sre-lead", name: "SRE Lead", requirements: [], satisfied, detail },
  ];
  render(
    <EntryRow
      entry={atlassian}
      rows={rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true })}
      sections={[
        {
          name: "Organization",
          tool: toolState({
            key: "atlassian",
            can_provision: true,
            seats: seat("mcp_env.atlassian.JIRA_API_TOKEN", true),
          }),
        },
        {
          name: "Jira",
          tool: toolState({ key: "jira", seats: seat("no Jira credential yet", false) }),
        },
        {
          name: "Confluence",
          tool: toolState({
            key: "confluence",
            seats: seat("no Confluence credential yet", false),
          }),
        },
      ]}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show Atlassian details/ }));
  expect(screen.getAllByText("SRE Lead").length).toBe(1);
  // THE SURFACE THAT PROVISIONS WINS, because its answer is about the
  // account rather than about one product's credential slot.
  expect(screen.getByText("mcp_env.atlassian.JIRA_API_TOKEN")).toBeTruthy();
  expect(screen.queryByText(/no Confluence credential/)).toBeNull();
});

// WHAT THIS COMPANY HAS COMES FIRST, then what it could have.
//
// Connected covers ATTEMPTED as well as working: a card with a block behind
// it, whatever the loop says about it, because a broken integration is one
// this company has and is the row most worth reaching first. Sorting on
// health instead would move a row out from under the cursor of somebody
// watching an app recover, which is exactly when they are looking at it.
test("configured integrations lead the list, each half alphabetical", () => {
  const rows = rowsOf(
    // Datadog works; Jira is broken. Both are this company's.
    { key: "datadog", configured: true, reconcile: { phase: "ready" } },
    { key: "jira", configured: true, reconcile: { phase: "degraded", detail: "x" } },
  );
  const order = [...CATALOG].sort(byConfiguredThenName(rows)).map((e) => e.name);
  expect(order).toEqual(["Atlassian", "Datadog", "GitHub", "GitLab", "Mattermost", "Slack"]);
  // And with nothing configured it is plain alphabetical, unchanged.
  const empty = [...CATALOG].sort(byConfiguredThenName(rowsOf())).map((e) => e.name);
  expect(empty).toEqual([...empty].sort((a, b) => a.localeCompare(b)));
});

// DISCONNECT MEANS THE CARD, and the provisioner goes last.
//
// It took the surface that happened to be listed first: on Atlassian the
// organization's account was deleted and its block removed while Jira and
// Confluence were left untouched, under a card still reading Connected. And
// the order is not arbitrary — the organization's credential is what removes
// the accounts, so taking it first would strand what the products still hold.
test("disconnect takes every configured surface, provisioner last", () => {
  const rows = rowsOf(
    { key: "atlassian", configured: true },
    { key: "jira", configured: true },
    { key: "confluence", configured: true },
  );
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: false }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: false }) },
  ];
  // The products in the catalogue's own order, and the organization after
  // both of them. What this asserts is the LAST position; the two products
  // are peers and either order between them takes the same things away.
  expect(disconnectOrder(atlassian, rows, sections)).toEqual(["confluence", "jira", "atlassian"]);
});

// AND A SURFACE NOBODY CONFIGURED IS NOT DELETED. A card lists what a tool
// CAN be made of; a delete against a surface with no block behind it is a
// request with nothing behind it.
test("disconnect skips the surfaces this company does not have", () => {
  const rows = rowsOf({ key: "jira", configured: true });
  const sections = [{ name: "Jira", tool: toolState({ key: "jira" }) }];
  expect(disconnectOrder(atlassian, rows, sections)).toEqual(["jira"]);
});

// A DROP IS A PROBLEM, so it survives the counters being removed.
//
// The three counters were dropped from the surface line because all three
// read zero on a healthy surface. One of them carried a real fault: a
// delivery this engine verified and then routed nowhere means whatever sent
// it is reaching the engine and reaching no agent. That becomes a sentence,
// beside the reconcile findings, rather than a number to interpret.
test("a dropped delivery is stated, not counted", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true, inbound: 12, skipped: 3 })}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  expect(screen.getByText(/3 deliveries were verified and dropped/)).toBeTruthy();
});

// And a surface dropping nothing says nothing, which is the whole reason the
// counters went: a zero is not news.
test("a surface dropping nothing carries no note about it", () => {
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true, inbound: 12, skipped: 0 })}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Show GitHub details/ }));
  expect(screen.queryByText(/verified and dropped/)).toBeNull();
});

// --- disconnecting ---------------------------------------------------------- //

// A TOOL NOBODY CONFIGURED HAS NOTHING TO DISCONNECT FROM, so it offers no
// control for it. Otherwise a catalogue of six unconfigured third-party apps would
// carry six buttons that undo nothing.
test("an unconfigured tool offers no disconnect", () => {
  render(<EntryRow entry={slack} rows={rowsOf()} onDisconnect={() => {}} />);
  expect(screen.queryByRole("button", { name: "Disconnect" })).toBeNull();
});

// And a connected one does, beside its own action rather than hidden in the
// body: taking an integration away is a decision about the whole tool.
test("a connected tool offers a disconnect", () => {
  const asked: string[] = [];
  render(
    <EntryRow
      entry={CATALOG.find((e) => e.key === "github")!}
      rows={rowsOf({ key: "github", configured: true })}
      onDisconnect={() => asked.push("github")}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Disconnect" }));
  expect(asked).toEqual(["github"]);
});

// A DISCONNECT SHOWS THE MOMENT IT IS ASKED FOR, not when the first teardown
// pass happens to run.
//
// Between the request and that pass the stored phase is still whatever the
// last reconcile concluded — usually ready — so a screen reading the phase
// alone showed a connected integration somebody had already asked to remove,
// and they pressed the button again. Datadog showed nothing at all, because
// it has no pass to write a phase and the row carried none.
test("a disconnect that has been asked for reads as Disconnecting", () => {
  const state = rollUp(
    CATALOG.find((e) => e.key === "datadog")!,
    rowsOf({
      key: "datadog",
      configured: true,
      // What the engine sends between the ask and the first pass: the
      // intent set, and the phase still the last reconcile's.
      reconcile: { phase: "ready", disconnecting: true },
    }),
  );
  expect(state.tag).toBe("Disconnecting");
  // NOT attention: the engine is doing it and nobody is owed anything.
  expect(state.attention).toBe(false);
});

// And it outranks a surface that is broken, because a tool being removed is
// not a tool anybody should be sent to fix.
test("a teardown outranks a failing surface", () => {
  const state = rollUp(
    atlassian,
    rowsOf(
      { key: "jira", configured: true, reconcile: { phase: "unconfigured" } },
      { key: "confluence", configured: true, reconcile: { phase: "ready", disconnecting: true } },
    ),
  );
  expect(state.tag).toBe("Disconnecting");
});

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
    expect(phaseTone(phase)).toBe("caution");
  }
  const amber = ["awaiting_admin", "provisioning", "activating", "disconnecting", "degraded"];
  for (const phase of amber) {
    expect(phaseTone(phase)).toBe("caution");
    expect(IN_FLIGHT.has(phase)).toBe(phase !== "degraded");
  }
  expect(phaseTone("ready")).toBe("positive");
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
  const rows = rowsOf({ key: "jira", configured: true }, { key: "confluence", configured: true });
  const sections = [
    { name: "Organization", tool: toolState({ key: "atlassian", can_provision: true }) },
    { name: "Confluence", tool: toolState({ key: "confluence", can_provision: false }) },
    { name: "Jira", tool: toolState({ key: "jira", can_provision: false }) },
  ];
  const order = disconnectOrder(atlassian, rows, sections);
  expect(order).toContain("atlassian");
  // AND STILL LAST: the organization's credential is what removes the
  // accounts, so taking it first would strand them.
  expect(order[order.length - 1]).toBe("atlassian");
});

// AND A SURFACE NOBODY CONFIGURED IS STILL LEFT ALONE. A delete against one
// is a request with nothing behind it.
test("disconnect skips a surface this company never configured", () => {
  const rows = rowsOf({ key: "jira", configured: true });
  const sections = [
    { name: "Jira", tool: toolState({ key: "jira", can_provision: false }) },
    // Declared by the catalogue, never configured by this company.
    { name: "Forge relay", tool: toolState({ key: "forge", configured: false }) },
  ];
  expect(disconnectOrder(atlassian, rows, sections)).toEqual(["jira"]);
});

// A CARD CANNOT BE CONNECTED AND OFFER TO CONNECT.
//
// The two halves of this screen arrive separately and the socket's rows are
// quicker. Straight after a connect the rows already say the block exists
// while the setup listing is still the pre-connect one, so the card drew
// "Connect" beside a tag reading Connected: two controls describing the same
// tool, disagreeing, with the button the wrong one.
test("a stale setup listing offers nothing rather than contradicting the tag", () => {
  const connected = rollUp(atlassian, rowsOf({ key: "jira", configured: true }));
  const behind = [
    toolState({ key: "atlassian", configured: false }),
    toolState({ key: "jira", configured: false }),
  ];
  // present: the rows say this company has the tool.
  expect(actionFor(connected, behind, true)).toBeNull();
  // AND A TOOL NOBODY HAS CONNECTED STILL OFFERS CONNECT. With no rows there
  // is no tag to contradict, and the button is the only thing on the card
  // that says anything.
  expect(actionFor(connected, behind, false)?.label).toBe("Connect");
});
